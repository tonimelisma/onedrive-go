package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func (r *oneShotRunner) runResultsLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	results <-chan WorkerResult,
) error {
	var outbox []*TrackedAction
	var fatalErr error

	for {
		if fatalErr != nil && len(outbox) > 0 {
			r.completeOutboxAsShutdown(outbox)
			outbox = nil
			continue
		}

		if len(outbox) == 0 {
			nextOutbox, nextFatal, done := r.runResultsLoopIdle(ctx, cancel, bl, results, fatalErr)
			outbox = nextOutbox
			fatalErr = nextFatal
			if done {
				return fatalErr
			}
			continue
		}

		nextOutbox, nextFatal, done := r.runResultsLoopWithOutbox(ctx, cancel, bl, results, outbox, fatalErr)
		outbox = nextOutbox
		fatalErr = nextFatal
		if done {
			return fatalErr
		}
	}
}

func (r *oneShotRunner) runResultsLoopIdle(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	results <-chan WorkerResult,
	fatalErr error,
) ([]*TrackedAction, error, bool) {
	select {
	case workerResult, ok := <-results:
		if !ok {
			return nil, fatalErr, true
		}
		nextOutbox, nextFatal := r.handleOneShotWorkerResult(ctx, cancel, bl, nil, fatalErr, &workerResult)
		return nextOutbox, nextFatal, false
	case <-resultsLoopCtxDone(ctx, fatalErr):
		return nil, fatalErr, true
	}
}

func (r *oneShotRunner) runResultsLoopWithOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	results <-chan WorkerResult,
	outbox []*TrackedAction,
	fatalErr error,
) ([]*TrackedAction, error, bool) {
	select {
	case r.dispatchCh <- outbox[0]:
		return outbox[1:], fatalErr, false
	case workerResult, ok := <-results:
		if !ok {
			return outbox, fatalErr, true
		}
		nextOutbox, nextFatal := r.handleOneShotWorkerResult(ctx, cancel, bl, outbox, fatalErr, &workerResult)
		return nextOutbox, nextFatal, false
	case <-resultsLoopCtxDone(ctx, fatalErr):
		return outbox, fatalErr, true
	}
}

func (r *oneShotRunner) handleOneShotWorkerResult(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	outbox []*TrackedAction,
	fatalErr error,
	workerResult *WorkerResult,
) ([]*TrackedAction, error) {
	outcome := r.processWorkerResult(ctx, nil, workerResult, bl)
	outbox = append(outbox, outcome.dispatched...)
	if !outcome.terminate || fatalErr != nil {
		return outbox, fatalErr
	}

	fatalErr = outcome.terminateErr
	if cancel != nil {
		cancel()
	}
	if len(outbox) > 0 {
		r.completeOutboxAsShutdown(outbox)
		outbox = nil
	}
	r.completeQueuedDispatchAsShutdown()

	return outbox, fatalErr
}

func resultsLoopCtxDone(ctx context.Context, fatalErr error) <-chan struct{} {
	if fatalErr != nil {
		return nil
	}

	return ctx.Done()
}

func (r *oneShotRunner) completeQueuedDispatchAsShutdown() {
	for {
		select {
		case ta := <-r.dispatchCh:
			r.completeTrackedActionAsShutdown(ta)
		default:
			return
		}
	}
}

func (f *engineFlow) completeOutboxAsShutdown(outbox []*TrackedAction) {
	for _, ta := range outbox {
		f.completeTrackedActionAsShutdown(ta)
	}
}

func (f *engineFlow) completeTrackedActionAsShutdown(ta *TrackedAction) {
	if ta == nil {
		return
	}

	ready := f.completeDepGraphAction(ta.ID, "completeTrackedActionAsShutdown")
	f.scopeController().completeSubtree(ready)
}

