package sync

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newBootstrapWatchPipelineForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	mode SyncMode,
	workers int,
) *watchPipeline {
	t.Helper()

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)

	completeCh := make(chan struct{})
	pool := NewWorkerPool(eng.execCfg, rt.dispatchCh, completeCh, eng.baseline, eng.logger, 1024)
	pool.Start(ctx, workers)
	t.Cleanup(pool.Stop)

	return &watchPipeline{
		runtime: rt,
		safety:  DefaultSafetyConfig(),
		pool:    pool,
		results: pool.Results(),
		mode:    mode,
	}
}

// Validates: R-2.1.2
func TestPhase0_RunWatch_BootstrapCompletesBeforeLocalObserverStarts(t *testing.T) {
	t.Parallel()

	uploadStarted := make(chan struct{})
	var uploadStartedOnce sync.Once
	allowUpload := make(chan struct{})

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadStartedOnce.Do(func() {
				close(uploadStarted)
			})
			<-allowUpload
			return &graph.Item{
				ID:           "uploaded-id",
				Name:         name,
				Size:         size,
				QuickXorHash: "upload-hash",
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	writeLocalFile(t, syncRoot, "local.txt", "bootstrap upload")
	recorder := attachDebugEventRecorder(eng)
	eng.localWatcherFactory = func() (FsWatcher, error) {
		return newEnospcWatcher(1 << 20), nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()
	select {
	case <-uploadStarted:
	case <-time.After(2 * time.Second):
		require.Fail(t, "bootstrap upload should start before observers")
	}

	close(allowUpload)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not exit after cancellation")
	}
}

// Validates: R-2.1.2
func TestPhase0_RunWatch_BootstrapCompletesBeforeRemoteObserverStarts(t *testing.T) {
	t.Parallel()

	var deltaCalls atomic.Int32
	downloadStarted := make(chan struct{})
	var downloadStartedOnce sync.Once
	allowDownload := make(chan struct{})
	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			call := deltaCalls.Add(1)
			if call == 1 {
				return deltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{
						ID:           "bootstrap-item",
						Name:         "remote.txt",
						DriveID:      driveID,
						ParentID:     "root",
						Size:         12,
						QuickXorHash: "remote-hash",
					},
				}, "bootstrap-token"), nil
			}

			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "steady-state-token"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadStartedOnce.Do(func() {
				close(downloadStarted)
			})
			<-allowDownload
			n, err := w.Write([]byte("remote-data"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncDownloadOnly, WatchOptions{
			PollInterval: 20 * time.Millisecond,
			Debounce:     5 * time.Millisecond,
		})
	}()
	select {
	case <-downloadStarted:
	case <-time.After(2 * time.Second):
		require.Fail(t, "bootstrap download should start before remote observer")
	}

	assert.Equal(t, int32(1), deltaCalls.Load(),
		"remote observer must not start polling until bootstrap has drained")

	close(allowDownload)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not exit after cancellation")
	}
}

// ---------------------------------------------------------------------------
// bootstrap/quiescence subroutines
// ---------------------------------------------------------------------------

func TestWaitForQuiescence_EmptyGraph(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	err := rt.runWatchUntilQuiescent(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
}

func TestWaitForQuiescence_ContextCancel(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "stuck.txt",
		DriveID: driveid.New(engineTestDriveID),
		ItemID:  "stuck-item",
	}, 1, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := rt.runWatchUntilQuiescent(ctx, &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
}

func TestBootstrapSync_NoChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	pipe := newBootstrapWatchPipelineForTest(t, eng, ctx, SyncBidirectional, 1)

	err := testWatchRuntime(t, eng).bootstrapSync(ctx, SyncBidirectional, pipe)
	require.NoError(t, err)
}

func TestBootstrapSync_WithChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "item-1",
					Name:         "newfile.txt",
					DriveID:      driveID,
					ParentID:     "root",
					Size:         10,
					QuickXorHash: "hash1",
				},
			}, "token-2"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()
	pipe := newBootstrapWatchPipelineForTest(t, eng, ctx, SyncDownloadOnly, 2)

	err := testWatchRuntime(t, eng).bootstrapSync(ctx, SyncDownloadOnly, pipe)
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Join(syncRoot, "newfile.txt"))
	require.NoError(t, statErr, "newfile.txt should have been downloaded")

	assert.Equal(t, 0, testWatchRuntime(t, eng).depGraph.InFlightCount())
}

