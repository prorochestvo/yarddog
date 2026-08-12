package httpapi

import (
	"bytes"
	"encoding/base64"
	"image/png"
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

	// The favicon rides inline in the page rather than as a second embedded
	// asset, so nothing but this test stands between a stray edit and a tab
	// that silently falls back to the browser placeholder. Asserted by marker,
	// never by the exact blob: the mark is regenerated from time to time.
	t.Run("the page carries the favicon as an inline PNG data URI", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")
		body := doRequest(t, srv, http.MethodGet, "/", "tok").Body.String()

		const iconAttr = `rel="icon"`
		if n := strings.Count(body, iconAttr); n != 1 {
			t.Fatalf("the page has %d %s links, want exactly 1", n, iconAttr)
		}

		link := body[strings.Index(body, iconAttr):]
		if end := strings.IndexByte(link, '>'); end >= 0 {
			link = link[:end]
		}

		const marker = "data:image/png;base64,"
		i := strings.Index(link, marker)
		if i < 0 {
			t.Fatalf("the icon href is not a %q URI: the favicon must stay inline so the dashboard remains a single self-contained asset", marker)
		}
		encoded := link[i+len(marker):]
		end := strings.IndexByte(encoded, '"')
		if end < 0 {
			t.Fatal("the icon data URI has no closing quote")
		}

		raw, err := base64.StdEncoding.DecodeString(encoded[:end])
		if err != nil {
			t.Fatalf("decode the icon payload: %v", err)
		}
		// Decode in full, not just the header: a truncated blob still carries
		// an intact IHDR, so DecodeConfig alone would wave a broken icon through.
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("the icon payload is not a valid PNG: %v", err)
		}
		if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
			t.Fatalf("icon is %dx%d, want 32x32", b.Dx(), b.Dy())
		}
	})
}
