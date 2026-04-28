package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	_ "modernc.org/sqlite"
)

// storeInspector is a read-only sync-state boundary for CLI status and other
// package-local readers that must not own raw SQLite access themselves.
type storeInspector struct {
	db     *sql.DB
	logger *slog.Logger
}

// sqlCountRemoteDriftItems counts already-observed remote-side differences
// that are not yet reflected in baseline. This is the durable remote half of
// "not fully converged yet"; exact local-only drift still requires a live scan.
const sqlCountRemoteDriftItems = `
SELECT COUNT(*) FROM (
	SELECT rs.item_id
	FROM remote_state rs
	LEFT JOIN baseline b
	  ON b.item_id = rs.item_id
	WHERE (
		b.item_id IS NULL OR
		b.path <> rs.path OR
		b.item_type <> rs.item_type OR
		COALESCE(b.remote_hash, '') <> COALESCE(rs.hash, '') OR
		COALESCE(b.remote_mtime, 0) <> COALESCE(rs.mtime, 0)
	  )
	UNION
	SELECT b.item_id
	FROM baseline b
	LEFT JOIN remote_state rs
	  ON rs.item_id = b.item_id
	WHERE rs.item_id IS NULL
) remote_drift`

// DriveStatusSnapshot is the raw per-mount authority snapshot consumed by the
// product-facing status command. The store owns the read-only database access,
// but the CLI owns grouping and presentation policy.
type DriveStatusSnapshot struct {
	BaselineEntryCount int
	RemoteDriftItems   int
	RetryingItems      int
	ObservationIssues  []ObservationIssueRow
	BlockScopes        []*BlockScope
	BlockedRetryWork   []RetryWorkRow
}

// ReadDriveStatusSnapshot opens a read-only inspector for one per-mount status
// snapshot and closes it before returning so callers do not own inspector
// lifecycle.
func ReadDriveStatusSnapshot(
	ctx context.Context,
	dbPath string,
	logger *slog.Logger,
) (DriveStatusSnapshot, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) (DriveStatusSnapshot, error) {
		return inspector.ReadDriveStatusSnapshot(ctx)
	})
}

// ReadShortcutRootStatusSnapshot opens a read-only inspector for parent-owned
// shortcut-root lifecycle state and closes it before returning.
func ReadShortcutRootStatusSnapshot(
	ctx context.Context,
	dbPath string,
	logger *slog.Logger,
) ([]ShortcutRootStatusView, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) ([]ShortcutRootStatusView, error) {
		return inspector.ReadShortcutRootStatusSnapshot(ctx)
	})
}

// ReadPathTruthStatus opens a read-only inspector, derives current truth
// availability for the requested paths from durable observation issues and
// read-boundary issue scope keys, and closes the inspector before returning.
func ReadPathTruthStatus(
	ctx context.Context,
	dbPath string,
	logger *slog.Logger,
	paths []string,
) (map[string]PathTruthStatus, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) (map[string]PathTruthStatus, error) {
		return inspector.ReadPathTruthStatus(ctx, paths)
	})
}

// ReadDriveStatusSnapshot reads the raw per-mount authority snapshot from an
// already-open sync store.
func (m *SyncStore) ReadDriveStatusSnapshot(ctx context.Context) (DriveStatusSnapshot, error) {
	if m == nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status snapshot: nil store")
	}

	inspector := &storeInspector{
		db:     m.db,
		logger: m.logger,
	}

	return inspector.ReadDriveStatusSnapshot(ctx)
}

// ReadShortcutRootStatusSnapshot reads parent-owned shortcut-root lifecycle
// state from an already-open sync store.
func (m *SyncStore) ReadShortcutRootStatusSnapshot(ctx context.Context) ([]ShortcutRootStatusView, error) {
	if m == nil {
		return nil, fmt.Errorf("read shortcut root status snapshot: nil store")
	}

	inspector := &storeInspector{
		db:     m.db,
		logger: m.logger,
	}

	return inspector.ReadShortcutRootStatusSnapshot(ctx)
}

// ReadPathTruthStatus derives current truth availability for the requested
// paths from an already-open sync store.
func (m *SyncStore) ReadPathTruthStatus(
	ctx context.Context,
	paths []string,
) (map[string]PathTruthStatus, error) {
	if m == nil {
		return nil, fmt.Errorf("read path truth status: nil store")
	}

	inspector := &storeInspector{
		db:     m.db,
		logger: m.logger,
	}

	return inspector.ReadPathTruthStatus(ctx, paths)
}

