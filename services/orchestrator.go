package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// Execute runs one soft or hard watchdog pass (mode is domain.ModeSoft or
// domain.ModeHard) and returns the process exit code (design §2). It is the
// single place that maps a terminal state to its (exit code, runs.outcome,
// Telegram message) triple (CLAUDE.md Error Handling) — chk, rb, and nt
// return plain %w-wrapped errors and never decide an outcome themselves. clk
// abstracts both "now" and "wait one RECOVERY_INTERVAL" so the recovery loop
// below never calls time.Sleep directly; main passes infrastructure.SystemClock
// in production and tests inject a fake that advances virtual time instead of
// sleeping for real. metricsRepo and mc are the host-telemetry snapshot's
// persistence and collector (plans/003-host-telemetry.md); telemetry is
// strictly best-effort and never influences the exit code below, and
// collectMetrics bounds mc.Collect by defaultMetricsTimeout so a wedged
// sensor read can never delay the reboot decision either.
func Execute(ctx context.Context, settings Settings, repo RunRepository, metricsRepo MetricsRepository, pingRepo PingRepository, chk Checker, rb Rebooter, nt Notifier, mc MetricsCollector, pc PingCollector, clk Clock, mode string) int {
	r := &runner{
		settings:       settings,
		repo:           repo,
		metricsRepo:    metricsRepo,
		pingRepo:       pingRepo,
		checker:        chk,
		rebooter:       rb,
		notifier:       nt,
		metrics:        mc,
		pings:          pc,
		clock:          clk,
		metricsTimeout: defaultMetricsTimeout,
		pingTimeout:    defaultPingTimeout,
	}
	return r.run(ctx, mode)
}

// runner bundles Execute's dependencies so its flow/state-machine methods
// don't each need to repeat the same parameters.
type runner struct {
	settings       Settings
	repo           RunRepository
	metricsRepo    MetricsRepository
	pingRepo       PingRepository
	checker        Checker
	rebooter       Rebooter
	notifier       Notifier
	metrics        MetricsCollector
	pings          PingCollector
	clock          Clock
	metricsTimeout time.Duration
	pingTimeout    time.Duration
}

// collectMetrics takes and persists one telemetry snapshot for runID, unless
// METRICS_ENABLED is off (plans/003-host-telemetry.md). It runs mc.Collect in
// its own goroutine, bounded by metricsTimeout and guarded by recover(): a
// wedged sensor read (e.g. the pi5's rp1_adc over I2C, or any other blocking
// os.ReadFile/syscall.Statfs a platform collector makes) can leave that
// goroutine parked forever, but collectMetrics itself always returns within
// the timeout so the run still reaches its reboot decision, and a collector
// panic is logged instead of crashing the process. A SaveMetrics failure is
// only logged too: telemetry is strictly best-effort and must never change a
// run's outcome or exit code.
func (r *runner) collectMetrics(ctx context.Context, runID string) {
	if !r.settings.MetricsEnabled {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, r.metricsTimeout)
	defer cancel()

	snapshots := make(chan domain.HostMetrics, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				r.logf("metrics collector panicked for run %s: %v", runID, p)
			}
		}()
		snapshots <- r.metrics.Collect(ctx)
	}()

	select {
	case snapshot := <-snapshots:
		if err := r.metricsRepo.SaveMetrics(ctx, runID, r.clock.Now(), snapshot); err != nil {
			r.logf("save metrics for run %s: %v", runID, err)
		}
	case <-ctx.Done():
		r.logf("collect metrics for run %s: %v (a hung collector must never block the reboot decision)", runID, ctx.Err())
	}
}

