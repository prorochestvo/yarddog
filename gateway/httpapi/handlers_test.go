package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/gateway/dto"
)

func TestHandlePing(t *testing.T) {
	t.Parallel()

	srv := newTestServer(&fakeRepo{}, "tok")

	rec := doRequest(t, srv, http.MethodGet, "/ping", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body dto.PingResponse
	decodeJSON(t, rec, &body)
	if body.Status != "ok" {
		t.Fatalf("Status = %q, want %q", body.Status, "ok")
	}
}

func TestParseIntParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		def     int
		want    int
		wantErr bool
	}{
		{"absent uses the default", "", 42, 42, false},
		{"a valid value parses", "limit=7", 42, 7, false},
		{"a negative value parses (clamping is QueryService's job)", "limit=-5", 42, -5, false},
		{"an unparseable value errors", "limit=abc", 42, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tt.query, err)
			}

			got, err := parseIntParam(q, "limit", tt.def)
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseIntParam() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseIntParam() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("parseIntParam() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseBoolParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		want    bool
		wantErr bool
	}{
		{"absent is false", "", false, false},
		{"true parses", "include_empty=true", true, false},
		{"1 parses as true", "include_empty=1", true, false},
		{"false parses", "include_empty=false", false, false},
		{"0 parses as false", "include_empty=0", false, false},
		{"an unparseable value errors", "include_empty=maybe", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tt.query, err)
			}

			got, err := parseBoolParam(q, "include_empty")
			if tt.wantErr {
				if err == nil {
					t.Fatal("parseBoolParam() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBoolParam() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("parseBoolParam() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMetricsFilter(t *testing.T) {
	t.Run("every param set parses correctly", func(t *testing.T) {
		t.Parallel()

		q, err := url.ParseQuery("since=2026-07-01T00:00:00Z&collector=cpu&limit=5&include_empty=true")
		if err != nil {
			t.Fatalf("ParseQuery: %v", err)
		}

		f, err := parseMetricsFilter(q)
		if err != nil {
			t.Fatalf("parseMetricsFilter() error = %v", err)
		}
		wantSince := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		if !f.Since.Equal(wantSince) {
			t.Errorf("Since = %v, want %v", f.Since, wantSince)
		}
		if f.Collector != domain.CollectorCPU {
			t.Errorf("Collector = %q, want %q", f.Collector, domain.CollectorCPU)
		}
		if f.Limit != 5 {
			t.Errorf("Limit = %d, want 5", f.Limit)
		}
		if !f.IncludeEmpty {
			t.Errorf("IncludeEmpty = false, want true")
		}
	})

	t.Run("every param absent is the zero filter", func(t *testing.T) {
		t.Parallel()

		f, err := parseMetricsFilter(url.Values{})
		if err != nil {
			t.Fatalf("parseMetricsFilter() error = %v", err)
		}
		if !f.Since.IsZero() || f.Collector != "" || f.Limit != 0 || f.IncludeEmpty {
			t.Fatalf("parseMetricsFilter(empty) = %+v, want the zero value", f)
		}
	})

	t.Run("an unparseable include_empty errors", func(t *testing.T) {
		t.Parallel()

		q, _ := url.ParseQuery("include_empty=maybe")
		if _, err := parseMetricsFilter(q); err == nil {
			t.Fatal("parseMetricsFilter() error = nil, want error for an invalid include_empty")
		}
	})

	t.Run("an unparseable since errors", func(t *testing.T) {
		t.Parallel()

		q, _ := url.ParseQuery("since=not-a-date")
		if _, err := parseMetricsFilter(q); err == nil {
			t.Fatal("parseMetricsFilter() error = nil, want error for an invalid since")
		}
	})

	t.Run("an unrecognized collector errors", func(t *testing.T) {
		t.Parallel()

		q, _ := url.ParseQuery("collector=bogus")
		if _, err := parseMetricsFilter(q); err == nil {
			t.Fatal("parseMetricsFilter() error = nil, want error for an unrecognized collector")
		}
	})

	t.Run("an unparseable limit errors", func(t *testing.T) {
		t.Parallel()

		q, _ := url.ParseQuery("limit=abc")
		if _, err := parseMetricsFilter(q); err == nil {
			t.Fatal("parseMetricsFilter() error = nil, want error for an invalid limit")
		}
	})
}

func TestParsePingsFilter(t *testing.T) {
	t.Run("every param set parses correctly", func(t *testing.T) {
		t.Parallel()

		q, err := url.ParseQuery("since=2026-07-01T00:00:00Z&host=1.1.1.1&limit=5&include_unreachable=true")
		if err != nil {
			t.Fatalf("ParseQuery: %v", err)
		}

		f, err := parsePingsFilter(q)
		if err != nil {
			t.Fatalf("parsePingsFilter() error = %v", err)
		}
		wantSince := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		if !f.Since.Equal(wantSince) {
			t.Errorf("Since = %v, want %v", f.Since, wantSince)
		}
		if f.Host != "1.1.1.1" {
			t.Errorf("Host = %q, want %q", f.Host, "1.1.1.1")
		}
		if f.Limit != 5 {
			t.Errorf("Limit = %d, want 5", f.Limit)
		}
		if !f.IncludeUnreachable {
			t.Errorf("IncludeUnreachable = false, want true")
		}
	})

	t.Run("every param absent is the zero filter", func(t *testing.T) {
		t.Parallel()

		f, err := parsePingsFilter(url.Values{})
		if err != nil {
			t.Fatalf("parsePingsFilter() error = %v", err)
		}
		if !f.Since.IsZero() || f.Host != "" || f.Limit != 0 || f.IncludeUnreachable {
			t.Fatalf("parsePingsFilter(empty) = %+v, want the zero value", f)
		}
	})

	t.Run("an unparseable include_unreachable errors", func(t *testing.T) {
		t.Parallel()

		q, _ := url.ParseQuery("include_unreachable=maybe")
		if _, err := parsePingsFilter(q); err == nil {
			t.Fatal("parsePingsFilter() error = nil, want error for an invalid include_unreachable")
		}
	})

	t.Run("an unparseable since errors", func(t *testing.T) {
		t.Parallel()

		q, _ := url.ParseQuery("since=not-a-date")
		if _, err := parsePingsFilter(q); err == nil {
			t.Fatal("parsePingsFilter() error = nil, want error for an invalid since")
		}
	})

	t.Run("an unparseable limit errors", func(t *testing.T) {
		t.Parallel()

		q, _ := url.ParseQuery("limit=abc")
		if _, err := parsePingsFilter(q); err == nil {
			t.Fatal("parsePingsFilter() error = nil, want error for an invalid limit")
		}
	})
}

func TestServer_handlePings(t *testing.T) {
	t.Run("empty result is 200 with an empty array, not 404", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !strings.Contains(rec.Body.String(), `"pings":[]`) {
			t.Fatalf("body = %s, want an empty array, not null", rec.Body.String())
		}
	})

	t.Run("returns newest-first pings", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		repo := &fakeRepo{listPings: []domain.PingRecord{
			{RunID: 2, TS: ts, Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 10, OK: true}},
			{RunID: 1, TS: ts.Add(-time.Minute), Result: domain.PingResult{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 20, OK: true}},
		}}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body dto.PingsListResponse
		decodeJSON(t, rec, &body)
		if len(body.Pings) != 2 || body.Pings[0].RunID != 2 || body.Pings[1].RunID != 1 {
			t.Fatalf("body.Pings = %+v, want run 2 before run 1 (newest first)", body.Pings)
		}
	})

	t.Run("?host= is parsed and forwarded", func(t *testing.T) {
		t.Parallel()

		repo := &fakeRepo{listPings: []domain.PingRecord{{RunID: 1, Result: domain.PingResult{Host: "8.8.8.8", OK: true, Received: 1}}}}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings?host=8.8.8.8", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var body dto.PingsListResponse
		decodeJSON(t, rec, &body)
		if len(body.Pings) != 1 || body.Pings[0].Host != "8.8.8.8" {
			t.Fatalf("body.Pings = %+v, want the one 8.8.8.8 row", body.Pings)
		}
	})

	t.Run("?include_unreachable=true includes null-avg rows", func(t *testing.T) {
		t.Parallel()

		repo := &fakeRepo{listPings: []domain.PingRecord{
			{RunID: 1, Result: domain.PingResult{Host: "unreachable.example", Sent: 5, Received: 0, OK: false, Error: "no route to host"}},
		}}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings?include_unreachable=true", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body dto.PingsListResponse
		decodeJSON(t, rec, &body)
		if len(body.Pings) != 1 || body.Pings[0].AvgMS != nil {
			t.Fatalf("body.Pings = %+v, want one row with a null avg_ms", body.Pings)
		}
	})

	t.Run("?limit= is parsed and forwarded", func(t *testing.T) {
		t.Parallel()

		repo := &fakeRepo{listPings: []domain.PingRecord{{RunID: 1, Result: domain.PingResult{Host: "1.1.1.1", OK: true, Received: 1}}}}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings?limit=1", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("requires the token like every other gated route", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("bad since is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings?since=not-a-date", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("bad limit is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings?limit=not-a-number", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("bad include_unreachable is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings?include_unreachable=maybe", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("a repo error is 500 with a generic body", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{listPingsErr: errors.New("boom")}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/pings", "tok")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

func TestServer_handleHealth(t *testing.T) {
	t.Run("all probes healthy is 200 status:true", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok", &fakeHealthProbe{name: "sqlite"})

		rec := doRequest(t, srv, http.MethodGet, "/health/check", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body dto.HealthResponse
		decodeJSON(t, rec, &body)
		if !body.Status {
			t.Fatal("Status = false, want true")
		}
		if body.Services["sqlite"] != "ok" {
			t.Fatalf(`Services["sqlite"] = %q, want "ok"`, body.Services["sqlite"])
		}
		if body.Server.Version != "v0.0.0-test" {
			t.Fatalf("Server.Version = %q, want %q", body.Server.Version, "v0.0.0-test")
		}
		if body.Server.Uptime == "" {
			t.Fatal("Server.Uptime is empty, want a rendered duration")
		}
	})

	t.Run("a failing probe is 503 status:false with its error", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok", &fakeHealthProbe{name: "sqlite", err: errors.New("ping failed")})

		rec := doRequest(t, srv, http.MethodGet, "/health/check", "tok")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}

		var body dto.HealthResponse
		decodeJSON(t, rec, &body)
		if body.Status {
			t.Fatal("Status = true, want false")
		}
		if body.Services["sqlite"] != "ping failed" {
			t.Fatalf(`Services["sqlite"] = %q, want %q`, body.Services["sqlite"], "ping failed")
		}
	})

	t.Run("requires the token like every other gated route", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/health/check", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})
}

func TestServer_handleLatestHost(t *testing.T) {
	t.Run("found returns the host DTO", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		repo := &fakeRepo{
			latestHostOK: true,
			latestHost:   domain.HostRecord{RunID: 128, TS: ts, Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"}},
		}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/host", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body dto.HostResponse
		decodeJSON(t, rec, &body)
		want := dto.HostResponse{RunID: 128, TS: "2026-07-07T04:07:00Z", Hostname: "pi5", OS: "linux", Arch: "arm64"}
		if body != want {
			t.Fatalf("body = %+v, want %+v", body, want)
		}
	})

	t.Run("no data yet is 404", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{latestHostOK: false}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/host", "tok")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}

		var body dto.ErrorResponse
		decodeJSON(t, rec, &body)
		if body.Error != "no data" {
			t.Fatalf("Error = %q, want %q", body.Error, "no data")
		}
	})

	t.Run("a repo error is 500 with a generic body, not the driver error", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{latestHostErr: errors.New("sql: no such table: host")}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/host", "tok")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}

		var body dto.ErrorResponse
		decodeJSON(t, rec, &body)
		if body.Error != "internal error" {
			t.Fatalf("Error = %q, want the generic %q (must not leak driver text)", body.Error, "internal error")
		}
	})
}

