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
	// defaultRetryBatchSize limits how many retry_state rows are processed per
	// retry sweep so a large durable retry queue cannot monopolize the watch loop.
	defaultRetryBatchSize = 1024
)

const (
	failureResolutionSourceWorkerSuccess = "worker_success"
	failureResolutionSourceRetryResolved = "retry_resolution"
)

type retryCandidate struct {
	skipped  *SkippedItem
	resolved bool
	err      error
}

func actionWorkKey(action *Action) RetryWorkKey {
	if action == nil {
		return RetryWorkKey{}
	}

	return RetryWorkKey{
		Path:       action.Path,
		OldPath:    action.OldPath,
		ActionType: action.Type,
	}
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
			if actionWorkKey(&plan.Actions[i]) != key {
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
		if actionWorkKey(&plan.Actions[i]) == key {
			return true
		}
	}

	return false
}

func (flow *engineFlow) reconcileStaleRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *RetryStateRow,
	source string,
) {
	if row == nil {
		return
	}

	failureRow, err := flow.retryStateRowAsFailure(ctx, row)
	if err != nil {
		flow.engine.logger.Warn("stale retry candidate hydration failed",
			slog.String("path", row.Path),
			slog.String("source", source),
			slog.String("error", err.Error()),
		)
		return
	}

	candidate := flow.buildRetryCandidate(ctx, bl, failureRow)
	switch {
	case candidate.err != nil:
		flow.engine.logger.Warn("stale retry candidate rebuild failed",
			slog.String("path", row.Path),
			slog.String("source", source),
			slog.String("error", candidate.err.Error()),
		)
	case candidate.skipped != nil:
		flow.recordRetryTrialSkippedItem(ctx, failureRow, candidate.skipped)
	case candidate.resolved:
		flow.clearFailureCandidate(ctx, failureRow, source)
	}
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
		if failures, listErr := rt.engine.baseline.ListSyncFailures(ctx); listErr == nil {
			for i := range failures {
				if failures[i].Role != FailureRoleHeld || failures[i].ScopeKey != key {
					continue
				}
				work := RetryWorkKey{
					Path:       failures[i].Path,
					OldPath:    "",
					ActionType: failureActionType(&failures[i]),
				}
				if planContainsWorkKey(plan, work) {
					continue
				}
				rt.reconcileStaleRetryCandidate(ctx, bl, &RetryStateRow{
					Path:       failures[i].Path,
					ActionType: failureActionType(&failures[i]),
					ScopeKey:   failures[i].ScopeKey,
					Blocked:    failures[i].Role == FailureRoleHeld,
				}, "runTrialDispatch")
			}
		}
		rt.engine.logger.Debug("runTrialDispatch: no usable trial candidate; preserving scope",
			slog.String("scope_key", key.String()),
		)
		rt.scopeController().preserveScopeTrial(ctx, rt, key)
		return nil
	}

	if rt.depGraph.HasInFlight(retryRow.Path) {
		return nil
	}

	work := RetryWorkKey{Path: retryRow.Path, OldPath: retryRow.OldPath, ActionType: retryRow.ActionType}
	subset := selectedActionPlanByKeys(plan, []RetryWorkKey{work})
	if subset == nil || len(subset.Actions) == 0 {
		if row, rowErr := rt.retryStateRowAsFailure(ctx, retryRow); rowErr == nil {
			rt.clearFailureCandidate(ctx, row, "runTrialDispatch")
		}
		rt.engine.logger.Debug("runTrialDispatch: current action set no longer contains blocked retry work",
			slog.String("scope_key", key.String()),
			slog.String("path", retryRow.Path),
			slog.String("action", retryRow.ActionType.String()),
		)
		rt.scopeController().preserveScopeTrial(ctx, rt, key)
		return nil
	}

	outbox, accepted := rt.dispatchCurrentPlan(ctx, subset, dispatchBatchOptions{
		trialScopeKey: key,
		trialPath:     retryRow.Path,
		trialDriveID:  rt.engine.driveID,
	})
	if accepted {
		return outbox
	}

	return nil
}

