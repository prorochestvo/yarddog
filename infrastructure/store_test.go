package infrastructure

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"
)

func TestNewStore(t *testing.T) {
	t.Run("creates the schema", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		if _, err := s.InsertRun(t.Context(), time.Now(), "soft", nil); err != nil {
			t.Fatalf("InsertRun() after NewStore error = %v, want schema already applied", err)
		}
	})

	t.Run("reopening the same database is idempotent", func(t *testing.T) {
		t.Parallel()

		// :memory: is per-connection, so a genuine reopen needs a file on
		// disk — that is the only way to exercise "CREATE TABLE IF NOT
		// EXISTS against an already-migrated schema" for real.
		path := filepath.Join(t.TempDir(), "yarddog.db")

		first, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() first open error = %v", err)
		}
		insertedID, err := first.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err != nil {
			t.Fatalf("InsertRun() on first open error = %v", err)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("db file mode = %v, want 0600 (it holds router credentials' error strings)", perm)
		}

		if err := first.Close(); err != nil {
			t.Fatalf("Close() first handle error = %v", err)
		}

		second, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() second open error = %v, want idempotent migration", err)
		}
		t.Cleanup(func() {
			if err := second.Close(); err != nil {
				t.Errorf("Close() second handle error = %v", err)
			}
		})

		run, err := second.GetRun(t.Context(), insertedID)
		if err != nil {
			t.Fatalf("GetRun(%s) after reopen error = %v, want the row inserted before reopen", insertedID, err)
		}
		if run.Mode != "soft" {
			t.Fatalf("GetRun(1).Mode = %q, want %q", run.Mode, "soft")
		}

		// a database already stamped with the current schema version must
		// never be quarantined (issue #4 FIX 3's needsQuarantine no-op
		// path): the original file stays the one live store, nothing gets
		// set aside.
		quarantined, err := filepath.Glob(path + ".*-*")
		if err != nil {
			t.Fatalf("Glob() error = %v", err)
		}
		if len(quarantined) != 0 {
			t.Fatalf("quarantined files matching %q = %v, want none for an already-current schema", path+".*-*", quarantined)
		}
	})
}

func TestOpenStore(t *testing.T) {
	t.Run("non-positive pool size is clamped to one, not an error", func(t *testing.T) {
		t.Parallel()

		for _, n := range []int{0, -5} {
			s, err := OpenStore(t.Context(), ":memory:", n, true)
			if err != nil {
				t.Fatalf("OpenStore(_, _, %d, true) error = %v, want the pool size floored to 1", n, err)
			}
			if _, err := s.InsertRun(t.Context(), time.Now(), "soft", nil); err != nil {
				t.Errorf("InsertRun() after OpenStore(_, _, %d) error = %v", n, err)
			}
			if err := s.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		}
	})

	t.Run("a larger pool size against :memory: still opens and migrates", func(t *testing.T) {
		t.Parallel()

		// production only ever pairs maxOpenConns>1 with a file-backed
		// database (see OpenStore's doc); this only proves the floor logic
		// doesn't reject a larger value outright.
		s, err := OpenStore(t.Context(), ":memory:", 8, true)
		if err != nil {
			t.Fatalf("OpenStore(_, _, 8, true) error = %v", err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})

		if _, err := s.InsertRun(t.Context(), time.Now(), "soft", nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
	})

	t.Run("reopening after a wall-clock rollback still mints ids that sort above what is already stored", func(t *testing.T) {
		t.Parallel()

		// issue #4 FIX 1: yarddog is a cron process, so newUUIDv7's shared
		// monotonic state starts blank on every single invocation. Without
		// a seeded watermark, a Pi whose wall clock has stepped backward
		// (no RTC, no battery, no NTP — precisely the outage scenario
		// yarddog exists to recover from) would mint an id that sorts
		// BELOW every row already on disk, corrupting every "newest run"
		// query from that point on. This reproduces exactly that: T2 is
		// written first with a LATER started_at, that store is closed, and
		// a fresh store reopened on the same file then writes T1 with an
		// EARLIER started_at — simulating the rollback across a restart.
		path := filepath.Join(t.TempDir(), "yarddog.db")

		first, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() first open error = %v", err)
		}
		laterWallClock := time.Now().UTC().Truncate(time.Second)
		t2ID, err := first.InsertRun(t.Context(), laterWallClock, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun(T2) error = %v", err)
		}
		if err := first.Close(); err != nil {
			t.Fatalf("Close() first handle error = %v", err)
		}

		second, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() second open error = %v", err)
		}
		t.Cleanup(func() {
			if err := second.Close(); err != nil {
				t.Errorf("Close() second handle error = %v", err)
			}
		})

		earlierWallClock := laterWallClock.Add(-24 * time.Hour) // the rolled-back clock
		t1ID, err := second.InsertRun(t.Context(), earlierWallClock, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun(T1) error = %v", err)
		}

		if t1ID <= t2ID {
			t.Fatalf("T1's id = %s, T2's id = %s: want T1 to sort strictly above T2 despite T1's earlier started_at (issue #4 FIX 1)", t1ID, t2ID)
		}

		newest, err := second.ListRuns(t.Context(), 1)
		if err != nil {
			t.Fatalf("ListRuns() error = %v", err)
		}
		if len(newest) != 1 {
			t.Fatalf("ListRuns(limit=1) = %d rows, want 1", len(newest))
		}
		if newest[0].ID != t1ID {
			t.Fatalf("ListRuns(limit=1)[0].ID = %s, want %s (T1, the most recently written run, despite its earlier started_at)", newest[0].ID, t1ID)
		}
	})

	t.Run("a pre-issue-4 database is quarantined and replaced by a fresh, working store", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "yarddog.db")

		// simulate a database written before issue #4: an INTEGER-
		// autoincrement runs table with a real row already in it, and no
		// meta table at all. migrate's own CREATE TABLE IF NOT EXISTS
		// leaves this runs table untouched (it already exists) but creates
		// meta fresh and empty alongside it — the exact ambiguity
		// needsQuarantine's non-empty-runs check exists to resolve, since
		// "no schema_version row" alone can't tell this apart from a
		// genuinely fresh database.
		oldDB, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open pre-issue-4 db: %v", err)
		}
		if _, err := oldDB.ExecContext(t.Context(), `CREATE TABLE runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			started_at TEXT NOT NULL,
			mode TEXT NOT NULL
		)`); err != nil {
			t.Fatalf("create pre-issue-4 runs table: %v", err)
		}
		if _, err := oldDB.ExecContext(t.Context(), `INSERT INTO runs (started_at, mode) VALUES (?, ?)`,
			formatTime(time.Now()), "soft"); err != nil {
			t.Fatalf("seed pre-issue-4 run: %v", err)
		}
		if err := oldDB.Close(); err != nil {
			t.Fatalf("close pre-issue-4 db: %v", err)
		}

		s, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() on a pre-issue-4 database error = %v, want it to self-heal instead", err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})

		// the old file is set aside, never deleted...
		quarantined, err := filepath.Glob(path + ".incompatible-*")
		if err != nil {
			t.Fatalf("Glob() error = %v", err)
		}
		if len(quarantined) != 1 {
			t.Fatalf("quarantined files matching %q = %v, want exactly 1", path+".incompatible-*", quarantined)
		}

		// ...and the fresh store at path actually works: a UUIDv7 string
		// id, not the "datatype mismatch" an un-wiped old INTEGER schema
		// would otherwise fail with forever.
		id, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() on the fresh, post-quarantine store error = %v, want it to self-heal, not brick", err)
		}
		if !uuidv7Pattern.MatchString(id) {
			t.Fatalf("InsertRun() id = %q, want a canonical UUIDv7", id)
		}

		got, err := s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun(%s) error = %v, want the fresh store to read back what it just wrote", id, err)
		}
		if got.Mode != domain.ModeSoft {
			t.Fatalf("GetRun(%s).Mode = %q, want %q", id, got.Mode, domain.ModeSoft)
		}
	})

	t.Run("a corrupt or non-database file is quarantined and replaced by a fresh, working store", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "yarddog.db")

		// a file that is not a SQLite database at all: opening it fails
		// with SQLITE_NOTADB (empirically confirmed: "file is not a
		// database (26)" from the very first PRAGMA in migrate()) — the
		// other isSQLiteCorruptionError trigger besides SQLITE_CORRUPT
		// (issue #4 FIX 3).
		if err := os.WriteFile(path, []byte("not a sqlite database, just garbage bytes"), 0600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		s, err := NewStore(t.Context(), path)
		if err != nil {
			t.Fatalf("NewStore() on a non-database file error = %v, want it to self-heal instead", err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})

		quarantined, err := filepath.Glob(path + ".corrupt-*")
		if err != nil {
			t.Fatalf("Glob() error = %v", err)
		}
		if len(quarantined) != 1 {
			t.Fatalf("quarantined files matching %q = %v, want exactly 1", path+".corrupt-*", quarantined)
		}

		id, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() on the fresh, post-quarantine store error = %v, want it to self-heal, not brick", err)
		}
		if !uuidv7Pattern.MatchString(id) {
			t.Fatalf("InsertRun() id = %q, want a canonical UUIDv7", id)
		}
	})

	t.Run("busy_timeout applies to every pooled connection, not just the first", func(t *testing.T) {
		t.Parallel()

		// issue #4 FIX 6: busy_timeout used to be a bare PRAGMA exec at
		// migrate() time, reaching only the one connection migrate()
		// happened to run on — the daemon's 2nd+ pooled connection
		// (DAEMON_MAX_CONNS) silently kept SQLite's 0 (no wait) default.
		// openOnce now sets it via the DSN's _pragma parameter instead,
		// which the driver reapplies on every new physical connection. To
		// actually prove that (rather than just re-querying whatever
		// connection migrate() already configured), this forces `conns`
		// distinct physical connections to be alive simultaneously — each
		// holds its own open transaction, so the pool cannot satisfy a
		// second goroutine by handing back the first goroutine's own
		// connection once it is done with it.
		const conns = 3
		path := filepath.Join(t.TempDir(), "yarddog.db")

		s, err := OpenStore(t.Context(), path, conns, true)
		if err != nil {
			t.Fatalf("OpenStore() error = %v", err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})

		var began, finished sync.WaitGroup
		began.Add(conns)
		finished.Add(conns)
		proceed := make(chan struct{})

		timeouts := make([]int, conns)
		errs := make([]error, conns)
		for i := range conns {
			go func() {
				defer finished.Done()

				tx, err := s.db.BeginTx(t.Context(), nil)
				if err != nil {
					errs[i] = fmt.Errorf("BeginTx: %w", err)
					began.Done()
					return
				}
				defer func() { _ = tx.Rollback() }()

				began.Done() // this goroutine now holds its own connection
				<-proceed    // wait until every goroutine holds one, too

				errs[i] = tx.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&timeouts[i])
			}()
		}
		began.Wait()
		close(proceed)
		finished.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d error = %v", i, err)
			}
			if timeouts[i] != 5000 {
				t.Fatalf("goroutine %d: PRAGMA busy_timeout = %d, want 5000 (issue #4 FIX 6)", i, timeouts[i])
			}
		}
	})

	t.Run("reopening across repeated restarts still mints ids above the highest CHILD id, not just the highest run id", func(t *testing.T) {
		// deliberately no t.Parallel(): every "restart" below forces the
		// shared package-level {uuidv7LastMs, uuidv7Counter} watermark to a
		// known blank baseline via forceUUIDv7State, simulating the state a
		// genuinely fresh OS process starts with — a concurrently running
		// t.Parallel() sibling that also mints ids would race that forced
		// state. Staying fully sequential is what TestSeedUUIDv7Watermark's
		// file comment (uuid_test.go) relies on too, for the same reason.
		//
		// this reproduces the regression the "reopening after a wall-clock
		// rollback..." subtest above does NOT catch: that one only ever
		// InsertRuns, so seeding from MAX(runs.id) alone happens to be
		// enough. Here every restart also writes checks/metrics/pings —
		// which share runs' own process-wide UUIDv7 counter (uuid.go) and
		// are always minted AFTER their parent run — so the true watermark
		// routinely lives on a CHILD row, not on runs. Seeding from runs
		// alone undershoots it, so a later restart's very first mint (its
		// own run row) is seeded off just "the previous run's own counter",
		// not "the previous run's children's" — landing BELOW the true
		// previous maximum instead of above it, corrupting ORDER BY
		// id/MAX(id) freshness queries against whichever table its next
		// children happen to re-tread into.
		//
		// the assertion below deliberately checks each restart's run id —
		// its very FIRST mint — against the true previous maximum, not the
		// cumulative max after that restart's own children are done: a
		// restart's own children always climb the shared counter by exactly
		// as many mints as it performs, so comparing post-batch cumulative
		// maxima happens to always land exactly one step ahead of the
		// previous batch's own maximum regardless of the bug (the
		// undershoot and the catch-up cancel out at the batch boundary) —
		// checking the run id in isolation, before its own children get a
		// chance to climb back over the gap, is what actually catches the
		// undershoot.
		path := filepath.Join(t.TempDir(), "yarddog.db")

		// every "restart" stamps its rows with the identical ts: isolates
		// the assertions below to the seeded watermark/counter alone (issue
		// #4's own stuck-clock scenario), with no help from an advancing
		// wall clock that would mask the bug by making ids sort correctly
		// for an unrelated reason.
		ts := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)

		proc1, runID1 := reopenAndMintTelemetry(t, path, ts)
		max1 := maxIDAcross(t, proc1, tableRuns, tableChecks, tableMetrics, tablePings)
		if runID1 >= max1 {
			t.Fatalf("precondition broken: run id %q >= true max %q, want a child row to hold the highest id", runID1, max1)
		}
		if err := proc1.Close(); err != nil {
			t.Fatalf("Close() proc1 error = %v", err)
		}

		proc2, runID2 := reopenAndMintTelemetry(t, path, ts)
		if runID2 <= max1 {
			t.Fatalf("restart 1: new run id %q does not sort strictly above the previous true max %q (issue #4 FIX 1)", runID2, max1)
		}
		max2 := maxIDAcross(t, proc2, tableRuns, tableChecks, tableMetrics, tablePings)
		if err := proc2.Close(); err != nil {
			t.Fatalf("Close() proc2 error = %v", err)
		}

		proc3, runID3 := reopenAndMintTelemetry(t, path, ts)
		if runID3 <= max2 {
			t.Fatalf("restart 2: new run id %q does not sort strictly above the previous true max %q (issue #4 FIX 1)", runID3, max2)
		}
		if err := proc3.Close(); err != nil {
			t.Fatalf("Close() proc3 error = %v", err)
		}
	})

	t.Run("selfHeal false surfaces an incompatible database's error instead of quarantining it", func(t *testing.T) {
		t.Parallel()

		// the daemon's own path (P0): it has no exclusive lock of its own and
		// systemd can start it before cron ever fires the collector, so it
		// must never rename the operator's real history aside — only the
		// collector (selfHeal true, reached via NewStore under its flock) may
		// do that. Build the exact same pre-issue-4 fixture as the
		// selfHeal-true sibling above: an INTEGER-autoincrement runs table
		// with a real row, no meta table.
		path := filepath.Join(t.TempDir(), "yarddog.db")

		oldDB, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open pre-issue-4 db: %v", err)
		}
		if _, err := oldDB.ExecContext(t.Context(), `CREATE TABLE runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			started_at TEXT NOT NULL,
			mode TEXT NOT NULL
		)`); err != nil {
			t.Fatalf("create pre-issue-4 runs table: %v", err)
		}
		if _, err := oldDB.ExecContext(t.Context(), `INSERT INTO runs (started_at, mode) VALUES (?, ?)`,
			formatTime(time.Now()), "soft"); err != nil {
			t.Fatalf("seed pre-issue-4 run: %v", err)
		}
		if err := oldDB.Close(); err != nil {
			t.Fatalf("close pre-issue-4 db: %v", err)
		}

		s, err := OpenStore(t.Context(), path, 1, false)
		if err == nil {
			if closeErr := s.Close(); closeErr != nil {
				t.Errorf("Close() unexpected store error = %v", closeErr)
			}
			t.Fatalf("OpenStore(selfHeal=false) on a pre-issue-4 database error = nil, want an error (not a silent quarantine)")
		}
		if !errors.Is(err, errIncompatibleSchema) {
			t.Fatalf("OpenStore(selfHeal=false) error = %v, want it to wrap errIncompatibleSchema", err)
		}

		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("Stat(%s) error = %v, want the original file left untouched", path, statErr)
		}
		quarantined, err := filepath.Glob(path + ".incompatible-*")
		if err != nil {
			t.Fatalf("Glob() error = %v", err)
		}
		if len(quarantined) != 0 {
			t.Fatalf("quarantined files matching %q = %v, want none: selfHeal=false must never rename the live file", path+".incompatible-*", quarantined)
		}
	})

	t.Run("selfHeal false surfaces a corrupt database's error instead of quarantining it", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "yarddog.db")
		const original = "not a sqlite database, just garbage bytes"
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		s, err := OpenStore(t.Context(), path, 1, false)
		if err == nil {
			if closeErr := s.Close(); closeErr != nil {
				t.Errorf("Close() unexpected store error = %v", closeErr)
			}
			t.Fatalf("OpenStore(selfHeal=false) on a non-database file error = nil, want an error (not a silent quarantine)")
		}
		if !isSQLiteCorruptionError(err) {
			t.Fatalf("OpenStore(selfHeal=false) error = %v, want the raw SQLITE_NOTADB error surfaced, not wrapped/replaced", err)
		}

		got, statErr := os.ReadFile(path)
		if statErr != nil {
			t.Fatalf("ReadFile(%s) error = %v, want the original file left untouched", path, statErr)
		}
		if string(got) != original {
			t.Fatalf("ReadFile(%s) = %q, want the untouched original %q", path, got, original)
		}
		quarantined, err := filepath.Glob(path + ".corrupt-*")
		if err != nil {
			t.Fatalf("Glob() error = %v", err)
		}
		if len(quarantined) != 0 {
			t.Fatalf("quarantined files matching %q = %v, want none: selfHeal=false must never rename the live file", path+".corrupt-*", quarantined)
		}
	})
}

