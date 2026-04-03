// Package syncstore persists sync baseline, observation, conflict, failure, and scope state.
//
// sync_failures is the durable per-item failure table.
//
// failure_role makes row meaning explicit:
//   - item: ordinary per-path failure or actionable issue
//   - held: path currently blocked behind an active scope
//   - boundary: actionable row that defines a scope-backed condition
//
// Transient item failures get next_retry_at computed by a caller-provided
// delayFn (typically retry.ReconcilePolicy().Delay). The engine failure retrier
// re-injects due items via buffer -> planner -> executor (R-6.8.10).
//
// Contents:
//   - IsActionableIssue:                classify issue types requiring user action
//   - RecordFailure:                    unified failure recording with remote_state transition
//   - transitionRemoteStateOnFailure:   downloading→download_failed, deleting→delete_failed
//   - ListSyncFailures:                 all failures ordered by last_seen_at
//   - ListActionableFailures:           user-actionable failures only
//   - ListSyncFailuresForRetry:         transient failures ready for retry
//   - ListSyncFailuresByIssueType:      failures filtered by issue type
//   - EarliestSyncFailureRetryAt:       minimum future retry time
//   - SyncFailureCount:                 count of transient failures
//   - ClearSyncFailure:                 remove by path + drive
//   - ClearSyncFailureByPath:           remove by path (any drive)
//   - ClearActionableSyncFailures:      remove all actionable failures
//   - ClearSyncFailuresByPrefix:        remove by path prefix + issue type
//   - ClearResolvedActionableFailures:  remove actionable failures no longer reported
//   - MarkSyncFailureActionable:        promote transient to actionable
//   - UpsertActionableFailures:         batch-upsert scanner-detected issues
//   - PickTrialCandidate:               oldest scope-blocked failure for trial probing
//   - SetScopeRetryAtNow:               unblock scope failures by setting next_retry_at
//   - scanSyncFailureRows:              scan multiple failure rows
//
// Related files:
//   - store.go:             SyncStore type definition and lifecycle
//   - store_observation.go: scanRemoteStateRow (used by transitionRemoteStateOnFailure)
//   - issue_types.go:       issue type constants referenced by IsActionableIssue
package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Shared column list for all sync_failures SELECT queries. Must match
// the scan order in scanSyncFailureRows. Update both when adding columns.
const sqlSelectSyncFailureCols = `path, drive_id, direction, action_type, failure_role, category,
		COALESCE(issue_type, ''), COALESCE(item_id, ''),
		failure_count, COALESCE(next_retry_at, 0), COALESCE(manual_trial_requested_at, 0),
		COALESCE(last_error, ''), COALESCE(http_status, 0),
		first_seen_at, last_seen_at,
		COALESCE(file_size, 0), COALESCE(local_hash, ''),
		scope_key`

func normalizeFailureActionType(direction synctypes.Direction, actionType synctypes.ActionType) synctypes.ActionType {
	switch actionType {
	case synctypes.ActionUpload,
		synctypes.ActionLocalDelete,
		synctypes.ActionRemoteDelete,
		synctypes.ActionLocalMove,
		synctypes.ActionRemoteMove,
		synctypes.ActionFolderCreate,
		synctypes.ActionConflict,
		synctypes.ActionUpdateSynced,
		synctypes.ActionCleanup:
		return actionType
	case synctypes.ActionDownload:
		return actionTypeForDirection(direction)
	default:
		return actionTypeForDirection(direction)
	}
}

func actionTypeForDirection(direction synctypes.Direction) synctypes.ActionType {
	switch direction {
	case synctypes.DirectionDownload:
		return synctypes.ActionDownload
	case synctypes.DirectionUpload:
		return synctypes.ActionUpload
	case synctypes.DirectionDelete:
		return synctypes.ActionRemoteDelete
	default:
		panic(fmt.Sprintf("syncstore: unsupported failure direction %q", direction))
	}
}

func normalizeFailureParams(
	params *synctypes.SyncFailureParams,
	delayFn func(int) time.Duration,
) (synctypes.FailureCategory, synctypes.FailureRole, string, error) {
	role, err := resolveFailureRole(params)
	if err != nil {
		return "", "", "", err
	}

	category := resolveFailureCategory(params, role)
	scopeWire := params.ScopeKey.String()
	if err := validateFailureRoleParams(params, role, category, delayFn); err != nil {
		return "", "", "", err
	}

	if category == synctypes.CategoryActionable && delayFn != nil {
		return "", "", "", fmt.Errorf("sync: recording failure for %s: actionable failures cannot schedule retry", params.Path)
	}

	return category, role, scopeWire, nil
}

