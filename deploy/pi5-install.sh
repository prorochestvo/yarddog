#!/usr/bin/env bash
# yarddog pi5 installer — establishes the release-layout deploy target and the
# system wiring under the org security model (see the release-layout skill).
#
# Security model, verified against beacon/hive/vpntunnel: the service runs as
# ROOT so it can read the root-only .env (secrets) and the root-owned database,
# while the CI/CD deploy user (github_aide) is confined to artifacts/ + bin/ and
# can never reach secrets, the DB, or the unit. Containment is the confined
# deploy user (Lever 1), not de-rooting the service. The objective: .env and the
# DB are readable by root alone — no non-root party gets a credential without a
# deliberate escalation to root.
#
# Layout: binaries live in immutable per-VERSION_ID dirs under
# /opt/yarddog/artifacts/ (owned by github_aide); /opt/yarddog/bin/release is the
# live channel symlink. The collector cron and the systemd unit both point at
# bin/release/<binary>, so a deploy is "upload artifacts/<VID> + flip bin/release
# + restart yarddogd" — which .github/workflows/release.yml does on a v* tag and
# `make deploy` does manually.
#
# One-time, root, re-runnable — it also serves as the seil -> root migration:
# re-running re-owns .env/DB to root, artifacts/+bin/ to github_aide, and flips
# the unit + cron to root.
#
# Run: sudo bash /opt/yarddog/pi5-install.sh
set -euo pipefail

DIR=/opt/yarddog
ENV="$DIR/.env"
CI_USER=github_aide
LAN_ADDR=192.168.1.77:8420
SYSTEMCTL=/usr/bin/systemctl

[ "$(id -u)" -eq 0 ] || { echo "FATAL: run as root (sudo bash $0)" >&2; exit 1; }
[ -f "$ENV" ] || { echo "FATAL: $ENV missing" >&2; exit 1; }
[ -x "$SYSTEMCTL" ] || { echo "FATAL: $SYSTEMCTL not found; adjust the sudoers path below" >&2; exit 1; }

# 1. CI deploy user — confined to artifacts/ + bin/, needs SSH login for scp.
#    Idempotent: skipped if the operator already created it.
if ! id "$CI_USER" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$CI_USER"
  echo "created CI deploy user $CI_USER"
fi

# 2. release-layout: the immutable artifact store + the channel symlink dir, both
#    owned by the CI user (setgid so new files inherit the group).
install -d -o "$CI_USER" -g "$CI_USER" -m 2755 "$DIR/artifacts" "$DIR/bin"

# 3. seed the channel from legacy flat binaries if it is not already populated.
#    First run after switching to release-layout: the binaries sit flat at
#    $DIR/{yarddog,yarddogd}. Move them into an initial artifacts/<VID> (CI-owned)
#    and point bin/release at it.
have_release() { [ -x "$DIR/bin/release/yarddog" ] && [ -x "$DIR/bin/release/yarddogd" ]; }
if ! have_release; then
  if [ -x "$DIR/yarddog" ] && [ -x "$DIR/yarddogd" ]; then
    VID="$(date -u +%Y%m%d%H%M%S)-r_bootstrap"
    install -d -o "$CI_USER" -g "$CI_USER" -m 2755 "$DIR/artifacts/$VID"
    mv "$DIR/yarddog" "$DIR/yarddogd" "$DIR/artifacts/$VID/"
    chown "$CI_USER:$CI_USER" "$DIR/artifacts/$VID/yarddog" "$DIR/artifacts/$VID/yarddogd"
    chmod 0755 "$DIR/artifacts/$VID/yarddog" "$DIR/artifacts/$VID/yarddogd"
    ln -sfn "../artifacts/$VID" "$DIR/bin/release"
    echo "seeded flat binaries into artifacts/$VID and pointed bin/release at it"
  else
    echo "FATAL: no bin/release channel and no legacy binaries in $DIR" >&2
    echo "  push a v* tag (CI deploys into artifacts/) or run 'make deploy', then re-run" >&2
    exit 1
  fi
fi

# 4. stop the daemon before re-owning the DB it holds open, so the migration is
#    clean; the restart in step 10 brings it back under the new root identity.
$SYSTEMCTL stop yarddogd 2>/dev/null || true

# 5. ownership split — the core of the security model:
#    - everything in $DIR EXCEPT the CI sandbox -> root:root (secrets, DB, WAL,
#      lock, logs, this script). .env drops to 0600 so only root reads it.
#    - artifacts/ + bin/ (+ their contents) -> github_aide, the deploy sandbox.
chown root:root "$DIR"
chmod 0755 "$DIR"
find "$DIR" -maxdepth 1 -mindepth 1 ! -name artifacts ! -name bin -exec chown -R root:root {} +
chmod 0600 "$ENV"
chown -R "$CI_USER:$CI_USER" "$DIR/artifacts" "$DIR/bin"
chmod 2755 "$DIR/artifacts" "$DIR/bin"

