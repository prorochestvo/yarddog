package httpapi

import (
	"log"
	"net/http"
	"time"

	"github.com/prorochestvo/yarddog/services"
)

// pingPath is the one route withAuth (see auth.go) exempts from the
// shared-token check. Both the route registration in register and the
// exemption in withAuth reference this single constant, so a future rename
// can't accidentally gate liveness or ungate a real route.
const pingPath = "/ping"

// NewServer builds a Server serving the query API and health endpoints over
// q and insp, gated by token (see auth.go) on every route but /ping. version
// and started feed /health/check's server identity block
// (gateway/dto.HealthServer): started is normally the process's own start
// time, from which Uptime is computed on every health check.
func NewServer(q *services.QueryService, insp *services.Inspector, token, version string, started time.Time) *Server {
	s := &Server{query: q, inspector: insp, token: token, version: version, started: started}

	mux := http.NewServeMux()
	s.register(mux)
	// logging wraps auth (not the reverse) so a wrong/missing token still
	// produces a log line — an operator watching for a scan or a
	// misconfigured client needs to see the 401s, not just the successes.
	s.handler = loggingMiddleware(withAuth(token, mux))

	return s
}

// Server is the daemon's inbound HTTP adapter (plans/004-query-daemon.md):
// it holds the concrete application-layer types it renders directly — the
// adapter→application direction needs no port of its own, since
// gateway/httpapi already sits at the boundary. Server implements
// http.Handler so cmd/yarddogd can hand it straight to http.Server.
type Server struct {
	query     *services.QueryService
	inspector *services.Inspector
	token     string
	version   string
	started   time.Time
	handler   http.Handler
}

// ServeHTTP implements http.Handler, delegating to the logging- and
// token-auth-wrapped mux built in NewServer.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// register wires every route (plans/004-query-daemon.md API surface) onto
// mux using Go's method-and-path ServeMux patterns; a request for a known
// path with the wrong method gets ServeMux's built-in 405.
func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+pingPath, handlePing)
	mux.HandleFunc("GET /health/check", s.handleHealth)
	mux.HandleFunc("GET /api/v1/host", s.handleLatestHost)
	mux.HandleFunc("GET /api/v1/metrics/latest", s.handleLatestMetrics)
	mux.HandleFunc("GET /api/v1/metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/v1/pings", s.handlePings)
	mux.HandleFunc("GET /api/v1/runs", s.handleRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.handleRunByID)
}

// loggingMiddleware logs each request's method, path, status, and duration
// server-side — never headers, so the shared token (see withAuth) never
// reaches a log line.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r)

		log.Printf("httpapi: %s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

// statusWriter captures the status code written through it so
// loggingMiddleware can log it after the wrapped handler returns.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records status before delegating to the wrapped
// http.ResponseWriter.
func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
