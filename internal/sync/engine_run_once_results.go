package sync

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// runResultsLoopWithInitialOutbox drives one pass's workers from the
// runner-owned outbox until the runtime settles. The outbox is a field, not a
// value threaded through the loop, because a completion applied inside the
// loop can take admitted work back out of it: a scope that activates retires
// what it blocks before a worker reaches it (R-6.8.24). While the loop runs,
// the end of ctx is recorded (noteRunCancel), so a stop's bound starts at the
// cancel. A pass that stopped returns the store failure its drain ended with
// (endStopDrain) when nothing failed it before.
func (r *oneShotRunner) runResultsLoopWithInitialOutbox(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *syncdomain.Baseline,
	completions <-chan syncdomain.ActionCompletion,
	initialOutbox []*syncdomain.TrackedAction,
) error {
	r.outbox = append([]*syncdomain.TrackedAction(nil), initialOutbox...)
	stopNoting := r.noteRunCancel(ctx)
	defer stopNoting()
	err := r.runResultsLoop(ctx, cancel, bl, completions)
	drainErr := r.endStopDrain()
	if err != nil {
		return err
	}

	return drainErr
}

func (r *oneShotRunner) runResultsLoop(
	ctx context.Context,
	cancel context.CancelFunc,
	bl *syncdomain.Baseline,
	completions <-chan syncdomain.ActionCompletion,
) error {
	var fatalErr error

	for {
		if fatalErr != nil || ctx.Err() != nil {
			// The pass is stopping: the stop's one bound is set, measured
			// from the cancel (beginStopDrain), and work admitted but not
			// handed to a worker completes as shutdown instead of being
			// offered to the workers after the stop, as a watch's drain
			// seals its outbox.
			r.beginStopDrain()
			if len(r.outbox) > 0 {
				r.completeOutboxAsShutdown(r.outbox)
				r.outbox = nil
			}
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
	bl *syncdomain.Baseline,
	completions <-chan syncdomain.ActionCompletion,
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

// finishResultsLoopIfSettled ends the loop once nothing is in flight or
// admitted and, while the pass goes on, no held work is due. A stopping pass
// releases no due held work: its held actions stay for the next sync.
func (r *oneShotRunner) finishResultsLoopIfSettled(fatalErr error) (bool, error) {
	switch {
	case fatalErr == nil && len(r.outbox) == 0 && r.sched.runningCount == 0 &&
		(r.stop != nil || !r.hasDueHeldWork(r.deps.now())):
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
	bl *syncdomain.Baseline,
	completions <-chan syncdomain.ActionCompletion,
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
// runtime is otherwise idle and the pass is not stopping. It reports whether
// it did anything; on a reduction failure the released work has already been
// completed as shutdown and the error is returned.
func (r *oneShotRunner) releaseIdleDueHeldWork(
	ctx context.Context,
	bl *syncdomain.Baseline,
) (error, bool) {
	if r.stop != nil || r.sched.runningCount != 0 || !r.hasDueHeldWork(r.deps.now()) {
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
	bl *syncdomain.Baseline,
	completions <-chan syncdomain.ActionCompletion,
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

	now := r.deps.now()
	next, admitAt := r.nextDispatchable(r.outbox, now)
	var (
		dispatchCh chan<- *syncdomain.TrackedAction
		nextAction *syncdomain.TrackedAction
		admitDue   <-chan struct{}
	)
	switch {
	case next >= 0:
		dispatchCh, nextAction = r.sched.dispatchCh, r.outbox[next]
	case !admitAt.IsZero():
		// Members of a released ramp batch wait for their admission instant
		// inside the loop (R-6.8.24): a one-shot pass has no runtime
		// signals, and ending the pass with them parked would leave them
		// for the next sync. The wait is bounded by
		// releaseBatchAdmissionWindow, and a completion still interrupts it.
		due := make(chan struct{})
		timer := r.deps.clock.AfterFunc(admitAt.Sub(now), func() { close(due) })
		defer timer.Stop()
		admitDue = due
	case r.sched.runningCount == 0:
		// Every queued action is held back by the slow start, yet nothing
		// is in flight to finish and make room: the in-flight accounting is
		// broken. Said loudly rather than waited on forever.
		return fmt.Errorf("sync: dispatch stalled: %d queued actions, none admissible, nothing in flight",
			len(r.outbox)), false
	}

	select {
	case dispatchCh <- nextAction:
		r.outbox = r.markDispatched(r.outbox, next)
		return fatalErr, false
	case <-admitDue:
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
	bl *syncdomain.Baseline,
	fatalErr error,
	completion *syncdomain.ActionCompletion,
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

// resultsLoopCtxDone is the end of the pass's context for a select of the
// loop, until the pass is stopping. From then the loop waits for completions
// only, which end when the workers' exit closes their channel; a select on a
// context that stays done would return at once on every turn and spin.
func resultsLoopCtxDone(ctx context.Context, fatalErr error) <-chan struct{} {
	if fatalErr != nil || ctx.Err() != nil {
		return nil
	}

	return ctx.Done()
}
