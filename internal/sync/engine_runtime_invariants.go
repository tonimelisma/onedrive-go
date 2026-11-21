package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// invariantChecksEnabled gates expensive invariant assertions used by tests
// and debug sessions. Production keeps this disabled by default.
func (flow *engineFlow) invariantChecksEnabled() bool {
	return flow.hooks.assertInvariants
}

func (flow *engineFlow) mustAssertInvariants(ctx context.Context, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertCurrentInvariants(ctx); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertReleasedScope(ctx context.Context, key syncdomain.ScopeKey, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	checkCtx, cancel := detachedInvariantCheckContext(ctx)
	defer cancel()
	if err := flow.assertReleasedScope(checkCtx, key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertDiscardedScope(ctx context.Context, key syncdomain.ScopeKey, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	checkCtx, cancel := detachedInvariantCheckContext(ctx)
	defer cancel()
	if err := flow.assertDiscardedScope(checkCtx, key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertDispatchAdmissionSealed(
	outbox []*syncdomain.TrackedAction,
	stage string,
) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertDispatchAdmissionSealed(outbox); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertHeldReleaseAllowed(
	release string,
	stage string,
) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertHeldReleaseAllowed(release); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertRefreshBookkeepingCleared(stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertRefreshBookkeepingCleared(); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertObserverExitPhase(
	shuttingDown bool,
	stage string,
) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertObserverExitPhase(shuttingDown); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) assertCurrentInvariants(ctx context.Context) error {
	if err := flow.assertWatchRuntimeInvariants(); err != nil {
		return err
	}

	if ctx == nil {
		return nil
	}

	// A canceled run, and a stopping run's completion (isStoppingRun), which
	// collapses its frontier as shutdown instead of admitting it, are not
	// checked against persisted truth.
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	if isStoppingRun(ctx) {
		return nil
	}

	checkCtx, cancel := detachedInvariantCheckContext(ctx)
	defer cancel()

	return flow.assertPersistedInvariants(checkCtx)
}

// detachedInvariantCheckContext keeps ctx's values without its cancellation,
// so an assertion is not skipped because a shutdown began, bounded like every
// detached store access (DetachedStoreContext) so a wedged store cannot hold
// it; inside a stopping run's drain it keeps the drain's one bound.
func detachedInvariantCheckContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return syncexec.DetachedStoreContext(ctx, time.Now().Add(syncdomain.OutcomeCommitTimeout))
}

func (flow *engineFlow) assertWatchRuntimeInvariants() error {
	{
		activeScopes := flow.scopes.snapshotActiveScopes()
		seen := make(map[syncdomain.ScopeKey]struct{}, len(activeScopes))
		for i := range activeScopes {
			key := activeScopes[i].Key
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate active scope key %s", key.String())
			}
			seen[key] = struct{}{}
		}
	}

	if flow.inspect.isDraining() {
		if flow.inspect.hasRetryTimer() {
			return fmt.Errorf("draining runtime still has retry timer armed")
		}
		if flow.inspect.hasTrialTimer() {
			return fmt.Errorf("draining runtime still has trial timer armed")
		}
		if flow.inspect.hasAdmitTimer() {
			return fmt.Errorf("draining runtime still has admit timer armed")
		}
	}

	return nil
}

func (flow *engineFlow) assertDispatchAdmissionSealed(
	outbox []*syncdomain.TrackedAction,
) error {
	if !flow.inspect.isDraining() || len(outbox) == 0 {
		return nil
	}

	return fmt.Errorf("draining runtime must not attempt to admit %d queued actions", len(outbox))
}

func (flow *engineFlow) assertHeldReleaseAllowed(release string) error {
	if !flow.inspect.isDraining() {
		return nil
	}

	return fmt.Errorf("%s must not start after drain begins", release)
}

func (flow *engineFlow) assertRefreshBookkeepingCleared() error {
	if !flow.inspect.hasActiveRefresh() {
		return nil
	}

	return fmt.Errorf("draining refresh bookkeeping must be cleared before continuing")
}

func (flow *engineFlow) assertObserverExitPhase(
	shuttingDown bool,
) error {
	if shuttingDown || !flow.inspect.isDraining() {
		return nil
	}

	return fmt.Errorf("draining runtime must not treat observer exit as fatal outside shutdown")
}

// assertPersistedInvariants checks what the database does not hold itself.
// The schema refuses an empty path, an attempt count below one, retry timing
// on a blocked row or none on a ready one, a blocked row without a scope,
// a blocked row under a block_scopes scope whose row is missing, and a
// duplicate or empty scope key (schema.go, generation 29); what is left is a
// known action type, the retry cache agreeing with the store, and every
// scope row having blocked work to trial.
func (flow *engineFlow) assertPersistedInvariants(ctx context.Context) error {
	blocks, err := flow.deps.store.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("listing block scopes: %w", err)
	}

	retryWork, err := flow.deps.store.ListRetryWork(ctx)
	if err != nil {
		return fmt.Errorf("listing retry_work rows: %w", err)
	}
	if cacheErr := flow.retryWork.matchesStore(retryWork); cacheErr != nil {
		return cacheErr
	}
	links := summarizePersistedScopeLinks(retryWork)

	for i := range blocks {
		if !links.hasRelatedRows(blocks[i].Key) {
			return fmt.Errorf("persisted block scope %s has no related blocked retry_work rows", blocks[i].Key.String())
		}
	}

	for i := range retryWork {
		if _, valueErr := retryWork[i].ActionType.Value(); valueErr != nil {
			return fmt.Errorf("retry_work %s has invalid action type: %w", retryWork[i].Path, valueErr)
		}
	}

	return nil
}

type persistedScopeLinks struct {
	retryCountByScope map[syncdomain.ScopeKey]int
}

func summarizePersistedScopeLinks(retryWork []syncdomain.RetryWorkRow) persistedScopeLinks {
	links := persistedScopeLinks{
		retryCountByScope: make(map[syncdomain.ScopeKey]int),
	}

	for i := range retryWork {
		if retryWork[i].ScopeKey.IsZero() || !retryWork[i].Blocked {
			continue
		}
		links.retryCountByScope[retryWork[i].ScopeKey]++
	}

	return links
}

func (links persistedScopeLinks) hasRelatedRows(key syncdomain.ScopeKey) bool {
	return links.retryCountByScope[key] > 0
}

func (flow *engineFlow) assertReleasedScope(ctx context.Context, key syncdomain.ScopeKey) error {
	if flow.scopes.hasActiveScope(key) {
		return fmt.Errorf("released scope %s still active in runtime state", key.String())
	}

	blocks, err := flow.deps.store.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("listing block scopes: %w", err)
	}
	for i := range blocks {
		if blocks[i] != nil && blocks[i].Key == key {
			return fmt.Errorf("released scope %s still persisted", key.String())
		}
	}

	retryWork, err := flow.deps.store.ListRetryWork(ctx)
	if err != nil {
		return fmt.Errorf("listing retry_work rows: %w", err)
	}
	for i := range retryWork {
		if retryWork[i].ScopeKey == key && retryWork[i].Blocked {
			return fmt.Errorf("released scope %s still has blocked retry_work %s", key.String(), retryWork[i].Path)
		}
	}

	return nil
}

func (flow *engineFlow) assertDiscardedScope(ctx context.Context, key syncdomain.ScopeKey) error {
	if flow.scopes.hasActiveScope(key) {
		return fmt.Errorf("discarded scope %s still active in runtime state", key.String())
	}

	blocks, err := flow.deps.store.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("listing block scopes: %w", err)
	}
	for i := range blocks {
		if blocks[i] != nil && blocks[i].Key == key {
			return fmt.Errorf("discarded scope %s still persisted", key.String())
		}
	}

	retryWork, err := flow.deps.store.ListRetryWork(ctx)
	if err != nil {
		return fmt.Errorf("listing retry_work rows: %w", err)
	}
	for i := range retryWork {
		if retryWork[i].Blocked && retryWork[i].ScopeKey == key {
			return fmt.Errorf("discarded scope %s still has blocked retry_work %s", key.String(), retryWork[i].Path)
		}
	}

	return nil
}
