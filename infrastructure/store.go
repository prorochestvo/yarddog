package infrastructure

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"

	"modernc.org/sqlite"
)

// NewStore opens the SQLite database at path with a single-connection pool
// and self-healing enabled — it is OpenStore(ctx, path, 1, true). The
// collector and every :memory: test in this codebase use NewStore; the
// daemon calls OpenStore directly with its own configured pool size and
// selfHeal false (see OpenStore's own doc for why).
func NewStore(ctx context.Context, path string) (*Store, error) {
	return OpenStore(ctx, path, 1, true)
}

// OpenStore opens (creating if absent) the SQLite database at path, applies
// the WAL/busy_timeout pragmas, and idempotently migrates the schema.
// maxOpenConns<1 is clamped to 1 (a floor, never an error): a pool of zero
// would mean sql.DB accepts no connections at all. path may be ":memory:"
// for tests: an in-memory database lives entirely on its first connection, so
// callers must never pair ":memory:" with maxOpenConns>1 — a second
// connection would see a blank, unmigrated database. A file-backed database
// under WAL has no such restriction, which is what the daemon's
// DAEMON_MAX_CONNS read pool relies on. A file-backed database is chmod'd to
// 0600 after migration (and so are its WAL -wal/-shm siblings): it holds
// router credentials' error strings and must sit at the same permission bar
// as the lock file and the .env file, not the umask default sql.Open would
// otherwise leave it at.
//
// Self-healing (issue #4 FIX 3) is gated behind selfHeal, granted only to
// the database's sole writer: a file that fails to open/migrate with a
// SQLITE_CORRUPT/SQLITE_NOTADB error, or a non-empty database whose
// meta.schema_version predates currentSchemaVersion (a database written
// before runs/checks/metrics/pings switched from autoincrement integers to
// UUIDv7 strings — migrate's CREATE TABLE IF NOT EXISTS silently no-ops
// against such a table, so every InsertRun would otherwise fail "datatype
// mismatch" forever), is quarantined aside (renamed, never deleted) and a
// fresh database is created at path in its place — but only when selfHeal is
// true. NewStore always passes true: the collector reaches it only after
// taking its exclusive flock, and the collector alone is the database's
// documented sole writer, so it alone may safely rename the live file aside
// and recreate it. The daemon calls OpenStore directly with selfHeal false:
// it is a read-only reader with no lock of its own, and systemd can start it
// before cron ever fires the collector — on a real upgrade, the daemon would
// otherwise be the process that first finds a pre-issue-4 or corrupt file
// and quarantines the operator's real history before the collector ever
// gets a chance to migrate it, then serves an empty database. With selfHeal
// false, both branches below instead surface the underlying problem as a
// plain error and leave the file untouched. The daemon maps that error to
// domain.ExitConfigError, so systemd restarts it until the collector's own
// next run has quarantined and recreated the database — a brief flap on
// first upgrade, no data destroyed, which is the intended behaviour.
func OpenStore(ctx context.Context, path string, maxOpenConns int, selfHeal bool) (*Store, error) {
	s, err := openOnce(ctx, path, maxOpenConns)
	if err != nil {
		if isMemoryDB(path) || !isSQLiteCorruptionError(err) || !selfHeal {
			return nil, err
		}
		log.Printf("yarddog: %s: %v — quarantining and starting a fresh database", path, err)
		if qErr := quarantineFile(path, "corrupt", time.Now()); qErr != nil {
			return nil, fmt.Errorf("%w (also failed to quarantine it: %v)", err, qErr)
		}
		if s, err = openOnce(ctx, path, maxOpenConns); err != nil {
			return nil, fmt.Errorf("open store after quarantining a corrupt database: %w", err)
		}
	}

	incompatible, err := s.needsQuarantine(ctx)
	if err != nil {
		if closeErr := s.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w (also failed to close db: %v)", err, closeErr)
		}
		return nil, err
	}
	if incompatible {
		if !selfHeal {
			err := fmt.Errorf("%w: the collector recreates it on its next run; this process starts serving once that has happened", errIncompatibleSchema)
			if closeErr := s.Close(); closeErr != nil {
				return nil, fmt.Errorf("%w (also failed to close db: %v)", err, closeErr)
			}
			return nil, err
		}
		if err := s.Close(); err != nil {
			return nil, fmt.Errorf("close pre-issue-4 database before quarantining: %w", err)
		}
		log.Printf("yarddog: %s predates the UUIDv7 schema — quarantining and starting a fresh database", path)
		if !isMemoryDB(path) {
			if err := quarantineFile(path, "incompatible", time.Now()); err != nil {
				return nil, fmt.Errorf("quarantine incompatible database: %w", err)
			}
		}
		if s, err = openOnce(ctx, path, maxOpenConns); err != nil {
			return nil, fmt.Errorf("open store after quarantining an incompatible database: %w", err)
		}
	}

	if err := s.ensureSchemaVersionStamped(ctx); err != nil {
		if closeErr := s.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w (also failed to close db: %v)", err, closeErr)
		}
		return nil, err
	}

	if err := s.seedUUIDv7WatermarkFromDB(ctx); err != nil {
		if closeErr := s.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w (also failed to close db: %v)", err, closeErr)
		}
		return nil, err
	}

	if !isMemoryDB(path) {
		if err := chmodStoreFiles(path); err != nil {
			if closeErr := s.Close(); closeErr != nil {
				return nil, fmt.Errorf("%w (also failed to close db: %v)", err, closeErr)
			}
			return nil, err
		}
	}

	return s, nil
}

// Store is the SQLite-backed persistence layer for runs, checks, host
// telemetry, pings, and the Telegram outbox (design §9;
// plans/003-host-telemetry.md; issue #2). It implements
// services.RunRepository, services.MetricsRepository, services.PingRepository,
// and services.OutboxRepository; main passes the one *Store into
// services.Execute for the run/metrics/ping repositories and into
// services.NewOutboxService as the outbox.
type Store struct {
	db *sql.DB
}

var (
	_ services.RunRepository     = (*Store)(nil)
	_ services.MetricsRepository = (*Store)(nil)
	_ services.PingRepository    = (*Store)(nil)
	_ services.OutboxRepository  = (*Store)(nil)
	_ services.HistoryRepository = (*Store)(nil)
	_ services.HealthProbe       = (*Store)(nil)
)

// CheckUP implements services.HealthProbe for the store: it reports the
// sqlite connection unhealthy unless both a plain connectivity ping and a
// real query succeed — a ping alone can stay green while queries are
// already failing.
func (s *Store) CheckUP(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite ping: %w", err)
	}

	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("sqlite query: %w", err)
	}

	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// EnqueueOutboxMessage inserts an unsent tbot_queue row carrying text and
// returns its id. Callers must persist before attempting to send (design
// §8.3) so a crash mid-send never loses a message.
func (s *Store) EnqueueOutboxMessage(ctx context.Context, createdAt time.Time, text string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, %s, %s) VALUES (?, ?, 0)`,
			tableOutbox, colOutboxCreatedAt, colOutboxText, colOutboxAttempts),
		formatTime(createdAt), text,
	)
	if err != nil {
		return 0, fmt.Errorf("enqueue outbox message: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("enqueue outbox message: last insert id: %w", err)
	}

	return id, nil
}

// GetHost reads back the host sidecar row for runID (a UUIDv7 string,
// issue #4). It exists so tests can verify what SaveMetrics persisted; it
// is off-port (not part of MetricsRepository) since the orchestrator never
// reads a snapshot back, mirroring GetRun's role for the runs table.
func (s *Store) GetHost(ctx context.Context, runID string) (domain.HostInfo, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s FROM %s WHERE %s = ?`,
			colHostHostname, colHostOS, colHostArch, tableHost, colHostRunID),
		runID,
	)

	var h domain.HostInfo
	if err := row.Scan(&h.Hostname, &h.OS, &h.Arch); err != nil {
		if err == sql.ErrNoRows {
			return domain.HostInfo{}, fmt.Errorf("get host for run %s: %w", runID, sql.ErrNoRows)
		}
		return domain.HostInfo{}, fmt.Errorf("get host for run %s: %w", runID, err)
	}

	return h, nil
}

// GetLastRebootStartedAt returns the reboot_started_at of the most recent
// runs row with action='reboot'. ok is false with a nil error when no such
// run exists yet, so callers never need to special-case a sentinel time.
func (s *Store) GetLastRebootStartedAt(ctx context.Context) (t time.Time, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ? AND %s IS NOT NULL ORDER BY %s DESC LIMIT 1`,
			colRunsRebootStartedAt, tableRuns, colRunsAction, colRunsRebootStartedAt, colRunsRebootStartedAt),
		domain.ActionReboot,
	)

	var raw string
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("get last reboot started at: %w", err)
	}

	parsed, err := parseTime(raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("get last reboot started at: %w", err)
	}

	return parsed, true, nil
}

// GetRun reads back the runs row id (a UUIDv7 string, issue #4) in full. It
// exists primarily so tests and any future inspection tooling can verify
// what InsertRun/UpdateRun persisted; the recovery loop itself only ever
// writes. It is off-port (not part of RunRepository) since the orchestrator
// never reads a run back. A missing id returns sql.ErrNoRows wrapped with
// %w, so callers (RunByID) can errors.Is against it.
func (s *Store) GetRun(ctx context.Context, id string) (domain.Run, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s FROM %s WHERE %s = ?`,
			colRunsID, colRunsStartedAt, colRunsMode, colRunsInternetOK, colRunsAction,
			colRunsRebootStartedAt, colRunsRouterDownAt, colRunsRouterUpAt,
			colRunsInternetRestoredAt, colRunsFinishedAt, colRunsOutcome, colRunsError,
			tableRuns, colRunsID),
		id,
	)

	r, err := scanRun(row)
	if err != nil {
		return domain.Run{}, fmt.Errorf("get run %s: %w", id, err)
	}
	return r, nil
}

