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

echo "==> boot (auth enabled)"
# Auth on so /callback is reachable and we can exercise the JWT path. TARGET_LOG
# must be info so the startup token is logged for capture below.
TARGET_CONFIG_JSON="$CONFIG" TARGET_LOG=info TARGET_AUTH=true "$BIN" >"$LOG" 2>&1 &
SVC_PID=$!

# Wait for readiness (or fail fast if the process died).
for _ in $(seq 1 50); do
  if ! kill -0 "$SVC_PID" 2>/dev/null; then
    echo "service exited during startup:"; cat "$LOG"; exit 1
  fi
  if curl -s -o /dev/null "http://127.0.0.1:$HTTP/healthz"; then break; fi
  sleep 0.1
done

# Grab the JWT the service logged on startup.
TOKEN=$(grep 'auth token' "$LOG" | tail -1 | sed 's/.*: //')
[ -n "$TOKEN" ] && ok "startup token logged" || bad "no auth token in log"
AUTH=(-H "Authorization: Bearer $TOKEN")

# With auth enabled, health/liveness probes stay open but every other route
# needs the Bearer token — so most checks below pass "${AUTH[@]}".
echo "==> health (probes exempt, no token)"
check_status  "GET /"        200 "http://127.0.0.1:$HTTP/"
check_status  "GET /healthz" 200 "http://127.0.0.1:$HTTP/healthz"
check_status  "GET /livez"   200 "http://127.0.0.1:$HTTP/livez"
check_status  "GET /readyz"  200 "http://127.0.0.1:$HTTP/readyz"
check_contains "GET /ping"   "pong"          "http://127.0.0.1:$HTTP/ping"
check_contains "GET /status" '"status": "ok"' "http://127.0.0.1:$HTTP/status" "${AUTH[@]}"

echo "==> global auth gating (non-health routes need a token)"
check_status "GET /status no token -> 401" 401 "http://127.0.0.1:$HTTP/status"
check_status "GET /status token -> 200"    200 "http://127.0.0.1:$HTTP/status" "${AUTH[@]}"
check_status "GET /echo no token -> 401"   401 "http://127.0.0.1:$HTTP/echo"
check_status "GET /target no token -> 401" 401 "http://127.0.0.1:$HTTP/target"

echo "==> generate"
check_status "GET /generate_404" 404 "http://127.0.0.1:$HTTP/generate_404" "${AUTH[@]}"
check_status "GET /generate_503" 503 "http://127.0.0.1:$HTTP/generate_503" "${AUTH[@]}"
check_status "GET /generate_xyz" 400 "http://127.0.0.1:$HTTP/generate_xyz" "${AUTH[@]}"
check_status "GET /nope (404)"   404 "http://127.0.0.1:$HTTP/nope" "${AUTH[@]}"

echo "==> behavior"
start=$(date +%s%N)
check_status "GET /delay/1" 200 "http://127.0.0.1:$HTTP/delay/1" "${AUTH[@]}"
elapsed_ms=$(( ($(date +%s%N) - start) / 1000000 ))
[ "$elapsed_ms" -ge 900 ] && ok "delay honored (${elapsed_ms}ms)" || bad "delay too fast (${elapsed_ms}ms)"
n=$(curl -s "${AUTH[@]}" "http://127.0.0.1:$HTTP/bytes/2048" | wc -c | tr -d ' ')
[ "$n" = "2048" ] && ok "bytes length ($n)" || bad "bytes: got $n want 2048"
check_contains "POST /echo body" '"body": "marco"'  "http://127.0.0.1:$HTTP/echo" "${AUTH[@]}" -X POST -d marco
check_contains "POST /echo method" '"method": "POST"' "http://127.0.0.1:$HTTP/echo" "${AUTH[@]}" -X POST -d marco

echo "==> target info"
check_contains "GET /target destination_ip" '"destination_ip": "127.0.0.1"' "http://127.0.0.1:$HTTP/target" "${AUTH[@]}"
check_contains "GET /target wildcard=true"   '"wildcard": true'  "http://127.0.0.1:$HTTP/target" "${AUTH[@]}"
check_contains "GET /target interfaces"      '"name":'           "http://127.0.0.1:$HTTP/target" "${AUTH[@]}"
check_contains "GET /target (lo) wildcard=false" '"wildcard": false'  "http://127.0.0.1:$LO/target" "${AUTH[@]}"
check_contains "GET /target (lo) bind"           '"bind": "127.0.0.1"' "http://127.0.0.1:$LO/target" "${AUTH[@]}"

