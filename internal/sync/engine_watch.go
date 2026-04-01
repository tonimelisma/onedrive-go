package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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
	e.shortcuts = shortcuts
}

// getShortcuts returns the latest shortcuts for result processing and
// permission handling.
func (e *Engine) getShortcuts() []synctypes.Shortcut {
	return e.shortcuts
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
	bl               *synctypes.Baseline
	safety           *synctypes.SafetyConfig
	ready            <-chan []synctypes.PathChanges
	results          <-chan synctypes.WorkerResult
	errs             <-chan error
	skippedCh        <-chan []synctypes.SkippedItem
	reconcileC       <-chan time.Time
	reconcileResults <-chan reconcileResult
	recheckC         <-chan time.Time
	activeObs        int
	mode             synctypes.SyncMode
	pool             *syncexec.WorkerPool // for bootstrapSync to access Results()
	cleanup          func()
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
//   - Buffer is promoted to e.watch.buf for observed and reconciliation work;
//     retry/trial work now enters through explicit engine-owned planner requests.
func (e *Engine) initWatchInfra(
	ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts,
) (*watchPipeline, error) {
	// Create watchState — all watch-mode-only fields live here.
	e.watch = &watchState{
		watchTimerState: watchTimerState{
			retryTimerCh: make(chan struct{}, 1),
		},
		watchReconcileState: watchReconcileState{
			reconcileResults: make(chan reconcileResult, 1),
		},
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

	// Buffer promoted to watchState so observed and reconciliation work share
	// the same debounce/planning path the watch loop already owns.
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
		safety:           e.resolveSafetyConfig(opts.Force),
		ready:            ready,
		results:          pool.Results(),
		reconcileC:       reconcileC,
		reconcileResults: e.watch.reconcileResults,
		recheckC:         recheckTicker.C,
		mode:             mode,
		pool:             pool,
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
