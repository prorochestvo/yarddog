package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

var _ HistoryRepository = (*fakeHistoryRepository)(nil)

func TestQueryService_LatestHost(t *testing.T) {
	t.Parallel()

	t.Run("delegates straight through", func(t *testing.T) {
		t.Parallel()

		want := domain.HostRecord{RunID: 5, Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"}}
		repo := &fakeHistoryRepository{latestHostResult: want, latestHostOK: true}
		q := NewQueryService(repo)

		got, ok, err := q.LatestHost(t.Context())
		if err != nil {
			t.Fatalf("LatestHost() error = %v", err)
		}
		if !ok {
			t.Fatal("LatestHost() ok = false, want true")
		}
		if got != want {
			t.Fatalf("LatestHost() = %+v, want %+v", got, want)
		}
	})

	t.Run("no data reports ok=false, not an error", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{latestHostOK: false}
		q := NewQueryService(repo)

		_, ok, err := q.LatestHost(t.Context())
		if err != nil {
			t.Fatalf("LatestHost() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("LatestHost() ok = true, want false")
		}
	})

	t.Run("a repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{latestHostErr: wantErr}
		q := NewQueryService(repo)

		_, _, err := q.LatestHost(t.Context())
		if !errors.Is(err, wantErr) {
			t.Fatalf("LatestHost() error = %v, want %v", err, wantErr)
		}
	})
}

func TestQueryService_LatestMetrics(t *testing.T) {
	t.Parallel()

	t.Run("delegates straight through", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{latestMetricsResult: []domain.MetricRecord{{RunID: 5}}}
		q := NewQueryService(repo)

		got, err := q.LatestMetrics(t.Context())
		if err != nil {
			t.Fatalf("LatestMetrics() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("LatestMetrics() = %d rows, want the repo's canned 1", len(got))
		}
	})

	t.Run("a repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{latestMetricsErr: wantErr}
		q := NewQueryService(repo)

		_, err := q.LatestMetrics(t.Context())
		if !errors.Is(err, wantErr) {
			t.Fatalf("LatestMetrics() error = %v, want %v", err, wantErr)
		}
	})
}

func TestQueryService_Metrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{"zero uses the default", 0, defaultMetricsLimit},
		{"negative uses the default", -1, defaultMetricsLimit},
		{"over the max is clamped", 5000, maxMetricsLimit},
		{"in range passes through", 250, 250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeHistoryRepository{}
			q := NewQueryService(repo)
			since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

			_, err := q.Metrics(t.Context(), MetricsFilter{Since: since, Collector: domain.CollectorCPU, Limit: tt.limit})
			if err != nil {
				t.Fatalf("Metrics() error = %v", err)
			}
			if repo.listMetricsFilter.Limit != tt.wantLimit {
				t.Fatalf("repo received Limit = %d, want %d", repo.listMetricsFilter.Limit, tt.wantLimit)
			}
			if !repo.listMetricsFilter.Since.Equal(since) {
				t.Fatalf("repo received Since = %v, want %v (untouched)", repo.listMetricsFilter.Since, since)
			}
			if repo.listMetricsFilter.Collector != domain.CollectorCPU {
				t.Fatalf("repo received Collector = %q, want %q (untouched)", repo.listMetricsFilter.Collector, domain.CollectorCPU)
			}
		})
	}

	t.Run("a repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{listMetricsErr: wantErr}
		q := NewQueryService(repo)

		_, err := q.Metrics(t.Context(), MetricsFilter{})
		if !errors.Is(err, wantErr) {
			t.Fatalf("Metrics() error = %v, want %v", err, wantErr)
		}
	})
}

func TestQueryService_Pings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{"zero uses the default", 0, defaultPingsLimit},
		{"negative uses the default", -1, defaultPingsLimit},
		{"over the max is clamped", 5000, maxPingsLimit},
		{"in range passes through", 250, 250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeHistoryRepository{}
			q := NewQueryService(repo)
			since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

			_, err := q.Pings(t.Context(), PingFilter{Since: since, Host: "1.1.1.1", Limit: tt.limit})
			if err != nil {
				t.Fatalf("Pings() error = %v", err)
			}
			if repo.listPingsFilter.Limit != tt.wantLimit {
				t.Fatalf("repo received Limit = %d, want %d", repo.listPingsFilter.Limit, tt.wantLimit)
			}
			if !repo.listPingsFilter.Since.Equal(since) {
				t.Fatalf("repo received Since = %v, want %v (untouched)", repo.listPingsFilter.Since, since)
			}
			if repo.listPingsFilter.Host != "1.1.1.1" {
				t.Fatalf("repo received Host = %q, want %q (untouched)", repo.listPingsFilter.Host, "1.1.1.1")
			}
		})
	}

	t.Run("a repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{listPingsErr: wantErr}
		q := NewQueryService(repo)

		_, err := q.Pings(t.Context(), PingFilter{})
		if !errors.Is(err, wantErr) {
			t.Fatalf("Pings() error = %v, want %v", err, wantErr)
		}
	})
}

