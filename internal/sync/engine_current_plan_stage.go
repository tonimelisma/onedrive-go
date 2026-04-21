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

func (r *oneShotRunner) prepareLiveCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
) (*preparedCurrentActionPlan, error) {
	observed, err := r.observeLiveCurrentState(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}

	return r.prepareCurrentPlanFromObservedState(ctx, bl, mode, opts, observed, true)
}

func (r *oneShotRunner) prepareDryRunCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
) (*preparedCurrentActionPlan, error) {
	observed, err := r.observeDryRunCurrentState(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}

	return r.prepareCurrentPlanFromObservedState(ctx, bl, mode, opts, observed, false)
}

func (r *oneShotRunner) observeLiveCurrentState(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (*observedCurrentState, error) {
	observeStart := r.engine.nowFunc()
	pendingCursorCommit, err := r.observeCurrentTruth(ctx, nil, bl, false, fullReconcile)
	if err != nil {
		return nil, err
	}
	inputs, err := r.loadCurrentActionPlanInputs(ctx, r.engine.baseline, r.engine.driveID)
	if err != nil {
		return nil, err
	}
	observed := observedCurrentState{
		inputs:              inputs,
		observedPaths:       len(inputs.localRows) + len(inputs.remoteRows),
		pendingCursorCommit: pendingCursorCommit,
	}
	r.engine.collector().RecordObserve(observed.observedPaths, r.engine.since(observeStart))

	return &observed, nil
}

func (r *oneShotRunner) observeDryRunCurrentState(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (*observedCurrentState, error) {
	observeStart := r.engine.nowFunc()
	planResult, err := r.buildDryRunCurrentActionPlan(ctx, bl, fullReconcile)
	if err != nil {
		return nil, err
	}
	observed := observedCurrentState{
		inputs:        planResult.currentActionPlanInputs,
		observedPaths: planResult.observedPaths,
	}
	r.engine.collector().RecordObserve(observed.observedPaths, r.engine.since(observeStart))

	return &observed, nil
}

func (r *oneShotRunner) prepareCurrentPlanFromObservedState(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
	observed *observedCurrentState,
	materialize bool,
) (*preparedCurrentActionPlan, error) {
	if observed == nil {
		return nil, fmt.Errorf("sync: preparing current plan: missing observed state")
	}

	safety := r.engine.resolveSafetyConfig()

	planStart := r.engine.nowFunc()
	plan, err := r.engine.buildCurrentActionPlanFromInputs(&observed.inputs, bl, mode, safety)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	r.engine.collector().RecordPlan(len(plan.Actions), r.engine.since(planStart))

	if materialize {
		if materializeErr := r.materializeCurrentActionPlan(ctx, plan); materializeErr != nil {
			return nil, materializeErr
		}
	}

	counts := CountByType(plan.Actions)
	report := buildReportFromCounts(counts, CountConflicts(plan.Actions), plan.DeferredByMode, mode, opts)

	return &preparedCurrentActionPlan{
		plan:                plan,
		report:              report,
		pendingCursorCommit: observed.pendingCursorCommit,
	}, nil
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

type preparedCurrentActionPlan struct {
	plan                *ActionPlan
	report              *Report
	pendingCursorCommit *pendingPrimaryCursorCommit
}

type currentActionPlanInputs struct {
	comparisons       []SQLiteComparisonRow
	reconciliations   []SQLiteReconciliationRow
	localRows         []LocalStateRow
	remoteRows        []RemoteStateRow
	observationIssues []ObservationIssueRow
	blockScopes       []*BlockScope
}

func (flow *engineFlow) materializeCurrentActionPlan(ctx context.Context, plan *ActionPlan) error {
	if err := flow.engine.baseline.PruneRetryWorkToCurrentActions(ctx, retryWorkKeysForActions(plan.Actions)); err != nil {
		return fmt.Errorf("sync: pruning retry_work to current actions: %w", err)
	}

	if err := flow.engine.baseline.PruneBlockScopesWithoutBlockedWork(ctx); err != nil {
		return fmt.Errorf("sync: pruning block scopes without blocked work: %w", err)
	}

	if flow.watch != nil {
		flow.watch.replaceCurrentPlan(plan)
	}

	return nil
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
	blockScopes, err := queryBlockScopeRowsWithRunner(ctx, tx)
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
