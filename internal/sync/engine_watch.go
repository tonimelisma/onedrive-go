package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncrecovery"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
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
	// changes (e.g., `issues force-deletes` via the CLI). Uses PRAGMA data_version
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

// initDeleteProtection sets up the rolling delete counter and clears stale
// big_delete_held entries from a prior daemon session. Force mode disables
// the counter (deleteCounter stays nil). Also seeds lastDataVersion so
// the first recheck tick doesn't fire spuriously.
func (rt *watchRuntime) initDeleteProtection(ctx context.Context, force bool) {
	if !force {
		rt.deleteCounter = syncdispatch.NewDeleteCounter(rt.engine.bigDeleteThreshold, deleteCounterWindow, rt.engine.nowFunc)
	}

	if err := rt.engine.baseline.ClearResolvedActionableFailures(ctx, synctypes.IssueBigDeleteHeld, nil); err != nil {
		rt.engine.logger.Warn("failed to clear stale big-delete-held entries",
			slog.String("error", err.Error()),
		)
	}

	if dv, dvErr := rt.engine.baseline.DataVersion(ctx); dvErr == nil {
		rt.lastDataVersion = dv
	}
}

// loadWatchState loads the baseline and shortcuts for the watch session.
// Both are loaded once after the initial sync. synctypes.Baseline is live-mutated
// under RWMutex; shortcuts are updated via setShortcuts when they change.
func (rt *watchRuntime) loadWatchState(ctx context.Context) (*synctypes.Baseline, error) {
	bl, err := rt.engine.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline for watch: %w", err)
	}

	shortcuts, scErr := rt.engine.baseline.ListShortcuts(ctx)
	if scErr != nil {
		rt.engine.logger.Warn("failed to load shortcuts for watch mode",
			slog.String("error", scErr.Error()),
		)
	}

	rt.setShortcuts(shortcuts)

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

	rt := newWatchRuntime(e)
	if e.watchRuntimeHook != nil {
		e.watchRuntimeHook(rt)
	}
	proof, proofErr := e.proveDriveIdentity(ctx)
	if proofErr != nil {
		if isWatchShutdownError(ctx, proofErr) {
			return nil
		}

		// Startup auth repair is the only case that should proceed past a
		// failing proof. Without a persisted auth scope there is nothing to
		// repair, so watch mode must abort before it allocates workers/timers.
		hasAuthScope, err := e.hasPersistedAuthScope(ctx)
		if err != nil {
			if isWatchShutdownError(ctx, err) {
				return nil
			}

			return err
		}
		if !hasAuthScope {
			return proofErr
		}
	}

	// Step 1: Set up watch infrastructure (no observers yet).
	pipe, err := rt.initWatchInfra(ctx, mode, opts, proof, proofErr)
	if err != nil {
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
	errs, activeObs, skippedCh, scopeChanges := rt.startObservers(ctx, pipe.bl, mode, rt.buf, opts)
	pipe.errs = errs
	pipe.activeObs = activeObs
	pipe.skippedCh = skippedCh
	pipe.scopeChanges = scopeChanges

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
	bl               *synctypes.Baseline
	safety           *synctypes.SafetyConfig
	batchReady       <-chan []synctypes.PathChanges
	results          <-chan synctypes.WorkerResult
	errs             <-chan error
	skippedCh        <-chan []synctypes.SkippedItem
	scopeChanges     <-chan syncscope.Change
	reconcileC       <-chan time.Time
	reconcileResults <-chan reconcileResult
	recheckC         <-chan time.Time
	activeObs        int
	mode             synctypes.SyncMode
	pool             *syncexec.WorkerPool // for bootstrapSync to access Results()
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
	mode synctypes.SyncMode,
	opts synctypes.WatchOpts,
	proof driveIdentityProof,
	proofErr error,
) (*watchPipeline, error) {
	// Enable watch-mode-specific executor behavior (pre-upload eTag
	// freshness checks to prevent silently overwriting concurrent remote
	// changes — see executor_transfer.go).
	rt.engine.execCfg.SetWatchMode(true)

	rt.initDeleteProtection(ctx, opts.Force)

	// Normalize persisted scope rows before loading runtime scope state.
	// Startup must not trust stale scope rows blindly; the durable store is
	// repaired against current persisted evidence before the watch loop loads
	// its ephemeral activeScopes working set.
	if err := rt.scopeController().repairPersistedScopes(ctx, rt, proof, proofErr); err != nil {
		return nil, fmt.Errorf("sync: repairing persisted scopes: %w", err)
	}

	// DepGraph tracks action dependencies. Active scope state is loaded from
	// the persisted scope_blocks table into watch-owned runtime state.
	depGraph := syncdispatch.NewDepGraph(rt.engine.logger)
	rt.depGraph = depGraph
	if err := rt.scopeController().loadActiveScopes(ctx, rt); err != nil {
		return nil, fmt.Errorf("sync: loading active scopes: %w", err)
	}

	rt.scopeState = syncdispatch.NewScopeState(rt.engine.nowFunc, rt.engine.logger)
	rt.nextActionID = 0

	// dispatchCh feeds admitted actions to workers. Buffer is generous to avoid
	// backpressure when a batch produces many immediately-ready actions.
	rt.dispatchCh = make(chan *synctypes.TrackedAction, watchResultBuf)

	// Never-closing done channel — DepGraph.Done() would fire prematurely
	// between batches when completed == total. Workers exit only via ctx.Done().
	neverDone := make(chan struct{})

	pool := syncexec.NewWorkerPool(rt.engine.execCfg, rt.dispatchCh, neverDone, rt.engine.baseline, rt.engine.logger, watchResultBuf)
	pool.Start(ctx, rt.engine.transferWorkers)

	// Buffer promoted to watchRuntime so observed and reconciliation work share
	// the same debounce/planning path the watch loop already owns.
	buf := syncobserve.NewBuffer(rt.engine.logger)
	rt.buf = buf
	batchReady := buf.FlushDebounced(ctx, rt.engine.resolveDebounce(opts))

	// Tickers.
	reconcileTicker := rt.engine.initReconcileTicker(opts)
	recheckTicker := rt.engine.newTicker(recheckInterval)

	// Arm retrier timer from DB — picks up items from prior crash or prior pass.
	rt.kickRetrySweepNow()
	rt.armTrialTimer()

	pipe := &watchPipeline{
		runtime:          rt,
		safety:           rt.engine.resolveSafetyConfig(opts.Force, true),
		batchReady:       batchReady,
		results:          pool.Results(),
		reconcileC:       tickerChan(reconcileTicker),
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
		stopTicker(reconcileTicker)

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
func (rt *watchRuntime) bootstrapSync(ctx context.Context, mode synctypes.SyncMode, pipe *watchPipeline) error {
	rt.engine.logger.Info("bootstrap sync starting", slog.String("mode", mode.String()))

	// Crash recovery: reset in-progress states from prior crash.
	if err := syncrecovery.ResetInProgressStates(
		ctx,
		rt.engine.baseline,
		rt.engine.syncTree,
		retry.ReconcilePolicy().Delay,
		rt.engine.logger,
	); err != nil {
		rt.engine.logger.Warn("failed to reset in-progress states", slog.String("error", err.Error()))
	}

	// Load baseline + shortcuts.
	bl, err := rt.loadWatchState(ctx)
	if err != nil {
		return err
	}
	pipe.bl = bl

	// Permission rechecks.
	if rt.engine.permHandler.HasPermChecker() {
		shortcuts := rt.getShortcuts()
		rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, rt.engine.permHandler.recheckPermissions(ctx, bl, shortcuts))
	}
	rt.scopeController().applyPermissionRecheckDecisions(ctx, rt, rt.engine.permHandler.recheckLocalPermissions(ctx))

	// Observe changes.
	changes, pendingTokens, err := rt.observeChanges(ctx, rt, bl, mode, false, false)
	if err != nil {
		return fmt.Errorf("sync: bootstrap observation failed: %w", err)
	}

	if len(changes) == 0 {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventBootstrapQuiesced})
		rt.engine.logger.Info("bootstrap sync complete: no changes detected")
		return nil
	}

	// Commit the deferred delta token before dispatching bootstrap actions.
	// Bootstrap uses watch-mode big-delete (rolling counter), not planner-level
	// threshold, so the token is always safe to commit here.
	if err := rt.commitDeferredDeltaTokens(ctx, pendingTokens); err != nil {
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
	rt.engine.logger.Info("bootstrap sync complete")
	return nil
}

