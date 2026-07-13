package infrastructure

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// uuidv7Pattern matches a canonical lowercase UUIDv7: version nibble 7,
// variant nibble one of 8/9/a/b (the RFC 9562 "10xx" variant bits).
var uuidv7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUIDv7(t *testing.T) {
	t.Parallel()

	t.Run("canonical lowercase shape with the version and variant bits set", func(t *testing.T) {
		t.Parallel()

		id, err := newUUIDv7(time.Now())
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}
		if len(id) != 36 {
			t.Fatalf("len(id) = %d, want 36", len(id))
		}
		if !uuidv7Pattern.MatchString(id) {
			t.Fatalf("id = %q, want it to match %s (version 7, variant 10)", id, uuidv7Pattern)
		}
	})

	t.Run("unique over many draws", func(t *testing.T) {
		t.Parallel()

		now := time.Now()
		const n = 5000
		seen := make(map[string]bool, n)
		for i := 0; i < n; i++ {
			id, err := newUUIDv7(now)
			if err != nil {
				t.Fatalf("newUUIDv7() error = %v", err)
			}
			if seen[id] {
				t.Fatalf("newUUIDv7() produced a duplicate: %s", id)
			}
			seen[id] = true
		}
	})

	// The remaining subtests each anchor on their own disjoint, far-future
	// timestamp range (year 2999, a different month per subtest) rather than
	// time.Now(): newUUIDv7's {lastMs, counter} state is shared package-wide,
	// so a subtest exercising "does the same ms increment the counter" or
	// "does an advancing ms reseed it" must not have its chosen millisecond
	// silently overtaken by a sibling t.Parallel() subtest's own calls (real
	// wall-clock calls elsewhere in this test can never exceed a year-2999
	// value, so these ranges are safe against that regardless of execution
	// order).

	t.Run("strictly increasing within the same millisecond", func(t *testing.T) {
		t.Parallel()

		ts := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
		prev, err := newUUIDv7(ts)
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}
		for i := 0; i < 1000; i++ {
			id, err := newUUIDv7(ts) // identical ts every call: forces the same-ms path
			if err != nil {
				t.Fatalf("newUUIDv7() error = %v", err)
			}
			if id <= prev {
				t.Fatalf("call %d = %q, want it to sort strictly after %q", i, id, prev)
			}
			prev = id
		}
	})

	t.Run("strictly increasing across increasing milliseconds", func(t *testing.T) {
		t.Parallel()

		base := time.Date(2999, 6, 1, 0, 0, 0, 0, time.UTC)
		prev, err := newUUIDv7(base)
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}
		for i := 1; i <= 1000; i++ {
			id, err := newUUIDv7(base.Add(time.Duration(i) * time.Millisecond))
			if err != nil {
				t.Fatalf("newUUIDv7() error = %v", err)
			}
			if id <= prev {
				t.Fatalf("call at +%dms = %q, want it to sort strictly after %q", i, id, prev)
			}
			prev = id
		}
	})

	t.Run("string sort order matches generation order", func(t *testing.T) {
		t.Parallel()

		base := time.Date(2999, 12, 1, 0, 0, 0, 0, time.UTC)
		var generated []string
		for i := 0; i < 200; i++ {
			// interleave same-ms bursts (mirroring several SaveMetrics rows
			// sharing one ts) with advancing ones (mirroring later, separately
			// timed rows), so the sort assertion below covers both transitions.
			ts := base
			if i%3 != 0 {
				ts = base.Add(time.Duration(i) * time.Millisecond)
			}
			id, err := newUUIDv7(ts)
			if err != nil {
				t.Fatalf("newUUIDv7() error = %v", err)
			}
			generated = append(generated, id)
		}

		sorted := append([]string(nil), generated...)
		sort.Strings(sorted)
		for i := range generated {
			if generated[i] != sorted[i] {
				t.Fatalf("generated[%d] = %q, sorted[%d] = %q: generation order must equal lexicographic sort order", i, generated[i], i, sorted[i])
			}
		}
	})

	// The remaining two subtests deliberately do NOT call t.Parallel(): Go
	// only starts a parent's t.Parallel() subtests once every t.Run call in
	// the parent has been issued, so a plain (non-parallel) subtest placed
	// anywhere among them always runs to completion first, in isolation —
	// which matters here because "counter overflow" reaches into the
	// package-level {uuidv7LastMs, uuidv7Counter} state directly.

	t.Run("counter overflow within one millisecond borrows a synthetic tick, never wraps", func(t *testing.T) {
		uuidv7Mu.Lock()
		savedLastMs, savedCounter := uuidv7LastMs, uuidv7Counter
		uuidv7Mu.Unlock()
		t.Cleanup(func() {
			uuidv7Mu.Lock()
			uuidv7LastMs, uuidv7Counter = savedLastMs, savedCounter
			uuidv7Mu.Unlock()
		})

		// force BOTH ends of the comparison from an explicit, known state —
		// never read prevID from ambient/leftover state first: under `go
		// test -count>1`, uuidv7LastMs persists across repeated runs of this
		// very test within the same process, so a prior iteration can leave
		// it past this subtest's anchor, silently making an
		// ambiently-generated "prevID" embed a *later* timestamp than the
		// forced one below and produce a false failure.
		anchor := time.Date(2999, 3, 1, 0, 0, 0, 0, time.UTC)
		forcedMs := anchor.UTC().UnixMilli()
		uuidv7Mu.Lock()
		uuidv7LastMs = forcedMs
		uuidv7Counter = maxUUIDv7Counter - 1
		uuidv7Mu.Unlock()

		// same ms, counter one below the ceiling: a normal same-ms increment
		// up to the ceiling.
		prevID, err := newUUIDv7(anchor)
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}

		// same ms again, counter now AT the ceiling: this call must borrow a
		// synthetic tick (forcedMs+1) rather than wrap the counter back to
		// 0, which would risk producing an id that does not sort after
		// prevID.
		got, err := newUUIDv7(anchor)
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}

		uuidv7Mu.Lock()
		newLastMs := uuidv7LastMs
		uuidv7Mu.Unlock()
		if newLastMs != forcedMs+1 {
			t.Fatalf("uuidv7LastMs after overflow = %d, want %d (a borrowed synthetic tick)", newLastMs, forcedMs+1)
		}
		if got <= prevID {
			t.Fatalf("newUUIDv7() after counter overflow = %q, want it to sort strictly after %q", got, prevID)
		}
	})

	t.Run("concurrent calls with the same ts never collide", func(t *testing.T) {
		// exercises the real concurrent path collectTelemetry relies on:
		// collectMetrics and collectPings mint ids in parallel goroutines,
		// both racing the same {uuidv7LastMs, uuidv7Counter} state.
		const goroutines = 50
		const perGoroutine = 200

		ts := time.Date(2999, 9, 9, 0, 0, 0, 0, time.UTC)

		var wg sync.WaitGroup
		ids := make([][]string, goroutines)
		errs := make([]error, goroutines)
		for g := range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := make([]string, perGoroutine)
				for i := range perGoroutine {
					id, err := newUUIDv7(ts)
					if err != nil {
						errs[g] = err
						return
					}
					local[i] = id
				}
				ids[g] = local
			}()
		}
		wg.Wait()

		for g, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d: newUUIDv7() error = %v", g, err)
			}
		}

		seen := make(map[string]bool, goroutines*perGoroutine)
		total := 0
		for _, local := range ids {
			for _, id := range local {
				if seen[id] {
					t.Fatalf("newUUIDv7() produced a duplicate under concurrent callers: %s", id)
				}
				seen[id] = true
				total++
			}
		}
		if total != goroutines*perGoroutine {
			t.Fatalf("total ids generated = %d, want %d", total, goroutines*perGoroutine)
		}
	})
}

