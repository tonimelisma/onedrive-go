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
	nowNano := storedTimeNano(now)

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
		if deleteErr := deleteBlockScopeWithRunner(ctx, tx, scopeKey); deleteErr != nil {
			return false, deleteErr
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

	if deleteErr := deleteBlockScopeWithRunner(ctx, tx, scopeKey); deleteErr != nil {
		return deleteErr
	}

	if retryErr := deleteRetryWorkByScopeTx(ctx, tx, wire); retryErr != nil {
		return retryErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing discard-scope for %s: %w", wire, err)
	}

	return nil
}

// PruneBlockScopesWithoutBlockedWork deletes every block scope row that no
// blocked retry_work row references: such a scope has nothing to trial or
// release. Ready work under the scope is left alone.
func (s *SyncStore) PruneBlockScopesWithoutBlockedWork(ctx context.Context) error {
	blocks, err := s.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing block scopes for pruning: %w", err)
	}

	for _, block := range blocks {
		hasBlockedRetryWork, keepErr := s.scopeHasBlockedRetryWork(ctx, block.Key)
		if keepErr != nil {
			return fmt.Errorf("sync: pruning block scope %s: %w", block.Key.String(), keepErr)
		}
		if shouldKeepTimedBlockScope(hasBlockedRetryWork) {
			continue
		}

		if err := deleteBlockScopeWithRunner(ctx, s.db, block.Key); err != nil {
			return fmt.Errorf("sync: pruning block scope %s without blocked work: %w", block.Key.String(), err)
		}
	}

	return nil
}

// scopeHasBlockedRetryWork reports whether any retry_work row is blocked
// under scopeKey, from a count of that scope's blocked rows.
func (s *SyncStore) scopeHasBlockedRetryWork(ctx context.Context, scopeKey ScopeKey) (bool, error) {
	blocked, err := countBlockedRetryWorkForScopeTx(ctx, s.db, scopeKey.String())
	if err != nil {
		return false, err
	}

	return blocked > 0, nil
}
