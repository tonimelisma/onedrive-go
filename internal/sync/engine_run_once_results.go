package sync

import "context"

// runResultsLoopWithInitialOutbox drives one pass's workers from the
// runner-owned outbox until the runtime settles. The outbox is a field, not a
// value threaded through the loop, because a completion applied inside the
// loop can take admitted work back out of it: a scope that activates retires
// what it blocks before a worker reaches it (R-6.8.24).
func (r *oneShotRunner) runResultsLoopWithInitialOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan actionCompletion,
	initialOutbox []*trackedAction,
) error {
	r.outbox = append([]*trackedAction(nil), initialOutbox...)
	var fatalErr error

	for {
		if fatalErr != nil && len(r.outbox) > 0 {
			r.completeOutboxAsShutdown(r.outbox)
			r.outbox = nil
			continue
		}
		if nextFatal, handled := r.pollImmediateCompletion(ctx, cancel, bl, completions, fatalErr); handled {
			fatalErr = nextFatal
			continue
		}
		if done, err := r.finishResultsLoopIfSettled(fatalErr); done {
			return err
		}

		if len(r.outbox) == 0 {
			nextFatal, done := r.runResultsLoopIdle(ctx, cancel, bl, completions, fatalErr)
			fatalErr = nextFatal
			if done {
				return fatalErr
			}
			continue
		}

		nextFatal, done := r.runResultsLoopWithOutbox(ctx, cancel, bl, completions, fatalErr)
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
	fatalErr error,
) (error, bool) {
	if len(r.outbox) != 0 || r.sched.runningCount != 0 {
		return fatalErr, false
	}

	select {
	case completion, ok := <-completions:
		if !ok {
			return fatalErr, false
		}
		return r.handleOneShotCompletion(ctx, cancel, bl, fatalErr, &completion), true
	default:
		return fatalErr, false
	}
}

func (r *oneShotRunner) finishResultsLoopIfSettled(fatalErr error) (bool, error) {
	switch {
	case fatalErr == nil && len(r.outbox) == 0 && r.sched.runningCount == 0 && !r.hasDueHeldWork(r.deps.now()):
		return true, nil
	case fatalErr != nil && len(r.outbox) == 0 && r.sched.runningCount == 0:
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
) (error, bool) {
	if nextFatal, handled := r.releaseIdleDueHeldWork(ctx, bl); handled {
		return nextFatal, false
	}

	select {
	case completion, ok := <-completions:
		if !ok {
			return fatalErr, true
		}
		return r.handleOneShotCompletion(ctx, cancel, bl, fatalErr, &completion), false
	case <-resultsLoopCtxDone(ctx, fatalErr):
		return fatalErr, r.sched.runningCount == 0
	}
}

// releaseIdleDueHeldWork moves due held work into the outbox while the
// runtime is otherwise idle. It reports whether it did anything; on a
// reduction failure the released work has already been completed as shutdown
// and the error is returned.
func (r *oneShotRunner) releaseIdleDueHeldWork(
	ctx context.Context,
	bl *Baseline,
) (error, bool) {
	if r.sched.runningCount != 0 || !r.hasDueHeldWork(r.deps.now()) {
		return nil, false
	}

	released, err := r.reduceReadyFrontierStage(ctx, bl, nil)
	if err != nil {
		r.completeOutboxAsShutdown(released)
		return err, true
	}
	r.outbox = append(r.outbox, released...)

	return nil, true
}

func (r *oneShotRunner) runResultsLoopWithOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	completions <-chan actionCompletion,
	fatalErr error,
) (error, bool) {
	// A finished outcome may change what the next dispatch is allowed to be
	// (a 429 activates a scope that takes the outbox back, R-6.8.24), so
	// outcomes already in hand are applied before more work reaches the
	// workers; otherwise an episode's 429s scale with the select's luck
	// rather than with the transfers in flight.
	select {
	case completion, ok := <-completions:
		if !ok {
			return fatalErr, true
		}
		return r.handleOneShotCompletion(ctx, cancel, bl, fatalErr, &completion), false
	default:
	}

	select {
	case r.sched.dispatchCh <- r.outbox[0]:
		r.sched.markRunning(r.outbox[0])
		r.outbox = r.outbox[1:]
		return fatalErr, false
	case completion, ok := <-completions:
		if !ok {
			return fatalErr, true
		}
		return r.handleOneShotCompletion(ctx, cancel, bl, fatalErr, &completion), false
	case <-resultsLoopCtxDone(ctx, fatalErr):
		return fatalErr, false
	}
}

// handleOneShotCompletion applies one completion to the runtime and appends
// the work it admitted to the outbox. It returns the pass's fatal error: the
// one it was given, or the one this completion produced, after which the
// outbox is completed as shutdown.
func (r *oneShotRunner) handleOneShotCompletion(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *Baseline,
	fatalErr error,
	completion *actionCompletion,
) error {
	if fatalErr != nil || ctx.Err() != nil {
		if err := r.processShutdownCompletion(ctx, completion, bl); err != nil {
			r.logSuppressedShutdownCompletionError(completion, err)
		}
		return fatalErr
	}

	ready, completionErr := r.applyRuntimeCompletionStage(ctx, completion, bl)
	r.outbox = append(r.outbox, ready...)
	if completionErr == nil {
		return nil
	}

	if cancel != nil {
		cancel()
	}
	if len(r.outbox) > 0 {
		r.completeOutboxAsShutdown(r.outbox)
		r.outbox = nil
	}
	r.completeQueuedDispatchAsShutdown()

	return completionErr
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
