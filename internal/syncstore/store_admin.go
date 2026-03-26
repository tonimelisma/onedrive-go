// store_admin.go — State reader and admin operations for SyncStore.
//
// Contents:
//   - ListUnreconciled:          remote_state rows needing action
//   - ListActionableRemoteState: pending/failed remote_state for initial dispatch
//   - queryRemoteStateRows:      shared multi-row remote_state scanner
//   - ResetFailure:              reset single failed path to pending
//   - ResetAllFailures:          reset all failures to pending
//   - ResetInProgressStates:     crash recovery for downloading/deleting states
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
	"os"
	"path/filepath"
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
		`DELETE FROM sync_failures WHERE path = ?`, path,
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
		`DELETE FROM sync_failures WHERE category = 'transient'`)
	if err != nil {
		return fmt.Errorf("sync: clearing transient sync failures: %w", err)
	}

	return nil
}

// ResetInProgressStates is crash recovery: downloading→pending_download.
// For deleting rows, checks the filesystem: file absent → deleted (complete
// the delete), file exists → pending_delete (re-attempt). Called at engine
// startup with the sync root path.
//
// After resetting remote_state, creates corresponding sync_failures entries
// for items that transitioned to pending states. This bridges remote_state
// to the sole retry mechanism (drain-loop retrier + sync_failures): without
// these entries, items stuck mid-execution during a crash would become
// zombies — the delta token was already advanced, so no new events arrive,
// and the retrier only queries sync_failures.
//
// delayFn computes backoff from failure count (engine passes retry.Reconcile.Delay).
func (m *SyncStore) ResetInProgressStates(ctx context.Context, syncRoot string, delayFn func(int) time.Duration) error {
	// Phase 1: Collect items that were downloading before reset.
	// We need to query BEFORE the status update to identify which items
	// were actually in-progress (not already pending_download).
	downloadingRows, err := m.queryResetCandidates(ctx, synctypes.SyncStatusDownloading)
	if err != nil {
		return fmt.Errorf("sync: querying downloading candidates: %w", err)
	}

	// downloading → pending_download (unconditional).
	_, err = m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloading,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting downloading states: %w", err)
	}

	// deleting → check filesystem to determine correct target state.
	deletingRows, err := m.queryResetCandidates(ctx, synctypes.SyncStatusDeleting)
	if err != nil {
		return fmt.Errorf("sync: querying deleting candidates: %w", err)
	}

	// Track which deleting items transition to pending_delete (need sync_failures).
	var pendingDeleteRows []resetCandidate

	for _, r := range deletingRows {
		fullPath := filepath.Join(syncRoot, r.path)

		var newStatus synctypes.SyncStatus
		if _, statErr := os.Stat(fullPath); statErr != nil {
			// File absent (deleted successfully before crash).
			newStatus = synctypes.SyncStatusDeleted
		} else {
			// File still exists (delete didn't complete).
			newStatus = synctypes.SyncStatusPendingDelete
			pendingDeleteRows = append(pendingDeleteRows, r)
		}

		if _, execErr := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ? WHERE drive_id = ? AND item_id = ?`,
			newStatus, r.driveID, r.itemID,
		); execErr != nil {
			return fmt.Errorf("sync: resetting deleting state for %s: %w", r.path, execErr)
		}
	}

	// Phase 2: Create sync_failures entries for reset items so the
	// retrier can rediscover them. RecordFailure uses UPSERT —
	// if a sync_failures entry already exists (from a prior failure before
	// the crash), the existing failure_count is preserved and incremented,
	// so backoff continues from where it left off.
	if err := m.createCrashRecoveryFailures(ctx, downloadingRows, synctypes.DirectionDownload, delayFn); err != nil {
		return err
	}

	if err := m.createCrashRecoveryFailures(ctx, pendingDeleteRows, synctypes.DirectionDelete, delayFn); err != nil {
		return err
	}

	return nil
}

// resetCandidate holds the identity of a remote_state row that was reset
// during crash recovery and needs a corresponding sync_failures entry.
type resetCandidate struct {
	driveID, itemID, path string
}

// queryResetCandidates returns identity info for remote_state rows matching
// a given status. Used to capture row data before the bulk status update.
func (m *SyncStore) queryResetCandidates(ctx context.Context, status synctypes.SyncStatus) ([]resetCandidate, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT drive_id, item_id, path FROM remote_state WHERE sync_status = ?`,
		status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []resetCandidate

	for rows.Next() {
		var r resetCandidate
		if scanErr := rows.Scan(&r.driveID, &r.itemID, &r.path); scanErr != nil {
			return nil, scanErr
		}

		result = append(result, r)
	}

	return result, rows.Err()
}

