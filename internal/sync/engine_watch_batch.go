package sync

import (
	"context"
	"log/slog"
	"time"
)

// processDirtyBatch refreshes current truth and replans from SQLite-owned
// comparison/reconciliation state. Dirty batches are scheduler hints only;
// the actionable set comes from snapshots plus baseline, never from the batch
// payload itself.
func (rt *watchRuntime) processDirtyBatch(
	ctx context.Context,
	batch DirtyBatch,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) []*TrackedAction {
	rt.engine.logger.Info("processing watch dirty batch",
		slog.Int("paths", len(batch.Paths)),
		slog.Bool("full_refresh", batch.FullRefresh),
	)
	rt.engine.collector().RecordWatchBatch(len(batch.Paths))

	rt.periodicPermRecheck(ctx, bl)

	observeStart := rt.engine.nowFunc()
	localResult, err := rt.observeLocalChanges(ctx, rt, bl)
	if err != nil {
		rt.engine.logger.Error("watch local refresh failed, skipping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}
	commitErr := rt.commitObservedLocalSnapshot(ctx, false, localResult)
	if commitErr != nil {
		rt.engine.logger.Error("watch local snapshot commit failed, skipping dirty batch",
			slog.String("error", commitErr.Error()),
		)
		return nil
	}
	rt.engine.collector().RecordObserve(len(batch.Paths), rt.engine.since(observeStart))

	planStart := rt.engine.nowFunc()
	plan, err := rt.buildCurrentActionPlan(ctx, bl, mode, safety)
	if err != nil {
		rt.engine.logger.Error("watch sqlite planning failed, skipping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}
	rt.engine.collector().RecordPlan(len(plan.Actions), rt.engine.since(planStart))

	materializeErr := rt.materializeCurrentActionPlan(ctx, plan, false)
	if materializeErr != nil {
		rt.engine.logger.Error("watch action-state materialization failed, skipping dirty batch",
			slog.String("error", materializeErr.Error()),
		)
		return nil
	}

	if len(plan.Actions) == 0 {
		rt.engine.logger.Debug("empty sqlite action plan for dirty batch")
		return nil
	}

	rt.deduplicateInFlight(plan)

	dispatch, dispatched, err := rt.dispatchCurrentPlan(ctx, plan, bl, dispatchBatchOptions{})
	if err != nil {
		rt.engine.logger.Error("watch dispatch failed, skipping dirty batch",
			slog.String("error", err.Error()),
		)
		return nil
	}
	if !dispatched {
		return nil
	}

	return dispatch
}

type dispatchBatchOptions struct {
	trialScopeKey ScopeKey
	trialPath     string
	trialWork     RetryWorkKey
}

func (rt *watchRuntime) dispatchCurrentPlan(
	ctx context.Context,
	plan *ActionPlan,
	bl *Baseline,
	opts dispatchBatchOptions,
) ([]*TrackedAction, bool, error) {
	if plan == nil || len(plan.Actions) == 0 {
		return nil, false, nil
	}

	return rt.dispatchBatchActions(ctx, plan, bl, opts)
}

// periodicPermRecheck runs permission rechecks at most once per 60 seconds.
// Throttled to avoid API hammering (R-2.10.9).
func (rt *watchRuntime) periodicPermRecheck(ctx context.Context, bl *Baseline) {
	const permRecheckInterval = 60 * time.Second

	now := rt.engine.nowFunc()
	if now.Sub(rt.lastPermRecheck) < permRecheckInterval {
		return
	}

	rt.lastPermRecheck = now

	// recheckPermissions calls the Graph API — skip during outage or
	// throttle to avoid wasting API calls (R-2.10.30). Local permission
	// rechecks (filesystem-only) proceed regardless.
	if rt.engine.permHandler.HasPermChecker() && !rt.scopeController().isObservationSuppressed(rt) {
		decisions := rt.engine.permHandler.recheckPermissions(ctx, bl)
		rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, decisions)
	}

	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, rt.engine.permHandler.recheckLocalPermissions(ctx))
}

// deduplicateInFlight cancels in-flight actions for paths that appear in the
// plan. B-122: newer observation supersedes in-progress action.
func (rt *watchRuntime) deduplicateInFlight(plan *ActionPlan) {
	for i := range plan.Actions {
		if rt.depGraph.HasInFlight(plan.Actions[i].Path) {
			rt.engine.logger.Info("canceling in-flight action for updated path",
				slog.String("path", plan.Actions[i].Path),
			)

			rt.depGraph.CancelByPath(plan.Actions[i].Path)
		}
	}
}

// dispatchBatchActions adds plan actions to the DepGraph with monotonic IDs,
// then admits ready actions through active-scope checks.
func (rt *watchRuntime) dispatchBatchActions(
	ctx context.Context,
	plan *ActionPlan,
	bl *Baseline,
	opts dispatchBatchOptions,
) ([]*TrackedAction, bool, error) {
	// Invariant: Planner always builds Deps with len(Actions).
	if len(plan.Actions) != len(plan.Deps) {
		rt.engine.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		return nil, false, nil
	}

	trialIndex := rt.findTrialActionIndex(plan, opts)
	if !opts.trialScopeKey.IsZero() && trialIndex < 0 {
		if err := rt.engine.baseline.ClearHeldRetryWork(ctx, opts.trialWork, opts.trialScopeKey); err != nil {
			rt.engine.logger.Debug("dispatchBatchActions: failed to clear stale trial failure",
				slog.String("path", opts.trialWork.Path),
				slog.String("scope_key", opts.trialScopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
		return nil, false, nil
	}

	// Allocate monotonic action IDs for this batch. Using a global monotonic
	// counter prevents ID collisions across batches without cross-goroutine sync.
	batchBaseID := rt.nextActionID
	rt.nextActionID += int64(len(plan.Actions))

	actionIDs := make([]int64, len(plan.Actions))
	for i := range plan.Actions {
		actionIDs[i] = batchBaseID + int64(i)
	}

	var ready []*TrackedAction

	for i := range plan.Actions {
		id := actionIDs[i]

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, actionIDs[depIdx])
		}

		if ta := rt.depGraph.Add(&plan.Actions[i], id, depIDs); ta != nil {
			if trialIndex == i {
				ta.IsTrial = true
				ta.TrialScopeKey = opts.trialScopeKey
			}
			ready = append(ready, ta)
		}

		if trialIndex == i {
			rt.depGraph.MarkTrial(id, opts.trialScopeKey)
		}
	}

	if len(ready) > 0 {
		ready = rt.scopeController().admitReady(ctx, rt, ready)
		drained, err := rt.drainPublicationReadyActions(ctx, rt, bl, nil, ready)
		if err != nil {
			rt.completeOutboxAsShutdown(drained)
			return nil, false, err
		}
		ready = drained
	}

	rt.engine.logger.Info("watch batch dispatched",
		slog.Int("actions", len(plan.Actions)),
	)
	rt.engine.collector().RecordExecute(len(plan.Actions), 0, 0, 0)

	if trialIndex >= 0 {
		rt.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventTrialDispatched,
			ScopeKey: opts.trialScopeKey,
			Path:     opts.trialPath,
		})
	}

	return ready, true, nil
}

func (rt *watchRuntime) findTrialActionIndex(plan *ActionPlan, opts dispatchBatchOptions) int {
	if opts.trialScopeKey.IsZero() {
		return -1
	}

	for i := range plan.Actions {
		action := &plan.Actions[i]
		if action.Path != opts.trialWork.Path ||
			action.OldPath != opts.trialWork.OldPath ||
			action.Type != opts.trialWork.ActionType {
			continue
		}
		if !opts.trialScopeKey.BlocksAction(
			action.Path,
			action.ThrottleTargetKey(),
			action.Type,
		) {
			continue
		}
		return i
	}

	for i := range plan.Actions {
		action := &plan.Actions[i]
		if opts.trialScopeKey.BlocksAction(
			action.Path,
			action.ThrottleTargetKey(),
			action.Type,
		) {
			return i
		}
	}

	return -1
}

// setDispatch is retained as a coordination hook for tracker admission, but
// dispatch no longer mutates durable remote_state lifecycle.
func (flow *engineFlow) setDispatch(ctx context.Context, action *Action) {
	_ = ctx
	_ = action
}
