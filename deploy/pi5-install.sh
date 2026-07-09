#!/usr/bin/env bash
# yarddog pi5 installer — establishes the release-layout deploy target and the
# system wiring for the monitor-only host. One-time, root, re-runnable.
#
# Layout (house release-layout): binaries live in immutable, per-VERSION_ID dirs
# under /opt/yarddog/artifacts/, and /opt/yarddog/bin/release is a symlink to the
# live one. The collector cron and the systemd unit both point at
# bin/release/<binary>, so a deploy is "upload a new artifacts/<VID> + flip the
# symlink + restart the daemon" — which the CI release pipeline
# (.github/workflows/release.yml) does automatically on a v* tag. This installer
# only has to run once, so the layout, the unit, and the passwordless restart
# grant exist before the first autodeploy.
#
# Runtime is unprivileged: both the collector (cron) and the daemon (systemd) run
# as user seil and share the WAL SQLite at /opt/yarddog/yarddog.sqlite; the
# collector's flock (yarddog.lock) sits in the same directory, so there is no
# /var/run provisioning. Root is needed only for this install (files under /etc,
# the sudoers grant, ownership).
#
# HARDENING GAP (documented, not fixed here): the CI deploy user and the service
# user are the SAME user (seil), so the release-layout confinement — CI writes
# only artifacts/+bin/, .env root-owned — does NOT hold. A leaked deploy key
# equals seil and can read /opt/yarddog/.env (router password + bot token). Full
# confinement needs a dedicated CI user, a root-owned .env, and the collector
# moved off plain cron onto a systemd timer (so root, not seil, reads the env
# file). Deferred deliberately.
#
# Run: sudo bash /opt/yarddog/pi5-install.sh
set -euo pipefail

RUN_USER=seil
DIR=/opt/yarddog
ENV="$DIR/.env"
LAN_ADDR=192.168.1.77:8420
SYSTEMCTL=/usr/bin/systemctl

[ -f "$ENV" ] || { echo "FATAL: $ENV missing" >&2; exit 1; }
[ -x "$SYSTEMCTL" ] || { echo "FATAL: $SYSTEMCTL not found; adjust the sudoers path below" >&2; exit 1; }

# 1. release-layout: the immutable artifact store + the channel symlink dir.
install -d -o "$RUN_USER" -g "$RUN_USER" -m 0755 "$DIR/artifacts" "$DIR/bin"

# 2. seed the channel from the current binaries if it is not already populated.
#    First run after switching to release-layout: the binaries sit flat at
#    $DIR/{yarddog,yarddogd} — move them into an initial artifacts/<VID> and point
#    bin/release at it. A running daemon keeps its old inode until the restart in
#    step 8, so the move is safe. With neither a release channel nor legacy
#    binaries there is nothing to run yet — push a v* tag (CI populates artifacts)
#    and re-run.
have_release() { [ -x "$DIR/bin/release/yarddog" ] && [ -x "$DIR/bin/release/yarddogd" ]; }
if ! have_release; then
  if [ -x "$DIR/yarddog" ] && [ -x "$DIR/yarddogd" ]; then
    VID="$(date -u +%Y%m%d%H%M%S)-r_bootstrap"
    install -d -o "$RUN_USER" -g "$RUN_USER" -m 0755 "$DIR/artifacts/$VID"
    mv "$DIR/yarddog" "$DIR/yarddogd" "$DIR/artifacts/$VID/"
    chmod 0755 "$DIR/artifacts/$VID/yarddog" "$DIR/artifacts/$VID/yarddogd"
    ln -sfn "../artifacts/$VID" "$DIR/bin/release"
    echo "seeded flat binaries into artifacts/$VID and pointed bin/release at it"
  else
    echo "FATAL: no bin/release channel and no legacy binaries in $DIR" >&2
    echo "  push a v* tag (CI deploys into artifacts/ and flips bin/release), then re-run" >&2
    exit 1
  fi
fi

# 3. ownership + perms: self-contained, seil-owned, .env not world-readable.
chown -R "$RUN_USER:$RUN_USER" "$DIR"
chmod 0755 "$DIR"
chmod 0600 "$ENV"

# 4. pin the keys the deploy layout depends on, only if the operator has not
#    already set them (never overwrite a real value): the daemon token, the bind
#    address, and the DB path. The DB path MUST live under $DIR — the one dir the
#    systemd sandbox (ReadWritePaths) and the seil cron can write, and beside which
#    the collector drops its flock. Left unset it falls back to the compiled
#    default /var/lib/yarddog, which the sandbox forbids (daemon crash-loop).
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
chown "$RUN_USER:$RUN_USER" "$ENV"

# 5. collector cron — monitor-only, so only the soft check (twice an hour); the
#    nightly hard-reboot line stays omitted until we go live. Runs the channel
#    symlink, so a deploy's flip takes effect on the next tick with no cron edit.
cat > /etc/cron.d/yarddog <<EOF
SHELL=/bin/sh
PATH=/usr/local/bin:/usr/bin:/bin
MAILTO=""
7,37 * * * *  $RUN_USER  $DIR/bin/release/yarddog >> $DIR/yarddog.log 2>&1
EOF
chmod 644 /etc/cron.d/yarddog

# 6. daemon systemd unit — runs as seil from the channel symlink, read-only API,
#    hardened sandbox. ExecStart is bin/release/yarddogd (never a concrete VID),
#    so a deploy is flip + restart.
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
User=$RUN_USER
Group=$RUN_USER
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

# 7. passwordless restart for the CI deploy user — exactly one command, no
#    wildcards, absolute systemctl path so sudo matches the release pipeline's
#    `sudo /usr/bin/systemctl restart yarddogd`. Validate off to the side before
#    installing: a malformed file in /etc/sudoers.d can lock out sudo entirely.
SUDOERS=/etc/sudoers.d/yarddog-deploy
TMP_SUDOERS="$(mktemp)"
printf '%s ALL=(root) NOPASSWD: %s restart yarddogd\n' "$RUN_USER" "$SYSTEMCTL" > "$TMP_SUDOERS"
if visudo -cf "$TMP_SUDOERS" >/dev/null; then
  install -m 0440 -o root -g root "$TMP_SUDOERS" "$SUDOERS"
  rm -f "$TMP_SUDOERS"
  echo "installed $SUDOERS ($RUN_USER NOPASSWD: $SYSTEMCTL restart yarddogd)"
else
  rm -f "$TMP_SUDOERS"
  echo "FATAL: generated sudoers failed visudo -c; not installed" >&2
  exit 1
fi

# 8. apply: reload units, enable, restart onto the new ExecStart/binary.
$SYSTEMCTL daemon-reload
$SYSTEMCTL enable yarddogd
$SYSTEMCTL restart yarddogd

# 9. report — names only, never echo secret values.
echo "=== yarddogd status ==="
$SYSTEMCTL --no-pager --full status yarddogd | head -n 12 || true
echo "=== bin/release -> $(readlink "$DIR/bin/release" 2>/dev/null || echo '(unset)') ==="
echo "=== keys present in $ENV (names only) ==="
grep -oE '^YARDDOG_[A-Z_]+=' "$ENV" | sort
echo "=== done. collector runs bin/release/yarddog at the next :07/:37. ==="
