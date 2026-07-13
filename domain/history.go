package domain

import "time"

// HostRecord is a persisted host table row: 003's HostInfo plus the run it
// was captured on. Returned by services.HistoryRepository.LatestHost,
// analogous to how Run is a persisted runs row. RunID is the owning run's
// UUIDv7 string id (issue #4) — host.run_id is also that row's own primary
// key, not a separately generated one.
type HostRecord struct {
	RunID string
	TS    time.Time
	Host  HostInfo
}

// MetricRecord is a persisted metrics table row: 003's MetricSample plus the
// run it was captured on. Returned by services.HistoryRepository's
// LatestMetrics and ListMetrics, analogous to how Check is a persisted
// checks row. RunID is the owning run's UUIDv7 string id (issue #4).
type MetricRecord struct {
	RunID  string
	TS     time.Time
	Sample MetricSample
}
