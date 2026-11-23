package sync

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

type watchLoopState struct {
	phase                         watchPhase
	outbox                        []*syncdomain.TrackedAction
	pendingReplan                 syncobserve.DirtyBatch
	hasPendingReplan              bool
	pendingReplanAt               time.Time
	pendingReplanDrainIdleAt      time.Time
	pendingReplanDrainIdleWorkers int
	postReplanOutbox              bool

	// Deduplication for the watch summaries: the set of condition kinds last
	// reported, the breakdown last written, and per-scope shared-folder
	// child-set signatures.
	lastSummarySignature string
	lastSummaryBreakdown string
	lastRemoteBlocked    map[syncdomain.ScopeKey]string
	// summaryReadFailing is set while the reads of the conditions for the
	// watch summary fail: the streak's first failure warns, its recovery is
	// noted once.
	summaryReadFailing bool

	// lastDroppedRescanErr is the error text last warned for a failed
	// dropped-event rescan; a repeat of the same failure goes to Debug
	// (R-6.6.30). Cleared when a rescan is scheduled.
	lastDroppedRescanErr string
	// lastBootstrapWait is the in-flight/running/held counts last logged
	// while bootstrap waited; an unchanged tick goes to Debug.
	lastBootstrapWait string

	// followUp tracks the work the installed plan left for the next pass
	// (PlanFollowUp). Watch replans on observed changes, and nothing is
	// observed when that work follows from the engine's own commits (a
	// re-key, a move made on OneDrive), so the loop replans by itself once
	// the plan's actions settle (R-2.1.7).
	followUp watchFollowUpState

	// createAttempts is when the creates this loop ran ended, by path, for
	// the drop of create intents after a remote poll (R-5.6.15).
	createAttempts watchCreateAttempts
}

// watchFollowUpState is the loop's record of a plan's follow-up work.
type watchFollowUpState struct {
	// pending: the installed plan left work for the next pass and its
	// actions have not all settled yet.
	pending bool
	// succeededAt and failedAt are the runtime's result counters when the
	// plan was installed. A plan that made no progress, or failed actions
	// (they wait in retry backoff), gets no follow-up replan.
	succeededAt int
	failedAt    int
	// queued: the loop marked the dirty buffer for a follow-up, so the next
	// replan is one; streak counts consecutive follow-up replans, bounded
	// like the passes of a one-shot sync.
	queued bool
	streak int
}

// watchTimerState has no lock: only the watch loop's goroutine arms, stops
// and reads the timers. Timer callbacks run on goroutines of their own and
// touch nothing but the channel they were handed when the timer was armed
// (and the debug event sink, which has its own lock).
type watchTimerState struct {
	// ownerCheck, when set, runs at the top of every timer accessor; nil in
	// production. Tests install a check that fails when a timer is armed or
	// read from a goroutine other than the loop's (installOwnerCheck).
	ownerCheck func()

	// Trial and retry timers are armed by the watch loop. The channels stay
	// persistent so timer callbacks only signal the loop; they never mutate
	// loop-owned state directly.
	trialTimer syncdomain.SyncTimer
	trialCh    chan struct{}

	// Retry timer — the watch loop releases due held retries on each tick.
	retryTimer   syncdomain.SyncTimer
	retryTimerCh chan struct{} // persistent, buffered(1)

	// Admit timer — wakes the loop when the earliest parked outbox entry
	// (TrackedAction.AdmitAt, a jittered ramp batch member) becomes due.
	// admitTimerAt is the instant the armed timer fires at, so a step that
	// finds the same parked head does not re-arm it.
	admitTimer   syncdomain.SyncTimer
	admitTimerAt time.Time
	admitCh      chan struct{} // persistent, buffered(1)
}