func resolveFailureRole(params *synctypes.SyncFailureParams) (synctypes.FailureRole, error) {
	if params.Role != "" {
		return params.Role, nil
	}
	if params.ScopeKey.IsZero() {
		return synctypes.FailureRoleItem, nil
	}
	return "", fmt.Errorf("sync: recording failure for %s: scoped failure requires explicit role", params.Path)
}

func resolveFailureCategory(
	params *synctypes.SyncFailureParams,
	role synctypes.FailureRole,
) synctypes.FailureCategory {
	if params.Category != "" {
		return params.Category
	}
	if role == synctypes.FailureRoleBoundary {
		return synctypes.CategoryActionable
	}
	return synctypes.CategoryTransient
}

func validateFailureRoleParams(
	params *synctypes.SyncFailureParams,
	role synctypes.FailureRole,
	category synctypes.FailureCategory,
	delayFn func(int) time.Duration,
) error {
	switch role {
	case synctypes.FailureRoleItem:
		return nil
	case synctypes.FailureRoleHeld:
		if params.ScopeKey.IsZero() {
			return fmt.Errorf("sync: recording failure for %s: held failures require a scope key", params.Path)
		}
		if category != synctypes.CategoryTransient {
			return fmt.Errorf("sync: recording failure for %s: held failures must be transient", params.Path)
		}
		if delayFn != nil {
			return fmt.Errorf("sync: recording failure for %s: held failures cannot schedule retry until release", params.Path)
		}
		return nil
	case synctypes.FailureRoleBoundary:
		if params.ScopeKey.IsZero() {
			return fmt.Errorf("sync: recording failure for %s: boundary failures require a scope key", params.Path)
		}
		if category != synctypes.CategoryActionable {
			return fmt.Errorf("sync: recording failure for %s: boundary failures must be actionable", params.Path)
		}
		if delayFn != nil {
			return fmt.Errorf("sync: recording failure for %s: boundary failures cannot schedule retry", params.Path)
		}
		return nil
	default:
		return fmt.Errorf("sync: recording failure for %s: invalid failure role %q", params.Path, role)
	}
}

func normalizeActionableFailure(failure *synctypes.ActionableFailure) (synctypes.FailureRole, string, error) {
	role := failure.Role
	if role == "" {
		role = synctypes.FailureRoleItem
	}

	scopeWire := failure.ScopeKey.String()
	switch role {
	case synctypes.FailureRoleItem:
	case synctypes.FailureRoleBoundary:
		if failure.ScopeKey.IsZero() {
			return "", "", fmt.Errorf("sync: upserting actionable failure for %s: boundary failures require a scope key", failure.Path)
		}
	case synctypes.FailureRoleHeld:
		return "", "", fmt.Errorf("sync: upserting actionable failure for %s: held failures are never actionable upserts", failure.Path)
	default:
		return "", "", fmt.Errorf("sync: upserting actionable failure for %s: invalid failure role %q", failure.Path, role)
	}

	return role, scopeWire, nil
}

// IsActionableIssue returns true for issue types that require user action and
// should not be auto-retried. Transient issues (e.g. IssueServiceOutage)
// are NOT matched — they auto-resolve when the external condition clears.
func IsActionableIssue(issueType string) bool {
	switch issueType {
	case synctypes.IssueInvalidFilename, synctypes.IssuePathTooLong, synctypes.IssueFileTooLarge,
		synctypes.IssuePermissionDenied, synctypes.IssueQuotaExceeded, synctypes.IssueLocalPermissionDenied,
		synctypes.IssueCaseCollision, synctypes.IssueDiskFull, synctypes.IssueFileTooLargeForSpace,
		synctypes.IssueBigDeleteHeld:
		return true
	default:
		return false
	}
}

