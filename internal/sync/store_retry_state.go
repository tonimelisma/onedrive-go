package sync

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"
)

const (
	sqlUpsertRetryState = `INSERT INTO retry_state
		(action_id, plan_id, path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(action_id) DO UPDATE SET
			plan_id = excluded.plan_id,
			path = excluded.path,
			action_type = excluded.action_type,
			scope_key = excluded.scope_key,
			blocked = excluded.blocked,
			attempt_count = excluded.attempt_count,
			next_retry_at = excluded.next_retry_at,
			last_error = excluded.last_error,
			first_seen_at = excluded.first_seen_at,
			last_seen_at = excluded.last_seen_at`
	sqlDeleteRetryStateByActionID = `DELETE FROM retry_state WHERE action_id = ?`
	sqlDeleteRetryStateByPathType = `DELETE FROM retry_state WHERE path = ? AND action_type = ?`
	sqlListRetryState             = `SELECT
		action_id, plan_id, path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_state
		ORDER BY path, action_id`
	sqlListRetryStateReady = `SELECT
		action_id, plan_id, path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_state
		WHERE blocked = 0 AND next_retry_at > 0 AND next_retry_at <= ?
		ORDER BY next_retry_at, path, action_id`
	sqlPickRetryTrialCandidate = `SELECT
		action_id, plan_id, path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_state
		WHERE blocked = 1 AND scope_key = ?
		ORDER BY RANDOM()
		LIMIT 1`
	sqlPruneRetryStateToLatestPlan = `DELETE FROM retry_state
		WHERE NOT EXISTS (
			SELECT 1 FROM planned_actions
			WHERE planned_actions.path = retry_state.path
				AND planned_actions.action_type = retry_state.action_type
		)`
	sqlPruneScopeBlocksWithoutBlockedRetries = `DELETE FROM scope_blocks
		WHERE NOT EXISTS (
			SELECT 1 FROM retry_state
			WHERE retry_state.blocked = 1
				AND retry_state.scope_key = scope_blocks.scope_key
		)`
	sqlLookupPlannedActionForRetry = `SELECT action_id, plan_id
		FROM planned_actions
		WHERE path = ? AND action_type = ?
		ORDER BY action_id
		LIMIT 1`
)

func retryStateSyntheticActionID(path string, actionType ActionType) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(actionType.String()))
	value := int64(h.Sum64() & 0x7fffffffffffffff)
	if value == 0 {
		return 1
	}

	return value
}

func retryStateActionID(row RetryStateRow) (int64, error) {
	if row.ActionID == "" {
		return retryStateSyntheticActionID(row.Path, row.ActionType), nil
	}

	parsed, err := strconv.ParseInt(row.ActionID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sync: parsing retry action_id %q: %w", row.ActionID, err)
	}
	if parsed == 0 {
		return retryStateSyntheticActionID(row.Path, row.ActionType), nil
	}

	return parsed, nil
}

func (m *SyncStore) UpsertRetryState(ctx context.Context, row RetryStateRow) error {
	return upsertRetryStateTx(ctx, m.db, row)
}

func upsertRetryStateTx(ctx context.Context, runner sqlTxRunner, row RetryStateRow) error {
	actionID, err := retryStateActionID(row)
	if err != nil {
		return err
	}

	_, err = runner.ExecContext(ctx, sqlUpsertRetryState,
		actionID,
		row.PlanID,
		row.Path,
		row.ActionType.String(),
		row.ScopeKey.String(),
		row.Blocked,
		row.AttemptCount,
		row.NextRetryAt,
		row.LastError,
		row.FirstSeenAt,
		row.LastSeenAt,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting retry_state for %s: %w", row.Path, err)
	}

	return nil
}

func deleteRetryStateByPathTx(ctx context.Context, runner sqlTxRunner, path string) error {
	if _, err := runner.ExecContext(ctx, `DELETE FROM retry_state WHERE path = ?`, path); err != nil {
		return fmt.Errorf("sync: deleting retry_state for %s: %w", path, err)
	}

	return nil
}

func deleteRetryStateByScopeTx(ctx context.Context, runner sqlTxRunner, scopeKey string) error {
	if _, err := runner.ExecContext(ctx, `DELETE FROM retry_state WHERE scope_key = ?`, scopeKey); err != nil {
		return fmt.Errorf("sync: deleting retry_state for scope %s: %w", scopeKey, err)
	}

	return nil
}

func markRetryStateScopeReadyTx(
	ctx context.Context,
	runner sqlTxRunner,
	scopeKey string,
	nowNano int64,
) error {
	if _, err := runner.ExecContext(ctx,
		`UPDATE retry_state
		SET blocked = 0, next_retry_at = ?
		WHERE scope_key = ? AND blocked = 1`,
		nowNano, scopeKey,
	); err != nil {
		return fmt.Errorf("sync: setting retry_state scope ready for %s: %w", scopeKey, err)
	}

	return nil
}