echo "==> callbacks (auth'd, self-referential egress)"
CB="http://127.0.0.1:$HTTP/callback"
# The self-referential http egress re-enters a gated route, so the spec carries
# the Bearer token in its own request headers.
CB_HTTP="{\"kind\":\"http\",\"url\":\"http://127.0.0.1:$HTTP/generate_204\",\"headers\":{\"Authorization\":\"Bearer $TOKEN\"}}"
check_contains "http callback ok"   '"ok": true'    "$CB" "${AUTH[@]}" -X POST -d "$CB_HTTP"
check_contains "http callback 204"  '"status": 204' "$CB" "${AUTH[@]}" -X POST -d "$CB_HTTP"
check_contains "tcp callback echo"  '"response": "cb"' "$CB" "${AUTH[@]}" -X POST -d "{\"kind\":\"tcp\",\"host\":\"127.0.0.1\",\"port\":$TCP,\"data\":\"cb\"}"
check_contains "udp callback echo"  '"response": "cb"' "$CB" "${AUTH[@]}" -X POST -d "{\"kind\":\"udp\",\"host\":\"127.0.0.1\",\"port\":$UDP,\"data\":\"cb\"}"
check_contains "ping callback ok"   '"ok": true'    "$CB" "${AUTH[@]}" -X POST -d '{"kind":"ping","host":"127.0.0.1","count":1}'
check_contains "callback failure body"   '"ok": false'  "$CB" "${AUTH[@]}" -X POST -d '{"kind":"tcp","host":"127.0.0.1","port":1}'
check_status   "callback failure -> 502"  502 "$CB" "${AUTH[@]}" -X POST -d '{"kind":"tcp","host":"127.0.0.1","port":1}'
check_status   "callback bad body -> 400" 400 "$CB" "${AUTH[@]}" -X POST -d 'not json'

echo "==> auth enforcement"
check_status "callback no token -> 401"  401 "$CB" -X POST -d '{"kind":"ping","host":"127.0.0.1"}'
check_status "callback bad token -> 401" 401 "$CB" -H "Authorization: Bearer garbage" -X POST -d '{"kind":"ping","host":"127.0.0.1"}'
check_status "callback authed GET -> 405" 405 "$CB" "${AUTH[@]}"
# Non-/callback paths need no token even with auth enabled.
check_status "healthz needs no token" 200 "http://127.0.0.1:$HTTP/healthz"

echo "==> auth disabled -> /callback off"
# Second short-lived instance with auth off: /callback must 404.
NOAUTH_PORT=28099
TARGET_CONFIG_JSON="{\"h\":{\"http\":{\"listen\":{\"ip\":\"127.0.0.1\"},\"port\":$NOAUTH_PORT,\"cert\":null}}}" \
  TARGET_LOG=warn "$BIN" >"${LOG}.noauth" 2>&1 &
NOAUTH_PID=$!
for _ in $(seq 1 50); do
  curl -s -o /dev/null "http://127.0.0.1:$NOAUTH_PORT/healthz" && break || sleep 0.1
done
check_status "callback disabled -> 404" 404 "http://127.0.0.1:$NOAUTH_PORT/callback" -X POST -d '{"kind":"ping","host":"127.0.0.1"}'
check_status "healthz still open"        200 "http://127.0.0.1:$NOAUTH_PORT/healthz"
kill "$NOAUTH_PID" 2>/dev/null || true
rm -f "${LOG}.noauth"

echo "==> https"
check_contains "GET https /"          "OK" "https://127.0.0.1:$HTTPS/"
check_status   "GET https /generate_500" 500 "https://127.0.0.1:$HTTPS/generate_500" "${AUTH[@]}"

echo "==> tcp/udp echo"
tcp_reply=$(printf 'ping-tcp' | nc -w1 127.0.0.1 "$TCP" || true)
[ "$tcp_reply" = "ping-tcp" ] && ok "tcp echo" || bad "tcp echo: got '$tcp_reply'"
udp_reply=$(printf 'ping-udp' | nc -u -w1 127.0.0.1 "$UDP" || true)
[ "$udp_reply" = "ping-udp" ] && ok "udp echo" || bad "udp echo: got '$udp_reply'"

echo
echo "==> $pass passed, $fail failed"
[ "$fail" -eq 0 ]
