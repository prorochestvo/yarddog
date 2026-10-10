---
name: yarddog-connectivity
description: How yarddog decides the internet is down and what it is then allowed to do about it — the two probe groups and the quorum rule that grades a round, why a domain-only failure is degraded and never reboots, the in-run confirmation and cross-run corroboration guards, edge-triggered announcements, the recovery loop, and the collector's exit-code/outcome contract. Load before touching services/orchestrator.go, domain.Verdict, Result/TargetResult, Downtime, the infrastructure Checker, gateway/router or a new RouterKind driver; before changing confirmDown, outageSustained, shouldAnnounce, PhaseConfirm, RunRepository.RecentRuns, internet_ok, the unconfirmed/unsustained/degraded outcomes, reboot.cgi, the --hard-reboot flag, flock, or any collector exit code; and before touching any CHECK_*, REBOOT_* or RECOVERY_* env var.
---

# yarddog connectivity and the reboot decision

Everything between "probe the uplink" and "the router was rebooted". The rules below are
what keeps a flaky DNS path from power-cycling a working router.

## The check

Two target groups, probed concurrently (one goroutine per target), each latency recorded:

- **`ip`** — raw `net.DialTimeout("tcp", host:port, …)` to e.g. `1.1.1.1:443`, `8.8.8.8:53`.
  Bypasses DNS; proves the raw link. Configured by `CHECK_IPS`.
- **`domain`** — HTTP GET to `generate_204`-style endpoints. Exercises the DNS path too.
  Configured by `CHECK_DOMAINS`.

Both run under `CHECK_TIMEOUT`. The adapter is the `infrastructure` `Checker`; the results are
`domain.Result` / `domain.TargetResult`, and each probed target is persisted as a `checks` row
carrying its `phase` (`initial` | `recovery`).

## The verdict

`domain.Verdict` grades one round into three states (issue #6):

- **`down` ⇔ all `ip` targets fail** — the only verdict a reboot may follow.
- **`degraded` ⇔ all `domain` targets fail while an `ip` target still answers** — the
  DNS/HTTPS path is broken, the raw link is not.
- everything else is **`up`**.

A single failed target within a group is never a trigger, and an empty group is "not
applicable" rather than vacuously failed — **except** that with no `ip` targets configured at
all, the `domain` group becomes the only evidence there is and a full failure of it counts as
`down`.

**The split is measured, not theoretical.** Of 29 "down" verdicts recorded in 21 days, 28 had
both `ip` targets connecting in 19-1118 ms and ICMP answering 4/4 while only the two HTTPS
probes timed out — they share one DNS path, in production through the very router being
rebooted. Four reboots in seven hours on 2026-07-30 did not stop them. That is the whole
reason `degraded` exists; do not collapse it back into `down`.

## The two guards before a reboot

Both live in `services/orchestrator.go`:

- **In-run confirmation** — `confirmDown` re-probes `CHECK_CONFIRMATIONS` times,
  `CHECK_CONFIRM_DELAY` apart (persisted under `PhaseConfirm`); one healthy sample ends the
  run as `unconfirmed`, correcting the `internet_ok` it wrote from the first sample.
- **Cross-run corroboration** — `outageSustained` requires the previous `REBOOT_MIN_STREAK - 1`
  runs to have seen the outage too, via `RunRepository.RecentRuns`. A run that falls short ends
  as `unsustained`. **Fail-closed**: an unreadable history withholds the reboot rather than
  granting it.

**Neither guard sends a Telegram message — silence is the point.** The `runs` row and a log
line carry the record. `degraded` and cooldown-skip notices are edge-triggered
(`shouldAnnounce`), so a state that lasts hours is announced once, not once per run.

`REBOOT_COOLDOWN` suppresses a reboot too soon after the last one (exit 5). `REBOOT_ENABLED=false`
is monitor-only: check, record and notify, but never reboot. Soft mode is the default; the
`--hard-reboot` flag reboots unconditionally without waiting for a `down` verdict.

After a reboot the recovery loop re-probes every `RECOVERY_INTERVAL` until `RECOVERY_TIMEOUT`,
writing `recovery`-phase `checks` rows; `Downtime` is the measured gap. Exhausting the timeout
without the link returning is exit 4.

## Rebooting: the device-driver family

`gateway/router` is a family, not a device. `New(kind, cfg)` dispatches on `ROUTER_KIND`
(default `nokia`) and returns a `services.Rebooter`. The Nokia driver logs into the router web
UI and posts `reboot.cgi`; its login form and reboot markers are `gateway/dto` shapes. A new
device (TP-Link ONT, Tapo/Sonoff smart-plug fallback) is **"add a driver file + a factory
case", never a fork** — no per-driver sub-package, one driver, one file. Router HTTP never
leaves `gateway/router`.

The router UI is plain HTTP; the password crosses the LAN in cleartext, which is why the env
file is `chmod 600` and the router UI stays off the internet.

## The collector exit-code contract

The machine/cron contract. `cmd/yarddog` is the only place that calls `os.Exit`, and
`services/orchestrator.go` is the single place that maps a terminal state to its (exit code,
`runs.outcome`, Telegram message) triple — lower layers return plain wrapped errors and let
`services` decide.

| Code | Meaning |
|---|---|
| 0 | internet up / reboot completed |
| 1 | configuration error (`.env`, DSN) |
| 2 | another instance already running (lock held) |
| 3 | reboot request failed (login / `reboot.cgi`) |
| 4 | reboot done, internet not restored within the timeout |
| 5 | reboot skipped due to cooldown |
| 6 | a failure was observed but deliberately not acted on (`degraded`, `unsustained`) |

Single-instance is a `flock` named `yarddog.lock` in the DB's directory, created on demand, so
a local run needs no `/var/run` provisioning. The daemon's exit codes are a separate, disjoint
contract — see the `yarddog-daemon` skill.

## Testing this layer

The orchestrator and recovery loop run against in-package fakes: a fake `Clock` and a fake
`Checker` injected through the ports, `httptest` servers for the router. **No real network and
no `time.Sleep` in the state machine, ever** — a sleeping test is the bug, not the flake.
