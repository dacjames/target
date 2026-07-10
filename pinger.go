package main

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
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

// defaultPinger is the process-wide pinger used by ping callbacks.
var defaultPinger Pinger = systemPinger{}

// systemPinger shells out to the platform `ping` binary.
type systemPinger struct{}

// pingStats matches the "N packets transmitted, M received/packets received"
// summary line emitted by both Linux (iputils) and BSD/macOS ping.
var pingStats = regexp.MustCompile(`(\d+) packets transmitted, (\d+)`)

func (systemPinger) Ping(ctx context.Context, host string, count int, timeout time.Duration) (PingResult, error) {
	if count <= 0 {
		count = 1
	}
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}

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
