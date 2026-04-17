package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// periodicScanJitterDivisor controls the jitter window for periodic full
// scans. With a divisor of 10, each tick sleeps 0-10% of the interval to
// prevent thundering-herd I/O spikes in multi-drive mode.
const periodicScanJitterDivisor = 10

// Default watch intervals.
const (
	defaultPollInterval = 5 * time.Minute
	defaultDebounce     = 5 * time.Second
	watchEventBuf       = 256
	// watchResultBuf is the buffer size for the worker result channel in watch
	// mode. Large enough for typical batches without blocking workers.
	watchResultBuf = 4096

	// recheckInterval is how often the engine checks for external DB
	// changes from other SQLite connections. Uses PRAGMA data_version
	// — one integer comparison per tick, essentially free.
	recheckInterval = 10 * time.Second
)

const (
	fullRemoteReconcileInterval = 24 * time.Hour
	localFullScanInterval       = 5 * time.Minute
)

// quiescenceLogInterval is how often bootstrapSync logs while waiting
// for in-flight actions to complete.
const quiescenceLogInterval = 30 * time.Second

// initRecheckState seeds the cross-connection change detector used by watch
// recheck ticks.
func (rt *watchRuntime) initRecheckState(ctx context.Context) {
	if dv, dvErr := rt.engine.baseline.DataVersion(ctx); dvErr == nil {
		rt.lastDataVersion = dv
	}
}

// loadWatchState loads the baseline for the watch session.
func (rt *watchRuntime) loadWatchState(ctx context.Context) error {
	_, err := rt.engine.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline for watch: %w", err)
	}

	return nil
}

// RunWatch runs a continuous sync loop: bootstrap sync through the watch
// pipeline, then watches for remote and local changes in batches.
// Blocks until the context is canceled, returning nil on clean shutdown.
//
// Flow: initWatchInfra → bootstrapSync → startObservers → runWatchLoop.
// Unlike the old approach (calling RunOnce with throwaway infrastructure),
// bootstrapSync dispatches through the same DepGraph, active scope working
// set, and WorkerPool that the steady-state watch loop uses.
func (e *Engine) RunWatch(ctx context.Context, mode Mode, opts WatchOptions) error {
	e.logger.Info("watch mode starting",
		slog.String("mode", mode.String()),
		slog.Duration("poll_interval", e.resolvePollInterval(opts)),
		slog.Duration("debounce", e.resolveDebounce(opts)),
	)

	rt := newWatchRuntime(e)
	if e.watchRuntimeHook != nil {
		e.watchRuntimeHook(rt)
	}
	hasAccountAuthRequirement, err := e.hasPersistedAccountAuthRequirement()
	if err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}

		return err
	}
	proof, proofErr := e.proveDriveIdentity(ctx)
	if proofErr != nil {
		if isWatchShutdownError(ctx, proofErr) {
			return nil
		}

		// Startup auth repair is the only case that should proceed past a
		// failing proof. Without a persisted catalog auth requirement there is
		// nothing to repair, so watch mode must abort before it allocates
		// workers or timers.
		if !hasAccountAuthRequirement {
			return proofErr
		}
	}

	// Step 1: Set up watch infrastructure (no observers yet).
	pipe, err := rt.initWatchInfra(ctx, mode, opts)
	if err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}

		return err
	}
	if err := e.repairPersistedAccountAuthRequirement(ctx, hasAccountAuthRequirement, proof, proofErr); err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}

		return err
	}
	if proofErr != nil {
		if isWatchShutdownError(ctx, proofErr) {
			return nil
		}

		return proofErr
	}
	e.logVerifiedDrive(proof)
	defer pipe.cleanup()

	// Step 2: Bootstrap — observe, plan, execute through watch pipeline.
	if err := rt.bootstrapSync(ctx, mode, pipe); err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}

		return fmt.Errorf("sync: initial sync failed: %w", err)
	}

	// Step 3: Start observers AFTER bootstrap — they see the post-bootstrap baseline.
	errs, activeObs, skippedCh := rt.startObservers(ctx, pipe.bl, rt.buf, opts)
	pipe.errs = errs
	pipe.activeObs = activeObs
	pipe.skippedCh = skippedCh

	// Step 4: Run the watch loop.
	return rt.runWatchLoop(ctx, pipe)
}

func isWatchShutdownError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() == nil {
		return false
	}

	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// watchPipeline holds all handles needed by the watch select loop.