// collectPings takes and persists one round of ping probes for runID, unless
// PING_ENABLED (settings.PingEnabled, derived from PING_HOSTS being
// non-empty) is off — mirroring collectMetrics exactly (issue #2). It runs
// pc.Collect in its own goroutine, bounded by pingTimeout and guarded by
// recover(): a wedged ping exec can leave that goroutine parked forever, but
// collectPings itself always returns within the timeout so the run still
// reaches its reboot decision, and a collector panic is logged instead of
// crashing the process. A SavePings failure is only logged too: ping
// telemetry is strictly best-effort and must never change a run's outcome
// or exit code.
func (r *runner) collectPings(ctx context.Context, runID string) {
	if !r.settings.PingEnabled {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, r.pingTimeout)
	defer cancel()

	results := make(chan []domain.PingResult, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				r.logf("ping collector panicked for run %s: %v", runID, p)
			}
		}()
		results <- r.pings.Collect(ctx)
	}()

	select {
	case rs := <-results:
		if err := r.pingRepo.SavePings(ctx, runID, r.clock.Now(), rs); err != nil {
			r.logf("save pings for run %s: %v", runID, err)
		}
	case <-ctx.Done():
		r.logf("collect pings for run %s: %v (a hung ping collector must never block the reboot decision)", runID, ctx.Err())
	}
}

// collectTelemetry takes the run's two best-effort snapshots — host metrics
// and ping RTTs — concurrently. Both are independently bounded and
// recover-guarded and neither can change the run's outcome, so running them in
// parallel keeps the pre-reboot delay at max(metricsTimeout, pingTimeout)
// rather than their sum: on an outage (exactly when a reboot is imminent) that
// latency is added downtime, so it must not stack.
func (r *runner) collectTelemetry(ctx context.Context, runID string) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.collectMetrics(ctx, runID)
	}()
	go func() {
		defer wg.Done()
		r.collectPings(ctx, runID)
	}()
	wg.Wait()
}

// confirmDown re-probes a down verdict CHECK_CONFIRMATIONS more times,
// CHECK_CONFIRM_DELAY apart, and returns the last result (issue #6): a single
// five-second sample must never cost the whole LAN a router reboot. It returns
// early on the first sample that is no longer down, and every attempt is
// persisted under domain.PhaseConfirm so the record shows what the decision
// was actually made on. The waiting goes through clock.After, so tests advance
// virtual time instead of sleeping. When the re-check clears the verdict it
// also corrects internet_ok, which InsertRun wrote from the mistaken first
// sample.
func (r *runner) confirmDown(ctx context.Context, runID string, initial domain.Result) domain.Result {
	result := initial
	for i := 0; i < r.settings.CheckConfirmations; i++ {
		<-r.clock.After(r.settings.CheckConfirmDelay)

		result = r.checker.Check(ctx)
		r.persistChecks(ctx, runID, domain.PhaseConfirm, result.Targets)
		if !result.IsDown() {
			break
		}
	}

	if !result.IsDown() {
		internetOK := true
		if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{InternetOK: &internetOK}); err != nil {
			r.logf("update run %s: %v", runID, err)
		}
		r.logf("run %s: down verdict not confirmed on re-check (state now %s): not rebooting", runID, result.State)
	}

	return result
}

// finishDegraded records a run whose domain (HTTPS) targets all failed while
// raw ip connectivity held (issue #6). The router is never rebooted for this:
// on 2026-07-30 four reboots in seven hours did not stop the same domain
// probes from stalling. The operator is told once per episode — shouldAnnounce
// suppresses the repeat while the state persists — because the fault is real
// (name resolution or the HTTPS path is broken for everything on the LAN) even
// though rebooting is not the answer to it.
func (r *runner) finishDegraded(ctx context.Context, runID string) int {
	if r.shouldAnnounce(ctx, runID, domain.OutcomeDegraded) {
		r.notify(ctx, "domain checks failing while raw connectivity holds (no reboot)")
	}
	if err := r.notifier.Flush(ctx); err != nil {
		r.logf("flush outbox: %v", err)
	}

	outcome := domain.OutcomeDegraded
	finishedAt := r.clock.Now()
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Outcome: &outcome, FinishedAt: &finishedAt}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return domain.ExitNoAction
}

// finishNoOutage closes a run that reached no reboot: outcome ok (the internet
// was up), unconfirmed (a down verdict the in-run re-check cleared), or
// unsustained (a confirmed outage the preceding runs did not corroborate). The
// last two send no Telegram message by design — silence is the point of the
// guards, and the run row plus the log line carry the record instead (issue
// #6).
func (r *runner) finishNoOutage(ctx context.Context, runID, outcome string, exitCode int) int {
	if outcome == domain.OutcomeUnsustained {
		r.logf("run %s: outage not corroborated by the previous %d run(s): not rebooting", runID, r.settings.RebootMinStreak-1)
	}

	finishedAt := r.clock.Now()
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Outcome: &outcome, FinishedAt: &finishedAt}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return exitCode
}

