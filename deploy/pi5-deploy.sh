#!/usr/bin/env bash
# Manual deploy of the local arm64 build to pi5 over a direct SSH as the CI deploy
# user (github_aide), mirroring the CI release-layout (.github/workflows/release.yml)
# for when a tag push isn't wanted. Invoked by `make deploy`, which first runs
# `make build OS=arm64 VERSION=<VERSION_ID>` so the daemon embeds the same id
# /health/check reports.
#
# The deploy user is confined to artifacts/ + bin/ and CANNOT read the root-only
# .env or the DB (org security model — see the release-layout skill). So nothing
# here reads .env: the health-gate hits /ping (liveness, ungated — no token) at a
# fixed loopback address, never /health/check (token-gated, and the token lives in
# the root-only .env this user can't read).
#
# Flow: upload both binaries into an immutable /opt/yarddog/artifacts/<VERSION_ID>/
# dir, verify their SHA256, flip bin/release to it, `sudo systemctl restart yarddogd`
# (the one grant github_aide has), gate on /ping (roll back to the previous release
# if it never answers), then prune old artifacts.
#
# Requires: key-based SSH as github_aide (pi5-install.sh seeds its authorized_keys),
# the layout + NOPASSWD systemctl-restart grant from pi5-install.sh already in place.
set -euo pipefail

# deploy identity: the confined CI user, not seil. Override host via PI5_DEPLOY.
SSH_HOST="${PI5_DEPLOY:-github_aide@pi5}"
# loopback address the daemon binds; /ping is ungated so no token is needed.
PING_ADDR="${PI5_PING_ADDR:-127.0.0.1:5000}"
KEEP=5

VERSION_ID="${1:?usage: pi5-deploy.sh <VERSION_ID> (run via 'make deploy')}"
COLLECTOR="build/yarddog-arm64"
DAEMON="build/yarddogd-arm64"

for f in "$COLLECTOR" "$DAEMON"; do
  [ -x "$f" ] || { echo "missing $f — run: make build OS=arm64" >&2; exit 1; }
done

# local SHA256 (macOS: shasum) to check against the Pi's sha256sum after transfer.
sha() { shasum -a 256 "$1" | awk '{print $1}'; }
C_SHA="$(sha "$COLLECTOR")"
D_SHA="$(sha "$DAEMON")"

ART="/opt/yarddog/artifacts/$VERSION_ID"
echo ">> deploying $VERSION_ID -> $SSH_HOST:$ART"
ssh "$SSH_HOST" "mkdir -p '$ART'"
scp -q "$COLLECTOR" "$SSH_HOST:$ART/yarddog"
scp -q "$DAEMON"    "$SSH_HOST:$ART/yarddogd"

# the single privileged, state-changing step: verify -> flip -> restart -> gate ->
# rollback -> prune. Positional args to `bash -s` keep the heredoc literal (quoted).
ssh "$SSH_HOST" bash -s -- "$VERSION_ID" "$C_SHA" "$D_SHA" "$PING_ADDR" "$KEEP" <<'REMOTE'
set -euo pipefail
VERSION_ID="$1"; C_SHA="$2"; D_SHA="$3"; ADDR="$4"; KEEP="$5"
APP=/opt/yarddog
ART="$APP/artifacts/$VERSION_ID"

check() {
  local f="$1" want="$2" got
  got="$(sha256sum "$f" | awk '{print $1}')"
  if [ "$got" != "$want" ]; then
    echo "ERROR: SHA256 mismatch for $f (uploaded binary corrupted/truncated)"
    echo "  want: $want"
    echo "  got:  $got"
    exit 1
  fi
  chmod +x "$f"
  echo "verified $(basename "$f")"
}
check "$ART/yarddog"  "$C_SHA"
check "$ART/yarddogd" "$D_SHA"

PREV="$(readlink "$APP/bin/release" 2>/dev/null || true)"
ln -sfn "../artifacts/$VERSION_ID" "$APP/bin/release"
sudo /usr/bin/systemctl restart yarddogd || echo "warning: restart exited non-zero, continuing to health gate"

# liveness gate on /ping — ungated, so the deploy user needs no token and never
# touches the root-only .env.
live=0
for _ in $(seq 1 10); do
  if curl -fsS -o /dev/null --max-time 5 "http://$ADDR/ping"; then live=1; break; fi
  sleep 1
done
if [ "$live" -ne 1 ]; then
  echo "ERROR: yarddogd did not answer /ping at http://$ADDR after restart"
  if [ -n "$PREV" ] && [ "$PREV" != "../artifacts/$VERSION_ID" ]; then
    echo "rolling back bin/release -> $PREV"
    ln -sfn "$PREV" "$APP/bin/release"
    sudo /usr/bin/systemctl restart yarddogd || echo "warning: rollback restart exited non-zero"
    echo "rolled back to $(basename "$PREV")"
  else
    echo "no previous release to roll back to"
  fi
  exit 1
fi
echo "released $VERSION_ID on $(hostname): /ping ok at http://$ADDR"

# prune: keep the newest KEEP artifact dirs; never delete one a bin/* channel still
# points at. Non-fatal — a prune hiccup must not fail an otherwise-good deploy.
in_use="$(for l in "$APP"/bin/*; do [ -e "$l" ] || continue; basename "$(readlink -f "$l")"; done | sort -u)"
ls -1dt "$APP"/artifacts/*/ 2>/dev/null | tail -n +$((KEEP + 1)) | while read -r d; do
  v="$(basename "$d")"
  if printf '%s\n' "$in_use" | grep -qxF "$v"; then continue; fi
  rm -rf "$d" && echo "pruned old artifact $v"
done
true
REMOTE
echo ">> done: $VERSION_ID is live on $SSH_HOST (bin/release flipped, yarddogd restarted)"