func TestIsSQLiteCorruptionCode(t *testing.T) {
	t.Parallel()

	// SQLite result codes (https://www.sqlite.org/rescode.html) — the same
	// public, stable numbering isSQLiteCorruptionCode masks against —
	// verified directly against modernc.org/sqlite@v1.53.0's own generated
	// consts (lib/sqlite.go) rather than assumed: every extended code below
	// is that package's own "primary | (extra info << 8)" encoding of the
	// base code alongside it.
	const (
		sqliteOK              = 0
		sqliteErrorCode       = 1
		sqliteConstraint      = 19
		sqliteCorruptCode     = 11
		sqliteNotADBCode      = 26
		sqliteCorruptVTab     = 267 // 11 | (2 << 8)
		sqliteCorruptSequence = 523 // 11 | (3 << 8)
		sqliteCorruptIndex    = 779 // 11 | (4 << 8)
		sqliteBusy            = 5
		sqliteBusyRecovery    = 261 // 5 | (1 << 8)
		sqliteBusySnapshot    = 517 // 5 | (2 << 8)
		sqliteBusyTimeout     = 773 // 5 | (3 << 8)
	)

	tests := []struct {
		name string
		code int
		want bool
	}{
		{"base SQLITE_CORRUPT", sqliteCorruptCode, true},
		{"base SQLITE_NOTADB", sqliteNotADBCode, true},
		{"extended SQLITE_CORRUPT_VTAB", sqliteCorruptVTab, true},
		{"extended SQLITE_CORRUPT_SEQUENCE", sqliteCorruptSequence, true},
		{"extended SQLITE_CORRUPT_INDEX", sqliteCorruptIndex, true},
		{"base SQLITE_BUSY is never corruption", sqliteBusy, false},
		{"extended SQLITE_BUSY_RECOVERY is never corruption", sqliteBusyRecovery, false},
		{"extended SQLITE_BUSY_SNAPSHOT is never corruption", sqliteBusySnapshot, false},
		{"extended SQLITE_BUSY_TIMEOUT is never corruption", sqliteBusyTimeout, false},
		{"SQLITE_OK", sqliteOK, false},
		{"SQLITE_ERROR", sqliteErrorCode, false},
		{"SQLITE_CONSTRAINT", sqliteConstraint, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isSQLiteCorruptionCode(tt.code); got != tt.want {
				t.Fatalf("isSQLiteCorruptionCode(%d) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestIsSQLiteCorruptionError(t *testing.T) {
	t.Run("a non-sqlite error is never a corruption error", func(t *testing.T) {
		t.Parallel()

		for _, err := range []error{
			errors.New("boom"),
			fmt.Errorf("wrapped: %w", errors.New("boom")),
		} {
			if isSQLiteCorruptionError(err) {
				t.Errorf("isSQLiteCorruptionError(%v) = true, want false (not a *sqlite.Error at all)", err)
			}
		}
	})

	t.Run("a genuine SQLITE_NOTADB error from opening a non-database file is a corruption error", func(t *testing.T) {
		t.Parallel()

		// exercises the real driver path, not a fabricated *sqlite.Error:
		// its fields are unexported and modernc.org/sqlite has no
		// constructor, so a genuine error is the only way to get one.
		path := filepath.Join(t.TempDir(), "yarddog.db")
		if err := os.WriteFile(path, []byte("not a sqlite database, just garbage bytes"), 0600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		_, err := openOnce(t.Context(), path, 1)
		if err == nil {
			t.Fatal("openOnce() on a non-database file error = nil, want a SQLITE_NOTADB failure")
		}
		if !isSQLiteCorruptionError(err) {
			t.Fatalf("isSQLiteCorruptionError(%v) = false, want true (a genuine SQLITE_NOTADB error)", err)
		}
	})
}

func TestQuarantineFile(t *testing.T) {
	t.Run("renames the main file and its wal/shm siblings under one shared suffix", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "yarddog.db")
		contents := map[string]string{
			path:          "main db bytes",
			path + "-wal": "wal bytes",
			path + "-shm": "shm bytes",
		}
		for p, content := range contents {
			if err := os.WriteFile(p, []byte(content), 0600); err != nil {
				t.Fatalf("WriteFile(%s) error = %v", p, err)
			}
		}

		now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := quarantineFile(path, "corrupt", now); err != nil {
			t.Fatalf("quarantineFile() error = %v", err)
		}

		for p := range contents {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("Stat(%s) error = %v, want the original renamed away", p, err)
			}
		}

		const wantSuffix = ".corrupt-20300102T030405Z"
		for p, content := range contents {
			quarantined := p + wantSuffix
			got, err := os.ReadFile(quarantined)
			if err != nil {
				t.Fatalf("ReadFile(%s) error = %v, want the sibling quarantined alongside the main file under the same suffix", quarantined, err)
			}
			if string(got) != content {
				t.Fatalf("ReadFile(%s) = %q, want untouched content %q", quarantined, got, content)
			}
		}
	})

	t.Run("missing wal/shm sidecars are tolerated, not an error", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "yarddog.db")
		if err := os.WriteFile(path, []byte("main db bytes"), 0600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		// deliberately no -wal/-shm siblings: WAL segments are created lazily
		// on first write, so a quarantine before that must not fail on their
		// absence.

		if err := quarantineFile(path, "corrupt", time.Now()); err != nil {
			t.Fatalf("quarantineFile() error = %v, want a missing sidecar to be tolerated", err)
		}

		quarantined, err := filepath.Glob(path + ".corrupt-*")
		if err != nil {
			t.Fatalf("Glob() error = %v", err)
		}
		if len(quarantined) != 1 {
			t.Fatalf("quarantined files matching %q = %v, want exactly 1 (just the main file)", path+".corrupt-*", quarantined)
		}
	})
}

func TestStore_CheckUP(t *testing.T) {
	t.Run("healthy on a freshly opened store", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		if err := s.CheckUP(t.Context()); err != nil {
			t.Fatalf("CheckUP() error = %v, want nil", err)
		}
	})

	t.Run("returns an error once the store is closed", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		if err := s.CheckUP(t.Context()); err == nil {
			t.Fatal("CheckUP() on a closed store error = nil, want non-nil")
		}
	})
}

