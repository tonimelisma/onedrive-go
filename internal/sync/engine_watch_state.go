package sync

import (
	"sync"
	"time"
)

type watchLoopState struct {
	phase            watchRuntimePhase
	outbox           []*TrackedAction
	pendingReplan    dirtyBatch
	hasPendingReplan bool
	pendingReplanAt  time.Time
	postReplanOutbox bool

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

type watchResources struct {
	// Dirty debounce buffer. Local and remote observers mark coarse replan or
	// full-refresh requests here; the watch loop refreshes snapshots and replans
	// from SQLite current truth after the debounce window closes.
	dirtyBuf *DirtyBuffer

	// Observer lifecycle is runtime-owned. startObservers populates these fields
	// directly; the watch loop nils channels as sources close and tracks how many
	// observer goroutines still own the shared error stream.
	observerErrs        <-chan error
	localEvents         <-chan ChangeEvent
	protectedRootEvents <-chan ProtectedRootEvent
	remoteBatches       <-chan remoteObservationBatch
	skippedItems        <-chan []SkippedItem
	activeObservers     int

	// Observer references — set in startObservers, nil'd on shutdown.
	remoteObs *RemoteObserver
	localObs  *LocalObserver

	// Socket.IO wake source lifecycle, when enabled for full-drive watch.
	socketIOWakeStop chan struct{}
	socketIOWakeDone chan struct{}

	// Full remote refresh is started by the watch loop and hands one loop-applied
	// remote observation batch back over refreshResults. These channels stay
	// stable for the runtime lifetime because timer callbacks and refresh
	// goroutines send through them asynchronously; the loop disables select cases
	// by phase instead of racing those senders by niling channel fields.
	refreshActive  bool
	refreshTimer   syncTimer
	refreshCh      chan time.Time
	refreshResults chan remoteObservationBatch
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
			refreshResults: make(chan remoteObservationBatch, 1),
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

func (rt *watchRuntime) canPrepareNow() bool {
	return len(rt.loop.outbox) == 0 && rt.runningCount == 0
}

func (rt *watchRuntime) queuePendingReplan(batch dirtyBatch) {
	wasPending := rt.loop.hasPendingReplan
	rt.loop.hasPendingReplan = true
	if !wasPending {
		rt.loop.pendingReplanAt = rt.engine.nowFunc()
		rt.emitRuntimeDebugEvent(engineDebugEventPendingReplanSet, "", 0, time.Time{})
	}
	if batch.FullRefresh {
		rt.loop.pendingReplan.FullRefresh = true
	}
	rt.retireOutboxForPendingReplan()
}

func (rt *watchRuntime) hasPendingReplan() bool {
	return rt.loop.hasPendingReplan
}

func (rt *watchRuntime) takePendingReplan() (dirtyBatch, bool) {
	if !rt.hasPendingReplan() {
		return dirtyBatch{}, false
	}

	batch := rt.loop.pendingReplan
	rt.loop.pendingReplan = dirtyBatch{}
	rt.loop.hasPendingReplan = false
	rt.loop.pendingReplanAt = time.Time{}

	return batch, true
}

func (rt *watchRuntime) pendingReplanStartedAt() time.Time {
	return rt.loop.pendingReplanAt
}

func (rt *watchRuntime) consumeOutboxHead() {
	if len(rt.loop.outbox) == 0 {
		return
	}

	rt.loop.outbox = rt.loop.outbox[1:]
}

func (rt *watchRuntime) retireOutboxForPendingReplan() {
	outbox := rt.currentOutbox()
	if len(outbox) == 0 {
		if rt.runningCount > 0 {
			rt.emitRuntimeDebugEvent(engineDebugEventWaitingForRunningActions, "", 0, rt.pendingReplanStartedAt())
		}
		return
	}

	rt.emitRuntimeDebugEvent(engineDebugEventDispatchPausedForReplan, "", len(outbox), rt.pendingReplanStartedAt())
	for _, ta := range outbox {
		rt.markFinished(ta)
	}
	rt.replaceOutbox(nil)
	rt.emitRuntimeDebugEvent(engineDebugEventOldOutboxRetired, "", len(outbox), rt.pendingReplanStartedAt())
	if rt.runningCount > 0 {
		rt.emitRuntimeDebugEvent(engineDebugEventWaitingForRunningActions, "", 0, rt.pendingReplanStartedAt())
	}
}

func (rt *watchRuntime) retireReadyFrontierForPendingReplan(ready []*TrackedAction) {
	if len(ready) == 0 {
		return
	}
	for _, ta := range ready {
		rt.markFinished(ta)
	}
	rt.emitRuntimeDebugEvent(engineDebugEventOldOutboxRetired, "released_ready_frontier", len(ready), rt.pendingReplanStartedAt())
}

func (rt *watchRuntime) totalWorkers() int {
	if rt == nil || rt.engine == nil || rt.engine.transferWorkers < 1 {
		return 0
	}
	return rt.engine.transferWorkers
}

func (rt *watchRuntime) idleWorkers() int {
	total := rt.totalWorkers()
	if total <= rt.runningCount {
		return 0
	}
	return total - rt.runningCount
}

func (rt *watchRuntime) emitRuntimeDebugEvent(
	eventType engineDebugEventType,
	note string,
	count int,
	start time.Time,
) {
	event := engineDebugEvent{
		Type:        eventType,
		Note:        note,
		Count:       count,
		Outbox:      len(rt.currentOutbox()),
		Running:     rt.runningCount,
		IdleWorkers: rt.idleWorkers(),
	}
	if !start.IsZero() {
		event.Delay = rt.engine.since(start)
	}
	rt.engine.emitDebugEvent(event)
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
