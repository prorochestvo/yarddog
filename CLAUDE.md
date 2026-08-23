# CLAUDE.md

`yarddog` is a self-hosted watchdog for a home GPON router (Nokia G-240W-A): it checks the
internet uplink and reboots the router over its web UI when the link is down. One Go module
(`github.com/prorochestvo/yarddog`), **two binaries** — `cmd/yarddog`, the short-lived
cron-fired collector and the SQLite database's sole writer, and `cmd/yarddogd`, a long-running
read-only **query daemon** serving what it recorded as a JSON REST API plus an embedded status
dashboard over the LAN. They share every layer package and the one database, but each has its
own composition root and its own, disjoint required config; `README.md` is the user-facing
description.

A map, not a manual: what applies to every task, plus the rules whose violation is silent.

## Project skills

Invoke by name (Skill tool) when the work touches their area; each is the full canon for its
subject, and this file keeps only the tripwire.

| Skill | Load before touching |
|---|---|
| `yarddog-connectivity` | `services/orchestrator.go`, `domain.Verdict`, the `Checker`, `gateway/router` and any `RouterKind` driver, `flock`, the collector exit codes, any `CHECK_*`/`REBOOT_*`/`RECOVERY_*` key |
| `yarddog-storage` | any SQL, `infrastructure/store.go`, the `Store`, the `runs`/`checks`/`metrics`/`host`/`pings`/`meta` tables or an `*_archive` twin, the `metrics_{linux,darwin}.go` collectors, any `METRICS_*`/`PING_*` key |
| `yarddog-daemon` | `cmd/yarddogd`, `gateway/httpapi`, `gateway/dto/api.go`, `QueryService`, `Inspector`, `HealthProbe`, `GET /ping`, `GET /health/check`, any `DAEMON_*` key |
| `yarddog-outbox` | `services.OutboxService`, `OutboxRepository`, `OutboxMessage`, `gateway/telegram`, the `tbot_queue` table, `TELEGRAMBOT_DSN`, `LABEL`, the wording of any `#REBOOT {LABEL} …` message |

Generic Go conventions (style, declaration order, test structure, godoc, error discipline,
build hygiene, organisation) come from the `stack-go` plugin skills and are not restated
anywhere in this repo. **Where this file contradicts them, this file wins** — notably the
testify ban below.

### Where new canon goes

This file loads whole into every session and never unloads, so its size taxes every
conversation. Keep it under **12k chars**, and route new documentation by *when the reader
needs it*:

- **CLAUDE.md** — what applies to every task (architecture map, layer table, dependency rule,
  config model, error contract, conventions, working agreement), plus rules whose violation is
  **silent**. Every subject moved out leaves one such line behind: the sentence that stops
  someone getting it wrong before they load anything.
- **A project skill** (`.claude/skills/<name>/SKILL.md`) — the depth for one subject area. Its
  `description` *is* the load trigger: name the packages, paths, symbols and env vars that
  should pull it in; one that summarises the prose instead never loads, and its knowledge is
  lost.
- **Neither** — incident narratives, enumerations derivable from `yarddog.env.example` or the
  code, and the reasoning behind a decision already taken: commit bodies and `plans/`.

**Measure, never estimate** — this file is mostly contracts and identifiers, which do not
compress. `wc -c` before and after, and prove a move lost nothing by extracting every
backticked span from the old text and confirming each appears in the new set.

## Standing tripwires

Both fail silently.

- **This repository is public.** Every commit, PR body, issue and code comment is
  world-readable the moment it is pushed, and removing a file from `HEAD` leaves it in history
  and in the PR diff — "pushed briefly, then removed" is not "never published". **Stage by
  explicit path** (`git add <paths>`), never `git add -A` / `git add .` / `git commit -a`, and
  check `git status --short` first — `git diff --stat` omits untracked files.
