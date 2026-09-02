package sync

import "context"

func (r *oneShotRunner) runResultsLoopWithInitialOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan actionCompletion,
	initialOutbox []*trackedAction,
) error {
	outbox := append([]*trackedAction(nil), initialOutbox...)
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
	completions <-chan actionCompletion,
	outbox []*trackedAction,
	fatalErr error,
) ([]*trackedAction, error, bool) {
	if len(outbox) != 0 || r.sched.runningCount != 0 {
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

func (r *oneShotRunner) finishResultsLoopIfSettled(outbox []*trackedAction, fatalErr error) (bool, error) {
	switch {
	case fatalErr == nil && len(outbox) == 0 && r.sched.runningCount == 0 && !r.hasDueHeldWork(r.deps.now()):
		return true, nil
	case fatalErr != nil && len(outbox) == 0 && r.sched.runningCount == 0:
		return true, fatalErr
	default:
		return false, nil
	}
}

func (r *oneShotRunner) runResultsLoopIdle(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan actionCompletion,
	fatalErr error,
) ([]*trackedAction, error, bool) {
	if nextOutbox, nextFatal, handled := r.releaseIdleDueHeldWork(ctx, bl); handled {
		return nextOutbox, nextFatal, false
	}

	select {
	case completion, ok := <-completions:
		if !ok {
			return nil, fatalErr, true
		}
		nextOutbox, nextFatal := r.handleOneShotCompletion(ctx, cancel, bl, nil, fatalErr, &completion)
		return nextOutbox, nextFatal, false
	case <-resultsLoopCtxDone(ctx, fatalErr):
		return nil, fatalErr, r.sched.runningCount == 0
	}
}

func (r *oneShotRunner) releaseIdleDueHeldWork(
	ctx context.Context,
	bl *Baseline,
) ([]*trackedAction, error, bool) {
	if r.sched.runningCount != 0 || !r.hasDueHeldWork(r.deps.now()) {
		return nil, nil, false
	}

	outbox, err := r.reduceReadyFrontierStage(ctx, bl, nil)
	if err != nil {
		r.completeOutboxAsShutdown(outbox)
		return nil, err, true
	}

	return outbox, err, true
}

func (r *oneShotRunner) runResultsLoopWithOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan actionCompletion,
	outbox []*trackedAction,
	fatalErr error,
) ([]*trackedAction, error, bool) {
	select {
	case r.sched.dispatchCh <- outbox[0]:
		r.sched.markRunning(outbox[0])
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
	outbox []*trackedAction,
	fatalErr error,
	completion *actionCompletion,
) ([]*trackedAction, error) {
	if fatalErr != nil || ctx.Err() != nil {
		if err := r.processShutdownCompletion(ctx, completion, bl); err != nil {
			r.logSuppressedShutdownCompletionError(completion, err)
		}
		return outbox, fatalErr
	}

	ready, completionErr := r.applyRuntimeCompletionStage(ctx, completion, bl)
	if completionErr == nil {
		return append(outbox, ready...), nil
	} else {
		outbox = append(outbox, ready...)
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

func (r *oneShotRunner) completeQueuedDispatchAsShutdown() {
	for {
		select {
		case ta := <-r.sched.dispatchCh:
			r.completeTrackedActionAsShutdown(ta)
		default:
			return
		}
	}
}

func resultsLoopCtxDone(ctx context.Context, fatalErr error) <-chan struct{} {
	if fatalErr != nil {
		return nil
	}

	return ctx.Done()
}