type watchResources struct {
	// Dirty debounce buffer. Local and remote observers mark coarse replan or
	// full-refresh requests here; the watch loop refreshes snapshots and replans
	// from SQLite current truth after the debounce window closes.
	dirtyBuf *syncobserve.DirtyBuffer

	// Observer lifecycle is runtime-owned. startObservers populates these fields
	// directly; the watch loop nils channels as sources close and tracks how many
	// observer goroutines still own the shared error stream.
	observerErrs        <-chan error
	localBatches        <-chan syncobserve.LocalObservation
	protectedRootEvents <-chan syncshortcut.ProtectedRootEvent
	remoteBatches       <-chan syncobserve.RemoteObservationBatch
	activeObservers     int

	// lastObserverErr is the most recent error an observer exited with while
	// the run went on, whatever its shape: a request that timed out, or a
	// cancel from below the engine, is kept like any other failure, and an
	// exit once the run has ended, the stop, is not kept. When the last
	// observer exits it becomes the cause of the returned watch error, so the
	// user reads why watching stopped. Owned by the watch loop goroutine.
	lastObserverErr error

	// Observer references — set in startObservers, nil'd on shutdown.
	remoteObs *syncobserve.RemoteObserver
	localObs  *syncobserve.LocalObserver

	// goroutines owns the observers, the full remote refresh and the
	// socket.io wake source: the pipeline's cleanup cancels them and waits
	// for them before the watch returns.
	goroutines watchGoroutines

	// Full remote refresh is started by the watch loop and hands one loop-applied
	// remote observation batch back over refreshResults. These channels stay
	// stable for the runtime lifetime because timer callbacks and refresh
	// goroutines send through them asynchronously; the loop disables select cases
	// by phase instead of racing those senders by niling channel fields.
	refreshActive  bool
	refreshTimer   syncdomain.SyncTimer
	refreshCh      chan time.Time
	refreshResults chan syncobserve.RemoteObservationBatch

	// heldDeleteConfirms is the engine's confirmation channel
	// (Engine.ConfirmHeldDeletes), read while running.
	heldDeleteConfirms <-chan heldDeleteConfirmRequest
}

// watchRuntime owns all mutable watch-mode state. It is created by RunWatch
// and discarded when the watch session ends.
// The three state groups are named fields rather than embedded ones. Embedding
// grouped the declarations but promoted every field back to the top level, so
// any method could still touch anything and the grouping was documentation
// rather than structure. Qualified access makes each method's reach visible at
// its call sites, which is the input any further decomposition needs.
type watchRuntime struct {
	*engineFlow
	loop      watchLoopState
	resources watchResources
	timers    watchTimerState
}

func newWatchRuntime(engine *Engine) *watchRuntime {
	rt := &watchRuntime{
		engineFlow: newEngineFlow(engine.flowInputs()),
		loop: watchLoopState{
			phase:             watchPhaseStarting,
			lastRemoteBlocked: make(map[syncdomain.ScopeKey]string),
		},
		timers: watchTimerState{
			ownerCheck:   engine.runOwnerCheck,
			trialCh:      make(chan struct{}, 1),
			retryTimerCh: make(chan struct{}, 1),
			admitCh:      make(chan struct{}, 1),
		},
		resources: watchResources{
			refreshCh:          make(chan time.Time, 1),
			refreshResults:     make(chan syncobserve.RemoteObservationBatch, 1),
			heldDeleteConfirms: engine.heldDeleteConfirms,
		},
	}

	// The watch runtime is its own signal sink and inspector: it is the live
	// future that completion and scope decisions are asking about.
	rt.signals = rt
	rt.inspect = rt
	rt.admitted = rt

	return rt
}

func (rt *watchRuntime) phase() watchPhase {
	return rt.loop.phase
}

// isDraining reports whether admission is sealed: the drain is running or
// has ended.
func (rt *watchRuntime) isDraining() bool {
	phase := rt.phase()
	return phase == watchPhaseDraining || phase == watchPhaseStopped
}

// beginObserverBackedRunning moves a settled bootstrap to running. During a
// drain it refuses with errWatchObserversDuringDrain, which RunWatch reads as
// the stop it asked for.
func (rt *watchRuntime) beginObserverBackedRunning() error {
	if rt.isDraining() {
		return errWatchObserversDuringDrain
	}
	if rt.resources.activeObservers != 0 {
		return fmt.Errorf("sync: start watch observers while %d observers are already active", rt.resources.activeObservers)
	}

	return rt.transition(watchEventObserversStarted)
}

// enterDraining seals admission, reporting whether this call began the drain.
func (rt *watchRuntime) enterDraining() bool {
	if rt.isDraining() {
		return false
	}

	// Every phase but the two draining ones moves to draining on this event,
	// so a refusal is a bug in the transition table.
	if err := rt.transition(watchEventShutdownRequested); err != nil {
		rt.deps.logger.Error("watch lifecycle refused the shutdown", slog.Any("error", err))
	}

	return true
}

