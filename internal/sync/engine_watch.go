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
	watchObservationBuf = 256
	// watchResultBuf is the buffer size for the action completion channel in watch
	// mode. Large enough for typical batches without blocking workers.
	watchResultBuf = 4096

	// maintenanceInterval is how often the watch loop runs summary logging and
	// maintenance bookkeeping that is independent of action dispatch.
	maintenanceInterval = 10 * time.Second
)

const (
	fullRemoteRefreshInterval           = 24 * time.Hour
	localWatchHealthySafetyScanInterval = 5 * time.Minute
	localWatchDegradedFullScanInterval  = time.Hour
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
	bl, err := rt.runStartupStage(ctx, rt)
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

		return fmt.Errorf("sync: initial sync failed: %w", err)
	}

	// Step 3: Start observers AFTER bootstrap — they see the post-bootstrap baseline.
	rt.startObservers(ctx, pipe.bl, opts)

	// Step 4: Run the watch loop.
	return rt.runWatchLoop(ctx, pipe)
}

func isWatchShutdownError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() == nil {
		return false
	}

	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// watchPipeline holds watch-loop dependencies that are not already runtime-owned.
// Created by initWatchInfra; cleaned up by its cleanup method.
type watchPipeline struct {
	runtime      *watchRuntime
	bl           *Baseline
	replanReady  <-chan dirtyBatch
	completions  <-chan ActionCompletion
	maintenanceC <-chan time.Time
	mode         Mode
	pool         *WorkerPool // for bootstrapSync to access Completions()
	cleanup      func()
}

type socketIOWakeSourceRunner interface {
	Run(ctx context.Context, wakes chan<- struct{}) error
}

// initWatchInfra sets up watch-mode infrastructure: watchRuntime, DepGraph,
// worker pool, dirty scheduler, persisted scope state, and tickers. Does NOT load
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
	mode Mode,
	opts WatchOptions,
) (*watchPipeline, error) {
	// DepGraph tracks action dependencies. Active scope state is loaded from
	// the persisted block_scopes table into watch-owned runtime state.
	depGraph := NewDepGraph(rt.engine.logger)
	rt.depGraph = depGraph
	if err := rt.loadActiveScopes(ctx, rt); err != nil {
		return nil, fmt.Errorf("sync: loading active scopes: %w", err)
	}

	rt.scopeState = NewScopeState(rt.engine.nowFunc, rt.engine.logger)
	rt.nextActionID = 0

	// dispatchCh feeds admitted actions to workers. Buffer is generous to avoid
	// backpressure when a batch produces many immediately-ready actions.
	rt.dispatchCh = make(chan *TrackedAction, watchResultBuf)

	pool := NewWorkerPool(rt.engine.execCfg, rt.dispatchCh, rt.engine.baseline, rt.engine.logger, watchResultBuf)
	pool.Start(ctx, rt.engine.transferWorkers)

	// DirtyBuffer is the watch scheduler boundary. Observation marks coarse
	// dirty/full-refresh signals only; snapshot refresh and planning happen
	// after debounce.
	dirtyBuf := NewDirtyBuffer(rt.engine.logger)
	rt.dirtyBuf = dirtyBuf
	replanReady := dirtyBuf.FlushDebounced(ctx, rt.engine.resolveDebounce(opts))

	// Tickers/timers.
	rt.resetRefreshTimer(nil)
	maintenanceTicker := rt.engine.newTicker(maintenanceInterval)

	// Arm retrier timer from DB — picks up items from prior crash or prior pass.
	rt.kickRetryHeldReleaseNow()
	rt.armTrialTimer()

	pipe := &watchPipeline{
		runtime:      rt,
		replanReady:  replanReady,
		completions:  pool.Completions(),
		maintenanceC: tickerChan(maintenanceTicker),
		mode:         mode,
		pool:         pool,
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

		stopTicker(maintenanceTicker)
		rt.resetRefreshTimer(nil)

		inFlight := depGraph.InFlightCount()
		if inFlight > 0 {
			rt.engine.logger.Info("graceful shutdown: draining in-flight actions",
				slog.Int("in_flight", inFlight),
			)
		}

		rt.stopRetryTimer()
		rt.stopTrialTimer()
		pool.Stop() // closes completions channel after workers exit
		rt.observerErrs = nil
		rt.localEvents = nil
		rt.remoteBatches = nil
		rt.skippedItems = nil
		rt.activeObservers = 0
		rt.remoteObs = nil
		rt.localObs = nil
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventWatchStopped})
		rt.engine.logger.Info("watch mode stopped")
	}

	return pipe, nil
}