// IncrementOutboxAttempt records a failed send attempt on the tbot_queue row
// id: attempts is incremented and last_error is set to err.
func (s *Store) IncrementOutboxAttempt(ctx context.Context, id int64, sendErr string) error {
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET %s = %s + 1, %s = ? WHERE %s = ?`,
			tableOutbox, colOutboxAttempts, colOutboxAttempts, colOutboxLastError, colOutboxID),
		sendErr, id,
	)
	if err != nil {
		return fmt.Errorf("increment outbox attempt %d: %w", id, err)
	}
	return nil
}

// InsertCheck inserts one checks row, generating its id as a UUIDv7 string
// stamped with c.TS (issue #4). c.RunID, c.Phase, c.Target, and c.Kind must
// be set; c.LatencyMS and c.Error are optional.
func (s *Store) InsertCheck(ctx context.Context, c domain.Check) error {
	id, err := newUUIDv7(c.TS)
	if err != nil {
		return fmt.Errorf("insert check for run %s target %s: %w", c.RunID, c.Target, err)
	}

	_, err = s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			tableChecks, colChecksID, colChecksRunID, colChecksTS, colChecksPhase, colChecksTarget,
			colChecksKind, colChecksOK, colChecksLatencyMS, colChecksError),
		id, c.RunID, formatTime(c.TS), c.Phase, c.Target, c.Kind, boolToInt(c.OK),
		nullInt64(c.LatencyMS), nullString(c.Error),
	)
	if err != nil {
		return fmt.Errorf("insert check for run %s target %s: %w", c.RunID, c.Target, err)
	}
	return nil
}

// InsertRun inserts a new runs row, generating its id as a UUIDv7 string
// stamped with startedAt (issue #4), and returns that id. mode is "soft" or
// "hard"; internetOK is nil in hard mode, where no initial check is made
// (design §7).
func (s *Store) InsertRun(ctx context.Context, startedAt time.Time, mode string, internetOK *bool) (string, error) {
	id, err := newUUIDv7(startedAt)
	if err != nil {
		return "", fmt.Errorf("insert run: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?)`,
			tableRuns, colRunsID, colRunsStartedAt, colRunsMode, colRunsInternetOK, colRunsAction),
		id, formatTime(startedAt), mode, nullBoolToInt(internetOK), domain.ActionNone,
	); err != nil {
		return "", fmt.Errorf("insert run: %w", err)
	}

	return id, nil
}

// LatestHost returns the host row of the most recently recorded run that has
// one. ok is false with a nil error when the host table has no rows yet (a
// fresh box before the collector's first METRICS_ENABLED run) — never a
// sql.ErrNoRows the caller has to know about.
func (s *Store) LatestHost(ctx context.Context) (domain.HostRecord, bool, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s FROM %s ORDER BY %s DESC LIMIT 1`,
			colHostRunID, colHostTS, colHostHostname, colHostOS, colHostArch,
			tableHost, colHostRunID),
	)

	var (
		rec domain.HostRecord
		ts  string
	)
	if err := row.Scan(&rec.RunID, &ts, &rec.Host.Hostname, &rec.Host.OS, &rec.Host.Arch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.HostRecord{}, false, nil
		}
		return domain.HostRecord{}, false, fmt.Errorf("latest host: %w", err)
	}

	parsed, err := parseTime(ts)
	if err != nil {
		return domain.HostRecord{}, false, fmt.Errorf("latest host: %w", err)
	}
	rec.TS = parsed

	return rec, true, nil
}

// LatestMetrics returns every metrics row of the newest run that has any
// ("newest run that has metrics" is not the same as "the newest run": a run
// with METRICS_ENABLED=false has no metrics rows at all, and the
// MAX(run_id) subquery skips straight past it). Empty, not an error, when
// the metrics table itself has no rows yet.
func (s *Store) LatestMetrics(ctx context.Context) ([]domain.MetricRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s, %s, %s, %s FROM %s WHERE %s = (SELECT MAX(%s) FROM %s) ORDER BY %s ASC`,
			colMetricsRunID, colMetricsTS, colMetricsCollector, colMetricsName,
			colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError,
			tableMetrics, colMetricsRunID, colMetricsRunID, tableMetrics, colMetricsID),
	)
	if err != nil {
		return nil, fmt.Errorf("latest metrics: %w", err)
	}
	defer rows.Close()

	out, err := scanMetricRecords(rows)
	if err != nil {
		return nil, fmt.Errorf("latest metrics: %w", err)
	}
	return out, nil
}

// ListChecksByRun returns every checks row for runID, ordered by id, so
// tests and any future report tooling see them in insertion order. It spans
// both tiers via UNION ALL: the run-boundary invariant guarantees a run's
// checks live wholly in one tier, so this never duplicates a row, and it
// sidesteps the ambiguity a "hot-first, archive-on-miss" lookup would have —
// a run can legitimately have zero checks (e.g. a failed hard-mode reboot),
// which looks identical to "this run is archived" under an empty result.
func (s *Store) ListChecksByRun(ctx context.Context, runID string) ([]domain.Check, error) {
	// id is selected only inside the inner legs (checksFullColumnList), not
	// by the outer projection (checksColumnList) — the outer ORDER BY can
	// still reference it because the outer SELECT is a simple query over a
	// derived (unioned) table, not itself compound.
	query := fmt.Sprintf(
		`SELECT %[1]s FROM (
			SELECT %[2]s FROM %[3]s WHERE %[4]s = ?
			UNION ALL
			SELECT %[2]s FROM %[5]s WHERE %[4]s = ?
		) ORDER BY %[6]s ASC`,
		checksColumnList, checksFullColumnList, tableChecks, colChecksRunID, tableChecksArchive, colChecksID,
	)

	rows, err := s.db.QueryContext(ctx, query, runID, runID)
	if err != nil {
		return nil, fmt.Errorf("list checks for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []domain.Check
	for rows.Next() {
		var (
			c         domain.Check
			ts        string
			ok        int64
			latencyMS sql.NullInt64
			errStr    sql.NullString
		)
		if err := rows.Scan(&c.RunID, &ts, &c.Phase, &c.Target, &c.Kind, &ok, &latencyMS, &errStr); err != nil {
			return nil, fmt.Errorf("list checks for run %s: scan: %w", runID, err)
		}
		c.TS, err = parseTime(ts)
		if err != nil {
			return nil, fmt.Errorf("list checks for run %s: %w", runID, err)
		}
		c.OK = ok != 0
		if latencyMS.Valid {
			v := latencyMS.Int64
			c.LatencyMS = &v
		}
		c.Error = errStr.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list checks for run %s: %w", runID, err)
	}

	return out, nil
}

// ListMetrics returns metrics history matching f, newest first, bounded by
// f.Limit (a parameterized LIMIT — never string-concatenated, Risk R8).
// f.Since's zero value omits the "ts >= ?"/"id >= ?" clauses; f.Collector's
// zero value omits the "collector = ?" clause. The query is hot-only: an
// f.Since older than the hot floor simply returns nothing older than hot
// holds (archive history is out of scope for the list endpoints).
func (s *Store) ListMetrics(ctx context.Context, f services.MetricsFilter) ([]domain.MetricRecord, error) {
	var (
		conds []string
		args  []any
	)
	if !f.Since.IsZero() {
		// uuidv7Floor gives the planner a sargable lower bound on the id
		// index so the scan can seek to the cutoff and stop early instead of
		// running to EOF. ts >= ? stays as the exact residual filter, since
		// uuidv7Floor's millisecond truncation can make it a hair less
		// restrictive than the real cutoff (issue #4 FIX 2).
		conds = append(conds, colMetricsID+" >= ?", colMetricsTS+" >= ?")
		args = append(args, uuidv7Floor(f.Since), formatTime(f.Since))
	}
	if f.Collector != "" {
		conds = append(conds, colMetricsCollector+" = ?")
		args = append(args, string(f.Collector))
	}
	if !f.IncludeEmpty {
		// drop unavailable rows in SQL so LIMIT counts only returned rows.
		conds = append(conds, colMetricsOK+" = 1")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	query := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s DESC LIMIT ?", metricsColumnList, tableMetrics, where, colMetricsID)
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list metrics: %w", err)
	}
	defer rows.Close()

	out, err := scanMetricRecords(rows)
	if err != nil {
		return nil, fmt.Errorf("list metrics: %w", err)
	}
	return out, nil
}

// ListMetricsByRun returns every metrics row for runID, ordered by id,
// mirroring ListChecksByRun. Off-port: it exists so tests can read back
// what SaveMetrics persisted.
func (s *Store) ListMetricsByRun(ctx context.Context, runID string) ([]domain.MetricSample, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s, %s FROM %s WHERE %s = ? ORDER BY %s ASC`,
			colMetricsCollector, colMetricsName, colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError,
			tableMetrics, colMetricsRunID, colMetricsID),
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list metrics for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []domain.MetricSample
	for rows.Next() {
		var (
			m      domain.MetricSample
			value  sql.NullFloat64
			ok     int64
			errStr sql.NullString
		)
		if err := rows.Scan(&m.Collector, &m.Name, &value, &m.Unit, &ok, &errStr); err != nil {
			return nil, fmt.Errorf("list metrics for run %s: scan: %w", runID, err)
		}
		m.Value = value.Float64
		m.OK = ok != 0
		m.Error = errStr.String
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list metrics for run %s: %w", runID, err)
	}

	return out, nil
}

// ListPings returns ping history matching f, newest first, bounded by
// f.Limit (a parameterized LIMIT — never string-concatenated, Risk R8).
// f.Since's zero value omits the "ts >= ?"/"id >= ?" clauses; f.Host's zero
// value omits the "host = ?" clause; f.IncludeUnreachable false additionally
// drops received=0 rows in SQL so LIMIT counts only returned rows.
// The query is hot-only: an f.Since older than the hot floor simply returns
// nothing older than hot holds (archive history is out of scope for the list
// endpoints).
func (s *Store) ListPings(ctx context.Context, f services.PingFilter) ([]domain.PingRecord, error) {
	var (
		conds []string
		args  []any
	)
	if !f.Since.IsZero() {
		// see ListMetrics: uuidv7Floor gives the planner a sargable id lower
		// bound so the scan can seek to the cutoff and stop early; ts >= ?
		// stays as the exact residual filter (issue #4 FIX 2).
		conds = append(conds, colPingsID+" >= ?", colPingsTS+" >= ?")
		args = append(args, uuidv7Floor(f.Since), formatTime(f.Since))
	}
	if f.Host != "" {
		conds = append(conds, colPingsHost+" = ?")
		args = append(args, f.Host)
	}
	if !f.IncludeUnreachable {
		// drop unreachable rows in SQL so LIMIT counts only returned rows.
		conds = append(conds, colPingsReceived+" > 0")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	query := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s DESC LIMIT ?", pingsColumnList, tablePings, where, colPingsID)
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pings: %w", err)
	}
	defer rows.Close()

	out, err := scanPingRecords(rows)
	if err != nil {
		return nil, fmt.Errorf("list pings: %w", err)
	}
	return out, nil
}

// ListPingsByRun returns every pings row for runID, ordered by id, mirroring
// ListMetricsByRun. Off-port: it exists so tests can read back what
// SavePings persisted.
func (s *Store) ListPingsByRun(ctx context.Context, runID string) ([]domain.PingResult, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s FROM %s WHERE %s = ? ORDER BY %s ASC`,
			colPingsHost, colPingsSent, colPingsReceived, colPingsAvgMS, colPingsError,
			tablePings, colPingsRunID, colPingsID),
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list pings for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []domain.PingResult
	for rows.Next() {
		var (
			r      domain.PingResult
			avgMS  sql.NullFloat64
			errStr sql.NullString
		)
		if err := rows.Scan(&r.Host, &r.Sent, &r.Received, &avgMS, &errStr); err != nil {
			return nil, fmt.Errorf("list pings for run %s: scan: %w", runID, err)
		}
		r.AvgMS = avgMS.Float64
		r.OK = r.Received > 0
		r.Error = errStr.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pings for run %s: %w", runID, err)
	}

	return out, nil
}

// ListRuns returns the newest limit runs (highest id first). limit is bound
// as a parameterized LIMIT — never string-concatenated (Risk R8: an
// unbounded read could scan the whole table).
func (s *Store) ListRuns(ctx context.Context, limit int) ([]domain.Run, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s FROM %s ORDER BY %s DESC LIMIT ?`,
			colRunsID, colRunsStartedAt, colRunsMode, colRunsInternetOK, colRunsAction,
			colRunsRebootStartedAt, colRunsRouterDownAt, colRunsRouterUpAt,
			colRunsInternetRestoredAt, colRunsFinishedAt, colRunsOutcome, colRunsError,
			tableRuns, colRunsID),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []domain.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}

	return out, nil
}

// ListUnsentOutboxMessages returns every tbot_queue row with sent_at still
// NULL, oldest first, so a flush delivers messages in the order they were
// queued.
func (s *Store) ListUnsentOutboxMessages(ctx context.Context) ([]domain.OutboxMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s FROM %s WHERE %s IS NULL ORDER BY %s ASC`,
			colOutboxID, colOutboxCreatedAt, colOutboxText, colOutboxAttempts, colOutboxLastError,
			tableOutbox, colOutboxSentAt, colOutboxCreatedAt),
	)
	if err != nil {
		return nil, fmt.Errorf("list unsent outbox messages: %w", err)
	}
	defer rows.Close()

	var out []domain.OutboxMessage
	for rows.Next() {
		var (
			m         domain.OutboxMessage
			createdAt string
			lastError sql.NullString
		)
		if err := rows.Scan(&m.ID, &createdAt, &m.Text, &m.Attempts, &lastError); err != nil {
			return nil, fmt.Errorf("list unsent outbox messages: scan: %w", err)
		}
		m.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("list unsent outbox messages: %w", err)
		}
		m.LastError = lastError.String
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list unsent outbox messages: %w", err)
	}

	return out, nil
}

