package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
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
	watchObservationBuf = 256
	watchDispatchBuf    = 0
	// watchCompletionBuf is the buffer size for the action completion channel in
	// watch mode. It stays independent from dispatch buffering so workers can
	// report results without letting old work escape engine-owned outbox control.
	watchCompletionBuf = 4096

	// maintenanceInterval is how often the watch loop runs summary logging and
	// maintenance bookkeeping that is independent of action dispatch.
	maintenanceInterval = 10 * time.Second
)

// quiescenceLogInterval is how often bootstrapSync logs while waiting
// for in-flight actions to complete.
const quiescenceLogInterval = 30 * time.Second

// RunWatch runs a continuous sync loop: bootstrap sync through the watch
// pipeline, then watches for remote and local changes that feed the shared
// steady-state replan path.
// Blocks until the context is canceled, returning nil on clean shutdown.
//
// Flow: runStartupStage → initWatchInfra → bootstrapSync →
// startObservers → runWatchLoop.
// bootstrapSync dispatches through the same dependency graph, active scope
// working set and worker pool that the steady-state watch loop uses.
// A timer callback that panics ends the watch with an error wrapping
// panicguard.ErrPanic (beginRun).
func (e *Engine) RunWatch(ctx context.Context, mode syncdomain.SyncMode, opts WatchOptions) (err error) {
	runCtx, end := e.beginRun(ctx)
	defer func() { err = end(err) }()

	return e.runWatch(runCtx, newWatchRuntime(e), mode, opts)
}

// runWatch is the watch RunWatch runs on rt under its bound context. The end
// of that context is recorded for the whole watch (noteRunCancel), so the
// stop's bound starts at the cancel.
func (e *Engine) runWatch(ctx context.Context, rt *watchRuntime, mode syncdomain.SyncMode, opts WatchOptions) error {
	e.logger.Info("watch mode starting",
		slog.String("mode", mode.String()),
		slog.Duration("poll_interval", e.resolvePollInterval(opts)),
		slog.Duration("debounce", e.resolveDebounce(opts)),
	)

	stopNoting := rt.noteRunCancel(ctx)
	defer stopNoting()
	bl, err := rt.runStartupStage(ctx)
	if err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}

		return err
	}

	// Step 1: Set up watch infrastructure (no observers yet).
	pipe, err := rt.initWatchInfra(ctx, mode, opts)
	if err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}

		return err
	}
	pipe.bl = bl
	defer pipe.cleanup()

	// Step 2: Bootstrap — observe, plan, execute through watch pipeline.
	if err := rt.bootstrapSync(ctx, mode, pipe); err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}

		return fmt.Errorf("sync: initial sync: %w", err)
	}

	// Step 3: Start observers AFTER bootstrap — they see the post-bootstrap baseline.
	if err := rt.engine.refreshProtectedRootsFromStore(ctx); err != nil {
		return fmt.Errorf("sync: refresh shortcut protected roots before watch observers: %w", err)
	}
	if err := rt.startObservers(ctx, pipe.bl, opts); err != nil {
		if isWatchObserverStartupStopped(ctx, err) {
			return nil
		}

		return err
	}

	// Step 4: Run the watch loop.
	if err := rt.runWatchLoop(ctx, pipe); err != nil {
		if isWatchShutdownError(ctx, err) {
			return nil
		}
		return err
	}
	return nil
}

// errWatchObserversDuringDrain reports a refusal to start watch observers
// after a drain has begun. Callers branch on it to tell an ordinary startup
// failure apart from a shutdown that overtook observer startup.
var errWatchObserversDuringDrain = errors.New("sync: start watch observers during drain")

// isWatchObserverStartupStopped reports whether a failure to start observers
// describes the stop the caller asked for. Cancellation can land between
// bootstrap draining and observer startup; the runtime then refuses to start
// observers, and with the context already done that refusal is the shutdown
// completing rather than a watch-mode failure.
func isWatchObserverStartupStopped(ctx context.Context, err error) bool {
	if isWatchShutdownError(ctx, err) {
		return true
	}

	return ctx.Err() != nil && errors.Is(err, errWatchObserversDuringDrain)
}

