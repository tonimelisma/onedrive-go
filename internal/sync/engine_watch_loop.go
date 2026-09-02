package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// runWatchUntilQuiescent drives the bootstrap watch loop until all work due
// now has drained through the shared runtime. Future-held retry/scope work may
// remain unresolved in the graph, so bootstrap quiescence is engine-owned
// rather than defined by graph emptiness.
func (rt *watchRuntime) runWatchUntilQuiescent(
	ctx context.Context,
	p *watchPipeline,
	initialOutbox []*trackedAction,
) error {
	ticker := rt.deps.clock.NewTicker(quiescenceLogInterval)
	defer stopTicker(ticker)

	rt.enterBootstrap()
	rt.replaceOutbox(initialOutbox)

	return rt.runWatchLoop(ctx, p, tickerChan(ticker))
}

// runWatchLoop owns steady-state watch execution. The same goroutine handles
// observed replans, action completions, retry/trial timers, reconcile
// completions, and outbox draining.
func (rt *watchRuntime) runWatchLoop(
	ctx context.Context,
	p *watchPipeline,
	bootstrapLogC ...<-chan time.Time,
) error {
	// Bootstrap and steady state share this loop but end for different
	// reasons. Bootstrap finishes when work due now has quiesced; steady
	// state may only finish by draining. A steady-state loop that reports
	// completion in any other phase has stopped watching while its caller
	// still believes the mount is live, which is silent data staleness
	// rather than a clean shutdown, so it is reported instead of returning
	// the nil that hides it.
	isBootstrapLoop := len(bootstrapLogC) > 0
	if !isBootstrapLoop {
		rt.replaceOutbox(nil)
	}

	var logC <-chan time.Time
	if isBootstrapLoop {
		logC = bootstrapLogC[0]
	}

	for {
		done, err := rt.runWatchLoopStep(ctx, p, logC)
		if err != nil {
			if rt.drainAfterCanceledStepError(ctx, p, err) {
				continue
			}

			return err
		}
		if done {
			if phase := rt.phase(); !isBootstrapLoop && phase != watchRuntimePhaseDraining {
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
	case watchRuntimePhaseDraining:
		return rt.runDrainStep(ctx, p)
	case watchRuntimePhaseBootstrap:
		return rt.runBootstrapLoopStep(ctx, p, logC)
	case watchRuntimePhaseRunning:
		return rt.runRunningLoopStep(ctx, p)
	default:
		return false, fmt.Errorf("sync: unknown watch runtime phase %q", rt.phase())
	}
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
		rt.enterRunning()
		return true, nil
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
	rt.emitRuntimeDebugEvent(engineDebugEventRunningActionsDrained, "", 0, pendingStartedAt)

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

	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		rt.handleWatchDispatch(nextAction)
		return false, nil
	case batch, ok := <-p.replanReady:
		if rt.beginWatchDrainIfContextCanceled(ctx, p) {
			return false, nil
		}
		return rt.handleWatchReplanSignal(ctx, p, batch, ok)
	case completion, ok := <-p.completions:
		return rt.handleWatchCompletionSignal(ctx, p, &completion, ok)
	case batch, ok := <-rt.resources.localBatches:
		return rt.handleWatchLocalObservationBatchSignal(ctx, &batch, ok)
	case event, ok := <-rt.resources.protectedRootEvents:
		return rt.handleWatchProtectedRootEventSignal(ctx, &event, ok)
	case batch, ok := <-rt.resources.remoteBatches:
		return rt.handleWatchRemoteBatchSignal(ctx, &batch, ok)
	case skipped, ok := <-rt.resources.skippedItems:
		return rt.handleWatchSkippedSignal(ctx, skipped, ok)
	case <-rt.resources.refreshCh:
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return false, nil
	case result, ok := <-rt.resources.refreshResults:
		return rt.handleWatchRefreshResultSignal(ctx, &result, ok)
	case <-p.maintenanceC:
		rt.handleMaintenanceTick(ctx)
		return false, nil
	case observerErr, ok := <-rt.resources.observerErrs:
		return rt.handleWatchObserverErrorSignal(ctx, p, observerErr, ok)
	case <-rt.trialTimerChan():
		return false, rt.handleWatchHeldReleaseSignal(ctx, p, true)
	case <-rt.retryTimerChan():
		return false, rt.handleWatchHeldReleaseSignal(ctx, p, false)
	case <-logC:
		rt.logBootstrapWait()
		return false, nil
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}
}

func (rt *watchRuntime) handleWatchDispatch(nextAction *trackedAction) {
	postReplanDispatch := rt.loop.postReplanOutbox
	rt.loop.postReplanOutbox = false
	rt.sched.markRunning(nextAction)
	rt.consumeOutboxHead()
	if postReplanDispatch {
		rt.emitRuntimeDebugEvent(engineDebugEventFirstPostReplanDispatch, "", 0, time.Time{})
	}
}

func (rt *watchRuntime) handleWatchReplanSignal(
	ctx context.Context,
	p *watchPipeline,
	batch dirtyBatch,
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
	completion *actionCompletion,
	ok bool,
) (bool, error) {
	if !ok {
		return false, rt.handleWatchCompletionsClosed(ctx, p)
	}

	return false, rt.handleWatchActionCompletion(ctx, p, completion)
}

func (rt *watchRuntime) handleWatchLocalObservationBatchSignal(
	ctx context.Context,
	batch *localObservationBatch,
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
	event *protectedRootEvent,
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
	roots, err := rt.deps.store.listShortcutRoots(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read shortcut roots after protected root event: %w", err)
	}
	return false, rt.engine.shortcutChildWorkSink(ctx, shortcutChildWorkSnapshotFromRootsWithParentRoot(
		rt.engine.shortcutNamespaceID,
		rt.engine.syncRoot,
		rt.engine.contentFilter,
		roots,
	))
}

func (rt *watchRuntime) handleWatchRemoteBatchSignal(
	ctx context.Context,
	batch *remoteObservationBatch,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.remoteBatches = nil
		return false, nil
	}

	return false, rt.handleRemoteObservationBatch(ctx, batch)
}

