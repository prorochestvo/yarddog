package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

var _ HealthProbe = (*fakeProbe)(nil)

func TestInspector_CheckUp(t *testing.T) {
	t.Run("all probes run even when one fails, and the failure is reported verbatim", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("disk full")
		insp := NewInspector(time.Second, &fakeProbe{name: "sqlite"}, &fakeProbe{name: "disk", err: wantErr})

		healthy, report := insp.CheckUp(t.Context())

		if healthy {
			t.Fatal("CheckUp() healthy = true, want false (one probe failed)")
		}
		if report["sqlite"] != "ok" {
			t.Fatalf(`report["sqlite"] = %q, want "ok"`, report["sqlite"])
		}
		if report["disk"] != wantErr.Error() {
			t.Fatalf(`report["disk"] = %q, want %q`, report["disk"], wantErr.Error())
		}
	})

	t.Run("every probe healthy reports healthy=true", func(t *testing.T) {
		t.Parallel()

		insp := NewInspector(time.Second, &fakeProbe{name: "a"}, &fakeProbe{name: "b"})

		healthy, report := insp.CheckUp(t.Context())

		if !healthy {
			t.Fatalf("CheckUp() healthy = false, want true; report = %v", report)
		}
		if len(report) != 2 || report["a"] != "ok" || report["b"] != "ok" {
			t.Fatalf("report = %v, want both a and b ok", report)
		}
	})

	t.Run("a probe that blocks past the timeout does not hang the sweep", func(t *testing.T) {
		t.Parallel()

		budget := 50 * time.Millisecond
		insp := NewInspector(budget, &fakeProbe{name: "wedged", block: true})

		start := time.Now()
		healthy, report := insp.CheckUp(t.Context())
		elapsed := time.Since(start)

		if healthy {
			t.Fatal("CheckUp() healthy = true, want false (the wedged probe never returns ok)")
		}
		if elapsed > 2*budget {
			t.Fatalf("CheckUp() took %v, want it bounded near the %v budget", elapsed, budget)
		}
		if report["wedged"] == "" {
			t.Fatal(`report["wedged"] is empty, want the probe's ctx-cancelled error`)
		}
	})

	t.Run("a panicking probe is recovered and reported unhealthy", func(t *testing.T) {
		t.Parallel()

		insp := NewInspector(time.Second, &fakeProbe{name: "flaky", panics: true})

		healthy, report := insp.CheckUp(t.Context())

		if healthy {
			t.Fatal("CheckUp() healthy = true, want false (the probe panicked)")
		}
		if report["flaky"] == "" {
			t.Fatal(`report["flaky"] is empty, want the recovered panic's message`)
		}
	})

	t.Run("a wedged probe starves a probe registered after it, but neither is dropped from the report", func(t *testing.T) {
		t.Parallel()

		budget := 50 * time.Millisecond
		insp := NewInspector(budget, &fakeProbe{name: "wedged", block: true}, &fakeProbe{name: "healthy"})

		start := time.Now()
		healthy, report := insp.CheckUp(t.Context())
		elapsed := time.Since(start)

		if elapsed > 2*budget {
			t.Fatalf("CheckUp() took %v, want it bounded near the %v whole-sweep budget", elapsed, budget)
		}
		if healthy {
			t.Fatal("CheckUp() healthy = true, want false (the wedged probe never returns ok)")
		}
		if _, ok := report["wedged"]; !ok {
			t.Fatalf(`report = %v, want a "wedged" entry`, report)
		}
		// the second probe is starved by the exhausted shared deadline (see
		// CheckUp's doc comment) and so is not expected to report "ok" — but
		// its exact error is ctx-derived and not asserted here, only that it
		// still made it into the report rather than being silently dropped.
		if _, ok := report["healthy"]; !ok {
			t.Fatalf(`report = %v, want a "healthy" entry even though it ran starved`, report)
		}
	})

	t.Run("a probe with an empty Name is skipped", func(t *testing.T) {
		t.Parallel()

		insp := NewInspector(time.Second, &fakeProbe{name: ""}, &fakeProbe{name: "real"})

		healthy, report := insp.CheckUp(t.Context())

		if !healthy {
			t.Fatalf("CheckUp() healthy = false, want true; report = %v", report)
		}
		if len(report) != 1 {
			t.Fatalf("report = %v, want exactly 1 entry (the empty-name probe skipped)", report)
		}
	})
}

// fakeProbe is a settable HealthProbe stand-in: it returns err (nil = ok)
// unless ctx is already done, mirroring a real dependency probe (e.g. a
// sqlite ping) that honours the context it's given rather than ignoring it;
// it can also optionally block until ctx is cancelled (mimicking a wedged
// dependency) or panic instead of returning — exercising Inspector's
// recover-wrap and its whole-sweep timeout.
type fakeProbe struct {
	name   string
	err    error
	block  bool
	panics bool
}

func (f *fakeProbe) CheckUP(ctx context.Context) error {
	if f.panics {
		panic("fakeProbe: simulated panic")
	}
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.err
}

func (f *fakeProbe) Name() string { return f.name }