func readWithInspector[T any](
	dbPath string,
	logger *slog.Logger,
	read func(*storeInspector) (T, error),
) (result T, err error) {
	inspector, err := openStoreInspector(dbPath, logger)
	if err != nil {
		return result, fmt.Errorf("open sync store inspector: %w", err)
	}

	defer func() {
		if closeErr := inspector.Close(); closeErr != nil {
			result, err = finalizeInspectorRead(dbPath, logger, result, err, closeErr)
		}
	}()

	return read(inspector)
}

func finalizeInspectorRead[T any](
	dbPath string,
	logger *slog.Logger,
	result T,
	readErr error,
	closeErr error,
) (T, error) {
	if closeErr == nil {
		return result, readErr
	}

	wrappedCloseErr := fmt.Errorf("close sync store inspector %s: %w", dbPath, closeErr)
	if readErr == nil {
		if logger != nil {
			logger.Warn("close sync store inspector after successful read",
				slog.String("path", dbPath),
				slog.Any("error", wrappedCloseErr),
			)
		}

		return result, nil
	}

	return result, errors.Join(readErr, wrappedCloseErr)
}

func openStoreInspector(dbPath string, logger *slog.Logger) (*storeInspector, error) {
	db, err := openReadOnlySyncStoreDB(dbPath)
	if err != nil {
		return nil, err
	}

	return &storeInspector{
		db:     db,
		logger: logger,
	}, nil
}

func openReadOnlySyncStoreDB(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only sync store %s: %w", dbPath, err)
	}

	return db, nil
}

func (i *storeInspector) Close() error {
	if err := i.db.Close(); err != nil {
		return fmt.Errorf("close read-only sync store: %w", err)
	}

	return nil
}

// ReadDriveStatusSnapshot returns the per-mount raw authority snapshot used by
// the product-facing status command.
func (i *storeInspector) ReadDriveStatusSnapshot(ctx context.Context) (DriveStatusSnapshot, error) {
	snapshot := DriveStatusSnapshot{}

	var err error
	snapshot.BaselineEntryCount, err = i.readCount(ctx, "SELECT COUNT(*) FROM baseline")
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status baseline count: %w", err)
	}
	snapshot.RemoteDriftItems, err = i.readCount(
		ctx,
		sqlCountRemoteDriftItems,
	)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status remote drift count: %w", err)
	}
	snapshot.RetryingItems, err = i.readCount(
		ctx,
		"SELECT COUNT(*) FROM retry_work WHERE blocked = 0 AND attempt_count >= 3",
	)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status retrying count: %w", err)
	}

	snapshot.ObservationIssues, err = queryObservationIssueRowsWithRunner(ctx, i.db)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status observation issues: %w", err)
	}
	snapshot.BlockScopes, err = queryBlockScopeRowsWithRunner(ctx, i.db)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status block scopes: %w", err)
	}
	snapshot.BlockedRetryWork, err = queryBlockedRetryWorkRowsWithRunner(ctx, i.db)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status blocked retry_work: %w", err)
	}

	return snapshot, nil
}

func (i *storeInspector) ReadShortcutRootStatusSnapshot(ctx context.Context) ([]ShortcutRootStatusView, error) {
	records, err := queryShortcutRootRecords(ctx, i.db, "read shortcut root status rows", "iterate shortcut root status rows")
	if err != nil {
		return nil, err
	}
	return ShortcutRootStatusViewsFromRecords(records), nil
}

// ReadPathTruthStatus returns the derived current-truth availability for the
// requested paths without materializing any additional durable rows.
func (i *storeInspector) ReadPathTruthStatus(
	ctx context.Context,
	paths []string,
) (map[string]PathTruthStatus, error) {
	if len(paths) == 0 {
		return map[string]PathTruthStatus{}, nil
	}

	observationIssues, err := queryObservationIssueRowsWithRunner(ctx, i.db)
	if err != nil {
		return nil, fmt.Errorf("read path truth status observation issues: %w", err)
	}
	return NewTruthAvailabilityIndex(observationIssues).StatusByPath(paths), nil
}

func (i *storeInspector) readCount(ctx context.Context, query string) (int, error) {
	var count int
	if err := i.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("scan count query: %w", err)
	}

	return count, nil
}
