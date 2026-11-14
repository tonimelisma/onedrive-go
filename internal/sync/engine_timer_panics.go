package sync

import (
	"context"
	"errors"
)

// beginRun binds a run to the engine's timer panics. The run runs under the
// returned context, which a timer callback's panic cancels; end, called as
// the run returns, unbinds it and returns the run's error with that panic
// (withTimerPanic). RunOnce and RunWatch bind; nothing inside them does.
func (e *Engine) beginRun(ctx context.Context) (context.Context, func(error) error) {
	runCtx, cancel := context.WithCancel(ctx)
	e.wallClock.panics.bind(cancel)

	return runCtx, func(err error) error {
		panicErr := e.wallClock.panics.unbind()
		cancel()

		return withTimerPanic(err, panicErr)
	}
}

// withTimerPanic is a run's error once a timer callback's panic ended the
// run: the panic's error alone when the run ended without an error or in the
// shutdown the panic's cancel caused, both joined otherwise.
func withTimerPanic(err, panicErr error) error {
	switch {
	case panicErr == nil:
		return err
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return panicErr
	default:
		return errors.Join(panicErr, err)
	}
}