// RecordFailure is the unified failure recording method. It always runs in a
// transaction and handles all failure types (upload, download, delete).
//
// For download/delete failures, it atomically transitions remote_state
// (downloading→download_failed, deleting→delete_failed). The WHERE clause
// is a natural no-op when no matching row exists (uploads, pre-upload
// validation, permission checks).
//
// When ItemID is not provided and direction is download/delete, it is
// auto-resolved from remote_state within the same transaction.
//
// The engine sets p.Category (CategoryTransient or CategoryActionable) and provides
// delayFn for computing next_retry_at from the current failure count.
// Passing delayFn=nil means no retry scheduling (actionable failures).
//
// UPSERT ON CONFLICT uses COALESCE for issue_type, item_id, file_size,
// local_hash to preserve existing values when new values are empty/zero.
func (m *SyncStore) RecordFailure(ctx context.Context, p *synctypes.SyncFailureParams, delayFn func(int) time.Duration) error {
	now := m.nowFunc()
	category, role, scopeWire, err := normalizeFailureParams(p, delayFn)
	if err != nil {
		return err
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning failure transaction for %s: %w", p.Path, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: Transition remote_state status for download/delete failures.
	if p.Direction == synctypes.DirectionDownload || p.Direction == synctypes.DirectionDelete {
		if transErr := m.transitionRemoteStateOnFailure(ctx, tx, p.Path); transErr != nil {
			return transErr
		}
	}

	// Step 2: Read current failure count and compute backoff.
	currentFailures := m.readFailureCount(ctx, tx, p)
	nextRetryNano := m.computeNextRetry(now, currentFailures, delayFn)
	newCount := currentFailures + 1
	nowNano := now.UnixNano()

	// Step 3: Auto-resolve item_id from remote_state for download/delete
	// when caller didn't provide one.
	itemID, resolveErr := m.resolveItemID(ctx, tx, p)
	if resolveErr != nil {
		return resolveErr
	}

	// Step 5: UPSERT into sync_failures with full field set.
	// COALESCE preserves existing values when new values are empty/zero.
	// NOTE: UpsertActionableFailures has a parallel INSERT with different
	// ON CONFLICT semantics — update both when adding columns.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sync_failures
			(path, drive_id, direction, action_type, failure_role, category, issue_type, item_id,
			 failure_count, next_retry_at, manual_trial_requested_at, last_error, http_status,
			 first_seen_at, last_seen_at, file_size, local_hash, scope_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path, drive_id) DO UPDATE SET
			direction = excluded.direction,
			action_type = excluded.action_type,
			failure_role = excluded.failure_role,
			category = excluded.category,
			issue_type = COALESCE(excluded.issue_type, sync_failures.issue_type),
			item_id = COALESCE(excluded.item_id, sync_failures.item_id),
			failure_count = sync_failures.failure_count + 1,
			next_retry_at = excluded.next_retry_at,
			manual_trial_requested_at = excluded.manual_trial_requested_at,
			last_error = excluded.last_error,
			http_status = excluded.http_status,
			last_seen_at = excluded.last_seen_at,
			file_size = COALESCE(excluded.file_size, sync_failures.file_size),
			local_hash = COALESCE(excluded.local_hash, sync_failures.local_hash),
			scope_key = excluded.scope_key`,
		p.Path, p.DriveID.String(), p.Direction, normalizeFailureActionType(p.Direction, p.ActionType), role, category,
		nullString(p.IssueType), nullString(itemID),
		newCount, nextRetryNano, int64(0), p.ErrMsg, p.HTTPStatus,
		nowNano, nowNano, nullInt64(p.FileSize), nullString(p.LocalHash), scopeWire,
	)
	if err != nil {
		return fmt.Errorf("sync: recording sync failure for %s: %w", p.Path, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing failure for %s: %w", p.Path, err)
	}

	return nil
}

// readFailureCount queries the current failure count for backoff computation.
// Returns 0 if the row doesn't exist or the query fails (non-critical).
func (m *SyncStore) readFailureCount(ctx context.Context, tx *sql.Tx, p *synctypes.SyncFailureParams) int {
	var count int
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT failure_count FROM sync_failures WHERE path = ? AND drive_id = ?`,
		p.Path, p.DriveID.String(),
	).Scan(&count); scanErr != nil && scanErr != sql.ErrNoRows {
		m.logger.Warn("RecordFailure: failed to read failure count, using 0",
			slog.String("path", p.Path),
			slog.String("error", scanErr.Error()),
		)
	}

	return count
}

