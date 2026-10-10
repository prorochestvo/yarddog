---
name: yarddog-daemon
description: The cmd/yarddogd query daemon — its read-only JSON REST API and go:embed-ed status dashboard, the shared-token auth middleware and the two ungated routes, the /ping plus /health/check pair with the services.Inspector probe model (sqlite, collector-freshness, and the conditional router-credential probe), and the daemon's own disjoint config and exit codes. Load before touching cmd/yarddogd, gateway/httpapi, gateway/dto/api.go, services.QueryService, services.Inspector, HistoryRepository, HealthProbe, services.Credentialer, infrastructure/health.go, infrastructure/daemon_config.go or LoadDaemonConfig; before adding or changing any daemon route, the ServeMux patterns, writeError500, web/index.html, Store.CheckUP, a probe's Name or CheckUP method, the http.Server timeouts or the signal.NotifyContext shutdown; and before touching DAEMON_TOKEN or any other DAEMON_* env var.
---

# yarddog query daemon

`cmd/yarddogd` is the second binary: a long-running, **read-only** window onto what the
collector recorded. It opens the shared `Store`, wires `QueryService` / `Inspector` /
`httpapi.Server`, runs an `http.Server` with timeouts and a `signal.NotifyContext` graceful
shutdown, and owns the only `os.Exit` in its tree.

**The daemon holds a `services.Credentialer`, never a `services.Rebooter`** — it is
structurally incapable of rebooting the router, and it must stay that way. Anything that would
hand it a `Rebooter` is the finding.

## HTTP surface

`gateway/httpapi` is the inbound adapter: a stdlib `net/http` `ServeMux` using Go 1.22
method-and-`{id}` patterns, the domain→DTO mapping, and the shared-token auth middleware. It
implements `http.Handler`, consumes `services.QueryService` + `services.Inspector`, and
**never imports `infrastructure`** (`go list -deps ./gateway/httpapi` naming `infrastructure`
is a bug). Wire shapes live in `gateway/dto/api.go`.

- `GET /{$}` — the status dashboard, a `go:embed`-ed `web/index.html` built into the binary,
  so the UI version always equals the daemon's. **Ungated.**
- `GET /ping` — liveness, always `200 {"status":"ok"}`, touches no dependency. **Ungated** —
  it and the dashboard are the only routes reachable without the token.
- everything else — the read-only run/check/metrics/ping queries, token-gated.

`QueryService` is the limit-clamping read layer; list endpoints read the hot tier only and
`runs/{id}` spans both tiers — see the `yarddog-storage` skill before writing a query.

The daemon speaks HTTP only to token-bearing LAN clients and returns **generic** bodies: a
driver or SQL error is logged server-side and never reaches a response (`writeError500`). The
starterkit `PublicError` pattern does not apply here — there are no end users.

Handlers are tested with `httptest`; no real listener is bound in tests.

## The health pair

Before changing either endpoint, **invoke the `health-check` skill** — it owns the inspector
contract, the response shape, and the status-code/auth rules. What follows is the yarddog delta.

`GET /health/check` is readiness: `services.Inspector` sweeps every `HealthProbe` under one
whole-sweep `DAEMON_HEALTH_TIMEOUT` deadline, recover-wrapped and read-only, returning
`{status, server{version,uptime}, services{name→"ok"|error}}` — `200` when every probe is
healthy, `503` otherwise. **Token-gated**, because the report names internal dependencies.

Each probe owns its `Name()` and `CheckUP(ctx)`; the `Inspector` holds **no per-dependency
logic** — add a dependency by adding a probe, never by touching the aggregator.

Two always-present probes:

- **`sqlite`** — `Store.CheckUP`: `PingContext` **plus a real `SELECT 1`**, since a ping alone
  can stay green while queries fail.
- **`collector-freshness`** (`infrastructure/health.go`) — newest `runs.started_at` within
  `DAEMON_STALE_AFTER`, catching a cron collector that has silently stopped writing.

One **conditional** probe: the **router credential probe**, active when both `ROUTER_USER` and
`ROUTER_PASS` are set on the daemon. It performs a live login to the router web UI and
immediately resets the session — no TTL cache, because this endpoint is a real-time "is auth
still working?" view — **never calls `reboot.cgi`**, and coalesces concurrent `CheckUP` calls
into one in-flight login via a stdlib single-flight, so there are never concurrent sessions. It
is registered **last** so a wedged login cannot starve the SQLite probes, and it is named in
the `services` map by `DAEMON_HEALTH_ROUTER`. Its per-request HTTP timeout must be greater
than zero and **less than `DAEMON_HEALTH_TIMEOUT`**, or the daemon refuses to start; setting
exactly one of the two credentials is likewise a fail-fast startup error.

Because that login executes on **every** `/health/check` hit, poll on demand rather than
sub-minute, to avoid router session churn. The endpoint is token-gated and LAN-only, so the
operator controls the rate.

**Enabling this probe places the router password on a host that previously needed none.** The
daemon still never reboots, but a compromised env file now also exposes the router login —
omit both credentials to keep monitor-only isolation.

Probes run **sequentially** under the shared deadline, so one that wedges until the deadline
starves those after it. That is acceptable today because the two permanent probes share one
SQLite handle (a genuine wedge is global) and the whole-sweep bound is the property that
matters. **The first independent, non-SQLite probe is when to revisit that.**

## The collector has no health endpoints, deliberately

`cmd/yarddog` is a short-lived cron process that opens no listening ports. The
liveness/readiness pair targets long-running services and does **not** apply to it — never add
an HTTP listener or health endpoints to the collector.

## Daemon config and exit codes

The daemon reads the same env file through its own `LoadDaemonConfig`
(`infrastructure/daemon_config.go`). Its **sole required key is `DAEMON_TOKEN`** — it never
requires or uses the router password or the Telegram DSN, so a monitor-only host can run just
the daemon. Daemon-only optional keys, defaults in parens: `DAEMON_ADDR` (`127.0.0.1:8420` —
loopback by default; production sets a LAN `ip:port` explicitly), `DAEMON_STALE_AFTER` (`90m`),
`DAEMON_MAX_CONNS` (`2` — headroom so the health sweep's SQLite probe never queues behind an
in-flight data read), `DAEMON_HEALTH_TIMEOUT` (`5s`), `DAEMON_READ_TIMEOUT` (`10s`),
`DAEMON_WRITE_TIMEOUT` (`15s`), `DAEMON_IDLE_TIMEOUT` (`60s`). `DB_PATH` is shared with the
collector.

Exit codes are the daemon's own contract, **disjoint from the collector's**:

| Code | Meaning |
|---|---|
| 0 | clean shutdown (SIGINT/SIGTERM, drained) |
| 1 | configuration error (missing `DAEMON_TOKEN`, unparseable value) |
| 2 | the HTTP listener failed to start (e.g. the address is already in use) |

There is **no `flock`**: the daemon's single-instance guard is the port bind itself.
