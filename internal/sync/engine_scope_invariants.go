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
	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}

	rows, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("listing sync failures: %w", err)
	}
	facts := summarizePersistedScopeFailures(rows)
	boundaryKeys := make(map[ScopeKey]struct{})

	for i := range rows {
		if err := validateFailureRowState(&rows[i]); err != nil {
			return err
		}
		if rows[i].Role != FailureRoleBoundary {
			continue
		}
		if _, ok := boundaryKeys[rows[i].ScopeKey]; ok {
			return fmt.Errorf("duplicate boundary row for scope %s", rows[i].ScopeKey.String())
		}
		boundaryKeys[rows[i].ScopeKey] = struct{}{}
	}

	for i := range blocks {
		key := blocks[i].Key
		if key.IsPermRemote() {
			return fmt.Errorf("legacy persisted perm:remote scope %s should have been normalized away", key.String())
		}
		if !key.IsPermDir() {
			continue
		}
		if !facts.boundaryKeys[key] {
			return fmt.Errorf("permission scope %s has no actionable boundary row", key.String())
		}
	}

	return nil
}

func (flow *engineFlow) assertReleasedScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	if watch != nil && flow.scopeController().isScopeBlocked(watch, key) {
		return fmt.Errorf("released scope %s still active in watch state", key.String())
	}

	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return fmt.Errorf("released scope %s still persisted", key.String())
		}
	}

	rows, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("listing sync failures: %w", err)
	}
	for i := range rows {
		if rows[i].ScopeKey != key {
			continue
		}
		if rows[i].Role == FailureRoleBoundary {
			return fmt.Errorf("released scope %s still has actionable boundary row %s", key.String(), rows[i].Path)
		}
		if rows[i].Role == FailureRoleHeld {
			return fmt.Errorf("released scope %s still has held transient row %s", key.String(), rows[i].Path)
		}
		if rows[i].Role == FailureRoleItem &&
			rows[i].Category == CategoryTransient &&
			rows[i].NextRetryAt <= 0 {
			return fmt.Errorf("released scope %s still has non-retryable transient row %s", key.String(), rows[i].Path)
		}
	}

	return nil
}

func (flow *engineFlow) assertDiscardedScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	if watch != nil && flow.scopeController().isScopeBlocked(watch, key) {
		return fmt.Errorf("discarded scope %s still active in watch state", key.String())
	}

	blocks, err := flow.engine.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return fmt.Errorf("listing scope blocks: %w", err)
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return fmt.Errorf("discarded scope %s still persisted", key.String())
		}
	}

	rows, err := flow.engine.baseline.ListSyncFailures(ctx)
	if err != nil {
		return fmt.Errorf("listing sync failures: %w", err)
	}
	for i := range rows {
		if rows[i].ScopeKey == key {
			return fmt.Errorf("discarded scope %s still has failure row %s", key.String(), rows[i].Path)
		}
	}

	return nil
}
