package sync

import (
	"context"
	"fmt"
)

// HeldDeleteRow is one delete the delete-safety hold keeps for confirmation,
// as status reads it. Side is "local" or "remote": where the delete would
// remove content.
type HeldDeleteRow struct {
	Path     string
	Side     string
	Reason   string
	ItemType ItemType
}

// ReplaceHeldDeletes records the current plan's held deletes, replacing the
// previous plan's. The hold is re-derived from current truth on every plan,
// so the table is a read-side projection for status, never planner input:
// an empty list (the hold released, confirmed, or no longer warranted)
// clears it.
func (s *SyncStore) ReplaceHeldDeletes(ctx context.Context, held []heldDelete) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: begin held deletes transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback held deletes transaction")
	}()

	if _, err = tx.ExecContext(ctx, `DELETE FROM held_deletes`); err != nil {
		return fmt.Errorf("sync: clearing held deletes: %w", err)
	}
	for i := range held {
		if _, err = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO held_deletes (path, side, reason, item_type) VALUES (?, ?, ?, ?)`,
			held[i].Path, string(held[i].Side), string(held[i].Reason), held[i].ItemType.String(),
		); err != nil {
			return fmt.Errorf("sync: recording held delete %s: %w", held[i].Path, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit held deletes transaction: %w", err)
	}

	return nil
}

func queryHeldDeleteRowsWithRunner(ctx context.Context, runner sqlTxRunner) ([]HeldDeleteRow, error) {
	rows, err := runner.QueryContext(ctx,
		`SELECT path, side, reason, item_type FROM held_deletes ORDER BY side, path`)
	if err != nil {
		return nil, fmt.Errorf("query held deletes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var result []HeldDeleteRow
	for rows.Next() {
		var (
			row      HeldDeleteRow
			itemType string
		)
		if err := rows.Scan(&row.Path, &row.Side, &row.Reason, &itemType); err != nil {
			return nil, fmt.Errorf("scan held delete: %w", err)
		}
		parsed, parseErr := parseItemType(itemType)
		if parseErr != nil {
			return nil, fmt.Errorf("scan held delete %s: %w", row.Path, parseErr)
		}
		row.ItemType = parsed
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate held deletes: %w", err)
	}

	return result, nil
}
