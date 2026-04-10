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
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
	changes       []synctypes.PathChanges
	trialScopeKey synctypes.ScopeKey
	trialPath     string
	trialDriveID  driveid.ID
}

type failureRebuildResult struct {
	event    *synctypes.ChangeEvent
	skipped  *synctypes.SkippedItem
	resolved bool
	err      error
}

func pathChangesFromEvent(ev *synctypes.ChangeEvent) []synctypes.PathChanges {
	if ev == nil {
		return nil
	}

	pc := synctypes.PathChanges{Path: ev.Path}
	switch ev.Source {
	case synctypes.SourceRemote:
		pc.RemoteEvents = append(pc.RemoteEvents, *ev)
	case synctypes.SourceLocal:
		pc.LocalEvents = append(pc.LocalEvents, *ev)
	default:
		return nil
	}

	return []synctypes.PathChanges{pc}
}

func (rt *watchRuntime) dispatchWorkRequest(
	ctx context.Context,
	request engineWorkRequest,
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
) ([]*synctypes.TrackedAction, bool) {
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
	changes []synctypes.PathChanges,
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
	options dispatchBatchOptions,
) ([]*synctypes.TrackedAction, bool) {
	denied := rt.engine.permHandler.DeniedPrefixes(ctx)
	plan, err := rt.engine.planner.Plan(changes, bl, mode, safety, denied)
	if err != nil {
		if errors.Is(err, synctypes.ErrBigDeleteTriggered) {
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
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
) []*synctypes.TrackedAction {
	rt.mustAssertPlannerSweepAllowed(rt, "runTrialDispatch", "run trial dispatch")
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventTrialSweepStarted})

	now := rt.engine.nowFunc()
	var dispatch []*synctypes.TrackedAction
	seen := make(map[synctypes.ScopeKey]bool)

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
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
	key synctypes.ScopeKey,
) []*synctypes.TrackedAction {
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

		if rt.isFailureResolved(ctx, row) {
			continue
		}

		rebuild := rt.rebuildFailureWork(ctx, row)
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
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
) []*synctypes.TrackedAction {
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
		if rt.isFailureResolved(ctx, row) {
			continue
		}

		rebuild := rt.rebuildFailureWork(ctx, row)
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
) []synctypes.PathChanges {
	rows, err := flow.engine.baseline.ListSyncFailuresForRetry(ctx, flow.engine.nowFunc())
	if err != nil {
		flow.engine.logger.Warn("one-shot retry replay: failed to list retriable items",
			slog.String("error", err.Error()),
		)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	var changes []synctypes.PathChanges

	for i := range rows {
		row := &rows[i]
		if observedPaths[row.Path] {
			continue
		}
		if flow.isFailureResolved(ctx, row) {
			continue
		}

		rebuild := flow.rebuildFailureWork(ctx, row)
		switch {
		case rebuild.err != nil:
			flow.engine.logger.Warn("one-shot retry replay: failed to rebuild retry candidate",
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
		flow.engine.logger.Info("one-shot retry replay",
			slog.Int("paths", len(changes)),
		)
	}

	return changes
}

func mergePathChangeBatches(batches ...[]synctypes.PathChanges) []synctypes.PathChanges {
	merged := make(map[string]*synctypes.PathChanges)

	for _, batch := range batches {
		for i := range batch {
			path := batch[i].Path
			if path == "" {
				continue
			}

			entry, ok := merged[path]
			if !ok {
				entry = &synctypes.PathChanges{Path: path}
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

	result := make([]synctypes.PathChanges, 0, len(paths))
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

// remoteStateToChangeEvent converts a sync_failures retry row backed by
// remote_state into a planner-ready change event.
func remoteStateToChangeEvent(rs *synctypes.RemoteStateRow, path string) *synctypes.ChangeEvent {
	ct := synctypes.ChangeModify
	isDeleted := false

	if isDeleteLikeSyncStatus(rs.SyncStatus) {
		ct = synctypes.ChangeDelete
		isDeleted = true
	}

	return &synctypes.ChangeEvent{
		Source:    synctypes.SourceRemote,
		Type:      ct,
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
		IsDeleted: isDeleted,
	}
}

// observeLocalFile rebuilds upload-side observation from current local truth.
func (flow *engineFlow) baselineEntryForPath(ctx context.Context, path string, driveID driveid.ID) *synctypes.BaselineEntry {
	bl := flow.engine.baseline.Baseline()
	if bl == nil {
		var err error
		bl, err = flow.engine.baseline.Load(ctx)
		if err != nil {
			flow.engine.logger.Debug("baselineEntryForPath: failed to load baseline",
				slog.String("path", path),
				slog.String("error", err.Error()),
			)
			return nil
		}
	}

	entry, ok := bl.GetByPath(path)
	if !ok || entry.DriveID != driveID {
		return nil
	}

	return entry
}

func (flow *engineFlow) clearFailureCandidate(ctx context.Context, row *synctypes.SyncFailureRow, caller string) {
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

func (flow *engineFlow) failureDriveID(row *synctypes.SyncFailureRow) driveid.ID {
	if row == nil || row.DriveID.IsZero() {
		return flow.engine.driveID
	}

	return row.DriveID
}

func failureActionType(row *synctypes.SyncFailureRow) synctypes.ActionType {
	if row == nil {
		return synctypes.ActionDownload
	}
	if row.ActionType == synctypes.ActionDownload && row.Direction != synctypes.DirectionDownload {
		switch row.Direction {
		case synctypes.DirectionDownload:
			return synctypes.ActionDownload
		case synctypes.DirectionUpload:
			return synctypes.ActionUpload
		case synctypes.DirectionDelete:
			return synctypes.ActionRemoteDelete
		}
	}
	return row.ActionType
}

func (flow *engineFlow) recordRetryTrialSkippedItem(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
	skipped *synctypes.SkippedItem,
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

	if err := flow.engine.baseline.UpsertActionableFailures(ctx, []synctypes.ActionableFailure{{
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

func (flow *engineFlow) rebuildFailureWork(ctx context.Context, row *synctypes.SyncFailureRow) failureRebuildResult {
	switch failureActionType(row) {
	case synctypes.ActionUpload, synctypes.ActionFolderCreate:
		return flow.observeLocalFailurePath(ctx, row)
	case synctypes.ActionRemoteMove:
		return flow.rebuildRemoteMoveFailure(ctx, row)
	case synctypes.ActionRemoteDelete:
		return flow.rebuildRemoteDeleteFailure(ctx, row)
	case synctypes.ActionDownload:
		return flow.rebuildDownloadFailure(ctx, row)
	case synctypes.ActionLocalDelete:
		return flow.rebuildLocalDeleteFailure(ctx, row)
	case synctypes.ActionLocalMove,
		synctypes.ActionConflict,
		synctypes.ActionUpdateSynced,
		synctypes.ActionCleanup:
		return flow.rebuildRemoteStateBackedFailure(ctx, row)
	default:
		panic(fmt.Sprintf("unknown action type %d", row.ActionType))
	}
}

func (flow *engineFlow) rebuildRemoteStateBackedFailure(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
) failureRebuildResult {
	rs, found, err := flow.engine.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
	if err != nil {
		return failureRebuildResult{err: fmt.Errorf("remote state lookup failed: %w", err)}
	}
	if !found {
		return failureRebuildResult{resolved: true}
	}

	return failureRebuildResult{event: remoteStateToChangeEvent(rs, row.Path)}
}

func (flow *engineFlow) rebuildDownloadFailure(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
) failureRebuildResult {
	rs, found, err := flow.engine.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
	if err != nil {
		return failureRebuildResult{err: fmt.Errorf("remote state lookup failed: %w", err)}
	}
	if !found {
		return failureRebuildResult{resolved: true}
	}

	event := remoteStateToChangeEvent(rs, row.Path)
	event.ForcedAction = synctypes.ActionDownload
	event.HasForcedAction = true

	return failureRebuildResult{event: event}
}

func (flow *engineFlow) rebuildLocalDeleteFailure(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
) failureRebuildResult {
	if row.ItemID != "" {
		rs, found, err := flow.engine.baseline.GetRemoteStateByID(ctx, row.DriveID, row.ItemID)
		if err != nil {
			return failureRebuildResult{err: fmt.Errorf("remote state lookup failed: %w", err)}
		}
		if found {
			event := remoteStateToChangeEvent(rs, row.Path)
			event.ForcedAction = synctypes.ActionLocalDelete
			event.HasForcedAction = true
			return failureRebuildResult{event: event}
		}
	}

	rebuild := flow.rebuildRemoteStateBackedFailure(ctx, row)
	if rebuild.event != nil {
		rebuild.event.ForcedAction = synctypes.ActionLocalDelete
		rebuild.event.HasForcedAction = true
	}
	return rebuild
}

func (flow *engineFlow) observeLocalFailurePath(ctx context.Context, row *synctypes.SyncFailureRow) failureRebuildResult {
	scopeSnapshot, err := flow.engine.buildScopeSnapshot(ctx)
	if err != nil {
		return failureRebuildResult{err: fmt.Errorf("build scope snapshot: %w", err)}
	}

	result, err := syncobserve.ObserveSinglePathWithScope(
		flow.engine.logger,
		flow.engine.syncTree,
		row.Path,
		flow.baselineEntryForPath(ctx, row.Path, row.DriveID),
		flow.engine.nowFunc().UnixNano(),
		nil,
		flow.engine.localFilter,
		flow.engine.localRules,
		scopeSnapshot,
	)
	if err != nil {
		return failureRebuildResult{err: err}
	}

	return failureRebuildResult{
		event:    result.Event,
		skipped:  result.Skipped,
		resolved: result.Resolved,
	}
}

func (flow *engineFlow) rebuildRemoteMoveFailure(ctx context.Context, row *synctypes.SyncFailureRow) failureRebuildResult {
	rebuild := flow.observeLocalFailurePath(ctx, row)
	if rebuild.err != nil || rebuild.skipped != nil || rebuild.resolved || rebuild.event == nil || row.ItemID == "" {
		return rebuild
	}

	bl, err := flow.engine.baseline.Load(ctx)
	if err != nil {
		return failureRebuildResult{err: err}
	}
	oldEntry, ok := bl.GetByID(driveid.NewItemKey(row.DriveID, row.ItemID))
	if !ok || oldEntry.Path == row.Path {
		return rebuild
	}

	moveEvent := *rebuild.event
	moveEvent.Type = synctypes.ChangeMove
	moveEvent.OldPath = oldEntry.Path

	return failureRebuildResult{event: &moveEvent}
}

func (flow *engineFlow) rebuildRemoteDeleteFailure(ctx context.Context, row *synctypes.SyncFailureRow) failureRebuildResult {
	bl, err := flow.engine.baseline.Load(ctx)
	if err != nil {
		return failureRebuildResult{err: err}
	}

	entry, ok := bl.GetByPath(row.Path)
	if !ok && row.ItemID != "" {
		entry, ok = bl.GetByID(driveid.NewItemKey(row.DriveID, row.ItemID))
	}
	if !ok {
		return failureRebuildResult{resolved: true}
	}

	_, statErr := flow.engine.syncTree.Stat(row.Path)
	switch {
	case statErr == nil:
		return failureRebuildResult{resolved: true}
	case !errors.Is(statErr, os.ErrNotExist):
		return failureRebuildResult{err: statErr}
	}

	return failureRebuildResult{event: &synctypes.ChangeEvent{
		Source:          synctypes.SourceLocal,
		Type:            synctypes.ChangeDelete,
		ForcedAction:    synctypes.ActionRemoteDelete,
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

// createEventFromDB rebuilds a planner-ready change from current durable
// state plus the local filesystem. Retry and trial work both use this shared
// reconstruction path.
func (flow *engineFlow) createEventFromDB(ctx context.Context, row *synctypes.SyncFailureRow) *synctypes.ChangeEvent {
	return flow.rebuildFailureWork(ctx, row).event
}

// isFailureResolved checks whether a retry/trial candidate has already been
// resolved by normal observation or action processing.
func (flow *engineFlow) isFailureResolved(ctx context.Context, row *synctypes.SyncFailureRow) bool {
	switch failureActionType(row) {
	case synctypes.ActionUpload, synctypes.ActionFolderCreate, synctypes.ActionRemoteMove:
		return flow.clearFailureIfResolved(ctx, row, flow.isUploadLikeFailureResolved(row.Path))
	case synctypes.ActionRemoteDelete:
		return flow.clearFailureIfResolved(ctx, row, flow.isRemoteDeleteFailureResolved(ctx, row))
	case synctypes.ActionDownload:
		return flow.clearFailureIfResolved(ctx, row, flow.isDownloadFailureResolved(ctx, row))
	case synctypes.ActionLocalDelete,
		synctypes.ActionLocalMove,
		synctypes.ActionConflict,
		synctypes.ActionUpdateSynced,
		synctypes.ActionCleanup:
		return flow.clearFailureIfResolved(ctx, row, flow.isRemoteStateBackedActionResolved(ctx, row))
	default:
		panic(fmt.Sprintf("unknown action type %d", row.ActionType))
	}
}

func (flow *engineFlow) isRemoteStateBackedActionResolved(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
) bool {
	switch failureActionType(row) {
	case synctypes.ActionUpload:
		panic(fmt.Sprintf("unexpected remote-state-backed action type %d", row.ActionType))
	case synctypes.ActionRemoteDelete:
		panic(fmt.Sprintf("unexpected remote-state-backed action type %d", row.ActionType))
	case synctypes.ActionRemoteMove:
		panic(fmt.Sprintf("unexpected remote-state-backed action type %d", row.ActionType))
	case synctypes.ActionFolderCreate:
		panic(fmt.Sprintf("unexpected remote-state-backed action type %d", row.ActionType))
	case synctypes.ActionLocalDelete:
		return flow.isDeleteDirectionFailureResolved(ctx, row)
	case synctypes.ActionDownload,
		synctypes.ActionLocalMove,
		synctypes.ActionConflict,
		synctypes.ActionUpdateSynced,
		synctypes.ActionCleanup:
		return flow.isDownloadFailureResolved(ctx, row)
	default:
		panic(fmt.Sprintf("unexpected remote-state-backed action type %d", row.ActionType))
	}
}

func (flow *engineFlow) isUploadLikeFailureResolved(path string) bool {
	_, err := flow.engine.syncTree.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (flow *engineFlow) isRemoteDeleteFailureResolved(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
) bool {
	bl, err := flow.engine.baseline.Load(ctx)
	if err != nil {
		return false
	}
	if _, exists := bl.GetByPath(row.Path); !exists {
		if row.ItemID == "" {
			return true
		}
		_, existsByID := bl.GetByID(driveid.NewItemKey(row.DriveID, row.ItemID))
		return !existsByID
	}

	_, statErr := flow.engine.syncTree.Stat(row.Path)
	return statErr == nil
}

func (flow *engineFlow) isDeleteDirectionFailureResolved(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
) bool {
	bl, err := flow.engine.baseline.Load(ctx)
	if err != nil {
		return false
	}
	_, exists := bl.GetByPath(row.Path)
	return !exists
}

func (flow *engineFlow) isDownloadFailureResolved(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
) bool {
	rs, found, err := flow.engine.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
	if err != nil {
		return false
	}
	if !found {
		return true
	}

	return isResolvedRemoteSyncStatus(rs.SyncStatus)
}

func (flow *engineFlow) clearFailureIfResolved(
	ctx context.Context,
	row *synctypes.SyncFailureRow,
	resolved bool,
) bool {
	if !resolved {
		return false
	}

	if err := flow.takeSyncFailureAndLogResolution(
		ctx,
		row.Path,
		flow.failureDriveID(row),
		failureResolutionSourceRetryResolved,
	); err != nil {
		flow.engine.logger.Debug("isFailureResolved: failed to clear resolved failure",
			slog.String("path", row.Path),
			slog.String("error", err.Error()),
		)
	}

	return true
}

// clearFailureOnSuccess removes the sync_failures row for a successfully
// completed action. The engine owns failure lifecycle — store_baseline's
// CommitOutcome handles only baseline/remote_state updates.
func (flow *engineFlow) clearFailureOnSuccess(ctx context.Context, r *synctypes.WorkerResult) {
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
	if row.Category != synctypes.CategoryTransient || row.Role != synctypes.FailureRoleItem {
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
	r *synctypes.WorkerResult,
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
