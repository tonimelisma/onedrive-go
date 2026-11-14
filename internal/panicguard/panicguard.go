// Package panicguard is how product code turns a recovered panic into one log
// record and an error. Every recover in onedrive-go calls Recovered, so a
// panic reads the same wherever it is caught: logged once at Error with the
// stack of the goroutine that panicked, and returned as an error that wraps
// ErrPanic. What each boundary then does with the error is set by R-6.3.8 and
// R-6.3.10 (spec/requirements/non-functional.md); this package adds no rule
// of its own.
//
// It is a leaf package: it imports only the standard library, so the CLI, the
// Graph client and the sync packages can all use it.
package panicguard

import (
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