func TestStore_EnqueueOutboxMessage(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	createdAt := time.Now().UTC().Truncate(time.Second)

	id, err := s.EnqueueOutboxMessage(t.Context(), createdAt, "hello")
	if err != nil {
		t.Fatalf("EnqueueOutboxMessage() error = %v", err)
	}
	if id == 0 {
		t.Fatal("EnqueueOutboxMessage() id = 0, want a positive id")
	}

	unsent, err := s.ListUnsentOutboxMessages(t.Context())
	if err != nil {
		t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
	}
	if len(unsent) != 1 {
		t.Fatalf("ListUnsentOutboxMessages() = %d messages, want 1", len(unsent))
	}
	if unsent[0].ID != id || unsent[0].Text != "hello" || unsent[0].Attempts != 0 {
		t.Fatalf("ListUnsentOutboxMessages()[0] = %+v, want id=%d text=hello attempts=0", unsent[0], id)
	}
	if !unsent[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("ListUnsentOutboxMessages()[0].CreatedAt = %v, want %v", unsent[0].CreatedAt, createdAt)
	}
}

func TestStore_GetLastRebootStartedAt(t *testing.T) {
	t.Run("no prior reboot signals none, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		_, ok, err := s.GetLastRebootStartedAt(t.Context())
		if err != nil {
			t.Fatalf("GetLastRebootStartedAt() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("GetLastRebootStartedAt() ok = true, want false with no prior reboot")
		}
	})

	t.Run("returns the most recent reboot, ignoring non-reboot actions", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		older := now.Add(-3 * time.Hour)
		newer := now.Add(-40 * time.Minute)

		insertReboot(t, s, older)
		insertRunWithAction(t, s, domain.ActionNone, now.Add(-10*time.Minute))
		insertReboot(t, s, newer)

		got, ok, err := s.GetLastRebootStartedAt(t.Context())
		if err != nil {
			t.Fatalf("GetLastRebootStartedAt() error = %v", err)
		}
		if !ok {
			t.Fatal("GetLastRebootStartedAt() ok = false, want true")
		}
		if !got.Equal(newer) {
			t.Fatalf("GetLastRebootStartedAt() = %v, want the newer reboot %v", got, newer)
		}
	})

	t.Run("age compares younger and older than the cooldown threshold", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC()
		cooldown := 2 * time.Hour

		insertReboot(t, s, now.Add(-40*time.Minute))

		got, ok, err := s.GetLastRebootStartedAt(t.Context())
		if err != nil {
			t.Fatalf("GetLastRebootStartedAt() error = %v", err)
		}
		if !ok {
			t.Fatal("GetLastRebootStartedAt() ok = false, want true")
		}
		if age := now.Sub(got); age >= cooldown {
			t.Fatalf("reboot age = %v, want younger than cooldown %v", age, cooldown)
		}
	})
}

func TestStore_GetRun(t *testing.T) {
	t.Run("not found returns an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		if _, err := s.GetRun(t.Context(), "does-not-exist"); err == nil {
			t.Fatal("GetRun(\"does-not-exist\") error = nil, want error for a missing row")
		}
	})
}

func TestStore_InsertCheck(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	runID, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	ts := time.Now().UTC().Truncate(time.Second)
	latency := int64(42)
	checks := []domain.Check{
		{RunID: runID, TS: ts, Phase: domain.PhaseInitial, Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true, LatencyMS: &latency},
		{RunID: runID, TS: ts, Phase: domain.PhaseInitial, Target: "https://example.com/generate_204", Kind: domain.CheckKindDomain, OK: false, Error: "status 500"},
	}
	for _, c := range checks {
		if err := s.InsertCheck(t.Context(), c); err != nil {
			t.Fatalf("InsertCheck(%+v) error = %v", c, err)
		}
	}

	got, err := s.ListChecksByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("ListChecksByRun() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListChecksByRun() = %d rows, want 2", len(got))
	}

	if got[0].Kind != domain.CheckKindIP || !got[0].OK || got[0].LatencyMS == nil || *got[0].LatencyMS != latency {
		t.Fatalf("ListChecksByRun()[0] = %+v, want the ip check with latency %d", got[0], latency)
	}
	if got[0].Error != "" {
		t.Fatalf("ListChecksByRun()[0].Error = %q, want empty", got[0].Error)
	}

	if got[1].Kind != domain.CheckKindDomain || got[1].OK || got[1].Error != "status 500" {
		t.Fatalf("ListChecksByRun()[1] = %+v, want the failed domain check", got[1])
	}
	if got[1].LatencyMS != nil {
		t.Fatalf("ListChecksByRun()[1].LatencyMS = %v, want nil (no latency recorded)", got[1].LatencyMS)
	}
}

func TestStore_ListChecksByRun(t *testing.T) {
	t.Run("returns hot checks ordered by id", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		// distinct Target per row (insertCheckAt's fixture check is
		// byte-identical every time) so the assertion below can actually
		// pin ORDER BY id ASC — two indistinguishable rows would still pass
		// with ASC flipped to DESC, or the ORDER BY dropped entirely.
		first := domain.Check{RunID: runID, TS: time.Now(), Phase: domain.PhaseInitial, Target: "1.1.1.1:443", Kind: domain.CheckKindIP, OK: true}
		second := domain.Check{RunID: runID, TS: time.Now(), Phase: domain.PhaseInitial, Target: "8.8.8.8:53", Kind: domain.CheckKindIP, OK: true}
		if err := s.InsertCheck(t.Context(), first); err != nil {
			t.Fatalf("InsertCheck(first) error = %v", err)
		}
		if err := s.InsertCheck(t.Context(), second); err != nil {
			t.Fatalf("InsertCheck(second) error = %v", err)
		}

		got, err := s.ListChecksByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListChecksByRun() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListChecksByRun() = %d rows, want 2", len(got))
		}
		if got[0].Target != first.Target || got[1].Target != second.Target {
			t.Fatalf("ListChecksByRun() targets = [%q, %q], want [%q, %q] in insertion order",
				got[0].Target, got[1].Target, first.Target, second.Target)
		}
	})

	t.Run("returns archived checks after rollover, identical shape to hot", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		runID, err := s.InsertRun(t.Context(), old, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		insertCheckAt(t, s, runID, old)

		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		got, err := s.ListChecksByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListChecksByRun() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListChecksByRun() after rollover = %d rows, want 1 (from checks_archive)", len(got))
		}
		if got[0].RunID != runID || got[0].Kind != domain.CheckKindIP {
			t.Fatalf("ListChecksByRun() after rollover = %+v, want the archived check intact", got[0])
		}
	})

	t.Run("an unknown run returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.ListChecksByRun(t.Context(), "does-not-exist")
		if err != nil {
			t.Fatalf("ListChecksByRun() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListChecksByRun(unknown) = %d rows, want 0", len(got))
		}
	})
}

func TestStore_InsertRun(t *testing.T) {
	t.Run("soft mode carries the initial check result", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		startedAt := time.Now().UTC().Truncate(time.Second)
		up := true

		id, err := s.InsertRun(t.Context(), startedAt, "soft", &up)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		run, err := s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if run.Mode != "soft" {
			t.Fatalf("Mode = %q, want %q", run.Mode, "soft")
		}
		if !run.StartedAt.Equal(startedAt) {
			t.Fatalf("StartedAt = %v, want %v", run.StartedAt, startedAt)
		}
		if run.InternetOK == nil || !*run.InternetOK {
			t.Fatalf("InternetOK = %v, want true", run.InternetOK)
		}
		if run.Action != domain.ActionNone {
			t.Fatalf("Action = %q, want default %q", run.Action, domain.ActionNone)
		}
		if run.RebootStartedAt != nil || run.FinishedAt != nil || run.Outcome != "" {
			t.Fatalf("unset phase fields are non-nil: %+v", run)
		}
	})

	t.Run("hard mode leaves internet_ok nil", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		id, err := s.InsertRun(t.Context(), time.Now(), "hard", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		run, err := s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if run.InternetOK != nil {
			t.Fatalf("InternetOK = %v, want nil for hard mode (no initial check)", run.InternetOK)
		}
	})

	t.Run("a crypto/rand failure propagates as a wrapped error, not a silently empty id", func(t *testing.T) {
		// NOT t.Parallel(): overrides the package-level uuidv7RandRead seam
		// every newUUIDv7 caller shares — see TestNewUUIDv7's isolation note
		// (uuid_test.go) for why a non-parallel subtest is safe here.
		original := uuidv7RandRead
		uuidv7RandRead = func([]byte) (int, error) { return 0, errors.New("forced entropy failure") }
		t.Cleanup(func() { uuidv7RandRead = original })

		s := newTestStore(t)

		id, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err == nil {
			t.Fatal("InsertRun() error = nil, want the forced entropy failure to propagate")
		}
		if id != "" {
			t.Fatalf("InsertRun() id = %q, want empty on error", id)
		}
	})
}

