package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// RunOnce executes a single sync pass:
//  1. Load baseline
//  2. Observe remote
//  3. Observe local
//  4. Reconcile buffered changes plus durable remote drift
//  5. Early return if no changes
//  6. Plan actions (flat list + dependency edges)
//  7. Return early if dry-run
//  8. Build DepGraph, start worker pool
//  9. Wait for completion, commit delta token
func (e *Engine) RunOnce(ctx context.Context, mode Mode, opts RunOptions) (*Report, error) {
	start := e.nowFunc()
	runner := newOneShotRunner(e)

	e.logger.Info("sync pass starting",
		slog.String("mode", mode.String()),
		slog.Bool("dry_run", opts.DryRun),
	)

	bl, err := e.prepareRunOnceBaseline(ctx, runner)
	if err != nil {
		return nil, err
	}

	fullReconcile, err := e.shouldRunFullRemoteReconcile(ctx, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	opts.FullReconcile = fullReconcile

	if opts.DryRun {
		return e.runOnceDryRun(ctx, runner, bl, mode, opts, start)
	}

	// Steps 2-4: refresh remote and local snapshots, then derive the current
	// actionable set from SQLite structural diff and reconciliation.
	observeStart := e.nowFunc()
	pendingCursorCommit, err := runner.observeCurrentTruth(ctx, nil, bl, opts.DryRun, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	e.collector().RecordObserve(0, e.since(observeStart))

	// Step 5: Plan actions from SQLite snapshots and reconciliation rows.
	safety := e.resolveSafetyConfig()

	planStart := e.nowFunc()
	plan, err := runner.buildSQLiteActionPlan(ctx, bl, mode, safety)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	e.collector().RecordPlan(len(plan.Actions), e.since(planStart))

	if materializeErr := runner.materializeSQLitePlan(ctx, plan, opts.DryRun); materializeErr != nil {
		return nil, materializeErr
	}

	// SQLite-derived plan approved — commit the deferred delta token now.
	if err := runner.commitPendingPrimaryCursor(ctx, pendingCursorCommit); err != nil {
		return nil, err
	}

	counts := CountByType(plan.Actions)
	report := buildReportFromCounts(counts, plan.DeferredByMode, mode, opts)

	if len(plan.Actions) == 0 {
		if report.DeferredByMode.Total() > 0 {
			report.Duration = e.since(start)
			e.logRunOnceCompletion(report)
			return report, nil
		}
		return e.completeRunOnceWithoutChanges(ctx, start, mode, opts), nil
	}

	if opts.DryRun {
		return e.completeDryRunReport(start, report), nil
	}

	// Execute plan: run workers, drain results (failures, 403s, upload issues).
	executeStart := e.nowFunc()
	if err := runner.executePlan(ctx, plan, report, bl); err != nil {
		report.Duration = e.since(start)
		e.collector().RecordExecute(len(plan.Actions), report.Succeeded, report.Failed, e.since(executeStart))
		return report, err
	}
	e.collector().RecordExecute(len(plan.Actions), report.Succeeded, report.Failed, e.since(executeStart))

	report.Duration = e.since(start)

	e.logRunOnceCompletion(report)

	runner.postSyncHousekeeping()

	// Persist one-shot status for status command queries.
	if metaErr := e.baseline.WriteSyncRunStatus(ctx, &SyncRunReport{
		CompletedAt: e.nowFunc(),
		Duration:    report.Duration,
		Succeeded:   report.Succeeded,
		Failed:      report.Failed,
		Errors:      report.Errors,
	}); metaErr != nil {
		e.logger.Warn("failed to write one-shot sync status", slog.String("error", metaErr.Error()))
	}

	return report, nil
}

func (e *Engine) runOnceDryRun(
	ctx context.Context,
	runner *oneShotRunner,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
	start time.Time,
) (*Report, error) {
	observeStart := e.nowFunc()
	changes, _, err := runner.observeChanges(ctx, nil, bl, true, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	e.collector().RecordObserve(len(changes), e.since(observeStart))

	safety := e.resolveSafetyConfig()
	blockedBoundaries := e.permHandler.ActiveRemoteBlockedBoundaries(ctx)

	planStart := e.nowFunc()
	plan, err := e.planner.Plan(changes, bl, mode, safety, blockedBoundaries)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	e.collector().RecordPlan(len(plan.Actions), e.since(planStart))

	counts := CountByType(plan.Actions)
	report := buildReportFromCounts(counts, plan.DeferredByMode, mode, opts)

	return e.completeDryRunReport(start, report), nil
}

func (flow *engineFlow) materializeCurrentActionPlan(ctx context.Context, plan *ActionPlan, dryRun bool) error {
	if dryRun {
		return nil
	}

	if err := flow.engine.baseline.PruneRetryStateToCurrentActions(ctx, retryWorkKeysForActions(plan.Actions)); err != nil {
		return fmt.Errorf("sync: pruning retry_state to current actions: %w", err)
	}

	if err := flow.engine.baseline.PruneScopeBlocksWithoutBlockedRetries(ctx); err != nil {
		return fmt.Errorf("sync: pruning scope blocks without blocked retries: %w", err)
	}

	return nil
}

func (e *Engine) prepareRunOnceBaseline(
	ctx context.Context,
	runner *oneShotRunner,
) (*Baseline, error) {
	if err := runner.prepareRunOnceState(ctx); err != nil {
		return nil, err
	}
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline after startup preparation: %w", err)
	}

	return bl, nil
}

func (e *Engine) completeRunOnceWithoutChanges(
	ctx context.Context,
	start time.Time,
	mode Mode,
	opts RunOptions,
) *Report {
	e.logger.Info("sync pass complete: no changes detected",
		slog.Duration("duration", e.since(start)),
	)

	report := &Report{
		Mode:     mode,
		DryRun:   opts.DryRun,
		Duration: e.since(start),
	}
	// Persist one-shot status even when no changes are detected.
	if metaErr := e.baseline.WriteSyncRunStatus(ctx, &SyncRunReport{
		CompletedAt: e.nowFunc(),
		Duration:    report.Duration,
		Succeeded:   report.Succeeded,
		Failed:      report.Failed,
		Errors:      report.Errors,
	}); metaErr != nil {
		e.logger.Warn("failed to write one-shot sync status", slog.String("error", metaErr.Error()))
	}

	return report
}

func (e *Engine) completeDryRunReport(start time.Time, report *Report) *Report {
	report.Duration = e.since(start)

	e.logger.Info("dry-run complete: no changes applied",
		slog.Duration("duration", report.Duration),
		slog.Int("deferred_folder_creates", report.DeferredByMode.FolderCreates),
		slog.Int("deferred_moves", report.DeferredByMode.Moves),
		slog.Int("deferred_downloads", report.DeferredByMode.Downloads),
		slog.Int("deferred_uploads", report.DeferredByMode.Uploads),
		slog.Int("deferred_local_deletes", report.DeferredByMode.LocalDeletes),
		slog.Int("deferred_remote_deletes", report.DeferredByMode.RemoteDeletes),
	)

	return report
}

func (e *Engine) logRunOnceCompletion(report *Report) {
	if report == nil {
		return
	}

	e.logger.Info("sync pass complete",
		slog.Duration("duration", report.Duration),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", report.Failed),
		slog.Int("deferred_folder_creates", report.DeferredByMode.FolderCreates),
		slog.Int("deferred_moves", report.DeferredByMode.Moves),
		slog.Int("deferred_downloads", report.DeferredByMode.Downloads),
		slog.Int("deferred_uploads", report.DeferredByMode.Uploads),
		slog.Int("deferred_local_deletes", report.DeferredByMode.LocalDeletes),
		slog.Int("deferred_remote_deletes", report.DeferredByMode.RemoteDeletes),
	)
}

func (r *oneShotRunner) prepareRunOnceState(ctx context.Context) error {
	eng := r.engine
	flow := &r.engineFlow

	hasAccountAuthRequirement, err := eng.hasPersistedAccountAuthRequirement()
	if err != nil {
		return err
	}

	proof, proofErr := eng.proveDriveIdentity(ctx)
	if proofErr != nil {
		if !hasAccountAuthRequirement {
			return proofErr
		}
	}

	repairErr := flow.scopeController().repairPersistedScopes(ctx, nil)
	if repairErr != nil {
		return fmt.Errorf("sync: repairing persisted scopes: %w", repairErr)
	}
	authRepairErr := eng.repairPersistedAccountAuthRequirement(ctx, hasAccountAuthRequirement, proof, proofErr)
	if authRepairErr != nil {
		return authRepairErr
	}
	if proofErr != nil {
		return proofErr
	}
	eng.logVerifiedDrive(proof)

	bl, err := eng.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline: %w", err)
	}

	// Recheck permissions — clear any permission_denied issues
	// for folders that have become writable since the last pass.
	if eng.permHandler.HasPermChecker() {
		decisions := eng.permHandler.recheckPermissions(ctx, bl)
		flow.scopeController().applyPermissionRecheckDecisions(ctx, nil, decisions)
	}

	// Recheck local permission denials — clear scope blocks for
	// directories that have become accessible since the last pass (R-2.10.13).
	flow.scopeController().applyPermissionRecheckDecisions(ctx, nil, eng.permHandler.recheckLocalPermissions(ctx))

	return nil
}

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (flow *engineFlow) postSyncHousekeeping() {
	driveops.CleanTransferArtifacts(flow.engine.syncTree, flow.engine.sessionStore, flow.engine.logger)
}

// executePlan populates the dependency graph and runs the worker pool.
// The engine processes results concurrently while workers run, classifying
// each result and calling depGraph.Complete (R-6.8.9).
//
// One-shot mode has NO watch-mode active-scope admission loop — all actions
// with satisfied deps go directly to dispatchCh. Scope detection (ScopeState) is
// absent in one-shot; watch-only lifecycle paths are nil-guarded → no-op.
func (r *oneShotRunner) executePlan(
	ctx context.Context, plan *ActionPlan, report *Report,
	bl *Baseline,
) error {
	if len(plan.Actions) == 0 {
		return nil
	}

	// Invariant: Planner.Plan() always builds Deps with len(Actions).
	// Assert here to catch any future regression that breaks this contract.
	if len(plan.Actions) != len(plan.Deps) {
		r.engine.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		report.Failed = len(plan.Actions)
		report.Errors = append(report.Errors,
			fmt.Errorf("plan invariant violation: %d actions but %d deps", len(plan.Actions), len(plan.Deps)))

		return nil
	}

	// Reset engine counters for this pass.
	r.resetResultStats()

	// One-shot mode: DepGraph + dispatchCh, no watch-mode active-scope admission
	// loop (e.watch == nil). Actions that pass dependency resolution go
	// straight to workers. Scope blocking is watch-mode only (§2.3).
	depGraph := NewDepGraph(r.engine.logger)
	r.depGraph = depGraph
	r.dispatchCh = make(chan *TrackedAction, len(plan.Actions))

	// Two-phase graph population: Register all actions first, then wire
	// dependencies. This avoids forward-reference issues where a parent
	// folder delete at index 0 depends on a child file delete at index 5 —
	// single-pass Add would silently drop the unregistered depID.
	for i := range plan.Actions {
		r.setDispatch(ctx, &plan.Actions[i])
		depGraph.Register(&plan.Actions[i], int64(i))
	}

	for i := range plan.Actions {
		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, int64(depIdx))
		}

		if ta := depGraph.WireDeps(int64(i), depIDs); ta != nil {
			r.dispatchCh <- ta
		}
	}

	pool := NewWorkerPool(r.engine.execCfg, r.dispatchCh, depGraph.Done(), r.engine.baseline, r.engine.logger, len(plan.Actions))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool.Start(runCtx, r.engine.transferWorkers)

	// Process results concurrently — engine classifies and calls Complete.
	// The one-shot engine loop reads from the results channel while workers
	// run. drainDone signals when it has finished processing all results,
	// including side effects (counter updates, failure recording). Without
	// this barrier, resultStats() could race with result processing.
	drainDone := make(chan struct{})
	var drainErr error
	go func() {
		defer close(drainDone)
		drainErr = r.runResultsLoop(runCtx, cancel, bl, pool.Results())
	}()

	pool.Wait() // blocks until depGraph.Done() (all actions at terminal state)
	pool.Stop() // cancels workers and closes results once workers exit
	<-drainDone // wait for the one-shot engine loop to finish all side effects
	if drainErr != nil {
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		report.Errors = append(report.Errors, drainErr)
		return drainErr
	}

	// End-of-pass failure summary — aggregates failures by issue type so
	// bulk sync produces WARN summaries instead of per-item noise (R-6.6.12).
	r.logFailureSummary()

	report.Succeeded, report.Failed, report.Errors = r.resultStats()
	return nil
}

