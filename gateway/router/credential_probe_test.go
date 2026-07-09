package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prorochestvo/yarddog/services"
)

// fakeCredentialer is a test double for services.Credentialer: it counts calls
// and returns a programmable error.
type fakeCredentialer struct {
	calls atomic.Int64
	err   error
	// block, when non-nil, is received before CheckCredentials returns — used
	// in concurrency tests to park goroutines inside the call long enough for
	// the test to verify coalescing.
	block <-chan struct{}
	// ready, when non-nil, is sent-to each time a goroutine enters CheckUP
	// via the single-flight path AND is about to block on gate — allowing a
	// concurrency test to count how many are parked before releasing gate.
	// It is sent from inside the fake (not the probe) because only the one
	// goroutine that wins the in-flight race actually calls CheckCredentials;
	// the rest park on c.done in CheckUP without calling the fake at all.
	// For the single-flight coalescing test, one send (from the one caller)
	// is all that is needed.
	ready chan<- struct{}
	// panicMsg, when non-empty, causes CheckCredentials to panic with that value.
	panicMsg string
}

var _ services.Credentialer = (*fakeCredentialer)(nil)

func (f *fakeCredentialer) CheckCredentials(_ context.Context) error {
	f.calls.Add(1)
	if f.ready != nil {
		f.ready <- struct{}{} // signal: in-flight call is now executing
	}
	if f.block != nil {
		<-f.block
	}
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	return f.err
}

func TestCredentialProbe_Name(t *testing.T) {
	t.Parallel()

	p := NewCredentialProbe("gipon", &fakeCredentialer{})
	if got := p.Name(); got != "gipon" {
		t.Fatalf("Name() = %q, want %q", got, "gipon")
	}
}

