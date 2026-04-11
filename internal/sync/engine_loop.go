package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (r *oneShotRunner) runResultsLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *synctypes.Baseline,
	results <-chan synctypes.WorkerResult,
) error {
	var outbox []*synctypes.TrackedAction
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
	bl *synctypes.Baseline,
	results <-chan synctypes.WorkerResult,
	fatalErr error,
) ([]*synctypes.TrackedAction, error, bool) {
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
	bl *synctypes.Baseline,
	results <-chan synctypes.WorkerResult,
	outbox []*synctypes.TrackedAction,
	fatalErr error,
) ([]*synctypes.TrackedAction, error, bool) {
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
	bl *synctypes.Baseline,
	outbox []*synctypes.TrackedAction,
	fatalErr error,
	workerResult *synctypes.WorkerResult,
) ([]*synctypes.TrackedAction, error) {
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

func (f *engineFlow) completeOutboxAsShutdown(outbox []*synctypes.TrackedAction) {
	for _, ta := range outbox {
		f.completeTrackedActionAsShutdown(ta)
	}
}

func (f *engineFlow) completeTrackedActionAsShutdown(ta *synctypes.TrackedAction) {
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
	initialOutbox []*synctypes.TrackedAction,
) error {
	ticker := rt.engine.newTicker(quiescenceLogInterval)
	defer stopTicker(ticker)

	outbox := append([]*synctypes.TrackedAction(nil), initialOutbox...)
	emptyCh := rt.depGraph.WaitForEmpty()
	logC := tickerChan(ticker)

	for {
		if ctx.Err() != nil && !rt.isDraining() {
			rt.beginWatchDrain(ctx, p, outbox)
			outbox = nil
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

		nextOutbox, done, err := rt.runBootstrapStep(ctx, p, outbox, logC, emptyCh)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		outbox = nextOutbox
	}
}

// runWatchLoop owns steady-state watch execution. The same goroutine handles
// observed batches, worker results, retry/trial timers, reconcile completions,
// and outbox draining.
func (rt *watchRuntime) runWatchLoop(ctx context.Context, p *watchPipeline) error {
	var outbox []*synctypes.TrackedAction

	for {
		if ctx.Err() != nil && !rt.isDraining() {
			rt.beginWatchDrain(ctx, p, outbox)
			outbox = nil
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

		nextOutbox, done, err := rt.runWatchStep(ctx, p, outbox)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		outbox = nextOutbox
	}
}

func (rt *watchRuntime) runWatchStep(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
) ([]*synctypes.TrackedAction, bool, error) {
	// Steady-state watch phase: dispatch, observer batches, skipped items,
	// retry/trial wakeups, and reconcile handoff are all allowed to be live.
	dispatchCh, nextAction := rt.dispatchChannelForOutbox(outbox)

	select {
	case dispatchCh <- nextAction:
		return rt.flushPendingUserIntent(ctx, p, outbox[1:]), false, nil
	case batch, ok := <-p.batchReady:
		return rt.handleWatchBatch(ctx, p, outbox, batch, ok)
	case workerResult, ok := <-p.results:
		return rt.handleWatchWorkerResult(ctx, p, outbox, &workerResult, ok)
	case skipped, ok := <-p.skippedCh:
		return rt.handleWatchSkipped(ctx, p, outbox, skipped, ok)
	case scopeChange, ok := <-p.scopeChanges:
		return rt.handleWatchScopeChange(ctx, p, outbox, &scopeChange, ok)
	case <-p.recheckC:
		rt.handleRecheckTick(ctx)
		rt.queueUserIntentDispatch()
		return rt.flushPendingUserIntent(ctx, p, outbox), false, nil
	case <-p.userIntentC:
		rt.queueUserIntentDispatch()
		return rt.flushPendingUserIntent(ctx, p, outbox), false, nil
	case <-p.reconcileC:
		rt.runFullReconciliationAsync(ctx, p.bl)
		return outbox, false, nil
	case result, ok := <-p.reconcileResults:
		return rt.handleWatchReconcileResult(ctx, p, outbox, result, ok)
	case obsErr, ok := <-p.errs:
		return rt.handleWatchObserverError(ctx, p, outbox, obsErr, ok)
	case <-rt.trialTimerChan():
		return append(outbox, rt.runTrialDispatch(ctx, p.bl, p.mode, p.safety)...), false, nil
	case <-rt.retryTimerChan():
		return append(outbox, rt.runRetrierSweep(ctx, p.bl, p.mode, p.safety)...), false, nil
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p, outbox)
		return nil, false, nil
	}
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
	outbox []*synctypes.TrackedAction,
) {
	if rt.enterDraining() {
		rt.stopDrainTimers()
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventShutdownStarted})
		rt.engine.logger.Info("graceful shutdown: sealing new work admission",
			slog.Int("in_flight", rt.depGraph.InFlightCount()),
		)
	}

	rt.completeDrainOutbox(outbox)
	rt.disableDrainInputs(p)
	rt.refreshDrainCompletionSources(p)
	rt.mustAssertInvariants(ctx, rt, "begin watch drain")
}

