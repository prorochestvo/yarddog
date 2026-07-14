# Task Breakdown

## Overview

Relocate the status dashboard from the hand-maintained `/var/www/…/index.html` into
this repo and ship it inside `yarddogd` via `go:embed`. The daemon serves the
single-page dashboard at `GET /` (ungated — the HTML holds no secrets); nginx proxies
`/` to the daemon and keeps its existing token-injecting `/api/v1/*` proxy unchanged.

Why embed rather than a separate web artifact: the release pipeline already versions,
ships, atomically flips, health-gates, and rolls back **the daemon binary**. Embedding
the UI makes it ride all of that for free — **UI version always equals API version** (no
skew between the page and the contract it consumes), and the hand-run `sudo cp` to
`/var/www` disappears. The deploy user `github_aide` is confined to `artifacts/`+`bin/`
and cannot write `/var/www` or `/etc/nginx`, so a CI-deployed static web artifact would
need to widen that confinement; embedding in the binary sidesteps it entirely.

This is a **relocation with zero UI behaviour change** — importing today's deployed page
verbatim and changing only *how it is served*. The weekly-overview feature is
`plans/012`, built on top of this.

## Assumptions

- The current deployed `/var/www/dev.seilbekskindirov.pi5/index.html` (self-contained,
  dependency-free, offline-capable, including the pending "waiting for data" loader) is
  imported **verbatim** as the repo baseline. It is the source of truth; there is no
  other copy.
- The daemon serves the page at `GET /{$}` (Go 1.22 exact-root pattern), **ungated** —
  it joins `/ping` in the `withAuth` exemption. External access stays gated by nginx +
  Cloudflare Access; the LAN daemon serving public HTML is acceptable (the page carries
  no token, and `/api/*` stays token-gated).
- The embedded asset lives under `gateway/httpapi` (the daemon's inbound-HTTP adapter
  owns its own UI); `go:embed` sits in that package. No new top-level package — honours
  "no speculative structure".
- nginx is reconfigured **once**, out-of-band by the operator (like `deploy/pi5-install.sh`
  established the layout), to proxy `/` to the daemon instead of serving `/var/www`. This
  is not a CI action — `github_aide` cannot touch nginx.
- Pure-Go `CGO_ENABLED=0` build is unaffected: `embed` is stdlib.

## Tasks

### Task 1: Import the dashboard into the repo
- Description: add `gateway/httpapi/web/index.html`, a **verbatim** copy of the live
  deployed page (fetch it from the Pi: `/var/www/dev.seilbekskindirov.pi5/index.html`).
  No edits — this task only relocates. Record the source path in a top-of-file comment
  is unnecessary (HTML), but note the provenance in the commit body.
- Acceptance Criteria: the file is byte-identical to the deployed page at import time;
  `node --check` on its inline `<script>` passes (it already ships in production).
- Pitfalls & edge cases: the page must stay **self-contained** (no external fetches —
  CSP/offline requirement); do not "tidy" it here.
- Complexity: Easy

### Task 2: Embed + serve (`gateway/httpapi/ui.go`)
- Description: `//go:embed web/index.html` into a package var; add a handler that serves
  it at `GET /{$}` with `Content-Type: text/html; charset=utf-8` and a sensible
  `Cache-Control` (short, so a deploy is visible without a hard refresh). Register it in
  `server.go`'s `register`. Public-surface-first file order per house rules.
- Acceptance Criteria: `GET /` returns the page (200, `text/html`); a versioned artifact
  now contains the UI; `go vet` clean.
- Pitfalls & edge cases: use `GET /{$}` (exact root), not `GET /` — a bare `GET /` is a
  catch-all that would shadow nothing (more specific `/api/...` patterns still win under
  Go 1.22 precedence) but would serve the page for every unknown path; `{$}` keeps
  unknown paths a clean 404. Serve the bytes directly (`http.ServeContent` with a fixed
  modtime, or `w.Write`), not `http.FileServer` over the FS (which would also expose
  directory listing semantics).
- Complexity: Easy

### Task 3: Ungate the UI route (`gateway/httpapi/auth.go`)
- Description: the root path joins `/ping` as exempt from the shared-token check. Follow
  the existing pattern — a named `const` referenced by both the route registration and
  the `withAuth` exemption, so a rename can't silently gate the page or ungate a real
  route.
- Acceptance Criteria: `GET /` needs no token; `GET /api/v1/*` still 401s without one;
  the exemption set is `{/ping, /}` and nothing else.
- Pitfalls & edge cases: keep the exemption an exact-match set — do not switch to a
  prefix check (a prefix would accidentally ungate anything under `/`).
- Complexity: Easy

