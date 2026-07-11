package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

var (
	_ services.HistoryRepository = (*fakeRepo)(nil)
	_ services.HealthProbe       = (*fakeHealthProbe)(nil)
)

// decodeJSON decodes rec's body into v, failing the test on any decode
// error so a malformed response surfaces at the assertion site instead of
// downstream as a confusing zero-value mismatch.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()

	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
}

// doRequest is a test helper issuing one httptest request against srv. An
// empty token omits the header entirely, which is how tests exercise
// /ping's auth exemption and the 401 path.
func doRequest(t *testing.T, srv *Server, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set(authHeader, "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// newTestServer builds a Server over repo and probes for httptest-based
// handler tests: a real services.QueryService and a real services.Inspector
// sit in front of the fakes, so only the repository/probe layer is faked —
// no DB, no network (plans/004-query-daemon.md Task 8).
func newTestServer(repo services.HistoryRepository, token string, probes ...services.HealthProbe) *Server {
	q := services.NewQueryService(repo)
	insp := services.NewInspector(time.Second, probes...)
	return NewServer(q, insp, token, "v0.0.0-test", time.Now())
}

// fakeRepo is a settable, in-memory stand-in for services.HistoryRepository:
// each method returns its canned "*"/"*OK"/"*Err" fields, so handler tests
// can drive every response shape (found/not-found/error) without a real
// store.
type fakeRepo struct {
	latestHost    domain.HostRecord
	latestHostOK  bool
	latestHostErr error

	latestMetrics    []domain.MetricRecord
	latestMetricsErr error

	listMetrics    []domain.MetricRecord
	listMetricsErr error

	listPings    []domain.PingRecord
	listPingsErr error

	listRuns    []domain.Run
	listRunsErr error

	runByID    domain.Run
	runByIDOK  bool
	runByIDErr error

	listChecks    []domain.Check
	listChecksErr error
}

func (f *fakeRepo) LatestHost(context.Context) (domain.HostRecord, bool, error) {
	return f.latestHost, f.latestHostOK, f.latestHostErr
}

func (f *fakeRepo) LatestMetrics(context.Context) ([]domain.MetricRecord, error) {
	return f.latestMetrics, f.latestMetricsErr
}

func (f *fakeRepo) ListChecksByRun(context.Context, int64) ([]domain.Check, error) {
	return f.listChecks, f.listChecksErr
}

func (f *fakeRepo) ListMetrics(context.Context, services.MetricsFilter) ([]domain.MetricRecord, error) {
	return f.listMetrics, f.listMetricsErr
}

func (f *fakeRepo) ListPings(context.Context, services.PingFilter) ([]domain.PingRecord, error) {
	return f.listPings, f.listPingsErr
}

func (f *fakeRepo) ListRuns(context.Context, int) ([]domain.Run, error) {
	return f.listRuns, f.listRunsErr
}

func (f *fakeRepo) RunByID(context.Context, int64) (domain.Run, bool, error) {
	return f.runByID, f.runByIDOK, f.runByIDErr
}

// fakeHealthProbe is a settable services.HealthProbe stand-in: it reports
// err (nil = healthy) under name.
type fakeHealthProbe struct {
	name string
	err  error
}

func (f *fakeHealthProbe) CheckUP(context.Context) error { return f.err }

func (f *fakeHealthProbe) Name() string { return f.name }

// discard is a next-handler stub for auth_test.go: it just answers 200, so
// TestWithAuth can assert purely on whether withAuth let the request through.
var discard = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})
