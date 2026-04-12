package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

type engineWorkReason int

const (
	engineWorkObserved engineWorkReason = iota
	engineWorkRetry
	engineWorkTrial

	// defaultRetryBatchSize limits how many sync_failures are processed per retry
	// sweep so a large durable retry queue cannot monopolize the watch loop.
	defaultRetryBatchSize = 1024
)

const (
	failureResolutionSourceWorkerSuccess = "worker_success"
	failureResolutionSourceRetryResolved = "retry_resolution"
)

type engineWorkRequest struct {
	reason        engineWorkReason
	changes       []PathChanges
	trialScopeKey ScopeKey
	trialPath     string
	trialDriveID  driveid.ID
}

type retryCandidate struct {
	event    *ChangeEvent
	skipped  *SkippedItem
	resolved bool
	err      error
}

func pathChangesFromEvent(ev *ChangeEvent) []PathChanges {
	if ev == nil {
		return nil
	}

	pc := PathChanges{Path: ev.Path}
	switch ev.Source {
	case SourceRemote:
		pc.RemoteEvents = append(pc.RemoteEvents, *ev)
	case SourceLocal:
		pc.LocalEvents = append(pc.LocalEvents, *ev)
	default:
		return nil
	}

	return []PathChanges{pc}
}

func (rt *watchRuntime) dispatchWorkRequest(
	ctx context.Context,
	request engineWorkRequest,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) ([]*TrackedAction, bool) {
	if len(request.changes) == 0 {
		return nil, false
	}

	options := dispatchBatchOptions{
		applyDeleteCounter: request.reason == engineWorkRetry,
		trialScopeKey:      request.trialScopeKey,
		trialPath:          request.trialPath,
		trialDriveID:       request.trialDriveID,
	}

	return rt.dispatchPlannerWork(ctx, request.changes, bl, mode, safety, options)
}

func (rt *watchRuntime) dispatchPlannerWork(
	ctx context.Context,
	changes []PathChanges,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
	options dispatchBatchOptions,
) ([]*TrackedAction, bool) {
	denied := rt.engine.permHandler.DeniedPrefixes(ctx)
	plan, err := rt.engine.planner.Plan(changes, bl, mode, safety, denied)
	if err != nil {
		if errors.Is(err, ErrDeleteSafetyThresholdExceeded) {
			rt.engine.logger.Warn("internal work request blocked by delete protection",
				slog.Int("paths", len(changes)),
			)
			return nil, false
		}

		rt.engine.logger.Error("internal work request planning failed",
			slog.String("error", err.Error()),
			slog.Int("paths", len(changes)),
		)
		return nil, false
	}

	if len(plan.Actions) == 0 {
		return nil, false
	}

	if options.applyDeleteCounter && rt.deleteCounter != nil {
		plan, err = rt.applyDeleteCounter(ctx, plan)
		if err != nil {
			rt.engine.logger.Error("delete protection failed",
				slog.String("error", err.Error()),
			)
			return nil, false
		}
		if len(plan.Actions) == 0 {
			return nil, false
		}
	}

	return rt.dispatchBatchActions(ctx, plan, options)
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
	for {
		row, found, err := rt.engine.baseline.PickTrialCandidate(ctx, key)
		if err != nil {
			rt.engine.logger.Warn("runTrialDispatch: failed to pick trial candidate",
				slog.String("scope_key", key.String()),
				slog.String("error", err.Error()),
			)
			return nil
		}

		if !found {
			rt.engine.logger.Debug("runTrialDispatch: no usable trial candidate; preserving scope",
				slog.String("scope_key", key.String()),
			)
			rt.scopeController().preserveScopeTrial(ctx, rt, key)
			return nil
		}

		if rt.depGraph.HasInFlight(row.Path) {
			return nil
		}

		rebuild := rt.buildRetryCandidate(ctx, bl, row)
		switch {
		case rebuild.err != nil:
			rt.engine.logger.Warn("runTrialDispatch: failed to rebuild trial candidate",
				slog.String("scope_key", key.String()),
				slog.String("path", row.Path),
				slog.String("error", rebuild.err.Error()),
			)
			return nil
		case rebuild.skipped != nil:
			rt.recordRetryTrialSkippedItem(ctx, row, rebuild.skipped)
			continue
		case rebuild.resolved:
			rt.clearFailureCandidate(ctx, row, "runTrialDispatch")
			continue
		case rebuild.event == nil:
			continue
		}

		outbox, accepted := rt.dispatchWorkRequest(ctx, engineWorkRequest{
			reason:        engineWorkTrial,
			changes:       pathChangesFromEvent(rebuild.event),
			trialScopeKey: key,
			trialPath:     row.Path,
			trialDriveID:  row.DriveID,
		}, bl, mode, safety)
		if accepted {
			return outbox
		}
	}
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

	rows, err := rt.engine.baseline.ListSyncFailuresForRetry(ctx, now)
	if err != nil {
		rt.engine.logger.Warn("retrier sweep: failed to list retriable items",
			slog.String("error", err.Error()),
		)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}
	if len(rows) == 0 {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}

	request := engineWorkRequest{reason: engineWorkRetry}
	dispatchedRows := 0
	batchLimit := rt.engine.effectiveRetryBatchLimit()

	for i := range rows {
		if dispatchedRows >= batchLimit {
			rt.kickRetrySweepNow()
			break
		}

		row := &rows[i]
		if rt.depGraph.HasInFlight(row.Path) {
			continue
		}
		rebuild := rt.buildRetryCandidate(ctx, bl, row)
		switch {
		case rebuild.err != nil:
			rt.engine.logger.Warn("retrier sweep: failed to rebuild retry candidate",
				slog.String("path", row.Path),
				slog.String("error", rebuild.err.Error()),
			)
			continue
		case rebuild.skipped != nil:
			rt.recordRetryTrialSkippedItem(ctx, row, rebuild.skipped)
			continue
		case rebuild.resolved:
			rt.clearFailureCandidate(ctx, row, "runRetrierSweep")
			continue
		case rebuild.event == nil:
			continue
		}

		request.changes = append(request.changes, pathChangesFromEvent(rebuild.event)...)
		dispatchedRows++
	}

	if dispatchedRows == 0 {
		rt.armRetryTimer(ctx)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRetrySweepCompleted})
		return nil
	}

	dispatch, dispatched := rt.dispatchWorkRequest(ctx, request, bl, mode, safety)
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