// isWatchShutdownError reports whether a watch step failed with the stop the
// caller asked for: the run's context has ended and err is the
// cancellation's own echo. A separate failure that landed during the stop is
// still the watch's error: the report of a loop that ended without draining
// says so even when the context is done (runWatchLoop). A request that timed
// out while the run goes on is never a stop, and neither is a store that did
// not answer within its bound (ErrStoreAccessTimedOut): that deadline is the
// engine's limit on its database, as for classifyResult, so a drain whose
// bound cut records short ends the watch with that store failure.
func isWatchShutdownError(ctx context.Context, err error) bool {
	return syncdomain.RunEnded(ctx, err) && syncdomain.IsCancellationEcho(err) && !errors.Is(err, syncdomain.ErrStoreAccessTimedOut)
}

// watchPipeline holds watch-loop dependencies that are not already runtime-owned.
// Created by initWatchInfra; cleaned up by its cleanup method.
type watchPipeline struct {
	runtime      *watchRuntime
	bl           *syncdomain.Baseline
	replanReady  <-chan syncobserve.DirtyBatch
	completions  <-chan syncdomain.ActionCompletion
	maintenanceC <-chan time.Time
	mode         syncdomain.SyncMode
	pool         *syncexec.WorkerPool // for bootstrapSync to access Completions()
	cleanup      func()
}

type socketIOWakeSourceRunner interface {
	Run(ctx context.Context, wakes chan<- struct{}) error
}

// initWatchInfra sets up watch-mode infrastructure: watchRuntime, the dependency
// graph, worker pool, dirty scheduler, persisted scope state, and tickers. Does NOT load
// baseline or start observers — those happen in bootstrapSync and RunWatch.
//
// Key differences from one-shot mode:
//   - Active scopes are loaded from DB into engine-owned runtime state
//   - Workers exit only via ctx.Done(); the watch runtime owns settle/draintime
//     instead of relying on dependency-graph completion
//   - Retrier and trials are handled by the watch control flow itself
//   - DirtyBuffer, not buffered planner input, decides when snapshots are refreshed
//     and the current actionable set is rebuilt.
func (rt *watchRuntime) initWatchInfra(
	ctx context.Context,
	mode syncdomain.SyncMode,
	opts WatchOptions,
) (*watchPipeline, error) {
	// The graph starts empty, and startRuntimeStage replaces it with each
	// plan's. Active scope state is loaded from the persisted block_scopes
	// table into watch-owned runtime state.
	rt.sched.graph = syncexec.NewDepGraph(rt.deps.logger)
	rt.sched.graph.SetOwnerCheck(rt.engine.runOwnerCheck)
	if err := rt.loadActiveScopes(ctx); err != nil {
		return nil, fmt.Errorf("sync: loading active scopes: %w", err)
	}

	rt.scopes.state = syncexec.NewScopeState(rt.engine.nowFunc, rt.deps.logger)
	rt.sched.nextActionID = 0

	// dispatchCh feeds admitted actions to workers. Watch mode keeps this
	// unbuffered so not-yet-started work remains engine-owned until a worker is
	// actually ready to receive it.
	rt.sched.dispatchCh = make(chan *syncdomain.TrackedAction, watchDispatchBuf)

	pool := syncexec.NewWorkerPool(rt.engine.execCfg, rt.sched.dispatchCh, executorStoreOf(rt.deps.store), rt.deps.logger, watchCompletionBuf)
	pool.SetRuntimeOptions(rt.engine.collector(), rt.engine.hooks.serializeWorkers)
	pool.Start(ctx, rt.engine.transferWorkers)

	// DirtyBuffer is the watch scheduler boundary. Observation marks coarse
	// dirty/full-refresh signals only; snapshot refresh and planning happen
	// after debounce.
	dirtyBuf := syncobserve.NewDirtyBuffer(rt.deps.logger)
	rt.resources.dirtyBuf = dirtyBuf
	replanReady := dirtyBuf.FlushDebounced(ctx, rt.engine.resolveDebounce(opts))

	// Tickers/timers.
	rt.resetRefreshTimer(nil)
	maintenanceTicker := rt.deps.clock.NewTicker(maintenanceInterval)

	// Arm retrier timer from DB — picks up items from prior crash or prior pass.
	rt.kickRetryHeldReleaseNow()
	rt.armTrialTimer()

	pipe := &watchPipeline{
		runtime:      rt,
		replanReady:  replanReady,
		completions:  pool.Completions(),
		maintenanceC: syncdomain.TickerChan(maintenanceTicker),
		mode:         mode,
		pool:         pool,
	}

	pipe.cleanup = func() {
		rt.resources.goroutines.stop(rt.deps.clock, rt.deps.logger)

		syncdomain.StopTicker(maintenanceTicker)
		rt.resetRefreshTimer(nil)

		rt.stopRetryTimer()
		rt.stopTrialTimer()
		rt.stopAdmitTimer()
		pool.Stop() // closes completions channel after workers exit
		rt.resources.observerErrs = nil
		rt.resources.localBatches = nil
		rt.resources.remoteBatches = nil
		rt.resources.activeObservers = 0
		rt.resources.remoteObs = nil
		rt.resources.localObs = nil
		rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventWatchStopped})
		rt.deps.logger.Info("watch mode stopped")
	}

	return pipe, nil
}