// finishRebootDisabled records a run that would otherwise have rebooted but
// REBOOT_ENABLED is off (plans/003-host-telemetry.md: monitor-only mode).
// Exit stays domain.ExitOK: the reboot path was never entered, so none of
// the reboot-attempt exit codes (3/4/5) apply — the outage is surfaced via
// msg and the reboot_disabled outcome instead. notifier.Flush still runs: a
// hard-mode monitor pass may have live internet and queued messages worth
// draining even though this run itself never touches the router.
func (r *runner) finishRebootDisabled(ctx context.Context, runID, msg string) int {
	r.notify(ctx, msg)
	if err := r.notifier.Flush(ctx); err != nil {
		r.logf("flush outbox: %v", err)
	}

	action := domain.ActionSkippedDisabled
	outcome := domain.OutcomeRebootDisabled
	finishedAt := r.clock.Now()
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Action: &action, Outcome: &outcome, FinishedAt: &finishedAt}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return domain.ExitOK
}

// finishRebootFailed records rebooter.Reboot's error as the run's terminal
// outcome (design §14: every reboot failure — bad login or an unconfirmed
// reboot — maps to the same outcome regardless of which error caused it).
func (r *runner) finishRebootFailed(ctx context.Context, runID string, rebootErr error) int {
	r.notify(ctx, fmt.Sprintf("reboot failed: %v", rebootErr))

	outcome := domain.OutcomeRebootFailed
	errStr := rebootErr.Error()
	finishedAt := r.clock.Now()
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Outcome: &outcome, Error: &errStr, FinishedAt: &finishedAt}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return domain.ExitRebootFailed
}

// finishRecoverySuccess records the internet's return and sends the final
// reboot-completed message. The end-of-loop flush (design §5 step 7) delivers
// the "initiated" message if it couldn't be sent live while the internet was
// still down. Duration is intentionally omitted from the alert: it is
// persisted (reboot_started_at -> internet_restored_at) and served by the
// daemon query API, so the message stays brief (issue #1).
func (r *runner) finishRecoverySuccess(ctx context.Context, runID string, now time.Time) int {
	r.notify(ctx, "completed, internet restored")
	if err := r.notifier.Flush(ctx); err != nil {
		r.logf("flush outbox: %v", err)
	}

	outcome := domain.OutcomeOK
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{InternetRestoredAt: &now, Outcome: &outcome, FinishedAt: &now}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return domain.ExitOK
}

// finishRecoveryTimeout gives up after RECOVERY_TIMEOUT with the internet
// still down (design §5 step 6).
func (r *runner) finishRecoveryTimeout(ctx context.Context, runID string, now time.Time) int {
	r.notify(ctx, fmt.Sprintf("internet still down after %s, giving up", humanDuration(r.settings.RecoveryTimeout)))
	if err := r.notifier.Flush(ctx); err != nil {
		r.logf("flush outbox: %v", err)
	}

	outcome := domain.OutcomeTimeout
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Outcome: &outcome, FinishedAt: &now}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return domain.ExitRecoveryTimeout
}

// handleGatewayTransition records the edge-triggered down/up timestamps (design
// §5): RouterDownAt/RouterUpAt are written only the first time the gateway's
// reachability flips, tracked via *alive across ticks. These transitions are no
// longer announced over Telegram (issue #1: the reboot flow emits only the
// "initiated" and "completed" messages) — only the timestamps are persisted,
// still feeding the data model and the daemon query API. If the router cycles
// between two polls (the "fast reboot" case), gw.OK is already true again by
// the time it's next observed, alive was never flipped to false, and neither
// timestamp is written.
func (r *runner) handleGatewayTransition(ctx context.Context, runID string, alive *bool, gw domain.TargetResult, now time.Time) {
	switch {
	case *alive && !gw.OK:
		*alive = false
		if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{RouterDownAt: &now}); err != nil {
			r.logf("update run %s: %v", runID, err)
		}
	case !*alive && gw.OK:
		*alive = true
		if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{RouterUpAt: &now}); err != nil {
			r.logf("update run %s: %v", runID, err)
		}
	}
}

