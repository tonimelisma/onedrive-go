package sync

import (
	"context"
	"sync"
	"time"
)

type watchLoopState struct {
	phase            watchRuntimePhase
	outbox           []*TrackedAction
	pendingReplan    DirtyBatch
	hasPendingReplan bool

	// syncBatch tracks the current best-effort watch batch so status updates are
	// written only after the loop has exhausted all currently admissible work and
	// returned to quiescence.
	syncBatch watchSyncBatchState

	// Deduplication: caches the last watch-condition signature and per-scope
	// shared-folder child-set signatures for watch summaries.
	lastSummarySignature string
	lastRemoteBlocked    map[ScopeKey]string
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

type watchSyncBatchState struct {
	active        bool
	startedAt     time.Time
	succeededBase int
	failedBase    int
	errorBase     int
}

type watchResources struct {
	// Dirty debounce buffer. Local and remote observers mark coarse replan or
	// full-refresh requests here; the watch loop refreshes snapshots and replans
	// from SQLite current truth after the debounce window closes.
	dirtyBuf *DirtyBuffer

	// Observer references — set in startObservers, nil'd on shutdown.
	remoteObs *RemoteObserver
	localObs  *LocalObserver

	// Socket.IO wake source lifecycle, when enabled for full-drive watch.
	socketIOWakeStop chan struct{}
	socketIOWakeDone chan struct{}

	// Full remote refresh is started by the watch loop and hands one loop-applied
	// remote observation batch back over refreshResults. The loop owns
	// refreshActive, durable apply, and dirty marking on receipt.
	refreshActive  bool
	refreshTimer   syncTimer
	refreshCh      chan time.Time
	refreshResults chan remoteRefreshResult
}

// watchRuntime owns all mutable watch-mode state. It is created by RunWatch
// and discarded when the watch session ends.
type watchRuntime struct {
	*engineFlow
	loop watchLoopState
	watchResources
	watchTimerState
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
		loop: watchLoopState{
			phase:             watchRuntimePhaseRunning,
			lastRemoteBlocked: make(map[ScopeKey]string),
		},
		watchTimerState: watchTimerState{
			trialCh:      make(chan struct{}, 1),
			retryTimerCh: make(chan struct{}, 1),
		},
		watchResources: watchResources{
			refreshCh:      make(chan time.Time, 1),
			refreshResults: make(chan remoteRefreshResult, 1),
		},
	}

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
	if rt.loop.syncBatch.active {
		return
	}

	rt.loop.syncBatch = watchSyncBatchState{
		active:        true,
		startedAt:     startedAt,
		succeededBase: rt.succeeded,
		failedBase:    rt.failed,
		errorBase:     len(rt.syncErrors),
	}
}

func (rt *watchRuntime) clearSyncStatusBatch() {
	rt.loop.syncBatch = watchSyncBatchState{}
}

func (rt *watchRuntime) finishSyncStatusBatch(ctx context.Context, mode Mode) {
	if !rt.loop.syncBatch.active {
		return
	}

	update := &SyncStatusUpdate{
		SyncedAt:  rt.engine.nowFunc(),
		Duration:  rt.engine.since(rt.loop.syncBatch.startedAt),
		Succeeded: rt.succeeded - rt.loop.syncBatch.succeededBase,
		Failed:    rt.failed - rt.loop.syncBatch.failedBase,
	}
	if rt.loop.syncBatch.errorBase < len(rt.syncErrors) {
		update.Errors = append(update.Errors, rt.syncErrors[rt.loop.syncBatch.errorBase:]...)
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
	rt.loop.hasPendingReplan = true
	if batch.FullRefresh {
		rt.loop.pendingReplan.FullRefresh = true
	}
}

func (rt *watchRuntime) hasPendingReplan() bool {
	return rt.loop.hasPendingReplan
}

func (rt *watchRuntime) takePendingReplan() (DirtyBatch, bool) {
	if !rt.hasPendingReplan() {
		return DirtyBatch{}, false
	}

	batch := rt.loop.pendingReplan
	rt.loop.pendingReplan = DirtyBatch{}
	rt.loop.hasPendingReplan = false

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
