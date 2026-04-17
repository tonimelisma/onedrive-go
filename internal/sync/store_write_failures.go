// Package sync persists sync baseline, observation, failure, scope-block, and metadata state.
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
//   - IsActionableIssue:               classify issue types requiring user action
//   - RecordFailure:                   unified failure recording
//   - ClearSyncFailure:                remove by path
//   - TakeSyncFailure:                 atomically load + remove by path
//   - ClearSyncFailureByPath:          remove by path (any drive)
//   - ClearActionableSyncFailures:     remove all actionable failures
//   - ClearSyncFailuresByPrefix:       remove by path prefix + issue type
//   - ClearResolvedActionableFailures: remove actionable failures no longer reported
//   - MarkSyncFailureActionable:       promote transient to actionable
//   - UpsertActionableFailures:        batch-upsert scanner-detected issues
//   - DeleteSyncFailuresByScope:       remove failures owned by a removed scope
//   - ResetRetryTimesForScope:         pull future retries forward
//   - SetScopeRetryAtNow:              unblock scope failures by setting next_retry_at
//
// Related files:
//   - store.go:               SyncStore type definition and lifecycle
//   - store_read_failures.go: sync_failures query helpers and scanners
//   - issue_types.go:         issue type constants referenced by IsActionableIssue
package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Shared column list for all sync_failures SELECT queries. Must match
// the scan order in scanSyncFailureRows. Update both when adding columns.
const sqlSelectSyncFailureCols = `path, direction, action_type, failure_role, category,
		COALESCE(issue_type, ''), COALESCE(item_id, ''),
		failure_count, COALESCE(next_retry_at, 0),
		COALESCE(last_error, ''), COALESCE(http_status, 0),
		first_seen_at, last_seen_at,
		COALESCE(file_size, 0), COALESCE(local_hash, ''),
		COALESCE(scope_key, '')`

func normalizeFailureActionType(direction Direction, actionType ActionType) ActionType {
	switch actionType {
	case ActionUpload,
		ActionLocalDelete,
		ActionRemoteDelete,
		ActionLocalMove,
		ActionRemoteMove,
		ActionFolderCreate,
		ActionConflict,
		ActionUpdateSynced,
		ActionCleanup:
		return actionType
	case ActionDownload:
		return actionTypeForDirection(direction)
	default:
		return actionTypeForDirection(direction)
	}
}

func normalizeFailureIdentity(
	direction Direction,
	actionType ActionType,
) (Direction, ActionType) {
	normalizedAction := normalizeFailureActionType(direction, actionType)
	return normalizedAction.Direction(), normalizedAction
}

func actionTypeForDirection(direction Direction) ActionType {
	switch direction {
	case DirectionDownload:
		return ActionDownload
	case DirectionUpload:
		return ActionUpload
	case DirectionDelete:
		return ActionRemoteDelete
	default:
		panic(fmt.Sprintf("sync: unsupported failure direction %q", direction))
	}
}

func normalizeFailureParams(
	params *SyncFailureParams,
	delayFn func(int) time.Duration,
) (FailureCategory, FailureRole, string, error) {
	role, err := resolveFailureRole(params)
	if err != nil {
		return "", "", "", err
	}

	category := resolveFailureCategory(params, role)
	scopeWire := params.ScopeKey.String()
	if err := validateFailureRoleParams(params, role, category, delayFn); err != nil {
		return "", "", "", err
	}

	if category == CategoryActionable && delayFn != nil {
		return "", "", "", fmt.Errorf("sync: recording failure for %s: actionable failures cannot schedule retry", params.Path)
	}

	return category, role, scopeWire, nil
}

func resolveFailureRole(params *SyncFailureParams) (FailureRole, error) {
	if params.Role != "" {
		return params.Role, nil
	}
	if params.ScopeKey.IsZero() {
		return FailureRoleItem, nil
	}
	return "", fmt.Errorf("sync: recording failure for %s: scoped failure requires explicit role", params.Path)
}

func resolveFailureCategory(
	params *SyncFailureParams,
	role FailureRole,
) FailureCategory {
	if params.Category != "" {
		return params.Category
	}
	if role == FailureRoleBoundary {
		return CategoryActionable
	}
	return CategoryTransient
}

