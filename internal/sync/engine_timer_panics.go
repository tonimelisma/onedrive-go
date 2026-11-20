package sync

import (
	"context"
	"errors"
)

// beginRun binds a run to the engine's timer panics. The run runs under the
// returned context, which a timer callback's panic cancels; end, called as
// the run returns, unbinds it and returns the run's error with that panic
// (withTimerPanic). RunOnce and RunWatch bind; nothing inside them does. The
// returned context carries the engine's perf collector (withOwnCollector).
func (e *Engine) beginRun(ctx context.Context) (context.Context, func(error) error) {
	runCtx, cancel := context.WithCancel(e.withOwnCollector(ctx))
	e.wallClock.panics.Bind(cancel)
	if e.ownerCheckFactory != nil {
		e.runOwnerCheck = e.ownerCheckFactory()
	}

	return runCtx, func(err error) error {
		e.runOwnerCheck = nil
		panicErr := e.wallClock.panics.Unbind()
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
	case err == nil, IsCancellationEcho(err):
		return panicErr
	default:
		return errors.Join(panicErr, err)
	}
}
