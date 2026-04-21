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
	watchEventRecheckTick          watchEventKind = "recheck_tick"
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

type watchTransition struct {
	consumeOutboxHead bool
	markRunning       *TrackedAction
	appendOutbox      []*TrackedAction
	replaceOutbox     []*TrackedAction
	replaceOutboxSet  bool
	startRefresh      bool
	beginDrain        bool
	done              bool
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
	case <-p.recheckC:
		return watchEvent{kind: watchEventRecheckTick}
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

func (rt *watchRuntime) transitionWatchEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (watchTransition, error) {
	if transition, handled, err := rt.transitionWatchDispatchEvent(ctx, p, event); handled {
		return transition, err
	}

	if transition, handled, err := rt.transitionWatchObservationEvent(ctx, p, event); handled {
		return transition, err
	}

	if transition, handled, err := rt.transitionWatchMaintenanceEvent(ctx, p, event); handled {
		return transition, err
	}

	return watchTransition{}, nil
}

func (rt *watchRuntime) applyWatchTransition(
	ctx context.Context,
	p *watchPipeline,
	transition watchTransition,
) (bool, error) {
	if transition.beginDrain {
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}

	if transition.consumeOutboxHead {
		rt.markRunning(transition.markRunning)
		rt.consumeOutboxHead()
	}
	if transition.replaceOutboxSet {
		rt.replaceOutbox(transition.replaceOutbox)
	} else if len(transition.appendOutbox) > 0 {
		rt.appendOutbox(transition.appendOutbox)
	}
	if transition.startRefresh {
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
	}
	if transition.done {
		return true, nil
	}
	return false, nil
}

func (rt *watchRuntime) transitionWatchDispatchEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (watchTransition, bool, error) {
	switch event.kind {
	case watchEventDispatchReady:
		return watchTransition{
			consumeOutboxHead: true,
			markRunning:       event.dispatched,
		}, true, nil
	case watchEventBatchReady:
		if !rt.canPrepareNow() {
			rt.queueDirtyReplan(event.batch)
			return watchTransition{}, true, nil
		}
		return watchTransition{
			appendOutbox: rt.processDirtyBatch(ctx, event.batch, p.bl, p.mode),
		}, true, nil
	case watchEventBatchClosed:
		return watchTransition{done: true}, true, nil
	case watchEventActionCompletion:
		outcome := rt.processActionCompletion(ctx, rt, event.completion, p.bl)
		if outcome.terminate {
			rt.clearSyncStatusBatch()
			return watchTransition{}, true, outcome.terminateErr
		}

		nextOutbox, err := rt.reducePublicationFrontier(ctx, rt, p.bl, rt.currentOutbox(), outcome.dispatched)
		if err != nil {
			rt.clearSyncStatusBatch()
			rt.completeOutboxAsShutdown(nextOutbox)
			return watchTransition{}, true, err
		}
		rt.maybeFinishSyncStatusBatch(ctx, p.mode, nextOutbox)
		return watchTransition{
			replaceOutbox:    nextOutbox,
			replaceOutboxSet: true,
		}, true, nil
	case watchEventCompletionsClosed:
		if contextIsCanceled(ctx) {
			p.completions = nil
			return watchTransition{beginDrain: true}, true, nil
		}

		return watchTransition{}, true, fmt.Errorf("sync: action completions channel closed unexpectedly")
	case watchEventLocalChange,
		watchEventLocalEventsClosed,
		watchEventRemoteBatch,
		watchEventRemoteBatchesClosed,
		watchEventSkipped,
		watchEventSkippedClosed,
		watchEventRecheckTick,
		watchEventRefreshTick,
		watchEventRefreshResult,
		watchEventRefreshResultsClosed,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return watchTransition{}, false, nil
	}

	return watchTransition{}, false, nil
}

func (rt *watchRuntime) transitionWatchObservationEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (watchTransition, bool, error) {
	if handled, err := rt.transitionWatchObservationBatchEvent(ctx, p, event); handled {
		return watchTransition{}, handled, err
	}

	return rt.transitionWatchObservationMaintenanceEvent(ctx, p, event)
}

func (rt *watchRuntime) transitionWatchObservationBatchEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (bool, error) {
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
		return true, nil
	case watchEventLocalEventsClosed:
		p.localEvents = nil
		return true, nil
	case watchEventRemoteBatch:
		if event.remoteBatch == nil {
			return true, nil
		}
		return true, rt.handleRemoteObservationBatch(ctx, event.remoteBatch)
	case watchEventRemoteBatchesClosed:
		p.remoteBatches = nil
		return true, nil
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventActionCompletion,
		watchEventCompletionsClosed,
		watchEventSkipped,
		watchEventSkippedClosed,
		watchEventRecheckTick,
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

func (rt *watchRuntime) transitionWatchObservationMaintenanceEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (watchTransition, bool, error) {
	switch event.kind {
	case watchEventSkipped:
		rt.reconcileSkippedObservationFindings(ctx, rt, event.skipped)
		return watchTransition{}, true, nil
	case watchEventSkippedClosed:
		p.skippedCh = nil
		return watchTransition{}, true, nil
	case watchEventRefreshTick:
		return watchTransition{startRefresh: true}, true, nil
	case watchEventRefreshResult:
		return watchTransition{}, true, rt.applyRemoteRefreshResult(ctx, &event.refreshResult)
	case watchEventRefreshResultsClosed:
		p.refreshResults = nil
		return watchTransition{}, true, nil
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventActionCompletion,
		watchEventCompletionsClosed,
		watchEventLocalChange,
		watchEventLocalEventsClosed,
		watchEventRemoteBatch,
		watchEventRemoteBatchesClosed,
		watchEventRecheckTick,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return watchTransition{}, false, nil
	}

	return watchTransition{}, false, nil
}

func (rt *watchRuntime) transitionWatchMaintenanceEvent(
	ctx context.Context,
	p *watchPipeline,
	event *watchEvent,
) (watchTransition, bool, error) {
	switch event.kind {
	case watchEventRecheckTick:
		rt.handleRecheckTick(ctx)
		return watchTransition{}, true, nil
	case watchEventObserverError:
		if isFatalObserverError(event.observerErr) {
			return watchTransition{}, true, event.observerErr
		}

		rt.logObserverError(event.observerErr)
		if err := rt.handleObserverExit(p, ctx.Err() != nil); err != nil {
			return watchTransition{}, true, err
		}
		if p.activeObs == 0 {
			p.errs = nil
		}

		return watchTransition{}, true, nil
	case watchEventObserverErrorsClosed:
		p.errs = nil
		return watchTransition{}, true, nil
	case watchEventTrialTick:
		if rt.hasPendingDirtyReplan() {
			return watchTransition{}, true, nil
		}
		return watchTransition{
			appendOutbox: rt.runTrialDispatch(ctx, p.bl, p.mode),
		}, true, nil
	case watchEventRetryTick:
		if rt.hasPendingDirtyReplan() {
			return watchTransition{}, true, nil
		}
		return watchTransition{
			appendOutbox: rt.runRetrierSweep(ctx, p.bl, p.mode),
		}, true, nil
	case watchEventContextCanceled:
		return watchTransition{beginDrain: true}, true, nil
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
		return watchTransition{}, false, nil
	}

	return watchTransition{}, false, nil
}
