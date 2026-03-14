// store.go — SyncStore type definition, constructor, lifecycle, and helpers.
//
// Contents:
//   - SyncStore:    struct definition (db, baseline, logger, nowFunc)
//   - NewSyncStore: open database, run migrations, return ready store
//   - Close:        WAL checkpoint + close database
//   - Checkpoint:   WAL checkpoint + optional pruning
//   - rawDB:        test-only access to underlying *sql.DB
//   - nullString:   empty string → SQL NULL
//   - nullInt64:    zero → SQL NULL
//
// Related files:
//   - store_baseline.go:    baseline CRUD, delta tokens, outcome commits
//   - store_observation.go: remote state observation persistence
//   - store_conflicts.go:   conflict management
//   - store_failures.go:    sync failure recording and queries
//   - store_admin.go:       state reader/admin operations
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	// Pure-Go SQLite driver (no CGO).
	_ "modernc.org/sqlite"
)

// Compile-time interface satisfaction checks.
var (
	_ ObservationWriter   = (*SyncStore)(nil)
	_ OutcomeWriter       = (*SyncStore)(nil)
	_ StateReader         = (*SyncStore)(nil)
	_ StateAdmin          = (*SyncStore)(nil)
	_ SyncFailureRecorder = (*SyncStore)(nil)
	_ ScopeBlockStore     = (*SyncStore)(nil)
)

// SyncStore is the sole writer to the sync database. It loads the
// baseline at pass start and commits outcomes at pass end.
type SyncStore struct {
	db         *sql.DB
	baseline   *Baseline
	baselineMu stdsync.Mutex // guards baseline cache (Load is called from multiple workers)
	logger     *slog.Logger
	nowFunc    func() time.Time // injectable for deterministic tests
}

// NewSyncStore opens the SQLite database at dbPath, runs migrations,
// and returns a ready-to-use manager. The database uses WAL mode with
// synchronous=FULL for crash-safe durability.
func NewSyncStore(dbPath string, logger *slog.Logger) (*SyncStore, error) {
	// DSN parameters ensure pragmas apply to every connection from the pool.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"+
			"&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"+
			"&_pragma=journal_size_limit(67108864)",
		dbPath,
	)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sync: opening database %s: %w", dbPath, err)
	}

	// Sole-writer pattern: only one connection writes at a time.
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	if err := runMigrations(ctx, db, logger); err != nil {
		db.Close()
		return nil, err
	}

	logger.Info("baseline manager initialized", slog.String("db_path", dbPath))

	return &SyncStore{
		db:      db,
		logger:  logger,
		nowFunc: time.Now,
	}, nil
}

// Close checkpoints the WAL and closes the underlying database connection.
// The explicit checkpoint ensures cross-process readers (e.g., `issues
// --history` after `sync`) see all committed data when they open a new
// connection to the same database file.
func (m *SyncStore) Close() error {
	// WAL checkpoint only (no pruning) on close.
	if err := m.Checkpoint(context.Background(), 0); err != nil {
		m.logger.Warn("checkpoint failed on close", slog.String("error", err.Error()))
	}

	return m.db.Close()
}

// Checkpoint performs WAL checkpoint and optionally prunes soft-deleted rows
// older than retention. Called: after initial sync, every 30 minutes, and on
// shutdown. Pass retention=0 to skip pruning (WAL checkpoint only).
func (m *SyncStore) Checkpoint(ctx context.Context, retention time.Duration) error {
	if _, err := m.db.ExecContext(ctx,
		"PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		m.logger.Warn("WAL checkpoint failed", slog.String("error", err.Error()))
	}

	if retention <= 0 {
		return nil
	}

	cutoff := m.nowFunc().Add(-retention).UnixNano()

	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM remote_state WHERE sync_status = 'deleted' AND observed_at < ?`,
		cutoff); err != nil {
		return fmt.Errorf("prune deleted remote_state: %w", err)
	}

	// Actionable failures are kept for user visibility but pruned after retention
	// to prevent unbounded growth of stale entries.
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE category = 'actionable' AND last_seen_at < ?`,
		cutoff); err != nil {
		return fmt.Errorf("prune actionable sync_failures: %w", err)
	}

	return nil
}

// DataVersion returns SQLite's PRAGMA data_version, which changes every time
// another connection commits a write. The engine's own writes don't change it.
// Used to detect CLI→DB modifications (e.g. `issues clear`) without polling
// the full table.
func (m *SyncStore) DataVersion(ctx context.Context) (int64, error) {
	var version int64
	if err := m.db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("sync: PRAGMA data_version: %w", err)
	}

	return version, nil
}

// rawDB returns the underlying database connection for test access.
// Unexported to prevent external packages from bypassing typed interfaces.
func (m *SyncStore) rawDB() *sql.DB {
	return m.db
}

// ---------------------------------------------------------------------------
// Nullable helpers: empty string / zero int → NULL in SQLite.
// ---------------------------------------------------------------------------

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{String: s, Valid: true}
}

// nullInt64 maps Go zero (0) to SQL NULL. This conflates "actual zero" with
// "absent" — acceptable for Size (zero-byte files are rare edge cases) and
// Mtime (Unix epoch is not a realistic modification time). If a legitimate
// zero value needs to be stored in the future, use a separate sentinel.
func nullInt64(n int64) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: n, Valid: true}
}
