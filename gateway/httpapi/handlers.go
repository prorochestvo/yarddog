package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/gateway/dto"
	"github.com/prorochestvo/yarddog/services"
)

// handleHealth runs the Inspector's whole-sweep readiness check and reports
// 200 when every dependency is healthy, 503 otherwise.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	healthy, report := s.inspector.CheckUp(r.Context())

	status := http.StatusOK
	if !healthy {
		status = http.StatusServiceUnavailable
	}

	writeJSON(w, status, dto.HealthResponse{
		Status: healthy,
		Server: dto.HealthServer{
			Version: s.version,
			Uptime:  time.Since(s.started).Round(time.Second).String(),
		},
		Services: report,
	})
}

// handleLatestHost answers GET /api/v1/host: 404 {"error":"no data"} until
// the collector has recorded at least one host snapshot.
func (s *Server) handleLatestHost(w http.ResponseWriter, r *http.Request) {
	rec, ok, err := s.query.LatestHost(r.Context())
	if err != nil {
		writeError500(w, "latest host", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no data")
		return
	}

	writeJSON(w, http.StatusOK, hostDTO(rec))
}

// handleLatestMetrics answers GET /api/v1/metrics/latest: every metrics row
// of the newest run that has any; 404 {"error":"no data"} until the
// collector has recorded at least one.
func (s *Server) handleLatestMetrics(w http.ResponseWriter, r *http.Request) {
	includeEmpty, err := parseBoolParam(r.URL.Query(), "include_empty")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	records, err := s.query.LatestMetrics(r.Context())
	if err != nil {
		writeError500(w, "latest metrics", err)
		return
	}
	// records is every row of the newest run that has any (null cells included),
	// so an all-null run is still a 200 with the run's identity below and an
	// empty metrics array — the 404 keys on "no metric rows at all".
	if len(records) == 0 {
		writeError(w, http.StatusNotFound, "no data")
		return
	}

	// the collector writes the host row together with the metrics rows, so the
	// run these records belong to has a host row; embed it. A missing one
	// (ok=false, not expected here) degrades to an empty host block, never a
	// failed metrics response.
	host, _, err := s.query.LatestHost(r.Context())
	if err != nil {
		writeError500(w, "latest metrics host", err)
		return
	}

	writeJSON(w, http.StatusOK, dto.MetricsLatestResponse{
		RunID:   records[0].RunID,
		TS:      formatTime(records[0].TS),
		Host:    hostInfoDTO(host.Host),
		Metrics: metricRowDTOs(records, includeEmpty),
	})
}

// handleMetrics answers GET /api/v1/metrics: filtered metrics history, 200
// with an empty array when nothing matches — unlike the "latest" singleton
// above, an empty list here is a valid answer, not a 404.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	filter, err := parseMetricsFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	records, err := s.query.Metrics(r.Context(), filter)
	if err != nil {
		writeError500(w, "list metrics", err)
		return
	}

	writeJSON(w, http.StatusOK, dto.MetricsListResponse{
		Metrics: metricDTOs(records),
	})
}

// handlePing answers the liveness probe: always 200, touches no dependency
// (plans/004-query-daemon.md — the one ungated route).
func handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, dto.PingResponse{Status: "ok"})
}

// handlePings answers GET /api/v1/pings: filtered ping history, 200 with an
// empty array when nothing matches (issue #2, mirrors handleMetrics — an
// empty list here is a valid answer, not a 404).
func (s *Server) handlePings(w http.ResponseWriter, r *http.Request) {
	filter, err := parsePingsFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	records, err := s.query.Pings(r.Context(), filter)
	if err != nil {
		writeError500(w, "list pings", err)
		return
	}

	writeJSON(w, http.StatusOK, dto.PingsListResponse{
		Pings: pingDTOs(records),
	})
}

// handleRunByID answers GET /api/v1/runs/{id}: the run plus every checks
// row recorded against it; 400 for an empty id, 404 for an unknown one. id
// is an opaque UUIDv7 string (issue #4) — there is no numeric format to
// validate beyond emptiness, so an arbitrary non-matching string simply
// resolves to 404 rather than 400.
func (s *Server) handleRunByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing run id")
		return
	}

	run, checks, found, err := s.query.Run(r.Context(), id)
	if err != nil {
		writeError500(w, "get run", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}

	writeJSON(w, http.StatusOK, dto.RunDetailResponse{
		Run:    runDTO(run),
		Checks: checkDTOs(checks),
	})
}

// handleRuns answers GET /api/v1/runs: newest-first, 200 with an empty
// array when the collector has never run.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit, err := parseIntParam(r.URL.Query(), "limit", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	runs, err := s.query.Runs(r.Context(), limit)
	if err != nil {
		writeError500(w, "list runs", err)
		return
	}

	writeJSON(w, http.StatusOK, dto.RunsListResponse{
		Runs: runDTOs(runs),
	})
}

