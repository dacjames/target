//go:build e2e

// Package e2e is a black-box test suite that drives a running target service
// over the network. It is intentionally decoupled from the server code: every
// endpoint address comes from a TARGET_E2E_* environment variable, so the same
// suite runs against a local Docker container (the defaults below) or any
// deployed backend by overriding the vars, e.g.:
//
//	TARGET_E2E_HTTPS=prod.example.com:443 \
//	TARGET_E2E_HTTP= TARGET_E2E_TCP= TARGET_E2E_UDP= TARGET_E2E_TCP_NOECHO= \
//	    go test -tags e2e -v ./e2e/...
//
// An address set to the empty string skips its test(s), so you only exercise
// what a given deployment actually exposes.
//
// Build-tagged `e2e` so `go test ./...` (unit runs) never dials the network.
package e2e

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Environment variables and their local-Docker defaults.
const (
	envHTTP      = "TARGET_E2E_HTTP"       // plain HTTP host:port
	envHTTPS     = "TARGET_E2E_HTTPS"      // TLS HTTP host:port
	envTCP       = "TARGET_E2E_TCP"        // TCP echo host:port
	envTCPNoEcho = "TARGET_E2E_TCP_NOECHO" // TCP accept+drain host:port
	envUDP       = "TARGET_E2E_UDP"        // UDP echo host:port

	defHTTP      = "localhost:8081"
	defHTTPS     = "localhost:8443"
	defTCP       = "localhost:9091"
	defTCPNoEcho = "localhost:9090"
	defUDP       = "localhost:8053"

	dialTimeout = 3 * time.Second
	ioTimeout   = 3 * time.Second
)

// addr resolves an endpoint address from env (falling back to def), skipping
// the calling test when the result is empty.
func addr(t *testing.T, env, def string) string {
	t.Helper()
	v, ok := os.LookupEnv(env)
	if !ok {
		v = def
	}
	if v == "" {
		t.Skipf("%s not set; skipping", env)
	}
	return v
}

// httpClient trusts self-signed certs (test target ships one) and does not
// follow redirects, so /generate_3xx status codes are observed verbatim.
func httpClient() *http.Client {
	return &http.Client{
		Timeout: ioTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// TestMain best-effort waits for the HTTP endpoint to come up (container
// startup race) before running the suite. Non-fatal: individual tests assert.
func TestMain(m *testing.M) {
	waitReady()
	os.Exit(m.Run())
}

func waitReady() {
	a, ok := os.LookupEnv(envHTTP)
	if !ok {
		a = defHTTP
	}
	if a == "" {
		return
	}
	client := httpClient()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + a + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func TestHTTPRoot(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	assertRoot(t, "http://"+a+"/")
}

func TestHTTPSRoot(t *testing.T) {
	a := addr(t, envHTTPS, defHTTPS)
	assertRoot(t, "https://"+a+"/")
}

func assertRoot(t *testing.T, url string) {
	t.Helper()
	resp, err := httpClient().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != "OK\n" {
		t.Fatalf("GET %s: body %q, want %q", url, got, "OK\n")
	}
}

func TestHTTPGenerate(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	cases := []struct {
		path string
		want int
	}{
		{"/generate_200", 200},
		{"/generate_404", 404},
		{"/generate_500", 500},
		{"/generate_503", 503},
		{"/generate_xyz", 400}, // non-numeric -> bad request
		{"/generate_999", 400}, // out of 100-599 range
		{"/nope", 404},         // unmatched path
	}
	client := httpClient()
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			url := "http://" + a + c.path
			resp, err := client.Get(url)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("GET %s: status %d, want %d", url, resp.StatusCode, c.want)
			}
		})
	}
}

func TestHTTPHealth(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	client := httpClient()
	for _, p := range []string{"/healthz", "/livez", "/readyz", "/ping", "/status"} {
		t.Run(p, func(t *testing.T) {
			url := "http://" + a + p
			resp, err := client.Get(url)
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
			}
		})
	}
}

func TestHTTPDelay(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	start := time.Now()
	url := "http://" + a + "/delay/1"
	resp, err := httpClient().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("GET %s returned in %s, want >=~1s", url, elapsed)
	}
}

func TestHTTPBytes(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	const want = 2048
	url := "http://" + a + "/bytes/" + strconv.Itoa(want)
	resp, err := httpClient().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != want {
		t.Fatalf("GET %s: got %d bytes, want %d", url, len(body), want)
	}
}

func TestHTTPEcho(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	url := "http://" + a + "/echo"
	resp, err := httpClient().Post(url, "text/plain", strings.NewReader("marco"))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var got struct {
		Method string `json:"method"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if got.Method != "POST" || got.Body != "marco" {
		t.Fatalf("echo = %+v, want method=POST body=marco", got)
	}
}

func TestHTTPSGenerate(t *testing.T) {
	a := addr(t, envHTTPS, defHTTPS)
	url := "https://" + a + "/generate_500"
	resp, err := httpClient().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("GET %s: status %d, want 500", url, resp.StatusCode)
	}
}

func TestTCPEcho(t *testing.T) {
	a := addr(t, envTCP, defTCP)
	conn, err := net.DialTimeout("tcp", a, dialTimeout)
	if err != nil {
		t.Fatalf("dial %s: %v", a, err)
	}
	defer conn.Close()

	payload := []byte("hello-echo")
	conn.SetDeadline(time.Now().Add(ioTimeout))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(payload) {
		t.Fatalf("echo = %q, want %q", buf, payload)
	}
}

// TestTCPNoEchoAccepts verifies a no-echo TCP target still accepts and drains
// connections (e.g. a stub port that should look open to a scanner).
func TestTCPNoEchoAccepts(t *testing.T) {
	a := addr(t, envTCPNoEcho, defTCPNoEcho)
	conn, err := net.DialTimeout("tcp", a, dialTimeout)
	if err != nil {
		t.Fatalf("dial %s: %v", a, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write to no-echo target: %v", err)
	}
	// No assertion on a reply: the point is the socket accepts data.
}

func TestUDPEcho(t *testing.T) {
	a := addr(t, envUDP, defUDP)
	conn, err := net.DialTimeout("udp", a, dialTimeout)
	if err != nil {
		t.Fatalf("dial %s: %v", a, err)
	}
	defer conn.Close()

	payload := []byte("hello-udp")
	conn.SetDeadline(time.Now().Add(ioTimeout))
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if got := string(buf[:n]); got != string(payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
}
