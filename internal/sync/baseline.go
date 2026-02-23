package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	// Pure-Go SQLite driver (no CGO).
	_ "modernc.org/sqlite"
)

// SQL statements for baseline operations.
const (
	sqlLoadBaseline = `SELECT path, drive_id, item_id, parent_id, item_type,
		local_hash, remote_hash, size, mtime, synced_at, etag
		FROM baseline`

	sqlGetDeltaToken = `SELECT token FROM delta_tokens WHERE drive_id = ?` //nolint:gosec // G101: "token" is a delta cursor, not credentials

	sqlUpsertBaseline = `INSERT INTO baseline
		(path, drive_id, item_id, parent_id, item_type, local_hash, remote_hash,
		 size, mtime, synced_at, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		 drive_id = excluded.drive_id,
		 item_id = excluded.item_id,
		 parent_id = excluded.parent_id,
		 item_type = excluded.item_type,
		 local_hash = excluded.local_hash,
		 remote_hash = excluded.remote_hash,
		 size = excluded.size,
		 mtime = excluded.mtime,
		 synced_at = excluded.synced_at,
		 etag = excluded.etag`

	sqlDeleteBaseline = `DELETE FROM baseline WHERE path = ?`

	sqlInsertConflict = `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at,
		 local_hash, remote_hash, local_mtime, remote_mtime, resolution)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'unresolved')`

	sqlUpsertDeltaToken = `INSERT INTO delta_tokens (drive_id, token, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(drive_id) DO UPDATE SET
		 token = excluded.token,
		 updated_at = excluded.updated_at`
)

// BaselineManager is the sole writer to the sync database. It loads the
// baseline at cycle start and commits outcomes at cycle end.
type BaselineManager struct {
	db       *sql.DB
	baseline *Baseline
	logger   *slog.Logger
	nowFunc  func() time.Time // injectable for deterministic tests
}

// NewBaselineManager opens the SQLite database at dbPath, runs migrations,
// and returns a ready-to-use manager. The database uses WAL mode with
// synchronous=FULL for crash-safe durability.
func NewBaselineManager(dbPath string, logger *slog.Logger) (*BaselineManager, error) {
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

	return &BaselineManager{
		db:      db,
		logger:  logger,
		nowFunc: time.Now,
	}, nil
}

// Load reads the entire baseline table into memory, populating ByPath and
// ByID maps. The result is cached on the manager for use during the cycle.
func (m *BaselineManager) Load(ctx context.Context) (*Baseline, error) {
	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline: %w", err)
	}
	defer rows.Close()

	b := &Baseline{
		ByPath: make(map[string]*BaselineEntry),
		ByID:   make(map[string]*BaselineEntry),
	}

	for rows.Next() {
		entry, err := scanBaselineRow(rows)
		if err != nil {
			return nil, err
		}

		b.ByPath[entry.Path] = entry
		b.ByID[entry.DriveID+":"+entry.ItemID] = entry
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating baseline rows: %w", err)
	}

	m.baseline = b
	m.logger.Debug("baseline loaded", slog.Int("entries", len(b.ByPath)))

	return b, nil
}

// scanBaselineRow scans a single row from the baseline table, handling
// nullable columns with sql.Null* types.
func scanBaselineRow(rows *sql.Rows) (*BaselineEntry, error) {
	var (
		e          BaselineEntry
		itemType   string
		parentID   sql.NullString
		localHash  sql.NullString
		remoteHash sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
	)

	err := rows.Scan(
		&e.Path, &e.DriveID, &e.ItemID, &parentID, &itemType,
		&localHash, &remoteHash, &size, &mtime, &e.SyncedAt, &etag,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning baseline row: %w", err)
	}

	parsed, err := ParseItemType(itemType)
	if err != nil {
		return nil, err
	}

	e.ItemType = parsed
	e.ParentID = parentID.String
	e.LocalHash = localHash.String
	e.RemoteHash = remoteHash.String
	e.ETag = etag.String

	if size.Valid {
		e.Size = size.Int64
	}

	if mtime.Valid {
		e.Mtime = mtime.Int64
	}

	return &e, nil
}

// GetDeltaToken returns the saved delta token for a drive, or empty string
// if no token has been saved yet.
func (m *BaselineManager) GetDeltaToken(ctx context.Context, driveID string) (string, error) {
	var token string

	err := m.db.QueryRowContext(ctx, sqlGetDeltaToken, driveID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("sync: getting delta token for drive %s: %w", driveID, err)
	}

	return token, nil
}