// hardFlow skips the initial connectivity check (internet_ok stays NULL) and
// goes straight to the reboot sequence (design §7), unless REBOOT_ENABLED is
// off (plans/003-host-telemetry.md: monitor-only mode never touches the
// router, even on a scheduled hard reboot).
func (r *runner) hardFlow(ctx context.Context, startedAt time.Time) int {
	runID, err := r.repo.InsertRun(ctx, startedAt, domain.ModeHard, nil)
	if err != nil {
		r.logf("insert run: %v", err)
		return domain.ExitConfigError
	}
	r.collectTelemetry(ctx, runID)

	if !r.settings.RebootEnabled {
		return r.finishRebootDisabled(ctx, runID, "reboot disabled (monitor-only): skipping scheduled hard reboot")
	}

	return r.rebootSequence(ctx, runID, domain.ReasonScheduledHardReboot)
}

// logf reports a store/notifier failure that isn't itself the run's terminal
// outcome (CLAUDE.md's sanctioned "logger" exception to "no skipped errors");
// the watchdog's job is restoring internet, so a persistence hiccup along the
// way is logged and the flow continues rather than aborting the reboot.
func (r *runner) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "yarddog: "+format+"\n", args...)
}

// notify sends text tagged and labeled per design §8.4
// ("#REBOOT {LABEL} ..."). A failure here is a store failure in the outbox
// itself (Notifier.Notify already swallows a live-send failure), not a
// reason to change the run's outcome, so it is only logged.
func (r *runner) notify(ctx context.Context, text string) {
	tagged := fmt.Sprintf("#REBOOT %s %s", r.settings.Label, text)
	if err := r.notifier.Notify(ctx, tagged); err != nil {
		r.logf("notify: %v", err)
	}
}

// outageSustained reports whether the REBOOT_MIN_STREAK-1 runs before runID
// also saw the internet as down (issue #6). One five-minute cron sample is not
// an outage: in 21 days of recorded history exactly one down verdict had a
// down predecessor, while 11 reboots fired on isolated samples. A run with no
// verdict at all (a hard-mode run, internet_ok NULL) does not corroborate — it
// never probed — and neither does a history too short to fill the streak. A
// failed query is fail-closed for the same reason as the cooldown guard: an
// unreadable history must not fall through to an unconditional reboot.
func (r *runner) outageSustained(ctx context.Context, runID string) bool {
	need := r.settings.RebootMinStreak - 1
	if need <= 0 {
		return true
	}

	previous, err := r.repo.RecentRuns(ctx, runID, need)
	if err != nil {
		r.logf("recent runs: %v", err)
		return false
	}
	if len(previous) < need {
		return false
	}

	for _, p := range previous {
		if p.InternetOK == nil || *p.InternetOK {
			return false
		}
	}
	return true
}

// persistCheck writes one probed target as a checks row (design §9).
func (r *runner) persistCheck(ctx context.Context, runID, phase string, tr domain.TargetResult) {
	latencyMS := tr.Latency.Milliseconds()
	c := domain.Check{
		RunID:     runID,
		TS:        r.clock.Now(),
		Phase:     phase,
		Target:    tr.Target,
		Kind:      tr.Kind,
		OK:        tr.OK,
		LatencyMS: &latencyMS,
		Error:     tr.Error,
	}
	if err := r.repo.InsertCheck(ctx, c); err != nil {
		r.logf("insert check for run %s target %s: %v", runID, tr.Target, err)
	}
}

// persistChecks writes every probed target in trs as a checks row.
func (r *runner) persistChecks(ctx context.Context, runID, phase string, trs []domain.TargetResult) {
	for _, tr := range trs {
		r.persistCheck(ctx, runID, phase, tr)
	}
}