// buildReportFromCounts populates a Report with plan counts and directionally
// deferred work observed by the planner.
func buildReportFromCounts(
	counts map[ActionType]int,
	deferred DeferredCounts,
	mode Mode,
	opts RunOptions,
) *Report {
	return &Report{
		Mode:           mode,
		DryRun:         opts.DryRun,
		FolderCreates:  counts[ActionFolderCreate],
		Moves:          counts[ActionLocalMove] + counts[ActionRemoteMove],
		Downloads:      counts[ActionDownload],
		Uploads:        counts[ActionUpload],
		LocalDeletes:   counts[ActionLocalDelete],
		RemoteDeletes:  counts[ActionRemoteDelete],
		Conflicts:      counts[ActionConflict],
		SyncedUpdates:  counts[ActionUpdateSynced],
		Cleanups:       counts[ActionCleanup],
		DeferredByMode: deferred,
	}
}

// observeRemote fetches delta changes from the Graph API. Automatically
// retries with an empty token if ErrDeltaExpired is returned (full resync).
func (flow *engineFlow) observeRemote(ctx context.Context, bl *Baseline) ([]ChangeEvent, string, error) {
	eng := flow.engine
	state, err := eng.baseline.ReadObservationState(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("sync: reading observation state: %w", err)
	}
	savedToken := state.Cursor

	obs := NewRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)

	events, token, err := obs.FullDelta(ctx, savedToken)
	if err != nil {
		if !errors.Is(err, ErrDeltaExpired) {
			return nil, "", fmt.Errorf("sync: observing remote delta: %w", err)
		}

		// Delta token expired — retry with empty token for full resync.
		eng.logger.Warn("delta token expired, performing full resync")

		events, token, err = obs.FullDelta(ctx, "")
		if err != nil {
			return nil, "", fmt.Errorf("sync: full resync after delta expiry: %w", err)
		}
	}

	return events, token, nil
}

