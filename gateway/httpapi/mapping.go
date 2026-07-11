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
