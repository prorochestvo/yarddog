# yarddog

A yard dog for a home network: a self-hosted watchdog that lives on a machine
inside your LAN, watches the internet uplink and reboots the GPON router when
the link goes down. Like a proper yard dog it guards its own yard only —
nothing is exposed to the outside world and no cloud is involved. The watchdog
itself is a short-lived process fired by cron on a home server (a Raspberry Pi
in the reference setup) and opens no ports; an optional companion daemon
(`yarddogd`) serves the recorded history as a read-only JSON API and listens on
the LAN only — loopback by default, behind a shared token.

Built for the **Nokia G-240W-A** GPON Home Gateway, whose stock firmware has a
reboot button but no reboot scheduler.

## What it does

- checks internet connectivity against several independent targets: raw IPs
  (bypassing DNS) and domain URLs (exercising DNS);
- **soft mode** (default, cron twice an hour): if — and only if — the internet
  is down, logs into the router web UI and triggers a reboot;
- **hard mode** (`--hard-reboot`, nightly cron): reboots unconditionally;
- after triggering a reboot it stays alive, polling the same targets every
  minute and recording the phases *router went down → router is up → internet
  restored* (timestamps persisted, served by the query daemon);
- announces the reboot in Telegram with two messages — *initiated* and
  *completed* — keeping the alert concise;
- records every check, run and phase timestamp in a local SQLite database;
- optionally captures a **host-telemetry snapshot** each run — CPU load, memory, disk,
  uptime, and (on Linux) temperature, fan RPM and per-interface network counters — each
  individually toggleable; pure-Go with no cgo, on Linux (`arm64`/`amd64`) and macOS
  (Apple Silicon, where temperature/fans are unavailable without cgo);
- supports a **monitor-only** mode (`REBOOT_ENABLED=false`) that watches and records
  everything but never reboots the router;
