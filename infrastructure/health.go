package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prorochestvo/yarddog/services"
)

// NewFreshnessProbe builds a freshnessProbe over store, reporting unhealthy
// once the newest run is older than staleAfter (or if there is no run at
// all). clock lets tests control "now" deterministically instead of racing
// the real wall clock; production wiring passes SystemClock.
func NewFreshnessProbe(store *Store, clock services.Clock, staleAfter time.Duration) *freshnessProbe {
	return &freshnessProbe{store: store, clock: clock, staleAfter: staleAfter}
}

// freshnessProbe implements services.HealthProbe by checking that the
// collector (a separate process) is still writing runs (plans/004-query-daemon.md):
// the daemon only reads the shared database, so a stale newest run means the
// cron collector has stopped, and the data /api/v1/* serves is going stale
// even though the daemon process itself is perfectly healthy. Unexported:
// its single consumer is the daemon's composition root, via NewFreshnessProbe.
type freshnessProbe struct {
	store      *Store
	clock      services.Clock
	staleAfter time.Duration
}

var _ services.HealthProbe = (*freshnessProbe)(nil)

// CheckUP reports unhealthy when no run has ever been recorded, or when the
// newest run is older than p.staleAfter; nil otherwise.
func (p *freshnessProbe) CheckUP(ctx context.Context) error {
	startedAt, ok, err := p.store.NewestRunStartedAt(ctx)
	if err != nil {
		return fmt.Errorf("check collector freshness: %w", err)
	}
	if !ok {
		return errors.New("no runs recorded yet")
	}

	if age := p.clock.Now().Sub(startedAt); age > p.staleAfter {
		return fmt.Errorf("last run %s ago exceeds %s staleness budget", age.Round(time.Second), p.staleAfter)
	}

	return nil
}

// Name implements services.HealthProbe, naming this probe
// "collector-freshness" in a /health/check report.
func (p *freshnessProbe) Name() string { return "collector-freshness" }
