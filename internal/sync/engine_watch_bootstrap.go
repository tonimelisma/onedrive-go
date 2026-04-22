package sync

import (
	"context"
	"log/slog"
	"time"
)

func (rt *watchRuntime) runBootstrapStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	// Bootstrap phase: dispatch, buffered bootstrap replans, action
	// completions, retry/trial wakeups, and quiescence logging are live until
	// all work due now has drained from the shared runtime.
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		rt.markRunning(nextAction)
		rt.consumeOutboxHead()
		return false, nil
	case batch, ok := <-p.replanReady:
		return rt.handleBootstrapReplan(ctx, p, batch, ok)
	case completion, ok := <-p.completions:
		return rt.handleBootstrapCompletion(ctx, p, &completion, ok)
	case <-rt.trialTimerChan():
		return false, rt.releaseHeldFrontier(ctx, p, true)
	case <-rt.retryTimerChan():
		return false, rt.releaseHeldFrontier(ctx, p, false)
	case <-logC:
		rt.logBootstrapWait()
		return false, nil
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}
}

func (rt *watchRuntime) handleBootstrapReplan(
	ctx context.Context,
	p *watchPipeline,
	batch DirtyBatch,
	ok bool,
) (bool, error) {
	if !ok {
		return true, nil
	}

	if !rt.canPrepareNow() {
		rt.queuePendingReplan(batch)
		return false, nil
	}

	if err := rt.handleWatchReplanReady(ctx, p, batch); err != nil {
		return false, err
	}

	return false, nil
}

func (rt *watchRuntime) handleBootstrapCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
	ok bool,
) (bool, error) {
	if !ok {
		if err := rt.handleWatchCompletionsClosed(ctx, p); err != nil {
			return false, err
		}
		if contextIsCanceled(ctx) {
			return rt.drainLoopDone(p), nil
		}
		return false, nil
	}

	return false, rt.handleWatchActionCompletion(ctx, p, completion)
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
