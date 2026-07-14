# Task Breakdown

## Overview

Add a new read-only endpoint, `GET /api/v1/overview`, that serves a fixed-size,
server-downsampled multi-day view of host metrics and ping reachability. It is the
data source for the dashboard's **retrospective** section ("what broke this week /
where to look"), which the existing list endpoints cannot feed.

Why a new endpoint rather than widening `/metrics` + `/pings`:

- The list endpoints are **row-capped** (`defaultMetricsLimit=100`, `maxMetricsLimit=1000`;
  same for pings) and metrics are stored **EAV** — roughly 13 rows per run (temperature,
  fans, cpu load1/5/15, memory total/available, disk used_ratio/used/total, uptime,
  network rx/tx). So a no-`since` `/metrics` call returns ~100 rows ≈ the **last ~7–8
  runs**, and even `limit=1000` is ~77 runs. A 7-day window cannot be expressed at any
  realistic collection cadence without blowing past the cap.
- A 34px sparkline cannot render thousands of raw points; the useful weekly view is a
  bounded set of time buckets (168 hourly buckets over 7 days), each carrying the
  min/max/avg that let a spike survive downsampling.

The endpoint moves **downsampling** and **ping-outage detection** into SQL + the
services layer, so the browser receives tens of KB of already-bucketed data instead of
megabytes of raw rows. Metric thresholds stay **client-side** (the browser flags
breaches from each bucket's `max`/`min` against its own thresholds); ping outages, being
**binary reachability** with no tunable threshold, are detected **server-side** and
returned as precise episodes.

This task is additive: `/metrics`, `/metrics/latest`, `/pings`, `/host`, `/runs*` are
untouched, and the dashboard's live cards keep using them.

## Assumptions

- **New endpoint, no change to the existing list endpoints.** The live "current status"
  cards keep reading `/metrics` + `/pings` (the ~100-row recent window is fine for them).
- **Window default = last 7 days** (reuse `services.defaultWindow`), overridable via
  `?since=<RFC3339>`, exactly like `/metrics`. **Hot-only** — 7 days sits inside the
  30-day hot window, so no archive spanning (consistent with `ListMetrics`/`ListPings`).
- **Bucket default = 1h** (`?bucket=<Go duration>`, e.g. `30m`, `15m`, `2h`), clamped to
  `[minBucket, maxBucket]`. 1h → 168 buckets/7d. Tunable was chosen deliberately: the
  right resolution depends on the real collection cadence, which we will observe before
  fixing a default in stone.
- **Uniform gauge aggregation.** Every `(collector, name)` series is aggregated as a
  gauge: per bucket `min`, `max`, `avg`, `count` over `ok=1` (non-null) rows. The server
  does **not** special-case any metric.
  - Consequence, flagged as a v1 limitation: `network/* rx_bytes|tx_bytes` are
    **cumulative counters**, so their min/max/avg buckets are *not* a rate; `memory` is
    stored as `total`/`available`, so a used-% peak is not a single bucketed field.
    Correct weekly rate (counter-delta with reset handling) and memory used-% are
    **deferred** — the client's weekly section surfaces the directly-comparable gauges
    (temperature, cpu load1, disk used_ratio, fans) plus ping outages in v1. The endpoint
    still returns the network/memory series faithfully; only their *presentation* is out
    of the first client cut.
- **Metric thresholds are NOT server-side.** The daemon returns raw bucket extrema; the
  browser owns the temp/load/mem/disk thresholds and computes metric incidents from them.
  Ping outage detection needs no threshold (reachability is binary) and stays server-side.
- **Incident time resolution for metrics = one bucket** in v1 (a breach is labelled by
  its bucket, e.g. "13:00–14:00"). Minute-exact peak attribution (`peak_ts` / argmax) is
  a deferred refinement, not in the v1 wire contract.
- **Portable bucketing SQL.** Bucket key = `(CAST(strftime('%s', ts) AS INTEGER) /
  bucketSecs) * bucketSecs` (bucket-start epoch). `strftime('%s', …)` is supported by
  every SQLite the driver could bundle, sidestepping any `unixepoch()` version question.
- Times are UTC / RFC3339 on the wire, matching every other response.

## Tasks

### Task 1: Domain read-model types (`domain/overview.go`)
- Description: add pure value types for the aggregated projection (no imports beyond
  `time`; `domain` imports nothing internal). A real consumer (the new endpoint)
  justifies the structure — this is not speculative:
  - `MetricBucket{ TS time.Time; Min, Max, Avg float64; Count int }`
  - `MetricSeries{ Collector Collector; Name, Unit string; Buckets []MetricBucket }`
  - `PingBucket{ TS time.Time; Sent, Received int; AvgMS, MaxMS float64; Samples int }`
    (`AvgMS`/`MaxMS` meaningful only when `Received > 0`)
  - `PingOutage{ Start, End time.Time; WorstLossPct int; Unreachable bool }`
    (`Unreachable` = at least one 100%-loss sample in the episode)
  - `PingSeries{ Host string; Buckets []PingBucket; Outages []PingOutage }`
  - `Overview{ Since, Until time.Time; Bucket time.Duration; Metrics []MetricSeries; Pings []PingSeries }`
- Acceptance Criteria: compiles; `go list -deps ./domain` names nothing internal/third-party;
  godoc on every exported identifier (name-first, period-terminated).
- Pitfalls & edge cases: keep these free of any SQL/JSON concern — they are the layer
  boundary the store produces and httpapi maps to DTOs.
- Complexity: Easy

### Task 2: Store aggregation methods (`infrastructure/store.go`)
- Description: add three `HistoryRepository` implementations, reusing the existing column
  const vocabulary (`colMetrics*`, `tableMetrics`, `colPings*`, `tablePings`) and the
  **same sargable `since` lower bound** as `ListMetrics` (the `id >= ? AND ts >= ?` pair,
  with the id floor derived from `since` by the existing helper) so the scans stay on the
  id index:
  - `OverviewMetrics(ctx, since time.Time, bucket time.Duration) ([]domain.MetricSeries, error)` —
    `SELECT collector, name, unit, <bucketEpoch> AS b, MIN(value), MAX(value), AVG(value),
    COUNT(value) FROM metrics WHERE id>=? AND ts>=? AND ok=1 GROUP BY collector, name, b
    ORDER BY collector, name, b`. Assemble rows into one `MetricSeries` per
    `(collector,name)`; convert `b` (epoch seconds) → `time.Time` (UTC).
  - `OverviewPings(ctx, since, bucket) ([]domain.PingSeries, error)` — buckets only
    (`Outages` left nil, filled by services): `SELECT host, <bucketEpoch> AS b,
    SUM(sent), SUM(received), AVG(CASE WHEN received>0 THEN avg_ms END),
    MAX(avg_ms), COUNT(*) FROM pings WHERE id>=? AND ts>=? GROUP BY host, b
    ORDER BY host, b`. **Do not** filter `received>0` for the sums — loss % needs the
    unreachable rows counted; only the `avg_ms` average excludes them.
  - `PingOutageSamples(ctx, since time.Time) ([]domain.PingRecord, error)` — the raw
    material for episode collapsing: `SELECT <ping cols> FROM pings WHERE id>=? AND ts>=?
    AND received < sent ORDER BY host, id` (only degraded samples, ascending, so services
    can walk them in order). Bounded by the 7-day hot window.
- Acceptance Criteria: three methods on `*Store`, each with its
  `var _ services.HistoryRepository` obligation satisfied by the interface extension in
  Task 3; hot-table only; column names via consts, no literals; `go vet` clean.
- Pitfalls & edge cases: `bucketSecs` comes from `int(bucket / time.Second)` — guard
  against 0 (services clamps first, but the SQL must never divide by zero); a bucket with
  only null-value metric rows contributes no row (fine — the client tolerates gaps);
  `AVG(CASE WHEN received>0 …)` yields SQL NULL for an all-unreachable bucket → scan into
  a `sql.NullFloat64` and store 0 with `Samples`/`Received` telling the real story.
- Complexity: Hard

### Task 3: Services assembly (`services/query.go`)
- Description:
  - Extend `HistoryRepository` with the three Task-2 methods.
  - Add `QueryService.Overview(ctx, since time.Time, bucket time.Duration) (domain.Overview, error)`:
    resolve `since` via the existing `defaultSince`; clamp `bucket` via a new
    `clampBucket`; call `OverviewMetrics`, `OverviewPings`, `PingOutageSamples`; **collapse
    outages** and attach each host's episodes to the matching `PingSeries`; return a
    populated `domain.Overview` (`Until = clock.Now()`).
  - Episode collapse (pure function, unit-tested): walk `PingOutageSamples` (already
    ordered by host, id); a maximal run of consecutive degraded samples for one host is
    one `PingOutage` — `Start` = first sample's TS, `End` = last sample's TS,
    `WorstLossPct` = max over the run of `round((1 - received/sent)*100)`,
    `Unreachable` = any sample with `received == 0`. Adjacency is the sequence itself
    (UUIDv7 id order), so no gap heuristic is needed.
  - Consts: `defaultBucket = time.Hour`, `minBucket = time.Minute`, `maxBucket = 24*time.Hour`.
- Acceptance Criteria: no-`since` → `Since = now-7d`; `bucket<=0` → 1h; `bucket` out of
  range → clamped; a host with no degraded samples has `Outages == nil`; back-to-back
  degraded samples split by one healthy sample yield **two** episodes.
- Pitfalls & edge cases: apply `defaultSince` only on zero (never overwrite an explicit
  `since`); a `PingSeries` present in buckets but absent from outage samples must still
  appear (buckets drive the host set, outages are attached); an outage host with no
  bucket row cannot happen (a degraded sample is also a bucket row) — assert it in a test
  rather than defending in code.
- Complexity: Medium

### Task 4: Wire DTOs (`gateway/dto/api.go`)
- Description: add the response shapes (JSON tags `snake_case`, times as RFC3339 strings,
  durations as strings):
  - `OverviewResponse{ Window OverviewWindowDTO; Metrics []MetricSeriesDTO; Pings []PingSeriesDTO }`
  - `OverviewWindowDTO{ Since, Until, Bucket string }` (`Bucket` e.g. `"1h0m0s"` or a
    trimmed `"1h"` — pick one and test it)
  - `MetricSeriesDTO{ Collector, Name, Unit string; Buckets []MetricBucketDTO }`
  - `MetricBucketDTO{ TS string; Min, Max, Avg float64; Count int }`
  - `PingSeriesDTO{ Host string; Buckets []PingBucketDTO; Outages []PingOutageDTO }`
  - `PingBucketDTO{ TS string; Sent, Received int; LossPct int; AvgMS, MaxMS *float64; Samples int }`
    (`avg_ms`/`max_ms` are `null` when the whole bucket was unreachable)
  - `PingOutageDTO{ Start, End, Kind string; WorstLossPct int }` where `Kind` ∈
    {`"unreachable"`, `"loss"`} derived from `PingOutage.Unreachable`
- Acceptance Criteria: godoc on each type stating which field is `null` when; a
  `dto_test.go` encode test pins the JSON shape (key names, null cells).
- Pitfalls & edge cases: `LossPct` is computed for the wire from `Sent`/`Received`
  (`Sent>0 ? round((1-Received/Sent)*100) : 0`) — do it in mapping, not the domain type.
- Complexity: Easy

### Task 5: HTTP adapter (`gateway/httpapi`)
- Description:
  - `handleOverview` (`handlers.go`): parse params, call `s.query.Overview`, map to
    `dto.OverviewResponse`, `writeJSON` 200 (empty series arrays are a valid 200, like
    `handleMetrics`).
  - `parseOverviewParams(q url.Values) (since time.Time, bucket time.Duration, err error)`:
    `since` via `time.Parse(time.RFC3339, …)` (reuse the `/metrics` idiom, same 400 on a
    bad value); `bucket` via `time.ParseDuration` (400 on unparseable); both optional
    (zero values → services applies defaults/clamp).
  - `register` (`server.go`): `mux.HandleFunc("GET /api/v1/overview", s.handleOverview)` —
    it inherits `withAuth` automatically (only `/ping` is exempt).
  - `mapping.go`: `overviewResponse(domain.Overview) dto.OverviewResponse` +
    per-type helpers, mirroring the existing `metricDTOs`/`pingDTOs` style.
- Acceptance Criteria: `GET /api/v1/overview` 200s with the mapped body; bad `since`/`bucket`
  → 400 `{"error":…}`; missing token → 401 (inherited); wrong method → 405 (ServeMux).
- Pitfalls & edge cases: keep the handler pure marshalling — no default/clamp logic here
  (services owns it); don't leak driver errors (`writeError500`).
- Complexity: Medium

### Task 6: Tests
- Description: one `Test*` per method, scenarios as `t.Run` subtests, stdlib `testing`
  only, fakes in `_test.go` with the `var _ services.X` assertion:
  - `infrastructure/store_test.go` (`:memory:`, seeded rows straddling bucket
    boundaries): correct bucket assignment; min/max/avg/count; ping sums include
    unreachable rows while `avg_ms` excludes them; `PingOutageSamples` returns only
    `received<sent` in id order; empty window → empty slices.
  - `services/query_test.go` (fakes + `fakeClock`): default window on zero `since`;
    `clampBucket` table; episode collapse edge cases (single degraded sample; episode at
    the window edge; two episodes split by one healthy sample; `unreachable` vs `loss`
    classification; `WorstLossPct`).
  - `gateway/httpapi/handlers_test.go` + `fakes_test.go`: `parseOverviewParams`
    (valid/invalid `since` and `bucket`, absent → zero); full-server `httptest` shape;
    401 without token; 405 wrong method.
  - `gateway/dto/dto_test.go`: `OverviewResponse` encode — key names, null `avg_ms`.
- Acceptance Criteria: `go vet ./... && go test ./...` green (the completion gate).
- Complexity: Medium

### Task 7: Docs (`README.md`, `yarddog.env.example` if a knob is added)
- Description: add the `/api/v1/overview` row + a short subsection to the README API
  table — params (`since`, `bucket`), the response shape, the "gauge-uniform, network/
  memory presentation deferred" note, and that metric thresholds live in the client while
  ping outages are server-detected. No new env key in v1 (bucket is a query param, window
  reuses the hardcoded default).
- Acceptance Criteria: README matches behaviour; no stale claims.
- Complexity: Easy

## Execution Order
1 (domain) → 2 (store) → 3 (services) → 4 (dto) → 5 (httpapi) → 6 (tests, once the
production path compiles) → 7 (docs). Tasks 2–5 each compile against 1; 6 lands the gate
green before the plan moves to `completed/`.

## Risks
- **Aggregation cost.** The GROUP-BY scans up to 7 days of hot rows per call. At the
  observed EAV volume this is a bounded, indexed scan (`id >= floor`), but the endpoint
  is heavier than the row-capped list endpoints — the client must call it on load /
  occasional refresh, not on the 3-minute live-card cadence. Note this in the client work.
- **Counter/derived metrics mislead if surfaced naively.** `network/*` (counters) and
  `memory` (total/available) return honest gauge buckets that are *not* a rate / used-%.
  Mitigated by scoping their weekly presentation out of the v1 client; the risk is a
  future contributor wiring them into a weekly graph without the derivation. The README
  note and this plan record the boundary.
- **Bucket/window interaction.** A tiny `bucket` over 7 days yields many buckets (7d/1m ≈
  10k) — `minBucket` guards the pathological case; document that fine buckets over long
  windows inflate the payload.
- **NULL handling in ping avg.** An all-unreachable bucket makes `AVG(avg_ms)` SQL NULL;
  a naive scan into `float64` fails. The `sql.NullFloat64` pitfall in Task 2 is the guard.

## Trade-offs
- **New endpoint over extending `/metrics`.** Keeps the list endpoints' contract stable
  and their handlers pure; the aggregation is a genuinely different read model (bucketed
  projection + episodes) that does not belong behind the same row-limited surface.
- **Server aggregation over client-side downsampling.** The row cap makes client-side
  impossible anyway, but even absent the cap, bucketing in SQL keeps the payload tens of
  KB and the browser dumb — worth the added daemon code.
- **Thresholds split (metrics client-side, ping outages server-side).** Honours the
  operator's "keep thresholds in the client" choice while still giving minute-precise
  ping outages, because reachability is binary and needs no threshold to detect. The cost
  is metric incidents at bucket (hour) resolution in v1 rather than minute-exact.
- **Deferring `peak_ts`/argmax.** Bucket-resolution incident timing ships first; a later
  pass can add per-bucket peak timestamps if minute precision proves necessary, without
  breaking the v1 wire contract (additive field).
- **Deferring network-rate / memory-used-% weekly views.** Correct counter-delta and
  ratio aggregation is a separate concern; shipping the clean gauge signals
  (temp/load/disk + ping outages) first delivers the "what broke this week" answer the
  operator actually asked for, and the endpoint already carries the raw series when those
  views are built.
```
