package httpapi

import (
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestCheckDTO(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
	latency := int64(12)

	t.Run("maps every field including latency", func(t *testing.T) {
		t.Parallel()

		c := domain.Check{RunID: "128", TS: ts, Phase: domain.PhaseInitial, Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true, LatencyMS: &latency}

		got := checkDTO(c)

		// run_id is hoisted to RunDetailResponse, so CheckDTO no longer carries it.
		if got.TS != "2026-07-07T04:07:00Z" || got.Phase != "initial" || got.Target != "1.1.1.1:443" || got.Kind != "ip" || !got.OK {
			t.Fatalf("checkDTO() = %+v, unexpected mapping", got)
		}
		if got.LatencyMS == nil || *got.LatencyMS != 12 {
			t.Fatalf("checkDTO().LatencyMS = %v, want 12", got.LatencyMS)
		}
	})

	t.Run("nil latency stays nil", func(t *testing.T) {
		t.Parallel()

		got := checkDTO(domain.Check{RunID: "1", TS: ts, Error: "timeout"})

		if got.LatencyMS != nil {
			t.Fatalf("checkDTO().LatencyMS = %v, want nil", got.LatencyMS)
		}
		if got.Error != "timeout" {
			t.Fatalf("checkDTO().Error = %q, want %q", got.Error, "timeout")
		}
	})
}

func TestCheckDTOs(t *testing.T) {
	t.Parallel()

	t.Run("maps every element in order", func(t *testing.T) {
		t.Parallel()

		checks := []domain.Check{{RunID: "1", Target: "a"}, {RunID: "1", Target: "b"}}

		got := checkDTOs(checks)

		if len(got) != 2 || got[0].Target != "a" || got[1].Target != "b" {
			t.Fatalf("checkDTOs() = %+v, want [a, b] in order", got)
		}
	})

	t.Run("a nil input still returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		got := checkDTOs(nil)

		if got == nil {
			t.Fatal("checkDTOs(nil) = nil, want a non-nil empty slice (must marshal as [] not null)")
		}
		if len(got) != 0 {
			t.Fatalf("checkDTOs(nil) = %+v, want empty", got)
		}
	})
}

func TestFormatNullTime(t *testing.T) {
	t.Parallel()

	t.Run("nil stays nil", func(t *testing.T) {
		t.Parallel()

		if got := formatNullTime(nil); got != nil {
			t.Fatalf("formatNullTime(nil) = %v, want nil", got)
		}
	})

	t.Run("a set time formats as RFC3339", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 3, 0, time.UTC)

		got := formatNullTime(&ts)

		if got == nil || *got != "2026-07-07T04:07:03Z" {
			t.Fatalf("formatNullTime() = %v, want %q", got, "2026-07-07T04:07:03Z")
		}
	})
}

func TestFormatTime(t *testing.T) {
	t.Parallel()

	t.Run("renders UTC RFC3339", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)

		if got := formatTime(ts); got != "2026-07-07T04:07:00Z" {
			t.Fatalf("formatTime() = %q, want %q", got, "2026-07-07T04:07:00Z")
		}
	})

	t.Run("converts a non-UTC time to UTC first", func(t *testing.T) {
		t.Parallel()

		loc := time.FixedZone("UTC+2", 2*60*60)
		ts := time.Date(2026, 7, 7, 6, 7, 0, 0, loc)

		if got := formatTime(ts); got != "2026-07-07T04:07:00Z" {
			t.Fatalf("formatTime() = %q, want the UTC-converted %q", got, "2026-07-07T04:07:00Z")
		}
	})
}