func (rt *watchRuntime) handleWatchSkippedSignal(
	ctx context.Context,
	skipped []skippedItem,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.skippedItems = nil
		return false, nil
	}

	reconcileCtx := ctx
	if ctx.Err() != nil {
		reconcileCtx = context.WithoutCancel(ctx)
	}

	err := rt.reconcileSkippedObservationFindings(reconcileCtx, skipped)
	if err != nil && isWatchShutdownError(ctx, err) {
		return false, nil
	}

	return false, err
}

func (rt *watchRuntime) handleWatchRefreshResultSignal(
	ctx context.Context,
	result *remoteObservationBatch,
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
	if rt.phase() == watchRuntimePhaseBootstrap {
		return rt.releaseHeldFrontier(ctx, p, trial)
	}

	return rt.handleWatchHeldRelease(ctx, p, trial)
}

func (rt *watchRuntime) appendReadyFrontier(ready []*trackedAction) error {
	if rt.hasPendingReplan() {
		rt.retireReadyFrontierForPendingReplan(ready)
		return nil
	}
	nextOutbox := append(rt.currentOutbox(), ready...)
	rt.replaceOutbox(nextOutbox)
	rt.deps.emit(engineDebugEvent{Type: engineDebugEventReadyFrontierAppended})
	return nil
}

func (rt *watchRuntime) releaseHeldFrontier(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	var (
		released []*trackedAction
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

	rt.deps.logger.Error("all observers have exited, stopping watch mode")
	return fmt.Errorf("sync: all observers exited")
}

func (rt *watchRuntime) logObserverError(obsErr error) {
	if obsErr == nil {
		return
	}

	rt.deps.logger.Warn("observer error",
		slog.String("error", obsErr.Error()),
	)
}

func (rt *watchRuntime) dispatchChannelForOutbox() (chan<- *trackedAction, *trackedAction) {
	outbox := rt.currentOutbox()
	nextAction := firstOutbox(outbox)
	if nextAction == nil {
		return nil, nil
	}

	if rt.isDraining() {
		rt.mustAssertDispatchAdmissionSealed(outbox, "dispatch channel for outbox")
		return nil, nil
	}
	if rt.hasPendingReplan() {
		return nil, nil
	}

	return rt.sched.dispatchCh, nextAction
}

func firstOutbox(outbox []*trackedAction) *trackedAction {
	if len(outbox) == 0 {
		return nil
	}

	return outbox[0]
}

func (rt *watchRuntime) handleWatchReplanReady(
	ctx context.Context,
	p *watchPipeline,
	batch dirtyBatch,
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
	completion *actionCompletion,
) error {
	hadPendingReplan := rt.hasPendingReplan()
	ready, err := rt.applyRuntimeCompletionStage(ctx, completion, p.bl)
	if hadPendingReplan {
		rt.advancePendingReplanDrainIdleTracking()
	}
	if err != nil {
		rt.completeOutboxAsShutdown(ready)
		return err
	}

	return rt.appendReadyFrontier(ready)
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

	rt.logObserverError(observerErr)
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

func (rt *watchRuntime) logBootstrapWait() {
	rt.deps.logger.Info("bootstrap: waiting for in-flight actions",
		slog.Int("in_flight", rt.sched.graph.InFlightCount()),
		slog.Int("running", rt.sched.runningCount),
		slog.Int("held", len(rt.retries.heldByKey)),
	)
}

func (rt *watchRuntime) isBootstrapQuiescent() bool {
	return len(rt.currentOutbox()) == 0 &&
		rt.sched.runningCount == 0 &&
		!rt.hasDueHeldWork(rt.deps.now())
}
