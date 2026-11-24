package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	sqlInsertLocalState = `INSERT OR REPLACE INTO local_state
		(path, item_type, hash, size, mtime, ctime, local_device, local_inode, local_birth_time, local_has_identity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	sqlListLocalState = `SELECT
		path, item_type, hash, size, mtime, ctime, local_device, local_inode, local_birth_time, local_has_identity
		FROM local_state
		ORDER BY path`
	sqlSelectLocalStateByPath = `SELECT
		path, item_type, hash, size, mtime, ctime, local_device, local_inode, local_birth_time, local_has_identity
		FROM local_state
		WHERE path = ?`

	// sqlBackfillBaselineIdentity records the observed filesystem identity on
	// baseline rows that predate identity tracking, so a later rename of the
	// same item pairs as a move (R-2.17.1). A folder needs only the same path
	// and type: it has no content of its own. A file must also still match the
	// baseline's content facts, so identity is never attached to content the
	// baseline did not record.
	sqlBackfillBaselineIdentity = `UPDATE baseline
		SET local_device = l.local_device,
			local_inode = l.local_inode,
			local_birth_time = l.local_birth_time,
			local_has_identity = 1
		FROM local_state l
		WHERE l.path = baseline.path
			AND baseline.local_has_identity = 0
			AND l.local_has_identity = 1
			AND COALESCE(l.item_type, '') = COALESCE(baseline.item_type, '')
			AND (
				COALESCE(baseline.item_type, '') = 'folder'
				OR (
					COALESCE(baseline.item_type, '') = 'file'
					AND NULLIF(baseline.local_hash, '') IS NOT NULL
					AND l.hash = baseline.local_hash
					AND l.size = baseline.local_size
					AND l.mtime = baseline.local_mtime
				)
			)`
)

// ReplaceLocalState atomically replaces the durable local snapshot with the
// current admissible observation result for the drive.
func (s *SyncStore) ReplaceLocalState(
	ctx context.Context,
	rows []syncdomain.LocalStateRow,
) error {
	_, err := s.CommitLocalSnapshot(ctx, &LocalSnapshotCommit{Rows: rows})
	return err
}

// LocalSnapshotCommit is what one full local scan commits.
type LocalSnapshotCommit struct {
	Rows []syncdomain.LocalStateRow
	// findings, when not nil, is the scan's whole finding set; it is
	// reconciled before the rows are replaced.
	Findings *syncdomain.ObservationFindingsBatch
	// bind, when not nil, is recorded as the sync folder's identity.
	Bind *synctree.RootIdentity
	// crossedMounts are the mount points the scan crossed.
	CrossedMounts []string
}

// LocalSnapshotCommitResult reports what a snapshot commit changed.
type LocalSnapshotCommitResult struct {
	RowsChanged     bool
	FindingsChanged bool
	// bound is the identity the store holds after a bind.
	Bound *synctree.RootIdentity
	// mountBoundaries are the folders recorded as crossed mount points
	// after the commit.
	MountBoundaries []string
}

// CommitLocalSnapshot commits one full local scan in one transaction: its
// findings, then its rows, its root binding and its crossed mounts. A failure
// anywhere stores none of them (D-001), so a scan's rows never land without
// its findings or its mounts. Only the rows that differ are written: watch
// mode commits a snapshot at every five-minute safety scan, and rewriting an
// idle 100k-file tree each time was gigabytes a day of SSD wear (D-081a).
func (s *SyncStore) CommitLocalSnapshot(
	ctx context.Context,
	commit *LocalSnapshotCommit,
) (result LocalSnapshotCommitResult, err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return result, fmt.Errorf("syncstore: beginning local_state transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "syncstore: rollback local_state transaction")
	}()

	backfilled, err := s.commitLocalSnapshotTx(ctx, tx, commit, &result)
	if err != nil {
		return result, err
	}
	var patch baselineCachePatch
	if backfilled > 0 {
		patch, err = s.reloadPatchTx(ctx, tx)
		if err != nil {
			return result, fmt.Errorf("syncstore: reloading baseline cache after identity backfill: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return result, fmt.Errorf("syncstore: committing local_state transaction: %w", err)
	}
	s.applyBaselineCachePatch(patch)

	return result, nil
}

// commitLocalSnapshotTx writes one snapshot inside tx, in the order
// CommitLocalSnapshot names, fills result, and returns how many baseline
// rows got their filesystem identity backfilled.
func (s *SyncStore) commitLocalSnapshotTx(
	ctx context.Context,
	tx sqlTxRunner,
	commit *LocalSnapshotCommit,
	result *LocalSnapshotCommitResult,
) (int64, error) {
	var err error
	if commit.Findings != nil {
		if result.FindingsChanged, err = s.reconcileObservationFindingsTx(ctx, tx, commit.Findings); err != nil {
			return 0, err
		}
	}
	changed, backfilled, err := replaceLocalTruthTx(ctx, tx, commit.Rows)
	if err != nil {
		return 0, err
	}
	result.RowsChanged = changed
	if commit.Bind != nil {
		if result.Bound, err = bindSyncRootIdentityTx(ctx, tx, *commit.Bind); err != nil {
			return 0, err
		}
	}
	if recordErr := recordMountBoundariesTx(ctx, tx, commit.CrossedMounts); recordErr != nil {
		return 0, recordErr
	}
	if result.MountBoundaries, err = listMountBoundariesTx(ctx, tx); err != nil {
		return 0, err
	}

	return backfilled, nil
}

// replaceLocalTruthTx writes the snapshot's rows and what follows from them:
// the prunes, the identity backfill and the complete-truth mark. It reports
// whether anything changed and how many baseline rows were backfilled.
func replaceLocalTruthTx(
	ctx context.Context,
	tx sqlTxRunner,
	rows []syncdomain.LocalStateRow,
) (bool, int64, error) {
	changed, err := replaceLocalStateTx(ctx, tx, rows)
	if err != nil {
		return false, 0, err
	}
	if pruneErr := pruneKeptLocalFoldersTx(ctx, tx); pruneErr != nil {
		return false, 0, pruneErr
	}
	if pruneErr := pruneLocalHashCacheTx(ctx, tx); pruneErr != nil {
		return false, 0, pruneErr
	}
	backfilled, err := backfillBaselineIdentityTx(ctx, tx)
	if err != nil {
		return false, 0, err
	}
	becameComplete, err := markLocalTruthCompleteTx(ctx, tx)
	if err != nil {
		return false, 0, err
	}
	// Restoring complete local truth is a change even when no row moved:
	// work held or superseded while truth was suspect needs a replan.
	return changed || becameComplete, backfilled, nil
}

// backfillBaselineIdentityTx attaches the full snapshot's filesystem identity
// to baseline rows that lack one and returns how many rows it changed.
func backfillBaselineIdentityTx(ctx context.Context, tx sqlTxRunner) (int64, error) {
	result, err := tx.ExecContext(ctx, sqlBackfillBaselineIdentity)
	if err != nil {
		return 0, fmt.Errorf("syncstore: backfilling baseline identity: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("syncstore: counting backfilled baseline identities: %w", err)
	}

	return changed, nil
}

// ListLocalState returns the durable local snapshot rows in path order.
func (s *SyncStore) ListLocalState(ctx context.Context) ([]syncdomain.LocalStateRow, error) {
	return listLocalStateRows(ctx, s.db)
}

// GetLocalStateByPath returns the durable local_state row for path, or nil
// when the path is absent from the committed local snapshot.
func (s *SyncStore) GetLocalStateByPath(ctx context.Context, path string) (*syncdomain.LocalStateRow, bool, error) {
	row, err := scanLocalStateRow(ctx, s.db, sqlSelectLocalStateByPath, path)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("syncstore: reading local_state row for %s: %w", path, err)
	}

	return row, true, nil
}

// UpsertLocalStateRows applies scoped local observations without changing the
// store's local-truth confidence. Confidence changes are owned by full snapshot
// replacement or explicit suspect markers.
func (s *SyncStore) UpsertLocalStateRows(ctx context.Context, rows []syncdomain.LocalStateRow) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("syncstore: beginning local_state upsert transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "syncstore: rollback local_state upsert transaction")
	}()

	if err := upsertLocalStateRowsTx(ctx, tx, rows); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("syncstore: committing local_state upsert transaction: %w", err)
	}

	return nil
}

func (s *SyncStore) DeleteLocalStatePath(ctx context.Context, path string) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("syncstore: beginning local_state delete transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "syncstore: rollback local_state delete transaction")
	}()

	if err := deleteLocalStatePathTx(ctx, tx, path); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("syncstore: committing local_state delete transaction: %w", err)
	}

	return nil
}

func (s *SyncStore) DeleteLocalStatePrefix(ctx context.Context, prefix string) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("syncstore: beginning local_state prefix delete transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "syncstore: rollback local_state prefix delete transaction")
	}()

	if err := deleteLocalStatePrefixTx(ctx, tx, prefix); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("syncstore: committing local_state prefix delete transaction: %w", err)
	}

	return nil
}

func (s *SyncStore) ApplyLocalStatePatch(
	ctx context.Context,
	rows []syncdomain.LocalStateRow,
	deletedPrefixes []string,
) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("syncstore: beginning local_state patch transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "syncstore: rollback local_state patch transaction")
	}()

	for _, prefix := range deletedPrefixes {
		if err := deleteLocalStatePrefixTx(ctx, tx, prefix); err != nil {
			return err
		}
	}
	if err := upsertLocalStateRowsTx(ctx, tx, rows); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("syncstore: committing local_state patch transaction: %w", err)
	}

	return nil
}

// replaceLocalStateTx makes local_state equal rows by writing only the
// difference, and reports whether anything changed.
//
// It used to delete every row and re-insert the snapshot. Watch mode applies
// a full snapshot at every five-minute safety scan, so an idle 100k-file tree
// rewrote 100k rows (and their index entries and WAL pages) every five
// minutes, gigabytes a day of SSD wear for no change (D-081a).
func replaceLocalStateTx(
	ctx context.Context,
	tx sqlTxRunner,
	rows []syncdomain.LocalStateRow,
) (bool, error) {
	current, err := listLocalStateRows(ctx, tx)
	if err != nil {
		return false, err
	}

	stored := make(map[string]syncdomain.LocalStateRow, len(current))
	for i := range current {
		stored[current[i].Path] = current[i]
	}

	changed := false
	for i := range rows {
		if old, ok := stored[rows[i].Path]; ok {
			delete(stored, rows[i].Path)
			if old == rows[i] {
				continue
			}
		}
		if err := upsertLocalStateRowTx(ctx, tx, rows[i]); err != nil {
			return false, err
		}
		changed = true
	}

	for path := range stored {
		if err := deleteLocalStatePathTx(ctx, tx, path); err != nil {
			return false, err
		}
		changed = true
	}

	return changed, nil
}

func upsertLocalStateRowsTx(ctx context.Context, tx sqlTxRunner, rows []syncdomain.LocalStateRow) error {
	for i := range rows {
		if err := upsertLocalStateRowTx(ctx, tx, rows[i]); err != nil {
			return err
		}
	}

	return nil
}

func upsertLocalStateRowTx(ctx context.Context, tx sqlTxRunner, row syncdomain.LocalStateRow) error {
	if _, err := tx.ExecContext(ctx, sqlInsertLocalState,
		row.Path,
		row.ItemType,
		nullString(row.Hash),
		nullKnownInt64(row.Size, true),
		nullOptionalInt64(row.Mtime),
		row.Ctime,
		int64(row.LocalDevice),
		int64(row.LocalInode),
		row.LocalBirthTime,
		boolInt(row.LocalHasIdentity),
	); err != nil {
		return fmt.Errorf("syncstore: inserting local_state row for %s: %w", row.Path, err)
	}

	return nil
}

func deleteLocalStatePathTx(ctx context.Context, tx sqlTxRunner, path string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_state WHERE path = ?`, path); err != nil {
		return fmt.Errorf("syncstore: deleting local_state row for %s: %w", path, err)
	}

	return nil
}