func (rt *watchRuntime) currentOutbox() []*syncdomain.TrackedAction {
	return rt.loop.outbox
}

func (rt *watchRuntime) replaceOutbox(outbox []*syncdomain.TrackedAction) {
	if len(outbox) == 0 {
		rt.loop.outbox = nil
		return
	}

	rt.loop.outbox = append(rt.loop.outbox[:0], outbox...)
}

func (rt *watchRuntime) canPrepareNow() bool {
	return len(rt.loop.outbox) == 0 && rt.sched.RunningCount() == 0
}

func (rt *watchRuntime) queuePendingReplan(batch syncobserve.DirtyBatch) {
	wasPending := rt.loop.hasPendingReplan
	rt.loop.hasPendingReplan = true
	if !wasPending {
		rt.loop.pendingReplanAt = rt.deps.now()
		rt.startPendingReplanDrainIdleTracking(rt.loop.pendingReplanAt)
		rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventPendingReplanSet, "", 0, time.Time{})
	}
	if batch.FullRefresh {
		rt.loop.pendingReplan.FullRefresh = true
	}
	rt.retireOutboxForPendingReplan()
}

func (rt *watchRuntime) hasPendingReplan() bool {
	return rt.loop.hasPendingReplan
}

func (rt *watchRuntime) takePendingReplan() (syncobserve.DirtyBatch, bool) {
	if !rt.hasPendingReplan() {
		return syncobserve.DirtyBatch{}, false
	}

	batch := rt.loop.pendingReplan
	rt.loop.pendingReplan = syncobserve.DirtyBatch{}
	rt.loop.hasPendingReplan = false
	rt.loop.pendingReplanAt = time.Time{}
	rt.loop.pendingReplanDrainIdleAt = time.Time{}
	rt.loop.pendingReplanDrainIdleWorkers = 0

	return batch, true
}

func (rt *watchRuntime) pendingReplanStartedAt() time.Time {
	return rt.loop.pendingReplanAt
}

// outboxEntry returns outbox entry i, or nil for the "nothing to dispatch"
// index.
func (rt *watchRuntime) outboxEntry(i int) *syncdomain.TrackedAction {
	if i < 0 || i >= len(rt.loop.outbox) {
		return nil
	}

	return rt.loop.outbox[i]
}

func (rt *watchRuntime) retireOutboxForPendingReplan() {
	outbox := rt.currentOutbox()
	if len(outbox) == 0 {
		if rt.sched.RunningCount() > 0 {
			rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventWaitingForRunningActions, "", 0, rt.pendingReplanStartedAt())
		}
		return
	}

	rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventDispatchPausedForReplan, "", len(outbox), rt.pendingReplanStartedAt())
	for _, ta := range outbox {
		// Retired old-runtime nodes stay unresolved; the replacement runtime
		// rebuilds dependency state from current truth.
		rt.sched.Finished(ta)
	}
	rt.replaceOutbox(nil)
	rt.deps.perf.RecordSuperseded(perf.SupersededSourcePendingReplanRetirement, len(outbox))
	rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventOldOutboxRetired, "", len(outbox), rt.pendingReplanStartedAt())
	if rt.sched.RunningCount() > 0 {
		rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventWaitingForRunningActions, "", 0, rt.pendingReplanStartedAt())
	}
}

// takeAdmittedWorkBlockedBy satisfies admittedWorkOwner: a scope that
// activates takes the admitted work it blocks back out of the outbox before
// a worker reaches it (R-6.8.24). Loop goroutine only, like every other
// outbox mutation.
func (rt *watchRuntime) takeAdmittedWorkBlockedBy(key syncdomain.ScopeKey) []*syncdomain.TrackedAction {
	taken, kept := takeOutboxBlockedBy(rt.currentOutbox(), key)
	if len(taken) == 0 {
		return nil
	}
	rt.replaceOutbox(kept)

	return taken
}

func (rt *watchRuntime) retireReadyFrontierForPendingReplan(ready []*syncdomain.TrackedAction) {
	if len(ready) == 0 {
		return
	}
	for _, ta := range ready {
		rt.sched.Finished(ta)
	}
	rt.deps.perf.RecordSuperseded(perf.SupersededSourcePendingReplanRetirement, len(ready))
	rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventOldOutboxRetired, "released_ready_frontier", len(ready), rt.pendingReplanStartedAt())
}

