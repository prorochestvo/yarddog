package infrastructure

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

func TestNewStore(t *testing.T) {
	t.Run("creates the schema", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		if _, err := s.InsertRun(t.Context(), time.Now(), "soft", nil); err != nil {
			t.Fatalf("InsertRun() after NewStore error = %v, want schema already applied", err)
		}
	})

	t.Run("reopening the same database is idempotent", func(t *testing.T) {
		t.Parallel()

		// :memory: is per-connection, so a genuine reopen needs a file on
		// disk — that is the only way to exercise "CREATE TABLE IF NOT
		// EXISTS against an already-migrated schema" for real.
		path := filepath.Join(t.TempDir(), "yarddog.db")

		first, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() first open error = %v", err)
		}
		if _, err := first.InsertRun(t.Context(), time.Now(), "soft", nil); err != nil {
			t.Fatalf("InsertRun() on first open error = %v", err)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("db file mode = %v, want 0600 (it holds router credentials' error strings)", perm)
		}

		if err := first.Close(); err != nil {
			t.Fatalf("Close() first handle error = %v", err)
		}

		second, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() second open error = %v, want idempotent migration", err)
		}
		t.Cleanup(func() {
			if err := second.Close(); err != nil {
				t.Errorf("Close() second handle error = %v", err)
			}
		})

		run, err := second.GetRun(t.Context(), 1)
		if err != nil {
			t.Fatalf("GetRun(1) after reopen error = %v, want the row inserted before reopen", err)
		}
		if run.Mode != "soft" {
			t.Fatalf("GetRun(1).Mode = %q, want %q", run.Mode, "soft")
		}
	})
}

func TestOpenStore(t *testing.T) {
	t.Run("non-positive pool size is clamped to one, not an error", func(t *testing.T) {
		t.Parallel()

		for _, n := range []int{0, -5} {
			s, err := OpenStore(t.Context(), ":memory:", n)
			if err != nil {
				t.Fatalf("OpenStore(_, _, %d) error = %v, want the pool size floored to 1", n, err)
			}
			if _, err := s.InsertRun(t.Context(), time.Now(), "soft", nil); err != nil {
				t.Errorf("InsertRun() after OpenStore(_, _, %d) error = %v", n, err)
			}
			if err := s.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		}
	})

	t.Run("a larger pool size against :memory: still opens and migrates", func(t *testing.T) {
		t.Parallel()

		// production only ever pairs maxOpenConns>1 with a file-backed
		// database (see OpenStore's doc); this only proves the floor logic
		// doesn't reject a larger value outright.
		s, err := OpenStore(t.Context(), ":memory:", 8)
		if err != nil {
			t.Fatalf("OpenStore(_, _, 8) error = %v", err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})

		if _, err := s.InsertRun(t.Context(), time.Now(), "soft", nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
	})
}

func TestStore_CheckUP(t *testing.T) {
	t.Run("healthy on a freshly opened store", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		if err := s.CheckUP(t.Context()); err != nil {
			t.Fatalf("CheckUP() error = %v, want nil", err)
		}
	})

	t.Run("returns an error once the store is closed", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		if err := s.CheckUP(t.Context()); err == nil {
			t.Fatal("CheckUP() on a closed store error = nil, want non-nil")
		}
	})
}

func TestStore_EnqueueOutboxMessage(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	createdAt := time.Now().UTC().Truncate(time.Second)

	id, err := s.EnqueueOutboxMessage(t.Context(), createdAt, "hello")
	if err != nil {
		t.Fatalf("EnqueueOutboxMessage() error = %v", err)
	}
	if id == 0 {
		t.Fatal("EnqueueOutboxMessage() id = 0, want a positive id")
	}

	unsent, err := s.ListUnsentOutboxMessages(t.Context())
	if err != nil {
		t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
	}
	if len(unsent) != 1 {
		t.Fatalf("ListUnsentOutboxMessages() = %d messages, want 1", len(unsent))
	}
	if unsent[0].ID != id || unsent[0].Text != "hello" || unsent[0].Attempts != 0 {
		t.Fatalf("ListUnsentOutboxMessages()[0] = %+v, want id=%d text=hello attempts=0", unsent[0], id)
	}
	if !unsent[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("ListUnsentOutboxMessages()[0].CreatedAt = %v, want %v", unsent[0].CreatedAt, createdAt)
	}
}

func TestStore_GetLastRebootStartedAt(t *testing.T) {
	t.Run("no prior reboot signals none, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		_, ok, err := s.GetLastRebootStartedAt(t.Context())
		if err != nil {
			t.Fatalf("GetLastRebootStartedAt() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("GetLastRebootStartedAt() ok = true, want false with no prior reboot")
		}
	})

	t.Run("returns the most recent reboot, ignoring non-reboot actions", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		older := now.Add(-3 * time.Hour)
		newer := now.Add(-40 * time.Minute)

		insertReboot(t, s, older)
		insertRunWithAction(t, s, domain.ActionNone, now.Add(-10*time.Minute))
		insertReboot(t, s, newer)

		got, ok, err := s.GetLastRebootStartedAt(t.Context())
		if err != nil {
			t.Fatalf("GetLastRebootStartedAt() error = %v", err)
		}
		if !ok {
			t.Fatal("GetLastRebootStartedAt() ok = false, want true")
		}
		if !got.Equal(newer) {
			t.Fatalf("GetLastRebootStartedAt() = %v, want the newer reboot %v", got, newer)
		}
	})

	t.Run("age compares younger and older than the cooldown threshold", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC()
		cooldown := 2 * time.Hour

		insertReboot(t, s, now.Add(-40*time.Minute))

		got, ok, err := s.GetLastRebootStartedAt(t.Context())
		if err != nil {
			t.Fatalf("GetLastRebootStartedAt() error = %v", err)
		}
		if !ok {
			t.Fatal("GetLastRebootStartedAt() ok = false, want true")
		}
		if age := now.Sub(got); age >= cooldown {
			t.Fatalf("reboot age = %v, want younger than cooldown %v", age, cooldown)
		}
	})
}

