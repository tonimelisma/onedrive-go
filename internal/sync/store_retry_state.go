package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	sqlUpsertRetryState = `INSERT INTO retry_state
		(work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(work_key) DO UPDATE SET
			path = excluded.path,
			old_path = excluded.old_path,
			action_type = excluded.action_type,
			scope_key = excluded.scope_key,
			blocked = excluded.blocked,
			attempt_count = excluded.attempt_count,
			next_retry_at = excluded.next_retry_at,
			last_error = excluded.last_error,
			first_seen_at = excluded.first_seen_at,
			last_seen_at = excluded.last_seen_at`
	sqlDeleteRetryStateByPathType = `DELETE FROM retry_state WHERE path = ? AND old_path = ? AND action_type = ?`
	sqlListRetryState             = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_state
		ORDER BY path, work_key`
	sqlListRetryStateReady = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_state
		WHERE blocked = 0 AND next_retry_at > 0 AND next_retry_at <= ?
		ORDER BY next_retry_at, path, work_key`
	sqlPickRetryTrialCandidate = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_state
		WHERE blocked = 1 AND scope_key = ?
		ORDER BY RANDOM()
		LIMIT 1`
	sqlPruneScopeBlocksWithoutBlockedRetries = `DELETE FROM scope_blocks
		WHERE NOT EXISTS (
			SELECT 1 FROM retry_state
			WHERE retry_state.blocked = 1
				AND retry_state.scope_key = scope_blocks.scope_key
		)`
)

func serializeRetryWorkKey(key RetryWorkKey) string {
	return fmt.Sprintf("%s\x00%s\x00%s", key.ActionType.String(), key.OldPath, key.Path)
}

func retryWorkKey(path string, oldPath string, actionType ActionType) RetryWorkKey {
	return RetryWorkKey{
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func (m *SyncStore) UpsertRetryState(ctx context.Context, row *RetryStateRow) error {
	return upsertRetryStateTx(ctx, m.db, row)
}

func upsertRetryStateTx(ctx context.Context, runner sqlTxRunner, row *RetryStateRow) error {
	if row == nil {
		return fmt.Errorf("sync: upserting retry_state: nil row")
	}
	if row.WorkKey == "" {
		row.WorkKey = serializeRetryWorkKey(retryWorkKey(row.Path, row.OldPath, row.ActionType))
	}

	_, err := runner.ExecContext(ctx, sqlUpsertRetryState,
		row.WorkKey,
		row.Path,
		row.OldPath,
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

func retryStateIdentityForWork(path string, oldPath string, actionType ActionType) RetryStateRow {
	work := retryWorkKey(path, oldPath, actionType)
	return RetryStateRow{
		WorkKey:    serializeRetryWorkKey(work),
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func (m *SyncStore) DeleteRetryStateByWork(ctx context.Context, work RetryWorkKey) error {
	if _, err := m.db.ExecContext(ctx, sqlDeleteRetryStateByPathType, work.Path, work.OldPath, work.ActionType.String()); err != nil {
		return fmt.Errorf("sync: deleting retry_state for %s: %w", work.Path, err)
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
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("sync: picking retry_state trial candidate for %s: %w", scopeKey.String(), err)
	}

	return &parsed, true, nil
}

func (m *SyncStore) PruneRetryStateToCurrentActions(
	ctx context.Context,
	work []RetryWorkKey,
) error {
	rows, err := m.ListRetryState(ctx)
	if err != nil {
		return err
	}

	keep := make(map[RetryWorkKey]struct{}, len(work))
	for i := range work {
		keep[work[i]] = struct{}{}
	}

	for i := range rows {
		key := retryWorkKey(rows[i].Path, rows[i].OldPath, rows[i].ActionType)
		if _, ok := keep[key]; ok {
			continue
		}
		if err := m.DeleteRetryStateByWork(ctx, key); err != nil {
			return err
		}
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

	var scopeKey string
	if err := scanner.Scan(
		&row.WorkKey,
		&row.Path,
		&row.OldPath,
		&row.ActionType,
		&scopeKey,
		&row.Blocked,
		&row.AttemptCount,
		&row.NextRetryAt,
		&row.LastError,
		&row.FirstSeenAt,
		&row.LastSeenAt,
	); err != nil {
		return fmt.Errorf("sync: scanning retry_state row: %w", err)
	}

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