- **The dev box IS the deploy target**, so `make deploy` cannot run here — it SSHes to a host
  that does not resolve locally, and `/opt/yarddog/` belongs to the confined CI deploy user.
  **Ship by pushing an annotated `v*` tag on `main`** and let `.github/workflows/release.yml`
  build, upload to `artifacts/<VERSION_ID>`, flip `bin/release`, restart `yarddogd` and gate on
  `/ping` with auto-rollback. A tag push is a production deploy — confirm before cutting one.

## Build & run

Pure-Go, `CGO_ENABLED=0` — the only non-stdlib dependency is `modernc.org/sqlite` (a cgo-free
SQLite driver), chosen so the binaries cross-compile to the Pi without a C toolchain. The
`make` targets (including `build OS=arm64` for the Pi) live in the Makefile; raw equivalents
need none — `CGO_ENABLED=0 go build -o build/yarddog ./cmd/yarddog` (and `./cmd/yarddogd`),
`go vet ./... && go test ./...`, plus the usual `go test` flags `-run TestName[/subtest]`,
`-v`, `-bench=. -benchmem -run=^$`. `make lint` is `go vet` plus the forbidden-dependency guard.

Gotcha: `-race` forces cgo, which the pure-Go build path avoids, so it is deliberately **not**
in the default gate — run `make test-race` on a box with a C compiler.

## Architecture

Layered packages under a strict inward dependency rule. Each `cmd/*` `main` is a composition
root; the layers below never import outward. Tests live next to the code they exercise, one
`*_test.go` per file.

```
domain  ←  services  ←  { infrastructure, gateway/* }  ←  { cmd/yarddog, cmd/yarddogd }
```

| Package | Responsibility |
|---|---|
| `domain/` | pure values + policy, importing nothing internal or third-party: `Result`/`TargetResult` + the `Verdict` quorum rule, `Run`/`Check`/`RunUpdate` + the `Mode`/`Action`/`Phase`/`Outcome` enums, `OutboxMessage`, `Exit*`/reason consts, `RouterKind`, `Downtime`. |
| `services/` | application layer, imports `domain` only: all eleven ports (`ports.go`, `query.go`, `health.go`), the orchestrator + recovery state machine (`Execute`), `OutboxService`, `QueryService`, `Inspector`, and the terminal-state mapping described under Error handling. |
| `infrastructure/` | driven adapters over local/generic tech: the SQLite `Store`, the connectivity `Checker`, `SystemClock`, the build-tagged host-metrics collectors, the `collector-freshness` `HealthProbe`, `.env` loading (`config.go`, `daemon_config.go`). |
| `gateway/router/` | the device-driver *family*: `New(kind, cfg)` dispatches on `ROUTER_KIND` (default `nokia`) and returns a `services.Rebooter`. A new reboot target is a driver file plus a factory case — never a fork, and nothing else moves. |
| `gateway/telegram/` | Telegram Bot API `Client`, a `Sender`. |
| `gateway/dto/` | wire shapes at those boundaries: Telegram JSON body, Nokia login form + reboot markers, the daemon's JSON responses (`api.go`). |
| `gateway/httpapi/` | the daemon's inbound-HTTP adapter: stdlib `net/http` `ServeMux`, the read-only JSON API, domain→DTO mapping, the `go:embed`-ed dashboard at `GET /{$}`, token auth. |
| `cmd/yarddog/`, `cmd/yarddogd/` | one composition root each, and the only `os.Exit` in each: flags, `flock`, `LoadConfig`, adapters wired through the ports, `services.Execute`; and `LoadDaemonConfig`, the shared `Store`, `httpapi.Server`, `http.Server` timeouts + `signal.NotifyContext` shutdown. |

Where code lives is a rule, not a taste — flag violations.

- **The dependency rule is enforced.** Nothing imports a `cmd/*` package; an import cycle, or
  `go list -deps ./services` / `go list -deps ./gateway/httpapi` naming `infrastructure`, is a
  bug to fix, not a style nit. Never put SQL outside `infrastructure`, device HTTP outside
  `gateway/router`, the daemon's HTTP outside `gateway/httpapi`, or the
  exit-code/outcome/message mapping outside `services`.
