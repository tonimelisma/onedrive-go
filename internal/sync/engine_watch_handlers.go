package sync

import (
	"context"
	"fmt"
)

func (rt *watchRuntime) handleWatchDispatchReady(dispatched *TrackedAction) {
	rt.markRunning(dispatched)
	rt.consumeOutboxHead()
}

func (rt *watchRuntime) handleWatchReplanChannel(
	ctx context.Context,
	p *watchPipeline,
	batch DirtyBatch,
	ok bool,
) (bool, error) {
	if !ok {
		return true, nil
	}

	return false, rt.handleWatchReplanReady(ctx, p, batch)
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

func (rt *watchRuntime) handleWatchCompletionChannel(
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

func (rt *watchRuntime) handleWatchLocalChangeChannel(
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

func (rt *watchRuntime) handleWatchRemoteBatchChannel(
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

func (rt *watchRuntime) handleWatchSkippedChannel(
	ctx context.Context,
	p *watchPipeline,
	skipped []SkippedItem,
	ok bool,
) (bool, error) {
	if !ok {
		p.skippedCh = nil
		return false, nil
	}

	rt.reconcileSkippedObservationFindings(ctx, rt, skipped)
	return false, nil
}

func (rt *watchRuntime) handleWatchRefreshResultChannel(
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

func (rt *watchRuntime) handleWatchObserverErrorChannel(
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
