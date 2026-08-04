package services

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

func TestExecute(t *testing.T) {
	t.Parallel()

	t.Run("soft, internet up: no reboot, no messages", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true, Latency: 5 * time.Millisecond}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (internet is up)", env.rb.calls)
		}
		if len(env.nt.messages) != 0 {
			t.Fatalf("messages sent = %v, want none", env.nt.messages)
		}

		run := env.repo.run(t, "1")
		if run.Mode != domain.ModeSoft {
			t.Fatalf("Mode = %q, want %q", run.Mode, domain.ModeSoft)
		}
		if run.InternetOK == nil || !*run.InternetOK {
			t.Fatalf("InternetOK = %v, want true", run.InternetOK)
		}
		if run.Action != domain.ActionNone {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionNone)
		}
		if run.Outcome != domain.OutcomeOK {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeOK)
		}
		if run.FinishedAt == nil {
			t.Fatal("FinishedAt is nil, want set")
		}

		if env.mc.calls != 1 {
			t.Fatalf("metrics collector calls = %d, want exactly 1", env.mc.calls)
		}
		if len(env.mr.calls) != 1 || env.mr.calls[0].runID != "1" {
			t.Fatalf("metrics saves = %+v, want exactly one save for run 1", env.mr.calls)
		}
	})

	t.Run("soft, internet down, within cooldown: skips the reboot", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootCooldown = 2 * time.Hour
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false, Error: "dial timeout"}}}}
		env.repo.lastRebootStartedAt = env.clk.now.Add(-40 * time.Minute)
		env.repo.lastRebootOK = true

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitSkippedCooldown {
			t.Fatalf("Execute() = %d, want ExitSkippedCooldown", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (cooldown active)", env.rb.calls)
		}
		if len(env.nt.messages) != 1 {
			t.Fatalf("messages sent = %v, want exactly 1", env.nt.messages)
		}
		want := "#REBOOT " + env.settings.Label + " no internet, skipping reboot (cooldown: last reboot 40m ago)"
		if env.nt.messages[0] != want {
			t.Fatalf("message = %q, want %q", env.nt.messages[0], want)
		}

		run := env.repo.run(t, "1")
		if run.Action != domain.ActionSkippedCooldown {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionSkippedCooldown)
		}
		if run.Outcome != domain.OutcomeSkipped {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeSkipped)
		}
		if run.RebootStartedAt != nil {
			t.Fatalf("RebootStartedAt = %v, want nil (no reboot attempted)", run.RebootStartedAt)
		}

		if len(env.mr.calls) != 1 {
			t.Fatalf("metrics saves = %+v, want exactly one save even on the cooldown-skip path", env.mr.calls)
		}
	})

	t.Run("soft, internet down, cooldown query fails: fails closed, no reboot", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false, Error: "dial timeout"}}}}
		// fakeRunRepo.lastRebootErr stands in for any transient store error
		// that makes the cooldown state genuinely unknown (e.g. a corrupted
		// reboot_started_at column) — the real SQL failure mode is covered
		// against :memory: in infrastructure/store_test.go.
		env.repo.lastRebootErr = errors.New(`parse time "not-a-timestamp": parsing time "not-a-timestamp" as "2006-01-02T15:04:05Z07:00": cannot parse "not-a-timestamp" as "2006"`)

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitSkippedCooldown {
			t.Fatalf("Execute() = %d, want ExitSkippedCooldown (fail closed on a broken cooldown query)", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (cooldown state unknown must never reboot)", env.rb.calls)
		}
		if len(env.nt.messages) != 1 {
			t.Fatalf("messages sent = %v, want exactly 1", env.nt.messages)
		}
		if !strings.Contains(env.nt.messages[0], "cooldown state unknown") {
			t.Fatalf("message = %q, want it to report the cooldown state as unknown", env.nt.messages[0])
		}

		run := env.repo.run(t, "1")
		if run.Action != domain.ActionSkippedCooldown {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionSkippedCooldown)
		}
		if run.Outcome != domain.OutcomeSkipped {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeSkipped)
		}
		if run.Error == "" {
			t.Fatal("Error is empty, want the cooldown query failure recorded")
		}
		if run.RebootStartedAt != nil {
			t.Fatalf("RebootStartedAt = %v, want nil (no reboot attempted)", run.RebootStartedAt)
		}
	})

	t.Run(`soft, internet down, no recent reboot: full sequence with reason "no internet"`, func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false, Error: "down"}}},
			{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}},
		}
		env.chk.gatewayResults = []domain.TargetResult{{Target: "192.168.1.1:80", Kind: domain.CheckKindGateway, OK: true}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if env.rb.calls != 1 {
			t.Fatalf("rebooter.Reboot calls = %d, want 1", env.rb.calls)
		}
		if len(env.nt.messages) == 0 || !strings.Contains(env.nt.messages[0], "reason: no internet") {
			t.Fatalf("first message = %v, want it to carry reason \"no internet\"", env.nt.messages)
		}

		run := env.repo.run(t, "1")
		if run.Action != domain.ActionReboot {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionReboot)
		}

		if env.mc.calls != 1 || len(env.mr.calls) != 1 {
			t.Fatalf("metrics collector calls = %d, saves = %+v, want exactly 1 of each (the recovery loop takes no extra snapshot)", env.mc.calls, env.mr.calls)
		}
	})

	t.Run(`hard: no initial check, reason "scheduled hard reboot"`, func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}
		env.chk.gatewayResults = []domain.TargetResult{{Target: "192.168.1.1:80", Kind: domain.CheckKindGateway, OK: true}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeHard)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if env.chk.checkCalls != 1 {
			t.Fatalf("checker.Check calls = %d, want exactly 1 (the recovery tick, no initial check)", env.chk.checkCalls)
		}

		run := env.repo.run(t, "1")
		if run.InternetOK != nil {
			t.Fatalf("InternetOK = %v, want nil (hard mode makes no initial check)", run.InternetOK)
		}
		if len(env.nt.messages) == 0 || !strings.Contains(env.nt.messages[0], "reason: scheduled hard reboot") {
			t.Fatalf("first message = %v, want it to carry reason \"scheduled hard reboot\"", env.nt.messages)
		}

		if env.mc.calls != 1 || len(env.mr.calls) != 1 || env.mr.calls[0].runID != "1" {
			t.Fatalf("metrics collector calls = %d, saves = %+v, want exactly one save for run 1", env.mc.calls, env.mr.calls)
		}
	})

	t.Run("happy recovery path: only initiated+completed, transitions still recorded", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // initial soft check
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // tick 1
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // tick 2
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // tick 3
			{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}},    // tick 4
		}
		env.chk.gatewayResults = []domain.TargetResult{
			{Target: "gw", Kind: domain.CheckKindGateway, OK: false}, // tick 1: went down
			{Target: "gw", Kind: domain.CheckKindGateway, OK: false}, // tick 2: still down
			{Target: "gw", Kind: domain.CheckKindGateway, OK: true},  // tick 3: is up
			{Target: "gw", Kind: domain.CheckKindGateway, OK: true},  // tick 4: still up
		}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}

		wantOrder := []string{
			"initiated (reason: no internet)",
			"completed, internet restored",
		}
		if len(env.nt.messages) != len(wantOrder) {
			t.Fatalf("messages = %v, want %d messages in order %v", env.nt.messages, len(wantOrder), wantOrder)
		}
		for i, want := range wantOrder {
			gotSuffix := strings.TrimPrefix(env.nt.messages[i], "#REBOOT "+env.settings.Label+" ")
			if gotSuffix != want {
				t.Fatalf("message[%d] = %q, want suffix %q", i, env.nt.messages[i], want)
			}
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeOK {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeOK)
		}
		for name, ts := range map[string]*time.Time{
			"RebootStartedAt":    run.RebootStartedAt,
			"RouterDownAt":       run.RouterDownAt,
			"RouterUpAt":         run.RouterUpAt,
			"InternetRestoredAt": run.InternetRestoredAt,
			"FinishedAt":         run.FinishedAt,
		} {
			if ts == nil {
				t.Fatalf("%s is nil, want set", name)
			}
		}
		if env.nt.flushCalls == 0 {
			t.Fatal("outbox was never flushed, want a flush at the end of the recovery loop")
		}
	})

	t.Run("fast reboot: down/up transitions suppressed, only starting+completed sent", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // initial
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // tick 1
			{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}},    // tick 2
		}
		// gateway never observed down: the router cycled between polls.
		env.chk.gatewayResults = []domain.TargetResult{{Target: "gw", Kind: domain.CheckKindGateway, OK: true}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}

		var gotSuffixes []string
		for _, m := range env.nt.messages {
			gotSuffixes = append(gotSuffixes, strings.TrimPrefix(m, "#REBOOT "+env.settings.Label+" "))
		}
		wantOrder := []string{
			"initiated (reason: no internet)",
			"completed, internet restored",
		}
		if len(gotSuffixes) != len(wantOrder) {
			t.Fatalf("messages = %v, want only %v (down/up suppressed)", gotSuffixes, wantOrder)
		}
		for i, want := range wantOrder {
			if gotSuffixes[i] != want {
				t.Fatalf("message[%d] = %q, want %q", i, gotSuffixes[i], want)
			}
		}

		run := env.repo.run(t, "1")
		if run.RouterDownAt != nil {
			t.Fatalf("RouterDownAt = %v, want nil (transition never observed)", run.RouterDownAt)
		}
		if run.RouterUpAt != nil {
			t.Fatalf("RouterUpAt = %v, want nil (transition never observed)", run.RouterUpAt)
		}
	})

	t.Run("recovery timeout: internet stays down past RECOVERY_TIMEOUT", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RecoveryInterval = time.Minute
		env.settings.RecoveryTimeout = 15 * time.Minute
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}}
		env.chk.gatewayResults = []domain.TargetResult{{Target: "gw", Kind: domain.CheckKindGateway, OK: false}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitRecoveryTimeout {
			t.Fatalf("Execute() = %d, want ExitRecoveryTimeout", code)
		}

		last := env.nt.messages[len(env.nt.messages)-1]
		want := "#REBOOT " + env.settings.Label + " internet still down after 15m, giving up"
		if last != want {
			t.Fatalf("last message = %q, want %q", last, want)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeTimeout {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeTimeout)
		}
		if run.InternetRestoredAt != nil {
			t.Fatalf("InternetRestoredAt = %v, want nil (internet never came back)", run.InternetRestoredAt)
		}
	})

	t.Run("reboot request fails", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}}
		env.rb.err = errRebootFailed

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitRebootFailed {
			t.Fatalf("Execute() = %d, want ExitRebootFailed", code)
		}
		if env.chk.gatewayCalls != 0 {
			t.Fatalf("gateway probes = %d, want 0 (recovery loop must not run)", env.chk.gatewayCalls)
		}

		last := env.nt.messages[len(env.nt.messages)-1]
		want := "#REBOOT " + env.settings.Label + " reboot failed: " + errRebootFailed.Error()
		if last != want {
			t.Fatalf("last message = %q, want %q", last, want)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeRebootFailed {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeRebootFailed)
		}
		if run.Error != errRebootFailed.Error() {
			t.Fatalf("Error = %q, want %q", run.Error, errRebootFailed.Error())
		}
	})

	t.Run("soft, internet down, reboot disabled: monitor-only, never touches the router", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootEnabled = false
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false, Error: "dial timeout"}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK (monitor-only never fails the reboot path)", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (REBOOT_ENABLED=false)", env.rb.calls)
		}
		want := "#REBOOT " + env.settings.Label + " no internet, reboot disabled (monitor-only)"
		if len(env.nt.messages) != 1 || env.nt.messages[0] != want {
			t.Fatalf("messages = %v, want exactly [%q]", env.nt.messages, want)
		}

		run := env.repo.run(t, "1")
		if run.Action != domain.ActionSkippedDisabled {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionSkippedDisabled)
		}
		if run.Outcome != domain.OutcomeRebootDisabled {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeRebootDisabled)
		}
		if run.FinishedAt == nil {
			t.Fatal("FinishedAt is nil, want set")
		}
		if env.mc.calls != 1 || len(env.mr.calls) != 1 {
			t.Fatalf("metrics collector calls = %d, saves = %+v, want exactly 1 of each (monitor-only still records telemetry)", env.mc.calls, env.mr.calls)
		}
	})

	t.Run("hard, reboot disabled: monitor-only, never touches the router", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootEnabled = false

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeHard)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (REBOOT_ENABLED=false)", env.rb.calls)
		}
		if env.chk.gatewayCalls != 0 {
			t.Fatalf("gateway probes = %d, want 0 (recovery loop must not run)", env.chk.gatewayCalls)
		}
		want := "#REBOOT " + env.settings.Label + " reboot disabled (monitor-only): skipping scheduled hard reboot"
		if len(env.nt.messages) != 1 || env.nt.messages[0] != want {
			t.Fatalf("messages = %v, want exactly [%q]", env.nt.messages, want)
		}

		run := env.repo.run(t, "1")
		if run.Action != domain.ActionSkippedDisabled {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionSkippedDisabled)
		}
		if run.Outcome != domain.OutcomeRebootDisabled {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeRebootDisabled)
		}
		if env.mc.calls != 1 || len(env.mr.calls) != 1 {
			t.Fatalf("metrics collector calls = %d, saves = %+v, want exactly 1 of each", env.mc.calls, env.mr.calls)
		}
	})

	t.Run("soft, internet up, reboot disabled: identical to a normal up run", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootEnabled = false
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if len(env.nt.messages) != 0 {
			t.Fatalf("messages sent = %v, want none (reboot-disabled only diverges when a reboot would have happened)", env.nt.messages)
		}

		run := env.repo.run(t, "1")
		if run.Action != domain.ActionNone {
			t.Fatalf("Action = %q, want %q (byte-for-byte a normal up run)", run.Action, domain.ActionNone)
		}
		if run.Outcome != domain.OutcomeOK {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeOK)
		}
	})

	t.Run("metrics disabled: collector never runs and nothing is saved", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.MetricsEnabled = false
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}},
			{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}},
		}
		env.chk.gatewayResults = []domain.TargetResult{{Target: "gw", Kind: domain.CheckKindGateway, OK: true}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if env.mc.calls != 0 {
			t.Fatalf("metrics collector calls = %d, want 0 (METRICS_ENABLED=false)", env.mc.calls)
		}
		if len(env.mr.calls) != 0 {
			t.Fatalf("metrics saves = %+v, want none", env.mr.calls)
		}
	})

	t.Run("metrics save failure is logged and swallowed: outcome and exit code unaffected", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.mr.err = errors.New("disk full")
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK (a SaveMetrics failure must never change the exit code)", code)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeOK {
			t.Fatalf("Outcome = %q, want %q (a SaveMetrics failure must never change the run's outcome)", run.Outcome, domain.OutcomeOK)
		}
		if env.mc.calls != 1 {
			t.Fatalf("metrics collector calls = %d, want 1 (the collector itself still ran)", env.mc.calls)
		}
	})

	t.Run("a hung metrics collector cannot block the run past the metrics timeout", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}

		// Built directly rather than through Execute (which has no way to
		// inject a shorter-than-production metricsTimeout): r.run is the
		// same code path Execute would take, just with a bound the test
		// doesn't have to wait out for real.
		r := &runner{
			settings:       env.settings,
			repo:           env.repo,
			metricsRepo:    env.mr,
			pingRepo:       env.pr,
			checker:        env.chk,
			rebooter:       env.rb,
			notifier:       env.nt,
			metrics:        blockingMetricsCollector{},
			pings:          env.pc,
			clock:          env.clk,
			metricsTimeout: 20 * time.Millisecond,
			pingTimeout:    defaultPingTimeout,
		}

		start := time.Now()
		code := r.run(t.Context(), domain.ModeSoft)
		elapsed := time.Since(start)

		if code != domain.ExitOK {
			t.Fatalf("run() = %d, want ExitOK (a hung collector must never change the run's outcome)", code)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("run() took %s, want it bounded by metricsTimeout (20ms), not a collector hang", elapsed)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeOK {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeOK)
		}
		if run.FinishedAt == nil {
			t.Fatal("FinishedAt is nil, want set (the run still reached its normal terminal state)")
		}
		if len(env.mr.calls) != 0 {
			t.Fatalf("metrics saves = %+v, want none (the collector never returned within the timeout)", env.mr.calls)
		}
	})

	t.Run("ping enabled: persists a round in both soft and hard flow", func(t *testing.T) {
		t.Parallel()

		t.Run("soft", func(t *testing.T) {
			t.Parallel()

			env := newTestEnv(t)
			env.pc.results = []domain.PingResult{{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 12.5, OK: true}}
			env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}

			code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

			if code != domain.ExitOK {
				t.Fatalf("Execute() = %d, want ExitOK", code)
			}
			if env.pc.calls != 1 {
				t.Fatalf("ping collector calls = %d, want exactly 1", env.pc.calls)
			}
			if len(env.pr.calls) != 1 || env.pr.calls[0].runID != "1" {
				t.Fatalf("ping saves = %+v, want exactly one save for run 1", env.pr.calls)
			}
		})

		t.Run("hard", func(t *testing.T) {
			t.Parallel()

			env := newTestEnv(t)
			env.pc.results = []domain.PingResult{{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 12.5, OK: true}}
			env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}
			env.chk.gatewayResults = []domain.TargetResult{{Target: "gw", Kind: domain.CheckKindGateway, OK: true}}

			code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeHard)

			if code != domain.ExitOK {
				t.Fatalf("Execute() = %d, want ExitOK", code)
			}
			if env.pc.calls != 1 {
				t.Fatalf("ping collector calls = %d, want exactly 1", env.pc.calls)
			}
			if len(env.pr.calls) != 1 || env.pr.calls[0].runID != "1" {
				t.Fatalf("ping saves = %+v, want exactly one save for run 1", env.pr.calls)
			}
		})
	})

	t.Run("ping disabled: collector never runs and nothing is saved", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.PingEnabled = false
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if env.pc.calls != 0 {
			t.Fatalf("ping collector calls = %d, want 0 (PingEnabled=false)", env.pc.calls)
		}
		if len(env.pr.calls) != 0 {
			t.Fatalf("ping saves = %+v, want none", env.pr.calls)
		}
	})

	t.Run("ping save failure is logged and swallowed: outcome and exit code unaffected", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.pr.err = errors.New("disk full")
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK (a SavePings failure must never change the exit code)", code)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeOK {
			t.Fatalf("Outcome = %q, want %q (a SavePings failure must never change the run's outcome)", run.Outcome, domain.OutcomeOK)
		}
		if env.pc.calls != 1 {
			t.Fatalf("ping collector calls = %d, want 1 (the collector itself still ran)", env.pc.calls)
		}
	})

	t.Run("a hung ping collector cannot block the run past the ping timeout", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}}

		// Built directly rather than through Execute (which has no way to
		// inject a shorter-than-production pingTimeout): r.run is the same
		// code path Execute would take, just with a bound the test doesn't
		// have to wait out for real (mirrors the hung-metrics-collector test).
		r := &runner{
			settings:       env.settings,
			repo:           env.repo,
			metricsRepo:    env.mr,
			pingRepo:       env.pr,
			checker:        env.chk,
			rebooter:       env.rb,
			notifier:       env.nt,
			metrics:        env.mc,
			pings:          blockingPingCollector{},
			clock:          env.clk,
			metricsTimeout: defaultMetricsTimeout,
			pingTimeout:    20 * time.Millisecond,
		}

		start := time.Now()
		code := r.run(t.Context(), domain.ModeSoft)
		elapsed := time.Since(start)

		if code != domain.ExitOK {
			t.Fatalf("run() = %d, want ExitOK (a hung collector must never change the run's outcome)", code)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("run() took %s, want it bounded by pingTimeout (20ms), not a collector hang", elapsed)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeOK {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeOK)
		}
		if run.FinishedAt == nil {
			t.Fatal("FinishedAt is nil, want set (the run still reached its normal terminal state)")
		}
		if len(env.pr.calls) != 0 {
			t.Fatalf("ping saves = %+v, want none (the collector never returned within the timeout)", env.pr.calls)
		}
	})

	t.Run("soft, down verdict cleared by the re-check: no reboot, recorded as unconfirmed", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.CheckConfirmations = 2
		env.settings.CheckConfirmDelay = 20 * time.Second
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // initial
			{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}},    // re-check clears it
		}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK (the outage was noise)", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (a single sample must never reboot)", env.rb.calls)
		}
		if len(env.nt.messages) != 0 {
			t.Fatalf("messages sent = %v, want none (silence is the point of the guard)", env.nt.messages)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeUnconfirmed {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeUnconfirmed)
		}
		if run.InternetOK == nil || !*run.InternetOK {
			t.Fatalf("InternetOK = %v, want true (the initial sample's verdict was corrected)", run.InternetOK)
		}
		if got, want := env.repo.phases(), []string{domain.PhaseInitial, domain.PhaseConfirm}; !equalStrings(got, want) {
			t.Fatalf("check phases = %v, want %v", got, want)
		}
	})

	t.Run("soft, down verdict survives the re-check: reboots on the confirmed outage", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.CheckConfirmations = 2
		env.settings.CheckConfirmDelay = 20 * time.Second
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // initial + both re-checks
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}},
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}},
			{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}}, // recovery tick
		}
		env.chk.gatewayResults = []domain.TargetResult{{Target: "192.168.1.1:80", Kind: domain.CheckKindGateway, OK: true}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK (reboot completed, internet restored)", code)
		}
		if env.rb.calls != 1 {
			t.Fatalf("rebooter.Reboot calls = %d, want 1 (three samples agreed)", env.rb.calls)
		}

		run := env.repo.run(t, "1")
		if run.InternetOK == nil || *run.InternetOK {
			t.Fatalf("InternetOK = %v, want false (the outage was real)", run.InternetOK)
		}
		if got, want := env.repo.phases()[:3], []string{domain.PhaseInitial, domain.PhaseConfirm, domain.PhaseConfirm}; !equalStrings(got, want) {
			t.Fatalf("check phases = %v, want %v", got, want)
		}
	})

	t.Run("soft, all domain targets down while raw ip holds: degraded, never rebooted", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.CheckConfirmations = 2
		env.settings.CheckConfirmDelay = 20 * time.Second
		env.chk.checkResults = []domain.Result{{State: domain.StateDegraded, Targets: []domain.TargetResult{
			{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true, Latency: 24 * time.Millisecond},
			{Target: "https://a.example/", Kind: domain.CheckKindDomain, OK: false, Error: "context deadline exceeded (stage=dns)"},
		}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitNoAction {
			t.Fatalf("Execute() = %d, want ExitNoAction", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (a reboot does not fix a broken DNS path)", env.rb.calls)
		}
		if env.chk.checkCalls != 1 {
			t.Fatalf("checker.Check calls = %d, want 1 (degraded is not a down verdict to re-confirm)", env.chk.checkCalls)
		}
		if len(env.nt.messages) != 1 || !strings.Contains(env.nt.messages[0], "domain checks failing") {
			t.Fatalf("messages = %v, want one degraded notice", env.nt.messages)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeDegraded {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeDegraded)
		}
		if run.Action != domain.ActionNone {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionNone)
		}
	})

	t.Run("soft, degraded again right after a degraded run: no repeat message", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.repo.recentRuns = []domain.RunSummary{{Outcome: domain.OutcomeDegraded}}
		env.chk.checkResults = []domain.Result{{State: domain.StateDegraded, Targets: []domain.TargetResult{
			{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true},
			{Target: "https://a.example/", Kind: domain.CheckKindDomain, OK: false},
		}}}

		Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if len(env.nt.messages) != 0 {
			t.Fatalf("messages = %v, want none (the previous run already announced this state)", env.nt.messages)
		}
	})

	t.Run("soft, confirmed outage the previous run did not see: unsustained, no reboot", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootMinStreak = 2
		healthy := true
		env.repo.recentRuns = []domain.RunSummary{{InternetOK: &healthy, Outcome: domain.OutcomeOK}}
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitNoAction {
			t.Fatalf("Execute() = %d, want ExitNoAction", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (one five-minute sample is not an outage)", env.rb.calls)
		}
		if len(env.nt.messages) != 0 {
			t.Fatalf("messages = %v, want none", env.nt.messages)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeUnsustained {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeUnsustained)
		}
		if run.InternetOK == nil || *run.InternetOK {
			t.Fatalf("InternetOK = %v, want false (the verdict stands, only the action is withheld)", run.InternetOK)
		}
	})

	t.Run("soft, confirmed outage the previous run also saw: reboots", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootMinStreak = 2
		outage := false
		env.repo.recentRuns = []domain.RunSummary{{InternetOK: &outage, Outcome: domain.OutcomeUnsustained}}
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}},
			{State: domain.StateUp, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}}},
		}
		env.chk.gatewayResults = []domain.TargetResult{{Target: "192.168.1.1:80", Kind: domain.CheckKindGateway, OK: true}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK", code)
		}
		if env.rb.calls != 1 {
			t.Fatalf("rebooter.Reboot calls = %d, want 1 (the outage spans two runs)", env.rb.calls)
		}
	})

	t.Run("soft, a hard-mode run in the history does not corroborate an outage", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootMinStreak = 2
		env.repo.recentRuns = []domain.RunSummary{{InternetOK: nil, Outcome: domain.OutcomeOK}}
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitNoAction {
			t.Fatalf("Execute() = %d, want ExitNoAction (a run that never probed cannot corroborate)", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0", env.rb.calls)
		}
	})

	t.Run("soft, unreadable history is fail-closed: no reboot", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.settings.RebootMinStreak = 2
		env.repo.recentRunsErr = errors.New("store: disk i/o error")
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitNoAction {
			t.Fatalf("Execute() = %d, want ExitNoAction", code)
		}
		if env.rb.calls != 0 {
			t.Fatalf("rebooter.Reboot calls = %d, want 0 (an unreadable history must not fall through to a reboot)", env.rb.calls)
		}
	})

	t.Run("soft, cooldown skip repeated after a skipped run: no repeat message", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.repo.lastRebootStartedAt = env.clk.now.Add(-30 * time.Minute)
		env.repo.lastRebootOK = true
		env.repo.recentRuns = []domain.RunSummary{{Outcome: domain.OutcomeSkipped}}
		env.chk.checkResults = []domain.Result{{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitSkippedCooldown {
			t.Fatalf("Execute() = %d, want ExitSkippedCooldown", code)
		}
		if len(env.nt.messages) != 0 {
			t.Fatalf("messages = %v, want none (the cooldown skip was already announced)", env.nt.messages)
		}

		run := env.repo.run(t, "1")
		if run.Outcome != domain.OutcomeSkipped {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, domain.OutcomeSkipped)
		}
	})

	t.Run("soft, recovery ends on a degraded verdict rather than waiting out the timeout", func(t *testing.T) {
		t.Parallel()

		env := newTestEnv(t)
		env.chk.checkResults = []domain.Result{
			{State: domain.StateDown, Targets: []domain.TargetResult{{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: false}}}, // initial
			{State: domain.StateDegraded, Targets: []domain.TargetResult{ // tick 1: link back, DNS still broken
				{Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true},
				{Target: "https://a.example/", Kind: domain.CheckKindDomain, OK: false},
			}},
		}
		env.chk.gatewayResults = []domain.TargetResult{{Target: "192.168.1.1:80", Kind: domain.CheckKindGateway, OK: true}}

		code := Execute(t.Context(), env.settings, env.repo, env.mr, env.pr, env.chk, env.rb, env.nt, env.mc, env.pc, env.clk, domain.ModeSoft)

		if code != domain.ExitOK {
			t.Fatalf("Execute() = %d, want ExitOK (the link the reboot targeted is back)", code)
		}

		run := env.repo.run(t, "1")
		if run.InternetRestoredAt == nil {
			t.Fatal("InternetRestoredAt is nil, want set")
		}
	})
}

