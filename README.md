# target

A generic **test target** service for network connectivity testing. Pure Go,
standard library plus `golang.org/x/net` (for unprivileged ICMP). Stands up an
arbitrary number of TCP/UDP/HTTP/HTTPS listeners from a declarative
`targets.json`.

## Install

**Docker** (multi-arch, amd64 + arm64):

```sh
docker run --rm -p 8081:8081 dacjames/target:1.2.0   # or :latest
```

**Prebuilt binary** — grab one from the
[latest release](https://github.com/dacjames/target/releases/latest)
(`linux`/`darwin`, `amd64`/`arm64`); verify against `SHA256SUMS`:

```sh
curl -sSLO https://github.com/dacjames/target/releases/download/v1.2.0/target_1.2.0_linux_amd64
chmod +x target_1.2.0_linux_amd64 && ./target_1.2.0_linux_amd64
```

> **macOS:** downloaded binaries are quarantined by Gatekeeper (they're ad-hoc
> signed, not notarized), so first strip the quarantine flag — otherwise you get
> "cannot be opened because the developer cannot be verified":
>
> ```sh
> xattr -d com.apple.quarantine target_1.2.0_darwin_arm64
> chmod +x target_1.2.0_darwin_arm64 && ./target_1.2.0_darwin_arm64
> ```
>
> Or right-click the binary in Finder → Open, once.

**From source:** `go build .` or `task go:build`.

## Features

- **TCP / UDP** listeners that echo whatever is sent (or accept-and-drain when
  `use_echo: false`).
- **HTTP / HTTPS** endpoints (see paths below).
- **HTTPS** with a supplied cert+key, or an auto-generated in-memory self-signed
  cert (zero setup).
- Listen on any number of protocols/ports, selected by IP or interface name.

### HTTP paths

| Path                       | Response                                                     |
| -------------------------- | ----------------------------------------------------------- |
| `/`                        | `200 OK`                                                     |
| `/healthz` `/livez` `/readyz` | `200 OK` (k8s-style health checks)                       |
| `/ping`                    | `pong`                                                       |
| `/status`                  | JSON: `{status, uptime, uptime_sec}`                        |
| `/target`                  | JSON: destination IP the client hit + this listener's interfaces/IPs |
| `/generate_<code>`         | forces that HTTP status (`/generate_404` → 404)             |
| `/delay/<seconds>`         | sleeps then `200` (fractional ok; capped at 60s)            |
| `/bytes/<n>`               | `n` bytes of body (capped at 10 MiB) — bandwidth testing    |
| `/echo`                    | reflects method + headers + body + origin as JSON           |
| `/headers` `/ip`           | reflect request headers / client IP                         |

Anything else → `404`. Bad path arguments → `400`.

### Callbacks (outbound / egress testing)

`POST /callback` makes the service open an **outbound** connection and reports the
result — the reverse of the listeners above. Useful for webhooks and networks with
**asymmetric ingress/egress** (reachable but can't reach out, or vice versa): you
hit ingress, the service exercises egress, you see whether egress works.

The callback is described entirely by the JSON request body (`kind` selects the
protocol). The HTTP status reflects the egress outcome so status-aware clients
(`curl --fail`, monitors, CI) react correctly, and the full result is always in
the JSON body:

| Egress outcome                                   | Status |
| ------------------------------------------------ | ------ |
| succeeded (`ok:true`)                            | `200`  |
| failed — refused / DNS / unreachable / 0 replies | `502`  |
| timed out                                        | `504`  |

A completed HTTP callback stays `ok:true`/`200` even if the **upstream** answered
5xx — you reached it; its status is in the body's `status`. Bad request (`400`),
missing/invalid token (`401`), wrong method (`405`), and auth-disabled (`404`)
are unchanged.

```sh
# http — call a webhook / another endpoint
curl -XPOST localhost:8081/callback \
  -d '{"kind":"http","method":"POST","url":"https://host/hook","headers":{"X-A":"b"},"body":"hi","insecure":true}'

# tcp / udp — connect to host:port, optionally send data, capture any reply
curl -XPOST localhost:8081/callback -d '{"kind":"tcp","host":"10.0.0.5","port":9091,"data":"ping"}'
curl -XPOST localhost:8081/callback -d '{"kind":"udp","host":"10.0.0.5","port":53,"data":"..."}'

# ping — ICMP echo (unprivileged ICMP socket; falls back to system ping)
curl -XPOST localhost:8081/callback -d '{"kind":"ping","host":"10.0.0.5","count":3}'
```

Common fields: `timeout_ms` (default 5000, cap 60000). Result fields by kind:
`status` (http); `bytes_sent`/`bytes_received`/`response` (tcp/udp);
`packets_sent`/`packets_received`/`output` (ping); plus `ok`, `latency_ms`, `error`.

> ⚠️ **SSRF by design.** `/callback` makes arbitrary outbound connections from the
> request body. This is a test target — do not expose it to untrusted callers.
> ICMP callbacks default to an **unprivileged ICMP datagram socket** (no root /
> CAP_NET_RAW; Linux gates it via `net.ipv4.ping_group_range`). If the socket
> can't be opened they fall back to the system `ping` binary (bundled in the
> Docker image via `iputils`). Force one impl with `TARGET_PINGER`.

#### Authentication (gating HTTP/HTTPS)

Auth is **off by default**. When on, it gates **every** route on the HTTP/HTTPS
listeners — health/liveness probes excepted, so orchestrators keep working.
`/callback` is special: its auth is **mandatory**, so it is disabled entirely
when auth is off.

| Route | Auth off | Auth on (`TARGET_AUTH=true`) |
| --- | --- | --- |
| Health probes (`/`, `/healthz`, `/livez`, `/readyz`, `/ping`) | open | **open** (exempt) |
| Everything else (`/status`, `/target`, `/echo`, `/generate_*`, …) | open | `401` without a valid `Authorization: Bearer <jwt>` |
| `/callback` | **`404` (disabled)** | `401` without a valid Bearer token |

The service issues its own tokens with an **ephemeral Ed25519 key** generated at
startup (EdDSA JWT, `sub=1`), logs one on boot, and logs a fresh one every
lifetime/2 so a valid token is always in recent logs. Tokens die on restart.

```sh
TARGET_AUTH=true task go:run
# ... log: auth token (expires 2026-07-10T20:00:00Z): eyJhbGciOiJFZERTQS...
curl -XPOST localhost:8081/callback -H "Authorization: Bearer eyJhbGc..." \
  -d '{"kind":"ping","host":"10.0.0.5","count":3}'
```

## Run

```sh
task go:run                    # uses ./targets.json
TARGET_LOG=debug task go:run   # verbose
TARGET_CONFIG=/etc/targets.json TARGET_LOG=info go run .
```

### Environment variables

| Var                  | Default        | Meaning                                          |
| -------------------- | -------------- | ------------------------------------------------ |
| `TARGET_CONFIG_JSON` | _(unset)_      | Literal targets JSON. Overrides `TARGET_CONFIG`. |
| `TARGET_CONFIG`      | `targets.json` | Path to the config file.                         |
| `TARGET_LOG`         | `info`         | Log level: debug, info, warn, error.             |
| `TARGET_PINGER`      | `auto`         | ICMP impl: `auto` (socket, fall back to ping), `socket`, or `system`. |
| `TARGET_AUTH`        | off            | `true`/`1`/`yes` enables JWT auth on all HTTP routes (health probes exempt; `/callback` mandatory). |
| `TARGET_AUTH_LIFETIME` | `4h`         | Token lifetime (`time.ParseDuration`). Rotates at lifetime/2. |

`TARGET_CONFIG_JSON` takes the same JSON as the file — handy when a file is
awkward (containers, secrets managers, CI):

```sh
TARGET_CONFIG_JSON='{"echo":{"tcp":{"port":9091}}}' task go:run
docker run --rm -p 9091:9091 -e TARGET_CONFIG_JSON='{"echo":{"tcp":{"port":9091}}}' target:latest
```

## targets.json

Structure: `{<target-name>: {<target-type>: {<params>}}}`. Type is one of
`tcp`, `udp`, `http` (HTTPS = an `http` target with a non-null `cert`).

```json
{
  "https-8443": {"http": {"listen": {"ip": "0.0.0.0"}, "port": 8443, "cert": {"hostname": "localhost"}}},
  "http-8081":  {"http": {"listen": {"ip": "0.0.0.0"}, "port": 8081, "cert": null}},
  "prom-stub":  {"tcp":  {"listen": {"ip": "0.0.0.0"}, "port": 9090, "use_echo": false}},
  "tcp-echo":   {"tcp":  {"listen": {"ip": "0.0.0.0"}, "port": 9091, "use_echo": true}},
  "dns-stub":   {"udp":  {"port": 8053, "use_echo": true}}
}
```

Parameters:

- `listen`: `{"ip": "0.0.0.0"}` or `{"interface": "eth0"}`. Omitted → `0.0.0.0`.
- `port`: TCP/UDP/HTTP port.
- `use_echo` (tcp/udp): echo data back (default `true`); `false` accepts + drains.
- `cert` (http): `null` for plain HTTP. For HTTPS, `{"hostname": "..."}` for a
  self-signed cert, or add `"cert": "/path/crt"` + `"key": "/path/key"` to load
  real files.

> **Privileged ports** (443/80/53) require root or `CAP_NET_BIND_SERVICE`. The
> shipped example uses high ports so it runs unprivileged.

## Verify

```sh
curl -s -o/dev/null -w '%{http_code}\n' localhost:8081/            # 200
curl -s -o/dev/null -w '%{http_code}\n' localhost:8081/generate_404  # 404
curl -sk https://localhost:8443/                                   # OK
printf ping | nc localhost 9091                                    # ping (tcp echo)
printf ping | nc -u -w1 localhost 8053                             # ping (udp echo)
```

## Docker

Multi-stage build on an Alpine base; the static binary ships with the example
`targets.json` (`TARGET_CONFIG=/app/targets.json`).

```sh
task docker:build   # docker build -t target:latest .
task docker:run     # run in foreground, ports published (Ctrl-C to stop)
task docker:stop    # remove a detached container
```

## e2e tests

Black-box suite in [e2e/](e2e/), build-tagged `e2e` so `go test ./...` never
touches the network. Every endpoint address comes from a `TARGET_E2E_*` env var
with local-Docker defaults, so the **same suite** runs against a container or a
deployed backend.

```sh
task e2e            # build image, run container, run suite against it, tear down
```

### Against a deployed backend

Override the addresses; set any you don't want to exercise to empty (that test
skips). TLS is dialed with `InsecureSkipVerify`, so self-signed targets pass.

