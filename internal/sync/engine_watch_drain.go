package sync

import (
	"context"
	"log/slog"
)

func (rt *watchRuntime) beginWatchDrain(
	ctx context.Context,
	p *watchPipeline,
) {
	if rt.enterDraining() {
		rt.clearSyncStatusBatch()
		rt.stopDrainTimers()
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventShutdownStarted})
		rt.engine.logger.Info("graceful shutdown: sealing new work admission",
			slog.Int("in_flight", rt.depGraph.InFlightCount()),
		)
	}

	rt.completeDrainOutbox()
	rt.disableDrainInputs(p)
	rt.refreshDrainCompletionSources(p)
	rt.mustAssertInvariants(ctx, rt, "begin watch drain")
}

func (rt *watchRuntime) stopDrainTimers() {
	rt.stopRetryTimer()
	rt.stopTrialTimer()
}

func (rt *watchRuntime) completeDrainOutbox() {
	outbox := rt.currentOutbox()
	if len(outbox) == 0 {
		return
	}

	rt.completeOutboxAsShutdown(outbox)
	rt.replaceOutbox(nil)
}

func (rt *watchRuntime) disableDrainInputs(p *watchPipeline) {
	p.replanReady = nil
	p.localEvents = nil
	p.remoteBatches = nil
	p.skippedCh = nil
	p.maintenanceC = nil
	p.refreshC = nil
}

func (rt *watchRuntime) refreshDrainCompletionSources(p *watchPipeline) {
	if !rt.refreshActive {
		p.refreshResults = nil
	}
	if p.activeObs == 0 {
		p.errs = nil
	}
}

func (rt *watchRuntime) runDrainStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	// Draining phase: no new work admission remains live. Only action
	// completions, refresh result cleanup, and terminal observer exit/error
	// handling may run.
	if rt.drainLoopDone(p) {
		return true, nil
	}

	select {
	case completion, ok := <-p.completions:
		return rt.handleDrainingCompletion(ctx, p, &completion, ok)
	case _, ok := <-p.refreshResults:
		return rt.handleDrainingRefreshResult(ctx, p, ok)
	case obsErr, ok := <-p.errs:
		return rt.handleDrainingObserverError(p, obsErr, ok)
	}
}

func (rt *watchRuntime) drainLoopDone(p *watchPipeline) bool {
	return p.completions == nil && p.refreshResults == nil && p.activeObs == 0
}

func (rt *watchRuntime) handleDrainingCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
	ok bool,
) (bool, error) {
	if !ok {
		p.completions = nil
		return rt.drainLoopDone(p), nil
	}

	err := rt.processShutdownCompletion(ctx, completion, p.bl)
	if err != nil {
		rt.logSuppressedShutdownCompletionError(completion, err)
		rt.mustAssertInvariants(ctx, rt, "handle draining completion")
		return false, nil
	}
	rt.mustAssertInvariants(ctx, rt, "handle draining completion")

	return false, nil
}

func (rt *watchRuntime) handleDrainingRefreshResult(
	ctx context.Context,
	p *watchPipeline,
	ok bool,
) (bool, error) {
	if !ok {
		p.refreshResults = nil
		return rt.drainLoopDone(p), nil
	}

	rt.dropRemoteRefreshResultOnShutdown()
	rt.mustAssertRefreshBookkeepingCleared(rt, "handle draining refresh result")
	p.refreshResults = nil
	rt.mustAssertInvariants(ctx, rt, "handle draining refresh result")

	return rt.drainLoopDone(p), nil
}

func (rt *watchRuntime) handleDrainingObserverError(
	p *watchPipeline,
	obsErr error,
	ok bool,
) (bool, error) {
	if !ok {
		p.errs = nil
		return rt.drainLoopDone(p), nil
	}

	rt.logObserverError(obsErr)
	if err := rt.handleObserverExit(p, true); err != nil {
		return false, err
	}
	if p.activeObs == 0 {
		p.errs = nil
	}

	return rt.drainLoopDone(p), nil
}
