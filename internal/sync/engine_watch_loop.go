package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
)

// runWatchUntilQuiescent drives the bootstrap watch loop until all work due
// now has drained through the shared runtime. Future-held retry/scope work may
// remain unresolved in the graph, so bootstrap quiescence is engine-owned
// rather than defined by graph emptiness. It ends in Ready when bootstrap
// settled and in Stopped when a stop overtook it.
func (rt *watchRuntime) runWatchUntilQuiescent(
	ctx context.Context,
	p *watchPipeline,
	initialOutbox []*syncdomain.TrackedAction,
) error {
	ticker := rt.deps.clock.NewTicker(quiescenceLogInterval)
	defer syncdomain.StopTicker(ticker)

	if err := rt.transition(watchEventBootstrapPlanned); err != nil {
		return err
	}
	rt.replaceOutbox(initialOutbox)

	return rt.runLoopSteps(ctx, p, syncdomain.TickerChan(ticker), true)
}

// runWatchLoop owns steady-state watch execution. The same goroutine handles
// observed replans, action completions, retry/trial timers, reconcile
// completions, and outbox draining.
func (rt *watchRuntime) runWatchLoop(
	ctx context.Context,
	p *watchPipeline,
) error {
	rt.replaceOutbox(nil)

	return rt.runLoopSteps(ctx, p, nil, false)
}

// runLoopSteps runs steps until one reports done. Bootstrap and steady state
// share the loop but end for different reasons: bootstrap may end in Ready
// (settled) or Stopped (drained); steady state only in Stopped. bootstrap
// says which of the two this loop is. A loop that
// reports completion in any other phase has stopped watching while its caller
// still believes the mount is live, which is silent data staleness rather
// than a clean shutdown, so it is reported instead of returning the nil that
// hides it.
func (rt *watchRuntime) runLoopSteps(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
	bootstrap bool,
) error {
	for {
		done, err := rt.runWatchLoopStep(ctx, p, logC)
		if err != nil {
			if rt.drainAfterCanceledStepError(ctx, p, err) {
				continue
			}

			return err
		}
		if done {
			if phase := rt.phase(); phase != watchPhaseStopped && (!bootstrap || phase != watchPhaseReady) {
				// Deliberately reports the context as a bool rather than
				// wrapping ctx.Err(): wrapping would make this error satisfy
				// isWatchShutdownError, and RunWatch would translate the very
				// report of a missing drain into the clean shutdown it denies.
				return fmt.Errorf(
					"sync: watch loop completed in phase %q without draining (context done: %t)",
					phase, ctx.Err() != nil)
			}

			return nil
		}
	}
}

// drainAfterCanceledStepError converts a step that failed because the caller
// canceled into a drain, reporting whether the loop should continue.
//
// Each step begins by draining if the context is already done, so a
// cancellation that lands between steps is handled. One that lands *inside* a
// step is not: the handler's own I/O fails with the context error, the step
// returns it, and returning that error leaves through a path that never
// drained. RunWatch then classifies the cancellation as a clean stop and
// returns nil, so the shutdown looks complete while the retry and trial timers
// it was supposed to stop are still armed.
//
// Draining and continuing hands the loop to the drain step, which is the
// observable shutdown boundary the design already promises for
// cancellation-closed intake. A second failure while draining is returned
// rather than converted, so this cannot spin.
func (rt *watchRuntime) drainAfterCanceledStepError(
	ctx context.Context,
	p *watchPipeline,
	err error,
) bool {
	if err == nil || ctx.Err() == nil || rt.isDraining() {
		return false
	}

	rt.beginWatchDrain(ctx, p)

	return true
}

func (rt *watchRuntime) runWatchLoopStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	rt.beginWatchDrainIfCanceled(ctx, p)

	switch rt.phase() {
	case watchPhaseDraining:
		return rt.runDrainStep(ctx, p)
	case watchPhaseStopped:
		return true, nil
	case watchPhaseBootstrap:
		return rt.runBootstrapLoopStep(ctx, p, logC)
	case watchPhaseRunning:
		return rt.runRunningLoopStep(ctx, p)
	case watchPhaseReady:
		// A settled bootstrap whose caller kept stepping: the bootstrap loop
		// is over, and steady state begins only once observers start.
		return true, nil
	case watchPhaseStarting, watchPhaseCount:
	}

	return false, fmt.Errorf("sync: no watch loop step in phase %q", rt.phase())
}

