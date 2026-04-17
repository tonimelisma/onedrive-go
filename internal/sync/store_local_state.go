package sync

import (
	"context"
	"fmt"
)

const (
	sqlDeleteLocalState = `DELETE FROM local_state`
	sqlInsertLocalState = `INSERT INTO local_state
		(path, item_type, hash, size, mtime, content_identity, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	sqlListLocalState = `SELECT
		path, item_type, hash, size, mtime, content_identity, observed_at
		FROM local_state
		ORDER BY path`
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

	if _, err := tx.ExecContext(ctx, sqlDeleteLocalState); err != nil {
		return fmt.Errorf("sync: deleting local_state rows: %w", err)
	}

	for i := range rows {
		row := rows[i]
		if _, err := tx.ExecContext(ctx, sqlInsertLocalState,
			row.Path,
			row.ItemType,
			nullString(row.Hash),
			nullKnownInt64(row.Size, true),
			nullOptionalInt64(row.Mtime),
			nullString(row.ContentIdentity),
			row.ObservedAt,
		); err != nil {
			return fmt.Errorf("sync: inserting local_state row for %s: %w", row.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing local_state transaction: %w", err)
	}

	return nil
}

// ListLocalState returns the durable local snapshot rows in path order.
func (m *SyncStore) ListLocalState(ctx context.Context) ([]LocalStateRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListLocalState)
	if err != nil {
		return nil, fmt.Errorf("sync: querying local_state: %w", err)
	}
	defer rows.Close()

	var result []LocalStateRow
	for rows.Next() {
		var row LocalStateRow
		if err := rows.Scan(
			&row.Path,
			&row.ItemType,
			&row.Hash,
			&row.Size,
			&row.Mtime,
			&row.ContentIdentity,
			&row.ObservedAt,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning local_state row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating local_state rows: %w", err)
	}

	return result, nil
}

func buildLocalStateRows(result ScanResult, observedAt int64) []LocalStateRow {
	rows := make([]LocalStateRow, len(result.Rows))
	copy(rows, result.Rows)
	for i := range rows {
		rows[i].ObservedAt = observedAt
	}

	return rows
}