func TestStore_GetRun(t *testing.T) {
	t.Run("not found returns an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		if _, err := s.GetRun(t.Context(), 999); err == nil {
			t.Fatal("GetRun(999) error = nil, want error for a missing row")
		}
	})
}

func TestStore_InsertCheck(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	runID, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	ts := time.Now().UTC().Truncate(time.Second)
	latency := int64(42)
	checks := []domain.Check{
		{RunID: runID, TS: ts, Phase: domain.PhaseInitial, Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true, LatencyMS: &latency},
		{RunID: runID, TS: ts, Phase: domain.PhaseInitial, Target: "https://example.com/generate_204", Kind: domain.CheckKindDomain, OK: false, Error: "status 500"},
	}
	for _, c := range checks {
		if err := s.InsertCheck(t.Context(), c); err != nil {
			t.Fatalf("InsertCheck(%+v) error = %v", c, err)
		}
	}

	got, err := s.ListChecksByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListChecksByRun() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListChecksByRun() = %d rows, want 2", len(got))
	}

	if got[0].Kind != domain.CheckKindIP || !got[0].OK || got[0].LatencyMS == nil || *got[0].LatencyMS != latency {
		t.Fatalf("ListChecksByRun()[0] = %+v, want the ip check with latency %d", got[0], latency)
	}
	if got[0].Error != "" {
		t.Fatalf("ListChecksByRun()[0].Error = %q, want empty", got[0].Error)
	}

	if got[1].Kind != domain.CheckKindDomain || got[1].OK || got[1].Error != "status 500" {
		t.Fatalf("ListChecksByRun()[1] = %+v, want the failed domain check", got[1])
	}
	if got[1].LatencyMS != nil {
		t.Fatalf("ListChecksByRun()[1].LatencyMS = %v, want nil (no latency recorded)", got[1].LatencyMS)
	}
}

func TestStore_InsertRun(t *testing.T) {
	t.Run("soft mode carries the initial check result", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		startedAt := time.Now().UTC().Truncate(time.Second)
		up := true

		id, err := s.InsertRun(t.Context(), startedAt, "soft", &up)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		run, err := s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if run.Mode != "soft" {
			t.Fatalf("Mode = %q, want %q", run.Mode, "soft")
		}
		if !run.StartedAt.Equal(startedAt) {
			t.Fatalf("StartedAt = %v, want %v", run.StartedAt, startedAt)
		}
		if run.InternetOK == nil || !*run.InternetOK {
			t.Fatalf("InternetOK = %v, want true", run.InternetOK)
		}
		if run.Action != domain.ActionNone {
			t.Fatalf("Action = %q, want default %q", run.Action, domain.ActionNone)
		}
		if run.RebootStartedAt != nil || run.FinishedAt != nil || run.Outcome != "" {
			t.Fatalf("unset phase fields are non-nil: %+v", run)
		}
	})

	t.Run("hard mode leaves internet_ok nil", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		id, err := s.InsertRun(t.Context(), time.Now(), "hard", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		run, err := s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if run.InternetOK != nil {
			t.Fatalf("InternetOK = %v, want nil for hard mode (no initial check)", run.InternetOK)
		}
	})
}