// runRetrierSweep processes a batch of due sync_failures and routes them back
// through the planner without going through the observer buffer or debounce
// path. Retry work is still rebuilt from current durable truth, not from
// cached action payloads.
func (rt *watchRuntime) runRetrierSweep(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) []*TrackedAction {
	rt.mustAssertPlannerSweepAllowed(rt, "runRetrierSweep", "run retrier sweep")
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepStarted})

	now := rt.engine.nowFunc()

	retryRows, err := rt.engine.baseline.ListRetryStateReady(ctx, now)
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

	dispatchedRows := 0
	batchLimit := rt.engine.effectiveRetryBatchLimit()
	keys := make([]RetryWorkKey, 0, len(retryRows))

	for i := range retryRows {
		if dispatchedRows >= batchLimit {
			rt.kickRetrySweepNow()
			break
		}

		work := RetryWorkKey{
			Path:       retryRows[i].Path,
			OldPath:    retryRows[i].OldPath,
			ActionType: retryRows[i].ActionType,
		}
		if !planContainsWorkKey(plan, work) {
			rt.reconcileStaleRetryCandidate(ctx, bl, &retryRows[i], "runRetrierSweep")
			continue
		}

		if rt.depGraph.HasInFlight(retryRows[i].Path) {
			continue
		}
		keys = append(keys, work)
		dispatchedRows++
	}

	subset := selectedActionPlanByKeys(plan, keys)
	if dispatchedRows == 0 || subset == nil || len(subset.Actions) == 0 {
		rt.armRetryTimer(ctx)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}

	dispatch, dispatched := rt.dispatchCurrentPlan(ctx, subset, dispatchBatchOptions{})
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

func (flow *engineFlow) retryStateRowAsFailure(
	ctx context.Context,
	row *RetryStateRow,
) (*SyncFailureRow, error) {
	if row == nil {
		return nil, fmt.Errorf("nil retry_state row")
	}

	driveID := flow.engine.driveID
	if configuredDriveID, configErr := flow.engine.baseline.configuredDriveIDForRead(
		ctx,
		driveid.ID{},
	); configErr == nil && !configuredDriveID.IsZero() {
		driveID = configuredDriveID
	}
	parsed := &SyncFailureRow{
		Path:         row.Path,
		DriveID:      driveID,
		ActionType:   row.ActionType,
		Direction:    row.ActionType.Direction(),
		Role:         FailureRoleItem,
		Category:     CategoryTransient,
		FailureCount: row.AttemptCount,
		NextRetryAt:  row.NextRetryAt,
		LastError:    row.LastError,
		FirstSeenAt:  row.FirstSeenAt,
		LastSeenAt:   row.LastSeenAt,
		ScopeKey:     row.ScopeKey,
	}
	if row.Blocked {
		parsed.Role = FailureRoleHeld
	}

	enriched, found, err := flow.engine.baseline.GetSyncFailureByPath(ctx, row.Path, driveid.ID{})
	if err != nil {
		return nil, err
	}
	if !found || enriched == nil {
		return parsed, nil
	}

	enriched.ActionType = row.ActionType
	enriched.Direction = row.ActionType.Direction()
	enriched.ScopeKey = row.ScopeKey
	enriched.NextRetryAt = row.NextRetryAt
	enriched.FailureCount = row.AttemptCount
	enriched.LastError = row.LastError
	enriched.FirstSeenAt = row.FirstSeenAt
	enriched.LastSeenAt = row.LastSeenAt
	if row.Blocked {
		enriched.Role = FailureRoleHeld
	}

	return enriched, nil
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
	if cached := flow.engine.baseline.Baseline(); cached != nil {
		return cached, nil
	}
	return flow.engine.baseline.Load(ctx)
}

