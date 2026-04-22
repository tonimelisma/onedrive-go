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
		rt.beginWatchDrainIfCanceled(ctx, p)

		done, err := rt.runWatchPhaseStep(ctx, p, logC)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (rt *watchRuntime) beginWatchDrainIfCanceled(ctx context.Context, p *watchPipeline) {
	if ctx.Err() != nil && !rt.isDraining() {
		rt.beginWatchDrain(ctx, p)
	}
}

func (rt *watchRuntime) runWatchPhaseStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	switch rt.phase() {
	case watchRuntimePhaseBootstrap:
		return rt.runBootstrapPhaseStep(ctx, p, logC)
	case watchRuntimePhaseDraining:
		return rt.runDrainStep(ctx, p)
	case watchRuntimePhaseRunning:
		return rt.runRunningPhaseStep(ctx, p)
	default:
		return false, fmt.Errorf("sync: unknown watch runtime phase %q", rt.phase())
	}
}

func (rt *watchRuntime) runBootstrapPhaseStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	if rt.isBootstrapQuiescent() {
		rt.enterRunning()
		return true, nil
	}

	return rt.runBootstrapStep(ctx, p, logC)
}

func (rt *watchRuntime) runRunningPhaseStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	replanned, err := rt.runPendingReplan(ctx, p)
	if err != nil {
		return false, err
	}
	if replanned {
		return false, nil
	}

	return rt.runWatchStep(ctx, p)
}

func (rt *watchRuntime) runPendingReplan(
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

func (rt *watchRuntime) runWatchStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	if len(rt.currentOutbox()) == 0 {
		return rt.runWatchStepIdle(ctx, p)
	}

	return rt.runWatchStepWithOutbox(ctx, p)
}

func (rt *watchRuntime) runWatchStepWithOutbox(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		rt.handleWatchDispatchReady(nextAction)
		return false, nil
	case batch, ok := <-p.replanReady:
		return rt.handleWatchReplanChannel(ctx, p, batch, ok)
	case completion, ok := <-p.completions:
		return rt.handleWatchCompletionChannel(ctx, p, &completion, ok)
	case change, ok := <-p.localEvents:
		return rt.handleWatchLocalChangeChannel(p, &change, ok)
	case batch, ok := <-p.remoteBatches:
		return rt.handleWatchRemoteBatchChannel(ctx, p, &batch, ok)
	case skipped, ok := <-p.skippedCh:
		return rt.handleWatchSkippedChannel(ctx, p, skipped, ok)
	case <-p.refreshC:
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return false, nil
	case result, ok := <-p.refreshResults:
		return rt.handleWatchRefreshResultChannel(ctx, p, &result, ok)
	case <-p.maintenanceC:
		rt.handleMaintenanceTick(ctx)
		return false, nil
	case observerErr, ok := <-p.errs:
		return rt.handleWatchObserverErrorChannel(ctx, p, observerErr, ok)
	case <-rt.trialTimerChan():
		return false, rt.handleWatchHeldRelease(ctx, p, true)
	case <-rt.retryTimerChan():
		return false, rt.handleWatchHeldRelease(ctx, p, false)
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}
}

func (rt *watchRuntime) runWatchStepIdle(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	select {
	case batch, ok := <-p.replanReady:
		return rt.handleWatchReplanChannel(ctx, p, batch, ok)
	case completion, ok := <-p.completions:
		return rt.handleWatchCompletionChannel(ctx, p, &completion, ok)
	case change, ok := <-p.localEvents:
		return rt.handleWatchLocalChangeChannel(p, &change, ok)
	case batch, ok := <-p.remoteBatches:
		return rt.handleWatchRemoteBatchChannel(ctx, p, &batch, ok)
	case skipped, ok := <-p.skippedCh:
		return rt.handleWatchSkippedChannel(ctx, p, skipped, ok)
	case <-p.refreshC:
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return false, nil
	case result, ok := <-p.refreshResults:
		return rt.handleWatchRefreshResultChannel(ctx, p, &result, ok)
	case <-p.maintenanceC:
		rt.handleMaintenanceTick(ctx)
		return false, nil
	case observerErr, ok := <-p.errs:
		return rt.handleWatchObserverErrorChannel(ctx, p, observerErr, ok)
	case <-rt.trialTimerChan():
		return false, rt.handleWatchHeldRelease(ctx, p, true)
	case <-rt.retryTimerChan():
		return false, rt.handleWatchHeldRelease(ctx, p, false)
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}
}

func (rt *watchRuntime) appendReadyFrontier(
	ctx context.Context,
	p *watchPipeline,
	ready []*TrackedAction,
) error {
	reduced, err := rt.drainPublicationFrontier(ctx, rt, p.bl, ready)
	nextOutbox := append(rt.currentOutbox(), reduced...)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.completeOutboxAsShutdown(nextOutbox)
		return err
	}
	if rt.afterAppendReadyFrontier != nil {
		rt.afterAppendReadyFrontier()
	}

	rt.maybeFinishSyncStatusBatch(ctx, p.mode, nextOutbox)
	rt.replaceOutbox(nextOutbox)
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
		released, err = rt.releaseDueHeldTrialsNow(ctx)
	} else {
		released, err = rt.releaseDueHeldRetriesNow(ctx)
	}
	if err != nil {
		return err
	}

	return rt.appendReadyFrontier(ctx, p, released)
}

func (rt *watchRuntime) applyRuntimeCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
) error {
	ready, err := rt.processActionCompletion(ctx, rt, completion, p.bl)
	if err != nil {
		rt.clearSyncStatusBatch()
		rt.completeOutboxAsShutdown(ready)
		return err
	}

	return rt.appendReadyFrontier(ctx, p, ready)
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