func (rt *watchRuntime) recordReplanWorkerIdle(phase perf.ReplanIdlePhase, start time.Time, idleWorkers int) {
	if rt == nil || start.IsZero() {
		return
	}

	rt.deps.perf.RecordReplanWorkerIdle(phase, idleWorkers, rt.deps.since(start))
}

func (rt *watchRuntime) startPendingReplanDrainIdleTracking(now time.Time) {
	if rt.sched.RunningCount() == 0 || now.IsZero() {
		return
	}

	rt.loop.pendingReplanDrainIdleAt = now
	rt.loop.pendingReplanDrainIdleWorkers = rt.idleWorkers()
}

func (rt *watchRuntime) advancePendingReplanDrainIdleTracking() {
	if !rt.hasPendingReplan() || rt.loop.pendingReplanDrainIdleAt.IsZero() {
		return
	}

	now := rt.deps.now()
	rt.deps.perf.RecordReplanWorkerIdle(
		perf.ReplanIdlePhaseWaitingDrain,
		rt.loop.pendingReplanDrainIdleWorkers,
		now.Sub(rt.loop.pendingReplanDrainIdleAt),
	)
	rt.loop.pendingReplanDrainIdleAt = now
	rt.loop.pendingReplanDrainIdleWorkers = rt.idleWorkers()
}

func (rt *watchRuntime) rescheduleReplanIntent(batch syncobserve.DirtyBatch) error {
	if rt.resources.dirtyBuf == nil {
		return fmt.Errorf("sync: reschedule watch replan: dirty buffer not initialized")
	}
	if batch.FullRefresh {
		rt.resources.dirtyBuf.MarkFullRefresh()
		return nil
	}
	rt.resources.dirtyBuf.MarkDirty()
	return nil
}

// totalWorkers is how many workers the run's pools run, the count the pool
// itself uses (workerPoolSize), so the idle count the replan reports matches
// the workers there are.
func (rt *watchRuntime) totalWorkers() int {
	if rt == nil {
		return 0
	}
	return rt.cfg.poolSize
}

func (rt *watchRuntime) idleWorkers() int {
	total := rt.totalWorkers()
	if total <= rt.sched.RunningCount() {
		return 0
	}
	return total - rt.sched.RunningCount()
}

func (rt *watchRuntime) emitRuntimeDebugEvent(
	eventType syncdomain.EngineDebugEventType,
	note string,
	count int,
	start time.Time,
) {
	event := syncdomain.EngineDebugEvent{
		Type:        eventType,
		Note:        note,
		Count:       count,
		Outbox:      len(rt.currentOutbox()),
		Running:     rt.sched.RunningCount(),
		IdleWorkers: rt.idleWorkers(),
	}
	if !start.IsZero() {
		event.Delay = rt.deps.since(start)
	}
	rt.deps.emit(event)
}

func (rt *watchRuntime) earliestTrialAt() (time.Time, bool) {
	var earliest time.Time
	found := false

	for _, scope := range rt.sched.Scopes() {
		if rt.sched.HeldUnderScope(scope.Key) == 0 {
			continue
		}
		if !found || scope.NextTrialAt.Before(earliest) {
			earliest = scope.NextTrialAt
			found = true
		}
	}

	return earliest, found
}

// checkOwner runs the timers' owner check, if a test installed one.
func (t *watchTimerState) checkOwner() {
	if t.ownerCheck != nil {
		t.ownerCheck()
	}
}

// signalTimerChannel is what a timer callback does: a non-blocking send on
// the buffered(1) channel it was handed. A signal already pending absorbs
// this one; the loop handles everything due when it takes the signal.
func signalTimerChannel(ch chan<- struct{}) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (rt *watchRuntime) resetTrialTimer(next syncdomain.SyncTimer) {
	rt.timers.checkOwner()

	if rt.timers.trialTimer != nil {
		rt.timers.trialTimer.Stop()
		rt.timers.trialTimer = nil
	}

	rt.timers.trialTimer = next
}

func (rt *watchRuntime) hasTrialTimer() bool {
	rt.timers.checkOwner()

	return rt.timers.trialTimer != nil
}

