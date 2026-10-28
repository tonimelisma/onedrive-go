package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
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
func (e *Engine) RunOnce(ctx context.Context, mode SyncMode, opts RunOptions) (*Report, error) {
	start := e.nowFunc()
	runner := newOneShotRunner(e)

	e.logger.Info("sync pass starting",
		slog.String("mode", mode.String()),
		slog.Bool("dry_run", opts.DryRun),
	)

	bl, err := e.runRunOnceStartup(ctx, runner)
	if err != nil {
		return nil, err
	}

	fullReconcile, err := e.shouldRunFullRemoteRefresh(ctx, opts.FullReconcile)
	if err != nil {
		return nil, err
	}
	opts.FullReconcile = fullReconcile

	if opts.DryRun {
		return e.runOnceDryRun(ctx, runner, bl, mode, opts, start)
	}

	runtime, err := runner.runLiveCurrentPlan(ctx, bl, mode, opts)
	if err != nil {
		return nil, err
	}
	if err := e.publishShortcutChildWorkSnapshot(ctx, runtime.ChildPublication); err != nil {
		return nil, err
	}

	// SQLite-derived plan approved — commit the deferred observation progress now.
	if _, err := runner.commitPendingRemoteObservation(ctx, runtime.PendingRemoteObservation); err != nil {
		return nil, err
	}

	plan := runtime.Plan
	report := runtime.Report

	if len(plan.Actions) == 0 {
		if report.DeferredByMode.Total() > 0 || report.HeldLocalDeletes+report.HeldRemoteDeletes > 0 {
			report.Duration = e.since(start)
			e.logRunOnceCompletion(report)
			return report, nil
		}
		return e.completeRunOnceWithoutChanges(start, mode, opts), nil
	}

	// Execute plan: run workers, drain results (failures, 403s, upload issues).
	executeStart := e.nowFunc()
	if err := runner.executePreparedPlan(ctx, runtime, bl); err != nil {
		report.Duration = e.since(start)
		e.collector().RecordExecute(len(plan.Actions), report.Succeeded, report.Failed, e.since(executeStart))
		return report, err
	}
	e.collector().RecordExecute(len(plan.Actions), report.Succeeded, report.Failed, e.since(executeStart))

	report.Duration = e.since(start)

	e.logRunOnceCompletion(report)

	runner.postSyncHousekeeping(ctx)

	return report, nil
}

func (e *Engine) runOnceDryRun(
	ctx context.Context,
	runner *oneShotRunner,
	bl *Baseline,
	mode SyncMode,
	opts RunOptions,
	start time.Time,
) (*Report, error) {
	runtime, err := runner.runDryRunCurrentPlan(ctx, bl, mode, opts)
	if err != nil {
		return nil, err
	}

	e.emitPlannedActivity(runtime.Plan)

	return e.completeDryRunReport(start, runtime.Report), nil
}

func (e *Engine) runRunOnceStartup(
	ctx context.Context,
	runner *oneShotRunner,
) (*Baseline, error) {
	return runner.runStartupStage(ctx)
}

