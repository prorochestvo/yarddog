package services

import (
	"context"
	"fmt"
	"time"
)

// HealthProbe is one dependency /health/check inspects (plans/004-query-daemon.md,
// the inspector pattern): Name is the stable label the report keys on (e.g.
// "sqlite"), and CheckUP reports that dependency's own health — nil means
// healthy, any other error is shown verbatim in the report.
type HealthProbe interface {
	Name() string
	CheckUP(ctx context.Context) error
}

// NewInspector builds an Inspector over probes, bounding every CheckUp sweep
// to timeout regardless of how many probes are registered or how slow any
// one of them is.
func NewInspector(timeout time.Duration, probes ...HealthProbe) *Inspector {
	return &Inspector{probes: probes, timeout: timeout}
}

// Inspector aggregates every registered HealthProbe into one readiness
// report (plans/004-query-daemon.md): a single dependency failing or hanging
// never hides or blocks the others, and never crashes the sweep.
type Inspector struct {
	probes  []HealthProbe
	timeout time.Duration
}

// CheckUp runs every probe under one whole-sweep context.WithTimeout
// deadline (i.timeout total, not per probe): healthy is true only when every
// probe reports ok; report maps each probe's Name to "ok" or its verbatim
// CheckUP error. A probe with an empty Name is skipped — it has nothing to
// key the report on. Probes are tried sequentially and every one is always
// attempted (a failure is recorded and the loop continues); the shared
// deadline, not a per-probe timeout, is what keeps a wedged probe from
// hanging the whole sweep, so long as the probe itself honours ctx.
//
// that shared deadline is not reset per probe: a probe that blocks until
// the deadline starves every probe registered after it — they run against
// an already-expired ctx and report their own ctx error rather than being
// skipped or given a fresh budget. this is not a bug to fix by probing
// concurrently: today's two real probes both read through the same *sql.DB
// handle, so a genuine wedge is a shared-fate failure either way, and the
// property that actually matters here is the whole-sweep bound, not
// per-probe isolation.
func (i *Inspector) CheckUp(ctx context.Context) (healthy bool, report map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()

	healthy, report = true, make(map[string]string, len(i.probes))
	for _, p := range i.probes {
		name := p.Name()
		if name == "" {
			continue
		}

		if err := probeSafely(ctx, p); err != nil {
			report[name] = err.Error()
			healthy = false
			continue
		}
		report[name] = "ok"
	}

	return healthy, report
}

// probeSafely calls p.CheckUP(ctx), recovering a panic into an error instead
// of letting one broken probe crash the whole health sweep.
func probeSafely(ctx context.Context, p HealthProbe) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return p.CheckUP(ctx)
}
