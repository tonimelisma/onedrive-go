package syncstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// UpsertHeldDeletes records delete actions held by big-delete protection.
// Existing approved rows are never downgraded back to held.
func (m *SyncStore) UpsertHeldDeletes(ctx context.Context, deletes []synctypes.HeldDeleteRecord) (retErr error) {
	if len(deletes) == 0 {
		return nil
	}

	now := m.nowFunc().UnixNano()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin held-delete upsert tx: %w", err)
	}
	defer func() {
		retErr = finalizeTxRollback(retErr, tx, "sync: rollback held-delete upsert tx")
	}()

	for i := range deletes {
		record := deletes[i]
		if record.HeldAt == 0 {
			record.HeldAt = now
		}
		if record.LastPlannedAt == 0 {
			record.LastPlannedAt = now
		}
		if record.State == "" {
			record.State = synctypes.HeldDeleteStateHeld
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO held_deletes
			(drive_id, action_type, path, item_id, state, held_at, approved_at, last_planned_at, last_error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(drive_id, action_type, path) DO UPDATE SET
				item_id = excluded.item_id,
				state = CASE
					WHEN held_deletes.state = 'approved' THEN held_deletes.state
					ELSE excluded.state
				END,
				approved_at = CASE
					WHEN held_deletes.state = 'approved' THEN held_deletes.approved_at
					ELSE excluded.approved_at
				END,
				last_planned_at = excluded.last_planned_at,
				last_error = excluded.last_error`,
			record.DriveID.String(),
			record.ActionType,
			record.Path,
			record.ItemID,
			record.State,
			record.HeldAt,
			nullOptionalInt64(record.ApprovedAt),
			record.LastPlannedAt,
			nullString(record.LastError),
		)
		if err != nil {
			return fmt.Errorf("sync: upsert held delete %s: %w", record.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit held-delete upsert tx: %w", err)
	}

	return nil
}

// ApproveHeldDeletes marks all currently held deletes approved. Approved rows
// remain durable until the engine consumes them.
func (m *SyncStore) ApproveHeldDeletes(ctx context.Context) error {
	now := m.nowFunc().UnixNano()
	_, err := m.db.ExecContext(ctx,
		`UPDATE held_deletes
		SET state = ?, approved_at = ?
		WHERE state = ?`,
		synctypes.HeldDeleteStateApproved,
		now,
		synctypes.HeldDeleteStateHeld,
	)
	if err != nil {
		return fmt.Errorf("sync: approving held deletes: %w", err)
	}

	return nil
}

func (m *SyncStore) ListHeldDeletesByState(
	ctx context.Context,
	state string,
) ([]synctypes.HeldDeleteRecord, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT drive_id, action_type, path, item_id, state, held_at, approved_at,
			last_planned_at, last_error
		FROM held_deletes
		WHERE state = ?
		ORDER BY last_planned_at, path`,
		state,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: listing held deletes in state %s: %w", state, err)
	}
	defer rows.Close()

	return scanHeldDeleteRows(rows)
}

func (m *SyncStore) ConsumeHeldDelete(
	ctx context.Context,
	driveID driveid.ID,
	actionType synctypes.ActionType,
	path string,
) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM held_deletes
		WHERE drive_id = ? AND action_type = ? AND path = ? AND state = ?`,
		driveID.String(),
		actionType,
		path,
		synctypes.HeldDeleteStateApproved,
	)
	if err != nil {
		return fmt.Errorf("sync: consuming held delete %s: %w", path, err)
	}

	return nil
}

func (m *SyncStore) DeleteHeldDelete(
	ctx context.Context,
	driveID driveid.ID,
	actionType synctypes.ActionType,
	path string,
) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM held_deletes
		WHERE drive_id = ? AND action_type = ? AND path = ?`,
		driveID.String(),
		actionType,
		path,
	)
	if err != nil {
		return fmt.Errorf("sync: deleting held delete %s: %w", path, err)
	}

	return nil
}

func scanHeldDeleteRows(rows *sql.Rows) ([]synctypes.HeldDeleteRecord, error) {
	var records []synctypes.HeldDeleteRecord
	for rows.Next() {
		var (
			record     synctypes.HeldDeleteRecord
			approvedAt sql.NullInt64
			lastError  sql.NullString
		)
		if err := rows.Scan(
			&record.DriveID,
			&record.ActionType,
			&record.Path,
			&record.ItemID,
			&record.State,
			&record.HeldAt,
			&approvedAt,
			&record.LastPlannedAt,
			&lastError,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning held delete row: %w", err)
		}
		if approvedAt.Valid {
			record.ApprovedAt = approvedAt.Int64
		}
		record.LastError = lastError.String
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating held delete rows: %w", err)
	}

	return records, nil
}