// Created by initWatchInfra; cleaned up by its cleanup method.
type watchPipeline struct {
	runtime          *watchRuntime
	bl               *Baseline
	safety           *SafetyConfig
	batchReady       <-chan []PathChanges
	results          <-chan WorkerResult
	errs             <-chan error
	skippedCh        <-chan []SkippedItem
	reconcileC       <-chan time.Time
	reconcileResults <-chan reconcileResult
	recheckC         <-chan time.Time
	activeObs        int
	mode             Mode
	pool             *WorkerPool // for bootstrapSync to access Results()
	cleanup          func()
}

type socketIOWakeSourceRunner interface {
	Run(ctx context.Context, wakes chan<- struct{}) error
}

// initWatchInfra sets up watch-mode infrastructure: watchRuntime, DepGraph,
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
func (rt *watchRuntime) initWatchInfra(
	ctx context.Context,
	mode Mode,
	opts WatchOptions,
) (*watchPipeline, error) {
	// Enable watch-mode-specific executor behavior (pre-upload eTag
	// freshness checks to prevent silently overwriting concurrent remote
	// changes — see executor_transfer.go).
	rt.engine.execCfg.SetWatchMode(true)

	rt.initRecheckState(ctx)

	// Normalize persisted scope rows before loading runtime scope state.
	// Startup must not trust stale scope rows blindly; the durable store is
	// repaired against current persisted evidence before the watch loop loads
	// its ephemeral activeScopes working set.
	if err := rt.scopeController().repairPersistedScopes(ctx, rt); err != nil {
		return nil, fmt.Errorf("sync: repairing persisted scopes: %w", err)
	}

	// DepGraph tracks action dependencies. Active scope state is loaded from
	// the persisted scope_blocks table into watch-owned runtime state.
	depGraph := NewDepGraph(rt.engine.logger)
	rt.depGraph = depGraph
	if err := rt.scopeController().loadActiveScopes(ctx, rt); err != nil {
		return nil, fmt.Errorf("sync: loading active scopes: %w", err)
	}

	rt.scopeState = NewScopeState(rt.engine.nowFunc, rt.engine.logger)
	rt.nextActionID = 0

	// dispatchCh feeds admitted actions to workers. Buffer is generous to avoid
	// backpressure when a batch produces many immediately-ready actions.
	rt.dispatchCh = make(chan *TrackedAction, watchResultBuf)

	// Never-closing done channel — DepGraph.Done() would fire prematurely
	// between batches when completed == total. Workers exit only via ctx.Done().
	neverDone := make(chan struct{})

	pool := NewWorkerPool(rt.engine.execCfg, rt.dispatchCh, neverDone, rt.engine.baseline, rt.engine.logger, watchResultBuf)
	pool.Start(ctx, rt.engine.transferWorkers)

	// Buffer promoted to watchRuntime so observed and reconciliation work share
	// the same debounce/planning path the watch loop already owns.
	buf := NewBuffer(rt.engine.logger)
	rt.buf = buf
	batchReady := buf.FlushDebounced(ctx, rt.engine.resolveDebounce(opts))

	// Tickers/timers.
	rt.resetReconcileTimer(nil)
	recheckTicker := rt.engine.newTicker(recheckInterval)

	// Arm retrier timer from DB — picks up items from prior crash or prior pass.
	rt.kickRetrySweepNow()
	rt.armTrialTimer()

	pipe := &watchPipeline{
		runtime:          rt,
		safety:           rt.engine.resolveSafetyConfig(),
		batchReady:       batchReady,
		results:          pool.Results(),
		reconcileC:       rt.reconcileCh,
		reconcileResults: rt.reconcileResults,
		recheckC:         tickerChan(recheckTicker),
		mode:             mode,
		pool:             pool,
	}

	pipe.cleanup = func() {
		if rt.socketIOWakeStop != nil {
			close(rt.socketIOWakeStop)
			if rt.socketIOWakeDone != nil {
				<-rt.socketIOWakeDone
			}
			rt.socketIOWakeStop = nil
			rt.socketIOWakeDone = nil
		}

		stopTicker(recheckTicker)
		rt.resetReconcileTimer(nil)

		inFlight := depGraph.InFlightCount()
		if inFlight > 0 {
			rt.engine.logger.Info("graceful shutdown: draining in-flight actions",
				slog.Int("in_flight", inFlight),
			)
		}

		rt.stopRetryTimer()
		rt.stopTrialTimer()
		pool.Stop() // closes results channel after workers exit
		rt.remoteObs = nil
		rt.localObs = nil
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventWatchStopped})
		rt.engine.logger.Info("watch mode stopped")
	}

	return pipe, nil
}