- **Ports live at the consumer** — declared in `services`, which *calls* them, never beside an
  implementation, and every adapter impl and test fake carries a compile-time
  `var _ services.Port = (*impl)(nil)` assertion. Those are the test seams: inject a fake,
  never reach for a global or a `time.Sleep`.
- **No speculative structure.** No new package, sub-package, interface or binary until a real
  consumer or test seam needs it: no port "for symmetry", no `mock` package (fakes are
  single-consumer and live in `_test.go`), and deliberately no `bootstrap`/`wiring` layer
  across the two mains, which copy the trivial `resolveEnvPath` helper rather than share one.
  A third binary follows suit.

## Configuration

Read once at startup by the `infrastructure` `.env` mini-loader from an env file (`--env`,
default `/opt/yarddog/.env`, fallback `./.env`); **real environment variables take precedence
over the file**, and a missing required value fails fast (exit 1). The file holds the router
password and bot token — `chmod 600` in production.

**Every key is namespaced with the `YARDDOG_` prefix** in both the file and the real
environment (`YARDDOG_LABEL`, `YARDDOG_DAEMON_TOKEN`, …), applied in exactly one place —
`newEnvLookup`'s `get` closure in `infrastructure/config.go`. Call sites therefore name the
**bare logical key**, and only operator-facing error messages re-attach the prefix; every key
named in this repo's docs is bare.

Required for the collector: `LABEL`, `TELEGRAMBOT_DSN`, `ROUTER_USER`, `ROUTER_PASS`; for the
daemon: `DAEMON_TOKEN` and nothing else, so a monitor-only host can run just the daemon.
`DB_PATH` is shared. **`yarddog.env.example` is the authoritative catalogue** of every optional
key, its default and its clamp range (`local.env.example` is the development counterpart) — do
not restate it here or in a skill.

> Never read or edit `.env` files — `.claude/settings.json` denies it; edit
> `yarddog.env.example` instead.

## Error handling

Wrap errors with `%w`. **`services/orchestrator.go` is the single place that maps a failure to
its (exit code, `runs.outcome`, Telegram message) triple** — lower layers return plain wrapped
errors and let `services` decide. The collector has no end users, so the starterkit
`PublicError` pattern does not apply; the daemon returns generic bodies and logs the detail
server-side. Each of the three contracts — collector exit codes, daemon exit codes, message
copy — lives in its skill.

## Conventions

- **Stdlib-only, one exception.** The *only* permitted third-party dependency is
  `modernc.org/sqlite`. Forbidden, and enforced by `make lint`:
  `github.com/stretchr/testify` (or any assertion lib), cgo SQLite drivers
  (`github.com/mattn/go-sqlite3`), Telegram/bot SDKs, `.env` parsing libraries.
- **Testing: stdlib `testing` only — no testify (overrides the `stack-go` default).** Structure
  otherwise follows the `stack-go` canon, one `Test*` per tested method. Router and Telegram
  are tested with `httptest` servers, the store on `:memory:`, the recovery loop on an injected
  fake clock/checker — no real network, no real sleep.
- **No CGO for builds.** `CGO_ENABLED=0` on every build and vet, **including `darwin`**, and
  build targets are **`linux` and `darwin` only** (`arm64`/`amd64`) — any other GOOS is an
  intentional, unsupported build, so do not add a stub fallback. (`make test-race` is the one
  cgo command.)
- **Build outputs in `./build/`, scratch in `./tmp/`, logs in `./logs/`** — the only gitignored
  root dirs; runtime `*.db` files are gitignored too.
- **Times are UTC, RFC3339.** Message copy and logs are English, tag-first
  (`#REBOOT {LABEL} …`).

## Working agreement

Plan-first pipeline; the canonical procedure is the `pipeline:working-agreement` skill — load it
before starting non-trivial work. Project delta:

- **Gate:** `go vet ./... && go test ./...`, or `make test`, which prefixes `go fmt ./...`.
  `-race` is **not** in it — see Build & run.
- **Lenses:** standard three.
- **Branching:** standard (`type/<issue>-<slug>`, PR into `main`); releases are `v*` tags cut
  from `main` — see Standing tripwires.
