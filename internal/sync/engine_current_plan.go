package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type currentPlanPipelineRequest struct {
	mode                  Mode
	opts                  RunOptions
	observeCurrentState   func(context.Context, *Baseline) (*observedCurrentState, error)
	reconcileRuntimeState bool
}

func (r *oneShotRunner) runLiveCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
) (*PreparedCurrentPlan, error) {
	return r.runCurrentPlanPipeline(ctx, bl, currentPlanPipelineRequest{
		mode: mode,
		opts: opts,
		observeCurrentState: func(ctx context.Context, bl *Baseline) (*observedCurrentState, error) {
			return r.observeLiveCurrentState(ctx, bl, opts.FullReconcile)
		},
		reconcileRuntimeState: true,
	})
}

func (r *oneShotRunner) runDryRunCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
) (*PreparedCurrentPlan, error) {
	return r.runCurrentPlanPipeline(ctx, bl, currentPlanPipelineRequest{
		mode: mode,
		opts: opts,
		observeCurrentState: func(ctx context.Context, bl *Baseline) (*observedCurrentState, error) {
			return r.observeDryRunCurrentState(ctx, bl, opts.FullReconcile)
		},
		reconcileRuntimeState: false,
	})
}

func (rt *watchRuntime) runBootstrapCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
) (*PreparedCurrentPlan, error) {
	fullRefresh, err := rt.engine.shouldRunFullRemoteRefresh(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("sync: deciding bootstrap full remote refresh: %w", err)
	}

	return rt.runCurrentPlanPipeline(ctx, bl, currentPlanPipelineRequest{
		mode: mode,
		opts: RunOptions{},
		observeCurrentState: func(ctx context.Context, bl *Baseline) (*observedCurrentState, error) {
			observeStart := rt.engine.nowFunc()
			pendingCursorCommit, err := rt.observeAndCommitCurrentState(ctx, bl, false, fullRefresh)
			if err != nil {
				return nil, fmt.Errorf("sync: bootstrap observation failed: %w", err)
			}
			observed, err := rt.loadObservedCurrentInputs(ctx, pendingCursorCommit)
			if err != nil {
				return nil, fmt.Errorf("sync: bootstrap observed-state load failed: %w", err)
			}
			rt.engine.collector().RecordObserve(observed.observedPaths, rt.engine.since(observeStart))
			return observed, nil
		},
		reconcileRuntimeState: true,
	})
}

func (rt *watchRuntime) runSteadyStateCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
) (*PreparedCurrentPlan, error) {
	return rt.runCurrentPlanPipeline(ctx, bl, currentPlanPipelineRequest{
		mode: mode,
		opts: RunOptions{},
		observeCurrentState: func(ctx context.Context, _ *Baseline) (*observedCurrentState, error) {
			observed, err := rt.loadObservedCurrentInputs(ctx, nil)
			if err != nil {
				return nil, fmt.Errorf("sync: watch observed-state load failed: %w", err)
			}
			return observed, nil
		},
		reconcileRuntimeState: true,
	})
}

func (flow *engineFlow) runCurrentPlanPipeline(
	ctx context.Context,
	bl *Baseline,
	req currentPlanPipelineRequest,
) (*PreparedCurrentPlan, error) {
	if req.observeCurrentState == nil {
		return nil, fmt.Errorf("sync: current plan pipeline: missing current-state observation")
	}

	observed, err := req.observeCurrentState(ctx, bl)
	if err != nil {
		return nil, err
	}

	build, err := flow.buildCurrentPlanStage(ctx, bl, req.mode, req.opts, observed)
	if err != nil {
		return nil, err
	}

	if !req.reconcileRuntimeState {
		return flow.keepCurrentPlanBuild(build), nil
	}

	return flow.reconcileRuntimeStateStage(ctx, build)
}

type dryRunPlanInput struct {
	currentActionPlanInputs
	observedPaths int
}

type observedCurrentState struct {
	inputs              currentActionPlanInputs
	observedPaths       int
	pendingCursorCommit *pendingPrimaryCursorCommit
}

type currentPlanBuild struct {
	Plan                *ActionPlan
	Report              *Report
	PendingCursorCommit *pendingPrimaryCursorCommit
}

type PreparedCurrentPlan struct {
	Plan                *ActionPlan
	Report              *Report
	PendingCursorCommit *pendingPrimaryCursorCommit
	RetryRows           []RetryWorkRow
	BlockScopes         []*BlockScope
}