// observeLocal scans the local filesystem for changes and collects skipped
// items (invalid names, path too long, file too large) for failure recording.
// The observer also receives platform-derived naming rules from the engine so
// SharePoint-specific validation stays aligned across one-shot, watch, and
// retry/trial observation paths.
func (flow *engineFlow) observeLocal(
	ctx context.Context,
	bl *Baseline,
) (ScanResult, error) {
	eng := flow.engine

	obs := NewLocalObserver(bl, eng.logger, eng.checkWorkers)
	obs.SetFilterConfig(eng.localFilter)
	obs.SetObservationRules(eng.localRules)

	result, err := obs.FullScan(ctx, eng.syncTree)
	if err != nil {
		return ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
}

// observeChanges runs remote and local observers based on mode, buffers their
// events, and returns the flushed change set plus a pending delta token.
//
// Observations (remote_state rows) are committed immediately. The delta token
// is returned but NOT committed — the caller must commit it only after the
// planner approves the changes. Skipped entirely for dry-run.
//
// When fullReconcile is true, runs a fresh delta with empty token (enumerates
// ALL remote items) and detects orphans — baseline entries not in the full
// enumeration, representing missed delta deletions.
func (flow *engineFlow) observeCurrentTruth(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	dryRun, fullReconcile bool,
) (*pendingPrimaryCursorCommit, error) {
	plan := flow.buildPrimaryRootObservationPlan(fullReconcile)
	remoteEvents, pendingCursorCommit, err := flow.observeRemoteChanges(
		ctx, bl, dryRun, plan,
	)
	if err != nil {
		return nil, err
	}
	_ = remoteEvents

	localResult, err := flow.observeLocalChanges(ctx, watch, bl)
	if err != nil {
		return nil, err
	}
	if commitLocalErr := flow.commitObservedLocalSnapshot(ctx, dryRun, localResult); commitLocalErr != nil {
		return nil, commitLocalErr
	}

	return pendingCursorCommit, nil
}

// observeChanges remains the legacy watch/bootstrap observation path until the
// long-lived runtime is cut over to snapshot refresh + SQLite reconciliation.
func (flow *engineFlow) observeChanges(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	dryRun, fullReconcile bool,
) ([]PathChanges, *pendingPrimaryCursorCommit, error) {
	plan := flow.buildPrimaryRootObservationPlan(fullReconcile)
	remoteEvents, pendingCursorCommit, err := flow.observeRemoteChanges(
		ctx, bl, dryRun, plan,
	)
	if err != nil {
		return nil, nil, err
	}

	finalRemoteEvents := flow.processCommittedPrimaryBatch(
		ctx,
		bl,
		remoteEvents,
		dryRun,
		fullReconcile,
	)

	localResult, err := flow.observeLocalChanges(ctx, watch, bl)
	if err != nil {
		return nil, nil, err
	}
	if commitLocalErr := flow.commitObservedLocalSnapshot(ctx, dryRun, localResult); commitLocalErr != nil {
		return nil, nil, commitLocalErr
	}

	buf := NewBuffer(flow.engine.logger)
	buf.AddAll(finalRemoteEvents)
	buf.AddAll(localResult.Events)
	remoteDriftEvents, err := flow.synthesizeRemoteMirrorDrift(ctx, bl)
	if err != nil {
		return nil, nil, err
	}
	buf.AddAll(remoteDriftEvents)

	return buf.FlushImmediate(), pendingCursorCommit, nil
}

func (flow *engineFlow) buildCurrentActionPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) (*ActionPlan, error) {
	comparisons, err := flow.engine.baseline.QueryComparisonState(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: querying comparison state: %w", err)
	}
	reconciliations, err := flow.engine.baseline.QueryReconciliationState(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: querying reconciliation state: %w", err)
	}
	localRows, err := flow.engine.baseline.ListLocalState(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing local_state rows: %w", err)
	}
	remoteRows, err := flow.engine.baseline.ListRemoteState(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing remote_state rows: %w", err)
	}

	return flow.engine.planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		bl,
		mode,
		safety,
	)
}

