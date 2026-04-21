package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

	prepared, err := runner.prepareLiveCurrentPlan(ctx, bl, mode, opts)
	if err != nil {
		return nil, err
	}

	// SQLite-derived plan approved — commit the deferred delta token now.
	if err := runner.commitPendingPrimaryCursor(ctx, prepared.pendingCursorCommit); err != nil {
		return nil, err
	}

	plan := prepared.plan
	report := prepared.report

	if len(plan.Actions) == 0 {
		if report.DeferredByMode.Total() > 0 {
			report.Duration = e.since(start)
			e.logRunOnceCompletion(report)
			e.writeOneShotRunStatusBestEffort(ctx, report)
			return report, nil
		}
		return e.completeRunOnceWithoutChanges(ctx, start, mode, opts), nil
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
	prepared, err := runner.prepareDryRunCurrentPlan(ctx, bl, mode, opts)
	if err != nil {
		return nil, err
	}

	return e.completeDryRunReport(start, prepared.report), nil
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

// commitPendingPrimaryCursor advances the primary observation cursor after the
// planner approves the changes. Full reconciliations also persist the restart-
// safe full-remote cadence timestamp here.
func (flow *engineFlow) commitPendingPrimaryCursor(
	ctx context.Context,
	pending *pendingPrimaryCursorCommit,
) error {
	if pending == nil {
		return nil
	}

	if pending.token != "" {
		if err := flow.engine.baseline.CommitObservationCursor(
			ctx,
			driveid.New(pending.driveID),
			pending.token,
		); err != nil {
			return fmt.Errorf("sync: committing primary observation cursor for root %q: %w", pending.rootID, err)
		}
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
