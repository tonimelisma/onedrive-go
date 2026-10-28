package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

const (
	sqlInsertLocalState = `INSERT OR REPLACE INTO local_state
		(path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	sqlListLocalState = `SELECT
		path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity
		FROM local_state
		ORDER BY path`
	sqlSelectLocalStateByPath = `SELECT
		path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity
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
	rows []localStateRow,
) error {
	_, _, err := s.replaceLocalState(ctx, rows, nil, nil)
	return err
}

// ReplaceLocalStateReportingChange is ReplaceLocalState for callers that need
// to know whether the snapshot differed from the stored one: the watch
// runtime replans only when a periodic safety scan found a difference.
func (s *SyncStore) ReplaceLocalStateReportingChange(
	ctx context.Context,
	rows []localStateRow,
) (bool, error) {
	changed, _, err := s.replaceLocalState(ctx, rows, nil, nil)
	return changed, err
}

// ReplaceLocalStateBindingRoot replaces local_state and, when bind is not
// nil, records it as the sync folder's identity in the same transaction: the
// binding and the first snapshot taken of that folder land together or not
// at all (D-001).
func (s *SyncStore) ReplaceLocalStateBindingRoot(
	ctx context.Context,
	rows []localStateRow,
	bind *synctree.RootIdentity,
	crossedMounts []string,
) (bound *synctree.RootIdentity, err error) {
	_, bound, err = s.replaceLocalState(ctx, rows, bind, crossedMounts)
	return bound, err
}

func (s *SyncStore) replaceLocalState(
	ctx context.Context,
	rows []localStateRow,
	bind *synctree.RootIdentity,
	crossedMounts []string,
) (changed bool, bound *synctree.RootIdentity, err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return false, nil, fmt.Errorf("sync: beginning local_state transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback local_state transaction")
	}()

	changed, err = replaceLocalStateTx(ctx, tx, rows)
	if err != nil {
		return false, nil, err
	}
	backfilled, backfillErr := backfillBaselineIdentityTx(ctx, tx)
	if backfillErr != nil {
		return false, nil, backfillErr
	}
	becameComplete, markErr := markLocalTruthCompleteTx(ctx, tx)
	if markErr != nil {
		return false, nil, markErr
	}
	// Restoring complete local truth is a change even when no row moved:
	// work held or superseded while truth was suspect needs a replan.
	changed = changed || becameComplete
	if bind != nil {
		if bound, err = bindSyncRootIdentityTx(ctx, tx, *bind); err != nil {
			return false, nil, err
		}
	}
	if err := recordMountBoundariesTx(ctx, tx, crossedMounts); err != nil {
		return false, nil, err
	}

	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("sync: committing local_state transaction: %w", err)
	}

	if backfilled > 0 {
		if err := s.reloadBaselineCache(ctx); err != nil {
			return false, nil, fmt.Errorf("sync: reloading baseline cache after identity backfill: %w", err)
		}
	}

	return changed, bound, nil
}

// backfillBaselineIdentityTx attaches the full snapshot's filesystem identity
// to baseline rows that lack one and returns how many rows it changed.
func backfillBaselineIdentityTx(ctx context.Context, tx sqlTxRunner) (int64, error) {
	result, err := tx.ExecContext(ctx, sqlBackfillBaselineIdentity)
	if err != nil {
		return 0, fmt.Errorf("sync: backfilling baseline identity: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: counting backfilled baseline identities: %w", err)
	}

	return changed, nil
}

// ListLocalState returns the durable local snapshot rows in path order.
func (s *SyncStore) ListLocalState(ctx context.Context) ([]localStateRow, error) {
	return listLocalStateRows(ctx, s.db)
}

// GetLocalStateByPath returns the durable local_state row for path, or nil
// when the path is absent from the committed local snapshot.
func (s *SyncStore) GetLocalStateByPath(ctx context.Context, path string) (*localStateRow, bool, error) {
	row, err := scanLocalStateRow(ctx, s.db, sqlSelectLocalStateByPath, path)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("sync: reading local_state row for %s: %w", path, err)
	}

	return row, true, nil
}

// UpsertLocalStateRows applies scoped local observations without changing the
// store's local-truth confidence. Confidence changes are owned by full snapshot
// replacement or explicit suspect markers.
func (s *SyncStore) UpsertLocalStateRows(ctx context.Context, rows []localStateRow) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning local_state upsert transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback local_state upsert transaction")
	}()

	if err := upsertLocalStateRowsTx(ctx, tx, rows); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing local_state upsert transaction: %w", err)
	}

	return nil
}

