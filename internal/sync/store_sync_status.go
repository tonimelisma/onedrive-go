package sync

import (
	"context"
	"fmt"
)

const sqlEnsureSyncStatusRow = `INSERT INTO sync_status
	(singleton_id, last_synced_at, last_sync_duration_ms, last_succeeded_count, last_failed_count, last_error)
	VALUES (1, 0, 0, 0, 0, '')
	ON CONFLICT(singleton_id) DO NOTHING`

// WriteSyncStatus persists the product-facing sync status row after a
// successful best-effort bidirectional batch.
func (m *SyncStore) WriteSyncStatus(ctx context.Context, status *SyncStatus) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin sync-status tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback sync-status tx")
	}()

	if _, execErr := tx.ExecContext(ctx, sqlEnsureSyncStatusRow); execErr != nil {
		return fmt.Errorf("sync: ensuring sync_status row: %w", execErr)
	}

	if _, execErr := tx.ExecContext(ctx, `
		UPDATE sync_status
		SET last_synced_at = ?,
			last_sync_duration_ms = ?,
			last_succeeded_count = ?,
			last_failed_count = ?,
			last_error = ?
		WHERE singleton_id = 1`,
		status.LastSyncedAt,
		status.LastSyncDurationMs,
		status.LastSucceededCount,
		status.LastFailedCount,
		status.LastError,
	); execErr != nil {
		return fmt.Errorf("sync: updating sync_status: %w", execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit sync-status tx: %w", err)
	}

	return nil
}

// ReadSyncStatus retrieves the typed sync status row.
func (m *SyncStore) ReadSyncStatus(ctx context.Context) (*SyncStatus, error) {
	if _, err := m.db.ExecContext(ctx, sqlEnsureSyncStatusRow); err != nil {
		return nil, fmt.Errorf("sync: ensuring sync_status row: %w", err)
	}

	status := &SyncStatus{}
	if err := m.db.QueryRowContext(ctx, `
		SELECT last_synced_at, last_sync_duration_ms, last_succeeded_count, last_failed_count, last_error
		FROM sync_status
		WHERE singleton_id = 1`,
	).Scan(
		&status.LastSyncedAt,
		&status.LastSyncDurationMs,
		&status.LastSucceededCount,
		&status.LastFailedCount,
		&status.LastError,
	); err != nil {
		return nil, fmt.Errorf("sync: reading sync_status: %w", err)
	}

	return status, nil
}