func (rt *watchRuntime) beginWatchDrainIfCanceled(ctx context.Context, p *watchPipeline) {
	_ = rt.beginWatchDrainIfContextCanceled(ctx, p)
}

func (rt *watchRuntime) beginWatchDrainIfContextCanceled(ctx context.Context, p *watchPipeline) bool {
	if ctx.Err() != nil && !rt.isDraining() {
		rt.beginWatchDrain(ctx, p)
		return true
	}

	return ctx.Err() != nil
}

func (rt *watchRuntime) runBootstrapLoopStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	if rt.isBootstrapQuiescent() {
		return true, rt.transition(watchEventBootstrapSettled)
	}

	return rt.runNonDrainingWatchStep(ctx, p, logC)
}

func (rt *watchRuntime) runRunningLoopStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	replanned, err := rt.runPendingWatchReplan(ctx, p)
	if err != nil {
		return false, err
	}
	if replanned {
		return false, nil
	}

	return rt.runNonDrainingWatchStep(ctx, p, nil)
}

func (rt *watchRuntime) runPendingWatchReplan(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	if !rt.canPrepareNow() {
		return false, nil
	}

	pendingStartedAt := rt.pendingReplanStartedAt()
	rt.advancePendingReplanDrainIdleTracking()
	batch, ok := rt.takePendingReplan()
	if !ok {
		return false, nil
	}
	rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventRunningActionsDrained, "", 0, pendingStartedAt)

	applied, err := rt.runSteadyStateReplan(ctx, p, batch)
	if err != nil {
		return true, err
	}
	if !applied {
		return true, rt.rescheduleReplanIntent(batch)
	}

	return true, nil
}

//nolint:gocyclo // The watch select owns one event fan-in point; splitting cases would obscure ownership.
func (rt *watchRuntime) runNonDrainingWatchStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	if rt.beginWatchDrainIfContextCanceled(ctx, p) {
		return false, nil
	}

	select {
	case batch, ok := <-p.replanReady:
		if rt.beginWatchDrainIfContextCanceled(ctx, p) {
			return false, nil
		}
		return rt.handleWatchReplanSignal(ctx, p, batch, ok)
	default:
	}

	// A finished outcome may change what the next dispatch is allowed to be
	// (a 429 activates a scope that takes the outbox back, R-6.8.24), so
	// outcomes already in hand are applied before more work reaches the
	// workers; otherwise an episode's 429s scale with the select's luck
	// rather than with the transfers in flight.
	select {
	case completion, ok := <-p.completions:
		return rt.handleWatchCompletionSignal(ctx, p, &completion, ok)
	default:
	}

	dispatchCh, next := rt.dispatchChannelForOutbox()
	phase := rt.phase()
	observe := watchInputOn(phase, watchInputObservations)

	select {
	case dispatchCh <- rt.outboxEntry(next):
		rt.handleWatchDispatch(next)
		return false, nil
	case batch, ok := <-p.replanReady:
		if rt.beginWatchDrainIfContextCanceled(ctx, p) {
			return false, nil
		}
		return rt.handleWatchReplanSignal(ctx, p, batch, ok)
	case completion, ok := <-p.completions:
		return rt.handleWatchCompletionSignal(ctx, p, &completion, ok)
	case batch, ok := <-recvIf(observe, rt.resources.localBatches):
		return rt.handleWatchLocalObservationBatchSignal(ctx, &batch, ok)
	case event, ok := <-recvIf(observe, rt.resources.protectedRootEvents):
		return rt.handleWatchProtectedRootEventSignal(ctx, &event, ok)
	case batch, ok := <-recvIf(observe, rt.resources.remoteBatches):
		return rt.handleWatchRemoteBatchSignal(ctx, &batch, ok)
	case <-recvIf(watchInputOn(phase, watchInputRefreshTick), rt.resources.refreshCh):
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return false, nil
	case result, ok := <-rt.resources.refreshResults:
		return rt.handleWatchRefreshResultSignal(ctx, &result, ok)
	case <-p.maintenanceC:
		rt.handleMaintenanceTick(ctx)
		return false, nil
	case observerErr, ok := <-recvIf(watchInputOn(phase, watchInputObserverErrs), rt.resources.observerErrs):
		return rt.handleWatchObserverErrorSignal(ctx, p, observerErr, ok)
	case <-rt.trialTimerChan():
		return false, rt.handleWatchHeldReleaseSignal(ctx, p, true)
	case <-rt.retryTimerChan():
		return false, rt.handleWatchHeldReleaseSignal(ctx, p, false)
	case <-rt.admitTimerChan():
		// A parked outbox entry is due; the next step dispatches it.
		return false, nil
	case <-recvIf(watchInputOn(phase, watchInputBootstrapLog), logC):
		rt.logBootstrapWait(ctx)
		return false, nil
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}
}

