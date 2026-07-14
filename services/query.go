package services

import (
	"context"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// MetricsFilter narrows HistoryRepository.ListMetrics (plans/004-query-daemon.md).
// Since's zero value means "no lower bound", which QueryService.Metrics fills
// with a default last-7-days window; Collector's zero value ("") means "every
// collector"; Limit<=0 means "use QueryService's default", and
// QueryService.Metrics clamps whatever it receives to a hard maximum.
// IncludeEmpty false (the default) drops unavailable rows (ok=false) in SQL so
// LIMIT counts only returned rows; true keeps them (?include_empty=true).
type MetricsFilter struct {
	Since        time.Time
	Collector    domain.Collector
	Limit        int
	IncludeEmpty bool
}

// PingFilter narrows HistoryRepository.ListPings (issue #2), mirroring
// MetricsFilter. Since's zero value means "no lower bound", which
// QueryService.Pings fills with a default last-7-days window; Host's zero
// value ("") means "every host"; Limit<=0 means "use QueryService's
// default", and QueryService.Pings clamps whatever it receives to a hard
// maximum. IncludeUnreachable false (the default) drops rows with no
// received replies (received=0) in SQL so LIMIT counts only returned rows;
// true keeps them (?include_unreachable=true).
type PingFilter struct {
	Since              time.Time
	Host               string
	Limit              int
	IncludeUnreachable bool
}

// HistoryRepository is the read-only persistence port the query daemon
// consumes (plans/004-query-daemon.md); infrastructure.Store satisfies it.
// Every "not found" case is reported through a bool return, never a sentinel
// error, so services never needs to import database/sql. id/runID are
// opaque UUIDv7 strings (issue #4).
type HistoryRepository interface {
	LatestHost(ctx context.Context) (domain.HostRecord, bool, error)
	LatestMetrics(ctx context.Context) ([]domain.MetricRecord, error)
	ListMetrics(ctx context.Context, f MetricsFilter) ([]domain.MetricRecord, error)
	ListPings(ctx context.Context, f PingFilter) ([]domain.PingRecord, error)
	ListRuns(ctx context.Context, limit int) ([]domain.Run, error)
	RunByID(ctx context.Context, id string) (domain.Run, bool, error)
	ListChecksByRun(ctx context.Context, runID string) ([]domain.Check, error)
	OverviewMetrics(ctx context.Context, since time.Time, bucket time.Duration) ([]domain.MetricSeries, error)
	OverviewPings(ctx context.Context, since time.Time, bucket time.Duration) ([]domain.PingSeries, error)
	PingSamples(ctx context.Context, since time.Time) ([]domain.PingRecord, error)
}

// NewQueryService builds a QueryService over repo, using clock to resolve the
// default last-7-days window on a filter that carries no explicit Since.
func NewQueryService(repo HistoryRepository, clock Clock) *QueryService {
	return &QueryService{repo: repo, clock: clock}
}

// QueryService is the thin application layer between gateway/httpapi's
// handlers and HistoryRepository (Trade-off T3): it clamps every
// caller-supplied limit to a safe range (Risk R8 — an unbounded limit could
// scan the whole table), defaults an absent time window to the last 7 days,
// and assembles a run together with its checks in one call, so handlers stay
// pure marshalling.
type QueryService struct {
	repo  HistoryRepository
	clock Clock
}

// LatestHost returns the newest host row, or ok=false if the collector has
// never recorded one.
func (q *QueryService) LatestHost(ctx context.Context) (domain.HostRecord, bool, error) {
	return q.repo.LatestHost(ctx)
}

// LatestMetrics returns every metrics row of the newest run that has any
// (empty if none).
func (q *QueryService) LatestMetrics(ctx context.Context) ([]domain.MetricRecord, error) {
	return q.repo.LatestMetrics(ctx)
}

// Metrics returns metrics history matching f, with f.Limit clamped to
// [1, maxMetricsLimit] (defaultMetricsLimit when f.Limit<=0) and an absent
// f.Since defaulted to the last 7 days.
func (q *QueryService) Metrics(ctx context.Context, f MetricsFilter) ([]domain.MetricRecord, error) {
	f.Limit = clampLimit(f.Limit, defaultMetricsLimit, maxMetricsLimit)
	f.Since = q.defaultSince(f.Since)
	return q.repo.ListMetrics(ctx, f)
}

// Overview returns the dashboard's server-downsampled multi-day view
// (plans/010, GET /api/v1/overview): since defaults to the last 7 days
// (defaultSince), then is floored to now-maxOverviewWindow regardless of
// what the caller asked for — an unclamped since (e.g. a naive client's
// Unix-epoch default) would force every repo call below into a GROUP BY
// over the whole hot table, which can starve the daemon's 2-connection pool
// and its own /health/check. bucket is clamped to [minBucket, maxBucket]
// (clampBucket, defaultBucket when unset), then every enabled metric
// collector and configured ping host is bucketed across that window. Each
// PingSeries' Outages come from a separate PingSamples call, collapsed
// by collapseOutages and attached to its matching host by a map lookup — a
// host with no degraded samples in the window simply keeps a nil Outages.
func (q *QueryService) Overview(ctx context.Context, since time.Time, bucket time.Duration) (domain.Overview, error) {
	since = q.defaultSince(since)
	if floor := q.clock.Now().Add(-maxOverviewWindow); since.Before(floor) {
		since = floor
	}
	bucket = clampBucket(bucket)

	metrics, err := q.repo.OverviewMetrics(ctx, since, bucket)
	if err != nil {
		return domain.Overview{}, err
	}

	pings, err := q.repo.OverviewPings(ctx, since, bucket)
	if err != nil {
		return domain.Overview{}, err
	}

	samples, err := q.repo.PingSamples(ctx, since)
	if err != nil {
		return domain.Overview{}, err
	}
	outages := collapseOutages(samples)
	for i := range pings {
		pings[i].Outages = outages[pings[i].Host]
	}

	return domain.Overview{
		Since:   since,
		Until:   q.clock.Now(),
		Bucket:  bucket,
		Metrics: metrics,
		Pings:   pings,
	}, nil
}

// Pings returns ping history matching f, with f.Limit clamped to
// [1, maxPingsLimit] (defaultPingsLimit when f.Limit<=0) and an absent
// f.Since defaulted to the last 7 days.
func (q *QueryService) Pings(ctx context.Context, f PingFilter) ([]domain.PingRecord, error) {
	f.Limit = clampLimit(f.Limit, defaultPingsLimit, maxPingsLimit)
	f.Since = q.defaultSince(f.Since)
	return q.repo.ListPings(ctx, f)
}

// Run returns run id together with every checks row recorded against it. id
// is an opaque UUIDv7 string (issue #4). found is false when no such run
// exists, in which case checks is always nil and ListChecksByRun is never
// called — there is nothing to look up.
func (q *QueryService) Run(ctx context.Context, id string) (run domain.Run, checks []domain.Check, found bool, err error) {
	run, found, err = q.repo.RunByID(ctx, id)
	if err != nil || !found {
		return domain.Run{}, nil, found, err
	}

	checks, err = q.repo.ListChecksByRun(ctx, id)
	return run, checks, true, err
}

// Runs returns the newest limit runs, with limit clamped to
// [1, maxRunsLimit] (defaultRunsLimit when limit<=0).
func (q *QueryService) Runs(ctx context.Context, limit int) ([]domain.Run, error) {
	return q.repo.ListRuns(ctx, clampLimit(limit, defaultRunsLimit, maxRunsLimit))
}

const (
	// defaultWindow is the last-N span a metrics/pings list falls back to when
	// the caller supplies no since.
	defaultWindow = 7 * 24 * time.Hour

	// defaultBucket, minBucket, and maxBucket bound Overview's bucket width
	// (plans/010): clampBucket resolves a caller-supplied bucket to
	// [minBucket, maxBucket], defaultBucket when unset (bucket<=0).
	defaultBucket = time.Hour
	minBucket     = time.Minute
	maxBucket     = 24 * time.Hour

	// maxOverviewWindow bounds how far back Overview will ever query,
	// regardless of an explicit caller-supplied since (plans/010): without
	// it, an old enough value forces every repo call to GROUP BY over the
	// entire hot table instead of a bounded slice of it.
	maxOverviewWindow = 31 * 24 * time.Hour

	defaultRunsLimit = 100
	maxRunsLimit     = 500

	defaultMetricsLimit = 100
	maxMetricsLimit     = 1000

	defaultPingsLimit = 100
	maxPingsLimit     = 1000
)

// clampBucket resolves a caller-supplied bucket width to a safe range,
// mirroring clampLimit: bucket<=0 (unset) becomes defaultBucket; outside
// [minBucket, maxBucket] is silently clamped to the nearer bound rather than
// rejected as an error, matching clampLimit's "too large/small ⇒ clamped,
// not an error" API surface.
func clampBucket(bucket time.Duration) time.Duration {
	if bucket <= 0 {
		return defaultBucket
	}
	if bucket < minBucket {
		return minBucket
	}
	if bucket > maxBucket {
		return maxBucket
	}
	return bucket
}

// clampLimit resolves a caller-supplied limit to a safe range: limit<=0
// (unset) becomes def; limit>max is silently capped to max rather than
// rejected as an error — a too-large limit is worth correcting, not worth
// failing the request over (API surface: "limit>max ⇒ clamped, not an
// error").
func clampLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// collapseOutages groups samples — already ordered (host, id) by
// PingSamples' own contract — into one domain.PingOutage per maximal run of
// consecutive degraded entries for one host (plans/010 FIX 1). A degraded
// sample (Received<Sent) opens or extends the currently open episode for its
// host; a healthy sample (Received>=Sent) closes it without starting a new
// one, so a recover-then-refail for the same host now reports two episodes,
// not one merged span. A host change also closes whatever episode was open
// for the previous host before the new row is considered. Returns a map
// keyed by host so Overview can attach each PingSeries' own episodes by a
// plain lookup; a host with no degraded sample in the window is simply
// absent from the map.
func collapseOutages(samples []domain.PingRecord) map[string][]domain.PingOutage {
	out := make(map[string][]domain.PingOutage)

	var cur *domain.PingOutage
	var curHost string
	flush := func() {
		if cur != nil {
			out[curHost] = append(out[curHost], *cur)
			cur = nil
		}
	}

	for _, s := range samples {
		if s.Result.Host != curHost {
			flush()
			curHost = s.Result.Host
		}

		if s.Result.Received >= s.Result.Sent {
			flush() // a healthy sample closes any open episode and starts nothing
			continue
		}

		lossPct := domain.LossPercent(s.Result.Sent, s.Result.Received)
		if cur == nil {
			cur = &domain.PingOutage{Start: s.TS, End: s.TS, WorstLossPct: lossPct, Unreachable: s.Result.Received == 0}
			continue
		}

		cur.End = s.TS
		if lossPct > cur.WorstLossPct {
			cur.WorstLossPct = lossPct
		}
		if s.Result.Received == 0 {
			cur.Unreachable = true
		}
	}
	flush() // the final row never sees a host change or healthy sample to trigger its own flush

	return out
}

// defaultSince resolves an absent (zero) time window to the last 7 days; a
// caller-supplied since passes through untouched. The list endpoints read the
// hot tables only, and the 7-day default sits well inside the hot span
// (HOT_WINDOW_DAYS, default 30), so the default view is always served from hot.
// An explicit since reaching before the hot floor returns only what hot still
// holds — archive history is not exposed on these endpoints.
func (q *QueryService) defaultSince(since time.Time) time.Time {
	if since.IsZero() {
		return q.clock.Now().Add(-defaultWindow)
	}
	return since
}
