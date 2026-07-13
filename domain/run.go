package domain

import "time"

// ModeSoft and ModeHard select the orchestrator's flow (design §6, §7): soft
// probes the internet first and reboots only when it's down and outside
// cooldown; hard reboots unconditionally.
const (
	ModeSoft = "soft"
	ModeHard = "hard"
)

// ActionNone, ActionReboot, ActionSkippedCooldown, and ActionSkippedDisabled
// are the values allowed in runs.action (design §9;
// plans/003-host-telemetry.md for ActionSkippedDisabled).
const (
	ActionNone            = "none"
	ActionReboot          = "reboot"
	ActionSkippedCooldown = "skipped_cooldown"
	ActionSkippedDisabled = "skipped_disabled"
)

// PhaseInitial and PhaseRecovery are the values allowed in checks.phase
// (design §9).
const (
	PhaseInitial  = "initial"
	PhaseRecovery = "recovery"
)

// OutcomeOK, OutcomeRebootFailed, OutcomeTimeout, OutcomeSkipped, and
// OutcomeRebootDisabled are the values written to runs.outcome (design §9;
// plans/003-host-telemetry.md for OutcomeRebootDisabled).
const (
	OutcomeOK             = "ok"
	OutcomeRebootFailed   = "reboot_failed"
	OutcomeTimeout        = "timeout"
	OutcomeSkipped        = "skipped"
	OutcomeRebootDisabled = "reboot_disabled"
)

// Run is one row of the runs table (design §9), as returned by
// RunRepository.GetRun. ID is a UUIDv7 string (issue #4: time-ordered, so
// lexicographic and chronological order coincide) rather than an
// autoincrement integer. InternetOK is nil for a hard-mode run, which
// performs no initial check; every *At field is nil until its phase
// transition has happened.
type Run struct {
	ID                 string
	StartedAt          time.Time
	Mode               string
	InternetOK         *bool
	Action             string
	RebootStartedAt    *time.Time
	RouterDownAt       *time.Time
	RouterUpAt         *time.Time
	InternetRestoredAt *time.Time
	FinishedAt         *time.Time
	Outcome            string
	Error              string
}

// Check is one row of the checks table (design §9). RunID is the owning
// run's UUIDv7 string id (issue #4).
type Check struct {
	RunID     string
	TS        time.Time
	Phase     string
	Target    string
	Kind      string
	OK        bool
	LatencyMS *int64
	Error     string
}

// RunUpdate carries the phase timestamps, action, outcome, and error to
// apply to a runs row. Every field is a pointer so RunRepository.UpdateRun
// can tell "leave unchanged" (nil) apart from "set this value". Its SQL
// rendering is a persistence detail that stays in infrastructure, not here.
type RunUpdate struct {
	Action             *string
	RebootStartedAt    *time.Time
	RouterDownAt       *time.Time
	RouterUpAt         *time.Time
	InternetRestoredAt *time.Time
	FinishedAt         *time.Time
	Outcome            *string
	Error              *string
}
