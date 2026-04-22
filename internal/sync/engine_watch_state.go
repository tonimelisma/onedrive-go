package sync

import (
	"context"
	"sort"
	"sync"
	"time"
)

type watchLoopState struct {
	phase            watchRuntimePhase
	outbox           []*TrackedAction
	replanQueued     bool
	pendingReplan    DirtyBatch
	hasPendingReplan bool
}

type watchRuntimeState struct {
	loop watchLoopState

	// syncBatch tracks the current best-effort watch batch so status updates are
	// written only after the loop has exhausted all currently admissible work and
	// returned to quiescence.
	syncBatch watchSyncBatchState

	// afterSteadyStateObserve is a test-only hook called after steady-state
	// local observation succeeds but before the watch loop commits the local
	// snapshot. Nil in production.
	afterSteadyStateObserve func()

	// afterAppendReadyFrontier is a test-only hook called immediately before the
	// watch runtime appends the reduced concrete frontier to its outbox. Nil in
	// production.
	afterAppendReadyFrontier func()
}

type watchSyncBatchState struct {
	active        bool
	startedAt     time.Time
	succeededBase int
	failedBase    int
	errorBase     int
}

type watchObservationState struct {
	// Dirty debounce buffer. Local and remote observers mark paths or full
	// refresh requests here; the watch loop refreshes snapshots and replans from
	// SQLite current truth after the debounce window closes.
	dirtyBuf *DirtyBuffer

	// Observer references — set in startObservers, nil'd on shutdown.
	remoteObs *RemoteObserver
	localObs  *LocalObserver

	// Socket.IO wake source lifecycle, when enabled for full-drive watch.
	socketIOWakeStop chan struct{}
	socketIOWakeDone chan struct{}
}

type watchTimerState struct {
	// timerMu guards trialTimer and retryTimer pointers. Timer callbacks only
	// signal channels; they never mutate loop-owned state directly.
	timerMu sync.RWMutex

	// Trial and retry timers are armed by the watch loop. The channels stay
	// persistent so timer callbacks only signal the loop; they never mutate
	// loop-owned state directly.
	trialTimer syncTimer
	trialCh    chan struct{}

	// Retry timer — the watch loop releases due held retries on each tick.
	retryTimer   syncTimer
	retryTimerCh chan struct{} // persistent, buffered(1)
}

type watchSummaryState struct {
	// Deduplication: caches the last watch-condition signature and per-scope
	// shared-folder child-set signatures for watch summaries.
	lastSummarySignature string
	lastRemoteBlocked    map[ScopeKey]string
}

type watchRefreshState struct {
	// Full remote refresh is started by the watch loop and hands one loop-applied
	// remote observation batch back over refreshResults. The loop owns
	// refreshActive, durable apply, and dirty marking on receipt.
	refreshActive  bool
	refreshTimer   syncTimer
	refreshCh      chan time.Time
	refreshResults chan remoteRefreshResult

	// afterRefreshCommit is a test-only hook called after the watch loop has
	// durably applied a full-refresh batch but before it marks the resulting
	// dirty work. Nil in production.
	afterRefreshCommit func()
}

// watchRuntime owns all mutable watch-mode state. It is created by RunWatch
// and discarded when the watch session ends.
type watchRuntime struct {
	*engineFlow
	watchRuntimeState
	watchObservationState
	watchTimerState
	watchSummaryState
	watchRefreshState
}

type watchRuntimePhase string

const (
	watchRuntimePhaseBootstrap watchRuntimePhase = "bootstrap"
	watchRuntimePhaseRunning   watchRuntimePhase = "running"
	watchRuntimePhaseDraining  watchRuntimePhase = "draining"
)

func newWatchRuntime(engine *Engine) *watchRuntime {
	rt := &watchRuntime{
		engineFlow: newEngineFlow(engine),
		watchRuntimeState: watchRuntimeState{
			loop: watchLoopState{
				phase: watchRuntimePhaseRunning,
			},
		},
		watchTimerState: watchTimerState{
			trialCh:      make(chan struct{}, 1),
			retryTimerCh: make(chan struct{}, 1),
		},
		watchSummaryState: watchSummaryState{
			lastRemoteBlocked: make(map[ScopeKey]string),
		},
		watchRefreshState: watchRefreshState{
			refreshCh:      make(chan time.Time, 1),
			refreshResults: make(chan remoteRefreshResult, 1),
		},
	}
	rt.watch = rt

	return rt
}

func (rt *watchRuntime) phase() watchRuntimePhase {
	return rt.loop.phase
}

func (rt *watchRuntime) isDraining() bool {
	return rt.phase() == watchRuntimePhaseDraining
}

func (rt *watchRuntime) enterBootstrap() {
	rt.loop.phase = watchRuntimePhaseBootstrap
}

func (rt *watchRuntime) enterRunning() {
	rt.loop.phase = watchRuntimePhaseRunning
}

func (rt *watchRuntime) enterDraining() bool {
	if rt.loop.phase == watchRuntimePhaseDraining {
		return false
	}

	rt.loop.phase = watchRuntimePhaseDraining

	return true
}

func (rt *watchRuntime) currentOutbox() []*TrackedAction {
	return rt.loop.outbox
}

