package syncstore

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// HeldDeleteRow is one delete the delete-safety hold keeps for confirmation,
// as status reads it. Side is "local" or "remote": where the delete would
// remove content.
type HeldDeleteRow struct {
	Path     string
	Side     string
	Reason   string
	ItemType syncdomain.ItemType
	ItemID   string
}

// ReplaceHeldDeletes records the current plan's held deletes, replacing the
// previous plan's. The hold is re-derived from current truth on every plan;
// the table is what `status` shows and the exact set a later
// `sync --confirm-deletes` approves. An empty list (the hold released,
// confirmed, or no longer warranted) clears it.
func (s *SyncStore) ReplaceHeldDeletes(ctx context.Context, held []syncdomain.HeldDelete) (err error) {
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
			`INSERT OR REPLACE INTO held_deletes (path, side, reason, item_type, item_id) VALUES (?, ?, ?, ?, ?)`,
			held[i].Path, string(held[i].Side), string(held[i].Reason), held[i].ItemType.String(), held[i].ItemID,
		); err != nil {
			return fmt.Errorf("sync: recording held delete %s: %w", held[i].Path, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit held deletes transaction: %w", err)
	}

	return nil
}

func QueryHeldDeleteRowsWithRunner(ctx context.Context, runner sqlTxRunner) ([]HeldDeleteRow, error) {
	rows, err := runner.QueryContext(ctx,
		`SELECT path, side, reason, item_type, item_id FROM held_deletes ORDER BY side, path`)
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
		if err := rows.Scan(&row.Path, &row.Side, &row.Reason, &itemType, &row.ItemID); err != nil {
			return nil, fmt.Errorf("scan held delete: %w", err)
		}
		parsed, parseErr := syncdomain.ParseItemType(itemType)
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

// ListHeldDeletes returns the recorded held set, the deletes a
// `sync --confirm-deletes` pass may release.
func (s *SyncStore) ListHeldDeletes(ctx context.Context) ([]syncdomain.HeldDelete, error) {
	rows, err := QueryHeldDeleteRowsWithRunner(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return HeldDeletesFromRows(rows), nil
}

// HeldDeletesFromRows is the held set the rows record, as the engine reads it:
// a status snapshot's rows name the same set, so the CLI derives the same
// confirmation from them (syncdomain.ConfirmationOf) as the watch it sends
// that confirmation to.
func HeldDeletesFromRows(rows []HeldDeleteRow) []syncdomain.HeldDelete {
	held := make([]syncdomain.HeldDelete, 0, len(rows))
	for i := range rows {
		held = append(held, syncdomain.HeldDelete{
			Path:     rows[i].Path,
			Side:     syncdomain.DeleteSafetySide(rows[i].Side),
			Reason:   syncdomain.DeleteSafetyReason(rows[i].Reason),
			ItemType: rows[i].ItemType,
			ItemID:   rows[i].ItemID,
		})
	}
	return held
}
