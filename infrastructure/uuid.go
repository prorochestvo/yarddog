package infrastructure

import (
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// newUUIDv7 generates a canonical lowercase UUIDv7 (RFC 9562) string id
// stamped with ts — the row's own timestamp (InsertRun's startedAt,
// InsertCheck's c.TS, SaveMetrics's/SavePings's ts), never time.Now(): an
// id's embedded timestamp must match the moment the row itself records, not
// the moment the process happened to reach this call. Canonical lowercase
// hex sorts lexicographically exactly as it sorts chronologically, so every
// ORDER BY id / MAX(id) query in store.go returns newest-first / newest-run
// correctly now that ids are UUIDv7 strings instead of an autoincrement
// integer — provided the shared {lastMs, counter} state below is itself
// monotonic (see the cross-process paragraph).
//
// Monotonic within a millisecond: several rows can share one ts (every
// sample SaveMetrics writes in a single call takes the same ts parameter),
// and they must still sort in call order so ORDER BY id preserves insertion
// order (ListChecksByRun, ListMetricsByRun, and scanMetricRecords's callers
// all depend on this). The package-level {lastMs, counter} pair guards this:
// a call whose millisecond does not exceed the last one seen increments
// counter and embeds it as rand_a (the 12 bits immediately after the
// version nibble — RFC 9562 §6.2's "Fixed-Length Dedicated Counter"
// method); a call whose millisecond advances reseeds counter from
// crypto/rand instead, so ids minted in different milliseconds don't leak a
// predictable sequence. Because a lexicographic byte compare is decided by
// the first differing byte, a strictly increasing (ms, counter) prefix
// alone guarantees strictly increasing ids regardless of what follows —
// rand_b (the remaining 62 bits) is freshly drawn from crypto/rand on every
// call and never influences ordering. If counter would overflow its 12
// bits within one millisecond (not observed at this application's scale —
// SaveMetrics writes at most a handful of rows per call), the shared state
// advances by one synthetic millisecond instead of wrapping the counter,
// which would silently break the ordering guarantee; wall-clock time simply
// catches back up once it passes that borrowed millisecond.
//
// Monotonic across processes rests on the DB-seeded watermark, not on the
// wall clock (issue #4 FIX 1): yarddog is a cron-fired process that starts
// this shared state fresh — lastMs/counter both zero — on every invocation,
// so without help a Pi whose wall clock has stepped backward (no RTC, no
// battery, no NTP — precisely the outage scenario this watchdog exists to
// recover from) would mint an id that sorts BELOW every row already stored,
// corrupting every "newest" query from that point on. OpenStore closes that
// gap by calling seedUUIDv7Watermark with the newest id already on disk
// (seedUUIDv7WatermarkFromDB, store.go) before returning the *Store to any
// caller, so the very first newUUIDv7 call in a fresh process compares
// against the database's own history rather than a bare, possibly-regressed
// time.Now(). A single collector instance (the flock) is what keeps this
// safe: two processes seeding and minting concurrently would still be able
// to race each other.
func newUUIDv7(ts time.Time) (string, error) {
	ms, counter, err := nextUUIDv7MsAndCounter(ts)
	if err != nil {
		return "", err
	}

	var b [16]byte
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = 0x70 | byte(counter>>8&0x0F) // version 0111 + counter's high 4 bits
	b[7] = byte(counter)                // counter's low 8 bits

	randB := make([]byte, 8)
	if _, err := uuidv7RandRead(randB); err != nil {
		return "", fmt.Errorf("uuidv7: read random bits: %w", err)
	}
	b[8] = 0x80 | (randB[0] & 0x3F) // variant 10 + 6 bits of rand_b
	copy(b[9:], randB[1:])          // remaining 56 bits of rand_b

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// maxUUIDv7Counter is the largest value the 12-bit rand_a counter can hold.
const maxUUIDv7Counter = 0x0FFF

// uuidv7Mu guards uuidv7LastMs/uuidv7Counter, the shared monotonic state
// every newUUIDv7 call advances. A single mutex is cheap here: the
// collector is a single-connection, single-writer process (an invariant
// this whole feature already depends on), so there is no real contention to
// optimize away.
var (
	uuidv7Mu      sync.Mutex
	uuidv7LastMs  int64
	uuidv7Counter uint16
)

// uuidv7RandRead is crypto/rand.Read behind a seam: a test overrides it to
// force an entropy-source failure and assert the wrapped error actually
// surfaces all the way through a caller (e.g. InsertRun returning ("", err))
// instead of a silently empty or zero-valued id.
var uuidv7RandRead = rand.Read

// nextUUIDv7MsAndCounter advances the shared monotonic state under
// uuidv7Mu and returns the (ms, counter) pair newUUIDv7 should embed. It is
// split out so the critical section holds nothing but integer arithmetic
// and, on a millisecond rollover or counter exhaustion, one crypto/rand
// draw to reseed the counter.
func nextUUIDv7MsAndCounter(ts time.Time) (int64, uint16, error) {
	ms := ts.UTC().UnixMilli()

	uuidv7Mu.Lock()
	defer uuidv7Mu.Unlock()

	switch {
	case ms > uuidv7LastMs:
		uuidv7LastMs = ms
	case uuidv7Counter < maxUUIDv7Counter:
		uuidv7Counter++
		return uuidv7LastMs, uuidv7Counter, nil
	default:
		// counter exhausted within this millisecond (never observed at this
		// application's scale): borrow a synthetic tick rather than wrap the
		// counter and silently violate the strictly-increasing guarantee.
		uuidv7LastMs++
	}

	seed, err := randCounterSeed()
	if err != nil {
		return 0, 0, err
	}
	uuidv7Counter = seed
	return uuidv7LastMs, uuidv7Counter, nil
}

// randCounterSeed draws a fresh 12-bit counter seed from crypto/rand for a
// millisecond newUUIDv7 has not embedded an id in yet.
func randCounterSeed() (uint16, error) {
	var b [2]byte
	if _, err := uuidv7RandRead(b[:]); err != nil {
		return 0, fmt.Errorf("uuidv7: read counter seed: %w", err)
	}
	return (uint16(b[0])<<8 | uint16(b[1])) & maxUUIDv7Counter, nil
}

// seedUUIDv7Watermark advances the shared monotonic state to at least
// (ms, counter) — the exact pair decoded from an id already on disk
// (issue #4 FIX 1): OpenStore calls this once at startup, before returning
// the *Store to any caller, with the newest id's own embedded (ms, counter)
// (store.go's seedUUIDv7WatermarkFromDB, uuidv7MsAndCounter) — see
// newUUIDv7's "Monotonic across processes" doc for why this closes the
// backward-clock gap a bare package-level zero value cannot. The comparison
// is lexicographic on the (ms, counter) pair, matching how the ids
// themselves sort: a strictly greater ms wins outright and resets counter to
// the stored id's own value (not 0 — see below); an equal ms only raises
// counter, never lowers it; a lesser ms is a no-op. Seeding counter to the
// stored id's own value, not 0, is load-bearing: if the very next newUUIDv7
// call in this process lands at this exact ms (the realistic case — a cron
// process mints its first id right after seeding, with no elapsed time to
// advance the counter further), nextUUIDv7MsAndCounter increments from
// here, so the new id's counter is the stored one plus one — guaranteed
// greater. Seeding to 0 instead would race the stored id's own
// crypto/rand-drawn counter (averaging half the 12-bit range) and lose
// about as often as it won.
func seedUUIDv7Watermark(ms int64, counter uint16) {
	uuidv7Mu.Lock()
	defer uuidv7Mu.Unlock()
	switch {
	case ms > uuidv7LastMs:
		uuidv7LastMs = ms
		uuidv7Counter = counter
	case ms == uuidv7LastMs && counter > uuidv7Counter:
		uuidv7Counter = counter
	}
}

// uuidv7Floor returns the smallest possible canonical UUIDv7 string whose
// embedded timestamp is since's millisecond: the same byte layout newUUIDv7
// writes, with counter and rand_b forced to zero instead of drawn. It gives
// a history query a sargable lower bound — "id >= uuidv7Floor(since)" — that
// SQLite can seek to directly on the id index, rather than "ts >= ?" alone,
// which is only a residual filter checked row-by-row during an id-ordered
// scan and so cannot stop that scan early (issue #4 FIX 2). It is a floor,
// not an exact match: callers keep the precise "ts >= ?" condition alongside
// it, since truncating since to millisecond precision here can make the
// floor slightly less restrictive than the real cutoff.
func uuidv7Floor(since time.Time) string {
	ms := since.UTC().UnixMilli()

	var b [16]byte
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = 0x70 // version 0111, counter's high 4 bits forced to zero
	b[7] = 0x00 // counter's low 8 bits forced to zero
	b[8] = 0x80 // variant 10, rand_b's top 6 bits forced to zero
	// b[9:16] left at zero — the rest of rand_b.

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// uuidv7MsAndCounter decodes the 48-bit big-endian Unix-ms timestamp and the
// 12-bit counter embedded in a canonical UUIDv7 string id — the inverse of
// newUUIDv7's encoding — so seedUUIDv7WatermarkFromDB can recover a full
// watermark from an id already on disk without storing either value
// separately. Both are needed, not just ms: seedUUIDv7Watermark must be
// able to resume the counter from exactly where the stored id left off.
func uuidv7MsAndCounter(id string) (ms int64, counter uint16, err error) {
	hexDigits := strings.ReplaceAll(id, "-", "")
	if len(hexDigits) != 32 {
		return 0, 0, fmt.Errorf("uuidv7: %q is not a canonical UUID (want 32 hex digits, got %d)", id, len(hexDigits))
	}

	ms, err = strconv.ParseInt(hexDigits[:12], 16, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("uuidv7: parse timestamp from %q: %w", id, err)
	}

	// hex digits [12:16) are b[6] (version nibble 0111 + counter's high 4
	// bits) and b[7] (counter's low 8 bits); masking off the fixed version
	// nibble recovers the 12-bit counter newUUIDv7 embedded.
	rawCounter, err := strconv.ParseUint(hexDigits[12:16], 16, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("uuidv7: parse counter from %q: %w", id, err)
	}

	return ms, uint16(rawCounter) & maxUUIDv7Counter, nil
}
