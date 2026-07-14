package httpapi

import (
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/gateway/dto"
)

// checkDTO maps one domain.Check onto its wire shape (run_id hoisted to the
// enclosing RunDetailResponse, so it is not repeated per check).
func checkDTO(c domain.Check) dto.CheckDTO {
	return dto.CheckDTO{
		TS:        formatTime(c.TS),
		Phase:     c.Phase,
		Target:    c.Target,
		Kind:      c.Kind,
		OK:        c.OK,
		LatencyMS: c.LatencyMS,
		Error:     c.Error,
	}
}

// checkDTOs maps a slice of domain.Check to their wire DTOs, always
// returning a non-nil (possibly empty) slice: a nil slice would encode as
// JSON null, but a run's "checks" field must marshal as [] when there are
// none (API surface: an empty list is a valid answer).
func checkDTOs(checks []domain.Check) []dto.CheckDTO {
	out := make([]dto.CheckDTO, 0, len(checks))
	for _, c := range checks {
		out = append(out, checkDTO(c))
	}
	return out
}

// formatNullTime renders a nullable *time.Time as a *string, keeping nil as
// nil (JSON null) instead of formatting a zero time.
func formatNullTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}

// formatTime renders t as UTC RFC3339, matching the store's own wire format.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// hostDTO maps one domain.HostRecord onto its wire shape.
func hostDTO(rec domain.HostRecord) dto.HostResponse {
	return dto.HostResponse{
		RunID:    rec.RunID,
		TS:       formatTime(rec.TS),
		Hostname: rec.Host.Hostname,
		OS:       rec.Host.OS,
		Arch:     rec.Host.Arch,
	}
}

// hostInfoDTO maps a domain.HostInfo onto the identity block embedded in
// MetricsLatestResponse (a zero HostInfo maps to an all-empty block).
func hostInfoDTO(h domain.HostInfo) dto.HostInfoDTO {
	return dto.HostInfoDTO{
		Hostname: h.Hostname,
		OS:       h.OS,
		Arch:     h.Arch,
	}
}

// metricBucketDTO maps one domain.MetricBucket onto its wire shape.
func metricBucketDTO(b domain.MetricBucket) dto.MetricBucketDTO {
	return dto.MetricBucketDTO{
		TS:    formatTime(b.TS),
		Min:   b.Min,
		Max:   b.Max,
		Avg:   b.Avg,
		Count: b.Count,
	}
}

// metricBucketDTOs maps a slice of domain.MetricBucket to their wire DTOs,
// always returning a non-nil (possibly empty) slice (see checkDTOs).
func metricBucketDTOs(buckets []domain.MetricBucket) []dto.MetricBucketDTO {
	out := make([]dto.MetricBucketDTO, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, metricBucketDTO(b))
	}
	return out
}

// metricDTO maps one domain.MetricRecord onto its wire shape. Value is nil
// (JSON null) exactly when the sample is unavailable (OK false), never a
// measured zero.
func metricDTO(rec domain.MetricRecord) dto.MetricDTO {
	m := dto.MetricDTO{
		RunID:     rec.RunID,
		TS:        formatTime(rec.TS),
		Collector: string(rec.Sample.Collector),
		Name:      rec.Sample.Name,
		Unit:      rec.Sample.Unit,
		OK:        rec.Sample.OK,
		Error:     rec.Sample.Error,
	}
	if rec.Sample.OK {
		v := rec.Sample.Value
		m.Value = &v
	}
	return m
}

// metricDTOs maps a slice of domain.MetricRecord to their wire DTOs, always
// returning a non-nil (possibly empty) slice (see checkDTOs). Null-cell
// filtering for the list happens in SQL (ListMetrics), so this maps every row
// it is given.
func metricDTOs(records []domain.MetricRecord) []dto.MetricDTO {
	out := make([]dto.MetricDTO, 0, len(records))
	for _, r := range records {
		out = append(out, metricDTO(r))
	}
	return out
}

