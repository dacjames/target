#!/usr/bin/env bash
# ping-check.sh — verify the ICMP callback across every TARGET_PINGER impl:
#   socket — unprivileged ICMP datagram socket (no root / CAP_NET_RAW)
#   system — shell out to /bin/ping
#   auto   — socket first, /bin/ping fallback (the default)
# Boots the service (auth on so /callback is reachable), pings 127.0.0.1, and
# asserts both that the callback is ok and that the impl that ran is the one
# selected — distinguished by the `output` field text. Used by `task ping:check`.
set -euo pipefail

cd "$(dirname "$0")/.."

BIN="$(mktemp -t target.XXXXXX)"
PORT=28190

pass=0
fail=0
SVC_PID=""

cleanup() {
  [ -n "$SVC_PID" ] && kill "$SVC_PID" 2>/dev/null || true
  rm -f "$BIN" "$BIN".log
}
trap cleanup EXIT

ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }

echo "==> build"
go build -o "$BIN" .

# check_impl <impl> <output-substring-that-proves-that-impl-ran>
check_impl() {
  local impl=$1 want=$2
  local log="$BIN.log"
  TARGET_CONFIG_JSON="{\"h\":{\"http\":{\"listen\":{\"ip\":\"127.0.0.1\"},\"port\":$PORT,\"cert\":null}}}" \
    TARGET_LOG=info TARGET_AUTH=true TARGET_PINGER="$impl" "$BIN" >"$log" 2>&1 &
  SVC_PID=$!

  for _ in $(seq 1 50); do
    if ! kill -0 "$SVC_PID" 2>/dev/null; then echo "service died:"; cat "$log"; exit 1; fi
    curl -s -o /dev/null "http://127.0.0.1:$PORT/healthz" && break || sleep 0.1
  done

  local token body
  token=$(grep 'auth token' "$log" | tail -1 | sed 's/.*: //')
  body=$(curl -sk -H "Authorization: Bearer $token" -X POST \
    "http://127.0.0.1:$PORT/callback" \
    -d '{"kind":"ping","host":"127.0.0.1","count":2}' || true)

  case "$body" in
    *'"ok": true'*) ok "TARGET_PINGER=$impl callback ok" ;;
    *)              bad "TARGET_PINGER=$impl not ok: ${body:0:160}" ;;
  esac
  case "$body" in
    *"$want"*) ok "TARGET_PINGER=$impl used the expected impl (output ~ '$want')" ;;
    *)         bad "TARGET_PINGER=$impl wrong impl; output missing '$want': ${body:0:200}" ;;
  esac

  kill "$SVC_PID" 2>/dev/null || true
  wait "$SVC_PID" 2>/dev/null || true
  SVC_PID=""
}

echo "==> socket (unprivileged ICMP datagram socket)"
check_impl socket "icmp socket"

echo "==> system (/bin/ping)"
check_impl system "packets transmitted"

echo "==> auto (socket primary; expect the socket path on this host)"
check_impl auto "icmp socket"

echo
echo "==> $pass passed, $fail failed"
[ "$fail" -eq 0 ]
