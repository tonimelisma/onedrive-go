package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func (r *oneShotRunner) runLiveCurrentPlan(
	ctx context.Context,
	bl syncdomain.BaselineView,
	mode syncdomain.SyncMode,
	opts RunOptions,
) (*runtimePlan, error) {
	observeStart := r.deps.now()
	pendingRemoteObservation, err := r.observeAndCommitCurrentState(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	observation, err := r.loadCommittedCurrentObservation(ctx, pendingRemoteObservation)
	if err != nil {
		return nil, err
	}
	r.deps.perf.RecordObserve(observation.observedPaths, r.deps.since(observeStart))

	build, err := r.buildCurrentPlanStage(ctx, bl, mode, opts, observation)
	if err != nil {
		return nil, err
	}

	return r.reconcileRuntimeStateStage(ctx, build, syncstore.PassObservedRemoteThenLocal)
}

func (r *oneShotRunner) runDryRunCurrentPlan(
	ctx context.Context,
	bl syncdomain.BaselineView,
	mode syncdomain.SyncMode,
	opts RunOptions,
) (*runtimePlan, error) {
	observeStart := r.deps.now()
	observation, err := r.loadDryRunCurrentObservation(ctx, bl, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	r.deps.perf.RecordObserve(observation.observedPaths, r.deps.since(observeStart))

	build, err := r.buildCurrentPlanStage(ctx, bl, mode, opts, observation)
	if err != nil {
		return nil, err
	}

	return r.keepBuiltCurrentPlan(build), nil
}

func (rt *watchRuntime) runBootstrapCurrentPlan(
	ctx context.Context,
	bl syncdomain.BaselineView,
	mode syncdomain.SyncMode,
) (*runtimePlan, error) {
	fullRefresh, err := rt.engine.shouldRunFullRemoteRefresh(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("sync: deciding whether to rescan OneDrive: %w", err)
	}

	observeStart := rt.deps.now()
	pendingRemoteObservation, err := rt.observeAndCommitCurrentState(ctx, bl, fullRefresh)
	if err != nil {
		return nil, fmt.Errorf("checking for changes: %w", err)
	}
	observation, err := rt.loadCommittedCurrentObservation(ctx, pendingRemoteObservation)
	if err != nil {
		return nil, err
	}
	rt.deps.perf.RecordObserve(observation.observedPaths, rt.deps.since(observeStart))

	build, err := rt.buildCurrentPlanStage(ctx, bl, mode, RunOptions{}, observation)
	if err != nil {
		return nil, err
	}

	return rt.reconcileRuntimeStateStage(ctx, build, syncstore.PassObservedRemoteThenLocal)
}

// runSteadyStateCurrentPlan plans on the local truth the replan just
// refreshed and the remote truth the observers committed before it: this pass
// observed the local tree only.
func (rt *watchRuntime) runSteadyStateCurrentPlan(
	ctx context.Context,
	bl syncdomain.BaselineView,
	mode syncdomain.SyncMode,
) (*runtimePlan, error) {
	observation, err := rt.loadCommittedCurrentObservation(ctx, nil)
	if err != nil {
		return nil, err
	}

	build, err := rt.buildCurrentPlanStage(ctx, bl, mode, RunOptions{}, observation)
	if err != nil {
		return nil, err
	}

	return rt.reconcileRuntimeStateStage(ctx, build, syncstore.PassRefreshedLocalOnly)
}

type currentObservation struct {
	inputs                   currentInputs
	observedPaths            int
	pendingRemoteObservation *syncobserve.RemoteObservationBatch
	childPublication         syncshortcut.ShortcutChildWorkSnapshot
}

type localCurrentRefreshStep string

const (
	localCurrentRefreshStepObservation    localCurrentRefreshStep = "local_observation"
	localCurrentRefreshStepSnapshotCommit localCurrentRefreshStep = "local_snapshot_commit"
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
	Plan                     *syncdomain.ActionPlan
	Report                   *Report
	PendingRemoteObservation *syncobserve.RemoteObservationBatch
	ChildPublication         syncshortcut.ShortcutChildWorkSnapshot
}

type runtimePlan struct {
	Plan                     *syncdomain.ActionPlan
	Report                   *Report
	PendingRemoteObservation *syncobserve.RemoteObservationBatch
	ChildPublication         syncshortcut.ShortcutChildWorkSnapshot
	RetryRows                []syncdomain.RetryWorkRow
	BlockScopes              []*syncdomain.BlockScope
}

type currentInputs struct {
	comparisons       []syncdomain.SQLiteComparisonRow
	reconciliations   []syncdomain.SQLiteReconciliationRow
	localRows         []syncdomain.LocalStateRow
	remoteRows        []syncdomain.RemoteStateRow
	observationIssues []syncdomain.ObservationIssueRow
	// baseline is read in the same transaction as the comparison rows, so a
	// baseline_present row always has its entry. The run's cached baseline
	// can lag or lead that snapshot when a commit lands between them.
	baseline syncdomain.BaselineView
}

func (flow *engineFlow) observeAndCommitCurrentState(
	ctx context.Context,
	bl syncdomain.BaselineView,
	fullReconcile bool,
) (*syncobserve.RemoteObservationBatch, error) {
	pendingRemoteObservation, err := flow.observeAndCommitRemoteCurrentState(ctx, bl, fullReconcile)
	if err != nil {
		return nil, err
	}

	if _, err := flow.refreshAndCommitLocalCurrentState(ctx, bl); err != nil {
		return nil, err
	}

	return pendingRemoteObservation, nil
}

func (flow *engineFlow) observeAndCommitRemoteCurrentState(
	ctx context.Context,
	bl syncdomain.BaselineView,
	fullReconcile bool,
) (*syncobserve.RemoteObservationBatch, error) {
	observationBatch, err := flow.executePrimaryRootObservation(ctx, bl, fullReconcile)
	if err != nil {
		return nil, err
	}

	if err := flow.applyShortcutObservationBatch(ctx, &observationBatch); err != nil {
		return nil, err
	}

	if len(observationBatch.Observed) > 0 {
		if err := flow.commitObservedItems(ctx, observationBatch.Observed, ""); err != nil {
			return nil, err
		}
	}
	if err := flow.applyObservationFindingsBatch(
		ctx,
		&observationBatch.Findings,
		recordSkippedOneDriveItems,
		engineDebugNoteRemoteCurrent,
	); err != nil {
		return nil, err
	}

	return observationBatch.DeferredProgress(), nil
}

func (flow *engineFlow) applyShortcutObservationBatch(ctx context.Context, batch *syncobserve.RemoteObservationBatch) error {
	if flow == nil || flow.engine == nil || batch == nil {
		return nil
	}

	topology := batch.ShortcutTopology
	if topology.NamespaceID == "" {
		topology.NamespaceID = flow.cfg.shortcutNamespaceID
	}
	if err := flow.engine.confirmShortcutCompleteOmissions(ctx, &topology); err != nil {
		return err
	}
	remoteChanged := topology.ShouldApply()
	if remoteChanged {
		if _, err := flow.deps.store.ApplyShortcutTopology(ctx, topology); err != nil {
			return fmt.Errorf("sync: persist parent shortcut root state: %w", err)
		}
	}
	localChanged, err := flow.engine.reconcileShortcutRootLocalState(ctx)
	if err != nil {
		return fmt.Errorf("sync: reconcile parent shortcut root local state: %w", err)
	}
	if !remoteChanged && !localChanged {
		return nil
	}
	if refreshErr := flow.engine.protectedRoots.refresh(ctx); refreshErr != nil {
		return fmt.Errorf("sync: refresh shortcut protected roots: %w", refreshErr)
	}
	parentRoots, err := flow.deps.store.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read parent shortcut root state: %w", err)
	}
	if flow.engine.shortcutChildWorkSink == nil {
		return nil
	}
	return flow.engine.shortcutChildWorkSink(ctx, syncshortcut.ShortcutChildWorkSnapshotFromRootsWithParentRoot(
		topology.NamespaceID,
		flow.cfg.syncRoot,
		flow.cfg.contentFilter,
		parentRoots,
	))
}

func (flow *engineFlow) observeLocalCurrentState(
	ctx context.Context,
	bl syncdomain.BaselineView,
	recordHashes bool,
) (syncobserve.ScanResult, error) {
	localResult, err := flow.observeLocal(ctx, bl, recordHashes)
	if err != nil {
		return syncobserve.ScanResult{}, localCurrentRefreshFailure(localCurrentRefreshStepObservation, err)
	}

	return localResult, nil
}

func (flow *engineFlow) refreshAndCommitLocalCurrentState(
	ctx context.Context,
	bl syncdomain.BaselineView,
) (syncobserve.ScanResult, error) {
	localResult, err := flow.observeLocalCurrentState(ctx, bl, true)
	if err != nil {
		return syncobserve.ScanResult{}, err
	}
	if _, err := flow.commitLocalScan(
		ctx, syncobserve.BuildLocalStateRows(localResult), localResult.Skipped, localResult.CrossedMounts,
	); err != nil {
		return syncobserve.ScanResult{}, localCurrentRefreshFailure(localCurrentRefreshStepSnapshotCommit, err)
	}

	return localResult, nil
}

func (flow *engineFlow) observeRemoteWithShortcutTopology(
	ctx context.Context,
	bl syncdomain.BaselineView,
) ([]syncdomain.RemoteItem, string, syncshortcut.ShortcutTopologyBatch, error) {
	eng := flow.engine
	state, err := flow.deps.store.ReadObservationState(ctx)
	if err != nil {
		return nil, "", syncshortcut.ShortcutTopologyBatch{}, fmt.Errorf("sync: reading observation state: %w", err)
	}
	savedToken := state.Cursor

	obs := eng.buildRemoteObserver(bl, flow.deps.logger)
	obs.SetItemClient(eng.itemsClient)
	obs.SetRemotePathLookup(eng.remotePathLookup(ctx))
	if refreshErr := eng.protectedRoots.refresh(ctx); refreshErr != nil {
		return nil, "", syncshortcut.ShortcutTopologyBatch{}, fmt.Errorf("sync: refresh shortcut protected roots: %w", refreshErr)
	}
	obs.SetShortcutTopology(flow.cfg.shortcutNamespaceID, eng.protectedRoots.current())

	events, token, topology, err := obs.FullDeltaWithShortcutTopology(ctx, savedToken)
	if err != nil {
		if errors.Is(err, syncdomain.ErrDeltaExpired) {
			// The caller recovers through the full refresh with orphan
			// detection (R-2.15.3); keep the resync flavor in the chain.
			return nil, "", syncshortcut.ShortcutTopologyBatch{}, err
		}
		return nil, "", syncshortcut.ShortcutTopologyBatch{}, fmt.Errorf("sync: observing remote delta: %w", err)
	}

	return events, token, topology, nil
}

// observeLocal scans the local filesystem for changes and collects skipped
// items (invalid names, path too long, file too large) for failure recording.
// The observer also receives platform-derived naming rules from the engine so
// SharePoint-specific validation stays aligned across one-shot, watch, and
// retry/trial observation paths.
//
// recordHashes installs the durable hash cache (R-2.2.8); a dry run passes
// false, since it must leave the store as it found it.
func (flow *engineFlow) observeLocal(
	ctx context.Context,
	bl syncdomain.BaselineView,
	recordHashes bool,
) (syncobserve.ScanResult, error) {
	eng := flow.engine
	if err := eng.protectedRoots.refresh(ctx); err != nil {
		return syncobserve.ScanResult{}, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}

	obs := eng.buildLocalObserver(bl, flow.deps.logger)
	// This scan runs no timers. Its racily-clean window is measured on the
	// run's clock, the one every other time decision of the run reads.
	if flow.deps.clock != nil {
		obs.SetClock(flow.deps.clock)
	}
	if recordHashes {
		cache, cacheErr := flow.deps.store.NewLocalHashCache(ctx)
		if cacheErr != nil {
			return syncobserve.ScanResult{}, cacheErr
		}
		obs.SetLocalHashCache(cache)
	}
	obs.SetFilterConfig(flow.cfg.contentFilter)
	obs.SetProtectedRoots(eng.protectedRoots.current())
	obs.SetObservationRules(flow.cfg.localRules)
	obs.SetSyncRootIdentity(eng.syncRootIdentity)
	mountFlags, err := flow.deps.store.ListMountBoundaries(ctx)
	if err != nil {
		return syncobserve.ScanResult{}, err
	}
	obs.SetMountBoundaryFlags(mountFlags)

	result, err := obs.FullScan(ctx, flow.cfg.syncTree)
	if err != nil {
		return syncobserve.ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
}

// errReadingPlanningState is the one wrap of a planning-state load error,
// the same in a one-shot pass, a dry run and every watch replan: the store's
// own error under it names the read that failed.
const errReadingPlanningState = "sync: reading sync state: %w"

func (flow *engineFlow) loadDryRunCurrentObservation(
	ctx context.Context,
	bl syncdomain.BaselineView,
	fullReconcile bool,
) (result *currentObservation, err error) {
	observationBatch, err := flow.executePrimaryRootObservation(ctx, bl, fullReconcile)
	if err != nil {
		return nil, err
	}

	localResult, err := flow.observeLocalCurrentState(ctx, bl, false)
	if err != nil {
		return nil, err
	}

	scratchStore, cleanup, err := flow.deps.store.CreateScratchPlanningStore(ctx, bl)
	if err != nil {
		return nil, err
	}
	defer func() {
		// The scratch store's close runs its checkpoint detached under its
		// own bound, so a canceled ctx does not skip the cleanup.
		if cleanupErr := cleanup(ctx); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	commitErr := scratchStore.CommitObservation(ctx, observationBatch.Observed, "", flow.cfg.driveID)
	if commitErr != nil {
		return nil, fmt.Errorf("sync: committing dry-run remote snapshot to scratch store: %w", commitErr)
	}
	if reconcileErr := scratchStore.ReconcileObservationFindings(ctx, &observationBatch.Findings); reconcileErr != nil {
		return nil, fmt.Errorf("sync: reconciling dry-run remote observation findings in scratch store: %w", reconcileErr)
	}
	flow.logSkippedObservationFindings(localResult.Skipped, flow.knownObservationIssues(ctx))
	localFindings := localObservationFindingsBatchFromSkippedItems(flow.cfg.driveID, localResult.Skipped)
	if _, commitErr := scratchStore.CommitLocalSnapshot(ctx, &syncstore.LocalSnapshotCommit{
		Rows:          syncobserve.BuildLocalStateRows(localResult),
		Findings:      &localFindings,
		CrossedMounts: localResult.CrossedMounts,
	}); commitErr != nil {
		return nil, fmt.Errorf("sync: committing dry-run local snapshot to scratch store: %w", commitErr)
	}

	inputs, err := flow.loadCurrentInputs(ctx, scratchStore, flow.cfg.driveID)
	if err != nil {
		return nil, fmt.Errorf(errReadingPlanningState, err)
	}

	return &currentObservation{
		inputs:        inputs,
		observedPaths: len(inputs.localRows) + len(inputs.remoteRows),
	}, nil
}

func (flow *engineFlow) loadCommittedCurrentObservation(
	ctx context.Context,
	pendingRemoteObservation *syncobserve.RemoteObservationBatch,
) (*currentObservation, error) {
	inputs, err := flow.loadCurrentInputs(ctx, flow.deps.store, flow.cfg.driveID)
	if err != nil {
		return nil, fmt.Errorf(errReadingPlanningState, err)
	}
	childPublication, err := flow.currentShortcutChildPublication(ctx)
	if err != nil {
		return nil, fmt.Errorf(errReadingPlanningState, err)
	}

	return &currentObservation{
		inputs:                   inputs,
		observedPaths:            len(inputs.localRows) + len(inputs.remoteRows),
		pendingRemoteObservation: pendingRemoteObservation,
		childPublication:         childPublication,
	}, nil
}

func (flow *engineFlow) currentShortcutChildPublication(
	ctx context.Context,
) (syncshortcut.ShortcutChildWorkSnapshot, error) {
	if flow == nil || flow.deps.store == nil ||
		flow.cfg.shortcutNamespaceID == "" {
		return syncshortcut.ShortcutChildWorkSnapshot{}, nil
	}
	roots, err := flow.deps.store.ListShortcutRoots(ctx)
	if err != nil {
		return syncshortcut.ShortcutChildWorkSnapshot{}, fmt.Errorf("sync: read current shortcut child publication: %w", err)
	}
	return syncshortcut.ShortcutChildWorkSnapshotFromRootsWithParentRoot(
		flow.cfg.shortcutNamespaceID,
		flow.cfg.syncRoot,
		flow.cfg.contentFilter,
		roots,
	), nil
}

func (flow *engineFlow) loadCurrentInputs(
	ctx context.Context,
	store *syncstore.SyncStore,
	defaultDriveID driveid.ID,
) (currentInputs, error) {
	tx, err := store.BeginPerfTx(ctx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: beginning current action planner read transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			flow.deps.logger.Debug("current action planner read transaction rollback failed",
				slog.Any("error", rollbackErr),
			)
		}
	}()

	return flow.loadCurrentInputsTx(ctx, tx, defaultDriveID)
}

func (flow *engineFlow) loadCurrentInputsTx(
	ctx context.Context,
	tx syncstore.SqlTxRunner,
	defaultDriveID driveid.ID,
) (currentInputs, error) {
	rawLocalRows, err := syncstore.ListLocalStateRows(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: listing local_state rows: %w", err)
	}
	observationState, err := syncstore.ReadObservationStateFromTx(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: reading observation state for remote_state: %w", err)
	}
	contentDriveID := observationState.ContentDriveID
	if contentDriveID.IsZero() {
		contentDriveID = defaultDriveID
	}
	remoteRows, err := syncstore.QueryRemoteStateRowsWithRunner(
		ctx,
		tx,
		`SELECT `+syncstore.SqlSelectRemoteStateCols+` FROM remote_state`,
		contentDriveID,
	)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: listing remote_state rows: %w", err)
	}
	localRows := syncstore.FilterLocalStateRowsForPlanning(rawLocalRows, flow.cfg.contentFilter)
	remoteRows = syncstore.FilterRemoteStateRowsForPlanning(remoteRows, flow.cfg.contentFilter)
	plannerVisibleRows, err := syncstore.ReplacePlannerVisibleStateTx(ctx, tx, localRows, remoteRows, contentDriveID)
	if err != nil {
		return currentInputs{}, err
	}
	comparisons, reconciliations, err := flow.queryPlannerRowsTx(ctx, tx)
	if err != nil {
		return currentInputs{}, err
	}
	observationIssues, err := syncstore.QueryObservationIssueRowsWithRunner(ctx, tx)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: listing observation issues: %w", err)
	}
	observationIssues = syncstore.FilterObservationIssueRowsForPlanning(observationIssues, flow.cfg.contentFilter)
	snapshotBaseline, err := syncstore.LoadBaselineWithRunner(ctx, tx, contentDriveID)
	if err != nil {
		return currentInputs{}, fmt.Errorf("sync: loading planning baseline snapshot: %w", err)
	}
	return currentInputs{
		baseline:          snapshotBaseline,
		comparisons:       comparisons,
		reconciliations:   reconciliations,
		localRows:         plannerVisibleRows.Local,
		remoteRows:        plannerVisibleRows.Remote,
		observationIssues: observationIssues,
	}, nil
}

