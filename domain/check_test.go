package domain

import "testing"

func TestVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		targets []TargetResult
		want    string
	}{
		{
			name: "one down target among several is not a failure verdict",
			targets: []TargetResult{
				{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: true},
				{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: false},
				{Target: "https://example.com/generate_204", Kind: CheckKindDomain, OK: true},
			},
			want: StateUp,
		},
		{
			name: "all ip targets down is a down verdict regardless of domains",
			targets: []TargetResult{
				{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: false},
				{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: false},
				{Target: "https://example.com/generate_204", Kind: CheckKindDomain, OK: true},
			},
			want: StateDown,
		},
		{
			name: "every target down is a down verdict",
			targets: []TargetResult{
				{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: false},
				{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: false},
				{Target: "https://a.example/", Kind: CheckKindDomain, OK: false},
			},
			want: StateDown,
		},
		{
			name: "ip up but all domains down is degraded, not down (issue #6)",
			targets: []TargetResult{
				{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: true},
				{Target: "https://a.example/", Kind: CheckKindDomain, OK: false},
				{Target: "https://b.example/", Kind: CheckKindDomain, OK: false},
			},
			want: StateDegraded,
		},
		{
			name: "one ip up among failures still keeps a domain wipeout at degraded",
			targets: []TargetResult{
				{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: false},
				{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: true},
				{Target: "https://a.example/", Kind: CheckKindDomain, OK: false},
				{Target: "https://b.example/", Kind: CheckKindDomain, OK: false},
			},
			want: StateDegraded,
		},
		{
			name: "with no ip targets configured, all domains down is the only evidence there is",
			targets: []TargetResult{
				{Target: "https://a.example/", Kind: CheckKindDomain, OK: false},
				{Target: "https://b.example/", Kind: CheckKindDomain, OK: false},
			},
			want: StateDown,
		},
		{
			name: "at least one ip and one domain up is an up verdict",
			targets: []TargetResult{
				{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: true},
				{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: false},
				{Target: "https://a.example/", Kind: CheckKindDomain, OK: true},
				{Target: "https://b.example/", Kind: CheckKindDomain, OK: false},
			},
			want: StateUp,
		},
		{
			name:    "empty group is not applicable, never vacuously down",
			targets: nil,
			want:    StateUp,
		},
		{
			name: "a gateway probe alone decides nothing",
			targets: []TargetResult{
				{Target: "192.168.1.1:80", Kind: CheckKindGateway, OK: false},
			},
			want: StateUp,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got := Verdict(c.targets)

			if got.State != c.want {
				t.Fatalf("State = %q, want %q: %+v", got.State, c.want, got.Targets)
			}
			if got.IsDown() != (c.want == StateDown) {
				t.Fatalf("IsDown() = %v, want %v", got.IsDown(), c.want == StateDown)
			}
			if got.IsDegraded() != (c.want == StateDegraded) {
				t.Fatalf("IsDegraded() = %v, want %v", got.IsDegraded(), c.want == StateDegraded)
			}
		})
	}

	t.Run("targets slice is carried through unmodified", func(t *testing.T) {
		t.Parallel()

		targets := []TargetResult{
			{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: true},
		}

		got := Verdict(targets)

		if len(got.Targets) != 1 || got.Targets[0].Target != "1.1.1.1:443" {
			t.Fatalf("Targets = %+v, want the input echoed back", got.Targets)
		}
	})
}
