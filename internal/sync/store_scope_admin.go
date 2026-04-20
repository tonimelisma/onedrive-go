package sync

import (
	"context"
	"fmt"
	"time"
)

const sqlPruneBlockScopesWithoutBlockedWork = `DELETE FROM block_scopes
	WHERE timing_source <> 'none'
		AND NOT EXISTS (
		SELECT 1 FROM retry_work
		WHERE retry_work.blocked = 1
			AND retry_work.scope_key = block_scopes.scope_key
	)`

// ReleaseScope atomically applies the semantic "this scope condition has
// resolved; blocked work may run again" transition.
//
// In one transaction it deletes the persisted block scope and marks blocked
// retry work ready immediately. observation_issues remain observation-owned.
func (m *SyncStore) ReleaseScope(
	ctx context.Context,
	scopeKey ScopeKey,
	now time.Time,
) (err error) {
	wire := scopeKey.String()
	nowNano := now.UnixNano()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin release-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback release-scope tx for %s", wire))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM block_scopes WHERE scope_key = ?`, wire,
	); execErr != nil {
		return fmt.Errorf("sync: deleting block scope %s: %w", wire, execErr)
	}

	if retryErr := markRetryWorkScopeReadyTx(ctx, tx, wire, nowNano); retryErr != nil {
		return retryErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing release-scope for %s: %w", wire, err)
	}

	return nil
}

// DiscardScope atomically applies the semantic "this scope and the work under
// it are no longer valid" transition.
//
// This is used when the blocked subtree itself disappears, for example when a
// configured root disappears. Discarding differs from release: blocked retry
// work is deleted instead of made retryable.
func (m *SyncStore) DiscardScope(ctx context.Context, scopeKey ScopeKey) (err error) {
	if scopeKey.IsZero() {
		return fmt.Errorf("sync: discard scope: missing scope key")
	}

	wire := scopeKey.String()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin discard-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback discard-scope tx for %s", wire))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM block_scopes WHERE scope_key = ?`, wire,
	); execErr != nil {
		return fmt.Errorf("sync: deleting block scope %s: %w", wire, execErr)
	}

	if retryErr := deleteRetryWorkByScopeTx(ctx, tx, wire); retryErr != nil {
		return retryErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing discard-scope for %s: %w", wire, err)
	}

	return nil
}

func (m *SyncStore) PruneBlockScopesWithoutBlockedWork(ctx context.Context) error {
	if _, err := m.db.ExecContext(ctx, sqlPruneBlockScopesWithoutBlockedWork); err != nil {
		return fmt.Errorf("sync: pruning block scopes without blocked work: %w", err)
	}

	return nil
}