func TestStore_LatestHost(t *testing.T) {
	t.Run("empty host table reports ok=false, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		_, ok, err := s.LatestHost(t.Context())
		if err != nil {
			t.Fatalf("LatestHost() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("LatestHost() ok = true, want false on an empty store")
		}
	})

	t.Run("returns the newest run's host row", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		olderID, err := s.InsertRun(t.Context(), base.Add(-time.Hour), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		saveMetricsAt(t, s, olderID, base.Add(-time.Hour), "older-host")

		newestID, err := s.InsertRun(t.Context(), base, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		saveMetricsAt(t, s, newestID, base, "newest-host")

		got, ok, err := s.LatestHost(t.Context())
		if err != nil {
			t.Fatalf("LatestHost() error = %v", err)
		}
		if !ok {
			t.Fatal("LatestHost() ok = false, want true")
		}
		if got.RunID != newestID {
			t.Fatalf("LatestHost().RunID = %s, want %s", got.RunID, newestID)
		}
		if got.Host.Hostname != "newest-host" {
			t.Fatalf("LatestHost().Host.Hostname = %q, want %q", got.Host.Hostname, "newest-host")
		}
		if !got.TS.Equal(base) {
			t.Fatalf("LatestHost().TS = %v, want %v", got.TS, base)
		}
	})
}

func TestStore_LatestMetrics(t *testing.T) {
	t.Run("empty metrics table returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.LatestMetrics(t.Context())
		if err != nil {
			t.Fatalf("LatestMetrics() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("LatestMetrics() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("returns every row of the newest run that has metrics, skipping a metrics-less newer run", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		insertMetricAt(t, s, base.Add(-time.Hour), domain.CollectorCPU, "load1", 1.0, true)

		// a run recorded with METRICS_ENABLED=false has no metrics rows at
		// all; LatestMetrics must not mistake "newest run" for "newest run
		// that has metrics".
		if _, err := s.InsertRun(t.Context(), base.Add(-30*time.Minute), domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		newestRunID := insertMetricAt(t, s, base, domain.CollectorMemory, "total", 100, true)

		got, err := s.LatestMetrics(t.Context())
		if err != nil {
			t.Fatalf("LatestMetrics() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("LatestMetrics() = %d rows, want 1 (only the newest run-with-metrics' row)", len(got))
		}
		if got[0].RunID != newestRunID {
			t.Fatalf("LatestMetrics()[0].RunID = %s, want %s", got[0].RunID, newestRunID)
		}
		if got[0].Sample.Collector != domain.CollectorMemory {
			t.Fatalf("LatestMetrics()[0].Sample.Collector = %q, want %q", got[0].Sample.Collector, domain.CollectorMemory)
		}
	})
}

func TestStore_SaveMetrics(t *testing.T) {
	t.Run("writes the host row and one metrics row per sample, mixed ok/unavailable", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		ts := time.Now().UTC().Truncate(time.Second)

		m := domain.HostMetrics{
			Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"},
			Samples: []domain.MetricSample{
				{Collector: domain.CollectorTemperature, Name: "cpu-thermal", Value: 52.35, Unit: "celsius", OK: true},
				{Collector: domain.CollectorFans, Name: "fans", Unit: "rpm", OK: false, Error: "no fan sensors present"},
			},
		}

		if err := s.SaveMetrics(t.Context(), runID, ts, m); err != nil {
			t.Fatalf("SaveMetrics() error = %v", err)
		}

		host, err := s.GetHost(t.Context(), runID)
		if err != nil {
			t.Fatalf("GetHost() error = %v", err)
		}
		if host != m.Host {
			t.Fatalf("GetHost() = %+v, want %+v", host, m.Host)
		}

		got, err := s.ListMetricsByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListMetricsByRun() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListMetricsByRun() = %d rows, want 2", len(got))
		}

		if got[0].Collector != domain.CollectorTemperature || got[0].Name != "cpu-thermal" || !got[0].OK || got[0].Value != 52.35 || got[0].Unit != "celsius" {
			t.Fatalf("ListMetricsByRun()[0] = %+v, want the ok temperature sample", got[0])
		}
		if got[0].Error != "" {
			t.Fatalf("ListMetricsByRun()[0].Error = %q, want empty", got[0].Error)
		}

		if got[1].Collector != domain.CollectorFans || got[1].OK || got[1].Error != "no fan sensors present" {
			t.Fatalf("ListMetricsByRun()[1] = %+v, want the unavailable fans sample", got[1])
		}
		if got[1].Value != 0 {
			t.Fatalf("ListMetricsByRun()[1].Value = %v, want 0 (an unavailable sample persists as SQL NULL)", got[1].Value)
		}
	})

	t.Run("a second call for the same run fails and leaves no partial rows", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		ts := time.Now().UTC().Truncate(time.Second)
		first := domain.HostMetrics{
			Host:    domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"},
			Samples: []domain.MetricSample{{Collector: domain.CollectorUptime, Name: "uptime", Value: 123, Unit: "seconds", OK: true}},
		}
		if err := s.SaveMetrics(t.Context(), runID, ts, first); err != nil {
			t.Fatalf("first SaveMetrics() error = %v", err)
		}

		// host.run_id is a PRIMARY KEY, so this second snapshot fails on its
		// very first statement (the host insert) — before any of its sample
		// rows are ever attempted. That still proves what matters: a failed
		// SaveMetrics call commits nothing at all, leaving the first
		// snapshot exactly as it was.
		second := domain.HostMetrics{
			Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"},
			Samples: []domain.MetricSample{
				{Collector: domain.CollectorUptime, Name: "uptime", Value: 456, Unit: "seconds", OK: true},
				{Collector: domain.CollectorMemory, Name: "total", Value: 789, Unit: "bytes", OK: true},
			},
		}
		if err := s.SaveMetrics(t.Context(), runID, ts, second); err == nil {
			t.Fatal("second SaveMetrics() for the same run error = nil, want a primary key violation")
		}

		got, err := s.ListMetricsByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListMetricsByRun() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListMetricsByRun() = %d rows, want 1 (the failed second snapshot must add nothing)", len(got))
		}
		if got[0].Value != 123 {
			t.Fatalf("ListMetricsByRun()[0].Value = %v, want the first snapshot's untouched value 123", got[0].Value)
		}
	})
}

func TestStore_SavePings(t *testing.T) {
	t.Run("writes one row per result, reachable and unreachable round-trip via ListPingsByRun", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		runID, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		ts := time.Now().UTC().Truncate(time.Second)

		results := []domain.PingResult{
			{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 12.5, OK: true},
			{Host: "unreachable.example", Sent: 5, Received: 0, OK: false, Error: "no route to host"},
		}

		if err := s.SavePings(t.Context(), runID, ts, results); err != nil {
			t.Fatalf("SavePings() error = %v", err)
		}

		got, err := s.ListPingsByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListPingsByRun() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListPingsByRun() = %d rows, want 2", len(got))
		}

		if got[0].Host != "1.1.1.1" || !got[0].OK || got[0].Sent != 5 || got[0].Received != 5 || got[0].AvgMS != 12.5 {
			t.Fatalf("ListPingsByRun()[0] = %+v, want the reachable result", got[0])
		}
		if got[0].Error != "" {
			t.Fatalf("ListPingsByRun()[0].Error = %q, want empty", got[0].Error)
		}

		if got[1].Host != "unreachable.example" || got[1].OK || got[1].Received != 0 || got[1].Error != "no route to host" {
			t.Fatalf("ListPingsByRun()[1] = %+v, want the unreachable result", got[1])
		}
		if got[1].AvgMS != 0 {
			t.Fatalf("ListPingsByRun()[1].AvgMS = %v, want 0 (an unreachable result persists avg_ms as SQL NULL)", got[1].AvgMS)
		}
	})
}

func TestStore_ListMetrics(t *testing.T) {
	t.Run("empty store returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListMetrics() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListMetrics() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("IncludeEmpty controls whether unavailable rows appear", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)
		insertMetricAt(t, s, base.Add(-1*time.Minute), domain.CollectorFans, "fans", 0, false)
		insertMetricAt(t, s, base, domain.CollectorCPU, "load1", 1.5, true)

		def, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListMetrics(default) error = %v", err)
		}
		if len(def) != 1 || def[0].Sample.Collector != domain.CollectorCPU {
			t.Fatalf("ListMetrics(default) = %+v, want only the ok row", def)
		}

		all, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10, IncludeEmpty: true})
		if err != nil {
			t.Fatalf("ListMetrics(IncludeEmpty) error = %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("ListMetrics(IncludeEmpty) = %d rows, want 2 (the ok and the unavailable)", len(all))
		}
	})

	t.Run("Since, Collector, and Limit each filter and combine", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		insertMetricAt(t, s, base.Add(-3*time.Hour), domain.CollectorCPU, "load1", 1.5, true)
		insertMetricAt(t, s, base.Add(-2*time.Hour), domain.CollectorTemperature, "cpu-thermal", 50, true)
		insertMetricAt(t, s, base.Add(-1*time.Hour), domain.CollectorCPU, "load1", 2.5, true)
		insertMetricAt(t, s, base, domain.CollectorCPU, "load1", 3.5, true)

		t.Run("Since excludes older rows", func(t *testing.T) {
			got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Since: base.Add(-90 * time.Minute), Limit: 10})
			if err != nil {
				t.Fatalf("ListMetrics() error = %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("ListMetrics(Since=-90m) = %d rows, want 2 (the -1h and now samples)", len(got))
			}
		})

		t.Run("Collector narrows to one kind", func(t *testing.T) {
			got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Collector: domain.CollectorTemperature, Limit: 10})
			if err != nil {
				t.Fatalf("ListMetrics() error = %v", err)
			}
			if len(got) != 1 || got[0].Sample.Collector != domain.CollectorTemperature {
				t.Fatalf("ListMetrics(Collector=temperature) = %+v, want exactly the one temperature sample", got)
			}
		})

		t.Run("Limit caps the result and Since+Collector combine, newest first", func(t *testing.T) {
			got, err := s.ListMetrics(t.Context(), services.MetricsFilter{
				Since:     base.Add(-150 * time.Minute),
				Collector: domain.CollectorCPU,
				Limit:     1,
			})
			if err != nil {
				t.Fatalf("ListMetrics() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ListMetrics(combined, limit=1) = %d rows, want 1", len(got))
			}
			// two cpu samples fall in range (-1h and now); newest first picks "now".
			if got[0].Sample.Value != 3.5 {
				t.Fatalf("ListMetrics(combined)[0].Sample.Value = %v, want the newest matching sample 3.5", got[0].Sample.Value)
			}
		})
	})

	t.Run("an unavailable sample reads back as OK=false, Value=0, with its Error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		insertMetricAt(t, s, time.Now(), domain.CollectorFans, "fans", 0, false)

		// IncludeEmpty: this asserts an unavailable sample round-trips, which the
		// default (ok=1) filter would now drop before we could inspect it.
		got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10, IncludeEmpty: true})
		if err != nil {
			t.Fatalf("ListMetrics() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListMetrics() = %d rows, want 1", len(got))
		}
		if got[0].Sample.OK {
			t.Fatal("Sample.OK = true, want false for an unavailable sample")
		}
		if got[0].Sample.Value != 0 {
			t.Fatalf("Sample.Value = %v, want 0 (SQL NULL for an unavailable sample)", got[0].Sample.Value)
		}
		if got[0].Sample.Error != "unavailable" {
			t.Fatalf("Sample.Error = %q, want %q", got[0].Sample.Error, "unavailable")
		}
	})

	t.Run("IncludeArchive spans hot and archive, newest first even when an archived id is lower", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		oldRunID := insertMetricAt(t, s, old, domain.CollectorCPU, "load1", 1.0, true)
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		// the archived run's id must sort lower than every subsequently
		// generated hot id: UUIDv7 is time-ordered and canonical lowercase
		// hex sorts lexicographically exactly as it sorts chronologically
		// (issue #4), so this holds for any two ids generated further apart
		// in time than the generator's within-millisecond counter spans.
		// This pins the monotonic-id assumption ORDER BY id DESC relies on
		// across the hot/archive union.
		newRunID := insertMetricAt(t, s, now, domain.CollectorCPU, "load1", 2.0, true)
		if oldRunID >= newRunID {
			t.Fatalf("fixture invariant broken: archived run id %q must sort lower than hot run id %q", oldRunID, newRunID)
		}

		withoutArchive, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListMetrics(IncludeArchive=false) error = %v", err)
		}
		if len(withoutArchive) != 1 {
			t.Fatalf("ListMetrics(IncludeArchive=false) = %d rows, want 1 (hot only)", len(withoutArchive))
		}

		withArchive, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 10, IncludeArchive: true})
		if err != nil {
			t.Fatalf("ListMetrics(IncludeArchive=true) error = %v", err)
		}
		if len(withArchive) != 2 {
			t.Fatalf("ListMetrics(IncludeArchive=true) = %d rows, want 2 (hot + archive)", len(withArchive))
		}
		if withArchive[0].RunID != newRunID || withArchive[1].RunID != oldRunID {
			t.Fatalf("ListMetrics(IncludeArchive=true) run ids = [%q, %q], want [%q, %q] newest first",
				withArchive[0].RunID, withArchive[1].RunID, newRunID, oldRunID)
		}
	})

	t.Run("Since older than the hot window pulls archived rows only when IncludeArchive is set", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		insertMetricAt(t, s, old, domain.CollectorCPU, "load1", 1.0, true)
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		withoutArchive, err := s.ListMetrics(t.Context(), services.MetricsFilter{Since: old.Add(-time.Hour), Limit: 10})
		if err != nil {
			t.Fatalf("ListMetrics(IncludeArchive=false) error = %v", err)
		}
		if len(withoutArchive) != 0 {
			t.Fatalf("ListMetrics(Since, IncludeArchive=false) = %d rows, want 0 (archived row excluded)", len(withoutArchive))
		}

		withArchive, err := s.ListMetrics(t.Context(), services.MetricsFilter{Since: old.Add(-time.Hour), Limit: 10, IncludeArchive: true})
		if err != nil {
			t.Fatalf("ListMetrics(IncludeArchive=true) error = %v", err)
		}
		if len(withArchive) != 1 {
			t.Fatalf("ListMetrics(Since, IncludeArchive=true) = %d rows, want 1 (the archived row)", len(withArchive))
		}
	})

	t.Run("Limit counts rows across the union", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		insertMetricAt(t, s, old, domain.CollectorCPU, "load1", 1.0, true)
		insertMetricAt(t, s, old.Add(time.Minute), domain.CollectorCPU, "load1", 1.5, true)
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		insertMetricAt(t, s, now, domain.CollectorCPU, "load1", 2.0, true)

		got, err := s.ListMetrics(t.Context(), services.MetricsFilter{Limit: 2, IncludeArchive: true})
		if err != nil {
			t.Fatalf("ListMetrics() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListMetrics(Limit=2, IncludeArchive) = %d rows, want 2 (limit applied over the union)", len(got))
		}
	})

	t.Run("IncludeArchive query plan merges instead of materializing and sorting", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		insertMetricAt(t, s, old, domain.CollectorCPU, "load1", 1.0, true)
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		insertMetricAt(t, s, now, domain.CollectorCPU, "load1", 2.0, true)

		// build the exact SQL ListMetrics(IncludeArchive: true) runs by
		// calling the same spanQuery helper, so this test can never drift
		// from the real query shape.
		query, _ := spanQuery(tableMetrics, tableMetricsArchive, metricsColumnList, metricsFullColumnList, colMetricsID,
			" WHERE "+colMetricsOK+" = 1", true)

		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, 100)
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
		}
		defer rows.Close()

		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatalf("scan query plan row: %v", err)
			}
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("query plan rows: %v", err)
		}

		got := plan.String()
		if !strings.Contains(got, "MERGE (UNION ALL)") {
			t.Fatalf("query plan =\n%swant it to contain %q — the archive-spanning UNION ALL must merge two pre-sorted legs, not materialize the whole result", got, "MERGE (UNION ALL)")
		}
		if strings.Contains(got, "TEMP B-TREE") {
			t.Fatalf("query plan =\n%swant no %q — that would mean SQLite is fully sorting instead of merging", got, "TEMP B-TREE")
		}
	})

	t.Run("Since adds a sargable id floor so the archive leg seeks instead of scanning to EOF", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		insertMetricAt(t, s, old, domain.CollectorCPU, "load1", 1.0, true)
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		insertMetricAt(t, s, now, domain.CollectorCPU, "load1", 2.0, true)

		// mirrors the exact WHERE clause ListMetrics builds for a Since
		// filter (issue #4 FIX 2): id >= uuidv7Floor(since) is the sargable
		// condition under test, ts >= since stays the exact residual, and
		// ok = 1 is IncludeEmpty's default. since sits inside the hot
		// window, so the archive leg matches zero rows — exactly the case
		// that used to cost a full scan to EOF despite returning nothing.
		since := now.Add(-time.Hour)
		where := fmt.Sprintf(" WHERE %s >= ? AND %s >= ? AND %s = 1", colMetricsID, colMetricsTS, colMetricsOK)
		query, legs := spanQuery(tableMetrics, tableMetricsArchive, metricsColumnList, metricsFullColumnList, colMetricsID, where, true)
		if legs != 2 {
			t.Fatalf("spanQuery() legs = %d, want 2 for IncludeArchive", legs)
		}

		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query,
			uuidv7Floor(since), formatTime(since), uuidv7Floor(since), formatTime(since), 100)
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
		}
		defer rows.Close()

		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatalf("scan query plan row: %v", err)
			}
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("query plan rows: %v", err)
		}

		got := plan.String()
		wantSearch := fmt.Sprintf("SEARCH %s USING INDEX", tableMetricsArchive)
		if !strings.Contains(got, wantSearch) {
			t.Fatalf("query plan =\n%swant %q — the id floor must let SQLite seek instead of scanning the whole archive tier just to check ts as a residual", got, wantSearch)
		}
		wantNoScan := fmt.Sprintf("SCAN %s USING INDEX", tableMetricsArchive)
		if strings.Contains(got, wantNoScan) {
			t.Fatalf("query plan =\n%swant no %q — that is exactly the full-scan-to-EOF regression the id floor fixes", got, wantNoScan)
		}
	})
}

