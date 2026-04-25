package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func (r *oneShotRunner) runLiveCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode SyncMode,
	opts RunOptions,
) (*runtimePlan, error) {
	observeStart := r.engine.nowFunc()
	pendingRemoteObservation, err := r.observeAndCommitCurrentState(ctx, bl, false, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	observation, err := r.loadCommittedCurrentObservation(ctx, pendingRemoteObservation)
	if err != nil {
		return nil, err
	}
	r.engine.collector().RecordObserve(observation.observedPaths, r.engine.since(observeStart))

	build, err := r.buildCurrentPlanStage(ctx, bl, mode, opts, observation)
	if err != nil {
		return nil, err
	}

	return r.reconcileRuntimeStateStage(ctx, build)
}

func (r *oneShotRunner) runDryRunCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode SyncMode,
	opts RunOptions,
) (*runtimePlan, error) {
	observeStart := r.engine.nowFunc()
	observation, err := r.loadDryRunCurrentObservation(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	r.engine.collector().RecordObserve(observation.observedPaths, r.engine.since(observeStart))

	build, err := r.buildCurrentPlanStage(ctx, bl, mode, opts, observation)
	if err != nil {
		return nil, err
	}

	return r.keepBuiltCurrentPlan(build), nil
}

func (rt *watchRuntime) runBootstrapCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode SyncMode,
) (*runtimePlan, error) {
	fullRefresh, err := rt.engine.shouldRunFullRemoteRefresh(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("sync: deciding bootstrap full remote refresh: %w", err)
	}

	observeStart := rt.engine.nowFunc()
	pendingRemoteObservation, err := rt.observeAndCommitCurrentState(ctx, bl, false, fullRefresh)
	if err != nil {
		return nil, fmt.Errorf("sync: bootstrap observation failed: %w", err)
	}
	observation, err := rt.loadCommittedCurrentObservation(ctx, pendingRemoteObservation)
	if err != nil {
		return nil, fmt.Errorf("sync: bootstrap load_current_inputs: %w", err)
	}
	rt.engine.collector().RecordObserve(observation.observedPaths, rt.engine.since(observeStart))

	build, err := rt.buildCurrentPlanStage(ctx, bl, mode, RunOptions{}, observation)
	if err != nil {
		return nil, err
	}

	return rt.reconcileRuntimeStateStage(ctx, build)
}

func (rt *watchRuntime) runSteadyStateCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode SyncMode,
) (*runtimePlan, error) {
	observation, err := rt.loadCommittedCurrentObservation(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sync: watch load_current_inputs: %w", err)
	}

	build, err := rt.buildCurrentPlanStage(ctx, bl, mode, RunOptions{}, observation)
	if err != nil {
		return nil, err
	}

	return rt.reconcileRuntimeStateStage(ctx, build)
}

type currentObservation struct {
	inputs                   currentInputs
	observedPaths            int
	pendingRemoteObservation *remoteObservationBatch
}

type localCurrentRefreshStep string

const (
	localCurrentRefreshStepObservation       localCurrentRefreshStep = "local_observation"
	localCurrentRefreshStepFindingsReconcile localCurrentRefreshStep = "local_observation_findings_reconcile"
	localCurrentRefreshStepSnapshotCommit    localCurrentRefreshStep = "local_snapshot_commit"
)

type localCurrentRefreshError struct {
	step localCurrentRefreshStep
	err  error
}

func (e localCurrentRefreshError) Error() string {
	if e.err == nil {
		return ""
	}

	return e.err.Error()
}

func (e localCurrentRefreshError) Unwrap() error {
	return e.err
}

func localCurrentRefreshFailure(step localCurrentRefreshStep, err error) error {
	if err == nil {
		return nil
	}

	return localCurrentRefreshError{step: step, err: err}
}

func currentLocalRefreshStep(err error) (localCurrentRefreshStep, bool) {
	var refreshErr localCurrentRefreshError
	if !errors.As(err, &refreshErr) {
		return "", false
	}

	return refreshErr.step, true
}

