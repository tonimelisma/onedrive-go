// Package sync persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - SyncStore:    struct definition (db, baseline, logger, nowFunc)
//   - NewSyncStore: open database, apply canonical schema, return ready store
//   - Close:        WAL checkpoint + close database
//   - Checkpoint:   WAL checkpoint + optional pruning
//   - rawDB:            test-only access to underlying *sql.DB
//   - nullString:       empty string → SQL NULL
//   - nullOptionalInt64: zero → SQL NULL for optional fields like mtimes
//   - nullKnownInt64:   preserve zero for known values like file sizes
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
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/fsroot"

	// Pure-Go SQLite driver (no CGO).
	_ "modernc.org/sqlite"
)

const syncStoreDirPerm = 0o700

// SyncStore is the sole writer to the sync database. It loads the
// baseline at pass start and commits outcomes at pass end.
type SyncStore struct {
	db         *sql.DB
	baseline   *Baseline
	baselineMu stdsync.Mutex // guards baseline cache (Load is called from multiple workers)
	logger     *slog.Logger
	nowFunc    func() time.Time // injectable for deterministic tests
}

// NewSyncStore opens the SQLite database at dbPath, applies the canonical
// schema, and returns a ready-to-use manager. The database uses WAL mode with
// synchronous=FULL for crash-safe durability.
func NewSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (*SyncStore, error) {
	root, _, err := fsroot.OpenPath(dbPath)
	if err != nil {
		return nil, fmt.Errorf("prepare sync store path: %w", err)
	}
	if mkdirErr := root.MkdirAll(syncStoreDirPerm); mkdirErr != nil {
		return nil, fmt.Errorf("create sync store directory: %w", mkdirErr)
	}

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

	if err := applySchema(ctx, db); err != nil {
		baseErr := fmt.Errorf("apply sync store schema: %w", err)
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(baseErr, fmt.Errorf("close sync store database: %w", closeErr))
		}

		return nil, baseErr
	}

	logger.Info("baseline manager initialized", slog.String("db_path", dbPath))

	store := &SyncStore{
		db:      db,
		logger:  logger,
		nowFunc: time.Now,
	}

	repairsApplied, repairErr := store.repairScopeStateConsistencyOnOpen(ctx)
	if repairErr != nil {
		baseErr := fmt.Errorf("repair sync store scope state: %w", repairErr)
		if closeErr := db.Close(); closeErr != nil {
			return nil, errors.Join(baseErr, fmt.Errorf("close sync store database: %w", closeErr))
		}

		return nil, baseErr
	}
	if repairsApplied > 0 {
		logger.Info("sync store scope state repaired",
			slog.Int("repairs", repairsApplied),
			slog.String("db_path", dbPath),
		)
	}

	return store, nil
}

// Close checkpoints the WAL and closes the underlying database connection.
// The explicit checkpoint ensures cross-process readers (e.g., `issues
// --history` after `sync`) see all committed data when they open a new
// connection to the same database file.
func (m *SyncStore) Close(ctx context.Context) error {
	// WAL checkpoint only (no pruning) on close.
	if err := m.Checkpoint(ctx, 0); err != nil {
		m.logger.Warn("checkpoint failed on close", slog.String("error", err.Error()))
	}

	if err := m.db.Close(); err != nil {
		return fmt.Errorf("close sync store database: %w", err)
	}

	return nil
}

// Checkpoint performs WAL checkpoint and optionally prunes old actionable
// failures. Called after initial sync, every 30 minutes, and on shutdown.
// Pass retention=0 to skip pruning (WAL checkpoint only).
func (m *SyncStore) Checkpoint(ctx context.Context, retention time.Duration) error {
	if _, err := m.db.ExecContext(ctx,
		"PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		m.logger.Warn("WAL checkpoint failed", slog.String("error", err.Error()))
	}

	if retention <= 0 {
		return nil
	}

	cutoff := m.nowFunc().Add(-retention).UnixNano()

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
// Used to detect CLI→DB modifications (e.g. `resolve deletes`) without polling
// the full table.
func (m *SyncStore) DataVersion(ctx context.Context) (int64, error) {
	var version int64
	if err := m.db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("sync: PRAGMA data_version: %w", err)
	}

	return version, nil
}

// DB returns the underlying *sql.DB for tests that need to verify schema or
// run raw queries. Do not use in production code — use the typed methods on
// SyncStore instead to maintain interface compliance.
func (m *SyncStore) DB() *sql.DB {
	return m.db
}

// SetNowFunc overrides the time source used for syncedAt timestamps in Commit.
// Used in tests to produce deterministic timestamps without mocking the real
// clock. Must be called before Commit.
func (m *SyncStore) SetNowFunc(fn func() time.Time) {
	m.nowFunc = fn
}

// Baseline returns the in-memory baseline cache populated by the most recent
// Load or Commit call. Returns nil before the first Load/Commit. Used by
// tests to inspect baseline state without a round-trip through Load().
func (m *SyncStore) Baseline() *Baseline {
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()

	return m.baseline
}

// ---------------------------------------------------------------------------
// Nullable helpers. Strings use empty = NULL. Integers are split by
// semantics: optional values use 0 = NULL, while known values preserve 0.
// ---------------------------------------------------------------------------

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{String: s, Valid: true}
}

func nullOptionalInt64(n int64) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: n, Valid: true}
}

func nullKnownInt64(n int64, known bool) sql.NullInt64 {
	if !known {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: n, Valid: true}
}
