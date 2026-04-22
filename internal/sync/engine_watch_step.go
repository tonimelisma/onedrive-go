package sync

import (
	"context"
	"fmt"
)

type watchEventKind string

const (
	watchEventDispatchReady        watchEventKind = "dispatch_ready"
	watchEventBatchReady           watchEventKind = "batch_ready"
	watchEventBatchClosed          watchEventKind = "batch_closed"
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
	case batch, ok := <-p.batchReady:
		if !ok {
			return watchEvent{kind: watchEventBatchClosed}
		}

		return watchEvent{kind: watchEventBatchReady, batch: batch}
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

func (rt *watchRuntime) handleWatchEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (bool, error) {
	switch event.kind {
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventActionCompletion,
		watchEventCompletionsClosed:
		return rt.handleDispatchEvent(ctx, p, event)
	case watchEventLocalChange,
		watchEventLocalEventsClosed,
		watchEventRemoteBatch,
		watchEventRemoteBatchesClosed,
		watchEventSkipped,
		watchEventSkippedClosed,
		watchEventRefreshTick,
		watchEventRefreshResult,
		watchEventRefreshResultsClosed:
		return false, rt.handleObservationEvent(ctx, p, event)
	case watchEventMaintenanceTick,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return false, rt.handleMaintenanceEvent(ctx, p, event)
	}

	return false, nil
}

func (rt *watchRuntime) handleDispatchEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (bool, error) {
	switch event.kind {
	case watchEventDispatchReady:
		rt.markRunning(event.dispatched)
		rt.consumeOutboxHead()
		return false, nil
	case watchEventBatchReady:
		if !rt.canPrepareNow() {
			rt.queueDirtyReplan(event.batch)
			return false, nil
		}
		rt.appendOutbox(rt.processDirtyBatch(ctx, event.batch, p.bl, p.mode))
		return false, nil
	case watchEventBatchClosed:
		return true, nil
	case watchEventActionCompletion:
		ready, completionErr := rt.processActionCompletion(ctx, rt, event.completion, p.bl)
		if completionErr != nil {
			rt.clearSyncStatusBatch()
			rt.completeOutboxAsShutdown(ready)
			return false, completionErr
		}

		reduced, err := rt.reduceReadyFrontier(ctx, rt, p.bl, ready)
		nextOutbox := append(rt.currentOutbox(), reduced...)
		if err != nil {
			rt.clearSyncStatusBatch()
			rt.completeOutboxAsShutdown(nextOutbox)
			return false, err
		}
		rt.maybeFinishSyncStatusBatch(ctx, p.mode, nextOutbox)
		rt.replaceOutbox(nextOutbox)
		return false, nil
	case watchEventCompletionsClosed:
		if contextIsCanceled(ctx) {
			p.completions = nil
			rt.beginWatchDrain(ctx, p)
			return false, nil
		}

		return false, fmt.Errorf("sync: action completions channel closed unexpectedly")
	case watchEventLocalChange,
		watchEventLocalEventsClosed,
		watchEventRemoteBatch,
		watchEventRemoteBatchesClosed,
		watchEventSkipped,
		watchEventSkippedClosed,
		watchEventMaintenanceTick,
		watchEventRefreshTick,
		watchEventRefreshResult,
		watchEventRefreshResultsClosed,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return false, nil
	}

	return false, nil
}

func (rt *watchRuntime) handleObservationEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) error {
	switch event.kind {
	case watchEventLocalChange,
		watchEventLocalEventsClosed,
		watchEventRemoteBatch,
		watchEventRemoteBatchesClosed,
		watchEventSkipped,
		watchEventSkippedClosed:
		return rt.handleObservationInputEvent(ctx, p, event)
	case watchEventRefreshTick,
		watchEventRefreshResult,
		watchEventRefreshResultsClosed:
		return rt.handleRefreshObservationEvent(ctx, p, event)
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventActionCompletion,
		watchEventCompletionsClosed,
		watchEventMaintenanceTick,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return nil
	}

	return nil
}