// Validates: R-2.5.1
func TestBootstrapSync_ReconcilesRemoteDeleteDriftWithoutFreshDelta(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	writeLocalFile(t, syncRoot, "gone.txt", "delete me")
	deleteHash := hashContentQuickXor(t, "delete me")
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "gone.txt",
		DriveID:         driveID,
		ItemID:          "gone",
		ItemType:        ItemTypeFile,
		LocalHash:       deleteHash,
		RemoteHash:      deleteHash,
		LocalSize:       int64(len("delete me")),
		LocalSizeKnown:  true,
		RemoteSize:      int64(len("delete me")),
		RemoteSizeKnown: true,
		LocalMtime:      time.Now().UnixNano(),
		RemoteMtime:     time.Now().UnixNano(),
	}}, "")

	pipe := newBootstrapWatchPipelineForTest(t, eng, ctx, SyncBidirectional, 1)

	err := testWatchRuntime(t, eng).bootstrapSync(ctx, SyncDownloadOnly, pipe)
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Join(syncRoot, "gone.txt"))
	require.ErrorIs(t, statErr, os.ErrNotExist)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	_, found := bl.GetByPath("gone.txt")
	assert.False(t, found, "bootstrap should settle remote delete drift from baseline + mirror truth")
}

// Validates: R-6.8.9
// Validates: R-2.10.5
func TestPhase0_OneShotEngineLoop_TrialFailureKeepsBlockedScopeIsolated(t *testing.T) {
	t.Parallel()

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done
	recorder := attachDebugEventRecorder(eng)
	rt := testWatchRuntime(t, eng)

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 30 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(30 * time.Millisecond),
	})

	ta := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "trial.txt",
		DriveID: driveid.New(engineTestDriveID),
		ItemID:  "trial-item",
	}, 99, nil)
	require.NotNil(t, ta)

	results <- WorkerResult{
		ActionID:      99,
		Path:          "trial.txt",
		ActionType:    ActionDownload,
		DriveID:       driveid.New(engineTestDriveID),
		Success:       false,
		HTTPStatus:    http.StatusInternalServerError,
		Err:           graph.ErrServerError,
		ErrMsg:        "trial failure",
		IsTrial:       true,
		TrialScopeKey: SKService(),
	}

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventTrialTimerArmed
	}, "trial timer re-armed after trial failure")

	block, ok := getTestScopeBlock(eng, SKService())
	require.True(t, ok, "service scope should remain blocked after trial failure")
	assert.Equal(t, 60*time.Millisecond, block.TrialInterval,
		"trial failure should only extend the active scope interval")

	assert.True(t, isTestScopeBlocked(eng, SKService()),
		"trial failure must not clear the blocked scope via the normal result path")
}

// Validates: R-2.10.5, R-2.10.11
func TestPhase0_OneShotEngineLoop_TrialSuccessMakesFailuresRetryableAndReinjectableWithoutExternalObservation(t *testing.T) {
	t.Parallel()

	const blockedPath = "blocked.txt"

	results, done, cancel, eng := startDrainLoop(t)
	defer cancel()
	_ = done
	recorder := attachDebugEventRecorder(eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "blocked-item",
		Path:     "blocked.txt",
		ItemType: ItemTypeFile,
		Hash:     "blocked-hash",
		Size:     42,
	}}, "", driveID))

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:           testThrottleScope(),
		IssueType:     IssueRateLimited,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      blockedPath,
		DriveID:   driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ErrMsg:    "rate limited",
		ScopeKey:  testThrottleScope(),
		ItemID:    "blocked-item",
	}, nil))

	ta := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial.txt",
		DriveID: driveID,
		ItemID:  "trial-item",
	}, 1, nil)
	require.NotNil(t, ta)

	results <- WorkerResult{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    ActionUpload,
		DriveID:       driveID,
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: testThrottleScope(),
	}

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventScopeReleased && event.ScopeKey == testThrottleScope()
	}, "trial success released blocked scope")
	assert.False(t, isTestScopeBlocked(eng, testThrottleScope()),
		"trial success should clear the scope block")

	retried := readReadyAction(t, rt.dispatchCh)
	require.Equal(t, blockedPath, retried.Action.Path,
		"trial success should re-dispatch the held failure without external observation")
	assert.Equal(t, ActionDownload, retried.Action.Type)
}