// MarkOutboxSent sets sent_at on the tbot_queue row id.
func (s *Store) MarkOutboxSent(ctx context.Context, id int64, sentAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ?`, tableOutbox, colOutboxSentAt, colOutboxID),
		formatTime(sentAt), id,
	)
	if err != nil {
		return fmt.Errorf("mark outbox message %d sent: %w", id, err)
	}
	return nil
}

// MaybeVacuum runs a full VACUUM at most once every vacuumInterval, gated by
// meta[last_vacuum_at] (issue #4): VACUUM reclaims and defragments the pages
// RolloverToArchive/PruneArchive's deletes free, which incremental_vacuum
// would not (it only returns freelist pages to the OS without repacking
// b-trees). VACUUM runs outside any transaction — SQLite rejects it inside
// one — as two separate autocommit statements bracketing the bare VACUUM.
// The stamp is written only after VACUUM succeeds, so a failure (e.g.
// SQLITE_BUSY from a concurrent daemon reader) retries next call instead of
// silently skipping a whole week. ran reports whether VACUUM ran and its
// timestamp was recorded — false on the no-op/still-fresh path and on any
// error, including a stamp write that fails after VACUUM itself succeeded —
// so a caller can log a breadcrumb only on the run that did real work
// instead of every call.
func (s *Store) MaybeVacuum(ctx context.Context, now time.Time) (ran bool, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, colMetaValue, tableMeta, colMetaKey),
		metaKeyLastVacuum,
	).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// never vacuumed before — fall through and do it now.
	case err != nil:
		return false, fmt.Errorf("maybe vacuum: read last vacuum: %w", err)
	default:
		last, perr := parseTime(raw)
		if perr != nil {
			return false, fmt.Errorf("maybe vacuum: %w", perr)
		}
		if now.Sub(last) < vacuumInterval {
			return false, nil
		}
	}

	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil { // must run outside a transaction
		return false, fmt.Errorf("maybe vacuum: vacuum: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT OR REPLACE INTO %s (%s, %s) VALUES (?, ?)`, tableMeta, colMetaKey, colMetaValue),
		metaKeyLastVacuum, formatTime(now),
	); err != nil {
		return false, fmt.Errorf("maybe vacuum: record: %w", err)
	}
	return true, nil
}

// Name implements services.HealthProbe, naming this probe "sqlite" in a
// /health/check report.
func (s *Store) Name() string { return "sqlite" }

