package infrastructure

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/prorochestvo/yarddog/domain"
	"github.com/prorochestvo/yarddog/services"

	_ "modernc.org/sqlite"
)

// NewStore opens the SQLite database at path with a single-connection pool —
// it is OpenStore(ctx, path, 1). The collector and every :memory: test in
// this codebase use NewStore; the daemon calls OpenStore directly with its
// own configured pool size.
func NewStore(ctx context.Context, path string) (*Store, error) {
	return OpenStore(ctx, path, 1)
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
func OpenStore(ctx context.Context, path string, maxOpenConns int) (*Store, error) {
	db, err := sql.Open("sqlite", path)
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

	if !isMemoryDB(path) {
		if err := chmodStoreFiles(path); err != nil {
			if closeErr := db.Close(); closeErr != nil {
				return nil, fmt.Errorf("%w (also failed to close db: %v)", err, closeErr)
			}
			return nil, err
		}
	}

	return s, nil
}

// Store is the SQLite-backed persistence layer for runs, checks, host
// telemetry, and the Telegram outbox (design §9; plans/003-host-telemetry.md).
// It implements services.RunRepository, services.MetricsRepository, and
// services.OutboxRepository; main passes the one *Store into
// services.Execute as the first two and into services.NewOutboxService as
// the third.
type Store struct {
	db *sql.DB
}

var (
	_ services.RunRepository     = (*Store)(nil)
	_ services.MetricsRepository = (*Store)(nil)
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

// EnqueueOutboxMessage inserts an unsent tg_outbox row carrying text and
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

// GetHost reads back the host sidecar row for runID. It exists so tests can
// verify what SaveMetrics persisted; it is off-port (not part of
// MetricsRepository) since the orchestrator never reads a snapshot back,
// mirroring GetRun's role for the runs table.
func (s *Store) GetHost(ctx context.Context, runID int64) (domain.HostInfo, error) {
	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s FROM %s WHERE %s = ?`,
			colHostHostname, colHostOS, colHostArch, tableHost, colHostRunID),
		runID,
	)

	var h domain.HostInfo
	if err := row.Scan(&h.Hostname, &h.OS, &h.Arch); err != nil {
		if err == sql.ErrNoRows {
			return domain.HostInfo{}, fmt.Errorf("get host for run %d: %w", runID, sql.ErrNoRows)
		}
		return domain.HostInfo{}, fmt.Errorf("get host for run %d: %w", runID, err)
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

// GetRun reads back the runs row id in full. It exists primarily so tests
// and any future inspection tooling can verify what InsertRun/UpdateRun
// persisted; the recovery loop itself only ever writes. It is off-port
// (not part of RunRepository) since the orchestrator never reads a run back.
// A missing id returns sql.ErrNoRows wrapped with %w, so callers (RunByID)
// can errors.Is against it.
func (s *Store) GetRun(ctx context.Context, id int64) (domain.Run, error) {
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
		return domain.Run{}, fmt.Errorf("get run %d: %w", id, err)
	}
	return r, nil
}

// IncrementOutboxAttempt records a failed send attempt on the tg_outbox row
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

// InsertCheck inserts one checks row. c.RunID, c.Phase, c.Target, and
// c.Kind must be set; c.LatencyMS and c.Error are optional.
func (s *Store) InsertCheck(ctx context.Context, c domain.Check) error {
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			tableChecks, colChecksRunID, colChecksTS, colChecksPhase, colChecksTarget,
			colChecksKind, colChecksOK, colChecksLatencyMS, colChecksError),
		c.RunID, formatTime(c.TS), c.Phase, c.Target, c.Kind, boolToInt(c.OK),
		nullInt64(c.LatencyMS), nullString(c.Error),
	)
	if err != nil {
		return fmt.Errorf("insert check for run %d target %s: %w", c.RunID, c.Target, err)
	}
	return nil
}

// InsertRun inserts a new runs row and returns its id. mode is "soft" or
// "hard"; internetOK is nil in hard mode, where no initial check is made
// (design §7).
func (s *Store) InsertRun(ctx context.Context, startedAt time.Time, mode string, internetOK *bool) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s) VALUES (?, ?, ?, ?)`,
			tableRuns, colRunsStartedAt, colRunsMode, colRunsInternetOK, colRunsAction),
		formatTime(startedAt), mode, nullBoolToInt(internetOK), domain.ActionNone,
	)
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert run: last insert id: %w", err)
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
// tests and any future report tooling see them in insertion order.
func (s *Store) ListChecksByRun(ctx context.Context, runID int64) ([]domain.Check, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s, %s, %s, %s FROM %s WHERE %s = ? ORDER BY %s ASC`,
			colChecksRunID, colChecksTS, colChecksPhase, colChecksTarget, colChecksKind,
			colChecksOK, colChecksLatencyMS, colChecksError,
			tableChecks, colChecksRunID, colChecksID),
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list checks for run %d: %w", runID, err)
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
			return nil, fmt.Errorf("list checks for run %d: scan: %w", runID, err)
		}
		c.TS, err = parseTime(ts)
		if err != nil {
			return nil, fmt.Errorf("list checks for run %d: %w", runID, err)
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
		return nil, fmt.Errorf("list checks for run %d: %w", runID, err)
	}

	return out, nil
}

// ListMetrics returns metrics history matching f, newest first, bounded by
// f.Limit (a parameterized LIMIT — never string-concatenated, Risk R8).
// f.Since's zero value omits the "ts >= ?" clause; f.Collector's zero value
// omits the "collector = ?" clause.
func (s *Store) ListMetrics(ctx context.Context, f services.MetricsFilter) ([]domain.MetricRecord, error) {
	query := fmt.Sprintf(`SELECT %s, %s, %s, %s, %s, %s, %s, %s FROM %s`,
		colMetricsRunID, colMetricsTS, colMetricsCollector, colMetricsName,
		colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError, tableMetrics)

	var (
		conds []string
		args  []any
	)
	if !f.Since.IsZero() {
		conds = append(conds, colMetricsTS+" >= ?")
		args = append(args, formatTime(f.Since))
	}
	if f.Collector != "" {
		conds = append(conds, colMetricsCollector+" = ?")
		args = append(args, string(f.Collector))
	}
	if !f.IncludeEmpty {
		// drop unavailable rows in SQL so LIMIT counts only returned rows.
		conds = append(conds, colMetricsOK+" = 1")
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY %s DESC LIMIT ?", colMetricsID)
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
func (s *Store) ListMetricsByRun(ctx context.Context, runID int64) ([]domain.MetricSample, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s, %s, %s, %s, %s, %s FROM %s WHERE %s = ? ORDER BY %s ASC`,
			colMetricsCollector, colMetricsName, colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError,
			tableMetrics, colMetricsRunID, colMetricsID),
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list metrics for run %d: %w", runID, err)
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
			return nil, fmt.Errorf("list metrics for run %d: scan: %w", runID, err)
		}
		m.Value = value.Float64
		m.OK = ok != 0
		m.Error = errStr.String
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list metrics for run %d: %w", runID, err)
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

