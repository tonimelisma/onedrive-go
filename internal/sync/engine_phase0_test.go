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
		runtime:     rt,
		safety:      DefaultSafetyConfig(),
		pool:        pool,
		completions: pool.Completions(),
		mode:        mode,
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
	case <-time.After(debugEventTimeout):
		require.Fail(t, "bootstrap upload should start before observers")
	}
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}), "bootstrap must not quiesce while the bootstrap upload is blocked")
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}), "local observer must not start before bootstrap quiesces")

	close(allowUpload)

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not exit after cancellation")
	}

	recorder.requireOrderedSubsequence(t, []func(engineDebugEvent) bool{
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventBootstrapQuiesced
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
		},
	}, "bootstrap quiescence must precede local observer startup")
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
	case <-time.After(debugEventTimeout):
		require.Fail(t, "bootstrap download should start before remote observer")
	}

	assert.Equal(t, int32(1), deltaCalls.Load(),
		"remote observer must not start polling until bootstrap has drained")
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}), "bootstrap must not quiesce while the bootstrap download is blocked")
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}), "remote observer must not start before bootstrap quiesces")

	close(allowDownload)

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not exit after cancellation")
	}

	recorder.requireOrderedSubsequence(t, []func(engineDebugEvent) bool{
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventBootstrapQuiesced
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
		},
	}, "bootstrap quiescence must precede remote observer startup")
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

	results, cancel, eng := startDrainLoop(t)
	defer cancel()
	recorder := attachDebugEventRecorder(eng)
	rt := testWatchRuntime(t, eng)

	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
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

	results <- ActionCompletion{
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

	block, ok := getTestBlockScope(eng, SKService())
	require.True(t, ok, "service scope should remain blocked after trial failure")
	assert.Equal(t, time.Minute, block.TrialInterval,
		"trial failure should only extend the active scope interval")

	assert.True(t, isTestBlockScopeed(eng, SKService()),
		"trial failure must not clear the blocked scope via the normal result path")
}

// Validates: R-2.10.5, R-2.10.11
func TestPhase0_OneShotEngineLoop_TrialSuccessMakesFailuresRetryableAndReinjectableWithoutExternalObservation(t *testing.T) {
	t.Parallel()

	const blockedPath = "blocked.txt"

	results, cancel, eng := startDrainLoop(t)
	defer cancel()
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

	setTestBlockScope(t, eng, &BlockScope{
		Key:           testThrottleScope(),
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})
	_, err := eng.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          blockedPath,
		ActionType:    ActionDownload,
		ConditionType: IssueRateLimited,
		ScopeKey:      testThrottleScope(),
		LastError:     "rate limited",
		Blocked:       true,
	}, nil)
	require.NoError(t, err)
	rt.replaceCurrentPlan(&ActionPlan{
		Actions: []Action{{
			Type:    ActionDownload,
			Path:    blockedPath,
			DriveID: driveID,
			ItemID:  "blocked-item",
		}},
		Deps: [][]int{nil},
	})

	ta := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "trial.txt",
		DriveID: driveID,
		ItemID:  "trial-item",
	}, 1, nil)
	require.NotNil(t, ta)

	results <- ActionCompletion{
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
	assert.False(t, isTestBlockScopeed(eng, testThrottleScope()),
		"trial success should clear the block scope")

	retried := readReadyAction(t, rt.dispatchCh)
	require.Equal(t, blockedPath, retried.Action.Path,
		"trial success should re-dispatch blocked retry work without external observation")
	assert.Equal(t, ActionDownload, retried.Action.Type)
}

// Validates: R-2.10.2, R-2.10.10
func TestPhase0_ObserveLocalChanges_ClearsResolvedFilePermissionIssueWithoutDeletingRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	require.NoError(t, os.MkdirAll(filepath.Join(eng.syncRoot, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(eng.syncRoot, "docs", "file.txt"), []byte("ok"), 0o600))

	seedObservationIssueRowForTest(t, eng.baseline, &ObservationIssue{
		Path:       "docs/file.txt",
		DriveID:    eng.driveID,
		ActionType: ActionUpload,
		IssueType:  IssueLocalReadDenied,
		Error:      "permission denied",
	})

	retryRow := retryWorkIdentityForWork("docs/file.txt", "", ActionUpload)
	retryRow.AttemptCount = 3
	retryRow.NextRetryAt = eng.nowFn().Add(time.Minute).UnixNano()
	retryRow.FirstSeenAt = eng.nowFn().UnixNano()
	retryRow.LastSeenAt = eng.nowFn().UnixNano()
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &retryRow))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	_, err = testEngineFlow(t, eng).observeLocalChanges(ctx, nil, bl)
	require.NoError(t, err)

	observationIssues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	assert.Empty(t, observationIssues)

	rows, err := eng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "docs/file.txt", rows[0].Path)
	assert.Equal(t, ActionUpload, rows[0].ActionType)
}

// Validates: R-2.10.5
func TestPhase0_BlockScopeFailureDoesNotReadmitDependentEarly(t *testing.T) {
	t.Parallel()

	results, cancel, eng := startDrainLoop(t)
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

	results <- ActionCompletion{
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
	}, "block scope activated from action completion")

	select {
	case ta := <-rt.dispatchCh:
		require.Failf(t, "dependent dispatched early", "unexpected path %s", ta.Action.Path)
	default:
	}
	assert.True(t, isTestBlockScopeed(eng, testThrottleScope()),
		"block scope should be activated from the action completion")

	retryRows, err := eng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Len(t, retryRows, 2, "parent failure and child cascade retry_work should both be persisted")
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
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)

	rt.runFullRemoteRefreshAsync(ctx, bl)
	waitForReconcileDone(t, eng)

	select {
	case ta := <-ready:
		require.Failf(t, "reconciliation dispatched directly", "unexpected action %s", ta.Action.Path)
	default:
	}

	batch := rt.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.Equal(t, []string{"reconcile.txt"}, batch.Paths)
}
