package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const (
	sqlUpsertRetryWork = `INSERT INTO retry_work
		(path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, condition_type, blocked_scope_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path, old_path, action_type) DO UPDATE SET
			scope_key = excluded.scope_key,
			blocked = excluded.blocked,
			attempt_count = excluded.attempt_count,
			next_retry_at = excluded.next_retry_at,
			condition_type = excluded.condition_type,
			blocked_scope_key = excluded.blocked_scope_key`
	sqlDeleteRetryWorkByPathType = `DELETE FROM retry_work WHERE path = ? AND old_path = ? AND action_type = ?`
	sqlSelectRetryWorkCols       = `path, old_path, action_type, scope_key, blocked, attempt_count, next_retry_at, condition_type`
	sqlListRetryWork             = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		ORDER BY path, old_path, action_type`
	sqlListRetryWorkBlocked = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		WHERE blocked = 1
		ORDER BY scope_key, path, old_path, action_type`
	// sqlListRetryWorkRetrying is the one definition of what status counts as
	// "Retrying": timed retries of transient failures from the third attempt.
	// The count is the rows it lists (DriveStatusSnapshot.RetryingItems). Work
	// a user-clearable condition refused is listed under that condition
	// instead (sqlListRetryWorkActionable).
	sqlListRetryWorkRetrying = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		WHERE blocked = 0 AND attempt_count >= 3 AND condition_type = ''
		ORDER BY path, old_path, action_type`
	// sqlListRetryWorkActionable lists the work a condition only the user can
	// clear refused, from its first attempt: status shows it under that
	// condition's issue the way it shows the scanner's own findings.
	sqlListRetryWorkActionable = `SELECT ` + sqlSelectRetryWorkCols + `
		FROM retry_work
		WHERE blocked = 0 AND condition_type <> ''
		ORDER BY condition_type, path, old_path, action_type`
)

func (s *SyncStore) UpsertRetryWork(ctx context.Context, row *syncdomain.RetryWorkRow) error {
	return upsertRetryWorkTx(ctx, s.db, row)
}

func upsertRetryWorkTx(ctx context.Context, runner SqlTxRunner, row *syncdomain.RetryWorkRow) error {
	if row == nil {
		return fmt.Errorf("sync: upserting retry_work: nil row")
	}
	// Never write what scanRetryWorkRow would refuse to read.
	if row.ConditionType != "" && !row.ConditionType.IsMember() {
		return fmt.Errorf("sync: upserting retry_work for %s: unknown condition type %q", row.Path, row.ConditionType)
	}

	_, err := runner.ExecContext(ctx, sqlUpsertRetryWork,
		row.Path,
		row.OldPath,
		row.ActionType.String(),
		row.ScopeKey.String(),
		row.Blocked,
		row.AttemptCount,
		row.NextRetryAt,
		string(row.ConditionType),
		blockedScopeKeyColumn(row),
	)
	if err != nil {
		return fmt.Errorf("sync: upserting retry_work for %s: %w", row.Path, err)
	}

	return nil
}

// blockedScopeKeyColumn is the value of retry_work.blocked_scope_key for row:
// its scope when it is blocked under a scope kept in block_scopes, which the
// key then references, and NULL otherwise. A row blocked under a read
// boundary has no block_scopes row to reference (observation owns that
// boundary), so it stays blocked with no key.
func blockedScopeKeyColumn(row *syncdomain.RetryWorkRow) sql.NullString {
	if !row.Blocked || !row.ScopeKey.PersistsInBlockScopes() {
		return sql.NullString{}
	}

	return sql.NullString{String: row.ScopeKey.String(), Valid: true}
}

func deleteRetryWorkByWorkTx(ctx context.Context, runner SqlTxRunner, work syncdomain.RetryWorkKey) error {
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

// deleteBlockedRetryWorkByScopeTx deletes the rows blocked under scopeKey
// and returns their keys. It reads only those rows, so a row stored under
// another scope never takes part.
func deleteBlockedRetryWorkByScopeTx(ctx context.Context, runner SqlTxRunner, scopeKey string) ([]syncdomain.RetryWorkKey, error) {
	rows, err := runner.QueryContext(ctx, `SELECT `+sqlSelectRetryWorkCols+`
		FROM retry_work
		WHERE scope_key = ? AND blocked = 1
		ORDER BY path, old_path, action_type`, scopeKey)
	if err != nil {
		return nil, fmt.Errorf("sync: listing blocked retry_work for scope %s: %w", scopeKey, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err
	blocked, err := scanRetryWorkRows(rows)
	if err != nil {
		return nil, fmt.Errorf("sync: reading blocked retry_work for scope %s: %w", scopeKey, err)
	}
	deleted := make([]syncdomain.RetryWorkKey, 0, len(blocked))
	for i := range blocked {
		deleted = append(deleted, blocked[i].WorkKey())
	}
	if _, err := runner.ExecContext(ctx,
		`DELETE FROM retry_work WHERE scope_key = ? AND blocked = 1`, scopeKey,
	); err != nil {
		return nil, fmt.Errorf("sync: deleting blocked retry_work for scope %s: %w", scopeKey, err)
	}

	return deleted, nil
}

// markRetryWorkReadyTx makes the named rows, blocked under scopeKey, ready
// at nowNano: one ramp batch of a scope release (R-6.8.24). A key that is not
// blocked under the scope any more is left alone. It returns the rows it
// made ready, as they are afterwards.
func markRetryWorkReadyTx(
	ctx context.Context,
	runner SqlTxRunner,
	scopeKey string,
	work []syncdomain.RetryWorkKey,
	nowNano int64,
) ([]syncdomain.RetryWorkRow, error) {
	var released []syncdomain.RetryWorkRow
	for i := range work {
		result, err := runner.ExecContext(ctx,
			`UPDATE retry_work
			SET blocked = 0, next_retry_at = ?, blocked_scope_key = NULL
			WHERE path = ? AND old_path = ? AND action_type = ? AND scope_key = ? AND blocked = 1`,
			nowNano, work[i].Path, work[i].OldPath, work[i].ActionType.String(), scopeKey,
		)
		if err != nil {
			return nil, fmt.Errorf("sync: setting retry_work ready for %s under %s: %w", work[i].Path, scopeKey, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("sync: counting retry_work set ready for %s: %w", work[i].Path, err)
		}
		if changed == 0 {
			continue
		}
		row, found, err := loadRetryWorkByWorkTx(ctx, runner, work[i])
		if err != nil {
			return nil, err
		}
		if found {
			released = append(released, *row)
		}
	}

	return released, nil
}

// ReleaseOrphanBlockedRetryWork makes every blocked row that references no
// block_scopes row ready at now, and returns how many it released. Such a
// row has nothing that will trial or release it (R-2.10.33). A row blocked
// under a scope kept in block_scopes references its scope row through
// blocked_scope_key, which the database holds to an existing row, so only
// the rows blocked under a read boundary are left for this to release; the
// startup normalization calls it after it has discarded empty scopes.
func (s *SyncStore) ReleaseOrphanBlockedRetryWork(ctx context.Context, now time.Time) (int, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE retry_work
		SET blocked = 0, next_retry_at = ?
		WHERE blocked = 1 AND blocked_scope_key IS NULL`,
		syncdomain.StoredTimeNano(now),
	)
	if err != nil {
		return 0, fmt.Errorf("sync: releasing blocked retry_work without a scope: %w", err)
	}
	released, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: counting released retry_work: %w", err)
	}

	return int(released), nil
}

