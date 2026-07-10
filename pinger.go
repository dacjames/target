package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// PingResult summarizes an ICMP echo attempt.
type PingResult struct {
	Sent   int
	Recv   int
	Output string
}

// Pinger sends ICMP echo requests to a host. Implementations may shell out to
// the system ping, use a raw socket, etc. Multiple pingers may be added later.
type Pinger interface {
	Ping(ctx context.Context, host string, count int, timeout time.Duration) (PingResult, error)
}

// defaultPinger is the process-wide pinger used by ping callbacks. It defaults
// to the auto (socket-primary, system-fallback) pinger; run() may override it
// from TARGET_PINGER.
var defaultPinger Pinger = fallbackPinger{primary: socketPinger{}, backup: systemPinger{}}

// selectPinger maps a TARGET_PINGER value to a Pinger implementation:
//
//	auto (default) — try the unprivileged ICMP socket, fall back to /bin/ping
//	socket         — unprivileged ICMP socket only
//	system         — shell out to /bin/ping only
//
// An unknown value warns and falls back to auto.
func selectPinger(name string, lg *logger) Pinger {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "auto":
		return fallbackPinger{primary: socketPinger{}, backup: systemPinger{}}
	case "socket":
		return socketPinger{}
	case "system":
		return systemPinger{}
	default:
		lg.warnf("unknown TARGET_PINGER %q; using auto", name)
		return fallbackPinger{primary: socketPinger{}, backup: systemPinger{}}
	}
}

// clampPing normalizes a ping count and overall timeout to sane minimums,
// shared by every Pinger implementation.
func clampPing(count int, timeout time.Duration) (int, time.Duration) {
	if count <= 0 {
		count = 1
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	return count, timeout
}

// fallbackPinger tries primary and, only if it fails to run at all (a non-nil
// error that is not context cancellation), falls back to backup. A valid "no
// reply" outcome (recv=0, err=nil) is NOT a failure and does not trigger the
// backup — matching the systemPinger contract.
type fallbackPinger struct {
	primary Pinger
	backup  Pinger
}

func (f fallbackPinger) Ping(ctx context.Context, host string, count int, timeout time.Duration) (PingResult, error) {
	res, err := f.primary.Ping(ctx, host, count, timeout)
	if err != nil && ctx.Err() == nil {
		return f.backup.Ping(ctx, host, count, timeout)
	}
	return res, err
}

// socketPinger sends ICMP echo requests over an unprivileged datagram socket —
// socket(AF_INET, SOCK_DGRAM, IPPROTO_ICMP) — requiring neither root nor
// CAP_NET_RAW. golang.org/x/net/icmp's "udp4" network opens exactly that
// socket; the kernel rewrites the ICMP id to the socket port and strips the IP
// header on receive, so replies are matched on the echo sequence number.
//
// See https://sturmflut.github.io/linux/ubuntu/2015/01/17/unprivileged-icmp-sockets-on-linux/
type socketPinger struct{}

func (socketPinger) Ping(ctx context.Context, host string, count int, timeout time.Duration) (PingResult, error) {
	count, timeout = clampPing(count, timeout)

	// IPv4 only, matching net.ipv4.ping_group_range. An unresolvable or
	// non-IPv4 host is a setup error so the fallback pinger can take over.
	dst, err := net.ResolveIPAddr("ip4", host)
	if err != nil {
		return PingResult{}, fmt.Errorf("resolve %q: %w", host, err)
	}

	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		// The socket could not be opened (e.g. Linux ping_group_range
		// disabled). Report it so fallbackPinger can shell out to ping.
		return PingResult{}, fmt.Errorf("icmp socket: %w", err)
	}
	defer conn.Close()

	id := os.Getpid() & 0xffff
	deadline := time.Now().Add(timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return PingResult{}, fmt.Errorf("icmp deadline: %w", err)
	}

	sent, recv := 0, 0
	for seq := 0; seq < count; seq++ {
		if ctx.Err() != nil {
			break
		}
		msg := icmp.Message{
			Type: ipv4.ICMPTypeEcho,
			Code: 0,
			Body: &icmp.Echo{
				ID:   id,
				Seq:  seq,
				Data: []byte("target-icmp-probe"),
			},
		}
		wb, err := msg.Marshal(nil)
		if err != nil {
			return PingResult{Sent: sent, Recv: recv}, fmt.Errorf("icmp marshal: %w", err)
		}
		if _, err := conn.WriteTo(wb, &net.UDPAddr{IP: dst.IP, Zone: dst.Zone}); err != nil {
			return PingResult{Sent: sent, Recv: recv}, fmt.Errorf("icmp send: %w", err)
		}
		sent++

		if waitForReply(ctx, conn, dst.IP, seq, deadline) {
			recv++
		}
	}

	res := PingResult{
		Sent: sent,
		Recv: recv,
		Output: fmt.Sprintf("%d packets transmitted, %d received (icmp socket to %s)",
			sent, recv, dst.IP),
	}
	return res, nil
}

// waitForReply reads until it sees an echo reply from want with sequence seq,
// or the deadline/ctx elapses. Stray replies (other sequence numbers, other
// peers) are skipped so a slow earlier reply cannot be miscounted.
func waitForReply(ctx context.Context, conn *icmp.PacketConn, want net.IP, seq int, deadline time.Time) bool {
	rb := make([]byte, 1500)
	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		n, peer, err := conn.ReadFrom(rb)
		if err != nil {
			return false // deadline exceeded or socket closed
		}
		if pu, ok := peer.(*net.UDPAddr); ok && !pu.IP.Equal(want) {
			continue
		}
		msg, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), rb[:n])
		if err != nil {
			continue
		}
		if msg.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		if echo, ok := msg.Body.(*icmp.Echo); ok && echo.Seq == seq {
			return true
		}
	}
}

// systemPinger shells out to the platform `ping` binary.
type systemPinger struct{}

// pingStats matches the "N packets transmitted, M received/packets received"
// summary line emitted by both Linux (iputils) and BSD/macOS ping.
var pingStats = regexp.MustCompile(`(\d+) packets transmitted, (\d+)`)

func (systemPinger) Ping(ctx context.Context, host string, count int, timeout time.Duration) (PingResult, error) {
	count, timeout = clampPing(count, timeout)
	secs := int(timeout.Seconds())

	// Flag names for the overall deadline differ across platforms.
	var args []string
	switch runtime.GOOS {
	case "linux":
		args = []string{"-c", strconv.Itoa(count), "-w", strconv.Itoa(secs), host}
	default: // darwin/bsd
		args = []string{"-c", strconv.Itoa(count), "-t", strconv.Itoa(secs), host}
	}

	out, err := exec.CommandContext(ctx, "ping", args...).CombinedOutput()
	res := PingResult{Output: strings.TrimSpace(string(out))}
	if m := pingStats.FindStringSubmatch(res.Output); m != nil {
		res.Sent, _ = strconv.Atoi(m[1])
		res.Recv, _ = strconv.Atoi(m[2])
	}
	// A non-zero exit (unreachable / 100% loss) is a valid callback outcome,
	// not a runtime failure: return the parsed result with a nil error so the
	// caller reports ok based on packets received. Only surface err when ping
	// could not run at all (binary missing, context cancelled).
	if err != nil && res.Sent == 0 {
		return res, err
	}
	return res, nil
}
