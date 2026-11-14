package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"
)

// storeInspector is a read-only sync-state boundary for CLI status and other
// package-local readers that must not own raw SQLite access themselves.
type storeInspector struct {
	db     *sql.DB
	logger *slog.Logger
	// betweenSnapshotReads, when set, runs between the two halves of a status
	// snapshot read (readDriveStatusCounts, readDriveStatusConditions).
	// Test-only; nil in production: a test writes the store there to prove
	// the snapshot is one moment.
	betweenSnapshotReads func()
}

// sqlListRemoteDriftItems lists already-observed remote-side differences
// that are not yet reflected in baseline, for countVisibleRemoteDrift to
// count the ones the content filter shows. This is the durable remote half of
// "not fully converged yet"; exact local-only drift still requires a live scan.
// The content test mirrors the planner's remote_changed (sqlite_compare.go):
// folders change only by path or type, and a file's hash decides when either
// side has one. Comparing folder mtimes, which baseline never records, made
// every remote folder count as drift on a fully synced drive.
const sqlListRemoteDriftItems = `
SELECT path, item_type FROM (
	SELECT rs.item_id, rs.path, rs.item_type
	FROM remote_state rs
	LEFT JOIN baseline b
	  ON b.item_id = rs.item_id
	WHERE (
		b.item_id IS NULL OR
		b.path <> rs.path OR
		b.item_type <> rs.item_type OR
		(rs.item_type <> 'folder' AND (
			CASE
				WHEN COALESCE(b.remote_hash, '') <> '' OR COALESCE(rs.hash, '') <> '' THEN
					COALESCE(b.remote_hash, '') <> COALESCE(rs.hash, '')
				ELSE
					COALESCE(b.remote_size, 0) <> COALESCE(rs.size, 0) OR
					COALESCE(b.remote_mtime, 0) <> COALESCE(rs.mtime, 0) OR
					COALESCE(b.etag, '') <> COALESCE(rs.etag, '')
			END
		))
	  )
	UNION
	SELECT b.item_id, b.path, b.item_type
	FROM baseline b
	LEFT JOIN remote_state rs
	  ON rs.item_id = b.item_id
	WHERE rs.item_id IS NULL
) remote_drift`

// sqlCountLocalDriftItems counts observed local-side differences that are not
// yet reflected in baseline: the local half of "not fully converged yet". In
// watch mode local_state is live truth, so an edit counts from the moment the
// observer commits it until its upload lands, and `status` cannot say
// "Up to date" while the engine still owes a plan or a transfer for it
// (R-2.10.50). The content test mirrors the planner's local_changed
// (sqlite_compare.go): folders differ only by presence or type, and a file's
// hash decides when either side has one. The root row is not an item.
const sqlCountLocalDriftItems = `
SELECT COUNT(*) FROM (
	SELECT ls.path
	FROM local_state ls
	LEFT JOIN baseline b
	  ON b.path = ls.path
	WHERE ls.path <> '' AND ls.item_type <> 'root' AND (
		b.path IS NULL OR
		b.item_type <> ls.item_type OR
		(ls.item_type <> 'folder' AND (
			CASE
				WHEN COALESCE(b.local_hash, '') <> '' OR COALESCE(ls.hash, '') <> '' THEN
					COALESCE(b.local_hash, '') <> COALESCE(ls.hash, '')
				ELSE
					COALESCE(b.local_size, 0) <> COALESCE(ls.size, 0) OR
					COALESCE(b.local_mtime, 0) <> COALESCE(ls.mtime, 0)
			END
		))
	  )
	UNION
	SELECT b.path
	FROM baseline b
	LEFT JOIN local_state ls
	  ON ls.path = b.path
	WHERE b.path <> '' AND b.item_type <> 'root' AND ls.path IS NULL
) local_drift`

// DriveStatusSnapshot is the raw per-mount authority snapshot consumed by the
// product-facing status command. The store owns the read-only database access,
// but the CLI owns grouping and presentation policy.
type DriveStatusSnapshot struct {
	BaselineFileCount int
	RemoteDriftItems  int
	LocalDriftItems   int
	RetryingItems     int
	RetryingWork      []RetryWorkRow
	// ActionableRetryWork is the work a condition only the user can clear
	// refused at execution (R-2.10.71); status lists it under that condition
	// next to the scanner's own findings.
	ActionableRetryWork []RetryWorkRow
	ObservationIssues   []ObservationIssueRow
	BlockScopes         []*BlockScope
	BlockedRetryWork    []RetryWorkRow
	// HeldDeletes are the deletes the delete-safety hold keeps until the
	// user runs `sync --confirm-deletes`.
	HeldDeletes []HeldDeleteRow
	// NeverSynced is a store that has not completed a sync: no delta cursor
	// committed and an empty baseline, as right after `drive
	// reset-sync-state`. Zero drift in such a store means nothing is known,
	// not that everything is up to date (R-6.6.25).
	NeverSynced bool
}

