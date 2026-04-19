package sync

import (
	"context"
	"fmt"
	"time"
)

const sqlPruneBlockScopesWithoutBlockedWork = `DELETE FROM block_scopes
	WHERE NOT EXISTS (
		SELECT 1 FROM retry_work
		WHERE retry_work.blocked = 1
			AND retry_work.scope_key = block_scopes.scope_key
	)`

// ReleaseScope atomically applies the semantic "this scope condition has
// resolved; blocked work may run again" transition.
//
// In one transaction it:
//   - deletes the persisted block_scopes row
//   - deletes boundary issue rows for the scope
//   - marks held transient descendants retryable immediately
//
// The actionable boundary row and the block scope are one semantic unit.
// Releasing them together prevents the split-brain state where one survives
// after the other has already been cleared.
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

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM sync_failures
			WHERE scope_key = ? AND failure_role = ?`,
		wire, FailureRoleBoundary,
	); execErr != nil {
		return fmt.Errorf("sync: deleting scope issue rows %s: %w", wire, execErr)
	}

	if _, execErr := tx.ExecContext(ctx,
		`UPDATE sync_failures
			SET failure_role = ?, next_retry_at = ?
			WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL AND category = ?`,
		FailureRoleItem, nowNano, wire, FailureRoleHeld, CategoryTransient,
	); execErr != nil {
		return fmt.Errorf("sync: unblocking failures for scope %s: %w", wire, execErr)
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
// configured root disappears. Discarding differs from release: held descendants are
// deleted instead of made retryable.
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

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM sync_failures WHERE scope_key = ?`, wire,
	); execErr != nil {
		return fmt.Errorf("sync: deleting scoped failures %s: %w", wire, execErr)
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
