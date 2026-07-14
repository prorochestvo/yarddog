package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

var _ HistoryRepository = (*fakeHistoryRepository)(nil)

// testNow is the fixed instant the query-service tests resolve the default
// 7-day window against, so defaultSince is deterministic.
var testNow = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

// newQueryService builds a QueryService over repo with a clock frozen at
// testNow.
func newQueryService(repo HistoryRepository) *QueryService {
	return NewQueryService(repo, &fakeClock{now: testNow})
}

func TestQueryService_LatestHost(t *testing.T) {
	t.Parallel()

	t.Run("delegates straight through", func(t *testing.T) {
		t.Parallel()

		want := domain.HostRecord{RunID: "5", Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"}}
		repo := &fakeHistoryRepository{latestHostResult: want, latestHostOK: true}
		q := newQueryService(repo)

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
		q := newQueryService(repo)

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
		q := newQueryService(repo)

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

		repo := &fakeHistoryRepository{latestMetricsResult: []domain.MetricRecord{{RunID: "5"}}}
		q := newQueryService(repo)

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
		q := newQueryService(repo)

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
			q := newQueryService(repo)
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
		q := newQueryService(repo)

		_, err := q.Metrics(t.Context(), MetricsFilter{})
		if !errors.Is(err, wantErr) {
			t.Fatalf("Metrics() error = %v, want %v", err, wantErr)
		}
	})

	t.Run("an absent Since defaults to the last 7 days", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)

		if _, err := q.Metrics(t.Context(), MetricsFilter{}); err != nil {
			t.Fatalf("Metrics() error = %v", err)
		}
		want := testNow.Add(-defaultWindow)
		if !repo.listMetricsFilter.Since.Equal(want) {
			t.Fatalf("repo received Since = %v, want %v (now-7d default)", repo.listMetricsFilter.Since, want)
		}
	})

	t.Run("an explicit Since overrides the default window", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)
		since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		if _, err := q.Metrics(t.Context(), MetricsFilter{Since: since}); err != nil {
			t.Fatalf("Metrics() error = %v", err)
		}
		if !repo.listMetricsFilter.Since.Equal(since) {
			t.Fatalf("repo received Since = %v, want %v (untouched)", repo.listMetricsFilter.Since, since)
		}
	})
}