- ships an optional read-only **query daemon** (`yarddogd`) that serves the recorded
  history — runs, checks, telemetry — as a JSON API over the LAN (see
  [Daemon / query API](#daemon--query-api)).

## How a reboot goes

```
initiated                       <- telegram, before anything happens
POST /login.cgi                 <- session cookies (sid/lsid)
GET  /reboot.cgi                <- fresh per-request CSRF token
POST /reboot.cgi?reboot         <- token in the body -> 200 "done reboot";
                                   a login-page redirect means rejected
poll every 60s, up to 15m:
    router went down            <- recorded (router_down_at), no telegram
    router is up                <- recorded (router_up_at), no telegram
    completed,
    internet restored           <- telegram, all checks green
```

The gateway down/up transitions are only recorded as timestamps, not announced;
the reboot flow sends just the two messages. If the router restarts faster than
the polling interval, the transitions are never observed and no timestamps are
written.

## Deciding "the internet is down"

Targets are split into two groups: `ip` (TCP dial to `1.1.1.1:443`,
`8.8.8.8:53`) and `domain` (HTTP requests to `generate_204`-style endpoints,
which also exercise the DNS path). The verdict is conservative: the internet is
considered down only when *all* targets of a group fail — either every IP
target (link is dead) or every domain target (DNS is dead). A single failed
target never triggers a reboot.

Soft mode also honours a **cooldown** (default 2 h): if the previous reboot was
recent and the internet is still down, the problem is most likely upstream (an
ISP outage), and rebooting the router every half hour would help nobody. Hard
mode ignores the cooldown.

## Telegram notifications

Every message starts with a tag and a label:

```
#REBOOT home initiated (reason: no internet)
#REBOOT home completed, internet restored
#REBOOT home no internet, skipping reboot (cooldown: last reboot 40m ago)
```

The bot is configured with a single DSN:

```
YARDDOG_TELEGRAMBOT_DSN="tbot://<chat_id>:@<bot_token>/"
```

**Outbox.** The machine running yarddog is normally connected through the very
router it reboots, so at the exact moment a message matters most there may be
no internet to deliver it. Every message is therefore written to an outbox
table first and sent when possible; anything undelivered is flushed once
connectivity returns (and again on the next run), annotated with the original
time: `... [queued 04:02]`.

## Installation

The host uses the release-layout: binaries live in immutable
`/opt/yarddog/artifacts/<VERSION_ID>/` dirs and `/opt/yarddog/bin/release` is a
symlink to the live one, which the cron collector and the systemd daemon both run.
Build, stage the binaries on the host, then run `deploy/pi5-install.sh` once — it
establishes that layout plus the cron, the systemd unit, and the
passwordless-restart grant. After that, tagged releases deploy automatically (see
[Continuous integration & deployment](#continuous-integration--deployment)).

```bash
git clone https://github.com/prorochestvo/yarddog.git
cd yarddog
make build OS=arm64                            # -> build/{yarddog,yarddogd}-arm64

# stage on the host; the installer seeds these into the artifact store on first run
sudo mkdir -p /opt/yarddog
sudo cp build/yarddog-arm64  /opt/yarddog/yarddog
sudo cp build/yarddogd-arm64 /opt/yarddog/yarddogd
sudo cp deploy/pi5-install.sh /opt/yarddog/
sudo cp yarddog.env.example   /opt/yarddog/.env
sudo chmod 600 /opt/yarddog/.env               # router password + bot token live here
# edit /opt/yarddog/.env, then:
sudo bash /opt/yarddog/pi5-install.sh
```

`pi5-install.sh` creates the confined CI deploy user `github_aide`, moves the two
staged binaries into `/opt/yarddog/artifacts/<VERSION_ID>/` (owned by
`github_aide`), points `bin/release` at them, re-owns `.env` + the database to
`root` (mode 600), writes the root cron + root systemd unit (both targeting
`bin/release/…`), and installs the sudoers drop-in the release pipeline needs to
restart the daemon. It is idempotent — re-running also migrates an older
`seil`-owned install onto this model.

Pure Go, no cgo (SQLite via `modernc.org/sqlite`), so cross-compilation is
painless:

```bash
GOOS=linux GOARCH=arm64 go build -o yarddog ./cmd/yarddog   # e.g. for a Raspberry Pi
```

### Upgrading to the hot/archive split

The `*_archive` tables (see [History](#history)) and the UUIDv7 string ids the
hot and archive tables share are created idempotently, but there is no
migration path for a database written before this change (`INTEGER`-id
`runs`/`checks`/`metrics`/`pings`) — the two schemas aren't compatible, so
rows never "age out" gracefully on an old database.

The collector self-heals this on its own: at startup it detects a pre-upgrade
database, renames it aside (never deletes it — look for the original filename
with an `.incompatible-<timestamp>` suffix) and starts a fresh database in its
place, logging loudly either way. The daemon (`yarddogd`) deliberately does
not — it is a read-only reader with no lock of its own, so it never renames
the shared database. If systemd starts it against a pre-upgrade database
before the collector's next run, it logs a configuration error and exits, and
systemd restarts it until the collector has recreated the database — a brief
restart loop on first upgrade, nothing destroyed. Manually resetting the file
yourself beforehand is therefore optional, not the only way forward — useful
if you'd rather control exactly when the switch happens, keep the old file's
original name free, or skip that first-upgrade daemon flap:

```bash
sudo systemctl stop yarddogd   # if the daemon is running
sudo rm -f /var/lib/yarddog/yarddog.db /var/lib/yarddog/yarddog.db-wal /var/lib/yarddog/yarddog.db-shm
```

Either way, any undelivered `tbot_queue` Telegram messages in the old
database do not carry over to the new one — they stay behind in whichever
old file results (self-quarantined or manually removed).

## Configuration

Settings are read from an env file (`--env /path`, default `/opt/yarddog/.env`,
fallback `./.env`); real environment variables take precedence over the file.
Every key is namespaced with the `YARDDOG_` prefix, in both the file and the
real environment.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `YARDDOG_LABEL` | yes | — | label in messages: `#REBOOT {LABEL}` |
| `YARDDOG_TELEGRAMBOT_DSN` | yes | — | `tbot://<chat_id>:@<bot_token>/` |
| `YARDDOG_ROUTER_ADDR` | no | `http://192.168.1.1` | router web UI address |
| `YARDDOG_ROUTER_KIND` | no | `nokia` | reboot driver to use (device family; more devices to come) |
| `YARDDOG_ROUTER_USER` | yes | — | router admin login |
| `YARDDOG_ROUTER_PASS` | yes | — | router admin password |
| `YARDDOG_DB_PATH` | no | `/var/lib/yarddog/yarddog.db` | SQLite location |
| `YARDDOG_CHECK_IPS` | no | `1.1.1.1:443,8.8.8.8:53` | csv of `host:port` TCP targets |
| `YARDDOG_CHECK_DOMAINS` | no | `https://www.google.com/generate_204,https://cloudflare.com/cdn-cgi/trace` | csv of HTTP targets |
| `YARDDOG_CHECK_TIMEOUT` | no | `5s` | per-target timeout |
| `YARDDOG_RECOVERY_INTERVAL` | no | `60s` | polling period after a reboot |
| `YARDDOG_RECOVERY_TIMEOUT` | no | `15m` | how long to wait for recovery |
| `YARDDOG_REBOOT_COOLDOWN` | no | `2h` | minimal gap between soft reboots |
| `YARDDOG_HOT_WINDOW_DAYS` | no | `30` | a run stays in the fast "hot" tables while younger than this; older runs roll into the `*_archive` tables (clamped to 1–100000) |
| `YARDDOG_RETENTION_DAYS` | no | `90` | prune *archived* runs and their checks/metrics older than this (0 = keep the archive forever; clamped to ≤100000 otherwise) |
| `YARDDOG_REBOOT_ENABLED` | no | `true` | `false` = monitor-only: check/record/notify, never reboot |
| `YARDDOG_METRICS_ENABLED` | no | `true` | master switch for the per-run host-telemetry snapshot |
| `YARDDOG_METRICS_TEMPERATURE` | no | `true` | temperatures in °C (Linux only) |
| `YARDDOG_METRICS_FANS` | no | `true` | fan RPM, best-effort — only if the hwmon driver exposes it (Linux only) |
| `YARDDOG_METRICS_CPU` | no | `true` | load average (1/5/15) |
| `YARDDOG_METRICS_MEMORY` | no | `true` | total/free/available memory |
| `YARDDOG_METRICS_DISK` | no | `true` | usage of `YARDDOG_METRICS_DISK_MOUNT` |
| `YARDDOG_METRICS_UPTIME` | no | `true` | uptime |
| `YARDDOG_METRICS_NETWORK` | no | `true` | per-interface rx/tx bytes (Linux only) |
| `YARDDOG_METRICS_DISK_MOUNT` | no | `/` | filesystem the disk collector stats |
| `YARDDOG_PING_HOSTS` | no | *(empty, feature off)* | csv of bare hosts (no port) to average-ping each run; prefer IP literals so a ping never depends on DNS, which may itself be down during an outage |
| `YARDDOG_PING_COUNT` | no | `5` | pings sent per host, clamped to `[4, 7]` |
| `YARDDOG_PING_TIMEOUT` | no | `4s` | overall per-host ping deadline, clamped to `[1s, 10s]` |

## Scheduling

```cron
# soft check twice an hour; minutes are offset from :00/:30
# so the 04:00 hard reboot never collides with a soft run
7,37 * * * *  /opt/yarddog/bin/release/yarddog >> /opt/yarddog/yarddog.log 2>&1

# unconditional nightly reboot
0 4 * * *     /opt/yarddog/bin/release/yarddog --hard-reboot >> /opt/yarddog/yarddog.log 2>&1
```

The command is the `bin/release` channel symlink, so a deploy's atomic flip
retargets the next tick with no cron edit.

A `flock` on `yarddog.lock` — kept in the database's own directory, so it stays
self-contained beside the data — additionally guarantees that runs never overlap
(a recovery loop can outlive the 30-minute cron interval). The lock is released
by the kernel when the process dies, so it can never go stale.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | internet is up / reboot completed successfully |
| 1 | configuration error |
| 2 | another instance is already running |
| 3 | reboot request failed (login / reboot.cgi) |
| 4 | reboot done, internet not restored within the timeout |
| 5 | reboot skipped due to cooldown |

## History

Everything lands in SQLite: `runs` (one row per invocation with all phase
timestamps), `checks` (one row per probed target), `tbot_queue` (the message
queue), and — when telemetry is enabled — `metrics` and `host` (the per-run
snapshot and its host identity). `yarddogd` serves all of it over HTTP (see
[Daemon / query API](#daemon--query-api)).

`runs`, `checks`, `metrics`, `host`, and `pings` form the fast "hot" working
set: at every collector startup, any run older than `YARDDOG_HOT_WINDOW_DAYS`
is moved — together with all its checks/metrics/host/pings rows — into a
same-shaped `*_archive` table in one step, so recent-history queries never
scan more than a bounded, `HOT_WINDOW_DAYS`-wide slice. Archived runs older
than `YARDDOG_RETENTION_DAYS` are then deleted outright, and a full `VACUUM`
runs at most once a week to reclaim and defragment the space both steps free.
All three are best-effort maintenance passes — a failure is logged and never
aborts the actual connectivity check.

```bash
sqlite3 /var/lib/yarddog/yarddog.db \
  "select started_at, mode, action, outcome from runs order by id desc limit 10;"
```

## Daemon / query API

`yarddog` records everything locally; `yarddogd` is an optional long-running
companion that serves that record as a read-only JSON REST API — plus its own
embedded, read-only status dashboard at `/` — so the built-in page, an uptime
monitor, or another host on the LAN can read the runs, checks and host telemetry
without touching the SQLite file directly. The dashboard is a static asset
compiled into the binary (its version therefore always equals the daemon's, and
the same value appears in `/health/check`'s `server.version`); it holds no secret
and is the one route besides `/ping` served without the token. The collector
stays the sole writer — the daemon only ever reads.

```bash
YARDDOG_DAEMON_TOKEN=$(openssl rand -hex 32) YARDDOG_DAEMON_ADDR=192.168.1.10:8420 yarddogd
```

It binds `YARDDOG_DAEMON_ADDR` (loopback `127.0.0.1:8420` by default — set a LAN
`ip:port` to expose it) and requires `YARDDOG_DAEMON_TOKEN`: every route but `/ping`
and `/` (the secret-free dashboard) demands that secret in the `Authorization: Bearer <token>` header. There is no TLS and no
per-user auth, so bind it to the LAN behind your firewall, never the open
internet.

| Method & path | Auth | Returns |
|---|---|---|
| `GET /` | — | embedded read-only status dashboard (HTML) |
| `GET /ping` | — | liveness, always `200 {"status":"ok"}` |
| `GET /health/check` | token | readiness: per-dependency probe report, `200` / `503` |
| `GET /api/v1/host` | token | newest host-identity snapshot (`404` until one is recorded) |
| `GET /api/v1/metrics/latest` | token | every metric of the newest run that has any (`404` if none) |
| `GET /api/v1/metrics?since=&collector=&limit=&include_empty=` | token | metrics history, newest first (`200`+`[]` when empty) |
| `GET /api/v1/pings?host=&since=&limit=&include_unreachable=` | token | ping history, newest first (`200`+`[]` when empty) |
| `GET /api/v1/runs?limit=` | token | runs, newest first (`200`+`[]` when empty) |
| `GET /api/v1/runs/{id}` | token | one run plus its checks (`400` empty id, `404` unknown) |
| `GET /api/v1/overview?since=&bucket=` | token | server-downsampled multi-day view: bucketed metrics + ping reachability with server-detected outage episodes (`200`+`[]` series when empty) |

Every `id`/`run_id` — `runs.id`, `checks`/`metrics`/`pings`' own row ids, and the
`host`/`host_archive` `run_id` — is a UUIDv7 string (RFC 9562), not a numeric
autoincrement value: it is time-ordered, so lexicographic order already matches
chronological order (newest-first listings need no separate sort key), and it
stays globally unique even if a future host ever aggregates records from more
than one collector. `since` is RFC3339 UTC and, when omitted from
`/api/v1/metrics` or `/api/v1/pings`, defaults to the **last 7 days**; an
explicit `since` overrides it. `collector` is one of
`temperature`/`fans`/`cpu`/`memory`/`disk`/`uptime`/`network`; `limit` is capped
server-side (runs 500, metrics 1000, pings 1000) and defaults to 100 when
absent (runs default 100, metrics/pings 100). `/api/v1/pings` rows carry
`{run_id, ts, host, sent, received, avg_ms, ok, error}`; `avg_ms` is `null`
unless `ok` (a host with zero replies has no round trip to average).
`include_unreachable=true` keeps those null-avg rows, which the default
response omits. The list endpoints read the hot tables only — the default
7-day window sits well inside the `HOT_WINDOW_DAYS` (default 30) hot span, and
an explicit `since` reaching before the hot floor simply returns what hot
holds (archive browsing is not yet exposed on the list endpoints).
`/api/v1/runs/{id}` still transparently resolves a run whether it is hot or
already archived. `/health/check` probes the SQLite handle
(`PING` + `SELECT 1`) and collector freshness (the newest run must be younger
than `DAEMON_STALE_AFTER` — a stale value means the cron collector has stopped);
it is token-gated because its body names internal dependencies.

`GET /api/v1/overview` serves the dashboard's retrospective view: every
enabled metric collector and configured ping host, bucketed across a window
instead of returned as raw per-run rows. `since` (RFC3339 UTC) and `bucket`
(a Go duration, e.g. `2h`) are both optional — omitted, they default to the
same last-7-days window and a 1-hour bucket, and an out-of-range `bucket` is
clamped to `[1m, 24h]` rather than rejected. Each `metrics[]` entry is one
collector/name pair's `{ts, min, max, avg, count}` buckets; each `pings[]`
entry is one host's `{ts, sent, received, loss_pct, avg_ms, max_ms, samples}`
buckets (`avg_ms`/`max_ms` are `null` when every sample in the bucket was
unreachable) plus its own `outages[]` — contiguous runs of degraded samples
the server has already detected and classified `loss`/`unreachable`, so the
dashboard never has to recompute them. Metric presentation stays
gauge-uniform across every collector for now; richer, collector-specific
presentation (e.g. network throughput, memory breakdown) is deferred to a
later pass. Everything past that — value thresholds, coloring, alerting — is
a client-side/dashboard concern; only ping outage detection happens
server-side. This endpoint is heavier than the row-capped list endpoints — an
unbounded `GROUP BY` over the whole window — so clients should call it on
dashboard load or occasional refresh, not on a tight poll.

```bash
curl -s -H "Authorization: Bearer $YARDDOG_DAEMON_TOKEN" \
  "http://192.168.1.10:8420/api/v1/runs?limit=5"
```

### Daemon configuration

Read from the same env file; only `YARDDOG_DAEMON_TOKEN` is required, and none of
the collector's router/Telegram keys are — a monitor-only host can run just the
daemon.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `YARDDOG_DAEMON_TOKEN` | yes | — | shared secret sent as `Authorization: Bearer <token>` |
| `YARDDOG_DAEMON_ADDR` | no | `127.0.0.1:8420` | bind address; set a LAN `ip:port` to expose |
| `YARDDOG_DB_PATH` | no | `/var/lib/yarddog/yarddog.db` | SQLite location (shared with the collector) |
| `YARDDOG_DAEMON_STALE_AFTER` | no | `90m` | freshness budget for `/health/check` |
| `YARDDOG_DAEMON_MAX_CONNS` | no | `2` | SQLite read-pool size (≥2 keeps the health probe off the data path) |
| `YARDDOG_DAEMON_HEALTH_TIMEOUT` | no | `5s` | whole-sweep budget for `/health/check` |
| `YARDDOG_DAEMON_READ_TIMEOUT` | no | `10s` | HTTP read timeout |
| `YARDDOG_DAEMON_WRITE_TIMEOUT` | no | `15s` | HTTP write timeout |
| `YARDDOG_DAEMON_IDLE_TIMEOUT` | no | `60s` | HTTP idle (keep-alive) timeout |

**Optional router credential probe** — adds a third `/health/check` entry that logs into the
router web UI and immediately resets the session (never reboots). Enabled only when **both**
`YARDDOG_ROUTER_USER` and `YARDDOG_ROUTER_PASS` are set; setting exactly one is a startup error.
The token-gated endpoint is LAN-only, so the operator controls poll cadence.

> **Security trade-off:** enabling the probe places the router password in the daemon's env file,
> on a host that previously needed none. The daemon never reboots (it holds only
> `CheckCredentials`, not `Reboot`), but a compromised env file now also exposes the router
> login. Omit both `ROUTER_USER` and `ROUTER_PASS` to keep the monitor-only isolation.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `YARDDOG_ROUTER_USER` | — | *(disables probe)* | router admin login — set with PASS to enable |
| `YARDDOG_ROUTER_PASS` | — | *(disables probe)* | router admin password — set with USER to enable |
| `YARDDOG_ROUTER_ADDR` | no | `http://192.168.1.1` | router web UI address |
| `YARDDOG_ROUTER_KIND` | no | `nokia` | device driver (`nokia` is the only current value) |
| `YARDDOG_DAEMON_HEALTH_ROUTER` | no | `router` | probe display name in the `services` map (e.g. `gipon`) |
| `YARDDOG_CHECK_TIMEOUT` | no | `3s` | per-request HTTP timeout for the login call; must be less than `DAEMON_HEALTH_TIMEOUT` (default 5s) |

Exit codes: `0` clean shutdown, `1` configuration error, `2` the listen address
is already in use. Run it under systemd from the hardened sample unit in
[`deploy/yarddogd.service`](deploy/yarddogd.service) — it runs as `root` so the
daemon and the collector both read the root-only `.env` and the root-owned
database (see the security model below).

## Router compatibility

Tested against Nokia G-240W-A, software `3FE56557HJHL16`. The web UI flow:
`POST /login.cgi` with `name`/`pswd` (plaintext) sets the `sid`/`lsid` session
cookies; `GET /reboot.cgi` returns a page carrying a fresh per-request
`csrf_token`; `POST /reboot.cgi?reboot` must echo that token in its
form-urlencoded body, on which a confirmed reboot answers `200` with
`done reboot` while a rejected one redirects back to the login page. Other
Nokia/ALU ONTs with the same interface will likely work; anything
else is a new driver in `gateway/router/` selected via `ROUTER_KIND` — the reboot path
is a driver family, so a different device (or a Tapo/Sonoff smart-plug hard-reset
fallback) is an added file, not a fork.

Note the router admin UI speaks plain HTTP, so the password travels in
cleartext over the LAN. yarddog itself opens no ports and talks only to the
router and `api.telegram.org`; keep `/opt/yarddog/.env` at mode `600` and never
expose the router UI beyond the LAN.

## Continuous integration & deployment

Two GitHub Actions workflows live in `.github/workflows/`:

- **`ci-main`** — on every push and PR to `main`: `make lint`, `make test`, and a
  production-shape `linux/arm64` sanity build of both binaries. No deploy.
- **`release`** — on every `v*` tag (and manual `workflow_dispatch` selecting a
  `v*` tag): lints and tests the tagged commit, cross-compiles both `linux/arm64`
  binaries (`yarddog` + `yarddogd`), and **deploys** them onto the Pi. The Pi's SSH
  sits behind **Cloudflare Access**, so the runner tunnels via `cloudflared access
  ssh` (a headless service token) and authenticates with the runner's deploy key.
  The deploy user is the confined `github_aide`, so a leaked key can ship a build
  but never read a secret.

The deploy is the release-layout flow: upload both binaries into an immutable
`/opt/yarddog/artifacts/<VERSION_ID>/` (the daemon embeds `VERSION_ID` and reports
it as `server.version` in `/health/check`), verify each SHA256 there, flip
`bin/release` to the new version (atomic symlink), and `sudo systemctl restart
yarddogd`. The collector re-execs `bin/release/yarddog` on its next cron tick, so
only the daemon is restarted. A liveness gate then polls the daemon's `/ping`; if
it never comes up the pipeline **rolls `bin/release` back** to the previous
`VERSION_ID`, restarts, and fails the job. Older artifacts are pruned to the
newest few (versions a channel still points at are never deleted).

**One-time host setup** (before the first `v*` tag): run `deploy/pi5-install.sh`
(see [Installation](#installation)). It creates the confined CI deploy user
`github_aide`, establishes the layout, points the unit + cron at `bin/release/…`,
and installs `/etc/sudoers.d/yarddog-deploy` granting `github_aide` exactly
`NOPASSWD: /usr/bin/systemctl restart yarddogd` — the sole privilege CI needs.
**Manual rollback**, any time:

```bash
ssh github_aide@pi5 'ln -sfn ../artifacts/<previous-VERSION_ID> /opt/yarddog/bin/release \
  && sudo systemctl restart yarddogd'
```

> **Security model.** Secrets (`/opt/yarddog/.env`) and the database are readable
> by `root` alone; the service — daemon and collector — runs as `root` to read
> them. The CI deploy user `github_aide` is confined to `artifacts/` + `bin/` and
> cannot reach `.env`, the DB, or the unit, so a leaked deploy key can ship a build
> but never read a credential. Containment is the confined deploy user, not
> de-rooting the service (`release-layout` skill: "two levers").

The `deploy` job runs under a `PRIME` GitHub Environment (configure the
required-reviewer rule and the values below in *Settings → Environments → PRIME*):

| Kind | Name | Purpose |
|---|---|---|
| Secret | `SSH_PRIVATEKEY` | deploy key; public half in `github_aide`'s `authorized_keys` on the Pi |
| Secret | `CF_ACCESS_CLIENT_ID` / `CF_ACCESS_CLIENT_SECRET` | Cloudflare Access service token — headless auth for the `cloudflared` SSH tunnel |
| Secret | `ACTION_EMITER_TBOT_TOKEN` | CI notifier bot token (optional; distinct from the app's `TELEGRAMBOT_DSN`) |
| Variable | `SSH_HOSTNAME` / `SSH_USERNAME` | the Pi's Cloudflare-Access hostname + the `github_aide` deploy user |
| Variable | `REMOTE_DIR` | the service base dir on the Pi (`/opt/yarddog`) |
| Variable | `TELEGRAM_ROOT_CHAT_ID` | chat for CI notifications (optional) |

The tunnel target is defined by the Cloudflare Access application, so no SSH port
is set on the runner; the Pi's host key is accepted trust-on-first-use
(`StrictHostKeyChecking=accept-new`). `cloudflared` is pinned to a pre-2026.6.0
release (2026.6.0 regressed service-token auth for `access ssh`).

## Development

```bash
go vet ./... && go test ./...
```

Tests use `httptest` servers for the router and Telegram, `:memory:` SQLite for
the store, and an injected fake clock/checker for the recovery state machine —
no real network required.
