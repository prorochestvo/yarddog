package infrastructure

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestNewChecker(t *testing.T) {
	t.Parallel()

	t.Run("http scheme without explicit port defaults to 80", func(t *testing.T) {
		t.Parallel()

		c, err := NewChecker(nil, nil, "http://192.168.1.1", time.Second)
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		if c.gatewayTarget != "192.168.1.1:80" {
			t.Fatalf("gatewayTarget = %q, want %q", c.gatewayTarget, "192.168.1.1:80")
		}
	})

	t.Run("https scheme without explicit port defaults to 443", func(t *testing.T) {
		t.Parallel()

		c, err := NewChecker(nil, nil, "https://192.168.1.1", time.Second)
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		if c.gatewayTarget != "192.168.1.1:443" {
			t.Fatalf("gatewayTarget = %q, want %q", c.gatewayTarget, "192.168.1.1:443")
		}
	})

	t.Run("explicit port is preserved", func(t *testing.T) {
		t.Parallel()

		c, err := NewChecker(nil, nil, "http://192.168.1.1:8080", time.Second)
		if err != nil {
			t.Fatalf("NewChecker: %v", err)
		}
		if c.gatewayTarget != "192.168.1.1:8080" {
			t.Fatalf("gatewayTarget = %q, want %q", c.gatewayTarget, "192.168.1.1:8080")
		}
	})

	t.Run("url with no host is an error", func(t *testing.T) {
		t.Parallel()

		if _, err := NewChecker(nil, nil, "not-a-url", time.Second); err == nil {
			t.Fatal("NewChecker: want error for hostless address, got nil")
		}
	})
}

func TestChecker_Check(t *testing.T) {
	t.Parallel()

	t.Run("one down target among several is not a down verdict", func(t *testing.T) {
		t.Parallel()

		up := newReachableListener(t)
		down := newClosedAddr(t)
		domainUp := newStatusServer(t, http.StatusNoContent)

		c := newTestChecker(t, []string{up, down}, []string{domainUp}, time.Second)
		result := c.Check(t.Context())

		if result.Down {
			t.Fatalf("Down = true, want false (only one ip target failed): %+v", result.Targets)
		}
	})

	t.Run("all ip targets down is a down verdict regardless of domains", func(t *testing.T) {
		t.Parallel()

		down1 := newClosedAddr(t)
		down2 := newClosedAddr(t)
		domainUp := newStatusServer(t, http.StatusNoContent)

		c := newTestChecker(t, []string{down1, down2}, []string{domainUp}, time.Second)
		result := c.Check(t.Context())

		if !result.Down {
			t.Fatalf("Down = false, want true (all ip targets failed): %+v", result.Targets)
		}
	})

	t.Run("ip up but all domains down is a down verdict", func(t *testing.T) {
		t.Parallel()

		up := newReachableListener(t)
		domainDown1 := newStatusServer(t, http.StatusInternalServerError)
		domainDown2 := newStatusServer(t, http.StatusBadGateway)

		c := newTestChecker(t, []string{up}, []string{domainDown1, domainDown2}, time.Second)
		result := c.Check(t.Context())

		if !result.Down {
			t.Fatalf("Down = false, want true (all domain targets failed): %+v", result.Targets)
		}
	})

	t.Run("at least one ip and one domain up is an up verdict", func(t *testing.T) {
		t.Parallel()

		up := newReachableListener(t)
		down := newClosedAddr(t)
		domainUp := newStatusServer(t, http.StatusNoContent)
		domainDown := newStatusServer(t, http.StatusInternalServerError)

		c := newTestChecker(t, []string{up, down}, []string{domainUp, domainDown}, time.Second)
		result := c.Check(t.Context())

		if result.Down {
			t.Fatalf("Down = true, want false: %+v", result.Targets)
		}
	})

	t.Run("non-2xx domain status is recorded as a failure with the status in error", func(t *testing.T) {
		t.Parallel()

		domainDown := newStatusServer(t, http.StatusFound)

		c := newTestChecker(t, nil, []string{domainDown}, time.Second)
		result := c.Check(t.Context())

		if len(result.Targets) != 1 {
			t.Fatalf("len(Targets) = %d, want 1", len(result.Targets))
		}
		got := result.Targets[0]
		if got.OK {
			t.Fatalf("OK = true, want false for a %d response", http.StatusFound)
		}
		if got.Error == "" {
			t.Fatal("Error is empty, want the response status recorded")
		}
	})

	t.Run("a redirect is not followed into a false up", func(t *testing.T) {
		t.Parallel()

		target := newStatusServer(t, http.StatusFound)

		c := newTestChecker(t, nil, []string{target}, time.Second)
		result := c.Check(t.Context())

		if result.Targets[0].OK {
			t.Fatal("OK = true, want false: a 302 must not be followed into a 2xx success")
		}
	})

	t.Run("latency is recorded for successful probes", func(t *testing.T) {
		t.Parallel()

		const delay = 20 * time.Millisecond
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(srv.Close)

		c := newTestChecker(t, nil, []string{srv.URL}, time.Second)
		result := c.Check(t.Context())

		got := result.Targets[0]
		if !got.OK {
			t.Fatalf("OK = false, want true: %+v", got)
		}
		if got.Latency < delay {
			t.Fatalf("Latency = %v, want at least %v", got.Latency, delay)
		}
	})

	t.Run("a hung domain target fails within the configured timeout", func(t *testing.T) {
		t.Parallel()

		const timeout = 30 * time.Millisecond
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// block until the client gives up (its Timeout cancels the
			// request context and drops the connection) rather than a fixed
			// real sleep, so httptest.Server.Close doesn't have to wait out
			// a long timer for this handler to notice and return.
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
				w.WriteHeader(http.StatusNoContent)
			}
		}))
		t.Cleanup(srv.Close)

		c := newTestChecker(t, nil, []string{srv.URL}, timeout)

		start := time.Now()
		result := c.Check(t.Context())
		elapsed := time.Since(start)

		if result.Targets[0].OK {
			t.Fatal("OK = true, want false: server never responds within the timeout")
		}
		if elapsed > 5*timeout {
			t.Fatalf("Check took %v, want it bounded near the %v timeout", elapsed, timeout)
		}
	})

	t.Run("an already-expired context fails the probe immediately", func(t *testing.T) {
		t.Parallel()

		up := newReachableListener(t)
		c := newTestChecker(t, []string{up}, nil, time.Second)

		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()

		result := c.Check(ctx)

		if result.Targets[0].OK {
			t.Fatal("OK = true, want false: dial with an already-expired context must fail")
		}
		if result.Targets[0].Error == "" {
			t.Fatal("Error is empty, want the context error recorded")
		}
	})
}