func TestStore_LatestHost(t *testing.T) {
	t.Run("empty host table reports ok=false, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		_, ok, err := s.LatestHost(t.Context())
		if err != nil {
			t.Fatalf("LatestHost() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("LatestHost() ok = true, want false on an empty store")
		}
	})

	t.Run("returns the newest run's host row", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		olderID, err := s.InsertRun(t.Context(), base.Add(-time.Hour), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		saveMetricsAt(t, s, olderID, base.Add(-time.Hour), "older-host")

		newestID, err := s.InsertRun(t.Context(), base, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		saveMetricsAt(t, s, newestID, base, "newest-host")

		got, ok, err := s.LatestHost(t.Context())
		if err != nil {
			t.Fatalf("LatestHost() error = %v", err)
		}
		if !ok {
			t.Fatal("LatestHost() ok = false, want true")
		}
		if got.RunID != newestID {
			t.Fatalf("LatestHost().RunID = %d, want %d", got.RunID, newestID)
		}
		if got.Host.Hostname != "newest-host" {
			t.Fatalf("LatestHost().Host.Hostname = %q, want %q", got.Host.Hostname, "newest-host")
		}
		if !got.TS.Equal(base) {
			t.Fatalf("LatestHost().TS = %v, want %v", got.TS, base)
		}
	})
}

func TestStore_LatestMetrics(t *testing.T) {
	t.Run("empty metrics table returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.LatestMetrics(t.Context())
		if err != nil {
			t.Fatalf("LatestMetrics() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("LatestMetrics() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("returns every row of the newest run that has metrics, skipping a metrics-less newer run", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		insertMetricAt(t, s, base.Add(-time.Hour), domain.CollectorCPU, "load1", 1.0, true)

		// a run recorded with METRICS_ENABLED=false has no metrics rows at
		// all; LatestMetrics must not mistake "newest run" for "newest run
		// that has metrics".
		if _, err := s.InsertRun(t.Context(), base.Add(-30*time.Minute), domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		newestRunID := insertMetricAt(t, s, base, domain.CollectorMemory, "total", 100, true)

		got, err := s.LatestMetrics(t.Context())
		if err != nil {
			t.Fatalf("LatestMetrics() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("LatestMetrics() = %d rows, want 1 (only the newest run-with-metrics' row)", len(got))
		}
		if got[0].RunID != newestRunID {
			t.Fatalf("LatestMetrics()[0].RunID = %d, want %d", got[0].RunID, newestRunID)
		}
		if got[0].Sample.Collector != domain.CollectorMemory {
			t.Fatalf("LatestMetrics()[0].Sample.Collector = %q, want %q", got[0].Sample.Collector, domain.CollectorMemory)
		}
	})
}

func TestStore_SaveMetrics(t *testing.T) {
	t.Run("writes the host row and one metrics row per sample, mixed ok/unavailable", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		ts := time.Now().UTC().Truncate(time.Second)

		m := domain.HostMetrics{
			Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"},
			Samples: []domain.MetricSample{
				{Collector: domain.CollectorTemperature, Name: "cpu-thermal", Value: 52.35, Unit: "celsius", OK: true},
				{Collector: domain.CollectorFans, Name: "fans", Unit: "rpm", OK: false, Error: "no fan sensors present"},
			},
		}

		if err := s.SaveMetrics(t.Context(), runID, ts, m); err != nil {
			t.Fatalf("SaveMetrics() error = %v", err)
		}

		host, err := s.GetHost(t.Context(), runID)
		if err != nil {
			t.Fatalf("GetHost() error = %v", err)
		}
		if host != m.Host {
			t.Fatalf("GetHost() = %+v, want %+v", host, m.Host)
		}

		got, err := s.ListMetricsByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListMetricsByRun() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListMetricsByRun() = %d rows, want 2", len(got))
		}

		if got[0].Collector != domain.CollectorTemperature || got[0].Name != "cpu-thermal" || !got[0].OK || got[0].Value != 52.35 || got[0].Unit != "celsius" {
			t.Fatalf("ListMetricsByRun()[0] = %+v, want the ok temperature sample", got[0])
		}
		if got[0].Error != "" {
			t.Fatalf("ListMetricsByRun()[0].Error = %q, want empty", got[0].Error)
		}

		if got[1].Collector != domain.CollectorFans || got[1].OK || got[1].Error != "no fan sensors present" {
			t.Fatalf("ListMetricsByRun()[1] = %+v, want the unavailable fans sample", got[1])
		}
		if got[1].Value != 0 {
			t.Fatalf("ListMetricsByRun()[1].Value = %v, want 0 (an unavailable sample persists as SQL NULL)", got[1].Value)
		}
	})

	t.Run("a second call for the same run fails and leaves no partial rows", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		ts := time.Now().UTC().Truncate(time.Second)
		first := domain.HostMetrics{
			Host:    domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"},
			Samples: []domain.MetricSample{{Collector: domain.CollectorUptime, Name: "uptime", Value: 123, Unit: "seconds", OK: true}},
		}
		if err := s.SaveMetrics(t.Context(), runID, ts, first); err != nil {
			t.Fatalf("first SaveMetrics() error = %v", err)
		}

		// host.run_id is a PRIMARY KEY, so this second snapshot fails on its
		// very first statement (the host insert) — before any of its sample
		// rows are ever attempted. That still proves what matters: a failed
		// SaveMetrics call commits nothing at all, leaving the first
		// snapshot exactly as it was.
		second := domain.HostMetrics{
			Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"},
			Samples: []domain.MetricSample{
				{Collector: domain.CollectorUptime, Name: "uptime", Value: 456, Unit: "seconds", OK: true},
				{Collector: domain.CollectorMemory, Name: "total", Value: 789, Unit: "bytes", OK: true},
			},
		}
		if err := s.SaveMetrics(t.Context(), runID, ts, second); err == nil {
			t.Fatal("second SaveMetrics() for the same run error = nil, want a primary key violation")
		}

		got, err := s.ListMetricsByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListMetricsByRun() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListMetricsByRun() = %d rows, want 1 (the failed second snapshot must add nothing)", len(got))
		}
		if got[0].Value != 123 {
			t.Fatalf("ListMetricsByRun()[0].Value = %v, want the first snapshot's untouched value 123", got[0].Value)
		}
	})
}

func TestStore_SavePings(t *testing.T) {
	t.Run("writes one row per result, reachable and unreachable round-trip via ListPingsByRun", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		ts := time.Now().UTC().Truncate(time.Second)

		results := []domain.PingResult{
			{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 12.5, OK: true},
			{Host: "unreachable.example", Sent: 5, Received: 0, OK: false, Error: "no route to host"},
		}

		if err := s.SavePings(t.Context(), runID, ts, results); err != nil {
			t.Fatalf("SavePings() error = %v", err)
		}

		got, err := s.ListPingsByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListPingsByRun() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListPingsByRun() = %d rows, want 2", len(got))
		}

		if got[0].Host != "1.1.1.1" || !got[0].OK || got[0].Sent != 5 || got[0].Received != 5 || got[0].AvgMS != 12.5 {
			t.Fatalf("ListPingsByRun()[0] = %+v, want the reachable result", got[0])
		}
		if got[0].Error != "" {
			t.Fatalf("ListPingsByRun()[0].Error = %q, want empty", got[0].Error)
		}

		if got[1].Host != "unreachable.example" || got[1].OK || got[1].Received != 0 || got[1].Error != "no route to host" {
			t.Fatalf("ListPingsByRun()[1] = %+v, want the unreachable result", got[1])
		}
		if got[1].AvgMS != 0 {
			t.Fatalf("ListPingsByRun()[1].AvgMS = %v, want 0 (an unreachable result persists avg_ms as SQL NULL)", got[1].AvgMS)
		}
	})
}

