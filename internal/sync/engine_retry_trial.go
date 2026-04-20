package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

const (
	// defaultRetryBatchSize limits how many retry_work rows are processed per
	// retry sweep so a large durable retry queue cannot monopolize the watch loop.
	defaultRetryBatchSize = 1024
)

const (
	retryResolutionSourceWorkerSuccess = "worker_success"
	retryResolutionSourceRevalidated   = "retry_resolution"
)

type retryCandidate struct {
	observation *SinglePathObservation
	skipped     *SkippedItem
	resolved    bool
	err         error
}

//nolint:gocyclo // The subset builder keeps dependency closure and remapping in one place.
func selectedActionPlanByKeys(plan *ActionPlan, keys []RetryWorkKey) *ActionPlan {
	if plan == nil || len(plan.Actions) == 0 || len(keys) == 0 {
		return nil
	}

	selected := make(map[int]struct{}, len(keys))
	queue := make([]int, 0, len(keys))

	for _, key := range keys {
		for i := range plan.Actions {
			if retryWorkKeyForAction(&plan.Actions[i]) != key {
				continue
			}
			if _, ok := selected[i]; ok {
				break
			}
			selected[i] = struct{}{}
			queue = append(queue, i)
			break
		}
	}

	for len(queue) > 0 {
		idx := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, depIdx := range plan.Deps[idx] {
			if _, ok := selected[depIdx]; ok {
				continue
			}
			selected[depIdx] = struct{}{}
			queue = append(queue, depIdx)
		}
	}

	if len(selected) == 0 {
		return nil
	}

	ordered := make([]int, 0, len(selected))
	for idx := range selected {
		ordered = append(ordered, idx)
	}
	sort.Ints(ordered)

	remap := make(map[int]int, len(ordered))
	actions := make([]Action, 0, len(ordered))
	deps := make([][]int, 0, len(ordered))
	for newIdx, oldIdx := range ordered {
		remap[oldIdx] = newIdx
		actions = append(actions, plan.Actions[oldIdx])
		deps = append(deps, nil)
	}

	for newIdx, oldIdx := range ordered {
		oldDeps := plan.Deps[oldIdx]
		if len(oldDeps) == 0 {
			continue
		}
		newDeps := make([]int, 0, len(oldDeps))
		for _, oldDep := range oldDeps {
			newDep, ok := remap[oldDep]
			if !ok {
				continue
			}
			newDeps = append(newDeps, newDep)
		}
		deps[newIdx] = newDeps
	}

	return &ActionPlan{
		Actions:        actions,
		Deps:           deps,
		DeferredByMode: DeferredCounts{},
	}
}

func (rt *watchRuntime) refreshCurrentActionPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) (*ActionPlan, error) {
	localResult, err := rt.observeLocalChanges(ctx, rt, bl)
	if err != nil {
		return nil, fmt.Errorf("sync: local refresh before retry/trial planning: %w", err)
	}
	observedAt := rt.engine.nowFunc().UnixNano()
	rows := buildLocalStateRows(localResult, observedAt)
	replaceErr := rt.engine.baseline.ReplaceLocalState(ctx, rows)
	if replaceErr != nil {
		return nil, fmt.Errorf("sync: replacing local snapshot before retry/trial planning: %w", replaceErr)
	}

	plan, err := rt.buildCurrentActionPlan(ctx, bl, mode, safety)
	if err != nil {
		return nil, fmt.Errorf("sync: building current action plan for retry/trial: %w", err)
	}
	if err := rt.materializeCurrentActionPlan(ctx, plan, false); err != nil {
		return nil, fmt.Errorf("sync: materializing current action plan for retry/trial: %w", err)
	}

	return plan, nil
}

func planContainsWorkKey(plan *ActionPlan, key RetryWorkKey) bool {
	if plan == nil {
		return false
	}

	for i := range plan.Actions {
		if retryWorkKeyForAction(&plan.Actions[i]) == key {
			return true
		}
	}

	return false
}

