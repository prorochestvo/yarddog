package domain

import "testing"

// TestPersistedConstants pins the literal string value of every domain
// vocabulary constant that is stored verbatim in a SQLite column (runs.mode,
// runs.action, checks.phase, runs.outcome, metrics.collector). These values
// are not free to rename: infrastructure's queries filter on the literal
// (e.g. "action = ?" bound to ActionReboot) and already-written production
// rows carry the old string, so an accidental rename here would silently
// desync code and data. ActionSkippedDisabled and OutcomeRebootDisabled
// (plans/003-host-telemetry.md) carry the same guarantee, as do the seven
// Collector* constants (domain/metrics.go).
func TestPersistedConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"ModeSoft", ModeSoft, "soft"},
		{"ModeHard", ModeHard, "hard"},
		{"ActionNone", ActionNone, "none"},
		{"ActionReboot", ActionReboot, "reboot"},
		{"ActionSkippedCooldown", ActionSkippedCooldown, "skipped_cooldown"},
		{"ActionSkippedDisabled", ActionSkippedDisabled, "skipped_disabled"},
		{"PhaseInitial", PhaseInitial, "initial"},
		{"PhaseRecovery", PhaseRecovery, "recovery"},
		{"OutcomeOK", OutcomeOK, "ok"},
		{"OutcomeRebootFailed", OutcomeRebootFailed, "reboot_failed"},
		{"OutcomeTimeout", OutcomeTimeout, "timeout"},
		{"OutcomeSkipped", OutcomeSkipped, "skipped"},
		{"OutcomeRebootDisabled", OutcomeRebootDisabled, "reboot_disabled"},
		{"CollectorTemperature", string(CollectorTemperature), "temperature"},
		{"CollectorFans", string(CollectorFans), "fans"},
		{"CollectorCPU", string(CollectorCPU), "cpu"},
		{"CollectorMemory", string(CollectorMemory), "memory"},
		{"CollectorDisk", string(CollectorDisk), "disk"},
		{"CollectorUptime", string(CollectorUptime), "uptime"},
		{"CollectorNetwork", string(CollectorNetwork), "network"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Fatalf("%s = %q, want %q (persisted verbatim in SQLite)", tt.name, tt.got, tt.want)
			}
		})
	}
}