func TestStore_ListMetrics(t *testing.T) {
	t.Run("empty store returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListMetrics() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListMetrics() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("IncludeEmpty controls whether unavailable rows appear", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)
		insertMetricAt(t, s, base.Add(-1*time.Minute), domain.CollectorFans, "fans", 0, false)
		insertMetricAt(t, s, base, domain.CollectorCPU, "load1", 1.5, true)

		def, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListMetrics(default) error = %v", err)
		}
		if len(def) != 1 || def[0].Sample.Collector != domain.CollectorCPU {
			t.Fatalf("ListMetrics(default) = %+v, want only the ok row", def)
		}

		all, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10, IncludeEmpty: true})
		if err != nil {
			t.Fatalf("ListMetrics(IncludeEmpty) error = %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("ListMetrics(IncludeEmpty) = %d rows, want 2 (the ok and the unavailable)", len(all))
		}
	})

	t.Run("Since, Collector, and Limit each filter and combine", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		insertMetricAt(t, s, base.Add(-3*time.Hour), domain.CollectorCPU, "load1", 1.5, true)
		insertMetricAt(t, s, base.Add(-2*time.Hour), domain.CollectorTemperature, "cpu-thermal", 50, true)
		insertMetricAt(t, s, base.Add(-1*time.Hour), domain.CollectorCPU, "load1", 2.5, true)
		insertMetricAt(t, s, base, domain.CollectorCPU, "load1", 3.5, true)

		t.Run("Since excludes older rows", func(t *testing.T) {
			got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Since: base.Add(-90 * time.Minute), Limit: 10})
			if err != nil {
				t.Fatalf("ListMetrics() error = %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("ListMetrics(Since=-90m) = %d rows, want 2 (the -1h and now samples)", len(got))
			}
		})

		t.Run("Collector narrows to one kind", func(t *testing.T) {
			got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Collector: domain.CollectorTemperature, Limit: 10})
			if err != nil {
				t.Fatalf("ListMetrics() error = %v", err)
			}
			if len(got) != 1 || got[0].Sample.Collector != domain.CollectorTemperature {
				t.Fatalf("ListMetrics(Collector=temperature) = %+v, want exactly the one temperature sample", got)
			}
		})

		t.Run("Limit caps the result and Since+Collector combine, newest first", func(t *testing.T) {
			got, err := s.ListMetrics(t.Context(), services.MetricsFilter{
				Since:     base.Add(-150 * time.Minute),
				Collector: domain.CollectorCPU,
				Limit:     1,
			})
			if err != nil {
				t.Fatalf("ListMetrics() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ListMetrics(combined, limit=1) = %d rows, want 1", len(got))
			}
			// two cpu samples fall in range (-1h and now); newest first picks "now".
			if got[0].Sample.Value != 3.5 {
				t.Fatalf("ListMetrics(combined)[0].Sample.Value = %v, want the newest matching sample 3.5", got[0].Sample.Value)
			}
		})
	})

	t.Run("an unavailable sample reads back as OK=false, Value=0, with its Error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		insertMetricAt(t, s, time.Now(), domain.CollectorFans, "fans", 0, false)

		// IncludeEmpty: this asserts an unavailable sample round-trips, which the
		// default (ok=1) filter would now drop before we could inspect it.
		got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10, IncludeEmpty: true})
		if err != nil {
			t.Fatalf("ListMetrics() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListMetrics() = %d rows, want 1", len(got))
		}
		if got[0].Sample.OK {
			t.Fatal("Sample.OK = true, want false for an unavailable sample")
		}
		if got[0].Sample.Value != 0 {
			t.Fatalf("Sample.Value = %v, want 0 (SQL NULL for an unavailable sample)", got[0].Sample.Value)
		}
		if got[0].Sample.Error != "unavailable" {
			t.Fatalf("Sample.Error = %q, want %q", got[0].Sample.Error, "unavailable")
		}
	})
}