func (rt *watchRuntime) resetRetryTimer(next syncdomain.SyncTimer) {
	rt.timers.checkOwner()

	if rt.timers.retryTimer != nil {
		rt.timers.retryTimer.Stop()
		rt.timers.retryTimer = nil
	}

	rt.timers.retryTimer = next
}

func (rt *watchRuntime) hasRetryTimer() bool {
	rt.timers.checkOwner()

	return rt.timers.retryTimer != nil
}

// armAdmitTimer wakes the loop at `at`, when the earliest parked outbox
// entry becomes due; the zero time stops the timer. A timer already armed
// for that instant is kept.
func (rt *watchRuntime) armAdmitTimer(at time.Time) {
	rt.timers.checkOwner()

	if rt.timers.admitTimer != nil && rt.timers.admitTimerAt.Equal(at) {
		return
	}
	if rt.timers.admitTimer != nil {
		rt.timers.admitTimer.Stop()
		rt.timers.admitTimer = nil
	}
	rt.timers.admitTimerAt = at
	if at.IsZero() {
		return
	}
	admitCh := rt.timers.admitCh
	rt.timers.admitTimer = rt.deps.clock.AfterFunc(at.Sub(rt.deps.now()), func() {
		signalTimerChannel(admitCh)
	})
}

func (rt *watchRuntime) stopAdmitTimer() {
	rt.armAdmitTimer(time.Time{})
}

func (rt *watchRuntime) hasAdmitTimer() bool {
	rt.timers.checkOwner()

	return rt.timers.admitTimer != nil
}

// admitTimerChan is the loop's wake-up for a parked outbox entry that has
// become due; the step that follows re-evaluates dispatch.
func (rt *watchRuntime) admitTimerChan() <-chan struct{} {
	return rt.timers.admitCh
}

func (rt *watchRuntime) resetRefreshTimer(next syncdomain.SyncTimer) {
	rt.timers.checkOwner()

	if rt.resources.refreshTimer != nil {
		rt.resources.refreshTimer.Stop()
		rt.resources.refreshTimer = nil
	}

	rt.resources.refreshTimer = next
}

// kickDueHeldRelease satisfies runtimeSignals; the watch loop releases
// due held retry work immediately rather than waiting for the next tick.
func (rt *watchRuntime) kickDueHeldRelease() {
	rt.kickRetryHeldReleaseNow()
}

// markReplanNeeded satisfies runtimeSignals: committed truth changed under the
// current plan, so the debounce buffer should schedule a replan.
func (rt *watchRuntime) markReplanNeeded() {
	if rt.resources.dirtyBuf == nil {
		return
	}

	rt.resources.dirtyBuf.MarkDirty()
}

// hasActiveRefresh satisfies watchInspector.
func (rt *watchRuntime) hasActiveRefresh() bool {
	return rt.resources.refreshActive
}

// noteInstalledPlan records whether a newly installed plan leaves work for
// the next pass. A replan the loop queued itself extends the follow-up
// streak; any other replan starts a new one.
func (rt *watchRuntime) noteInstalledPlan(plan *syncdomain.ActionPlan) {
	f := &rt.loop.followUp
	if f.queued {
		f.streak++
	} else {
		f.streak = 0
	}
	f.queued = false
	f.pending = plan != nil && len(plan.Actions) > 0 && plan.FollowUp.Pending()
	f.succeededAt = rt.results.succeeded
	f.failedAt = rt.results.failed
}

// scheduleFollowUpIfSettled replans once the installed plan's actions have
// all settled, when that plan left work for the next pass, made progress and
// failed nothing.
func (rt *watchRuntime) scheduleFollowUpIfSettled() {
	f := &rt.loop.followUp
	if !f.pending || len(rt.currentOutbox()) > 0 || rt.sched.RunningCount() > 0 || rt.resources.dirtyBuf == nil {
		return
	}
	f.pending = false
	if rt.results.succeeded == f.succeededAt || rt.results.failed != f.failedAt {
		return
	}
	if f.streak+1 >= oneShotMaxPasses {
		rt.deps.logger.Debug("follow-up replan limit reached; the next observed change replans the remaining work",
			slog.Int("follow_up_replans", f.streak),
		)
		return
	}

	rt.deps.logger.Debug("replanning for work the previous plan left for the next pass")
	f.queued = true
	rt.resources.dirtyBuf.MarkDirty()
}