// queryPlannerRowsTx reads the comparison and reconciliation rows over the
// planner-visible tables. When a row came back unclassified, it also reads
// that row's decision-table inputs in the same transaction, the only place
// they can be read (the planner-visible tables are transaction-local), so the
// plan error PlanCurrentState returns names them (R-6.2.16). A plan whose
// rows are all classified pays nothing for it.
func (flow *engineFlow) queryPlannerRowsTx(
	ctx context.Context,
	tx syncstore.SqlTxRunner,
) ([]syncdomain.SQLiteComparisonRow, []syncdomain.SQLiteReconciliationRow, error) {
	comparisons, err := syncstore.QueryComparisonStateWithRunnerForTables(
		ctx,
		tx,
		syncstore.PlannerVisibleLocalStateTable,
		syncstore.PlannerVisibleRemoteStateTable,
	)
	if err != nil {
		return nil, nil, err
	}
	reconciliationQuery := syncstore.SqlQueryReconciliationState
	if flow.hooks.reconciliationQuery != "" {
		reconciliationQuery = flow.hooks.reconciliationQuery
	}
	reconciliations, err := syncstore.QueryReconciliationRows(ctx, tx, syncstore.SqlQueryForStateTables(
		reconciliationQuery,
		syncstore.PlannerVisibleLocalStateTable,
		syncstore.PlannerVisibleRemoteStateTable,
	))
	if err != nil {
		return nil, nil, err
	}
	first := syncplan.FirstUnclassifiedRow(reconciliations)
	if first < 0 {
		return comparisons, reconciliations, nil
	}
	reconciliations[first].DecisionInputs, err = syncstore.QueryPlannerDecisionInputs(
		ctx,
		tx,
		reconciliations[first].Path,
		syncstore.PlannerVisibleLocalStateTable,
		syncstore.PlannerVisibleRemoteStateTable,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: reading the decision inputs of unclassified %q: %w", reconciliations[first].Path, err)
	}

	return comparisons, reconciliations, nil
}