// ListUnsentOutboxMessages returns every tg_outbox row with sent_at still
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

// MarkOutboxSent sets sent_at on the tg_outbox row id.
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

// PruneChecks deletes checks rows older than retentionDays. retentionDays<=0
// means "keep forever" and is a no-op — the caller passes RETENTION_DAYS
// straight through without special-casing 0 (design §9: a 0-day cutoff must
// never be computed as "now", which would wipe every row). Off-port: main
// calls it directly on the concrete *Store at startup.
func (s *Store) PruneChecks(ctx context.Context, now time.Time, retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}

	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	_, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE %s < ?`, tableChecks, colChecksTS),
		formatTime(cutoff),
	)
	if err != nil {
		return fmt.Errorf("prune checks older than %d days: %w", retentionDays, err)
	}
	return nil
}

// PruneMetrics deletes metrics and host rows older than retentionDays,
// mirroring PruneChecks (retentionDays<=0 means "keep forever", a no-op).
// Off-port: main calls it directly on the concrete *Store at startup, next
// to PruneChecks.
func (s *Store) PruneMetrics(ctx context.Context, now time.Time, retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}

	cutoff := formatTime(now.Add(-time.Duration(retentionDays) * 24 * time.Hour))
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s < ?`, tableMetrics, colMetricsTS), cutoff); err != nil {
		return fmt.Errorf("prune metrics older than %d days: %w", retentionDays, err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE %s < ?`, tableHost, colHostTS), cutoff); err != nil {
		return fmt.Errorf("prune host older than %d days: %w", retentionDays, err)
	}
	return nil
}

// RunByID reads back runs row id in full, translating GetRun's sql.ErrNoRows
// into a plain found bool (services.HistoryRepository's "not found" contract,
// so services never needs to import database/sql). Any other GetRun error
// still propagates.
func (s *Store) RunByID(ctx context.Context, id int64) (domain.Run, bool, error) {
	run, err := s.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Run{}, false, nil
		}
		return domain.Run{}, false, err
	}
	return run, true, nil
}

// SaveMetrics writes one telemetry snapshot: the host sidecar row plus one
// metrics row per sample, all in a single transaction so a partial snapshot
// (e.g. a failed sample insert) never persists (plans/003-host-telemetry.md).
// host.run_id is a PRIMARY KEY, so a second SaveMetrics call for the same
// runID fails outright — collectMetrics calls it exactly once per run, so
// this is a correctness guard rather than a case to INSERT OR REPLACE
// around.
func (s *Store) SaveMetrics(ctx context.Context, runID int64, ts time.Time, m domain.HostMetrics) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save metrics for run %d: begin: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit (sanctioned discard, error-recovery path)

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?)`,
			tableHost, colHostRunID, colHostTS, colHostHostname, colHostOS, colHostArch),
		runID, formatTime(ts), m.Host.Hostname, m.Host.OS, m.Host.Arch,
	); err != nil {
		return fmt.Errorf("save metrics for run %d: host: %w", runID, err)
	}

	for _, sm := range m.Samples {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (%s, %s, %s, %s, %s, %s, %s, %s) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				tableMetrics, colMetricsRunID, colMetricsTS, colMetricsCollector, colMetricsName,
				colMetricsValue, colMetricsUnit, colMetricsOK, colMetricsError),
			runID, formatTime(ts), string(sm.Collector), sm.Name,
			nullFloat(sm.Value, sm.OK), sm.Unit, boolToInt(sm.OK), nullString(sm.Error),
		); err != nil {
			return fmt.Errorf("save metrics for run %d: sample %s/%s: %w", runID, sm.Collector, sm.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save metrics for run %d: commit: %w", runID, err)
	}
	return nil
}

