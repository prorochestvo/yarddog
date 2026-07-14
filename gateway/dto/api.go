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
// sidecar row. RunID is a UUIDv7 string (issue #4).
type HostResponse struct {
	RunID    string `json:"run_id"`
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
// it carries its own run_id/ts. RunID is a UUIDv7 string (issue #4). Value is
// nil (JSON null) exactly when OK is false — an unavailable, unsupported, or
// failed sample never carries a measured value.
type MetricDTO struct {
	RunID     string   `json:"run_id"`
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
// RunID (a UUIDv7 string, issue #4)/TS and Host identity (hoisted here, not
// repeated per row — the collector writes the host row with the metrics
// rows, so that run always has one). Rows with an unavailable sample are
// omitted unless ?include_empty=true.
type MetricsLatestResponse struct {
	RunID   string         `json:"run_id"`
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

// PingDTO is one pings row of the multi-run list (GET /api/v1/pings), so it
// carries its own run_id/ts (issue #2). RunID is a UUIDv7 string (issue #4).
// AvgMS is nil (JSON null) exactly when OK is false — an unreachable host
// never carries a measured round trip.
type PingDTO struct {
	RunID    string   `json:"run_id"`
	TS       string   `json:"ts"`
	Host     string   `json:"host"`
	Sent     int      `json:"sent"`
	Received int      `json:"received"`
	AvgMS    *float64 `json:"avg_ms"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error"`
}

// PingsListResponse is the body of GET /api/v1/pings: filtered ping history,
// each row carrying its own RunID/TS since it may span many runs.
// Unreachable rows are omitted unless ?include_unreachable=true.
type PingsListResponse struct {
	Pings []PingDTO `json:"pings"`
}

// RunDTO mirrors domain.Run. ID is a UUIDv7 string (issue #4) — an
// API-visible change from the previously numeric id. Every *At field is nil
// (JSON null) until that phase transition has happened, matching the domain
// type's own semantics.
type RunDTO struct {
	ID                 string  `json:"id"`
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

// OverviewResponse is the body of GET /api/v1/overview (plans/010): the
// dashboard's server-downsampled multi-day view, one bucketed series per
// metric collector/name and per configured ping host.
type OverviewResponse struct {
	Window  OverviewWindowDTO `json:"window"`
	Metrics []MetricSeriesDTO `json:"metrics"`
	Pings   []PingSeriesDTO   `json:"pings"`
}

// OverviewWindowDTO describes the resolved [Since, Until] span and bucket
// width an OverviewResponse was computed over — resolved server-side
// (defaults/clamps), never echoing a raw, possibly-absent request param.
// Bucket is a Go duration string (e.g. "1h0m0s"), not a number of seconds.
type OverviewWindowDTO struct {
	Since  string `json:"since"`
	Until  string `json:"until"`
	Bucket string `json:"bucket"`
}

// MetricSeriesDTO is one (Collector, Name) metric's bucketed history within
// an OverviewResponse.
type MetricSeriesDTO struct {
	Collector string            `json:"collector"`
	Name      string            `json:"name"`
	Unit      string            `json:"unit"`
	Buckets   []MetricBucketDTO `json:"buckets"`
}

// MetricBucketDTO is one time bucket of a MetricSeriesDTO. Count is always
// >0 — an empty bucket is simply absent from Buckets, never a zero row.
type MetricBucketDTO struct {
	TS    string  `json:"ts"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Avg   float64 `json:"avg"`
	Count int     `json:"count"`
}

// PingSeriesDTO is one host's bucketed ping reachability plus every outage
// episode detected within the window, inside an OverviewResponse.
type PingSeriesDTO struct {
	Host    string          `json:"host"`
	Buckets []PingBucketDTO `json:"buckets"`
	Outages []PingOutageDTO `json:"outages"`
}

// PingBucketDTO is one time bucket of a PingSeriesDTO. LossPct is derived
// from Sent/Received (0 when Sent is 0, per domain.LossPercent). AvgMS and MaxMS are nil (JSON
// null) exactly when Received is 0 for the whole bucket — no reply ever came
// back, so there is no round trip to report.
type PingBucketDTO struct {
	TS       string   `json:"ts"`
	Sent     int      `json:"sent"`
	Received int      `json:"received"`
	LossPct  int      `json:"loss_pct"`
	AvgMS    *float64 `json:"avg_ms"`
	MaxMS    *float64 `json:"max_ms"`
	Samples  int      `json:"samples"`
}

// PingOutageDTO is one detected outage episode within a PingSeriesDTO. Kind
// is "unreachable" when at least one sample in the episode had zero
// replies, "loss" when every sample kept at least a partial reply.
type PingOutageDTO struct {
	Start        string `json:"start"`
	End          string `json:"end"`
	Kind         string `json:"kind"`
	WorstLossPct int    `json:"worst_loss_pct"`
}

// ErrorResponse is the body of every 4xx/5xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}