// buildCurrentPlanStage is the shared planning stage after an entrypoint has
// already observed current truth. It builds the current plan and report but
// does not touch durable retry/scope state.
func (flow *engineFlow) buildCurrentPlanStage(
	ctx context.Context,
	bl syncdomain.BaselineView,
	mode syncdomain.SyncMode,
	opts RunOptions,
	observation *currentObservation,
) (*builtCurrentPlan, error) {
	if observation == nil {
		return nil, fmt.Errorf("sync: building current plan: missing observed state")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sync: building current plan from observed state: %w", err)
	}

	planStart := flow.deps.now()
	plan, err := flow.engine.buildCurrentActionPlanFromInputs(&observation.inputs, bl, mode, opts)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	var approved []syncdomain.HeldDelete
	if opts.ConfirmDeletes {
		approved, err = flow.deps.store.ListHeldDeletes(ctx)
		if err != nil {
			return nil, fmt.Errorf("sync: reading the held deletes to confirm: %w", err)
		}
	}
	recentDeletes, err := flow.deps.store.RecentUnconfirmedDeletes(ctx, flow.deps.now().Add(-syncdomain.DeleteSafetyWindow))
	if err != nil {
		return nil, fmt.Errorf("sync: reading the recent deletes the hold counts: %w", err)
	}
	mountBoundaries, err := flow.deps.store.ListMountBoundaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: reading the recorded mount points the hold guards: %w", err)
	}
	if err := syncplan.ApplyDeleteSafetyHold(plan, &syncplan.DeleteSafetyInput{
		Baseline:        planningBaseline(&observation.inputs, bl),
		LocalRows:       observation.inputs.localRows,
		RemoteRows:      observation.inputs.remoteRows,
		Approved:        approved,
		RecentDeletes:   recentDeletes,
		MountBoundaries: mountBoundaries,
	}); err != nil {
		return nil, fmt.Errorf("sync: holding unconfirmed deletes: %w", err)
	}
	if len(approved) > 0 {
		// The confirmation reviewed the deletes that went before it on
		// those sides: they stop counting toward the next hold.
		if err := flow.deps.store.ClearDeleteLedger(ctx, syncdomain.HeldDeleteSides(approved)); err != nil {
			return nil, fmt.Errorf("sync: clearing the confirmed sides of the delete ledger: %w", err)
		}
	}
	logDeleteSafetyHold(flow.deps.logger, plan.HeldDeletes)
	flow.deps.perf.RecordPlan(len(plan.Actions), flow.deps.since(planStart))

	counts := syncplan.CountByType(plan.Actions)
	report := buildReportFromCounts(counts, plan.DeferredByMode, mode, opts)
	report.HeldLocalDeletes, report.HeldRemoteDeletes = syncdomain.CountHeldDeletesBySide(plan.HeldDeletes)

	return &builtCurrentPlan{
		Plan:                     plan,
		Report:                   report,
		PendingRemoteObservation: observation.pendingRemoteObservation,
		ChildPublication:         observation.childPublication,
	}, nil
}