// computeNextRetry returns the next_retry_at nanosecond timestamp, or nil for
// actionable failures (no retry scheduling). Uses delayFn(currentFailures) to
// compute the backoff duration.
func (m *SyncStore) computeNextRetry(now time.Time, currentFailures int, delayFn func(int) time.Duration) *int64 {
	if delayFn == nil {
		return nil
	}

	delay := delayFn(currentFailures)
	n := now.Add(delay).UnixNano()

	return &n
}

// resolveItemID returns the item_id for this failure. Uses p.ItemID if set;
// for download/delete failures, falls back to looking up remote_state.
func (m *SyncStore) resolveItemID(ctx context.Context, tx *sql.Tx, p *synctypes.SyncFailureParams) (string, error) {
	if p.ItemID != "" {
		return p.ItemID, nil
	}

	if p.Direction != synctypes.DirectionDownload && p.Direction != synctypes.DirectionDelete {
		return "", nil
	}

	var itemID string
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT item_id FROM remote_state WHERE path = ? AND drive_id = ?`,
		p.Path, p.DriveID.String(),
	).Scan(&itemID); scanErr != nil && scanErr != sql.ErrNoRows {
		return "", fmt.Errorf("sync: reading item_id for %s: %w", p.Path, scanErr)
	}

	return itemID, nil
}

// transitionRemoteStateOnFailure transitions remote_state status for
// download/delete failures (downloading→download_failed, deleting→delete_failed).
// The WHERE clause is a safe no-op when no matching row exists.
func (m *SyncStore) transitionRemoteStateOnFailure(ctx context.Context, tx *sql.Tx, path string) error {
	result, execErr := tx.ExecContext(ctx,
		`UPDATE remote_state SET
			sync_status = CASE sync_status
				WHEN ? THEN ?
				WHEN ? THEN ?
			END
		WHERE path = ? AND sync_status IN (?, ?)`,
		synctypes.SyncStatusDownloading, synctypes.SyncStatusDownloadFailed,
		synctypes.SyncStatusDeleting, synctypes.SyncStatusDeleteFailed,
		path, synctypes.SyncStatusDownloading, synctypes.SyncStatusDeleting,
	)
	if execErr != nil {
		return fmt.Errorf("sync: transitioning remote_state for %s: %w", path, execErr)
	}

	affected, rowErr := result.RowsAffected()
	if rowErr != nil {
		return fmt.Errorf("sync: checking rows affected for %s: %w", path, rowErr)
	}

	if affected == 0 {
		m.logger.Debug("RecordFailure: remote_state row already transitioned or absent",
			slog.String("path", path),
		)
	}

	return nil
}

// ListSyncFailures returns all sync_failures rows ordered by last_seen_at DESC.
func (m *SyncStore) ListSyncFailures(ctx context.Context) ([]synctypes.SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ListActionableFailures returns sync_failures rows where category is actionable.
// Used by the issues command to show user-actionable file issues.
func (m *SyncStore) ListActionableFailures(ctx context.Context) ([]synctypes.SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'actionable' ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing actionable sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ListRemoteBlockedFailures returns the held rows that define the derived
// remote read-only scopes. These rows remain transient because they represent
// blocked work, not independent actionable failures.
func (m *SyncStore) ListRemoteBlockedFailures(ctx context.Context) ([]synctypes.SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE failure_role = ? AND scope_key LIKE 'perm:remote:%'
		ORDER BY last_seen_at DESC`,
		synctypes.FailureRoleHeld,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: listing remote blocked failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ClearSyncFailure removes a specific sync_failures row by path and drive.
func (m *SyncStore) ClearSyncFailure(ctx context.Context, path string, driveID driveid.ID) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ? AND drive_id = ?`,
		path, driveID.String())
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}

	return nil
}

// ClearSyncFailureByPath removes all sync_failures rows for a path regardless
// of drive. Used by CLI commands where the drive context isn't known.
func (m *SyncStore) ClearSyncFailureByPath(ctx context.Context, path string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}

	return nil
}

// ClearActionableSyncFailures removes all actionable sync_failures rows.
func (m *SyncStore) ClearActionableSyncFailures(ctx context.Context) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE category = 'actionable'`)
	if err != nil {
		return fmt.Errorf("sync: clearing resolved sync failures: %w", err)
	}

	return nil
}

