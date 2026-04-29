package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	sqlDeleteLocalState = `DELETE FROM local_state`
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
)

// ReplaceLocalState atomically replaces the durable local snapshot with the
// current admissible observation result for the drive.
func (m *SyncStore) ReplaceLocalState(
	ctx context.Context,
	rows []LocalStateRow,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning local_state transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback local_state transaction")
	}()

	if err := replaceLocalStateTx(ctx, tx, rows); err != nil {
		return err
	}
	if err := markLocalTruthCompleteTx(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing local_state transaction: %w", err)
	}

	return nil
}

// ListLocalState returns the durable local snapshot rows in path order.
func (m *SyncStore) ListLocalState(ctx context.Context) ([]LocalStateRow, error) {
	return listLocalStateRows(ctx, m.db)
}

// GetLocalStateByPath returns the durable local_state row for path, or nil
// when the path is absent from the committed local snapshot.
func (m *SyncStore) GetLocalStateByPath(ctx context.Context, path string) (*LocalStateRow, bool, error) {
	row, err := scanLocalStateRow(ctx, m.db, sqlSelectLocalStateByPath, path)
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
func (m *SyncStore) UpsertLocalStateRows(ctx context.Context, rows []LocalStateRow) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
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

func (m *SyncStore) DeleteLocalStatePath(ctx context.Context, path string) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
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

func (m *SyncStore) DeleteLocalStatePrefix(ctx context.Context, prefix string) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
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

func (m *SyncStore) applyLocalStatePatch(
	ctx context.Context,
	rows []LocalStateRow,
	deletedPaths []string,
	deletedPrefixes []string,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
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

func replaceLocalStateTx(
	ctx context.Context,
	tx sqlTxRunner,
	rows []LocalStateRow,
) error {
	if _, err := tx.ExecContext(ctx, sqlDeleteLocalState); err != nil {
		return fmt.Errorf("sync: deleting local_state rows: %w", err)
	}

	return upsertLocalStateRowsTx(ctx, tx, rows)
}

func upsertLocalStateRowsTx(ctx context.Context, tx sqlTxRunner, rows []LocalStateRow) error {
	for i := range rows {
		if err := upsertLocalStateRowTx(ctx, tx, rows[i]); err != nil {
			return err
		}
	}

	return nil
}

func upsertLocalStateRowTx(ctx context.Context, tx sqlTxRunner, row LocalStateRow) error {
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
			   OR substr(path, 1, ?) = ? COLLATE BINARY`,
		prefix,
		len(descendantPrefix),
		descendantPrefix,
	); err != nil {
		return fmt.Errorf("sync: deleting local_state prefix for %s: %w", prefix, err)
	}

	return nil
}

func markLocalTruthCompleteTx(ctx context.Context, tx sqlTxRunner) error {
	state, err := readObservationStateFromTx(ctx, tx)
	if err != nil {
		return err
	}
	state.LocalTruthComplete = true
	state.LocalTruthRecoveryReason = ""
	return writeObservationStateToTx(ctx, tx, state)
}

func listLocalStateRows(ctx context.Context, runner sqlTxRunner) ([]LocalStateRow, error) {
	return listLocalStateRowsWithQuery(ctx, runner, sqlListLocalState)
}

func listLocalStateRowsWithQuery(ctx context.Context, runner sqlTxRunner, query string) ([]LocalStateRow, error) {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sync: querying local_state: %w", err)
	}
	defer rows.Close()

	var result []LocalStateRow
	for rows.Next() {
		var (
			row              LocalStateRow
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
) (*LocalStateRow, error) {
	var (
		row              LocalStateRow
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

func buildLocalStateRows(result ScanResult) []LocalStateRow {
	rows := make([]LocalStateRow, len(result.Rows))
	copy(rows, result.Rows)
	return rows
}