// equalStrings reports whether two string slices hold the same values in the
// same order, so a phase-sequence assertion reads as one comparison instead
// of a hand-rolled loop at every call site.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// errRebootFailed is a stand-in for gateway/router's real reboot errors;
// the orchestrator only needs an error value to test its (exit code,
// outcome, message) mapping, not any particular router failure mode.
var errRebootFailed = errors.New("router: reboot request: connection refused")

// testEnv bundles one test's Execute dependencies: fakes for every one of
// the nine ports, all wired to the same fake clock so message timestamps
// and duration math agree with each other.
type testEnv struct {
	settings Settings
	repo     *fakeRunRepo
	mr       *fakeMetricsRepo
	pr       *fakePingRepo
	chk      *fakeChecker
	rb       *fakeRebooter
	nt       *fakeNotifier
	mc       *fakeMetricsCollector
	pc       *fakePingCollector
	clk      *fakeClock
}

// newTestEnv builds a testEnv with sane defaults (2h cooldown, 60s recovery
// interval, 15m recovery timeout, reboot, metrics, and ping all enabled — the
// design's own defaults) that a subtest can override before calling Execute.
// The issue #6 false-positive guards are left at their "off" values
// (CheckConfirmations 0, RebootMinStreak 0) so every pre-existing scenario
// keeps asserting the flow it was written for; the guards have their own
// subtests, which set them explicitly.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	return &testEnv{
		settings: Settings{
			Label:            "TESTLABEL",
			RebootCooldown:   2 * time.Hour,
			RecoveryInterval: time.Minute,
			RecoveryTimeout:  15 * time.Minute,
			RebootEnabled:    true,
			MetricsEnabled:   true,
			PingEnabled:      true,
		},
		repo: newFakeRunRepo(),
		mr:   &fakeMetricsRepo{},
		pr:   &fakePingRepo{},
		chk:  &fakeChecker{},
		rb:   &fakeRebooter{},
		nt:   &fakeNotifier{},
		mc:   &fakeMetricsCollector{},
		pc:   &fakePingCollector{},
		clk:  &fakeClock{now: time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC)},
	}
}
