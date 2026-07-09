# Task Breakdown

## Overview

Close the deploy gap: today `.github/workflows/release.yml` builds the two `linux/arm64`
binaries on a `v*` tag, stages them to `/tmp/{yarddog,yarddogd}` on `pi5` (SHA256-verified),
and stops. Promotion into place and the daemon restart are manual. This task makes a tag push
(and a manual `workflow_dispatch`) build → deploy → restart with **zero manual steps**, on top of
the house **release-layout** (immutable per-`VERSION_ID` artifact store + `bin/release` channel
symlink + one-command rollback).

Chosen shape (settled with the operator before implementation):

- **Deploy user** = `seil` — the same user the daemon (systemd) and collector (cron) run as, so
  `/opt/yarddog` is directly writable by CI and only the daemon restart needs privilege.
- **Layout** = full release-layout: `artifacts/<VERSION_ID>/{yarddog,yarddogd}` + `bin/release`
  relative symlink; deploy = upload artifact set → flip symlink → restart; rollback = repoint the
  symlink and restart.
- **Restart** = a narrow `sudoers` `NOPASSWD` drop-in for exactly `systemctl restart yarddogd`.
- **Health gate** = liveness (`/ping`, ungated) triggers **auto-rollback**; readiness
  (`/health/check`) is reported but does not gate (a transient router/dependency blip must not roll
  back a good binary).
- **Trigger** = keep `push: tags: ['v*']`, add `workflow_dispatch` (dispatch selecting a `v*` tag
  ref reuses the same semver/`VERSION_ID` logic; a non-tag ref fails the semver gate fast).
- **Git** = squash all outstanding work (v1.1 + this task) into one commit on `main`, first tag
  `v0.0.0` (pre-1.0 setup), force-push-with-lease to sync origin.

## Assumptions

1. `PI5_SSH_USERNAME` (the CI deploy key's user) is `seil`, which owns `/opt/yarddog` (755) and
   runs both binaries. Confirmed by the operator.
2. The one-time Pi setup (`sudo bash /opt/yarddog/pi5-install.sh`) runs **before** the first
   autodeploy tag: it establishes `artifacts/`+`bin/`, seeds the current live binaries into an
   initial `VERSION_ID`, repoints cron + the systemd unit at `bin/release/...`, and installs the
   sudoers drop-in. CI assumes the layout and the unit's `ExecStart=.../bin/release/yarddogd`
   already exist.
3. `systemctl` is at `/usr/bin/systemctl` on the Pi (Debian/RPi OS). The sudoers rule pins that
   absolute path; the installer verifies it exists.
4. The daemon's bind address and token are read from `/opt/yarddog/.env` **on the Pi** at deploy
   time (`YARDDOG_DAEMON_ADDR`, default `127.0.0.1:8420`; `YARDDOG_DAEMON_TOKEN`) — the token never
   leaves the Pi and is never passed through the runner.
5. The collector (cron) needs no restart: it re-execs `bin/release/yarddog` on its next tick, so
   the symlink flip is enough. Only the long-running daemon is restarted. Pruning an old
   `VERSION_ID` while an old collector process is mid-run is inode-safe on Linux (the running
   process holds the inode until exit); the `flock` prevents overlapping collector runs.

## Tasks

### Task 1: Extend `release.yml` — promote, flip, restart, health-gate, rollback, prune

- Description: Replace the two steps "Copy binaries to pi5 via be-happy" (→ `/tmp`) and "Verify
  checksums on pi5" with a release-layout deploy against `APP=/opt/yarddog`:
  1. `ssh mkdir -p $APP/artifacts/$VERSION_ID`.
  2. `scp` both binaries into `$APP/artifacts/$VERSION_ID/` (loop, as today).
  3. In one `ssh` block: verify each SHA256 in the artifact dir (reuse the shared `check()`
     escaping pattern), `chmod +x`, capture `PREV=$(readlink $APP/bin/release || true)`, atomic
     flip `ln -sfn ../artifacts/$VERSION_ID $APP/bin/release`, `sudo systemctl restart yarddogd`,
     then a bounded liveness retry loop against `http://$ADDR/ping` (`$ADDR` from `.env`). On
     failure: if `$PREV` is a real prior VID, `ln -sfn $PREV bin/release` + restart (rollback),
     then `exit 1`; else `exit 1` with a "no prior version to roll back to" message. On success,
     fetch `/health/check` with the Bearer token from `.env` and echo its status for the trace
     (non-gating).
  4. A follow-up `ssh` step: channel-aware prune — delete `artifacts/*` dirs older than the newest
     5 that no `bin/*` symlink references; non-fatal (`|| true`).
  5. Add `workflow_dispatch:` (no inputs) to `on:`. Leave the semver validation + `VERSION_ID`
     derivation unchanged — a dispatch against a `v*` tag ref reuses them; a branch ref fails the
     semver gate.
  6. Update the "Workflow summary" + "Notify success" copy from "staged at /tmp" to
     "released `<VERSION_ID>`, `bin/release -> <VID>` on `<host>`"; the failure notify additionally
     surfaces whether an auto-rollback fired (from the shared trace).
- Acceptance Criteria: On a `v*` tag the job uploads to `artifacts/<VID>`, flips `bin/release`,
  restarts the daemon, and the daemon answers `/ping`; a deliberately broken binary rolls back to
  the prior VID and fails the job. No secret is echoed (token read + used only on the Pi). All SSH
  steps keep `set -euo pipefail` and feed `$RUNNER_TEMP/deploy_trace.log`. No `continue-on-error`
  on any SSH step.
- Pitfalls & edge cases: heredoc escaping — runner-side vars (`$VERSION_ID`, `$*_SHA256`) expand
  before SSH, every remote-side var (`\$PREV`, `\$ADDR`, the retry counter) is escaped so it
  reaches the remote shell literally (same rule the existing verify step documents). Relative
  symlink target (`../artifacts/$VID`) so a base-dir rename never dangles. First deploy has no
  `PREV` → skip rollback, `exit 1`. Daemon needs ~1–2 s to rebind → retry `/ping` ~10× with a 1 s
  sleep, not a single shot. Do **not** stop the daemon before the flip (flip then restart — the old
  inode is held until restart, no `ETXTBSY`).
- Complexity: Hard

### Task 2: Migrate `deploy/pi5-install.sh` to release-layout + sudoers

- Description: The installer establishes the layout and system wiring (one-time, re-runnable):
  1. `install -d -o seil -g seil -m 0755 $DIR/artifacts $DIR/bin`.
  2. Seed/migrate: if `bin/release` does not already resolve to a dir holding both binaries and
     the legacy flat `$DIR/yarddog`/`$DIR/yarddogd` exist, compute a bootstrap
     `VERSION_ID=$(date -u +%Y%m%d%H%M%S)-r_bootstrap`, move the flat binaries into
     `$DIR/artifacts/$VERSION_ID/`, `chmod +x`, and `ln -sfn ../artifacts/$VERSION_ID $DIR/bin/release`.
  3. Rewrite the cron command to `$DIR/bin/release/yarddog` and the systemd `ExecStart` to
     `$DIR/bin/release/yarddogd`; `daemon-reload`; keep `enable --now`.
  4. Install `/etc/sudoers.d/yarddog-deploy`: `seil ALL=(root) NOPASSWD: /usr/bin/systemctl restart
     yarddogd`, mode `0440`, validated with `visudo -cf` on the new file before moving it into
     place (never write a broken sudoers file). Verify `/usr/bin/systemctl` exists first.
  5. Keep `.env` `seil:seil 0600` (unchanged). Add a comment block documenting the **hardening
     gap**: because CI == SVC == `seil`, a leaked deploy key equals `seil` and can read `.env`
     (router password + bot token); full confinement needs a separate CI user + `.env` root-owned +
     the collector moved off plain cron — deferred, not done here.
- Acceptance Criteria: Re-running the installer is idempotent and converges the live flat layout to
  release-layout without downtime; `sudo -l -U seil` shows exactly the one restart command;
  `visudo -c` parses OK; the daemon runs from `bin/release/yarddogd`.
- Pitfalls & edge cases: never overwrite an operator's real `.env` values (the existing
  set-if-unset guards stay). Do not `mv` a binary that is currently the live `ExecStart` target
  while the daemon runs from the flat path — seed first, flip the unit's `ExecStart`, then
  `daemon-reload` + `restart`. Validate the sudoers file off to the side (`visudo -cf tmpfile`)
  before `install`-ing it to `/etc/sudoers.d/`.