// reconcileRuntimeStateStage turns a built current plan into the runtime-start
// handoff by reconciling durable held-work state and loading the surviving
// retry/block-scope rows the runtime owns. observed is what the pass observed
// before it planned.
func (flow *engineFlow) reconcileRuntimeStateStage(
	ctx context.Context,
	build *builtCurrentPlan,
	observed syncstore.PassObservation,
) (*runtimePlan, error) {
	if build == nil {
		return nil, fmt.Errorf("sync: reconciling runtime state: missing current plan build")
	}
	if err := flow.reconcileRuntimeState(ctx, build.Plan, observed); err != nil {
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
		ChildPublication:         build.ChildPublication,
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
		ChildPublication:         build.ChildPublication,
	}
}

func (flow *engineFlow) reconcileRuntimeState(ctx context.Context, plan *syncdomain.ActionPlan, observed syncstore.PassObservation) error {
	if err := flow.deps.store.ReplaceHeldDeletes(ctx, plan.HeldDeletes); err != nil {
		return fmt.Errorf("sync: recording held deletes: %w", err)
	}
	if err := flow.retryWork.prune(ctx, flow.deps.store, retryWorkKeysForActions(plan.Actions)); err != nil {
		return fmt.Errorf("sync: pruning retry_work to current actions: %w", err)
	}

	if err := flow.deps.store.PruneBlockScopesWithoutBlockedWork(ctx); err != nil {
		return fmt.Errorf("sync: pruning block scopes without blocked work: %w", err)
	}
	// Observation claims the empty file of an intent whose create landed.
	// After a pass that observed OneDrive before the local tree, an intent
	// still without an item is judged by its age and the local scan; after a
	// watch replan, which observed the local tree only, by its age alone, and
	// the change batch that shows a landed create still claims it. The record
	// of an adopted placeholder whose local file is empty too, and the record
	// of an item at a path the scan does not visit, go after any plan.
	if err := flow.deps.store.PrunePendingCreates(ctx, observed, flow.localScanCoverage()); err != nil {
		return fmt.Errorf("sync: pruning pending creates: %w", err)
	}

	return nil
}

