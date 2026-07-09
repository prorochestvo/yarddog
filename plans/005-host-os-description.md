# Task Breakdown

## Overview

Two related changes to the host-telemetry surface:

1. **Enrich `HostInfo.OS`** from the bare `runtime.GOOS` ("linux") to a human-readable
   distro description ("Ubuntu 26.04 LTS"). Source: `/etc/os-release` `PRETTY_NAME` on
   Linux, `sw_vers` on macOS, falling back to `runtime.GOOS` when unavailable. `Arch`
   stays `runtime.GOARCH` ("arm64"). This flows automatically to every reader of the
   `host` row — both `GET /api/v1/host` and the new embed below.

2. **Embed a nested `host{hostname,os,arch}` block in `GET /api/v1/metrics/latest`** so a
   single latest-metrics fetch also carries the host identity it was taken on.
   `GET /api/v1/metrics` (the multi-run list) does **not** get it — each of its rows has
   its own run. `GET /api/v1/host` is unchanged (it already returns the same, now-enriched
   fields). `count` is left as-is (its removal is a separate queued v1.2 item).

Settled shape (with the operator): OS and arch stay **separate** fields (structured, arch
independently readable); OS is the **verbatim `PRETTY_NAME`**; the metrics embed is a
**nested `host{}` object**.

## Assumptions

1. The `host` table's only writer is `Store.SaveMetrics`, which writes the host row and the
   metric rows of a run together. So the newest run with metric rows always has a host row,
   and `HistoryRepository.LatestHost` returns exactly that run's host — no per-run host
   query (`HostByRun`) is needed; reusing `LatestHost` is correct and avoids a speculative
   port method. (`Store.GetHost` exists but is an off-port test helper returning
   `sql.ErrNoRows`, not the port's `(val, bool, error)` shape — leave it as-is.)
2. Host identity (hostname/os/arch) is stable across a box's runs, so the microsecond TOCTOU
   between the handler's `LatestMetrics` and `LatestHost` calls (a new run landing between
   them) is cosmetically irrelevant — same host, same OS. A single combined query is not
   worth the `LatestMetrics` signature churn.
3. Pure parsers live in the untagged `metrics.go` and are unit-tested on the Linux runner;
   the platform file reads/execs are thin wrappers (matching the existing collector pattern).
   `CGO_ENABLED=0` stays on every target.

## Tasks

### Task 1: Enrich the OS description at collection

- Description:
  - `infrastructure/metrics.go` (untagged): add pure `parseOSRelease(content string) string`
    (returns `PRETTY_NAME`'s value with surrounding quotes stripped, else "") and
    `parseSwVers(content string) string` (from `sw_vers` output → "macOS <ProductVersion>",
    else ""). Change `hostInfo()` to set `OS: osDescription()` (keep `Arch: runtime.GOARCH`).
  - `infrastructure/metrics_linux.go`: add `osDescription() string` — `os.ReadFile("/etc/os-release")`
    → `parseOSRelease` → fall back to `runtime.GOOS` on error/empty. Add the `runtime` import.
  - `infrastructure/metrics_darwin.go`: add `osDescription() string` — `runCmd(context.Background(), swVersPath)`
    → `parseSwVers` → fall back to `runtime.GOOS`. Add `swVersPath = "/usr/bin/sw_vers"`.
  - `domain/metrics.go`: update the `HostInfo.OS` doc — it is now a description, not GOOS.
- Acceptance Criteria: on the Pi, `host.os` becomes "Ubuntu 26.04 LTS"; on a box without
  `/etc/os-release`, it falls back to "linux"; `parseOSRelease`/`parseSwVers` are covered by
  `TestParseOSRelease`/`TestParseSwVers` subtests (quoted/unquoted/missing/empty/extra-keys).
- Pitfalls & edge cases: os-release values are shell-quoted (`PRETTY_NAME="Ubuntu 26.04 LTS"`)
  — strip surrounding `"`/`'`. A missing `PRETTY_NAME` (only NAME/VERSION_ID) → "" → GOOS
  fallback. `hostInfo()` has no ctx; darwin's `osDescription` uses `context.Background()` bounded
  by `runCmd`'s own 3s timeout. Keep `runtime` imported in `metrics.go` (still used for GOARCH).
- Complexity: Medium

### Task 2: Embed host{} in metrics/latest

- Description:
  - `gateway/dto/api.go`: add `HostInfoDTO{Hostname,OS,Arch}` (json hostname/os/arch); add
    `Host HostInfoDTO json:"host"` to `MetricsLatestResponse` (after `TS`, before `Count`).
  - `gateway/httpapi/mapping.go`: add `hostInfoDTO(domain.HostInfo) dto.HostInfoDTO`.
  - `gateway/httpapi/handlers.go`: in `handleLatestMetrics`, after fetching `records`, call
    `s.query.LatestHost(ctx)`; embed `hostInfoDTO(rec.Host)` (zero-value block if `!ok`, which
    cannot happen when `records` is non-empty per Assumption 1). A `LatestHost` error is a 500.
- Acceptance Criteria: `GET /api/v1/metrics/latest` returns `{run_id,ts,host{hostname,os,arch},
  count,metrics[]}`; `/api/v1/metrics` and `/api/v1/host` shapes are unchanged; the handler
  test asserts the host block; the empty case is still a 404.
- Pitfalls & edge cases: `metrics/latest` now issues two reads (metrics + host) — acceptable
  for an infrequently-polled endpoint; do not refactor `LatestMetrics` to join. Keep the
  no-data 404 keyed on the metrics records, not the host.
- Complexity: Easy

## Execution Order

1. Task 1 (collection enrichment) — the data source.
2. Task 2 (metrics/latest embed) — surfaces it.
3. `go vet ./... && go test ./...`; confirm the build-tagged `osDescription` compiles for
   linux/arm64 and darwin/arm64. Then review.

## Risks

- **darwin `sw_vers` exec path is not unit-tested** on the Linux runner (only `parseSwVers`
  is), consistent with the existing sysctl/vm_stat darwin paths. Low risk — the production
  target is Linux.
- **Contract change** to `metrics/latest` (added `host` field) — additive, so existing
  consumers that ignore unknown fields are unaffected.

## Trade-offs

- **Reuse `LatestHost` vs. add `HostByRun`**: reuse is correct under the "host+metrics written
  together" invariant and avoids a speculative port method, at the cost of a documented
  coupling to that invariant. If a future writer decouples them, revisit.
- **OS verbatim `PRETTY_NAME`** (not lower-cased/trimmed): least munging, and macOS `sw_vers`
  yields "macOS <ver>" naturally; the "·"-joined display is left to the client.