// bootstrapSync performs the initial sync using the watch pipeline. It
// observes current truth, builds the current plan, reconciles runtime state,
// and starts the shared runtime through the
// shared current-plan stage sequence, then dispatches through the same
// dependency graph, active scope working set and worker pool that the
// steady-state watch loop uses. Blocks until all bootstrap actions due now complete.
//
// Must be called after startup + initWatchInfra and before startObservers.
func (rt *watchRuntime) bootstrapSync(ctx context.Context, mode syncdomain.SyncMode, pipe *watchPipeline) error {
	rt.deps.logger.Info("bootstrap sync starting", slog.String("mode", mode.String()))

	if pipe == nil || pipe.bl == nil {
		return fmt.Errorf("sync: bootstrap requires startup baseline")
	}

	runtime, err := rt.runBootstrapCurrentPlan(ctx, pipe.bl, mode)
	if err != nil {
		return err
	}
	if publishErr := rt.engine.publishShortcutChildWorkSnapshot(ctx, runtime.ChildPublication); publishErr != nil {
		return publishErr
	}
	if len(runtime.Plan.Actions) == 0 {
		return rt.finishBootstrapWithoutActions(ctx, runtime.PendingRemoteObservation)
	}

	// Commit the deferred delta token before dispatching bootstrap actions.
	_, cursorCommitErr := rt.commitPendingRemoteObservation(ctx, runtime.PendingRemoteObservation)
	if cursorCommitErr != nil {
		return cursorCommitErr
	}
	// Dispatch through the watch runtime (same frontier path steady-state uses).
	rt.noteInstalledPlan(runtime.Plan)
	initialOutbox, _, err := rt.startRuntimeStage(ctx, runtime, pipe.bl)
	if err != nil {
		return fmt.Errorf("starting transfers: %w", err)
	}

	// Wait for all bootstrap actions to complete.
	if err := rt.runWatchUntilQuiescent(ctx, pipe, initialOutbox); err != nil {
		return fmt.Errorf("finishing transfers: %w", err)
	}

	return rt.finishBootstrapAfterActions(ctx)
}

