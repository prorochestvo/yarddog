package domain

import "testing"

func TestVerdict(t *testing.T) {
	t.Parallel()

	t.Run("one down target among several is not a down verdict", func(t *testing.T) {
		t.Parallel()

		targets := []TargetResult{
			{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: true},
			{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: false},
			{Target: "https://example.com/generate_204", Kind: CheckKindDomain, OK: true},
		}

		got := Verdict(targets)

		if got.Down {
			t.Fatalf("Down = true, want false (only one ip target failed): %+v", got.Targets)
		}
	})

	t.Run("all ip targets down is a down verdict regardless of domains", func(t *testing.T) {
		t.Parallel()

		targets := []TargetResult{
			{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: false},
			{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: false},
			{Target: "https://example.com/generate_204", Kind: CheckKindDomain, OK: true},
		}

		got := Verdict(targets)

		if !got.Down {
			t.Fatalf("Down = false, want true (all ip targets failed): %+v", got.Targets)
		}
	})

	t.Run("ip up but all domains down is a down verdict", func(t *testing.T) {
		t.Parallel()

		targets := []TargetResult{
			{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: true},
			{Target: "https://a.example/", Kind: CheckKindDomain, OK: false},
			{Target: "https://b.example/", Kind: CheckKindDomain, OK: false},
		}

		got := Verdict(targets)

		if !got.Down {
			t.Fatalf("Down = false, want true (all domain targets failed): %+v", got.Targets)
		}
	})

	t.Run("at least one ip and one domain up is an up verdict", func(t *testing.T) {
		t.Parallel()

		targets := []TargetResult{
			{Target: "1.1.1.1:443", Kind: CheckKindIP, OK: true},
			{Target: "8.8.8.8:53", Kind: CheckKindIP, OK: false},
			{Target: "https://a.example/", Kind: CheckKindDomain, OK: true},
			{Target: "https://b.example/", Kind: CheckKindDomain, OK: false},
		}

		got := Verdict(targets)

		if got.Down {
			t.Fatalf("Down = true, want false: %+v", got.Targets)
		}
	})

	t.Run("empty group is not applicable, never vacuously down", func(t *testing.T) {
		t.Parallel()

		got := Verdict(nil)

		if got.Down {
			t.Fatal("Down = true, want false: an empty target list must never be a vacuous down verdict")
		}
	})

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
