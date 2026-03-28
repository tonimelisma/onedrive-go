package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// periodicScanJitterDivisor controls the jitter window for periodic full
// scans. With a divisor of 10, each tick sleeps 0-10% of the interval to
// prevent thundering-herd I/O spikes in multi-drive mode.
const periodicScanJitterDivisor = 10

// Default watch intervals.
const (
	defaultPollInterval = 5 * time.Minute
	defaultDebounce     = 2 * time.Second
	watchEventBuf       = 256
	// watchResultBuf is the buffer size for the worker result channel in watch
	// mode. Large enough for typical batches without blocking workers.
	watchResultBuf = 4096

	// deleteCounterWindow is the rolling time window for the watch-mode
	// big-delete counter. Deletes within this window accumulate toward
	// the threshold. Expired entries drop off, preventing normal sustained
	// file management from triggering false positives.
	deleteCounterWindow = 5 * time.Minute

	// recheckInterval is how often the engine checks for external DB
	// changes (e.g., `issues clear` via the CLI). Uses PRAGMA data_version
	// — one integer comparison per tick, essentially free.
	recheckInterval = 10 * time.Second
)

// defaultReconcileInterval is the default interval for periodic full
// reconciliation in daemon mode. A full enumeration of 100K items costs
// ~500 API calls (~17% of a single 5-minute rate window), so 24h is safe.
const defaultReconcileInterval = 24 * time.Hour

// minReconcileInterval is the minimum allowed reconcile interval. A full
// enumeration of 100K items costs ~500 API calls; anything under 15 minutes
// risks rate-limit exhaustion.
const minReconcileInterval = 15 * time.Minute

// quiescenceLogInterval is how often bootstrapSync logs while waiting
// for in-flight actions to complete.
const quiescenceLogInterval = 30 * time.Second

// setShortcuts updates the shortcuts used by result processing and permission
// handling.
// Called by the watch goroutine after observation when shortcuts may have changed.
func (e *Engine) setShortcuts(shortcuts []synctypes.Shortcut) {
	e.watchShortcutsMu.Lock()
	e.watchShortcuts = shortcuts
	e.watchShortcutsMu.Unlock()
}

// getShortcuts returns the latest shortcuts for result processing and
// permission handling.
func (e *Engine) getShortcuts() []synctypes.Shortcut {
	e.watchShortcutsMu.RLock()
	defer e.watchShortcutsMu.RUnlock()

	return e.watchShortcuts
}

// initDeleteProtection sets up the rolling delete counter and clears stale
// big_delete_held entries from a prior daemon session. Force mode disables
// the counter (deleteCounter stays nil). Also seeds lastDataVersion so
// the first recheck tick doesn't fire spuriously.
func (e *Engine) initDeleteProtection(ctx context.Context, force bool) {
	if !force {
		e.watch.deleteCounter = syncdispatch.NewDeleteCounter(e.bigDeleteThreshold, deleteCounterWindow, time.Now)
	}

	if err := e.baseline.ClearResolvedActionableFailures(ctx, synctypes.IssueBigDeleteHeld, nil); err != nil {
		e.logger.Warn("failed to clear stale big-delete-held entries",
			slog.String("error", err.Error()),
		)
	}

	if dv, dvErr := e.baseline.DataVersion(ctx); dvErr == nil {
		e.watch.lastDataVersion = dv
	}
}

// loadWatchState loads the baseline and shortcuts for the watch session.
// Both are loaded once after the initial sync. synctypes.Baseline is live-mutated
// under RWMutex; shortcuts are updated via setShortcuts when they change.
func (e *Engine) loadWatchState(ctx context.Context) (*synctypes.Baseline, error) {
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline for watch: %w", err)
	}

	shortcuts, scErr := e.baseline.ListShortcuts(ctx)
	if scErr != nil {
		e.logger.Warn("failed to load shortcuts for watch mode",
			slog.String("error", scErr.Error()),
		)
	}

	e.setShortcuts(shortcuts)

	return bl, nil
}

// RunWatch runs a continuous sync loop: bootstrap sync through the watch
// pipeline, then watches for remote and local changes in batches.
// Blocks until the context is canceled, returning nil on clean shutdown.
//
// Flow: initWatchInfra → bootstrapSync → startObservers → runWatchLoop.
// Unlike the old approach (calling RunOnce with throwaway infrastructure),
// bootstrapSync dispatches through the same DepGraph, active scope working
// set, and WorkerPool that the steady-state watch loop uses.
func (e *Engine) RunWatch(ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts) error {
	e.logger.Info("watch mode starting",
		slog.String("mode", mode.String()),
		slog.Bool("force", opts.Force),
		slog.Duration("poll_interval", e.resolvePollInterval(opts)),
		slog.Duration("debounce", e.resolveDebounce(opts)),
	)

	// Step 1: Set up watch infrastructure (no observers yet).
	pipe, err := e.initWatchInfra(ctx, mode, opts)
	if err != nil {
		return err
	}
	defer pipe.cleanup()

	// Step 2: Bootstrap — observe, plan, execute through watch pipeline.
	if err := e.bootstrapSync(ctx, mode, pipe); err != nil {
		return fmt.Errorf("sync: initial sync failed: %w", err)
	}

	// Step 3: Start observers AFTER bootstrap — they see the post-bootstrap baseline.
	errs, activeObs, skippedCh := e.startObservers(ctx, pipe.bl, mode, e.watch.buf, opts)
	pipe.errs = errs
	pipe.activeObs = activeObs
	pipe.skippedCh = skippedCh

	// Step 4: Run the watch loop.
	return e.runWatchLoop(ctx, pipe)
}