func TestStore_ListPings(t *testing.T) {
	t.Run("empty store returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListPings() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListPings() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("IncludeUnreachable controls whether received=0 rows appear", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)
		insertPingAt(t, s, base.Add(-1*time.Minute), "unreachable.example", 5, 0, 0, "no route to host")
		insertPingAt(t, s, base, "1.1.1.1", 5, 5, 12.5, "")

		def, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListPings(default) error = %v", err)
		}
		if len(def) != 1 || def[0].Result.Host != "1.1.1.1" {
			t.Fatalf("ListPings(default) = %+v, want only the reachable row", def)
		}

		all, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10, IncludeUnreachable: true})
		if err != nil {
			t.Fatalf("ListPings(IncludeUnreachable) error = %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("ListPings(IncludeUnreachable) = %d rows, want 2 (the reachable and the unreachable)", len(all))
		}
	})

	t.Run("Since, Host, and Limit each filter and combine, newest first", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		insertPingAt(t, s, base.Add(-3*time.Hour), "1.1.1.1", 5, 5, 10, "")
		insertPingAt(t, s, base.Add(-2*time.Hour), "8.8.8.8", 5, 5, 20, "")
		insertPingAt(t, s, base.Add(-1*time.Hour), "1.1.1.1", 5, 5, 30, "")
		insertPingAt(t, s, base, "1.1.1.1", 5, 5, 40, "")

		t.Run("Since excludes older rows", func(t *testing.T) {
			got, err := s.ListPings(t.Context(), services.PingFilter{Since: base.Add(-90 * time.Minute), Limit: 10})
			if err != nil {
				t.Fatalf("ListPings() error = %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("ListPings(Since=-90m) = %d rows, want 2 (the -1h and now samples)", len(got))
			}
		})

		t.Run("Host narrows to one host", func(t *testing.T) {
			got, err := s.ListPings(t.Context(), services.PingFilter{Host: "8.8.8.8", Limit: 10})
			if err != nil {
				t.Fatalf("ListPings() error = %v", err)
			}
			if len(got) != 1 || got[0].Result.Host != "8.8.8.8" {
				t.Fatalf("ListPings(Host=8.8.8.8) = %+v, want exactly the one 8.8.8.8 ping", got)
			}
		})

		t.Run("Limit caps the result and Since+Host combine, newest first", func(t *testing.T) {
			got, err := s.ListPings(t.Context(), services.PingFilter{
				Since: base.Add(-150 * time.Minute),
				Host:  "1.1.1.1",
				Limit: 1,
			})
			if err != nil {
				t.Fatalf("ListPings() error = %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ListPings(combined, limit=1) = %d rows, want 1", len(got))
			}
			// two 1.1.1.1 pings fall in range (-1h and now); newest first picks "now".
			if got[0].Result.AvgMS != 40 {
				t.Fatalf("ListPings(combined)[0].Result.AvgMS = %v, want the newest matching sample 40", got[0].Result.AvgMS)
			}
		})
	})

	t.Run("an unreachable result reads back as OK=false, AvgMS=0, with its Error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		insertPingAt(t, s, time.Now(), "unreachable.example", 5, 0, 0, "no route to host")

		// IncludeUnreachable: this asserts an unreachable result round-trips, which
		// the default (received>0) filter would now drop before we could inspect it.
		got, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10, IncludeUnreachable: true})
		if err != nil {
			t.Fatalf("ListPings() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ListPings() = %d rows, want 1", len(got))
		}
		if got[0].Result.OK {
			t.Fatal("Result.OK = true, want false for an unreachable result")
		}
		if got[0].Result.AvgMS != 0 {
			t.Fatalf("Result.AvgMS = %v, want 0 (SQL NULL for an unreachable result)", got[0].Result.AvgMS)
		}
		if got[0].Result.Error != "no route to host" {
			t.Fatalf("Result.Error = %q, want %q", got[0].Result.Error, "no route to host")
		}
	})

	t.Run("IncludeArchive spans hot and archive, newest first even when an archived id is lower", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		oldRunID := insertPingAt(t, s, old, "1.1.1.1", 5, 5, 10, "")
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		// UUIDv7 is time-ordered (issue #4), so the archived id sorts lower
		// than the later-generated hot id, pinning the assumption
		// ORDER BY id DESC relies on across the hot/archive union.
		newRunID := insertPingAt(t, s, now, "1.1.1.1", 5, 5, 20, "")
		if oldRunID >= newRunID {
			t.Fatalf("fixture invariant broken: archived run id %q must sort lower than hot run id %q", oldRunID, newRunID)
		}

		withoutArchive, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10})
		if err != nil {
			t.Fatalf("ListPings(IncludeArchive=false) error = %v", err)
		}
		if len(withoutArchive) != 1 {
			t.Fatalf("ListPings(IncludeArchive=false) = %d rows, want 1 (hot only)", len(withoutArchive))
		}

		withArchive, err := s.ListPings(t.Context(), services.PingFilter{Limit: 10, IncludeArchive: true})
		if err != nil {
			t.Fatalf("ListPings(IncludeArchive=true) error = %v", err)
		}
		if len(withArchive) != 2 {
			t.Fatalf("ListPings(IncludeArchive=true) = %d rows, want 2 (hot + archive)", len(withArchive))
		}
		if withArchive[0].RunID != newRunID || withArchive[1].RunID != oldRunID {
			t.Fatalf("ListPings(IncludeArchive=true) run ids = [%q, %q], want [%q, %q] newest first",
				withArchive[0].RunID, withArchive[1].RunID, newRunID, oldRunID)
		}
	})

	t.Run("Since older than the hot window pulls archived rows only when IncludeArchive is set", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		insertPingAt(t, s, old, "1.1.1.1", 5, 5, 10, "")
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		withoutArchive, err := s.ListPings(t.Context(), services.PingFilter{Since: old.Add(-time.Hour), Limit: 10})
		if err != nil {
			t.Fatalf("ListPings(IncludeArchive=false) error = %v", err)
		}
		if len(withoutArchive) != 0 {
			t.Fatalf("ListPings(Since, IncludeArchive=false) = %d rows, want 0 (archived row excluded)", len(withoutArchive))
		}

		withArchive, err := s.ListPings(t.Context(), services.PingFilter{Since: old.Add(-time.Hour), Limit: 10, IncludeArchive: true})
		if err != nil {
			t.Fatalf("ListPings(IncludeArchive=true) error = %v", err)
		}
		if len(withArchive) != 1 {
			t.Fatalf("ListPings(Since, IncludeArchive=true) = %d rows, want 1 (the archived row)", len(withArchive))
		}
	})

	t.Run("Limit counts rows across the union", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		insertPingAt(t, s, old, "1.1.1.1", 5, 5, 10, "")
		insertPingAt(t, s, old.Add(time.Minute), "1.1.1.1", 5, 5, 15, "")
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		insertPingAt(t, s, now, "1.1.1.1", 5, 5, 20, "")

		got, err := s.ListPings(t.Context(), services.PingFilter{Limit: 2, IncludeArchive: true})
		if err != nil {
			t.Fatalf("ListPings() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListPings(Limit=2, IncludeArchive) = %d rows, want 2 (limit applied over the union)", len(got))
		}
	})

	t.Run("Since adds a sargable id floor so the archive leg seeks instead of scanning to EOF", func(t *testing.T) {
		t.Parallel()

		// mirrors ListMetrics' equivalent subtest (issue #4 FIX 2): the
		// same uuidv7Floor lower bound applies to ListPings' archive leg,
		// so its query plan must show the same SEARCH-not-SCAN shape.
		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		insertPingAt(t, s, old, "1.1.1.1", 5, 5, 10, "")
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		insertPingAt(t, s, now, "1.1.1.1", 5, 5, 20, "")

		// mirrors the exact WHERE clause ListPings builds for a Since
		// filter with the default IncludeUnreachable=false: id >= ? is the
		// sargable condition under test, ts >= ? stays the exact residual,
		// received > 0 is IncludeUnreachable's default.
		since := now.Add(-time.Hour)
		where := fmt.Sprintf(" WHERE %s >= ? AND %s >= ? AND %s > 0", colPingsID, colPingsTS, colPingsReceived)
		query, legs := spanQuery(tablePings, tablePingsArchive, pingsColumnList, pingsFullColumnList, colPingsID, where, true)
		if legs != 2 {
			t.Fatalf("spanQuery() legs = %d, want 2 for IncludeArchive", legs)
		}

		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query,
			uuidv7Floor(since), formatTime(since), uuidv7Floor(since), formatTime(since), 100)
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
		}
		defer rows.Close()

		var plan strings.Builder
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatalf("scan query plan row: %v", err)
			}
			plan.WriteString(detail)
			plan.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("query plan rows: %v", err)
		}

		got := plan.String()
		wantSearch := fmt.Sprintf("SEARCH %s USING INDEX", tablePingsArchive)
		if !strings.Contains(got, wantSearch) {
			t.Fatalf("query plan =\n%swant %q — the id floor must let SQLite seek instead of scanning the whole archive tier just to check ts as a residual", got, wantSearch)
		}
		wantNoScan := fmt.Sprintf("SCAN %s USING INDEX", tablePingsArchive)
		if strings.Contains(got, wantNoScan) {
			t.Fatalf("query plan =\n%swant no %q — that is exactly the full-scan-to-EOF regression the id floor fixes", got, wantNoScan)
		}
	})
}

