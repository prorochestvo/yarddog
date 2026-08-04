package domain

import "time"

// CheckKindIP, CheckKindDomain, and CheckKindGateway are the values allowed
// in checks.kind (design §4, §9).
const (
	CheckKindIP      = "ip"
	CheckKindDomain  = "domain"
	CheckKindGateway = "gateway"
)

// StateUp, StateDegraded, and StateDown are the tri-state verdict Verdict
// produces (issue #6). The two failure states are deliberately not the same
// event: StateDown means the raw link is gone and a router reboot is on the
// table, while StateDegraded means only the name-resolution/HTTPS path broke
// while raw connectivity provably held — recorded and announced, never
// rebooted.
const (
	StateUp       = "up"
	StateDegraded = "degraded"
	StateDown     = "down"
)

// Verdict grades one round of probes into StateUp, StateDegraded, or StateDown
// (issue #6, superseding design §4.2's single "all ip OR all domain failed"
// rule). Only a fully failed ip group — raw TCP dials to IP literals, which
// need no DNS — is StateDown. A fully failed domain group while any ip target
// still connects is StateDegraded: both domain probes are HTTPS GETs sharing
// one DNS path (in production, through the very router this watchdog reboots),
// so they fail together as a matter of course, and 28 of 29 historical "down"
// verdicts were exactly that with the ip group answering in 19-1118ms.
//
// When no ip targets are configured at all, the domain group is the only
// evidence there is and a fully failed one is StateDown rather than a verdict
// nothing could ever act on. A single failed target within a group never trips
// anything; an empty group is "not applicable", never vacuously failed.
func Verdict(targets []TargetResult) Result {
	var ip, dom []TargetResult
	for _, t := range targets {
		switch t.Kind {
		case CheckKindIP:
			ip = append(ip, t)
		case CheckKindDomain:
			dom = append(dom, t)
		}
	}

	state := StateUp
	switch {
	case allFailed(ip):
		state = StateDown
	case allFailed(dom) && len(ip) == 0:
		state = StateDown
	case allFailed(dom):
		state = StateDegraded
	}

	return Result{State: state, Targets: targets}
}

// Result is the outcome of one connectivity check: every probed target's
// individual result plus the aggregate verdict (design §4.2, issue #6).
type Result struct {
	State   string
	Targets []TargetResult
}

// IsDegraded reports whether the application-layer path is broken while raw
// connectivity holds — worth recording and announcing, never worth a reboot.
func (r Result) IsDegraded() bool { return r.State == StateDegraded }

// IsDown reports whether the raw link is gone, the only verdict a reboot may
// ever follow from.
func (r Result) IsDown() bool { return r.State == StateDown }

// TargetResult is one probed target's outcome, shaped to map directly onto a
// checks row (design §9) once the caller supplies RunID and Phase.
type TargetResult struct {
	Target  string
	Kind    string
	OK      bool
	Latency time.Duration
	Error   string
}

// allFailed reports whether every result in a group failed. An empty group
// returns false rather than the vacuous true it would otherwise compute, so
// a misconfigured empty target list cannot force a permanent down verdict
// (design §4.2 pitfall).
func allFailed(results []TargetResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.OK {
			return false
		}
	}
	return true
}
