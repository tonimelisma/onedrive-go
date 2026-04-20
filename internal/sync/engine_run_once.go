package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// RunOnce executes a single sync pass:
//  1. Load baseline
//  2. Refresh remote truth
//  3. Refresh local truth
//  4. Read SQLite comparison and reconciliation from committed snapshots
//  5. Build the current actionable set in Go
//  6. Return early if dry-run
//  7. Build DepGraph, start worker pool
//  8. Wait for completion, commit delta token
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
	plan, err := runner.buildCurrentActionPlan(ctx, bl, mode, safety)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	e.collector().RecordPlan(len(plan.Actions), e.since(planStart))

	if materializeErr := runner.materializeCurrentActionPlan(ctx, plan, opts.DryRun); materializeErr != nil {
		return nil, materializeErr
	}

	// SQLite-derived plan approved — commit the deferred delta token now.
	if err := runner.commitPendingPrimaryCursor(ctx, pendingCursorCommit); err != nil {
		return nil, err
	}

	counts := CountByType(plan.Actions)
	report := buildReportFromCounts(counts, CountConflicts(plan.Actions), plan.DeferredByMode, mode, opts)

	if len(plan.Actions) == 0 {
		if report.DeferredByMode.Total() > 0 {
			report.Duration = e.since(start)
			e.logRunOnceCompletion(report)
			e.writeOneShotRunStatusBestEffort(ctx, report)
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

	e.writeOneShotRunStatusBestEffort(ctx, report)

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
	planResult, err := runner.buildDryRunCurrentActionPlan(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	e.collector().RecordObserve(planResult.observedPaths, e.since(observeStart))

	safety := e.resolveSafetyConfig()

	planStart := e.nowFunc()
	plan, err := e.buildCurrentActionPlanFromInputs(&planResult.currentActionPlanInputs, bl, mode, safety)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	e.collector().RecordPlan(len(plan.Actions), e.since(planStart))

	counts := CountByType(plan.Actions)
	report := buildReportFromCounts(counts, CountConflicts(plan.Actions), plan.DeferredByMode, mode, opts)

	return e.completeDryRunReport(start, report), nil
}

type dryRunPlanInput struct {
	currentActionPlanInputs
	observedPaths int
}

type currentActionPlanInputs struct {
	comparisons       []SQLiteComparisonRow
	reconciliations   []SQLiteReconciliationRow
	localRows         []LocalStateRow
	remoteRows        []RemoteStateRow
	observationIssues []ObservationIssueRow
	blockScopes       []*BlockScope
}

func (flow *engineFlow) materializeCurrentActionPlan(ctx context.Context, plan *ActionPlan, dryRun bool) error {
	if dryRun {
		return nil
	}

	if err := flow.engine.baseline.PruneRetryWorkToCurrentActions(ctx, retryWorkKeysForActions(plan.Actions)); err != nil {
		return fmt.Errorf("sync: pruning retry_work to current actions: %w", err)
	}

	if err := flow.engine.baseline.PruneBlockScopesWithoutBlockedWork(ctx); err != nil {
		return fmt.Errorf("sync: pruning block scopes without blocked work: %w", err)
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
	e.writeOneShotRunStatusBestEffort(ctx, report)

	return report
}

func (e *Engine) writeOneShotRunStatusBestEffort(ctx context.Context, report *Report) {
	if report == nil {
		return
	}

	if metaErr := e.baseline.WriteSyncRunStatus(ctx, &SyncRunReport{
		CompletedAt: e.nowFunc(),
		Duration:    report.Duration,
		Succeeded:   report.Succeeded,
		Failed:      report.Failed,
		Errors:      report.Errors,
	}); metaErr != nil {
		e.logger.Warn("failed to write one-shot sync status", slog.String("error", metaErr.Error()))
	}
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

	normalizeErr := flow.scopeController().normalizePersistedScopes(ctx, nil)
	if normalizeErr != nil {
		return fmt.Errorf("sync: normalizing persisted scopes: %w", normalizeErr)
	}
	authNormalizeErr := eng.normalizePersistedAccountAuthRequirement(ctx, hasAccountAuthRequirement, proof, proofErr)
	if authNormalizeErr != nil {
		return authNormalizeErr
	}
	if proofErr != nil {
		return proofErr
	}
	eng.logVerifiedDrive(proof)

	bl, err := eng.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline: %w", err)
	}

	flow.scopeController().runStartupPermissionMaintenance(ctx, nil, eng.permHandler, bl)

	return nil
}

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (flow *engineFlow) postSyncHousekeeping() {
	driveops.CleanTransferArtifacts(flow.engine.syncTree, flow.engine.sessionStore, flow.engine.logger)
}

func (r *oneShotRunner) dispatchInitialReadyActions(
	ctx context.Context,
	bl *Baseline,
	depGraph *DepGraph,
	initialReady []*TrackedAction,
	report *Report,
) ([]*TrackedAction, bool, error) {
	initialOutbox, err := r.drainPublicationReadyActions(ctx, nil, bl, nil, initialReady)
	if err != nil {
		r.completeOutboxAsShutdown(initialOutbox)
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		report.Errors = append(report.Errors, err)
		return nil, true, err
	}

	if depGraph.InFlightCount() == 0 {
		r.logFailureSummary()
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		return nil, true, nil
	}

	return initialOutbox, false, nil
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

	// Invariant: the current actionable-set builder always returns one
	// dependency slice per action. Assert here to catch regressions early.
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
	initialReady := make([]*TrackedAction, 0, len(plan.Actions))

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
			initialReady = append(initialReady, ta)
		}
	}

	initialOutbox, done, err := r.dispatchInitialReadyActions(ctx, bl, depGraph, initialReady, report)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	for _, ta := range initialOutbox {
		r.dispatchCh <- ta
	}

	pool := NewWorkerPool(r.engine.execCfg, r.dispatchCh, depGraph.Done(), r.engine.baseline, r.engine.logger, len(plan.Actions))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool.Start(runCtx, r.engine.transferWorkers)

	// Process completions concurrently — engine classifies and calls Complete.
	// The one-shot engine loop reads from the completions channel while workers
	// run. drainDone signals when it has finished processing all completions,
	// including side effects (counter updates, failure recording). Without
	// this barrier, resultStats() could race with result processing.
	drainDone := make(chan struct{})
	var drainErr error
	go func() {
		defer close(drainDone)
		drainErr = r.runResultsLoop(runCtx, cancel, bl, pool.Completions())
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
	conflicts int,
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
		Conflicts:      conflicts,
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

func (e *Engine) buildCurrentActionPlanFromInputs(
	inputs *currentActionPlanInputs,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) (*ActionPlan, error) {
	return e.planner.PlanCurrentState(
		inputs.comparisons,
		inputs.reconciliations,
		inputs.localRows,
		inputs.remoteRows,
		inputs.observationIssues,
		inputs.blockScopes,
		bl,
		mode,
		safety,
	)
}

func (flow *engineFlow) loadCurrentActionPlanInputs(
	ctx context.Context,
	store *SyncStore,
	defaultDriveID driveid.ID,
) (currentActionPlanInputs, error) {
	tx, err := beginPerfTx(ctx, store.db)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: beginning current action planner read transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			flow.engine.logger.Debug("current action planner read transaction rollback failed",
				slog.String("error", rollbackErr.Error()),
			)
		}
	}()

	return flow.loadCurrentActionPlanInputsTx(ctx, store, tx, defaultDriveID)
}