func TestStore_ListRuns(t *testing.T) {
	t.Run("empty store returns an empty slice, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		got, err := s.ListRuns(t.Context(), 10)
		if err != nil {
			t.Fatalf("ListRuns() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListRuns() = %d rows, want 0 on an empty store", len(got))
		}
	})

	t.Run("newest first and honours limit", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		var ids []string
		for range 5 {
			id, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
			if err != nil {
				t.Fatalf("InsertRun() error = %v", err)
			}
			ids = append(ids, id)
		}

		got, err := s.ListRuns(t.Context(), 3)
		if err != nil {
			t.Fatalf("ListRuns() error = %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("ListRuns() = %d rows, want 3 (limit honoured)", len(got))
		}

		// UUIDv7 ids are time-ordered (issue #4), and the generator's
		// monotonic counter keeps them strictly increasing even across
		// calls minted within the same millisecond, so insertion order and
		// id order coincide here exactly as they did under the old
		// autoincrement integer.
		wantIDs := []string{ids[4], ids[3], ids[2]}
		for i, run := range got {
			if run.ID != wantIDs[i] {
				t.Fatalf("ListRuns()[%d].ID = %s, want %s (newest first)", i, run.ID, wantIDs[i])
			}
		}
	})
}

func TestStore_ListUnsentOutboxMessages(t *testing.T) {
	t.Run("orders oldest first and excludes sent rows", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		base := time.Now().UTC().Truncate(time.Second)

		older, err := s.EnqueueOutboxMessage(t.Context(), base.Add(-time.Hour), "older")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage() error = %v", err)
		}
		newer, err := s.EnqueueOutboxMessage(t.Context(), base, "newer")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage() error = %v", err)
		}
		sent, err := s.EnqueueOutboxMessage(t.Context(), base.Add(-2*time.Hour), "already sent")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage() error = %v", err)
		}
		if err := s.MarkOutboxSent(t.Context(), sent, base); err != nil {
			t.Fatalf("MarkOutboxSent() error = %v", err)
		}

		got, err := s.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListUnsentOutboxMessages() = %d rows, want 2 (sent row excluded)", len(got))
		}
		if got[0].ID != older || got[1].ID != newer {
			t.Fatalf("ListUnsentOutboxMessages() ids = [%d, %d], want [%d, %d] oldest first", got[0].ID, got[1].ID, older, newer)
		}
	})
}

func TestStore_IncrementOutboxAttempt(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	id, err := s.EnqueueOutboxMessage(t.Context(), time.Now(), "hello")
	if err != nil {
		t.Fatalf("EnqueueOutboxMessage() error = %v", err)
	}

	if err := s.IncrementOutboxAttempt(t.Context(), id, "dial tcp: timeout"); err != nil {
		t.Fatalf("IncrementOutboxAttempt() error = %v", err)
	}

	unsent, err := s.ListUnsentOutboxMessages(t.Context())
	if err != nil {
		t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
	}
	if len(unsent) != 1 {
		t.Fatalf("ListUnsentOutboxMessages() = %d rows, want the row to remain unsent", len(unsent))
	}
	if unsent[0].Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", unsent[0].Attempts)
	}
	if unsent[0].LastError != "dial tcp: timeout" {
		t.Fatalf("LastError = %q, want %q", unsent[0].LastError, "dial tcp: timeout")
	}
}

func TestStore_MarkOutboxSent(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	id, err := s.EnqueueOutboxMessage(t.Context(), time.Now(), "hello")
	if err != nil {
		t.Fatalf("EnqueueOutboxMessage() error = %v", err)
	}

	if err := s.MarkOutboxSent(t.Context(), id, time.Now()); err != nil {
		t.Fatalf("MarkOutboxSent() error = %v", err)
	}

	unsent, err := s.ListUnsentOutboxMessages(t.Context())
	if err != nil {
		t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
	}
	if len(unsent) != 0 {
		t.Fatalf("ListUnsentOutboxMessages() = %d rows, want 0 after mark-sent", len(unsent))
	}
}

func TestStore_Name(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	if got := s.Name(); got != "sqlite" {
		t.Fatalf("Name() = %q, want %q", got, "sqlite")
	}
}

func TestStore_NewestRunStartedAt(t *testing.T) {
	t.Run("no runs signals none, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		_, ok, err := s.NewestRunStartedAt(t.Context())
		if err != nil {
			t.Fatalf("NewestRunStartedAt() error = %v, want nil", err)
		}
		if ok {
			t.Fatal("NewestRunStartedAt() ok = true, want false with no runs")
		}
	})

	t.Run("returns the most recently inserted run regardless of its action", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		older := now.Add(-3 * time.Hour)
		newer := now.Add(-10 * time.Minute)

		if _, err := s.InsertRun(t.Context(), older, domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		insertReboot(t, s, now.Add(-2*time.Hour)) // a reboot in between must not win just by action
		if _, err := s.InsertRun(t.Context(), newer, domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		got, ok, err := s.NewestRunStartedAt(t.Context())
		if err != nil {
			t.Fatalf("NewestRunStartedAt() error = %v", err)
		}
		if !ok {
			t.Fatal("NewestRunStartedAt() ok = false, want true")
		}
		if !got.Equal(newer) {
			t.Fatalf("NewestRunStartedAt() = %v, want the most recently inserted run %v", got, newer)
		}
	})
}

func TestStore_RolloverToArchive(t *testing.T) {
	t.Run("an aged run with every child type moves wholly to archive; a fresh run is untouched", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		oldRunID, err := s.InsertRun(t.Context(), old, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun(old) error = %v", err)
		}
		insertCheckAt(t, s, oldRunID, old)
		saveMetricsAt(t, s, oldRunID, old, "old-host")
		if err := s.SavePings(t.Context(), oldRunID, old, []domain.PingResult{{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 10, OK: true}}); err != nil {
			t.Fatalf("SavePings(old) error = %v", err)
		}

		freshRunID, err := s.InsertRun(t.Context(), now, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun(fresh) error = %v", err)
		}
		insertCheckAt(t, s, freshRunID, now)

		moved, err := s.RolloverToArchive(t.Context(), now, 30)
		if err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}
		if moved != 1 {
			t.Fatalf("RolloverToArchive() moved = %d, want 1 (only the aged run, issue #4 FIX 5)", moved)
		}

		// the old run is wholly gone from hot...
		if _, err := s.GetRun(t.Context(), oldRunID); err == nil {
			t.Fatal("GetRun(old run) after rollover error = nil, want it gone from hot")
		}
		if _, err := s.GetHost(t.Context(), oldRunID); err == nil {
			t.Fatal("GetHost(old run) after rollover error = nil, want it gone from hot")
		}
		if remaining, err := s.ListMetricsByRun(t.Context(), oldRunID); err != nil || len(remaining) != 0 {
			t.Fatalf("ListMetricsByRun(old run) = %v (err=%v), want 0 rows left in hot", remaining, err)
		}
		if remaining, err := s.ListPingsByRun(t.Context(), oldRunID); err != nil || len(remaining) != 0 {
			t.Fatalf("ListPingsByRun(old run) = %v (err=%v), want 0 rows left in hot", remaining, err)
		}

		// ...and wholly present in archive, reachable via the spanning reads.
		archivedRun, found, err := s.RunByID(t.Context(), oldRunID)
		if err != nil {
			t.Fatalf("RunByID(old run) error = %v", err)
		}
		if !found {
			t.Fatal("RunByID(old run) found = false, want true (archived)")
		}
		if archivedRun.ID != oldRunID {
			t.Fatalf("RunByID(old run).ID = %s, want %s", archivedRun.ID, oldRunID)
		}
		archivedChecks, err := s.ListChecksByRun(t.Context(), oldRunID)
		if err != nil {
			t.Fatalf("ListChecksByRun(old run) error = %v", err)
		}
		if len(archivedChecks) != 1 {
			t.Fatalf("ListChecksByRun(old run) = %d rows, want 1 (from checks_archive)", len(archivedChecks))
		}

		// the fresh run is untouched.
		if _, err := s.GetRun(t.Context(), freshRunID); err != nil {
			t.Fatalf("GetRun(fresh run) after rollover error = %v, want it still in hot", err)
		}
		if remaining, err := s.ListChecksByRun(t.Context(), freshRunID); err != nil || len(remaining) != 1 {
			t.Fatalf("ListChecksByRun(fresh run) = %v (err=%v), want 1 row still in hot", remaining, err)
		}
	})

	t.Run("a run moves as a whole even when a child's own ts is inside the window", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		oldStartedAt := now.Add(-40 * 24 * time.Hour) // outside a 30-day window

		runID, err := s.InsertRun(t.Context(), oldStartedAt, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		// the check's own ts is recent (inside the window) — rollover must
		// key off the run's started_at only, never a child's own ts (the
		// old, replaced Prune* model did the latter).
		insertCheckAt(t, s, runID, now.Add(-time.Minute))

		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		if _, err := s.GetRun(t.Context(), runID); err == nil {
			t.Fatal("GetRun() after rollover error = nil, want the aged run gone from hot")
		}
		remaining, err := s.ListChecksByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListChecksByRun() error = %v", err)
		}
		if len(remaining) != 1 {
			t.Fatalf("ListChecksByRun() after rollover = %d rows, want 1 (found via the archive leg, travelling with its run)", len(remaining))
		}
	})

	t.Run("re-running with nothing newly aged moves zero rows and returns nil", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)

		runID, err := s.InsertRun(t.Context(), now, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		firstMoved, err := s.RolloverToArchive(t.Context(), now, 30)
		if err != nil {
			t.Fatalf("first RolloverToArchive() error = %v", err)
		}
		if firstMoved != 0 {
			t.Fatalf("first RolloverToArchive() moved = %d, want 0 (issue #4 FIX 5: the run is fresh, not aged)", firstMoved)
		}
		secondMoved, err := s.RolloverToArchive(t.Context(), now, 30)
		if err != nil {
			t.Fatalf("second RolloverToArchive() error = %v, want idempotent-in-effect", err)
		}
		if secondMoved != 0 {
			t.Fatalf("second RolloverToArchive() moved = %d, want 0", secondMoved)
		}

		if _, err := s.GetRun(t.Context(), runID); err != nil {
			t.Fatalf("GetRun() after two no-op rollovers error = %v, want the fresh run still in hot", err)
		}
	})

	t.Run("hotWindowDays<=0 is a no-op", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		old := time.Now().UTC().Add(-1000 * 24 * time.Hour)

		runID, err := s.InsertRun(t.Context(), old, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		for _, days := range []int{0, -5} {
			moved, err := s.RolloverToArchive(t.Context(), time.Now(), days)
			if err != nil {
				t.Fatalf("RolloverToArchive(days=%d) error = %v, want nil (no-op)", days, err)
			}
			if moved != 0 {
				t.Fatalf("RolloverToArchive(days=%d) moved = %d, want 0 (issue #4 FIX 5 no-op)", days, moved)
			}
		}

		if _, err := s.GetRun(t.Context(), runID); err != nil {
			t.Fatalf("GetRun() after hotWindowDays<=0 error = %v, want the ancient run still in hot (no-op)", err)
		}
	})

	t.Run("tbot_queue is never touched", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		if _, err := s.InsertRun(t.Context(), old, domain.ModeSoft, nil); err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		if _, err := s.EnqueueOutboxMessage(t.Context(), old, "queued before rollover"); err != nil {
			t.Fatalf("EnqueueOutboxMessage() error = %v", err)
		}

		before, err := s.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
		}

		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		after, err := s.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages() error = %v", err)
		}
		if len(after) != len(before) {
			t.Fatalf("tbot_queue rows after rollover = %d, want unchanged %d", len(after), len(before))
		}
	})

	t.Run("a forced failure mid-move rolls back the whole transaction, leaving hot intact", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		runID, err := s.InsertRun(t.Context(), old, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		insertCheckAt(t, s, runID, old)

		// read back the check's real (UUIDv7) id — domain.Check carries no
		// id field of its own, so this is a raw query, not something
		// obtainable through the public API — so we can seed a colliding row
		// with that exact value directly in checks_archive: the rollover's
		// INSERT...SELECT (which preserves the hot row's id) then hits a
		// primary key violation partway through, forcing a rollback we can
		// observe.
		var checkID string
		checkIDQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, colChecksID, tableChecks, colChecksRunID)
		if err := s.db.QueryRowContext(t.Context(), checkIDQuery, runID).Scan(&checkID); err != nil {
			t.Fatalf("read back check id: %v", err)
		}

		seedQuery := fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			tableChecksArchive, colChecksID, colChecksRunID, colChecksTS, colChecksPhase,
			colChecksTarget, colChecksKind, colChecksOK, colChecksLatencyMS, colChecksError)
		if _, err := s.db.ExecContext(t.Context(), seedQuery, checkID, "nonexistent-run", formatTime(old), domain.PhaseInitial, "collision", domain.CheckKindIP, 1, nil, nil); err != nil {
			t.Fatalf("seed colliding archive row: %v", err)
		}

		if _, err := s.RolloverToArchive(t.Context(), now, 30); err == nil {
			t.Fatal("RolloverToArchive() error = nil, want the seeded id collision to fail the transaction")
		}

		// hot must be untouched: the run and its check must still be there.
		if _, err := s.GetRun(t.Context(), runID); err != nil {
			t.Fatalf("GetRun(%s) after failed rollover error = %v, want the run still in hot (tx rolled back)", runID, err)
		}
		remaining, err := s.ListChecksByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListChecksByRun() error = %v", err)
		}
		if len(remaining) != 1 {
			t.Fatalf("ListChecksByRun() = %d rows, want 1 (the check must still be in hot, untouched by the rolled-back tx)", len(remaining))
		}
	})
}