// UpdateRun applies every non-nil field of u to the runs row id. Callers
// build a domain.RunUpdate with only the fields that changed at a given
// phase transition (e.g. just RouterDownAt) and leave the rest nil.
func (s *Store) UpdateRun(ctx context.Context, id int64, u domain.RunUpdate) error {
	sets, args := runUpdateAssignments(u)
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)

	query := fmt.Sprintf(`UPDATE %s SET %s WHERE %s = ?`, tableRuns, joinAssignments(sets), colRunsID)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update run %d: %w", id, err)
	}
	return nil
}

const (
	tableRuns    = "runs"
	tableChecks  = "checks"
	tableOutbox  = "tg_outbox"
	tableMetrics = "metrics"
	tableHost    = "host"

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

	colChecksID        = "id"
	colChecksRunID     = "run_id"
	colChecksTS        = "ts"
	colChecksPhase     = "phase"
	colChecksTarget    = "target"
	colChecksKind      = "kind"
	colChecksOK        = "ok"
	colChecksLatencyMS = "latency_ms"
	colChecksError     = "error"

	idxChecksRun = "idx_checks_run"
	idxChecksTS  = "idx_checks_ts"

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

	idxMetricsRun = "idx_metrics_run"
	idxMetricsTS  = "idx_metrics_ts"

	colHostRunID    = "run_id"
	colHostTS       = "ts"
	colHostHostname = "hostname"
	colHostOS       = "os"
	colHostArch     = "arch"

	// timeFormat is RFC3339 in UTC (design §9). Every timestamp column is
	// stored in this fixed-width format so retention pruning can compare
	// them lexicographically without parsing.
	timeFormat = time.RFC3339
)

// boolToInt renders a Go bool as the 0/1 SQLite stores in an INTEGER
// column (checks.ok, which is NOT NULL and so needs no separate nullable
// helper).
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
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
	pragmas := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
	}
	for _, p := range pragmas {
		if _, err := s.db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}

	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s INTEGER PRIMARY KEY,
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
			%s INTEGER PRIMARY KEY,
			%s INTEGER NOT NULL REFERENCES %s(%s),
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
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxChecksTS, tableChecks, colChecksTS),
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
			%s INTEGER PRIMARY KEY,
			%s INTEGER NOT NULL REFERENCES %s(%s),
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
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s(%s)`, idxMetricsTS, tableMetrics, colMetricsTS),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			%s INTEGER PRIMARY KEY REFERENCES %s(%s),
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL,
			%s TEXT NOT NULL
		)`, tableHost,
			colHostRunID, tableRuns, colRunsID, colHostTS, colHostHostname, colHostOS, colHostArch,
		),
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}

	return nil
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
