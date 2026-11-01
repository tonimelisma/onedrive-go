package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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

// Validates: R-6.7.1
//
// The full remote refresh runs in its own goroutine. A panic there used to
// unwind the process; now it reaches the loop as a refresh result whose
// fatalErr the loop returns, so the mount fails and multisync restarts it,
// and the refresh is marked finished so a restart is not told one is still
// running. Before the recover was wired in, this test crashed the binary.
func TestRunFullRemoteRefreshAsync_PanicFailsMountInsteadOfCrashing(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			panic("delta exploded")
		},
	}

	e, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := e.baseline.Load(ctx)
	require.NoError(t, err)

	setupWatchEngine(t, e)
	rt := testWatchRuntime(t, e)
	rt.resources.dirtyBuf = newDirtyBuffer(e.logger)

	runFullRemoteRefreshAsyncForTest(t, e, ctx, bl)

	select {
	case result := <-rt.resources.refreshResults:
		applyErr := rt.applyRemoteRefreshResult(ctx, &result)
		require.Error(t, applyErr, "a panicking refresh must fail the mount")
		assert.Contains(t, applyErr.Error(), "delta exploded")
		assert.Contains(t, applyErr.Error(), "full remote refresh")
	case <-time.After(10 * time.Second):
		require.Fail(t, "the refresh panic was not delivered to the loop")
	}

	assert.False(t, rt.resources.refreshActive, "a panicked refresh is finished, not still running")
}

// Validates: R-6.7.1
//
// The socket.io wake source only accelerates polling, so a panic in it takes
// the ordinary wake-source failure path: logged, and watch keeps polling on
// its interval. Before the recover was wired in, this test crashed the
// binary instead of stopping cleanly.
func TestRunWatch_SocketIOWakeSourcePanicKeepsWatchRunning(t *testing.T) {
	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.enableWebsocket = true
	entered := make(chan struct{})
	eng.socketIOWakeSourceFactory = func(_ socketIOEndpointFetcher, _ driveid.ID, _ socketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{
			runFn: func(context.Context, chan<- struct{}) error {
				close(entered)
				panic("wake source exploded")
			},
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncDownloadOnly, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		require.Fail(t, "the wake source was never started")
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "watch keeps running without its wake source and stops cleanly")
	case <-time.After(30 * time.Second):
		require.Fail(t, "watch mode did not stop after the wake source panic")
	}
}