func TestUuidv7MsAndCounter(t *testing.T) {
	// deliberately no t.Parallel() anywhere in this test: the round-trip
	// subtest below calls newUUIDv7 and depends on its own chosen ts being
	// embedded exactly, which a concurrent t.Parallel() subtest elsewhere
	// in the package (anchored at a later timestamp, and so free to push
	// the shared watermark past this one first) could silently break —
	// see TestSeedUUIDv7Watermark's file-level comment for why staying
	// fully sequential is what rules that out.

	t.Run("round-trips a freshly generated id's timestamp and counter", func(t *testing.T) {
		ts := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
		forceUUIDv7State(t, 0, 0) // see forceUUIDv7State: guarantees ts embeds exactly, not a leftover watermark
		id, err := newUUIDv7(ts)
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}

		gotMs, _, err := uuidv7MsAndCounter(id)
		if err != nil {
			t.Fatalf("uuidv7MsAndCounter(%q) error = %v", id, err)
		}
		if wantMs := ts.UnixMilli(); gotMs != wantMs {
			t.Fatalf("uuidv7MsAndCounter(%q) ms = %d, want %d", id, gotMs, wantMs)
		}
	})

	t.Run("decodes a hand-built id's timestamp and counter exactly", func(t *testing.T) {
		// hand-built, not generated: ms = 0x000102030405, counter = 0x0abc
		// (version nibble 7 occupies the top 4 bits of that group), the
		// remaining bytes irrelevant to this decode.
		const id = "00010203-0405-7abc-8000-000000000000"

		gotMs, gotCounter, err := uuidv7MsAndCounter(id)
		if err != nil {
			t.Fatalf("uuidv7MsAndCounter(%q) error = %v", id, err)
		}
		if wantMs := int64(0x000102030405); gotMs != wantMs {
			t.Fatalf("uuidv7MsAndCounter(%q) ms = %#x, want %#x", id, gotMs, wantMs)
		}
		if wantCounter := uint16(0x0abc); gotCounter != wantCounter {
			t.Fatalf("uuidv7MsAndCounter(%q) counter = %#x, want %#x", id, gotCounter, wantCounter)
		}
	})

	t.Run("rejects malformed input", func(t *testing.T) {
		for _, id := range []string{
			"",
			"not-a-uuid",
			"00010203-0405-7abc-8000-00000000000",   // one hex digit short
			"00010203-0405-7abc-8000-0000000000000", // one hex digit long
			"0g010203-0405-7abc-8000-000000000000",  // non-hex digit in the timestamp
			"00010203-0405-7abg-8000-000000000000",  // non-hex digit in the counter
		} {
			if _, _, err := uuidv7MsAndCounter(id); err == nil {
				t.Errorf("uuidv7MsAndCounter(%q) error = nil, want an error", id)
			}
		}
	})
}