// MarkSyncFailureActionable sets a sync_failures row to category='actionable'
// and clears its next_retry_at.
func (m *SyncStore) MarkSyncFailureActionable(ctx context.Context, path string, driveID driveid.ID) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE sync_failures
		SET category = 'actionable', failure_role = 'item', next_retry_at = NULL
		WHERE path = ? AND drive_id = ?`,
		path, driveID.String())
	if err != nil {
		return fmt.Errorf("sync: marking sync failure actionable for %s: %w", path, err)
	}

	return nil
}

// UpsertActionableFailures batch-upserts scanner-detected issues into
// sync_failures as actionable entries. Each failure is inserted with
// failure_count=1 and no next_retry_at. On conflict, the existing row is
// updated with the latest error info.
func (m *SyncStore) UpsertActionableFailures(ctx context.Context, failures []synctypes.ActionableFailure) error {
	if len(failures) == 0 {
		return nil
	}

	now := m.nowFunc()
	nowNano := now.UnixNano()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin upsert actionable failures: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// NOTE: RecordFailure has a parallel INSERT with different ON CONFLICT
	// semantics (COALESCE, failure_count increment) — update both when
	// adding columns.
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO sync_failures
			(path, drive_id, direction, action_type, failure_role, category, issue_type, item_id,
			 failure_count, next_retry_at, manual_trial_requested_at, last_error, http_status,
			 first_seen_at, last_seen_at, file_size, local_hash, scope_key)
		VALUES (?, ?, ?, ?, ?, 'actionable', ?, '', 1, NULL, 0, ?, 0, ?, ?, ?, '', ?)
		ON CONFLICT(path, drive_id) DO UPDATE SET
			direction = excluded.direction,
			action_type = excluded.action_type,
			failure_role = excluded.failure_role,
			category = 'actionable',
			issue_type = excluded.issue_type,
			next_retry_at = NULL,
			manual_trial_requested_at = 0,
			last_error = excluded.last_error,
			last_seen_at = excluded.last_seen_at,
			file_size = excluded.file_size,
			scope_key = excluded.scope_key`)
	if err != nil {
		return fmt.Errorf("sync: prepare upsert actionable: %w", err)
	}
	defer stmt.Close()

	for i := range failures {
		f := &failures[i]
		role, scopeWire, err := normalizeActionableFailure(f)
		if err != nil {
			return err
		}

		if _, err := stmt.ExecContext(ctx,
			f.Path, f.DriveID.String(), f.Direction, normalizeFailureActionType(f.Direction, f.ActionType), role,
			nullString(f.IssueType), f.Error,
			nowNano, nowNano, f.FileSize, scopeWire,
		); err != nil {
			return fmt.Errorf("sync: upsert actionable failure for %s: %w", f.Path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit actionable failure upsert: %w", err)
	}

	return nil
}

// ClearResolvedActionableFailures removes actionable sync_failures entries of
// the given issue type whose paths are NOT in currentPaths. This cleans up
// issues that the scanner no longer reports (the underlying cause was resolved).
func (m *SyncStore) ClearResolvedActionableFailures(ctx context.Context, issueType string, currentPaths []string) error {
	if issueType == "" {
		return nil
	}

	// Build a set of currently-skipped paths for IN clause.
	if len(currentPaths) == 0 {
		// No current paths → all entries for this issue type are resolved.
		_, err := m.db.ExecContext(ctx,
			`DELETE FROM sync_failures
			WHERE category = 'actionable' AND failure_role = 'item' AND issue_type = ?`,
			issueType)
		if err != nil {
			return fmt.Errorf("sync: clearing resolved actionable failures for %s: %w", issueType, err)
		}

		return nil
	}

	// Build parameterized IN clause with literal placeholders.
	placeholders := "?" + strings.Repeat(",?", len(currentPaths)-1)
	args := make([]any, 0, len(currentPaths)+1)
	args = append(args, issueType)

	for _, p := range currentPaths {
		args = append(args, p)
	}

	//nolint:gosec // G202: placeholders is strings.Repeat(",?", n) — literal, not user input
	query := `DELETE FROM sync_failures
		WHERE category = 'actionable' AND failure_role = 'item' AND issue_type = ? AND path NOT IN (` +
		placeholders + `)`

	_, err := m.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sync: clearing resolved actionable failures for %s: %w", issueType, err)
	}

	return nil
}