type builtCurrentPlan struct {
	Plan                     *ActionPlan
	Report                   *Report
	PendingRemoteObservation *remoteObservationBatch
}

type runtimePlan struct {
	Plan                     *ActionPlan
	Report                   *Report
	PendingRemoteObservation *remoteObservationBatch
	RetryRows                []RetryWorkRow
	BlockScopes              []*BlockScope
}

type currentInputs struct {
	comparisons       []SQLiteComparisonRow
	reconciliations   []SQLiteReconciliationRow
	localRows         []LocalStateRow
	remoteRows        []RemoteStateRow
	observationIssues []ObservationIssueRow
}

func (flow *engineFlow) observeAndCommitCurrentState(
	ctx context.Context,
	bl *Baseline,
	dryRun, fullReconcile bool,
) (*remoteObservationBatch, error) {
	_, pendingRemoteObservation, err := flow.observeAndCommitRemoteCurrentState(ctx, bl, dryRun, fullReconcile)
	if err != nil {
		return nil, err
	}

	if _, err := flow.refreshAndCommitLocalCurrentState(ctx, bl, dryRun); err != nil {
		return nil, err
	}

	return pendingRemoteObservation, nil
}

func (flow *engineFlow) observeAndCommitRemoteCurrentState(
	ctx context.Context,
	bl *Baseline,
	dryRun bool,
	fullReconcile bool,
) ([]ChangeEvent, *remoteObservationBatch, error) {
	observationBatch, err := flow.executePrimaryRootObservation(ctx, bl, fullReconcile)
	if err != nil {
		return nil, nil, err
	}

	// Dry-run previews must never advance remote observation cursors.
	if dryRun {
		observationBatch.cursorToken = ""
		observationBatch.markFullRemoteRefresh = false
	}

	if !dryRun && len(observationBatch.observed) > 0 {
		if err := flow.commitObservedItems(ctx, observationBatch.observed, ""); err != nil {
			return nil, nil, err
		}
	}
	if !dryRun {
		if err := flow.applyObservationFindingsBatch(
			ctx,
			&observationBatch.findings,
			"failed to reconcile remote observation findings",
			engineDebugNoteRemoteCurrent,
		); err != nil {
			return nil, nil, err
		}
	}

	return observationBatch.emitted, observationBatch.deferredProgress(), nil
}

func (flow *engineFlow) refreshLocalCurrentState(
	ctx context.Context,
	bl *Baseline,
) (ScanResult, error) {
	localResult, err := flow.observeLocal(ctx, bl)
	if err != nil {
		return ScanResult{}, localCurrentRefreshFailure(localCurrentRefreshStepObservation, err)
	}

	if err := flow.reconcileSkippedObservationFindings(ctx, localResult.Skipped); err != nil {
		return ScanResult{}, localCurrentRefreshFailure(localCurrentRefreshStepFindingsReconcile, err)
	}

	return localResult, nil
}