### Task 4: nginx reconfig (`deploy/` note + operator step)
- Description: document and provide the nginx change: the `location /` block **proxies to
  the daemon** (`proxy_pass http://127.0.0.1:<daemon>;`) instead of `root /var/www …`;
  the `location /api/v1/` block (token injection via the auth snippet) is unchanged; keep
  `absolute_redirect off;` and the port-allowlist blocks intact. Add it as a commented
  reference in `deploy/` (e.g. a `deploy/nginx-dev.pi5.conf` sample), applied out-of-band
  by the operator, mirroring how `yarddogd.service`/`yarddog.cron` are samples.
- Acceptance Criteria: after the operator applies it, `GET /` through nginx serves the
  embedded page; `/api/v1/*` still carries the injected Bearer; the token never reaches
  the browser (grep the page for `bearer` = empty).
- Pitfalls & edge cases: this is a one-time operator action requiring `sudo`/nginx reload
  — Claude proposes the exact commands and waits (host-mutation rule); the reload is
  async, so poll the endpoint before asserting success.
- Complexity: Medium

### Task 5: Retire `/var/www` (operator step)
- Description: once `/` serves from the daemon, the hand-edited `/var/www/…/index.html`
  is dead. Document its removal (and the `403.html`/`404.html` disposition — either keep
  nginx's own error pages or serve them from the daemon too, decide in-task).
- Acceptance Criteria: no live traffic depends on `/var/www`; the file's removal changes
  nothing observable.
- Complexity: Easy

### Task 6: Tests (`gateway/httpapi`, `httptest`)
- Description: `GET /` → 200, `text/html`, non-empty body containing a known marker from
  the page (e.g. `yarddog status`); unknown path (`/nope`) → 404; `GET /api/v1/metrics`
  without a token → 401 (exemption didn't leak); the embedded bytes are non-empty (guards
  a broken `//go:embed` path at build time). One `Test*` per handler, subtests per case.
- Acceptance Criteria: `go vet ./... && go test ./...` green.
- Complexity: Easy

### Task 7: Docs (`README.md`, `CLAUDE.md`)
- Description: state that the daemon now serves its own **embedded, read-only** dashboard
  at `/` (ungated), in addition to the JSON API; refine the "read-only JSON REST API"
  framing so it no longer reads as JSON-only. Note that the UI version equals the daemon
  version (so `/health/check`'s `server.version` also identifies the page). No business
  logic lives in the HTML — it is a static asset the daemon happens to host.
- Acceptance Criteria: docs match behaviour; the architecture table row for
  `gateway/httpapi` mentions the embedded UI.
- Complexity: Easy

## Execution Order
1 (import) → 2 (embed+serve) → 3 (ungate) → 6 (tests, once 2–3 compile) → 7 (docs).
Tasks 4–5 (nginx cutover + `/var/www` retirement) are **operator/host steps** run after
a release carrying tasks 1–3 is live, not repo code — sequence them last and out-of-band.

## Risks
- **Daemon-down now blanks the page.** Today nginx serves the static HTML even when
  `yarddogd` is down (the page then shows the loader because `/api` fails). Under embed,
  nginx proxies `/` to the daemon, so a fully-down daemon makes `/` a 502 — the graceful
  "waiting for data" state (the very loader just shipped) only covers *daemon-up,
  no-data*, not *daemon-down*. Mitigation options for Task 4: an nginx `error_page 502`
  → a tiny static holding page, or accept it (the daemon has `Restart=on-failure` and
  deploys are health-gated, so hard-down is transient — and the daemon *is* the whole
  app). Decide in the executing session; do not silently regress the down-state UX.
- **Role expansion.** The daemon stops being strictly a JSON API. Bounded and
  intentional (it serves one static asset, no new business logic), but the CLAUDE.md
  framing and any reviewer lens expecting "JSON only" must be updated (Task 7).
- **Cache staleness.** Too-long `Cache-Control` on `/` hides a freshly deployed UI behind
  a browser cache. Keep it short (Task 2).

## Trade-offs
- **Embed over a separate web artifact.** Ties the UI to the daemon's release cadence and
  expands the daemon's role, in exchange for zero new deploy machinery, guaranteed
  UI/API version parity, and the elimination of hand-edited `/var/www`. Chosen
  deliberately (see the fork discussion) given the existing release-layout and the
  `github_aide` confinement.
- **Ungated `/` over gating the page.** The page has no secrets and needs to load before
  any token exists (nginx injects the token only on `/api/*`), so gating it would break
  the browser flow; external exposure stays controlled by nginx + Cloudflare Access.
- **`{$}` exact-root over an SPA catch-all.** One page, one route; unknown paths 404
  honestly rather than silently returning the dashboard.