// watchPipeline holds all handles needed by the watch select loop.
// Created by initWatchInfra; cleaned up by its cleanup method.
type watchPipeline struct {
	bl         *synctypes.Baseline
	safety     *synctypes.SafetyConfig
	ready      <-chan []synctypes.PathChanges
	results    <-chan synctypes.WorkerResult
	errs       <-chan error
	skippedCh  <-chan []synctypes.SkippedItem
	reconcileC <-chan time.Time
	recheckC   <-chan time.Time
	activeObs  int
	mode       synctypes.SyncMode
	pool       *syncexec.WorkerPool // for bootstrapSync to access Results()
	cleanup    func()
}

// initWatchInfra sets up watch-mode infrastructure: watchState, DepGraph,
// worker pool, buffer, persisted scope state, and tickers. Does NOT load
// baseline or start observers — those happen in bootstrapSync and RunWatch.
//
// Key differences from one-shot mode (executePlan):
//   - Active scopes are loaded from DB into engine-owned runtime state
//   - Done channel is never-closing — DepGraph.Done() fires when completed >= total,
//     which would prematurely close between batches. Workers exit only via ctx.Done().
//   - Retrier and trials are handled by the watch control flow itself
//   - Buffer is promoted to e.watch.buf so retrier/trial work can re-enter
//     via buffer → planner → tracker.
func (e *Engine) initWatchInfra(
	ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts,
) (*watchPipeline, error) {
	// Create watchState — all watch-mode-only fields live here.
	e.watch = &watchState{
		trialPending: make(map[string]trialEntry),
		retryTimerCh: make(chan struct{}, 1),
	}

	// Enable watch-mode-specific executor behavior (pre-upload eTag
	// freshness checks to prevent silently overwriting concurrent remote
	// changes — see executor_transfer.go).
	e.execCfg.SetWatchMode(true)

	e.initDeleteProtection(ctx, opts.Force)

	// Normalize persisted scope rows before loading runtime scope state.
	// Startup must not trust stale scope rows blindly; the durable store is
	// repaired against current persisted evidence before the watch loop loads
	// its ephemeral activeScopes working set.
	if err := e.repairPersistedScopes(ctx); err != nil {
		return nil, fmt.Errorf("sync: repairing persisted scopes: %w", err)
	}

	// DepGraph tracks action dependencies. Active scope state is loaded from
	// the persisted scope_blocks table into watch-owned runtime state.
	depGraph := syncdispatch.NewDepGraph(e.logger)
	e.depGraph = depGraph
	if err := e.loadActiveScopes(ctx); err != nil {
		return nil, fmt.Errorf("sync: loading active scopes: %w", err)
	}

	e.watch.scopeState = syncdispatch.NewScopeState(e.nowFunc, e.logger)
	e.watch.nextActionID = 0

	// readyCh feeds admitted actions to workers. Buffer is generous to avoid
	// backpressure when a batch produces many immediately-ready actions.
	e.readyCh = make(chan *synctypes.TrackedAction, watchResultBuf)

	// Never-closing done channel — DepGraph.Done() would fire prematurely
	// between batches when completed == total. Workers exit only via ctx.Done().
	neverDone := make(chan struct{})

	pool := syncexec.NewWorkerPool(e.execCfg, e.readyCh, neverDone, e.baseline, e.logger, watchResultBuf)
	pool.Start(ctx, e.transferWorkers)

	// Buffer promoted to watchState so retry/trial work can re-enter via
	// e.watch.buf.Add() and follow the normal planner/admission path.
	buf := syncobserve.NewBuffer(e.logger)
	e.watch.buf = buf
	ready := buf.FlushDebounced(ctx, e.resolveDebounce(opts))

	// Tickers.
	reconcileC, stopReconcile := e.initReconcileTicker(opts)
	recheckTicker := time.NewTicker(recheckInterval)

	// Arm retrier timer from DB — picks up items from prior crash or prior pass.
	e.kickRetrySweepNow()
	e.armTrialTimer()

	pipe := &watchPipeline{
		safety:     e.resolveSafetyConfig(opts.Force),
		ready:      ready,
		results:    pool.Results(),
		reconcileC: reconcileC,
		recheckC:   recheckTicker.C,
		mode:       mode,
		pool:       pool,
	}

	pipe.cleanup = func() {
		recheckTicker.Stop()
		if stopReconcile != nil {
			stopReconcile()
		}

		inFlight := depGraph.InFlightCount()
		if inFlight > 0 {
			e.logger.Info("graceful shutdown: draining in-flight actions",
				slog.Int("in_flight", inFlight),
			)
		}

		e.stopRetryTimer()
		e.stopTrialTimer()
		pool.Stop() // closes results channel after workers exit
		e.logger.Info("watch mode stopped")
	}

	return pipe, nil
}