func (flow *engineFlow) refreshAndCommitLocalCurrentState(
	ctx context.Context,
	bl *Baseline,
	dryRun bool,
) (ScanResult, error) {
	localResult, err := flow.refreshLocalCurrentState(ctx, bl)
	if err != nil {
		return ScanResult{}, err
	}
	if err := flow.commitCurrentLocalSnapshot(ctx, dryRun, localResult); err != nil {
		return ScanResult{}, localCurrentRefreshFailure(localCurrentRefreshStepSnapshotCommit, err)
	}

	return localResult, nil
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
	obs.SetItemClient(eng.itemsClient)

	events, token, err := obs.FullDelta(ctx, savedToken)
	if err != nil {
		if !errors.Is(err, ErrDeltaExpired) {
			return nil, "", fmt.Errorf("sync: observing remote delta: %w", err)
		}

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
	obs.SetManagedRootEventSink(eng.managedRootEvents)

	result, err := obs.FullScan(ctx, eng.syncTree)
	if err != nil {
		return ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
}

func (flow *engineFlow) commitCurrentLocalSnapshot(
	ctx context.Context,
	dryRun bool,
	localResult ScanResult,
) error {
	if dryRun {
		return nil
	}

	rows := buildLocalStateRows(localResult)
	if err := flow.engine.baseline.ReplaceLocalState(ctx, rows); err != nil {
		return fmt.Errorf("sync: replacing local_state snapshot: %w", err)
	}

	return nil
}

func (flow *engineFlow) loadDryRunCurrentObservation(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (result *currentObservation, err error) {
	observationBatch, err := flow.executePrimaryRootObservation(ctx, bl, fullReconcile)
	if err != nil {
		return nil, err
	}

	localResult, err := flow.refreshLocalCurrentState(ctx, bl)
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

	commitErr := scratchStore.CommitObservation(ctx, observationBatch.observed, "", flow.engine.driveID)
	if commitErr != nil {
		return nil, fmt.Errorf("sync: committing dry-run remote snapshot to scratch store: %w", commitErr)
	}
	if reconcileErr := scratchStore.ReconcileObservationFindings(ctx, &observationBatch.findings, flow.engine.nowFunc()); reconcileErr != nil {
		return nil, fmt.Errorf("sync: reconciling dry-run remote observation findings in scratch store: %w", reconcileErr)
	}

	localRows := buildLocalStateRows(localResult)
	replaceErr := scratchStore.ReplaceLocalState(ctx, localRows)
	if replaceErr != nil {
		return nil, fmt.Errorf("sync: replacing dry-run local snapshot in scratch store: %w", replaceErr)
	}

	inputs, err := flow.loadCurrentInputs(ctx, scratchStore, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return &currentObservation{
		inputs:        inputs,
		observedPaths: len(inputs.localRows) + len(inputs.remoteRows),
	}, nil
}

func (flow *engineFlow) loadCommittedCurrentObservation(
	ctx context.Context,
	pendingRemoteObservation *remoteObservationBatch,
) (*currentObservation, error) {
	inputs, err := flow.loadCurrentInputs(ctx, flow.engine.baseline, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return &currentObservation{
		inputs:                   inputs,
		observedPaths:            len(inputs.localRows) + len(inputs.remoteRows),
		pendingRemoteObservation: pendingRemoteObservation,
	}, nil
}

func (flow *engineFlow) loadCurrentInputs(
	ctx context.Context,
	store *SyncStore,
	defaultDriveID driveid.ID,
) (currentInputs, error) {
	tx, err := beginPerfTx(ctx, store.db)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: beginning current action planner read transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			flow.engine.logger.Debug("current action planner read transaction rollback failed",
				slog.String("error", rollbackErr.Error()),
			)
		}
	}()

	return flow.loadCurrentInputsTx(ctx, store, tx, defaultDriveID)
}

func (flow *engineFlow) loadCurrentInputsTx(
	ctx context.Context,
	store *SyncStore,
	tx sqlTxRunner,
	defaultDriveID driveid.ID,
) (currentInputs, error) {
	comparisons, err := queryComparisonStateWithRunner(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: querying comparison state: %w", err)
	}
	reconciliations, err := queryReconciliationStateWithRunner(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: querying reconciliation state: %w", err)
	}
	localRows, err := listLocalStateRows(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: listing local_state rows: %w", err)
	}
	observationState, err := store.readObservationStateTx(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: reading observation state for remote_state: %w", err)
	}
	contentDriveID := observationState.ContentDriveID
	if contentDriveID.IsZero() {
		contentDriveID = defaultDriveID
	}
	remoteRows, err := queryRemoteStateRowsWithRunner(
		ctx,
		tx,
		`SELECT `+sqlSelectRemoteStateCols+` FROM remote_state`,
		contentDriveID,
	)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: listing remote_state rows: %w", err)
	}
	observationIssues, err := queryObservationIssueRowsWithRunner(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: listing observation issues: %w", err)
	}
	return currentInputs{
		comparisons:       comparisons,
		reconciliations:   reconciliations,
		localRows:         localRows,
		remoteRows:        remoteRows,
		observationIssues: observationIssues,
	}, nil
}

// buildCurrentPlanStage is the shared planning stage after an entrypoint has
// already observed current truth. It builds the current plan and report but
// does not touch durable retry/scope state.
func (flow *engineFlow) buildCurrentPlanStage(
	ctx context.Context,
	bl *Baseline,
	mode SyncMode,
	opts RunOptions,
	observation *currentObservation,
) (*builtCurrentPlan, error) {
	if observation == nil {
		return nil, fmt.Errorf("sync: building current plan: missing observed state")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sync: building current plan from observed state: %w", err)
	}

	planStart := flow.engine.nowFunc()
	plan, err := flow.engine.buildCurrentActionPlanFromInputs(&observation.inputs, bl, mode)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	flow.engine.collector().RecordPlan(len(plan.Actions), flow.engine.since(planStart))

	counts := CountByType(plan.Actions)
	report := buildReportFromCounts(counts, CountConflicts(plan.Actions), plan.DeferredByMode, mode, opts)

	return &builtCurrentPlan{
		Plan:                     plan,
		Report:                   report,
		PendingRemoteObservation: observation.pendingRemoteObservation,
	}, nil
}

// reconcileRuntimeStateStage turns a built current plan into the runtime-start
// handoff by reconciling durable held-work state and loading the surviving
// retry/block-scope rows the runtime owns.
func (flow *engineFlow) reconcileRuntimeStateStage(
	ctx context.Context,
	build *builtCurrentPlan,
) (*runtimePlan, error) {
	if build == nil {
		return nil, fmt.Errorf("sync: reconciling runtime state: missing current plan build")
	}
	if err := flow.reconcileRuntimeState(ctx, build.Plan); err != nil {
		return nil, err
	}

	retryRows, blockScopes, err := flow.loadRuntimeState(ctx)
	if err != nil {
		return nil, err
	}

	return &runtimePlan{
		Plan:                     build.Plan,
		Report:                   build.Report,
		PendingRemoteObservation: build.PendingRemoteObservation,
		RetryRows:                retryRows,
		BlockScopes:              blockScopes,
	}, nil
}

// keepBuiltCurrentPlan preserves the build-stage handoff without reconciling or
// loading durable runtime state.
func (flow *engineFlow) keepBuiltCurrentPlan(build *builtCurrentPlan) *runtimePlan {
	if build == nil {
		return nil
	}

	return &runtimePlan{
		Plan:                     build.Plan,
		Report:                   build.Report,
		PendingRemoteObservation: build.PendingRemoteObservation,
	}
}

func (flow *engineFlow) reconcileRuntimeState(ctx context.Context, plan *ActionPlan) error {
	if err := flow.engine.baseline.PruneRetryWorkToCurrentActions(ctx, retryWorkKeysForActions(plan.Actions)); err != nil {
		return fmt.Errorf("sync: pruning retry_work to current actions: %w", err)
	}

	if err := flow.engine.baseline.PruneBlockScopesWithoutBlockedWork(ctx); err != nil {
		return fmt.Errorf("sync: pruning block scopes without blocked work: %w", err)
	}

	return nil
}

func (flow *engineFlow) loadRuntimeState(ctx context.Context) ([]RetryWorkRow, []*BlockScope, error) {
	retryRows, err := flow.engine.baseline.ListRetryWork(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: listing retry_work for runtime-state handoff: %w", err)
	}

	blockScopes, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: listing block_scopes for runtime-state handoff: %w", err)
	}

	return retryRows, blockScopes, nil
}

func (e *Engine) buildCurrentActionPlanFromInputs(
	inputs *currentInputs,
	bl *Baseline,
	mode SyncMode,
) (*ActionPlan, error) {
	return e.planner.PlanCurrentState(
		inputs.comparisons,
		inputs.reconciliations,
		inputs.localRows,
		inputs.remoteRows,
		inputs.observationIssues,
		bl,
		plannerMountContext{
			DriveID: e.driveID,
		},
		mode,
	)
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
