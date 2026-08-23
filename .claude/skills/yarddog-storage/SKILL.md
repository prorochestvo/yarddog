---
name: yarddog-storage
description: yarddog's SQLite data model and the host telemetry that fills it — the hot telemetry tables and their *_archive twins, UUIDv7 primary keys and what depends on them being time-ordered, the startup roll-over then retention then VACUUM sequence, which reads span both tiers and which read hot only, and the build-tagged metrics collectors with their untagged pure parsers. Load before writing any SQL or touching infrastructure/store.go, the Store, or the runs, checks, metrics, host, pings, tbot_queue and meta tables or any *_archive twin; before adding a column, a table- or column-name const, or a RunRepository, OutboxRepository, MetricsRepository or HistoryRepository method; before changing HOT_WINDOW_DAYS, RETENTION_DAYS, DB_PATH, last_vacuum_at, busy_timeout=5000, WAL mode or the CREATE TABLE IF NOT EXISTS bootstrap; and before touching metrics.go, metrics_linux.go, metrics_darwin.go, NewMetricsCollector, MetricsCollector, METRICS_ENABLED and its METRICS_* siblings, or PING_HOSTS, PING_COUNT and PING_TIMEOUT.
---

# yarddog storage and telemetry

The one SQLite database, its tiering, and the collectors that write into it. The collector
(`cmd/yarddog`) is the **sole writer**; the daemon only reads.

## Engine

SQLite via the pure-Go `modernc.org/sqlite` driver, WAL mode, `busy_timeout=5000`, times
stored UTC/RFC3339. Schema is applied idempotently with `CREATE TABLE IF NOT EXISTS` at
startup — there is no migrator binary and no migration files.

Telemetry primary keys are **UUIDv7 (RFC 9562) strings** — time-ordered, so `ORDER BY id` and
`MAX(id)` stay chronological, and globally unique against a future multi-collector merge.
Do not swap one for an autoincrement integer: the ordering guarantee is load-bearing.

The real runtime DB lives at `/var/lib/yarddog/yarddog.db` (outside the repo), overridable via
`DB_PATH`; tests use `:memory:`.

**Reference every table and column name through a `const` declaration in
`infrastructure/store.go`** so a schema rename surfaces at compile time or via `grep`, never as
a runtime "no such column".

## Tables

Five **telemetry** tables, each split into a bounded **hot** working set (the names below) plus
a static **`*_archive`** twin of identical schema:

- `runs` — one row per invocation, carrying every phase timestamp and the outcome.
- `checks` — one row per probed target (`phase` = `initial` | `recovery`).
- `metrics` — host-telemetry snapshot, EAV rows per run (`collector`, `name`, `value`, `unit`,
  `ok`, `error`); strictly best-effort — a collector never errors a run, an unsupported or
  absent sensor is an `ok=0` row.
- `host` — per-run host-identity sidecar (`hostname` / `os` / `arch`), keyed by `run_id`.
- `pings` — average-ping metrics per configured host (`sent` / `received` / `avg_ms`).

Plus two untiered tables:

- `tbot_queue` — the Telegram message queue. Keeps an `INTEGER` id rather than a UUIDv7: it is
  a local queue, never aggregated across hosts. Semantics live in the `yarddog-outbox` skill.
- `meta` — a `key` / `value` sidecar (currently `last_vacuum_at`).

## Tiering, roll-over and retention

At collector startup, in order:

1. A **run-boundary roll-over** moves runs — and all their children — older than
   `HOT_WINDOW_DAYS` from hot into archive **in one transaction**. Run-boundary means a run and
   its `checks`/`metrics`/`host`/`pings` rows never straddle the two tiers.
2. `RETENTION_DAYS` then prunes whole aged runs from the **archive** (`0` = keep forever). It
   does not touch the hot window.
3. A weekly, cadence-gated `VACUUM` (tracked in `meta` under `last_vacuum_at`) compacts the
   file — after the connectivity check, never before it, so compaction never delays a probe.

**Which reads span which tier is a rule, not an accident.** The list endpoints read **hot
only** (an absent `since` on `metrics`/`pings` defaults to the last 7 days, well inside the hot
window); only `runs/{id}` spans both tiers transparently. Archive browsing is deliberately not
exposed on the list endpoints yet — a later cursor-pagination pass reintroduces it. A read that
silently skips the archive tier returns partial history **without erroring**, so state the tier
a new query targets when you add it.

## Host telemetry collectors

One snapshot per run, taken alongside the connectivity check, gated by `METRICS_ENABLED` and
its per-sensor siblings `METRICS_TEMPERATURE`, `METRICS_FANS`, `METRICS_CPU`, `METRICS_MEMORY`,
`METRICS_DISK`, `METRICS_UPTIME`, `METRICS_NETWORK`, `METRICS_DISK_MOUNT`.

`NewMetricsCollector` is defined **solely** in `metrics_linux.go` and `metrics_darwin.go` —
sysfs/procfs on Linux, `syscall.Statfs` plus `os/exec` of `sysctl` and `vm_stat` on macOS,
where temperature and fans are unsupported. **The pure parsers live in an untagged
`metrics.go`** precisely so both platforms' parsing is unit-tested on the Linux runner; keep new
parsing there rather than behind a build tag. Any GOOS other than `linux`/`darwin` is an
intentional, unsupported build — do not add a stub fallback.

Average-ping metrics (`PING_HOSTS`, `PING_COUNT`, `PING_TIMEOUT`) take one round-trip average
per host per run and land in `pings`. Bare hosts only, no port; empty (the default) disables
the feature. **Prefer IP literals over hostnames** — a hostname makes every ping depend on DNS,
which may itself be down during the outage being measured.

Telemetry is best-effort by design: nothing here may fail a run.
