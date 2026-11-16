package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are the watch helpers of internal/sync's tests, declared again here
// with the same names and messages: a watch or loop a test starts runs only
// through runInBackground, whose cleanup stops it, and a wait for an event
// that must come is one call that fails the test at once, naming what it
// waited for. No test here cancels a run itself, so runInBackground returns
// only the result channel, and stopWatch and runWatchInBackground are not
// declared. closeWithin, which bounds a cleanup's close of a server, is this
// package's own.

// errRunExitedWithoutReturning is the result of a background run whose
// goroutine ended without run returning: a require inside it failed.
var errRunExitedWithoutReturning = errors.New("the run's goroutine ended before it returned: a require inside it failed")

// runInBackground runs run in a goroutine the test owns, on a context derived
// from ctx, and returns the channel run's one result arrives on. A cleanup
// cancels the context and waits, within eventWait, until run has returned, so
// a test that fails early never leaves a watch running into TempDir removal or
// the next test. The test reads the result itself with receiveWithin.
func runInBackground(t *testing.T, ctx context.Context, run func(context.Context) error) <-chan error {
	t.Helper()

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		returned := false
		defer func() {
			if !returned {
				done <- errRunExitedWithoutReturning
			}
		}()
		err := run(runCtx)
		returned = true
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(eventWait):
			assert.Fail(t, "a background watch did not return after its cleanup canceled it")
		}
	})

	return done
}

// runWithin is a test's own call of a watch or loop: it runs run on a context
// derived from ctx and returns run's result, failing the test naming what it
// waited for when run has not returned within eventWait. Its return is an
// event like any other, so it is bounded like one.
func runWithin(t *testing.T, ctx context.Context, what string, run func(context.Context) error) error {
	t.Helper()

	done := runInBackground(t, ctx, run)

	return receiveWithin(t, done, what)
}

// receiveWithin returns what ch delivers, and fails the test naming what it
// waited for when nothing comes within eventWait: a run that never returns
// fails in a minute with a useful line instead of at go test's deadline.
func receiveWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(eventWait):
		require.FailNow(t, "timed out waiting for "+what)
		var none T
		return none
	}
}

// closeWithin closes a server a test started, on a context that ends eventWait
// from now, and fails the test naming the server when the close fails or does
// not finish within that bound. A cleanup closes a server through it: the
// test's own context has ended by then, and without a bound a connection that
// never goes idle would hold the close forever.
func closeWithin(t *testing.T, what string, closeServer func(context.Context) error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), eventWait)
	defer cancel()

	assert.NoError(t, closeServer(ctx), "closing %s within eventWait", what)
}