func TestStore_ListPings(t *testing.T) {
	t.Run("empty store returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListPings() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListPings() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("IncludeUnreachable controls whether received=0 rows appear", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)
		insertPingAt(t, s, base.Add(-1*time.Minute), "unreachable.example", 5, 0, 0, "no route to host")
		insertPingAt(t, s, base, "1.1.1.1", 5, 5, 12.5, "")

		def, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListPings(default) error = %v", err)
		}
		if len(def) != 1 || def[0].Result.Host != "1.1.1.1" {
			t.Fatalf("ListPings(default) = %+v, want only the reachable row", def)
		}

		all, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10, IncludeUnreachable: true})
		if err != nil {
			t.Fatalf("ListPings(IncludeUnreachable) error = %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("ListPings(IncludeUnreachable) = %d rows, want 2 (the reachable and the unreachable)", len(all))
		}
	})

	t.Run("Since, Host, and Limit each filter and combine, newest first", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		insertPingAt(t, s, base.Add(-3*time.Hour), "1.1.1.1", 5, 5, 10, "")
		insertPingAt(t, s, base.Add(-2*time.Hour), "8.8.8.8", 5, 5, 20, "")
		insertPingAt(t, s, base.Add(-1*time.Hour), "1.1.1.1", 5, 5, 30, "")
		insertPingAt(t, s, base, "1.1.1.1", 5, 5, 40, "")

		t.Run("Since excludes older rows", func(t *testing.T) {
			got, err := s.ListPings(t.Context(), services.PingFilter{Since: base.Add(-90 * time.Minute), Limit: 10})
			if err != nil {
				t.Fatalf("ListPings() error = %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("ListPings(Since=-90m) = %d rows, want 2 (the -1h and now samples)", len(got))
			}
		})

		t.Run("Host narrows to one host", func(t *testing.T) {
			got, err := s.ListPings(t.Context(), services.PingFilter{Host: "8.8.8.8", Limit: 10})
			if err != nil {
				t.Fatalf("ListPings() error = %v", err)
			}
			if len(got) != 1 || got[0].Result.Host != "8.8.8.8" {
				t.Fatalf("ListPings(Host=8.8.8.8) = %+v, want exactly the one 8.8.8.8 ping", got)
			}
		})

		t.Run("Limit caps the result and Since+Host combine, newest first", func(t *testing.T) {
			got, err := s.ListPings(t.Context(), services.PingFilter{
				Since: base.Add(-150 * time.Minute),
				Host:  "1.1.1.1",
				Limit: 1,
			})
			if err != nil {
				t.Fatalf("ListPings() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ListPings(combined, limit=1) = %d rows, want 1", len(got))
			}
			// two 1.1.1.1 pings fall in range (-1h and now); newest first picks "now".
			if got[0].Result.AvgMS != 40 {
				t.Fatalf("ListPings(combined)[0].Result.AvgMS = %v, want the newest matching sample 40", got[0].Result.AvgMS)
			}
		})
	})

	t.Run("an unreachable result reads back as OK=false, AvgMS=0, with its Error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		insertPingAt(t, s, time.Now(), "unreachable.example", 5, 0, 0, "no route to host")

		// IncludeUnreachable: this asserts an unreachable result round-trips, which
		// the default (received>0) filter would now drop before we could inspect it.
		got, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10, IncludeUnreachable: true})
		if err != nil {
			t.Fatalf("ListPings() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListPings() = %d rows, want 1", len(got))
		}
		if got[0].Result.OK {
			t.Fatal("Result.OK = true, want false for an unreachable result")
		}
		if got[0].Result.AvgMS != 0 {
			t.Fatalf("Result.AvgMS = %v, want 0 (SQL NULL for an unreachable result)", got[0].Result.AvgMS)
		}
		if got[0].Result.Error != "no route to host" {
			t.Fatalf("Result.Error = %q, want %q", got[0].Result.Error, "no route to host")
		}
	})
}

func TestStore_ListRuns(t *testing.T) {
	t.Run("empty store returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.ListRuns(t.Context(), 10)
		if err != nil {
			t.Fatalf("ListRuns() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListRuns() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("newest first and honours limit", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		var ids []int64
		for range 5 {
			id, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
			if err != nil {
				t.Fatalf("InsertRun() error = %v", err)
			}
			ids = append(ids, id)
		}

		got, err := s.ListRuns(t.Context(), 3)
		if err != nil {
			t.Fatalf("ListRuns() error = %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("ListRuns() = %d rows, want 3 (limit honoured)", len(got))
		}

		wantIDs := []int64{ids[4], ids[3], ids[2]}
		for i, run := range got {
			if run.ID != wantIDs[i] {
				t.Fatalf("ListRuns()[%d].ID = %d, want %d (newest first)", i, run.ID, wantIDs[i])
			}
		}
	})
}

func TestStore_ListUnsentOutboxMessages(t *testing.T) {
	t.Run("orders oldest first and excludes sent rows", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		older, err := s.EnqueueOutboxMessage(t.Context(), base.Add(-time.Hour), "older")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage() error = %v", err)
		}
		newer, err := s.EnqueueOutboxMessage(t.Context(), base, "newer")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage() error = %v", err)
		}
		sent, err := s.EnqueueOutboxMessage(t.Context(), base.Add(-2*time.Hour), "already sent")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage() error = %v", err)
		}
		if err := s.MarkOutboxSent(t.Context(), sent, base); err != nil {
			t.Fatalf("MarkOutboxSent() error = %v", err)
		}

		got, err := s.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListUnsentOutboxMessages() = %d rows, want 2 (sent row excluded)", len(got))
		}
		if got[0].ID != older || got[1].ID != newer {
			t.Fatalf("ListUnsentOutboxMessages() ids = [%d, %d], want [%d, %d] oldest first", got[0].ID, got[1].ID, older, newer)
		}
	})
}

