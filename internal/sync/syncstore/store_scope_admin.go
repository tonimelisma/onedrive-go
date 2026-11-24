package syncstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// ReleaseScopeWork applies one ramp batch of a scope release (R-6.8.24): the
// named rows, blocked under scopeKey, become ready at now, and when no
// blocked row remains under the scope afterwards the block scope row is
// deleted in the same transaction, so a persisted scope never outlives its
// blocked work. The engine names the rows in its held order; the store has
// nothing to order blocked rows by (they all carry next_retry_at = 0).
// observation_issues remain observation-owned. It returns the rows it
// released, as they are after the release (a named row that was not blocked
// under the scope is left alone and not returned), and whether the scope row
// was removed.
func (s *SyncStore) ReleaseScopeWork(
	ctx context.Context,
	scopeKey syncdomain.ScopeKey,
	work []syncdomain.RetryWorkKey,
	now time.Time,
) (released []syncdomain.RetryWorkRow, scopeRemoved bool, err error) {
	wire := scopeKey.String()
	nowNano := syncdomain.StoredTimeNano(now)

	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return nil, false, fmt.Errorf("sync: begin release-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback release-scope tx for %s", wire))
	}()

	released, err = markRetryWorkReadyTx(ctx, tx, wire, work, nowNano)
	if err != nil {
		return nil, false, err
	}
	remaining, countErr := countBlockedRetryWorkForScopeTx(ctx, tx, wire)
	if countErr != nil {
		return nil, false, countErr
	}
	if remaining == 0 {
		if deleteErr := deleteBlockScopeWithRunner(ctx, tx, scopeKey); deleteErr != nil {
			return nil, false, deleteErr
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("sync: committing release-scope for %s: %w", wire, err)
	}

	return released, remaining == 0, nil
}

// DiscardScope atomically applies the semantic "this scope and the work
// blocked under it are no longer valid" transition, and returns the keys of
// the rows it deleted.
//
// This is used when the blocked subtree itself disappears, for example when a
// mount root disappears. Discarding differs from release: retry work blocked
// under the scope is deleted instead of made retryable. A row that names the
// scope without being blocked under it (released work, or work that recorded
// the scope as evidence) keeps its attempt count and its next retry time.
func (s *SyncStore) DiscardScope(ctx context.Context, scopeKey syncdomain.ScopeKey) (deleted []syncdomain.RetryWorkKey, err error) {
	if scopeKey.IsZero() {
		return nil, errors.New("sync: discard scope: missing scope key")
	}

	wire := scopeKey.String()

	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("sync: begin discard-scope tx for %s: %w", wire, err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback discard-scope tx for %s", wire))
	}()

	// The blocked rows go first: they reference the scope row, and the
	// database refuses to delete a scope row they still reference.
	deleted, err = deleteBlockedRetryWorkByScopeTx(ctx, tx, wire)
	if err != nil {
		return nil, err
	}
	if deleteErr := deleteBlockScopeWithRunner(ctx, tx, scopeKey); deleteErr != nil {
		return nil, deleteErr
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync: committing discard-scope for %s: %w", wire, err)
	}

	return deleted, nil
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
		hasBlockedRetryWork, keepErr := s.ScopeHasBlockedRetryWork(ctx, block.Key)
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

// ScopeHasBlockedRetryWork reports whether any retry_work row is blocked
// under scopeKey, from a count of that scope's blocked rows.
func (s *SyncStore) ScopeHasBlockedRetryWork(ctx context.Context, scopeKey syncdomain.ScopeKey) (bool, error) {
	blocked, err := countBlockedRetryWorkForScopeTx(ctx, s.db, scopeKey.String())
	if err != nil {
		return false, err
	}

	return blocked > 0, nil
}