func TestQueryService_Overview(t *testing.T) {
	t.Parallel()

	bucketTests := []struct {
		name       string
		bucket     time.Duration
		wantBucket time.Duration
	}{
		{"zero uses the default", 0, defaultBucket},
		{"negative uses the default", -time.Hour, defaultBucket},
		{"below the minimum is clamped up", 30 * time.Second, minBucket},
		{"above the maximum is clamped down", 48 * time.Hour, maxBucket},
		{"in range passes through", 6 * time.Hour, 6 * time.Hour},
	}
	for _, tt := range bucketTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := &fakeHistoryRepository{}
			q := newQueryService(repo)

			got, err := q.Overview(t.Context(), time.Time{}, tt.bucket)
			if err != nil {
				t.Fatalf("Overview() error = %v", err)
			}
			if repo.overviewMetricsBucket != tt.wantBucket || repo.overviewPingsBucket != tt.wantBucket {
				t.Fatalf("repo received bucket = %v/%v, want %v", repo.overviewMetricsBucket, repo.overviewPingsBucket, tt.wantBucket)
			}
			if got.Bucket != tt.wantBucket {
				t.Fatalf("Overview().Bucket = %v, want %v", got.Bucket, tt.wantBucket)
			}
		})
	}

	t.Run("an absent since defaults to the last 7 days on every repo call", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)

		got, err := q.Overview(t.Context(), time.Time{}, 0)
		if err != nil {
			t.Fatalf("Overview() error = %v", err)
		}

		want := testNow.Add(-defaultWindow)
		if !repo.overviewMetricsSince.Equal(want) || !repo.overviewPingsSince.Equal(want) || !repo.pingSamplesSince.Equal(want) {
			t.Fatalf("repo received since = %v/%v/%v, want %v (now-7d default) on every call",
				repo.overviewMetricsSince, repo.overviewPingsSince, repo.pingSamplesSince, want)
		}
		if !got.Since.Equal(want) {
			t.Fatalf("Overview().Since = %v, want %v", got.Since, want)
		}
	})

	t.Run("an explicit in-range since overrides the default window on every repo call", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)
		since := testNow.Add(-10 * 24 * time.Hour) // well inside the 31-day max window

		got, err := q.Overview(t.Context(), since, 0)
		if err != nil {
			t.Fatalf("Overview() error = %v", err)
		}
		if !repo.overviewMetricsSince.Equal(since) || !repo.overviewPingsSince.Equal(since) || !repo.pingSamplesSince.Equal(since) {
			t.Fatalf("repo received since not forwarded untouched: %v/%v/%v, want %v",
				repo.overviewMetricsSince, repo.overviewPingsSince, repo.pingSamplesSince, since)
		}
		if !got.Since.Equal(since) {
			t.Fatalf("Overview().Since = %v, want %v", got.Since, since)
		}
	})

	t.Run("an explicit since older than the max window is clamped to now-maxOverviewWindow on every repo call", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)
		since := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

		got, err := q.Overview(t.Context(), since, 0)
		if err != nil {
			t.Fatalf("Overview() error = %v", err)
		}
		want := testNow.Add(-maxOverviewWindow)
		if !repo.overviewMetricsSince.Equal(want) || !repo.overviewPingsSince.Equal(want) || !repo.pingSamplesSince.Equal(want) {
			t.Fatalf("repo received since = %v/%v/%v, want %v (clamped to the max window) on every call",
				repo.overviewMetricsSince, repo.overviewPingsSince, repo.pingSamplesSince, want)
		}
		if !got.Since.Equal(want) {
			t.Fatalf("Overview().Since = %v, want %v", got.Since, want)
		}
	})

	t.Run("Until is the clock's current time", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)

		got, err := q.Overview(t.Context(), time.Time{}, 0)
		if err != nil {
			t.Fatalf("Overview() error = %v", err)
		}
		if !got.Until.Equal(testNow) {
			t.Fatalf("Overview().Until = %v, want the clock's now %v", got.Until, testNow)
		}
	})

	t.Run("attaches each host's outages by a map lookup, leaving an unaffected host nil", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
		repo := &fakeHistoryRepository{
			overviewPingsResult: []domain.PingSeries{{Host: "1.1.1.1"}, {Host: "8.8.8.8"}},
			pingSamplesResult: []domain.PingRecord{
				{TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}},
			},
		}
		q := newQueryService(repo)

		got, err := q.Overview(t.Context(), time.Time{}, 0)
		if err != nil {
			t.Fatalf("Overview() error = %v", err)
		}
		if len(got.Pings) != 2 {
			t.Fatalf("Overview().Pings = %d series, want 2", len(got.Pings))
		}
		if len(got.Pings[0].Outages) != 1 {
			t.Fatalf("Pings[0] (%s) Outages = %+v, want 1 episode", got.Pings[0].Host, got.Pings[0].Outages)
		}
		if got.Pings[1].Outages != nil {
			t.Fatalf("Pings[1] (%s) Outages = %+v, want nil (no degraded samples for this host)", got.Pings[1].Host, got.Pings[1].Outages)
		}
	})

	t.Run("an OverviewMetrics repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{overviewMetricsErr: wantErr}
		q := newQueryService(repo)

		_, err := q.Overview(t.Context(), time.Time{}, 0)
		if !errors.Is(err, wantErr) {
			t.Fatalf("Overview() error = %v, want %v", err, wantErr)
		}
	})

	t.Run("an OverviewPings repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{overviewPingsErr: wantErr}
		q := newQueryService(repo)

		_, err := q.Overview(t.Context(), time.Time{}, 0)
		if !errors.Is(err, wantErr) {
			t.Fatalf("Overview() error = %v, want %v", err, wantErr)
		}
	})

	t.Run("a PingSamples repo error propagates", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("db exploded")
		repo := &fakeHistoryRepository{pingSamplesErr: wantErr}
		q := newQueryService(repo)

		_, err := q.Overview(t.Context(), time.Time{}, 0)
		if !errors.Is(err, wantErr) {
			t.Fatalf("Overview() error = %v, want %v", err, wantErr)
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
			q := newQueryService(repo)
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
		q := newQueryService(repo)

		_, err := q.Pings(t.Context(), PingFilter{})
		if !errors.Is(err, wantErr) {
			t.Fatalf("Pings() error = %v, want %v", err, wantErr)
		}
	})

	t.Run("an absent Since defaults to the last 7 days", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)

		if _, err := q.Pings(t.Context(), PingFilter{}); err != nil {
			t.Fatalf("Pings() error = %v", err)
		}
		want := testNow.Add(-defaultWindow)
		if !repo.listPingsFilter.Since.Equal(want) {
			t.Fatalf("repo received Since = %v, want %v (now-7d default)", repo.listPingsFilter.Since, want)
		}
	})

	t.Run("an explicit Since overrides the default window", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{}
		q := newQueryService(repo)
		since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		if _, err := q.Pings(t.Context(), PingFilter{Since: since}); err != nil {
			t.Fatalf("Pings() error = %v", err)
		}
		if !repo.listPingsFilter.Since.Equal(since) {
			t.Fatalf("repo received Since = %v, want %v (untouched)", repo.listPingsFilter.Since, since)
		}
	})
}