// bootstrapSync performs the initial sync using the watch pipeline. Unlike
// the old approach (calling RunOnce with throwaway infrastructure), this
// dispatches through the same DepGraph, active scope working set, and WorkerPool that
// the watch loop uses. Blocks until all bootstrap actions complete.
//
// Must be called after initWatchInfra and before startObservers.
func (e *Engine) bootstrapSync(ctx context.Context, mode synctypes.SyncMode, pipe *watchPipeline) error {
	e.logger.Info("bootstrap sync starting", slog.String("mode", mode.String()))

	// Drive identity check (B-074).
	if err := e.verifyDriveIdentity(ctx); err != nil {
		return err
	}

	// Crash recovery: reset in-progress states from prior crash.
	if err := e.baseline.ResetInProgressStates(ctx, e.syncRoot, retry.ReconcilePolicy().Delay); err != nil {
		e.logger.Warn("failed to reset in-progress states", slog.String("error", err.Error()))
	}

	// Load baseline + shortcuts.
	bl, err := e.loadWatchState(ctx)
	if err != nil {
		return err
	}
	pipe.bl = bl

	// Permission rechecks.
	if e.permHandler.HasPermChecker() {
		shortcuts := e.getShortcuts()
		e.applyPermissionRecheckDecisions(ctx, e.permHandler.recheckPermissions(ctx, bl, shortcuts))
	}
	e.applyPermissionRecheckDecisions(ctx, e.permHandler.recheckLocalPermissions(ctx))

	// Observe changes.
	changes, pendingToken, err := e.observeChanges(ctx, bl, mode, false, false)
	if err != nil {
		return fmt.Errorf("sync: bootstrap observation failed: %w", err)
	}

	if len(changes) == 0 {
		e.logger.Info("bootstrap sync complete: no changes detected")
		return nil
	}

	// Commit the deferred delta token before dispatching bootstrap actions.
	// Bootstrap uses watch-mode big-delete (rolling counter), not planner-level
	// threshold, so the token is always safe to commit here.
	if err := e.commitDeferredDeltaToken(ctx, pendingToken); err != nil {
		return err
	}

	// Dispatch through watch pipeline (same path as steady-state batches).
	initialOutbox := e.processBatch(ctx, changes, bl, mode, pipe.safety)

	// Wait for all bootstrap actions to complete.
	if err := e.runWatchUntilQuiescent(ctx, pipe, initialOutbox); err != nil {
		return fmt.Errorf("sync: bootstrap quiescence failed: %w", err)
	}

	e.postSyncHousekeeping()
	e.logger.Info("bootstrap sync complete")
	return nil
}

// startObservers launches remote and local observer goroutines that feed
// events into the buffer. Returns an error channel for observer failures and
// the number of observers started. The events channel is closed automatically
// when all observers exit, allowing the bridge goroutine to drain cleanly.
func (e *Engine) startObservers(
	ctx context.Context, bl *synctypes.Baseline, mode synctypes.SyncMode, buf *syncobserve.Buffer, opts synctypes.WatchOpts,
) (<-chan error, int, <-chan []synctypes.SkippedItem) {
	events := make(chan synctypes.ChangeEvent, watchEventBuf)
	errs := make(chan error, 2)

	var obsWg stdsync.WaitGroup

	// Bridge goroutine: reads from shared events channel, adds to buffer.
	// Exits when events is closed (all observers done) or ctx canceled.
	go func() {
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}

				buf.Add(&ev)
			case <-ctx.Done():
				return
			}
		}
	}()

	count := 0

	// Remote observer (skip for upload-only mode).
	if mode != synctypes.SyncUploadOnly {
		remoteObs := syncobserve.NewRemoteObserver(e.fetcher, bl, e.driveID, e.logger)
		remoteObs.SetObsWriter(e.baseline)
		e.watch.remoteObs = remoteObs

		savedToken, tokenErr := e.baseline.GetDeltaToken(ctx, e.driveID.String(), "")
		if tokenErr != nil {
			e.logger.Warn("failed to get delta token for watch",
				slog.String("error", tokenErr.Error()),
			)
		}

		obsWg.Add(1)
		count++

		go func() {
			defer obsWg.Done()
			errs <- remoteObs.Watch(ctx, savedToken, events, e.resolvePollInterval(opts))
		}()
	}

	// Channel for forwarding SkippedItems from safety scans to the engine.
	// Buffered(2) — at most 2 safety scans could overlap before draining.
	skippedCh := make(chan []synctypes.SkippedItem, 2)

	// Local observer (skip for download-only mode).
	if mode != synctypes.SyncDownloadOnly {
		localObs := syncobserve.NewLocalObserver(bl, e.logger, e.checkWorkers)
		localObs.SetSafetyScanInterval(opts.SafetyScanInterval)
		localObs.SetSkippedChannel(skippedCh)

		if e.localWatcherFactory != nil {
			localObs.SetWatcherFactory(e.localWatcherFactory)
		}

		e.watch.localObs = localObs

		obsWg.Add(1)
		count++

		go func() {
			defer obsWg.Done()

			watchErr := localObs.Watch(ctx, e.syncRoot, events)
			if errors.Is(watchErr, synctypes.ErrWatchLimitExhausted) {
				e.logger.Warn("inotify watch limit exhausted, falling back to periodic full scan",
					slog.Duration("poll_interval", e.resolvePollInterval(opts)),
				)

				e.runPeriodicFullScan(ctx, localObs, e.syncRoot, events, e.resolvePollInterval(opts))
				errs <- nil // clean exit after context cancel

				return
			}

			errs <- watchErr
		}()
	}

	// Close events channel when all observers exit so the bridge goroutine
	// drains remaining events and exits cleanly.
	go func() {
		obsWg.Wait()
		close(events)
	}()

	return errs, count, skippedCh
}

