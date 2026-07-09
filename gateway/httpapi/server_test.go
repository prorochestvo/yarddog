package httpapi

import (
	"net/http"
	"testing"
)

func TestServer_ServeHTTP(t *testing.T) {
	t.Run("unknown path is 404", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/nope", "tok")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})

	t.Run("wrong method on a known path is 405", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/runs", "tok")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("a gated route without a token is 401 through the real server, not just withAuth in isolation", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("every registered route is reachable with a valid token", func(t *testing.T) {
		t.Parallel()

		repo := &fakeRepo{runByIDOK: true}
		srv := newTestServer(repo, "tok", &fakeHealthProbe{name: "sqlite"})

		for _, path := range []string{
			"/ping",
			"/health/check",
			"/api/v1/host",
			"/api/v1/metrics/latest",
			"/api/v1/metrics",
			"/api/v1/runs",
			"/api/v1/runs/1",
		} {
			rec := doRequest(t, srv, http.MethodGet, path, "tok")
			if rec.Code >= http.StatusInternalServerError {
				t.Errorf("GET %s = %d, want a registered route (not a 5xx)", path, rec.Code)
			}
		}
	})
}
