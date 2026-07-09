#!/usr/bin/env bash
# smoke.sh — build the service, boot it on a private set of high ports via
# TARGET_CONFIG_JSON, exercise every HTTP path plus TCP/UDP echo, tear down.
# Exits non-zero on the first failed assertion. Used by `task smoke`.
set -euo pipefail

cd "$(dirname "$0")/.."

BIN="$(mktemp -t target.XXXXXX)"
LOG="$(mktemp -t target-smoke.XXXXXX)"

# Distinct ports so a smoke run never collides with a dev instance.
HTTP=28090   # wildcard bind (0.0.0.0)
LO=28091     # loopback-only bind (127.0.0.1)
HTTPS=28453  # wildcard TLS
TCP=29191    # tcp echo
UDP=29153    # udp echo

CONFIG=$(cat <<JSON
{
  "http":  {"http": {"listen": {"ip": "0.0.0.0"},   "port": $HTTP,  "cert": null}},
  "lo":    {"http": {"listen": {"ip": "127.0.0.1"}, "port": $LO,    "cert": null}},
  "https": {"http": {"listen": {"ip": "0.0.0.0"},   "port": $HTTPS, "cert": {"hostname": "localhost"}}},
  "tcp":   {"tcp":  {"listen": {"ip": "127.0.0.1"}, "port": $TCP}},
  "udp":   {"udp":  {"listen": {"ip": "127.0.0.1"}, "port": $UDP}}
}
JSON
)

pass=0
fail=0
SVC_PID=""

cleanup() {
  [ -n "$SVC_PID" ] && kill "$SVC_PID" 2>/dev/null || true
  rm -f "$BIN" "$LOG"
}
trap cleanup EXIT

ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }

# check_status <label> <expected-code> <url> [curl-args...]
check_status() {
  local label=$1 want=$2 url=$3; shift 3
  local got
  got=$(curl -sk -o /dev/null -w '%{http_code}' "$@" "$url" || echo 000)
  [ "$got" = "$want" ] && ok "$label ($got)" || bad "$label: got $got want $want"
}

# check_contains <label> <substring> <url> [curl-args...]
check_contains() {
  local label=$1 want=$2 url=$3; shift 3
  local body
  body=$(curl -sk "$@" "$url" || true)
  case "$body" in
    *"$want"*) ok "$label" ;;
    *)         bad "$label: %q missing in response: ${body:0:120}" ; printf '        wanted: %s\n' "$want" ;;
  esac
}

echo "==> build"
go build -o "$BIN" .

echo "==> boot"
TARGET_CONFIG_JSON="$CONFIG" TARGET_LOG=warn "$BIN" >"$LOG" 2>&1 &
SVC_PID=$!

# Wait for readiness (or fail fast if the process died).
for _ in $(seq 1 50); do
  if ! kill -0 "$SVC_PID" 2>/dev/null; then
    echo "service exited during startup:"; cat "$LOG"; exit 1
  fi
  if curl -s -o /dev/null "http://127.0.0.1:$HTTP/healthz"; then break; fi
  sleep 0.1
done

echo "==> health"
check_status  "GET /"        200 "http://127.0.0.1:$HTTP/"
check_status  "GET /healthz" 200 "http://127.0.0.1:$HTTP/healthz"
check_status  "GET /livez"   200 "http://127.0.0.1:$HTTP/livez"
check_status  "GET /readyz"  200 "http://127.0.0.1:$HTTP/readyz"
check_contains "GET /ping"   "pong"          "http://127.0.0.1:$HTTP/ping"
check_contains "GET /status" '"status": "ok"' "http://127.0.0.1:$HTTP/status"

echo "==> generate"
check_status "GET /generate_404" 404 "http://127.0.0.1:$HTTP/generate_404"
check_status "GET /generate_503" 503 "http://127.0.0.1:$HTTP/generate_503"
check_status "GET /generate_xyz" 400 "http://127.0.0.1:$HTTP/generate_xyz"
check_status "GET /nope (404)"   404 "http://127.0.0.1:$HTTP/nope"

echo "==> behavior"
start=$(date +%s%N)
check_status "GET /delay/1" 200 "http://127.0.0.1:$HTTP/delay/1"
elapsed_ms=$(( ($(date +%s%N) - start) / 1000000 ))
[ "$elapsed_ms" -ge 900 ] && ok "delay honored (${elapsed_ms}ms)" || bad "delay too fast (${elapsed_ms}ms)"
n=$(curl -s "http://127.0.0.1:$HTTP/bytes/2048" | wc -c | tr -d ' ')
[ "$n" = "2048" ] && ok "bytes length ($n)" || bad "bytes: got $n want 2048"
check_contains "POST /echo body" '"body": "marco"'  "http://127.0.0.1:$HTTP/echo" -X POST -d marco
check_contains "POST /echo method" '"method": "POST"' "http://127.0.0.1:$HTTP/echo" -X POST -d marco

echo "==> target info"
check_contains "GET /target destination_ip" '"destination_ip": "127.0.0.1"' "http://127.0.0.1:$HTTP/target"
check_contains "GET /target wildcard=true"   '"wildcard": true'  "http://127.0.0.1:$HTTP/target"
check_contains "GET /target interfaces"      '"name":'           "http://127.0.0.1:$HTTP/target"
check_contains "GET /target (lo) wildcard=false" '"wildcard": false'  "http://127.0.0.1:$LO/target"
check_contains "GET /target (lo) bind"           '"bind": "127.0.0.1"' "http://127.0.0.1:$LO/target"

echo "==> https"
check_contains "GET https /"          "OK" "https://127.0.0.1:$HTTPS/"
check_status   "GET https /generate_500" 500 "https://127.0.0.1:$HTTPS/generate_500"

echo "==> tcp/udp echo"
tcp_reply=$(printf 'ping-tcp' | nc -w1 127.0.0.1 "$TCP" || true)
[ "$tcp_reply" = "ping-tcp" ] && ok "tcp echo" || bad "tcp echo: got '$tcp_reply'"
udp_reply=$(printf 'ping-udp' | nc -u -w1 127.0.0.1 "$UDP" || true)
[ "$udp_reply" = "ping-udp" ] && ok "udp echo" || bad "udp echo: got '$udp_reply'"

echo
echo "==> $pass passed, $fail failed"
[ "$fail" -eq 0 ]
