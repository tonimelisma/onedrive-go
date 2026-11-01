package sync

import (
	"context"
	"fmt"
	"time"
)

// ReleaseScopeWork applies one ramp batch of a scope release (R-6.8.24): the
// named rows, blocked under scopeKey, become ready at now, and when no
// blocked row remains under the scope afterwards the block scope row is
// deleted in the same transaction, so a persisted scope never outlives its
// blocked work. The engine names the rows in its held order; the store has
// nothing to order blocked rows by (they all carry next_retry_at = 0).
// observation_issues remain observation-owned. It reports whether the scope
// row was removed.
func (s *SyncStore) ReleaseScopeWork(
	ctx context.Context,
	scopeKey ScopeKey,
	work []RetryWorkKey,
	now time.Time,
) (scopeRemoved bool, err error) {
	wire := scopeKey.String()
	nowNano := now.UnixNano()

	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("sync: begin release-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback release-scope tx for %s", wire))
	}()

	if retryErr := markRetryWorkReadyTx(ctx, tx, wire, work, nowNano); retryErr != nil {
		return false, retryErr
	}
	remaining, countErr := countBlockedRetryWorkForScopeTx(ctx, tx, wire)
	if countErr != nil {
		return false, countErr
	}
	if remaining == 0 {
		if _, execErr := tx.ExecContext(ctx,
			`DELETE FROM block_scopes WHERE scope_key = ?`, wire,
		); execErr != nil {
			return false, fmt.Errorf("sync: deleting block scope %s: %w", wire, execErr)
		}
	}

	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("sync: committing release-scope for %s: %w", wire, err)
	}

	return remaining == 0, nil
}

// DiscardScope atomically applies the semantic "this scope and the work under
// it are no longer valid" transition.
//
// This is used when the blocked subtree itself disappears, for example when a
// mount root disappears. Discarding differs from release: blocked retry
// work is deleted instead of made retryable.
func (s *SyncStore) DiscardScope(ctx context.Context, scopeKey ScopeKey) (err error) {
	if scopeKey.IsZero() {
		return fmt.Errorf("sync: discard scope: missing scope key")
	}

	wire := scopeKey.String()

	tx, err := beginPerfTx(ctx, s.db)
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

func (s *SyncStore) PruneBlockScopesWithoutBlockedWork(ctx context.Context) error {
	blocks, err := s.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing block scopes for pruning: %w", err)
	}

	for i := range blocks {
		block := blocks[i]
		if block == nil {
			continue
		}

		hasBlockedRetryWork, keepErr := s.scopeHasBlockedRetryWork(ctx, block.Key)
		if keepErr != nil {
			return keepErr
		}
		if shouldKeepTimedBlockScope(hasBlockedRetryWork) {
			continue
		}

		if err := s.deleteBlockScopeOnly(ctx, block.Key); err != nil {
			return fmt.Errorf("sync: pruning block scope %s without blocked work: %w", block.Key.String(), err)
		}
	}

	return nil
}

func (s *SyncStore) deleteBlockScopeOnly(ctx context.Context, scopeKey ScopeKey) (err error) {
	wire := scopeKey.String()

	tx, err := beginPerfTx(ctx, s.db)
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

func (s *SyncStore) scopeHasBlockedRetryWork(ctx context.Context, scopeKey ScopeKey) (bool, error) {
	rows, err := s.ListBlockedRetryWork(ctx)
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
