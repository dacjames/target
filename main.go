package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	envConfig       = "TARGET_CONFIG"      // path to a targets.json file
	envConfigJSON   = "TARGET_CONFIG_JSON" // literal targets JSON (wins over the path)
	envLog          = "TARGET_LOG"
	envPinger       = "TARGET_PINGER"        // ping impl: auto (default) | socket | system
	envAuth         = "TARGET_AUTH"          // truthy enables JWT auth on /callback
	envAuthLifetime = "TARGET_AUTH_LIFETIME" // token lifetime (time.ParseDuration)

	defaultConfig       = "targets.json"
	defaultAuthLifetime = 4 * time.Hour
	shutdownDrain       = 10 * time.Second
)

// version is the build version, overridden at release time via
// -ldflags "-X main.version=<v>". Defaults to "dev" for local builds.
var version = "dev"

// truthy reports whether an env value enables a flag.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func main() {
	os.Exit(run())
}

func run() int {
	lg := newLogger(os.Getenv(envLog))
	lg.infof("target %s", version)

	// Log the config-controlling env vars so a running instance is
	// self-documenting.
	lg.infof("env %s=%q", envLog, os.Getenv(envLog))
	lg.infof("env %s=%q", envConfig, os.Getenv(envConfig))
	lg.infof("env %s set=%t", envConfigJSON, os.Getenv(envConfigJSON) != "")
	lg.infof("env %s=%q", envPinger, os.Getenv(envPinger))

	defaultPinger = selectPinger(os.Getenv(envPinger), lg)

	var (
		targets []target
		err     error
		source  string
		raw     []byte
	)
	if j := os.Getenv(envConfigJSON); j != "" {
		source = "$" + envConfigJSON
		raw = []byte(j)
	} else {
		source = os.Getenv(envConfig)
		if source == "" {
			source = defaultConfig
		}
		if raw, err = os.ReadFile(source); err != nil {
			lg.errorf("config: read %q: %v", source, err)
			return 1
		}
	}
	lg.infof("config source: %s", source)
	lg.infof("config: %s", compactJSON(raw))

	targets, err = parseConfig(raw, source)
	if err != nil {
		lg.errorf("config: %v", err)
		return 1
	}
	if len(targets) == 0 {
		lg.errorf("config %s defines no targets", source)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Optional JWT auth. When enabled it gates all HTTP/HTTPS routes (health
	// probes exempt) via authMiddleware; when disabled, /callback is turned off
	// (see http.go, callback.go).
	if truthy(os.Getenv(envAuth)) {
		lifetime := defaultAuthLifetime
		if v := os.Getenv(envAuthLifetime); v != "" {
			lifetime, err = time.ParseDuration(v)
			if err != nil || lifetime <= 0 {
				lg.errorf("auth: invalid %s=%q: %v", envAuthLifetime, v, err)
				return 1
			}
		}
		authenticator, err = newAuthenticator(lifetime)
		if err != nil {
			lg.errorf("auth: %v", err)
			return 1
		}
		lg.infof("auth enabled (lifetime %s); all HTTP routes require a Bearer token (health probes exempt); /callback mandatory", lifetime)
		go authenticator.rotate(ctx, lg)
	} else {
		lg.infof("auth disabled; /callback endpoint disabled")
	}

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