func (rt *watchRuntime) replaceOutbox(outbox []*TrackedAction) {
	if len(outbox) == 0 {
		rt.loop.outbox = nil
		return
	}

	rt.loop.outbox = append(rt.loop.outbox[:0], outbox...)
}

func (rt *watchRuntime) beginSyncStatusBatch(startedAt time.Time) {
	if rt.syncBatch.active {
		return
	}

	rt.syncBatch = watchSyncBatchState{
		active:        true,
		startedAt:     startedAt,
		succeededBase: rt.succeeded,
		failedBase:    rt.failed,
		errorBase:     len(rt.syncErrors),
	}
}

func (rt *watchRuntime) clearSyncStatusBatch() {
	rt.syncBatch = watchSyncBatchState{}
}

func (rt *watchRuntime) finishSyncStatusBatch(ctx context.Context, mode Mode) {
	if !rt.syncBatch.active {
		return
	}

	update := &SyncStatusUpdate{
		SyncedAt:  rt.engine.nowFunc(),
		Duration:  rt.engine.since(rt.syncBatch.startedAt),
		Succeeded: rt.succeeded - rt.syncBatch.succeededBase,
		Failed:    rt.failed - rt.syncBatch.failedBase,
	}
	if rt.syncBatch.errorBase < len(rt.syncErrors) {
		update.Errors = append(update.Errors, rt.syncErrors[rt.syncBatch.errorBase:]...)
	}

	rt.clearSyncStatusBatch()
	rt.engine.writeSyncStatusBestEffort(ctx, mode, false, update)
}

func (rt *watchRuntime) maybeFinishSyncStatusBatch(
	ctx context.Context,
	mode Mode,
	outbox []*TrackedAction,
) {
	if len(outbox) > 0 || rt.runningCount > 0 || rt.hasDueHeldWork(rt.engine.nowFunc()) {
		return
	}

	rt.finishSyncStatusBatch(ctx, mode)
}

func (rt *watchRuntime) canPrepareNow() bool {
	return len(rt.loop.outbox) == 0 && rt.runningCount == 0
}

func (rt *watchRuntime) queuePendingReplan(batch DirtyBatch) {
	rt.loop.replanQueued = true
	if batch.FullRefresh {
		rt.loop.pendingReplan.FullRefresh = true
	}
	if len(batch.Paths) == 0 {
		return
	}

	pathSet := make(map[string]struct{}, len(rt.loop.pendingReplan.Paths)+len(batch.Paths))
	for _, path := range rt.loop.pendingReplan.Paths {
		if path == "" {
			continue
		}
		pathSet[path] = struct{}{}
	}
	for _, path := range batch.Paths {
		if path == "" {
			continue
		}
		pathSet[path] = struct{}{}
	}

	paths := rt.loop.pendingReplan.Paths[:0]
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	rt.loop.pendingReplan.Paths = paths
	rt.loop.hasPendingReplan = rt.loop.pendingReplan.FullRefresh || len(paths) > 0
}

func (rt *watchRuntime) hasPendingReplan() bool {
	return rt.loop.replanQueued && rt.loop.hasPendingReplan
}

func (rt *watchRuntime) takePendingReplan() (DirtyBatch, bool) {
	if !rt.hasPendingReplan() {
		return DirtyBatch{}, false
	}

	batch := rt.loop.pendingReplan
	rt.loop.pendingReplan = DirtyBatch{}
	rt.loop.hasPendingReplan = false
	rt.loop.replanQueued = false

	return batch, true
}

func (rt *watchRuntime) consumeOutboxHead() {
	if len(rt.loop.outbox) == 0 {
		return
	}

	rt.loop.outbox = rt.loop.outbox[1:]
}

func (rt *watchRuntime) earliestTrialAt() (time.Time, bool) {
	var earliest time.Time
	found := false

	for _, scope := range rt.snapshotActiveScopes() {
		if len(rt.heldScopeOrder[scope.Key]) == 0 {
			continue
		}
		if !found || scope.NextTrialAt.Before(earliest) {
			earliest = scope.NextTrialAt
			found = true
		}
	}

	return earliest, found
}

func (rt *watchRuntime) resetTrialTimer(next syncTimer) {
	rt.timerMu.Lock()
	defer rt.timerMu.Unlock()

	if rt.trialTimer != nil {
		rt.trialTimer.Stop()
		rt.trialTimer = nil
	}

	rt.trialTimer = next
}

func (rt *watchRuntime) hasTrialTimer() bool {
	rt.timerMu.RLock()
	defer rt.timerMu.RUnlock()

	return rt.trialTimer != nil
}

func (rt *watchRuntime) resetRetryTimer(next syncTimer) {
	rt.timerMu.Lock()
	defer rt.timerMu.Unlock()

	if rt.retryTimer != nil {
		rt.retryTimer.Stop()
		rt.retryTimer = nil
	}

	rt.retryTimer = next
}

func (rt *watchRuntime) hasRetryTimer() bool {
	rt.timerMu.RLock()
	defer rt.timerMu.RUnlock()

	return rt.retryTimer != nil
}

func (rt *watchRuntime) resetRefreshTimer(next syncTimer) {
	rt.timerMu.Lock()
	defer rt.timerMu.Unlock()

	if rt.refreshTimer != nil {
		rt.refreshTimer.Stop()
		rt.refreshTimer = nil
	}

	rt.refreshTimer = next
}
