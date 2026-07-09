package infrastructure

import (
	"strings"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

var _ services.Clock = fakeClock{}

func TestFreshnessProbe_CheckUP(t *testing.T) {
	t.Run("a run at now is healthy", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		if _, err := s.InsertRun(t.Context(), now, domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		p := NewFreshnessProbe(s, fakeClock{now: now}, 90*time.Minute)
		if err := p.CheckUP(t.Context()); err != nil {
			t.Fatalf("CheckUP() error = %v, want nil for a fresh run", err)
		}
	})

	t.Run("a run older than staleAfter names the age and the budget", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		startedAt := now.Add(-2 * time.Hour)
		if _, err := s.InsertRun(t.Context(), startedAt, domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		p := NewFreshnessProbe(s, fakeClock{now: now}, 90*time.Minute)

		err := p.CheckUP(t.Context())
		if err == nil {
			t.Fatal("CheckUP() error = nil, want an error for a stale run")
		}
		if !strings.Contains(err.Error(), "2h0m0s") {
			t.Errorf("CheckUP() error = %q, want it to name the run's age (2h0m0s)", err)
		}
		if !strings.Contains(err.Error(), "1h30m0s") {
			t.Errorf("CheckUP() error = %q, want it to name the staleness budget (1h30m0s)", err)
		}
	})

	t.Run("no runs recorded yet is unhealthy", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		p := NewFreshnessProbe(s, fakeClock{now: time.Now()}, 90*time.Minute)

		err := p.CheckUP(t.Context())
		if err == nil {
			t.Fatal("CheckUP() error = nil, want an error when no run has ever been recorded")
		}
		if err.Error() != "no runs recorded yet" {
			t.Fatalf("CheckUP() error = %q, want %q", err, "no runs recorded yet")
		}
	})
}

func TestFreshnessProbe_Name(t *testing.T) {
	t.Parallel()

	p := NewFreshnessProbe(newTestStore(t), fakeClock{}, time.Hour)
	if got := p.Name(); got != "collector-freshness" {
		t.Fatalf("Name() = %q, want %q", got, "collector-freshness")
	}
}

// fakeClock is a settable stand-in for services.Clock: Now returns a fixed
// point in time (the zero value if unset). After is unused by
// freshnessProbe but implemented so fakeClock satisfies the interface.
type fakeClock struct {
	now time.Time
}

func (f fakeClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (f fakeClock) Now() time.Time { return f.now }