func baselineEntryForFailureInBaseline(bl *Baseline, row *SyncFailureRow) *BaselineEntry {
	if bl == nil || row == nil {
		return nil
	}

	if entry, ok := bl.GetByPath(row.Path); ok && entry.DriveID == row.DriveID {
		return entry
	}
	if row.ItemID == "" {
		return nil
	}

	entry, ok := bl.GetByID(row.ItemID)
	if !ok {
		return nil
	}

	return entry
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

func (flow *engineFlow) clearFailureCandidate(ctx context.Context, row *SyncFailureRow, caller string) {
	if err := flow.takeSyncFailureAndLogResolution(
		ctx,
		row.Path,
		flow.failureDriveID(row),
		failureResolutionSourceRetryResolved,
	); err != nil {
		flow.engine.logger.Debug(caller+": failed to clear resolved failure",
			slog.String("path", row.Path),
			slog.String("error", err.Error()),
		)
	}
}

func (flow *engineFlow) failureDriveID(row *SyncFailureRow) driveid.ID {
	if row == nil || row.DriveID.IsZero() {
		return flow.engine.driveID
	}

	return row.DriveID
}

func failureActionType(row *SyncFailureRow) ActionType {
	if row == nil {
		return ActionDownload
	}
	if row.ActionType == ActionDownload && row.Direction != DirectionDownload {
		switch row.Direction {
		case DirectionDownload:
			return ActionDownload
		case DirectionUpload:
			return ActionUpload
		case DirectionDelete:
			return ActionRemoteDelete
		}
	}
	return row.ActionType
}

func (flow *engineFlow) recordRetryTrialSkippedItem(
	ctx context.Context,
	row *SyncFailureRow,
	skipped *SkippedItem,
) {
	if skipped == nil {
		return
	}
	if skipped.Reason == "" {
		flow.clearFailureCandidate(ctx, row, "recordRetryTrialSkippedItem")
		return
	}

	driveID := flow.failureDriveID(row)

	flow.engine.logger.Warn("retry/trial observation filter: skipped file",
		slog.String("path", skipped.Path),
		slog.String("issue_type", skipped.Reason),
		slog.String("detail", skipped.Detail),
	)

	if err := flow.engine.baseline.UpsertActionableFailures(ctx, []ActionableFailure{{
		Path:       skipped.Path,
		DriveID:    driveID,
		Direction:  failureActionType(row).Direction(),
		ActionType: failureActionType(row),
		IssueType:  skipped.Reason,
		Error:      skipped.Detail,
		FileSize:   skipped.FileSize,
	}}); err != nil {
		flow.engine.logger.Error("failed to record retry/trial skipped item",
			slog.String("path", skipped.Path),
			slog.String("issue_type", skipped.Reason),
			slog.String("error", err.Error()),
		)
	}
}

func (flow *engineFlow) buildRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *SyncFailureRow,
) retryCandidate {
	bl, err := flow.loadRetryBaseline(ctx, bl)
	if err != nil {
		return retryCandidate{err: fmt.Errorf("load baseline: %w", err)}
	}

	switch failureActionType(row) {
	case ActionUpload, ActionFolderCreate:
		return flow.buildLocalObservationRetryCandidate(bl, row)
	case ActionRemoteMove:
		return flow.buildRemoteMoveRetryCandidate(bl, row)
	case ActionRemoteDelete:
		return flow.buildRemoteDeleteRetryCandidate(bl, row)
	case ActionDownload:
		return flow.buildMirrorRetryCandidate(ctx, bl, row, true)
	case ActionLocalDelete:
		return flow.buildLocalDeleteRetryCandidate(ctx, bl, row)
	case ActionConflictCopy,
		ActionLocalMove,
		ActionUpdateSynced,
		ActionCleanup:
		return flow.buildMirrorRetryCandidate(ctx, bl, row, false)
	default:
		panic(fmt.Sprintf("unknown action type %d", row.ActionType))
	}
}

func (flow *engineFlow) buildMirrorRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *SyncFailureRow,
	forceDownload bool,
) retryCandidate {
	rs, found, err := flow.engine.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
	if err != nil {
		return retryCandidate{err: fmt.Errorf("remote state lookup failed: %w", err)}
	}
	if !found {
		return retryCandidate{resolved: true}
	}
	if baselineEntryMatchesRemoteState(baselineEntryForFailureInBaseline(bl, row), rs) {
		return retryCandidate{resolved: true}
	}
	_ = forceDownload

	return retryCandidate{}
}

