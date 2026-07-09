package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithAuth(t *testing.T) {
	t.Parallel()

	const token = "s3cr3t"

	t.Run("valid Bearer token passes through", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()

		withAuth(token, discard).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("wrong token returns 401", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		rec := httptest.NewRecorder()

		withAuth(token, discard).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("missing Authorization header returns 401", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		rec := httptest.NewRecorder()

		withAuth(token, discard).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("raw token without Bearer scheme returns 401", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", token)
		rec := httptest.NewRecorder()

		withAuth(token, discard).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (raw token without scheme must be rejected)", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("Basic scheme with the token value returns 401", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", "Basic "+token)
		rec := httptest.NewRecorder()

		withAuth(token, discard).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (non-Bearer scheme must be rejected)", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("lowercase bearer scheme is accepted (RFC 7235 case-insensitive)", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", "bearer "+token)
		rec := httptest.NewRecorder()

		withAuth(token, discard).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (lowercase bearer must be accepted)", rec.Code, http.StatusOK)
		}
	})

	t.Run("/ping bypasses auth even with no header at all", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest(http.MethodGet, "/ping", nil)
		rec := httptest.NewRecorder()

		withAuth(token, discard).ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (/ping must never require the token)", rec.Code, http.StatusOK)
		}
	})

	t.Run("empty configured token fails closed even with a matching-looking header", func(t *testing.T) {
		t.Parallel()

		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		req.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()

		withAuth("", next).ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d (an empty configured token must never authenticate)", rec.Code, http.StatusUnauthorized)
		}
		if called {
			t.Fatal("next was called, want it never invoked when the configured token is empty")
		}
	})
}
