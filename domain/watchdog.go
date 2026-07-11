package domain

import (
	"fmt"
	"strings"
)

// ExitOK, ExitConfigError, ExitLockHeld, ExitRebootFailed, ExitRecoveryTimeout,
// and ExitSkippedCooldown are the process exit codes (design §2). services.Execute
// is the single place that decides the outcome of a watchdog pass and returns
// ExitOK, ExitRebootFailed, ExitRecoveryTimeout, or ExitSkippedCooldown;
// ExitConfigError and ExitLockHeld are produced by main itself before Execute
// is ever called (a bad .env, or a lock already held) and are declared here
// only so every code in the design's table has one canonical home.
const (
	ExitOK              = 0
	ExitConfigError     = 1
	ExitLockHeld        = 2
	ExitRebootFailed    = 3
	ExitRecoveryTimeout = 4
	ExitSkippedCooldown = 5
)

// ReasonNoInternet and ReasonScheduledHardReboot are the two reasons the
// "starting router reboot" message can carry (design §8.4).
const (
	ReasonNoInternet          = "no internet"
	ReasonScheduledHardReboot = "scheduled hard reboot"
)

// RouterKind selects which gateway/router driver the factory builds (design
// §14): the reboot path is a device-adapter family, not a single client.
type RouterKind string

// RouterKindNokia is the only RouterKind gateway/router currently implements;
// ParseRouterKind also defaults an empty ROUTER_KIND to it.
const RouterKindNokia RouterKind = "nokia"

// ParseRouterKind normalizes s (trimmed, lowercased) into a RouterKind,
// defaulting an empty string to RouterKindNokia (ROUTER_KIND is optional;
// existing installs have no such key). Any other unrecognized value is an
// error rather than a silent fallback to nokia — a typo in ROUTER_KIND must
// surface at startup, not silently mis-drive a device.
func ParseRouterKind(s string) (RouterKind, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return RouterKindNokia, nil
	}

	switch RouterKind(s) {
	case RouterKindNokia:
		return RouterKindNokia, nil
	default:
		return "", fmt.Errorf("unknown ROUTER_KIND %q", s)
	}
}