// NewestRunStartedAt returns the started_at of the most recently inserted
// runs row, mirroring GetLastRebootStartedAt but without the
// action='reboot' filter: freshnessProbe uses it to detect "the collector
// has stopped running" regardless of what any given run did. ok is false
// with a nil error when no run exists yet.
func (s *Store) NewestRunStartedAt(ctx context.Context) (t time.Time, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s ORDER BY %s DESC LIMIT 1`, colRunsStartedAt, tableRuns, colRunsID))

	var raw string
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("newest run started at: %w", err)
	}

	parsed, err := parseTime(raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("newest run started at: %w", err)
	}

	return parsed, true, nil
}

// OverviewMetrics returns every hot metrics row from since onward, bucketed
// at bucket-wide intervals and grouped into one domain.MetricSeries per
// (collector, name): the dashboard's server-downsampled retrospective view
// (plans/010, GET /api/v1/overview). Mirrors ListMetrics' sargable
// "id >= ? AND ts >= ?" since floor (uuidv7Floor), but always drops
// unavailable samples (ok=1, no IncludeEmpty knob here) — an unavailable
// reading contributes nothing to a MIN/MAX/AVG bucket. bucket is assumed
// already clamped by services.QueryService.Overview; bucketEpochExpr floors
// a non-positive value defensively rather than dividing by zero.
func (s *Store) OverviewMetrics(ctx context.Context, since time.Time, bucket time.Duration) ([]domain.MetricSeries, error) {
	query := fmt.Sprintf(
		`SELECT %s, %s, %s, %s AS b, MIN(%s), MAX(%s), AVG(%s), COUNT(%s)
		 FROM %s
		 WHERE %s >= ? AND %s >= ? AND %s = 1
		 GROUP BY %s, %s, b
		 ORDER BY %s, %s, b`,
		colMetricsCollector, colMetricsName, colMetricsUnit, bucketEpochExpr(colMetricsTS, bucket),
		colMetricsValue, colMetricsValue, colMetricsValue, colMetricsValue,
		tableMetrics,
		colMetricsID, colMetricsTS, colMetricsOK,
		colMetricsCollector, colMetricsName,
		colMetricsCollector, colMetricsName,
	)

	rows, err := s.db.QueryContext(ctx, query, uuidv7Floor(since), formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("overview metrics: %w", err)
	}
	defer rows.Close()

	var out []domain.MetricSeries
	var cur *domain.MetricSeries
	for rows.Next() {
		var (
			collector, name, unit string
			epoch                 int64
			min, max, avg         float64
			count                 int
		)
		if err := rows.Scan(&collector, &name, &unit, &epoch, &min, &max, &avg, &count); err != nil {
			return nil, fmt.Errorf("overview metrics: scan: %w", err)
		}

		// each row's own collector/name is authoritative for whether it starts
		// a new series — GROUP BY ... ORDER BY collector, name, b already
		// guarantees every row of one series arrives contiguously.
		if cur == nil || cur.Collector != domain.Collector(collector) || cur.Name != name {
			out = append(out, domain.MetricSeries{Collector: domain.Collector(collector), Name: name, Unit: unit})
			cur = &out[len(out)-1]
		}
		cur.Buckets = append(cur.Buckets, domain.MetricBucket{
			TS:    time.Unix(epoch, 0).UTC(),
			Min:   min,
			Max:   max,
			Avg:   avg,
			Count: count,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("overview metrics: %w", err)
	}

	return out, nil
}

// OverviewPings returns every hot pings row from since onward, bucketed at
// bucket-wide intervals and grouped into one domain.PingSeries per host
// (Outages left nil — services.QueryService.Overview attaches those
// separately from PingSamples): the dashboard's server-downsampled
// retrospective view (plans/010, GET /api/v1/overview). Mirrors ListPings'
// sargable "id >= ? AND ts >= ?" since floor (uuidv7Floor). Unlike
// ListPings, sent/received are summed over EVERY sample including fully
// unreachable ones — a loss percentage needs the unreachable rows counted —
// while avg_ms is averaged only over reachable samples (received>0); an
// all-unreachable bucket leaves both the average and the max SQL NULL,
// scanned into sql.NullFloat64 and reported as 0 with Samples/Received
// telling the real story (domain.PingBucket's own doc).
func (s *Store) OverviewPings(ctx context.Context, since time.Time, bucket time.Duration) ([]domain.PingSeries, error) {
	query := fmt.Sprintf(
		`SELECT %s, %s AS b, SUM(%s), SUM(%s), AVG(CASE WHEN %s > 0 THEN %s END), MAX(%s), COUNT(*)
		 FROM %s
		 WHERE %s >= ? AND %s >= ?
		 GROUP BY %s, b
		 ORDER BY %s, b`,
		colPingsHost, bucketEpochExpr(colPingsTS, bucket), colPingsSent, colPingsReceived,
		colPingsReceived, colPingsAvgMS, colPingsAvgMS,
		tablePings,
		colPingsID, colPingsTS,
		colPingsHost,
		colPingsHost,
	)

	rows, err := s.db.QueryContext(ctx, query, uuidv7Floor(since), formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("overview pings: %w", err)
	}
	defer rows.Close()

	var out []domain.PingSeries
	var cur *domain.PingSeries
	for rows.Next() {
		var (
			host       string
			epoch      int64
			sent, recv int
			avgMS      sql.NullFloat64
			maxMS      sql.NullFloat64
			samples    int
		)
		if err := rows.Scan(&host, &epoch, &sent, &recv, &avgMS, &maxMS, &samples); err != nil {
			return nil, fmt.Errorf("overview pings: scan: %w", err)
		}

		// each row's own host is authoritative for whether it starts a new
		// series — GROUP BY ... ORDER BY host, b already guarantees every row
		// of one series arrives contiguously.
		if cur == nil || cur.Host != host {
			out = append(out, domain.PingSeries{Host: host})
			cur = &out[len(out)-1]
		}
		cur.Buckets = append(cur.Buckets, domain.PingBucket{
			TS:       time.Unix(epoch, 0).UTC(),
			Sent:     sent,
			Received: recv,
			AvgMS:    avgMS.Float64,
			MaxMS:    maxMS.Float64,
			Samples:  samples,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("overview pings: %w", err)
	}

	return out, nil
}

// PingSamples returns every hot pings row from since onward, ascending by
// (host, id) so services.QueryService.Overview can walk each host's samples
// in chronological order and collapse them into domain.PingOutage episodes
// (plans/010 FIX 1). Unlike ListPings/OverviewPings, this returns healthy
// rows too, not just degraded ones: collapseOutages needs to see a healthy
// sample to know that a recover-then-refail for one host is two episodes,
// not one merged span. Mirrors ListPings' sargable "id >= ? AND ts >= ?"
// since floor (uuidv7Floor); hot-only and unbounded by any LIMIT — the
// 7-day default window this is scoped to is already bounded by since.
func (s *Store) PingSamples(ctx context.Context, since time.Time) ([]domain.PingRecord, error) {
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s >= ? AND %s >= ? ORDER BY %s, %s`,
		pingsColumnList, tablePings, colPingsID, colPingsTS, colPingsHost, colPingsID,
	)

	rows, err := s.db.QueryContext(ctx, query, uuidv7Floor(since), formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("ping samples: %w", err)
	}
	defer rows.Close()

	out, err := scanPingRecords(rows)
	if err != nil {
		return nil, fmt.Errorf("ping samples: %w", err)
	}
	return out, nil
}

// PruneArchive deletes every archived run (started_at older than
// now - retentionDays days) and all its archived children, in one
// transaction, run-boundary — never by a child's own ts, which would
// re-split a run across the retention boundary (the bug the old per-table
// PruneChecks/PruneMetrics/PrunePings model had, and the reason runs_archive
// itself was never pruned before). Only *_archive tables are touched; hot is
// bounded by RolloverToArchive, not by retention. retentionDays<=0 means
// "keep forever" and is a no-op (design §9's original semantics, now scoped
// to the archive tier). Off-port: main calls it directly on the concrete
// *Store at startup, after RolloverToArchive. pruned reports how many runs
// were removed (issue #4 FIX 5) — 0 on the retentionDays<=0 no-op and
// whenever nothing was old enough — so a caller can log a breadcrumb only
// when real work happened, mirroring MaybeVacuum's ran bool.
func (s *Store) PruneArchive(ctx context.Context, now time.Time, retentionDays int) (pruned int64, err error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := formatTime(now.Add(-time.Duration(retentionDays) * 24 * time.Hour))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("prune archive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit (sanctioned discard, error-recovery path)

	agedArchivedRunIDs := fmt.Sprintf(`SELECT %s FROM %s WHERE %s < ?`, colRunsID, tableRunsArchive, colRunsStartedAt)

	// same ordering rule as RolloverToArchive: every child DELETE (which
	// keys off agedArchivedRunIDs, itself reading runs_archive) must run
	// before runs_archive's own rows disappear, or the subquery goes empty
	// on the later statements and strands their children. runs_archive's own
	// DELETE runs last, separately from the loop, so its RowsAffected — one
	// row per pruned run — can be captured and returned.
	childStmts := []string{
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tableChecksArchive, colChecksRunID, agedArchivedRunIDs),
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tableMetricsArchive, colMetricsRunID, agedArchivedRunIDs),
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tableHostArchive, colHostRunID, agedArchivedRunIDs),
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tablePingsArchive, colPingsRunID, agedArchivedRunIDs),
	}
	for _, stmt := range childStmts {
		if _, err := tx.ExecContext(ctx, stmt, cutoff); err != nil {
			return 0, fmt.Errorf("prune archive: %w", err)
		}
	}

	res, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s < ?`, tableRunsArchive, colRunsStartedAt), cutoff) // runs_archive LAST
	if err != nil {
		return 0, fmt.Errorf("prune archive: %w", err)
	}
	if pruned, err = res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("prune archive: rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("prune archive: commit: %w", err)
	}
	return pruned, nil
}

// RolloverToArchive moves every run (and all its children) with
// started_at older than now - hotWindowDays days from the hot tables into
// their *_archive twins, in one transaction (issue #4): this run-boundary
// maintenance pass is what bounds the hot working set — the collector's
// write path stays single, into hot; this is a startup *move*, not a
// dual-write. tbot_queue is a queue, not history, and is never touched.
// hotWindowDays<=0 is a defense-in-depth no-op (LoadConfig already clamps
// HOT_WINDOW_DAYS to >=1) so a stray zero/negative value can never archive
// everything. Off-port: main calls it directly on the concrete *Store at
// startup, before PruneArchive. moved reports how many runs were archived
// (issue #4 FIX 5) — 0 on the hotWindowDays<=0 no-op and whenever nothing
// was old enough — so a caller can log a breadcrumb only when real work
// happened, mirroring MaybeVacuum's ran bool.
func (s *Store) RolloverToArchive(ctx context.Context, now time.Time, hotWindowDays int) (moved int64, err error) {
	if hotWindowDays <= 0 {
		return 0, nil
	}
	cutoff := formatTime(now.Add(-time.Duration(hotWindowDays) * 24 * time.Hour))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rollover to archive: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit (sanctioned discard, error-recovery path)

	agedRunIDs := fmt.Sprintf(`SELECT %s FROM %s WHERE %s < ?`, colRunsID, tableRuns, colRunsStartedAt)

	// ordering is load-bearing (Risk R1): every INSERT...SELECT (children,
	// then runs) must run before any DELETE, and `runs` must be deleted
	// LAST. Every child statement below keys off agedRunIDs, which reads
	// from `runs` — deleting `runs` first would make that subquery return
	// nothing on every later statement and strand that run's children in
	// hot. runs' own DELETE runs last, separately from the loop, so its
	// RowsAffected — one row per moved run — can be captured and returned.
	stmts := []string{
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s IN (%s)`,
			tableChecksArchive, checksFullColumnList, checksFullColumnList, tableChecks, colChecksRunID, agedRunIDs),
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s IN (%s)`,
			tableMetricsArchive, metricsFullColumnList, metricsFullColumnList, tableMetrics, colMetricsRunID, agedRunIDs),
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s IN (%s)`,
			tableHostArchive, hostColumnList, hostColumnList, tableHost, colHostRunID, agedRunIDs),
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s IN (%s)`,
			tablePingsArchive, pingsFullColumnList, pingsFullColumnList, tablePings, colPingsRunID, agedRunIDs),
		fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s < ?`,
			tableRunsArchive, runColumnList, runColumnList, tableRuns, colRunsStartedAt),
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tableChecks, colChecksRunID, agedRunIDs),
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tableMetrics, colMetricsRunID, agedRunIDs),
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tableHost, colHostRunID, agedRunIDs),
		fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`, tablePings, colPingsRunID, agedRunIDs),
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, cutoff); err != nil {
			return 0, fmt.Errorf("rollover to archive: %w", err)
		}
	}

	res, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s < ?`, tableRuns, colRunsStartedAt), cutoff) // runs LAST
	if err != nil {
		return 0, fmt.Errorf("rollover to archive: %w", err)
	}
	if moved, err = res.RowsAffected(); err != nil {
		return 0, fmt.Errorf("rollover to archive: rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rollover to archive: commit: %w", err)
	}
	return moved, nil
}

