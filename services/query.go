package services

import (
	"context"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// MetricsFilter narrows HistoryRepository.ListMetrics (plans/004-query-daemon.md).
// Since's zero value means "no lower bound"; Collector's zero value ("")
// means "every collector"; Limit<=0 means "use QueryService's default", and
// QueryService.Metrics clamps whatever it receives to a hard maximum.
// IncludeEmpty false (the default) drops unavailable rows (ok=false) in SQL so
// LIMIT counts only returned rows; true keeps them (?include_empty=true).
type MetricsFilter struct {
	Since        time.Time
	Collector    domain.Collector
	Limit        int
	IncludeEmpty bool
}

// HistoryRepository is the read-only persistence port the query daemon
// consumes (plans/004-query-daemon.md); infrastructure.Store satisfies it.
// Every "not found" case is reported through a bool return, never a sentinel
// error, so services never needs to import database/sql.
type HistoryRepository interface {
	LatestHost(ctx context.Context) (domain.HostRecord, bool, error)
	LatestMetrics(ctx context.Context) ([]domain.MetricRecord, error)
	ListMetrics(ctx context.Context, f MetricsFilter) ([]domain.MetricRecord, error)
	ListRuns(ctx context.Context, limit int) ([]domain.Run, error)
	RunByID(ctx context.Context, id int64) (domain.Run, bool, error)
	ListChecksByRun(ctx context.Context, runID int64) ([]domain.Check, error)
}

// NewQueryService builds a QueryService over repo.
func NewQueryService(repo HistoryRepository) *QueryService {
	return &QueryService{repo: repo}
}

// QueryService is the thin application layer between gateway/httpapi's
// handlers and HistoryRepository (Trade-off T3): it clamps every
// caller-supplied limit to a safe range (Risk R8 — an unbounded limit could
// scan the whole table) and assembles a run together with its checks in one
// call, so handlers stay pure marshalling.
type QueryService struct {
	repo HistoryRepository
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
// [1, maxMetricsLimit] (defaultMetricsLimit when f.Limit<=0).
func (q *QueryService) Metrics(ctx context.Context, f MetricsFilter) ([]domain.MetricRecord, error) {
	f.Limit = clampLimit(f.Limit, defaultMetricsLimit, maxMetricsLimit)
	return q.repo.ListMetrics(ctx, f)
}

// Run returns run id together with every checks row recorded against it.
// found is false when no such run exists, in which case checks is always nil
// and ListChecksByRun is never called — there is nothing to look up.
func (q *QueryService) Run(ctx context.Context, id int64) (run domain.Run, checks []domain.Check, found bool, err error) {
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
	defaultRunsLimit = 50
	maxRunsLimit     = 500

	defaultMetricsLimit = 100
	maxMetricsLimit     = 1000
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
