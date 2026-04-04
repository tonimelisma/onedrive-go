package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// processBatch plans and dispatches a batch of path changes. On planner
// error (e.g. big-delete protection), the batch is skipped and the loop
// continues. In-flight actions for overlapping paths are canceled and
// replaced (B-122 deduplication).
func (rt *watchRuntime) processBatch(
	ctx context.Context, batch []synctypes.PathChanges, bl *synctypes.Baseline,
	mode synctypes.SyncMode, safety *synctypes.SafetyConfig,
) []*synctypes.TrackedAction {
	return rt.planAndDispatchBatch(ctx, batch, bl, mode, safety, dispatchBatchOptions{
		applyDeleteCounter:   true,
		deduplicateInFlight:  true,
		runPeriodicPermCheck: true,
		clearScannerResolved: true,
	})
}

type dispatchBatchOptions struct {
	applyDeleteCounter   bool
	deduplicateInFlight  bool
	runPeriodicPermCheck bool
	clearScannerResolved bool
	trialScopeKey        synctypes.ScopeKey
	trialPath            string
	trialDriveID         driveid.ID
}

func (rt *watchRuntime) planAndDispatchBatch(
	ctx context.Context,
	batch []synctypes.PathChanges,
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
	opts dispatchBatchOptions,
) []*synctypes.TrackedAction {
	rt.engine.logger.Info("processing watch batch",
		slog.Int("paths", len(batch)),
	)

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
	plan, err := rt.engine.planner.Plan(batch, bl, mode, safety, denied)
	if err != nil {
		if errors.Is(err, synctypes.ErrBigDeleteTriggered) {
			rt.engine.logger.Warn("big-delete protection triggered, skipping batch",
				slog.Int("paths", len(batch)),
			)

			return nil
		}

		rt.engine.logger.Error("planner error, skipping batch",
			slog.String("error", err.Error()),
		)

		return nil
	}

	if len(plan.Actions) == 0 {
		rt.engine.logger.Debug("empty plan for batch, nothing to do")
		return nil
	}

	// Rolling-window big-delete protection: count planned deletes and
	// filter them out if the counter trips. Non-delete actions continue
	// flowing. The planner-level check is disabled in watch mode
	// (threshold=MaxInt32) — this counter replaces it.
	if opts.applyDeleteCounter && rt.deleteCounter != nil {
		plan = rt.applyDeleteCounter(ctx, plan)
		if len(plan.Actions) == 0 {
			return nil
		}
	}

	if opts.deduplicateInFlight {
		rt.deduplicateInFlight(plan)
	}

	dispatch, _ := rt.dispatchBatchActions(ctx, plan, opts)
	return dispatch
}

// periodicPermRecheck runs permission rechecks at most once per 60 seconds.
// Throttled to avoid API hammering (R-2.10.9).
func (rt *watchRuntime) periodicPermRecheck(ctx context.Context, bl *synctypes.Baseline) {
	const permRecheckInterval = 60 * time.Second

	now := time.Now()
	if now.Sub(rt.lastPermRecheck) < permRecheckInterval {
		return
	}

	rt.lastPermRecheck = now

	// recheckPermissions calls the Graph API — skip during outage or
	// throttle to avoid wasting API calls (R-2.10.30). Local permission
	// rechecks (filesystem-only) proceed regardless.
	if rt.engine.permHandler.HasPermChecker() && !rt.scopeController().isObservationSuppressed(rt) {
		shortcuts, err := rt.engine.baseline.ListShortcuts(ctx)
		if err == nil {
			requestedKeys, reqErr := rt.engine.baseline.ListRequestedScopeRechecks(ctx)
			if reqErr != nil {
				rt.engine.logger.Warn("failed to list requested permission rechecks",
					slog.String("error", reqErr.Error()),
				)
			}
			decisions := rt.engine.permHandler.recheckPermissions(ctx, bl, shortcuts)
			rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, decisions)
			if len(requestedKeys) > 0 {
				clearRequestedScopeRechecks(ctx, rt.engine.baseline, rt.engine.logger, requestedKeys)
			}
		}
	}

	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, rt.engine.permHandler.recheckLocalPermissions(ctx))
}

// deduplicateInFlight cancels in-flight actions for paths that appear in the
// plan. B-122: newer observation supersedes in-progress action.
func (rt *watchRuntime) deduplicateInFlight(plan *synctypes.ActionPlan) {
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
	plan *synctypes.ActionPlan,
	opts dispatchBatchOptions,
) ([]*synctypes.TrackedAction, bool) {
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

	var ready []*synctypes.TrackedAction

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

	if trialIndex >= 0 {
		rt.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventTrialDispatched,
			ScopeKey: opts.trialScopeKey,
			Path:     opts.trialPath,
		})
	}

	return ready, true
}