// RunByID reads back a run by id, spanning both tiers via UNION ALL: the
// run-boundary invariant (RolloverToArchive/PruneArchive only ever move or
// delete whole runs, never split one) guarantees id resolves in exactly one
// of runs/runs_archive, so this never risks a duplicate. found is false, not
// an error, when id exists in neither tier (services.HistoryRepository's
// "not found" contract, so services never needs to import database/sql).
// GetRun stays hot-only and is untouched by this method.
func (s *Store) RunByID(ctx context.Context, id string) (domain.Run, bool, error) {
	query := fmt.Sprintf(
		`SELECT %[1]s FROM (
			SELECT %[1]s FROM %[2]s WHERE %[3]s = ?
			UNION ALL
			SELECT %[1]s FROM %[4]s WHERE %[3]s = ?
		) LIMIT 1`,
		runColumnList, tableRuns, colRunsID, tableRunsArchive,
	)

	row := s.db.QueryRowContext(ctx, query, id, id)
	run, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Run{}, false, nil
		}
		return domain.Run{}, false, fmt.Errorf("run by id %s: %w", id, err)
	}
	return run, true, nil
}

// SaveMetrics writes one telemetry snapshot: the host sidecar row plus one
// metrics row per sample, all in a single transaction so a partial snapshot
// (e.g. a failed sample insert) never persists (plans/003-host-telemetry.md).
// Each metrics row gets its own freshly generated UUIDv7 id stamped with ts
// (issue #4) — every sample in one call shares that same ts, so the
// generator's monotonic counter is what keeps them in insertion order under
// ORDER BY id. host.run_id is itself the PRIMARY KEY (not a separately
// generated id — it is runID, the owning run's own id), so a second
// SaveMetrics call for the same runID fails outright — collectMetrics calls
// it exactly once per run, so this is a correctness guard rather than a
// case to INSERT OR REPLACE around.
func (s *Store) SaveMetrics(ctx context.Context, runID string, ts time.Time, m domain.HostMetrics) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save metrics for run %s: begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit (sanctioned discard, error-recovery path)

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?)`,
			tableHost, colHostRunID, colHostTS, colHostHostname, colHostOS, colHostArch),
		runID, formatTime(ts), m.Host.Hostname, m.Host.OS, m.Host.Arch,
	); err != nil {
		return fmt.Errorf("save metrics for run %s: host: %w", runID, err)
	}

	for _, sm := range m.Samples {
		id, err := newUUIDv7(ts)
		if err != nil {
			return fmt.Errorf("save metrics for run %s: sample %s/%s: %w", runID, sm.Collector, sm.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				tableMetrics, colMetricsID, colMetricsRunID, colMetricsTS, colMetricsCollector, colMetricsName,
				colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError),
			id, runID, formatTime(ts), string(sm.Collector), sm.Name,
			nullFloat(sm.Value, sm.OK), sm.Unit, boolToInt(sm.OK), nullString(sm.Error),
		); err != nil {
			return fmt.Errorf("save metrics for run %s: sample %s/%s: %w", runID, sm.Collector, sm.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save metrics for run %s: commit: %w", runID, err)
	}
	return nil
}

// SavePings writes one round of ping probes for runID, one row per result,
// all in a single transaction so a partial round never persists (mirrors
// SaveMetrics). Each row gets its own freshly generated UUIDv7 id stamped
// with ts (issue #4); every result in one call shares that same ts, so the
// generator's monotonic counter is what keeps them in insertion order under
// ORDER BY id. avg_ms is SQL NULL when the result is unavailable (OK
// false), so an unreachable host's zero AvgMS is never mistaken for a
// measured 0ms round trip.
func (s *Store) SavePings(ctx context.Context, runID string, ts time.Time, results []domain.PingResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save pings for run %s: begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit (sanctioned discard, error-recovery path)

	for _, r := range results {
		id, err := newUUIDv7(ts)
		if err != nil {
			return fmt.Errorf("save pings for run %s: host %s: %w", runID, r.Host, err)
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				tablePings, colPingsID, colPingsRunID, colPingsTS, colPingsHost, colPingsSent, colPingsReceived, colPingsAvgMS, colPingsError),
			id, runID, formatTime(ts), r.Host, r.Sent, r.Received, nullFloat(r.AvgMS, r.OK), nullString(r.Error),
		); err != nil {
			return fmt.Errorf("save pings for run %s: host %s: %w", runID, r.Host, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save pings for run %s: commit: %w", runID, err)
	}
	return nil
}

// UpdateRun applies every non-nil field of u to the runs row id. Callers
// build a domain.RunUpdate with only the fields that changed at a given
// phase transition (e.g. just RouterDownAt) and leave the rest nil.
func (s *Store) UpdateRun(ctx context.Context, id string, u domain.RunUpdate) error {
	sets, args := runUpdateAssignments(u)
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)

	query := fmt.Sprintf(`UPDATE %s SET %s WHERE %s = ?`, tableRuns, joinAssignments(sets), colRunsID)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update run %s: %w", id, err)
	}
	return nil
}

const (
	tableRuns    = "runs"
	tableChecks  = "checks"
	tableOutbox  = "tbot_queue"
	tableMetrics = "metrics"
	tableHost    = "host"
	tablePings   = "pings"

	// tableRunsArchive, tableChecksArchive, tableMetricsArchive,
	// tableHostArchive, and tablePingsArchive are the static "cold" twins
	// (issue #4): identical schema to their hot counterpart, fed by
	// RolloverToArchive's run-boundary move rather than a second write path.
	tableRunsArchive    = "runs_archive"
	tableChecksArchive  = "checks_archive"
	tableMetricsArchive = "metrics_archive"
	tableHostArchive    = "host_archive"
	tablePingsArchive   = "pings_archive"

	// tableMeta is a small key/value sidecar (currently just
	// metaKeyLastVacuum) for maintenance bookkeeping that doesn't belong on
	// any domain table.
	tableMeta = "meta"

	colRunsID                 = "id"
	colRunsStartedAt          = "started_at"
	colRunsMode               = "mode"
	colRunsInternetOK         = "internet_ok"
	colRunsAction             = "action"
	colRunsRebootStartedAt    = "reboot_started_at"
	colRunsRouterDownAt       = "router_down_at"
	colRunsRouterUpAt         = "router_up_at"
	colRunsInternetRestoredAt = "internet_restored_at"
	colRunsFinishedAt         = "finished_at"
	colRunsOutcome            = "outcome"
	colRunsError              = "error"

	// runColumnList is the fixed 12-column projection every runs/runs_archive
	// query selects, in scanRun's positional order: GetRun, ListRuns, and
	// RunByID's tier-spanning query all depend on this exact order. Built
	// from the column consts above (not hardcoded), so a column rename still
	// surfaces at compile time.
	runColumnList = colRunsID + ", " + colRunsStartedAt + ", " + colRunsMode + ", " + colRunsInternetOK + ", " +
		colRunsAction + ", " + colRunsRebootStartedAt + ", " + colRunsRouterDownAt + ", " + colRunsRouterUpAt + ", " +
		colRunsInternetRestoredAt + ", " + colRunsFinishedAt + ", " + colRunsOutcome + ", " + colRunsError

	colChecksID        = "id"
	colChecksRunID     = "run_id"
	colChecksTS        = "ts"
	colChecksPhase     = "phase"
	colChecksTarget    = "target"
	colChecksKind      = "kind"
	colChecksOK        = "ok"
	colChecksLatencyMS = "latency_ms"
	colChecksError     = "error"

	// checksColumnList is the eight-column projection (no id) ListChecksByRun
	// scans. checksFullColumnList prefixes id: RolloverToArchive's
	// INSERT...SELECT must preserve ids, and ListChecksByRun's
	// archive-spanning UNION ALL needs id in each inner leg so the outer
	// ORDER BY can reference it without adding an id scan destination.
	checksColumnList = colChecksRunID + ", " + colChecksTS + ", " + colChecksPhase + ", " + colChecksTarget + ", " +
		colChecksKind + ", " + colChecksOK + ", " + colChecksLatencyMS + ", " + colChecksError
	checksFullColumnList = colChecksID + ", " + checksColumnList

	idxChecksRun = "idx_checks_run"

	idxChecksArchiveRun = "idx_checks_archive_run"

	colOutboxID        = "id"
	colOutboxCreatedAt = "created_at"
	colOutboxText      = "text"
	colOutboxSentAt    = "sent_at"
	colOutboxAttempts  = "attempts"
	colOutboxLastError = "last_error"

	colMetricsID        = "id"
	colMetricsRunID     = "run_id"
	colMetricsTS        = "ts"
	colMetricsCollector = "collector"
	colMetricsName      = "name"
	colMetricsValue     = "value"
	colMetricsUnit      = "unit"
	colMetricsOK        = "ok"
	colMetricsError     = "error"

	// metricsColumnList and metricsFullColumnList mirror checksColumnList/
	// checksFullColumnList: the former is what ListMetrics/LatestMetrics scan
	// via scanMetricRecords, the latter (id-prefixed) is what
	// RolloverToArchive's INSERT...SELECT and ListMetrics' archive-spanning
	// UNION ALL legs select.
	metricsColumnList = colMetricsRunID + ", " + colMetricsTS + ", " + colMetricsCollector + ", " + colMetricsName + ", " +
		colMetricsValue + ", " + colMetricsUnit + ", " + colMetricsOK + ", " + colMetricsError
	metricsFullColumnList = colMetricsID + ", " + metricsColumnList

	idxMetricsRun = "idx_metrics_run"

	idxMetricsArchiveRun = "idx_metrics_archive_run"

	colHostRunID    = "run_id"
	colHostTS       = "ts"
	colHostHostname = "hostname"
	colHostOS       = "os"
	colHostArch     = "arch"

	// hostColumnList is host/host_archive's full column list. Unlike
	// checks/metrics/pings, host has no separate id column (run_id is itself
	// the primary key), so there is no "with/without id" split to make.
	hostColumnList = colHostRunID + ", " + colHostTS + ", " + colHostHostname + ", " + colHostOS + ", " + colHostArch

	colPingsID       = "id"
	colPingsRunID    = "run_id"
	colPingsTS       = "ts"
	colPingsHost     = "host"
	colPingsSent     = "sent"
	colPingsReceived = "received"
	colPingsAvgMS    = "avg_ms"
	colPingsError    = "error"

	// pingsColumnList and pingsFullColumnList mirror checksColumnList/
	// checksFullColumnList.
	pingsColumnList = colPingsRunID + ", " + colPingsTS + ", " + colPingsHost + ", " + colPingsSent + ", " +
		colPingsReceived + ", " + colPingsAvgMS + ", " + colPingsError
	pingsFullColumnList = colPingsID + ", " + pingsColumnList

	idxPingsRun = "idx_pings_run"

	idxPingsArchiveRun = "idx_pings_archive_run"

	colMetaKey   = "key"
	colMetaValue = "value"

	// metaKeyLastVacuum is the meta row key MaybeVacuum reads and writes to
	// gate its cadence.
	metaKeyLastVacuum = "last_vacuum_at"

	// metaKeySchemaVersion is the meta row key OpenStore reads and writes
	// (issue #4 FIX 3) to tell a genuinely fresh database (migrate just
	// created meta, and every table with it) apart from a pre-issue-4
	// database (migrate's CREATE TABLE IF NOT EXISTS no-ops against its
	// already-existing, still-INTEGER-id runs table, but freshly creates an
	// empty meta table alongside it) — both look identical as "meta has no
	// schema_version row" without also checking whether runs already holds
	// data. currentSchemaVersion is the version this binary's schema is at;
	// bump it if a future change again makes an old on-disk schema
	// incompatible with what migrate() assumes.
	metaKeySchemaVersion = "schema_version"
	currentSchemaVersion = "2"

	// timeFormat is RFC3339 in UTC (design §9). Every timestamp column is
	// stored in this fixed-width format so retention pruning can compare
	// them lexicographically without parsing.
	timeFormat = time.RFC3339

	// vacuumInterval is MaybeVacuum's cadence gate: weekly, a package const
	// rather than a config knob (Trade-off T7) since no operator has asked
	// to tune it and unrequested config is not added speculatively.
	vacuumInterval = 7 * 24 * time.Hour

	// sqliteCorrupt and sqliteNotADB are the standard, stable SQLite result
	// codes (https://www.sqlite.org/rescode.html; part of the public C API,
	// unrelated to and unexported by modernc.org/sqlite's own Go surface)
	// isSQLiteCorruptionError checks a *sqlite.Error against (issue #4
	// FIX 3): "the database disk image is malformed" and "file opened that
	// is not a database file" are the two ways a genuinely damaged or
	// non-SQLite file can fail to open or migrate.
	sqliteCorrupt = 11
	sqliteNotADB  = 26
)

// errIncompatibleSchema is the sentinel OpenStore wraps and returns when
// needsQuarantine reports a pre-issue-4 database and selfHeal is false: the
// caller (the daemon) is a read-only reader with no exclusive lock of its
// own, so it must never rename the live file aside itself — only the
// collector, the database's sole writer and the only caller OpenStore ever
// grants selfHeal to, may quarantine and recreate it (see OpenStore's own
// doc).
var errIncompatibleSchema = errors.New("database predates the UUIDv7 schema")

// boolToInt renders a Go bool as the 0/1 SQLite stores in an INTEGER
// column (checks.ok, which is NOT NULL and so needs no separate nullable
// helper).
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// bucketEpochExpr renders the portable "bucket-start epoch seconds" SQL
// expression for tsCol at the given bucket width (plans/010): strftime('%s',
// tsCol) converts the stored RFC3339 string to a Unix-epoch second count —
// portable across any SQLite build, unlike the newer unixepoch() function —
// integer-divided by the bucket width in seconds and multiplied back rounds
// it down to that bucket's start. services.QueryService.Overview always
// clamps bucket to a positive range before calling OverviewMetrics/
// OverviewPings; a non-positive bucket here is floored to one second so the
// division can never be by zero.
func bucketEpochExpr(tsCol string, bucket time.Duration) string {
	secs := int(bucket / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf(`(CAST(strftime('%%s', %s) AS INTEGER) / %d) * %d`, tsCol, secs, secs)
}

// chmodStoreFiles restricts path and its WAL-mode -wal/-shm siblings to
// 0600, matching the same bar as the flock lock file and the .env file
// (CLAUDE.md): the database holds router credentials' error strings. A
// sidecar that does not exist yet is not an error — WAL segments are
// created lazily on first write — but any other chmod failure is, since
// permissions must never silently stay at sql.Open's umask default.
func chmodStoreFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0600); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	return nil
}

// ensureSchemaVersionStamped writes meta.schema_version = currentSchemaVersion,
// unconditionally via INSERT OR REPLACE (issue #4 FIX 3). This runs on every
// OpenStore call, not just a database's first: a freshly created database
// gets its first stamp here (needsQuarantine already ran and found it merely
// unstamped, not incompatible), and an already-stamped one is simply
// rewritten with the same value — so a future schema bump only has to change
// currentSchemaVersion, never add a migration path for "already at the
// previous version".
func (s *Store) ensureSchemaVersionStamped(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT OR REPLACE INTO %s (%s, %s) VALUES (?, ?)`, tableMeta, colMetaKey, colMetaValue),
		metaKeySchemaVersion, currentSchemaVersion,
	)
	if err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// formatTime renders t as UTC RFC3339, the fixed-width string format every