func TestQueryService_Run(t *testing.T) {
	t.Run("found run returns its checks", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{
			runByIDResult:    domain.Run{ID: 7},
			runByIDOK:        true,
			listChecksResult: []domain.Check{{RunID: 7, Target: "1.1.1.1:443"}},
		}
		q := NewQueryService(repo)

		run, checks, found, err := q.Run(t.Context(), 7)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !found {
			t.Fatal("Run() found = false, want true")
		}
		if run.ID != 7 {
			t.Fatalf("Run() run.ID = %d, want 7", run.ID)
		}
		if len(checks) != 1 {
			t.Fatalf("Run() checks = %d rows, want 1", len(checks))
		}
		if repo.listChecksByRunCalls != 1 {
			t.Fatalf("ListChecksByRun calls = %d, want 1", repo.listChecksByRunCalls)
		}
	})

	t.Run("not found never calls ListChecksByRun", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{runByIDOK: false}
		q := NewQueryService(repo)

		_, checks, found, err := q.Run(t.Context(), 999)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if found {
			t.Fatal("Run() found = true, want false")
		}
		if checks != nil {
			t.Fatalf("Run() checks = %v, want nil", checks)
		}
		if repo.listChecksByRunCalls != 0 {
			t.Fatalf("ListChecksByRun calls = %d, want 0 (must not be called for a missing run)", repo.listChecksByRunCalls)
		}
	})

	t.Run("a repo error on RunByID propagates and skips ListChecksByRun", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{runByIDErr: wantErr}
		q := NewQueryService(repo)

		_, checks, found, err := q.Run(t.Context(), 1)
		if !errors.Is(err, wantErr) {
			t.Fatalf("Run() error = %v, want %v", err, wantErr)
		}
		if found {
			t.Fatal("Run() found = true, want false on error")
		}
		if checks != nil {
			t.Fatalf("Run() checks = %v, want nil", checks)
		}
		if repo.listChecksByRunCalls != 0 {
			t.Fatalf("ListChecksByRun calls = %d, want 0 (must not be called when RunByID errors)", repo.listChecksByRunCalls)
		}
	})

	t.Run("a repo error on ListChecksByRun still reports the run as found", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{
			runByIDResult: domain.Run{ID: 7},
			runByIDOK:     true,
			listChecksErr: wantErr,
		}
		q := NewQueryService(repo)

		_, _, found, err := q.Run(t.Context(), 7)
		if !found {
			t.Fatal("Run() found = false, want true (the run itself was found; only its checks errored)")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("Run() error = %v, want %v", err, wantErr)
		}
	})
}

func TestQueryService_Runs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{"zero uses the default", 0, defaultRunsLimit},
		{"negative uses the default", -5, defaultRunsLimit},
		{"over the max is clamped", 10000, maxRunsLimit},
		{"in range passes through", 20, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeHistoryRepository{listRunsResult: []domain.Run{{ID: 1}}}
			q := NewQueryService(repo)

			got, err := q.Runs(t.Context(), tt.limit)
			if err != nil {
				t.Fatalf("Runs() error = %v", err)
			}
			if repo.listRunsLimit != tt.wantLimit {
				t.Fatalf("repo received limit = %d, want %d", repo.listRunsLimit, tt.wantLimit)
			}
			if len(got) != 1 {
				t.Fatalf("Runs() = %d rows, want the repo's canned 1", len(got))
			}
		})
	}

	t.Run("a repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{listRunsErr: wantErr}
		q := NewQueryService(repo)

		_, err := q.Runs(t.Context(), 10)
		if !errors.Is(err, wantErr) {
			t.Fatalf("Runs() error = %v, want %v", err, wantErr)
		}
	})
}

// fakeHistoryRepository is a settable in-memory stand-in for
// HistoryRepository: each method returns its canned "*Result"/"*OK"/"*Err"
// fields and records the arguments it was called with (the last filter/limit,
// and a ListChecksByRun call count), so QueryService tests can assert both
// what was returned and exactly what was asked of the repo.
type fakeHistoryRepository struct {
	latestHostResult domain.HostRecord
	latestHostOK     bool
	latestHostErr    error

	latestMetricsResult []domain.MetricRecord
	latestMetricsErr    error

	listMetricsResult []domain.MetricRecord
	listMetricsErr    error
	listMetricsFilter MetricsFilter

	listPingsResult []domain.PingRecord
	listPingsErr    error
	listPingsFilter PingFilter

	listRunsResult []domain.Run
	listRunsErr    error
	listRunsLimit  int

	runByIDResult domain.Run
	runByIDOK     bool
	runByIDErr    error

	listChecksResult     []domain.Check
	listChecksErr        error
	listChecksByRunCalls int
}

func (f *fakeHistoryRepository) LatestHost(context.Context) (domain.HostRecord, bool, error) {
	return f.latestHostResult, f.latestHostOK, f.latestHostErr
}

func (f *fakeHistoryRepository) LatestMetrics(context.Context) ([]domain.MetricRecord, error) {
	return f.latestMetricsResult, f.latestMetricsErr
}

func (f *fakeHistoryRepository) ListChecksByRun(context.Context, int64) ([]domain.Check, error) {
	f.listChecksByRunCalls++
	return f.listChecksResult, f.listChecksErr
}

func (f *fakeHistoryRepository) ListMetrics(_ context.Context, filter MetricsFilter) ([]domain.MetricRecord, error) {
	f.listMetricsFilter = filter
	return f.listMetricsResult, f.listMetricsErr
}

func (f *fakeHistoryRepository) ListPings(_ context.Context, filter PingFilter) ([]domain.PingRecord, error) {
	f.listPingsFilter = filter
	return f.listPingsResult, f.listPingsErr
}

func (f *fakeHistoryRepository) ListRuns(_ context.Context, limit int) ([]domain.Run, error) {
	f.listRunsLimit = limit
	return f.listRunsResult, f.listRunsErr
}

func (f *fakeHistoryRepository) RunByID(context.Context, int64) (domain.Run, bool, error) {
	return f.runByIDResult, f.runByIDOK, f.runByIDErr
}
