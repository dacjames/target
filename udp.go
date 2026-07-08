package main

import (
	"context"
	"net"
)

// startUDP binds a UDP socket and echoes (or drains) datagrams until ctx is
// cancelled. The returned conn is registered for shutdown by the caller.
func startUDP(ctx context.Context, lg *logger, name string, t *UDPTarget) (net.PacketConn, error) {
	addr, err := resolveAddr(t.Listen, t.Port)
	if err != nil {
		return nil, err
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	echo := echoEnabled(t.UseEcho)
	lg.infof("target %q udp listening on %s (echo=%t)", name, conn.LocalAddr(), echo)

	go func() {
		buf := make([]byte, 65535)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-ctx.Done():
					return // expected on shutdown
				default:
					lg.warnf("target %q udp read: %v", name, err)
					return
				}
			}
			lg.debugf("target %q udp %d bytes from %s", name, n, src)
			if echo {
				if _, err := conn.WriteToUDP(buf[:n], src); err != nil {
					lg.warnf("target %q udp write to %s: %v", name, src, err)
				}
			}
		}
	}()
	return conn, nil
}
