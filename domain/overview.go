package domain

import (
	"math"
	"time"
)

// MetricBucket is one time-bucketed aggregate of a metric series: Min, Max,
// and Avg are computed only over the samples that landed in this bucket, and
// Count is how many contributed (always >0 — an empty bucket is simply
// absent from MetricSeries.Buckets).
type MetricBucket struct {
	TS    time.Time
	Min   float64
	Max   float64
	Avg   float64
	Count int
}

// MetricSeries is one (Collector, Name) metric's bucketed history across the
// requested window, e.g. the cpu collector's "load1" reading.
type MetricSeries struct {
	Collector Collector
	Name      string
	Unit      string
	Buckets   []MetricBucket
}

// PingBucket is one time-bucketed aggregate of ping reachability against a
// single host: Sent/Received sum every sample in the bucket (including fully
// unreachable ones, so a loss percentage can be derived from them), while
// AvgMS/MaxMS are meaningful only when Received>0 — a bucket where every
// sample was 100% loss has no round trip to average.
type PingBucket struct {
	TS       time.Time
	Sent     int
	Received int
	AvgMS    float64
	MaxMS    float64
	Samples  int
}

// PingOutage is one episode of degraded reachability against a host: a
// maximal run of consecutive samples (in id order) that each lost at least
// one reply. WorstLossPct is the highest loss percentage seen at any single
// sample within the episode; Unreachable is true when at least one sample in
// the run had zero replies (a full outage), as opposed to sustained partial
// loss.
type PingOutage struct {
	Start        time.Time
	End          time.Time
	WorstLossPct int
	Unreachable  bool
}

// PingSeries is one host's bucketed reachability history across the
// requested window, plus every outage episode detected within it.
type PingSeries struct {
	Host    string
	Buckets []PingBucket
	Outages []PingOutage
}

// Overview is the server-downsampled multi-day view GET /api/v1/overview
// serves: every enabled metric collector and every configured ping host,
// bucketed across [Since, Until] at Bucket-wide intervals.
type Overview struct {
	Since   time.Time
	Until   time.Time
	Bucket  time.Duration
	Metrics []MetricSeries
	Pings   []PingSeries
}

// LossPercent rounds a ping sample's or bucket's packet loss to the nearest
// whole percent, given how many probes were sent and how many replies came
// back. sent<=0 reports 0 rather than dividing by zero, protecting a caller
// that passes in a degenerate (zero-sent) value directly rather than one
// already produced by a real ping round.
func LossPercent(sent, received int) int {
	if sent <= 0 {
		return 0
	}
	return int(math.Round((1 - float64(received)/float64(sent)) * 100))
}
