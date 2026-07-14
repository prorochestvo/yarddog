package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestHandleDashboard(t *testing.T) {
	t.Parallel()

	const marker = "yarddog status --watch" // a stable string from the embedded page

	t.Run("serves the embedded page at the exact root", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")
		rec := doRequest(t, srv, http.MethodGet, "/", "tok")

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
		}
		if body := rec.Body.String(); !strings.Contains(body, marker) {
			t.Fatalf("body does not contain the expected page marker %q", marker)
		}
	})

	t.Run("the root page needs no token", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")
		rec := doRequest(t, srv, http.MethodGet, "/", "") // no Authorization header

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (root is ungated, like /ping)", rec.Code, http.StatusOK)
		}
	})

	t.Run("an unknown path is a clean 404, not the dashboard", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")
		// a valid token so the request reaches the mux; the exact-root {$}
		// pattern must not catch /nope.
		rec := doRequest(t, srv, http.MethodGet, "/nope", "tok")

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d (exact-root {$} must not catch unknown paths)", rec.Code, http.StatusNotFound)
		}
		if strings.Contains(rec.Body.String(), marker) {
			t.Fatal("an unknown path served the dashboard body; the root pattern is not exact-root")
		}
	})

	t.Run("the root exemption does not leak to gated API routes", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics", "") // no token

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (only /ping and / are ungated)", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("the embedded asset is non-empty", func(t *testing.T) {
		t.Parallel()

		if len(dashboardHTML) == 0 {
			t.Fatal("dashboardHTML is empty: the //go:embed path is broken or the asset is empty")
		}
	})
}
