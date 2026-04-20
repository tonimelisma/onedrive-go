package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// externalDBChanged checks whether another process (e.g., the CLI) wrote to
// the database since the last check. Uses PRAGMA data_version — changes every
// time another connection commits a write. The engine's own writes don't
// change it. Returns true if the version advanced.
func (rt *watchRuntime) externalDBChanged(ctx context.Context) bool {
	dv, err := rt.engine.baseline.DataVersion(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to check data_version",
			slog.String("error", err.Error()),
		)

		return false
	}

	if dv == rt.lastDataVersion {
		return false
	}

	rt.lastDataVersion = dv

	return true
}

// handleRecheckTick processes a recheck timer tick: detects external DB
// changes and logs a watch summary.
func (rt *watchRuntime) handleRecheckTick(ctx context.Context) {
	if rt.externalDBChanged(ctx) {
		rt.handleExternalChanges(ctx)
	}

	rt.logWatchSummary(ctx)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRecheckTickHandled})
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version.
func (rt *watchRuntime) handleExternalChanges(ctx context.Context) {
	rt.clearResolvedPermissionScopes(ctx)
	rt.mustAssertInvariants(ctx, rt, "handle external changes")
}

// clearResolvedPermissionScopes checks whether persisted permission scope
// authorities still exist and releases any runtime scope whose backing
// block_scopes / blocked retry_work rows disappeared externally.
func (rt *watchRuntime) clearResolvedPermissionScopes(ctx context.Context) {
	scopeKeys := rt.scopeController().blockScopeKeys(rt)
	if len(scopeKeys) == 0 {
		return
	}

	blocks, err := rt.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		rt.engine.logger.Warn("failed to check persisted block scopes",
			slog.String("error", err.Error()),
		)

		return
	}

	activeScopes := make(map[ScopeKey]bool, len(blocks))
	for i := range blocks {
		if blocks[i] != nil && (blocks[i].Key.IsPermDir() || blocks[i].Key.IsPermRemote()) {
			activeScopes[blocks[i].Key] = true
		}
	}

	for _, key := range scopeKeys {
		if (key.IsPermDir() || key.IsPermRemote()) && !activeScopes[key] {
			if err := rt.scopeController().releaseScope(ctx, rt, key); err != nil {
				rt.engine.logger.Warn("failed to release externally-cleared permission scope",
					slog.String("scope", key.String()),
					slog.String("error", err.Error()),
				)
				continue
			}

			rt.engine.logger.Info("permission block scope cleared by user",
				slog.String("scope", key.String()),
			)
		}
	}
}

func (e *Engine) fullRemoteReconcileDelay(ctx context.Context) (time.Duration, error) {
	state, err := e.baseline.ReadObservationState(ctx)
	if err != nil {
		return 0, fmt.Errorf("sync: reading observation state for reconcile cadence: %w", err)
	}
	if state.NextFullRemoteRefreshAt == 0 {
		if state.LastFullRemoteRefreshAt == 0 {
			return 0, nil
		}
		state.NextFullRemoteRefreshAt = time.Unix(0, state.LastFullRemoteRefreshAt).
			Add(remoteRefreshIntervalForMode(state.RemoteRefreshMode)).
			UnixNano()
	}

	dueAt := time.Unix(0, state.NextFullRemoteRefreshAt)
	delay := dueAt.Sub(e.nowFunc())
	if delay < 0 {
		return 0, nil
	}

	return delay, nil
}

func (e *Engine) shouldRunFullRemoteReconcile(ctx context.Context, requested bool) (bool, error) {
	if requested {
		return true, nil
	}

	state, err := e.baseline.ReadObservationState(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: reading observation state for full reconcile: %w", err)
	}
	if state.Cursor == "" || state.NextFullRemoteRefreshAt == 0 {
		return true, nil
	}

	dueAt := time.Unix(0, state.NextFullRemoteRefreshAt)
	return !e.nowFunc().Before(dueAt), nil
}

func (rt *watchRuntime) armFullReconcileTimer(ctx context.Context) error {
	delay, err := rt.engine.fullRemoteReconcileDelay(ctx)
	if err != nil {
		return err
	}
	state, err := rt.engine.baseline.ReadObservationState(ctx)
	if err != nil {
		return fmt.Errorf("sync: reading observation state for reconcile timer: %w", err)
	}
	interval := remoteRefreshIntervalForMode(state.RemoteRefreshMode)

	rt.resetReconcileTimer(rt.engine.afterFunc(delay, func() {
		select {
		case rt.reconcileCh <- rt.engine.nowFunc():
		default:
		}
	}))

	rt.engine.logger.Info("full remote reconciliation armed",
		slog.Duration("delay", delay),
		slog.Duration("interval", interval),
	)

	return nil
}