// sqlDeleteLocalStateSubtree removes a path and every row below it. Every
// path below p sorts between p + "/" and p + "0" under the primary key's
// BINARY collation ('0' follows '/'), so the delete is two seeks on the
// primary key instead of a scan of the table, and no character of the path
// is a pattern. Its arguments are localStateSubtreeArgs.
const sqlDeleteLocalStateSubtree = `DELETE FROM local_state
	WHERE path = ? OR (path >= ? AND path < ?)`

// localStateSubtreeArgs are the arguments of sqlDeleteLocalStateSubtree for
// the subtree at prefix.
func localStateSubtreeArgs(prefix string) []any {
	return []any{prefix, prefix + "/", prefix + "0"}
}

// deleteLocalStatePrefixTx removes the row of prefix and every row below it.
func deleteLocalStatePrefixTx(ctx context.Context, tx sqlTxRunner, prefix string) error {
	if _, err := tx.ExecContext(ctx, sqlDeleteLocalStateSubtree, localStateSubtreeArgs(prefix)...); err != nil {
		return fmt.Errorf("syncstore: deleting local_state prefix for %s: %w", prefix, err)
	}

	return nil
}

// markLocalTruthCompleteTx marks local truth complete and reports whether it
// was not complete before.
func markLocalTruthCompleteTx(ctx context.Context, tx sqlTxRunner) (bool, error) {
	state, err := readObservationStateFromTx(ctx, tx)
	if err != nil {
		return false, err
	}
	if state.LocalTruthComplete && state.LocalTruthRecoveryReason == "" {
		// Already complete: rewriting the row would be one more write per
		// safety scan with nothing to record.
		return false, nil
	}
	wasComplete := state.LocalTruthComplete
	state.LocalTruthComplete = true
	state.LocalTruthRecoveryReason = ""

	return !wasComplete, writeObservationStateToTx(ctx, tx, state)
}