// startObservers launches remote and local observer goroutines that feed
// events into the buffer. Returns an error channel for observer failures and
// the number of observers started. The events channel is closed automatically
// when all observers exit, allowing the bridge goroutine to drain cleanly.
func (rt *watchRuntime) startObservers(
	ctx context.Context, bl *synctypes.Baseline, mode synctypes.SyncMode, buf *syncobserve.Buffer, opts synctypes.WatchOpts,
) (<-chan error, int, <-chan []synctypes.SkippedItem, <-chan syncscope.Change) {
	events := make(chan synctypes.ChangeEvent, watchEventBuf)
	errs := make(chan error, 2)

	var obsWg stdsync.WaitGroup

	rt.startWatchEventBridge(ctx, buf, events)

	count := 0

	// Remote observer (skip for upload-only mode).
	if mode != synctypes.SyncUploadOnly {
		obsWg.Add(1)
		count++
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverStarted, Observer: engineDebugObserverRemote})
		rt.startRemoteObserver(ctx, &obsWg, bl, events, errs, opts)
	}

	// Channel for forwarding SkippedItems from safety scans to the engine.
	// Buffered(2) — at most 2 safety scans could overlap before draining.
	skippedCh := make(chan []synctypes.SkippedItem, 2)

	// Local observer (skip for download-only mode).
	if mode != synctypes.SyncDownloadOnly {
		localObs := syncobserve.NewLocalObserver(bl, rt.engine.logger, rt.engine.checkWorkers)
		localObs.SetFilterConfig(rt.engine.localFilter)
		localObs.SetObservationRules(rt.engine.localRules)
		localObs.SetSafetyScanInterval(opts.SafetyScanInterval)
		localObs.SetSkippedChannel(skippedCh)
		localObs.SetScopeSnapshot(rt.currentScopeSnapshot())
		localObs.SetScopeChangeChannel(rt.scopeChanges)

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
			if errors.Is(watchErr, synctypes.ErrWatchLimitExhausted) {
				rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverFallbackStarted, Observer: engineDebugObserverLocal})
				rt.engine.logger.Warn("inotify watch limit exhausted, falling back to periodic full scan",
					slog.Duration("poll_interval", rt.engine.resolvePollInterval(opts)),
				)

				rt.runPeriodicFullScan(ctx, localObs, rt.engine.syncTree, events, rt.engine.resolvePollInterval(opts))
				rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverFallbackStopped, Observer: engineDebugObserverLocal})
				errs <- nil // clean exit after context cancel

				return
			}

			errs <- watchErr
		}()
	}

	rt.closeWatchEventsWhenObserversExit(&obsWg, events)

	return errs, count, skippedCh, rt.scopeChanges
}