func (rt *watchRuntime) handleWatchDispatch(next int) {
	postReplanDispatch := rt.loop.postReplanOutbox
	rt.loop.postReplanOutbox = false
	rt.loop.createAttempts.dispatched(rt.outboxEntry(next))
	rt.loop.outbox = rt.markDispatched(rt.loop.outbox, next)
	if postReplanDispatch {
		rt.emitRuntimeDebugEvent(syncdomain.EngineDebugEventFirstPostReplanDispatch, "", 0, time.Time{})
	}
}

func (rt *watchRuntime) handleWatchReplanSignal(
	ctx context.Context,
	p *watchPipeline,
	batch syncobserve.DirtyBatch,
	ok bool,
) (bool, error) {
	if !ok {
		if rt.beginWatchDrainIfContextCanceled(ctx, p) {
			return false, nil
		}

		return false, fmt.Errorf("sync: dirty scheduler stopped unexpectedly")
	}

	return false, rt.handleWatchReplanReady(ctx, p, batch)
}

func (rt *watchRuntime) handleWatchCompletionSignal(
	ctx context.Context,
	p *watchPipeline,
	completion *syncdomain.ActionCompletion,
	ok bool,
) (bool, error) {
	if !ok {
		return false, rt.handleWatchCompletionsClosed(ctx, p)
	}

	return false, rt.handleWatchActionCompletion(ctx, p, completion)
}

func (rt *watchRuntime) handleWatchLocalObservationBatchSignal(
	ctx context.Context,
	batch *syncobserve.LocalObservation,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.localBatches = nil
		return false, nil
	}

	return false, rt.handleWatchLocalObservationBatch(ctx, batch)
}

func (rt *watchRuntime) handleWatchProtectedRootEventSignal(
	ctx context.Context,
	event *syncshortcut.ProtectedRootEvent,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.protectedRootEvents = nil
		return false, nil
	}
	if event == nil {
		return false, nil
	}
	bl, err := rt.deps.store.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: load baseline for protected root event: %w", err)
	}
	if _, refreshErr := rt.refreshAndCommitLocalCurrentState(ctx, bl); refreshErr != nil {
		return false, fmt.Errorf("sync: refresh local_state for protected root event: %w", refreshErr)
	}
	changed, err := rt.engine.reconcileShortcutRootLocalState(ctx)
	if err != nil {
		return false, err
	}
	if !changed || rt.engine.shortcutChildWorkSink == nil {
		return false, nil
	}
	roots, err := rt.deps.store.ListShortcutRoots(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read shortcut roots after protected root event: %w", err)
	}
	return false, rt.engine.shortcutChildWorkSink(ctx, syncshortcut.ShortcutChildWorkSnapshotFromRootsWithParentRoot(
		rt.cfg.shortcutNamespaceID,
		rt.cfg.syncRoot,
		rt.cfg.contentFilter,
		roots,
	))
}

func (rt *watchRuntime) handleWatchRemoteBatchSignal(
	ctx context.Context,
	batch *syncobserve.RemoteObservationBatch,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.remoteBatches = nil
		return false, nil
	}

	return false, rt.handleRemoteObservationBatch(ctx, batch)
}

