package sync

import (
	"context"
	"fmt"
	"time"
)

// The delete ledger records every delete sync ran without the user's
// confirmation, by side and commit time, so the proportional delete-safety
// rule can count a slow deletion across passes (R-6.2.5). The planner reads
// the counts inside deleteSafetyWindow; a confirmation clears the side it
// approved, and rows older than the window are pruned on read.

// recordDeleteLedgerTx appends a committed, unconfirmed delete to the ledger
// inside the outcome's own transaction, so a crash leaves the ledger and the
// baseline agreeing.
func (s *SyncStore) recordDeleteLedgerTx(ctx context.Context, tx sqlTxRunner, o *BaselineMutation) error {
	if o.DeleteConfirmed {
		return nil
	}
	if !o.Action.isMember() {
		// classifyMutation refuses this first, inside the same transaction.
		return fmt.Errorf("sync: recording %s in the delete ledger: %s", o.Path, outsideTheEnum(o.Action))
	}
	side, isDelete := deleteSafetySideOf(o.Action)
	if !isDelete {
		return nil
	}
	now := s.nowFunc()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO delete_ledger (side, path, item_id, committed_at) VALUES (?, ?, ?, ?)`,
		string(side), o.Path, o.ItemID, now.UnixNano(),
	); err != nil {
		return fmt.Errorf("sync: recording delete of %s in the delete ledger: %w", o.Path, err)
	}
	// Rows older than the window no longer count; pruning here, on a write
	// the store makes anyway, keeps the planner's read read-only (a dry run
	// plans too).
	if _, err := tx.ExecContext(ctx, `DELETE FROM delete_ledger WHERE committed_at < ?`,
		now.Add(-deleteSafetyWindow).UnixNano()); err != nil {
		return fmt.Errorf("sync: pruning the delete ledger: %w", err)
	}

	return nil
}

// recentUnconfirmedDeletes counts the ledger's deletes toward each side
// committed at or after since. Read-only: the planner calls it on every
// plan, dry runs included.
func (s *SyncStore) recentUnconfirmedDeletes(ctx context.Context, since time.Time) (map[deleteSafetySide]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT side, COUNT(*) FROM delete_ledger WHERE committed_at >= ? GROUP BY side`, since.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("sync: counting recent deletes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	counts := map[deleteSafetySide]int{}
	for rows.Next() {
		var (
			side  string
			count int
		)
		if err := rows.Scan(&side, &count); err != nil {
			return nil, fmt.Errorf("sync: scanning recent deletes: %w", err)
		}
		counts[deleteSafetySide(side)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating recent deletes: %w", err)
	}

	return counts, nil
}

// clearDeleteLedger forgets the ledger's deletes toward the given sides: the
// user has just confirmed a held set there, which reviewed what went before.
func (s *SyncStore) clearDeleteLedger(ctx context.Context, sides []deleteSafetySide) error {
	for _, side := range sides {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM delete_ledger WHERE side = ?`, string(side)); err != nil {
			return fmt.Errorf("sync: clearing the %s delete ledger: %w", side, err)
		}
	}

	return nil
}
