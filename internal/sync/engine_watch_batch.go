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
func (e *Engine) processBatch(
	ctx context.Context, batch []synctypes.PathChanges, bl *synctypes.Baseline,
	mode synctypes.SyncMode, safety *synctypes.SafetyConfig,
) []*synctypes.TrackedAction {
	return e.planAndDispatchBatch(ctx, batch, bl, mode, safety, dispatchBatchOptions{
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

func (e *Engine) planAndDispatchBatch(
	ctx context.Context,
	batch []synctypes.PathChanges,
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
	opts dispatchBatchOptions,
) []*synctypes.TrackedAction {
	e.logger.Info("processing watch batch",
		slog.Int("paths", len(batch)),
	)

	if opts.runPeriodicPermCheck {
		e.periodicPermRecheck(ctx, bl)
	}

	// R-2.10.10: use scanner output as proof-of-accessibility to clear
	// permission denials for paths observed in this batch.
	if opts.clearScannerResolved {
		e.applyPermissionRecheckDecisions(ctx, e.permHandler.clearScannerResolvedPermissions(ctx, pathSetFromBatch(batch)))
	}

	denied := e.permHandler.DeniedPrefixes(ctx)
	plan, err := e.planner.Plan(batch, bl, mode, safety, denied)
	if err != nil {
		if errors.Is(err, synctypes.ErrBigDeleteTriggered) {
			e.logger.Warn("big-delete protection triggered, skipping batch",
				slog.Int("paths", len(batch)),
			)

			return nil
		}

		e.logger.Error("planner error, skipping batch",
			slog.String("error", err.Error()),
		)

		return nil
	}

	if len(plan.Actions) == 0 {
		e.logger.Debug("empty plan for batch, nothing to do")
		return nil
	}

	// Rolling-window big-delete protection: count planned deletes and
	// filter them out if the counter trips. Non-delete actions continue
	// flowing. The planner-level check is disabled in watch mode
	// (threshold=MaxInt32) — this counter replaces it.
	if opts.applyDeleteCounter && e.watch != nil && e.watch.deleteCounter != nil {
		plan = e.applyDeleteCounter(ctx, plan)
		if len(plan.Actions) == 0 {
			return nil
		}
	}

	if opts.deduplicateInFlight {
		e.deduplicateInFlight(plan)
	}

	dispatch, _ := e.dispatchBatchActions(ctx, plan, opts)
	return dispatch
}

// periodicPermRecheck runs permission rechecks at most once per 60 seconds.
// Throttled to avoid API hammering (R-2.10.9).
func (e *Engine) periodicPermRecheck(ctx context.Context, bl *synctypes.Baseline) {
	const permRecheckInterval = 60 * time.Second

	now := time.Now()
	if now.Sub(e.watch.lastPermRecheck) < permRecheckInterval {
		return
	}

	e.watch.lastPermRecheck = now

	// recheckPermissions calls the Graph API — skip during outage or
	// throttle to avoid wasting API calls (R-2.10.30). Local permission
	// rechecks (filesystem-only) proceed regardless.
	if e.permHandler.HasPermChecker() && !e.isObservationSuppressed() {
		shortcuts, err := e.baseline.ListShortcuts(ctx)
		if err == nil {
			e.applyPermissionRecheckDecisions(ctx, e.permHandler.recheckPermissions(ctx, bl, shortcuts))
		}
	}

	e.applyPermissionRecheckDecisions(ctx, e.permHandler.recheckLocalPermissions(ctx))
}

// deduplicateInFlight cancels in-flight actions for paths that appear in the
// plan. B-122: newer observation supersedes in-progress action.
func (e *Engine) deduplicateInFlight(plan *synctypes.ActionPlan) {
	for i := range plan.Actions {
		if e.depGraph.HasInFlight(plan.Actions[i].Path) {
			e.logger.Info("canceling in-flight action for updated path",
				slog.String("path", plan.Actions[i].Path),
			)

			e.depGraph.CancelByPath(plan.Actions[i].Path)
		}
	}
}

// dispatchBatchActions adds plan actions to the DepGraph with monotonic IDs,
// then admits ready actions through active-scope checks.
func (e *Engine) dispatchBatchActions(
	ctx context.Context,
	plan *synctypes.ActionPlan,
	opts dispatchBatchOptions,
) ([]*synctypes.TrackedAction, bool) {
	// Invariant: Planner always builds Deps with len(Actions).
	if len(plan.Actions) != len(plan.Deps) {
		e.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		return nil, false
	}

	trialIndex := e.findTrialActionIndex(plan, opts)
	if !opts.trialScopeKey.IsZero() && trialIndex < 0 {
		if err := e.baseline.ClearSyncFailure(ctx, opts.trialPath, opts.trialDriveID); err != nil {
			e.logger.Debug("dispatchBatchActions: failed to clear stale trial failure",
				slog.String("path", opts.trialPath),
				slog.String("error", err.Error()),
			)
		}
		return nil, false
	}

	// Allocate monotonic action IDs for this batch. Using a global monotonic
	// counter prevents ID collisions across batches without cross-goroutine sync.
	batchBaseID := e.watch.nextActionID
	e.watch.nextActionID += int64(len(plan.Actions))

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

		if ta := e.depGraph.Add(&plan.Actions[i], id, depIDs); ta != nil {
			if trialIndex == i {
				ta.IsTrial = true
				ta.TrialScopeKey = opts.trialScopeKey
			}
			ready = append(ready, ta)
		}

		if trialIndex == i {
			e.depGraph.MarkTrial(id, opts.trialScopeKey)
		}
	}

	if len(ready) > 0 {
		ready = e.admitReady(ctx, ready)
	}

	e.logger.Info("watch batch dispatched",
		slog.Int("actions", len(plan.Actions)),
	)

	if trialIndex >= 0 {
		e.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventTrialDispatched,
			ScopeKey: opts.trialScopeKey,
			Path:     opts.trialPath,
		})
	}

	return ready, true
}