func (rt *watchRuntime) finishBootstrapWithoutActions(
	ctx context.Context,
	pendingRemoteObservation *syncobserve.RemoteObservationBatch,
) error {
	if _, err := rt.commitPendingRemoteObservation(ctx, pendingRemoteObservation); err != nil {
		return err
	}
	rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventBootstrapQuiesced})
	if err := rt.armFullRefreshTimer(ctx); err != nil {
		return fmt.Errorf("sync: arming full remote refresh timer: %w", err)
	}
	rt.deps.logger.Info("bootstrap sync complete: no changes detected")
	return nil
}

func (rt *watchRuntime) finishBootstrapAfterActions(
	ctx context.Context,
) error {
	rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventBootstrapQuiesced})
	rt.scheduleFollowUpIfSettled()
	rt.postSyncHousekeeping(ctx)
	if err := rt.armFullRefreshTimer(ctx); err != nil {
		return fmt.Errorf("sync: arming full remote refresh timer: %w", err)
	}
	rt.deps.logger.Info("bootstrap sync complete")
	return nil
}

// startObservers launches remote and local observer goroutines and stores
// their channels on the watch runtime. The watch loop owns local and remote
// batch application and dirty marking; observers do not commit SQLite directly.
func (rt *watchRuntime) startObservers(
	ctx context.Context, bl *syncdomain.Baseline, opts WatchOptions,
) error {
	// Bootstrap uses loop termination to mean "quiesced"; the observer-backed
	// loop must always start from steady-state running ownership.
	if err := rt.beginObserverBackedRunning(); err != nil {
		return err
	}

	localBatches := make(chan syncobserve.LocalObservation, watchObservationBuf)
	protectedRootEvents := make(chan syncshortcut.ProtectedRootEvent, watchObservationBuf)
	remoteBatches := make(chan syncobserve.RemoteObservationBatch, watchObservationBuf)
	errs := make(chan error, 2)
	// Every step that can fail runs before the first goroutine starts, so a
	// start that fails leaves nothing running.
	localObs, err := rt.buildWatchLocalObserver(ctx, bl, localBatches, protectedRootEvents)
	if err != nil {
		return err
	}

	rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventObserverStarted, Observer: syncdomain.EngineDebugObserverRemote})
	rt.startRemoteObserver(ctx, bl, remoteBatches, errs, opts)

	rt.resources.localObs = localObs
	rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventObserverStarted, Observer: syncdomain.EngineDebugObserverLocal})
	rt.startLocalObserver(ctx, localObs, localBatches, protectedRootEvents, errs)

	rt.resources.observerErrs = errs
	rt.resources.activeObservers = watchObserverCount
	rt.resources.localBatches = localBatches
	rt.resources.protectedRootEvents = protectedRootEvents
	rt.resources.remoteBatches = remoteBatches
	return nil
}

// watchObserverCount is how many observers startObservers starts, the remote
// and the local one, each sending one exit error.
const watchObserverCount = 2

// buildWatchLocalObserver builds the watch's local observer, reading what it
// needs from the store: the hash cache and the mount boundaries. It starts
// nothing.
func (rt *watchRuntime) buildWatchLocalObserver(
	ctx context.Context,
	bl *syncdomain.Baseline,
	localBatches chan<- syncobserve.LocalObservation,
	protectedRootEvents chan<- syncshortcut.ProtectedRootEvent,
) (*syncobserve.LocalObserver, error) {
	localObs := rt.engine.buildLocalObserver(bl, rt.deps.logger)
	hashCache, err := rt.engine.baseline.NewLocalHashCache(ctx)
	if err != nil {
		return nil, err
	}
	localObs.SetLocalHashCache(hashCache)
	localObs.SetFilterConfig(rt.engine.contentFilter)
	localObs.SetProtectedRoots(rt.engine.currentProtectedRoots())
	localObs.SetObservationRules(rt.engine.localRules)
	localObs.SetSyncRootIdentity(rt.engine.syncRootIdentity)
	mountFlags, err := rt.engine.baseline.ListMountBoundaries(ctx)
	if err != nil {
		return nil, err
	}
	localObs.SetMountBoundaryFlags(mountFlags)
	localObs.SetProtectedRootEventSink(func(event syncshortcut.ProtectedRootEvent) {
		select {
		case protectedRootEvents <- event:
		default:
			// Debug: self-healing back-pressure; reconciliation retries.
			rt.deps.logger.Debug("managed shortcut root event channel full; parent engine reconciliation will retry",
				slog.String("path", event.Path),
				slog.String("binding_id", event.BindingID),
			)
		}
	})
	localObs.SetLocalObservationBatchChannel(localBatches)
	localObs.SetSafetyScan(syncobserve.LocalWatchHealthySafetyScanInterval, true)
	localObs.SetDebugEvents(rt.deps.emit)

	if rt.engine.hooks.localWatcherFactory != nil {
		localObs.SetWatcherFactory(rt.engine.hooks.localWatcherFactory)
	}

	return localObs, nil
}