```sh
TARGET_E2E_HTTP=probe.internal:8080 \
TARGET_E2E_HTTPS=probe.internal:443 \
TARGET_E2E_TCP= TARGET_E2E_TCP_NOECHO= TARGET_E2E_UDP= \
    task e2e:remote
```

| Var                     | Default          | Exercises                    |
| ----------------------- | ---------------- | ---------------------------- |
| `TARGET_E2E_HTTP`       | `localhost:8081` | `/`, `/generate_<code>`      |
| `TARGET_E2E_HTTPS`      | `localhost:8443` | TLS `/`, `/generate_<code>`  |
| `TARGET_E2E_TCP`        | `localhost:9091` | TCP echo                     |
| `TARGET_E2E_TCP_NOECHO` | `localhost:9090` | TCP accept+drain (open stub) |
| `TARGET_E2E_UDP`        | `localhost:8053` | UDP echo                     |

## Dev tasks (go-task)

```sh
task go:build   # go build ./...
task go:vet     # go vet ./...
task go:tests   # go test ./...  (unit; skips e2e)
task go:fmt     # gofmt -l -w .
task go:tidy    # go mod tidy
task go:run     # go run .
task smoke      # boot locally, curl every HTTP/HTTPS/TCP/UDP path (scripts/smoke.sh)
```
