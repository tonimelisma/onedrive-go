package sync

import (
	"context"
	"fmt"
)

type watchEventKind string

const (
	watchEventDispatchReady          watchEventKind = "dispatch_ready"
	watchEventBatchReady             watchEventKind = "batch_ready"
	watchEventBatchClosed            watchEventKind = "batch_closed"
	watchEventWorkerResult           watchEventKind = "worker_result"
	watchEventResultsClosed          watchEventKind = "results_closed"
	watchEventSkipped                watchEventKind = "skipped"
	watchEventSkippedClosed          watchEventKind = "skipped_closed"
	watchEventRecheckTick            watchEventKind = "recheck_tick"
	watchEventReconcileTick          watchEventKind = "reconcile_tick"
	watchEventReconcileResult        watchEventKind = "reconcile_result"
	watchEventReconcileResultsClosed watchEventKind = "reconcile_results_closed"
	watchEventObserverError          watchEventKind = "observer_error"
	watchEventObserverErrorsClosed   watchEventKind = "observer_errors_closed"
	watchEventTrialTick              watchEventKind = "trial_tick"
	watchEventRetryTick              watchEventKind = "retry_tick"
	watchEventContextCanceled        watchEventKind = "context_canceled"
)

type watchEvent struct {
	kind            watchEventKind
	batch           []PathChanges
	workerResult    *WorkerResult
	skipped         []SkippedItem
	reconcileResult reconcileResult
	observerErr     error
}

type watchTransition struct {
	consumeOutboxHead bool
	appendOutbox      []*TrackedAction
	startReconcile    bool
	beginDrain        bool
	done              bool
}

//nolint:gocyclo // The watch loop deliberately centralizes all event decoding in one owner boundary.
func (rt *watchRuntime) waitWatchEvent(ctx context.Context, p *watchPipeline) watchEvent {
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		return watchEvent{kind: watchEventDispatchReady}
	case batch, ok := <-p.batchReady:
		if !ok {
			return watchEvent{kind: watchEventBatchClosed}
		}

		return watchEvent{kind: watchEventBatchReady, batch: batch}
	case workerResult, ok := <-p.results:
		if !ok {
			return watchEvent{kind: watchEventResultsClosed}
		}

		return watchEvent{kind: watchEventWorkerResult, workerResult: &workerResult}
	case skipped, ok := <-p.skippedCh:
		if !ok {
			return watchEvent{kind: watchEventSkippedClosed}
		}

		return watchEvent{kind: watchEventSkipped, skipped: skipped}
	case <-p.recheckC:
		return watchEvent{kind: watchEventRecheckTick}
	case <-p.reconcileC:
		return watchEvent{kind: watchEventReconcileTick}
	case result, ok := <-p.reconcileResults:
		if !ok {
			return watchEvent{kind: watchEventReconcileResultsClosed}
		}

		return watchEvent{kind: watchEventReconcileResult, reconcileResult: result}
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

	if transition, handled := rt.transitionWatchObservationEvent(ctx, p, event); handled {
		return transition, nil
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
		rt.consumeOutboxHead()
	}
	if len(transition.appendOutbox) > 0 {
		rt.appendOutbox(transition.appendOutbox)
	}
	if transition.startReconcile {
		rt.runFullReconciliationAsync(ctx, p.bl)
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
		return watchTransition{consumeOutboxHead: true}, true, nil
	case watchEventBatchReady:
		return watchTransition{
			appendOutbox: rt.processBatch(ctx, event.batch, p.bl, p.mode, p.safety),
		}, true, nil
	case watchEventBatchClosed:
		return watchTransition{done: true}, true, nil
	case watchEventWorkerResult:
		outcome := rt.processWorkerResult(ctx, rt, event.workerResult, p.bl)
		if outcome.terminate {
			return watchTransition{}, true, outcome.terminateErr
		}

		return watchTransition{appendOutbox: outcome.dispatched}, true, nil
	case watchEventResultsClosed:
		if contextIsCanceled(ctx) {
			p.results = nil
			return watchTransition{beginDrain: true}, true, nil
		}

		return watchTransition{}, true, fmt.Errorf("sync: worker results channel closed unexpectedly")
	case watchEventSkipped,
		watchEventSkippedClosed,
		watchEventRecheckTick,
		watchEventReconcileTick,
		watchEventReconcileResult,
		watchEventReconcileResultsClosed,
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
) (watchTransition, bool) {
	switch event.kind {
	case watchEventSkipped:
		rt.recordSkippedItems(ctx, event.skipped)
		rt.clearResolvedSkippedItems(ctx, event.skipped)
		return watchTransition{}, true
	case watchEventSkippedClosed:
		p.skippedCh = nil
		return watchTransition{}, true
	case watchEventReconcileTick:
		return watchTransition{startReconcile: true}, true
	case watchEventReconcileResult:
		rt.applyReconcileResult(event.reconcileResult)
		return watchTransition{}, true
	case watchEventReconcileResultsClosed:
		p.reconcileResults = nil
		return watchTransition{}, true
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventWorkerResult,
		watchEventResultsClosed,
		watchEventRecheckTick,
		watchEventObserverError,
		watchEventObserverErrorsClosed,
		watchEventTrialTick,
		watchEventRetryTick,
		watchEventContextCanceled:
		return watchTransition{}, false
	}

	return watchTransition{}, false
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
		return watchTransition{
			appendOutbox: rt.runTrialDispatch(ctx, p.bl, p.mode, p.safety),
		}, true, nil
	case watchEventRetryTick:
		return watchTransition{
			appendOutbox: rt.runRetrierSweep(ctx, p.bl, p.mode, p.safety),
		}, true, nil
	case watchEventContextCanceled:
		return watchTransition{beginDrain: true}, true, nil
	case watchEventDispatchReady,
		watchEventBatchReady,
		watchEventBatchClosed,
		watchEventWorkerResult,
		watchEventResultsClosed,
		watchEventSkipped,
		watchEventSkippedClosed,
		watchEventReconcileTick,
		watchEventReconcileResult,
		watchEventReconcileResultsClosed:
		return watchTransition{}, false, nil
	}

	return watchTransition{}, false, nil
}