// timestamp column uses (design §9).
func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

// isMemoryDB reports whether path is the sqlite in-memory sentinel
// (":memory:") or a "file::memory:"/"mode=memory" DSN variant. None of
// these ever touch disk, so there is no file for chmodStoreFiles to chmod.
func isMemoryDB(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, "file::memory:") || strings.Contains(path, "mode=memory")
}

// isSQLiteCorruptionCode reports whether code — a *sqlite.Error's own
// Code() — signals SQLITE_CORRUPT or SQLITE_NOTADB, base or extended (issue
// #4 FIX 3). modernc.org/sqlite enables extended result codes on every
// connection (conn.go's newConn), so a genuinely corrupt database usually
// arrives as an extended code — SQLITE_CORRUPT_INDEX (779),
// SQLITE_CORRUPT_SEQUENCE (523), SQLITE_CORRUPT_VTAB (267), and so on — not
// the bare primary SQLITE_CORRUPT (11); masking to the low byte recovers
// the primary code regardless of which extended variant fired, since an
// extended code is always "primary | (extra info << 8)", the same encoding
// sqlite3_extended_result_codes itself uses. This can never fold some
// unrelated primary code down onto 11/26: every primary SQLite result code
// occupies its own number in 0-28 (sqlite.org/rescode.html), so the masked
// low byte only ever matches sqliteCorrupt/sqliteNotADB when the error
// really is one of those two families — a transient SQLITE_BUSY (5) and its
// extended forms (e.g. SQLITE_BUSY_TIMEOUT, 773) mask down to 5, never 11.
func isSQLiteCorruptionCode(code int) bool {
	switch code & 0xff {
	case sqliteCorrupt, sqliteNotADB:
		return true
	default:
		return false
	}
}

// isSQLiteCorruptionError reports whether err is a *sqlite.Error carrying a
// SQLITE_CORRUPT or SQLITE_NOTADB result code (isSQLiteCorruptionCode) — the
// two ways opening or migrating the file at a configured path fails because
// it is genuinely damaged or not a SQLite database at all. Any other error
// (permissions, disk full, a locked file) is left alone: quarantining and
// starting fresh would not fix those, and OpenStore must surface them as-is
// rather than destroy data over an unrelated failure.
func isSQLiteCorruptionError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return isSQLiteCorruptionCode(sqliteErr.Code())
}

// joinAssignments joins "col = ?" fragments with ", " for an UPDATE ... SET
// clause.
func joinAssignments(sets []string) string {
	out := sets[0]
	for _, s := range sets[1:] {
		out += ", " + s
	}
	return out
}