// localScanCoverage returns the test of whether a full local scan visits a
// path: the content filter hides some paths, and a shortcut root reserves its
// subtree for the shortcut's own sync (shouldObserveWithFilter). local_state
// never holds such a path, which says nothing about a file there.
func (flow *engineFlow) localScanCoverage() func(path string) bool {
	filter := syncdomain.NewContentFilter(flow.cfg.contentFilter)
	protectedRoots := flow.engine.protectedRoots.current()

	return func(path string) bool {
		if _, reserved := syncshortcut.ProtectedRootPathReservation(path, protectedRoots); reserved {
			return false
		}

		return filter.Visible(path, syncdomain.ItemTypeFile)
	}
}

func (flow *engineFlow) loadRuntimeState(ctx context.Context) ([]syncdomain.RetryWorkRow, []*syncdomain.BlockScope, error) {
	retryRows, err := flow.deps.store.ListRetryWork(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: listing retry_work for runtime-state handoff: %w", err)
	}

	blockScopes, err := flow.deps.store.ListBlockScopes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: listing block_scopes for runtime-state handoff: %w", err)
	}

	return retryRows, blockScopes, nil
}

// planningBaseline is the baseline the planner read: the snapshot loaded in
// the same transaction as the comparison rows when there is one.
func planningBaseline(inputs *currentInputs, fallback syncdomain.BaselineView) syncdomain.BaselineView {
	if inputs != nil && inputs.baseline != nil {
		return inputs.baseline
	}
	return fallback
}

