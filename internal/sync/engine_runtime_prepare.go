package sync

import (
	"context"
	"fmt"
)

func (r *oneShotRunner) prepareLiveCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
) (*PreparedCurrentPlan, error) {
	observed, err := r.observeLiveCurrentState(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}

	build, err := r.buildCurrentPlanFromObservedState(ctx, bl, mode, opts, observed)
	if err != nil {
		return nil, err
	}

	return r.prepareRuntimeCurrentPlan(ctx, build)
}

func (r *oneShotRunner) prepareDryRunCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
) (*PreparedCurrentPlan, error) {
	observed, err := r.observeDryRunCurrentState(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}

	build, err := r.buildCurrentPlanFromObservedState(ctx, bl, mode, opts, observed)
	if err != nil {
		return nil, err
	}

	return r.engineFlow.prepareDryRunCurrentPlan(build), nil
}

// buildCurrentPlanFromObservedState is the shared planning stage after an
// entrypoint has already observed current truth. It builds the current plan
// and report but does not touch durable retry/scope state.
func (flow *engineFlow) buildCurrentPlanFromObservedState(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	opts RunOptions,
	observed *observedCurrentState,
) (*currentPlanBuild, error) {
	if observed == nil {
		return nil, fmt.Errorf("sync: preparing current plan: missing observed state")
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

// prepareRuntimeCurrentPlan turns a built current plan into the runtime-start
// handoff by reconciling durable held-work state and loading the surviving
// retry/block-scope rows execution owns.
func (flow *engineFlow) prepareRuntimeCurrentPlan(
	ctx context.Context,
	build *currentPlanBuild,
) (*PreparedCurrentPlan, error) {
	if build == nil {
		return nil, fmt.Errorf("sync: preparing runtime current plan: missing build")
	}
	if err := flow.reconcileDurablePlanState(ctx, build.Plan); err != nil {
		return nil, err
	}

	retryRows, blockScopes, err := flow.loadPreparedRuntimeState(ctx)
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

// prepareDryRunCurrentPlan preserves the build-stage plan/report handoff
// without reconciling or loading durable runtime state.
func (flow *engineFlow) prepareDryRunCurrentPlan(build *currentPlanBuild) *PreparedCurrentPlan {
	if build == nil {
		return nil
	}

	return &PreparedCurrentPlan{
		Plan:                build.Plan,
		Report:              build.Report,
		PendingCursorCommit: build.PendingCursorCommit,
	}
}

type currentPlanBuild struct {
	Plan                *ActionPlan
	Report              *Report
	PendingCursorCommit *pendingPrimaryCursorCommit
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

func (flow *engineFlow) reconcileDurablePlanState(ctx context.Context, plan *ActionPlan) error {
	if err := flow.engine.baseline.PruneRetryWorkToCurrentActions(ctx, retryWorkKeysForActions(plan.Actions)); err != nil {
		return fmt.Errorf("sync: pruning retry_work to current actions: %w", err)
	}

	if err := flow.engine.baseline.PruneBlockScopesWithoutBlockedWork(ctx); err != nil {
		return fmt.Errorf("sync: pruning block scopes without blocked work: %w", err)
	}

	return nil
}

func (flow *engineFlow) loadPreparedRuntimeState(ctx context.Context) ([]RetryWorkRow, []*BlockScope, error) {
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

// prepareBootstrapCurrentPlan observes bootstrap truth once, builds the
// current plan from that observed state, then prepares the runtime-start
// durable handoff.
func (rt *watchRuntime) prepareBootstrapCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
) (*PreparedCurrentPlan, error) {
	fullRefresh, err := rt.engine.shouldRunFullRemoteRefresh(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("sync: deciding bootstrap full remote refresh: %w", err)
	}

	observeStart := rt.engine.nowFunc()
	pendingCursorCommit, err := rt.observeAndCommitCurrentTruth(ctx, bl, false, fullRefresh)
	if err != nil {
		return nil, fmt.Errorf("sync: bootstrap observation failed: %w", err)
	}
	observed, err := rt.loadObservedCurrentState(ctx, pendingCursorCommit)
	if err != nil {
		return nil, fmt.Errorf("sync: bootstrap observed-state load failed: %w", err)
	}
	rt.engine.collector().RecordObserve(observed.observedPaths, rt.engine.since(observeStart))

	build, err := rt.buildCurrentPlanFromObservedState(ctx, bl, mode, RunOptions{}, observed)
	if err != nil {
		return nil, fmt.Errorf("sync: bootstrap planning failed: %w", err)
	}

	prepared, err := rt.prepareRuntimeCurrentPlan(ctx, build)
	if err != nil {
		return nil, fmt.Errorf("sync: bootstrap runtime prepare failed: %w", err)
	}

	return prepared, nil
}

// prepareSteadyStateCurrentPlan loads already-committed watch truth after the
// steady-state replan path has refreshed the local snapshot, then runs the
// same build-and-runtime-prepare stages bootstrap uses.
func (rt *watchRuntime) prepareSteadyStateCurrentPlan(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
) (*PreparedCurrentPlan, error) {
	observed, err := rt.loadObservedCurrentState(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sync: watch observed-state load failed: %w", err)
	}

	build, err := rt.buildCurrentPlanFromObservedState(ctx, bl, mode, RunOptions{}, observed)
	if err != nil {
		return nil, fmt.Errorf("sync: watch planning failed: %w", err)
	}

	prepared, err := rt.prepareRuntimeCurrentPlan(ctx, build)
	if err != nil {
		return nil, fmt.Errorf("sync: watch runtime prepare failed: %w", err)
	}

	return prepared, nil
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
