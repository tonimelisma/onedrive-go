package sync

import (
	"context"
	"fmt"
)

func (rt *watchRuntime) handleWatchDispatchReady(dispatched *TrackedAction) {
	rt.markRunning(dispatched)
	rt.consumeOutboxHead()
}

func (rt *watchRuntime) handleWatchReplanReady(
	ctx context.Context,
	p *watchPipeline,
	batch DirtyBatch,
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
	return rt.applyRuntimeCompletion(ctx, p, completion)
}

func (rt *watchRuntime) handleWatchCompletionsClosed(
	ctx context.Context,
	p *watchPipeline,
) error {
	if contextIsCanceled(ctx) {
		p.completions = nil
		rt.beginWatchDrain(ctx, p)
		return nil
	}

	return fmt.Errorf("sync: action completions channel closed unexpectedly")
}

func (rt *watchRuntime) handleWatchLocalChange(change *ChangeEvent) {
	if change == nil || rt.dirtyBuf == nil {
		return
	}
	if change.Path != "" {
		rt.dirtyBuf.MarkPath(change.Path)
	}
	if change.OldPath != "" {
		rt.dirtyBuf.MarkPath(change.OldPath)
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