// runPeriodicFullScan runs periodic full filesystem scans as a fallback when
// inotify watch limits are exhausted. Blocks until the context is canceled.
// Each scan's events are forwarded to the events channel via trySend.
func (e *Engine) runPeriodicFullScan(
	ctx context.Context, obs *syncobserve.LocalObserver, syncRoot string,
	events chan<- synctypes.ChangeEvent, interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	e.logger.Info("periodic full scan fallback started",
		slog.Duration("interval", interval),
	)

	for {
		select {
		case <-ticker.C:
			// Jitter: sleep 0-10% of interval to prevent thundering-herd
			// when multiple drives fire periodic scans simultaneously.
			if jitter := interval / periodicScanJitterDivisor; jitter > 0 {
				time.Sleep(rand.N(jitter)) //nolint:gosec // non-cryptographic jitter for I/O scheduling
			}

			result, err := obs.FullScan(ctx, syncRoot)
			if err != nil {
				if ctx.Err() != nil {
					return
				}

				e.logger.Warn("periodic full scan failed",
					slog.String("error", err.Error()),
				)

				continue
			}

			// Forward events only — skipped items are logged at DEBUG.
			// The primary scan and safety scan handle recording to sync_failures.
			for i := range result.Events {
				obs.TrySend(ctx, events, &result.Events[i])
			}

			if len(result.Skipped) > 0 {
				e.logger.Debug("periodic scan: skipped items",
					slog.Int("count", len(result.Skipped)))
			}
		case <-ctx.Done():
			return
		}
	}
}

// processBatch plans and dispatches a batch of path changes. On planner
// error (e.g. big-delete protection), the batch is skipped and the loop
// continues. In-flight actions for overlapping paths are canceled and
// replaced (B-122 deduplication).
func (e *Engine) processBatch(
	ctx context.Context, batch []synctypes.PathChanges, bl *synctypes.Baseline,
	mode synctypes.SyncMode, safety *synctypes.SafetyConfig,
) []*synctypes.TrackedAction {
	e.logger.Info("processing watch batch",
		slog.Int("paths", len(batch)),
	)

	e.periodicPermRecheck(ctx, bl)

	// R-2.10.10: use scanner output as proof-of-accessibility to clear
	// permission denials for paths observed in this batch.
	e.applyPermissionRecheckDecisions(ctx, e.permHandler.clearScannerResolvedPermissions(ctx, pathSetFromBatch(batch)))

	denied := e.permHandler.DeniedPrefixes(ctx)
	plan, err := e.planner.Plan(batch, bl, mode, safety, denied)
	if err != nil {
		if errors.Is(err, synctypes.ErrBigDeleteTriggered) {
			e.logger.Warn("big-delete protection triggered, skipping batch",
				slog.Int("paths", len(batch)),
			)

			return nil
		}

		e.logger.Error("planner error, skipping batch",
			slog.String("error", err.Error()),
		)

		return nil
	}

	if len(plan.Actions) == 0 {
		e.logger.Debug("empty plan for batch, nothing to do")
		return nil
	}

	// Rolling-window big-delete protection: count planned deletes and
	// filter them out if the counter trips. Non-delete actions continue
	// flowing. The planner-level check is disabled in watch mode
	// (threshold=MaxInt32) — this counter replaces it.
	if e.watch != nil && e.watch.deleteCounter != nil {
		plan = e.applyDeleteCounter(ctx, plan)
		if len(plan.Actions) == 0 {
			return nil
		}
	}

	e.deduplicateInFlight(plan)
	return e.dispatchBatchActions(ctx, plan)
}

// periodicPermRecheck runs permission rechecks at most once per 60 seconds.
// Throttled to avoid API hammering (R-2.10.9).
func (e *Engine) periodicPermRecheck(ctx context.Context, bl *synctypes.Baseline) {
	const permRecheckInterval = 60 * time.Second

	now := time.Now()
	if now.Sub(e.watch.lastPermRecheck) < permRecheckInterval {
		return
	}

	e.watch.lastPermRecheck = now

	// recheckPermissions calls the Graph API — skip during outage or
	// throttle to avoid wasting API calls (R-2.10.30). Local permission
	// rechecks (filesystem-only) proceed regardless.
	if e.permHandler.HasPermChecker() && !e.isObservationSuppressed() {
		shortcuts, err := e.baseline.ListShortcuts(ctx)
		if err == nil {
			e.applyPermissionRecheckDecisions(ctx, e.permHandler.recheckPermissions(ctx, bl, shortcuts))
		}
	}

	e.applyPermissionRecheckDecisions(ctx, e.permHandler.recheckLocalPermissions(ctx))
}