// bootstrapSync performs the initial sync using the watch pipeline. Unlike
// the old approach (calling RunOnce with throwaway infrastructure), this
// dispatches through the same DepGraph, active scope working set, and WorkerPool that
// the watch loop uses. Blocks until all bootstrap actions complete.
//
// Must be called after initWatchInfra and before startObservers.
func (rt *watchRuntime) bootstrapSync(ctx context.Context, mode Mode, pipe *watchPipeline) error {
	rt.engine.logger.Info("bootstrap sync starting", slog.String("mode", mode.String()))

	// Load baseline for watch startup.
	if err := rt.loadWatchState(ctx); err != nil {
		return err
	}
	bl, err := rt.engine.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: reloading baseline after watch startup: %w", err)
	}
	pipe.bl = bl

	// Permission rechecks.
	if rt.engine.permHandler.HasPermChecker() {
		rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, rt.engine.permHandler.recheckPermissions(ctx, bl))
	}
	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, rt.engine.permHandler.recheckLocalPermissions(ctx))

	fullReconcile, err := rt.engine.shouldRunFullRemoteReconcile(ctx, false)
	if err != nil {
		return fmt.Errorf("sync: deciding bootstrap full reconcile: %w", err)
	}

	// Observe changes.
	changes, pendingCursorCommit, err := rt.observeChanges(ctx, rt, bl, false, fullReconcile)
	if err != nil {
		return fmt.Errorf("sync: bootstrap observation failed: %w", err)
	}
	if len(changes) == 0 {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventBootstrapQuiesced})
		if err := rt.armFullReconcileTimer(ctx); err != nil {
			return fmt.Errorf("sync: arming full reconcile timer: %w", err)
		}
		rt.engine.logger.Info("bootstrap sync complete: no changes detected")
		return nil
	}

	// Commit the deferred delta token before dispatching bootstrap actions.
	if err := rt.commitPendingPrimaryCursor(ctx, pendingCursorCommit); err != nil {
		return err
	}

	// Dispatch through watch pipeline (same path as steady-state batches).
	initialOutbox := rt.processBatch(ctx, changes, bl, mode, pipe.safety)

	// Wait for all bootstrap actions to complete.
	if err := rt.runWatchUntilQuiescent(ctx, pipe, initialOutbox); err != nil {
		return fmt.Errorf("sync: bootstrap quiescence failed: %w", err)
	}

	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventBootstrapQuiesced})
	rt.postSyncHousekeeping()
	if err := rt.armFullReconcileTimer(ctx); err != nil {
		return fmt.Errorf("sync: arming full reconcile timer: %w", err)
	}
	rt.engine.logger.Info("bootstrap sync complete")
	return nil
}