func TestServer_handleLatestMetrics(t *testing.T) {
	t.Run("found returns every metric of the newest run with its host", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		repo := &fakeRepo{
			latestMetrics: []domain.MetricRecord{
				{RunID: 128, TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorTemperature, Name: "cpu-thermal", Value: 52.35, Unit: "celsius", OK: true}},
			},
			latestHostOK: true,
			latestHost:   domain.HostRecord{RunID: 128, TS: ts, Host: domain.HostInfo{Hostname: "pi5", OS: "Ubuntu 26.04 LTS", Arch: "arm64"}},
		}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics/latest", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body dto.MetricsLatestResponse
		decodeJSON(t, rec, &body)
		if len(body.Metrics) != 1 {
			t.Fatalf("body = %+v, want 1 metric", body)
		}
		if body.RunID != 128 || body.TS != "2026-07-07T04:07:00Z" {
			t.Fatalf("body.RunID/TS = %d/%s, want 128/2026-07-07T04:07:00Z", body.RunID, body.TS)
		}
		if body.Host.Hostname != "pi5" || body.Host.OS != "Ubuntu 26.04 LTS" || body.Host.Arch != "arm64" {
			t.Fatalf("body.Host = %+v, want {pi5 Ubuntu 26.04 LTS arm64}", body.Host)
		}
	})

	t.Run("drops null cells by default, includes them with ?include_empty", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		repo := &fakeRepo{
			latestMetrics: []domain.MetricRecord{
				{RunID: 200, TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorTemperature, Name: "cpu-thermal", Value: 52.35, Unit: "celsius", OK: true}},
				{RunID: 200, TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorFans, Name: "fans", OK: false, Error: "no fan sensors present"}},
			},
			latestHostOK: true,
			latestHost:   domain.HostRecord{RunID: 200, TS: ts, Host: domain.HostInfo{Hostname: "pi5", OS: "Ubuntu 26.04 LTS", Arch: "arm64"}},
		}
		srv := newTestServer(repo, "tok")

		var def dto.MetricsLatestResponse
		decodeJSON(t, doRequest(t, srv, http.MethodGet, "/api/v1/metrics/latest", "tok"), &def)
		if len(def.Metrics) != 1 || def.Metrics[0].Name != "cpu-thermal" {
			t.Fatalf("default metrics = %+v, want only the ok row", def.Metrics)
		}

		var all dto.MetricsLatestResponse
		decodeJSON(t, doRequest(t, srv, http.MethodGet, "/api/v1/metrics/latest?include_empty=true", "tok"), &all)
		if len(all.Metrics) != 2 {
			t.Fatalf("include_empty metrics len = %d, want 2", len(all.Metrics))
		}
	})

	t.Run("a bad include_empty is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics/latest?include_empty=maybe", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("no data yet is 404", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics/latest", "tok")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})

	t.Run("a repo error is 500 with a generic body", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{latestMetricsErr: errors.New("boom")}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics/latest", "tok")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("a host lookup error is 500", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		repo := &fakeRepo{
			latestMetrics: []domain.MetricRecord{
				{RunID: 128, TS: ts, Sample: domain.MetricSample{Collector: domain.CollectorUptime, Name: "uptime", Value: 3600, Unit: "seconds", OK: true}},
			},
			latestHostErr: errors.New("sql: no such table: host"),
		}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics/latest", "tok")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

func TestServer_handleMetrics(t *testing.T) {
	t.Run("empty result is 200 with an empty array, not 404", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !strings.Contains(rec.Body.String(), `"metrics":[]`) {
			t.Fatalf("body = %s, want an empty array, not null", rec.Body.String())
		}
	})

	t.Run("filters are parsed and forwarded to the query service", func(t *testing.T) {
		t.Parallel()

		repo := &fakeRepo{listMetrics: []domain.MetricRecord{{RunID: 1}}}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics?since=2026-07-01T00:00:00Z&collector=cpu&limit=5", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var body dto.MetricsListResponse
		decodeJSON(t, rec, &body)
		if len(body.Metrics) != 1 {
			t.Fatalf("len(Metrics) = %d, want 1", len(body.Metrics))
		}
	})

	t.Run("bad since is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics?since=not-a-date", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("bad collector is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics?collector=bogus", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("bad limit is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics?limit=not-a-number", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("bad include_empty is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics?include_empty=maybe", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("a repo error is 500 with a generic body", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{listMetricsErr: errors.New("boom")}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/metrics", "tok")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

func TestServer_handleRunByID(t *testing.T) {
	t.Run("found returns the run and its checks", func(t *testing.T) {
		t.Parallel()

		startedAt := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		repo := &fakeRepo{
			runByIDOK: true,
			runByID:   domain.Run{ID: 128, StartedAt: startedAt, Mode: domain.ModeSoft, Outcome: domain.OutcomeOK},
			listChecks: []domain.Check{
				{RunID: 128, TS: startedAt, Phase: domain.PhaseInitial, Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true},
			},
		}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs/128", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body dto.RunDetailResponse
		decodeJSON(t, rec, &body)
		if body.Run.ID != 128 {
			t.Fatalf("Run.ID = %d, want 128", body.Run.ID)
		}
		if len(body.Checks) != 1 || body.Checks[0].Target != "1.1.1.1:443" {
			t.Fatalf("Checks = %+v, want the one ip check", body.Checks)
		}
	})

	t.Run("unknown id is 404", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{runByIDOK: false}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs/999", "tok")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}

		var body dto.ErrorResponse
		decodeJSON(t, rec, &body)
		if body.Error != "run not found" {
			t.Fatalf("Error = %q, want %q", body.Error, "run not found")
		}
	})

	t.Run("non-numeric id is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs/not-a-number", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("a repo error is 500 with a generic body", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{runByIDErr: errors.New("boom")}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs/1", "tok")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

func TestServer_handleRuns(t *testing.T) {
	t.Run("empty result is 200 with an empty array, not 404", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if !strings.Contains(rec.Body.String(), `"runs":[]`) {
			t.Fatalf("body = %s, want an empty array, not null", rec.Body.String())
		}
	})

	t.Run("returns the mapped run list", func(t *testing.T) {
		t.Parallel()

		startedAt := time.Date(2026, 7, 7, 4, 7, 0, 0, time.UTC)
		repo := &fakeRepo{listRuns: []domain.Run{{ID: 1, StartedAt: startedAt, Mode: domain.ModeSoft, Action: domain.ActionNone, Outcome: domain.OutcomeOK}}}
		srv := newTestServer(repo, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs?limit=10", "tok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body dto.RunsListResponse
		decodeJSON(t, rec, &body)
		if len(body.Runs) != 1 {
			t.Fatalf("body = %+v, want 1 run", body)
		}
		if body.Runs[0].ID != 1 || body.Runs[0].StartedAt != "2026-07-07T04:07:00Z" {
			t.Fatalf("Runs[0] = %+v, want id=1 started_at=2026-07-07T04:07:00Z", body.Runs[0])
		}
	})

	t.Run("bad limit is 400", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs?limit=abc", "tok")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("a repo error is 500 with a generic body", func(t *testing.T) {
		t.Parallel()

		srv := newTestServer(&fakeRepo{listRunsErr: errors.New("boom")}, "tok")

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/runs", "tok")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

func TestValidCollector(t *testing.T) {
	t.Parallel()

	for _, c := range []domain.Collector{
		domain.CollectorTemperature, domain.CollectorFans, domain.CollectorCPU,
		domain.CollectorMemory, domain.CollectorDisk, domain.CollectorUptime, domain.CollectorNetwork,
	} {
		if !validCollector(c) {
			t.Errorf("validCollector(%q) = false, want true", c)
		}
	}

	if validCollector(domain.Collector("bogus")) {
		t.Error(`validCollector("bogus") = true, want false`)
	}
	if validCollector("") {
		t.Error(`validCollector("") = true, want false`)
	}
}

func TestWriteError(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "bad request")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	var body dto.ErrorResponse
	decodeJSON(t, rec, &body)
	if body.Error != "bad request" {
		t.Fatalf("Error = %q, want %q", body.Error, "bad request")
	}
}

func TestWriteError500(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writeError500(rec, "some context", errors.New("sql: no such table: runs"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var body dto.ErrorResponse
	decodeJSON(t, rec, &body)
	if body.Error != "internal error" {
		t.Fatalf("Error = %q, want the generic %q (must not leak the driver error text)", body.Error, "internal error")
	}
	if strings.Contains(body.Error, "sql") {
		t.Fatalf("Error = %q, leaks internal SQL text", body.Error)
	}
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusTeapot, dto.PingResponse{Status: "ok"})

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", ct, "application/json")
	}

	var body dto.PingResponse
	decodeJSON(t, rec, &body)
	if body.Status != "ok" {
		t.Fatalf("Status = %q, want %q", body.Status, "ok")
	}
}
