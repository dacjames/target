package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCallbackTimeout = 5 * time.Second
	maxCallbackTimeout     = 60 * time.Second
	maxCallbackSnippet     = 4 << 10 // 4 KiB of captured response body
)

// callbackSpec is the inline description of an outbound callback, decoded from
// the POST /callback request body. Fields are shared across kinds; only those
// relevant to `kind` are used.
type callbackSpec struct {
	Kind      string `json:"kind"`       // http | tcp | udp | ping
	TimeoutMS int    `json:"timeout_ms"` // default 5000, capped 60000

	// http
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body"`
	Insecure bool              `json:"insecure"` // skip TLS verification

	// tcp / udp
	Host string `json:"host"`
	Port int    `json:"port"`
	Data string `json:"data"` // optional payload to send

	// ping
	Count int `json:"count"`
}

// callbackResult is the JSON outcome of a callback. Kind-specific fields are
// omitempty so each result stays compact.
type callbackResult struct {
	Kind      string `json:"kind"`
	Target    string `json:"target"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`

	Status int `json:"status,omitempty"` // http

	BytesSent int    `json:"bytes_sent,omitempty"`     // tcp/udp
	BytesRecv int    `json:"bytes_received,omitempty"` // tcp/udp
	Response  string `json:"response,omitempty"`       // tcp/udp reply snippet

	PacketsSent int    `json:"packets_sent,omitempty"`     // ping
	PacketsRecv int    `json:"packets_received,omitempty"` // ping
	Output      string `json:"output,omitempty"`           // ping raw output
}

func (s callbackSpec) timeout() time.Duration {
	if s.TimeoutMS <= 0 {
		return defaultCallbackTimeout
	}
	d := time.Duration(s.TimeoutMS) * time.Millisecond
	if d > maxCallbackTimeout {
		return maxCallbackTimeout
	}
	return d
}

// runCallback executes the described outbound callback and returns its result.
// Egress failures are reported in the result (ok=false, error set), not as a
// Go error — the call itself succeeds in describing what happened.
func runCallback(ctx context.Context, spec callbackSpec) callbackResult {
	switch strings.ToLower(spec.Kind) {
	case "http":
		return httpCallback(ctx, spec)
	case "tcp":
		return streamCallback(ctx, spec, "tcp")
	case "udp":
		return streamCallback(ctx, spec, "udp")
	case "ping":
		return pingCallback(ctx, spec)
	default:
		return callbackResult{Kind: spec.Kind, OK: false, Error: fmt.Sprintf("unknown callback kind %q", spec.Kind)}
	}
}

func httpCallback(ctx context.Context, spec callbackSpec) callbackResult {
	res := callbackResult{Kind: "http", Target: spec.URL}
	if spec.URL == "" {
		res.Error = "url required"
		return res
	}
	method := strings.ToUpper(spec.Method)
	if method == "" {
		if spec.Body != "" {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}

	ctx, cancel := context.WithTimeout(ctx, spec.timeout())
	defer cancel()

	var body io.Reader
	if spec.Body != "" {
		body = strings.NewReader(spec.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, spec.URL, body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: spec.Insecure},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	start := time.Now()
	resp, err := client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxCallbackSnippet))
	res.Status = resp.StatusCode
	res.BytesRecv = len(snippet)
	res.Response = string(snippet)
	res.OK = true
	return res
}

// streamCallback handles tcp and udp callbacks: dial, optionally send data,
// optionally read a reply.
func streamCallback(ctx context.Context, spec callbackSpec, network string) callbackResult {
	addr := net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port))
	res := callbackResult{Kind: network, Target: addr}
	if spec.Host == "" || spec.Port <= 0 {
		res.Error = "host and port required"
		return res
	}

	timeout := spec.timeout()
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.Error = err.Error()
		return res
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	if spec.Data != "" {
		n, err := conn.Write([]byte(spec.Data))
		res.BytesSent = n
		if err != nil {
			res.LatencyMS = time.Since(start).Milliseconds()
			res.Error = err.Error()
			return res
		}
	}

	// Best-effort read of a reply. For UDP (connectionless) a missing reply is
	// not a failure; for TCP a closed connection likewise just ends the read.
	buf := make([]byte, maxCallbackSnippet)
	n, rerr := conn.Read(buf)
	res.LatencyMS = time.Since(start).Milliseconds()
	if n > 0 {
		res.BytesRecv = n
		res.Response = string(buf[:n])
	}
	// The dial (and any write) succeeded, so the callback is ok. A read
	// timeout / EOF with no data is expected for no-echo or one-way targets.
	res.OK = true
	if rerr != nil && n == 0 && !isExpectedReadEnd(rerr) {
		res.Error = rerr.Error()
	}
	return res
}

func isExpectedReadEnd(err error) bool {
	if err == io.EOF {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

func pingCallback(ctx context.Context, spec callbackSpec) callbackResult {
	res := callbackResult{Kind: "ping", Target: spec.Host}
	if spec.Host == "" {
		res.Error = "host required"
		return res
	}
	start := time.Now()
	pr, err := defaultPinger.Ping(ctx, spec.Host, spec.Count, spec.timeout())
	res.LatencyMS = time.Since(start).Milliseconds()
	res.PacketsSent = pr.Sent
	res.PacketsRecv = pr.Recv
	res.Output = pr.Output
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = pr.Recv > 0
	if !res.OK && res.Error == "" {
		res.Error = "no packets received"
	}
	return res
}

// callback handles POST /callback: decode the inline spec, run the outbound
// callback, and return its result JSON. Egress failures are reported in the
// body (ok=false) with HTTP 200 — only a bad request (wrong method, malformed
// body) uses a 4xx status.
func callback(w http.ResponseWriter, r *http.Request) {
	// Auth gates this endpoint and only this endpoint. Disabled auth => the
	// endpoint does not exist; enabled auth => a valid Bearer token is required.
	if authenticator == nil {
		http.NotFound(w, r)
		return
	}
	if err := authenticator.verifyRequest(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "unauthorized: %v\n", err)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintln(w, "POST required")
		return
	}
	spec, err := decodeCallbackSpec(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "invalid callback spec: %v\n", err)
		return
	}
	res := runCallback(r.Context(), spec)
	writeJSON(w, http.StatusOK, res)
}

// decodeCallbackSpec reads and decodes a callback spec from a request body,
// capping the read to guard against oversized payloads.
func decodeCallbackSpec(r io.Reader) (callbackSpec, error) {
	var spec callbackSpec
	raw, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return spec, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return spec, fmt.Errorf("empty request body")
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, err
	}
	return spec, nil
}