func (rt *watchRuntime) handleWatchRefreshResultSignal(
	ctx context.Context,
	result *syncobserve.RemoteObservationBatch,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.refreshResults = nil
		return false, nil
	}

	return false, rt.applyRemoteRefreshResult(ctx, result)
}

func (rt *watchRuntime) handleWatchObserverErrorSignal(
	ctx context.Context,
	p *watchPipeline,
	observerErr error,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.observerErrs = nil
		return false, nil
	}

	return false, rt.handleWatchObserverError(ctx, p, observerErr)
}

func (rt *watchRuntime) handleWatchHeldReleaseSignal(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	if rt.phase() == watchPhaseBootstrap {
		return rt.releaseHeldFrontier(ctx, p, trial)
	}

	return rt.handleWatchHeldRelease(ctx, p, trial)
}

func (rt *watchRuntime) appendReadyFrontier(ready []*syncdomain.TrackedAction) error {
	if rt.hasPendingReplan() {
		rt.retireReadyFrontierForPendingReplan(ready)
		return nil
	}
	nextOutbox := append(rt.currentOutbox(), ready...)
	rt.replaceOutbox(nextOutbox)
	rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventReadyFrontierAppended})
	return nil
}

func (rt *watchRuntime) releaseHeldFrontier(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	var (
		released []*syncdomain.TrackedAction
		err      error
	)
	if trial {
		released, err = rt.releaseDueHeldTrialsNow(ctx, p.bl)
	} else {
		released, err = rt.releaseDueHeldRetriesNow(ctx, p.bl)
	}
	if err != nil {
		rt.completeOutboxAsShutdown(released)
		return err
	}

	return rt.appendReadyFrontier(released)
}

func (rt *watchRuntime) handleObserverExit(_ *watchPipeline, shuttingDown bool) error {
	rt.mustAssertObserverExitPhase(shuttingDown, "handle observer exit")

	rt.resources.activeObservers--
	if rt.resources.activeObservers > 0 {
		return nil
	}

	if shuttingDown {
		rt.deps.logger.Info("all observers exited during shutdown")
		return nil
	}

	// Returned, not logged: the caller reports the watch error once.
	if rt.resources.lastObserverErr != nil {
		return fmt.Errorf("sync: stopped watching for changes: %w", rt.resources.lastObserverErr)
	}
	return fmt.Errorf("sync: stopped watching for changes")
}

// logObserverError reports one observer's exit while the other keeps
// running. Watch then no longer sees changes on that side until it is
// restarted, so this is a Warn naming the side (observer errors are wrapped
// by observerExitError). An exit once the run has ended is shutdown, not news
// (a drain begins only then); an observer whose request timed out while the
// run goes on has stopped watching like after any other failure.
func (rt *watchRuntime) logObserverError(ctx context.Context, obsErr error) {
	if obsErr == nil {
		return
	}
	if syncdomain.RunEnded(ctx, obsErr) {
		rt.deps.logger.Debug("observer error during shutdown", slog.Any("error", obsErr))
		return
	}

	rt.resources.lastObserverErr = obsErr
	rt.deps.logger.Warn("Stopped " + obsErr.Error() + "; those changes will not sync until onedrive-go is restarted")
}

// observerExitError names which side an observer watched, so the one-line
// report says what stopped. Nil stays nil; fatal observer errors stay
// recognizable through the wrap.
func observerExitError(observerName string, err error) error {
	if err == nil {
		return nil
	}
	if observerName == observerNameLocal {
		return fmt.Errorf("watching local files for changes: %w", err)
	}

	return fmt.Errorf("checking OneDrive for changes: %w", err)
}

