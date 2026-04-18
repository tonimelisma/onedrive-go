package sync

import (
	"context"
	"fmt"
)

const sqlEnsureRunStatusRow = `INSERT INTO run_status
	(singleton_id, last_completed_at, last_duration_ms, last_succeeded_count, last_failed_count, last_error)
	VALUES (1, 0, 0, 0, 0, '')
	ON CONFLICT(singleton_id) DO NOTHING`

// WriteSyncRunStatus persists the typed one-shot status row after a completed
// RunOnce pass.
func (m *SyncStore) WriteSyncRunStatus(ctx context.Context, report *SyncRunReport) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin run-status tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback run-status tx")
	}()

	if _, execErr := tx.ExecContext(ctx, sqlEnsureRunStatusRow); execErr != nil {
		return fmt.Errorf("sync: ensuring run_status row: %w", execErr)
	}

	lastError := ""
	if len(report.Errors) > 0 {
		lastError = report.Errors[0].Error()
	}

	completedAt := report.CompletedAt
	if completedAt.IsZero() {
		completedAt = m.nowFunc()
	}

	if _, execErr := tx.ExecContext(ctx, `
		UPDATE run_status
		SET last_completed_at = ?,
			last_duration_ms = ?,
			last_succeeded_count = ?,
			last_failed_count = ?,
			last_error = ?
		WHERE singleton_id = 1`,
		completedAt.UnixNano(),
		report.Duration.Milliseconds(),
		report.Succeeded,
		report.Failed,
		lastError,
	); execErr != nil {
		return fmt.Errorf("sync: updating run_status: %w", execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit run-status tx: %w", err)
	}

	return nil
}

// ReadSyncRunStatus retrieves the typed one-shot status row.
func (m *SyncStore) ReadSyncRunStatus(ctx context.Context) (*SyncRunStatus, error) {
	if _, err := m.db.ExecContext(ctx, sqlEnsureRunStatusRow); err != nil {
		return nil, fmt.Errorf("sync: ensuring run_status row: %w", err)
	}

	status := &SyncRunStatus{}
	if err := m.db.QueryRowContext(ctx, `
		SELECT last_completed_at, last_duration_ms, last_succeeded_count, last_failed_count, last_error
		FROM run_status
		WHERE singleton_id = 1`,
	).Scan(
		&status.LastCompletedAt,
		&status.LastDurationMs,
		&status.LastSucceededCount,
		&status.LastFailedCount,
		&status.LastError,
	); err != nil {
		return nil, fmt.Errorf("sync: reading run_status: %w", err)
	}

	return status, nil
}