// ReadDriveStatusSnapshot opens a read-only inspector for one per-mount status
// snapshot and closes it before returning so callers do not own inspector
// lifecycle.
func ReadDriveStatusSnapshot(
	ctx context.Context,
	dbPath string,
	logger *slog.Logger,
) (DriveStatusSnapshot, error) {
	return ReadDriveStatusSnapshotWithFilter(ctx, dbPath, logger, ContentFilterConfig{})
}

// ReadDriveStatusSnapshotWithFilter is ReadDriveStatusSnapshot for a drive
// synced under a content filter (included_dirs, ignores): the remote-changes
// count skips items outside the scope, which the planner never acts on and
// which a drive limited to one folder would otherwise report forever as
// pending (R-2.10.48).
func ReadDriveStatusSnapshotWithFilter(
	ctx context.Context,
	dbPath string,
	logger *slog.Logger,
	filter ContentFilterConfig,
) (DriveStatusSnapshot, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) (DriveStatusSnapshot, error) {
		return inspector.ReadDriveStatusSnapshotWithFilter(ctx, filter)
	})
}

// countVisibleRemoteDrift counts the remote drift rows the filter lets the
// planner see (store_planner_visible.go applies the same test to its rows).
func countVisibleRemoteDrift(ctx context.Context, runner sqlTxRunner, filter ContentFilterConfig) (int, error) {
	rows, err := runner.QueryContext(ctx, sqlListRemoteDriftItems)
	if err != nil {
		return 0, fmt.Errorf("query remote drift rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	visibility := newContentFilter(filter)
	count := 0
	for rows.Next() {
		var path string
		var itemType ItemType
		if err := rows.Scan(&path, &itemType); err != nil {
			return 0, fmt.Errorf("scan remote drift row: %w", err)
		}
		if visibility.Visible(path, itemType) {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate remote drift rows: %w", err)
	}

	return count, nil
}

// readPathTruthStatus opens a read-only inspector, derives current truth
// availability for the requested paths from durable observation issues and
// read-boundary issue scope keys, and closes the inspector before returning.
func readPathTruthStatus(
	ctx context.Context,
	dbPath string,
	logger *slog.Logger,
	paths []string,
) (map[string]pathTruthStatus, error) {
	return readWithInspector(dbPath, logger, func(inspector *storeInspector) (map[string]pathTruthStatus, error) {
		return inspector.ReadPathTruthStatus(ctx, paths)
	})
}

// ReadDriveStatusSnapshot reads the raw per-mount authority snapshot from an
// already-open sync store.
func (s *SyncStore) ReadDriveStatusSnapshot(ctx context.Context) (DriveStatusSnapshot, error) {
	if s == nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status snapshot: nil store")
	}

	inspector := &storeInspector{
		db:     s.db,
		logger: s.logger,
	}

	return inspector.ReadDriveStatusSnapshot(ctx)
}

// ReadPathTruthStatus derives current truth availability for the requested
// paths from an already-open sync store.
func (s *SyncStore) ReadPathTruthStatus(
	ctx context.Context,
	paths []string,
) (map[string]pathTruthStatus, error) {
	if s == nil {
		return nil, fmt.Errorf("read path truth status: nil store")
	}

	inspector := &storeInspector{
		db:     s.db,
		logger: s.logger,
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

// finalizeInspectorRead joins the inspector's close error to the read's
// result. A close that fails after a successful read is logged at Debug and
// dropped on purpose: the connection is read-only and the rows are already
// read, so the read's answer stands.
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
			logger.Debug("close sync store inspector after successful read",
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

// inspectorBusyTimeout is how long a read-only inspector statement waits for
// a lock another connection holds before it fails with SQLITE_BUSY.
const inspectorBusyTimeout = time.Second

func openReadOnlySyncStoreDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open(sqliteDriverName, sqliteFileURI(dbPath, url.Values{
		"mode": {"ro"},
		sqlitePragmaParam: {
			"query_only(1)",
			fmt.Sprintf("busy_timeout(%d)", inspectorBusyTimeout.Milliseconds()),
		},
	}))
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
	return i.ReadDriveStatusSnapshotWithFilter(ctx, ContentFilterConfig{})
}

// ReadDriveStatusSnapshotWithFilter reads the snapshot for a drive whose sync
// scope is filter: remote drift counts only items the planner would see.
// Every part is read in one transaction, so the snapshot is one moment of a
// store a live engine writes: a write committed while it reads is seen whole
// or not at all.
func (i *storeInspector) ReadDriveStatusSnapshotWithFilter(
	ctx context.Context, filter ContentFilterConfig,
) (snapshot DriveStatusSnapshot, err error) {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status: begin read transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "read drive status: end read transaction")
	}()

	if snapshot, err = readDriveStatusCounts(ctx, tx, filter); err != nil {
		return DriveStatusSnapshot{}, err
	}
	if i.betweenSnapshotReads != nil {
		i.betweenSnapshotReads()
	}
	if conditionsErr := readDriveStatusConditions(ctx, tx, &snapshot); conditionsErr != nil {
		return DriveStatusSnapshot{}, conditionsErr
	}

	return snapshot, nil
}

// readDriveStatusCounts reads the first half of a status snapshot: the
// baseline and drift counts and the observation issues.
func readDriveStatusCounts(ctx context.Context, runner sqlTxRunner, filter ContentFilterConfig) (DriveStatusSnapshot, error) {
	snapshot := DriveStatusSnapshot{}

	var err error
	snapshot.BaselineFileCount, err = readCount(ctx, runner, "SELECT COUNT(*) FROM baseline WHERE item_type = 'file'")
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status baseline count: %w", err)
	}
	baselineItems, err := readCount(ctx, runner, "SELECT COUNT(*) FROM baseline WHERE item_type <> 'root'")
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status baseline items: %w", err)
	}
	cursors, err := readCount(ctx, runner, "SELECT COUNT(*) FROM observation_state WHERE cursor <> ''")
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status observation cursor: %w", err)
	}
	snapshot.NeverSynced = baselineItems == 0 && cursors == 0
	snapshot.RemoteDriftItems, err = countVisibleRemoteDrift(ctx, runner, filter)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status remote drift count: %w", err)
	}
	snapshot.LocalDriftItems, err = readCount(ctx, runner, sqlCountLocalDriftItems)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status local drift count: %w", err)
	}
	snapshot.ObservationIssues, err = queryObservationIssueRowsWithRunner(ctx, runner)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status observation issues: %w", err)
	}

	return snapshot, nil
}

