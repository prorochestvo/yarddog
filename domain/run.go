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

// PhaseInitial, PhaseConfirm, and PhaseRecovery are the values allowed in
// checks.phase (design §9; PhaseConfirm from issue #6's re-check of a down
// verdict).
const (
	PhaseInitial  = "initial"
	PhaseConfirm  = "confirm"
	PhaseRecovery = "recovery"
)

// OutcomeOK, OutcomeRebootFailed, OutcomeTimeout, OutcomeSkipped, and
// OutcomeRebootDisabled are the values written to runs.outcome (design §9;
// plans/003-host-telemetry.md for OutcomeRebootDisabled).
//
// OutcomeDegraded, OutcomeUnconfirmed, and OutcomeUnsustained (issue #6) name
// the three ways a run can see a failure and still, correctly, not reboot:
// the failure was application-layer only (StateDegraded), a down verdict did
// not survive re-probing within the run, or a confirmed outage was not yet
// corroborated by the preceding runs. They are distinct values because an
// operator reads them differently — degraded means "something really is
// broken, just not the link", the other two mean "nothing was broken long
// enough to act on".
const (
	OutcomeOK             = "ok"
	OutcomeRebootFailed   = "reboot_failed"
	OutcomeTimeout        = "timeout"
	OutcomeSkipped        = "skipped"
	OutcomeRebootDisabled = "reboot_disabled"
	OutcomeDegraded       = "degraded"
	OutcomeUnconfirmed    = "unconfirmed"
	OutcomeUnsustained    = "unsustained"
)

// Run is one row of the runs table (design §9), as returned by
// RunRepository.GetRun. ID is a UUIDv7 string (issue #4: time-ordered, so
// lexicographic and chronological order coincide) rather than an
// autoincrement integer. InternetOK is nil for a hard-mode run, which
// performs no initial check; every *At field is nil until its phase
// transition has happened. Since issue #6 InternetOK tracks StateDown
// specifically — a degraded run (raw link up, domain probes failing) records
// true and says so through its outcome — so it reads as "was the link itself
// gone", the one question the reboot decision turns on.
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

// RunSummary is the slice of a past run the reboot guards read back (issue
// #6): whether that run saw the link as down, and how it ended. Deliberately
// narrower than Run — the guards corroborate a present outage against recent
// history and must not grow a dependency on every column of it.
type RunSummary struct {
	InternetOK *bool
	Outcome    string
}

// RunUpdate carries the phase timestamps, action, outcome, and error to
// apply to a runs row. Every field is a pointer so RunRepository.UpdateRun
// can tell "leave unchanged" (nil) apart from "set this value". Its SQL
// rendering is a persistence detail that stays in infrastructure, not here.
// InternetOK exists so a run whose down verdict is later cleared by
// re-probing (issue #6) can correct the column it wrote from its first,
// mistaken sample.
type RunUpdate struct {
	InternetOK         *bool
	Action             *string
	RebootStartedAt    *time.Time
	RouterDownAt       *time.Time
	RouterUpAt         *time.Time
	InternetRestoredAt *time.Time
	FinishedAt         *time.Time
	Outcome            *string
	Error              *string
}
