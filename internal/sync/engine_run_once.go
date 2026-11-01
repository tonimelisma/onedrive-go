package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// oneShotMaxPasses bounds the passes one RunOnce makes, and the follow-up
// replans watch schedules by itself in a row. A plan that moves items leaves
// work for the pass after it (planFollowUp), and that work normally finishes
// in the second pass: an edit inside a renamed folder is uploaded, a parked
// swap item reaches its name. The bound only stops a planner that never
// settles from looping; whatever is left is the next sync's work.
const oneShotMaxPasses = 4

// RunOnce executes a one-shot sync: a pass, and further passes while the
// previous one left work for the next (R-2.1.7). Each pass:
//  1. Load baseline
//  2. Refresh remote truth
//  3. Refresh local truth
//  4. Read SQLite comparison and reconciliation from committed snapshots
//  5. Build the current actionable set in Go
//  6. Return early if dry-run
//  7. Build DepGraph, start worker pool
//  8. Wait for completion, commit delta token
//
// A dry run is always one pass: it executes nothing, so a second pass would
// see the same truth. The returned report covers every pass.
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

	var total *Report
	executed := false
	for pass := 1; ; pass++ {
		result, err := e.runOneShotLivePass(ctx, runner, bl, mode, opts)
		total = total.withPass(result.report)
		if total != nil {
			total.Duration = e.since(start)
		}
		if err != nil {
			return total, err
		}
		executed = executed || result.executed

		reason, again := e.oneShotFollowUp(ctx, pass, result)
		if !again {
			break
		}
		e.logger.Debug("running a follow-up sync pass for work the previous pass left",
			slog.Int("pass", pass+1),
			slog.String("reason", reason),
		)

		// A follow-up pass is an ordinary pass over freshly observed truth.
		// It decides the full refresh again (the first pass's request is
		// done) and never releases held deletes: those were confirmed by
		// the first pass, and any held since are judged by the hold again.
		runner = newOneShotRunner(e)
		if bl, err = e.baseline.Load(ctx); err != nil {
			return total, fmt.Errorf("sync: loading baseline for follow-up pass: %w", err)
		}
		if opts.FullReconcile, err = e.shouldRunFullRemoteRefresh(ctx, false); err != nil {
			return total, err
		}
		opts.ConfirmDeletes = false
		e.logger.Info("sync pass starting",
			slog.String("mode", mode.String()),
			slog.Bool("dry_run", false),
		)
	}

	if executed {
		runner.postSyncHousekeeping(ctx)
	}

	return total, nil
}

// oneShotPass is what one live pass of RunOnce did.
type oneShotPass struct {
	report   *Report
	followUp planFollowUp
	executed bool
}

// oneShotFollowUp decides whether RunOnce runs another pass after this one,
// and says why. Only the planner knows that work was left on purpose; the
// engine adds the conditions under which another pass cannot finish it: a
// pass that made no progress, failed actions (they wait in retry backoff, and
// the summary already says they will be retried), a canceled run, or the pass
// bound.
func (e *Engine) oneShotFollowUp(ctx context.Context, pass int, result oneShotPass) (string, bool) {
	f := result.followUp
	switch {
	case !f.pending() || result.report == nil:
		return "", false
	case result.report.Succeeded == 0 || result.report.Failed > 0 || ctx.Err() != nil:
		return "", false
	case pass >= e.oneShotPasses():
		e.logger.Debug("sync pass limit reached; the next sync finishes the remaining work",
			slog.Int("passes", pass),
		)
		return "", false
	}

	return fmt.Sprintf("renames=%d deferred=%d dropped=%d", f.Renames, f.Deferred, f.Dropped), true
}

// oneShotPasses is the most passes one RunOnce makes.
func (e *Engine) oneShotPasses() int {
	if e.oneShotPassLimit > 0 {
		return min(e.oneShotPassLimit, oneShotMaxPasses)
	}

	return oneShotMaxPasses
}