// bootstrapSync performs the initial sync using the watch pipeline. It
// observes current truth, builds the current plan, reconciles runtime state,
// and starts the shared runtime through the
// shared current-plan stage sequence, then dispatches through the same
// DepGraph, active scope working set, and WorkerPool that the steady-state
// watch loop uses. Blocks until all bootstrap actions due now complete.
//
// Must be called after startup + initWatchInfra and before startObservers.
func (rt *watchRuntime) bootstrapSync(ctx context.Context, mode Mode, pipe *watchPipeline) error {
	rt.engine.logger.Info("bootstrap sync starting", slog.String("mode", mode.String()))

	if pipe == nil || pipe.bl == nil {
		return fmt.Errorf("sync: bootstrap requires startup baseline")
	}

	runtime, err := rt.runBootstrapCurrentPlan(ctx, pipe.bl, mode)
	if err != nil {
		return err
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
	initialOutbox, _, err := rt.startRuntimeStage(ctx, runtime, pipe.bl, rt)
	if err != nil {
		return fmt.Errorf("sync: bootstrap dispatch failed: %w", err)
	}

	// Wait for all bootstrap actions to complete.
	if err := rt.runWatchUntilQuiescent(ctx, pipe, initialOutbox); err != nil {
		return fmt.Errorf("sync: bootstrap quiescence failed: %w", err)
	}

	return rt.finishBootstrapAfterActions(ctx)
}

func (rt *watchRuntime) finishBootstrapWithoutActions(
	ctx context.Context,
	pendingRemoteObservation *remoteObservationBatch,
) error {
	if _, err := rt.commitPendingRemoteObservation(ctx, pendingRemoteObservation); err != nil {
		return err
	}
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventBootstrapQuiesced})
	if err := rt.armFullRefreshTimer(ctx); err != nil {
		return fmt.Errorf("sync: arming full remote refresh timer: %w", err)
	}
	rt.engine.logger.Info("bootstrap sync complete: no changes detected")
	return nil
}

func (rt *watchRuntime) finishBootstrapAfterActions(
	ctx context.Context,
) error {
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventBootstrapQuiesced})
	rt.postSyncHousekeeping()
	if err := rt.armFullRefreshTimer(ctx); err != nil {
		return fmt.Errorf("sync: arming full remote refresh timer: %w", err)
	}
	rt.engine.logger.Info("bootstrap sync complete")
	return nil
}

// startObservers launches remote and local observer goroutines and stores
// their channels on the watch runtime. The watch loop owns remote batch
// application and dirty marking; observers emit only local change hints or
// loop-applied remote batches.
func (rt *watchRuntime) startObservers(
	ctx context.Context, bl *Baseline, opts WatchOptions,
) {
	localEvents := make(chan ChangeEvent, watchObservationBuf)
	remoteBatches := make(chan remoteObservationBatch, watchObservationBuf)
	errs := make(chan error, 2)

	var obsWg stdsync.WaitGroup

	count := 0

	obsWg.Add(1)
	count++
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverStarted, Observer: engineDebugObserverRemote})
	rt.startRemoteObserver(ctx, &obsWg, bl, remoteBatches, errs, opts)

	// Channel for forwarding SkippedItems from safety scans to the engine.
	// Buffered(2) — at most 2 safety scans could overlap before draining.
	skippedCh := make(chan []SkippedItem, 2)

	localObs := NewLocalObserver(bl, rt.engine.logger, rt.engine.checkWorkers)
	localObs.SetFilterConfig(rt.engine.localFilter)
	localObs.SetObservationRules(rt.engine.localRules)
	localObs.SetSkippedChannel(skippedCh)
	localObs.safetyScanInterval = localWatchHealthySafetyScanInterval

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
		defer close(localEvents)

		watchErr := localObs.Watch(ctx, rt.engine.syncTree, localEvents)
		if errors.Is(watchErr, ErrWatchLimitExhausted) {
			rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverFallbackStarted, Observer: engineDebugObserverLocal})
			rt.engine.logger.Warn("inotify watch limit exhausted, falling back to periodic full scan",
				slog.Duration("scan_interval", localWatchDegradedFullScanInterval),
			)

			rt.runPeriodicFullScan(ctx, localObs, rt.engine.syncTree, localEvents, localWatchDegradedFullScanInterval)
			rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverFallbackStopped, Observer: engineDebugObserverLocal})
			errs <- nil // clean exit after context cancel

			return
		}

		errs <- watchErr
	}()

	rt.observerErrs = errs
	rt.activeObservers = count
	rt.skippedItems = skippedCh
	rt.localEvents = localEvents
	rt.remoteBatches = remoteBatches
}

func (rt *watchRuntime) startRemoteObserver(
	ctx context.Context,
	obsWg *stdsync.WaitGroup,
	bl *Baseline,
	remoteBatches chan<- remoteObservationBatch,
	errs chan<- error,
	opts WatchOptions,
) {
	pollInterval := rt.engine.resolvePollInterval(opts)
	rt.startPrimaryRootWatch(ctx, obsWg, bl, remoteBatches, errs, pollInterval)
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
			// The primary scan and safety scan handle persisting observation issues.
			for i := range result.Events {
				obs.TrySend(ctx, events, &result.Events[i])
			}

			if len(result.Skipped) > 0 {
				rt.engine.logger.Debug("periodic scan: skipped items",
					slog.Int("count", len(result.Skipped)))
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