// metricRowDTO maps one domain.MetricRecord onto the slim single-run row, with
// run_id/ts hoisted to the enclosing MetricsLatestResponse.
func metricRowDTO(rec domain.MetricRecord) dto.MetricRowDTO {
	m := dto.MetricRowDTO{
		Collector: string(rec.Sample.Collector),
		Name:      rec.Sample.Name,
		Unit:      rec.Sample.Unit,
		OK:        rec.Sample.OK,
		Error:     rec.Sample.Error,
	}
	if rec.Sample.OK {
		v := rec.Sample.Value
		m.Value = &v
	}
	return m
}

// metricRowDTOs maps records to slim rows, dropping unavailable samples (OK
// false) unless includeEmpty — /metrics/latest hides null cells by default,
// ?include_empty=true keeps them. The filter is here (not in the query) so the
// response's hoisted run_id/ts/host survive even when every cell is null.
// Always returns a non-nil (possibly empty) slice (see checkDTOs).
func metricRowDTOs(records []domain.MetricRecord, includeEmpty bool) []dto.MetricRowDTO {
	out := make([]dto.MetricRowDTO, 0, len(records))
	for _, r := range records {
		if !includeEmpty && !r.Sample.OK {
			continue
		}
		out = append(out, metricRowDTO(r))
	}
	return out
}

// metricSeriesDTO maps one domain.MetricSeries onto its wire shape.
func metricSeriesDTO(s domain.MetricSeries) dto.MetricSeriesDTO {
	return dto.MetricSeriesDTO{
		Collector: string(s.Collector),
		Name:      s.Name,
		Unit:      s.Unit,
		Buckets:   metricBucketDTOs(s.Buckets),
	}
}

// metricSeriesDTOs maps a slice of domain.MetricSeries to their wire DTOs,
// always returning a non-nil (possibly empty) slice (see checkDTOs).
func metricSeriesDTOs(series []domain.MetricSeries) []dto.MetricSeriesDTO {
	out := make([]dto.MetricSeriesDTO, 0, len(series))
	for _, s := range series {
		out = append(out, metricSeriesDTO(s))
	}
	return out
}

// overviewResponse maps a domain.Overview onto its wire shape (plans/010):
// Window is resolved server-side (services.QueryService.Overview's own
// defaults/clamps), never an echo of the raw request params.
func overviewResponse(o domain.Overview) dto.OverviewResponse {
	return dto.OverviewResponse{
		Window: dto.OverviewWindowDTO{
			Since:  formatTime(o.Since),
			Until:  formatTime(o.Until),
			Bucket: o.Bucket.String(),
		},
		Metrics: metricSeriesDTOs(o.Metrics),
		Pings:   pingSeriesDTOs(o.Pings),
	}
}

// pingBucketDTO maps one domain.PingBucket onto its wire shape. LossPct
// comes from domain.LossPercent rather than a field stored on the domain
// type, since it is a presentation detail derived from Sent/Received, not a
// value the domain layer itself needs to hold. AvgMS/MaxMS are nil (JSON
// null) exactly when Received is 0 for the whole bucket — no reply ever came
// back, so there is no round trip to report — mirroring pingDTO's AvgMS
// handling.
func pingBucketDTO(b domain.PingBucket) dto.PingBucketDTO {
	d := dto.PingBucketDTO{
		TS:       formatTime(b.TS),
		Sent:     b.Sent,
		Received: b.Received,
		LossPct:  domain.LossPercent(b.Sent, b.Received),
		Samples:  b.Samples,
	}
	if b.Received > 0 {
		avg, max := b.AvgMS, b.MaxMS
		d.AvgMS = &avg
		d.MaxMS = &max
	}
	return d
}

// pingBucketDTOs maps a slice of domain.PingBucket to their wire DTOs,
// always returning a non-nil (possibly empty) slice (see checkDTOs).
func pingBucketDTOs(buckets []domain.PingBucket) []dto.PingBucketDTO {
	out := make([]dto.PingBucketDTO, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, pingBucketDTO(b))
	}
	return out
}