// startObservers launches remote and local observer goroutines that feed
// events into the buffer. Returns an error channel for observer failures and
// the number of observers started. The events channel is closed automatically
// when all observers exit, allowing the bridge goroutine to drain cleanly.
func (rt *watchRuntime) startObservers(
	ctx context.Context, bl *Baseline, buf *Buffer, opts WatchOptions,
) (<-chan error, int, <-chan []SkippedItem) {
	events := make(chan ChangeEvent, watchEventBuf)
	errs := make(chan error, 2)

	var obsWg stdsync.WaitGroup

	rt.startWatchEventBridge(ctx, buf, events)

	count := 0

	obsWg.Add(1)
	count++
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverStarted, Observer: engineDebugObserverRemote})
	rt.startRemoteObserver(ctx, &obsWg, bl, events, errs, opts)

	// Channel for forwarding SkippedItems from safety scans to the engine.
	// Buffered(2) — at most 2 safety scans could overlap before draining.
	skippedCh := make(chan []SkippedItem, 2)

	localObs := NewLocalObserver(bl, rt.engine.logger, rt.engine.checkWorkers)
	localObs.SetFilterConfig(rt.engine.localFilter)
	localObs.SetObservationRules(rt.engine.localRules)
	localObs.SetSkippedChannel(skippedCh)
	localObs.safetyScanInterval = localRefreshIntervalForMode(localRefreshModeWatchHealthy)
	localObs.AfterSafetyScan = func() {
		refreshCtx := context.WithoutCancel(ctx)
		if err := rt.engine.baseline.MarkFullLocalRefresh(
			refreshCtx,
			rt.engine.driveID,
			rt.engine.nowFunc(),
			localRefreshModeWatchHealthy,
		); err != nil {
			rt.engine.logger.Warn("failed to mark healthy local refresh after safety scan",
				slog.String("error", err.Error()),
			)
		}
	}

	if rt.engine.localWatcherFactory != nil {
		localObs.SetWatcherFactory(rt.engine.localWatcherFactory)
	}

	rt.localObs = localObs

	obsWg.Add(1)
	count++
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverStarted, Observer: engineDebugObserverLocal})

	go func() {
		defer obsWg.Done()
		defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverLocal})

		watchErr := localObs.Watch(ctx, rt.engine.syncTree, events)
		if errors.Is(watchErr, ErrWatchLimitExhausted) {
			if err := rt.engine.baseline.MarkFullLocalRefresh(
				context.WithoutCancel(ctx),
				rt.engine.driveID,
				rt.engine.nowFunc(),
				localRefreshModeWatchDegraded,
			); err != nil {
				rt.engine.logger.Warn("failed to mark degraded local refresh before fallback",
					slog.String("error", err.Error()),
				)
			}
			rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverFallbackStarted, Observer: engineDebugObserverLocal})
			rt.engine.logger.Warn("inotify watch limit exhausted, falling back to periodic full scan",
				slog.Duration("scan_interval", localRefreshIntervalForMode(localRefreshModeWatchDegraded)),
			)

			rt.runPeriodicFullScan(ctx, localObs, rt.engine.syncTree, events, localRefreshIntervalForMode(localRefreshModeWatchDegraded))
			rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverFallbackStopped, Observer: engineDebugObserverLocal})
			errs <- nil // clean exit after context cancel

			return
		}

		errs <- watchErr
	}()

	rt.closeWatchEventsWhenObserversExit(&obsWg, events)
	if err := rt.engine.baseline.MarkFullLocalRefresh(
		context.WithoutCancel(ctx),
		rt.engine.driveID,
		rt.engine.nowFunc(),
		localRefreshModeWatchHealthy,
	); err != nil {
		rt.engine.logger.Warn("failed to mark healthy local refresh at watcher startup",
			slog.String("error", err.Error()),
		)
	}

	return errs, count, skippedCh
}

func (rt *watchRuntime) startRemoteObserver(
	ctx context.Context,
	obsWg *stdsync.WaitGroup,
	bl *Baseline,
	events chan<- ChangeEvent,
	errs chan<- error,
	opts WatchOptions,
) {
	pollInterval := rt.engine.resolvePollInterval(opts)
	plan := rt.buildPrimaryRootObservationPlan(false)
	rt.startPrimaryRootWatch(ctx, obsWg, bl, events, errs, pollInterval, plan)
}

func (rt *watchRuntime) startSocketIOWakeSource(ctx context.Context) <-chan struct{} {
	if !rt.engine.enableWebsocket || rt.engine.socketIOFetcher == nil {
		return nil
	}

	rt.engine.logger.Info("starting socket.io wake source",
		slog.String("drive_id", rt.engine.driveID.String()),
	)

	wakeCh := make(chan struct{}, 1)

	stopCh := make(chan struct{})
	rt.socketIOWakeStop = stopCh
	rt.socketIOWakeDone = make(chan struct{})
	wakeSource := rt.engine.socketIOWakeSourceFactory(
		rt.engine.socketIOFetcher,
		rt.engine.driveID,
		SocketIOWakeSourceOptions{
			Logger:        rt.engine.logger,
			LifecycleHook: rt.emitSocketIOLifecycleEvent,
		},
	)

	go func() {
		wakeCtx, wakeCancel := context.WithCancel(ctx)
		defer wakeCancel()
		defer close(rt.socketIOWakeDone)

		go func() {
			select {
			case <-stopCh:
				wakeCancel()
			case <-wakeCtx.Done():
			}
		}()

		if err := wakeSource.Run(wakeCtx, wakeCh); err != nil {
			rt.engine.logger.Warn("socket.io wake source exited",
				slog.String("drive_id", rt.engine.driveID.String()),
				slog.String("error", err.Error()),
			)
		}
	}()

	return wakeCh
}

