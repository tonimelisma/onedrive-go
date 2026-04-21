// Package sync persists sync baseline, observation issues, retry work,
// block-scope, and sync-status state.
//
// Contents:
//   - SyncStore:    struct definition (db, baseline, logger, nowFunc)
//   - NewSyncStore: open database, apply canonical schema, return ready store
//   - Close:        WAL checkpoint + close database
//   - Checkpoint:   WAL checkpoint + optional pruning
//   - nullString:   empty string → SQL NULL
//   - nullOptionalInt64: zero → SQL NULL for optional fields like mtimes
//   - nullKnownInt64:   preserve zero for known values like file sizes
//
// Related files:
//   - store_write_baseline.go:     baseline CRUD and outcome commits
//   - store_write_observation.go:  remote state observation persistence
//   - store_observation_issues.go: observation issue recording and read helpers
//   - store_retry_work.go:         exact retry work persistence and mutation helpers
//   - store_inspect.go:            read-only raw status snapshot helpers and inspector lifecycle
//   - store_sync_status.go:        product-facing sync-status persistence
//   - store_scope_admin.go:        scope release/discard mutation helpers
//   - store_compatibility.go:      startup diagnosis for unsupported existing DBs
//   - store_reset.go:              explicit delete-and-recreate helper
package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

	configuredDriveMu       stdsync.RWMutex
	cachedConfiguredDriveID driveid.ID
}

// NewSyncStore opens the SQLite database at dbPath, applies the canonical
// schema, and returns a ready-to-use manager. The database uses WAL mode with
// synchronous=FULL for crash-safe durability.
func NewSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (*SyncStore, error) {
	return openSyncStore(ctx, dbPath, logger, true)
}

func openSyncStore(ctx context.Context, dbPath string, logger *slog.Logger, ensureSchema bool) (*SyncStore, error) {
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

	if ensureSchema {
		if err := applySchema(ctx, db); err != nil {
			baseErr := fmt.Errorf("apply sync store schema: %w", err)
			if closeErr := db.Close(); closeErr != nil {
				return nil, errors.Join(baseErr, fmt.Errorf("close sync store database: %w", closeErr))
			}

			return nil, baseErr
		}
	}

	logger.Info("sync store initialized", slog.String("db_path", dbPath))

	store := &SyncStore{
		db:      db,
		logger:  logger,
		nowFunc: time.Now,
	}

	return store, nil
}

// Close checkpoints the WAL and closes the underlying database connection.
// The explicit checkpoint ensures cross-process readers (for example `status`
// after `sync`) see all committed data when they open a new connection to the
// same database file.
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

// Checkpoint performs WAL checkpoint and optionally prunes stale observation
// issues. Called after initial sync, every 30 minutes, and on shutdown.
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

	// Observation issues are user-visible current-truth problems and can be
	// rebuilt from fresh observation, so stale rows are pruned after retention
	// to prevent unbounded growth.
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM observation_issues WHERE last_seen_at < ?`,
		cutoff); err != nil {
		return fmt.Errorf("prune stale observation issues: %w", err)
	}

	return nil
}

// rawDB exposes the underlying SQLite handle for same-package tests and store
// internals that need assertions below the typed API surface.
func (m *SyncStore) rawDB() *sql.DB {
	return m.db
}

// setNowFunc overrides the time source used for syncedAt timestamps in Commit.
// Used in tests to produce deterministic timestamps without mocking the real
// clock. Must be called before Commit.
func (m *SyncStore) setNowFunc(fn func() time.Time) {
	m.nowFunc = fn
}

// cachedBaseline returns the in-memory baseline cache populated by the most recent
// Load or Commit call. Returns nil before the first Load/Commit. Used by
// tests to inspect baseline state without a round-trip through Load().
func (m *SyncStore) cachedBaseline() *Baseline {
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