// pingDTO maps one domain.PingRecord onto its wire shape (issue #2). AvgMS
// is nil (JSON null) exactly when the result is unreachable (OK false),
// mirroring metricDTO's Value handling.
func pingDTO(rec domain.PingRecord) dto.PingDTO {
	p := dto.PingDTO{
		RunID:    rec.RunID,
		TS:       formatTime(rec.TS),
		Host:     rec.Result.Host,
		Sent:     rec.Result.Sent,
		Received: rec.Result.Received,
		OK:       rec.Result.OK,
		Error:    rec.Result.Error,
	}
	if rec.Result.OK {
		v := rec.Result.AvgMS
		p.AvgMS = &v
	}
	return p
}

// pingDTOs maps a slice of domain.PingRecord to their wire DTOs, always
// returning a non-nil (possibly empty) slice (see checkDTOs). Unreachable-row
// filtering for the list happens in SQL (ListPings), so this maps every row
// it is given.
func pingDTOs(records []domain.PingRecord) []dto.PingDTO {
	out := make([]dto.PingDTO, 0, len(records))
	for _, r := range records {
		out = append(out, pingDTO(r))
	}
	return out
}

// pingOutageDTO maps one domain.PingOutage onto its wire shape. Kind is
// "unreachable" when the episode saw at least one 100%-loss sample, "loss"
// when every sample in it kept at least a partial reply.
func pingOutageDTO(o domain.PingOutage) dto.PingOutageDTO {
	kind := "loss"
	if o.Unreachable {
		kind = "unreachable"
	}
	return dto.PingOutageDTO{
		Start:        formatTime(o.Start),
		End:          formatTime(o.End),
		Kind:         kind,
		WorstLossPct: o.WorstLossPct,
	}
}

// pingOutageDTOs maps a slice of domain.PingOutage to their wire DTOs,
// always returning a non-nil (possibly empty) slice (see checkDTOs).
func pingOutageDTOs(outages []domain.PingOutage) []dto.PingOutageDTO {
	out := make([]dto.PingOutageDTO, 0, len(outages))
	for _, o := range outages {
		out = append(out, pingOutageDTO(o))
	}
	return out
}

// pingSeriesDTO maps one domain.PingSeries onto its wire shape.
func pingSeriesDTO(s domain.PingSeries) dto.PingSeriesDTO {
	return dto.PingSeriesDTO{
		Host:    s.Host,
		Buckets: pingBucketDTOs(s.Buckets),
		Outages: pingOutageDTOs(s.Outages),
	}
}

// pingSeriesDTOs maps a slice of domain.PingSeries to their wire DTOs,
// always returning a non-nil (possibly empty) slice (see checkDTOs).
func pingSeriesDTOs(series []domain.PingSeries) []dto.PingSeriesDTO {
	out := make([]dto.PingSeriesDTO, 0, len(series))
	for _, s := range series {
		out = append(out, pingSeriesDTO(s))
	}
	return out
}

// runDTO maps one domain.Run onto its wire shape: every nullable *At field
// becomes a *string (RFC3339), nil staying nil.
func runDTO(r domain.Run) dto.RunDTO {
	return dto.RunDTO{
		ID:                 r.ID,
		StartedAt:          formatTime(r.StartedAt),
		Mode:               r.Mode,
		InternetOK:         r.InternetOK,
		Action:             r.Action,
		RebootStartedAt:    formatNullTime(r.RebootStartedAt),
		RouterDownAt:       formatNullTime(r.RouterDownAt),
		RouterUpAt:         formatNullTime(r.RouterUpAt),
		InternetRestoredAt: formatNullTime(r.InternetRestoredAt),
		FinishedAt:         formatNullTime(r.FinishedAt),
		Outcome:            r.Outcome,
		Error:              r.Error,
	}
}

// runDTOs maps a slice of domain.Run to their wire DTOs, always returning a
// non-nil (possibly empty) slice (see checkDTOs).
func runDTOs(runs []domain.Run) []dto.RunDTO {
	out := make([]dto.RunDTO, 0, len(runs))
	for _, r := range runs {
		out = append(out, runDTO(r))
	}
	return out
}
