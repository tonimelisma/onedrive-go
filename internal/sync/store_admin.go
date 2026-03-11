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
//   - BaselineEntryCount:        count of entries in baseline table
//
// Related files:
//   - store.go:          SyncStore type definition and lifecycle
//   - store_failures.go: failure recording and queries
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ---------------------------------------------------------------------------
// StateReader methods
// ---------------------------------------------------------------------------

// ListUnreconciled returns remote_state rows that need action (not synced,
// filtered, or deleted).
func (m *SyncStore) ListUnreconciled(ctx context.Context) ([]RemoteStateRow, error) {
	return m.queryRemoteStateRows(ctx,
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at
		FROM remote_state WHERE sync_status NOT IN (?, ?, ?)`,
		statusSynced, statusFiltered, statusDeleted,
	)
}

// ListActionableRemoteState returns remote_state rows with pending or failed status
// that don't have pending sync_failures (used for initial dispatch, not retry scheduling).
func (m *SyncStore) ListActionableRemoteState(ctx context.Context) ([]RemoteStateRow, error) {
	return m.queryRemoteStateRows(ctx,
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, sync_status, observed_at
		FROM remote_state
		WHERE sync_status IN (?, ?, ?, ?)`,
		statusPendingDownload, statusDownloadFailed, statusPendingDelete, statusDeleteFailed,
	)
}

// queryRemoteStateRows is a shared helper for scanning multiple remote_state rows.
func (m *SyncStore) queryRemoteStateRows(ctx context.Context, query string, args ...any) ([]RemoteStateRow, error) {
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sync: querying remote_state: %w", err)
	}
	defer rows.Close()

	var result []RemoteStateRow

	for rows.Next() {
		var (
			row      RemoteStateRow
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
// pending and removes the sync_failures row.
func (m *SyncStore) ResetFailure(ctx context.Context, path string) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET
			sync_status = ?
		WHERE path = ? AND sync_status IN (?, ?)`,
		statusPendingDownload,
		path, statusDownloadFailed, statusDeleteFailed,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting failure for %s: %w", path, err)
	}

	// Also remove from sync_failures.
	_, err = m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}

	return nil
}

// ResetAllFailures resets all failed rows: transitions remote_state back to
// pending and clears all transient sync_failures.
func (m *SyncStore) ResetAllFailures(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		statusPendingDownload, statusDownloadFailed,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting download failures: %w", err)
	}

	_, err = m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		statusPendingDelete, statusDeleteFailed,
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
func (m *SyncStore) ResetInProgressStates(ctx context.Context, syncRoot string) error {
	// downloading → pending_download (unconditional, same as before).
	_, err := m.db.ExecContext(ctx,
		`UPDATE remote_state SET sync_status = ? WHERE sync_status = ?`,
		statusPendingDownload, statusDownloading,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting downloading states: %w", err)
	}

	// deleting → check filesystem to determine correct target state.
	rows, err := m.db.QueryContext(ctx,
		`SELECT drive_id, item_id, path FROM remote_state WHERE sync_status = ?`,
		statusDeleting,
	)
	if err != nil {
		return fmt.Errorf("sync: querying deleting states: %w", err)
	}
	defer rows.Close()

	type deletingRow struct {
		driveID, itemID, path string
	}

	var deletingRows []deletingRow

	for rows.Next() {
		var r deletingRow
		if scanErr := rows.Scan(&r.driveID, &r.itemID, &r.path); scanErr != nil {
			return fmt.Errorf("sync: scanning deleting row: %w", scanErr)
		}

		deletingRows = append(deletingRows, r)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("sync: iterating deleting rows: %w", err)
	}

	for _, r := range deletingRows {
		fullPath := filepath.Join(syncRoot, r.path)

		var newStatus string
		if _, statErr := os.Stat(fullPath); statErr != nil {
			// File absent (deleted successfully before crash).
			newStatus = statusDeleted
		} else {
			// File still exists (delete didn't complete).
			newStatus = statusPendingDelete
		}

		if _, execErr := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ? WHERE drive_id = ? AND item_id = ?`,
			newStatus, r.driveID, r.itemID,
		); execErr != nil {
			return fmt.Errorf("sync: resetting deleting state for %s: %w", r.path, execErr)
		}
	}

	return nil
}

// SetDispatchStatus transitions a remote_state row from pending/failed to
// in-progress before the action is dispatched to the worker pool. Uses
// optimistic concurrency: only updates if the current status is valid for
// the given action type.
func (m *SyncStore) SetDispatchStatus(ctx context.Context, driveID, itemID string, actionType ActionType) error {
	switch actionType { //nolint:exhaustive // only download and delete dispatches touch remote_state
	case ActionDownload:
		_, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status IN (?, ?)`,
			statusDownloading,
			driveID, itemID, statusPendingDownload, statusDownloadFailed,
		)
		if err != nil {
			return fmt.Errorf("sync: setting dispatch status downloading for %s: %w", itemID, err)
		}

	case ActionLocalDelete:
		_, err := m.db.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status IN (?, ?)`,
			statusDeleting,
			driveID, itemID, statusPendingDelete, statusDeleteFailed,
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
func (m *SyncStore) WriteSyncMetadata(ctx context.Context, report *SyncReport) error {
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

// BaselineEntryCount returns the number of entries in the baseline table.
func (m *SyncStore) BaselineEntryCount(ctx context.Context) (int, error) {
	var count int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM baseline`).Scan(&count); err != nil {
		return 0, fmt.Errorf("baseline entry count: %w", err)
	}

	return count, nil
}