func plannedActionIdentityForRetryTx(
	ctx context.Context,
	runner sqlTxRunner,
	path string,
	actionType ActionType,
) (RetryStateRow, error) {
	row := RetryStateRow{
		Path:       path,
		ActionType: actionType,
	}
	var actionID int64
	err := runner.QueryRowContext(ctx, sqlLookupPlannedActionForRetry, path, actionType.String()).
		Scan(&actionID, &row.PlanID)
	switch {
	case err == nil:
		row.ActionID = strconv.FormatInt(actionID, 10)
		return row, nil
	case err == sql.ErrNoRows:
		row.ActionID = strconv.FormatInt(retryStateSyntheticActionID(path, actionType), 10)
		return row, nil
	default:
		return RetryStateRow{}, fmt.Errorf("sync: looking up planned action for retry %s: %w", path, err)
	}
}

func (m *SyncStore) DeleteRetryStateByActionID(ctx context.Context, actionID string) error {
	if actionID == "" {
		return nil
	}

	parsed, err := strconv.ParseInt(actionID, 10, 64)
	if err != nil {
		return fmt.Errorf("sync: parsing retry_state delete action_id %q: %w", actionID, err)
	}

	if _, err := m.db.ExecContext(ctx, sqlDeleteRetryStateByActionID, parsed); err != nil {
		return fmt.Errorf("sync: deleting retry_state action %s: %w", actionID, err)
	}

	return nil
}

func (m *SyncStore) DeleteRetryStateByPathAction(
	ctx context.Context,
	path string,
	actionType ActionType,
) error {
	if _, err := m.db.ExecContext(ctx, sqlDeleteRetryStateByPathType, path, actionType.String()); err != nil {
		return fmt.Errorf("sync: deleting retry_state for %s: %w", path, err)
	}

	return nil
}

func (m *SyncStore) ListRetryState(ctx context.Context) ([]RetryStateRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListRetryState)
	if err != nil {
		return nil, fmt.Errorf("sync: querying retry_state: %w", err)
	}
	defer rows.Close()

	return scanRetryStateRows(rows)
}

func (m *SyncStore) ListRetryStateReady(ctx context.Context, now time.Time) ([]RetryStateRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListRetryStateReady, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("sync: querying ready retry_state rows: %w", err)
	}
	defer rows.Close()

	return scanRetryStateRows(rows)
}

func (m *SyncStore) PickRetryTrialCandidate(
	ctx context.Context,
	scopeKey ScopeKey,
) (*RetryStateRow, bool, error) {
	row := m.db.QueryRowContext(ctx, sqlPickRetryTrialCandidate, scopeKey.String())
	var parsed RetryStateRow
	if err := scanRetryStateRow(row, &parsed); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("sync: picking retry_state trial candidate for %s: %w", scopeKey.String(), err)
	}

	return &parsed, true, nil
}

func (m *SyncStore) PruneRetryStateToLatestPlan(ctx context.Context) error {
	if _, err := m.db.ExecContext(ctx, sqlPruneRetryStateToLatestPlan); err != nil {
		return fmt.Errorf("sync: pruning retry_state to latest plan: %w", err)
	}

	return nil
}

func (m *SyncStore) PruneScopeBlocksWithoutBlockedRetries(ctx context.Context) error {
	if _, err := m.db.ExecContext(ctx, sqlPruneScopeBlocksWithoutBlockedRetries); err != nil {
		return fmt.Errorf("sync: pruning scope blocks without blocked retries: %w", err)
	}

	return nil
}

type retryStateScanner interface {
	Scan(dest ...any) error
}

func scanRetryStateRow(scanner retryStateScanner, row *RetryStateRow) error {
	if row == nil {
		return fmt.Errorf("sync: scanning retry_state row: nil destination")
	}

	var (
		actionID int64
		scopeKey string
	)
	if err := scanner.Scan(
		&actionID,
		&row.PlanID,
		&row.Path,
		&row.ActionType,
		&scopeKey,
		&row.Blocked,
		&row.AttemptCount,
		&row.NextRetryAt,
		&row.LastError,
		&row.FirstSeenAt,
		&row.LastSeenAt,
	); err != nil {
		return err
	}

	row.ActionID = strconv.FormatInt(actionID, 10)
	row.ScopeKey = ParseScopeKey(scopeKey)
	return nil
}

func scanRetryStateRows(rows *sql.Rows) ([]RetryStateRow, error) {
	var result []RetryStateRow
	for rows.Next() {
		var row RetryStateRow
		if err := scanRetryStateRow(rows, &row); err != nil {
			return nil, fmt.Errorf("sync: scanning retry_state row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating retry_state rows: %w", err)
	}

	return result, nil
}
