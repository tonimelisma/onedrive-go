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
		(path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path, old_path, action_type) DO UPDATE SET
			scope_key = excluded.scope_key,
			blocked = excluded.blocked,
			attempt_count = excluded.attempt_count,
			next_retry_at = excluded.next_retry_at`
	sqlDeleteRetryWorkByPathType = `DELETE FROM retry_work WHERE path = ? AND old_path = ? AND action_type = ?`
	sqlSelectRetryWorkCols       = `path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at`
	sqlListRetryWork             = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		ORDER BY path, old_path, action_type`
	sqlListRetryWorkBlocked = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		WHERE blocked = 1
		ORDER BY scope_key, path, old_path, action_type`
)

func (m *SyncStore) UpsertRetryWork(ctx context.Context, row *RetryWorkRow) error {
	return upsertRetryWorkTx(ctx, m.db, row)
}

func upsertRetryWorkTx(ctx context.Context, runner sqlTxRunner, row *RetryWorkRow) error {
	if row == nil {
		return fmt.Errorf("sync: upserting retry_work: nil row")
	}

	_, err := runner.ExecContext(ctx, sqlUpsertRetryWork,
		row.Path,
		row.OldPath,
		row.ActionType.String(),
		row.ScopeKey.String(),
		row.Blocked,
		row.AttemptCount,
		row.NextRetryAt,
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
	if failure.Work.Path == "" {
		return nil, fmt.Errorf("sync: record retry_work failure: missing path")
	}
	if _, valueErr := failure.Work.ActionType.Value(); valueErr != nil {
		return nil, fmt.Errorf("sync: record retry_work failure for %s: invalid action type: %w", failure.Work.Path, valueErr)
	}
	if delayFn == nil {
		return nil, fmt.Errorf("sync: record retry_work failure for %s: retryable work requires delay function", failure.Work.Path)
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return nil, fmt.Errorf("sync: beginning retry_work failure transaction for %s: %w", failure.Work.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback retry_work failure transaction for %s", failure.Work.Path))
	}()

	existing, found, err := loadRetryWorkByWorkTx(ctx, tx, failure.Work)
	if err != nil {
		return nil, err
	}

	row = &RetryWorkRow{
		Path:         failure.Work.Path,
		OldPath:      failure.Work.OldPath,
		ActionType:   failure.Work.ActionType,
		ScopeKey:     failure.ScopeKey,
		AttemptCount: 1,
	}
	if found && existing != nil {
		row.AttemptCount = existing.AttemptCount + 1
	}
	row.NextRetryAt = m.nowFunc().Add(delayFn(row.AttemptCount - 1)).UnixNano()

	if err := upsertRetryWorkTx(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync: committing retry_work failure for %s: %w", failure.Work.Path, err)
	}

	return row, nil
}

func (m *SyncStore) RecordBlockedRetryWork(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) (row *RetryWorkRow, err error) {
	if work.Path == "" {
		return nil, fmt.Errorf("sync: record blocked retry_work: missing path")
	}
	if _, valueErr := work.ActionType.Value(); valueErr != nil {
		return nil, fmt.Errorf("sync: record blocked retry_work for %s: invalid action type: %w", work.Path, valueErr)
	}
	if scopeKey.IsZero() {
		return nil, fmt.Errorf("sync: record blocked retry_work for %s: missing scope key", work.Path)
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return nil, fmt.Errorf("sync: beginning blocked retry_work transaction for %s: %w", work.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback blocked retry_work transaction for %s", work.Path))
	}()

	existing, found, err := loadRetryWorkByWorkTx(ctx, tx, work)
	if err != nil {
		return nil, err
	}

	row = &RetryWorkRow{
		Path:         work.Path,
		OldPath:      work.OldPath,
		ActionType:   work.ActionType,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}
	if found && existing != nil {
		row.AttemptCount = existing.AttemptCount + 1
	}

	if err := upsertRetryWorkTx(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync: committing blocked retry_work for %s: %w", work.Path, err)
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

func (m *SyncStore) CountRetryingWork(ctx context.Context) (int, error) {
	var count int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM retry_work WHERE blocked = 0 AND attempt_count >= 3`,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("sync: counting retry_work rows: %w", err)
	}

	return count, nil
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
		key := rows[i].WorkKey()
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
		&row.Path,
		&row.OldPath,
		&row.ActionType,
		&scopeKey,
		&row.Blocked,
		&row.AttemptCount,
		&row.NextRetryAt,
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

func retryConditionTypeForRow(row *RetryWorkRow) string {
	if row == nil || row.ScopeKey.IsZero() {
		return ""
	}

	return row.ScopeKey.ConditionType()
}
