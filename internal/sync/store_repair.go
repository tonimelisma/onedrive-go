// Package sync persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - ResetFailure:             reset single failed path
//   - ResetAllFailures:         reset all failed rows
//   - WriteSyncMetadata:        persist sync report after RunOnce
//   - ReadSyncMetadata:         retrieve all sync metadata key-value pairs
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

// DropLegacyRemoteBlockedScope removes old persisted perm:remote authority
// rows while leaving held failures intact. New code derives the runtime scope
// entirely from the held rows.
func (m *SyncStore) DropLegacyRemoteBlockedScope(
	ctx context.Context,
	scopeKey ScopeKey,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin remote legacy cleanup tx for %s: %w", scopeKey.String(), err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback remote legacy cleanup tx for %s", scopeKey.String()))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`, scopeKey.String(),
	); execErr != nil {
		return fmt.Errorf("sync: deleting legacy remote scope block %s: %w", scopeKey.String(), execErr)
	}

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ? AND failure_role = ?`,
		scopeKey.String(), FailureRoleBoundary,
	); execErr != nil {
		return fmt.Errorf("sync: deleting legacy remote scope boundary %s: %w", scopeKey.String(), execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing remote legacy cleanup for %s: %w", scopeKey.String(), err)
	}

	return nil
}

// WriteSyncMetadata persists sync metadata after a completed RunOnce pass.
// Keys: last_sync_time, last_sync_duration_ms, last_sync_error,
// last_sync_succeeded, last_sync_failed.
func (m *SyncStore) WriteSyncMetadata(ctx context.Context, report *SyncMetadata) (err error) {
	now := m.nowFunc().UTC().Format(time.RFC3339)
	durationMS := fmt.Sprintf("%d", report.Duration.Milliseconds())
	succeeded := fmt.Sprintf("%d", report.Succeeded)
	failed := fmt.Sprintf("%d", report.Failed)

	syncErr := ""
	if len(report.Errors) > 0 {
		syncErr = report.Errors[0].Error()
	}

	pairs := [][2]string{
		{"last_sync_time", now},
		{"last_sync_duration_ms", durationMS},
		{"last_sync_error", syncErr},
		{"last_sync_succeeded", succeeded},
		{"last_sync_failed", failed},
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync metadata begin tx: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync metadata rollback")
	}()

	const upsertSQL = `INSERT INTO sync_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`

	for _, kv := range pairs {
		if _, execErr := tx.ExecContext(ctx, upsertSQL, kv[0], kv[1]); execErr != nil {
			return fmt.Errorf("sync metadata upsert %s: %w", kv[0], execErr)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit sync metadata reset: %w", err)
	}

	return nil
}

// ReadSyncMetadata retrieves all sync metadata key-value pairs.
// Returns an empty map if the table doesn't exist or has no rows.
func (m *SyncStore) ReadSyncMetadata(ctx context.Context) (map[string]string, error) {
	result := make(map[string]string)

	rows, err := m.db.QueryContext(ctx, `SELECT key, value FROM sync_metadata`)
	if err != nil {
		exists, existsErr := m.syncMetadataTableExists(ctx)
		if existsErr != nil {
			return nil, existsErr
		}

		if !exists {
			return result, nil
		}

		return nil, fmt.Errorf("sync metadata query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("sync metadata scan: %w", err)
		}

		result[k] = v
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sync metadata rows: %w", err)
	}

	return result, nil
}

func (m *SyncStore) syncMetadataTableExists(ctx context.Context) (bool, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='sync_metadata')`)

	var exists int
	if err := row.Scan(&exists); err != nil {
		return false, fmt.Errorf("sync metadata schema check: %w", err)
	}

	return exists == 1, nil
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
// shortcut is removed. Discarding differs from release: held descendants are
// deleted instead of made retryable.
func (m *SyncStore) DiscardScope(ctx context.Context, scopeKey ScopeKey) (err error) {
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