func (r *oneShotRunner) materializeSQLitePlan(ctx context.Context, plan *ActionPlan, dryRun bool) error {
	return r.materializeCurrentActionPlan(ctx, plan, dryRun)
}

func (r *oneShotRunner) buildSQLiteActionPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) (*ActionPlan, error) {
	return r.buildCurrentActionPlan(ctx, bl, mode, safety)
}

func retryWorkKeysForActions(actions []Action) []RetryWorkKey {
	keys := make([]RetryWorkKey, 0, len(actions))
	seen := make(map[RetryWorkKey]struct{}, len(actions))

	for i := range actions {
		key := RetryWorkKey{
			Path:       actions[i].Path,
			ActionType: actions[i].Type,
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	return keys
}

func (flow *engineFlow) observeRemoteChanges(
	ctx context.Context,
	bl *Baseline,
	dryRun bool,
	plan primaryRootObservationPlan,
) ([]ChangeEvent, *pendingPrimaryCursorCommit, error) {
	fetchResult, err := flow.fetchRemoteChanges(ctx, bl, plan)
	if err != nil {
		return nil, nil, err
	}

	// Dry-run previews must never advance remote observation cursors.
	if dryRun {
		fetchResult.pending = nil
	}

	projectedRemote := projectRemoteObservations(flow.engine.logger, fetchResult.events)
	if err := flow.commitObservedRemoteChanges(
		ctx,
		dryRun,
		projectedRemote.observed,
	); err != nil {
		return nil, nil, err
	}

	return projectedRemote.emitted, fetchResult.pending, nil
}

func (flow *engineFlow) fetchRemoteChanges(
	ctx context.Context,
	bl *Baseline,
	plan primaryRootObservationPlan,
) (remoteFetchResult, error) {
	return flow.executePrimaryRootObservation(ctx, bl, plan)
}

func (flow *engineFlow) commitObservedRemoteChanges(
	ctx context.Context,
	dryRun bool,
	observed []ObservedItem,
) error {
	if dryRun {
		return nil
	}

	if len(observed) == 0 {
		return nil
	}

	if err := flow.commitObservedItems(ctx, observed, ""); err != nil {
		return err
	}

	return nil
}

func (flow *engineFlow) observeLocalChanges(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
) (ScanResult, error) {
	localResult, err := flow.observeLocal(ctx, bl)
	if err != nil {
		return ScanResult{}, err
	}

	flow.recordSkippedItems(ctx, localResult.Skipped)
	flow.clearResolvedSkippedItems(ctx, localResult.Skipped)

	pathSet := pathSetFromLocalRows(localResult.Rows)
	flow.scopeController().applyPermissionRecheckDecisions(
		ctx,
		watch,
		flow.engine.permHandler.clearScannerResolvedPermissions(ctx, pathSet),
	)
	flow.scopeController().clearResolvedRemoteBlockedFailures(ctx, watch, pathSet)

	return localResult, nil
}

func (flow *engineFlow) commitObservedLocalSnapshot(
	ctx context.Context,
	dryRun bool,
	localResult ScanResult,
) error {
	if dryRun {
		return nil
	}

	observedAt := flow.engine.nowFunc().UnixNano()
	rows := buildLocalStateRows(localResult, observedAt)
	if err := flow.engine.baseline.ReplaceLocalState(ctx, rows); err != nil {
		return fmt.Errorf("sync: replacing local_state snapshot: %w", err)
	}
	mode := localRefreshModeWatchHealthy
	state, err := flow.engine.baseline.ReadObservationState(ctx)
	if err == nil && state != nil {
		mode = state.LocalRefreshMode
	}
	if err := flow.engine.baseline.MarkFullLocalRefresh(
		ctx,
		flow.engine.driveID,
		time.Unix(0, observedAt),
		mode,
	); err != nil {
		return fmt.Errorf("sync: marking full local refresh: %w", err)
	}

	return nil
}

func (flow *engineFlow) synthesizeRemoteMirrorDrift(
	ctx context.Context,
	bl *Baseline,
) ([]ChangeEvent, error) {
	rows, err := flow.engine.baseline.ListRemoteState(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing remote mirror state: %w", err)
	}

	seenRemote := make(map[string]struct{}, len(rows))
	events := make([]ChangeEvent, 0, len(rows))

	for i := range rows {
		row := &rows[i]

		seenRemote[row.ItemID] = struct{}{}

		entry, found := bl.GetByID(row.ItemID)
		if found && !remoteMirrorDiffers(entry, row) {
			continue
		}

		event := ChangeEvent{
			Source:   SourceRemote,
			Type:     ChangeModify,
			Path:     row.Path,
			ItemID:   row.ItemID,
			ParentID: row.ParentID,
			DriveID:  row.DriveID,
			ItemType: row.ItemType,
			Name:     filepath.Base(row.Path),
			Size:     row.Size,
			Hash:     row.Hash,
			Mtime:    row.Mtime,
			ETag:     row.ETag,
		}
		if found && entry.Path != row.Path {
			event.Type = ChangeMove
			event.OldPath = entry.Path
		}

		events = append(events, event)
	}

	bl.ForEachPath(func(path string, entry *BaselineEntry) {
		if entry == nil {
			return
		}

		if _, ok := seenRemote[entry.ItemID]; ok {
			return
		}

		events = append(events, ChangeEvent{
			Source:    SourceRemote,
			Type:      ChangeDelete,
			Path:      path,
			ItemID:    entry.ItemID,
			ParentID:  entry.ParentID,
			DriveID:   entry.DriveID,
			ItemType:  entry.ItemType,
			Name:      filepath.Base(path),
			Size:      entry.RemoteSize,
			Hash:      entry.RemoteHash,
			Mtime:     entry.RemoteMtime,
			IsDeleted: true,
		})
	})

	sort.Slice(events, func(i, j int) bool {
		if events[i].Path != events[j].Path {
			return events[i].Path < events[j].Path
		}
		if events[i].Type != events[j].Type {
			return events[i].Type < events[j].Type
		}
		return events[i].OldPath < events[j].OldPath
	})

	return events, nil
}

func remoteMirrorDiffers(entry *BaselineEntry, row *RemoteStateRow) bool {
	if entry == nil || row == nil {
		return true
	}
	if entry.Path != row.Path || entry.ItemType != row.ItemType {
		return true
	}
	if entry.RemoteHash != row.Hash {
		return true
	}
	if entry.RemoteSizeKnown && entry.RemoteSize != row.Size {
		return true
	}
	return entry.RemoteMtime != row.Mtime
}

// commitPendingPrimaryCursor advances the primary observation cursor after the
// planner approves the changes. Full reconciliations also persist the restart-
// safe full-remote cadence timestamp here.
func (flow *engineFlow) commitPendingPrimaryCursor(
	ctx context.Context,
	pending *pendingPrimaryCursorCommit,
) error {
	if pending == nil || pending.token == "" {
		return nil
	}

	if err := flow.engine.baseline.CommitObservationCursor(
		ctx,
		driveid.New(pending.driveID),
		pending.token,
	); err != nil {
		return fmt.Errorf("sync: committing primary observation cursor for root %q: %w", pending.rootID, err)
	}
	if pending.markFullRemoteReconcile {
		if err := flow.engine.baseline.MarkFullRemoteReconcile(
			ctx,
			driveid.New(pending.driveID),
			flow.engine.nowFunc(),
		); err != nil {
			return fmt.Errorf("sync: marking full remote reconcile for root %q: %w", pending.rootID, err)
		}
	}

	return nil
}

// observeRemoteFull runs a fresh delta with empty token (enumerates ALL remote
// items) and compares against the baseline to find orphans: items in baseline
// but not in the full enumeration = deleted remotely but missed by incremental
// delta. Returns all events (creates/modifies from the full enumeration +
// synthesized deletes for orphans) and the new delta token.
func (flow *engineFlow) observeRemoteFull(ctx context.Context, bl *Baseline) ([]ChangeEvent, string, error) {
	eng := flow.engine
	obs := NewRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)

	// Full enumeration: empty token returns ALL items as create/modify events.
	events, token, err := obs.FullDelta(ctx, "")
	if err != nil {
		return nil, "", fmt.Errorf("sync: full reconciliation delta: %w", err)
	}

	// Build seen set from all non-deleted events in the full enumeration.
	seen := make(map[string]struct{}, len(events))
	for i := range events {
		if events[i].IsDeleted {
			continue
		}

		seen[events[i].ItemID] = struct{}{}
	}

	// Detect orphans: baseline entries whose ItemID is not in the seen set.
	orphans := findBaselineOrphans(bl, seen, eng.driveID, "")

	if len(orphans) > 0 {
		eng.logger.Info("full reconciliation: detected orphaned items",
			slog.Int("orphans", len(orphans)),
		)

		events = append(events, orphans...)
	}

	eng.logger.Info("full reconciliation complete",
		slog.Int("total_events", len(events)),
		slog.Int("orphans", len(orphans)),
	)

	return events, token, nil
}

// resolveSafetyConfig returns the planner safety settings for a run-once pass.
// Batch delete protection is disabled; only per-item executor-time safety
// checks remain.
func (e *Engine) resolveSafetyConfig() *SafetyConfig {
	return &SafetyConfig{}
}