// startLocalObserver owns the local observer goroutine, mirroring
// startRemoteObserver so both observers are launched through one shape.
func (rt *watchRuntime) startLocalObserver(
	ctx context.Context,
	localObs *syncobserve.LocalObserver,
	localBatches chan<- syncobserve.LocalObservation,
	protectedRootEvents chan<- syncshortcut.ProtectedRootEvent,
	errs chan<- error,
) {
	rt.resources.goroutines.start(ctx, watchGoroutineLocalObserver, func(ctx context.Context) {
		defer rt.deps.emit(syncdomain.EngineDebugEvent{
			Type: syncdomain.EngineDebugEventObserverExited, Observer: syncdomain.EngineDebugObserverLocal,
		})
		defer close(localBatches)
		defer close(protectedRootEvents)
		defer recoverObserverPanic(rt.deps.logger, observerNameLocal, errs)

		errs <- rt.watchLocalWithFallback(ctx, localObs)
	})
}

// watchLocalWithFallback runs the local watcher and, while the operating
// system cannot provide the watches it needs, falls back to periodic full
// scans and retries the watcher with backoff. It returns the observer's exit
// error, or nil on shutdown.
//
// The fallback used to scan hourly for the rest of the session: a transient
// limit (another program briefly holding inotify instances or descriptors)
// degraded the session forever, the first scan came an hour late, and the
// write-coalescing hashes and recovery scan the stopped watcher had pending
// were simply lost. Now the fallback scans at once, then at the normal
// safety-scan interval, and tries to watch again after 10 minutes, doubling
// to an hour while the limit persists.
func (rt *watchRuntime) watchLocalWithFallback(ctx context.Context, localObs *syncobserve.LocalObserver) error {
	tree := rt.engine.syncTree
	retryDelay := syncobserve.LocalWatchRetryInitialDelay
	var warnedCause syncobserve.WatchUnavailableCause

	for {
		watchStart := rt.deps.now()
		watchErr := localObs.Watch(ctx, tree)
		cause, ok := syncobserve.WatchUnavailableCauseOf(watchErr)
		if !ok {
			return observerExitError(observerNameLocal, watchErr)
		}
		if rt.deps.since(watchStart) >= syncobserve.LocalWatchRetryMaxDelay {
			// Watching worked for a long stretch before failing again: this
			// is a new episode, not a persisting limit.
			retryDelay = syncobserve.LocalWatchRetryInitialDelay
		}

		interval := localObs.EffectiveSafetyScanInterval()
		rt.deps.emit(syncdomain.EngineDebugEvent{
			Type: syncdomain.EngineDebugEventObserverFallbackStarted, Observer: syncdomain.EngineDebugObserverLocal,
		})
		if cause != warnedCause {
			warnedCause = cause
			rt.deps.logger.Warn("Cannot watch the sync folder for changes ("+syncobserve.WatchUnavailableReason(cause)+"). "+
				"Local changes will be noticed every "+syncobserve.WaitText(interval)+
				" instead of at once; watching will be retried. To fix: "+syncobserve.WatchUnavailableFix(cause),
				slog.Duration("scan_interval", interval),
				slog.Duration("retry_after", retryDelay),
				slog.Any("error", watchErr),
			)
		} else {
			rt.deps.logger.Debug("watching the sync folder is still unavailable",
				slog.Duration("retry_after", retryDelay),
				slog.Any("error", watchErr),
			)
		}

		retryDue := rt.runPeriodicFullScan(ctx, localObs, tree, interval, retryDelay)
		rt.deps.emit(syncdomain.EngineDebugEvent{
			Type: syncdomain.EngineDebugEventObserverFallbackStopped, Observer: syncdomain.EngineDebugObserverLocal,
		})
		if !retryDue {
			return nil // clean exit after context cancel
		}

		retryDelay = min(2*retryDelay, syncobserve.LocalWatchRetryMaxDelay)
		rt.deps.logger.Info("retrying to watch the sync folder for changes")
	}
}