func TestStore_IncrementOutboxAttempt(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	id, err := s.EnqueueOutboxMessage(t.Context(), time.Now(), "hello")
	if err != nil {
		t.Fatalf("EnqueueOutboxMessage() error = %v", err)
	}

	if err := s.IncrementOutboxAttempt(t.Context(), id, "dial tcp: timeout"); err != nil {
		t.Fatalf("IncrementOutboxAttempt() error = %v", err)
	}

	unsent, err := s.ListUnsentOutboxMessages(t.Context())
	if err != nil {
		t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
	}
	if len(unsent) != 1 {
		t.Fatalf("ListUnsentOutboxMessages() = %d rows, want the row to remain unsent", len(unsent))
	}
	if unsent[0].Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", unsent[0].Attempts)
	}
	if unsent[0].LastError != "dial tcp: timeout" {
		t.Fatalf("LastError = %q, want %q", unsent[0].LastError, "dial tcp: timeout")
	}
}

func TestStore_MarkOutboxSent(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	id, err := s.EnqueueOutboxMessage(t.Context(), time.Now(), "hello")
	if err != nil {
		t.Fatalf("EnqueueOutboxMessage() error = %v", err)
	}

	if err := s.MarkOutboxSent(t.Context(), id, time.Now()); err != nil {
		t.Fatalf("MarkOutboxSent() error = %v", err)
	}

	unsent, err := s.ListUnsentOutboxMessages(t.Context())
	if err != nil {
		t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
	}
	if len(unsent) != 0 {
		t.Fatalf("ListUnsentOutboxMessages() = %d rows, want 0 after mark-sent", len(unsent))
	}
}

func TestStore_Name(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	if got := s.Name(); got != "sqlite" {
		t.Fatalf("Name() = %q, want %q", got, "sqlite")
	}
}

func TestStore_NewestRunStartedAt(t *testing.T) {
	t.Run("no runs signals none, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		_, ok, err := s.NewestRunStartedAt(t.Context())
		if err != nil {
			t.Fatalf("NewestRunStartedAt() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("NewestRunStartedAt() ok = true, want false with no runs")
		}
	})

	t.Run("returns the most recently inserted run regardless of its action", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		older := now.Add(-3 * time.Hour)
		newer := now.Add(-10 * time.Minute)

		if _, err := s.InsertRun(t.Context(), older, domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		insertReboot(t, s, now.Add(-2*time.Hour)) // a reboot in between must not win just by action
		if _, err := s.InsertRun(t.Context(), newer, domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		got, ok, err := s.NewestRunStartedAt(t.Context())
		if err != nil {
			t.Fatalf("NewestRunStartedAt() error = %v", err)
		}
		if !ok {
			t.Fatal("NewestRunStartedAt() ok = false, want true")
		}
		if !got.Equal(newer) {
			t.Fatalf("NewestRunStartedAt() = %v, want the most recently inserted run %v", got, newer)
		}
	})
}

func TestStore_PruneChecks(t *testing.T) {
	t.Run("retention N deletes rows older than N days and keeps newer ones", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		now := time.Now().UTC()

		insertCheckAt(t, s, runID, now.Add(-100*24*time.Hour))
		insertCheckAt(t, s, runID, now.Add(-1*time.Hour))

		if err := s.PruneChecks(t.Context(), now, 90); err != nil {
			t.Fatalf("PruneChecks() error = %v", err)
		}

		remaining, err := s.ListChecksByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListChecksByRun() error = %v", err)
		}
		if len(remaining) != 1 {
			t.Fatalf("ListChecksByRun() = %d rows after prune, want 1 (only the recent check)", len(remaining))
		}
		if !remaining[0].TS.After(now.Add(-90 * 24 * time.Hour)) {
			t.Fatalf("remaining check TS = %v, want it newer than the 90-day cutoff", remaining[0].TS)
		}
	})

	t.Run("retention zero keeps everything", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		now := time.Now().UTC()

		insertCheckAt(t, s, runID, now.Add(-1000*24*time.Hour))
		insertCheckAt(t, s, runID, now)

		if err := s.PruneChecks(t.Context(), now, 0); err != nil {
			t.Fatalf("PruneChecks() error = %v", err)
		}

		remaining, err := s.ListChecksByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListChecksByRun() error = %v", err)
		}
		if len(remaining) != 2 {
			t.Fatalf("ListChecksByRun() = %d rows after prune with RETENTION_DAYS=0, want both kept (2)", len(remaining))
		}
	})
}

