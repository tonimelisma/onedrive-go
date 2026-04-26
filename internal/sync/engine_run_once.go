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

	// SQLite-derived plan approved — commit the deferred observation progress now.
	if _, err := runner.commitPendingRemoteObservation(ctx, runtime.PendingRemoteObservation); err != nil {
		return nil, err
	}

	plan := runtime.Plan
	report := runtime.Report

	if len(plan.Actions) == 0 {
		if report.DeferredByMode.Total() > 0 {
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

	return e.completeDryRunReport(start, runtime.Report), nil
}

func (e *Engine) runRunOnceStartup(
	ctx context.Context,
	runner *oneShotRunner,
) (*Baseline, error) {
	return runner.runStartupStage(ctx, nil)
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
	r.dispatchCh = make(chan *TrackedAction, len(plan.Actions))
	initialOutbox, done, err := r.dispatchInitialReadyActions(ctx, bl, runtime, report)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	pool := NewWorkerPool(r.engine.execCfg, r.dispatchCh, r.engine.baseline, r.engine.logger, len(plan.Actions))
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
) ([]*TrackedAction, bool, error) {
	initialOutbox, dispatched, err := r.startRuntimeStage(ctx, runtime, bl, nil)
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
	counts map[ActionType]int,
	conflicts int,
	deferred DeferredCounts,
	mode SyncMode,
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
		if err := flow.engine.baseline.CommitObservationCursor(
			ctx,
			flow.engine.driveID,
			observation.cursorToken,
		); err != nil {
			return false, fmt.Errorf("sync: committing primary observation cursor: %w", err)
		}
	}
	if observation.markFullRemoteRefresh {
		if err := flow.engine.baseline.MarkFullRemoteRefresh(
			ctx,
			flow.engine.driveID,
			flow.engine.nowFunc(),
			observation.observationMode,
		); err != nil {
			return false, fmt.Errorf("sync: marking full remote refresh: %w", err)
		}
		return false, nil
	}
	if observation.observationMode == remoteObservationModeEnumerate {
		deadlineChanged, err := flow.engine.baseline.ClampFullRemoteRefreshDeadline(
			ctx,
			flow.engine.driveID,
			flow.engine.nowFunc().Add(remoteRefreshEnumerateInterval),
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
func (flow *engineFlow) observeRemoteFull(ctx context.Context, bl *Baseline) ([]ChangeEvent, string, error) {
	events, token, _, err := flow.observeRemoteFullWithShortcutTopology(ctx, bl)
	return events, token, err
}

func (flow *engineFlow) observeRemoteFullWithShortcutTopology(
	ctx context.Context,
	bl *Baseline,
) ([]ChangeEvent, string, shortcutTopologyBatch, error) {
	eng := flow.engine
	obs := NewRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)
	obs.SetItemClient(eng.itemsClient)
	if err := eng.refreshProtectedRootsFromStore(ctx); err != nil {
		return nil, "", shortcutTopologyBatch{}, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}
	obs.SetShortcutTopology(eng.shortcutTopologyNamespaceID, eng.protectedRoots)

	// Full enumeration: empty token returns ALL items as create/modify events.
	events, token, topology, err := obs.FullDeltaWithShortcutTopology(ctx, "")
	if err != nil {
		return nil, "", shortcutTopologyBatch{}, fmt.Errorf("sync: full remote refresh delta: %w", err)
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
		eng.logger.Info("full remote refresh: detected orphaned items",
			slog.Int("orphans", len(orphans)),
		)

		events = append(events, orphans...)
	}

	eng.logger.Info("full remote refresh complete",
		slog.Int("total_events", len(events)),
		slog.Int("orphans", len(orphans)),
	)

	return events, token, topology, nil
}
