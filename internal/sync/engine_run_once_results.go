package sync

import "context"

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
	f.completeSubtree(ready)
}

func (r *oneShotRunner) isOneShotQuiescent(outbox []*TrackedAction) bool {
	return len(outbox) == 0 && r.runningCount == 0 && !r.hasDueHeldWork(r.engine.nowFunc())
}