func validateFailureRoleParams(
	params *SyncFailureParams,
	role FailureRole,
	category FailureCategory,
	delayFn func(int) time.Duration,
) error {
	switch role {
	case FailureRoleItem:
		return nil
	case FailureRoleHeld:
		if params.ScopeKey.IsZero() {
			return fmt.Errorf("sync: recording failure for %s: held failures require a scope key", params.Path)
		}
		if category != CategoryTransient {
			return fmt.Errorf("sync: recording failure for %s: held failures must be transient", params.Path)
		}
		if delayFn != nil {
			return fmt.Errorf("sync: recording failure for %s: held failures cannot schedule retry until release", params.Path)
		}
		return nil
	case FailureRoleBoundary:
		if params.ScopeKey.IsZero() {
			return fmt.Errorf("sync: recording failure for %s: boundary failures require a scope key", params.Path)
		}
		if category != CategoryActionable {
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

func normalizeActionableFailure(failure *ActionableFailure) (FailureRole, string, error) {
	role := failure.Role
	if role == "" {
		role = FailureRoleItem
	}

	scopeWire := failure.ScopeKey.String()
	switch role {
	case FailureRoleItem:
	case FailureRoleBoundary:
		if failure.ScopeKey.IsZero() {
			return "", "", fmt.Errorf("sync: upserting actionable failure for %s: boundary failures require a scope key", failure.Path)
		}
	case FailureRoleHeld:
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
	case IssueInvalidFilename, IssuePathTooLong, IssueFileTooLarge,
		IssuePermissionDenied, IssueQuotaExceeded, IssueLocalPermissionDenied,
		IssueCaseCollision, IssueDiskFull, IssueFileTooLargeForSpace:
		return true
	default:
		return false
	}
}

// RecordFailure is the unified failure recording method. It always runs in a
// transaction and handles all failure types (upload, download, delete).
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
func (m *SyncStore) RecordFailure(
	ctx context.Context,
	p *SyncFailureParams,
	delayFn func(int) time.Duration,
) (err error) {
	now := m.nowFunc()
	category, role, scopeWire, err := normalizeFailureParams(p, delayFn)
	if err != nil {
		return err
	}
	direction, actionType := normalizeFailureIdentity(p.Direction, p.ActionType)

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning failure transaction for %s: %w", p.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback failure transaction for %s", p.Path))
	}()

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := m.ensureConfiguredDriveIDTx(ctx, tx, p.DriveID, state); ensureErr != nil {
		return ensureErr
	}
	if !state.ConfiguredDriveID.IsZero() {
		p.DriveID = state.ConfiguredDriveID
	}

	// Step 1: Read current failure count and compute backoff.
	currentFailures := m.readFailureCount(ctx, tx, p)
	nextRetryNano := m.computeNextRetry(now, currentFailures, delayFn)
	newCount := currentFailures + 1
	nowNano := now.UnixNano()

	// Step 2: Auto-resolve item_id from remote_state for download/delete
	// when caller didn't provide one.
	itemID, resolveErr := m.resolveItemID(ctx, tx, p.Path, p.ItemID, direction)
	if resolveErr != nil {
		return resolveErr
	}

	// Step 3: UPSERT into sync_failures with full field set.
	// COALESCE preserves existing values when new values are empty/zero.
	// NOTE: UpsertActionableFailures has a parallel INSERT with different
	// ON CONFLICT semantics — update both when adding columns.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sync_failures
			(path, direction, action_type, failure_role, category, issue_type, item_id,
			 failure_count, next_retry_at, last_error, http_status,
			 first_seen_at, last_seen_at, file_size, local_hash, scope_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			direction = excluded.direction,
			action_type = excluded.action_type,
			failure_role = excluded.failure_role,
			category = excluded.category,
			issue_type = COALESCE(excluded.issue_type, sync_failures.issue_type),
			item_id = COALESCE(excluded.item_id, sync_failures.item_id),
			failure_count = sync_failures.failure_count + 1,
			next_retry_at = excluded.next_retry_at,
			last_error = excluded.last_error,
			http_status = excluded.http_status,
			last_seen_at = excluded.last_seen_at,
			file_size = COALESCE(excluded.file_size, sync_failures.file_size),
			local_hash = COALESCE(excluded.local_hash, sync_failures.local_hash),
			scope_key = excluded.scope_key`,
		p.Path, direction, actionType, role, category,
		nullString(p.IssueType), nullString(itemID),
		newCount, nextRetryNano, p.ErrMsg, p.HTTPStatus,
		nowNano, nowNano, nullOptionalInt64(p.FileSize), nullString(p.LocalHash), scopeWire,
	)
	if err != nil {
		return fmt.Errorf("sync: recording sync failure for %s: %w", p.Path, err)
	}
	if category == CategoryTransient && role != FailureRoleBoundary {
		retryRow, retryErr := plannedActionIdentityForRetryTx(ctx, tx, p.Path, actionType)
		if retryErr != nil {
			return retryErr
		}
		retryRow.ScopeKey = p.ScopeKey
		retryRow.Blocked = role == FailureRoleHeld
		retryRow.AttemptCount = newCount
		retryRow.LastError = p.ErrMsg
		retryRow.FirstSeenAt = nowNano
		retryRow.LastSeenAt = nowNano
		if nextRetryNano != nil {
			retryRow.NextRetryAt = *nextRetryNano
		}
		if retryErr := upsertRetryStateTx(ctx, tx, retryRow); retryErr != nil {
			return retryErr
		}
	} else if retryErr := deleteRetryStateByPathTx(ctx, tx, p.Path); retryErr != nil {
		return retryErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing failure for %s: %w", p.Path, err)
	}

	return nil
}

// readFailureCount queries the current failure count for backoff computation.
// Returns 0 if the row doesn't exist or the query fails (non-critical).
func (m *SyncStore) readFailureCount(ctx context.Context, tx sqlTxRunner, p *SyncFailureParams) int {
	var count int
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT failure_count FROM sync_failures WHERE path = ?`,
		p.Path,
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
func (m *SyncStore) resolveItemID(
	ctx context.Context,
	tx sqlTxRunner,
	path string,
	itemID string,
	direction Direction,
) (string, error) {
	if itemID != "" {
		return itemID, nil
	}

	if direction != DirectionDownload && direction != DirectionDelete {
		return "", nil
	}

	var resolvedItemID string
	if scanErr := tx.QueryRowContext(ctx,
		`SELECT item_id FROM remote_state WHERE path = ?`,
		path,
	).Scan(&resolvedItemID); scanErr != nil && scanErr != sql.ErrNoRows {
		return "", fmt.Errorf("sync: reading item_id for %s: %w", path, scanErr)
	}

	return resolvedItemID, nil
}

// ClearSyncFailure removes a specific sync_failures row by path.
func (m *SyncStore) ClearSyncFailure(ctx context.Context, path string, driveID driveid.ID) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ?`,
		path)
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM retry_state WHERE path = ?`, path); err != nil {
		return fmt.Errorf("sync: clearing retry_state for %s: %w", path, err)
	}

	return nil
}