// migrate applies the schema idempotently. Safe to call more than once (on
// the same or a fresh Store) since every statement is CREATE ... IF NOT
// EXISTS (design §9).
func (s *Store) migrate(ctx context.Context) error {
	// journal_mode=WAL is sticky at the database-FILE level (once set, every
	// future connection to this file sees WAL without asking again), so a
	// single exec here is enough. busy_timeout is the opposite — a
	// per-CONNECTION setting the driver forgets on every new physical
	// connection — so it is set via the DSN's _pragma parameter instead
	// (see openOnce/FIX 6), which every pooled connection re-applies for
	// itself; it must not also be exec'd here or it would only cover this
	// one connection, leaving the daemon's 2nd+ (DAEMON_MAX_CONNS) pooled
	// connection with SQLite's 0-timeout default.
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("apply pragma %q: %w", `PRAGMA journal_mode=WAL`, err)
	}

	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s INTEGER,
			%s TEXT NOT NULL,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT
		)`, tableRuns,
			colRunsID, colRunsStartedAt, colRunsMode, colRunsInternetOK, colRunsAction,
			colRunsRebootStartedAt, colRunsRouterDownAt, colRunsRouterUpAt,
			colRunsInternetRestoredAt, colRunsFinishedAt, colRunsOutcome, colRunsError,
		),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s INTEGER NOT NULL,
			%s INTEGER,
			%s TEXT
		)`, tableChecks,
			colChecksID, colChecksRunID, tableRuns, colRunsID, colChecksTS, colChecksPhase,
			colChecksTarget, colChecksKind, colChecksOK, colChecksLatencyMS, colChecksError,
		),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxChecksRun, tableChecks, colChecksRunID),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s INTEGER PRIMARY KEY,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT,
			%s INTEGER NOT NULL DEFAULT 0,
			%s TEXT
		)`, tableOutbox,
			colOutboxID, colOutboxCreatedAt, colOutboxText, colOutboxSentAt,
			colOutboxAttempts, colOutboxLastError,
		),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s REAL,
			%s TEXT NOT NULL,
			%s INTEGER NOT NULL,
			%s TEXT
		)`, tableMetrics,
			colMetricsID, colMetricsRunID, tableRuns, colRunsID, colMetricsTS, colMetricsCollector,
			colMetricsName, colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError,
		),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxMetricsRun, tableMetrics, colMetricsRunID),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL
		)`, tableHost,
			colHostRunID, tableRuns, colRunsID, colHostTS, colHostHostname, colHostOS, colHostArch,
		),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s INTEGER NOT NULL,
			%s INTEGER NOT NULL,
			%s REAL,
			%s TEXT
		)`, tablePings,
			colPingsID, colPingsRunID, tableRuns, colRunsID, colPingsTS, colPingsHost,
			colPingsSent, colPingsReceived, colPingsAvgMS, colPingsError,
		),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxPingsRun, tablePings, colPingsRunID),

		// *_archive twins (issue #4): byte-identical columns to their hot
		// counterpart bar the table name and the FK target (runs_archive
		// instead of runs), fed by RolloverToArchive rather than a second
		// write path. Every id/run_id column, hot and archive alike, is a
		// UUIDv7 TEXT string (issue #4 supersedes the AUTOINCREMENT-integer
		// refinement this schema originally shipped with): a UUIDv7 is
		// time-ordered and never reused, so "archived ids sort before live
		// hot ids" is intrinsic to the id itself rather than a property the
		// engine's rowid sequence had to be asked to preserve — it holds
		// even if the hot set fully empties (a collector outage longer than
		// HOT_WINDOW_DAYS) and a fresh InsertRun follows.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s INTEGER,
			%s TEXT NOT NULL,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT,
			%s TEXT
		)`, tableRunsArchive,
			colRunsID, colRunsStartedAt, colRunsMode, colRunsInternetOK, colRunsAction,
			colRunsRebootStartedAt, colRunsRouterDownAt, colRunsRouterUpAt,
			colRunsInternetRestoredAt, colRunsFinishedAt, colRunsOutcome, colRunsError,
		),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s INTEGER NOT NULL,
			%s INTEGER,
			%s TEXT
		)`, tableChecksArchive,
			colChecksID, colChecksRunID, tableRunsArchive, colRunsID, colChecksTS, colChecksPhase,
			colChecksTarget, colChecksKind, colChecksOK, colChecksLatencyMS, colChecksError,
		),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxChecksArchiveRun, tableChecksArchive, colChecksRunID),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s REAL,
			%s TEXT NOT NULL,
			%s INTEGER NOT NULL,
			%s TEXT
		)`, tableMetricsArchive,
			colMetricsID, colMetricsRunID, tableRunsArchive, colRunsID, colMetricsTS, colMetricsCollector,
			colMetricsName, colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError,
		),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxMetricsArchiveRun, tableMetricsArchive, colMetricsRunID),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL
		)`, tableHostArchive,
			colHostRunID, tableRunsArchive, colRunsID, colHostTS, colHostHostname, colHostOS, colHostArch,
		),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s TEXT PRIMARY KEY,
			%s TEXT NOT NULL REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s INTEGER NOT NULL,
			%s INTEGER NOT NULL,
			%s REAL,
			%s TEXT
		)`, tablePingsArchive,
			colPingsID, colPingsRunID, tableRunsArchive, colRunsID, colPingsTS, colPingsHost,
			colPingsSent, colPingsReceived, colPingsAvgMS, colPingsError,
		),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxPingsArchiveRun, tablePingsArchive, colPingsRunID),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (%s TEXT PRIMARY KEY, %s TEXT NOT NULL)`,
			tableMeta, colMetaKey, colMetaValue),
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}

	return nil
}

// needsQuarantine reports whether the already-open database s predates the
// UUIDv7 schema (issue #4 FIX 3) and must be set aside rather than used
// as-is. migrate's CREATE TABLE IF NOT EXISTS silently no-ops against an
// already-existing runs table, so an old INTEGER-id schema survives
// untouched underneath it and every subsequent InsertRun then fails
// "datatype mismatch" forever. A meta.schema_version already reading
// currentSchemaVersion is always compatible. Anything else is ambiguous by
// itself, because a genuinely fresh database also has no schema_version row
// yet (ensureSchemaVersionStamped only writes it after this check runs) —
// so a missing/stale version is treated as incompatible only when runs
// already holds at least one row; an empty runs table means "freshly
// created, not yet stamped", not "predates issue #4".
func (s *Store) needsQuarantine(ctx context.Context) (bool, error) {
	var version string
	err := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE %s = ?`, colMetaValue, tableMeta, colMetaKey),
		metaKeySchemaVersion,
	).Scan(&version)
	switch {
	case err == nil && version == currentSchemaVersion:
		return false, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("read schema version: %w", err)
	}

	var probe int
	err = s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT 1 FROM %s LIMIT 1`, tableRuns)).Scan(&probe)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil // freshly created, just not stamped yet
	case err != nil:
		return false, fmt.Errorf("probe runs table: %w", err)
	default:
		return true, nil // non-empty runs table with a missing/stale schema_version: predates issue #4
	}
}

// nullBoolToInt renders a *bool as the nullable INTEGER runs.internet_ok
// needs: nil (hard mode, no initial check) stays SQL NULL, otherwise 0/1.
func nullBoolToInt(b *bool) any {
	if b == nil {
		return nil
	}
	return boolToInt(*b)
}

// nullFloat renders value as SQL NULL when ok is false, instead of
// persisting the unavailable sample's zero value as if it had been measured
// (metrics.value).
func nullFloat(value float64, ok bool) any {
	if !ok {
		return nil
	}
	return value
}

// nullInt64 renders a *int64 as a driver value, keeping nil as SQL NULL
// instead of coercing it to 0 (checks.latency_ms is absent, not zero, when
// a probe times out before recording a duration).
func nullInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullString renders an empty string as SQL NULL for optional TEXT columns
// (checks.error), so an absent error is distinguishable from a stored empty
// string.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// openOnce opens (creating if absent) the SQLite database at path and
// migrates the schema, applying busy_timeout via the connection DSN rather
// than a PRAGMA exec (issue #4 FIX 6): modernc.org/sqlite's _pragma query
// parameter is re-applied by the driver on every new physical connection
// (conn.go's newConn calls applyQueryParams for each one), whereas a bare
// PRAGMA exec only ever reaches the single connection it ran on — leaving
// the daemon's 2nd+ pooled connection (DAEMON_MAX_CONNS) at SQLite's 0
// (no wait) default. journal_mode=WAL stays a migrate()-time exec: it is
// sticky at the file level, so one connection setting it is enough. path is
// left untouched for an in-memory sentinel (":memory:"/"file::memory:"/
// "mode=memory"): production never pairs one with maxOpenConns>1 (OpenStore's
// doc), so there is no pooled-connection gap here to close, and appending a
// query string to the bare ":memory:" form risks the driver no longer
// recognizing it as the sentinel. openOnce is split out from OpenStore so
// the self-healing retry (open, discover corruption or an incompatible
// schema, quarantine, open again) has one simple "just open it" primitive to
// call more than once.
func openOnce(ctx context.Context, path string, maxOpenConns int) (*Store, error) {
	dsn := path
	if !isMemoryDB(path) {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		dsn = path + sep + "_pragma=busy_timeout%3d5000"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	if maxOpenConns < 1 {
		maxOpenConns = 1
	}
	db.SetMaxOpenConns(maxOpenConns)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		if closeErr := db.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w (also failed to close db: %v)", err, closeErr)
		}
		return nil, err
	}

	return s, nil
}