func (rt *watchRuntime) startRemoteObserver(
	ctx context.Context,
	bl *syncdomain.Baseline,
	remoteBatches chan<- syncobserve.RemoteObservationBatch,
	errs chan<- error,
	opts WatchOptions,
) {
	pollInterval := rt.engine.resolvePollInterval(opts)
	rt.startPrimaryRootWatch(ctx, bl, remoteBatches, errs, pollInterval)
}

func (rt *watchRuntime) startSocketIOWakeSource(ctx context.Context) <-chan struct{} {
	if !rt.engine.enableWebsocket || rt.engine.socketIOFetcher == nil {
		return nil
	}

	rt.deps.logger.Info("starting socket.io wake source",
		slog.String("drive_id", rt.engine.driveID.String()),
	)

	wakeCh := make(chan struct{}, 1)
	wakeSource := rt.engine.newSocketIOWakeSource(syncobserve.SocketIOWakeSourceOptions{
		Logger:        rt.deps.logger,
		Clock:         rt.deps.clock,
		LifecycleHook: rt.emitSocketIOLifecycleEvent,
	})

	rt.resources.goroutines.start(ctx, watchGoroutineWakeSource, func(ctx context.Context) {
		rt.runSocketIOWakeSource(ctx, wakeSource, wakeCh)
	})

	return wakeCh
}

// runSocketIOWakeSource runs the wake source until ctx ends, and is the panic
// boundary of its goroutine. A panic takes the goroutine's
// ordinary failure path: the wake source is an accelerator, so losing it
// leaves watch polling on its interval and nothing else changes. The panic
// itself is still logged at Error with its stack, because it is a bug.
// Unrecovered, it would end the process and every mount with it.
func (rt *watchRuntime) runSocketIOWakeSource(
	ctx context.Context,
	wakeSource socketIOWakeSourceRunner,
	wakeCh chan<- struct{},
) {
	defer func() {
		if r := recover(); r != nil {
			rt.reportSocketIOWakeSourceStop(panicguard.Recovered(rt.deps.logger, "in the socket.io wake source", r,
				slog.String("drive_id", rt.engine.driveID.String())))
		}
	}()

	if err := wakeSource.Run(ctx, wakeCh); err != nil {
		rt.reportSocketIOWakeSourceStop(err)
	}
}

// reportSocketIOWakeSourceStop says, at Info, that the wake source ended with
// err: watch keeps polling on its interval regardless.
func (rt *watchRuntime) reportSocketIOWakeSourceStop(err error) {
	rt.deps.logger.Info("Instant change notifications from OneDrive stopped; still checking on the poll interval",
		slog.String("drive_id", rt.engine.driveID.String()),
		slog.Any("error", err),
	)
}