// runTrialDispatch handles due scope trials without re-observing through a
// bespoke API path. Trial candidates are reconstructed from current durable
// state, planned through the normal engine path, and marked explicitly as
// trials in the dependency graph. Lack of a usable candidate is not proof that
// the scope recovered; preserve semantics keep the scope active until an
// actual release signal arrives.
func (rt *watchRuntime) runTrialDispatch(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) []*TrackedAction {
	rt.mustAssertPlannerSweepAllowed(rt, "runTrialDispatch", "run trial dispatch")
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventTrialSweepStarted})

	now := rt.engine.nowFunc()
	var dispatch []*TrackedAction
	seen := make(map[ScopeKey]bool)

	for _, key := range rt.dueTrials(now) {
		seen[key] = true
		outbox := rt.dispatchDueTrialScope(ctx, bl, mode, safety, key)
		dispatch = append(dispatch, outbox...)
	}

	rt.armTrialTimer()
	rt.engine.emitDebugEvent(engineDebugEvent{
		Type:  engineDebugEventTrialSweepCompleted,
		Count: len(dispatch),
	})
	return dispatch
}

func (rt *watchRuntime) dispatchDueTrialScope(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
	key ScopeKey,
) []*TrackedAction {
	probeRow, hadBlockedWork, err := rt.engine.baseline.PickRetryTrialCandidate(ctx, key)
	if err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to probe blocked retry work before refresh",
			slog.String("scope_key", key.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	plan, err := rt.refreshCurrentActionPlan(ctx, bl, mode, safety)
	if err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to refresh current action plan",
			slog.String("scope_key", key.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	retryRow, found, err := rt.engine.baseline.PickRetryTrialCandidate(ctx, key)
	if err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to pick trial candidate",
			slog.String("scope_key", key.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}

	if !found {
		return rt.handleMissingTrialCandidate(ctx, bl, key, probeRow, hadBlockedWork)
	}

	if rt.depGraph.HasInFlight(retryRow.Path) {
		return nil
	}

	work := retryWorkKeyForRetryWork(retryRow)
	subset := selectedActionPlanByKeys(plan, []RetryWorkKey{work})
	if subset == nil || len(subset.Actions) == 0 {
		rt.clearStaleTrialRetryWork(ctx, key, retryRow)
		return nil
	}

	outbox, accepted, err := rt.dispatchCurrentPlan(ctx, subset, bl, dispatchBatchOptions{
		trialScopeKey: key,
		trialPath:     retryRow.Path,
		trialWork:     work,
	})
	if err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to dispatch trial action set",
			slog.String("scope_key", key.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if accepted {
		return outbox
	}

	return nil
}

func (rt *watchRuntime) handleMissingTrialCandidate(
	ctx context.Context,
	bl *Baseline,
	key ScopeKey,
	probeRow *RetryWorkRow,
	hadBlockedWork bool,
) []*TrackedAction {
	if hadBlockedWork {
		driveID, driveErr := rt.retryWorkDriveID(ctx)
		if driveErr != nil {
			rt.engine.logger.Warn("runTrialDispatch: failed to load drive for disappeared blocked retry work",
				slog.String("scope_key", key.String()),
				slog.String("path", probeRow.Path),
				slog.String("error", driveErr.Error()),
			)
		} else {
			candidate := rt.buildRetryCandidateFromRetryWork(ctx, bl, probeRow, driveID)
			if candidate.err != nil {
				rt.engine.logger.Warn("runTrialDispatch: failed to revalidate disappeared blocked retry work",
					slog.String("scope_key", key.String()),
					slog.String("path", probeRow.Path),
					slog.String("error", candidate.err.Error()),
				)
			} else if candidate.resolved {
				rt.scopeController().clearBlockedRetryWork(ctx, probeRow, "runTrialDispatch")
			}
		}

		rt.scopeController().rearmOrDiscardScope(ctx, rt, key)
		rt.engine.logger.Debug("runTrialDispatch: blocked retry work disappeared during refresh; rechecking scope state",
			slog.String("scope_key", key.String()),
		)
		return nil
	}

	if err := rt.scopeController().discardScope(ctx, rt, key); err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to discard scope without blocked retry work",
			slog.String("scope_key", key.String()),
			slog.String("error", err.Error()),
		)
	}
	rt.engine.logger.Debug("runTrialDispatch: no blocked retry work remains for scope",
		slog.String("scope_key", key.String()),
	)
	return nil
}

func (rt *watchRuntime) clearStaleTrialRetryWork(
	ctx context.Context,
	key ScopeKey,
	row *RetryWorkRow,
) {
	if row == nil {
		return
	}

	scopeKey := row.ScopeKey
	if scopeKey.IsZero() {
		scopeKey = key
	}

	work := retryWorkKeyForRetryWork(row)

	if err := rt.engine.baseline.ClearBlockedRetryWork(ctx, work, scopeKey); err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to clear stale blocked retry work",
			slog.String("scope_key", scopeKey.String()),
			slog.String("path", row.Path),
			slog.String("action", row.ActionType.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	rt.engine.logger.Debug("runTrialDispatch: current action set no longer contains blocked retry work",
		slog.String("scope_key", scopeKey.String()),
		slog.String("path", row.Path),
		slog.String("action", row.ActionType.String()),
	)

	if _, found, err := rt.engine.baseline.PickRetryTrialCandidate(ctx, scopeKey); err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to relist blocked retry work after stale clear",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	} else if found {
		rt.scopeController().rearmOrDiscardScope(ctx, rt, scopeKey)
		return
	}

	if err := rt.scopeController().discardScope(ctx, rt, scopeKey); err != nil {
		rt.engine.logger.Warn("runTrialDispatch: failed to discard stale blocked scope",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// runRetrierSweep processes a batch of due retry_work rows and routes them
// back through the current actionable-set builder without going through the
// observer buffer or debounce path.
func (rt *watchRuntime) runRetrierSweep(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) []*TrackedAction {
	rt.mustAssertPlannerSweepAllowed(rt, "runRetrierSweep", "run retrier sweep")
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepStarted})

	now := rt.engine.nowFunc()

	retryRows, err := rt.engine.baseline.ListRetryWorkReady(ctx, now)
	if err != nil {
		rt.engine.logger.Warn("retrier sweep: failed to list retriable items",
			slog.String("error", err.Error()),
		)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}
	if len(retryRows) == 0 {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}

	plan, err := rt.refreshCurrentActionPlan(ctx, bl, mode, safety)
	if err != nil {
		rt.engine.logger.Warn("retrier sweep: failed to refresh current action plan",
			slog.String("error", err.Error()),
		)
		rt.armRetryTimer(ctx)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}

	keys, dispatchedRows := rt.collectRetrySweepKeys(ctx, bl, plan, retryRows)

	subset := selectedActionPlanByKeys(plan, keys)
	if dispatchedRows == 0 || subset == nil || len(subset.Actions) == 0 {
		rt.armRetryTimer(ctx)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}

	dispatch, dispatched, err := rt.dispatchCurrentPlan(ctx, subset, bl, dispatchBatchOptions{})
	if err != nil {
		rt.engine.logger.Warn("retrier sweep: failed to dispatch current action plan",
			slog.String("error", err.Error()),
		)
		rt.armRetryTimer(ctx)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}
	if !dispatched {
		rt.engine.logger.Debug("retrier sweep produced no dispatchable work",
			slog.Int("candidate_rows", dispatchedRows),
		)
	}
	rt.engine.logger.Info("retrier sweep",
		slog.Int("dispatched", dispatchedRows),
	)
	rt.armRetryTimer(ctx)
	rt.engine.emitDebugEvent(engineDebugEvent{
		Type:  engineDebugEventRetrySweepCompleted,
		Count: len(dispatch),
	})
	return dispatch
}

func (rt *watchRuntime) collectRetrySweepKeys(
	ctx context.Context,
	bl *Baseline,
	plan *ActionPlan,
	retryRows []RetryWorkRow,
) ([]RetryWorkKey, int) {
	dispatchedRows := 0
	batchLimit := rt.engine.effectiveRetryBatchLimit()
	keys := make([]RetryWorkKey, 0, len(retryRows))

	for i := range retryRows {
		if dispatchedRows >= batchLimit {
			rt.kickRetrySweepNow()
			break
		}

		work := retryWorkKeyForRetryWork(&retryRows[i])
		if !planContainsWorkKey(plan, work) {
			rt.clearStaleRetrySweepRow(ctx, bl, &retryRows[i], work)
			continue
		}

		if rt.depGraph.HasInFlight(retryRows[i].Path) {
			continue
		}
		keys = append(keys, work)
		dispatchedRows++
	}

	return keys, dispatchedRows
}

func (rt *watchRuntime) clearStaleRetrySweepRow(
	ctx context.Context,
	bl *Baseline,
	row *RetryWorkRow,
	work RetryWorkKey,
) {
	if row == nil {
		return
	}

	driveID, driveErr := rt.retryWorkDriveID(ctx)
	if driveErr != nil {
		rt.engine.logger.Warn("retrier sweep: failed to load drive for stale retry_work",
			slog.String("path", row.Path),
			slog.String("action", row.ActionType.String()),
			slog.String("error", driveErr.Error()),
		)
		return
	}

	candidate := rt.buildRetryCandidateFromRetryWork(ctx, bl, row, driveID)
	if candidate.err != nil {
		rt.engine.logger.Warn("retrier sweep: failed to revalidate stale retry_work row",
			slog.String("path", row.Path),
			slog.String("action", row.ActionType.String()),
			slog.String("error", candidate.err.Error()),
		)
		return
	}

	if candidate.skipped != nil {
		rt.recordRetryTrialSkippedItem(ctx, rt, work, driveID, candidate.skipped)
	} else if candidate.resolved {
		rt.reconcileRetryTrialObservationResult(ctx, rt, work, driveID, row.Path, candidate.observation)
		rt.clearRetryWorkCandidate(ctx, work, driveID, "runRetrierSweep")
	} else if candidate.observation != nil {
		rt.reconcileRetryTrialObservationResult(ctx, rt, work, driveID, row.Path, candidate.observation)
	}
}

func (e *Engine) effectiveRetryBatchLimit() int {
	if e.retryBatchLimit > 0 {
		return e.retryBatchLimit
	}

	return defaultRetryBatchSize
}

func baselineEntryMatchesRemoteState(entry *BaselineEntry, rs *RemoteStateRow) bool {
	if entry == nil || rs == nil {
		return false
	}

	if entry.DriveID != rs.DriveID || entry.ItemID != rs.ItemID || entry.Path != rs.Path || entry.ItemType != rs.ItemType {
		return false
	}

	return entry.RemoteHash == rs.Hash
}

func (flow *engineFlow) loadRetryBaseline(ctx context.Context, bl *Baseline) (*Baseline, error) {
	if bl != nil {
		return bl, nil
	}
	if cached := flow.engine.baseline.cachedBaseline(); cached != nil {
		return cached, nil
	}
	return flow.engine.baseline.Load(ctx)
}

func baselineEntryForPathInBaseline(bl *Baseline, path string, driveID driveid.ID) *BaselineEntry {
	if bl == nil {
		return nil
	}

	entry, ok := bl.GetByPath(path)
	if !ok || entry.DriveID != driveID {
		return nil
	}

	return entry
}

func (flow *engineFlow) clearRetryWorkCandidate(
	ctx context.Context,
	work RetryWorkKey,
	driveID driveid.ID,
	caller string,
) {
	_ = driveID

	if err := flow.resolveRetryWorkAndLogResolution(
		ctx,
		work,
		retryResolutionSourceRevalidated,
	); err != nil {
		flow.engine.logger.Debug(caller+": failed to clear resolved retry work",
			slog.String("path", work.Path),
			slog.String("action", work.ActionType.String()),
			slog.String("error", err.Error()),
		)
	}
}

func (flow *engineFlow) retryWorkDriveID(ctx context.Context) (driveid.ID, error) {
	configuredDriveID, err := flow.engine.baseline.configuredDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return driveid.ID{}, fmt.Errorf("load configured drive for retry_work row: %w", err)
	}
	if configuredDriveID.IsZero() {
		configuredDriveID = flow.engine.driveID
	}

	return configuredDriveID, nil
}

func (flow *engineFlow) buildRetryCandidateFromRetryWork(
	ctx context.Context,
	bl *Baseline,
	row *RetryWorkRow,
	driveID driveid.ID,
) retryCandidate {
	if row == nil {
		return retryCandidate{}
	}

	bl, err := flow.loadRetryBaseline(ctx, bl)
	if err != nil {
		return retryCandidate{err: fmt.Errorf("load baseline: %w", err)}
	}

	switch row.ActionType {
	case ActionUpload, ActionFolderCreate:
		return flow.buildLocalObservationRetryCandidate(bl, row, driveID)
	case ActionRemoteMove:
		return flow.buildRemoteMoveRetryCandidate(bl, row, driveID)
	case ActionRemoteDelete:
		return flow.buildRemoteDeleteRetryCandidate(bl, row, driveID)
	case ActionDownload:
		return flow.buildMirrorRetryCandidate(ctx, bl, row, driveID)
	case ActionLocalDelete:
		return flow.buildLocalDeleteRetryCandidate(ctx, bl, row, driveID)
	case ActionConflictCopy,
		ActionLocalMove,
		ActionUpdateSynced,
		ActionCleanup:
		return flow.buildMirrorRetryCandidate(ctx, bl, row, driveID)
	default:
		panic(fmt.Sprintf("unknown action type %d", row.ActionType))
	}
}

func (flow *engineFlow) recordRetryTrialSkippedItem(
	ctx context.Context,
	watch *watchRuntime,
	work RetryWorkKey,
	driveID driveid.ID,
	skipped *SkippedItem,
) {
	if skipped == nil {
		return
	}
	if skipped.Reason == "" {
		flow.clearRetryWorkCandidate(ctx, work, driveID, "recordRetryTrialSkippedItem")
		return
	}

	flow.engine.logger.Warn("retry/trial observation filter: skipped file",
		slog.String("path", skipped.Path),
		slog.String("issue_type", skipped.Reason),
		slog.String("detail", skipped.Detail),
	)
	flow.reconcileRetryTrialObservationResult(ctx, watch, work, driveID, skipped.Path, &SinglePathObservation{Skipped: skipped})

	flow.clearRetryWorkCandidate(ctx, work, driveID, "recordRetryTrialSkippedItem")
}

func (flow *engineFlow) reconcileRetryTrialObservationResult(
	ctx context.Context,
	watch *watchRuntime,
	work RetryWorkKey,
	driveID driveid.ID,
	managedPath string,
	observation *SinglePathObservation,
) {
	batch, ok := observationFindingsBatchFromSinglePathObservation(driveID, managedPath, observation)
	if !ok {
		return
	}

	if err := flow.engine.baseline.ReconcileObservationFindings(ctx, &batch, flow.engine.nowFunc()); err != nil {
		flow.engine.logger.Warn("retry/trial failed to reconcile observation findings",
			slog.String("path", managedPath),
			slog.String("action_type", work.ActionType.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	if watch == nil {
		return
	}
	if err := flow.scopeController().loadActiveScopes(ctx, watch); err != nil {
		flow.engine.logger.Warn("retry/trial failed to refresh watch scopes",
			slog.String("path", managedPath),
			slog.String("error", err.Error()),
		)
	}
}

func (flow *engineFlow) buildMirrorRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *RetryWorkRow,
	driveID driveid.ID,
) retryCandidate {
	rs, found, err := flow.engine.baseline.GetRemoteStateByPath(ctx, row.Path, driveID)
	if err != nil {
		return retryCandidate{err: fmt.Errorf("remote state lookup failed: %w", err)}
	}
	if !found {
		return retryCandidate{resolved: true}
	}
	if baselineEntryMatchesRemoteState(baselineEntryForPathInBaseline(bl, row.Path, driveID), rs) {
		return retryCandidate{resolved: true}
	}

	return retryCandidate{}
}

func (flow *engineFlow) buildLocalDeleteRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *RetryWorkRow,
	driveID driveid.ID,
) retryCandidate {
	entry := baselineEntryForPathInBaseline(bl, row.Path, driveID)
	if entry == nil {
		return retryCandidate{resolved: true}
	}

	if entry.ItemID != "" {
		_, found, err := flow.engine.baseline.GetRemoteStateByID(ctx, driveID, entry.ItemID)
		if err != nil {
			return retryCandidate{err: fmt.Errorf("remote state lookup failed: %w", err)}
		}
		if found {
			return retryCandidate{resolved: true}
		}
	}

	return retryCandidate{}
}

func (flow *engineFlow) buildLocalObservationRetryCandidate(
	bl *Baseline,
	row *RetryWorkRow,
	driveID driveid.ID,
) retryCandidate {
	result, err := ObserveSinglePathWithFilter(
		flow.engine.logger,
		flow.engine.syncTree,
		row.Path,
		baselineEntryForPathInBaseline(bl, row.Path, driveID),
		flow.engine.nowFunc().UnixNano(),
		nil,
		flow.engine.localFilter,
		flow.engine.localRules,
	)
	if err != nil {
		return retryCandidate{err: err}
	}

	return retryCandidate{
		observation: &result,
		skipped:     result.Skipped,
		resolved:    result.Resolved,
	}
}

func (flow *engineFlow) buildRemoteMoveRetryCandidate(
	bl *Baseline,
	row *RetryWorkRow,
	driveID driveid.ID,
) retryCandidate {
	rebuild := flow.buildLocalObservationRetryCandidate(bl, row, driveID)
	if rebuild.err != nil || rebuild.skipped != nil || rebuild.resolved || row.OldPath == "" {
		return rebuild
	}

	oldEntry, ok := bl.GetByPath(row.OldPath)
	if !ok || oldEntry.Path == row.Path {
		return rebuild
	}

	return retryCandidate{}
}

func (flow *engineFlow) buildRemoteDeleteRetryCandidate(
	bl *Baseline,
	row *RetryWorkRow,
	driveID driveid.ID,
) retryCandidate {
	_, ok := bl.GetByPath(row.Path)
	if !ok {
		ok = baselineEntryForPathInBaseline(bl, row.Path, driveID) != nil
	}
	if !ok {
		return retryCandidate{resolved: true}
	}

	_, statErr := flow.engine.syncTree.Stat(row.Path)
	switch {
	case statErr == nil:
		return retryCandidate{resolved: true}
	case !errors.Is(statErr, os.ErrNotExist):
		return retryCandidate{err: statErr}
	}

	return retryCandidate{}
}

// clearRetryWorkOnSuccess removes the retry_work row for a successfully
// completed action. The engine owns retry_work and observation-issue lifecycle;
// CommitMutation handles only baseline and remote_state updates.
func (flow *engineFlow) clearRetryWorkOnSuccess(ctx context.Context, r *ActionCompletion) {
	if clearErr := flow.resolveRetryWorkAndLogResolution(
		ctx,
		retryWorkKeyForCompletion(r),
		retryResolutionSourceWorkerSuccess,
	); clearErr != nil {
		flow.engine.logger.Warn("failed to clear retry_work on success",
			slog.String("path", r.Path),
			slog.String("error", clearErr.Error()),
		)
	}
}

func (flow *engineFlow) resolveRetryWorkAndLogResolution(
	ctx context.Context,
	work RetryWorkKey,
	resolutionSource string,
) error {
	row, found, err := flow.engine.baseline.ResolveRetryWork(ctx, work)
	if err != nil {
		return fmt.Errorf("resolve retry work %s: %w", work.Path, err)
	}
	if !found || row == nil {
		return nil
	}

	flow.engine.logger.Info("retry_work resolved",
		slog.String("path", row.Path),
		slog.String("issue_type", row.IssueType),
		slog.String("action_type", row.ActionType.String()),
		slog.Int("attempt_count", row.AttemptCount),
		slog.String("resolution_source", resolutionSource),
	)

	return nil
}

func (flow *engineFlow) applyResultPersistence(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
) {
	switch decision.Persistence {
	case persistNone:
		return
	case persistRetryWork:
		flow.recordRetryWork(ctx, watch, decision, r, retry.ReconcilePolicy().Delay)
	default:
		panic(fmt.Sprintf("unknown failure persistence mode %d", decision.Persistence))
	}
}
