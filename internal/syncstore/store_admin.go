// Package syncstore persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - ListUnreconciled:          remote_state rows needing action
//   - ListActionableRemoteState: pending/failed remote_state for initial dispatch
//   - queryRemoteStateRows:      shared multi-row remote_state scanner
//   - ResetFailure:              reset single failed path to pending
//   - ResetAllFailures:          reset all failures to pending
//   - ResetDownloadingStates:    crash recovery for downloading states
//   - ListDeletingCandidates:    crash recovery candidates for deleting states
//   - FinalizeDeletingStates:    apply delete crash-recovery decisions
//   - SetDispatchStatus:         transition pending→in-progress before dispatch
//   - WriteSyncMetadata:         persist sync report after RunOnce
//   - ReadSyncMetadata:          retrieve all sync metadata key-value pairs
//   - ReleaseScope:               atomic scope release + failure unblock
//   - DiscardScope:               atomic scope discard + failure delete
//   - BaselineEntryCount:        count of entries in baseline table
//
// Related files:
//   - store.go:          SyncStore type definition and lifecycle
//   - store_failures.go: failure recording and queries
package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// StateReader methods
// ---------------------------------------------------------------------------

// ListUnreconciled returns remote_state rows that need action (not synced,
// filtered, or deleted).
func (m *SyncStore) ListUnreconciled(ctx context.Context) ([]synctypes.RemoteStateRow, error) {
	return m.queryRemoteStateRows(ctx,
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at
		FROM remote_state WHERE sync_status NOT IN (?, ?, ?)`,
		synctypes.SyncStatusSynced, synctypes.SyncStatusFiltered, synctypes.SyncStatusDeleted,
	)
}

// ListActionableRemoteState returns remote_state rows with pending or failed status
// that don't have pending sync_failures (used for initial dispatch, not retry scheduling).
func (m *SyncStore) ListActionableRemoteState(ctx context.Context) ([]synctypes.RemoteStateRow, error) {
	return m.queryRemoteStateRows(ctx,
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at
		FROM remote_state
		WHERE sync_status IN (?, ?, ?, ?)`,
		synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloadFailed,
		synctypes.SyncStatusPendingDelete, synctypes.SyncStatusDeleteFailed,
	)
}

// queryRemoteStateRows is a shared helper for scanning multiple remote_state rows.
func (m *SyncStore) queryRemoteStateRows(ctx context.Context, query string, args ...any) ([]synctypes.RemoteStateRow, error) {
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sync: querying remote_state: %w", err)
	}
	defer rows.Close()

	var result []synctypes.RemoteStateRow

	for rows.Next() {
		var (
			row      synctypes.RemoteStateRow
			parentID sql.NullString
			hash     sql.NullString
			size     sql.NullInt64
			mtime    sql.NullInt64
			etag     sql.NullString
			prevPath sql.NullString
		)

		if err := rows.Scan(
			&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
			&hash, &size, &mtime, &etag,
			&prevPath, &row.SyncStatus, &row.ObservedAt,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning remote_state row: %w", err)
		}

		row.ParentID = parentID.String
		row.Hash = hash.String
		row.ETag = etag.String
		row.PreviousPath = prevPath.String

		if size.Valid {
			row.Size = size.Int64
		}

		if mtime.Valid {
			row.Mtime = mtime.Int64
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating remote_state rows: %w", err)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// StateAdmin methods
// ---------------------------------------------------------------------------

// ResetFailure resets a single failed path: transitions remote_state back to
// pending and removes the sync_failures row. Uses a transaction to ensure
// atomicity — crash between statements cannot leave inconsistent state.
func (m *SyncStore) ResetFailure(ctx context.Context, path string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin reset-failure tx for %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()

	// download_failed → pending_download
	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE path = ? AND sync_status = ?`,
		synctypes.SyncStatusPendingDownload, path, synctypes.SyncStatusDownloadFailed,
	); err != nil {
		return fmt.Errorf("sync: resetting download failure for %s: %w", path, err)
	}

	// delete_failed → pending_delete (not pending_download — the item
	// should be re-attempted as a delete, not a download).
	if _, err := tx.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE path = ? AND sync_status = ?`,
		synctypes.SyncStatusPendingDelete, path, synctypes.SyncStatusDeleteFailed,
	); err != nil {
		return fmt.Errorf("sync: resetting delete failure for %s: %w", path, err)
	}

	// Remove from sync_failures.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ? AND failure_role != ?`, path, synctypes.FailureRoleHeld,
	); err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing reset-failure for %s: %w", path, err)
	}

	return nil
}