type currentActionPlanInputs struct {
	comparisons       []SQLiteComparisonRow
	reconciliations   []SQLiteReconciliationRow
	localRows         []LocalStateRow
	remoteRows        []RemoteStateRow
	observationIssues []ObservationIssueRow
}

func (r *oneShotRunner) observeLiveCurrentState(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (*observedCurrentState, error) {
	observeStart := r.engine.nowFunc()
	pendingCursorCommit, err := r.observeAndCommitCurrentState(ctx, bl, false, fullReconcile)
	if err != nil {
		return nil, err
	}
	observed, err := r.loadObservedCurrentInputs(ctx, pendingCursorCommit)
	if err != nil {
		return nil, err
	}
	r.engine.collector().RecordObserve(observed.observedPaths, r.engine.since(observeStart))

	return observed, nil
}

func (r *oneShotRunner) observeDryRunCurrentState(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (*observedCurrentState, error) {
	observeStart := r.engine.nowFunc()
	inputs, err := r.loadDryRunCurrentInputs(ctx, bl, fullReconcile)
	if err != nil {
		return nil, err
	}
	observed := observedCurrentState{
		inputs:        inputs.currentActionPlanInputs,
		observedPaths: inputs.observedPaths,
	}
	r.engine.collector().RecordObserve(observed.observedPaths, r.engine.since(observeStart))

	return &observed, nil
}

func (flow *engineFlow) observeAndCommitCurrentState(
	ctx context.Context,
	bl *Baseline,
	dryRun, fullReconcile bool,
) (*pendingPrimaryCursorCommit, error) {
	plan := flow.buildPrimaryRootObservationPlan(fullReconcile)
	_, pendingCursorCommit, err := flow.observeAndCommitRemoteCurrentState(ctx, bl, dryRun, plan)
	if err != nil {
		return nil, err
	}

	if _, err := flow.observeAndCommitLocalCurrentState(ctx, bl, dryRun); err != nil {
		return nil, err
	}

	return pendingCursorCommit, nil
}

func (flow *engineFlow) observeAndCommitRemoteCurrentState(
	ctx context.Context,
	bl *Baseline,
	dryRun bool,
	plan primaryRootObservationPlan,
) ([]ChangeEvent, *pendingPrimaryCursorCommit, error) {
	fetchResult, err := flow.executePrimaryRootObservation(ctx, bl, plan)
	if err != nil {
		return nil, nil, err
	}

	// Dry-run previews must never advance remote observation cursors.
	if dryRun {
		fetchResult.pending = nil
	}

	projectedRemote := projectRemoteObservations(flow.engine.logger, fetchResult.events)
	if !dryRun && len(projectedRemote.observed) > 0 {
		if err := flow.commitObservedItems(ctx, projectedRemote.observed, ""); err != nil {
			return nil, nil, err
		}
	}
	if !dryRun {
		if err := flow.applyObservationFindingsBatch(
			ctx,
			&fetchResult.findings,
			"failed to reconcile remote observation findings",
			engineDebugNoteRemoteCurrent,
		); err != nil {
			return nil, nil, err
		}
	}

	return projectedRemote.emitted, fetchResult.pending, nil
}

func (flow *engineFlow) observeLocalCurrentState(
	ctx context.Context,
	bl *Baseline,
) (ScanResult, error) {
	localResult, err := flow.observeLocal(ctx, bl)
	if err != nil {
		return ScanResult{}, err
	}

	if err := flow.reconcileSkippedObservationFindings(ctx, localResult.Skipped); err != nil {
		return ScanResult{}, err
	}

	return localResult, nil
}

func (flow *engineFlow) observeAndCommitLocalCurrentState(
	ctx context.Context,
	bl *Baseline,
	dryRun bool,
) (ScanResult, error) {
	localResult, err := flow.observeLocalCurrentState(ctx, bl)
	if err != nil {
		return ScanResult{}, err
	}
	if err := flow.commitObservedLocalSnapshot(ctx, dryRun, localResult); err != nil {
		return ScanResult{}, err
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

	result, err := obs.FullScan(ctx, eng.syncTree)
	if err != nil {
		return ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
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
	rows := buildLocalStateRows(localResult)
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

func (flow *engineFlow) loadDryRunCurrentInputs(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (result *dryRunPlanInput, err error) {
	plan := flow.buildPrimaryRootObservationPlan(fullReconcile)
	fetchResult, err := flow.executePrimaryRootObservation(ctx, bl, plan)
	if err != nil {
		return nil, err
	}

	projectedRemote := projectRemoteObservations(flow.engine.logger, fetchResult.events)

	localResult, err := flow.observeLocalCurrentState(ctx, bl)
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

	localRows := buildLocalStateRows(localResult)
	replaceErr := scratchStore.ReplaceLocalState(ctx, localRows)
	if replaceErr != nil {
		return nil, fmt.Errorf("sync: replacing dry-run local snapshot in scratch store: %w", replaceErr)
	}

	inputs, err := flow.loadCurrentInputsStage(ctx, scratchStore, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return &dryRunPlanInput{
		currentActionPlanInputs: inputs,
		observedPaths:           len(inputs.localRows) + len(inputs.remoteRows),
	}, nil
}

func (flow *engineFlow) loadObservedCurrentInputs(
	ctx context.Context,
	pendingCursorCommit *pendingPrimaryCursorCommit,
) (*observedCurrentState, error) {
	inputs, err := flow.loadCurrentInputsStage(ctx, flow.engine.baseline, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return &observedCurrentState{
		inputs:              inputs,
		observedPaths:       len(inputs.localRows) + len(inputs.remoteRows),
		pendingCursorCommit: pendingCursorCommit,
	}, nil
}

func (flow *engineFlow) loadCurrentInputsStage(
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

	return flow.loadCurrentInputsStageTx(ctx, store, tx, defaultDriveID)
}

func (flow *engineFlow) loadCurrentInputsStageTx(
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
		`SELECT `+sqlSelectRemoteStateCols+` FROM remote_state`,
		configuredDriveID,
	)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing remote_state rows: %w", err)
	}
	observationIssues, err := queryObservationIssueRowsWithRunner(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing observation issues: %w", err)
	}
	return currentActionPlanInputs{
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
	mode Mode,
	opts RunOptions,
	observed *observedCurrentState,
) (*currentPlanBuild, error) {
	if observed == nil {
		return nil, fmt.Errorf("sync: building current plan: missing observed state")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sync: building current plan from observed state: %w", err)
	}

	planStart := flow.engine.nowFunc()
	plan, err := flow.engine.buildCurrentActionPlanFromInputs(&observed.inputs, bl, mode)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	flow.engine.collector().RecordPlan(len(plan.Actions), flow.engine.since(planStart))

	counts := CountByType(plan.Actions)
	report := buildReportFromCounts(counts, CountConflicts(plan.Actions), plan.DeferredByMode, mode, opts)

	return &currentPlanBuild{
		Plan:                plan,
		Report:              report,
		PendingCursorCommit: observed.pendingCursorCommit,
	}, nil
}

// reconcileRuntimeStateStage turns a built current plan into the runtime-start
// handoff by reconciling durable held-work state and loading the surviving
// retry/block-scope rows the runtime owns.
func (flow *engineFlow) reconcileRuntimeStateStage(
	ctx context.Context,
	build *currentPlanBuild,
) (*PreparedCurrentPlan, error) {
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

	return &PreparedCurrentPlan{
		Plan:                build.Plan,
		Report:              build.Report,
		PendingCursorCommit: build.PendingCursorCommit,
		RetryRows:           retryRows,
		BlockScopes:         blockScopes,
	}, nil
}

// keepCurrentPlanBuild preserves the build-stage handoff without reconciling
// or loading durable runtime state.
func (flow *engineFlow) keepCurrentPlanBuild(build *currentPlanBuild) *PreparedCurrentPlan {
	if build == nil {
		return nil
	}

	return &PreparedCurrentPlan{
		Plan:                build.Plan,
		Report:              build.Report,
		PendingCursorCommit: build.PendingCursorCommit,
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
		return nil, nil, fmt.Errorf("sync: listing retry_work for prepared runtime state: %w", err)
	}

	blockScopes, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: listing block_scopes for prepared runtime state: %w", err)
	}

	return retryRows, blockScopes, nil
}

func (e *Engine) buildCurrentActionPlanFromInputs(
	inputs *currentActionPlanInputs,
	bl *Baseline,
	mode Mode,
) (*ActionPlan, error) {
	return e.planner.PlanCurrentState(
		inputs.comparisons,
		inputs.reconciliations,
		inputs.localRows,
		inputs.remoteRows,
		inputs.observationIssues,
		bl,
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
