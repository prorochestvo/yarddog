# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in
this repository.

`yarddog` is a self-hosted watchdog for a home GPON router (Nokia G-240W-A): a
short-lived CLI process, fired by cron on a Raspberry Pi, that checks the internet
uplink and reboots the router over its web UI when the link is down. See `README.md`
for the user-facing description.

The module ships **two binaries**: `cmd/yarddog`, the short-lived collector above, and
`cmd/yarddogd`, a long-running **query daemon** that serves the data the collector
records (runs, checks, host telemetry) as a read-only JSON REST API over the LAN, plus
its own embedded, read-only status dashboard at `/` (a static asset built into the
binary, so the UI version always equals the daemon's). They
share every layer package and the one SQLite database — the collector is its sole writer
— but each has its own composition root and its own, disjoint required config.

## Build & Run Commands

Pure-Go, `CGO_ENABLED=0` — the only non-stdlib dependency is `modernc.org/sqlite` (a
cgo-free SQLite driver), chosen so the binary cross-compiles to the Pi without a C
toolchain.

```bash
make build           # -> ./build/{yarddog,yarddogd}          (native host, CGO_ENABLED=0)
make build OS=arm64  # -> ./build/{yarddog,yarddogd}-arm64     (GOOS=linux GOARCH=arm64, for the Pi)
make deploy          # build OS=arm64 + ship to pi5 (release-layout: flip bin/release, restart, /ping gate)
make run        # go run ./cmd/yarddog   (collector, soft mode)
make run-daemon # go run ./cmd/yarddogd  (query daemon)
make test       # go fmt + go vet + go test ./...   (the completion gate)
make test-race  # go test -race ./...   (opt-in; needs a C toolchain — the race runtime uses cgo)
make lint       # go vet + forbidden-dependency guard
make format     # go fmt ./...
make clean      # rm -rf build/ + go mod tidy
```

Raw equivalents (no Makefile needed): `CGO_ENABLED=0 go build -o build/yarddog ./cmd/yarddog`
(and `./cmd/yarddogd` for the daemon), `go vet ./... && go test ./...`. Targeted runs use
the standard `go test` flags —
`-run TestName[/subtest]`, `-v`, `-bench=. -benchmem -run=^$`.

**The completion gate is `go vet ./... && go test ./...`** (matches the README and
PLAN). `-race` is intentionally *not* in the default gate: it forces cgo, which the
pure-Go build path avoids — run `make test-race` on a dev box that has a C compiler.

## Architecture Overview

Two binaries, one Go module (`github.com/prorochestvo/yarddog`), organized into layered
packages by a strict inward dependency rule. Each binary's `main` under `cmd/` is a
composition root; the layers below never import outward.

```
domain  ←  services  ←  { infrastructure, gateway/* }  ←  { cmd/yarddog, cmd/yarddogd }
```

| Package | Responsibility |
|---|---|
| `domain/` | pure value types + pure policy; imports nothing internal, no third-party. `Result`/`TargetResult` + the `Verdict` quorum rule; `Run`/`Check`/`RunUpdate` + `Mode`/`Action`/`Phase`/`Outcome` enums; `OutboxMessage`; `Exit*`/reason consts; `RouterKind`; `Downtime`. |
| `services/` | application layer; imports `domain` only. Defines every port at the consumer: the collector's in `ports.go` (`Checker`, `Rebooter`, `Notifier`, `Sender`, `Clock`, `RunRepository`, `OutboxRepository`, `MetricsCollector`, `MetricsRepository`) and the daemon's beside their service (`HistoryRepository` in `query.go`, `HealthProbe` in `health.go`) — eleven in all. Holds the orchestrator + recovery state machine (`Execute`), the `OutboxService`, the daemon's `QueryService` (limit-clamping read layer) and `Inspector` (health aggregator); the single place mapping a terminal state to its (exit code, `runs.outcome`, message) triple. |
| `infrastructure/` | driven adapters over local/generic tech: SQLite `Store` (satisfies `RunRepository`, `OutboxRepository`, `MetricsRepository`, `HistoryRepository`, and the `sqlite` `HealthProbe`), connectivity `Checker` (TCP/HTTP probes), `SystemClock`, host-metrics collectors (build-tagged `metrics_linux.go`/`metrics_darwin.go`, pure parsers in an untagged `metrics.go` so both are unit-tested on the Linux runner), the `collector-freshness` `HealthProbe` (`health.go`), config + `.env` loading (`config.go`) plus the daemon's own `LoadDaemonConfig` (`daemon_config.go`). |
| `gateway/router/` | the device-driver *family*. `New(kind, cfg)` dispatches on `ROUTER_KIND` (default `nokia`) and returns a `services.Rebooter`. A new device (TP-Link ONT, Tapo/Sonoff smart-plug fallback) is "add a driver file + a factory case", never a fork. |
| `gateway/telegram/` | Telegram Bot API `Client` (implements `Sender`): DSN parse, `sendMessage`, bot-token redaction. |
| `gateway/dto/` | the wire shapes at those boundaries (Telegram JSON body, Nokia login form + reboot markers, and the daemon's JSON response bodies in `api.go`). |
| `gateway/httpapi/` | the daemon's inbound-HTTP adapter: a stdlib `net/http` `ServeMux` (Go 1.22 method+`{id}` patterns) serving the read-only JSON REST API, the `go:embed`-ed static dashboard at `GET /{$}` (ungated, `web/index.html`), the shared-token auth middleware, and the domain→DTO mapping. Implements `http.Handler`; consumes `services.QueryService` + `services.Inspector`, never `infrastructure`. |
| `cmd/yarddog/` | the collector's composition root: flags, `flock`, `LoadConfig`, wire every adapter through the ports, `services.Execute`, the only `os.Exit`. |
| `cmd/yarddogd/` | the daemon's composition root: `LoadDaemonConfig`, open the shared `Store`, wire `QueryService`/`Inspector`/`httpapi.Server`, run `http.Server` with timeouts + `signal.NotifyContext` graceful shutdown, the only `os.Exit`. Its own exit-code contract (0/1/2), disjoint from the collector's. |

Tests live next to the code they exercise, one `*_test.go` per file.

**Dependency rule (enforced, not aspirational).** `domain` never imports outward;
`services` imports only `domain` (never an adapter); `infrastructure` and `gateway/*`
implement the ports and may import `domain` + `services`; nothing imports a `cmd/*`
package. A violation — an import cycle, `go list -deps ./services` naming
`infrastructure`/`gateway`, or `go list -deps ./gateway/httpapi` naming `infrastructure`
— is a bug to fix, not a style nit.

**Testability seams.** The eleven ports are defined at the consumer (`services`), so the
orchestrator, outbox, query service and inspector run in tests against in-package fakes —
no real network, no real sleeping, no real HTTP listener (the daemon's handlers use
`httptest`). Every adapter impl and every test fake carries a compile-time
`var _ services.Port = (*impl)(nil)` assertion. Inject fakes; never reach for globals or
`time.Sleep` in the state machine.

### Connectivity check & the "internet is down" verdict

Two target groups, probed concurrently (one goroutine per target), each latency
recorded:

- **`ip`** — raw `net.DialTimeout("tcp", host:port, …)` to e.g. `1.1.1.1:443`,
  `8.8.8.8:53`. Bypasses DNS; proves raw link.
- **`domain`** — HTTP GET to `generate_204`-style endpoints. Exercises the DNS path too.

The verdict is deliberately conservative: **down ⇔ (all `ip` targets fail) OR (all
`domain` targets fail)**. A single failed target is never a trigger — that guards
against rebooting the router because one remote server hiccuped.

### Data model

SQLite (`modernc.org/sqlite`), WAL mode, `busy_timeout=5000`, times stored UTC/RFC3339.
Telemetry primary keys are UUIDv7 (RFC 9562) strings — time-ordered, so `ORDER BY id` /
`MAX(id)` stay chronological, and globally unique against a future multi-collector merge.
Schema applied idempotently with `CREATE TABLE IF NOT EXISTS` at startup.

Five **telemetry** tables, each split into a bounded **hot** working set (the names
below) plus a static **`*_archive`** twin of identical schema:

- `runs` — one row per invocation, carrying every phase timestamp and the outcome.
- `checks` — one row per probed target (`phase` = `initial` | `recovery`).
- `metrics` — host-telemetry snapshot, EAV rows per run (`collector`, `name`, `value`, `unit`, `ok`, `error`); strictly best-effort (a collector never errors a run — an unsupported/absent sensor is an `ok=0` row).
- `host` — per-run host-identity sidecar (`hostname`/`os`/`arch`), keyed by `run_id`.
- `pings` — average-ping metrics per configured host (`sent`/`received`/`avg_ms`).

Plus `tbot_queue` — the Telegram message queue (see **Telegram & the outbox**; keeps an
`INTEGER` id — it is a local queue, never aggregated) — and `meta`, a `key`/`value`
sidecar (currently `last_vacuum_at`).

Reference table/column names through `const` declarations in `infrastructure/store.go`
so a schema rename surfaces at compile time / via `grep`, never as a runtime "no such
column".

At collector startup, in order: a **run-boundary roll-over** moves runs (and all their
children) older than `HOT_WINDOW_DAYS` from hot → archive in one transaction;
`RETENTION_DAYS` then prunes whole aged runs from the archive (`0` = keep forever); a
weekly, cadence-gated `VACUUM` (tracked in `meta`) compacts the file after the
connectivity check. The list endpoints read hot only (an absent `since` on
`metrics`/`pings` defaults to the last 7 days, well inside the hot window); only
`runs/{id}` spans both tiers transparently. Archive browsing is not exposed on the list
endpoints yet (a later cursor-pagination pass reintroduces it). The real runtime DB
lives at `/var/lib/yarddog/yarddog.db` (outside the repo); tests use `:memory:`.

### Health checks — collector N/A, daemon has the pair

Two processes, two answers. The **collector** (`cmd/yarddog`) is a short-lived cron
process that opens no listening ports; the liveness/readiness pair targets long-running
services and does **not** apply to it — never add an HTTP listener or health endpoints to
the collector.

The **daemon** (`cmd/yarddogd`) *is* a long-running listener, so it ships the standard
pair (the inspector pattern):

- **`GET /ping`** — liveness: always `200 {"status":"ok"}`, touches no dependency,
  ungated (the sole route reachable without the token).
- **`GET /health/check`** — readiness: `services.Inspector` sweeps every `HealthProbe`
  under one whole-sweep `DAEMON_HEALTH_TIMEOUT` deadline, recover-wrapped and read-only,
  returning `{status, server{version,uptime}, services{name→"ok"|error}}` — `200` when
  every probe is healthy, `503` otherwise. Token-gated (the report names internal
  dependencies). Two always-present probes: `sqlite` (`Store.CheckUP` — `PingContext` + a
  real `SELECT 1`, since a ping alone can stay green while queries fail) and
  `collector-freshness` (newest `runs.started_at` within `DAEMON_STALE_AFTER`, catching a
  cron collector that has silently stopped writing). A third, **conditional** router
  credential probe is active when both `YARDDOG_ROUTER_USER` and `YARDDOG_ROUTER_PASS` are
  set: it performs a live login to the router web UI and immediately resets the session
  (no TTL cache — the daemon's `/health/check` is a real-time "is auth still working?"
  view), **never calls reboot.cgi**, and coalesces concurrent `CheckUP` calls into one
  in-flight login via a stdlib single-flight (no concurrent sessions). The router probe
  is registered last so a wedged login cannot starve the SQLite probes. The daemon holds
  a `services.Credentialer`, not a `services.Rebooter`, making it structurally incapable
  of rebooting. Because the login executes on every `/health/check` hit, poll on demand
  rather than sub-minute to avoid unnecessary router session churn (the endpoint is
  token-gated and LAN-only, so the operator controls the rate).

Each probe owns its `Name()` + `CheckUP(ctx)`; the `Inspector` holds no per-dependency
logic — add a dependency by adding a probe, not by touching the aggregator. Probes run
sequentially under the shared deadline, so one that wedges until the deadline starves
those after it; acceptable today because the two permanent probes share one SQLite handle
(a genuine wedge is global) and the whole-sweep bound is the property that matters — the
first independent, non-SQLite probe is when to revisit that. Before changing these
endpoints, **invoke the health-check skill**: it owns the inspector contract, the
response shape, and the status-code/auth rules.

## Environment Variables

Config is read once at startup by the `infrastructure` `.env` mini-loader from an env file (`--env`,
default `/opt/yarddog/.env`, fallback `./.env`); **real environment variables take
precedence over the file**. Required values are validated at startup and the binary
fails fast (exit 1) on any missing one. The env file holds the router password and bot
token — it is `chmod 600` in production.

**Every key is namespaced with the `YARDDOG_` prefix** in both the file and the real
environment (`YARDDOG_LABEL`, `YARDDOG_DAEMON_TOKEN`, …). The prefix is applied in one
place — `newEnvLookup`'s `get` closure (`infrastructure/config.go`) — so call sites name
the bare logical key and only operator-facing error messages re-attach the prefix. The
bare names below are the logical keys; the real variable is each one with `YARDDOG_`
prepended.

`README.md` holds the authoritative table. Required: `LABEL`, `TELEGRAMBOT_DSN`,
`ROUTER_USER`, `ROUTER_PASS`. Optional (with defaults): `ROUTER_ADDR`, `ROUTER_KIND`,
`DB_PATH`, `CHECK_IPS`, `CHECK_DOMAINS`, `CHECK_TIMEOUT`, `RECOVERY_INTERVAL`,
`RECOVERY_TIMEOUT`, `REBOOT_COOLDOWN`, `RETENTION_DAYS`, `REBOOT_ENABLED` (monitor-only
when `false`: check/record/notify but never reboot), `METRICS_ENABLED`,
`METRICS_TEMPERATURE`, `METRICS_FANS`, `METRICS_CPU`, `METRICS_MEMORY`, `METRICS_DISK`,
`METRICS_UPTIME`, `METRICS_NETWORK`, `METRICS_DISK_MOUNT`.

The **daemon** reads the same env file through its own `LoadDaemonConfig`, whose sole
required key is `DAEMON_TOKEN` — it never requires or uses the router password or the
Telegram DSN, so a monitor-only host can run just the daemon. Daemon-only optional keys
(defaults in parens): `DAEMON_ADDR` (`127.0.0.1:8420` — loopback by default; production
sets a LAN `ip:port` explicitly), `DAEMON_STALE_AFTER` (`90m`), `DAEMON_MAX_CONNS` (`2`),
`DAEMON_HEALTH_TIMEOUT` (`5s`), `DAEMON_READ_TIMEOUT` (`10s`), `DAEMON_WRITE_TIMEOUT`
(`15s`), `DAEMON_IDLE_TIMEOUT` (`60s`). `DB_PATH` is shared with the collector.

> Never read or edit `.env` files (`.claude/settings.json` denies it). Edit
> `yarddog.env.example` when the config surface changes, never a real env file.

## Error Handling

The **collector** has no end users, so the starterkit's `PublicError` pattern does
**not** apply. The **daemon** speaks HTTP but only to token-bearing LAN clients and
returns generic bodies — a driver/SQL error is logged server-side and never leaks into a
response (`writeError500`). The contract has these surfaces:

- **Collector exit codes** — the machine/cron contract:

  | Code | Meaning |
  |---|---|
  | 0 | internet up / reboot completed |
  | 1 | configuration error (`.env`, DSN) |
  | 2 | another instance already running (lock held) |
  | 3 | reboot request failed (login / `reboot.cgi`) |
  | 4 | reboot done, internet not restored within the timeout |
  | 5 | reboot skipped due to cooldown |

- **Daemon exit codes** — its own disjoint contract: `0` clean shutdown (SIGINT/SIGTERM,
  drained), `1` configuration error (missing `DAEMON_TOKEN`, unparseable value), `2` the
  HTTP listener failed to start (e.g. the address is already in use). No `flock`: the
  daemon's single-instance guard is the port bind itself.

- **Telegram messages** — the human contract: tag-first, English, templated
  `#REBOOT {LABEL} …`. Every failure the operator should know about maps to a message
  *and* to an `outcome`/`error` column on the `runs` row.

Wrap errors with `%w`. The orchestrator (`services/orchestrator.go`) is the single place
that maps a failure to its (exit code, `runs.outcome`, Telegram message) triple — lower
layers return plain wrapped errors and let `services` decide.

### Telegram & the outbox

The Pi's only uplink is *through the router it reboots*, so at the moment a message
matters most there is often no internet. Every message is therefore written to
`tbot_queue` first, then a send is attempted; failures stay queued and are flushed when
connectivity returns (end of the recovery loop) and again at the start of the next run.
A late message carries its original time: `… [queued 04:02]`. Telegram is spoken over
raw `net/http` — no bot library. The outbox spans three layers: the queue table + CRUD
in `infrastructure` (`OutboxRepository`), reliable-delivery orchestration + the
`[queued …]` annotation in `services.OutboxService` (a `Notifier`), and the raw Bot API
POST + bot-token redaction in `gateway/telegram` (a `Sender`).

## Code Organization Principles

These govern *where code lives*. Treat a violation as something to flag, not silently
accept.

- **Layered by the dependency rule, not by launcher.** Code lands in the layer its
  responsibility belongs to (see **Architecture Overview**): pure values/policy in
  `domain`, use-cases + ports in `services`, local-tech adapters in `infrastructure`,
  external-system adapters in `gateway/*` (inbound HTTP included — the daemon's API lives
  in `gateway/httpapi`), wiring in each `cmd/*` main. Judge each package by one clear
  responsibility and a clean inward dependency. Never put SQL outside `infrastructure`,
  device HTTP outside `gateway/router`, the daemon's HTTP outside `gateway/httpapi`, or
  the exit-code/outcome/message mapping outside `services`.
- **Ports live at the consumer.** An interface is defined in the package that *calls* it
  (`services`), not next to its implementation. An adapter names the port only to satisfy
  it and to host its `var _ services.Port = (*impl)(nil)` assertion.
- **Add a device, don't fork one.** A new reboot target is a new driver in
  `gateway/router` plus a `ROUTER_KIND` case in the factory — nothing else moves.
- **Deduplication is not a goal in itself.** Distinguish coincidental similarity (let it
  diverge — duplicate the few trivial lines) from a genuine cross-cutting invariant
  (centralize once, in the layer that owns it). Premature extraction imposes a contract
  where code should be free to change. An abstraction can re-introduce the complexity it
  pretends to hide.
- **No speculative structure — even within the layers.** No new package, sub-package,
  interface, or binary until a real consumer or test seam needs it. The eleven ports and
  the layered packages earn their place from the layering, the device-driver family, and
  the collector/daemon split — that is the bar; `gateway/httpapi`, `HistoryRepository`,
  and `cmd/yarddogd` each landed only because the query API is a real consumer. Do not add
  a port "for symmetry", a `mock` package (fakes are single-consumer and live in
  `_test.go`), or a per-driver sub-package (one driver, one file).
- The daemon is the second binary that rule anticipated: `cmd/yarddogd` has its own
  composition root and wires the shared layer packages itself. There is **no**
  `bootstrap`/`wiring` layer across the two mains — `cmd/yarddog` and `cmd/yarddogd` even
  copy the trivial `resolveEnvPath` helper rather than share one. Any third binary follows
  the same rule.

## File Declaration Order

Order top-level declarations in each `*.go` file public-surface-first — a reader should
see everything important without digging:

1. Exported `const`/`var` and the `New<Object>` constructor(s).
2. The object's struct definition.
3. The object's methods (prefer alphabetical).
4. Unexported `const`/`var`.
5. Auxiliary (unexported) support structs.
6. Unexported methods/functions (prefer alphabetical).

Multiple structs in one file: same layout, primary struct first. Files with no object
(free functions + a data type): exported type(s)/function(s) on top, then unexported
consts/vars, then helpers. Treat a file that buries its public surface as something to
fix.

## Constraints

- **Stdlib-only, one exception.** The *only* permitted third-party dependency is
  `modernc.org/sqlite`. Forbidden: `github.com/stretchr/testify` (and any assertion
  lib), cgo SQLite drivers (`github.com/mattn/go-sqlite3`), Telegram/bot SDKs, `.env`
  parsing libraries. Enforced by `make lint`.
- **No CGO for builds.** `CGO_ENABLED=0` on every build/vet, **including `darwin`** — the
  host-metrics collectors stay pure-Go (sysfs/procfs on Linux; `syscall.Statfs` + `os/exec`
  of `sysctl`/`vm_stat` on macOS, where temperature/fans are unsupported). Build targets are
  **`linux` and `darwin` only** (`arm64`/`amd64`): `NewMetricsCollector` is defined solely in
  `metrics_{linux,darwin}.go`, so any other GOOS is an intentional, unsupported build — do not
  add a stub fallback. (`make test-race` is the one cgo-using command, and it is opt-in.)
- **Testing: stdlib `testing` only.** No testify. One `Test*` per tested
  method/function, every scenario a `t.Run("descriptive name", …)` subtest inside it —
  `TestEncode` with subtests, never `TestEncode_Empty` / `TestEncode_Unicode`. Methods
  on a type use `TestType_Method`. `t.Parallel()` where there is no shared mutable
  state; `t.Helper()` in helpers. Router and Telegram are tested with `httptest`
  servers; the store on `:memory:`; the recovery loop on injected fake clock/checker —
  no real network, no real sleep.
- **Compile-time interface checks.** Every adapter impl and every fake/stub gets a
  `var _ services.Checker = (*fakeChecker)(nil)`-style assertion next to it.
- **No skipped errors.** Never `_`-discard an error in production or test code. The only
  exceptions: `fmt.Fprint*` to loggers, `Rollback()` in error-recovery paths, and
  resource `.Close()` in `defer` / `t.Cleanup`.
- **No section-divider comments** (`// --- foo ---`). Let structure speak.
- **Comments**: lowercase first word (e.g. `// wrap the driver error so callers can match on it`).
  Explain the non-obvious *why*, never narrate what the code plainly says.
- **Godoc on exported identifiers**: start with the identifier name, end with a period.
  The root package doc is `// Command yarddog …` (a `main` package); each layer package
  carries its own `// Package <name> …` doc. Skip a comment when it would only restate
  the signature.
- **Build outputs live in `./build/`, scratch in `./tmp/`, logs in `./logs/`** — the
  only root dirs gitignored. Never `go build` without `-o build/…` (a bare build drops
  a `yarddog` binary in the root). Runtime `*.db` files are gitignored too.
- **Times are UTC, RFC3339.** Message copy and logs are English, tag-first
  (`#REBOOT {LABEL} …`).

## Planning Workflow

All non-trivial work is tracked as a Markdown plan file in `plans/` before
implementation begins.

### Directory layout

```
plans/
├── NNN-task-slug.md     # active / in-progress plans (e.g. 001-env-loader.md)
├── completed/           # plans for fully shipped tasks (e.g. 260706.0001.env-loader.md)
└── history/             # archived / cancelled plans
```

### File naming

- **Active (`plans/`)** — zero-padded sequential index + kebab-case slug:
  `NNN-description.md`. Pick the next number from the highest existing prefix across
  `plans/`, `plans/completed/`, and `plans/history/`.
- **Completed (`plans/completed/`)** — date prefix + zero-padded daily index:
  `YYMMDD.NNNN.description.md`. `NNNN` resets to `0001` each day.
- **Archived (`plans/history/`)** — keep the original `NNN-` filename.

### Lifecycle

1. **Create** — before touching code, produce `plans/NNN-slug.md` (use `/new-plan <slug>`).
2. **Implement** — work the plan; it stays in `plans/` while in progress.
3. **Complete** — once every acceptance criterion is met and the test gate is green,
   `git mv` it to `plans/completed/YYMMDD.NNNN.slug.md` (use `/complete-plan <NNN|slug>`).
4. **Archive** — if abandoned/superseded, move it to `plans/history/` instead.

### Plan file format

```markdown
# Task Breakdown
## Overview
## Assumptions
## Tasks
### Task N: <Title>
- Description:
- Acceptance Criteria:
- Pitfalls & edge cases:
- Complexity: Easy / Medium / Hard
## Execution Order
## Risks
## Trade-offs
```

One plan per concern. If implementation diverges, update the plan before moving it to
`completed/`.

## Agent Pipeline

All non-trivial tasks follow a three-stage pipeline using specialized agents. The review
stage fans out to **three `gocode-reviewer` instances running in parallel**, each with a
distinct lens. A separate `gocode-testdoctor` is invoked on-demand whenever tests fail,
at any stage.

```
User describes task
    ↓
1. gocode-architect   → writes plans/NNN-slug.md (see Planning Workflow)
    ↓
2. gocode-engineer    → implements the tasks in the plan
    ↓
3. gocode-reviewer × 3 (parallel — single message, three tool calls)
    Lens A: correctness & tests — bugs, races, edge cases, error paths,
            context propagation, resource cleanup, test coverage,
            test structure (one Test* per method with subtests), fixtures
    Lens B: security & operations — input validation, auth boundaries,
            secrets handling, injection, observability, log volume,
            operator/runbook UX
    Lens C: performance & architecture — allocations, blocking I/O,
            goroutine/resource leaks, layer/file boundaries, API contracts,
            interface scope, future-proofing
    ↓
   Orchestrator synthesises all three reports, deduplicates, resolves conflicts,
   and presents the merged punch list.
    ↓
  ❌ P0/P1 found?   → back to gocode-engineer with the consolidated findings; after
                      the fix, run ONE targeted reviewer pass on the changed lines.
  ⚠️  Tests failing? → gocode-testdoctor diagnoses and patches, then rerun the targeted pass.
  ✅ All three approve? → move the plan: git mv plans/NNN-slug.md
                          plans/completed/YYMMDD.NNNN.slug.md
```

Priority scale: **P0** (must fix before merge — data loss, security, state-corrupting
race, broken contract) / **P1** (should fix — correctness, leaks, missing tests for a
tested branch) / **P2** (nice to fix — maintainability, naming) / **P3** (pure style).

### Rules

- First review is always the full three-lens fan-out; the post-fix re-review is a single
  pass scoped to the changed lines.
- Each lens prompt is self-contained: lens name, what to SKIP (the other lenses), file
  list, deliverable shape (P0–P3 + `file:line` + patch sketch), ~600-word cap.
- The test gate (`go vet ./... && go test ./...`) must be green before review; a red
  tree goes to `gocode-testdoctor` first.
- On conflicting verdicts the orchestrator decides, names the rejected suggestion and
  its reasoning; the user has final say.
- The plan moves to `completed/` only once every P0/P1 is fixed or explicitly accepted.