// readDriveStatusConditions reads the second half of a status snapshot: block
// scopes, retry work and held deletes.
func readDriveStatusConditions(ctx context.Context, runner sqlTxRunner, snapshot *DriveStatusSnapshot) error {
	var err error
	snapshot.BlockScopes, err = queryBlockScopeRowsWithRunner(ctx, runner)
	if err != nil {
		return fmt.Errorf("read drive status block scopes: %w", err)
	}
	snapshot.BlockedRetryWork, err = queryBlockedRetryWorkRowsWithRunner(ctx, runner)
	if err != nil {
		return fmt.Errorf("read drive status blocked retry_work: %w", err)
	}
	snapshot.HeldDeletes, err = queryHeldDeleteRowsWithRunner(ctx, runner)
	if err != nil {
		return fmt.Errorf("read drive status held deletes: %w", err)
	}
	snapshot.RetryingWork, err = queryRetryingRetryWorkRowsWithRunner(ctx, runner)
	if err != nil {
		return fmt.Errorf("read drive status retrying retry_work: %w", err)
	}
	// The count is the rows just listed, which status names.
	snapshot.RetryingItems = len(snapshot.RetryingWork)
	snapshot.ActionableRetryWork, err = queryActionableRetryWorkRowsWithRunner(ctx, runner)
	if err != nil {
		return fmt.Errorf("read drive status actionable retry_work: %w", err)
	}

	return nil
}

func (i *storeInspector) ReadShortcutRootStatusSnapshot(
	ctx context.Context,
	namespaceID string,
	parentSyncRoot string,
) ([]ShortcutRootStatusView, error) {
	records, err := queryShortcutRootRecords(ctx, i.db, "read shortcut root status rows", "iterate shortcut root status rows")
	if err != nil {
		return nil, err
	}
	return shortcutRootStatusViewsFromRecords(records, namespaceID, parentSyncRoot), nil
}

// ReadPathTruthStatus returns the derived current-truth availability for the
// requested paths without materializing any additional durable rows.
func (i *storeInspector) ReadPathTruthStatus(
	ctx context.Context,
	paths []string,
) (map[string]pathTruthStatus, error) {
	if len(paths) == 0 {
		return map[string]pathTruthStatus{}, nil
	}

	observationIssues, err := queryObservationIssueRowsWithRunner(ctx, i.db)
	if err != nil {
		return nil, fmt.Errorf("read path truth status observation issues: %w", err)
	}
	return newTruthAvailabilityIndex(observationIssues).StatusByPath(paths), nil
}

func readCount(ctx context.Context, runner sqlTxRunner, query string) (int, error) {
	var count int
	if err := runner.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("scan count query: %w", err)
	}

	return count, nil
}