// deduplicateInFlight cancels in-flight actions for paths that appear in the
// plan. B-122: newer observation supersedes in-progress action.
func (e *Engine) deduplicateInFlight(plan *synctypes.ActionPlan) {
	for i := range plan.Actions {
		if e.depGraph.HasInFlight(plan.Actions[i].Path) {
			e.logger.Info("canceling in-flight action for updated path",
				slog.String("path", plan.Actions[i].Path),
			)

			e.depGraph.CancelByPath(plan.Actions[i].Path)
		}
	}
}

// dispatchBatchActions adds plan actions to the DepGraph with monotonic IDs,
// then admits ready actions through active-scope checks.
func (e *Engine) dispatchBatchActions(ctx context.Context, plan *synctypes.ActionPlan) []*synctypes.TrackedAction {
	// Invariant: Planner always builds Deps with len(Actions).
	if len(plan.Actions) != len(plan.Deps) {
		e.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		return nil
	}

	// Allocate monotonic action IDs for this batch. Using a global atomic
	// counter prevents ID collisions across batches — loop indices (int64(i))
	// would collide when multiple batches are processed.
	batchBaseID := e.watch.nextActionID
	e.watch.nextActionID += int64(len(plan.Actions))

	// Map from plan index → action ID for dependency resolution.
	actionIDs := make([]int64, len(plan.Actions))
	for i := range plan.Actions {
		actionIDs[i] = batchBaseID + int64(i)
	}

	// Add actions to DepGraph and collect immediately-ready ones. Dispatch
	// transitions (setDispatch) are deferred to admission, which runs AFTER
	// active-scope checks — setDispatch on a blocked action would be incorrect
	// (section 2.2: no dispatch before admission).
	var ready []*synctypes.TrackedAction

	for i := range plan.Actions {
		id := actionIDs[i]

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, actionIDs[depIdx])
		}

		if ta := e.depGraph.Add(&plan.Actions[i], id, depIDs); ta != nil {
			ready = append(ready, ta)
		}
	}

	// Admit ready actions through the active-scope working set. The watch loop
	// owns actual sends to readyCh via its outbox.
	if len(ready) > 0 {
		ready = e.admitReady(ctx, ready)
	}

	e.logger.Info("watch batch dispatched",
		slog.Int("actions", len(plan.Actions)),
	)

	return ready
}