func (flow *engineFlow) collectDueRetryChanges(
	ctx context.Context,
	observedPaths map[string]bool,
) []PathChanges {
	bl, loadErr := flow.engine.baseline.Load(ctx)
	if loadErr != nil {
		flow.engine.logger.Warn("one-shot retry rebuild: failed to load baseline",
			slog.String("error", loadErr.Error()),
		)
		return nil
	}

	rows, err := flow.engine.baseline.ListSyncFailuresForRetry(ctx, flow.engine.nowFunc())
	if err != nil {
		flow.engine.logger.Warn("one-shot retry rebuild: failed to list retriable items",
			slog.String("error", err.Error()),
		)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	var changes []PathChanges

	for i := range rows {
		row := &rows[i]
		if observedPaths[row.Path] {
			continue
		}

		rebuild := flow.buildRetryCandidate(ctx, bl, row)
		switch {
		case rebuild.err != nil:
			flow.engine.logger.Warn("one-shot retry rebuild: failed to rebuild retry candidate",
				slog.String("path", row.Path),
				slog.String("error", rebuild.err.Error()),
			)
		case rebuild.skipped != nil:
			flow.recordRetryTrialSkippedItem(ctx, row, rebuild.skipped)
		case rebuild.resolved:
			flow.clearFailureCandidate(ctx, row, "collectDueRetryChanges")
		case rebuild.event != nil:
			changes = append(changes, pathChangesFromEvent(rebuild.event)...)
		}
	}

	if len(changes) > 0 {
		flow.engine.logger.Info("one-shot retry rebuild",
			slog.Int("paths", len(changes)),
		)
	}

	return changes
}

func mergePathChangeBatches(batches ...[]PathChanges) []PathChanges {
	merged := make(map[string]*PathChanges)

	for _, batch := range batches {
		for i := range batch {
			path := batch[i].Path
			if path == "" {
				continue
			}

			entry, ok := merged[path]
			if !ok {
				entry = &PathChanges{Path: path}
				merged[path] = entry
			}

			entry.RemoteEvents = append(entry.RemoteEvents, batch[i].RemoteEvents...)
			entry.LocalEvents = append(entry.LocalEvents, batch[i].LocalEvents...)
		}
	}

	if len(merged) == 0 {
		return nil
	}

	paths := make([]string, 0, len(merged))
	for path := range merged {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	result := make([]PathChanges, 0, len(paths))
	for _, path := range paths {
		result = append(result, *merged[path])
	}

	return result
}

func (e *Engine) effectiveRetryBatchLimit() int {
	if e.retryBatchLimit > 0 {
		return e.retryBatchLimit
	}

	return defaultRetryBatchSize
}

// remoteStateToChangeEvent converts current remote mirror truth into a
// planner-ready remote change event. Remote deletion is represented by mirror
// absence and handled separately by delete-specific rebuild paths.
func remoteStateToChangeEvent(rs *RemoteStateRow, path string) *ChangeEvent {
	return &ChangeEvent{
		Source:    SourceRemote,
		Type:      ChangeModify,
		Path:      path,
		ItemID:    rs.ItemID,
		ParentID:  rs.ParentID,
		DriveID:   rs.DriveID,
		ItemType:  rs.ItemType,
		Name:      filepath.Base(path),
		Size:      rs.Size,
		Hash:      rs.Hash,
		Mtime:     rs.Mtime,
		ETag:      rs.ETag,
		IsDeleted: false,
	}
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

	entry, ok := bl.GetByID(driveid.NewItemKey(row.DriveID, row.ItemID))
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
		return flow.buildLocalObservationRetryCandidate(ctx, bl, row)
	case ActionRemoteMove:
		return flow.buildRemoteMoveRetryCandidate(ctx, bl, row)
	case ActionRemoteDelete:
		return flow.buildRemoteDeleteRetryCandidate(bl, row)
	case ActionDownload:
		return flow.buildMirrorRetryCandidate(ctx, bl, row, true)
	case ActionLocalDelete:
		return flow.buildLocalDeleteRetryCandidate(ctx, bl, row)
	case ActionLocalMove,
		ActionConflict,
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
	if !found || rs.IsFiltered {
		return retryCandidate{resolved: true}
	}
	if baselineEntryMatchesRemoteState(baselineEntryForFailureInBaseline(bl, row), rs) {
		return retryCandidate{resolved: true}
	}

	event := remoteStateToChangeEvent(rs, row.Path)
	if forceDownload {
		event.ForcedAction = ActionDownload
		event.HasForcedAction = true
	}

	return retryCandidate{event: event}
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

	return retryCandidate{event: &ChangeEvent{
		Source:          SourceRemote,
		Type:            ChangeDelete,
		ForcedAction:    ActionLocalDelete,
		HasForcedAction: true,
		Path:            entry.Path,
		ItemID:          entry.ItemID,
		ParentID:        entry.ParentID,
		DriveID:         entry.DriveID,
		ItemType:        entry.ItemType,
		Name:            filepath.Base(entry.Path),
		Size:            entry.RemoteSize,
		Hash:            entry.RemoteHash,
		Mtime:           entry.RemoteMtime,
		IsDeleted:       true,
	}}
}

func (flow *engineFlow) buildLocalObservationRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *SyncFailureRow,
) retryCandidate {
	scopeSnapshot, err := flow.engine.buildScopeSnapshot(ctx)
	if err != nil {
		return retryCandidate{err: fmt.Errorf("build scope snapshot: %w", err)}
	}

	result, err := ObserveSinglePathWithScope(
		flow.engine.logger,
		flow.engine.syncTree,
		row.Path,
		baselineEntryForPathInBaseline(bl, row.Path, row.DriveID),
		flow.engine.nowFunc().UnixNano(),
		nil,
		flow.engine.localFilter,
		flow.engine.localRules,
		scopeSnapshot,
	)
	if err != nil {
		return retryCandidate{err: err}
	}

	return retryCandidate{
		event:    result.Event,
		skipped:  result.Skipped,
		resolved: result.Resolved,
	}
}

func (flow *engineFlow) buildRemoteMoveRetryCandidate(
	ctx context.Context,
	bl *Baseline,
	row *SyncFailureRow,
) retryCandidate {
	rebuild := flow.buildLocalObservationRetryCandidate(ctx, bl, row)
	if rebuild.err != nil || rebuild.skipped != nil || rebuild.resolved || rebuild.event == nil || row.ItemID == "" {
		return rebuild
	}

	oldEntry, ok := bl.GetByID(driveid.NewItemKey(row.DriveID, row.ItemID))
	if !ok || oldEntry.Path == row.Path {
		return rebuild
	}

	moveEvent := *rebuild.event
	moveEvent.Type = ChangeMove
	moveEvent.OldPath = oldEntry.Path

	return retryCandidate{event: &moveEvent}
}

func (flow *engineFlow) buildRemoteDeleteRetryCandidate(bl *Baseline, row *SyncFailureRow) retryCandidate {
	entry, ok := bl.GetByPath(row.Path)
	if !ok && row.ItemID != "" {
		entry, ok = bl.GetByID(driveid.NewItemKey(row.DriveID, row.ItemID))
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

	return retryCandidate{event: &ChangeEvent{
		Source:          SourceLocal,
		Type:            ChangeDelete,
		ForcedAction:    ActionRemoteDelete,
		HasForcedAction: true,
		Path:            row.Path,
		DriveID:         row.DriveID,
		ItemType:        entry.ItemType,
		Name:            filepath.Base(row.Path),
		Size:            entry.LocalSize,
		Mtime:           entry.LocalMtime,
		IsDeleted:       true,
	}}
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