func TestStore_PruneMetrics(t *testing.T) {
	t.Run("retention N deletes both tables' rows older than N days and keeps newer ones", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC()

		oldRunID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		saveMetricsAt(t, s, oldRunID, now.Add(-100*24*time.Hour), "old-host")

		recentRunID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		saveMetricsAt(t, s, recentRunID, now.Add(-1*time.Hour), "recent-host")

		if err := s.PruneMetrics(t.Context(), now, 90); err != nil {
			t.Fatalf("PruneMetrics() error = %v", err)
		}

		if _, err := s.GetHost(t.Context(), oldRunID); err == nil {
			t.Fatal("GetHost(old run) error = nil, want the old host row pruned")
		}
		if remaining, err := s.ListMetricsByRun(t.Context(), oldRunID); err != nil || len(remaining) != 0 {
			t.Fatalf("ListMetricsByRun(old run) = %v (err = %v), want 0 rows after prune", remaining, err)
		}

		host, err := s.GetHost(t.Context(), recentRunID)
		if err != nil {
			t.Fatalf("GetHost(recent run) error = %v, want the recent host row kept", err)
		}
		if host.Hostname != "recent-host" {
			t.Fatalf("GetHost(recent run).Hostname = %q, want %q", host.Hostname, "recent-host")
		}
		if remaining, err := s.ListMetricsByRun(t.Context(), recentRunID); err != nil || len(remaining) != 1 {
			t.Fatalf("ListMetricsByRun(recent run) = %v (err = %v), want 1 row kept", remaining, err)
		}

		if _, err := s.GetRun(t.Context(), oldRunID); err != nil {
			t.Fatalf("GetRun(old run) error = %v, want the runs row itself never pruned", err)
		}
	})

	t.Run("retention zero keeps everything", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		now := time.Now().UTC()
		saveMetricsAt(t, s, runID, now.Add(-1000*24*time.Hour), "ancient-host")

		if err := s.PruneMetrics(t.Context(), now, 0); err != nil {
			t.Fatalf("PruneMetrics() error = %v", err)
		}

		if _, err := s.GetHost(t.Context(), runID); err != nil {
			t.Fatalf("GetHost() error = %v, want the host row kept when RETENTION_DAYS=0", err)
		}
		if remaining, err := s.ListMetricsByRun(t.Context(), runID); err != nil || len(remaining) != 1 {
			t.Fatalf("ListMetricsByRun() = %v (err = %v), want 1 row kept", remaining, err)
		}
	})
}

func TestStore_PrunePings(t *testing.T) {
	t.Run("retention N deletes rows older than N days and keeps newer ones", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC()

		oldRunID := insertPingAt(t, s, now.Add(-100*24*time.Hour), "1.1.1.1", 5, 5, 10, "")
		recentRunID := insertPingAt(t, s, now.Add(-1*time.Hour), "1.1.1.1", 5, 5, 20, "")

		if err := s.PrunePings(t.Context(), now, 90); err != nil {
			t.Fatalf("PrunePings() error = %v", err)
		}

		if remaining, err := s.ListPingsByRun(t.Context(), oldRunID); err != nil || len(remaining) != 0 {
			t.Fatalf("ListPingsByRun(old run) = %v (err = %v), want 0 rows after prune", remaining, err)
		}
		if remaining, err := s.ListPingsByRun(t.Context(), recentRunID); err != nil || len(remaining) != 1 {
			t.Fatalf("ListPingsByRun(recent run) = %v (err = %v), want 1 row kept", remaining, err)
		}
	})

	t.Run("retention zero keeps everything", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC()
		runID := insertPingAt(t, s, now.Add(-1000*24*time.Hour), "1.1.1.1", 5, 5, 10, "")

		if err := s.PrunePings(t.Context(), now, 0); err != nil {
			t.Fatalf("PrunePings() error = %v", err)
		}

		if remaining, err := s.ListPingsByRun(t.Context(), runID); err != nil || len(remaining) != 1 {
			t.Fatalf("ListPingsByRun() = %v (err = %v), want 1 row kept when RETENTION_DAYS=0", remaining, err)
		}
	})
}

func TestStore_RunByID(t *testing.T) {
	t.Run("an existing id is found", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		id, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		run, found, err := s.RunByID(t.Context(), id)
		if err != nil {
			t.Fatalf("RunByID() error = %v", err)
		}
		if !found {
			t.Fatal("RunByID() found = false, want true")
		}
		if run.ID != id {
			t.Fatalf("RunByID().ID = %d, want %d", run.ID, id)
		}
	})

	t.Run("a missing id reports found=false, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		run, found, err := s.RunByID(t.Context(), 999)
		if err != nil {
			t.Fatalf("RunByID() error = %v, want nil", err)
		}
		if found {
			t.Fatal("RunByID() found = true, want false")
		}
		if run != (domain.Run{}) {
			t.Fatalf("RunByID() run = %+v, want the zero value", run)
		}
	})

	t.Run("a real query error propagates", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		_, found, err := s.RunByID(t.Context(), 1)
		if err == nil {
			t.Fatal("RunByID() on a closed store error = nil, want an error")
		}
		if found {
			t.Fatal("RunByID() found = true, want false on error")
		}
	})
}

func TestStore_UpdateRun(t *testing.T) {
	t.Run("applies only the non-nil fields", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		id, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		rebootStartedAt := time.Now().UTC().Truncate(time.Second)
		action := domain.ActionReboot
		if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{
			Action:          &action,
			RebootStartedAt: &rebootStartedAt,
		}); err != nil {
			t.Fatalf("UpdateRun() error = %v", err)
		}

		run, err := s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if run.Action != domain.ActionReboot {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionReboot)
		}
		if run.RebootStartedAt == nil || !run.RebootStartedAt.Equal(rebootStartedAt) {
			t.Fatalf("RebootStartedAt = %v, want %v", run.RebootStartedAt, rebootStartedAt)
		}
		if run.FinishedAt != nil || run.Outcome != "" {
			t.Fatalf("fields not passed in the update changed: FinishedAt=%v Outcome=%q", run.FinishedAt, run.Outcome)
		}

		finishedAt := rebootStartedAt.Add(5 * time.Minute)
		outcome := "ok"
		if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{
			FinishedAt: &finishedAt,
			Outcome:    &outcome,
		}); err != nil {
			t.Fatalf("second UpdateRun() error = %v", err)
		}

		run, err = s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() after second update error = %v", err)
		}
		if run.Outcome != "ok" {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, "ok")
		}
		if run.FinishedAt == nil || !run.FinishedAt.Equal(finishedAt) {
			t.Fatalf("FinishedAt = %v, want %v", run.FinishedAt, finishedAt)
		}
		// the first update's fields must survive the second, narrower update.
		if run.Action != domain.ActionReboot {
			t.Fatalf("Action = %q, want the earlier update's %q to still hold", run.Action, domain.ActionReboot)
		}
	})

	t.Run("no fields set is a no-op, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		id, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{}); err != nil {
			t.Fatalf("UpdateRun(empty) error = %v, want nil", err)
		}
	})
}