func (r *oneShotRunner) executePreparedPlan(
	ctx context.Context,
	runtime *runtimePlan,
	bl *Baseline,
) error {
	if runtime == nil {
		return nil
	}

	plan := runtime.Plan
	report := runtime.Report
	if len(plan.Actions) == 0 {
		return nil
	}

	// Invariant: the current actionable-set builder always returns one
	// dependency slice per action. Assert here to catch regressions early.
	if len(plan.Actions) != len(plan.Deps) {
		r.deps.logger.Debug("plan invariant violation: Actions/Deps length mismatch",
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
	r.sched.dispatchCh = make(chan *trackedAction, len(plan.Actions))
	initialOutbox, done, err := r.dispatchInitialReadyActions(ctx, bl, runtime, report)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	pool := newWorkerPool(r.engine.execCfg, r.sched.dispatchCh, r.deps.store, r.deps.logger, len(plan.Actions))
	pool.perfCollector = r.engine.collector()
	pool.serialize = r.engine.serializeWorkers
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pool.Start(runCtx, r.engine.transferWorkers)

	// Process completions until the engine-owned runtime settles. Held retry/scope
	// actions intentionally keep graph nodes unresolved, so the dependency graph
	// tracks only dependency satisfaction, not runtime completion.
	drainErr := r.runResultsLoopWithInitialOutbox(runCtx, cancel, bl, pool.Completions(), initialOutbox)
	pool.Stop()
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

func (r *oneShotRunner) dispatchInitialReadyActions(
	ctx context.Context,
	bl *Baseline,
	runtime *runtimePlan,
	report *Report,
) ([]*trackedAction, bool, error) {
	initialOutbox, dispatched, err := r.startRuntimeStage(ctx, runtime, bl)
	if err != nil {
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		report.Errors = append(report.Errors, err)
		return nil, true, err
	}

	if !dispatched {
		r.logFailureSummary()
		report.Succeeded, report.Failed, report.Errors = r.resultStats()
		return nil, true, nil
	}

	return initialOutbox, false, nil
}

// buildReportFromCounts populates a Report with plan counts and directionally
// deferred work observed by the planner.
func buildReportFromCounts(
	counts map[actionType]int,
	deferred DeferredCounts,
	mode SyncMode,
	opts RunOptions,
) *Report {
	return &Report{
		Mode:            mode,
		DryRun:          opts.DryRun,
		FolderCreates:   counts[ActionFolderCreate],
		Moves:           counts[ActionLocalMove] + counts[ActionRemoteMove],
		Downloads:       counts[ActionDownload],
		Uploads:         counts[ActionUpload],
		LocalDeletes:    counts[ActionLocalDelete],
		RemoteDeletes:   counts[ActionRemoteDelete],
		ConflictCopies:  counts[ActionConflictCopy],
		BaselineUpdates: counts[ActionBaselineUpdate],
		Cleanups:        counts[ActionCleanup],
		DeferredByMode:  deferred,
	}
}

// commitPendingRemoteObservation advances the deferred primary observation
// cursor after the planner approves the changes. Full remote refreshes also
// persist the restart-safe full-remote cadence timestamp here.
func (flow *engineFlow) commitPendingRemoteObservation(
	ctx context.Context,
	observation *remoteObservationBatch,
) (bool, error) {
	if observation == nil {
		return false, nil
	}

	if observation.cursorToken != "" {
		if err := flow.deps.store.CommitObservationCursor(
			ctx,
			flow.engine.driveID,
			observation.cursorToken,
		); err != nil {
			return false, fmt.Errorf("sync: committing primary observation cursor: %w", err)
		}
	}
	if observation.markFullRemoteRefresh {
		if err := flow.deps.store.MarkFullRemoteRefresh(
			ctx,
			flow.engine.driveID,
			flow.deps.now(),
			observation.observationMode,
		); err != nil {
			return false, fmt.Errorf("sync: marking full remote refresh: %w", err)
		}
		return false, nil
	}
	if observation.fullRefreshDueNow {
		deadlineChanged, err := flow.deps.store.ClampFullRemoteRefreshDeadline(
			ctx,
			flow.engine.driveID,
			flow.deps.now(),
		)
		if err != nil {
			return false, fmt.Errorf("sync: pulling full remote refresh forward: %w", err)
		}

		return deadlineChanged, nil
	}
	if observation.observationMode == remoteObservationModeEnumerate {
		deadlineChanged, err := flow.deps.store.ClampFullRemoteRefreshDeadline(
			ctx,
			flow.engine.driveID,
			flow.deps.now().Add(remoteRefreshEnumerateInterval),
		)
		if err != nil {
			return false, fmt.Errorf("sync: clamping full remote refresh: %w", err)
		}

		return deadlineChanged, nil
	}

	return false, nil
}

// observeRemoteFull runs a fresh delta with empty token (enumerates ALL remote
// items) and compares against the baseline to find orphans: items in baseline
// but not in the full enumeration = deleted remotely but missed by incremental
// delta. Returns all events (creates/modifies from the full enumeration +
// synthesized deletes for orphans) and the new delta token.
func (flow *engineFlow) observeRemoteFull(ctx context.Context, bl *Baseline) ([]changeEvent, string, error) {
	events, token, _, err := flow.observeRemoteFullWithShortcutTopology(ctx, bl, resyncKindApplyDifferences)
	return events, token, err
}

// observeRemoteFullWithShortcutTopology is the one full-enumeration path with
// orphan detection: the periodic and requested full refreshes and every
// expired-delta recovery use it (R-2.15.3). kind says what a confirmed orphan
// means. A refresh this client chose is resyncKindApplyDifferences: an item
// OneDrive no longer has was deleted. A resync that asked for local items to
// be uploaded again, or did not say, turns orphans into re-uploads instead.
func (flow *engineFlow) observeRemoteFullWithShortcutTopology(
	ctx context.Context,
	bl *Baseline,
	kind resyncKind,
) ([]changeEvent, string, shortcutTopologyBatch, error) {
	eng := flow.engine
	obs := newRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)
	obs.SetItemClient(eng.itemsClient)
	if err := eng.refreshProtectedRootsFromStore(ctx); err != nil {
		return nil, "", shortcutTopologyBatch{}, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}
	obs.SetShortcutTopology(eng.shortcutNamespaceID, eng.protectedRoots)

	// Full enumeration: empty token returns ALL items as create/modify
	// events, plus the raw ID of every live item Graph returned. The seen set
	// is the raw IDs: an item the converter dropped was still enumerated, and
	// building seen from events synthesized a delete for it (D-008).
	events, token, topology, seen, err := obs.FullEnumerationWithShortcutTopology(ctx)
	if err != nil {
		return nil, "", shortcutTopologyBatch{}, fmt.Errorf("sync: full remote refresh delta: %w", err)
	}
	for i := range events {
		if !events[i].IsDeleted && events[i].ItemID != "" {
			seen[events[i].ItemID] = struct{}{}
		}
	}

	// Detect orphans: baseline entries whose ItemID is not in the seen set.
	orphans := findBaselineOrphans(bl, seen, eng.driveID, "")

	// remote_state can also hold items that were never synced (one whose name
	// the local filesystem cannot store is never downloaded). A missed delete
	// left such a row behind forever, keeping status at "Syncing" with a
	// phantom remote change, and the baseline scan above never sees it.
	remoteRows, err := eng.baseline.ListRemoteState(ctx)
	if err != nil {
		return nil, "", shortcutTopologyBatch{}, fmt.Errorf("sync: listing remote state for full refresh: %w", err)
	}
	orphans = append(orphans, findRemoteStateOrphans(remoteRows, seen, orphans, eng.driveID)...)

	// A complete enumeration that omits an item is not proof it is gone
	// (Graph omits items transiently); only a live 404 or deleted facet is.
	orphans = eng.confirmEnumerationOrphans(ctx, orphans, nil)
	if !kind.orphansAreDeletes() {
		markOrphansForReupload(orphans)
	}
	if len(orphans) > 0 {
		events = append(events, orphans...)
	}

	summary := fmt.Sprintf("Full OneDrive rescan complete: %d synced items no longer exist online", len(orphans))
	if !kind.orphansAreDeletes() && len(orphans) > 0 {
		summary = fmt.Sprintf("Full OneDrive rescan complete: %d items OneDrive no longer has will be uploaded again", len(orphans))
	}
	eng.logger.Info(summary,
		slog.Int("total_events", len(events)),
		slog.Int("orphans", len(orphans)),
	)

	return events, token, topology, nil
}

// markOrphansForReupload turns confirmed orphans into re-uploads: their
// remote rows go, and so does their synced agreement, so a surviving local
// copy plans as an upload rather than a local delete (R-2.15.3).
func markOrphansForReupload(orphans []changeEvent) {
	for i := range orphans {
		orphans[i].ReuploadLocal = true
	}
}
