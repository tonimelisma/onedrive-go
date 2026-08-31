package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.7.1
//
// A watch observer runs for the life of a mount. Without recovery at the
// goroutine boundary, a panic in one mount's observer terminates the process
// and every other mount with it.
func TestRecoverObserverPanic_DeliversPanicAsObserverError(t *testing.T) {
	t.Parallel()

	errs := make(chan error, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer recoverObserverPanic(synctest.TestLogger(t), "local", errs)

		panic("observer exploded")
	}()

	<-done

	err := <-errs
	require.Error(t, err)
	assert.Contains(t, err.Error(), "observer exploded")
	assert.Contains(t, err.Error(), "local")
}

// Validates: R-6.7.1
func TestRecoverObserverPanic_CleanExitReportsNothing(t *testing.T) {
	t.Parallel()

	errs := make(chan error, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer recoverObserverPanic(synctest.TestLogger(t), "remote", errs)
	}()

	<-done

	assert.Empty(t, errs, "a clean observer exit must not synthesize an error")
}

// Validates: R-6.7.1
//
// The report must never block the panicking goroutine: a full channel means
// the runtime is already failing, and hanging here would strand the mount.
func TestRecoverObserverPanic_FullChannelDoesNotBlock(t *testing.T) {
	t.Parallel()

	errs := make(chan error, 1)
	errs <- assert.AnError

	done := make(chan struct{})

	go func() {
		defer close(done)
		defer recoverObserverPanic(synctest.TestLogger(t), "local", errs)

		panic("observer exploded")
	}()

	<-done
}

// Validates: R-6.7.1
//
// End-to-end proof of the wiring: a panic raised inside the real local
// observer goroutine must surface as a watch-mode error rather than killing
// the process. Before recovery was wired in, this test crashed the test
// binary instead of failing.
func TestRunWatch_LocalObserverPanicFailsMountInsteadOfCrashing(t *testing.T) {
	eng, _ := newTestEngine(t, &engineMockClient{})

	eng.localWatcherFactory = func() (fsWatcher, error) {
		panic("watcher factory exploded")
	}

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(t.Context(), SyncUploadOnly, WatchOptions{
			PollInterval: 1 * time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	select {
	case err := <-done:
		require.Error(t, err, "a panicking observer must fail watch mode")
		assert.Contains(t, err.Error(), "watcher factory exploded")
	case <-time.After(30 * time.Second):
		require.Fail(t, "watch mode did not report the observer panic")
	}
}
