package dto

// PingResponse is the body of GET /ping (plans/004-query-daemon.md): a
// liveness probe that touches no dependency, so its shape never varies.
type PingResponse struct {
	Status string `json:"status"`
}

// HealthResponse is the body of GET /health/check: Status is true (HTTP 200)
// only when every entry in Services reports "ok"; any other value is that
// dependency's verbatim error and Status is false (HTTP 503).
type HealthResponse struct {
	Status   bool              `json:"status"`
	Server   HealthServer      `json:"server"`
	Services map[string]string `json:"services"`
}

// HealthServer is HealthResponse's server identity block. Uptime is a
// preformatted duration string (e.g. "2h34m12s"), not a number of seconds.
type HealthServer struct {
	Version string `json:"version"`
	Uptime  string `json:"uptime"`
}

// HostResponse is the body of GET /api/v1/host: the newest host-identity
// sidecar row.
type HostResponse struct {
	RunID    int64  `json:"run_id"`
	TS       string `json:"ts"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// HostInfoDTO is the host-identity block embedded in MetricsLatestResponse:
// the same hostname/os/arch as HostResponse without the run_id/ts, which the
// enclosing response already carries. OS is a description ("Ubuntu 26.04 LTS").
type HostInfoDTO struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// MetricDTO is one metrics row of the multi-run list (GET /api/v1/metrics), so
// it carries its own run_id/ts. Value is nil (JSON null) exactly when OK is
// false — an unavailable, unsupported, or failed sample never carries a
// measured value.
type MetricDTO struct {
	RunID     int64    `json:"run_id"`
	TS        string   `json:"ts"`
	Collector string   `json:"collector"`
	Name      string   `json:"name"`
	Value     *float64 `json:"value"`
	Unit      string   `json:"unit"`
	OK        bool     `json:"ok"`
	Error     string   `json:"error"`
}

// MetricRowDTO is one metrics row of a single-run response
// (MetricsLatestResponse): run_id/ts are hoisted to the enclosing response, so
// the row omits them. Value nil-ness matches MetricDTO.
type MetricRowDTO struct {
	Collector string   `json:"collector"`
	Name      string   `json:"name"`
	Value     *float64 `json:"value"`
	Unit      string   `json:"unit"`
	OK        bool     `json:"ok"`
	Error     string   `json:"error"`
}

// MetricsLatestResponse is the body of GET /api/v1/metrics/latest: the
// available metrics rows of the newest run that has any, sharing that run's
// RunID/TS and Host identity (hoisted here, not repeated per row — the
// collector writes the host row with the metrics rows, so that run always has
// one). Rows with an unavailable sample are omitted unless ?include_empty=true.
type MetricsLatestResponse struct {
	RunID   int64          `json:"run_id"`
	TS      string         `json:"ts"`
	Host    HostInfoDTO    `json:"host"`
	Metrics []MetricRowDTO `json:"metrics"`
}

// MetricsListResponse is the body of GET /api/v1/metrics: filtered metrics
// history, each row carrying its own RunID/TS since it may span many runs.
// Unavailable rows are omitted unless ?include_empty=true.
type MetricsListResponse struct {
	Metrics []MetricDTO `json:"metrics"`
}

// RunDTO mirrors domain.Run. Every *At field is nil (JSON null) until that
// phase transition has happened, matching the domain type's own semantics.
type RunDTO struct {
	ID                 int64   `json:"id"`
	StartedAt          string  `json:"started_at"`
	Mode               string  `json:"mode"`
	InternetOK         *bool   `json:"internet_ok"`
	Action             string  `json:"action"`
	RebootStartedAt    *string `json:"reboot_started_at"`
	RouterDownAt       *string `json:"router_down_at"`
	RouterUpAt         *string `json:"router_up_at"`
	InternetRestoredAt *string `json:"internet_restored_at"`
	FinishedAt         *string `json:"finished_at"`
	Outcome            string  `json:"outcome"`
	Error              string  `json:"error"`
}

// CheckDTO mirrors domain.Check within RunDetailResponse — always single-run,
// so run_id is hoisted to the enclosing run and omitted here. LatencyMS is nil
// (JSON null) when the probe recorded no duration (e.g. it timed out before one
// was measured).
type CheckDTO struct {
	TS        string `json:"ts"`
	Phase     string `json:"phase"`
	Target    string `json:"target"`
	Kind      string `json:"kind"`
	OK        bool   `json:"ok"`
	LatencyMS *int64 `json:"latency_ms"`
	Error     string `json:"error"`
}

// RunsListResponse is the body of GET /api/v1/runs: newest-first, up to the
// request's clamped limit.
type RunsListResponse struct {
	Runs []RunDTO `json:"runs"`
}

// RunDetailResponse is the body of GET /api/v1/runs/{id}: one run and every
// checks row recorded against it.
type RunDetailResponse struct {
	Run    RunDTO     `json:"run"`
	Checks []CheckDTO `json:"checks"`
}

// ErrorResponse is the body of every 4xx/5xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}