// rebootSequence runs the reboot flow shared by soft and hard mode (design
// §5): flush the outbox, announce the reboot, request it, then hand off to
// the recovery loop. A rebooter.Reboot failure ends the run immediately —
// there is nothing to recover from without a reboot having happened.
func (r *runner) rebootSequence(ctx context.Context, runID, reason string) int {
	if err := r.notifier.Flush(ctx); err != nil {
		r.logf("flush outbox: %v", err)
	}

	action := domain.ActionReboot
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Action: &action}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}

	r.notify(ctx, fmt.Sprintf("initiated (reason: %s)", reason))

	if err := r.rebooter.Reboot(ctx); err != nil {
		return r.finishRebootFailed(ctx, runID, err)
	}

	rebootStartedAt := r.clock.Now()
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{RebootStartedAt: &rebootStartedAt}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}

	return r.recoveryLoop(ctx, runID, rebootStartedAt)
}

// recoveryLoop polls the gateway and the internet every RECOVERY_INTERVAL,
// up to RECOVERY_TIMEOUT total, recording every probe as a checks row
// (design §5). alive starts true because rebooter.Reboot just finished
// talking to the router — it was reachable a moment ago — so the first
// transition handleGatewayTransition can observe is "went down", never a
// spurious "is up" on the very first tick.
func (r *runner) recoveryLoop(ctx context.Context, runID string, rebootStartedAt time.Time) int {
	alive := true

	for {
		<-r.clock.After(r.settings.RecoveryInterval)
		now := r.clock.Now()

		gw := r.checker.Gateway(ctx)
		r.persistCheck(ctx, runID, domain.PhaseRecovery, gw)
		r.handleGatewayTransition(ctx, runID, &alive, gw, now)

		inet := r.checker.Check(ctx)
		r.persistChecks(ctx, runID, domain.PhaseRecovery, inet.Targets)

		// anything short of a down verdict ends the loop: a degraded state
		// (issue #6) means the link the reboot was meant to restore is back,
		// and waiting out the full RECOVERY_TIMEOUT for DNS to also recover
		// would only hold the process open for a fault a reboot cannot fix.
		if !inet.IsDown() {
			return r.finishRecoverySuccess(ctx, runID, now)
		}
		if now.Sub(rebootStartedAt) >= r.settings.RecoveryTimeout {
			return r.finishRecoveryTimeout(ctx, runID, now)
		}
	}
}

// run dispatches to the soft or hard flow (design §6, §7).
func (r *runner) run(ctx context.Context, mode string) int {
	startedAt := r.clock.Now()

	if mode == domain.ModeHard {
		return r.hardFlow(ctx, startedAt)
	}
	return r.softFlow(ctx, startedAt)
}

// shouldAnnounce reports whether a repeating state is worth another Telegram
// message: true only when the run before runID did not already end in the same
// outcome (issue #6). A cron collector runs every few minutes, so a state that
// lasts an hour would otherwise send a message per run — the storm visible in
// issue #6's screenshot, where five identical cooldown notices arrived between
// 01:50 and 03:05. A failed history query announces: a duplicate message costs
// the operator far less than a silent outage.
func (r *runner) shouldAnnounce(ctx context.Context, runID, outcome string) bool {
	previous, err := r.repo.RecentRuns(ctx, runID, 1)
	if err != nil {
		r.logf("recent runs: %v", err)
		return true
	}
	if len(previous) == 0 {
		return true
	}
	return previous[0].Outcome != outcome
}

// skipCooldown records a reboot skipped because the last one is still within
// REBOOT_COOLDOWN (design §6) — rebooting through a provider-side outage
// would otherwise cycle the router every run for no benefit. The notice is
// edge-triggered (shouldAnnounce): the skip repeats every run for as long as
// the cooldown lasts, but it is only news the first time.
func (r *runner) skipCooldown(ctx context.Context, runID string, age time.Duration) int {
	if r.shouldAnnounce(ctx, runID, domain.OutcomeSkipped) {
		r.notify(ctx, fmt.Sprintf("no internet, skipping reboot (cooldown: last reboot %s ago)", humanDuration(age)))
	}

	action := domain.ActionSkippedCooldown
	outcome := domain.OutcomeSkipped
	finishedAt := r.clock.Now()
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Action: &action, Outcome: &outcome, FinishedAt: &finishedAt}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return domain.ExitSkippedCooldown
}