func (rt *watchRuntime) startRemoteObserver(
	ctx context.Context,
	obsWg *stdsync.WaitGroup,
	bl *synctypes.Baseline,
	events chan<- synctypes.ChangeEvent,
	errs chan<- error,
	opts synctypes.WatchOpts,
) {
	pollInterval := rt.engine.resolvePollInterval(opts)
	if rt.engine.usesPrimaryPathScopes() {
		scopes, rootFallback, err := rt.engine.resolvePrimaryObservationScopes(ctx)
		if err != nil {
			go func() {
				defer obsWg.Done()
				defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
				errs <- err
			}()

			return
		}

		if !rootFallback && len(scopes) > 0 {
			if rt.engine.enableWebsocket {
				rt.engine.emitDebugEvent(engineDebugEvent{
					Type: engineDebugEventWebsocketFallback,
					Note: "sync_paths",
				})
				rt.engine.logger.Warn("websocket watch is not supported for sync_paths-scoped sessions; falling back to polling",
					slog.String("drive_id", rt.engine.driveID.String()),
				)
			}

			go func() {
				defer obsWg.Done()
				defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
				errs <- rt.watchPrimaryScopedRemote(ctx, bl, events, pollInterval, scopes)
			}()

			return
		}
	}

	rt.warnWebsocketFallbackIfNeeded()
	if rt.engine.hasScopedRoot() {
		go func() {
			defer obsWg.Done()
			defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
			errs <- rt.watchScopedRootRemote(ctx, bl, events, pollInterval)
		}()

		return
	}

	remoteObs := syncobserve.NewRemoteObserver(rt.engine.fetcher, bl, rt.engine.driveID, rt.engine.logger)
	remoteObs.SetObsWriter(rt.engine.baseline)
	remoteObs.SetWatchObservationPreparer(func(
		_ context.Context,
		events []synctypes.ChangeEvent,
	) ([]synctypes.ObservedItem, []synctypes.ChangeEvent, error) {
		scoped := applyRemoteScope(rt.engine.logger, rt.currentScopeSnapshot(), events)
		return scoped.observed, scoped.emitted, nil
	})
	rt.remoteObs = remoteObs
	rt.startSocketIOWakeSource(ctx, remoteObs)

	savedToken, tokenErr := rt.engine.baseline.GetDeltaToken(ctx, rt.engine.driveID.String(), "")
	if tokenErr != nil {
		rt.engine.logger.Warn("failed to get delta token for watch",
			slog.String("error", tokenErr.Error()),
		)
	}

	go func() {
		defer obsWg.Done()
		defer rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventObserverExited, Observer: engineDebugObserverRemote})
		errs <- remoteObs.Watch(ctx, savedToken, events, pollInterval)
	}()
}