// TakeSyncFailure atomically loads and removes a specific sync_failures row by
// path. Returns found=false when no matching row exists.
func (m *SyncStore) TakeSyncFailure(
	ctx context.Context,
	path string,
	driveID driveid.ID,
) (row *SyncFailureRow, found bool, err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return nil, false, fmt.Errorf("sync: beginning take sync failure for %s: %w", path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback take sync failure transaction for %s", path))
	}()

	configuredDriveID, err := m.configuredDriveIDForRead(ctx, driveID)
	if err != nil {
		return nil, false, fmt.Errorf("sync: reading configured drive for taken sync failure %s: %w", path, err)
	}
	if matchErr := ensureMatchingConfiguredDriveID(driveID, configuredDriveID); matchErr != nil {
		return nil, false, matchErr
	}

	row = &SyncFailureRow{}
	if scanErr := scanSyncFailureRow(tx.QueryRowContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE path = ?`,
		path,
	), row, configuredDriveID); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("sync: taking sync failure for %s: %w", path, scanErr)
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ?`,
		path)
	if err != nil {
		return nil, false, fmt.Errorf("sync: deleting taken sync failure for %s: %w", path, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("sync: checking taken sync failure delete count for %s: %w", path, err)
	}
	if affected != 1 {
		return nil, false, fmt.Errorf("sync: deleting taken sync failure for %s: expected 1 row, got %d", path, affected)
	}
	if err := deleteRetryStateByPathTx(ctx, tx, path); err != nil {
		return nil, false, err
	}

	if err = tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("sync: committing take sync failure for %s: %w", path, err)
	}

	return row, true, nil
}

// ClearSyncFailureByPath removes all sync_failures rows for a path regardless
// of drive. Used by CLI commands where the drive context isn't known.
func (m *SyncStore) ClearSyncFailureByPath(ctx context.Context, path string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE path = ?`, path)
	if err != nil {
		return fmt.Errorf("sync: clearing sync failure for %s: %w", path, err)
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM retry_state WHERE path = ?`, path); err != nil {
		return fmt.Errorf("sync: clearing retry_state for %s: %w", path, err)
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
		WHERE path = ?`,
		path)
	if err != nil {
		return fmt.Errorf("sync: marking sync failure actionable for %s: %w", path, err)
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM retry_state WHERE path = ?`, path); err != nil {
		return fmt.Errorf("sync: clearing retry_state for actionable %s: %w", path, err)
	}

	return nil
}