// dispatchChannelForOutbox returns the channel to hand the next outbox entry
// to a worker on and that entry's index, or a nil channel and -1 when nothing
// may be dispatched now: an empty outbox, a draining or replanning runtime,
// every entry's target full under the slow start (R-6.8.25; a completion
// wakes the loop), or every due entry parked on its admission instant
// (R-6.8.24; the admit timer wakes the loop).
func (rt *watchRuntime) dispatchChannelForOutbox() (chan<- *syncdomain.TrackedAction, int) {
	outbox := rt.currentOutbox()
	if len(outbox) == 0 {
		rt.stopAdmitTimer()
		return nil, -1
	}

	if rt.isDraining() {
		rt.mustAssertDispatchAdmissionSealed(outbox, "dispatch channel for outbox")
		return nil, -1
	}
	if rt.hasPendingReplan() {
		return nil, -1
	}

	next, admitAt := rt.nextDispatchable(outbox, rt.deps.now())
	rt.armAdmitTimer(admitAt)
	if next < 0 {
		return nil, -1
	}

	return rt.sched.dispatchCh, next
}

func (rt *watchRuntime) handleWatchReplanReady(
	ctx context.Context,
	p *watchPipeline,
	batch syncobserve.DirtyBatch,
) error {
	if !rt.canPrepareNow() {
		rt.queuePendingReplan(batch)
		return nil
	}

	applied, err := rt.runSteadyStateReplan(ctx, p, batch)
	if err != nil {
		return err
	}
	if !applied {
		return rt.rescheduleReplanIntent(batch)
	}
	return nil
}

func (rt *watchRuntime) handleWatchActionCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *syncdomain.ActionCompletion,
) error {
	rt.loop.createAttempts.completed(completion, rt.deps.now())
	hadPendingReplan := rt.hasPendingReplan()
	ready, err := rt.applyRuntimeCompletionStage(ctx, completion, p.bl)
	if hadPendingReplan {
		rt.advancePendingReplanDrainIdleTracking()
	}
	if err != nil {
		rt.completeOutboxAsShutdown(ready)
		return err
	}
	if err := rt.appendReadyFrontier(ready); err != nil {
		return err
	}
	rt.scheduleFollowUpIfSettled()

	return nil
}

func (rt *watchRuntime) handleWatchCompletionsClosed(
	ctx context.Context,
	p *watchPipeline,
) error {
	select {
	case <-ctx.Done():
		p.completions = nil
		rt.beginWatchDrain(ctx, p)
		return nil
	default:
	}

	return fmt.Errorf("sync: action completions channel closed unexpectedly")
}

func (rt *watchRuntime) handleWatchObserverError(
	ctx context.Context,
	p *watchPipeline,
	observerErr error,
) error {
	if isFatalObserverError(observerErr) {
		return observerErr
	}

	rt.logObserverError(ctx, observerErr)
	rt.beginWatchDrainIfCanceled(ctx, p)
	if err := rt.handleObserverExit(p, ctx.Err() != nil); err != nil {
		return err
	}
	if rt.resources.activeObservers == 0 {
		rt.resources.observerErrs = nil
	}

	return nil
}

func (rt *watchRuntime) handleWatchHeldRelease(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	if rt.hasPendingReplan() {
		return nil
	}

	return rt.releaseHeldFrontier(ctx, p, trial)
}

// logBootstrapWait is the periodic progress line while the initial pass
// waits for its last actions. It is verbose-level progress only when the
// counts moved since the previous tick; an unchanged tick goes to Debug so a
// long transfer does not repeat the same line.
func (rt *watchRuntime) logBootstrapWait(ctx context.Context) {
	inFlight := rt.sched.graph.InFlightCount()
	running := rt.sched.runningCount
	held := len(rt.retries.heldByKey)
	signature := fmt.Sprintf("%d/%d/%d", inFlight, running, held)
	level := slog.LevelInfo
	if signature == rt.loop.lastBootstrapWait {
		level = slog.LevelDebug
	}
	rt.loop.lastBootstrapWait = signature
	rt.deps.logger.Log(ctx, level, "Waiting for the remaining changes to finish",
		slog.Int("in_flight", inFlight),
		slog.Int("running", running),
		slog.Int("held", held),
	)
}

func (rt *watchRuntime) isBootstrapQuiescent() bool {
	return len(rt.currentOutbox()) == 0 &&
		rt.sched.runningCount == 0 &&
		!rt.hasDueHeldWork(rt.deps.now())
}