func (rt *watchRuntime) emitSocketIOLifecycleEvent(event syncobserve.SocketIOLifecycleEvent) {
	debugEvent := syncdomain.EngineDebugEvent{
		DriveID: event.DriveID,
		Note:    event.Note,
		Delay:   event.Delay,
		Error:   event.Error,
	}

	switch event.Type {
	case syncobserve.SocketIOLifecycleEventStarted:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketWakeSourceStarted
	case syncobserve.SocketIOLifecycleEventEndpointFetchFail:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketEndpointFetchFail
	case syncobserve.SocketIOLifecycleEventConnectFail:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketConnectFail
	case syncobserve.SocketIOLifecycleEventConnected:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketConnected
		if event.SID != "" {
			debugEvent.Note = "sid=" + event.SID
		}
	case syncobserve.SocketIOLifecycleEventRefreshRequested:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketRefreshRequested
	case syncobserve.SocketIOLifecycleEventConnectionDropped:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketConnectionDropped
	case syncobserve.SocketIOLifecycleEventNotificationWake:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketNotificationWake
	case syncobserve.SocketIOLifecycleEventWakeCoalesced:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketWakeCoalesced
	case syncobserve.SocketIOLifecycleEventStopped:
		debugEvent.Type = syncdomain.EngineDebugEventWebsocketWakeSourceStopped
	default:
		return
	}

	rt.deps.emit(debugEvent)
}

// runPeriodicFullScan runs full filesystem scans while watching is
// unavailable: one at once (it also covers whatever the stopped watcher had
// pending), then every interval. It returns true when retryAfter has passed
// and the caller should try to watch again, false when the context ended.
func (rt *watchRuntime) runPeriodicFullScan(
	ctx context.Context, obs *syncobserve.LocalObserver, tree *synctree.Root,
	interval time.Duration, retryAfter time.Duration,
) bool {
	ticker := rt.deps.clock.NewTicker(interval)
	defer syncdomain.StopTicker(ticker)

	retryCh := make(chan struct{}, 1)
	retryTimer := rt.deps.clock.AfterFunc(retryAfter, func() {
		select {
		case retryCh <- struct{}{}:
		default:
		}
	})
	defer retryTimer.Stop()

	rt.deps.logger.Info("periodic full scan fallback started",
		slog.Duration("interval", interval),
		slog.Duration("retry_after", retryAfter),
	)

	// scanFailing keeps a persistent scan failure to one Warn per streak.
	scanFailing := false
	if !rt.runFallbackScan(ctx, obs, tree, &scanFailing) {
		return false
	}
	for {
		select {
		case <-syncdomain.TickerChan(ticker):
			// Jitter: sleep 0-10% of interval to prevent thundering-herd
			// when multiple drives fire periodic scans simultaneously.
			if jitter := interval / periodicScanJitterDivisor; jitter > 0 {
				if err := rt.deps.clock.Sleep(ctx, rt.deps.clock.Jitter(jitter)); err != nil {
					return false
				}
			}
			if !rt.runFallbackScan(ctx, obs, tree, &scanFailing) {
				return false
			}
		case <-retryCh:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

// runFallbackScan runs one full scan and hands its snapshot, rows and finding
// set together, to the runtime. The scan stands in for both the watcher and
// the safety scan, so its findings go exactly as a safety scan's do, the
// empty set included. It returns false when the context ended.
func (rt *watchRuntime) runFallbackScan(
	ctx context.Context, obs *syncobserve.LocalObserver, tree *synctree.Root, scanFailing *bool,
) bool {
	result, err := obs.FullScan(ctx, tree)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}

		level := slog.LevelDebug
		if !*scanFailing {
			level = slog.LevelWarn
		}
		*scanFailing = true
		rt.deps.logger.Log(ctx, level, "Could not rescan the sync folder; will retry",
			slog.Any("error", err),
		)
		obs.TrySendLocalTruthSuspect(ctx, syncdomain.LocalTruthRecoveryFullScanFailed)

		return true
	}
	*scanFailing = false

	obs.TrySendFullLocalSnapshot(ctx, result)
	rt.deps.emit(syncdomain.EngineDebugEvent{
		Type: syncdomain.EngineDebugEventObserverFallbackScanned, Observer: syncdomain.EngineDebugObserverLocal,
	})

	return true
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