func (flow *engineFlow) loadCurrentActionPlanInputsTx(
	ctx context.Context,
	store *SyncStore,
	tx sqlTxRunner,
	defaultDriveID driveid.ID,
) (currentActionPlanInputs, error) {
	comparisons, err := queryComparisonStateWithRunner(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: querying comparison state: %w", err)
	}
	reconciliations, err := queryReconciliationStateWithRunner(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: querying reconciliation state: %w", err)
	}
	localRows, err := listLocalStateRows(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing local_state rows: %w", err)
	}
	observationState, err := store.readObservationStateTx(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: reading observation state for remote_state: %w", err)
	}
	configuredDriveID := observationState.ConfiguredDriveID
	if configuredDriveID.IsZero() {
		configuredDriveID = defaultDriveID
	}
	remoteRows, err := queryRemoteStateRowsWithRunner(
		ctx,
		tx,
		`SELECT item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path
		FROM remote_state`,
		configuredDriveID,
	)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing remote_state rows: %w", err)
	}
	observationIssues, err := queryObservationIssueRowsDB(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing observation issues: %w", err)
	}
	blockScopes, err := queryBlockScopesDB(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing block scopes: %w", err)
	}

	return currentActionPlanInputs{
		comparisons:       comparisons,
		reconciliations:   reconciliations,
		localRows:         localRows,
		remoteRows:        remoteRows,
		observationIssues: observationIssues,
		blockScopes:       blockScopes,
	}, nil
}

