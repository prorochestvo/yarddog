package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

var (
	_ Checker           = (*fakeChecker)(nil)
	_ Rebooter          = (*fakeRebooter)(nil)
	_ Notifier          = (*fakeNotifier)(nil)
	_ Clock             = (*fakeClock)(nil)
	_ RunRepository     = (*fakeRunRepo)(nil)
	_ MetricsCollector  = (*fakeMetricsCollector)(nil)
	_ MetricsCollector  = blockingMetricsCollector{}
	_ MetricsRepository = (*fakeMetricsRepo)(nil)
	_ OutboxRepository  = (*fakeOutboxRepo)(nil)
	_ Sender            = (*fakeSender)(nil)
)

// fakeChecker returns checkResults and gatewayResults in call order — index 0
// of checkResults is the soft flow's initial check (skipped entirely in hard
// mode, so there the first entry lands on the first recovery tick instead);
// every later index is one recovery-loop tick. Once a slice is exhausted, its
// last entry repeats so a test only needs to script the ticks it cares about.
type fakeChecker struct {
	checkResults   []domain.Result
	gatewayResults []domain.TargetResult
	checkCalls     int
	gatewayCalls   int
}

func (f *fakeChecker) Check(context.Context) domain.Result {
	r := f.checkResults[min(f.checkCalls, len(f.checkResults)-1)]
	f.checkCalls++
	return r
}

func (f *fakeChecker) Gateway(context.Context) domain.TargetResult {
	r := f.gatewayResults[min(f.gatewayCalls, len(f.gatewayResults)-1)]
	f.gatewayCalls++
	return r
}

// fakeRebooter reports err (nil by default) from Reboot and counts calls, so
// tests can assert whether a reboot was attempted at all.
type fakeRebooter struct {
	err   error
	calls int
}

func (f *fakeRebooter) Reboot(context.Context) error {
	f.calls++
	return f.err
}

// fakeNotifier records every message in send order and counts Flush calls,
// so orchestrator tests can assert exact message ordering (the graded
// correctness property for the recovery loop) without a real OutboxService.
type fakeNotifier struct {
	messages   []string
	flushCalls int
}

func (f *fakeNotifier) Notify(_ context.Context, text string) error {
	f.messages = append(f.messages, text)
	return nil
}

func (f *fakeNotifier) Flush(context.Context) error {
	f.flushCalls++
	return nil
}

// fakeClock advances its virtual time by d on every After call instead of
// actually waiting, so a 15-minute recovery timeout runs instantly; the
// returned channel is pre-filled so a receive on it never blocks.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// newFakeRunRepo returns an empty fakeRunRepo ready to accept its first
// InsertRun.
func newFakeRunRepo() *fakeRunRepo {
	return &fakeRunRepo{runs: make(map[int64]*domain.Run)}
}

// fakeRunRepo is a dumb, settable in-memory stand-in for RunRepository
// (Risk R2): GetLastRebootStartedAt never derives its answer from inserted
// rows — a test sets lastRebootStartedAt/lastRebootOK/lastRebootErr directly
// — and UpdateRun only ever applies non-nil RunUpdate fields onto the
// in-memory row, never re-implementing any store SQL. The real cooldown
// query is exercised for real against SQLite in
// infrastructure/store_test.go.
type fakeRunRepo struct {
	runs   map[int64]*domain.Run
	nextID int64

	lastRebootStartedAt time.Time
	lastRebootOK        bool
	lastRebootErr       error
}

func (f *fakeRunRepo) GetLastRebootStartedAt(context.Context) (time.Time, bool, error) {
	if f.lastRebootErr != nil {
		return time.Time{}, false, f.lastRebootErr
	}
	return f.lastRebootStartedAt, f.lastRebootOK, nil
}

func (f *fakeRunRepo) InsertCheck(_ context.Context, _ domain.Check) error {
	return nil
}

func (f *fakeRunRepo) InsertRun(_ context.Context, startedAt time.Time, mode string, internetOK *bool) (int64, error) {
	f.nextID++
	f.runs[f.nextID] = &domain.Run{ID: f.nextID, StartedAt: startedAt, Mode: mode, InternetOK: internetOK, Action: domain.ActionNone}
	return f.nextID, nil
}

// run returns a copy of the in-memory row id for readback assertions,
// failing the test if it was never inserted.
func (f *fakeRunRepo) run(t *testing.T, id int64) domain.Run {
	t.Helper()

	run, ok := f.runs[id]
	if !ok {
		t.Fatalf("fakeRunRepo: no run %d", id)
	}
	return *run
}

