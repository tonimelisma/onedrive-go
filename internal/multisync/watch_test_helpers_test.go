package multisync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// These are the watch helpers of internal/sync's tests, declared again here
// with the same names and messages: a watch or loop a test starts runs only
// through runInBackground, whose cleanup stops it, and a wait for an event
// that must come is one call that fails the test at once, naming what it
// waited for. closeWithin, which bounds a cleanup's close of a server, is this
// package's own.

// errRunExitedWithoutReturning is the result of a background run whose
// goroutine ended without run returning: a require inside it failed.
var errRunExitedWithoutReturning = errors.New("the run's goroutine ended before it returned: a require inside it failed")

// runInBackground runs run in a goroutine the test owns, on a context derived
// from ctx, and returns that context's cancel and the channel run's one result
// arrives on. A cleanup cancels the context and waits, within eventWait, until
// run has returned, so a test that fails early never leaves a watch running
// into TempDir removal or the next test. The test reads the result itself with
// receiveWithin or stopWatch.
func runInBackground(
	t *testing.T, ctx context.Context, run func(context.Context) error,
) (context.CancelFunc, <-chan error) {
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

	return cancel, done
}

// runWithin is a test's own call of a watch or loop: it runs run on a context
// derived from ctx and returns run's result, failing the test naming what it
// waited for when run has not returned within eventWait. Its return is an
// event like any other, so it is bounded like one.
func runWithin(t *testing.T, ctx context.Context, what string, run func(context.Context) error) error {
	t.Helper()

	_, done := runInBackground(t, ctx, run)

	return receiveWithin(t, done, what)
}

// runWatchOf is orch.RunWatch, bidirectional with default options as every
// test here runs it, as a run for runInBackground or runWithin.
func runWatchOf(orch *Orchestrator) func(context.Context) error {
	return func(ctx context.Context) error {
		return orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}
}

// runWatchInBackground runs orch.RunWatch in the background
// (runInBackground, runWatchOf).
func runWatchInBackground(t *testing.T, orch *Orchestrator) (context.CancelFunc, <-chan error) {
	t.Helper()

	return runInBackground(t, t.Context(), runWatchOf(orch))
}

// runOnceInBackground runs orch.RunOnce, bidirectional with default options,
// in the background (runInBackground). The pass writes the result before its
// nil arrives on the channel, so the test reads the result after receiving.
func runOnceInBackground(t *testing.T, orch *Orchestrator) (*RunOnceResult, <-chan error) {
	t.Helper()

	result := &RunOnceResult{}
	_, done := runInBackground(t, t.Context(), func(ctx context.Context) error {
		*result = orch.RunOnce(ctx, syncengine.SyncBidirectional, syncengine.RunOptions{})
		return nil
	})

	return result, done
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

// stopWatch cancels a background watch and requires that it returns nil
// within eventWait.
func stopWatch(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	cancel()
	require.NoError(t, receiveWithin(t, done, "the watch to return after cancellation"))
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