func (rt *watchRuntime) warnWebsocketFallbackIfNeeded() {
	if rt.engine.enableWebsocket && rt.engine.hasScopedRoot() {
		rt.engine.emitDebugEvent(engineDebugEvent{
			Type: engineDebugEventWebsocketFallback,
			Note: "scoped_root",
		})
		rt.engine.logger.Warn("websocket watch is not supported for scoped-root sessions; falling back to polling",
			slog.String("drive_id", rt.engine.driveID.String()),
			slog.String("root_item_id", rt.engine.rootItemID),
		)
	}
	if rt.engine.enableWebsocket && rt.engine.socketIOFetcher == nil {
		rt.engine.emitDebugEvent(engineDebugEvent{
			Type: engineDebugEventWebsocketFallback,
			Note: "missing_fetcher",
		})
		rt.engine.logger.Warn("websocket watch requested but no Socket.IO fetcher is available; falling back to polling",
			slog.String("drive_id", rt.engine.driveID.String()),
		)
	}
}

func (rt *watchRuntime) startSocketIOWakeSource(ctx context.Context, remoteObs *syncobserve.RemoteObserver) {
	if !rt.engine.enableWebsocket || rt.engine.socketIOFetcher == nil || remoteObs == nil {
		return
	}

	rt.engine.logger.Info("starting socket.io wake source",
		slog.String("drive_id", rt.engine.driveID.String()),
	)

	wakeCh := make(chan struct{}, 1)
	remoteObs.SetWakeChannel(wakeCh)

	stopCh := make(chan struct{})
	rt.socketIOWakeStop = stopCh
	rt.socketIOWakeDone = make(chan struct{})
	wakeSource := rt.engine.socketIOWakeSourceFactory(
		rt.engine.socketIOFetcher,
		rt.engine.driveID,
		syncobserve.SocketIOWakeSourceOptions{
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
}

func (rt *watchRuntime) emitSocketIOLifecycleEvent(event syncobserve.SocketIOLifecycleEvent) {
	debugEvent := engineDebugEvent{
		DriveID: event.DriveID,
		Note:    event.Note,
		Delay:   event.Delay,
		Error:   event.Error,
	}

	switch event.Type {
	case syncobserve.SocketIOLifecycleEventStarted:
		debugEvent.Type = engineDebugEventWebsocketWakeSourceStarted
	case syncobserve.SocketIOLifecycleEventEndpointFetchFail:
		debugEvent.Type = engineDebugEventWebsocketEndpointFetchFail
	case syncobserve.SocketIOLifecycleEventConnectFail:
		debugEvent.Type = engineDebugEventWebsocketConnectFail
	case syncobserve.SocketIOLifecycleEventConnected:
		debugEvent.Type = engineDebugEventWebsocketConnected
		if event.SID != "" {
			debugEvent.Note = "sid=" + event.SID
		}
	case syncobserve.SocketIOLifecycleEventRefreshRequested:
		debugEvent.Type = engineDebugEventWebsocketRefreshRequested
	case syncobserve.SocketIOLifecycleEventConnectionDropped:
		debugEvent.Type = engineDebugEventWebsocketConnectionDropped
	case syncobserve.SocketIOLifecycleEventNotificationWake:
		debugEvent.Type = engineDebugEventWebsocketNotificationWake
	case syncobserve.SocketIOLifecycleEventWakeCoalesced:
		debugEvent.Type = engineDebugEventWebsocketWakeCoalesced
	case syncobserve.SocketIOLifecycleEventStopped:
		debugEvent.Type = engineDebugEventWebsocketWakeSourceStopped
	default:
		return
	}

	rt.engine.emitDebugEvent(debugEvent)
}

func (rt *watchRuntime) startWatchEventBridge(
	ctx context.Context,
	buf *syncobserve.Buffer,
	events <-chan synctypes.ChangeEvent,
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
	events chan synctypes.ChangeEvent,
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
	ctx context.Context, obs *syncobserve.LocalObserver, tree *synctree.Root,
	events chan<- synctypes.ChangeEvent, interval time.Duration,
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