// DeleteSyncFailuresByScope removes all sync_failures rows matching the
// given scope_key. Used when a scope source is removed (e.g., shortcut
// deleted) to clean up orphaned failure records.
func (m *SyncStore) DeleteSyncFailuresByScope(ctx context.Context, scopeKey synctypes.ScopeKey) error {
	wire := scopeKey.String()

	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ?`, wire)
	if err != nil {
		return fmt.Errorf("sync: deleting failures for scope %s: %w", wire, err)
	}

	return nil
}

// ResetRetryTimesForScope sets next_retry_at to the given time for all
// transient sync_failures matching the given scope_key whose next_retry_at
// is in the future. The caller (engine) provides the timestamp so the
// engine's clock is authoritative — the store doesn't decide when "now" is.
// This is the "thundering herd" mechanism (R-2.10.11, R-2.10.15).
func (m *SyncStore) ResetRetryTimesForScope(ctx context.Context, scopeKey synctypes.ScopeKey, now time.Time) error {
	wire := scopeKey.String()
	nowNano := now.UnixNano()

	_, err := m.db.ExecContext(ctx,
		`UPDATE sync_failures SET next_retry_at = ?
		WHERE scope_key = ? AND next_retry_at > ? AND category = 'transient'`,
		nowNano, wire, nowNano,
	)
	if err != nil {
		return fmt.Errorf("sync: resetting retry times for scope %s: %w", wire, err)
	}

	return nil
}

// PendingRetrySummary returns aggregated counts of transient failures
// grouped by scope_key, with the earliest next_retry_at per group.
// Used by the issues command to show pending retries (R-2.10.22).
func (m *SyncStore) PendingRetrySummary(ctx context.Context) ([]synctypes.PendingRetryGroup, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT COALESCE(scope_key, ''), COUNT(*), MIN(next_retry_at)
		 FROM sync_failures
		 WHERE category = 'transient' AND next_retry_at > 0
		 GROUP BY scope_key
		 ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: querying pending retry summary: %w", err)
	}
	defer rows.Close()

	var result []synctypes.PendingRetryGroup

	for rows.Next() {
		var g synctypes.PendingRetryGroup
		var minNano int64
		var wireScopeKey string

		if scanErr := rows.Scan(&wireScopeKey, &g.Count, &minNano); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning pending retry group: %w", scanErr)
		}

		g.ScopeKey = synctypes.ParseScopeKey(wireScopeKey)

		if minNano > 0 {
			g.EarliestNext = time.Unix(0, minNano)
		}

		result = append(result, g)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating pending retry groups: %w", err)
	}

	return result, nil
}

// ListSyncFailuresForRetry returns transient sync_failures rows whose
// next_retry_at has expired (ready for retry).
func (m *SyncStore) ListSyncFailuresForRetry(ctx context.Context, now time.Time) ([]synctypes.SyncFailureRow, error) {
	nowNano := now.UnixNano()
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'transient'
			AND next_retry_at IS NOT NULL
			AND next_retry_at <= ?`,
		nowNano)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures for retry: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// EarliestSyncFailureRetryAt returns the minimum future next_retry_at across
// transient sync_failures. Returns zero time if none exist.
func (m *SyncStore) EarliestSyncFailureRetryAt(ctx context.Context, now time.Time) (time.Time, error) {
	nowNano := now.UnixNano()

	var minNano *int64
	err := m.db.QueryRowContext(ctx,
		`SELECT MIN(next_retry_at) FROM sync_failures
		WHERE category = 'transient'
			AND next_retry_at IS NOT NULL
			AND next_retry_at > ?`,
		nowNano,
	).Scan(&minNano)
	if err != nil {
		return time.Time{}, fmt.Errorf("sync: querying earliest sync failure retry: %w", err)
	}

	if minNano == nil {
		return time.Time{}, nil
	}

	return time.Unix(0, *minNano), nil
}

