// Package sync persists sync baseline, observation, failure, scope-block, and
// run-status state.
//
// Contents:
//   - ResetFailure:             reset single failed path
//   - ResetAllFailures:         reset all failed rows
//   - WriteSyncRunStatus:       persist one-shot status after RunOnce
//   - ReadSyncRunStatus:        retrieve the typed one-shot status row
//   - ReleaseScope:             atomic scope release + failure unblock
//   - DiscardScope:             atomic scope discard + failure delete
//   - BaselineEntryCount:       count of entries in baseline table
//
// Related files:
//   - store.go:          SyncStore type definition and lifecycle
//   - store_read_failures.go:  failure query helpers
//   - store_write_failures.go: failure recording and mutation helpers
package sync

import (
	"context"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// StateAdmin methods
// ---------------------------------------------------------------------------

// ResetFailure resets a single failed path by removing its retry record.
func (m *SyncStore) ResetFailure(ctx context.Context, path string) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin reset-failure tx for %s: %w", path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback reset-failure tx for %s", path))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ? AND failure_role != ?`, path, FailureRoleHeld,
	); execErr != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing reset-failure for %s: %w", path, err)
	}

	return nil
}

// ResetAllFailures clears all transient retry rows.
func (m *SyncStore) ResetAllFailures(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE category = 'transient' AND failure_role = 'item'`)
	if err != nil {
		return fmt.Errorf("sync: clearing transient sync failures: %w", err)
	}

	return nil
}

// DeleteRemotePermissionScopeAuthorities removes invalid persisted
// `perm:remote:*` scope rows. Remote permission scopes are rebuilt from held
// failures at startup and should not survive in `scope_blocks` or boundary
// rows on their own.
func (m *SyncStore) DeleteRemotePermissionScopeAuthorities(
	ctx context.Context,
	scopeKey ScopeKey,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin remote permission scope cleanup tx for %s: %w", scopeKey.String(), err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback remote permission scope cleanup tx for %s", scopeKey.String()))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`, scopeKey.String(),
	); execErr != nil {
		return fmt.Errorf("sync: deleting remote permission scope block %s: %w", scopeKey.String(), execErr)
	}

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ? AND failure_role = ?`,
		scopeKey.String(), FailureRoleBoundary,
	); execErr != nil {
		return fmt.Errorf("sync: deleting remote permission scope boundary %s: %w", scopeKey.String(), execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing remote permission scope cleanup for %s: %w", scopeKey.String(), err)
	}

	return nil
}

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

// ReleaseScope atomically applies the semantic "this scope condition has
// resolved; blocked work may run again" transition.
//
// In one transaction it:
//   - deletes the persisted scope_blocks row
//   - deletes boundary issue rows for the scope
//   - marks held transient descendants retryable immediately
//
// The actionable boundary row and the scope block are one semantic unit.
// Releasing them together prevents the split-brain state where one survives
// after the other has already been cleared.
func (m *SyncStore) ReleaseScope(
	ctx context.Context,
	scopeKey ScopeKey,
	now time.Time,
) (err error) {
	wire := scopeKey.String()
	nowNano := now.UnixNano()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin release-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback release-scope tx for %s", wire))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`, wire,
	); execErr != nil {
		return fmt.Errorf("sync: deleting scope block %s: %w", wire, execErr)
	}

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM sync_failures
			WHERE scope_key = ? AND failure_role = ?`,
		wire, FailureRoleBoundary,
	); execErr != nil {
		return fmt.Errorf("sync: deleting scope issue rows %s: %w", wire, execErr)
	}

	if _, execErr := tx.ExecContext(ctx,
		`UPDATE sync_failures
			SET failure_role = ?, next_retry_at = ?
			WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL AND category = ?`,
		FailureRoleItem, nowNano, wire, FailureRoleHeld, CategoryTransient,
	); execErr != nil {
		return fmt.Errorf("sync: unblocking failures for scope %s: %w", wire, execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing release-scope for %s: %w", wire, err)
	}

	return nil
}

// DiscardScope atomically applies the semantic "this scope and the work under
// it are no longer valid" transition.
//
// This is used when the blocked subtree itself disappears, for example when a
// configured root disappears. Discarding differs from release: held descendants are
// deleted instead of made retryable.
func (m *SyncStore) DiscardScope(ctx context.Context, scopeKey ScopeKey) (err error) {
	if scopeKey.IsZero() {
		return fmt.Errorf("sync: discard scope: missing scope key")
	}

	wire := scopeKey.String()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin discard-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback discard-scope tx for %s", wire))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`, wire,
	); execErr != nil {
		return fmt.Errorf("sync: deleting scope block %s: %w", wire, execErr)
	}

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ?`, wire,
	); execErr != nil {
		return fmt.Errorf("sync: deleting scoped failures %s: %w", wire, execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing discard-scope for %s: %w", wire, err)
	}

	return nil
}

// BaselineEntryCount returns the number of entries in the baseline table.
func (m *SyncStore) BaselineEntryCount(ctx context.Context) (int, error) {
	var count int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline`).Scan(&count); err != nil {
		return 0, fmt.Errorf("baseline entry count: %w", err)
	}

	return count, nil
}