func (f *fakeRunRepo) UpdateRun(_ context.Context, id int64, u domain.RunUpdate) error {
	run, ok := f.runs[id]
	if !ok {
		return fmt.Errorf("fakeRunRepo: no run %d", id)
	}

	if u.Action != nil {
		run.Action = *u.Action
	}
	if u.RebootStartedAt != nil {
		run.RebootStartedAt = u.RebootStartedAt
	}
	if u.RouterDownAt != nil {
		run.RouterDownAt = u.RouterDownAt
	}
	if u.RouterUpAt != nil {
		run.RouterUpAt = u.RouterUpAt
	}
	if u.InternetRestoredAt != nil {
		run.InternetRestoredAt = u.InternetRestoredAt
	}
	if u.FinishedAt != nil {
		run.FinishedAt = u.FinishedAt
	}
	if u.Outcome != nil {
		run.Outcome = *u.Outcome
	}
	if u.Error != nil {
		run.Error = *u.Error
	}
	return nil
}

// fakeMetricsCollector returns snapshot from every Collect call and counts
// them, so orchestrator tests can assert the collector ran (or didn't) and
// with what payload, without any real sysfs/procfs/exec access.
type fakeMetricsCollector struct {
	snapshot domain.HostMetrics
	calls    int
}

func (f *fakeMetricsCollector) Collect(context.Context) domain.HostMetrics {
	f.calls++
	return f.snapshot
}

// blockingMetricsCollector simulates a wedged sensor read (e.g. a hung
// hwmon/I2C file the pi5's rp1_adc can produce): Collect blocks until ctx is
// cancelled, exactly like a real blocking syscall yarddog's runtime cannot
// interrupt mid-read. It proves collectMetrics's own goroutine+timeout is
// the guarantee against a stall, not cooperation from the collector.
type blockingMetricsCollector struct{}

func (blockingMetricsCollector) Collect(ctx context.Context) domain.HostMetrics {
	<-ctx.Done()
	return domain.HostMetrics{}
}

// fakeMetricsRepo records every SaveMetrics call in order, so orchestrator
// tests can assert exactly one snapshot was saved per run with the expected
// runID/ts/payload; err lets a test force a save failure to verify it is
// only logged, never surfaced as a changed outcome.
type fakeMetricsRepo struct {
	err error

	calls []metricsSave
}

// metricsSave is one recorded fakeMetricsRepo.SaveMetrics call.
type metricsSave struct {
	runID int64
	ts    time.Time
	m     domain.HostMetrics
}

func (f *fakeMetricsRepo) SaveMetrics(_ context.Context, runID int64, ts time.Time, m domain.HostMetrics) error {
	f.calls = append(f.calls, metricsSave{runID: runID, ts: ts, m: m})
	return f.err
}

// newFakeOutboxRepo returns an empty fakeOutboxRepo ready to accept its
// first EnqueueOutboxMessage.
func newFakeOutboxRepo() *fakeOutboxRepo {
	return &fakeOutboxRepo{
		messages: make(map[int64]*domain.OutboxMessage),
		sent:     make(map[int64]bool),
	}
}

// fakeOutboxRepo is a dumb, settable in-memory stand-in for
// OutboxRepository: markSentErr lets a test force MarkOutboxSent to fail for
// one specific id, replacing the old CREATE TRIGGER white-box (Risk R2). The
// real queue SQL is exercised for real against SQLite in
// infrastructure/store_test.go.
type fakeOutboxRepo struct {
	messages map[int64]*domain.OutboxMessage
	order    []int64
	sent     map[int64]bool
	nextID   int64

	markSentErr map[int64]error
}

func (f *fakeOutboxRepo) EnqueueOutboxMessage(_ context.Context, createdAt time.Time, text string) (int64, error) {
	f.nextID++
	id := f.nextID
	f.messages[id] = &domain.OutboxMessage{ID: id, CreatedAt: createdAt, Text: text}
	f.order = append(f.order, id)
	return id, nil
}

func (f *fakeOutboxRepo) IncrementOutboxAttempt(_ context.Context, id int64, sendErr string) error {
	m, ok := f.messages[id]
	if !ok {
		return fmt.Errorf("fakeOutboxRepo: no message %d", id)
	}
	m.Attempts++
	m.LastError = sendErr
	return nil
}

func (f *fakeOutboxRepo) ListUnsentOutboxMessages(context.Context) ([]domain.OutboxMessage, error) {
	var out []domain.OutboxMessage
	for _, id := range f.order {
		if f.sent[id] {
			continue
		}
		out = append(out, *f.messages[id])
	}
	return out, nil
}

func (f *fakeOutboxRepo) MarkOutboxSent(_ context.Context, id int64, _ time.Time) error {
	if err := f.markSentErr[id]; err != nil {
		return err
	}
	f.sent[id] = true
	return nil
}

// fakeSender records every message it was asked to send and returns err
// (nil by default), standing in for gateway/telegram.Client's Send without
// any real HTTP call.
type fakeSender struct {
	err  error
	sent []string
}

func (f *fakeSender) Send(_ context.Context, text string) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, text)
	return nil
}