// Commit atomically applies all successful outcomes and saves the delta
// token in a single transaction. After commit, the in-memory baseline
// cache is refreshed.
func (m *BaselineManager) Commit(ctx context.Context, outcomes []Outcome, deltaToken, driveID string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning commit transaction: %w", err)
	}
	defer tx.Rollback()

	syncedAt := m.nowFunc().UnixNano()

	if err := m.applyOutcomes(ctx, tx, outcomes, syncedAt); err != nil {
		return err
	}

	if deltaToken != "" {
		if err := m.saveDeltaToken(ctx, tx, driveID, deltaToken, syncedAt); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing transaction: %w", err)
	}

	m.logger.Info("baseline committed",
		slog.Int("outcomes", len(outcomes)),
		slog.String("drive_id", driveID),
	)

	// Refresh the in-memory cache after commit.
	if _, err := m.Load(ctx); err != nil {
		return fmt.Errorf("sync: refreshing baseline after commit: %w", err)
	}

	return nil
}

// applyOutcomes iterates through outcomes and dispatches each to the
// appropriate commit helper based on action type.
func (m *BaselineManager) applyOutcomes(
	ctx context.Context, tx *sql.Tx, outcomes []Outcome, syncedAt int64,
) error {
	for i := range outcomes {
		o := &outcomes[i]
		if !o.Success {
			continue
		}

		var err error

		switch o.Action {
		case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
			err = commitUpsert(ctx, tx, o, syncedAt)
		case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
			err = commitDelete(ctx, tx, o.Path)
		case ActionLocalMove, ActionRemoteMove:
			err = commitMove(ctx, tx, o, syncedAt)
		case ActionConflict:
			err = commitConflict(ctx, tx, o, syncedAt)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// commitUpsert inserts or updates a baseline entry for download, upload,
// folder create, and update-synced outcomes.
func commitUpsert(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	_, err := tx.ExecContext(ctx, sqlUpsertBaseline,
		o.Path, o.DriveID, o.ItemID,
		nullString(o.ParentID),
		o.ItemType.String(),
		nullString(o.LocalHash),
		nullString(o.RemoteHash),
		nullInt64(o.Size),
		nullInt64(o.Mtime),
		syncedAt,
		nullString(o.ETag),
	)
	if err != nil {
		return fmt.Errorf("sync: upserting baseline for %s: %w", o.Path, err)
	}

	return nil
}

// commitDelete removes a baseline entry for delete and cleanup outcomes.
func commitDelete(ctx context.Context, tx *sql.Tx, path string) error {
	_, err := tx.ExecContext(ctx, sqlDeleteBaseline, path)
	if err != nil {
		return fmt.Errorf("sync: deleting baseline for %s: %w", path, err)
	}

	return nil
}

// commitMove deletes the old path and inserts the new path for move outcomes.
func commitMove(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	if err := commitDelete(ctx, tx, o.OldPath); err != nil {
		return err
	}

	return commitUpsert(ctx, tx, o, syncedAt)
}

// commitConflict inserts a conflict record for later resolution.
func commitConflict(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	conflictID := uuid.New().String()

	_, err := tx.ExecContext(ctx, sqlInsertConflict,
		conflictID, o.DriveID,
		nullString(o.ItemID),
		o.Path, o.ConflictType, syncedAt,
		nullString(o.LocalHash),
		nullString(o.RemoteHash),
		nullInt64(o.Mtime),
		nullInt64(0), // remote_mtime not available in Outcome
	)
	if err != nil {
		return fmt.Errorf("sync: inserting conflict for %s: %w", o.Path, err)
	}

	return nil
}

// saveDeltaToken persists the delta token in the same transaction as
// baseline updates.
func (m *BaselineManager) saveDeltaToken(
	ctx context.Context, tx *sql.Tx, driveID, token string, updatedAt int64,
) error {
	_, err := tx.ExecContext(ctx, sqlUpsertDeltaToken, driveID, token, updatedAt)
	if err != nil {
		return fmt.Errorf("sync: saving delta token for drive %s: %w", driveID, err)
	}

	return nil
}

// Close closes the underlying database connection.
func (m *BaselineManager) Close() error {
	return m.db.Close()
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

func nullInt64(n int64) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: n, Valid: true}
}