func countBlockedRetryWorkForScopeTx(ctx context.Context, runner SqlTxRunner, scopeKey string) (int, error) {
	var count int
	if err := runner.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM retry_work WHERE scope_key = ? AND blocked = 1`, scopeKey,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("sync: counting blocked retry_work for %s: %w", scopeKey, err)
	}

	return count, nil
}

func (s *SyncStore) DeleteRetryWorkByWork(ctx context.Context, work syncdomain.RetryWorkKey) error {
	return deleteRetryWorkByWorkTx(ctx, s.db, work)
}

func (s *SyncStore) RecordRetryWorkFailure(
	ctx context.Context,
	failure *syncdomain.RetryWorkFailure,
	delayFn func(int) time.Duration,
) (row *syncdomain.RetryWorkRow, err error) {
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

	tx, err := BeginPerfTx(ctx, s.db)
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

	row = &syncdomain.RetryWorkRow{
		Path:          failure.Work.Path,
		OldPath:       failure.Work.OldPath,
		ActionType:    failure.Work.ActionType,
		ScopeKey:      failure.ScopeKey,
		AttemptCount:  1,
		ConditionType: failure.ConditionType,
	}
	if found && existing != nil {
		row.AttemptCount = existing.AttemptCount + 1
	}
	row.NextRetryAt = syncdomain.StoredTimeNano(s.nowFunc().Add(delayFn(row.AttemptCount - 1)))

	if err := upsertRetryWorkTx(ctx, tx, row); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync: committing retry_work failure for %s: %w", failure.Work.Path, err)
	}

	return row, nil
}

// RecordBlockedRetryWork records work blocked under scopeKey, one more
// attempt than its row had. When activates is not nil it is the block scope
// row the failure activates (or re-activates), written in the same
// transaction before the blocked row that references it, so neither is ever
// stored without the other. When it is nil, a scope kept in block_scopes must
// already have its row: the database refuses a blocked row that references
// a missing one.
func (s *SyncStore) RecordBlockedRetryWork(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	scopeKey syncdomain.ScopeKey,
	activates *syncdomain.BlockScope,
) (row *syncdomain.RetryWorkRow, err error) {
	if work.Path == "" {
		return nil, fmt.Errorf("sync: record blocked retry_work: missing path")
	}
	if _, valueErr := work.ActionType.Value(); valueErr != nil {
		return nil, fmt.Errorf("sync: record blocked retry_work for %s: invalid action type: %w", work.Path, valueErr)
	}
	if scopeKey.IsZero() {
		return nil, fmt.Errorf("sync: record blocked retry_work for %s: missing scope key", work.Path)
	}
	if activates != nil && activates.Key != scopeKey {
		return nil, fmt.Errorf("sync: record blocked retry_work for %s under %s: activates scope %s",
			work.Path, scopeKey.String(), activates.Key.String())
	}

	tx, err := BeginPerfTx(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("sync: beginning blocked retry_work transaction for %s: %w", work.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback blocked retry_work transaction for %s", work.Path))
	}()

	if activates != nil {
		if scopeErr := upsertBlockScopeWithRunner(ctx, tx, activates); scopeErr != nil {
			return nil, scopeErr
		}
	}

	existing, found, err := loadRetryWorkByWorkTx(ctx, tx, work)
	if err != nil {
		return nil, err
	}

	row = &syncdomain.RetryWorkRow{
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
	runner SqlTxRunner,
	work syncdomain.RetryWorkKey,
) (*syncdomain.RetryWorkRow, bool, error) {
	row := &syncdomain.RetryWorkRow{}
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

func (s *SyncStore) ResolveRetryWork(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
) (row *syncdomain.RetryWorkRow, found bool, err error) {
	tx, err := BeginPerfTx(ctx, s.db)
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

// ClearBlockedRetryWork deletes work's row when it is blocked under
// scopeKey, and reports whether it deleted one.
func (s *SyncStore) ClearBlockedRetryWork(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	scopeKey syncdomain.ScopeKey,
) (cleared bool, err error) {
	tx, err := BeginPerfTx(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("sync: begin clear blocked retry_work tx for %s: %w", work.Path, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback clear blocked retry_work tx for %s", work.Path))
	}()

	result, execErr := tx.ExecContext(ctx,
		`DELETE FROM retry_work
			WHERE path = ? AND old_path = ? AND action_type = ? AND scope_key = ? AND blocked = 1`,
		work.Path,
		work.OldPath,
		work.ActionType.String(),
		scopeKey.String(),
	)
	if execErr != nil {
		return false, fmt.Errorf("sync: deleting blocked retry_work for %s: %w", work.Path, execErr)
	}
	deleted, countErr := result.RowsAffected()
	if countErr != nil {
		return false, fmt.Errorf("sync: counting deleted blocked retry_work for %s: %w", work.Path, countErr)
	}

	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("sync: commit clear blocked retry_work for %s: %w", work.Path, err)
	}

	return deleted > 0, nil
}

func (s *SyncStore) ListRetryWork(ctx context.Context) ([]syncdomain.RetryWorkRow, error) {
	return listRetryWorkWithRunner(ctx, s.db)
}

func listRetryWorkWithRunner(ctx context.Context, runner SqlTxRunner) ([]syncdomain.RetryWorkRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListRetryWork)
	if err != nil {
		return nil, fmt.Errorf("sync: querying retry_work: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	return scanRetryWorkRows(rows)
}

func queryBlockedRetryWorkRowsWithRunner(
	ctx context.Context,
	runner SqlTxRunner,
) ([]syncdomain.RetryWorkRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListRetryWorkBlocked)
	if err != nil {
		return nil, fmt.Errorf("query blocked retry_work rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	return scanRetryWorkRows(rows)
}

// queryActionableRetryWorkRowsWithRunner lists the work a condition only the
// user can clear refused (R-2.10.71), for status to show under that condition.
func queryActionableRetryWorkRowsWithRunner(
	ctx context.Context,
	runner SqlTxRunner,
) ([]syncdomain.RetryWorkRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListRetryWorkActionable)
	if err != nil {
		return nil, fmt.Errorf("query actionable retry_work rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	return scanRetryWorkRows(rows)
}

func queryRetryingRetryWorkRowsWithRunner(
	ctx context.Context,
	runner SqlTxRunner,
) ([]syncdomain.RetryWorkRow, error) {
	rows, err := runner.QueryContext(ctx, sqlListRetryWorkRetrying)
	if err != nil {
		return nil, fmt.Errorf("query retrying retry_work rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	return scanRetryWorkRows(rows)
}

func (s *SyncStore) ListBlockedRetryWork(ctx context.Context) ([]syncdomain.RetryWorkRow, error) {
	rows, err := s.db.QueryContext(ctx, sqlListRetryWorkBlocked)
	if err != nil {
		return nil, fmt.Errorf("sync: querying blocked retry_work rows: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	return scanRetryWorkRows(rows)
}

// PruneRetryWorkToCurrentActions deletes every retry_work row whose work is
// not in work, in one transaction: a failure part way leaves every row.
func (s *SyncStore) PruneRetryWorkToCurrentActions(
	ctx context.Context,
	work []syncdomain.RetryWorkKey,
) (err error) {
	keep := make(map[syncdomain.RetryWorkKey]struct{}, len(work))
	for i := range work {
		keep[work[i]] = struct{}{}
	}

	tx, err := BeginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning retry_work prune: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback retry_work prune")
	}()

	rows, err := listRetryWorkWithRunner(ctx, tx)
	if err != nil {
		return err
	}
	for i := range rows {
		key := rows[i].WorkKey()
		if _, ok := keep[key]; ok {
			continue
		}
		if deleteErr := deleteRetryWorkByWorkTx(ctx, tx, key); deleteErr != nil {
			return deleteErr
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing retry_work prune: %w", err)
	}

	return nil
}

type retryWorkScanner interface {
	Scan(dest ...any) error
}

func scanRetryWorkRow(scanner retryWorkScanner, row *syncdomain.RetryWorkRow) error {
	if row == nil {
		return fmt.Errorf("sync: scanning retry_work row: nil destination")
	}

	var scopeKey, conditionType string
	if err := scanner.Scan(
		&row.Path,
		&row.OldPath,
		&row.ActionType,
		&scopeKey,
		&row.Blocked,
		&row.AttemptCount,
		&row.NextRetryAt,
		&conditionType,
	); err != nil {
		return fmt.Errorf("sync: scanning retry_work row: %w", err)
	}

	row.ScopeKey = syncdomain.ParseScopeKey(scopeKey)
	// An empty condition_type is a transient failure; any other value must be
	// a member (ParseIssueType).
	if conditionType != "" {
		parsed, err := syncdomain.ParseIssueType(conditionType)
		if err != nil {
			return fmt.Errorf("sync: scanning retry_work row %s: %w", row.Path, err)
		}
		row.ConditionType = parsed
	}

	return nil
}

func scanRetryWorkRows(rows *sql.Rows) ([]syncdomain.RetryWorkRow, error) {
	var result []syncdomain.RetryWorkRow
	for rows.Next() {
		var row syncdomain.RetryWorkRow
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

func RetryConditionTypeForRow(row *syncdomain.RetryWorkRow) syncdomain.IssueType {
	if row == nil || row.ScopeKey.IsZero() {
		return ""
	}

	return row.ScopeKey.ConditionType()
}
