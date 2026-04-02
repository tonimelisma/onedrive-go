package sync

import (
	"context"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (e *Engine) registerShortcuts(ctx context.Context, events []synctypes.ChangeEvent) error {
	return testEngineFlowFromEngine(e).registerShortcuts(ctx, events)
}

func (e *Engine) handleRemovedShortcuts(ctx context.Context, deletedItemIDs map[string]bool, shortcuts []synctypes.Shortcut) error {
	return testEngineFlowFromEngine(e).handleRemovedShortcuts(ctx, deletedItemIDs, shortcuts)
}

func (e *Engine) detectDriveType(ctx context.Context, remoteDriveID string) (string, string) {
	return testEngineFlowFromEngine(e).detectDriveType(ctx, remoteDriveID)
}

func (e *Engine) observeShortcutContentFromList(
	ctx context.Context,
	shortcuts []synctypes.Shortcut,
	bl *synctypes.Baseline,
	collisions map[string]bool,
) ([]synctypes.ChangeEvent, error) {
	return testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, collisions)
}

func (e *Engine) processShortcuts(
	ctx context.Context,
	remoteEvents []synctypes.ChangeEvent,
	bl *synctypes.Baseline,
	dryRun bool,
) ([]synctypes.ChangeEvent, error) {
	return testEngineFlowFromEngine(e).processShortcuts(ctx, remoteEvents, bl, dryRun)
}

func (e *Engine) reconcileShortcutScopes(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, error) {
	return testEngineFlowFromEngine(e).reconcileShortcutScopes(ctx, bl)
}

func (e *Engine) loadActiveScopes(ctx context.Context, watch *watchRuntime) error {
	return testEngineFlowFromEngine(e).loadActiveScopes(ctx, watch)
}

func (e *Engine) repairPersistedScopes(ctx context.Context, watch *watchRuntime) error {
	return testEngineFlowFromEngine(e).repairPersistedScopes(ctx, watch)
}

func (e *Engine) releaseScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	return testEngineFlowFromEngine(e).releaseScope(ctx, watch, key)
}

func (e *Engine) discardScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	return testEngineFlowFromEngine(e).discardScope(ctx, watch, key)
}

func (e *Engine) assertCurrentScopeInvariants(ctx context.Context, watch *watchRuntime) error {
	return testEngineFlowFromEngine(e).assertCurrentScopeInvariants(ctx, watch)
}

func (e *Engine) assertReleasedScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	return testEngineFlowFromEngine(e).assertReleasedScope(ctx, watch, key)
}

func (e *Engine) assertDiscardedScope(ctx context.Context, watch *watchRuntime, key synctypes.ScopeKey) error {
	return testEngineFlowFromEngine(e).assertDiscardedScope(ctx, watch, key)
}

func (e *Engine) admitReady(
	ctx context.Context,
	flow *engineFlow,
	watch *watchRuntime,
	ready []*synctypes.TrackedAction,
) []*synctypes.TrackedAction {
	return flow.admitReady(ctx, watch, ready)
}

func (e *Engine) cascadeRecordAndComplete(
	ctx context.Context,
	flow *engineFlow,
	ta *synctypes.TrackedAction,
	scopeKey synctypes.ScopeKey,
) {
	flow.cascadeRecordAndComplete(ctx, ta, scopeKey)
}

func (e *Engine) createEventFromDB(ctx context.Context, row *synctypes.SyncFailureRow) *synctypes.ChangeEvent {
	return testEngineFlowFromEngine(e).createEventFromDB(ctx, row)
}

func (e *Engine) isFailureResolved(ctx context.Context, row *synctypes.SyncFailureRow) bool {
	return testEngineFlowFromEngine(e).isFailureResolved(ctx, row)
}

func (e *Engine) clearFailureCandidate(ctx context.Context, row *synctypes.SyncFailureRow, caller string) {
	testEngineFlowFromEngine(e).clearFailureCandidate(ctx, row, caller)
}

func (e *Engine) recordRetryTrialSkippedItem(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
	skipped *synctypes.SkippedItem,
) {
	testEngineFlowFromEngine(e).recordRetryTrialSkippedItem(ctx, row, skipped)
}

func (e *Engine) activeBlockingScope(watch *watchRuntime, ta *synctypes.TrackedAction) synctypes.ScopeKey {
	return testEngineFlowFromEngine(e).activeBlockingScope(watch, ta)
}

func (e *Engine) applyScopeBlock(ctx context.Context, watch *watchRuntime, sr synctypes.ScopeUpdateResult) {
	testEngineFlowFromEngine(e).applyScopeBlock(ctx, watch, sr)
}

func (e *Engine) feedScopeDetection(ctx context.Context, watch *watchRuntime, r *synctypes.WorkerResult) {
	testEngineFlowFromEngine(e).feedScopeDetection(ctx, watch, r)
}

func (e *Engine) isObservationSuppressed(watch *watchRuntime) bool {
	return testEngineFlowFromEngine(e).isObservationSuppressed(watch)
}

func testEngineFlowFromEngine(e *Engine) *engineFlow {
	flow := newEngineFlow(e)
	return &flow
}