func TestChecker_Gateway(t *testing.T) {
	t.Parallel()

	t.Run("reachable gateway is ok", func(t *testing.T) {
		t.Parallel()

		up := newReachableListener(t)
		c := newTestChecker(t, nil, nil, time.Second)
		c.gatewayTarget = up

		got := c.Gateway(t.Context())

		if !got.OK {
			t.Fatalf("OK = false, want true: %+v", got)
		}
		if got.Kind != domain.CheckKindGateway {
			t.Fatalf("Kind = %q, want %q", got.Kind, domain.CheckKindGateway)
		}
	})

	t.Run("unreachable gateway is not ok", func(t *testing.T) {
		t.Parallel()

		down := newClosedAddr(t)
		c := newTestChecker(t, nil, nil, time.Second)
		c.gatewayTarget = down

		got := c.Gateway(t.Context())

		if got.OK {
			t.Fatal("OK = true, want false: nothing is listening on this port")
		}
		if got.Error == "" {
			t.Fatal("Error is empty, want the dial error recorded")
		}
	})
}

// newClosedAddr returns a loopback host:port that is guaranteed to refuse a
// connection: a listener is opened and immediately closed, so the port is
// known-free but nothing answers on it.
func newClosedAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// newReachableListener opens a loopback TCP listener that accepts and
// immediately closes every connection, giving probeTCP something to
// successfully dial.
func newReachableListener(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Logf("close listener: %v", err)
		}
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if err := conn.Close(); err != nil {
				return
			}
		}
	}()

	return ln.Addr().String()
}

// newStatusServer starts an httptest.Server that always answers with code
// and no body, standing in for the generate_204/cdn-cgi/trace targets.
func newStatusServer(t *testing.T, code int) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// newTestChecker builds a Checker directly against a dummy, always-resolving
// router address so tests can exercise Check/Gateway without depending on
// hostPortFromURL's default-port behavior (covered separately by
// TestNewChecker).
func newTestChecker(t *testing.T, ipTargets, domainTargets []string, timeout time.Duration) *Checker {
	t.Helper()

	c, err := NewChecker(ipTargets, domainTargets, "http://127.0.0.1", timeout)
	if err != nil {
		t.Fatalf("NewChecker: %v", err)
	}
	return c
}
