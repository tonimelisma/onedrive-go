package sync

import (
	"context"
	"testing"
	"time"
)

// quietWatchOptions are watch options for a test that drives every change
// itself: the remote poll an hour apart, so it never fires, and a short
// debounce.
func quietWatchOptions() WatchOptions {
	return WatchOptions{PollInterval: time.Hour, Debounce: 5 * time.Millisecond}
}

// runWatchInBackground runs eng.RunWatch in mode with opts in the background
// (runInBackground).
func runWatchInBackground(t *testing.T, eng *testEngine, mode SyncMode, opts WatchOptions) (context.CancelFunc, <-chan error) {
	t.Helper()

	return runInBackground(t, t.Context(), func(ctx context.Context) error {
		return eng.RunWatch(ctx, mode, opts)
	})
}
