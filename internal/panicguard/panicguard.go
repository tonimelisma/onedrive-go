// Package panicguard is how product code turns a recovered panic into one log
// record and an error. Every recover in onedrive-go calls Recovered, so a
// panic reads the same wherever it is caught: logged once at Error with the
// stack of the goroutine that panicked, and returned as an error that wraps
// ErrPanic. A boundary that ends its work through a cancel hands the cancel
// AsCancelCause of that error. What each boundary then does with the error is
// set by R-6.3.8 and R-6.3.10 (spec/requirements/non-functional.md); this
// package adds no rule of its own.
//
// It is a leaf package: it imports only the standard library, so the CLI, the
// Graph client and the sync packages can all use it.
package panicguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// ErrPanic marks an error produced from a recovered panic.
var ErrPanic = errors.New("internal error")

// panicError is what Recovered returns: where the panic was recovered, the
// value it carried and the stack of the goroutine that panicked.
type panicError struct {
	where string
	value any
	stack string
}

// Error is the user-facing account of the panic. The log record Recovered
// wrote is the diagnostic one.
func (e *panicError) Error() string {
	return fmt.Sprintf("%s %s: %v. This is a bug; please report it, with the log file if you have one",
		ErrPanic, e.where, e.value)
}

func (e *panicError) Unwrap() error {
	return ErrPanic
}

// Recovered logs a recovered panic at Error, with attrs, the panic value and
// the stack of the panicking goroutine, and returns an error wrapping
// ErrPanic. Call it only from the deferred function that called recover, with
// the value recover returned: a deferred call runs on the panicking
// goroutine's stack, so the stack taken here still holds the frames that
// panicked. That is why it is taken at all: the value names what failed but
// not where, and the goroutine that could answer is gone by the time anyone
// reads the error.
//
// where completes the sentence "Internal error ...", for example "in the
// watch loop" or "while syncing Documents". A nil logger logs through
// slog.Default, so a panic is never silent.
func Recovered(logger *slog.Logger, where string, r any, attrs ...slog.Attr) error {
	stack := string(debug.Stack())
	if logger == nil {
		logger = slog.Default()
	}

	args := make([]any, 0, len(attrs)+2)
	for i := range attrs {
		args = append(args, attrs[i])
	}
	args = append(args, slog.Any("panic", r), slog.String("stack", stack))
	logger.Error("Internal error "+where+"; this is a bug, please report it", args...)

	return &panicError{where: where, value: r, stack: stack}
}

// AsCancelCause is err, the error Recovered returned, as the cause of a
// cancel: the boundary of a helper goroutine with no error result ends the
// work it belongs to through the work's context. A request or a wait that the
// cancel interrupts hands back the context's cause in place of
// context.Canceled, so the cause answers to context.Canceled as well as to
// ErrPanic: the interrupted work reads as stopped, as at any other stop, and
// whoever reads the cause still finds the panic, its text and its stack. A nil
// err is nil, a cancel without a cause.
func AsCancelCause(err error) error {
	if err == nil {
		return nil
	}

	return &stopCauseError{err: err}
}

// stopCauseError is a recovered panic's error as the cause of a cancel.
type stopCauseError struct {
	err error
}

func (c *stopCauseError) Error() string { return c.err.Error() }

// Unwrap is the panic's error, through which ErrPanic and Stack find it.
func (c *stopCauseError) Unwrap() error { return c.err }

// Is answers true for context.Canceled, and for nothing else of its own.
func (*stopCauseError) Is(target error) bool { return target == context.Canceled }

// Stack is the stack of the goroutine whose recovered panic err carries, or
// "" when err carries none. The CLI prints it under the error that ends a
// command, because a command run without a log file has nowhere else to keep
// it and the bug report needs it.
func Stack(err error) string {
	var recovered *panicError
	if errors.As(err, &recovered) {
		return recovered.stack
	}

	return ""
}
