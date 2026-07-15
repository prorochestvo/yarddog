#!/usr/bin/env bash
#
# yarddog dashboard nginx cutover  (plans/011 Task 4 + overview proxy for plans/010+012)
#
# Switches the pi5 vhost so GET / is served by the yarddog daemon's embedded
# dashboard instead of the static /var/www/.../index.html, and adds the
# token-injecting proxy for GET /api/v1/overview the weekly section consumes.
#
# This is the runnable companion to nginx-dashboard.sample.conf (which documents
# the same location blocks). RUN AS ROOT, AND ONLY AFTER a daemon that serves /
# and /api/v1/overview is deployed (v0.0.3+, via a v* tag through release.yml or
# `make deploy`):
#     sudo bash deploy/nginx-cutover.sh
#
# It is SAFE to run early or twice: it refuses to touch nginx until the new
# daemon actually serves the embedded dashboard at / (ordering guard),
# it is idempotent (no-op if already applied), it backs the vhost up before any
# edit, validates with `nginx -t`, and auto-rolls-back if the post-reload checks
# fail. Nothing is deleted; the dead index.html is only reported, not removed.
set -euo pipefail

VHOST=/etc/nginx/sites-available/dev.seilbekskindirov.pi5
DAEMON=127.0.0.1:5000              # loopback the daemon binds (matches the live /api/v1/* blocks)
VHOST_HOST=pi5.seilbekskindirov.dev
VHOST_LISTEN=127.0.0.1:5588        # this vhost's plain-http listen (behind the cloudflare tunnel)
MARKER='yarddog status --watch'    # stable string in the embedded dashboard HTML

die(){ echo "ABORT: $*" >&2; exit 1; }

# localhost probes must ignore any ambient proxy and curlrc — under sudo, curl
# runs as root and reads /root/.curlrc, so a proxy configured there would reroute
# a 127.0.0.1 request and the probe would test the proxy, not the daemon. -q
# ignores curlrc; --noproxy '*' ignores http(s)_proxy for every host.
cget(){ curl -q --noproxy '*' "$@"; }

# one scratch file for HTTP probes: fetch with `-o "$PROBE"` and check the FILE
# (grep/wc), never a captured variable — a piped `grep -q` SIGPIPEs under
# pipefail, and `$(...)` capture proved flakier than a file under sudo here.
PROBE="$(mktemp)"
trap 'rm -f "$PROBE"' EXIT

[ "$(id -u)" = 0 ] || die "run as root (sudo bash $0)."
[ -f "$VHOST" ]    || die "vhost not found: $VHOST"

# idempotency: the cutover installs a `@dashboard_down` block; if it is already
# there, we have nothing to do.
if grep -q '@dashboard_down' "$VHOST"; then
  echo "already applied (@dashboard_down present in $VHOST) — nothing to do."
  exit 0
fi

# precondition: the exact static-root block we are about to replace is present.
grep -q 'try_files /index.html =404;' "$VHOST" \
  || die "the expected 'location = /' try_files block is not in $VHOST — config drifted; inspect it by hand."

# ORDERING GUARD — the *new* daemon must already be deployed and answering.
# The previously-shipped daemon has no ungated GET / (everything but /ping was
# token-gated), so a 200 carrying the dashboard marker proves the release that
# added the embedded UI — and the /api/v1/overview it ships alongside — has landed
# before we flip nginx. /api/v1/overview is itself token-gated and cannot be probed
# here without the root-only token; it is verified through nginx after the reload.
echo "pre-flight: probing the daemon on http://$DAEMON ..."
# retry a few times: a single flaky response must not abort the cutover, but a
# genuinely old/absent daemon still fails all attempts and blocks the flip.
ok=0
for attempt in 1 2 3 4 5; do
  # fetch to a file and grep the FILE (never a piped grep -q or a captured var):
  # a piped grep -q SIGPIPEs under pipefail, and variable capture flapped under sudo.
  if cget -fsS -o "$PROBE" --max-time 5 "http://$DAEMON/" && grep -qF "$MARKER" "$PROBE"; then
    ok=1; break
  fi
  echo "  attempt $attempt/5: / not serving the dashboard marker yet, retrying in 2s ..."
  sleep 2
done
[ "$ok" -eq 1 ] \
  || die "daemon GET / did not return the embedded dashboard (marker '$MARKER') after 5 tries — is the new daemon (v0.0.3+) deployed and healthy on $DAEMON?"
echo "  ok: the new daemon serves the embedded dashboard at /."

# backup the vhost (mirrors the existing .bak.<ts> convention in sites-available).
ts="$(date -u +%Y%m%dT%H%M%SZ)"
bak="$VHOST.bak.$ts"
cp -a "$VHOST" "$bak"
echo "backup: $bak"

# patch: one auditable exact-string replacement of the static-root block with
# (a) the /api/v1/overview proxy, (b) the /-serves-daemon proxy, (c) its down page.
python3 - "$VHOST" <<'PY'
import sys
path = sys.argv[1]
src = open(path).read()

OLD = """    # welcome page when no port is given
    location = / {
        try_files /index.html =404;
    }"""

