package sync

import (
	"context"
	"fmt"
)

// invariantChecksEnabled gates expensive invariant assertions used by tests
// and debug sessions. Production keeps this disabled by default.
func (flow *engineFlow) invariantChecksEnabled() bool {
	return flow.engine.assertInvariants
}

func (flow *engineFlow) mustAssertInvariants(ctx context.Context, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertCurrentInvariants(ctx); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertReleasedScope(ctx context.Context, key ScopeKey, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertReleasedScope(context.WithoutCancel(ctx), key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertDiscardedScope(ctx context.Context, key ScopeKey, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertDiscardedScope(context.WithoutCancel(ctx), key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertDispatchAdmissionSealed(
	outbox []*trackedAction,
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

	select {
	case <-ctx.Done():
		return nil
	default:
	}

	return flow.assertPersistedInvariants(context.WithoutCancel(ctx))
}

func (flow *engineFlow) assertWatchRuntimeInvariants() error {
	{
		activeScopes := flow.scopes.snapshotActiveScopes()
		seen := make(map[ScopeKey]struct{}, len(activeScopes))
		for i := range activeScopes {
			key := activeScopes[i].Key
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate active scope key %s", key.String())
			}
			seen[key] = struct{}{}
		}
	}

	if flow.inspect.phase() == watchRuntimePhaseDraining {
		if flow.inspect.hasRetryTimer() {
			return fmt.Errorf("draining runtime still has retry timer armed")
		}
		if flow.inspect.hasTrialTimer() {
			return fmt.Errorf("draining runtime still has trial timer armed")
		}
	}

	return nil
}

func (flow *engineFlow) assertDispatchAdmissionSealed(
	outbox []*trackedAction,
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

func (flow *engineFlow) assertPersistedInvariants(ctx context.Context) error {
	blocks, err := flow.deps.store.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("listing block scopes: %w", err)
	}

	retryWork, err := flow.deps.store.ListRetryWork(ctx)
	if err != nil {
		return fmt.Errorf("listing retry_work rows: %w", err)
	}
	links := summarizePersistedScopeLinks(retryWork)

	seenBlocks := make(map[ScopeKey]struct{}, len(blocks))
	for i := range blocks {
		if blocks[i] == nil {
			return fmt.Errorf("nil persisted block scope")
		}
		if blocks[i].Key.IsZero() {
			return fmt.Errorf("persisted block scope missing key")
		}
		if _, ok := seenBlocks[blocks[i].Key]; ok {
			return fmt.Errorf("duplicate persisted block scope %s", blocks[i].Key.String())
		}
		seenBlocks[blocks[i].Key] = struct{}{}
		if !links.hasRelatedRows(blocks[i].Key) {
			return fmt.Errorf("persisted block scope %s has no related blocked retry_work rows", blocks[i].Key.String())
		}
	}

	for i := range retryWork {
		if err := assertPersistedRetryWorkInvariant(&retryWork[i], seenBlocks); err != nil {
			return err
		}
	}

	return nil
}

func assertPersistedRetryWorkInvariant(row *RetryWorkRow, seenBlocks map[ScopeKey]struct{}) error {
	if row.Path == "" {
		return fmt.Errorf("retry_work row missing path for action %s", row.ActionType.String())
	}
	if _, valueErr := row.ActionType.Value(); valueErr != nil {
		return fmt.Errorf("retry_work %s has invalid action type: %w", row.Path, valueErr)
	}
	if row.AttemptCount <= 0 {
		return fmt.Errorf("retry_work %s has invalid attempt count %d", row.Path, row.AttemptCount)
	}
	if !row.Blocked {
		return assertDelayedRetryWorkInvariant(row)
	}

	return assertBlockedRetryWorkInvariant(row, seenBlocks)
}

func assertDelayedRetryWorkInvariant(row *RetryWorkRow) error {
	if row.NextRetryAt <= 0 {
		return fmt.Errorf("retry_work %s is missing retry timing", row.Path)
	}

	return nil
}

func assertBlockedRetryWorkInvariant(row *RetryWorkRow, seenBlocks map[ScopeKey]struct{}) error {
	if row.ScopeKey.IsZero() {
		return fmt.Errorf("blocked retry_work %s is missing scope key", row.Path)
	}
	if _, ok := seenBlocks[row.ScopeKey]; !ok {
		return fmt.Errorf("blocked retry_work %s references missing block scope %s", row.Path, row.ScopeKey.String())
	}
	if row.NextRetryAt != 0 {
		return fmt.Errorf("blocked retry_work %s must not have retry timing", row.Path)
	}

	return nil
}

type persistedScopeLinks struct {
	retryCountByScope map[ScopeKey]int
}

func summarizePersistedScopeLinks(retryWork []RetryWorkRow) persistedScopeLinks {
	links := persistedScopeLinks{
		retryCountByScope: make(map[ScopeKey]int),
	}

	for i := range retryWork {
		if retryWork[i].ScopeKey.IsZero() || !retryWork[i].Blocked {
			continue
		}
		links.retryCountByScope[retryWork[i].ScopeKey]++
	}

	return links
}

func (links persistedScopeLinks) hasRelatedRows(key ScopeKey) bool {
	return links.retryCountByScope[key] > 0
}

func (flow *engineFlow) assertReleasedScope(ctx context.Context, key ScopeKey) error {
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

func (flow *engineFlow) assertDiscardedScope(ctx context.Context, key ScopeKey) error {
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
		if retryWork[i].ScopeKey == key {
			return fmt.Errorf("discarded scope %s still has retry_work %s", key.String(), retryWork[i].Path)
		}
	}

	return nil
}
