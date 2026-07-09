package router

import (
	"context"
	"fmt"
	"sync"

	"github.com/prorochestvo/yarddog/services"
)

// NewCredentialProbe builds a services.HealthProbe that reports router
// authentication health under name, calling cred.CheckCredentials on every
// CheckUP — live on every request, with no time-based cache (operator
// decision: /health/check must reflect current state). Concurrent CheckUP
// calls are coalesced into one in-flight login via a stdlib single-flight so a
// burst of health requests does not open multiple simultaneous router sessions.
// name is the stable label shown in the /health/check report (e.g. "router"
// or "gipon"); cred is the driver that performs the real login/logout.
func NewCredentialProbe(name string, cred services.Credentialer) services.HealthProbe {
	return &credentialProbe{name: name, cred: cred}
}

// credentialProbe implements services.HealthProbe by calling
// cred.CheckCredentials on every sweep. Concurrent calls are coalesced: if a
// login is already in flight when CheckUP is called, the caller waits for that
// shared result rather than opening a second session. Once the in-flight call
// returns, the next CheckUP starts fresh — there is no time-based staleness.
type credentialProbe struct {
	name string
	cred services.Credentialer

	mu       sync.Mutex
	inflight *call

	// onWait, when non-nil, is called just before a goroutine blocks on an
	// in-flight call's done channel. Used only in tests to signal that a waiter
	// is parked, making the single-flight concurrency test deterministic without
	// requiring real sleeps. Never set in production.
	onWait func()
}

var _ services.HealthProbe = (*credentialProbe)(nil)

// Name implements services.HealthProbe, returning the display name supplied at
// construction (e.g. "router" or "gipon").
func (p *credentialProbe) Name() string { return p.name }

// CheckUP implements services.HealthProbe. If a login is already in flight
// (another concurrent request started it), CheckUP waits for that shared result
// and returns it — at most one session is open at a time. If none is in flight,
// CheckUP starts a fresh login, stores the in-flight call, and runs it; all
// waiters receive the same result when it completes. A panic in
// CheckCredentials is recovered and converted to an error, and the in-flight
// slot is always cleared so subsequent calls can start a new attempt. ctx is
// forwarded to CheckCredentials so the Inspector's whole-sweep deadline can
// cancel a wedged router login.
func (p *credentialProbe) CheckUP(ctx context.Context) (err error) {
	p.mu.Lock()
	if p.inflight != nil {
		c := p.inflight
		if p.onWait != nil {
			p.onWait()
		}
		p.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	p.inflight = c
	p.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("router credential probe panicked: %v", r)
		}
		c.err = err
		close(c.done)
		p.mu.Lock()
		p.inflight = nil
		p.mu.Unlock()
	}()

	err = p.cred.CheckCredentials(ctx)
	return err
}

// call is a single in-flight CheckCredentials invocation shared by all
// concurrent CheckUP callers. done is closed when the call finishes; err
// carries its result.
type call struct {
	done chan struct{}
	err  error
}