// runOneShotLivePass observes both sides, plans and executes one pass.
func (e *Engine) runOneShotLivePass(
	ctx context.Context,
	runner *oneShotRunner,
	bl *Baseline,
	mode SyncMode,
	opts RunOptions,
) (oneShotPass, error) {
	passStart := e.nowFunc()

	runtime, err := runner.runLiveCurrentPlan(ctx, bl, mode, opts)
	if err != nil {
		return oneShotPass{}, err
	}
	if err := e.publishShortcutChildWorkSnapshot(ctx, runtime.ChildPublication); err != nil {
		return oneShotPass{}, err
	}

	// SQLite-derived plan approved — commit the deferred observation progress now.
	if _, err := runner.commitPendingRemoteObservation(ctx, runtime.PendingRemoteObservation); err != nil {
		return oneShotPass{}, err
	}

	plan := runtime.Plan
	report := runtime.Report

	if len(plan.Actions) == 0 {
		if report.DeferredByMode.Total() > 0 || report.HeldLocalDeletes+report.HeldRemoteDeletes > 0 {
			report.Duration = e.since(passStart)
			e.logRunOnceCompletion(report)
			return oneShotPass{report: report}, nil
		}
		return oneShotPass{report: e.completeRunOnceWithoutChanges(passStart, mode, opts)}, nil
	}

	// Execute plan: run workers, drain results (failures, 403s, upload issues).
	executeStart := e.nowFunc()
	if err := runner.executePreparedPlan(ctx, runtime, bl); err != nil {
		report.Duration = e.since(passStart)
		e.collector().RecordExecute(len(plan.Actions), report.Succeeded, report.Failed, e.since(executeStart))
		return oneShotPass{report: report}, err
	}
	e.collector().RecordExecute(len(plan.Actions), report.Succeeded, report.Failed, e.since(executeStart))

	report.Duration = e.since(passStart)
	e.logRunOnceCompletion(report)

	return oneShotPass{report: report, followUp: plan.FollowUp, executed: true}, nil
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
		// An orphan the refresh could not confirm needs another full
		// enumeration (the saved delta position never reports it again),
		// sooner than the normal cadence but backing off while it stays
		// undecided.
		deadlineChanged, err := flow.deps.store.RecordFullRefreshOrphans(
			ctx, flow.engine.driveID, flow.deps.now(), observation.orphansUndecided,
		)
		if err != nil {
			return false, fmt.Errorf("sync: recording undecided orphans: %w", err)
		}
		if !observation.fullRefreshDueSoon {
			return deadlineChanged, nil
		}
	}
	if observation.fullRefreshDueSoon {
		deadlineChanged, err := flow.deps.store.ClampFullRemoteRefreshDeadline(
			ctx,
			flow.engine.driveID,
			// Deferred, not now: one burst of folder deletions shares one
			// enumeration, and a deadline only ever moves earlier, so
			// refreshes stay this far apart.
			flow.deps.now().Add(fullRefreshAfterFolderDeleteDelay),
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
	full, err := flow.observeRemoteFullWithShortcutTopology(ctx, bl, resyncKindApplyDifferences)
	return full.events, full.token, err
}

// fullRemoteObservation is what a full enumeration with orphan detection
// observed.
type fullRemoteObservation struct {
	events   []changeEvent
	token    string
	topology shortcutTopologyBatch
	// orphansUndecided reports an item missing from the enumeration whose
	// lookup failed. It is kept, not deleted, but the delta position saved
	// with this enumeration will never report it again: only another full
	// enumeration can, so the next one is due soon rather than in a day.
	orphansUndecided bool
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
) (fullRemoteObservation, error) {
	eng := flow.engine
	obs := newRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)
	obs.SetItemClient(eng.itemsClient)
	if err := eng.refreshProtectedRootsFromStore(ctx); err != nil {
		return fullRemoteObservation{}, fmt.Errorf("sync: refresh shortcut protected roots: %w", err)
	}
	obs.SetShortcutTopology(eng.shortcutNamespaceID, eng.currentProtectedRoots())

	// Full enumeration: empty token returns ALL items as create/modify
	// events, plus the raw ID of every live item Graph returned. The seen set
	// is the raw IDs: an item the converter dropped was still enumerated, and
	// building seen from events synthesized a delete for it (D-008).
	events, token, topology, seen, err := obs.FullEnumerationWithShortcutTopology(ctx)
	if err != nil {
		return fullRemoteObservation{}, fmt.Errorf("sync: full remote refresh delta: %w", err)
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
		return fullRemoteObservation{}, fmt.Errorf("sync: listing remote state for full refresh: %w", err)
	}
	orphans = append(orphans, findRemoteStateOrphans(remoteRows, seen, orphans, eng.driveID)...)

	// A complete enumeration that omits an item is not proof it is gone
	// (Graph omits items transiently); only a live 404 or deleted facet is.
	orphans, undecided := eng.confirmEnumerationOrphans(ctx, orphans, nil)
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

	return fullRemoteObservation{events: events, token: token, topology: topology, orphansUndecided: undecided}, nil
}

// markOrphansForReupload turns confirmed orphans into re-uploads: their
// remote rows go, and so does their synced agreement, so a surviving local
// copy plans as an upload rather than a local delete (R-2.15.3).
func markOrphansForReupload(orphans []changeEvent) {
	for i := range orphans {
		orphans[i].ReuploadLocal = true
	}
}
