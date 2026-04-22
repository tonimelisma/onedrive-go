package sync

import (
	"context"
	"fmt"
)

type watchEventKind string

const (
	watchEventDispatchReady        watchEventKind = "dispatch_ready"
	watchEventReplanReady          watchEventKind = "replan_ready"
	watchEventReplanClosed         watchEventKind = "replan_closed"
	watchEventActionCompletion     watchEventKind = "action_completion"
	watchEventCompletionsClosed    watchEventKind = "completions_closed"
	watchEventLocalChange          watchEventKind = "local_change"
	watchEventLocalEventsClosed    watchEventKind = "local_events_closed"
	watchEventRemoteBatch          watchEventKind = "remote_batch"
	watchEventRemoteBatchesClosed  watchEventKind = "remote_batches_closed"
	watchEventSkipped              watchEventKind = "skipped"
	watchEventSkippedClosed        watchEventKind = "skipped_closed"
	watchEventMaintenanceTick      watchEventKind = "maintenance_tick"
	watchEventRefreshTick          watchEventKind = "refresh_tick"
	watchEventRefreshResult        watchEventKind = "refresh_result"
	watchEventRefreshResultsClosed watchEventKind = "refresh_results_closed"
	watchEventObserverError        watchEventKind = "observer_error"
	watchEventObserverErrorsClosed watchEventKind = "observer_errors_closed"
	watchEventTrialTick            watchEventKind = "trial_tick"
	watchEventRetryTick            watchEventKind = "retry_tick"
	watchEventContextCanceled      watchEventKind = "context_canceled"
)

type watchEvent struct {
	kind          watchEventKind
	batch         DirtyBatch
	change        *ChangeEvent
	remoteBatch   *remoteObservationBatch
	dispatched    *TrackedAction
	completion    *ActionCompletion
	skipped       []SkippedItem
	refreshResult remoteRefreshResult
	observerErr   error
}

//nolint:gocyclo // The watch loop deliberately centralizes all event decoding in one owner boundary.
func (rt *watchRuntime) waitWatchEvent(ctx context.Context, p *watchPipeline) watchEvent {
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		return watchEvent{kind: watchEventDispatchReady, dispatched: nextAction}
	case batch, ok := <-p.replanReady:
		if !ok {
			return watchEvent{kind: watchEventReplanClosed}
		}

		return watchEvent{kind: watchEventReplanReady, batch: batch}
	case change, ok := <-p.localEvents:
		if !ok {
			return watchEvent{kind: watchEventLocalEventsClosed}
		}

		return watchEvent{kind: watchEventLocalChange, change: &change}
	case batch, ok := <-p.remoteBatches:
		if !ok {
			return watchEvent{kind: watchEventRemoteBatchesClosed}
		}

		return watchEvent{kind: watchEventRemoteBatch, remoteBatch: &batch}
	case completion, ok := <-p.completions:
		if !ok {
			return watchEvent{kind: watchEventCompletionsClosed}
		}

		return watchEvent{kind: watchEventActionCompletion, completion: &completion}
	case skipped, ok := <-p.skippedCh:
		if !ok {
			return watchEvent{kind: watchEventSkippedClosed}
		}

		return watchEvent{kind: watchEventSkipped, skipped: skipped}
	case <-p.maintenanceC:
		return watchEvent{kind: watchEventMaintenanceTick}
	case <-p.refreshC:
		return watchEvent{kind: watchEventRefreshTick}
	case result, ok := <-p.refreshResults:
		if !ok {
			return watchEvent{kind: watchEventRefreshResultsClosed}
		}

		return watchEvent{kind: watchEventRefreshResult, refreshResult: result}
	case obsErr, ok := <-p.errs:
		if !ok {
			return watchEvent{kind: watchEventObserverErrorsClosed}
		}

		return watchEvent{kind: watchEventObserverError, observerErr: obsErr}
	case <-rt.trialTimerChan():
		return watchEvent{kind: watchEventTrialTick}
	case <-rt.retryTimerChan():
		return watchEvent{kind: watchEventRetryTick}
	case <-ctx.Done():
		return watchEvent{kind: watchEventContextCanceled}
	}
}

//nolint:gocyclo,funlen // The watch loop intentionally keeps one direct owner switch over all watch events.
func (rt *watchRuntime) handleWatchEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (bool, error) {
	switch event.kind {
	case watchEventDispatchReady:
		rt.markRunning(event.dispatched)
		rt.consumeOutboxHead()
		return false, nil
	case watchEventReplanReady:
		if !rt.canPrepareNow() {
			rt.queuePendingReplan(event.batch)
			return false, nil
		}
		return false, rt.runSteadyStateReplan(ctx, p, event.batch)
	case watchEventReplanClosed:
		return true, nil
	case watchEventActionCompletion:
		return false, rt.handleWatchActionCompletion(ctx, p, event.completion)
	case watchEventCompletionsClosed:
		return false, rt.handleWatchCompletionsClosed(ctx, p)
	case watchEventLocalChange:
		rt.handleWatchLocalChange(event.change)
		return false, nil
	case watchEventLocalEventsClosed:
		p.localEvents = nil
		return false, nil
	case watchEventRemoteBatch:
		if event.remoteBatch == nil {
			return false, nil
		}
		return false, rt.handleRemoteObservationBatch(ctx, event.remoteBatch)
	case watchEventRemoteBatchesClosed:
		p.remoteBatches = nil
		return false, nil
	case watchEventSkipped:
		rt.reconcileSkippedObservationFindings(ctx, rt, event.skipped)
		return false, nil
	case watchEventSkippedClosed:
		p.skippedCh = nil
		return false, nil
	case watchEventRefreshTick:
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return false, nil
	case watchEventRefreshResult:
		return false, rt.applyRemoteRefreshResult(ctx, &event.refreshResult)
	case watchEventRefreshResultsClosed:
		p.refreshResults = nil
		return false, nil
	case watchEventMaintenanceTick:
		rt.handleMaintenanceTick(ctx)
		return false, nil
	case watchEventObserverError:
		return false, rt.handleWatchObserverError(ctx, p, event.observerErr)
	case watchEventObserverErrorsClosed:
		p.errs = nil
		return false, nil
	case watchEventTrialTick:
		return false, rt.handleWatchHeldRelease(ctx, p, true)
	case watchEventRetryTick:
		return false, rt.handleWatchHeldRelease(ctx, p, false)
	case watchEventContextCanceled:
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}

	return false, nil
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
