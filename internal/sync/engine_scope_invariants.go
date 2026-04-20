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

func (flow *engineFlow) mustAssertInvariants(ctx context.Context, watch *watchRuntime, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertCurrentInvariants(ctx, watch); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertReleasedScope(ctx context.Context, watch *watchRuntime, key ScopeKey, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertReleasedScope(context.WithoutCancel(ctx), watch, key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertDiscardedScope(ctx context.Context, watch *watchRuntime, key ScopeKey, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertDiscardedScope(context.WithoutCancel(ctx), watch, key); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertDispatchAdmissionSealed(
	watch *watchRuntime,
	outbox []*TrackedAction,
	stage string,
) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertDispatchAdmissionSealed(watch, outbox); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertPlannerSweepAllowed(
	watch *watchRuntime,
	sweep string,
	stage string,
) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertPlannerSweepAllowed(watch, sweep); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertReconcileBookkeepingCleared(watch *watchRuntime, stage string) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertReconcileBookkeepingCleared(watch); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) mustAssertObserverExitPhase(
	watch *watchRuntime,
	shuttingDown bool,
	stage string,
) {
	if !flow.invariantChecksEnabled() {
		return
	}
	if err := flow.assertObserverExitPhase(watch, shuttingDown); err != nil {
		panic(fmt.Sprintf("%s: %v", stage, err))
	}
}

func (flow *engineFlow) assertCurrentInvariants(ctx context.Context, watch *watchRuntime) error {
	if err := flow.assertWatchRuntimeInvariants(watch); err != nil {
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

func (flow *engineFlow) assertWatchRuntimeInvariants(watch *watchRuntime) error {
	if watch != nil {
		activeScopes := watch.snapshotActiveScopes()
		seen := make(map[ScopeKey]struct{}, len(activeScopes))
		for i := range activeScopes {
			key := activeScopes[i].Key
			if _, ok := seen[key]; ok {
				return fmt.Errorf("duplicate active scope key %s", key.String())
			}
			seen[key] = struct{}{}
		}
	}

	if watch != nil && watch.phase() == watchRuntimePhaseDraining {
		if watch.hasRetryTimer() {
			return fmt.Errorf("draining runtime still has retry timer armed")
		}
		if watch.hasTrialTimer() {
			return fmt.Errorf("draining runtime still has trial timer armed")
		}
	}

	return nil
}

func (flow *engineFlow) assertDispatchAdmissionSealed(
	watch *watchRuntime,
	outbox []*TrackedAction,
) error {
	if watch == nil || !watch.isDraining() || len(outbox) == 0 {
		return nil
	}

	return fmt.Errorf("draining runtime must not attempt to admit %d queued actions", len(outbox))
}

func (flow *engineFlow) assertPlannerSweepAllowed(watch *watchRuntime, sweep string) error {
	if watch == nil || !watch.isDraining() {
		return nil
	}

	return fmt.Errorf("%s must not start after drain begins", sweep)
}

func (flow *engineFlow) assertReconcileBookkeepingCleared(watch *watchRuntime) error {
	if watch == nil || !watch.reconcileActive {
		return nil
	}

	return fmt.Errorf("draining reconcile bookkeeping must be cleared before continuing")
}

func (flow *engineFlow) assertObserverExitPhase(
	watch *watchRuntime,
	shuttingDown bool,
) error {
	if watch == nil || shuttingDown || !watch.isDraining() {
		return nil
	}

	return fmt.Errorf("draining runtime must not treat observer exit as fatal outside shutdown")
}

func (flow *engineFlow) assertPersistedInvariants(ctx context.Context) error {
	blocks, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("listing block scopes: %w", err)
	}

	retryWork, err := flow.engine.baseline.ListRetryWork(ctx)
	if err != nil {
		return fmt.Errorf("listing retry_work rows: %w", err)
	}

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
	}

	for i := range retryWork {
		if !retryWork[i].Blocked {
			continue
		}
		if retryWork[i].ScopeKey.IsZero() {
			return fmt.Errorf("blocked retry_work %s is missing scope key", retryWork[i].Path)
		}
		if _, ok := seenBlocks[retryWork[i].ScopeKey]; !ok {
			return fmt.Errorf("blocked retry_work %s references missing block scope %s", retryWork[i].Path, retryWork[i].ScopeKey.String())
		}
		if retryWork[i].NextRetryAt != 0 {
			return fmt.Errorf("blocked retry_work %s must not have retry timing", retryWork[i].Path)
		}
	}

	return nil
}

func (flow *engineFlow) assertReleasedScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	if watch != nil && flow.scopeController().isBlockScopeed(watch, key) {
		return fmt.Errorf("released scope %s still active in watch state", key.String())
	}

	blocks, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("listing block scopes: %w", err)
	}
	for i := range blocks {
		if blocks[i] != nil && blocks[i].Key == key {
			return fmt.Errorf("released scope %s still persisted", key.String())
		}
	}

	retryWork, err := flow.engine.baseline.ListRetryWork(ctx)
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

func (flow *engineFlow) assertDiscardedScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	if watch != nil && flow.scopeController().isBlockScopeed(watch, key) {
		return fmt.Errorf("discarded scope %s still active in watch state", key.String())
	}

	blocks, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("listing block scopes: %w", err)
	}
	for i := range blocks {
		if blocks[i] != nil && blocks[i].Key == key {
			return fmt.Errorf("discarded scope %s still persisted", key.String())
		}
	}

	retryWork, err := flow.engine.baseline.ListRetryWork(ctx)
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