func TestQueryService_Run(t *testing.T) {
	t.Run("found run returns its checks", func(t *testing.T) {
		t.Parallel()

		repo := &fakeHistoryRepository{
			runByIDResult:    domain.Run{ID: "7"},
			runByIDOK:        true,
			listChecksResult: []domain.Check{{RunID: "7", Target: "1.1.1.1:443"}},
		}
		q := newQueryService(repo)

		run, checks, found, err := q.Run(t.Context(), "7")
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !found {
			t.Fatal("Run() found = false, want true")
		}
		if run.ID != "7" {
			t.Fatalf("Run() run.ID = %q, want %q", run.ID, "7")
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
		q := newQueryService(repo)

		_, checks, found, err := q.Run(t.Context(), "999")
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
		q := newQueryService(repo)

		_, checks, found, err := q.Run(t.Context(), "1")
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
			runByIDResult: domain.Run{ID: "7"},
			runByIDOK:     true,
			listChecksErr: wantErr,
		}
		q := newQueryService(repo)

		_, _, found, err := q.Run(t.Context(), "7")
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

			repo := &fakeHistoryRepository{listRunsResult: []domain.Run{{ID: "1"}}}
			q := newQueryService(repo)

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
		q := newQueryService(repo)

		_, err := q.Runs(t.Context(), 10)
		if !errors.Is(err, wantErr) {
			t.Fatalf("Runs() error = %v, want %v", err, wantErr)
		}
	})
}