func TestHostDTO(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
	rec := domain.HostRecord{RunID: "128", TS: ts, Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"}}

	got := hostDTO(rec)

	if got.RunID != "128" || got.TS != "2026-07-07T04:07:00Z" || got.Hostname != "pi5" || got.OS != "linux" || got.Arch != "arm64" {
		t.Fatalf("hostDTO() = %+v, unexpected mapping", got)
	}
}

func TestHostInfoDTO(t *testing.T) {
	t.Parallel()

	got := hostInfoDTO(domain.HostInfo{Hostname: "pi5", OS: "Ubuntu 26.04 LTS", Arch: "arm64"})

	if got.Hostname != "pi5" || got.OS != "Ubuntu 26.04 LTS" || got.Arch != "arm64" {
		t.Fatalf("hostInfoDTO() = %+v, unexpected mapping", got)
	}
}

func TestMetricDTO(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)

	t.Run("an ok sample carries its value", func(t *testing.T) {
		t.Parallel()

		rec := domain.MetricRecord{RunID: "128", TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorTemperature, Name: "cpu-thermal", Value: 52.35, Unit: "celsius", OK: true}}

		got := metricDTO(rec)

		if got.Value == nil || *got.Value != 52.35 {
			t.Fatalf("metricDTO().Value = %v, want 52.35", got.Value)
		}
		if got.Collector != "temperature" || got.Name != "cpu-thermal" || got.Unit != "celsius" || !got.OK {
			t.Fatalf("metricDTO() = %+v, unexpected mapping", got)
		}
	})

	t.Run("an unavailable sample has a nil value even though the domain struct's Value field is its zero value", func(t *testing.T) {
		t.Parallel()

		rec := domain.MetricRecord{RunID: "128", TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorFans, Name: "fans", OK: false, Error: "no fan sensors present"}}

		got := metricDTO(rec)

		if got.Value != nil {
			t.Fatalf("metricDTO().Value = %v, want nil for an unavailable sample", *got.Value)
		}
		if got.Error != "no fan sensors present" {
			t.Fatalf("metricDTO().Error = %q, want %q", got.Error, "no fan sensors present")
		}
	})
}

func TestMetricDTOs(t *testing.T) {
	t.Parallel()

	t.Run("a nil input still returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		got := metricDTOs(nil)

		if got == nil {
			t.Fatal("metricDTOs(nil) = nil, want a non-nil empty slice (must marshal as [] not null)")
		}
	})
}

func TestMetricRowDTO(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)

	t.Run("an ok sample carries its value and no run_id/ts field", func(t *testing.T) {
		t.Parallel()

		rec := domain.MetricRecord{RunID: "128", TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorTemperature, Name: "cpu-thermal", Value: 52.35, Unit: "celsius", OK: true}}

		got := metricRowDTO(rec)

		if got.Value == nil || *got.Value != 52.35 {
			t.Fatalf("metricRowDTO().Value = %v, want 52.35", got.Value)
		}
		if got.Collector != "temperature" || got.Name != "cpu-thermal" || got.Unit != "celsius" || !got.OK {
			t.Fatalf("metricRowDTO() = %+v, unexpected mapping", got)
		}
	})

	t.Run("an unavailable sample has a nil value", func(t *testing.T) {
		t.Parallel()

		got := metricRowDTO(domain.MetricRecord{RunID: "128", TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorFans, Name: "fans", OK: false, Error: "no fan sensors present"}})

		if got.Value != nil {
			t.Fatalf("metricRowDTO().Value = %v, want nil for an unavailable sample", *got.Value)
		}
		if got.Error != "no fan sensors present" {
			t.Fatalf("metricRowDTO().Error = %q, want %q", got.Error, "no fan sensors present")
		}
	})
}

func TestMetricRowDTOs(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
	records := []domain.MetricRecord{
		{RunID: "1", TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorTemperature, Name: "cpu-thermal", Value: 52.35, Unit: "celsius", OK: true}},
		{RunID: "1", TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorFans, Name: "fans", OK: false, Error: "no fan sensors present"}},
	}

	t.Run("drops unavailable rows by default", func(t *testing.T) {
		t.Parallel()

		got := metricRowDTOs(records, false)

		if len(got) != 1 || got[0].Name != "cpu-thermal" {
			t.Fatalf("metricRowDTOs(_, false) = %+v, want only the ok row", got)
		}
	})

	t.Run("includeEmpty keeps unavailable rows", func(t *testing.T) {
		t.Parallel()

		if got := metricRowDTOs(records, true); len(got) != 2 {
			t.Fatalf("metricRowDTOs(_, true) len = %d, want 2", len(got))
		}
	})

	t.Run("a nil input still returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		if metricRowDTOs(nil, true) == nil {
			t.Fatal("metricRowDTOs(nil, true) = nil, want a non-nil empty slice (must marshal as [] not null)")
		}
	})
}

func TestPingDTO(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)

	t.Run("a reachable result carries its avg_ms", func(t *testing.T) {
		t.Parallel()

		rec := domain.PingRecord{RunID: "128", TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 12.5, OK: true}}

		got := pingDTO(rec)

		if got.AvgMS == nil || *got.AvgMS != 12.5 {
			t.Fatalf("pingDTO().AvgMS = %v, want 12.5", got.AvgMS)
		}
		if got.Host != "1.1.1.1" || got.Sent != 5 || got.Received != 5 || !got.OK {
			t.Fatalf("pingDTO() = %+v, unexpected mapping", got)
		}
	})

	t.Run("an unreachable result has a nil avg_ms even though the domain struct's AvgMS field is its zero value", func(t *testing.T) {
		t.Parallel()

		rec := domain.PingRecord{RunID: "128", TS: ts, Result: domain.PingResult{Host: "unreachable.example", Sent: 5, Received: 0, OK: false, Error: "no route to host"}}

		got := pingDTO(rec)

		if got.AvgMS != nil {
			t.Fatalf("pingDTO().AvgMS = %v, want nil for an unreachable result", *got.AvgMS)
		}
		if got.Error != "no route to host" {
			t.Fatalf("pingDTO().Error = %q, want %q", got.Error, "no route to host")
		}
	})
}