// parseBoolParam parses q's key as a bool, returning false when the key is
// absent or empty. Accepts strconv.ParseBool's forms (1/t/T/true/0/f/F/false,
// and their variants); anything else is the caller's mistake, reported as an
// error the handler turns into a 400 — never silently coerced to false.
func parseBoolParam(q url.Values, key string) (bool, error) {
	raw := q.Get(key)
	if raw == "" {
		return false, nil
	}

	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s %q", key, raw)
	}
	return v, nil
}

// parseIntParam parses q's key as a base-10 int, returning def when the key
// is absent or empty (services.QueryService already treats limit<=0 as
// "use my default", so handing it 0 here is enough — no default resolution
// belongs at this layer). An unparseable value is the caller's mistake,
// reported by returning an error the handler turns into a 400.
func parseIntParam(q url.Values, key string, def int) (int, error) {
	raw := q.Get(key)
	if raw == "" {
		return def, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", key, raw)
	}
	return v, nil
}

// parseMetricsFilter builds a services.MetricsFilter from the request's
// query params: since (RFC3339), collector (one of domain's seven), and
// limit (int) are all optional. archive (bool, issue #4) opts into spanning
// the metrics_archive twin; omitted or false stays hot-only. An unparseable
// value is reported as an error the handler turns into a 400, never
// silently ignored or coerced into "no filter".
func parseMetricsFilter(q url.Values) (services.MetricsFilter, error) {
	var f services.MetricsFilter

	if raw := q.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return services.MetricsFilter{}, fmt.Errorf("invalid since %q: not RFC3339", raw)
		}
		f.Since = since
	}

	if raw := q.Get("collector"); raw != "" {
		collector := domain.Collector(raw)
		if !validCollector(collector) {
			return services.MetricsFilter{}, fmt.Errorf("invalid collector %q", raw)
		}
		f.Collector = collector
	}

	limit, err := parseIntParam(q, "limit", 0)
	if err != nil {
		return services.MetricsFilter{}, err
	}
	f.Limit = limit

	includeEmpty, err := parseBoolParam(q, "include_empty")
	if err != nil {
		return services.MetricsFilter{}, err
	}
	f.IncludeEmpty = includeEmpty

	includeArchive, err := parseBoolParam(q, "archive")
	if err != nil {
		return services.MetricsFilter{}, err
	}
	f.IncludeArchive = includeArchive

	return f, nil
}

// parsePingsFilter builds a services.PingFilter from the request's query
// params (issue #2): since (RFC3339), host, limit (int), and
// include_unreachable (bool) are all optional. archive (bool, issue #4)
// opts into spanning the pings_archive twin; omitted or false stays
// hot-only. An unparseable value is reported as an error the handler turns
// into a 400, never silently ignored or coerced into "no filter".
func parsePingsFilter(q url.Values) (services.PingFilter, error) {
	var f services.PingFilter

	if raw := q.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return services.PingFilter{}, fmt.Errorf("invalid since %q: not RFC3339", raw)
		}
		f.Since = since
	}

	f.Host = q.Get("host")

	limit, err := parseIntParam(q, "limit", 0)
	if err != nil {
		return services.PingFilter{}, err
	}
	f.Limit = limit

	includeUnreachable, err := parseBoolParam(q, "include_unreachable")
	if err != nil {
		return services.PingFilter{}, err
	}
	f.IncludeUnreachable = includeUnreachable

	includeArchive, err := parseBoolParam(q, "archive")
	if err != nil {
		return services.PingFilter{}, err
	}
	f.IncludeArchive = includeArchive

	return f, nil
}

// validCollector reports whether c is one of the seven collectors
// domain.MetricsSettings recognizes, so an unparseable ?collector= is a 400
// instead of silently matching nothing.
func validCollector(c domain.Collector) bool {
	switch c {
	case domain.CollectorTemperature, domain.CollectorFans, domain.CollectorCPU,
		domain.CollectorMemory, domain.CollectorDisk, domain.CollectorUptime, domain.CollectorNetwork:
		return true
	default:
		return false
	}
}

// writeError writes an ErrorResponse{Error: msg} body at status. msg must
// already be safe to show a LAN client (a validation message, "not found",
// "unauthorized", …) — never call this with a raw driver/SQL error; see
// writeError500 for that path.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, dto.ErrorResponse{Error: msg})
}

// writeError500 logs err's full detail server-side (it may contain SQL text
// or other internal topology) and returns a generic 500 body to the LAN
// client — never the error itself.
func writeError500(w http.ResponseWriter, context string, err error) {
	log.Printf("httpapi: %s: %v", context, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// writeJSON writes status and encodes v as the JSON response body.
// Content-Type is set before WriteHeader (headers are frozen once
// WriteHeader is called). An Encode failure can only be logged — the status
// and headers have already been sent to the client by this point.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("httpapi: encode response: %v", err)
	}
}
