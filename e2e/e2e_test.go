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

// withAuth attaches the Bearer token from envToken when the target has auth
// enabled (token set). A no-op against an auth-less backend, so the same suite
// works both ways.
func withAuth(req *http.Request) *http.Request {
	if tok := os.Getenv(envToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req
}

// authGet issues a GET with the auth token attached when configured.
func authGet(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	return client.Do(withAuth(req))
}

// authPost issues a POST with the auth token attached when configured.
func authPost(t *testing.T, client *http.Client, url, ctype string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("new request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", ctype)
	return client.Do(withAuth(req))
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
			resp, err := authGet(t, client, url)
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
			// /status is auth-gated; probes are exempt. authGet attaches the
			// token when configured, so all pass whether auth is on or off.
			resp, err := authGet(t, client, url)
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
	resp, err := authGet(t, httpClient(), url)
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
	resp, err := authGet(t, httpClient(), url)
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
	resp, err := authPost(t, httpClient(), url, "text/plain", strings.NewReader("marco"))
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

func TestHTTPTarget(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	url := "http://" + a + "/target"
	resp, err := authGet(t, httpClient(), url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var got struct {
		Target        string `json:"target"`
		DestinationIP string `json:"destination_ip"`
		Interfaces    []struct {
			Name      string   `json:"name"`
			Addresses []string `json:"addresses"`
		} `json:"interfaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if got.Target == "" {
		t.Errorf("target name empty")
	}
	if got.DestinationIP == "" {
		t.Errorf("destination_ip empty")
	}
	if len(got.Interfaces) == 0 {
		t.Errorf("no interfaces reported")
	}
}

// envToken carries a Bearer token for /callback when the target has auth enabled
// (the e2e task extracts it from the container logs). Empty ⇒ no auth header.
const envToken = "TARGET_E2E_TOKEN"

// postCallback fires POST /callback expecting a successful (200) egress result.
func postCallback(t *testing.T, httpAddr, spec string) map[string]any {
	t.Helper()
	return postCallbackExpect(t, httpAddr, spec, http.StatusOK)
}

// postCallbackExpect fires POST /callback, asserts the HTTP status, and decodes
// the result. The Bearer token from envToken is attached when set. /callback
// reflects the egress outcome in the status (200 ok / 502 fail / 504 timeout).
func postCallbackExpect(t *testing.T, httpAddr, spec string, wantStatus int) map[string]any {
	t.Helper()
	url := "http://" + httpAddr + "/callback"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(spec))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv(envToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("POST %s: status %d, want %d", url, resp.StatusCode, wantStatus)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode callback result: %v", err)
	}
	return out
}

func TestHTTPCallbackHTTP(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	// The server calls back to its own HTTP endpoint (egress -> ingress). That
	// ingress route is auth-gated, so the spec carries the token in its own
	// request headers when auth is enabled.
	hdrs := ""
	if tok := os.Getenv(envToken); tok != "" {
		hdrs = `,"headers":{"Authorization":"Bearer ` + tok + `"}`
	}
	spec := `{"kind":"http","url":"http://` + a + `/generate_204"` + hdrs + `}`
	res := postCallback(t, a, spec)
	if res["ok"] != true {
		t.Fatalf("http callback not ok: %v", res)
	}
	if status, _ := res["status"].(float64); int(status) != 204 {
		t.Fatalf("http callback status = %v, want 204", res["status"])
	}
}

func TestHTTPCallbackTCP(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	tcpAddr := addr(t, envTCP, defTCP)
	host, port, err := net.SplitHostPort(tcpAddr)
	if err != nil {
		t.Fatalf("split %s: %v", tcpAddr, err)
	}
	spec := `{"kind":"tcp","host":"` + host + `","port":` + port + `,"data":"cb-e2e"}`
	res := postCallback(t, a, spec)
	if res["ok"] != true || res["response"] != "cb-e2e" {
		t.Fatalf("tcp callback = %v, want ok + echoed data", res)
	}
}

func TestHTTPCallbackPing(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	host, _, err := net.SplitHostPort(a)
	if err != nil {
		host = a
	}
	spec := `{"kind":"ping","host":"` + host + `","count":1}`
	res := postCallback(t, a, spec)
	if res["ok"] != true {
		t.Fatalf("ping callback not ok (needs ping binary + ICMP): %v", res)
	}
}

func TestHTTPCallbackErrors(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	url := "http://" + a + "/callback"
	tok := os.Getenv(envToken)
	authed := func(req *http.Request) *http.Request {
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return req
	}
	// authenticated GET -> 405
	getReq, _ := http.NewRequest(http.MethodGet, url, nil)
	if resp, err := httpClient().Do(authed(getReq)); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET /callback: status %d, want 405", resp.StatusCode)
		}
	} else {
		t.Errorf("GET /callback: %v", err)
	}
	// authenticated malformed body -> 400
	postReq, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("not json"))
	postReq.Header.Set("Content-Type", "application/json")
	if resp, err := httpClient().Do(authed(postReq)); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST malformed: status %d, want 400", resp.StatusCode)
		}
	} else {
		t.Errorf("POST malformed: %v", err)
	}
}

func TestHTTPCallbackFailureStatus(t *testing.T) {
	a := addr(t, envHTTP, defHTTP)
	// Egress to a refused port -> ok:false, reflected as HTTP 502.
	res := postCallbackExpect(t, a, `{"kind":"tcp","host":"127.0.0.1","port":1}`, http.StatusBadGateway)
	if res["ok"] != false {
		t.Fatalf("expected ok:false for refused connection, got %v", res)
	}
}

// TestHTTPCallbackAuth verifies auth enforcement when a token is configured
// (i.e. the target has auth enabled). Skipped against an auth-less backend.
func TestHTTPCallbackAuth(t *testing.T) {
	if os.Getenv(envToken) == "" {
		t.Skipf("%s not set; target has no auth", envToken)
	}
	a := addr(t, envHTTP, defHTTP)
	url := "http://" + a + "/callback"
	spec := `{"kind":"ping","host":"127.0.0.1","count":1}`
	// No token -> 401.
	if resp, err := httpClient().Post(url, "application/json", strings.NewReader(spec)); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no-token POST /callback: status %d, want 401", resp.StatusCode)
		}
	} else {
		t.Errorf("no-token POST: %v", err)
	}
	// Bad token -> 401.
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(spec))
	req.Header.Set("Authorization", "Bearer garbage")
	if resp, err := httpClient().Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("bad-token POST /callback: status %d, want 401", resp.StatusCode)
		}
	} else {
		t.Errorf("bad-token POST: %v", err)
	}
}

// TestHTTPGlobalAuth verifies auth gates non-callback routes too, while
// health/liveness probes stay open. Skipped against an auth-less backend.
func TestHTTPGlobalAuth(t *testing.T) {
	tok := os.Getenv(envToken)
	if tok == "" {
		t.Skipf("%s not set; target has no auth", envToken)
	}
	a := addr(t, envHTTP, defHTTP)

	// Non-callback route: 401 without a token.
	statusURL := "http://" + a + "/status"
	if resp, err := httpClient().Get(statusURL); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no-token GET /status: status %d, want 401", resp.StatusCode)
		}
	} else {
		t.Errorf("no-token GET /status: %v", err)
	}

	// Non-callback route: 200 with a valid token.
	req, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := httpClient().Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("token GET /status: status %d, want 200", resp.StatusCode)
		}
	} else {
		t.Errorf("token GET /status: %v", err)
	}

	// Health probe: open without a token.
	if resp, err := httpClient().Get("http://" + a + "/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("no-token GET /healthz: status %d, want 200 (exempt)", resp.StatusCode)
		}
	} else {
		t.Errorf("no-token GET /healthz: %v", err)
	}
}

func TestHTTPSGenerate(t *testing.T) {
	a := addr(t, envHTTPS, defHTTPS)
	url := "https://" + a + "/generate_500"
	resp, err := authGet(t, httpClient(), url)
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
