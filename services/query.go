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

	defaultRunsLimit = 100
	maxRunsLimit     = 500

	defaultMetricsLimit = 100
	maxMetricsLimit     = 1000

	defaultPingsLimit = 100
	maxPingsLimit     = 1000
)

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