func TestCollapseOutages(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns an empty map", func(t *testing.T) {
		t.Parallel()

		if got := collapseOutages(nil); len(got) != 0 {
			t.Fatalf("collapseOutages(nil) = %+v, want empty", got)
		}
	})

	t.Run("a single degraded sample is its own one-sample episode", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
		got := collapseOutages([]domain.PingRecord{
			{TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}},
		})

		outages := got["1.1.1.1"]
		if len(outages) != 1 {
			t.Fatalf("outages = %+v, want 1 episode", outages)
		}
		if !outages[0].Start.Equal(ts) || !outages[0].End.Equal(ts) {
			t.Fatalf("outage Start/End = %v/%v, want both %v", outages[0].Start, outages[0].End, ts)
		}
	})

	t.Run("the last sample in the input still closes its episode (window edge)", func(t *testing.T) {
		t.Parallel()

		ts1 := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
		ts2 := ts1.Add(time.Minute)
		got := collapseOutages([]domain.PingRecord{
			{TS: ts1, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}},
			{TS: ts2, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 2}},
		})

		outages := got["1.1.1.1"]
		if len(outages) != 1 {
			t.Fatalf("outages = %+v, want 1 merged episode", outages)
		}
		if !outages[0].End.Equal(ts2) {
			t.Fatalf("outage End = %v, want the final sample's ts %v (the trailing run must not be dropped)", outages[0].End, ts2)
		}
	})

	t.Run("a healthy sample between two degraded clusters for one host splits them into two episodes", func(t *testing.T) {
		t.Parallel()

		// PingSamples now returns every sample, healthy included, so the
		// midday recovery below is visible to collapseOutages and must close
		// the morning episode rather than let it merge with the evening
		// relapse (plans/010 FIX 1: a recover-then-refail is two episodes).
		morning := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
		midday := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
		evening := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
		got := collapseOutages([]domain.PingRecord{
			{TS: morning, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}}, // degraded
			{TS: midday, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 5}},  // healthy — closes it
			{TS: evening, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 4}}, // degraded again — new episode
		})

		outages := got["1.1.1.1"]
		if len(outages) != 2 {
			t.Fatalf("outages = %+v, want exactly 2 episodes, split by the healthy sample", outages)
		}
		if !outages[0].Start.Equal(morning) || !outages[0].End.Equal(morning) {
			t.Fatalf("outages[0] Start/End = %v/%v, want both %v (closed at the healthy sample, not extended)", outages[0].Start, outages[0].End, morning)
		}
		if !outages[1].Start.Equal(evening) || !outages[1].End.Equal(evening) {
			t.Fatalf("outages[1] Start/End = %v/%v, want both %v (the relapse starts a fresh episode)", outages[1].Start, outages[1].End, evening)
		}
	})

	t.Run("truly consecutive degraded samples for one host still merge into one episode", func(t *testing.T) {
		t.Parallel()

		// Contrast with the split above: with no healthy sample in between,
		// adjacency alone still merges consecutive degraded runs.
		morning := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
		evening := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
		got := collapseOutages([]domain.PingRecord{
			{TS: morning, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}},
			{TS: evening, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 4}},
		})

		outages := got["1.1.1.1"]
		if len(outages) != 1 {
			t.Fatalf("outages = %+v, want exactly 1 merged episode, not one per cluster", outages)
		}
		if !outages[0].Start.Equal(morning) || !outages[0].End.Equal(evening) {
			t.Fatalf("outage Start/End = %v/%v, want %v/%v (spanning both clusters)", outages[0].Start, outages[0].End, morning, evening)
		}
	})

	t.Run("a different host starts its own separate episode", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
		got := collapseOutages([]domain.PingRecord{
			{TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}},
			{TS: ts, Result: domain.PingResult{Host: "8.8.8.8", Sent: 5, Received: 4}},
		})

		if len(got) != 2 {
			t.Fatalf("collapseOutages() = %d hosts, want 2", len(got))
		}
		if len(got["1.1.1.1"]) != 1 || len(got["8.8.8.8"]) != 1 {
			t.Fatalf("got = %+v, want exactly one episode per host", got)
		}
	})

	t.Run("Unreachable", func(t *testing.T) {
		ts := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)

		t.Run("sustained partial loss never sets Unreachable", func(t *testing.T) {
			t.Parallel()

			got := collapseOutages([]domain.PingRecord{
				{TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}},
				{TS: ts.Add(time.Minute), Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 4}},
			})
			if got["1.1.1.1"][0].Unreachable {
				t.Fatal("Unreachable = true, want false (every sample kept at least a partial reply)")
			}
		})

		t.Run("one zero-reply sample sets Unreachable for the whole episode", func(t *testing.T) {
			t.Parallel()

			got := collapseOutages([]domain.PingRecord{
				{TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}},
				{TS: ts.Add(time.Minute), Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 0}},
			})
			if !got["1.1.1.1"][0].Unreachable {
				t.Fatal("Unreachable = false, want true (one sample had zero replies)")
			}
		})
	})

	t.Run("WorstLossPct is the maximum loss percentage seen across the run", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
		got := collapseOutages([]domain.PingRecord{
			{TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 4}},                      // 20% loss
			{TS: ts.Add(time.Minute), Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 1}},     // 80% loss, the worst
			{TS: ts.Add(2 * time.Minute), Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 3}}, // 40% loss
		})

		if got["1.1.1.1"][0].WorstLossPct != 80 {
			t.Fatalf("WorstLossPct = %d, want 80 (the worst single-sample loss in the run)", got["1.1.1.1"][0].WorstLossPct)
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

	overviewMetricsResult []domain.MetricSeries
	overviewMetricsErr    error
	overviewMetricsSince  time.Time
	overviewMetricsBucket time.Duration

	overviewPingsResult []domain.PingSeries
	overviewPingsErr    error
	overviewPingsSince  time.Time
	overviewPingsBucket time.Duration

	pingSamplesResult []domain.PingRecord
	pingSamplesErr    error
	pingSamplesSince  time.Time
}

func (f *fakeHistoryRepository) LatestHost(context.Context) (domain.HostRecord, bool, error) {
	return f.latestHostResult, f.latestHostOK, f.latestHostErr
}

func (f *fakeHistoryRepository) LatestMetrics(context.Context) ([]domain.MetricRecord, error) {
	return f.latestMetricsResult, f.latestMetricsErr
}

func (f *fakeHistoryRepository) ListChecksByRun(context.Context, string) ([]domain.Check, error) {
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

func (f *fakeHistoryRepository) RunByID(context.Context, string) (domain.Run, bool, error) {
	return f.runByIDResult, f.runByIDOK, f.runByIDErr
}

func (f *fakeHistoryRepository) OverviewMetrics(_ context.Context, since time.Time, bucket time.Duration) ([]domain.MetricSeries, error) {
	f.overviewMetricsSince = since
	f.overviewMetricsBucket = bucket
	return f.overviewMetricsResult, f.overviewMetricsErr
}

func (f *fakeHistoryRepository) OverviewPings(_ context.Context, since time.Time, bucket time.Duration) ([]domain.PingSeries, error) {
	f.overviewPingsSince = since
	f.overviewPingsBucket = bucket
	return f.overviewPingsResult, f.overviewPingsErr
}

func (f *fakeHistoryRepository) PingSamples(_ context.Context, since time.Time) ([]domain.PingRecord, error) {
	f.pingSamplesSince = since
	return f.pingSamplesResult, f.pingSamplesErr
}