func (e *Engine) findTrialActionIndex(plan *synctypes.ActionPlan, opts dispatchBatchOptions) int {
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
func (e *Engine) setDispatch(ctx context.Context, action *synctypes.Action) {
	if err := e.baseline.SetDispatchStatus(ctx, action.DriveID.String(), action.ItemID, action.Type); err != nil {
		e.logger.Warn("failed to set dispatch status",
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
func (e *Engine) applyDeleteCounter(ctx context.Context, plan *synctypes.ActionPlan) *synctypes.ActionPlan {
	deleteCount := 0
	for i := range plan.Actions {
		if isDeleteAction(plan.Actions[i].Type) {
			deleteCount++
		}
	}

	if deleteCount == 0 {
		return plan
	}

	tripped := e.watch.deleteCounter.Add(deleteCount)
	if tripped {
		e.logger.Warn("big-delete protection triggered in watch mode",
			slog.Int("delete_count", e.watch.deleteCounter.Count()),
			slog.Int("threshold", e.watch.deleteCounter.Threshold()),
		)
	}

	if !e.watch.deleteCounter.IsHeld() {
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

	e.recordHeldDeletes(ctx, heldDeletes)

	return plan
}

// recordHeldDeletes writes held delete actions to sync_failures as actionable
// issues with type big_delete_held. Uses UpsertActionableFailures for batch
// upsert — idempotent when the same deletes are re-observed.
func (e *Engine) recordHeldDeletes(ctx context.Context, actions []synctypes.Action) {
	if len(actions) == 0 {
		return
	}

	failures := make([]synctypes.ActionableFailure, len(actions))
	for i := range actions {
		failures[i] = synctypes.ActionableFailure{
			Path:      actions[i].Path,
			DriveID:   actions[i].DriveID,
			Direction: synctypes.DirectionDelete,
			IssueType: synctypes.IssueBigDeleteHeld,
			Error:     fmt.Sprintf("held by big-delete protection (threshold: %d)", e.bigDeleteThreshold),
		}
	}

	if err := e.baseline.UpsertActionableFailures(ctx, failures); err != nil {
		e.logger.Error("failed to record held deletes",
			slog.Int("count", len(failures)),
			slog.String("error", err.Error()),
		)
	}

	e.logger.Info("held delete actions recorded as issues",
		slog.Int("count", len(failures)),
	)
}