// SyncFailureCount returns the number of transient sync_failures rows.
func (m *SyncStore) SyncFailureCount(ctx context.Context) (int, error) {
	var count int

	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sync_failures WHERE category = 'transient'`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sync: counting sync failures: %w", err)
	}

	return count, nil
}

// ListSyncFailuresByIssueType returns all sync_failures rows with the given issue_type.
func (m *SyncStore) ListSyncFailuresByIssueType(ctx context.Context, issueType string) ([]synctypes.SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE issue_type = ? ORDER BY last_seen_at DESC`, issueType)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures by type %s: %w", issueType, err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ClearSyncFailuresByPrefix removes all sync_failures rows whose path starts
// with the given prefix and matches the given issue_type.
func (m *SyncStore) ClearSyncFailuresByPrefix(ctx context.Context, pathPrefix, issueType string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE issue_type = ? AND (path = ? OR path LIKE ?)`,
		issueType, pathPrefix, pathPrefix+"/%")
	if err != nil {
		return fmt.Errorf("sync: clearing sync failures by prefix %s: %w", pathPrefix, err)
	}

	return nil
}

// PickTrialCandidate returns the oldest scope-blocked failure for the given
// scope key — i.e., a row with matching scope_key and NULL next_retry_at
// (scope-blocked failures have no retry scheduling; they wait for the scope
// to clear).
//
// The engine uses this to find a real action to execute as a trial probe
// when a scope block's trial timer fires.
func (m *SyncStore) PickTrialCandidate(
	ctx context.Context,
	scopeKey synctypes.ScopeKey,
) (*synctypes.SyncFailureRow, bool, error) {
	wire := scopeKey.String()

	row := m.db.QueryRowContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL
		ORDER BY CASE WHEN manual_trial_requested_at > 0 THEN 0 ELSE 1 END,
			manual_trial_requested_at ASC, first_seen_at ASC
		LIMIT 1`,
		wire, synctypes.FailureRoleHeld,
	)

	var r synctypes.SyncFailureRow
	var wireScopeKey string

	err := row.Scan(
		&r.Path, &r.DriveID, &r.Direction, &r.ActionType, &r.Role, &r.Category,
		&r.IssueType, &r.ItemID,
		&r.FailureCount, &r.NextRetryAt, &r.ManualTrialRequestedAt,
		&r.LastError, &r.HTTPStatus,
		&r.FirstSeenAt, &r.LastSeenAt,
		&r.FileSize, &r.LocalHash,
		&wireScopeKey,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("sync: picking trial candidate for scope %s: %w", wire, err)
	}

	r.ScopeKey = synctypes.ParseScopeKey(wireScopeKey)

	return &r, true, nil
}

// SetScopeRetryAtNow sets next_retry_at to the given time for all transient
// sync_failures matching the given scope_key whose next_retry_at IS NULL.
// These are the scope-blocked failures that were waiting for the scope to
// clear. Making them retryable is the "thundering herd" mechanism that
// re-injects all scope-blocked items when a scope clears (R-2.10.11).
//
// Returns the number of rows updated.
func (m *SyncStore) SetScopeRetryAtNow(ctx context.Context, scopeKey synctypes.ScopeKey, now time.Time) (int64, error) {
	wire := scopeKey.String()
	nowNano := now.UnixNano()

	result, err := m.db.ExecContext(ctx,
		`UPDATE sync_failures
		SET failure_role = ?, next_retry_at = ?
		WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL AND category = ?`,
		synctypes.FailureRoleItem, nowNano, wire, synctypes.FailureRoleHeld, synctypes.CategoryTransient,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: setting scope retry-at for %s: %w", wire, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: rows affected for scope retry-at %s: %w", wire, err)
	}

	return affected, nil
}

// scanSyncFailureRows scans multiple sync_failures rows from a query result.
func scanSyncFailureRows(rows *sql.Rows) ([]synctypes.SyncFailureRow, error) {
	var result []synctypes.SyncFailureRow

	for rows.Next() {
		var r synctypes.SyncFailureRow
		var wireScopeKey string
		if scanErr := rows.Scan(
			&r.Path, &r.DriveID, &r.Direction, &r.ActionType, &r.Role, &r.Category,
			&r.IssueType, &r.ItemID,
			&r.FailureCount, &r.NextRetryAt, &r.ManualTrialRequestedAt,
			&r.LastError, &r.HTTPStatus,
			&r.FirstSeenAt, &r.LastSeenAt,
			&r.FileSize, &r.LocalHash,
			&wireScopeKey,
		); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning sync failure row: %w", scanErr)
		}

		r.ScopeKey = synctypes.ParseScopeKey(wireScopeKey)
		result = append(result, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating sync failure rows: %w", err)
	}

	return result, nil
}
