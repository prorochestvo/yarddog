package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestResolveEnvPath(t *testing.T) {
	t.Run("explicit flag wins over everything", func(t *testing.T) {
		t.Parallel()

		got := resolveEnvPath("/some/explicit/path")
		if want := "/some/explicit/path"; got != want {
			t.Fatalf("resolveEnvPath(explicit) = %q, want %q", got, want)
		}
	})

	t.Run("falls back when default path is absent", func(t *testing.T) {
		t.Parallel()

		// defaultEnvPath (/opt/yarddog/.env) is not expected to exist in the
		// test sandbox; if it does, this subtest can't observe the fallback
		// branch and is skipped rather than asserting a false negative.
		if _, err := os.Stat(defaultEnvPath); err == nil {
			t.Skipf("%s exists in this environment, cannot exercise the fallback branch", defaultEnvPath)
		}

		got := resolveEnvPath("")
		if got != fallbackEnvPath {
			t.Fatalf("resolveEnvPath(\"\") = %q, want fallback %q", got, fallbackEnvPath)
		}
	})
}

func TestServe(t *testing.T) {
	t.Run("answers a request, then drains cleanly on ctx cancellation", func(t *testing.T) {
		t.Parallel()

		addr := freeAddr(t)
		mux := http.NewServeMux()
		mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		httpServer := &http.Server{Addr: addr, Handler: mux}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan int, 1)
		go func() { done <- serve(ctx, httpServer) }()

		waitForServer(t, addr)

		resp, err := http.Get("http://" + addr + "/ping")
		if err != nil {
			t.Fatalf("GET /ping: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ping status = %d, want %d", resp.StatusCode, http.StatusOK)
		}

		cancel()

		select {
		case code := <-done:
			if code != domain.ExitOK {
				t.Fatalf("serve() = %d, want %d after a clean shutdown", code, domain.ExitOK)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("serve() did not return within 5s of ctx cancellation")
		}
	})

	t.Run("a bind failure returns exitListenError", func(t *testing.T) {
		t.Parallel()

		addr := freeAddr(t)

		// hold the address so the serve() call below fails to bind it.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("net.Listen(%q): %v", addr, err)
		}
		defer func() {
			if err := ln.Close(); err != nil {
				t.Errorf("close holder listener: %v", err)
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		code := serve(ctx, &http.Server{Addr: addr, Handler: http.NewServeMux()})
		if code != exitListenError {
			t.Fatalf("serve() = %d, want %d for an address already in use", code, exitListenError)
		}
	})
}

// freeAddr returns a "127.0.0.1:<port>" address that was free at the moment
// of the call: it briefly binds an ephemeral port (:0) and closes it again.
// This is the standard — if in theory TOCTOU-racy — way to hand a test a
// concrete port before the real server under test binds it.
func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close temporary listener: %v", err)
	}
	return addr
}

// waitForServer polls addr until something accepts a connection (or fails
// the test once its own short deadline runs out), so the request right
// after doesn't race serve's ListenAndServe against a listener that has not
// bound yet.
func waitForServer(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			if cerr := conn.Close(); cerr != nil {
				t.Errorf("close probe connection: %v", cerr)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server at %s did not start accepting connections in time", addr)
}