NEW = """    # yarddog weekly overview aggregation (plans/010+012), for the dashboard's
    # retrospective section. same token injection + down-fallback as the
    # /api/v1/metrics and /api/v1/pings blocks; query string forwarded so the
    # page can pass ?bucket= / ?since= through to the daemon.
    location = /api/v1/overview {
        include /etc/nginx/snippets/yarddog-auth.conf;

        proxy_pass http://127.0.0.1:5000/api/v1/overview$is_args$args;
        proxy_set_header Host $host;
        proxy_http_version 1.1;

        proxy_connect_timeout 2s;
        proxy_read_timeout    8s;

        add_header Cache-Control "no-store" always;

        proxy_intercept_errors on;
        error_page 401 403 404 500 502 503 504 = @metrics_down;
    }

    # serve the dashboard from the daemon (plans/011): the daemon owns the page
    # now, so the UI version always equals the API version. a hard-down daemon
    # would make / a bare 502, so intercept it with a tiny holding page (the
    # daemon has Restart=on-failure and deploys are /ping-gated -> transient).
    location = / {
        proxy_pass http://127.0.0.1:5000/;
        proxy_set_header Host $host;
        proxy_http_version 1.1;

        proxy_connect_timeout 2s;
        proxy_read_timeout    5s;

        proxy_intercept_errors on;
        error_page 502 503 504 = @dashboard_down;
    }

    location @dashboard_down {
        default_type text/html;
        add_header Cache-Control "no-store" always;
        return 503 '<!doctype html><meta charset=utf-8><title>pi5 · status</title><body style="background:#0b0b0c;color:#e5e5e5;font:14px monospace;padding:2rem"><p>&#36; yarddog status</p><p>dashboard is starting &mdash; the daemon is not answering yet.</p>';
    }"""

n = src.count(OLD)
if n != 1:
    sys.stderr.write("patch: expected exactly one static-root block, found %d — aborting without writing\n" % n)
    sys.exit(3)

open(path, "w").write(src.replace(OLD, NEW))
print("patched:", path)
PY

# validate; restore and bail on any syntax error.
if ! nginx -t; then
  echo "nginx -t FAILED -> restoring $bak" >&2
  cp -a "$bak" "$VHOST"
  die "invalid nginx config; original restored from $bak."
fi

systemctl reload nginx
echo "reloaded nginx."

# post-verify through nginx on the public path; auto-roll-back on any failure.
rollback(){ echo "post-verify FAILED -> restoring $bak" >&2; cp -a "$bak" "$VHOST"; systemctl reload nginx; die "rolled back: $*"; }

# nginx reload is asynchronous: for a brief window an old worker can still answer
# with the pre-cutover config (no overview block; / served statically from
# /var/www, which carries the SAME dashboard marker — so a marker check alone
# cannot tell old from new). /api/v1/overview exists ONLY in the new config, so
# poll it until it is live: that is the unambiguous "new config is in effect"
# signal, and only then are the remaining assertions meaningful.
echo "post-verify: waiting for the new config to take effect ..."
ov_ok=0
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  cget -fsS -o "$PROBE" --max-time 9 -H "Host: $VHOST_HOST" "http://$VHOST_LISTEN/api/v1/overview" 2>/dev/null || true
  if grep -q '"window"' "$PROBE" 2>/dev/null; then ov_ok=1; break; fi
  echo "  attempt $attempt/10: overview not live through nginx yet, waiting 1s ..."
  sleep 1
done
[ "$ov_ok" -eq 1 ] || rollback "/api/v1/overview through nginx never returned a window object (waited ~10s after reload)"

echo "post-verify: checking GET / through nginx ..."
# retry like the probes above: the / marker read can transiently miss under some
# runtime contexts even though the daemon serves it deterministically (verified
# 60/60 direct). overview already proved nginx->daemon is live, so tolerate a
# flaky read here rather than roll back a good cutover on one miss.
slash_ok=0
for attempt in 1 2 3 4 5 6; do
  if cget -fsS -o "$PROBE" --max-time 6 -H "Host: $VHOST_HOST" "http://$VHOST_LISTEN/" && grep -qF "$MARKER" "$PROBE"; then
    slash_ok=1; break
  fi
  echo "  attempt $attempt/6: / marker not seen yet, waiting 1s ..."
  sleep 1
done
if [ "$slash_ok" -ne 1 ]; then
  code="$(cget -sS -o /dev/null -w '%{http_code}' --max-time 6 -H "Host: $VHOST_HOST" "http://$VHOST_LISTEN/" 2>/dev/null || echo ERR)"
  rollback "GET / through nginx never showed the dashboard marker after 6 tries (last HTTP status=$code)"
fi
if grep -qiF bearer "$PROBE"; then
  rollback "a bearer token leaked into the / page"
fi
echo "  ok: dashboard + overview served through nginx; no token leak."

echo
echo "CUTOVER COMPLETE."
echo "  backup kept at : $bak"
echo "  the static index.html at /var/www/dev.seilbekskindirov.pi5/index.html is now DEAD"
echo "  (location = / no longer serves it; 403.html/404.html still come from /var/www)."
echo
echo "  optional (plans/011 Task 5) — retire it reversibly, changes nothing observable:"
echo "     mv /var/www/dev.seilbekskindirov.pi5/index.html \\"
echo "        /var/www/dev.seilbekskindirov.pi5/index.html.retired.$ts"
