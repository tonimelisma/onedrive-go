package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// processBatch plans and dispatches a batch of path changes. On planner
// error, the batch is skipped and the loop continues. In-flight actions for
// overlapping paths are canceled and replaced (B-122 deduplication).
func (rt *watchRuntime) processBatch(
	ctx context.Context, batch []PathChanges, bl *Baseline,
	mode Mode, safety *SafetyConfig,
) []*TrackedAction {
	return rt.planAndDispatchBatch(ctx, batch, bl, mode, safety, dispatchBatchOptions{
		deduplicateInFlight:  true,
		runPeriodicPermCheck: true,
		clearScannerResolved: true,
	})
}

type dispatchBatchOptions struct {
	deduplicateInFlight  bool
	runPeriodicPermCheck bool
	clearScannerResolved bool
	trialScopeKey        ScopeKey
	trialPath            string
	trialDriveID         driveid.ID
}

func (rt *watchRuntime) planAndDispatchBatch(
	ctx context.Context,
	batch []PathChanges,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
	opts dispatchBatchOptions,
) []*TrackedAction {
	rt.engine.logger.Info("processing watch batch",
		slog.Int("paths", len(batch)),
	)
	rt.engine.collector().RecordWatchBatch(len(batch))

	if opts.runPeriodicPermCheck {
		rt.periodicPermRecheck(ctx, bl)
	}

	// R-2.10.10: use scanner output as proof-of-accessibility to clear
	// permission denials for paths observed in this batch.
	if opts.clearScannerResolved {
		rt.scopeController().applyPermissionRecheckDecisions(
			ctx,
			rt,
			rt.engine.permHandler.clearScannerResolvedPermissions(ctx, pathSetFromBatch(batch)),
		)
		rt.scopeController().clearResolvedRemoteBlockedFailures(ctx, rt, pathSetFromBatch(batch))
	}

	denied := rt.engine.permHandler.DeniedPrefixes(ctx)
	planStart := rt.engine.nowFunc()
	plan, err := rt.engine.planner.Plan(batch, bl, mode, safety, denied)
	if err != nil {
		rt.engine.logger.Error("planner error, skipping batch",
			slog.String("error", err.Error()),
		)

		return nil
	}
	rt.engine.collector().RecordPlan(len(plan.Actions), rt.engine.since(planStart))

	if len(plan.Actions) == 0 {
		rt.engine.logger.Debug("empty plan for batch, nothing to do")
		return nil
	}

	if opts.deduplicateInFlight {
		rt.deduplicateInFlight(plan)
	}

	dispatch, dispatched := rt.dispatchBatchActions(ctx, plan, opts)
	if !dispatched {
		return nil
	}

	return dispatch
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
	opts dispatchBatchOptions,
) ([]*TrackedAction, bool) {
	// Invariant: Planner always builds Deps with len(Actions).
	if len(plan.Actions) != len(plan.Deps) {
		rt.engine.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		return nil, false
	}

	trialIndex := rt.findTrialActionIndex(plan, opts)
	if !opts.trialScopeKey.IsZero() && trialIndex < 0 {
		if err := rt.engine.baseline.ClearSyncFailure(ctx, opts.trialPath, opts.trialDriveID); err != nil {
			rt.engine.logger.Debug("dispatchBatchActions: failed to clear stale trial failure",
				slog.String("path", opts.trialPath),
				slog.String("error", err.Error()),
			)
		}
		return nil, false
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

	return ready, true
}

func (rt *watchRuntime) findTrialActionIndex(plan *ActionPlan, opts dispatchBatchOptions) int {
	if opts.trialScopeKey.IsZero() {
		return -1
	}

	for i := range plan.Actions {
		action := &plan.Actions[i]
		if action.Path != opts.trialPath {
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