func TestStore_PruneArchive(t *testing.T) {
	t.Run("deletes aged archived runs and their children, keeps younger archived runs and all hot rows untouched", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)

		ancientRunID, err := s.InsertRun(t.Context(), now.Add(-200*24*time.Hour), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun(ancient) error = %v", err)
		}
		insertCheckAt(t, s, ancientRunID, now.Add(-200*24*time.Hour))
		saveMetricsAt(t, s, ancientRunID, now.Add(-200*24*time.Hour), "ancient-host")
		if err := s.SavePings(t.Context(), ancientRunID, now.Add(-200*24*time.Hour), []domain.PingResult{{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 10, OK: true}}); err != nil {
			t.Fatalf("SavePings(ancient) error = %v", err)
		}

		recentArchivedRunID, err := s.InsertRun(t.Context(), now.Add(-40*24*time.Hour), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun(recent archived) error = %v", err)
		}
		insertCheckAt(t, s, recentArchivedRunID, now.Add(-40*24*time.Hour))

		hotRunID, err := s.InsertRun(t.Context(), now, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun(hot) error = %v", err)
		}
		insertCheckAt(t, s, hotRunID, now)

		// move both aged runs into the archive first — PruneArchive only
		// ever touches *_archive tables.
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		pruned, err := s.PruneArchive(t.Context(), now, 90)
		if err != nil {
			t.Fatalf("PruneArchive() error = %v", err)
		}
		if pruned != 1 {
			t.Fatalf("PruneArchive() pruned = %d, want 1 (only the ancient run, issue #4 FIX 5)", pruned)
		}

		// the ancient archived run (and every child table) is gone...
		if _, found, err := s.RunByID(t.Context(), ancientRunID); err != nil || found {
			t.Fatalf("RunByID(ancient) = found=%v err=%v, want found=false after PruneArchive", found, err)
		}
		if _, err := s.GetHost(t.Context(), ancientRunID); err == nil {
			t.Fatal("GetHost(ancient) error = nil, want the archived host row pruned")
		}
		if remaining, err := s.ListChecksByRun(t.Context(), ancientRunID); err != nil || len(remaining) != 0 {
			t.Fatalf("ListChecksByRun(ancient) = %v (err=%v), want 0 rows after PruneArchive", remaining, err)
		}
		if remaining, err := s.ListMetricsByRun(t.Context(), ancientRunID); err != nil || len(remaining) != 0 {
			t.Fatalf("ListMetricsByRun(ancient) = %v (err=%v), want 0 rows after PruneArchive", remaining, err)
		}
		if remaining, err := s.ListPingsByRun(t.Context(), ancientRunID); err != nil || len(remaining) != 0 {
			t.Fatalf("ListPingsByRun(ancient) = %v (err=%v), want 0 rows after PruneArchive", remaining, err)
		}

		// ...the younger archived run survives...
		if _, found, err := s.RunByID(t.Context(), recentArchivedRunID); err != nil || !found {
			t.Fatalf("RunByID(recent archived) = found=%v err=%v, want found=true (younger than retention)", found, err)
		}

		// ...and every hot row is untouched.
		if _, found, err := s.RunByID(t.Context(), hotRunID); err != nil || !found {
			t.Fatalf("RunByID(hot) = found=%v err=%v, want found=true", found, err)
		}
	})

	t.Run("retention zero keeps the archive forever", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)

		ancientRunID, err := s.InsertRun(t.Context(), now.Add(-1000*24*time.Hour), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		insertCheckAt(t, s, ancientRunID, now.Add(-1000*24*time.Hour))

		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		pruned, err := s.PruneArchive(t.Context(), now, 0)
		if err != nil {
			t.Fatalf("PruneArchive() error = %v", err)
		}
		if pruned != 0 {
			t.Fatalf("PruneArchive() pruned = %d, want 0 (issue #4 FIX 5: retentionDays<=0 is a no-op)", pruned)
		}

		if _, found, err := s.RunByID(t.Context(), ancientRunID); err != nil || !found {
			t.Fatalf("RunByID(ancient) = found=%v err=%v, want found=true (RETENTION_DAYS=0 keeps forever)", found, err)
		}
	})
}

func TestStore_MaybeVacuum(t *testing.T) {
	t.Run("first call (no meta row) vacuums, reports ran=true, and stamps last_vacuum_at", func(t *testing.T) {
		t.Parallel()

		s := newFileTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)

		ran, err := s.MaybeVacuum(t.Context(), now)
		if err != nil {
			t.Fatalf("MaybeVacuum() error = %v", err)
		}
		if !ran {
			t.Fatal("MaybeVacuum() ran = false, want true on the first-ever call")
		}

		got, ok := readMetaLastVacuum(t, s)
		if !ok {
			t.Fatal("meta[last_vacuum_at] not set after first MaybeVacuum()")
		}
		if !got.Equal(now) {
			t.Fatalf("meta[last_vacuum_at] = %v, want %v", got, now)
		}
	})

	t.Run("a call within vacuumInterval reports ran=false and leaves the stamp untouched", func(t *testing.T) {
		t.Parallel()

		s := newFileTestStore(t)
		first := time.Now().UTC().Truncate(time.Second)
		if ran, err := s.MaybeVacuum(t.Context(), first); err != nil || !ran {
			t.Fatalf("first MaybeVacuum() = ran=%v err=%v, want ran=true err=nil", ran, err)
		}

		second := first.Add(vacuumInterval / 2)
		ran, err := s.MaybeVacuum(t.Context(), second)
		if err != nil {
			t.Fatalf("second MaybeVacuum() error = %v", err)
		}
		if ran {
			t.Fatal("MaybeVacuum() ran = true, want false (still within vacuumInterval)")
		}

		got, ok := readMetaLastVacuum(t, s)
		if !ok {
			t.Fatal("meta[last_vacuum_at] missing after second MaybeVacuum()")
		}
		if !got.Equal(first) {
			t.Fatalf("meta[last_vacuum_at] = %v, want unchanged %v (still within vacuumInterval)", got, first)
		}
	})

	t.Run("a call after vacuumInterval reports ran=true again and updates the stamp", func(t *testing.T) {
		t.Parallel()

		s := newFileTestStore(t)
		first := time.Now().UTC().Truncate(time.Second)
		if ran, err := s.MaybeVacuum(t.Context(), first); err != nil || !ran {
			t.Fatalf("first MaybeVacuum() = ran=%v err=%v, want ran=true err=nil", ran, err)
		}

		later := first.Add(vacuumInterval + time.Hour)
		ran, err := s.MaybeVacuum(t.Context(), later)
		if err != nil {
			t.Fatalf("second MaybeVacuum() error = %v", err)
		}
		if !ran {
			t.Fatal("MaybeVacuum() ran = false, want true once vacuumInterval has elapsed")
		}

		got, ok := readMetaLastVacuum(t, s)
		if !ok {
			t.Fatal("meta[last_vacuum_at] missing after third call")
		}
		if !got.Equal(later) {
			t.Fatalf("meta[last_vacuum_at] = %v, want updated to %v", got, later)
		}
	})

	t.Run("a closed store's VACUUM fails with an error and reports ran=false", func(t *testing.T) {
		t.Parallel()

		s := newFileTestStore(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		ran, err := s.MaybeVacuum(t.Context(), time.Now())
		if err == nil {
			t.Fatal("MaybeVacuum() on a closed store error = nil, want an error")
		}
		if ran {
			t.Fatal("MaybeVacuum() ran = true, want false when VACUUM itself failed")
		}
	})
}

func TestStore_RunByID(t *testing.T) {
	t.Run("an existing id is found", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		id, err := s.InsertRun(t.Context(), time.Now(), domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		run, found, err := s.RunByID(t.Context(), id)
		if err != nil {
			t.Fatalf("RunByID() error = %v", err)
		}
		if !found {
			t.Fatal("RunByID() found = false, want true")
		}
		if run.ID != id {
			t.Fatalf("RunByID().ID = %s, want %s", run.ID, id)
		}
	})

	t.Run("a missing id reports found=false, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)

		run, found, err := s.RunByID(t.Context(), "does-not-exist")
		if err != nil {
			t.Fatalf("RunByID() error = %v, want nil", err)
		}
		if found {
			t.Fatal("RunByID() found = true, want false")
		}
		if run != (domain.Run{}) {
			t.Fatalf("RunByID() run = %+v, want the zero value", run)
		}
	})

	t.Run("an archived id is found", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		now := time.Now().UTC().Truncate(time.Second)
		old := now.Add(-40 * 24 * time.Hour)

		id, err := s.InsertRun(t.Context(), old, domain.ModeSoft, nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}
		if _, err := s.RolloverToArchive(t.Context(), now, 30); err != nil {
			t.Fatalf("RolloverToArchive() error = %v", err)
		}

		run, found, err := s.RunByID(t.Context(), id)
		if err != nil {
			t.Fatalf("RunByID() error = %v", err)
		}
		if !found {
			t.Fatal("RunByID() found = false, want true for an archived run")
		}
		if run.ID != id {
			t.Fatalf("RunByID().ID = %s, want %s", run.ID, id)
		}
	})

	t.Run("a real query error propagates", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		_, found, err := s.RunByID(t.Context(), "1")
		if err == nil {
			t.Fatal("RunByID() on a closed store error = nil, want an error")
		}
		if found {
			t.Fatal("RunByID() found = true, want false on error")
		}
	})
}