// insertCheckAt is a test helper inserting a single passing ip check on
// runID at ts, for retention tests that only care about timestamps.
func insertCheckAt(t *testing.T, s *Store, runID int64, ts time.Time) {
	t.Helper()

	err := s.InsertCheck(t.Context(), domain.Check{
		RunID:  runID,
		TS:     ts,
		Phase:  domain.PhaseInitial,
		Target: "1.1.1.1:443",
		Kind:   domain.CheckKindIP,
		OK:     true,
	})
	if err != nil {
		t.Fatalf("InsertCheck() error = %v", err)
	}
}

// insertMetricAt is a test helper for TestStore_ListMetrics/TestStore_LatestMetrics:
// it inserts a fresh run and saves a single metrics sample for it at ts,
// returning the new run's id. Each call needs its own run since host.run_id
// is a PRIMARY KEY (SaveMetrics can only be called once per run) — unlike
// saveMetricsAt, this helper lets a test choose the sample's collector,
// name, value, and ok, which the fixed-collector saveMetricsAt does not.
func insertMetricAt(t *testing.T, s *Store, ts time.Time, collector domain.Collector, name string, value float64, ok bool) int64 {
	t.Helper()

	runID, err := s.InsertRun(t.Context(), ts, domain.ModeSoft, nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	sample := domain.MetricSample{Collector: collector, Name: name, Value: value, Unit: "unit", OK: ok}
	if !ok {
		sample.Error = "unavailable"
	}
	m := domain.HostMetrics{
		Host:    domain.HostInfo{Hostname: "host", OS: "linux", Arch: "arm64"},
		Samples: []domain.MetricSample{sample},
	}
	if err := s.SaveMetrics(t.Context(), runID, ts, m); err != nil {
		t.Fatalf("SaveMetrics() error = %v", err)
	}

	return runID
}

// insertPingAt is a test helper for TestStore_ListPings/TestStore_PrunePings:
// it inserts a fresh run and saves a single ping result for it at ts,
// returning the new run's id. errStr is stored verbatim (empty means no
// error); ok is derived from received>0, mirroring SavePings/ListPings'
// own contract.
func insertPingAt(t *testing.T, s *Store, ts time.Time, host string, sent, received int, avgMS float64, errStr string) int64 {
	t.Helper()

	runID, err := s.InsertRun(t.Context(), ts, domain.ModeSoft, nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	result := domain.PingResult{Host: host, Sent: sent, Received: received, AvgMS: avgMS, OK: received > 0, Error: errStr}
	if err := s.SavePings(t.Context(), runID, ts, []domain.PingResult{result}); err != nil {
		t.Fatalf("SavePings() error = %v", err)
	}

	return runID
}

// insertReboot is a test helper inserting a runs row with action='reboot'
// and reboot_started_at=at, for GetLastRebootStartedAt tests.
func insertReboot(t *testing.T, s *Store, at time.Time) int64 {
	t.Helper()
	return insertRunWithAction(t, s, domain.ActionReboot, at)
}

// insertRunWithAction is a test helper inserting a runs row whose action
// and reboot_started_at are set directly via UpdateRun, since InsertRun
// always starts a row at action='none'.
func insertRunWithAction(t *testing.T, s *Store, action string, rebootStartedAt time.Time) int64 {
	t.Helper()

	id, err := s.InsertRun(t.Context(), rebootStartedAt, "soft", nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{
		Action:          &action,
		RebootStartedAt: &rebootStartedAt,
	}); err != nil {
		t.Fatalf("UpdateRun() error = %v", err)
	}

	return id
}

// newTestStore opens a fresh :memory: Store for a single test and closes
// it on cleanup. Each call gets its own connection pool (capped at one
// connection, see NewStore) so parallel subtests never share state.
func newTestStore(t *testing.T) *Store {
	t.Helper()

	s, err := NewStore(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	return s
}

// saveMetricsAt is a test helper saving a one-sample telemetry snapshot for
// runID at ts, for prune tests that only care about timestamps and readback.
func saveMetricsAt(t *testing.T, s *Store, runID int64, ts time.Time, hostname string) {
	t.Helper()

	m := domain.HostMetrics{
		Host:    domain.HostInfo{Hostname: hostname, OS: "linux", Arch: "arm64"},
		Samples: []domain.MetricSample{{Collector: domain.CollectorUptime, Name: "uptime", Value: 1, Unit: "seconds", OK: true}},
	}
	if err := s.SaveMetrics(t.Context(), runID, ts, m); err != nil {
		t.Fatalf("SaveMetrics() error = %v", err)
	}
}