func (flow *engineFlow) buildLocalDeleteRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *SyncFailureRow,
) retryCandidate {
	entry := baselineEntryForFailureInBaseline(bl, row)
	if entry == nil {
		return retryCandidate{resolved: true}
	}

	if row.ItemID != "" {
		_, found, err := flow.engine.baseline.GetRemoteStateByID(ctx, row.DriveID, row.ItemID)
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
	row *SyncFailureRow,
) retryCandidate {
	result, err := ObserveSinglePathWithFilter(
		flow.engine.logger,
		flow.engine.syncTree,
		row.Path,
		baselineEntryForPathInBaseline(bl, row.Path, row.DriveID),
		flow.engine.nowFunc().UnixNano(),
		nil,
		flow.engine.localFilter,
		flow.engine.localRules,
	)
	if err != nil {
		return retryCandidate{err: err}
	}

	return retryCandidate{
		skipped:  result.Skipped,
		resolved: result.Resolved,
	}
}

func (flow *engineFlow) buildRemoteMoveRetryCandidate(
	bl *Baseline,
	row *SyncFailureRow,
) retryCandidate {
	rebuild := flow.buildLocalObservationRetryCandidate(bl, row)
	if rebuild.err != nil || rebuild.skipped != nil || rebuild.resolved || row.ItemID == "" {
		return rebuild
	}

	oldEntry, ok := bl.GetByID(row.ItemID)
	if !ok || oldEntry.Path == row.Path {
		return rebuild
	}

	return retryCandidate{}
}

func (flow *engineFlow) buildRemoteDeleteRetryCandidate(bl *Baseline, row *SyncFailureRow) retryCandidate {
	_, ok := bl.GetByPath(row.Path)
	if !ok && row.ItemID != "" {
		_, ok = bl.GetByID(row.ItemID)
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

// clearFailureOnSuccess removes the sync_failures row for a successfully
// completed action. The engine owns failure lifecycle — store_baseline's
// CommitMutation handles only baseline/remote_state updates.
func (flow *engineFlow) clearFailureOnSuccess(ctx context.Context, r *WorkerResult) {
	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	if clearErr := flow.takeSyncFailureAndLogResolution(
		ctx,
		r.Path,
		driveID,
		failureResolutionSourceWorkerSuccess,
	); clearErr != nil {
		flow.engine.logger.Warn("failed to clear sync failure on success",
			slog.String("path", r.Path),
			slog.String("error", clearErr.Error()),
		)
	}
}

func (flow *engineFlow) takeSyncFailureAndLogResolution(
	ctx context.Context,
	path string,
	driveID driveid.ID,
	resolutionSource string,
) error {
	row, found, err := flow.engine.baseline.TakeSyncFailure(ctx, path, driveID)
	if err != nil {
		return fmt.Errorf("take sync failure %s: %w", path, err)
	}
	if !found || row == nil {
		return nil
	}
	if row.Category != CategoryTransient || row.Role != FailureRoleItem {
		return nil
	}

	flow.engine.logger.Info("transient failure resolved",
		slog.String("path", row.Path),
		slog.String("drive_id", row.DriveID.String()),
		slog.String("issue_type", row.IssueType),
		slog.String("action_type", row.ActionType.String()),
		slog.Int("attempt_count", row.FailureCount),
		slog.String("resolution_source", resolutionSource),
	)

	return nil
}

func (flow *engineFlow) applyFailurePersistence(
	ctx context.Context,
	decision *ResultDecision,
	r *WorkerResult,
) {
	switch decision.Persistence {
	case persistNone:
		return
	case persistActionableFailure:
		flow.recordFailure(ctx, decision, r, nil)
	case persistTransientFailure:
		flow.recordFailure(ctx, decision, r, retry.ReconcilePolicy().Delay)
	default:
		panic(fmt.Sprintf("unknown failure persistence mode %d", decision.Persistence))
	}
}
