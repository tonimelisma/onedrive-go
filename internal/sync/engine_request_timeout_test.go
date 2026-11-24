package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// timedOutRequestRetryWait is past the first retry of a transient failure,
// which is due within 1 s plus a quarter of jitter (R-6.8.11).
const timedOutRequestRetryWait = 2 * time.Second

// A folder create whose request the transport timed out, in a one-shot run
// that goes on: the create is a failure kept for retry with its issue, so the
// run leaves work it will retry and exits 1 saying so.
//
// Validates: R-6.8.29, R-6.6.28
func TestRunOnce_TimedOutRequestIsKeptForRetryAndLeftWork(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	server, _ := syncfixture.NewStalledServer(t)
	stalled, requests := syncfixture.StalledGraphClient(t, server, &http.Client{Timeout: syncfixture.StalledRequestTimeout})
	client.CreateFolderFn = func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
		return stalled.CreateFolder(ctx, driveID, parentID, name)
	}
	eng, syncRoot := newTestEngine(t, client)
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "deep"), 0o750))

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncUploadOnly, RunOptions{})

	require.NoError(t, err)
	assert.Equal(t, int64(1), requests.Load(), "one dispatch is one request")
	assert.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, 1, report.LastPassKeptForRetry, "the timed-out create is retry work")
	assert.Equal(t, LeftWork{WillRetry: 1}, report.LeftWork(), "the run left the create to retry")
	require.Len(t, report.Errors, 1, "the failure is shown")
	require.ErrorIs(t, report.Errors[0], context.DeadlineExceeded)
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "deep", rows[0].Path)
	assert.Equal(t, syncdomain.ActionFolderCreate, rows[0].ActionType)
	assert.Equal(t, 1, rows[0].AttemptCount)
}

// A cancellation that reaches an action while its run goes on came from
// something narrower than the run with no reason the engine names: the
// action is kept for retry, not dropped as a shutdown, and said once at Warn
// with its path and kind.
//
// Validates: R-6.8.29
func TestRunOnce_ACancelWhileTheRunGoesOnIsRetriedAndSaid(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	client.CreateFolderFn = func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
		return nil, fmt.Errorf("create folder: %w", context.Canceled)
	}
	logger, logs := syncfixture.NewCapturedLogger()
	eng, syncRoot := newTestEngineWithLogger(t, client, logger)
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "deep"), 0o750))

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncUploadOnly, RunOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, report.LastPassKeptForRetry, "errors: %v", report.Errors)
	output := logs.String()
	assert.Equal(t, 1, strings.Count(output, "Syncing deep was canceled while the sync went on"), output)
	assert.Contains(t, output, "action_type=folder_create")
	assert.Equal(t, 1, syncfixture.CountLevel(output, "WARN"), output)
}

// A fatal completion ends the one-shot pass, and a transfer it interrupts
// fails the way the transports fail a canceled request: with the cancel's
// cause in place of context.Canceled. That transfer is a shutdown: it leaves
// no retry work, is neither counted nor shown as failed, and is not said at
// Warn.
//
// Validates: R-6.8.29, R-2.10.5
func TestRunOnce_AFatalFailureEndsTheTransferItInterruptsAsAShutdown(t *testing.T) {
	t.Parallel()

	mock := newRunOnceBidirectionalMock(driveid.New(syncfixture.EngineTestDriveID))
	downloading := make(chan struct{})
	mock.DownloadFn = func(ctx context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
		close(downloading)
		<-ctx.Done()

		return 0, fmt.Errorf("request canceled: %w", context.Cause(ctx))
	}
	mock.UploadFn = func(
		ctx context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc,
	) (*graph.Item, error) {
		select {
		case <-downloading:
		case <-ctx.Done():
		}

		return nil, syncfixture.GraphStatusError(http.StatusUnauthorized, graph.ErrUnauthorized)
	}
	logger, logs := syncfixture.NewCapturedLogger()
	eng, syncRoot := newTestEngineWithLogger(t, mock, logger)
	// The download is in flight while the upload fails only on two workers,
	// whatever dispatch order the test runs under.
	eng.hooks.serializeWorkers = false
	var activities []Activity
	eng.SetActivityHook(func(a Activity) { activities = append(activities, a) })
	syncfixture.WriteLocalFile(t, syncRoot, "local.txt", "local content\n")

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})

	require.ErrorIs(t, err, graph.ErrUnauthorized)
	require.NotNil(t, report)
	assert.Equal(t, 1, report.Failed, "only the upload failed: %v", report.Errors)
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows, "the interrupted download is no retry work")
	assert.Zero(t, syncfixture.CountLevel(logs.String(), "WARN"), logs.String())
	require.Len(t, activities, 1, "only the upload is shown as failed: %v", activities)
	assert.Equal(t, "local.txt", activities[0].Path)
}