# 6. pin the keys the deploy layout depends on, only if the operator has not
#    already set them (never overwrite a real value). .env is root-owned now, so
#    root writes here. The DB path MUST live under $DIR — the one dir the systemd
#    sandbox (ReadWritePaths) and the root cron write, and beside which the
#    collector drops its flock.
if ! grep -qE '^YARDDOG_DAEMON_TOKEN=.+' "$ENV"; then
  TOKEN=$(openssl rand -hex 32)
  [ -n "$TOKEN" ] || { echo "FATAL: openssl produced no token" >&2; exit 1; }
  printf 'YARDDOG_DAEMON_TOKEN=%s\n' "$TOKEN" >> "$ENV"
  echo "generated YARDDOG_DAEMON_TOKEN"
fi
if ! grep -qE '^YARDDOG_DAEMON_ADDR=.+' "$ENV"; then
  printf 'YARDDOG_DAEMON_ADDR=%s\n' "$LAN_ADDR" >> "$ENV"
  echo "set YARDDOG_DAEMON_ADDR=$LAN_ADDR"
fi
if ! grep -qE '^YARDDOG_DB_PATH=.+' "$ENV"; then
  printf 'YARDDOG_DB_PATH=%s\n' "$DIR/yarddog.sqlite" >> "$ENV"
  echo "set YARDDOG_DB_PATH=$DIR/yarddog.sqlite"
fi
chown root:root "$ENV"
chmod 0600 "$ENV"

# 7. collector cron — runs as ROOT (same identity as the daemon), soft check every
#    5 minutes. Runs the channel symlink, so a deploy's flip takes effect on the
#    next tick with no cron edit. Add a nightly `--hard-reboot` line if wanted.
cat > /etc/cron.d/yarddog <<EOF
SHELL=/bin/sh
PATH=/usr/local/bin:/usr/bin:/bin
MAILTO=""
*/5 * * * *  root  $DIR/bin/release/yarddog >> $DIR/yarddog.log 2>&1
EOF
chmod 644 /etc/cron.d/yarddog

# 8. daemon systemd unit — runs as ROOT from the channel symlink, read-only API,
#    hardened sandbox (ReadWritePaths=$DIR covers the DB + its WAL siblings).
cat > /etc/systemd/system/yarddogd.service <<EOF
[Unit]
Description=yarddog query daemon (read-only JSON API over the LAN)
Documentation=https://github.com/prorochestvo/yarddog
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV
ExecStart=$DIR/bin/release/yarddogd
Restart=on-failure
RestartSec=5s
User=root
Group=root
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DIR
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF

# 9. passwordless restart for the CI deploy user — exactly one command, no
#    wildcards, absolute systemctl path so sudo matches the deploy's
#    `sudo /usr/bin/systemctl restart yarddogd`. Validate off to the side: a
#    malformed file in /etc/sudoers.d can lock out sudo entirely.
SUDOERS=/etc/sudoers.d/yarddog-deploy
TMP_SUDOERS="$(mktemp)"
printf '%s ALL=(root) NOPASSWD: %s restart yarddogd\n' "$CI_USER" "$SYSTEMCTL" > "$TMP_SUDOERS"
if visudo -cf "$TMP_SUDOERS" >/dev/null; then
  install -m 0440 -o root -g root "$TMP_SUDOERS" "$SUDOERS"
  rm -f "$TMP_SUDOERS"
  echo "installed $SUDOERS ($CI_USER NOPASSWD: $SYSTEMCTL restart yarddogd)"
else
  rm -f "$TMP_SUDOERS"
  echo "FATAL: generated sudoers failed visudo -c; not installed" >&2
  exit 1
fi

# 10. seed the CI user's authorized_keys from seil's, so the Mac/CI key that
#     already reaches this host can deploy as github_aide too. Never overwrites an
#     existing set (the operator may have installed a dedicated deploy key).
CI_HOME="$(getent passwd "$CI_USER" | cut -d: -f6)"
if [ -n "$CI_HOME" ] && [ ! -s "$CI_HOME/.ssh/authorized_keys" ] && [ -s /home/seil/.ssh/authorized_keys ]; then
  install -d -o "$CI_USER" -g "$CI_USER" -m 0700 "$CI_HOME/.ssh"
  install -o "$CI_USER" -g "$CI_USER" -m 0600 /home/seil/.ssh/authorized_keys "$CI_HOME/.ssh/authorized_keys"
  echo "seeded $CI_USER authorized_keys from seil (for make deploy / CI)"
fi

# 11. apply: reload units, enable, restart onto the new root identity/binary.
$SYSTEMCTL daemon-reload
$SYSTEMCTL enable yarddogd
$SYSTEMCTL restart yarddogd

# 12. report — names only, never echo secret values.
echo "=== yarddogd status ==="
$SYSTEMCTL --no-pager --full status yarddogd | head -n 12 || true
echo "=== ownership (secrets/DB root-only; artifacts/bin = $CI_USER) ==="
ls -la "$DIR" | grep -E '\.env|sqlite|artifacts|bin|\.lock|\.log' || true
echo "=== bin/release -> $(readlink "$DIR/bin/release" 2>/dev/null || echo '(unset)') ==="
echo "=== keys present in $ENV (names only) ==="
grep -oE '^YARDDOG_[A-Z_]+=' "$ENV" | sort
echo "=== done. collector runs bin/release/yarddog as root every 5 minutes. ==="