// skipCooldownUnknown records a reboot skipped because the cooldown query
// itself failed (design §6 guard, fail-closed): a broken
// GetLastRebootStartedAt must never fall through to an unconditional
// reboot, so a transient store error is treated the same as "cooldown still
// active" rather than silently bypassed. queryErr is a plain store error
// (no router/Telegram secrets pass through this query) so it is safe to
// include verbatim in the Telegram message and the run's error column.
func (r *runner) skipCooldownUnknown(ctx context.Context, runID string, queryErr error) int {
	r.notify(ctx, fmt.Sprintf("no internet, skipping reboot (cooldown state unknown: %v)", queryErr))

	action := domain.ActionSkippedCooldown
	outcome := domain.OutcomeSkipped
	errStr := queryErr.Error()
	finishedAt := r.clock.Now()
	if err := r.repo.UpdateRun(ctx, runID, domain.RunUpdate{Action: &action, Outcome: &outcome, Error: &errStr, FinishedAt: &finishedAt}); err != nil {
		r.logf("update run %s: %v", runID, err)
	}
	return domain.ExitSkippedCooldown
}

// softFlow probes the internet first (design §6): up means nothing to do;
// down goes through the issue #6 guards — re-probe within the run, then
// require the preceding runs to agree — before REBOOT_ENABLED and the
// cooldown query are ever consulted, so a monitor-only host never touches the
// router (plans/003-host-telemetry.md) and a five-second blip never reboots
// anything.
func (r *runner) softFlow(ctx context.Context, startedAt time.Time) int {
	result := r.checker.Check(ctx)
	initialOK := !result.IsDown()

	runID, err := r.repo.InsertRun(ctx, startedAt, domain.ModeSoft, &initialOK)
	if err != nil {
		r.logf("insert run: %v", err)
		return domain.ExitConfigError
	}
	r.persistChecks(ctx, runID, domain.PhaseInitial, result.Targets)
	r.collectTelemetry(ctx, runID)

	if result.IsDown() {
		result = r.confirmDown(ctx, runID, result)
	}

	switch {
	case result.IsDegraded():
		return r.finishDegraded(ctx, runID)
	case !result.IsDown() && !initialOK:
		return r.finishNoOutage(ctx, runID, domain.OutcomeUnconfirmed, domain.ExitOK)
	case !result.IsDown():
		return r.finishNoOutage(ctx, runID, domain.OutcomeOK, domain.ExitOK)
	}

	if !r.settings.RebootEnabled {
		return r.finishRebootDisabled(ctx, runID, "no internet, reboot disabled (monitor-only)")
	}

	if !r.outageSustained(ctx, runID) {
		return r.finishNoOutage(ctx, runID, domain.OutcomeUnsustained, domain.ExitNoAction)
	}

	lastRebootStartedAt, ok, err := r.repo.GetLastRebootStartedAt(ctx)
	if err != nil {
		r.logf("get last reboot started at: %v", err)
		return r.skipCooldownUnknown(ctx, runID, err)
	}
	if ok {
		if age := r.clock.Now().Sub(lastRebootStartedAt); age < r.settings.RebootCooldown {
			return r.skipCooldown(ctx, runID, age)
		}
	}

	return r.rebootSequence(ctx, runID, domain.ReasonNoInternet)
}

// defaultMetricsTimeout bounds one collectMetrics call (collector + save) so
// a wedged sensor read can never stall the run ahead of the reboot decision
// (host-telemetry P0 fix). It seeds runner.metricsTimeout in Execute; a test
// shrinks that field directly instead of waiting out the real bound.
const defaultMetricsTimeout = 10 * time.Second

// defaultPingTimeout bounds one collectPings call (collector + save),
// mirroring defaultMetricsTimeout: a wedged ping exec can never stall the run
// ahead of the reboot decision (issue #2). It seeds runner.pingTimeout in
// Execute; a test shrinks that field directly instead of waiting out the
// real bound. It is longer than defaultMetricsTimeout since a ping round
// against several hosts, each with its own multi-second -w/-t deadline, is
// inherently slower than a local sensor read.
const defaultPingTimeout = 15 * time.Second

// humanDuration renders d rounded to the second as compact units with no
// trailing zero component ("4m10s", "40m", "2h"), matching the message
// templates in design §8.4 exactly rather than time.Duration.String()'s
// always-present smallest unit (which would render an exact 40 minutes as
// "40m0s").
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)

	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
