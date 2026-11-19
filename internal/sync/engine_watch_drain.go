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
		rt.stopDrainTimers()
		// The stop's one bound for recording the completions still to come
		// is set when the watch first sees its stop, measured from the
		// cancel (beginStopDrain).
		rt.beginStopDrain()
		rt.deps.emit(engineDebugEvent{Type: engineDebugEventShutdownStarted})
		rt.deps.logger.Info("graceful shutdown: sealing new work admission",
			slog.Int("in_flight", rt.sched.graph.InFlightCount()),
		)
	}

	rt.completeDrainOutbox()
	rt.disableDrainInputs(p)
	rt.refreshDrainCompletionSources()
	rt.mustAssertInvariants(ctx, "begin watch drain")
}

func (rt *watchRuntime) stopDrainTimers() {
	rt.stopRetryTimer()
	rt.stopTrialTimer()
	rt.stopAdmitTimer()
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
	rt.resources.localBatches = nil
	rt.resources.remoteBatches = nil
	rt.resources.skippedItems = nil
	p.maintenanceC = nil
}

func (rt *watchRuntime) refreshDrainCompletionSources() {
	if rt.resources.activeObservers == 0 {
		rt.resources.observerErrs = nil
	}
}

// runDrainStep runs one step of the drain. When the drain is over, because
// its sources ran dry or a step failed, the stop's bound ends: what the bound
// cut short is said once, and its store failure is the watch's error unless a
// step failed first (endStopDrain).
func (rt *watchRuntime) runDrainStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	done, err := rt.takeDrainStep(ctx, p)
	if !done && err == nil {
		return false, nil
	}

	drainErr := rt.endStopDrain()
	if err != nil {
		return done, err
	}

	return done, drainErr
}

func (rt *watchRuntime) takeDrainStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	// Draining phase: no new work admission remains live. Only action
	// completions, refresh result cleanup, and terminal observer exit/error
	// handling may run.
	if rt.drainLoopDone(p) {
		return true, nil
	}

	// Once every completion is in, the drain waits only for the observers'
	// exits and a refresh's result. That wait is bounded like the stop's:
	// when the bound passes, the drain ends and the stop names what is still
	// running (watchGoroutines.stop).
	var goroutinesDue <-chan struct{}
	if p.completions == nil {
		goroutinesDue = rt.resources.goroutines.deadline(rt.deps.clock)
	}

	select {
	case completion, ok := <-p.completions:
		return rt.handleDrainingCompletion(ctx, p, &completion, ok)
	case _, ok := <-rt.resources.refreshResults:
		return rt.handleDrainingRefreshResult(ctx, p, ok)
	case obsErr, ok := <-rt.resources.observerErrs:
		return rt.handleDrainingObserverError(ctx, p, obsErr, ok)
	case <-goroutinesDue:
		return true, nil
	}
}

func (rt *watchRuntime) drainLoopDone(p *watchPipeline) bool {
	return p.completions == nil && !rt.resources.refreshActive && rt.resources.activeObservers == 0
}

func (rt *watchRuntime) handleDrainingCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *actionCompletion,
	ok bool,
) (bool, error) {
	if !ok {
		p.completions = nil
		return rt.drainLoopDone(p), nil
	}

	err := rt.processShutdownCompletion(ctx, completion, p.bl)
	if err != nil {
		rt.logSuppressedShutdownCompletionError(completion, err)
		rt.mustAssertInvariants(ctx, "handle draining completion")
		return false, nil
	}
	rt.mustAssertInvariants(ctx, "handle draining completion")

	return false, nil
}

func (rt *watchRuntime) handleDrainingRefreshResult(
	ctx context.Context,
	p *watchPipeline,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.refreshActive = false
		return rt.drainLoopDone(p), nil
	}

	rt.dropRemoteRefreshResultOnShutdown()
	rt.mustAssertRefreshBookkeepingCleared("handle draining refresh result")
	rt.mustAssertInvariants(ctx, "handle draining refresh result")

	return rt.drainLoopDone(p), nil
}

func (rt *watchRuntime) handleDrainingObserverError(
	ctx context.Context,
	p *watchPipeline,
	obsErr error,
	ok bool,
) (bool, error) {
	if !ok {
		rt.resources.observerErrs = nil
		return rt.drainLoopDone(p), nil
	}

	rt.logObserverError(ctx, obsErr)
	if err := rt.handleObserverExit(p, true); err != nil {
		return false, err
	}
	if rt.resources.activeObservers == 0 {
		rt.resources.observerErrs = nil
	}

	return rt.drainLoopDone(p), nil
}