func TestUuidv7Floor(t *testing.T) {
	// deliberately no t.Parallel() anywhere in this test: see
	// TestUuidv7MsAndCounter's file-level comment — the two newUUIDv7-
	// calling subtests below need their own chosen ts to land exactly,
	// which only staying fully sequential guarantees.

	t.Run("matches the canonical UUID shape with counter and rand_b zeroed", func(t *testing.T) {
		// a pure function of its input (no shared state), so time.Now() is
		// safe here — unlike newUUIDv7, there is no package-level watermark
		// for a concurrent call to interfere with.
		got := uuidv7Floor(time.Now())
		if !uuidv7Pattern.MatchString(got) {
			t.Fatalf("uuidv7Floor() = %q, want it to match %s", got, uuidv7Pattern)
		}
		if !strings.HasSuffix(got, "-8000-000000000000") {
			t.Fatalf("uuidv7Floor() = %q, want a zeroed counter/rand_b suffix", got)
		}
	})

	t.Run("is a floor: at or below every id minted at or after since", func(t *testing.T) {
		since := time.Date(3001, 3, 1, 0, 0, 0, 0, time.UTC)
		floor := uuidv7Floor(since)

		for i, ts := range []time.Time{since, since.Add(time.Millisecond), since.Add(time.Hour)} {
			forceUUIDv7State(t, 0, 0) // see forceUUIDv7State: guarantees ts embeds exactly, not a leftover watermark
			id, err := newUUIDv7(ts)
			if err != nil {
				t.Fatalf("newUUIDv7() error = %v", err)
			}
			if id < floor {
				t.Fatalf("case %d: newUUIDv7(%v) = %q, want it to sort at or after uuidv7Floor(since) = %q", i, ts, id, floor)
			}
		}
	})

	t.Run("sorts strictly above an id minted before since", func(t *testing.T) {
		since := time.Date(3002, 3, 1, 0, 0, 0, 0, time.UTC)
		floor := uuidv7Floor(since)

		forceUUIDv7State(t, 0, 0) // see forceUUIDv7State: guarantees the mint below embeds exactly, not a leftover watermark
		id, err := newUUIDv7(since.Add(-time.Millisecond))
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}
		if id >= floor {
			t.Fatalf("newUUIDv7(before since) = %q, want it to sort strictly before uuidv7Floor(since) = %q", id, floor)
		}
	})
}