// createCrashRecoveryFailures creates sync_failures entries for items that
// were reset during crash recovery. This ensures the retrier can rediscover
// them on the next bootstrap sweep.
func (m *SyncStore) createCrashRecoveryFailures(
	ctx context.Context, candidates []resetCandidate, direction synctypes.Direction, delayFn func(int) time.Duration,
) error {
	for _, r := range candidates {
		if err := m.RecordFailure(ctx, &synctypes.SyncFailureParams{
			Path:      r.path,
			DriveID:   driveid.New(r.driveID),
			Direction: direction,
			Category:  synctypes.CategoryTransient,
			ItemID:    r.itemID,
			ErrMsg:    "crash recovery: reset from in-progress state",
		}, delayFn); err != nil {
			return fmt.Errorf("sync: creating crash recovery failure for %s: %w", r.path, err)
		}
	}

	return nil
}

// SetDispatchStatus transitions a remote_state row from pending/failed to
// in-progress before the action is dispatched to the worker pool. Uses
// optimistic concurrency: only updates if the current status is valid for
// the given action type.
func (m *SyncStore) SetDispatchStatus(ctx context.Context, driveID, itemID string, actionType synctypes.ActionType) error {
	switch actionType { //nolint:exhaustive // only download and delete dispatches touch remote_state
	case synctypes.ActionDownload:
		_, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status IN (?, ?)`,
			synctypes.SyncStatusDownloading,
			driveID, itemID, synctypes.SyncStatusPendingDownload, synctypes.SyncStatusDownloadFailed,
		)
		if err != nil {
			return fmt.Errorf("sync: setting dispatch status downloading for %s: %w", itemID, err)
		}

	case synctypes.ActionLocalDelete:
		_, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status IN (?, ?)`,
			synctypes.SyncStatusDeleting,
			driveID, itemID, synctypes.SyncStatusPendingDelete, synctypes.SyncStatusDeleteFailed,
		)
		if err != nil {
			return fmt.Errorf("sync: setting dispatch status deleting for %s: %w", itemID, err)
		}
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

	return tx.Commit()
}

// ReadSyncMetadata retrieves all sync metadata key-value pairs.
// Returns an empty map if the table doesn't exist or has no rows.
func (m *SyncStore) ReadSyncMetadata(ctx context.Context) (map[string]string, error) {
	result := make(map[string]string)

	rows, err := m.db.QueryContext(ctx, `SELECT key, value FROM sync_metadata`)
	if err != nil {
		// Table might not exist in pre-migration DBs — return empty map.
		return result, nil //nolint:nilerr // graceful fallback for old DBs
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("sync metadata scan: %w", err)
		}

		result[k] = v
	}

	return result, rows.Err()
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
		WHERE scope_key = ? AND issue_type IS NOT NULL`,
		wire,
	); err != nil {
		return fmt.Errorf("sync: deleting scope issue rows %s: %w", wire, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sync_failures
		SET next_retry_at = ?
		WHERE scope_key = ? AND next_retry_at IS NULL AND category = ?`,
		nowNano, wire, synctypes.CategoryTransient,
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
