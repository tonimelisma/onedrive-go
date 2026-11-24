package sync

import (
	"context"
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

	build, err := r.buildCurrentPlanStage(ctx, mode, opts, observation)
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

	build, err := r.buildCurrentPlanStage(ctx, mode, opts, observation)
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
	fullRefresh, err := rt.shouldRunFullRemoteRefresh(ctx, false)
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

	build, err := rt.buildCurrentPlanStage(ctx, mode, RunOptions{}, observation)
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
	mode syncdomain.SyncMode,
) (*runtimePlan, error) {
	observation, err := rt.loadCommittedCurrentObservation(ctx, nil)
	if err != nil {
		return nil, err
	}

	build, err := rt.buildCurrentPlanStage(ctx, mode, RunOptions{}, observation)
	if err != nil {
		return nil, err
	}

	return rt.reconcileRuntimeStateStage(ctx, build, syncstore.PassRefreshedLocalOnly)
}

type currentObservation struct {
	snapshot                 *syncdomain.PlanningSnapshot
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
	if flow == nil || flow.shortcuts == nil || batch == nil {
		return nil
	}

	topology := batch.ShortcutTopology
	if topology.NamespaceID == "" {
		topology.NamespaceID = flow.cfg.shortcutNamespaceID
	}
	if err := flow.shortcuts.confirmShortcutCompleteOmissions(ctx, &topology); err != nil {
		return err
	}
	remoteChanged := topology.ShouldApply()
	if remoteChanged {
		if _, err := flow.deps.store.ApplyShortcutTopology(ctx, topology); err != nil {
			return fmt.Errorf("sync: persist parent shortcut root state: %w", err)
		}
	}
	localChanged, err := flow.shortcuts.reconcileShortcutRootLocalState(ctx)
	if err != nil {
		return fmt.Errorf("sync: reconcile parent shortcut root local state: %w", err)
	}
	if !remoteChanged && !localChanged {
		return nil
	}
	if refreshErr := flow.roots.refresh(ctx); refreshErr != nil {
		return fmt.Errorf("sync: refresh shortcut protected roots: %w", refreshErr)
	}
	parentRoots, err := flow.deps.store.ListShortcutRoots(ctx)
	if err != nil {
		return fmt.Errorf("sync: read parent shortcut root state: %w", err)
	}
	if flow.shortcuts.sink == nil {
		return nil
	}
	return flow.shortcuts.sink(ctx, syncshortcut.ShortcutChildWorkSnapshotFromRootsWithParentRoot(
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

func (flow *observation) observeRemoteWithShortcutTopology(
	ctx context.Context,
	bl syncdomain.BaselineView,
) ([]syncdomain.RemoteItem, string, syncshortcut.ShortcutTopologyBatch, error) {
	state, err := flow.deps.store.ReadObservationState(ctx)
	if err != nil {
		return nil, "", syncshortcut.ShortcutTopologyBatch{}, fmt.Errorf("sync: reading observation state: %w", err)
	}
	savedToken := state.Cursor

	if refreshErr := flow.roots.refresh(ctx); refreshErr != nil {
		return nil, "", syncshortcut.ShortcutTopologyBatch{}, fmt.Errorf("sync: refresh shortcut protected roots: %w", refreshErr)
	}
	obs := flow.remote.observer(ctx, bl, flow.deps.logger, flow.cfg.shortcutNamespaceID, flow.roots.current())

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
	if err := flow.roots.refresh(ctx); err != nil {
		return syncobserve.ScanResult{}, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}

	obs := flow.local.observer(bl, flow.deps.logger)
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
	obs.SetProtectedRoots(flow.roots.current())
	obs.SetObservationRules(flow.cfg.localRules)
	obs.SetSyncRootIdentity(flow.local.identity)
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

	scratchStore, cleanup, err := flow.deps.store.CreateScratchPlanningStore(ctx)
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

	snapshot, err := flow.readPlanningSnapshot(ctx, scratchStore, flow.cfg.driveID)
	if err != nil {
		return nil, fmt.Errorf(errReadingPlanningState, err)
	}

	return &currentObservation{
		snapshot:      snapshot,
		observedPaths: len(snapshot.Local) + len(snapshot.Remote),
	}, nil
}

func (flow *engineFlow) loadCommittedCurrentObservation(
	ctx context.Context,
	pendingRemoteObservation *syncobserve.RemoteObservationBatch,
) (*currentObservation, error) {
	snapshot, err := flow.readPlanningSnapshot(ctx, flow.deps.store, flow.cfg.driveID)
	if err != nil {
		return nil, fmt.Errorf(errReadingPlanningState, err)
	}
	childPublication, err := flow.currentShortcutChildPublication(ctx)
	if err != nil {
		return nil, fmt.Errorf(errReadingPlanningState, err)
	}

	return &currentObservation{
		snapshot:                 snapshot,
		observedPaths:            len(snapshot.Local) + len(snapshot.Remote),
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

// readPlanningSnapshot reads the current truth a plan is built from in one
// store read (syncstore's ReadPlanningSnapshot). The store owns the read and
// the schema it runs over; the planner decides its rows
// (planCurrentActions). The caller wraps a read error once
// (errReadingPlanningState): the store's error names the read that failed.
func (flow *engineFlow) readPlanningSnapshot(
	ctx context.Context,
	store *syncstore.SyncStore,
	defaultDriveID driveid.ID,
) (*syncdomain.PlanningSnapshot, error) {
	snapshot, err := store.ReadPlanningSnapshot(ctx, defaultDriveID, flow.cfg.contentFilter)
	if err != nil {
		return nil, err
	}
	flow.mustAssertDecisionStatesValid(snapshot.Comparisons)

	return snapshot, nil
}

// decider is the decision table this run plans with: syncplan.Decide,
// unless a test replaced it (engineHooks.decideOverride).
func (flow *engineFlow) decider() syncplan.Decider {
	if flow.hooks.decideOverride != nil {
		return flow.hooks.decideOverride
	}

	return syncplan.Decide
}

// mustAssertDecisionStatesValid holds, under the engine's invariant checks,
// every row the comparison query produced to the states it can produce
// (syncdomain.DecisionState.Valid): the decision table's properties are
// proven over valid states only, so a row outside them is a query bug.
func (flow *engineFlow) mustAssertDecisionStatesValid(comparisons []syncdomain.ComparisonRow) {
	if !flow.invariantChecksEnabled() {
		return
	}
	for i := range comparisons {
		if err := comparisons[i].DecisionState().Valid(); err != nil {
			panic(fmt.Sprintf("planning %s: %v", comparisons[i].Path, err))
		}
	}
}

// buildCurrentPlanStage is the shared planning stage after an entrypoint has
// already observed current truth. It builds the current plan and report but
// does not touch durable retry/scope state.
func (flow *engineFlow) buildCurrentPlanStage(
	ctx context.Context,
	mode syncdomain.SyncMode,
	opts RunOptions,
	observation *currentObservation,
) (*builtCurrentPlan, error) {
	if observation == nil {
		return nil, errors.New("sync: building current plan: missing observed state")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("sync: building current plan from observed state: %w", err)
	}

	planStart := flow.deps.now()
	plan, err := flow.planCurrentActions(observation.snapshot, mode, opts)
	if err != nil {
		return nil, fmt.Errorf("sync: planning actions: %w", err)
	}
	approved, err := flow.approvedHeldDeletes(ctx, opts)
	if err != nil {
		return nil, err
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
		Baseline:        observation.snapshot.Baseline,
		LocalRows:       observation.snapshot.Local,
		RemoteRows:      observation.snapshot.Remote,
		Approved:        approved,
		RecentDeletes:   recentDeletes,
		MountBoundaries: mountBoundaries,
	}); err != nil {
		return nil, fmt.Errorf("sync: holding unconfirmed deletes: %w", err)
	}
	if len(approved) > 0 {
		// The confirmation reviewed the deletes that went before it on
		// those sides: they stop counting toward the next hold.
		if err := flow.deps.store.ClearDeleteLedger(ctx, heldDeleteSides(approved)); err != nil {
			return nil, fmt.Errorf("sync: clearing the confirmed sides of the delete ledger: %w", err)
		}
	}
	logDeleteSafetyHold(flow.deps.logger, plan.HeldDeletes)
	flow.deps.perf.RecordPlan(len(plan.Actions), flow.deps.since(planStart))

	counts := syncplan.CountByType(plan.Actions)
	report := buildReportFromCounts(counts, plan.DeferredByMode, mode, opts)
	report.LocalFolderDeletes, report.RemoteFolderDeletes = countFolderDeletes(plan.Actions)
	report.HeldLocalDeletes, report.HeldRemoteDeletes = countHeldDeletesBySide(plan.HeldDeletes)

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
		return nil, errors.New("sync: reconciling runtime state: missing current plan build")
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
	prunedUploads, err := flow.retryWork.prune(ctx, flow.deps.store, retryWorkKeysForActions(plan.Actions))
	if err != nil {
		return fmt.Errorf("sync: pruning retry_work to current actions: %w", err)
	}
	flow.dropGivenUpUploadSessions(ctx, plan, prunedUploads)

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

// dropGivenUpUploadSessions ends the upload sessions of the uploads whose
// retry rows the prune just deleted: no current action names them, so sync
// gave them up (a conflict took the path, the file moved or was deleted), and
// a session record kept for a resume would only hold the server session until
// it expired (R-5.6.13). It runs after the prune's transaction, never under
// it, since a cancel is a Graph call. A plan that deferred uploads by its mode
// (download-only) still wants them on the next run in both directions, so it
// drops nothing. An upload still in the plan keeps its row and its record, so
// its retry resumes. A failure to read the session store is logged and costs
// only the server session's quota until it expires: it is no reason to fail
// the pass.
func (flow *engineFlow) dropGivenUpUploadSessions(ctx context.Context, plan *syncdomain.ActionPlan, prunedUploads []string) {
	if len(prunedUploads) == 0 || plan.DeferredByMode.Uploads > 0 {
		return
	}
	dropped, err := flow.exec.DropUploadSessions(ctx, prunedUploads)
	if err != nil {
		flow.deps.logger.Warn("could not end the upload sessions of uploads sync gave up; they expire on their own",
			slog.Int("uploads", len(prunedUploads)),
			slog.Any("error", err),
		)
	}
	if dropped > 0 {
		flow.deps.logger.Debug("ended the upload sessions of uploads sync gave up",
			slog.Int("sessions", dropped),
		)
	}
}

// localScanCoverage returns the test of whether a full local scan visits a
// path: the content filter hides some paths, and a shortcut root reserves its
// subtree for the shortcut's own sync (shouldObserveWithFilter). local_state
// never holds such a path, which says nothing about a file there.
func (flow *engineFlow) localScanCoverage() func(path string) bool {
	filter := syncdomain.NewContentFilter(flow.cfg.contentFilter)
	protectedRoots := flow.roots.current()

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

// planCurrentActions plans the current actionable set from the snapshot one
// store read returned, deciding its rows with the run's decision table
// (decider). A removed shared folder's final drain
// (RunOptions.ReleaseLocalTree) plans for a mount that releases its local
// tree, so it creates nothing new locally (leaveOutNewLocalItems).
func (flow *engineFlow) planCurrentActions(
	snapshot *syncdomain.PlanningSnapshot,
	mode syncdomain.SyncMode,
	opts RunOptions,
) (*syncdomain.ActionPlan, error) {
	return flow.planner.PlanSnapshot(
		snapshot,
		flow.decider(),
		syncplan.PlannerMountContext{
			DriveID:           flow.cfg.driveID,
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
	local, remote := countHeldDeletesBySide(held)
	logger.Info("deletes held for confirmation; run 'onedrive-go sync --confirm-deletes' to apply them",
		slog.Int("held_local_deletes", local),
		slog.Int("held_remote_deletes", remote),
		slog.String("reason", string(held[0].Reason)),
	)
}

// countHeldDeletesBySide splits held deletes into the side each would remove
// content from, for the pass report.
func countHeldDeletesBySide(held []syncdomain.HeldDelete) (local int, remote int) {
	for i := range held {
		if held[i].Side == syncdomain.DeleteSafetySideLocal {
			local++
			continue
		}
		remote++
	}
	return local, remote
}

// heldDeleteSides lists the distinct sides of held, in order of first
// appearance.
func heldDeleteSides(held []syncdomain.HeldDelete) []syncdomain.DeleteSafetySide {
	var sides []syncdomain.DeleteSafetySide
	for i := range held {
		seen := false
		for _, side := range sides {
			if side == held[i].Side {
				seen = true
				break
			}
		}
		if !seen {
			sides = append(sides, held[i].Side)
		}
	}
	return sides
}
