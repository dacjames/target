package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

const generatePrefix = "/generate_"

// startHTTP binds and serves an HTTP or HTTPS target. The returned server is
// registered for graceful shutdown by the caller.
func startHTTP(lg *logger, name string, t *HTTPTarget) (*http.Server, error) {
	addr, err := resolveAddr(t.Listen, t.Port)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Addr: addr, Handler: handler(lg, name)}

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

// handler dispatches by path: / → 200 OK, /generate_<code> → forced status,
// anything else → 404. A single handler (not ServeMux routes) is used because
// /generate_ is a prefix match, which ServeMux only supports for patterns
// ending in "/".
func handler(lg *logger, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, generatePrefix):
			generate(w, r, lg, name)
		case path == "/":
			lg.debugf("target %q GET / from %s", name, r.RemoteAddr)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "OK")
		default:
			lg.debugf("target %q 404 %s from %s", name, path, r.RemoteAddr)
			http.NotFound(w, r)
		}
	}
}

// generate serves /generate_<code>, forcing the given HTTP status code.
func generate(w http.ResponseWriter, r *http.Request, lg *logger, name string) {
	suffix := strings.TrimPrefix(r.URL.Path, generatePrefix)
	code, err := strconv.Atoi(suffix)
	if err != nil || code < 100 || code > 599 {
		lg.debugf("target %q bad generate path %q from %s", name, r.URL.Path, r.RemoteAddr)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "invalid status code %q\n", suffix)
		return
	}
	lg.debugf("target %q generate %d from %s", name, code, r.RemoteAddr)
	w.WriteHeader(code)
	fmt.Fprintf(w, "%d %s\n", code, http.StatusText(code))
}
