# Task Breakdown

## Overview

The Nokia G-240W-A hard reboot fails in production. Empirical capture against the live
router (firmware `3FE56557HJHL16` / `HJHL16`) shows the reboot page requires a
per-request CSRF token the driver never sends:

- `POST /login.cgi` (`name`/`pswd`, plaintext) → `302`, sets `sid`/`lsid`. Works.
- `GET /reboot.cgi` → `200`, a page embedding a **fresh** token, regenerated every GET:
  `<input type="hidden" name="csrf_token" value="…" />`, and the same token in a
  `$(document).ajaxSend(...)` prefilter that injects it into every POST body.
- The Reboot button POSTs `data:'data'`; the prefilter appends `&csrf_token=<token>`, so
  the browser sends body **`data&csrf_token=<token>`** with
  `Content-Type: application/x-www-form-urlencoded; charset=UTF-8`.
- A missing/stale token → the firmware **redirects the POST to the login page**
  (`302 Location:/`), which the current driver (auto-following redirects) reads as
  login-page HTML → `ErrRebootNotConfirmed`. Recorded run 19 confirms this.

The current driver POSTs an **empty body** with no prior GET and no token, so the firmware
bounces it to the login page and the reboot never fires. The `"done reboot"` success
marker was never the problem: a live probe (2026-07-10) fired a real reboot and captured
**`200` with body `done reboot`**, the WAN dropping ~54s later. This task makes the driver
perform the real GET-token → POST-token flow; success is the confirmed `200` + marker,
failure the login-page redirect.

The token-transmission mechanism was resolved by inspection (no reboot fired): **not** a
cookie (`GET /reboot.cgi` sets none) and **not** a header — the token travels in the POST
**body**. The success response was then confirmed by a real, user-authorized reboot.

## Assumptions

- A confirmed reboot answers `200` with `done reboot` in the body (captured live
  2026-07-10); a rejected one (stale/missing CSRF token) redirects to the login root.
  Success is checked as `200` + marker, failure as the redirect or a marker-less `200`.
- Preserving the button's literal `data` prefix (`data&csrf_token=…`) is faithful capture,
  not invention; the firmware's own empty-POST path also accepts a bare `csrf_token=…`, so
  the extra key is inert.
- The hidden-input form `name="csrf_token" value="…"` is the stable parse target (standard
  HTML attribute), preferred over the JS string literals.

## Tasks

### Task 1: DTO — CSRF parse + reboot-body encode, drop dead constants
- Description: In `gateway/dto/nokia.go` remove `RebootRequestBody` (empty); keep
  `RebootDoneMarker` (`"done reboot"`, now verified live). Add `CSRFTokenField = "csrf_token"`,
  `ErrCSRFTokenNotFound`, `ParseCSRFToken(page []byte) (string, error)` (regex on the
  hidden input), and `EncodeRebootBody(token string) string` → `data&csrf_token=<token>`
  (url-escaped token). Fix the stale "do not invent bytes / no CSRF" comments.
- Acceptance Criteria: `ParseCSRFToken` extracts the token from a reboot-page fixture and
  returns `ErrCSRFTokenNotFound` on a token-less page; `EncodeRebootBody` round-trips
  through `url.ParseQuery` yielding the token under `csrf_token`. Unit tests in
  `dto_test.go`.
- Pitfalls & edge cases: regex must match `name="csrf_token" value="…"` but not the
  `csrf_token=…` JS literals; empty capture → not-found.
- Complexity: Easy

### Task 2: Driver — real GET-token → POST-token flow, redirect-based success
- Description: In `gateway/router/nokia.go`: disable auto-redirect on the client
  (`CheckRedirect` → `http.ErrUseLastResponse`) so login still captures cookies and Reboot
  sees the raw `302`. Rewrite `Reboot()` to `login` → `fetchCSRFToken` (GET `/reboot.cgi`,
  parse token) → POST `/reboot.cgi?reboot` with `dto.EncodeRebootBody(token)` and the
  form-urlencoded Content-Type. Success = `200` with the `done reboot` marker; a `3xx`
  bounce or a marker-less `200` → `ErrRebootNotConfirmed`; any other status → error. Update
  `ErrRebootNotConfirmed` doc to both failure modes.
- Acceptance Criteria: gate green; `errors.Is(err, ErrRebootNotConfirmed)` on a simulated
  bounce and on a marker-less `200`; `errors.Is(err, dto.ErrCSRFTokenNotFound)` on a
  token-less page; login unchanged for `CheckCredentials`/the credential probe.
- Pitfalls & edge cases: disabling redirects must not break login (jar captures cookies
  pre-redirect) or `CheckCredentials`; GET a rejected session returns `302`/empty → surface
  a clear error; do not fire the POST when the token parse fails.
- Complexity: Medium

### Task 3: Tests — fake router serves the two-step CSRF flow
- Description: Rewrite `gateway/router/nokia_test.go`'s fake so `GET /reboot.cgi` serves a
  token page and `POST /reboot.cgi?reboot` accepts (`200` + `done reboot`) only a body
  echoing that token, else `302`-bounces. Replace the `rebootBody` tuple with an
  observations struct capturing the reboot POST's cookie, `csrf_token`, and Content-Type.
  Cover: happy path (token + cookie + CT round-trip), lsid, bad-creds (no GET/POST), bounce
  → `ErrRebootNotConfirmed`, marker-less `200` → `ErrRebootNotConfirmed`, token-less page →
  `ErrCSRFTokenNotFound` (no POST), trailing-slash (paths `login.cgi`,`reboot.cgi`,
  `reboot.cgi`). Update `CheckCredentials` subtests to the new config.
- Acceptance Criteria: `go test ./gateway/...` green; every reboot scenario asserted.
- Pitfalls & edge cases: POST handler must compare against the same token it served; keep
  one `Test*` per method with descriptive subtests.
- Complexity: Medium

### Task 4: Docs — README router flow + compatibility
- Description: Correct `README.md` "How a reboot goes" (add the `GET /reboot.cgi` token
  step; show `200 "done reboot"` on success, a login redirect on rejection) and "Router
  compatibility" (replace "there are no CSRF tokens" with the real per-request CSRF flow
  and the verified `200`/`done reboot` success signal).
- Acceptance Criteria: no stale "no CSRF" claim remains; the CSRF flow and success signal
  are documented accurately.
- Complexity: Easy

## Execution Order

1 → 2 → 3 → 4, then the gate (`go vet` + `go test` + both cross-builds).

## Risks

- **Success signal verified end-to-end.** A user-authorized live probe (2026-07-10) fired
  a real reboot: `200 "done reboot"`, WAN down ~54s then recovered. The driver checks
  `200` + marker, matching the captured contract.
- **Token regex brittleness** if a future firmware reorders attributes; scoped to the
  observed contract deliberately (no speculative generalization).

## Trade-offs

- Success = `200` + `done reboot` marker (both, not either): the live capture shows the
  marker is real, so checking it alongside the status rejects a hypothetical `200` that
  isn't a reboot page — cheap defense over trusting the status code alone.
- Keeping the `data` prefix: byte-faithful to the button over a marginally cleaner
  `csrf_token=<token>`; both are router-accepted, faithful wins under the "capture, don't
  invent" constraint.