func listLocalStateRows(ctx context.Context, runner sqlTxRunner) ([]syncdomain.LocalStateRow, error) {
	return listLocalStateRowsWithQuery(ctx, runner, sqlListLocalState)
}

func listLocalStateRowsWithQuery(ctx context.Context, runner sqlTxRunner, query string) ([]syncdomain.LocalStateRow, error) {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("syncstore: querying local_state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var result []syncdomain.LocalStateRow
	for rows.Next() {
		var (
			row              syncdomain.LocalStateRow
			hash             sql.NullString
			size             sql.NullInt64
			mtime            sql.NullInt64
			localDevice      int64
			localInode       int64
			localHasIdentity int
		)
		if err := rows.Scan(
			&row.Path,
			&row.ItemType,
			&hash,
			&size,
			&mtime,
			&row.Ctime,
			&localDevice,
			&localInode,
			&row.LocalBirthTime,
			&localHasIdentity,
		); err != nil {
			return nil, fmt.Errorf("syncstore: scanning local_state row: %w", err)
		}
		row.Hash = hash.String
		if size.Valid {
			row.Size = size.Int64
		}
		if mtime.Valid {
			row.Mtime = mtime.Int64
		}
		row.LocalDevice = uint64(localDevice)
		row.LocalInode = uint64(localInode)
		row.LocalHasIdentity = localHasIdentity != 0
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("syncstore: iterating local_state rows: %w", err)
	}

	return result, nil
}

func scanLocalStateRow(
	ctx context.Context,
	runner sqlTxRunner,
	query string,
	args ...any,
) (*syncdomain.LocalStateRow, error) {
	var (
		row              syncdomain.LocalStateRow
		hash             sql.NullString
		size             sql.NullInt64
		mtime            sql.NullInt64
		localDevice      int64
		localInode       int64
		localHasIdentity int
	)
	err := runner.QueryRowContext(ctx, query, args...).Scan(
		&row.Path,
		&row.ItemType,
		&hash,
		&size,
		&mtime,
		&row.Ctime,
		&localDevice,
		&localInode,
		&row.LocalBirthTime,
		&localHasIdentity,
	)
	if err != nil {
		return nil, fmt.Errorf("syncstore: scanning local_state row: %w", err)
	}

	row.Hash = hash.String
	if size.Valid {
		row.Size = size.Int64
	}
	if mtime.Valid {
		row.Mtime = mtime.Int64
	}
	row.LocalDevice = uint64(localDevice)
	row.LocalInode = uint64(localInode)
	row.LocalHasIdentity = localHasIdentity != 0

	return &row, nil
}

// recordMountBoundariesTx remembers folders a scan crossed into another
// filesystem, and forgets flags for folders that are neither synced nor
// present any more. A flag outlives an unmount on purpose: it is what tells
// the next scan that the empty folder is a disk that is not mounted.
func recordMountBoundariesTx(ctx context.Context, tx sqlTxRunner, crossed []string) error {
	for _, boundary := range crossed {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mount_boundaries (path) VALUES (?)`, boundary); err != nil {
			return fmt.Errorf("syncstore: recording mount boundary %s: %w", boundary, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mount_boundaries
		WHERE path NOT IN (SELECT path FROM baseline) AND path NOT IN (SELECT path FROM local_state)`); err != nil {
		return fmt.Errorf("syncstore: pruning mount boundaries: %w", err)
	}
	return nil
}

// ListMountBoundaries returns the folders recorded as crossed mount points.
func (s *SyncStore) ListMountBoundaries(ctx context.Context) ([]string, error) {
	return listMountBoundariesTx(ctx, s.db)
}

func listMountBoundariesTx(ctx context.Context, runner sqlTxRunner) ([]string, error) {
	return queryPathsTx(ctx, runner, `SELECT path FROM mount_boundaries ORDER BY path`, "mount boundaries")
}

// queryPathsTx runs a query whose one column is a path and returns the
// paths in the query's order. what names the rows in its errors.
func queryPathsTx(ctx context.Context, runner sqlTxRunner, query, what string) ([]string, error) {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("syncstore: listing %s: %w", what, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("syncstore: reading %s: %w", what, err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("syncstore: listing %s: %w", what, err)
	}
	return paths, nil
}

// pruneKeptLocalFoldersTx forgets kept folders (recordKeptLocalFolderTx) that
// a full snapshot no longer shows as the same folder at the same path: one
// the user deleted, moved or replaced. The kept row is aliased b so the one
// SQL identity rule, sqlSameLocalEntryL, decides "the same folder".
func pruneKeptLocalFoldersTx(ctx context.Context, tx sqlTxRunner) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM kept_local_folders AS b
		WHERE NOT EXISTS (
			SELECT 1 FROM local_state l
			WHERE l.path = b.path AND l.item_type = 'folder' AND l.local_has_identity = 1
				AND `+sqlSameLocalEntryL+`
		)`); err != nil {
		return fmt.Errorf("syncstore: pruning kept local folders: %w", err)
	}

	return nil
}
