package sync

import (
	"context"
	"fmt"
	"time"
)

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
// mount root disappears. Discarding differs from release: blocked retry
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
	blocks, err := m.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing block scopes for pruning: %w", err)
	}

	for i := range blocks {
		block := blocks[i]
		if block == nil {
			continue
		}

		hasBlockedRetryWork, keepErr := m.scopeHasBlockedRetryWork(ctx, block.Key)
		if keepErr != nil {
			return keepErr
		}
		if shouldKeepTimedBlockScope(hasBlockedRetryWork) {
			continue
		}

		if err := m.deleteBlockScopeOnly(ctx, block.Key); err != nil {
			return fmt.Errorf("sync: pruning block scope %s without blocked work: %w", block.Key.String(), err)
		}
	}

	return nil
}

func (m *SyncStore) deleteBlockScopeOnly(ctx context.Context, scopeKey ScopeKey) (err error) {
	wire := scopeKey.String()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin delete-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback delete-scope tx for %s", wire))
	}()

	if _, execErr := tx.ExecContext(ctx,
		`DELETE FROM block_scopes WHERE scope_key = ?`, wire,
	); execErr != nil {
		return fmt.Errorf("sync: deleting block scope %s: %w", wire, execErr)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing delete-scope for %s: %w", wire, err)
	}

	return nil
}

func (m *SyncStore) scopeHasBlockedRetryWork(ctx context.Context, scopeKey ScopeKey) (bool, error) {
	rows, err := m.ListBlockedRetryWork(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: listing blocked retry work for scope pruning: %w", err)
	}

	for i := range rows {
		if rows[i].ScopeKey == scopeKey {
			return true, nil
		}
	}

	return false, nil
}
