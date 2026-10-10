# Task Breakdown

## Overview

Align yarddog's on-server security model with the reference deployment (`beacon` on
be-happy.kz), whose invariant is: **`.env` and the database are readable only by root; the
CI/CD deploy user is a separate non-root account (`github_aide`) confined to `artifacts/`
+ `bin/` and cannot reach secrets, the DB, or the unit; the service itself runs as root**.
Containment comes from the confined deploy user, not from de-rooting the service.

Verified against the live beacon host: `beacon.service` is `User=root`; `.env`, `*.sqlite`,
wrapper `*.sh`, `logs/` are `root:root` (`.env`/DB `0600`); `artifacts/` + `bin/` and the
binaries inside are `github_aide:github_aide` `0755` (setgid). A `sudoers.d/beacon-deploy`
grants `github_aide` a narrow `systemctl restart` only.

yarddog today violates this: `yarddogd` runs as `seil`, `.env`/DB are `seil`-owned (so `seil`
reads every secret), the deploy user is also `seil`, and there is no `github_aide` on pi5. A
leaked deploy key = `seil` = full secret disclosure. This task closes that.

## Assumptions

- The collector (cron) and daemon (systemd) both move to **root**, so both read the
  root-only `.env` and write the root-owned DB — no permission split between them.
- `github_aide` (system user, uid to match beacon's convention) ships builds over SSH into
  `artifacts/`+`bin/` only; it never reads `.env`/DB. The deploy health-gate therefore uses
  `/ping` (ungated) with the address passed as a parameter, not read from `.env`.
- The one-time migration runs as root on the live pi5 (the user executes the privileged
  commands; the assistant cannot `sudo` over BatchMode SSH).
- `DB_PATH=/opt/yarddog/yarddog.sqlite` stays; ownership flips `seil` → `root`.

## Tasks

### Task 1: systemd unit — run the daemon as root
- Description: `deploy/yarddogd.service` → `User=root`/`Group=root` (mirror beacon's unit +
  its "SVC=root; CI confined" comment). Keep `EnvironmentFile=/opt/yarddog/.env`,
  `ExecStart=/opt/yarddog/bin/release/yarddogd`.
- Acceptance: `systemctl show -p User yarddogd` = `root` after migration; `/ping` answers.
- Complexity: Easy

### Task 2: pi5-install.sh — beacon permission model + github_aide
- Description: Rewrite the installer to: create `github_aide` (system, nologin); own
  `artifacts/`+`bin/` as `github_aide:github_aide` (setgid `2755`); force `.env` root:600,
  DB + `yarddog.log` + lock root-owned; base dir root:755; install `sudoers.d/yarddog-deploy`
  (`github_aide` NOPASSWD `systemctl restart yarddogd`); seed `github_aide`'s
  `authorized_keys` (Mac + CI deploy pubkeys). Idempotent + re-runnable so it doubles as the
  migration. Drop the old HARDENING-GAP note (gap is now closed).
- Acceptance: after a run, `ls -la /opt/yarddog` matches the beacon ownership table; `sudo -l -U github_aide` shows only the restart grant.
- Pitfalls: chown of a live DB — stop the daemon first, chown, restart; the collector flock
  serializes any cron tick mid-migration.
- Complexity: Hard

### Task 3: collector cron — run as root
- Description: `deploy/yarddog.cron` already uses `root`; ensure the live host runs the
  collector as **root** (not `seil`, not a personal crontab) at `*/5`, reading root:600 `.env`
  and writing the root-owned DB. Canonical form is `/etc/cron.d/yarddog` (root).
- Acceptance: `/etc/cron.d/yarddog` present, `root` field, `*/5`; a tick writes the DB with
  root ownership and no permission error in `yarddog.log`.
- Complexity: Easy

### Task 4: pi5-deploy.sh — deploy as github_aide, .env-free health-gate
- Description: Deploy over `ssh github_aide@pi5` (not `seil`). Remove all `.env` reads (the
  deploy user can't read root:600 `.env`): pass the daemon address as a parameter
  (default `127.0.0.1:5000`) and gate on `/ping` only (ungated — no token). Keep
  verify-sha → flip `bin/release` → `sudo systemctl restart yarddogd` → `/ping` gate →
  rollback → prune. `make deploy` targets the `github_aide` SSH identity.
- Acceptance: `make deploy` from the Mac ships + flips + restarts + gates green, never
  touching `.env`.
- Complexity: Medium

### Task 5: CI release.yml — deploy user + .env-free gate (consistency)
- Description: Point the pi5 deploy user at `github_aide`; replace the `env_val` `.env` reads
  in the promote step with a parameter address + `/ping` gate, matching Task 4. (CI is
  unproven pending PRIME secrets — change for consistency, not activation.)
- Acceptance: workflow lint clean; promote step reads no `.env`.
- Complexity: Medium

### Task 6: Docs — README + CLAUDE.md
- Description: Document the root-service + confined-`github_aide` model, the root-only
  `.env`/DB, and `make deploy` under `github_aide`. Correct the old "runs as seil / unprivileged"
  claims.
- Acceptance: no stale `seil`-runtime / HARDENING-GAP claims remain.
- Complexity: Easy

## Execution Order

Repo first (1 → 6, no server impact), reviewed, committed. Then the live migration on pi5
(Task 2's installer run + Task 3 cron + key seeding), executed by the user as root, verified
by `/ping` + a clean collector tick. `make deploy` (Task 4) last, once `github_aide` exists.

## Risks

- **Live migration downtime.** chown of the DB requires stopping `yarddogd`, chowning, and
  restarting; a few seconds of daemon downtime. The collector flock prevents a mid-migration
  cron tick from racing.
- **Lock-out.** If `github_aide`'s `authorized_keys` is mis-seeded, `make deploy` can't reach
  pi5. `seil` SSH stays as the fallback admin path; the migration never removes it.
- **CI drift.** release.yml and pi5-deploy.sh must gate identically (both `/ping`, no `.env`),
  or a tag deploy behaves differently from a manual one.

## Trade-offs

- Service as root (vs. de-rooted `seil`): matches the org-wide reference (beacon, hive,
  vpntunnel) and the actual goal — secrets/DB reachable only by root — at the cost of a
  larger blast radius if the *service binary* is compromised. The org accepts this; the
  confined deploy user is the primary containment lever.
- Collector on cron-as-root (vs. a systemd timer that de-roots env reads): keeps the existing
  cron mechanism; revisiting to a timer is the future hardening step, not required here.