- Complexity: Medium

### Task 3: Update deploy sample templates + README

- Description: Point `deploy/yarddogd.service` `ExecStart` and `deploy/yarddog.cron` command at
  the `bin/release/...` channel path; refresh their comments that still describe the `/tmp`
  staging as the end state. Rewrite the README **Installation**, **Scheduling**, the daemon
  systemd note, and **Continuous integration & deployment** sections: the release-layout paths
  (`/opt/yarddog/{artifacts,bin/release}`, not `/usr/local/bin`), the now-complete autodeploy flow
  (build → `artifacts/<VID>` → flip → restart → health-gate + auto-rollback → prune), the one-time
  Pi setup (`pi5-install.sh` installs the layout + sudoers), the manual rollback one-liner
  (`ln -sfn ../artifacts/<prev> bin/release && sudo systemctl restart yarddogd`), and the CI≠SVC
  hardening note.
- Acceptance Criteria: No README/deploy sample still claims release "only stages to /tmp" or points
  at `/usr/local/bin`; the rollback command and one-time setup are documented; the CI/CD secret/var
  table is unchanged (still accurate).
- Complexity: Easy

## Execution Order

1. Task 1 (`release.yml`) — the core deploy logic.
2. Task 2 (`pi5-install.sh`) — the Pi side the workflow assumes.
3. Task 3 (docs/templates) — reflect the new contract.
4. Review (self + bash `-n` on the extracted remote scripts; `go vet ./... && go test ./...` stays
   green — no Go touched), then `git mv plans/001 + plans/002 → completed/`, squash into one commit
   on `main`, force-push-with-lease.

## Risks

- **A tag-fired deploy is only fully exercised on GitHub** (two-hop SSH, PRIME secrets, a real Pi).
  Local verification is limited to `bash -n` + careful review; first real proof is the `v0.0.0`
  push. Mitigation: liveness gate + auto-rollback means a bad binary self-heals to the prior VID.
- **Health-gate false negative**: `/ping` is liveness only, so a binary that starts and binds but
  is subtly broken passes. Accepted for a first cut; `/health/check` is reported for the operator.
- **Hardening gap** (CI == SVC == `seil`): a leaked deploy key can read `.env`. Documented, not
  fixed here; fixing it is a separate task (dedicated CI user, root-owned `.env`, collector off
  plain cron).

## Trade-offs

- **Liveness gate, not readiness gate**, for the rollback trigger: avoids a transient Nokia-login
  or SQLite blip rolling back a perfectly good binary, at the cost of not catching a
  starts-but-misbehaves binary automatically.
- **Release-layout now, hardening later**: the operator gets immutable artifacts + one-command
  rollback immediately without the larger surgery of splitting the CI and service users.
- **`workflow_dispatch` with no inputs**: reuses the exact tag-ref logic (simple, one code path) at
  the cost of requiring the dispatcher to pick a `v*` tag in the ref dropdown rather than typing a
  version.