// ResetAllFailures resets all failed rows: transitions remote_state back to
// pending and clears all transient sync_failures.
func (m *SyncStore) ResetAllFailures(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloadFailed,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting download failures: %w", err)
	}

	_, err = m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		synctypes.SyncStatusPendingDelete, synctypes.SyncStatusDeleteFailed,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting delete failures: %w", err)
	}

	// Clear all transient failures from sync_failures.
	_, err = m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE category = 'transient' AND failure_role = 'item'`)
	if err != nil {
		return fmt.Errorf("sync: clearing transient sync failures: %w", err)
	}

	return nil
}

// FindRemoteBlockedTarget resolves user input to either one held blocked path
// or a whole derived perm:remote boundary. Boundary matches win over exact-row
// matches so a blocked folder-create at the boundary path still treats the
// scope name as the scope, not as a single held row.
func (m *SyncStore) ApproveHeldDeletes(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE issue_type = ? AND category = ?`,
		synctypes.IssueBigDeleteHeld, synctypes.CategoryActionable,
	)
	if err != nil {
		return fmt.Errorf("sync: approving held deletes: %w", err)
	}

	return nil
}

// DropLegacyRemoteBlockedScope removes old persisted perm:remote authority
// rows while leaving held failures intact. New code derives the runtime scope
// entirely from the held rows.
func (m *SyncStore) DropLegacyRemoteBlockedScope(ctx context.Context, scopeKey synctypes.ScopeKey) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin remote legacy cleanup tx for %s: %w", scopeKey.String(), err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`, scopeKey.String(),
	); err != nil {
		return fmt.Errorf("sync: deleting legacy remote scope block %s: %w", scopeKey.String(), err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ? AND failure_role = ?`,
		scopeKey.String(), synctypes.FailureRoleBoundary,
	); err != nil {
		return fmt.Errorf("sync: deleting legacy remote scope boundary %s: %w", scopeKey.String(), err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing remote legacy cleanup for %s: %w", scopeKey.String(), err)
	}

	return nil
}

// ResetDownloadingStates is the state-only half of crash recovery for
// downloads: downloading → pending_download, plus sync_failures entries so the
// retrier can rediscover the reset items on the next bootstrap sweep.
func (m *SyncStore) ResetDownloadingStates(ctx context.Context, delayFn func(int) time.Duration) error {
	downloadingRows, err := m.queryResetCandidates(ctx, synctypes.SyncStatusDownloading)
	if err != nil {
		return fmt.Errorf("sync: querying downloading candidates: %w", err)
	}

	_, err = m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloading,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting downloading states: %w", err)
	}

	if err := m.createCrashRecoveryFailures(
		ctx,
		downloadingRows,
		synctypes.DirectionDownload,
		synctypes.ActionDownload,
		delayFn,
	); err != nil {
		return err
	}

	return nil
}

// ListDeletingCandidates returns deleting rows that the crash-recovery
// filesystem layer must classify as completed deletes or pending retries.
func (m *SyncStore) ListDeletingCandidates(ctx context.Context) ([]synctypes.RecoveryCandidate, error) {
	candidates, err := m.queryResetCandidates(ctx, synctypes.SyncStatusDeleting)
	if err != nil {
		return nil, fmt.Errorf("sync: querying deleting candidates: %w", err)
	}

	return candidates, nil
}

// FinalizeDeletingStates applies the crash-recovery delete classification
// computed outside the store. `deleted` rows become SyncStatusDeleted;
// `pending` rows become SyncStatusPendingDelete plus transient sync_failures.
func (m *SyncStore) FinalizeDeletingStates(
	ctx context.Context,
	deleted []synctypes.RecoveryCandidate,
	pending []synctypes.RecoveryCandidate,
	delayFn func(int) time.Duration,
) error {
	for _, candidate := range deleted {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ? WHERE drive_id = ? AND item_id = ?`,
			synctypes.SyncStatusDeleted, candidate.DriveID, candidate.ItemID,
		); err != nil {
			return fmt.Errorf("sync: marking deleting state complete for %s: %w", candidate.Path, err)
		}
	}

	for _, candidate := range pending {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ? WHERE drive_id = ? AND item_id = ?`,
			synctypes.SyncStatusPendingDelete, candidate.DriveID, candidate.ItemID,
		); err != nil {
			return fmt.Errorf("sync: resetting deleting state for %s: %w", candidate.Path, err)
		}
	}

	if err := m.createCrashRecoveryFailures(
		ctx,
		pending,
		synctypes.DirectionDelete,
		synctypes.ActionLocalDelete,
		delayFn,
	); err != nil {
		return err
	}

	return nil
}

// queryResetCandidates returns identity info for remote_state rows matching
// a given status. Used to capture row data before the bulk status update.
func (m *SyncStore) queryResetCandidates(ctx context.Context, status synctypes.SyncStatus) ([]synctypes.RecoveryCandidate, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT drive_id, item_id, path FROM remote_state WHERE sync_status = ?`,
		status,
	)
	if err != nil {
		return nil, fmt.Errorf("query reset candidates: %w", err)
	}
	defer rows.Close()

	var result []synctypes.RecoveryCandidate

	for rows.Next() {
		var (
			driveID string
			itemID  string
			path    string
		)
		if scanErr := rows.Scan(&driveID, &itemID, &path); scanErr != nil {
			return nil, fmt.Errorf("scan reset candidate: %w", scanErr)
		}

		result = append(result, synctypes.RecoveryCandidate{
			DriveID: driveID,
			ItemID:  itemID,
			Path:    path,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reset candidates: %w", err)
	}

	return result, nil
}

// createCrashRecoveryFailures creates sync_failures entries for items that
// were reset during crash recovery. This ensures the retrier can rediscover
// them on the next bootstrap sweep.
func (m *SyncStore) createCrashRecoveryFailures(
	ctx context.Context,
	candidates []synctypes.RecoveryCandidate,
	direction synctypes.Direction,
	actionType synctypes.ActionType,
	delayFn func(int) time.Duration,
) error {
	for _, candidate := range candidates {
		if err := m.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:       candidate.Path,
			DriveID:    driveid.New(candidate.DriveID),
			Direction:  direction,
			ActionType: actionType,
			Category:   synctypes.CategoryTransient,
			ItemID:     candidate.ItemID,
			ErrMsg:     "crash recovery: reset from in-progress state",
		}, delayFn); err != nil {
			return fmt.Errorf("sync: creating crash recovery failure for %s: %w", candidate.Path, err)
		}
	}

	return nil
}

