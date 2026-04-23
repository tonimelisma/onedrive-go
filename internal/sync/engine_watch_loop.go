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
	initialOutbox []*TrackedAction,
) error {
	ticker := rt.engine.newTicker(quiescenceLogInterval)
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
	if len(bootstrapLogC) == 0 {
		rt.replaceOutbox(nil)
	}

	var logC <-chan time.Time
	if len(bootstrapLogC) > 0 {
		logC = bootstrapLogC[0]
	}

	for {
		done, err := rt.runWatchLoopStep(ctx, p, logC)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
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
	if ctx.Err() != nil && !rt.isDraining() {
		rt.beginWatchDrain(ctx, p)
	}
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

	batch, ok := rt.takePendingReplan()
	if !ok {
		return false, nil
	}

	return true, rt.runSteadyStateReplan(ctx, p, batch)
}

func (rt *watchRuntime) runNonDrainingWatchStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		rt.handleWatchDispatch(nextAction)
		return false, nil
	case batch, ok := <-p.replanReady:
		return rt.handleWatchReplanSignal(ctx, p, batch, ok)
	case completion, ok := <-p.completions:
		return rt.handleWatchCompletionSignal(ctx, p, &completion, ok)
	case change, ok := <-p.localEvents:
		return rt.handleWatchLocalChangeSignal(p, &change, ok)
	case batch, ok := <-p.remoteBatches:
		return rt.handleWatchRemoteBatchSignal(ctx, p, &batch, ok)
	case skipped, ok := <-p.skippedCh:
		return rt.handleWatchSkippedSignal(ctx, p, skipped, ok)
	case <-p.refreshC:
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return false, nil
	case result, ok := <-p.refreshResults:
		return rt.handleWatchRefreshResultSignal(ctx, p, &result, ok)
	case <-p.maintenanceC:
		rt.handleMaintenanceTick(ctx)
		return false, nil
	case observerErr, ok := <-p.errs:
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

func (rt *watchRuntime) handleWatchDispatch(nextAction *TrackedAction) {
	rt.markRunning(nextAction)
	rt.consumeOutboxHead()
}

func (rt *watchRuntime) handleWatchReplanSignal(
	ctx context.Context,
	p *watchPipeline,
	batch dirtyBatch,
	ok bool,
) (bool, error) {
	if !ok {
		return true, nil
	}

	return false, rt.handleWatchReplanReady(ctx, p, batch)
}

func (rt *watchRuntime) handleWatchCompletionSignal(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
	ok bool,
) (bool, error) {
	if !ok {
		return false, rt.handleWatchCompletionsClosed(ctx, p)
	}

	return false, rt.handleWatchActionCompletion(ctx, p, completion)
}

func (rt *watchRuntime) handleWatchLocalChangeSignal(
	p *watchPipeline,
	change *ChangeEvent,
	ok bool,
) (bool, error) {
	if !ok {
		p.localEvents = nil
		return false, nil
	}

	rt.handleWatchLocalChange(change)
	return false, nil
}

func (rt *watchRuntime) handleWatchRemoteBatchSignal(
	ctx context.Context,
	p *watchPipeline,
	batch *remoteObservationBatch,
	ok bool,
) (bool, error) {
	if !ok {
		p.remoteBatches = nil
		return false, nil
	}

	return false, rt.handleRemoteObservationBatch(ctx, batch)
}

func (rt *watchRuntime) handleWatchSkippedSignal(
	ctx context.Context,
	p *watchPipeline,
	skipped []SkippedItem,
	ok bool,
) (bool, error) {
	if !ok {
		p.skippedCh = nil
		return false, nil
	}

	return false, rt.reconcileSkippedObservationFindings(ctx, skipped)
}

func (rt *watchRuntime) handleWatchRefreshResultSignal(
	ctx context.Context,
	p *watchPipeline,
	result *remoteRefreshResult,
	ok bool,
) (bool, error) {
	if !ok {
		p.refreshResults = nil
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
		p.errs = nil
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

func (rt *watchRuntime) appendReadyFrontier(
	ctx context.Context,
	p *watchPipeline,
	ready []*TrackedAction,
) error {
	nextOutbox := append(rt.currentOutbox(), ready...)
	rt.maybeFinishSyncStatusBatch(ctx, p.mode, nextOutbox)
	rt.replaceOutbox(nextOutbox)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReadyFrontierAppended})
	return nil
}

func (rt *watchRuntime) releaseHeldFrontier(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	var (
		released []*TrackedAction
		err      error
	)
	if trial {
		released, err = rt.releaseDueHeldTrialsNow(ctx, p.bl)
	} else {
		released, err = rt.releaseDueHeldRetriesNow(ctx, p.bl)
	}
	if err != nil {
		return err
	}

	return rt.appendReadyFrontier(ctx, p, released)
}

func (rt *watchRuntime) handleObserverExit(p *watchPipeline, shuttingDown bool) error {
	rt.mustAssertObserverExitPhase(rt, shuttingDown, "handle observer exit")

	p.activeObs--
	if p.activeObs > 0 {
		return nil
	}

	if shuttingDown {
		rt.engine.logger.Info("all observers exited during shutdown")
		return nil
	}

	rt.engine.logger.Error("all observers have exited, stopping watch mode")
	return fmt.Errorf("sync: all observers exited")
}

func (rt *watchRuntime) logObserverError(obsErr error) {
	if obsErr == nil {
		return
	}

	rt.engine.logger.Warn("observer error",
		slog.String("error", obsErr.Error()),
	)
}

func (rt *watchRuntime) dispatchChannelForOutbox() (chan<- *TrackedAction, *TrackedAction) {
	outbox := rt.currentOutbox()
	nextAction := firstOutbox(outbox)
	if nextAction == nil {
		return nil, nil
	}

	if rt.isDraining() {
		rt.mustAssertDispatchAdmissionSealed(rt, outbox, "dispatch channel for outbox")
		return nil, nil
	}

	return rt.dispatchCh, nextAction
}

func firstOutbox(outbox []*TrackedAction) *TrackedAction {
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

	return rt.runSteadyStateReplan(ctx, p, batch)
}

func (rt *watchRuntime) handleWatchActionCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
) error {
	ready, err := rt.applyRuntimeCompletionStage(ctx, rt, completion, p.bl)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.completeOutboxAsShutdown(ready)
		return err
	}

	return rt.appendReadyFrontier(ctx, p, ready)
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

func (rt *watchRuntime) handleWatchLocalChange(change *ChangeEvent) {
	if change == nil || rt.dirtyBuf == nil {
		return
	}
	if change.Path != "" || change.OldPath != "" {
		rt.dirtyBuf.MarkDirty()
	}
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
	if err := rt.handleObserverExit(p, ctx.Err() != nil); err != nil {
		return err
	}
	if p.activeObs == 0 {
		p.errs = nil
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
	rt.engine.logger.Info("bootstrap: waiting for in-flight actions",
		slog.Int("in_flight", rt.depGraph.InFlightCount()),
		slog.Int("running", rt.runningCount),
		slog.Int("held", len(rt.heldByKey)),
	)
}

func (rt *watchRuntime) isBootstrapQuiescent() bool {
	return len(rt.currentOutbox()) == 0 &&
		rt.runningCount == 0 &&
		!rt.hasDueHeldWork(rt.engine.nowFunc())
}