// setDispatch writes the dispatch state transition for an action before it
// enters the tracker. Only applies to downloads and local deletes (the action
// types that have remote_state lifecycle).
func (e *Engine) setDispatch(ctx context.Context, action *synctypes.Action) {
	if err := e.baseline.SetDispatchStatus(ctx, action.DriveID.String(), action.ItemID, action.Type); err != nil {
		e.logger.Warn("failed to set dispatch status",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}

// resolvePollInterval returns the configured poll interval or the default.
func (e *Engine) resolvePollInterval(opts synctypes.WatchOpts) time.Duration {
	if opts.PollInterval > 0 {
		return opts.PollInterval
	}

	return defaultPollInterval
}

// resolveDebounce returns the configured debounce or the default.
func (e *Engine) resolveDebounce(opts synctypes.WatchOpts) time.Duration {
	if opts.Debounce > 0 {
		return opts.Debounce
	}

	return defaultDebounce
}

// isDeleteAction returns true if the action type is a local or remote delete.
func isDeleteAction(t synctypes.ActionType) bool {
	return t == synctypes.ActionLocalDelete || t == synctypes.ActionRemoteDelete
}

// applyDeleteCounter counts planned deletes in the plan, feeds them to the
// rolling counter, and — if the counter is held — filters delete actions out
// of the plan and records them as actionable issues. Returns the (possibly
// filtered) plan. When all actions are filtered, returns a plan with empty
// Actions/Deps.
func (e *Engine) applyDeleteCounter(ctx context.Context, plan *synctypes.ActionPlan) *synctypes.ActionPlan {
	// Count planned deletes.
	deleteCount := 0
	for i := range plan.Actions {
		if isDeleteAction(plan.Actions[i].Type) {
			deleteCount++
		}
	}

	if deleteCount == 0 {
		return plan
	}

	// Feed to the rolling counter. tripped=true means this call caused
	// the first transition from not-held → held.
	tripped := e.watch.deleteCounter.Add(deleteCount)
	if tripped {
		e.logger.Warn("big-delete protection triggered in watch mode",
			slog.Int("delete_count", e.watch.deleteCounter.Count()),
			slog.Int("threshold", e.watch.deleteCounter.Threshold()),
		)
	}

	if !e.watch.deleteCounter.IsHeld() {
		return plan
	}

	// Filter: separate deletes from non-deletes and rebuild the plan.
	// Dependency indices must be remapped to the new action positions.
	kept := make([]synctypes.Action, 0, len(plan.Actions))
	keptDeps := make([][]int, 0, len(plan.Deps))
	oldToNew := make(map[int]int, len(plan.Actions))

	var heldDeletes []synctypes.Action

	for i := range plan.Actions {
		if isDeleteAction(plan.Actions[i].Type) {
			heldDeletes = append(heldDeletes, plan.Actions[i])
			continue
		}

		oldToNew[i] = len(kept)
		kept = append(kept, plan.Actions[i])
		keptDeps = append(keptDeps, nil) // placeholder, remap below
	}

	// Remap dependency indices for kept actions. Drop deps pointing to
	// filtered-out (delete) actions — the non-delete action can proceed
	// independently since the delete won't run.
	for newIdx := range kept {
		// Find the original index by scanning oldToNew (small N, fast enough).
		var origIdx int
		for oi, ni := range oldToNew {
			if ni == newIdx {
				origIdx = oi
				break
			}
		}

		for _, depOld := range plan.Deps[origIdx] {
			if depNew, ok := oldToNew[depOld]; ok {
				keptDeps[newIdx] = append(keptDeps[newIdx], depNew)
			}
		}
	}

	plan.Actions = kept
	plan.Deps = keptDeps

	// Record held deletes as actionable issues for user visibility.
	e.recordHeldDeletes(ctx, heldDeletes)

	return plan
}

// recordHeldDeletes writes held delete actions to sync_failures as actionable
// issues with type big_delete_held. Uses UpsertActionableFailures for batch
// upsert — idempotent when the same deletes are re-observed.
func (e *Engine) recordHeldDeletes(ctx context.Context, actions []synctypes.Action) {
	if len(actions) == 0 {
		return
	}

	failures := make([]synctypes.ActionableFailure, len(actions))
	for i := range actions {
		failures[i] = synctypes.ActionableFailure{
			Path:      actions[i].Path,
			DriveID:   actions[i].DriveID,
			Direction: synctypes.DirectionDelete,
			IssueType: synctypes.IssueBigDeleteHeld,
			Error:     fmt.Sprintf("held by big-delete protection (threshold: %d)", e.bigDeleteThreshold),
		}
	}

	if err := e.baseline.UpsertActionableFailures(ctx, failures); err != nil {
		e.logger.Error("failed to record held deletes",
			slog.Int("count", len(failures)),
			slog.String("error", err.Error()),
		)
	}

	e.logger.Info("held delete actions recorded as issues",
		slog.Int("count", len(failures)),
	)
}

// externalDBChanged checks whether another process (e.g., the CLI) wrote to
// the database since the last check. Uses PRAGMA data_version — changes every
// time another connection commits a write. The engine's own writes don't
// change it. Returns true if the version advanced.
func (e *Engine) externalDBChanged(ctx context.Context) bool {
	dv, err := e.baseline.DataVersion(ctx)
	if err != nil {
		e.logger.Warn("failed to check data_version",
			slog.String("error", err.Error()),
		)

		return false
	}

	if dv == e.watch.lastDataVersion {
		return false
	}

	e.watch.lastDataVersion = dv

	return true
}

// handleRecheckTick processes a recheck timer tick: detects external DB
// changes (e.g., `issues clear`) and logs a watch summary.
func (e *Engine) handleRecheckTick(ctx context.Context) {
	if e.externalDBChanged(ctx) {
		e.handleExternalChanges(ctx)
	}

	e.logWatchSummary(ctx)
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version. Currently handles big-delete clearance: if the
// counter is held but all big_delete_held rows have been cleared (via
// `issues clear`), releases the counter so deletes resume on the next
// observation cycle.
func (e *Engine) handleExternalChanges(ctx context.Context) {
	// Big-delete clearance: check if user approved held deletes.
	if e.watch != nil && e.watch.deleteCounter != nil && e.watch.deleteCounter.IsHeld() {
		rows, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueBigDeleteHeld)
		if err != nil {
			e.logger.Warn("failed to check big-delete-held entries",
				slog.String("error", err.Error()),
			)

			return
		}

		if len(rows) == 0 {
			e.watch.deleteCounter.Release()
			e.logger.Info("big-delete protection cleared by user")
		}
	}

	// Permission clearance: if user cleared permission boundary failures via CLI,
	// release the corresponding in-memory scope blocks.
	e.clearResolvedPermissionScopes(ctx)
	e.mustAssertScopeInvariants(ctx, "handle external changes")
}

// clearResolvedPermissionScopes checks if any permission scope blocks have had
// their sync_failures cleared (by user action via CLI), and releases the
// corresponding scope blocks.
func (e *Engine) clearResolvedPermissionScopes(ctx context.Context) {
	if e.watch == nil {
		return
	}

	scopeKeys := e.scopeBlockKeys()
	if len(scopeKeys) == 0 {
		return
	}

	localIssues, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueLocalPermissionDenied)
	if err != nil {
		e.logger.Warn("failed to check local permission failures",
			slog.String("error", err.Error()),
		)

		return
	}

	remoteIssues, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssuePermissionDenied)
	if err != nil {
		e.logger.Warn("failed to check remote permission failures",
			slog.String("error", err.Error()),
		)

		return
	}

	// Build set of still-active scope keys from DB.
	activeScopes := make(map[synctypes.ScopeKey]bool, len(localIssues)+len(remoteIssues))
	for i := range localIssues {
		if localIssues[i].ScopeKey.IsPermDir() {
			activeScopes[localIssues[i].ScopeKey] = true
		}
	}
	for i := range remoteIssues {
		if remoteIssues[i].ScopeKey.IsPermRemote() {
			activeScopes[remoteIssues[i].ScopeKey] = true
		}
	}

	// Release any scope blocks whose failures were cleared.
	for _, key := range scopeKeys {
		if (key.IsPermDir() || key.IsPermRemote()) && !activeScopes[key] {
			if err := e.releaseScope(ctx, key); err != nil {
				e.logger.Warn("failed to release externally-cleared permission scope",
					slog.String("scope", key.String()),
					slog.String("error", err.Error()),
				)
				continue
			}

			e.logger.Info("permission scope block cleared by user",
				slog.String("scope", key.String()),
			)
		}
	}
}

