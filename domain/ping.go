package domain

import "time"

// PingResult is one ICMP ping probe against a host (issue #2): like
// MetricSample, it is strictly best-effort — a DNS failure, unreachable
// host, or missing ping binary never fails a run, it only sets OK false.
// OK is always Received>0 (some replies came back); AvgMS is meaningful
// only when OK is true — a 100%-loss probe (Received==0) leaves AvgMS at
// its zero value with no error, since a full timeout is still a
// successfully parsed summary, not a probe failure.
type PingResult struct {
	Host     string
	Sent     int
	Received int
	AvgMS    float64
	OK       bool
	Error    string
}

// PingRecord is a persisted pings table row: PingResult plus the run it was
// captured on, analogous to how MetricRecord wraps MetricSample. RunID is
// the owning run's UUIDv7 string id (issue #4).
type PingRecord struct {
	RunID  string
	TS     time.Time
	Result PingResult
}