func TestStore_UpdateRun(t *testing.T) {
	t.Run("applies only the non-nil fields", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		id, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		rebootStartedAt := time.Now().UTC().Truncate(time.Second)
		action := domain.ActionReboot
		if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{
			Action:          &action,
			RebootStartedAt: &rebootStartedAt,
		}); err != nil {
			t.Fatalf("UpdateRun() error = %v", err)
		}

		run, err := s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if run.Action != domain.ActionReboot {
			t.Fatalf("Action = %q, want %q", run.Action, domain.ActionReboot)
		}
		if run.RebootStartedAt == nil || !run.RebootStartedAt.Equal(rebootStartedAt) {
			t.Fatalf("RebootStartedAt = %v, want %v", run.RebootStartedAt, rebootStartedAt)
		}
		if run.FinishedAt != nil || run.Outcome != "" {
			t.Fatalf("fields not passed in the update changed: FinishedAt=%v Outcome=%q", run.FinishedAt, run.Outcome)
		}

		finishedAt := rebootStartedAt.Add(5 * time.Minute)
		outcome := "ok"
		if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{
			FinishedAt: &finishedAt,
			Outcome:    &outcome,
		}); err != nil {
			t.Fatalf("second UpdateRun() error = %v", err)
		}

		run, err = s.GetRun(t.Context(), id)
		if err != nil {
			t.Fatalf("GetRun() after second update error = %v", err)
		}
		if run.Outcome != "ok" {
			t.Fatalf("Outcome = %q, want %q", run.Outcome, "ok")
		}
		if run.FinishedAt == nil || !run.FinishedAt.Equal(finishedAt) {
			t.Fatalf("FinishedAt = %v, want %v", run.FinishedAt, finishedAt)
		}
		// the first update's fields must survive the second, narrower update.
		if run.Action != domain.ActionReboot {
			t.Fatalf("Action = %q, want the earlier update's %q to still hold", run.Action, domain.ActionReboot)
		}
	})

	t.Run("no fields set is a no-op, not an error", func(t *testing.T) {
		t.Parallel()

		s := newTestStore(t)
		id, err := s.InsertRun(t.Context(), time.Now(), "soft", nil)
		if err != nil {
			t.Fatalf("InsertRun() error = %v", err)
		}

		if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{}); err != nil {
			t.Fatalf("UpdateRun(empty) error = %v, want nil", err)
		}
	})
}

// insertCheckAt is a test helper inserting a single passing ip check on
// runID at ts, for retention tests that only care about timestamps.
func insertCheckAt(t *testing.T, s *Store, runID string, ts time.Time) {
	t.Helper()

	err := s.InsertCheck(t.Context(), domain.Check{
		RunID:  runID,
		TS:     ts,
		Phase:  domain.PhaseInitial,
		Target: "1.1.1.1:443",
		Kind:   domain.CheckKindIP,
		OK:     true,
	})
	if err != nil {
		t.Fatalf("InsertCheck() error = %v", err)
	}
}

// insertMetricAt is a test helper for TestStore_ListMetrics/TestStore_LatestMetrics:
// it inserts a fresh run and saves a single metrics sample for it at ts,
// returning the new run's id. Each call needs its own run since host.run_id
// is a PRIMARY KEY (SaveMetrics can only be called once per run) — unlike
// saveMetricsAt, this helper lets a test choose the sample's collector,
// name, value, and ok, which the fixed-collector saveMetricsAt does not.
func insertMetricAt(t *testing.T, s *Store, ts time.Time, collector domain.Collector, name string, value float64, ok bool) string {
	t.Helper()

	runID, err := s.InsertRun(t.Context(), ts, domain.ModeSoft, nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	sample := domain.MetricSample{Collector: collector, Name: name, Value: value, Unit: "unit", OK: ok}
	if !ok {
		sample.Error = "unavailable"
	}
	m := domain.HostMetrics{
		Host:    domain.HostInfo{Hostname: "host", OS: "linux", Arch: "arm64"},
		Samples: []domain.MetricSample{sample},
	}
	if err := s.SaveMetrics(t.Context(), runID, ts, m); err != nil {
		t.Fatalf("SaveMetrics() error = %v", err)
	}

	return runID
}

// insertPingAt is a test helper for TestStore_ListPings/TestStore_PruneArchive:
// it inserts a fresh run and saves a single ping result for it at ts,
// returning the new run's id. errStr is stored verbatim (empty means no
// error); ok is derived from received>0, mirroring SavePings/ListPings'
// own contract.
func insertPingAt(t *testing.T, s *Store, ts time.Time, host string, sent, received int, avgMS float64, errStr string) string {
	t.Helper()

	runID, err := s.InsertRun(t.Context(), ts, domain.ModeSoft, nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	result := domain.PingResult{Host: host, Sent: sent, Received: received, AvgMS: avgMS, OK: received > 0, Error: errStr}
	if err := s.SavePings(t.Context(), runID, ts, []domain.PingResult{result}); err != nil {
		t.Fatalf("SavePings() error = %v", err)
	}

	return runID
}

// insertReboot is a test helper inserting a runs row with action='reboot'
// and reboot_started_at=at, for GetLastRebootStartedAt tests.
func insertReboot(t *testing.T, s *Store, at time.Time) string {
	t.Helper()
	return insertRunWithAction(t, s, domain.ActionReboot, at)
}

// insertRunWithAction is a test helper inserting a runs row whose action
// and reboot_started_at are set directly via UpdateRun, since InsertRun
// always starts a row at action='none'.
func insertRunWithAction(t *testing.T, s *Store, action string, rebootStartedAt time.Time) string {
	t.Helper()

	id, err := s.InsertRun(t.Context(), rebootStartedAt, "soft", nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	if err := s.UpdateRun(t.Context(), id, domain.RunUpdate{
		Action:          &action,
		RebootStartedAt: &rebootStartedAt,
	}); err != nil {
		t.Fatalf("UpdateRun() error = %v", err)
	}

	return id
}

// maxIDAcross returns the lexicographically greatest id across every table
// listed (each queried directly via its own "SELECT MAX(id) FROM <table>",
// bypassing seedUUIDv7WatermarkFromDB entirely so it can serve as an
// independent ground truth for TestOpenStore's restart-collision subtest),
// or "" if none of them have any rows. Canonical UUIDv7 ids sort
// byte-for-byte exactly as they sort chronologically (uuid.go's newUUIDv7
// doc), so a plain string comparison across tables is exactly "the true
// watermark" seedUUIDv7WatermarkFromDB is supposed to converge on.
func maxIDAcross(t *testing.T, s *Store, tables ...string) string {
	t.Helper()

	var maxID string
	for _, table := range tables {
		var id sql.NullString
		if err := s.db.QueryRowContext(t.Context(), fmt.Sprintf(`SELECT MAX(id) FROM %s`, table)).Scan(&id); err != nil {
			t.Fatalf("SELECT MAX(id) FROM %s: %v", table, err)
		}
		if id.Valid && id.String > maxID {
			maxID = id.String
		}
	}
	return maxID
}

// newFileTestStore opens a fresh file-backed Store under t.TempDir() and
// closes it on cleanup, for tests that need a real file on disk — VACUUM has
// nothing to shrink against newTestStore's :memory: default.
func newFileTestStore(t *testing.T) *Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "yarddog.db")
	s, err := NewStore(t.Context(), path)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	return s
}

// newTestStore opens a fresh :memory: Store for a single test and closes
// it on cleanup. Each call gets its own connection pool (capped at one
// connection, see NewStore) so parallel subtests never share state.
func newTestStore(t *testing.T) *Store {
	t.Helper()

	s, err := NewStore(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	return s
}

// readMetaLastVacuum reads meta[last_vacuum_at] directly (bypassing any
// Store method) so TestStore_MaybeVacuum can assert what MaybeVacuum wrote
// without depending on a second reader method that exists only for tests.
func readMetaLastVacuum(t *testing.T, s *Store) (time.Time, bool) {
	t.Helper()

	var raw string
	err := s.db.QueryRowContext(t.Context(),
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, colMetaValue, tableMeta, colMetaKey),
		metaKeyLastVacuum,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false
		}
		t.Fatalf("read meta last_vacuum_at: %v", err)
	}

	ts, err := parseTime(raw)
	if err != nil {
		t.Fatalf("parse meta last_vacuum_at: %v", err)
	}
	return ts, true
}

// reopenAndMintTelemetry simulates one cron invocation after a process
// restart, for TestOpenStore's "reopening across repeated restarts..."
// subtest: it resets the shared package-level UUIDv7 watermark to a blank
// slate (forceUUIDv7State), reopens the store at path — exercising
// OpenStore's seedUUIDv7WatermarkFromDB exactly as a fresh cron process
// would — and mints one run plus its usual checks/metrics/pings children,
// all stamped with ts. It fails the test immediately on any insert error.
// It returns the reopened *Store (left open; the caller closes it once done
// so the next simulated restart can reopen the same file) and the run's own
// id: the run is always this function's very first mint, so — unlike the
// cumulative max after every child is also inserted — comparing it directly
// against the true previous maximum is what actually exposes a
// runs-only-seeded watermark's undershoot (see the subtest's own comment).
func reopenAndMintTelemetry(t *testing.T, path string, ts time.Time) (*Store, string) {
	t.Helper()

	forceUUIDv7State(t, 0, 0) // a fresh OS process's blank watermark: only the reopen below can raise it
	s, err := NewStore(t.Context(), path)
	if err != nil {
		t.Fatalf("NewStore(%s) error = %v", path, err)
	}

	runID, err := s.InsertRun(t.Context(), ts, domain.ModeSoft, nil)
	if err != nil {
		t.Fatalf("InsertRun() error = %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.InsertCheck(t.Context(), domain.Check{
			RunID:  runID,
			TS:     ts,
			Phase:  domain.PhaseInitial,
			Target: fmt.Sprintf("10.0.0.%d:443", i),
			Kind:   domain.CheckKindIP,
			OK:     true,
		}); err != nil {
			t.Fatalf("InsertCheck() #%d error = %v", i, err)
		}
	}

	m := domain.HostMetrics{
		Host: domain.HostInfo{Hostname: "pi5", OS: "linux", Arch: "arm64"},
		Samples: []domain.MetricSample{
			{Collector: domain.CollectorCPU, Name: "load1", Value: 0.1, Unit: "load", OK: true},
			{Collector: domain.CollectorMemory, Name: "used_ratio", Value: 0.2, Unit: "ratio", OK: true},
		},
	}
	if err := s.SaveMetrics(t.Context(), runID, ts, m); err != nil {
		t.Fatalf("SaveMetrics() error = %v", err)
	}

	if err := s.SavePings(t.Context(), runID, ts, []domain.PingResult{
		{Host: "1.1.1.1", Sent: 5, Received: 5, AvgMS: 10, OK: true},
	}); err != nil {
		t.Fatalf("SavePings() error = %v", err)
	}

	return s, runID
}

// saveMetricsAt is a test helper saving a one-sample telemetry snapshot for
// runID at ts, for prune tests that only care about timestamps and readback.
func saveMetricsAt(t *testing.T, s *Store, runID string, ts time.Time, hostname string) {
	t.Helper()

	m := domain.HostMetrics{
		Host:    domain.HostInfo{Hostname: hostname, OS: "linux", Arch: "arm64"},
		Samples: []domain.MetricSample{{Collector: domain.CollectorUptime, Name: "uptime", Value: 1, Unit: "seconds", OK: true}},
	}
	if err := s.SaveMetrics(t.Context(), runID, ts, m); err != nil {
		t.Fatalf("SaveMetrics() error = %v", err)
	}
}