func (s *SyncStore) DeleteLocalStatePath(ctx context.Context, path string) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning local_state delete transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback local_state delete transaction")
	}()

	if err := deleteLocalStatePathTx(ctx, tx, path); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing local_state delete transaction: %w", err)
	}

	return nil
}

func (s *SyncStore) DeleteLocalStatePrefix(ctx context.Context, prefix string) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning local_state prefix delete transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback local_state prefix delete transaction")
	}()

	if err := deleteLocalStatePrefixTx(ctx, tx, prefix); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing local_state prefix delete transaction: %w", err)
	}

	return nil
}

func (s *SyncStore) applyLocalStatePatch(
	ctx context.Context,
	rows []localStateRow,
	deletedPaths []string,
	deletedPrefixes []string,
) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning local_state patch transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback local_state patch transaction")
	}()

	for _, prefix := range deletedPrefixes {
		if err := deleteLocalStatePrefixTx(ctx, tx, prefix); err != nil {
			return err
		}
	}
	for _, path := range deletedPaths {
		if err := deleteLocalStatePathTx(ctx, tx, path); err != nil {
			return err
		}
	}
	if err := upsertLocalStateRowsTx(ctx, tx, rows); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing local_state patch transaction: %w", err)
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
	rows []localStateRow,
) (bool, error) {
	current, err := listLocalStateRows(ctx, tx)
	if err != nil {
		return false, err
	}

	stored := make(map[string]localStateRow, len(current))
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

func upsertLocalStateRowsTx(ctx context.Context, tx sqlTxRunner, rows []localStateRow) error {
	for i := range rows {
		if err := upsertLocalStateRowTx(ctx, tx, rows[i]); err != nil {
			return err
		}
	}

	return nil
}

func upsertLocalStateRowTx(ctx context.Context, tx sqlTxRunner, row localStateRow) error {
	if _, err := tx.ExecContext(ctx, sqlInsertLocalState,
		row.Path,
		row.ItemType,
		nullString(row.Hash),
		nullKnownInt64(row.Size, true),
		nullOptionalInt64(row.Mtime),
		int64(row.LocalDevice),
		int64(row.LocalInode),
		boolInt(row.LocalHasIdentity),
	); err != nil {
		return fmt.Errorf("sync: inserting local_state row for %s: %w", row.Path, err)
	}

	return nil
}

func deleteLocalStatePathTx(ctx context.Context, tx sqlTxRunner, path string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_state WHERE path = ?`, path); err != nil {
		return fmt.Errorf("sync: deleting local_state row for %s: %w", path, err)
	}

	return nil
}

func deleteLocalStatePrefixTx(ctx context.Context, tx sqlTxRunner, prefix string) error {
	descendantPrefix := prefix + "/"
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM local_state
			WHERE path = ? COLLATE BINARY
			   OR substr(path, 1, length(?)) = ? COLLATE BINARY`,
		prefix,
		descendantPrefix,
		descendantPrefix,
	); err != nil {
		return fmt.Errorf("sync: deleting local_state prefix for %s: %w", prefix, err)
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

func listLocalStateRows(ctx context.Context, runner sqlTxRunner) ([]localStateRow, error) {
	return listLocalStateRowsWithQuery(ctx, runner, sqlListLocalState)
}

func listLocalStateRowsWithQuery(ctx context.Context, runner sqlTxRunner, query string) ([]localStateRow, error) {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sync: querying local_state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var result []localStateRow
	for rows.Next() {
		var (
			row              localStateRow
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
			&localDevice,
			&localInode,
			&localHasIdentity,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning local_state row: %w", err)
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
		return nil, fmt.Errorf("sync: iterating local_state rows: %w", err)
	}

	return result, nil
}

func scanLocalStateRow(
	ctx context.Context,
	runner sqlTxRunner,
	query string,
	args ...any,
) (*localStateRow, error) {
	var (
		row              localStateRow
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
		&localDevice,
		&localInode,
		&localHasIdentity,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning local_state row: %w", err)
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
			return fmt.Errorf("sync: recording mount boundary %s: %w", boundary, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mount_boundaries
		WHERE path NOT IN (SELECT path FROM baseline) AND path NOT IN (SELECT path FROM local_state)`); err != nil {
		return fmt.Errorf("sync: pruning mount boundaries: %w", err)
	}
	return nil
}

// listMountBoundaries returns the folders recorded as crossed mount points.
func (s *SyncStore) listMountBoundaries(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM mount_boundaries ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing mount boundaries: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var paths []string
	for rows.Next() {
		var boundary string
		if err := rows.Scan(&boundary); err != nil {
			return nil, fmt.Errorf("sync: scanning mount boundary: %w", err)
		}
		paths = append(paths, boundary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating mount boundaries: %w", err)
	}
	return paths, nil
}
