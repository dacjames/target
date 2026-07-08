package main

import (
	"context"
	"io"
	"net"
)

// startTCP binds a TCP listener and serves echo (or drain) connections until
// ctx is cancelled. The returned listener is registered for shutdown by the
// caller.
func startTCP(ctx context.Context, lg *logger, name string, t *TCPTarget) (net.Listener, error) {
	addr, err := resolveAddr(t.Listen, t.Port)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	echo := echoEnabled(t.UseEcho)
	lg.infof("target %q tcp listening on %s (echo=%t)", name, ln.Addr(), echo)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return // expected on shutdown
				default:
					lg.warnf("target %q tcp accept: %v", name, err)
					return
				}
			}
			go handleTCPConn(lg, name, conn, echo)
		}
	}()
	return ln, nil
}

func handleTCPConn(lg *logger, name string, conn net.Conn, echo bool) {
	defer conn.Close()
	lg.debugf("target %q tcp conn from %s", name, conn.RemoteAddr())
	if echo {
		io.Copy(conn, conn) // echo until peer closes
	} else {
		io.Copy(io.Discard, conn) // accept + drain
	}
	lg.debugf("target %q tcp conn closed %s", name, conn.RemoteAddr())
}