func (rt *watchRuntime) stopDrainTimers() {
	rt.stopRetryTimer()
	rt.stopTrialTimer()
}

func (rt *watchRuntime) completeDrainOutbox(outbox []*synctypes.TrackedAction) {
	if len(outbox) == 0 {
		return
	}

	rt.completeOutboxAsShutdown(outbox)
}

func (rt *watchRuntime) disableDrainInputs(p *watchPipeline) {
	p.batchReady = nil
	p.skippedCh = nil
	p.scopeChanges = nil
	p.recheckC = nil
	p.userIntentC = nil
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
	outbox []*synctypes.TrackedAction,
	logC <-chan time.Time,
	emptyCh <-chan struct{},
) ([]*synctypes.TrackedAction, bool, error) {
	// Bootstrap phase: dispatch, buffered bootstrap batches, worker results,
	// retry/trial wakeups, and quiescence logging are live until the graph empties.
	dispatchCh, nextAction := rt.dispatchChannelForOutbox(outbox)

	select {
	case dispatchCh <- nextAction:
		return outbox[1:], false, nil
	case batch, ok := <-p.batchReady:
		return rt.handleBootstrapBatch(ctx, p, outbox, batch, ok)
	case workerResult, ok := <-p.results:
		return rt.handleBootstrapWorkerResult(ctx, p, outbox, &workerResult, ok)
	case <-rt.trialTimerChan():
		return append(outbox, rt.runTrialDispatch(ctx, p.bl, p.mode, p.safety)...), false, nil
	case <-rt.retryTimerChan():
		return append(outbox, rt.runRetrierSweep(ctx, p.bl, p.mode, p.safety)...), false, nil
	case <-logC:
		rt.logBootstrapWait()
		return outbox, false, nil
	case <-emptyCh:
		return outbox, true, nil
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p, outbox)
		return nil, false, nil
	}
}

func (rt *watchRuntime) dispatchChannelForOutbox(
	outbox []*synctypes.TrackedAction,
) (chan<- *synctypes.TrackedAction, *synctypes.TrackedAction) {
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
	outbox []*synctypes.TrackedAction,
	batch []synctypes.PathChanges,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		return outbox, true, nil
	}

	return append(outbox, rt.processBatch(ctx, batch, p.bl, p.mode, p.safety)...), false, nil
}

func (rt *watchRuntime) handleBootstrapWorkerResult(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
	workerResult *synctypes.WorkerResult,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		if contextIsCanceled(ctx) {
			p.results = nil
			rt.beginWatchDrain(ctx, p, outbox)
			return nil, rt.drainLoopDone(p), nil
		}
		return rt.handleBootstrapResultsClosed(ctx)
	}

	outcome := rt.processWorkerResult(ctx, rt, workerResult, p.bl)
	if outcome.terminate {
		return outbox, false, outcome.terminateErr
	}
	return rt.flushPendingUserIntent(ctx, p, append(outbox, outcome.dispatched...)), false, nil
}

func (rt *watchRuntime) handleBootstrapResultsClosed(
	ctx context.Context,
) ([]*synctypes.TrackedAction, bool, error) {
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

func (rt *watchRuntime) handleWatchBatch(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
	batch []synctypes.PathChanges,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		return outbox, true, nil
	}

	return append(outbox, rt.processBatch(ctx, batch, p.bl, p.mode, p.safety)...), false, nil
}

func (rt *watchRuntime) handleWatchWorkerResult(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
	workerResult *synctypes.WorkerResult,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		if rt.isDraining() {
			p.results = nil
			return outbox, rt.drainLoopDone(p), nil
		}
		if contextIsCanceled(ctx) {
			p.results = nil
			rt.beginWatchDrain(ctx, p, outbox)
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

func (rt *watchRuntime) handleWatchSkipped(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
	skipped []synctypes.SkippedItem,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		p.skippedCh = nil
		return outbox, false, nil
	}

	rt.recordSkippedItems(ctx, skipped)
	rt.clearResolvedSkippedItems(ctx, skipped)
	return outbox, false, nil
}

func (rt *watchRuntime) handleWatchReconcileResult(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
	result reconcileResult,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		p.reconcileResults = nil
		return outbox, false, nil
	}

	rt.applyReconcileResult(ctx, result)
	return outbox, false, nil
}

func (rt *watchRuntime) handleWatchObserverError(
	ctx context.Context,
	p *watchPipeline,
	outbox []*synctypes.TrackedAction,
	obsErr error,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		p.errs = nil
		return outbox, false, nil
	}

	rt.logObserverError(obsErr)

	if err := rt.handleObserverExit(p, ctx.Err() != nil); err != nil {
		return outbox, false, err
	}
	if p.activeObs == 0 {
		p.errs = nil
	}

	return outbox, false, nil
}

func (rt *watchRuntime) handleDrainingWorkerResult(
	ctx context.Context,
	p *watchPipeline,
	workerResult *synctypes.WorkerResult,
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

func firstOutbox(outbox []*synctypes.TrackedAction) *synctypes.TrackedAction {
	if len(outbox) == 0 {
		return nil
	}

	return outbox[0]
}
