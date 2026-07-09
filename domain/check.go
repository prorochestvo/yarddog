package domain

import "time"

// CheckKindIP, CheckKindDomain, and CheckKindGateway are the values allowed
// in checks.kind (design §4, §9).
const (
	CheckKindIP      = "ip"
	CheckKindDomain  = "domain"
	CheckKindGateway = "gateway"
)

// Verdict applies the conservative down rule (design §4.2): the internet is down when
// every ip target fails OR every domain target fails. A single failed target within a
// group never trips it; an empty group is "not applicable", never vacuously down.
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
	return Result{Down: allFailed(ip) || allFailed(dom), Targets: targets}
}

// Result is the outcome of one connectivity check: every probed target's
// individual result plus the aggregate quorum verdict (design §4.2).
type Result struct {
	Down    bool
	Targets []TargetResult
}

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