// buildCurrentActionPlanFromInputs plans the run's actions from the inputs
// one transaction read. A removed shared folder's final drain
// (RunOptions.ReleaseLocalTree) plans for a mount that releases its local
// tree, so it creates nothing new locally (leaveOutNewLocalItems).
func (e *Engine) buildCurrentActionPlanFromInputs(
	inputs *currentInputs,
	bl syncdomain.BaselineView,
	mode syncdomain.SyncMode,
	opts RunOptions,
) (*syncdomain.ActionPlan, error) {
	if inputs.baseline != nil {
		bl = inputs.baseline
	}
	return e.planner.PlanCurrentState(
		inputs.comparisons,
		inputs.reconciliations,
		inputs.localRows,
		inputs.remoteRows,
		inputs.observationIssues,
		bl,
		syncplan.PlannerMountContext{
			DriveID:           e.driveID,
			ReleasesLocalTree: opts.ReleaseLocalTree,
		},
		mode,
	)
}

func retryWorkKeysForActions(actions []syncdomain.Action) []syncdomain.RetryWorkKey {
	keys := make([]syncdomain.RetryWorkKey, 0, len(actions))
	seen := make(map[syncdomain.RetryWorkKey]struct{}, len(actions))

	for i := range actions {
		key := syncdomain.RetryWorkKeyForAction(&actions[i])
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	return keys
}

// logDeleteSafetyHold reports a pass whose deletes were held. Info, not Warn:
// watch mode replans on every batch while the hold stands, and the durable
// condition (status, the watch condition summary, the one-shot summary line)
// is the user-facing report.
func logDeleteSafetyHold(logger *slog.Logger, held []syncdomain.HeldDelete) {
	if logger == nil || len(held) == 0 {
		return
	}
	local, remote := syncdomain.CountHeldDeletesBySide(held)
	logger.Info("deletes held for confirmation; run 'onedrive-go sync --confirm-deletes' to apply them",
		slog.Int("held_local_deletes", local),
		slog.Int("held_remote_deletes", remote),
		slog.String("reason", string(held[0].Reason)),
	)
}