func TestSeedUUIDv7Watermark(t *testing.T) {
	// deliberately no t.Parallel() anywhere in this test (function or
	// subtests): every subtest drives the shared package-level
	// {uuidv7LastMs, uuidv7Counter} state directly through known before/after
	// values, and Go only starts other tests' t.Parallel() subtests once
	// every top-level non-parallel test — this one included — has run to
	// completion, so staying fully sequential is what keeps this test's
	// forced states from racing anyone else's concurrent newUUIDv7 calls.

	set := func(ms int64, counter uint16) {
		uuidv7Mu.Lock()
		uuidv7LastMs, uuidv7Counter = ms, counter
		uuidv7Mu.Unlock()
	}
	get := func() (int64, uint16) {
		uuidv7Mu.Lock()
		defer uuidv7Mu.Unlock()
		return uuidv7LastMs, uuidv7Counter
	}

	savedMs, savedCounter := get()
	t.Cleanup(func() { set(savedMs, savedCounter) })

	t.Run("a strictly greater ms raises both lastMs and counter to the seeded values", func(t *testing.T) {
		set(1000, 5)
		seedUUIDv7Watermark(2000, 42)
		if gotMs, gotCounter := get(); gotMs != 2000 || gotCounter != 42 {
			t.Fatalf("after seed = (%d, %d), want (2000, 42)", gotMs, gotCounter)
		}
	})

	t.Run("an equal ms with a greater counter raises only the counter", func(t *testing.T) {
		set(1000, 5)
		seedUUIDv7Watermark(1000, 42)
		if gotMs, gotCounter := get(); gotMs != 1000 || gotCounter != 42 {
			t.Fatalf("after seed = (%d, %d), want (1000, 42)", gotMs, gotCounter)
		}
	})

	t.Run("an equal ms with a lesser or equal counter is a no-op", func(t *testing.T) {
		for _, counter := range []uint16{5, 4, 0} {
			set(1000, 5)
			seedUUIDv7Watermark(1000, counter)
			if gotMs, gotCounter := get(); gotMs != 1000 || gotCounter != 5 {
				t.Fatalf("seedUUIDv7Watermark(1000, %d) after (1000, 5) = (%d, %d), want unchanged (1000, 5)", counter, gotMs, gotCounter)
			}
		}
	})

	t.Run("a lesser ms is a no-op regardless of counter", func(t *testing.T) {
		set(1000, 5)
		seedUUIDv7Watermark(999, maxUUIDv7Counter)
		if gotMs, gotCounter := get(); gotMs != 1000 || gotCounter != 5 {
			t.Fatalf("after seed with a lesser ms = (%d, %d), want unchanged (1000, 5)", gotMs, gotCounter)
		}
	})

	t.Run("seeding from a stored id guarantees the next same-instant mint sorts strictly above it", func(t *testing.T) {
		// this is the actual property issue #4 FIX 1 exists for, and the
		// one a naive "reset counter to 0" seed gets wrong: the stored id's
		// own counter came from crypto/rand (randCounterSeed), so it can be
		// anywhere in [0, maxUUIDv7Counter]. Seeding to 0 and incrementing
		// would only sort above roughly half of that range; seeding to the
		// stored counter itself and incrementing is unconditionally above
		// it, and even the counter-exhausted case (stored counter already
		// at the ceiling) still resolves correctly via the same borrowed-
		// synthetic-tick path nextUUIDv7MsAndCounter already uses.
		anchor := time.Date(2998, 1, 1, 0, 0, 0, 0, time.UTC)

		set(0, 0) // simulates the writing process's own state, starting blank
		stored, err := newUUIDv7(anchor)
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}
		storedMs, storedCounter, err := uuidv7MsAndCounter(stored)
		if err != nil {
			t.Fatalf("uuidv7MsAndCounter(%q) error = %v", stored, err)
		}

		set(0, 0) // simulates a fresh process: shared state starts blank again
		seedUUIDv7Watermark(storedMs, storedCounter)

		got, err := newUUIDv7(anchor) // the fresh process's first mint, same instant
		if err != nil {
			t.Fatalf("newUUIDv7() error = %v", err)
		}
		if got <= stored {
			t.Fatalf("newUUIDv7() after seeding from %q = %q, want it to sort strictly after", stored, got)
		}
	})
}

// forceUUIDv7State sets the shared {uuidv7LastMs, uuidv7Counter} watermark
// to (ms, counter), restoring whatever was there before once t completes.
// Tests that need newUUIDv7 to embed an exact, known timestamp call this
// first with a baseline safely below anything they are about to mint —
// otherwise the shared state left over from an earlier subtest, or (under
// `go test -count>1`) an earlier repetition of the very same test within
// one process, could already sit at or above the chosen timestamp and
// silently embed that leftover value instead.
func forceUUIDv7State(t *testing.T, ms int64, counter uint16) {
	t.Helper()
	uuidv7Mu.Lock()
	savedMs, savedCounter := uuidv7LastMs, uuidv7Counter
	uuidv7LastMs, uuidv7Counter = ms, counter
	uuidv7Mu.Unlock()
	t.Cleanup(func() {
		uuidv7Mu.Lock()
		uuidv7LastMs, uuidv7Counter = savedMs, savedCounter
		uuidv7Mu.Unlock()
	})
}