// SetDispatchStatus transitions a remote_state row from pending/failed to
// in-progress before the action is dispatched to the worker pool. Uses
// optimistic concurrency: only updates if the current status is valid for
// the given action type.
func (m *SyncStore) SetDispatchStatus(ctx context.Context, driveID, itemID string, actionType synctypes.ActionType) error {
	nextStatus, validStatuses, label, ok := dispatchStatusTransition(actionType)
	if !ok {
		return nil
	}

	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ?
		WHERE drive_id = ? AND item_id = ? AND sync_status IN (?, ?)`,
		nextStatus,
		driveID, itemID, validStatuses[0], validStatuses[1],
	)
	if err != nil {
		return fmt.Errorf("sync: setting dispatch status %s for %s: %w", label, itemID, err)
	}

	return nil
}

// WriteSyncMetadata persists sync metadata after a completed RunOnce pass.
// Keys: last_sync_time, last_sync_duration_ms, last_sync_error,
// last_sync_succeeded, last_sync_failed.
func (m *SyncStore) WriteSyncMetadata(ctx context.Context, report *synctypes.SyncReport) error {
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

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync metadata begin tx: %w", err)
	}
	defer tx.Rollback() // rollback after commit is benign

	const upsertSQL = `INSERT INTO sync_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`

	for _, kv := range pairs {
		if _, err := tx.ExecContext(ctx, upsertSQL, kv[0], kv[1]); err != nil {
			return fmt.Errorf("sync metadata upsert %s: %w", kv[0], err)
		}
	}

	if err := tx.Commit(); err != nil {
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

func dispatchStatusTransition(actionType synctypes.ActionType) (synctypes.SyncStatus, [2]synctypes.SyncStatus, string, bool) {
	if actionType == synctypes.ActionDownload {
		return synctypes.SyncStatusDownloading,
			[2]synctypes.SyncStatus{synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloadFailed},
			"downloading",
			true
	}

	if actionType == synctypes.ActionLocalDelete {
		return synctypes.SyncStatusDeleting,
			[2]synctypes.SyncStatus{synctypes.SyncStatusPendingDelete, synctypes.SyncStatusDeleteFailed},
			"deleting",
			true
	}

	return "", [2]synctypes.SyncStatus{}, "", false
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
func (m *SyncStore) ReleaseScope(ctx context.Context, scopeKey synctypes.ScopeKey, now time.Time) error {
	wire := scopeKey.String()
	nowNano := now.UnixNano()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin release-scope tx for %s: %w", wire, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`, wire,
	); err != nil {
		return fmt.Errorf("sync: deleting scope block %s: %w", wire, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures
		WHERE scope_key = ? AND failure_role = ?`,
		wire, synctypes.FailureRoleBoundary,
	); err != nil {
		return fmt.Errorf("sync: deleting scope issue rows %s: %w", wire, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sync_failures
		SET failure_role = ?, next_retry_at = ?
		WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL AND category = ?`,
		synctypes.FailureRoleItem, nowNano, wire, synctypes.FailureRoleHeld, synctypes.CategoryTransient,
	); err != nil {
		return fmt.Errorf("sync: unblocking failures for scope %s: %w", wire, err)
	}

	if err := tx.Commit(); err != nil {
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
func (m *SyncStore) DiscardScope(ctx context.Context, scopeKey synctypes.ScopeKey) error {
	wire := scopeKey.String()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin discard-scope tx for %s: %w", wire, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM scope_blocks WHERE scope_key = ?`, wire,
	); err != nil {
		return fmt.Errorf("sync: deleting scope block %s: %w", wire, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ?`, wire,
	); err != nil {
		return fmt.Errorf("sync: deleting scoped failures %s: %w", wire, err)
	}

	if err := tx.Commit(); err != nil {
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