// UpsertActionableFailures batch-upserts scanner-detected issues into
// sync_failures as actionable entries. Each failure is inserted with
// failure_count=1 and no next_retry_at. On conflict, the existing row is
// updated with the latest error info.
func (m *SyncStore) UpsertActionableFailures(
	ctx context.Context,
	failures []ActionableFailure,
) (err error) {
	if len(failures) == 0 {
		return nil
	}

	now := m.nowFunc()
	nowNano := now.UnixNano()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin upsert actionable failures: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback actionable failure upsert")
	}()

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	for i := range failures {
		if failures[i].DriveID.IsZero() {
			continue
		}
		if ensureErr := m.ensureConfiguredDriveIDTx(ctx, tx, failures[i].DriveID, state); ensureErr != nil {
			return ensureErr
		}
		break
	}

	// NOTE: RecordFailure has a parallel INSERT with different ON CONFLICT
	// semantics (COALESCE, failure_count increment) — update both when
	// adding columns.
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO sync_failures
			(path, direction, action_type, failure_role, category, issue_type, item_id,
			 failure_count, next_retry_at, last_error, http_status,
			 first_seen_at, last_seen_at, file_size, local_hash, scope_key)
		VALUES (?, ?, ?, ?, 'actionable', ?, '', 1, NULL, ?, 0, ?, ?, ?, '', ?)
		ON CONFLICT(path) DO UPDATE SET
			direction = excluded.direction,
			action_type = excluded.action_type,
			failure_role = excluded.failure_role,
			category = 'actionable',
			issue_type = excluded.issue_type,
			next_retry_at = NULL,
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
		role, scopeWire, normalizeErr := normalizeActionableFailure(f)
		if normalizeErr != nil {
			return normalizeErr
		}
		direction, actionType := normalizeFailureIdentity(f.Direction, f.ActionType)

		if _, execErr := stmt.ExecContext(ctx,
			f.Path, direction, actionType, role,
			nullString(f.IssueType), f.Error,
			nowNano, nowNano, f.FileSize, scopeWire,
		); execErr != nil {
			return fmt.Errorf("sync: upsert actionable failure for %s: %w", f.Path, execErr)
		}
		if retryErr := deleteRetryStateByPathTx(ctx, tx, f.Path); retryErr != nil {
			return retryErr
		}
	}

	if err = tx.Commit(); err != nil {
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
// given scope_key. Used when the source subtree disappears to clean up
// orphaned failure records.
func (m *SyncStore) DeleteSyncFailuresByScope(ctx context.Context, scopeKey ScopeKey) error {
	wire := scopeKey.String()

	_, err := m.db.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ?`, wire)
	if err != nil {
		return fmt.Errorf("sync: deleting failures for scope %s: %w", wire, err)
	}
	if err := deleteRetryStateByScopeTx(ctx, m.db, wire); err != nil {
		return err
	}

	return nil
}

// ResetRetryTimesForScope sets next_retry_at to the given time for all
// transient sync_failures matching the given scope_key whose next_retry_at
// is in the future. The caller (engine) provides the timestamp so the
// engine's clock is authoritative — the store doesn't decide when "now" is.
// This is the "thundering herd" mechanism (R-2.10.11, R-2.10.15).
func (m *SyncStore) ResetRetryTimesForScope(ctx context.Context, scopeKey ScopeKey, now time.Time) error {
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

// SetScopeRetryAtNow sets next_retry_at to the given time for all transient
// sync_failures matching the given scope_key whose next_retry_at IS NULL.
// These are the scope-blocked failures that were waiting for the scope to
// clear. Making them retryable is the "thundering herd" mechanism that
// re-injects all scope-blocked items when a scope clears (R-2.10.11).
//
// Returns the number of rows updated.
func (m *SyncStore) SetScopeRetryAtNow(ctx context.Context, scopeKey ScopeKey, now time.Time) (int64, error) {
	wire := scopeKey.String()
	nowNano := now.UnixNano()

	result, err := m.db.ExecContext(ctx,
		`UPDATE sync_failures
		SET failure_role = ?, next_retry_at = ?
		WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL AND category = ?`,
		FailureRoleItem, nowNano, wire, FailureRoleHeld, CategoryTransient,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: setting scope retry-at for %s: %w", wire, err)
	}
	if err := markRetryStateScopeReadyTx(ctx, m.db, wire, nowNano); err != nil {
		return 0, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: rows affected for scope retry-at %s: %w", wire, err)
	}

	return affected, nil
}