// logWatchSummary logs a periodic one-liner summary of actionable issues
// in watch mode. Only logs when the count changes since the last summary
// to avoid noisy repeated output.
func (e *Engine) logWatchSummary(ctx context.Context) {
	issues, err := e.baseline.ListActionableFailures(ctx)
	if err != nil || len(issues) == 0 {
		if e.watch.lastSummaryTotal != 0 {
			e.watch.lastSummaryTotal = 0
		}

		return
	}

	// Only log if count changed since last summary.
	if len(issues) == e.watch.lastSummaryTotal {
		return
	}

	e.watch.lastSummaryTotal = len(issues)

	// Group by issue_type, emit one-liner.
	counts := make(map[string]int)
	for i := range issues {
		counts[issues[i].IssueType]++
	}

	parts := make([]string, 0, len(counts))
	for typ, n := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", n, typ))
	}

	sort.Strings(parts)

	e.logger.Warn("actionable issues",
		slog.Int("total", len(issues)),
		slog.String("breakdown", strings.Join(parts, ", ")),
	)
}

// recordSkippedItems records observation-time rejections (invalid names,
// path too long, file too large) as actionable failures in sync_failures.
// Groups items by issue type and uses UpsertActionableFailures for efficient
// batch upserts. Aggregated logging: >10 same-type items → 1 WARN with
// count + sample paths; ≤10 → per-file WARN.
func (e *Engine) recordSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
	if len(skipped) == 0 {
		return
	}

	// Group by issue type for batch upsert and aggregated logging.
	byReason := make(map[string][]synctypes.SkippedItem)
	for i := range skipped {
		byReason[skipped[i].Reason] = append(byReason[skipped[i].Reason], skipped[i])
	}

	for reason, items := range byReason {
		// Aggregated logging.
		const aggregateThreshold = 10
		if len(items) > aggregateThreshold {
			// Log summary with sample paths.
			const sampleCount = 3
			samples := make([]string, 0, sampleCount)
			for i := range items {
				if i >= sampleCount {
					break
				}
				samples = append(samples, items[i].Path)
			}

			e.logger.Warn("observation filter: skipped files",
				slog.String("issue_type", reason),
				slog.Int("count", len(items)),
				slog.Any("sample_paths", samples),
			)
		} else {
			for i := range items {
				e.logger.Warn("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		}

		// Build synctypes.ActionableFailure slice for batch upsert.
		failures := make([]synctypes.ActionableFailure, len(items))
		for i := range items {
			failures[i] = synctypes.ActionableFailure{
				Path:      items[i].Path,
				DriveID:   e.driveID,
				Direction: synctypes.DirectionUpload,
				IssueType: reason,
				Error:     items[i].Detail,
				FileSize:  items[i].FileSize,
			}
		}

		if err := e.baseline.UpsertActionableFailures(ctx, failures); err != nil {
			e.logger.Error("failed to record skipped items",
				slog.String("issue_type", reason),
				slog.Int("count", len(failures)),
				slog.String("error", err.Error()),
			)
		}
	}
}

// clearResolvedSkippedItems removes sync_failures entries for scanner-detectable
// issue types that are no longer present in the current scan. For example, if a
// user renames an invalid file to a valid name, the old failure is auto-cleared.
func (e *Engine) clearResolvedSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
	// Collect current paths per scanner-detectable issue type.
	currentByType := make(map[string][]string)
	for i := range skipped {
		currentByType[skipped[i].Reason] = append(currentByType[skipped[i].Reason], skipped[i].Path)
	}

	// For each scanner-detectable issue type, clear entries not in the current scan.
	// If no items of that type were found, pass empty slice (clears all of that type).
	scannerIssueTypes := []string{
		synctypes.IssueInvalidFilename, synctypes.IssuePathTooLong,
		synctypes.IssueFileTooLarge, synctypes.IssueCaseCollision,
	}
	for _, issueType := range scannerIssueTypes {
		paths := currentByType[issueType] // nil if no items — that's fine (clears all)
		if err := e.baseline.ClearResolvedActionableFailures(ctx, issueType, paths); err != nil {
			e.logger.Error("failed to clear resolved failures",
				slog.String("issue_type", issueType),
				slog.String("error", err.Error()),
			)
		}
	}
}

// resolveReconcileInterval returns the configured reconcile interval or the
// default. Negative values disable periodic reconciliation. Values below
// minReconcileInterval are clamped up.
func (e *Engine) resolveReconcileInterval(opts synctypes.WatchOpts) time.Duration {
	if opts.ReconcileInterval < 0 {
		return 0 // disabled
	}

	if opts.ReconcileInterval > 0 {
		if opts.ReconcileInterval < minReconcileInterval {
			e.logger.Warn("reconcile interval below minimum, clamping",
				slog.Duration("requested", opts.ReconcileInterval),
				slog.Duration("minimum", minReconcileInterval),
			)

			return minReconcileInterval
		}

		return opts.ReconcileInterval
	}

	return defaultReconcileInterval
}

// newReconcileTicker creates a ticker for periodic reconciliation. Returns
// nil if the interval is 0 (disabled).
func (e *Engine) newReconcileTicker(interval time.Duration) *time.Ticker {
	if interval <= 0 {
		return nil
	}

	return time.NewTicker(interval)
}

// initReconcileTicker creates the periodic full-reconciliation timer and
// returns its channel plus a stop function. If reconciliation is disabled,
// both the channel and stop function are nil.
func (e *Engine) initReconcileTicker(opts synctypes.WatchOpts) (<-chan time.Time, func()) {
	interval := e.resolveReconcileInterval(opts)
	ticker := e.newReconcileTicker(interval)

	if ticker == nil {
		return nil, nil
	}

	e.logger.Info("periodic full reconciliation enabled",
		slog.Duration("interval", interval),
	)

	return ticker.C, ticker.Stop
}

// runFullReconciliationAsync spawns a goroutine for full delta enumeration +
// orphan detection. Non-blocking — the watch loop continues processing events
// while reconciliation runs. Events are fed into the watch buffer so they flow
// through the normal pipeline (FlushDebounced → processBatch).
func (e *Engine) runFullReconciliationAsync(ctx context.Context, bl *synctypes.Baseline) {
	if !e.watch.reconcileRunning.CompareAndSwap(false, true) {
		e.logger.Info("full reconciliation skipped — previous still running")
		return
	}

	go func() {
		defer e.watch.reconcileRunning.Store(false)

		start := time.Now()
		e.logger.Info("periodic full reconciliation starting")

		events, deltaToken, err := e.observeRemoteFull(ctx, bl)
		if err != nil {
			// Suppress error logging during shutdown — context cancellation
			// is expected when the daemon is stopping.
			if ctx.Err() == nil {
				e.logger.Error("full reconciliation failed",
					slog.String("error", err.Error()),
				)
			}

			return
		}

		// Commit observations and delta token.
		observed := changeEventsToObservedItems(e.logger, events)
		if commitErr := e.baseline.CommitObservation(
			ctx, observed, deltaToken, e.driveID,
		); commitErr != nil {
			e.logger.Error("failed to commit full reconciliation observations",
				slog.String("error", commitErr.Error()),
			)

			return
		}

		if e.watch.afterReconcileCommit != nil {
			e.watch.afterReconcileCommit()
		}

		// Observations are durably committed. If we're shutting down, skip
		// feeding events to the buffer — the watch loop is also stopping and
		// won't process them. Next startup will re-observe idempotently.
		if ctx.Err() != nil {
			e.logger.Info("full reconciliation: observations committed, stopping for shutdown")
			return
		}

		// Filter out shortcut events and process shortcut scopes.
		events = filterOutShortcuts(events)

		shortcutEvents, scErr := e.reconcileShortcutScopes(ctx, bl)
		if scErr != nil {
			e.logger.Warn("shortcut reconciliation failed during full reconciliation",
				slog.String("error", scErr.Error()),
			)
		}

		events = append(events, shortcutEvents...)

		if len(events) == 0 {
			e.logger.Info("periodic full reconciliation complete: no changes",
				slog.Duration("duration", time.Since(start)),
			)
			return
		}

		// Feed events into the watch buffer — they flow through
		// FlushDebounced → processBatch in the watch loop. This avoids
		// calling processBatch directly from this goroutine, which would
		// race with the watch loop's own processBatch calls.
		for i := range events {
			e.watch.buf.Add(&events[i])
		}

		// Refresh watch shortcuts — reconcileShortcutScopes may have
		// added or removed shortcuts.
		if refreshed, refreshErr := e.baseline.ListShortcuts(ctx); refreshErr == nil {
			e.setShortcuts(refreshed)
		}

		e.logger.Info("periodic full reconciliation complete",
			slog.Int("events", len(events)),
			slog.Duration("duration", time.Since(start)),
		)
	}()
}
