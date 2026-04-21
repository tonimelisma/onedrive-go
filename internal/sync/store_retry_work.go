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
		(work_key, path, old_path, action_type, condition_type, scope_key, blocked,
		 attempt_count, next_retry_at, last_error, http_status, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(work_key) DO UPDATE SET
			path = excluded.path,
			old_path = excluded.old_path,
			action_type = excluded.action_type,
			condition_type = excluded.condition_type,
			scope_key = excluded.scope_key,
			blocked = excluded.blocked,
			attempt_count = excluded.attempt_count,
			next_retry_at = excluded.next_retry_at,
			last_error = excluded.last_error,
			http_status = excluded.http_status,
			first_seen_at = excluded.first_seen_at,
			last_seen_at = excluded.last_seen_at`
	sqlDeleteRetryWorkByPathType = `DELETE FROM retry_work WHERE path = ? AND old_path = ? AND action_type = ?`
	sqlSelectRetryWorkCols       = `work_key, path, old_path, action_type, condition_type, scope_key, blocked, ` +
		`attempt_count, next_retry_at, last_error, http_status, first_seen_at, last_seen_at`
	sqlListRetryWork = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		ORDER BY path, work_key`
	sqlListRetryWorkBlocked = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		WHERE blocked = 1
		ORDER BY scope_key, path, work_key`
	sqlListRetryWorkReady = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		WHERE blocked = 0 AND next_retry_at > 0 AND next_retry_at <= ?
		ORDER BY next_retry_at, path, work_key`
	sqlPickRetryTrialCandidate = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		WHERE blocked = 1 AND scope_key = ?
		ORDER BY RANDOM()
		LIMIT 1`
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
		row.ConditionType,
		row.ScopeKey.String(),
		row.Blocked,
		row.AttemptCount,
		row.NextRetryAt,
		row.LastError,
		row.HTTPStatus,
		row.FirstSeenAt,
		row.LastSeenAt,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting retry_work for %s: %w", row.Path, err)
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

func (m *SyncStore) DeleteRetryWorkByWork(ctx context.Context, work RetryWorkKey) error {
	return deleteRetryWorkByWorkTx(ctx, m.db, work)
}

func (m *SyncStore) RecordRetryWorkFailure(
	ctx context.Context,
	failure *RetryWorkFailure,
	delayFn func(int) time.Duration,
) (row *RetryWorkRow, err error) {
	if failure == nil {
		return nil, fmt.Errorf("sync: record retry_work failure: nil failure")
	}
	if failure.Path == "" {
		return nil, fmt.Errorf("sync: record retry_work failure: missing path")
	}
	if _, valueErr := failure.ActionType.Value(); valueErr != nil {
		return nil, fmt.Errorf("sync: record retry_work failure for %s: invalid action type: %w", failure.Path, valueErr)
	}
	if !failure.Blocked && delayFn == nil {
		return nil, fmt.Errorf("sync: record retry_work failure for %s: retryable work requires delay function", failure.Path)
	}
	if failure.Blocked && failure.ScopeKey.IsZero() {
		return nil, fmt.Errorf("sync: record retry_work failure for %s: blocked work requires scope key", failure.Path)
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return nil, fmt.Errorf("sync: beginning retry_work failure transaction for %s: %w", failure.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback retry_work failure transaction for %s", failure.Path))
	}()

	workKey := retryWorkKey(failure.Path, failure.OldPath, failure.ActionType)
	existing, found, err := loadRetryWorkByWorkTx(ctx, tx, workKey)
	if err != nil {
		return nil, err
	}

	nowNano := m.nowFunc().UnixNano()
	row = &RetryWorkRow{
		WorkKey:       serializeRetryWorkKey(workKey),
		Path:          failure.Path,
		OldPath:       failure.OldPath,
		ActionType:    failure.ActionType,
		ConditionType: failure.ConditionType,
		ScopeKey:      failure.ScopeKey,
		Blocked:       failure.Blocked,
		AttemptCount:  1,
		LastError:     failure.LastError,
		HTTPStatus:    failure.HTTPStatus,
		FirstSeenAt:   nowNano,
		LastSeenAt:    nowNano,
	}
	if found && existing != nil {
		row.AttemptCount = existing.AttemptCount + 1
		row.FirstSeenAt = existing.FirstSeenAt
	}
	if !failure.Blocked {
		row.NextRetryAt = m.nowFunc().Add(delayFn(row.AttemptCount - 1)).UnixNano()
	}

	if err := upsertRetryWorkTx(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync: committing retry_work failure for %s: %w", failure.Path, err)
	}

	return row, nil
}

func loadRetryWorkByWorkTx(
	ctx context.Context,
	runner sqlTxRunner,
	work RetryWorkKey,
) (*RetryWorkRow, bool, error) {
	row := &RetryWorkRow{}
	scanErr := scanRetryWorkRow(runner.QueryRowContext(ctx,
		`SELECT `+sqlSelectRetryWorkCols+` FROM retry_work
			WHERE path = ? AND old_path = ? AND action_type = ?`,
		work.Path,
		work.OldPath,
		work.ActionType.String(),
	), row)
	switch {
	case scanErr == nil:
		return row, true, nil
	case errors.Is(scanErr, sql.ErrNoRows):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("sync: loading retry_work for %s: %w", work.Path, scanErr)
	}
}

func (m *SyncStore) ResolveRetryWork(
	ctx context.Context,
	work RetryWorkKey,
) (row *RetryWorkRow, found bool, err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return nil, false, fmt.Errorf("sync: beginning resolve retry_work for %s: %w", work.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback resolve retry_work for %s", work.Path))
	}()

	row, found, err = loadRetryWorkByWorkTx(ctx, tx, work)
	if err != nil {
		return nil, false, err
	}
	if deleteErr := deleteRetryWorkByWorkTx(ctx, tx, work); deleteErr != nil {
		return nil, false, deleteErr
	}

	if err = tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("sync: committing retry_work resolution for %s: %w", work.Path, err)
	}

	return row, found, nil
}

func (m *SyncStore) ClearBlockedRetryWork(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) (err error) {
	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin clear blocked retry_work tx for %s: %w", work.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback clear blocked retry_work tx for %s", work.Path))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM retry_work
			WHERE path = ? AND old_path = ? AND action_type = ? AND scope_key = ? AND blocked = 1`,
		work.Path,
		work.OldPath,
		work.ActionType.String(),
		scopeKey.String(),
	); execErr != nil {
		return fmt.Errorf("sync: deleting blocked retry_work for %s: %w", work.Path, execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit clear blocked retry_work for %s: %w", work.Path, err)
	}

	return nil
}

func (m *SyncStore) ListRetryWork(ctx context.Context) ([]RetryWorkRow, error) {
	rows, err := m.db.QueryContext(ctx, sqlListRetryWork)
	if err != nil {
		return nil, fmt.Errorf("sync: querying retry_work: %w", err)
	}
	defer rows.Close()

	return scanRetryWorkRows(rows)
}

func queryBlockedRetryWorkRowsWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
) ([]RetryWorkRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListRetryWorkBlocked)
	if err != nil {
		return nil, fmt.Errorf("query blocked retry_work rows: %w", err)
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

func (m *SyncStore) CountRetryingWork(ctx context.Context) (int, error) {
	var count int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM retry_work WHERE blocked = 0 AND attempt_count >= 3`,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("sync: counting retry_work rows: %w", err)
	}

	return count, nil
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
		&row.ConditionType,
		&scopeKey,
		&row.Blocked,
		&row.AttemptCount,
		&row.NextRetryAt,
		&row.LastError,
		&row.HTTPStatus,
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