// parseNullTime parses a nullable RFC3339 column: an invalid (unset) ns
// stays nil, distinguishing "phase not reached yet" from a zero time.
func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// parseTime parses a UTC RFC3339 timestamp as stored by formatTime.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// quarantineFile renames path and its -wal/-shm siblings aside — never
// deletes — so OpenStore can create a fresh, working database at path in
// their place (issue #4 FIX 3), preserving the set-aside files for manual
// inspection or recovery. The -wal/-shm siblings are renamed BEFORE the main
// file, not after: a crash between renames is the exact threat this ordering
// guards against, and SQLite locates a WAL/shm sidecar purely by the main
// file's own path — so if the main file moved FIRST and a crash struck
// before its sidecars followed, the next boot would create a brand-new
// empty database at path, and that fresh file would silently inherit the
// OLD sidecars still sitting at their original names, corrupting it via
// stale WAL replay. Renaming the main file LAST means path is only ever
// vacated once its own sidecars are already safely out of the way, so no
// crash ordering can produce that outcome — the worst a crash can do is
// leave path (and thus the whole problem) untouched for the next boot to
// detect and retry from scratch. Each of the three files is renamed only if
// it exists — a missing -wal/-shm (WAL not currently active, or already
// quarantined by an earlier, interrupted attempt) is not an error; only the
// collector, the database's sole writer and the only caller OpenStore ever
// grants selfHeal to, calls this function, always under its own exclusive
// flock, so there is no concurrent second quarantine attempt racing this one
// that the tolerance needs to guard against either.
func quarantineFile(path, reason string, now time.Time) error {
	suffix := fmt.Sprintf(".%s-%s", reason, now.UTC().Format("20060102T150405Z"))
	for _, p := range []string{path + "-wal", path + "-shm", path} {
		if err := os.Rename(p, p+suffix); err != nil {
			if os.IsNotExist(err) {
				continue // tolerated: no concurrent quarantine attempt can race this one (see doc above)
			}
			return fmt.Errorf("quarantine %s: %w", p, err)
		}
	}
	return nil
}

// rowScanner abstracts *sql.Row and *sql.Rows behind their shared Scan
// method, so scanRun can serve both GetRun's single-row query and ListRuns's
// per-row loop without duplicating the column list or the null-handling.
type rowScanner interface {
	Scan(dest ...any) error
}

// runUpdateAssignments turns the non-nil fields of u into SQL "col = ?"
// fragments and their bound args, in a fixed column order so the generated
// query is stable and easy to eyeball in logs. This is persistence detail
// carved out of domain.RunUpdate (which stays a pure struct) rather than a
// method on it, since SQL-fragment building must not live in the pure layer.
func runUpdateAssignments(u domain.RunUpdate) ([]string, []any) {
	var sets []string
	var args []any

	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}

	if u.Action != nil {
		add(colRunsAction, *u.Action)
	}
	if u.RebootStartedAt != nil {
		add(colRunsRebootStartedAt, formatTime(*u.RebootStartedAt))
	}
	if u.RouterDownAt != nil {
		add(colRunsRouterDownAt, formatTime(*u.RouterDownAt))
	}
	if u.RouterUpAt != nil {
		add(colRunsRouterUpAt, formatTime(*u.RouterUpAt))
	}
	if u.InternetRestoredAt != nil {
		add(colRunsInternetRestoredAt, formatTime(*u.InternetRestoredAt))
	}
	if u.FinishedAt != nil {
		add(colRunsFinishedAt, formatTime(*u.FinishedAt))
	}
	if u.Outcome != nil {
		add(colRunsOutcome, *u.Outcome)
	}
	if u.Error != nil {
		add(colRunsError, *u.Error)
	}

	return sets, args
}

// scanMetricRecords scans the (run_id, ts, collector, name, value, unit, ok,
// error) column set shared by LatestMetrics and ListMetrics into
// domain.MetricRecord values, closing over rows.Next()/Err() so both callers
// only need to build their own query and hand off the resulting *sql.Rows.
func scanMetricRecords(rows *sql.Rows) ([]domain.MetricRecord, error) {
	var out []domain.MetricRecord
	for rows.Next() {
		var (
			rec    domain.MetricRecord
			ts     string
			value  sql.NullFloat64
			ok     int64
			errStr sql.NullString
		)
		if err := rows.Scan(&rec.RunID, &ts, &rec.Sample.Collector, &rec.Sample.Name, &value, &rec.Sample.Unit, &ok, &errStr); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}

		parsed, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		rec.TS = parsed
		rec.Sample.Value = value.Float64
		rec.Sample.OK = ok != 0
		rec.Sample.Error = errStr.String
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// scanPingRecords scans the (run_id, ts, host, sent, received, avg_ms,
// error) column set ListPings uses into domain.PingRecord values, mirroring
// scanMetricRecords: OK is derived as received>0 rather than stored, and
// avg_ms's nullability distinguishes an unreachable host from a genuine
// 0ms round trip.
func scanPingRecords(rows *sql.Rows) ([]domain.PingRecord, error) {
	var out []domain.PingRecord
	for rows.Next() {
		var (
			rec    domain.PingRecord
			ts     string
			avgMS  sql.NullFloat64
			errStr sql.NullString
		)
		if err := rows.Scan(&rec.RunID, &ts, &rec.Result.Host, &rec.Result.Sent, &rec.Result.Received, &avgMS, &errStr); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}

		parsed, err := parseTime(ts)
		if err != nil {
			return nil, err
		}
		rec.TS = parsed
		rec.Result.AvgMS = avgMS.Float64
		rec.Result.OK = rec.Result.Received > 0
		rec.Result.Error = errStr.String
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// scanRun scans one runs row (the same twelve-column SELECT list GetRun and
// ListRuns both use) into a domain.Run. A missing row's sql.ErrNoRows
// surfaces verbatim so callers can wrap it or errors.Is against it as needed.
func scanRun(row rowScanner) (domain.Run, error) {
	var (
		r                                                             domain.Run
		startedAt                                                     string
		internetOK                                                    sql.NullInt64
		rebootStartedAt, routerDownAt, routerUpAt, internetRestoredAt sql.NullString
		finishedAt, outcome, errStr                                   sql.NullString
	)
	if err := row.Scan(&r.ID, &startedAt, &r.Mode, &internetOK, &r.Action,
		&rebootStartedAt, &routerDownAt, &routerUpAt, &internetRestoredAt,
		&finishedAt, &outcome, &errStr); err != nil {
		return domain.Run{}, err
	}

	parsed, err := parseTime(startedAt)
	if err != nil {
		return domain.Run{}, err
	}
	r.StartedAt = parsed

	if internetOK.Valid {
		v := internetOK.Int64 != 0
		r.InternetOK = &v
	}
	if r.RebootStartedAt, err = parseNullTime(rebootStartedAt); err != nil {
		return domain.Run{}, err
	}
	if r.RouterDownAt, err = parseNullTime(routerDownAt); err != nil {
		return domain.Run{}, err
	}
	if r.RouterUpAt, err = parseNullTime(routerUpAt); err != nil {
		return domain.Run{}, err
	}
	if r.InternetRestoredAt, err = parseNullTime(internetRestoredAt); err != nil {
		return domain.Run{}, err
	}
	if r.FinishedAt, err = parseNullTime(finishedAt); err != nil {
		return domain.Run{}, err
	}
	r.Outcome = outcome.String
	r.Error = errStr.String

	return r, nil
}

// seedUUIDv7WatermarkFromDB seeds the process-wide UUIDv7 monotonic
// watermark (uuid.go's seedUUIDv7Watermark) from the newest id already on
// disk, across EVERY id-bearing table and both tiers (issue #4 FIX 1): a
// cron process that restarts after the Pi's wall clock has stepped backward
// (no RTC, no battery, no NTP — precisely the outage scenario yarddog exists
// to recover from) would otherwise mint a fresh id whose embedded timestamp
// sorts BELOW every row already stored, corrupting every "newest run/check/
// metric" query from that point on. Seeding from runs alone undershoots
// that watermark: within one run, the parent runs row is minted first and
// its checks/metrics/pings children after, so a child id routinely carries
// a higher (ms, counter) than its own run's — MAX(runs.id) misses it
// entirely. A watermark seeded that low then mints its very next id (the
// restarted process's own first run) right back inside the range the
// previous process's children already occupy: not a literal duplicate id
// (newUUIDv7's trailing random bits still make every id practically
// unique), but one that sorts BELOW rows already on disk — exactly the
// ordering corruption this function exists to prevent. seedUUIDv7Watermark
// only ever raises the watermark, never lowers it, so visiting every table
// below and seeding each one's own MAX(id) converges on the true global
// maximum regardless of visiting order. This list must cover EVERY
// id-bearing telemetry table — host/host_archive are the only ones
// deliberately absent, since run_id is itself their primary key rather than
// a separately generated id — a future table that mints its own UUIDv7 id
// and is left off this list silently reintroduces the same undershoot.
func (s *Store) seedUUIDv7WatermarkFromDB(ctx context.Context) error {
	tables := []struct {
		name     string
		idColumn string
	}{
		{tableRuns, colRunsID},
		{tableChecks, colChecksID},
		{tableMetrics, colMetricsID},
		{tablePings, colPingsID},
		{tableRunsArchive, colRunsID},
		{tableChecksArchive, colChecksID},
		{tableMetricsArchive, colMetricsID},
		{tablePingsArchive, colPingsID},
	}
	for _, tbl := range tables {
		var id sql.NullString
		if err := s.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT MAX(%s) FROM %s`, tbl.idColumn, tbl.name),
		).Scan(&id); err != nil {
			return fmt.Errorf("seed uuidv7 watermark from %s: %w", tbl.name, err)
		}
		if !id.Valid {
			continue
		}
		ms, counter, err := uuidv7MsAndCounter(id.String)
		if err != nil {
			return fmt.Errorf("seed uuidv7 watermark from %s: %w", tbl.name, err)
		}
		seedUUIDv7Watermark(ms, counter)
	}
	return nil
}