// In watch mode a folder create whose request the transport timed out is
// retried once its backoff is due, and the upload below the folder, which
// waits for the create, still runs.
//
// Validates: R-6.8.29, R-6.8.10
func TestRunWatch_TimedOutRequestIsRetriedAndItsDependentRuns(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	client := remote.client(t)
	server, _ := syncfixture.NewStalledServer(t)
	stalled, requests := syncfixture.StalledGraphClient(t, server, &http.Client{Timeout: syncfixture.StalledRequestTimeout})
	var creates atomic.Int32
	client.CreateFolderFn = func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
		if creates.Add(1) == 1 {
			return stalled.CreateFolder(ctx, driveID, parentID, name)
		}
		return remote.createFolder(parentID, name)
	}
	eng, syncRoot := newTestEngine(t, client)
	recorder := attachDebugEventRecorder(eng)
	clock := syncfixture.NewManualClock(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "dir"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "dir", "a.txt"), []byte("below the folder\n"), 0o600))

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "the bootstrap to settle and the local observer to start")
	require.Equal(t, int32(1), creates.Load(), "the bootstrap's create timed out")
	assert.NotContains(t, remote.tree(), "dir/a.txt", "the upload waits for its folder")

	clock.Advance(timedOutRequestRetryWait)
	require.Eventually(t, func() bool {
		return remote.tree()["dir/a.txt"] == "below the folder\n"
	}, syncfixture.EventWait, 10*time.Millisecond, "the create is retried and the upload below it runs")

	syncfixture.StopWatch(t, cancel, done)
	assert.Equal(t, int32(2), creates.Load())
	assert.Equal(t, int64(1), requests.Load())
}

// mountRootPollTest is a mount-root engine's watch runtime past one synced
// pass of an empty folder, whose next folder poll fails.
type mountRootPollTest struct {
	rt    *watchRuntime
	bl    syncdomain.BaselineView
	clock *syncfixture.ManualClock
	polls atomic.Int32
	fail  atomic.Bool
}

// newMountRootPollTest builds a mountRootPollTest whose next poll fails with
// failure and whose polls after it see an unchanged folder.
func newMountRootPollTest(t *testing.T, failure error) *mountRootPollTest {
	t.Helper()

	const mountRoot = "mount-root-root"
	w := &mountRootPollTest{}
	mock := &syncfixture.EngineMockClient{
		FolderDeltaFn: func(context.Context, driveid.ID, string, string) ([]graph.Item, string, error) {
			w.polls.Add(1)
			if w.fail.CompareAndSwap(true, false) {
				return nil, "", failure
			}

			return nil, folderDeltaToken, nil
		},
	}
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.RemoteRootItemID = mountRoot
		inputs.RemoteRootDeltaCapable = true
	})
	w.clock = syncfixture.NewManualClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, w.clock)
	runScenarioPass(t, eng)
	setupWatchEngine(t, eng)
	w.rt = testWatchRuntime(t, eng)
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	w.bl = bl
	w.fail.Store(true)

	return w
}

// A mount-root poll whose request timed out while the watch goes on backs
// off on the run's clock and polls again: the mount-root watch goes on.
//
// Validates: R-6.8.29, R-2.15.5
func TestWatch_MountRootPollThatTimedOutBacksOffAndPollsAgain(t *testing.T) {
	t.Parallel()

	w := newMountRootPollTest(t, stalledGraphRequestError(t, clientTimeoutKind()))
	polledBefore := w.polls.Load()
	bo := retry.NewBackoff(retry.WatchRemotePolicy(), nil)
	bo.SetMaxOverride(syncdomain.MinPollInterval)
	backoff := bo.Next()
	backoffEnds := w.clock.Now().Add(backoff)
	batches := make(chan syncobserve.RemoteObservationBatch)
	cancel, done := syncfixture.RunInBackground(t, t.Context(), func(ctx context.Context) error {
		return w.rt.watchMountRootRemote(ctx, w.bl, batches, syncdomain.MinPollInterval)
	})

	require.Eventually(t, func() bool { return w.clock.HasPendingTimerAt(backoffEnds) }, syncfixture.EventWait, time.Millisecond,
		"the watch waits out one backoff after the timed-out poll")
	assert.Equal(t, polledBefore+1, w.polls.Load(), "the timed-out poll")
	w.clock.Advance(backoff)
	batch := syncfixture.ReceiveWithin(t, batches, "the batch of the poll after the backoff")

	assert.Equal(t, syncobserve.RemoteObservationBatchMountRoot, batch.Source)
	assert.Equal(t, polledBefore+2, w.polls.Load())
	syncfixture.StopWatch(t, cancel, done)
}

// An observer whose request timed out while the run goes on has stopped
// watching as after any other failure: it is said once at Warn, and its error
// is the cause the watch returns once the last observer has exited.
//
// Validates: R-6.8.29, R-6.6.21
func TestWatchRuntime_AnObserverWhoseRequestTimedOutIsWarnedAndReturned(t *testing.T) {
	t.Parallel()

	logger, logs := syncfixture.NewCapturedLogger()
	eng, _ := newTestEngineWithLogger(t, &syncfixture.EngineMockClient{}, logger)
	rt := newWatchRuntime(eng.Engine)
	rt.resources.activeObservers = 2
	timedOut := stalledGraphRequestError(t, clientTimeoutKind())
	pipeline := &watchPipeline{}

	// Through the watch loop's own handler, so what is checked is what the
	// loop does with the exit and the context it passes on, not two of the
	// handler's parts called by hand.
	require.NoError(t, rt.handleWatchObserverError(t.Context(), pipeline, observerExitError(observerNameRemote, timedOut)))
	output := logs.String()
	assert.Equal(t, 1, syncfixture.CountLevel(output, "WARN"), output)
	assert.Contains(t, output, "Stopped checking OneDrive for changes")

	// The other observer exits without an error of its own.
	err := rt.handleWatchObserverError(t.Context(), pipeline, nil)
	require.ErrorIs(t, err, timedOut, "the returned error names why watching stopped")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