func (rt *watchRuntime) findTrialActionIndex(plan *synctypes.ActionPlan, opts dispatchBatchOptions) int {
	if opts.trialScopeKey.IsZero() {
		return -1
	}

	for i := range plan.Actions {
		action := &plan.Actions[i]
		if action.Path != opts.trialPath {
			continue
		}
		if !opts.trialScopeKey.BlocksAction(action.Path, action.ShortcutKey(), action.Type, action.TargetsOwnDrive()) {
			continue
		}
		return i
	}

	for i := range plan.Actions {
		action := &plan.Actions[i]
		if opts.trialScopeKey.BlocksAction(action.Path, action.ShortcutKey(), action.Type, action.TargetsOwnDrive()) {
			return i
		}
	}

	return -1
}

// setDispatch writes the dispatch state transition for an action before it
// enters the tracker. Only applies to downloads and local deletes (the action
// types that have remote_state lifecycle).
func (flow *engineFlow) setDispatch(ctx context.Context, action *synctypes.Action) {
	if err := flow.engine.baseline.SetDispatchStatus(ctx, action.DriveID.String(), action.ItemID, action.Type); err != nil {
		flow.engine.logger.Warn("failed to set dispatch status",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}

// isDeleteAction returns true if the action type is a local or remote delete.
func isDeleteAction(t synctypes.ActionType) bool {
	return t == synctypes.ActionLocalDelete || t == synctypes.ActionRemoteDelete
}

// applyDeleteCounter counts planned deletes in the plan, feeds them to the
// rolling counter, and — if the counter is held — filters delete actions out
// of the plan and records them as actionable issues. Returns the (possibly
// filtered) plan. When all actions are filtered, returns a plan with empty
// Actions/Deps.
func (rt *watchRuntime) applyDeleteCounter(ctx context.Context, plan *synctypes.ActionPlan) *synctypes.ActionPlan {
	deleteCount := 0
	for i := range plan.Actions {
		if isDeleteAction(plan.Actions[i].Type) {
			deleteCount++
		}
	}

	if deleteCount == 0 {
		return plan
	}

	tripped := rt.deleteCounter.Add(deleteCount)
	if tripped {
		rt.engine.logger.Warn("big-delete protection triggered in watch mode",
			slog.Int("delete_count", rt.deleteCounter.Count()),
			slog.Int("threshold", rt.deleteCounter.Threshold()),
		)
	}

	if !rt.deleteCounter.IsHeld() {
		return plan
	}

	kept := make([]synctypes.Action, 0, len(plan.Actions))
	keptDeps := make([][]int, 0, len(plan.Deps))
	oldToNew := make(map[int]int, len(plan.Actions))

	var heldDeletes []synctypes.Action

	for i := range plan.Actions {
		if isDeleteAction(plan.Actions[i].Type) {
			heldDeletes = append(heldDeletes, plan.Actions[i])
			continue
		}

		oldToNew[i] = len(kept)
		kept = append(kept, plan.Actions[i])
		keptDeps = append(keptDeps, nil)
	}

	for newIdx := range kept {
		var origIdx int
		for oi, ni := range oldToNew {
			if ni == newIdx {
				origIdx = oi
				break
			}
		}

		for _, depOld := range plan.Deps[origIdx] {
			if depNew, ok := oldToNew[depOld]; ok {
				keptDeps[newIdx] = append(keptDeps[newIdx], depNew)
			}
		}
	}

	plan.Actions = kept
	plan.Deps = keptDeps

	rt.recordHeldDeletes(ctx, heldDeletes)

	return plan
}

// recordHeldDeletes writes held delete actions to sync_failures as actionable
// issues with type big_delete_held. Uses UpsertActionableFailures for batch
// upsert — idempotent when the same deletes are re-observed.
func (rt *watchRuntime) recordHeldDeletes(ctx context.Context, actions []synctypes.Action) {
	if len(actions) == 0 {
		return
	}

	failures := make([]synctypes.ActionableFailure, len(actions))
	for i := range actions {
		failures[i] = synctypes.ActionableFailure{
			Path:       actions[i].Path,
			DriveID:    actions[i].DriveID,
			Direction:  synctypes.DirectionDelete,
			ActionType: synctypes.ActionRemoteDelete,
			IssueType:  synctypes.IssueBigDeleteHeld,
			Error:      fmt.Sprintf("held by big-delete protection (threshold: %d)", rt.engine.bigDeleteThreshold),
		}
	}

	if err := rt.engine.baseline.UpsertActionableFailures(ctx, failures); err != nil {
		rt.engine.logger.Error("failed to record held deletes",
			slog.Int("count", len(failures)),
			slog.String("error", err.Error()),
		)
	}

	rt.engine.logger.Info("held delete actions recorded as issues",
		slog.Int("count", len(failures)),
	)
}
