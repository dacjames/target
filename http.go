package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	generatePrefix = "/generate_"
	delayPrefix    = "/delay/"
	bytesPrefix    = "/bytes/"

	maxDelay    = 60 * time.Second // cap for /delay to avoid hung sockets
	maxBytes    = 10 << 20         // 10 MiB cap for /bytes
	maxEchoBody = 1 << 20          // 1 MiB cap on reflected request bodies
)

// startTime anchors the /status uptime report.
var startTime = time.Now()

// listenerInfo is the per-listener metadata surfaced by /target.
type listenerInfo struct {
	Name      string // target name from targets.json
	BindIP    string // resolved host this listener bound to (e.g. "0.0.0.0")
	Port      int
	Interface string // configured interface name, if any
}

// startHTTP binds and serves an HTTP or HTTPS target. The returned server is
// registered for graceful shutdown by the caller.
func startHTTP(lg *logger, name string, t *HTTPTarget) (*http.Server, error) {
	addr, err := resolveAddr(t.Listen, t.Port)
	if err != nil {
		return nil, err
	}

	bindHost, _, _ := net.SplitHostPort(addr)
	info := listenerInfo{Name: name, BindIP: bindHost, Port: t.Port, Interface: t.Listen.Interface}
	srv := &http.Server{Addr: addr, Handler: handler(lg, info)}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	https := t.Cert != nil
	if https {
		cert, err := certFor(t.Cert)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("target %q tls: %w", name, err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	scheme := "http"
	if https {
		scheme = "https"
	}
	lg.infof("target %q %s listening on %s", name, scheme, ln.Addr())

	go func() {
		var err error
		if https {
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			lg.warnf("target %q %s serve: %v", name, scheme, err)
		}
	}()
	return srv, nil
}

// handler dispatches by path. A single handler (not ServeMux routes) is used
// because several routes are prefix matches (/generate_, /delay/, /bytes/),
// which ServeMux only supports for patterns ending in "/". Unmatched → 404.
func handler(lg *logger, info listenerInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		lg.debugf("target %q %s %s from %s", info.Name, r.Method, path, r.RemoteAddr)
		switch {
		// Prefix routes (ServeMux only prefix-matches trailing-slash patterns,
		// so dispatch by hand).
		case strings.HasPrefix(path, generatePrefix):
			generate(w, r)
		case strings.HasPrefix(path, delayPrefix):
			delay(w, r)
		case strings.HasPrefix(path, bytesPrefix):
			serveBytes(w, r)

		// Health / liveness — plain 200.
		case path == "/" || path == "/healthz" || path == "/livez" || path == "/readyz":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		case path == "/ping":
			fmt.Fprintln(w, "pong")
		case path == "/status":
			status(w)
		case path == "/target":
			targetInfo(w, r, info)
		case path == "/callback":
			callback(w, r)

		// Reflection.
		case path == "/echo":
			echo(w, r)
		case path == "/headers":
			writeJSON(w, http.StatusOK, map[string]any{"headers": flattenHeaders(r.Header)})
		case path == "/ip":
			writeJSON(w, http.StatusOK, map[string]any{"origin": clientIP(r)})

		default:
			http.NotFound(w, r)
		}
	}
}

// generate serves /generate_<code>, forcing the given HTTP status code.
func generate(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, generatePrefix)
	code, err := strconv.Atoi(suffix)
	if err != nil || code < 100 || code > 599 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "invalid status code %q\n", suffix)
		return
	}
	w.WriteHeader(code)
	fmt.Fprintf(w, "%d %s\n", code, http.StatusText(code))
}

// delay serves /delay/<seconds>, sleeping (capped at maxDelay) then 200.
// Fractional seconds allowed; the sleep aborts early if the client disconnects.
func delay(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, delayPrefix)
	secs, err := strconv.ParseFloat(suffix, 64)
	if err != nil || secs < 0 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "invalid delay %q\n", suffix)
		return
	}
	d := time.Duration(secs * float64(time.Second))
	if d > maxDelay {
		d = maxDelay
	}
	select {
	case <-time.After(d):
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "slept %s\n", d)
	case <-r.Context().Done():
		return // client gave up
	}
}

// serveBytes serves /bytes/<n>, returning n bytes (capped at maxBytes) of a
// deterministic filler payload.
func serveBytes(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, bytesPrefix)
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 0 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "invalid byte count %q\n", suffix)
		return
	}
	if n > maxBytes {
		n = maxBytes
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(n))
	w.WriteHeader(http.StatusOK)
	const chunk = 32 << 10
	buf := make([]byte, chunk)
	for i := range buf {
		buf[i] = 'x'
	}
	for n > 0 {
		m := n
		if m > len(buf) {
			m = len(buf)
		}
		if _, err := w.Write(buf[:m]); err != nil {
			return
		}
		n -= m
	}
}

// ifaceInfo describes one network interface for the /target report.
type ifaceInfo struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
}

// targetInfo reports which local address the client actually reached
// (destination_ip) and the interface(s)/IP(s) this listener is bound on: all
// interfaces when bound to a wildcard address, otherwise the single interface
// owning the bind IP.
func targetInfo(w http.ResponseWriter, r *http.Request, info listenerInfo) {
	dest := ""
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if h, _, err := net.SplitHostPort(la.String()); err == nil {
			dest = h
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":         info.Name,
		"destination_ip": dest,
		"listener": map[string]any{
			"bind":      info.BindIP,
			"port":      info.Port,
			"interface": info.Interface,
			"wildcard":  isWildcard(info.BindIP),
		},
		"interfaces": listenerInterfaces(info.BindIP),
	})
}

// isWildcard reports whether host is empty or an unspecified address
// (0.0.0.0 / ::), i.e. bound to every interface.
func isWildcard(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// listenerInterfaces returns the interfaces the listener is reachable on: every
// interface with an address when bound to a wildcard, otherwise just the one
// owning bindIP.
func listenerInterfaces(bindIP string) []ifaceInfo {
	wildcard := isWildcard(bindIP)
	var want net.IP
	if !wildcard {
		want = net.ParseIP(bindIP)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]ifaceInfo, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var ips []string
		owns := false
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ips = append(ips, ipnet.String())
			if want != nil && ipnet.IP.Equal(want) {
				owns = true
			}
		}
		if len(ips) == 0 {
			continue
		}
		if wildcard || owns {
			out = append(out, ifaceInfo{Name: ifc.Name, Addresses: ips})
		}
	}
	return out
}

// status reports a small liveness JSON with process uptime.
func status(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"uptime":     time.Since(startTime).Round(time.Second).String(),
		"uptime_sec": int64(time.Since(startTime).Seconds()),
	})
}

// echo reflects the request (method, path, query, headers, body, origin) back
// as JSON — useful for debugging what a client or proxy actually sent.
func echo(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxEchoBody))
	writeJSON(w, http.StatusOK, map[string]any{
		"method":  r.Method,
		"path":    r.URL.Path,
		"query":   r.URL.Query(),
		"headers": flattenHeaders(r.Header),
		"origin":  clientIP(r),
		"body":    string(body),
	})
}

// flattenHeaders joins multi-valued headers for compact JSON output.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// clientIP returns the peer address (host portion of RemoteAddr).
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
