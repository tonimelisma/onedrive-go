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
	completions <-chan ActionCompletion,
) error {
	return r.runResultsLoopWithInitialOutbox(ctx, cancel, bl, completions, nil)
}

func (r *oneShotRunner) runResultsLoopWithInitialOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan ActionCompletion,
	initialOutbox []*TrackedAction,
) error {
	outbox := append([]*TrackedAction(nil), initialOutbox...)
	var fatalErr error

	for {
		if fatalErr != nil && len(outbox) > 0 {
			r.completeOutboxAsShutdown(outbox)
			outbox = nil
			continue
		}
		if nextOutbox, nextFatal, handled := r.pollImmediateCompletion(ctx, cancel, bl, completions, outbox, fatalErr); handled {
			outbox = nextOutbox
			fatalErr = nextFatal
			continue
		}
		if done, err := r.finishResultsLoopIfSettled(outbox, fatalErr); done {
			return err
		}

		if len(outbox) == 0 {
			nextOutbox, nextFatal, done := r.runResultsLoopIdle(ctx, cancel, bl, completions, fatalErr)
			outbox = nextOutbox
			fatalErr = nextFatal
			if done {
				return fatalErr
			}
			continue
		}

		nextOutbox, nextFatal, done := r.runResultsLoopWithOutbox(ctx, cancel, bl, completions, outbox, fatalErr)
		outbox = nextOutbox
		fatalErr = nextFatal
		if done {
			return fatalErr
		}
	}
}

func (r *oneShotRunner) pollImmediateCompletion(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan ActionCompletion,
	outbox []*TrackedAction,
	fatalErr error,
) ([]*TrackedAction, error, bool) {
	if len(outbox) != 0 || r.runningCount != 0 {
		return nil, fatalErr, false
	}

	select {
	case completion, ok := <-completions:
		if !ok {
			return nil, fatalErr, false
		}
		nextOutbox, nextFatal := r.handleOneShotCompletion(ctx, cancel, bl, nil, fatalErr, &completion)
		return nextOutbox, nextFatal, true
	default:
		return nil, fatalErr, false
	}
}

func (r *oneShotRunner) finishResultsLoopIfSettled(outbox []*TrackedAction, fatalErr error) (bool, error) {
	switch {
	case fatalErr == nil && r.isOneShotQuiescent(outbox):
		return true, nil
	case fatalErr != nil && len(outbox) == 0 && r.runningCount == 0:
		return true, fatalErr
	default:
		return false, nil
	}
}

func (r *oneShotRunner) runResultsLoopIdle(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan ActionCompletion,
	fatalErr error,
) ([]*TrackedAction, error, bool) {
	select {
	case completion, ok := <-completions:
		if !ok {
			return nil, fatalErr, true
		}
		nextOutbox, nextFatal := r.handleOneShotCompletion(ctx, cancel, bl, nil, fatalErr, &completion)
		return nextOutbox, nextFatal, false
	case <-resultsLoopCtxDone(ctx, fatalErr):
		return nil, fatalErr, r.runningCount == 0
	}
}

func (r *oneShotRunner) runResultsLoopWithOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan ActionCompletion,
	outbox []*TrackedAction,
	fatalErr error,
) ([]*TrackedAction, error, bool) {
	select {
	case r.dispatchCh <- outbox[0]:
		r.markRunning(outbox[0])
		return outbox[1:], fatalErr, false
	case completion, ok := <-completions:
		if !ok {
			return outbox, fatalErr, true
		}
		nextOutbox, nextFatal := r.handleOneShotCompletion(ctx, cancel, bl, outbox, fatalErr, &completion)
		return nextOutbox, nextFatal, false
	case <-resultsLoopCtxDone(ctx, fatalErr):
		return outbox, fatalErr, false
	}
}

func (r *oneShotRunner) handleOneShotCompletion(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	outbox []*TrackedAction,
	fatalErr error,
	completion *ActionCompletion,
) ([]*TrackedAction, error) {
	ready, completionErr := r.processActionCompletion(ctx, nil, completion, bl)
	if completionErr == nil {
		reduced, err := r.reduceReadyFrontier(ctx, nil, bl, ready)
		if err == nil || fatalErr != nil {
			return append(outbox, reduced...), fatalErr
		}
		outbox = append(outbox, reduced...)
		fatalErr = err
	} else {
		outbox = append(outbox, ready...)
		if fatalErr != nil {
			return outbox, fatalErr
		}
		fatalErr = completionErr
	}

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

	f.markFinished(ta)
	ready := f.completeDepGraphAction(ta.ID, "completeTrackedActionAsShutdown")
	f.scopeController().completeSubtree(ready)
}

func (r *oneShotRunner) isOneShotQuiescent(outbox []*TrackedAction) bool {
	return len(outbox) == 0 && r.runningCount == 0 && !r.hasDueHeldWork(r.engine.nowFunc())
}

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

	rt.replaceOutbox(initialOutbox)
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

		if rt.isBootstrapQuiescent() {
			return nil
		}

		done, err := rt.runBootstrapStep(ctx, p, logC)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// runWatchLoop owns steady-state watch execution. The same goroutine handles