// runWatchUntilQuiescent drives the bootstrap watch loop until the dependency
// graph empties. Bootstrap uses the same watch-owned result/admission logic as
// steady-state watch mode but stops once there is no remaining in-flight work.
func (rt *watchRuntime) runWatchUntilQuiescent(
	ctx context.Context,
	p *watchPipeline,
	initialOutbox []*TrackedAction,
) error {
	ticker := rt.engine.newTicker(quiescenceLogInterval)
	defer stopTicker(ticker)

	rt.replaceOutbox(initialOutbox)
	emptyCh := rt.depGraph.WaitForEmpty()
	logC := tickerChan(ticker)

	for {
		if ctx.Err() != nil && !rt.isDraining() {
			rt.beginWatchDrain(ctx, p)
		}
		if rt.isDraining() {
			done, err := rt.runDrainStep(ctx, p)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}

		done, err := rt.runBootstrapStep(ctx, p, logC, emptyCh)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// runWatchLoop owns steady-state watch execution. The same goroutine handles
// observed batches, worker results, retry/trial timers, reconcile completions,
// and outbox draining.
func (rt *watchRuntime) runWatchLoop(ctx context.Context, p *watchPipeline) error {
	rt.replaceOutbox(nil)

	for {
		if ctx.Err() != nil && !rt.isDraining() {
			rt.beginWatchDrain(ctx, p)
		}
		if rt.isDraining() {
			done, err := rt.runDrainStep(ctx, p)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}

		done, err := rt.runWatchStep(ctx, p)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (rt *watchRuntime) runWatchStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	event := rt.waitWatchEvent(ctx, p)
	transition, err := rt.transitionWatchEvent(ctx, p, &event)
	if err != nil {
		return false, err
	}

	return rt.applyWatchTransition(ctx, p, transition)
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

func (rt *watchRuntime) beginWatchDrain(
	ctx context.Context,
	p *watchPipeline,
) {
	if rt.enterDraining() {
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
	p.batchReady = nil
	p.skippedCh = nil
	p.scopeChanges = nil
	p.recheckC = nil
	p.reconcileC = nil
}

func (rt *watchRuntime) refreshDrainCompletionSources(p *watchPipeline) {
	if !rt.reconcileActive {
		p.reconcileResults = nil
	}
	if p.activeObs == 0 {
		p.errs = nil
	}
}

func (rt *watchRuntime) runDrainStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	// Draining phase: no new work admission remains live. Only worker results,
	// reconcile result cleanup, and terminal observer exit/error handling may run.
	if rt.drainLoopDone(p) {
		return true, nil
	}

	select {
	case workerResult, ok := <-p.results:
		return rt.handleDrainingWorkerResult(ctx, p, &workerResult, ok)
	case _, ok := <-p.reconcileResults:
		return rt.handleDrainingReconcileResult(ctx, p, ok)
	case obsErr, ok := <-p.errs:
		return rt.handleDrainingObserverError(p, obsErr, ok)
	}
}

func (rt *watchRuntime) drainLoopDone(p *watchPipeline) bool {
	return p.results == nil && p.reconcileResults == nil && p.activeObs == 0
}

func (rt *watchRuntime) logObserverError(obsErr error) {
	if obsErr == nil {
		return
	}

	rt.engine.logger.Warn("observer error",
		slog.String("error", obsErr.Error()),
	)
}

func (rt *watchRuntime) runBootstrapStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
	emptyCh <-chan struct{},
) (bool, error) {
	outbox := rt.currentOutbox()

	// Bootstrap phase: dispatch, buffered bootstrap batches, worker results,
	// retry/trial wakeups, and quiescence logging are live until the graph empties.
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		rt.consumeOutboxHead()
		return false, nil
	case batch, ok := <-p.batchReady:
		nextOutbox, done := rt.handleBootstrapBatch(ctx, p, outbox, batch, ok)
		rt.replaceOutbox(nextOutbox)
		return done, nil
	case workerResult, ok := <-p.results:
		nextOutbox, done, err := rt.handleBootstrapWorkerResult(ctx, p, outbox, &workerResult, ok)
		rt.replaceOutbox(nextOutbox)
		return done, err
	case <-rt.trialTimerChan():
		rt.appendOutbox(rt.runTrialDispatch(ctx, p.bl, p.mode, p.safety))
		return false, nil
	case <-rt.retryTimerChan():
		rt.appendOutbox(rt.runRetrierSweep(ctx, p.bl, p.mode, p.safety))
		return false, nil
	case <-logC:
		rt.logBootstrapWait()
		return false, nil
	case <-emptyCh:
		return true, nil
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}
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

func (rt *watchRuntime) handleBootstrapBatch(
	ctx context.Context,
	p *watchPipeline,
	outbox []*TrackedAction,
	batch []PathChanges,
	ok bool,
) ([]*TrackedAction, bool) {
	if !ok {
		return outbox, true
	}

	return append(outbox, rt.processBatch(ctx, batch, p.bl, p.mode, p.safety)...), false
}

func (rt *watchRuntime) handleBootstrapWorkerResult(
	ctx context.Context,
	p *watchPipeline,
	outbox []*TrackedAction,
	workerResult *WorkerResult,
	ok bool,
) ([]*TrackedAction, bool, error) {
	if !ok {
		if contextIsCanceled(ctx) {
			p.results = nil
			rt.beginWatchDrain(ctx, p)
			return nil, rt.drainLoopDone(p), nil
		}
		return rt.handleBootstrapResultsClosed(ctx)
	}

	outcome := rt.processWorkerResult(ctx, rt, workerResult, p.bl)
	if outcome.terminate {
		return outbox, false, outcome.terminateErr
	}
	return append(outbox, outcome.dispatched...), false, nil
}

func (rt *watchRuntime) handleBootstrapResultsClosed(
	ctx context.Context,
) ([]*TrackedAction, bool, error) {
	select {
	case <-ctx.Done():
		return nil, false, fmt.Errorf("sync: watch bootstrap context done: %w", ctx.Err())
	default:
	}

	return nil, false, fmt.Errorf("sync: worker results channel closed unexpectedly")
}

func (rt *watchRuntime) logBootstrapWait() {
	rt.engine.logger.Info("bootstrap: waiting for in-flight actions",
		slog.Int("in_flight", rt.depGraph.InFlightCount()),
	)
}

func (rt *watchRuntime) handleWatchWorkerResult(
	ctx context.Context,
	p *watchPipeline,
	outbox []*TrackedAction,
	workerResult *WorkerResult,
	ok bool,
) ([]*TrackedAction, bool, error) {
	if !ok {
		if rt.isDraining() {
			p.results = nil
			return outbox, rt.drainLoopDone(p), nil
		}
		if contextIsCanceled(ctx) {
			p.results = nil
			rt.beginWatchDrain(ctx, p)
			return nil, rt.drainLoopDone(p), nil
		}
		return outbox, false, fmt.Errorf("sync: worker results channel closed unexpectedly")
	}

	outcome := rt.processWorkerResult(ctx, rt, workerResult, p.bl)
	if outcome.terminate {
		return outbox, false, outcome.terminateErr
	}
	return append(outbox, outcome.dispatched...), false, nil
}

func contextIsCanceled(ctx context.Context) bool {
	return ctx.Err() != nil
}

func (rt *watchRuntime) handleDrainingWorkerResult(
	ctx context.Context,
	p *watchPipeline,
	workerResult *WorkerResult,
	ok bool,
) (bool, error) {
	if !ok {
		p.results = nil
		return rt.drainLoopDone(p), nil
	}

	outcome := rt.processWorkerResult(ctx, rt, workerResult, p.bl)
	rt.completeOutboxAsShutdown(outcome.dispatched)
	rt.mustAssertInvariants(ctx, rt, "handle draining worker result")

	return false, nil
}

func (rt *watchRuntime) handleDrainingReconcileResult(
	ctx context.Context,
	p *watchPipeline,
	ok bool,
) (bool, error) {
	if !ok {
		p.reconcileResults = nil
		return rt.drainLoopDone(p), nil
	}

	rt.dropReconcileResultOnShutdown()
	rt.mustAssertReconcileBookkeepingCleared(rt, "handle draining reconcile result")
	p.reconcileResults = nil
	rt.mustAssertInvariants(ctx, rt, "handle draining reconcile result")

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

func firstOutbox(outbox []*TrackedAction) *TrackedAction {
	if len(outbox) == 0 {
		return nil
	}

	return outbox[0]
}
