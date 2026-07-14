# Task Breakdown

## Overview

Add the retrospective **"what broke this week"** section to the dashboard (now
repo-embedded per `plans/011`), fed by `GET /api/v1/overview` (`plans/010`). The live
"current status" cards stay on top and unchanged — they are the primary view ("is it
100% OK right now"). Below them, a new **weekly** section renders per-metric downsampled
graphs (168 hourly buckets over 7 days) with threshold lines and peak markers, plus an
**incident feed**: the operator looks once, occasionally, and sees where to pay
attention — spikes, threshold breaches, ping outages — without babysitting the page.

Division of labour, per the operator's choice: **metric thresholds live in the client**
(the browser flags breaches from each bucket's `max`/`min` against its own thresholds,
at bucket/hour resolution); **ping outages are server-precomputed** (`overview.pings[].outages[]`)
as precise episodes, because reachability is binary and needs no threshold.

## Assumptions

- Depends on `plans/010` (the `/overview` endpoint) and `plans/011` (UI in-repo). The
  section **degrades gracefully** when `/overview` is absent or errors (fetch → null →
  section hidden), so this can merge before `/overview` reaches a release — it simply
  stays dark until the endpoint ships.
- v1 weekly graphs cover the **directly-comparable gauges**: temperature (`cpu_thermal`),
  cpu `load1`, disk `used_ratio`, fans. `memory` (stored as total/available) and
  `network` (cumulative counters) weekly views are **deferred** (`plans/010`'s note):
  `/overview` returns their series, but rendering a correct weekly used-% / rate needs
  derivation the bucket min/max/avg doesn't cleanly give — out of the v1 client cut. The
  live cards still show memory/network as today.
- Thresholds stay in **one place** in the JS (the existing `statusFor` thresholds: temp
  65/78, load 2.8/4, disk 80/92), reused for both live-card colouring and weekly incident
  detection — never duplicated.
- `/overview` is heavier than the list endpoints (a 7-day GROUP-BY scan), so it is
  fetched **on load and on a slow cadence** (piggyback the existing ~3-min refresh at
  most, or a longer dedicated interval), never hammered. The live cards keep their own
  in-place 3-min refresh.
- Pure client work: no build step, self-contained HTML/CSS/JS (CSP/offline), dark-theme
  and `prefers-reduced-motion` consistent with the existing page.

## Tasks

### Task 1: Fetch + parse `/overview`
- Description: add a tolerant fetch (reuse the page's `fetchJSON` → null on any failure)
  for `GET /api/v1/overview` (optionally `?bucket=1h`, the server default). Parse into the
  `{window, metrics[], pings[]}` shape (`plans/010`). Drive the weekly section off it;
  if null/empty, hide the section (no error noise).
- Acceptance Criteria: endpoint present → section renders; absent/errored/empty → section
  hidden, live cards unaffected.
- Complexity: Easy

### Task 2: Weekly gauge graph component
- Description: a wider SVG per gauge metric (full content width, taller than the 34px
  live spark). Plot per bucket: the **max** as the primary line (spike-preserving — a
  short spike survives downsampling as its bucket's max), the **min** as a faint band/area
  beneath, `avg` optional/muted. Draw a horizontal **threshold line** (warn and/or crit)
  from the client thresholds, and a **marker on the worst bucket** (peak value + its
  bucket time). ~168 points render cleanly at full width.
- Acceptance Criteria: each of temp/load1/disk renders a legible 7-day trend with its
  threshold line and peak marker; gaps (empty buckets) don't break the path.
- Pitfalls & edge cases: shared y-domain must include the threshold so a breach is
  visibly above the line; disk `used_ratio` is 0..1 (render as %); a flat series still
  draws (reuse the live spark's flat-series centring).
- Complexity: Medium

### Task 3: Metric incident detection (client)
- Description: for each gauge series, walk buckets and flag those whose `max` ≥ crit (or
  ≥ warn) threshold; collapse **consecutive** breached buckets into one episode
  `{metric, level, startTS, endTS, peak}` (peak = max over the run). Bucket/hour
  resolution timing (no minute-exact `peak_ts` in `plans/010` v1).
- Acceptance Criteria: a sustained breach across N buckets = one episode; two breaches
  split by a healthy bucket = two episodes; crit outranks warn for the same run.
- Pitfalls & edge cases: use `max` for upper-bound metrics; if a lower-bound gauge is
  ever added, it keys on `min` — keep the direction per-metric, not global.
- Complexity: Medium

### Task 4: Ping outages (server-provided)
- Description: render `overview.pings[].outages[]` directly — each `{start, end, kind
  (unreachable|loss), worst_loss_pct}` becomes an incident line. No client threshold;
  these are already precise (minute-resolution) from the server.
- Acceptance Criteria: an unreachable episode shows as crit with its duration; a
  partial-loss episode as warn with worst-loss %.
- Complexity: Easy

### Task 5: Incident feed UI
- Description: a unified, **newest-first** list merging metric episodes (Task 3) + ping
  outages (Task 4). Each line: status dot/icon, metric/host, peak-or-worst value, and the
  time range (terminal-styled, matching the page). Empty state is a *good* outcome — say
  it calmly: `// no incidents in the last 7 days`.
- Acceptance Criteria: mixed metric+ping incidents sort correctly by time; empty feed
  reads as reassurance, not an error.
- Pitfalls & edge cases: cap the visible list (e.g. top 20) with a "+N more" affordance so
  a bad week can't produce an endless wall.
- Complexity: Medium

### Task 6: Weekly section layout
- Description: a full-width `// WEEKLY` divider (reuse the existing `.sect`
  `grid-column:1/-1` pattern), then the gauge graphs, then the incident feed. Live cards
  stay untouched above. Respect the design tokens, dark theme, and
  `prefers-reduced-motion` (no draw animations when reduced); wide graphs must be
  horizontally scroll-safe on mobile (`overflow-x:auto`, no body overflow).
- Acceptance Criteria: section reads as one system with the live cards; mobile layout
  doesn't overflow the body.
- Complexity: Easy

### Task 7: Refresh policy
- Description: fetch `/overview` on load and on a slow cadence (piggyback or a longer
  dedicated interval), decoupled from the live cards' 3-min in-place refresh; re-render
  the weekly section in place on success, leave it as-is on a transient failure.
- Acceptance Criteria: the weekly section updates without a full reload; a failed
  overview fetch never blanks an already-rendered section or the live cards.
- Complexity: Easy

## Execution Order
1 → 2 → 3 → 4 → 5 → 6 → 7. (2–4 produce the pieces; 5–6 assemble them; 7 wires refresh.)
Ship-able before `plans/010` reaches a release thanks to the graceful-degrade in Task 1.

## Risks
- **`/overview` cost vs cadence.** A too-frequent fetch loads the daemon with 7-day
  scans; keep the weekly cadence slow (Task 7 / `plans/010` Risk).
- **Bucket-resolution incident timing.** Metric incidents are labelled by hour, not the
  exact minute of the peak (no `peak_ts` in `plans/010` v1). Acceptable for "where to
  look"; a later endpoint field can sharpen it without a client rewrite.
- **Deferred memory/network weekly.** Flagged, not silently dropped — the section states
  which metrics it covers so their absence doesn't read as "nothing happened".

## Trade-offs
- **Client-side metric incidents, server-side ping outages.** Honours "thresholds in the
  client" while still giving precise ping outages (binary reachability needs no
  threshold). Cost: metric incidents at hour resolution in v1.
- **max-as-primary-line over avg.** The weekly graph optimises for *spotting spikes*
  (the operator's actual goal), so the max line leads and the min band trails; avg is
  secondary. A pure "typical value" reading is not the point of this view.
- **Gauge-only v1.** Shipping the clean thresholded signals (temp/load/disk + ping
  outages) first delivers the "what broke this week" answer now; memory-used-% and
  network-rate weekly views land when their derivation is built, against the same
  endpoint.
