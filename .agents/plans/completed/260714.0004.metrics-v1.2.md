# Task Breakdown

## Overview

v1.2 tightens the query API's response shapes (mostly the metrics surface) and folds in two
small carried-over fixes. All changes are pre-1.0, LAN-only, single-consumer — breaking the
wire shape is acceptable and not flagged as a BREAKING CHANGE footer.

- **#1 Hoist `run_id`/`ts` in single-run responses.** In a single-run response the rows repeat
  the enclosing run's `run_id`/`ts`. Drop them from the rows:
  - `MetricsLatestResponse` → a slim metric row (no `run_id`/`ts`); the list response keeps the
    full `MetricDTO` (its rows span runs and genuinely need per-row `run_id`/`ts`).
  - `RunDetailResponse.Checks` → drop `CheckDTO.run_id` outright (checks are *only* ever returned
    per-run, never in a multi-run list, so it is always redundant).
- **#2 Drop null cells by default + `?include_empty`.** A null cell is an unavailable sample
  (`ok=false`, `value=null`). By default omit them; `?include_empty=true` returns them.
  - List (`/api/v1/metrics`): filter in SQL (`ok = 1`) so `LIMIT` counts only returned rows.
  - Latest (`/api/v1/metrics/latest`): filter in the mapping, leaving `LatestMetrics` returning
    every row of the newest run so the response's `run_id`/`ts`/`host` survive even when every
    cell is null (empty `metrics` array, still 200).
- **#3 Trim the disk `/ ` name prefix when the mount is `/`.** `diskSamples` names samples
  `"<mount> <metric>"`; for the root mount that reads `"/ total"`. Emit `"total"` (no prefix)
  only when `mount == "/"`; other mounts keep the prefix (`"/data total"`).
- **#4 Remove `count` from list responses.** `len(array)` is derivable; drop `Count` from
  `MetricsLatestResponse`, `MetricsListResponse`, `RunsListResponse`.
- **P3 (carried).** `daemon_config.go:72-75` still hardcodes `YARDDOG_CHECK_TIMEOUT` /
  `YARDDOG_DAEMON_HEALTH_TIMEOUT` string literals; use `envPrefix+"KEY"` like every other message.
- **env_val quote-strip (carried).** `release.yml`'s `env_val` reads the daemon token from `.env`
  without stripping surrounding quotes, so the (non-gating) `/health/check` report line 401s when
  the operator quotes `YARDDOG_DAEMON_TOKEN`. Strip surrounding quotes systemd-style.

## Assumptions

1. `CheckDTO` is used only in `RunDetailResponse` (verified) — dropping its `run_id` touches no
   multi-run path.
2. `MetricDTO` stays the full row (list uses it); the latest response gets a new slim row type.
   Two coincidental-but-differently-framed shapes, not a forced dedup.
3. `LatestMetrics`'s `MAX(run_id)` subquery already restricts to runs that have metric rows, so
   the returned slice is non-empty; filtering nulls in the mapping never turns a real run into a
   spurious 404 (the 404 still keys on "no metric rows at all").
4. `?include_empty` accepts the usual truthy forms; anything else is a 400 (never silently
   coerced), matching the existing param-parsing discipline.

## Tasks

### Task 1: #1 hoist + #4 remove count (response shapes)

- Description:
  - `gateway/dto/api.go`: add `MetricRowDTO{Collector,Name,Value,Unit,OK,Error}` (no run_id/ts);
    change `MetricsLatestResponse.Metrics` to `[]MetricRowDTO` and drop its `Count`; drop
    `MetricsListResponse.Count` and `RunsListResponse.Count`; drop `CheckDTO.RunID`.
  - `gateway/httpapi/mapping.go`: add `metricRowDTO`/`metricRowDTOs`; remove `RunID` from
    `checkDTO`.
  - `gateway/httpapi/handlers.go`: latest builds `metricRowDTOs`; drop the three `Count:` lines.
  - Update dto/mapping/handler tests for the new shapes.
- Acceptance: `/metrics/latest` rows carry no `run_id`/`ts`; run-detail checks carry no `run_id`;
  no response carries `count`; the list's rows still carry `run_id`/`ts`.
- Complexity: Medium

### Task 2: #2 drop null cells + `?include_empty`

- Description:
  - `services/query.go`: add `IncludeEmpty bool` to `MetricsFilter`.
  - `infrastructure/store.go`: `ListMetrics` adds `colMetricsOK + " = 1"` to the WHERE conds
    unless `f.IncludeEmpty`.
  - `gateway/httpapi/mapping.go`: `metricRowDTOs(records, includeEmpty)` skips `!OK` rows unless
    `includeEmpty`.
  - `gateway/httpapi/handlers.go`: both handlers parse `?include_empty` (bool); latest passes it
    to the mapping, list into the filter. Add a `parseBoolParam` helper (unset → false;
    unparseable → 400).
  - Tests: default drops nulls, `?include_empty=true` keeps them, bad value → 400.
- Acceptance: both endpoints omit `ok=false` rows by default and include them with
  `?include_empty=true`; the list's `LIMIT` counts only returned rows.
- Complexity: Medium

### Task 3: #3 disk name + P3 config + env_val

- Description:
  - `infrastructure/metrics.go` `diskSamples`: `prefix := mount + " "; if mount == "/" { prefix = "" }`,
    name each sample `prefix + "<metric>"`.
  - `infrastructure/daemon_config.go` (72-75): `"%sCHECK_TIMEOUT (...) < %sDAEMON_HEALTH_TIMEOUT (...)"`
    with `envPrefix` args.
  - `.github/workflows/release.yml` `env_val`: after the `cut`/`tr`, strip one leading and one
    trailing `"`.
  - Tests: `TestDiskSamples` root-vs-non-root naming.
- Acceptance: root disk samples are `total`/`free`/`avail`/`used`/`used_ratio`; a `/data` mount
  keeps `/data total`; daemon_config error uses the prefix; `env_val` returns an unquoted token.
- Complexity: Easy

## Execution Order

Task 1 → Task 2 (builds on the slim row) → Task 3 (independent). Then `go vet ./... && go test ./...`
+ both cross-builds, then the 3-lens review, then land.

## Risks

- **Wire-shape breakage** for any existing client. Acceptable pre-1.0/single-consumer; called out
  so it is a decision, not an accident.
- **`metricRowDTOs` filtering vs the list's SQL filtering** diverge by design (identity
  preservation vs `LIMIT` correctness) — documented so it does not read as an inconsistency bug.

## Trade-offs

- **Slim row type vs reusing `MetricDTO` with omitempty**: a distinct type states intent and keeps
  the list's per-row identity honest, at the cost of one more DTO + mapper.
- **Two filter sites for #2**: the alternative (filter both in SQL) would break the latest
  response's identity when a run is all-null; the alternative (filter both in mapping) would break
  the list's `LIMIT` semantics. Splitting is the correct-by-construction choice.
