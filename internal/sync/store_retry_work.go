package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	sqlUpsertRetryWork = `INSERT INTO retry_work
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
	sqlDeleteRetryWorkByPathType = `DELETE FROM retry_work WHERE path = ? AND old_path = ? AND action_type = ?`
	sqlListRetryWork             = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_work
		ORDER BY path, work_key`
	sqlListRetryWorkBlocked = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_work
		WHERE blocked = 1
		ORDER BY scope_key, path, work_key`
	sqlListRetryWorkReady = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_work
		WHERE blocked = 0 AND next_retry_at > 0 AND next_retry_at <= ?
		ORDER BY next_retry_at, path, work_key`
	sqlPickRetryTrialCandidate = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_work
		WHERE blocked = 1 AND scope_key = ?
		ORDER BY RANDOM()
		LIMIT 1`
	sqlGetRetryWorkByWork = `SELECT
		work_key, path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, last_error, first_seen_at, last_seen_at
		FROM retry_work
		WHERE work_key = ?`
	sqlEarliestRetryWorkAt = `SELECT MIN(next_retry_at) FROM retry_work
		WHERE blocked = 0
			AND next_retry_at > ?`
)

func serializeRetryWorkKey(key RetryWorkKey) string {
	return fmt.Sprintf("%s\x00%s\x00%s", key.ActionType.String(), key.OldPath, key.Path)
}

func (m *SyncStore) UpsertRetryWork(ctx context.Context, row *RetryWorkRow) error {
	return upsertRetryWorkTx(ctx, m.db, row)
}

func upsertRetryWorkTx(ctx context.Context, runner sqlTxRunner, row *RetryWorkRow) error {
	if row == nil {
		return fmt.Errorf("sync: upserting retry_work: nil row")
	}
	if row.WorkKey == "" {
		row.WorkKey = serializeRetryWorkKey(retryWorkKey(row.Path, row.OldPath, row.ActionType))
	}

	_, err := runner.ExecContext(ctx, sqlUpsertRetryWork,
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
		return fmt.Errorf("sync: upserting retry_work for %s: %w", row.Path, err)
	}

	return nil
}

func deleteRetryWorkByWorkTx(ctx context.Context, runner sqlTxRunner, work RetryWorkKey) error {
	if _, err := runner.ExecContext(
		ctx,
		sqlDeleteRetryWorkByPathType,
		work.Path,
		work.OldPath,
		work.ActionType.String(),
	); err != nil {
		return fmt.Errorf("sync: deleting retry_work for %s: %w", work.Path, err)
	}

	return nil
}

func deleteRetryWorkByScopeTx(ctx context.Context, runner sqlTxRunner, scopeKey string) error {
	if _, err := runner.ExecContext(ctx, `DELETE FROM retry_work WHERE scope_key = ?`, scopeKey); err != nil {
		return fmt.Errorf("sync: deleting retry_work for scope %s: %w", scopeKey, err)
	}

	return nil
}

func markRetryWorkScopeReadyTx(
	ctx context.Context,
	runner sqlTxRunner,
	scopeKey string,
	nowNano int64,
) error {
	if _, err := runner.ExecContext(ctx,
		`UPDATE retry_work
		SET blocked = 0, next_retry_at = ?
		WHERE scope_key = ? AND blocked = 1`,
		nowNano, scopeKey,
	); err != nil {
		return fmt.Errorf("sync: setting retry_work scope ready for %s: %w", scopeKey, err)
	}

	return nil
}

func retryWorkIdentityForWork(path string, oldPath string, actionType ActionType) RetryWorkRow {
	work := retryWorkKey(path, oldPath, actionType)
	return RetryWorkRow{
		WorkKey:    serializeRetryWorkKey(work),
		Path:       path,
		OldPath:    oldPath,
		ActionType: actionType,
	}
}

func (m *SyncStore) DeleteRetryWorkByWork(ctx context.Context, work RetryWorkKey) error {
	return deleteRetryWorkByWorkTx(ctx, m.db, work)
}

// ResolveTransientRetryWork clears one exact retry_work work item and, when a
// matching transient item failure still exists, removes that reporting row in
// the same transaction.
func (m *SyncStore) ResolveTransientRetryWork(
	ctx context.Context,
	work RetryWorkKey,
) (resolved *ResolvedRetryWork, found bool, err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return nil, false, fmt.Errorf("sync: beginning resolve transient retry work for %s: %w", work.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback resolve transient retry work for %s", work.Path))
	}()

	workKey := serializeRetryWorkKey(work)
	var retryRow RetryWorkRow
	scanErr := scanRetryWorkRow(tx.QueryRowContext(ctx, sqlGetRetryWorkByWork, workKey), &retryRow)
	switch {
	case scanErr == nil:
		found = true
		resolved = &ResolvedRetryWork{
			Work: RetryWorkKey{
				Path:       retryRow.Path,
				OldPath:    retryRow.OldPath,
				ActionType: retryRow.ActionType,
			},
			AttemptCount: retryRow.AttemptCount,
		}
	case errors.Is(scanErr, sql.ErrNoRows):
	default:
		return nil, false, fmt.Errorf("sync: resolving transient retry work for %s: %w", work.Path, scanErr)
	}

	var issueType sql.NullString
	issueScanErr := tx.QueryRowContext(ctx,
		`SELECT issue_type FROM sync_failures
			WHERE path = ?
				AND category = ?
				AND failure_role = ?
				AND action_type = ?`,
		work.Path,
		CategoryTransient,
		FailureRoleItem,
		work.ActionType.String(),
	).Scan(&issueType)
	switch {
	case issueScanErr == nil:
		if resolved == nil {
			resolved = &ResolvedRetryWork{Work: work}
		}
		if issueType.Valid {
			resolved.IssueType = issueType.String
		}
		resolved.HadIssueRow = true
	case errors.Is(issueScanErr, sql.ErrNoRows):
	default:
		return nil, false, fmt.Errorf("sync: resolving transient retry work issue row for %s: %w", work.Path, issueScanErr)
	}

	if retryErr := deleteRetryWorkByWorkTx(ctx, tx, work); retryErr != nil {
		return nil, false, retryErr
	}
	if resolved != nil && resolved.HadIssueRow {
		if _, execErr := tx.ExecContext(ctx,
			`DELETE FROM sync_failures
				WHERE path = ?
					AND category = ?
					AND failure_role = ?
					AND action_type = ?`,
			work.Path,
			CategoryTransient,
			FailureRoleItem,
			work.ActionType.String(),
		); execErr != nil {
			return nil, false, fmt.Errorf("sync: deleting transient sync failure for %s: %w", work.Path, execErr)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("sync: committing transient retry work resolution for %s: %w", work.Path, err)
	}

	if !found {
		return nil, false, nil
	}

	return resolved, true, nil
}

func (m *SyncStore) ListRetryWork(ctx context.Context) ([]RetryWorkRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListRetryWork)
	if err != nil {
		return nil, fmt.Errorf("sync: querying retry_work: %w", err)
	}
	defer rows.Close()

	return scanRetryWorkRows(rows)
}

func (m *SyncStore) ListBlockedRetryWork(ctx context.Context) ([]RetryWorkRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListRetryWorkBlocked)
	if err != nil {
		return nil, fmt.Errorf("sync: querying blocked retry_work rows: %w", err)
	}
	defer rows.Close()

	return scanRetryWorkRows(rows)
}

func (m *SyncStore) ListRetryWorkReady(ctx context.Context, now time.Time) ([]RetryWorkRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListRetryWorkReady, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("sync: querying ready retry_work rows: %w", err)
	}
	defer rows.Close()

	return scanRetryWorkRows(rows)
}

func (m *SyncStore) EarliestRetryWorkAt(ctx context.Context, now time.Time) (time.Time, error) {
	nowNano := now.UnixNano()

	var minNano *int64
	if err := m.db.QueryRowContext(ctx, sqlEarliestRetryWorkAt, nowNano).Scan(&minNano); err != nil {
		return time.Time{}, fmt.Errorf("sync: querying earliest retry_work at: %w", err)
	}

	if minNano == nil {
		return time.Time{}, nil
	}

	return time.Unix(0, *minNano), nil
}

func (m *SyncStore) PickRetryTrialCandidate(
	ctx context.Context,
	scopeKey ScopeKey,
) (*RetryWorkRow, bool, error) {
	row := m.db.QueryRowContext(ctx, sqlPickRetryTrialCandidate, scopeKey.String())
	var parsed RetryWorkRow
	if err := scanRetryWorkRow(row, &parsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("sync: picking retry_work trial candidate for %s: %w", scopeKey.String(), err)
	}

	return &parsed, true, nil
}

func (m *SyncStore) PruneRetryWorkToCurrentActions(
	ctx context.Context,
	work []RetryWorkKey,
) error {
	rows, err := m.ListRetryWork(ctx)
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
		if err := m.DeleteRetryWorkByWork(ctx, key); err != nil {
			return err
		}
	}

	return nil
}

type retryWorkScanner interface {
	Scan(dest ...any) error
}

func scanRetryWorkRow(scanner retryWorkScanner, row *RetryWorkRow) error {
	if row == nil {
		return fmt.Errorf("sync: scanning retry_work row: nil destination")
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
		return fmt.Errorf("sync: scanning retry_work row: %w", err)
	}

	row.ScopeKey = ParseScopeKey(scopeKey)
	return nil
}

func scanRetryWorkRows(rows *sql.Rows) ([]RetryWorkRow, error) {
	var result []RetryWorkRow
	for rows.Next() {
		var row RetryWorkRow
		if err := scanRetryWorkRow(rows, &row); err != nil {
			return nil, fmt.Errorf("sync: scanning retry_work row: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating retry_work rows: %w", err)
	}

	return result, nil
}