func (rt *watchRuntime) emitSocketIOLifecycleEvent(event SocketIOLifecycleEvent) {
	debugEvent := engineDebugEvent{
		DriveID: event.DriveID,
		Note:    event.Note,
		Delay:   event.Delay,
		Error:   event.Error,
	}

	switch event.Type {
	case SocketIOLifecycleEventStarted:
		debugEvent.Type = engineDebugEventWebsocketWakeSourceStarted
	case SocketIOLifecycleEventEndpointFetchFail:
		debugEvent.Type = engineDebugEventWebsocketEndpointFetchFail
	case SocketIOLifecycleEventConnectFail:
		debugEvent.Type = engineDebugEventWebsocketConnectFail
	case SocketIOLifecycleEventConnected:
		debugEvent.Type = engineDebugEventWebsocketConnected
		if event.SID != "" {
			debugEvent.Note = "sid=" + event.SID
		}
	case SocketIOLifecycleEventRefreshRequested:
		debugEvent.Type = engineDebugEventWebsocketRefreshRequested
	case SocketIOLifecycleEventConnectionDropped:
		debugEvent.Type = engineDebugEventWebsocketConnectionDropped
	case SocketIOLifecycleEventNotificationWake:
		debugEvent.Type = engineDebugEventWebsocketNotificationWake
	case SocketIOLifecycleEventWakeCoalesced:
		debugEvent.Type = engineDebugEventWebsocketWakeCoalesced
	case SocketIOLifecycleEventStopped:
		debugEvent.Type = engineDebugEventWebsocketWakeSourceStopped
	default:
		return
	}

	rt.engine.emitDebugEvent(debugEvent)
}

func (rt *watchRuntime) startWatchEventBridge(
	ctx context.Context,
	buf *Buffer,
	events <-chan ChangeEvent,
) {
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
}

func (rt *watchRuntime) closeWatchEventsWhenObserversExit(
	obsWg *stdsync.WaitGroup,
	events chan ChangeEvent,
) {
	go func() {
		obsWg.Wait()
		close(events)
	}()
}

// runPeriodicFullScan runs periodic full filesystem scans as a fallback when
// inotify watch limits are exhausted. Blocks until the context is canceled.
// Each scan's events are forwarded to the events channel via trySend.
func (rt *watchRuntime) runPeriodicFullScan(
	ctx context.Context, obs *LocalObserver, tree *synctree.Root,
	events chan<- ChangeEvent, interval time.Duration,
) {
	ticker := rt.engine.newTicker(interval)
	defer stopTicker(ticker)

	rt.engine.logger.Info("periodic full scan fallback started",
		slog.Duration("interval", interval),
	)

	for {
		select {
		case <-tickerChan(ticker):
			// Jitter: sleep 0-10% of interval to prevent thundering-herd
			// when multiple drives fire periodic scans simultaneously.
			if jitter := interval / periodicScanJitterDivisor; jitter > 0 {
				if err := rt.engine.sleepFn(ctx, rt.engine.jitterFn(jitter)); err != nil {
					return
				}
			}

			result, err := obs.FullScan(ctx, tree)
			if err != nil {
				if ctx.Err() != nil {
					return
				}

				rt.engine.logger.Warn("periodic full scan failed",
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
				rt.engine.logger.Debug("periodic scan: skipped items",
					slog.Int("count", len(result.Skipped)))
			}
			if err := rt.engine.baseline.MarkFullLocalRefresh(
				context.WithoutCancel(ctx),
				rt.engine.driveID,
				rt.engine.nowFunc(),
				localRefreshModeWatchDegraded,
			); err != nil {
				rt.engine.logger.Warn("failed to mark degraded local refresh after periodic scan",
					slog.String("error", err.Error()),
				)
			}
		case <-ctx.Done():
			return
		}
	}
}

// resolvePollInterval returns the configured poll interval or the default.
func (e *Engine) resolvePollInterval(opts WatchOptions) time.Duration {
	if opts.PollInterval > 0 {
		return opts.PollInterval
	}

	return defaultPollInterval
}

// resolveDebounce returns the configured debounce or the default.
func (e *Engine) resolveDebounce(opts WatchOptions) time.Duration {
	if opts.Debounce > 0 {
		return opts.Debounce
	}

	return defaultDebounce
}
