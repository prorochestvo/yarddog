package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authHeader is the standard HTTP authorization header every gated route
// requires. bearerPrefix is the scheme token (RFC 7235 §2.1: scheme is
// case-insensitive, but we normalise via strings.ToLower before stripping).
const (
	authHeader   = "Authorization"
	bearerPrefix = "bearer "
)

// withAuth gates every route but pingPath and rootPath (the secret-free
// dashboard, see ui.go) behind a Bearer-token check. The
// static shared secret travels in the Authorization header as
// "Bearer <token>" (RFC 6750 §2.1 format, though the token is NOT
// OAuth-issued — it is a static LAN secret set via DAEMON_TOKEN, never
// rotated by an authorization server). The scheme token is compared
// case-insensitively per RFC 7235; only the credential itself is compared
// with subtle.ConstantTimeCompare so neither a missing nor a wrong token
// leaks a timing signal; the value never appears in a log line or error body.
// An empty configured token always fails closed: ConstantTimeCompare("","")
// reports a match, so without the explicit token=="" guard a misconfigured
// empty DAEMON_TOKEN would authenticate every header-less request.
// LoadDaemonConfig already rejects an empty DAEMON_TOKEN, but this boundary
// does not rely on that — it stays safe by construction even if a future
// caller wires it up some other way.
func withAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pingPath || r.URL.Path == rootPath {
			next.ServeHTTP(w, r)
			return
		}

		raw := r.Header.Get(authHeader)
		// normalise the scheme only — timing-variable ops are safe here
		// because the scheme is not the secret.
		lower := strings.ToLower(raw)
		if !strings.HasPrefix(lower, bearerPrefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		got := raw[len(bearerPrefix):]

		// ConstantTimeCompare returns 0 (not a panic) on a length mismatch,
		// so there is no reason to check len() first — doing so would
		// reintroduce the timing side-channel this check exists to avoid.
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}
