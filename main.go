package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	envConfig = "TARGET_CONFIG"
	envLog    = "TARGET_LOG"

	defaultConfig = "targets.json"
	shutdownDrain = 10 * time.Second
)

func main() {
	os.Exit(run())
}

func run() int {
	lg := newLogger(os.Getenv(envLog))

	configPath := os.Getenv(envConfig)
	if configPath == "" {
		configPath = defaultConfig
	}

	targets, err := loadConfig(configPath)
	if err != nil {
		lg.errorf("config: %v", err)
		return 1
	}
	if len(targets) == 0 {
		lg.errorf("config %q defines no targets", configPath)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var closers []io.Closer    // listeners / packet conns
	var servers []*http.Server // http(s) servers
	started := 0

	for _, t := range targets {
		switch t.Type {
		case "tcp":
			ln, err := startTCP(ctx, lg, t.Name, t.TCP)
			if err != nil {
				lg.errorf("target %q tcp: %v (skipping)", t.Name, err)
				continue
			}
			closers = append(closers, ln)
		case "udp":
			conn, err := startUDP(ctx, lg, t.Name, t.UDP)
			if err != nil {
				lg.errorf("target %q udp: %v (skipping)", t.Name, err)
				continue
			}
			closers = append(closers, conn)
		case "http", "https":
			srv, err := startHTTP(lg, t.Name, t.HTTP)
			if err != nil {
				lg.errorf("target %q http: %v (skipping)", t.Name, err)
				continue
			}
			servers = append(servers, srv)
		default:
			lg.warnf("target %q: unknown type %q (skipping)", t.Name, t.Type)
			continue
		}
		started++
	}

	if started == 0 {
		lg.errorf("no targets started")
		return 1
	}
	lg.infof("%d/%d targets started; press Ctrl-C to stop", started, len(targets))

	<-ctx.Done()
	stop() // restore default signal handling; a second signal now force-quits
	lg.infof("shutting down")

	// Close raw listeners/conns, unblocking their accept/read loops.
	for _, c := range closers {
		c.Close()
	}

	// Gracefully drain HTTP servers.
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(shutCtx); err != nil {
			lg.warnf("server shutdown: %v", err)
			srv.Close()
		}
	}

	lg.infof("stopped")
	return 0
}