func (flow *engineFlow) buildCurrentActionPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) (*ActionPlan, error) {
	inputs, err := flow.loadCurrentActionPlanInputs(ctx, flow.engine.baseline, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return flow.engine.buildCurrentActionPlanFromInputs(&inputs, bl, mode, safety)
}

func (flow *engineFlow) buildDryRunCurrentActionPlan(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (result *dryRunPlanInput, err error) {
	plan := flow.buildPrimaryRootObservationPlan(fullReconcile)
	fetchResult, err := flow.fetchRemoteChanges(ctx, bl, plan)
	if err != nil {
		return nil, err
	}

	projectedRemote := projectRemoteObservations(flow.engine.logger, fetchResult.events)

	localResult, err := flow.observeLocalChanges(ctx, nil, bl)
	if err != nil {
		return nil, err
	}

	scratchStore, cleanup, err := flow.engine.baseline.createScratchPlanningStore(ctx, bl)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanupErr := cleanup(context.WithoutCancel(ctx)); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	commitErr := scratchStore.CommitObservation(ctx, projectedRemote.observed, "", flow.engine.driveID)
	if commitErr != nil {
		return nil, fmt.Errorf("sync: committing dry-run remote snapshot to scratch store: %w", commitErr)
	}
	if reconcileErr := scratchStore.ReconcileObservationFindings(ctx, &fetchResult.findings, flow.engine.nowFunc()); reconcileErr != nil {
		return nil, fmt.Errorf("sync: reconciling dry-run remote observation findings in scratch store: %w", reconcileErr)
	}

	observedAt := flow.engine.nowFunc().UnixNano()
	localRows := buildLocalStateRows(localResult, observedAt)
	replaceErr := scratchStore.ReplaceLocalState(ctx, localRows)
	if replaceErr != nil {
		return nil, fmt.Errorf("sync: replacing dry-run local snapshot in scratch store: %w", replaceErr)
	}

	inputs, err := flow.loadCurrentActionPlanInputs(ctx, scratchStore, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return &dryRunPlanInput{
		currentActionPlanInputs: inputs,
		observedPaths:           len(inputs.localRows) + len(inputs.remoteRows),
	}, nil
}

func retryWorkKeysForActions(actions []Action) []RetryWorkKey {
	keys := make([]RetryWorkKey, 0, len(actions))
	seen := make(map[RetryWorkKey]struct{}, len(actions))

	for i := range actions {
		key := retryWorkKeyForAction(&actions[i])
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
	if !dryRun {
		flow.reconcileObservationFindingsBatch(
			ctx,
			nil,
			&fetchResult.findings,
			"failed to reconcile remote observation findings",
		)
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

	flow.reconcileSkippedObservationFindings(ctx, watch, localResult.Skipped)

	pathSet := pathSetFromLocalRows(localResult.Rows)
	flow.scopeController().clearResolvedRemoteBlockedRetryWork(ctx, watch, pathSet)

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
