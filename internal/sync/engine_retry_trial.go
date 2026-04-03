package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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

	// retryBatchSize limits how many sync_failures are processed per retry
	// sweep so a large durable retry queue cannot monopolize the watch loop.
	retryBatchSize = 1024
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
		plan = rt.applyDeleteCounter(ctx, plan)
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
	now := rt.engine.nowFunc()
	var dispatch []*synctypes.TrackedAction

	for _, key := range rt.dueTrials(now) {
		outbox, _ := rt.dispatchDueTrialScope(ctx, bl, mode, safety, key)
		dispatch = append(dispatch, outbox...)
	}

	rt.armTrialTimer()
	return dispatch
}

func (rt *watchRuntime) dispatchDueTrialScope(
	ctx context.Context,
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
	key synctypes.ScopeKey,
) ([]*synctypes.TrackedAction, bool) {
	for {
		row, found, err := rt.engine.baseline.PickTrialCandidate(ctx, key)
		if err != nil {
			rt.engine.logger.Warn("runTrialDispatch: failed to pick trial candidate",
				slog.String("scope_key", key.String()),
				slog.String("error", err.Error()),
			)
			return nil, false
		}

		if !found {
			rt.engine.logger.Debug("runTrialDispatch: no usable trial candidate; preserving scope",
				slog.String("scope_key", key.String()),
			)
			rt.scopeController().preserveScopeTrial(ctx, rt, key)
			return nil, false
		}

		if rt.depGraph.HasInFlight(row.Path) {
			return nil, false
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
			return nil, false
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
			return outbox, true
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
	now := rt.engine.nowFunc()

	rows, err := rt.engine.baseline.ListSyncFailuresForRetry(ctx, now)
	if err != nil {
		rt.engine.logger.Warn("retrier sweep: failed to list retriable items",
			slog.String("error", err.Error()),
		)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	request := engineWorkRequest{reason: engineWorkRetry}
	dispatchedRows := 0

	for i := range rows {
		if dispatchedRows >= retryBatchSize {
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
		return nil
	}

	dispatch, _ := rt.dispatchWorkRequest(ctx, request, bl, mode, safety)
	rt.engine.logger.Info("retrier sweep",
		slog.Int("dispatched", dispatchedRows),
	)
	rt.armRetryTimer(ctx)
	return dispatch
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
	if err := flow.engine.baseline.ClearSyncFailure(ctx, row.Path, row.DriveID); err != nil {
		flow.engine.logger.Debug(caller+": failed to clear resolved failure",
			slog.String("path", row.Path),
			slog.String("error", err.Error()),
		)
	}
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

	driveID := row.DriveID
	if driveID.IsZero() {
		driveID = flow.engine.driveID
	}

	flow.engine.logger.Warn("retry/trial observation filter: skipped file",
		slog.String("path", skipped.Path),
		slog.String("issue_type", skipped.Reason),
		slog.String("detail", skipped.Detail),
	)

	if err := flow.engine.baseline.UpsertActionableFailures(ctx, []synctypes.ActionableFailure{{
		Path:      skipped.Path,
		DriveID:   driveID,
		Direction: row.Direction,
		IssueType: skipped.Reason,
		Error:     skipped.Detail,
		FileSize:  skipped.FileSize,
	}}); err != nil {
		flow.engine.logger.Error("failed to record retry/trial skipped item",
			slog.String("path", skipped.Path),
			slog.String("issue_type", skipped.Reason),
			slog.String("error", err.Error()),
		)
	}
}

func (flow *engineFlow) rebuildFailureWork(ctx context.Context, row *synctypes.SyncFailureRow) failureRebuildResult {
	if row.Direction == synctypes.DirectionUpload {
		result, err := syncobserve.ObserveSinglePath(
			flow.engine.logger,
			flow.engine.syncTree,
			row.Path,
			flow.baselineEntryForPath(ctx, row.Path, row.DriveID),
			flow.engine.nowFunc().UnixNano(),
			nil,
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

	rs, found, err := flow.engine.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
	if err != nil {
		return failureRebuildResult{err: fmt.Errorf("remote state lookup failed: %w", err)}
	}
	if !found {
		return failureRebuildResult{resolved: true}
	}

	return failureRebuildResult{event: remoteStateToChangeEvent(rs, row.Path)}
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
	var resolved bool

	switch row.Direction {
	case synctypes.DirectionDownload:
		rs, found, err := flow.engine.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
		if err != nil {
			return false
		}
		if !found {
			resolved = true
		} else if isResolvedRemoteSyncStatus(rs.SyncStatus) {
			resolved = true
		}

	case synctypes.DirectionUpload:
		_, err := flow.engine.syncTree.Stat(row.Path)
		if errors.Is(err, os.ErrNotExist) {
			resolved = true
		}

	case synctypes.DirectionDelete:
		bl, err := flow.engine.baseline.Load(ctx)
		if err != nil {
			return false
		}
		_, exists := bl.GetByPath(row.Path)
		if !exists {
			resolved = true
		}
	}

	if resolved {
		if err := flow.engine.baseline.ClearSyncFailure(ctx, row.Path, row.DriveID); err != nil {
			flow.engine.logger.Debug("isFailureResolved: failed to clear resolved failure",
				slog.String("path", row.Path),
				slog.String("error", err.Error()),
			)
		}
	}

	return resolved
}

// clearFailureOnSuccess removes the sync_failures row for a successfully
// completed action. The engine owns failure lifecycle — store_baseline's
// CommitOutcome handles only baseline/remote_state updates.
func (flow *engineFlow) clearFailureOnSuccess(ctx context.Context, r *synctypes.WorkerResult) {
	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = flow.engine.driveID
	}

	if clearErr := flow.engine.baseline.ClearSyncFailure(ctx, r.Path, driveID); clearErr != nil {
		flow.engine.logger.Warn("failed to clear sync failure on success",
			slog.String("path", r.Path),
			slog.String("error", clearErr.Error()),
		)
	}
}

func (flow *engineFlow) applyFailureRecordMode(
	ctx context.Context,
	mode failureRecordMode,
	r *synctypes.WorkerResult,
) {
	switch mode {
	case recordFailureNone:
		return
	case recordFailureActionable:
		flow.recordFailure(ctx, r, nil)
	case recordFailureReconcile:
		flow.recordFailure(ctx, r, retry.ReconcilePolicy().Delay)
	default:
		panic(fmt.Sprintf("unknown failure record mode %d", mode))
	}
}