// Validates: R-2.10.13, R-2.10.11
func TestPhase0_RecheckLocalPermissions_ReleasesHeldFailuresImmediately(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.buf = NewBuffer(eng.logger)

	scopeKey := SKPermDir("Private")
	accessibleDir := filepath.Join(syncRoot, "Private")
	require.NoError(t, os.MkdirAll(accessibleDir, 0o750))

	require.NoError(t, eng.baseline.CommitObservation(ctx, []ObservedItem{{
		DriveID:  eng.driveID,
		ItemID:   "private-item",
		Path:     "Private/doc.txt",
		ItemType: ItemTypeFile,
		Hash:     "private-hash",
		Size:     64,
	}}, "", eng.driveID))

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "Private",
		DriveID:   eng.driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleBoundary,
		IssueType: IssueLocalPermissionDenied,
		Category:  CategoryActionable,
		ErrMsg:    "directory not accessible",
		ScopeKey:  scopeKey,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "Private/doc.txt",
		DriveID:   eng.driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleHeld,
		Category:  CategoryTransient,
		ErrMsg:    "blocked by perm scope",
		ScopeKey:  scopeKey,
		ItemID:    "private-item",
	}, nil))
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueLocalPermissionDenied,
		BlockedAt: eng.nowFunc(),
	})

	decisions := applyLocalPermissionRecheck(t, eng, ctx)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	assert.False(t, isTestScopeBlocked(eng, scopeKey),
		"local permission recheck should clear the active scope block")

	rows, err := eng.baseline.ListSyncFailuresForRetry(ctx, eng.nowFunc())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Private/doc.txt", rows[0].Path)

	outbox := runTestRetrierSweep(t, eng, ctx)
	require.Len(t, outbox, 1, "released failure should be dispatchable immediately via the retrier")
	assert.Equal(t, "Private/doc.txt", outbox[0].Action.Path)
}

// Validates: R-2.10.5
func TestPhase0_ScopeBlockFailureDoesNotReadmitDependentEarly(t *testing.T) {
	t.Parallel()

	results, _, cancel, eng := startDrainLoop(t)
	defer cancel()
	recorder := attachDebugEventRecorder(eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	parent := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "parent.txt",
		DriveID: driveID,
		ItemID:  "parent-item",
	}, 1, nil)
	require.NotNil(t, parent)

	child := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "child.txt",
		DriveID: driveID,
		ItemID:  "child-item",
	}, 2, []int64{1})
	require.Nil(t, child)

	rt.dispatchCh <- parent
	readReady(t, rt.dispatchCh)

	results <- WorkerResult{
		ActionID:   1,
		Path:       "parent.txt",
		ActionType: ActionUpload,
		DriveID:    driveID,
		Success:    false,
		HTTPStatus: http.StatusTooManyRequests,
		RetryAfter: 25 * time.Millisecond,
		Err:        graph.ErrThrottled,
		ErrMsg:     "rate limited",
	}

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventScopeActivated && event.ScopeKey == testThrottleScope()
	}, "scope block activated from worker result")

	select {
	case ta := <-rt.dispatchCh:
		require.Failf(t, "dependent dispatched early", "unexpected path %s", ta.Action.Path)
	default:
	}
	assert.True(t, isTestScopeBlocked(eng, testThrottleScope()),
		"scope block should be activated from the worker result")

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	assert.Len(t, failures, 2, "parent failure and child cascade failure should both be persisted")
}

// Validates: R-2.1.6
func TestPhase0_RunFullReconciliationAsync_UsesBufferHandoffInsteadOfDirectDispatch(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{
						ID:           "reconcile-item",
						Name:         "reconcile.txt",
						DriveID:      driveID,
						ParentID:     "root",
						Size:         21,
						QuickXorHash: "reconcile-hash",
					},
				},
				DeltaLink: "reconcile-token",
			}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	ready := setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.buf = NewBuffer(eng.logger)

	rt.runFullReconciliationAsync(ctx, bl)
	waitForReconcileDone(t, eng)

	select {
	case ta := <-ready:
		require.Failf(t, "reconciliation dispatched directly", "unexpected action %s", ta.Action.Path)
	default:
	}

	batch := rt.buf.FlushImmediate()
	require.NotEmpty(t, batch)
	assert.Equal(t, "reconcile.txt", batch[0].Path)
}