func TestPingDTOs(t *testing.T) {
	t.Parallel()

	t.Run("a nil input still returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		got := pingDTOs(nil)

		if got == nil {
			t.Fatal("pingDTOs(nil) = nil, want a non-nil empty slice (must marshal as [] not null)")
		}
		if len(got) != 0 {
			t.Fatalf("pingDTOs(nil) = %+v, want empty", got)
		}
	})

	t.Run("maps every element in order", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		records := []domain.PingRecord{
			{RunID: "1", TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 10, OK: true}},
			{RunID: "1", TS: ts, Result: domain.PingResult{Host: "unreachable.example", Sent: 5, Received: 0, OK: false, Error: "no route to host"}},
		}

		got := pingDTOs(records)

		if len(got) != 2 || got[0].Host != "1.1.1.1" || got[1].Host != "unreachable.example" {
			t.Fatalf("pingDTOs() = %+v, want both hosts in order", got)
		}
		if got[0].AvgMS == nil || *got[0].AvgMS != 10 {
			t.Fatalf("pingDTOs()[0].AvgMS = %v, want 10", got[0].AvgMS)
		}
		if got[1].AvgMS != nil {
			t.Fatalf("pingDTOs()[1].AvgMS = %v, want nil (unreachable)", *got[1].AvgMS)
		}
	})
}

func TestRunDTO(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)

	t.Run("every optional field nil stays nil", func(t *testing.T) {
		t.Parallel()

		r := domain.Run{ID: "128", StartedAt: startedAt, Mode: domain.ModeHard, Action: domain.ActionNone}

		got := runDTO(r)

		if got.InternetOK != nil || got.RebootStartedAt != nil || got.RouterDownAt != nil ||
			got.RouterUpAt != nil || got.InternetRestoredAt != nil || got.FinishedAt != nil {
			t.Fatalf("runDTO() = %+v, want every optional field nil", got)
		}
		if got.StartedAt != "2026-07-07T04:07:00Z" {
			t.Fatalf("runDTO().StartedAt = %q, want %q", got.StartedAt, "2026-07-07T04:07:00Z")
		}
	})

	t.Run("every optional field set is formatted", func(t *testing.T) {
		t.Parallel()

		internetOK := true
		finishedAt := startedAt.Add(3 * time.Second)
		r := domain.Run{
			ID: "128", StartedAt: startedAt, Mode: domain.ModeSoft, InternetOK: &internetOK,
			Action: domain.ActionReboot, RebootStartedAt: &startedAt, FinishedAt: &finishedAt,
			Outcome: domain.OutcomeOK,
		}

		got := runDTO(r)

		if got.InternetOK == nil || !*got.InternetOK {
			t.Fatalf("runDTO().InternetOK = %v, want true", got.InternetOK)
		}
		if got.RebootStartedAt == nil || *got.RebootStartedAt != "2026-07-07T04:07:00Z" {
			t.Fatalf("runDTO().RebootStartedAt = %v, want %q", got.RebootStartedAt, "2026-07-07T04:07:00Z")
		}
		if got.FinishedAt == nil || *got.FinishedAt != "2026-07-07T04:07:03Z" {
			t.Fatalf("runDTO().FinishedAt = %v, want %q", got.FinishedAt, "2026-07-07T04:07:03Z")
		}
		if got.Outcome != domain.OutcomeOK {
			t.Fatalf("runDTO().Outcome = %q, want %q", got.Outcome, domain.OutcomeOK)
		}
	})
}

func TestRunDTOs(t *testing.T) {
	t.Parallel()

	t.Run("a nil input still returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		got := runDTOs(nil)

		if got == nil {
			t.Fatal("runDTOs(nil) = nil, want a non-nil empty slice (must marshal as [] not null)")
		}
	})

	t.Run("maps every element in order", func(t *testing.T) {
		t.Parallel()

		runs := []domain.Run{{ID: "1"}, {ID: "2"}}

		got := runDTOs(runs)

		if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
			t.Fatalf("runDTOs() = %+v, want ids [1, 2] in order", got)
		}
	})
}