func TestCredentialProbe_CheckUP(t *testing.T) {
	t.Parallel()

	t.Run("healthy cred returns nil", func(t *testing.T) {
		t.Parallel()

		cred := &fakeCredentialer{}
		p := NewCredentialProbe("router", cred)

		if err := p.CheckUP(t.Context()); err != nil {
			t.Fatalf("CheckUP() error = %v, want nil", err)
		}
		if cred.calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", cred.calls.Load())
		}
	})

	t.Run("cred error is returned verbatim", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("login failed")
		cred := &fakeCredentialer{err: wantErr}
		p := NewCredentialProbe("router", cred)

		err := p.CheckUP(t.Context())
		if !errors.Is(err, wantErr) {
			t.Fatalf("CheckUP() error = %v, want it to wrap %v", err, wantErr)
		}
	})

	t.Run("each independent CheckUP starts a fresh login", func(t *testing.T) {
		t.Parallel()

		cred := &fakeCredentialer{}
		p := NewCredentialProbe("router", cred)

		if err := p.CheckUP(t.Context()); err != nil {
			t.Fatalf("first CheckUP() error = %v", err)
		}
		if err := p.CheckUP(t.Context()); err != nil {
			t.Fatalf("second CheckUP() error = %v", err)
		}
		if got := cred.calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2 (two sequential CheckUPs each login once)", got)
		}
	})

	t.Run("concurrent CheckUPs coalesce into exactly one login", func(t *testing.T) {
		t.Parallel()

		// Prove the single-flight invariant: N CheckUP calls that arrive while
		// one login is in-flight all share that one result. Exactly one
		// CheckCredentials call must happen.
		//
		// Synchronization:
		// (a) gate blocks CheckCredentials so the in-flight call stays open.
		// (b) ready signals once CheckCredentials has started — confirming
		//     inflight is set and gate is the only thing keeping it open.
		// (c) onWait is called (from inside the probe, under the mutex) by
		//     each goroutine that finds inflight!=nil and is about to park on
		//     c.done. We count n-1 onWait calls to confirm all waiters are
		//     parked before releasing gate — this is the deterministic barrier.
		const n = 5
		gate := make(chan struct{})
		ready := make(chan struct{}, 1)
		cred := &fakeCredentialer{block: gate, ready: ready}
		p := NewCredentialProbe("router", cred).(*credentialProbe)

		parked := make(chan struct{}, n)
		p.onWait = func() { parked <- struct{}{} }

		var wg sync.WaitGroup
		wg.Add(n)
		for range n {
			go func() {
				defer wg.Done()
				_ = p.CheckUP(context.Background())
			}()
		}

		// wait until CheckCredentials is executing — inflight is now set.
		<-ready

		// wait until n-1 goroutines have parked on c.done (onWait fires for
		// each waiter, under the mutex, before it blocks). Once we've seen n-1
		// parked signals, every caller is either the one in CheckCredentials
		// or parked on c.done — none can start a new call.
		for range n - 1 {
			<-parked
		}

		close(gate) // release the in-flight call; all waiters unblock
		wg.Wait()

		if got := cred.calls.Load(); got != 1 {
			t.Fatalf("calls = %d, want 1 (N concurrent CheckUPs must share one login)", got)
		}
	})

	t.Run("ctx cancellation while waiting returns ctx error, not the shared result", func(t *testing.T) {
		t.Parallel()

		gate := make(chan struct{})
		ready := make(chan struct{}, 1)
		cred := &fakeCredentialer{block: gate, ready: ready}
		p := NewCredentialProbe("router", cred)

		// start one goroutine that occupies the in-flight slot.
		go func() {
			_ = p.CheckUP(context.Background())
		}()

		// wait until CheckCredentials is executing so inflight is set and
		// the next CheckUP will find it (rather than starting a new call).
		<-ready

		// cancel before gate is closed so the second CheckUP's ctx.Done fires
		// in the select rather than waiting for the in-flight result.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := p.CheckUP(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CheckUP() with cancelled ctx error = %v, want context.Canceled", err)
		}

		close(gate) // release the blocked goroutine
	})

	t.Run("panic in CheckCredentials is recovered and returned as error", func(t *testing.T) {
		t.Parallel()

		// A panic inside CheckCredentials must not escape as a runtime panic.
		// The probe must: (a) return a non-nil error, (b) propagate that same
		// non-nil error to any coalesced waiter, and (c) clear inflight so
		// subsequent calls start fresh (no permanent wedge).
		//
		// Synchronization via onWait: the waiter goroutine signals ready once
		// it is parked on c.done; we release the gate only after that, ensuring
		// the panic fires while a waiter is in flight.
		ready := make(chan struct{}, 1)
		gate := make(chan struct{})
		cred := &fakeCredentialer{block: gate, panicMsg: "unexpected nil pointer"}
		concrete := NewCredentialProbe("router", cred).(*credentialProbe)

		// track when the waiter is parked.
		waiterReady := make(chan struct{}, 1)
		concrete.onWait = func() { waiterReady <- struct{}{} }

		// the fake's ready channel signals when CheckCredentials has started.
		cred.ready = ready

		waiterErrCh := make(chan error, 1)
		go func() {
			waiterErrCh <- concrete.CheckUP(context.Background())
		}()

		// wait until CheckCredentials is executing (inflight is set).
		<-ready

		// start a waiter that will park on c.done.
		waiter2ErrCh := make(chan error, 1)
		go func() {
			waiter2ErrCh <- concrete.CheckUP(context.Background())
		}()
		<-waiterReady // waiter is parked

		// release the gate — CheckCredentials will panic.
		close(gate)

		err1 := <-waiterErrCh
		err2 := <-waiter2ErrCh

		if err1 == nil {
			t.Fatal("CheckUP() = nil after panic, want non-nil error")
		}
		if err2 == nil {
			t.Fatal("coalesced waiter CheckUP() = nil after panic, want non-nil error")
		}

		// subsequent call must not block forever — inflight must have been cleared.
		cred2 := &fakeCredentialer{}
		concrete.cred = cred2
		if err := concrete.CheckUP(t.Context()); err != nil {
			t.Fatalf("subsequent CheckUP() after panic = %v, want nil (no permanent wedge)", err)
		}
		if cred2.calls.Load() != 1 {
			t.Fatalf("subsequent calls = %d, want 1", cred2.calls.Load())
		}
	})
}