func (rt *watchRuntime) handleObservationInputEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) error {
	switch event.kind {
	case watchEventLocalChange:
		if event.change != nil && rt.dirtyBuf != nil {
			if event.change.Path != "" {
				rt.dirtyBuf.MarkPath(event.change.Path)
			}
			if event.change.OldPath != "" {
				rt.dirtyBuf.MarkPath(event.change.OldPath)
			}
		}
		return nil
	case watchEventLocalEventsClosed:
		p.localEvents = nil
		return nil
	case watchEventRemoteBatch:
		if event.remoteBatch == nil {
			return nil
		}
		return rt.handleRemoteObservationBatch(ctx, event.remoteBatch)
	case watchEventRemoteBatchesClosed:
		p.remoteBatches = nil
		return nil
	case watchEventSkipped:
		rt.reconcileSkippedObservationFindings(ctx, rt, event.skipped)
		return nil
	case watchEventSkippedClosed:
		p.skippedCh = nil
		return nil
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventActionCompletion,
		watchEventCompletionsClosed,
		watchEventMaintenanceTick,
		watchEventRefreshTick,
		watchEventRefreshResult,
		watchEventRefreshResultsClosed,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return nil
	}

	return nil
}

func (rt *watchRuntime) handleRefreshObservationEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) error {
	switch event.kind {
	case watchEventRefreshTick:
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return nil
	case watchEventRefreshResult:
		return rt.applyRemoteRefreshResult(ctx, &event.refreshResult)
	case watchEventRefreshResultsClosed:
		p.refreshResults = nil
		return nil
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventActionCompletion,
		watchEventCompletionsClosed,
		watchEventLocalChange,
		watchEventLocalEventsClosed,
		watchEventRemoteBatch,
		watchEventRemoteBatchesClosed,
		watchEventSkipped,
		watchEventSkippedClosed,
		watchEventMaintenanceTick,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return nil
	}

	return nil
}

func (rt *watchRuntime) handleMaintenanceEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) error {
	switch event.kind {
	case watchEventMaintenanceTick:
		rt.handleMaintenanceTick(ctx)
		return nil
	case watchEventObserverError:
		if isFatalObserverError(event.observerErr) {
			return event.observerErr
		}

		rt.logObserverError(event.observerErr)
		if err := rt.handleObserverExit(p, ctx.Err() != nil); err != nil {
			return err
		}
		if p.activeObs == 0 {
			p.errs = nil
		}

		return nil
	case watchEventObserverErrorsClosed:
		p.errs = nil
		return nil
	case watchEventTrialTick:
		if rt.hasPendingDirtyReplan() {
			return nil
		}
		released, err := rt.releaseDueHeldTrialsNow(ctx)
		if err != nil {
			return err
		}
		reduced, err := rt.reduceReadyFrontier(ctx, rt, p.bl, released)
		nextOutbox := append(rt.currentOutbox(), reduced...)
		rt.replaceOutbox(nextOutbox)
		return err
	case watchEventRetryTick:
		if rt.hasPendingDirtyReplan() {
			return nil
		}
		released, err := rt.releaseDueHeldRetriesNow(ctx)
		if err != nil {
			return err
		}
		reduced, err := rt.reduceReadyFrontier(ctx, rt, p.bl, released)
		nextOutbox := append(rt.currentOutbox(), reduced...)
		rt.replaceOutbox(nextOutbox)
		return err
	case watchEventContextCanceled:
		rt.beginWatchDrain(ctx, p)
		return nil
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventActionCompletion,
		watchEventCompletionsClosed,
		watchEventLocalChange,
		watchEventLocalEventsClosed,
		watchEventRemoteBatch,
		watchEventRemoteBatchesClosed,
		watchEventSkipped,
		watchEventSkippedClosed,
		watchEventRefreshTick,
		watchEventRefreshResult,
		watchEventRefreshResultsClosed:
		return nil
	}

	return nil
}