// runFullReconciliationAsync spawns a goroutine for full delta enumeration +
// orphan detection. Non-blocking — the watch loop continues processing events
// while reconciliation runs. The goroutine sends a reconcileResult back to the
// watch loop, and the loop feeds the returned events into its buffer from its
// own goroutine.
func (rt *watchRuntime) runFullReconciliationAsync(ctx context.Context, bl *Baseline) {
	if rt.reconcileActive {
		rt.engine.logger.Info("full reconciliation skipped — previous still running")
		return
	}
	rt.reconcileActive = true
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileStarted})

	go func() {
		rt.finishFullReconciliation(ctx, rt.performFullReconciliation(ctx, bl))
	}()
}

func (rt *watchRuntime) performFullReconciliation(
	ctx context.Context,
	bl *Baseline,
) reconcileResult {
	result := reconcileResult{}
	start := rt.engine.nowFunc()
	defer func() {
		rt.engine.collector().RecordReconcile(len(result.events), rt.engine.since(start))
	}()

	rt.engine.logger.Info("periodic full reconciliation starting")

	plan := rt.buildPrimaryRootObservationPlan(true)
	projectedPrimary, err := rt.observeCommittedFullReconciliationBatch(ctx, bl, plan)
	if err != nil {
		if ctx.Err() == nil {
			rt.engine.logger.Error("full reconciliation failed",
				slog.String("error", err.Error()),
			)
		}
		return result
	}

	if ctx.Err() != nil {
		rt.engine.logger.Info("full reconciliation: observations committed, stopping for shutdown")
		return result
	}

	events := rt.processCommittedPrimaryBatch(
		ctx,
		bl,
		projectedPrimary.emitted,
		false,
		true,
	)
	if len(events) == 0 {
		rt.engine.logger.Info("periodic full reconciliation complete: no changes",
			slog.Duration("duration", rt.engine.since(start)),
		)
		return result
	}

	result.events = events

	rt.engine.logger.Info("periodic full reconciliation complete",
		slog.Int("events", len(events)),
		slog.Duration("duration", rt.engine.since(start)),
	)

	return result
}

func (rt *watchRuntime) observeCommittedFullReconciliationBatch(
	ctx context.Context,
	bl *Baseline,
	plan primaryRootObservationPlan,
) (remoteObservationResult, error) {
	fetchResult, err := rt.executePrimaryRootObservation(ctx, bl, plan)
	if err != nil {
		return remoteObservationResult{}, err
	}

	projectedPrimary := projectRemoteObservations(rt.engine.logger, fetchResult.events)
	if commitErr := rt.commitObservedItems(ctx, projectedPrimary.observed, ""); commitErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full reconciliation observations: %w", commitErr)
	}
	if tokenErr := rt.commitPendingPrimaryCursor(ctx, fetchResult.pending); tokenErr != nil {
		return remoteObservationResult{}, fmt.Errorf("commit full reconciliation primary cursor: %w", tokenErr)
	}
	if armErr := rt.armFullReconcileTimer(ctx); armErr != nil {
		return remoteObservationResult{}, fmt.Errorf("arm full reconciliation timer: %w", armErr)
	}

	if rt.afterReconcileCommit != nil {
		rt.afterReconcileCommit()
	}

	return projectedPrimary, nil
}

func (rt *watchRuntime) finishFullReconciliation(ctx context.Context, result reconcileResult) {
	select {
	case rt.reconcileResults <- result:
	case <-ctx.Done():
		select {
		case rt.reconcileResults <- result:
		default:
		}
	}
}

func (rt *watchRuntime) applyReconcileResult(result reconcileResult) {
	rt.reconcileActive = false

	if rt.dirtyBuf != nil {
		if len(result.events) == 0 {
			rt.dirtyBuf.MarkFullRefresh()
		}
		for i := range result.events {
			if result.events[i].Path != "" {
				rt.dirtyBuf.MarkPath(result.events[i].Path)
			}
			if result.events[i].OldPath != "" {
				rt.dirtyBuf.MarkPath(result.events[i].OldPath)
			}
		}
	}

	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileApplied})
}

func (rt *watchRuntime) dropReconcileResultOnShutdown() {
	rt.reconcileActive = false
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReconcileDroppedOnShutdown})
}
