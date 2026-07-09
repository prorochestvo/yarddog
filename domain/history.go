package domain

import "time"

// HostRecord is a persisted host table row: 003's HostInfo plus the run it
// was captured on. Returned by services.HistoryRepository.LatestHost,
// analogous to how Run is a persisted runs row.
type HostRecord struct {
	RunID int64
	TS    time.Time
	Host  HostInfo
}

// MetricRecord is a persisted metrics table row: 003's MetricSample plus the
// run it was captured on. Returned by services.HistoryRepository's
// LatestMetrics and ListMetrics, analogous to how Check is a persisted
// checks row.
type MetricRecord struct {
	RunID  int64
	TS     time.Time
	Sample MetricSample
}