// observed batches, action completions, retry/trial timers, reconcile completions,
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
		replanned, err := rt.runPendingDirtyReplan(ctx, p)
		if err != nil {
			return err
		}
		if replanned {
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

func (rt *watchRuntime) runPendingDirtyReplan(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	if !rt.canPrepareNow() {
		return false, nil
	}

	batch, ok := rt.takePendingDirtyReplan()
	if !ok {
		return false, nil
	}

	return true, rt.runSteadyStateReplan(ctx, p, batch)
}

func (rt *watchRuntime) runWatchStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	event := rt.waitWatchEvent(ctx, p)
	return rt.handleWatchEvent(ctx, p, &event)
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
	p.batchReady = nil
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
	// Draining phase: no new work admission remains live. Only action completions,
	// refresh result cleanup, and terminal observer exit/error handling may run.
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
) (bool, error) {
	outbox := rt.currentOutbox()

	// Bootstrap phase: dispatch, buffered bootstrap batches, action completions,
	// retry/trial wakeups, and quiescence logging are live until all work due
	// now has drained from the shared runtime.
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		rt.markRunning(nextAction)
		rt.consumeOutboxHead()
		return false, nil
	case batch, ok := <-p.batchReady:
		nextOutbox, done, err := rt.handleBootstrapBatch(ctx, p, outbox, batch, ok)
		rt.replaceOutbox(nextOutbox)
		return done, err
	case completion, ok := <-p.completions:
		nextOutbox, done, err := rt.handleBootstrapCompletion(ctx, p, outbox, &completion, ok)
		rt.replaceOutbox(nextOutbox)
		return done, err
	case <-rt.trialTimerChan():
		released, err := rt.releaseDueHeldTrialsNow(ctx)
		if err != nil {
			return false, err
		}
		reduced, err := rt.reduceReadyFrontier(ctx, rt, p.bl, released)
		nextOutbox := append(rt.currentOutbox(), reduced...)
		rt.replaceOutbox(nextOutbox)
		return false, err
	case <-rt.retryTimerChan():
		released, err := rt.releaseDueHeldRetriesNow(ctx)
		if err != nil {
			return false, err
		}
		reduced, err := rt.reduceReadyFrontier(ctx, rt, p.bl, released)
		nextOutbox := append(rt.currentOutbox(), reduced...)
		rt.replaceOutbox(nextOutbox)
		return false, err
	case <-logC:
		rt.logBootstrapWait()
		return false, nil
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
	batch DirtyBatch,
	ok bool,
) ([]*TrackedAction, bool, error) {
	if !ok {
		return outbox, true, nil
	}

	if !rt.canPrepareNow() {
		rt.queueDirtyReplan(batch)
		return outbox, false, nil
	}

	if err := rt.runSteadyStateReplan(ctx, p, batch); err != nil {
		return nil, false, err
	}

	return rt.currentOutbox(), false, nil
}

func (rt *watchRuntime) handleBootstrapCompletion(
	ctx context.Context,
	p *watchPipeline,
	outbox []*TrackedAction,
	completion *ActionCompletion,
	ok bool,
) ([]*TrackedAction, bool, error) {
	if !ok {
		if contextIsCanceled(ctx) {
			p.completions = nil
			rt.beginWatchDrain(ctx, p)
			return nil, rt.drainLoopDone(p), nil
		}
		return rt.handleBootstrapResultsClosed(ctx)
	}

	ready, completionErr := rt.processActionCompletion(ctx, rt, completion, p.bl)
	if completionErr != nil {
		return append(outbox, ready...), false, completionErr
	}
	reduced, err := rt.reduceReadyFrontier(ctx, rt, p.bl, ready)
	outbox = append(outbox, reduced...)
	if err != nil {
		rt.completeOutboxAsShutdown(outbox)
		return nil, false, err
	}
	rt.maybeFinishSyncStatusBatch(ctx, p.mode, outbox)
	return outbox, false, nil
}

func (rt *watchRuntime) handleBootstrapResultsClosed(
	ctx context.Context,
) ([]*TrackedAction, bool, error) {
	select {
	case <-ctx.Done():
		return nil, false, fmt.Errorf("sync: watch bootstrap context done: %w", ctx.Err())
	default:
	}

	return nil, false, fmt.Errorf("sync: action completions channel closed unexpectedly")
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

func contextIsCanceled(ctx context.Context) bool {
	return ctx.Err() != nil
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

	ready, err := rt.processActionCompletion(ctx, rt, completion, p.bl)
	rt.completeOutboxAsShutdown(ready)
	if err != nil {
		return false, err
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

func firstOutbox(outbox []*TrackedAction) *TrackedAction {
	if len(outbox) == 0 {
		return nil
	}

	return outbox[0]
}
