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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func newBootstrapWatchPipelineForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	mode syncdomain.SyncMode,
	workers int,
) *watchPipeline {
	t.Helper()

	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.scopes.state = syncexec.NewScopeState(eng.nowFunc, eng.logger)
	bl, err := rt.runStartupStage(ctx)
	require.NoError(t, err)

	pool := syncexec.NewWorkerPool(eng.execCfg, rt.sched.dispatchCh, eng.baseline, eng.logger, 1024)
	pool.Start(ctx, workers)
	t.Cleanup(pool.Stop)

	return &watchPipeline{
		runtime:     rt,
		bl:          bl,
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

	mock := &syncfixture.EngineMockClient{
		UploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
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
	syncfixture.WriteLocalFile(t, syncRoot, "local.txt", "bootstrap upload")
	recorder := attachDebugEventRecorder(eng)
	eng.hooks.observe.WatcherFactory = func() (syncobserve.FsWatcher, error) {
		return syncfixture.NewEnospcWatcher(1 << 20), nil
	}

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())
	syncfixture.ReceiveWithin(t, uploadStarted, "the bootstrap upload to start before the observers")
	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}), "bootstrap must not quiesce while the bootstrap upload is blocked")
	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}), "local observer must not start before bootstrap quiesces")

	close(allowUpload)

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")

	syncfixture.StopWatch(t, cancel, done)

	recorder.RequireOrderedSubsequence(t, []func(syncdomain.EngineDebugEvent) bool{
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
		},
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
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
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			call := deltaCalls.Add(1)
			if call == 1 {
				return syncfixture.DeltaPageWithItems([]graph.Item{
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

			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "steady-state-token"), nil
		},
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
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

	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncDownloadOnly, WatchOptions{
		PollInterval: 20 * time.Millisecond,
		Debounce:     5 * time.Millisecond,
	})
	syncfixture.ReceiveWithin(t, downloadStarted, "the bootstrap download to start before the remote observer")

	assert.Equal(t, int32(1), deltaCalls.Load(),
		"remote observer must not start polling until bootstrap has drained")
	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}), "bootstrap must not quiesce while the bootstrap download is blocked")
	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverRemote
	}), "remote observer must not start before bootstrap quiesces")

	close(allowDownload)

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverRemote
	}, "remote observer started")

	syncfixture.StopWatch(t, cancel, done)

	recorder.RequireOrderedSubsequence(t, []func(syncdomain.EngineDebugEvent) bool{
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
		},
		func(event syncdomain.EngineDebugEvent) bool {
			return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverRemote
		},
	}, "bootstrap quiescence must precede remote observer startup")
}

// Validates: R-2.1.2, R-2.10.33
func TestPhase0_RunWatch_BootstrapQuiescesWithFutureHeldRetryWork(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	var uploadCalls atomic.Int32

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-future-retry"), nil
		},
		UploadFn: func(_ context.Context, _ driveid.ID, _ string, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalls.Add(1)
			return &graph.Item{ID: "unexpected-upload"}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	syncfixture.WriteLocalFile(t, syncRoot, "retry-later.txt", "retry me later")
	now := eng.nowFunc()
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "retry-later.txt",
		ActionType:   syncdomain.ActionUpload,
		AttemptCount: 1,
		NextRetryAt:  now.Add(time.Hour).UnixNano(),
	}))

	recorder := attachDebugEventRecorder(eng)
	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")

	assert.Equal(t, int32(0), uploadCalls.Load(), "future-held retry work must not dispatch during bootstrap")

	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.1.2, R-2.10.5
func TestPhase0_RunWatch_DueTrialDrainsDuringBootstrapBeforeObserversStart(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	uploadStarted := make(chan struct{})
	var uploadStartedOnce sync.Once
	allowUpload := make(chan struct{})
	scopeKey := syncdomain.SKService()

	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-due-trial"), nil
		},
		UploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadStartedOnce.Do(func() {
				close(uploadStarted)
			})
			<-allowUpload
			return &graph.Item{
				ID:           "trial-upload-id",
				Name:         name,
				Size:         size,
				QuickXorHash: "trial-upload-hash",
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	syncfixture.WriteLocalFile(t, syncRoot, "trial-held.txt", "trial upload")
	now := eng.nowFunc()
	require.NoError(t, eng.baseline.UpsertRetryWork(t.Context(), &syncdomain.RetryWorkRow{
		Path:         "trial-held.txt",
		ActionType:   syncdomain.ActionUpload,
		ScopeKey:     scopeKey,
		Blocked:      true,
		AttemptCount: 1,
	}))
	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: 10 * time.Second,
		NextTrialAt:   now.Add(-time.Second),
	}))

	recorder := attachDebugEventRecorder(eng)
	cancel, done := runWatchInBackground(t, eng, syncdomain.SyncUploadOnly, quietWatchOptions())

	syncfixture.ReceiveWithin(t, uploadStarted, "the due trial to dispatch bootstrap work before the observers start")

	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}), "bootstrap must not quiesce while the due trial action is still running")
	assert.False(t, recorder.FindEvent(func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}), "observers must not start before the due trial action drains")

	close(allowUpload)

	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventObserverStarted && event.Observer == syncdomain.EngineDebugObserverLocal
	}, "local observer started")

	syncfixture.StopWatch(t, cancel, done)
}

// ---------------------------------------------------------------------------
// bootstrap/quiescence subroutines
// ---------------------------------------------------------------------------

func TestWaitForQuiescence_EmptyGraph(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{}
	eng, _ := newTestEngine(t, mock)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	err := rt.runWatchUntilQuiescent(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
}

// Validates: R-2.8.3
func TestWaitForQuiescence_ContextCancel(t *testing.T) {
	t.Parallel()

	mock := &syncfixture.EngineMockClient{}
	eng, _ := newTestEngine(t, mock)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "stuck.txt",
		DriveID: driveid.New(syncfixture.EngineTestDriveID),
		ItemID:  "stuck-item",
	}, 1, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := rt.runWatchUntilQuiescent(ctx, &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
}

func TestBootstrapSync_NoChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()
	pipe := newBootstrapWatchPipelineForTest(t, eng, ctx, syncdomain.SyncBidirectional, 1)

	err := testWatchRuntime(t, eng).bootstrapSync(ctx, syncdomain.SyncBidirectional, pipe)
	require.NoError(t, err)
}

// Validates: R-2.1.2
func TestBootstrapSync_WithChanges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "item-1",
					Name:         "newfile.txt",
					DriveID:      driveID,
					ParentID:     "root",
					Size:         int64(len("downloaded-content")),
					QuickXorHash: syncfixture.TestQuickXor("downloaded-content"),
				},
			}, "token-2"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()
	pipe := newBootstrapWatchPipelineForTest(t, eng, ctx, syncdomain.SyncDownloadOnly, 2)

	err := syncfixture.RunWithin(t, ctx, "bootstrap to finish the download", func(runCtx context.Context) error {
		return testWatchRuntime(t, eng).bootstrapSync(runCtx, syncdomain.SyncDownloadOnly, pipe)
	})
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Join(syncRoot, "newfile.txt"))
	require.NoError(t, statErr, "newfile.txt should have been downloaded")

	assert.Equal(t, 0, testWatchRuntime(t, eng).sched.graph.InFlightCount())
}

// Validates: R-2.5.1
func TestBootstrapSync_ReconcilesRemoteDeleteDriftWithoutFreshDelta(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			// The kept folder keeps the remote side from being empty, which
			// the delete-safety hold would rightly refuse to act on.
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{ID: "kept", Name: "kept", ParentID: "root", IsFolder: true, DriveID: driveID},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	syncfixture.WriteLocalFile(t, syncRoot, "gone.txt", "delete me")
	deleteHash := syncfixture.HashContentQuickXor(t, "delete me")
	seedBaseline(t, eng.baseline, ctx, []syncdomain.ActionOutcome{{
		Action:          syncdomain.ActionDownload,
		Success:         true,
		Path:            "gone.txt",
		DriveID:         driveID,
		ItemID:          "gone",
		ItemType:        syncdomain.ItemTypeFile,
		LocalHash:       deleteHash,
		RemoteHash:      deleteHash,
		LocalSize:       int64(len("delete me")),
		LocalSizeKnown:  true,
		RemoteSize:      int64(len("delete me")),
		RemoteSizeKnown: true,
		LocalMtime:      time.Now().UnixNano(),
		RemoteMtime:     time.Now().UnixNano(),
	}}, "")

	pipe := newBootstrapWatchPipelineForTest(t, eng, ctx, syncdomain.SyncBidirectional, 1)

	err := syncfixture.RunWithin(t, ctx, "bootstrap to finish the local delete", func(runCtx context.Context) error {
		return testWatchRuntime(t, eng).bootstrapSync(runCtx, syncdomain.SyncDownloadOnly, pipe)
	})
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

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           syncdomain.SKService(),
		TrialInterval: 30 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(30 * time.Millisecond),
	})

	ta := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "trial.txt",
		DriveID: driveid.New(syncfixture.EngineTestDriveID),
		ItemID:  "trial-item",
	}, 99, nil)
	require.NotNil(t, ta)

	results <- syncdomain.ActionCompletion{
		ActionID:      99,
		Path:          "trial.txt",
		ActionType:    syncdomain.ActionDownload,
		DriveID:       driveid.New(syncfixture.EngineTestDriveID),
		Success:       false,
		HTTPStatus:    http.StatusInternalServerError,
		Err:           graph.ErrServerError,
		ErrMsg:        "trial failure",
		IsTrial:       true,
		TrialScopeKey: syncdomain.SKService(),
	}

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventTrialTimerArmed
	}, "trial timer re-armed after trial failure")
	cancel()

	block, ok := getTestBlockScope(eng, syncdomain.SKService())
	require.True(t, ok, "service scope should remain blocked after trial failure")
	assert.NotZero(t, block.TrialInterval,
		"trial failure should leave the scope timed and blocked")

	assert.True(t, isTestBlockScopeed(eng, syncdomain.SKService()),
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
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, eng.baseline.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "blocked-item",
		Path:     "blocked.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "blocked-hash",
		Size:     42,
	}}, "", driveID))

	setTestBlockScope(t, eng, &syncdomain.BlockScope{
		Key:           testThrottleScope(),
		TrialInterval: 10 * time.Millisecond,
		NextTrialAt:   eng.nowFunc().Add(10 * time.Millisecond),
	})
	_, err := eng.baseline.RecordBlockedRetryWork(ctx, syncfixture.TestRetryWorkKey(blockedPath, "", syncdomain.ActionDownload), testThrottleScope())
	require.NoError(t, err)
	blocked := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    blockedPath,
		DriveID: driveID,
		ItemID:  "blocked-item",
		View: &syncdomain.PathView{
			Path: blockedPath,
			Remote: &syncdomain.RemoteState{
				DriveID:  driveID,
				ItemID:   "blocked-item",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "blocked-hash",
				Size:     42,
			},
		},
	}, 2, nil)
	require.NotNil(t, blocked)
	rt.holdAction(blocked, heldReasonScope, testThrottleScope(), time.Time{})

	ta := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "trial.txt",
		DriveID: driveID,
		ItemID:  "trial-item",
	}, 1, nil)
	require.NotNil(t, ta)

	results <- syncdomain.ActionCompletion{
		ActionID:      1,
		Path:          "trial.txt",
		ActionType:    syncdomain.ActionUpload,
		DriveID:       driveID,
		Success:       true,
		IsTrial:       true,
		TrialScopeKey: testThrottleScope(),
	}

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventScopeReleased && event.ScopeKey == testThrottleScope()
	}, "trial success released blocked scope")

	retried := readReadyAction(t, rt.sched.dispatchCh)
	require.Equal(t, blockedPath, retried.Action.Path,
		"trial success should re-dispatch blocked retry work without external observation")
	assert.Equal(t, syncdomain.ActionDownload, retried.Action.Type)

	cancel()
	assert.False(t, isTestBlockScopeed(eng, testThrottleScope()),
		"trial success should clear the block scope")
}

// Validates: R-2.10.2, R-2.10.10
func TestPhase0_ObserveLocalChanges_ClearsResolvedFilePermissionIssueWithoutDeletingRetryWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	require.NoError(t, os.MkdirAll(filepath.Join(eng.syncRoot, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(eng.syncRoot, "docs", "file.txt"), []byte("ok"), 0o600))

	syncstoretest.SeedObservationIssueRowForTest(t, eng.baseline, &syncdomain.ObservationIssue{
		Path:      "docs/file.txt",
		DriveID:   eng.driveID,
		IssueType: syncdomain.IssueLocalReadDenied,
	})

	retryRow := syncfixture.TestRetryWorkRow("docs/file.txt", "", syncdomain.ActionUpload)
	retryRow.AttemptCount = 3
	retryRow.NextRetryAt = eng.nowFunc().Add(time.Minute).UnixNano()
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &retryRow))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	_, err = testEngineFlow(t, eng).refreshAndCommitLocalCurrentState(ctx, bl)
	require.NoError(t, err)

	observationIssues, err := eng.baseline.ListObservationIssues(ctx)
	require.NoError(t, err)
	assert.Empty(t, observationIssues)

	rows, err := eng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "docs/file.txt", rows[0].Path)
	assert.Equal(t, syncdomain.ActionUpload, rows[0].ActionType)
}

// Validates: R-2.10.5
func TestPhase0_BlockScopeFailureDoesNotReadmitDependentEarly(t *testing.T) {
	t.Parallel()

	results, cancel, eng := startDrainLoop(t)
	defer cancel()
	recorder := attachDebugEventRecorder(eng)
	rt := testWatchRuntime(t, eng)

	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	parent := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "parent.txt",
		DriveID: driveID,
		ItemID:  "parent-item",
	}, 1, nil)
	require.NotNil(t, parent)

	child := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "child.txt",
		DriveID: driveID,
		ItemID:  "child-item",
	}, 2, []int64{1})
	require.Nil(t, child)

	rt.sched.dispatchCh <- parent
	readReady(t, rt.sched.dispatchCh)

	results <- syncdomain.ActionCompletion{
		ActionID:   1,
		Path:       "parent.txt",
		ActionType: syncdomain.ActionUpload,
		DriveID:    driveID,
		Success:    false,
		HTTPStatus: http.StatusTooManyRequests,
		RetryAfter: 25 * time.Millisecond,
		Err:        graph.ErrThrottled,
		ErrMsg:     "rate limited",
	}

	recorder.WaitForEvent(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventScopeActivated && event.ScopeKey == testThrottleScope()
	}, "block scope activated from action completion")

	select {
	case ta := <-rt.sched.dispatchCh:
		require.Failf(t, "dependent dispatched early", "unexpected path %s", ta.Action.Path)
	default:
	}
	cancel()
	assert.True(t, isTestBlockScopeed(eng, testThrottleScope()),
		"block scope should be activated from the action completion")

	retryRows, err := eng.baseline.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Len(t, retryRows, 1, "only the exact failed action should persist retry_work")
	assert.Equal(t, "parent.txt", retryRows[0].Path)
}

// Validates: R-2.1.6
func TestPhase0_RunFullReconciliationAsync_UsesBufferHandoffInsteadOfDirectDispatch(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	rt.resources.dirtyBuf = syncobserve.NewDirtyBuffer(eng.logger)

	rt.runFullRemoteRefreshAsync(ctx, bl)
	waitForRefreshDone(t, t.Context(), eng)

	select {
	case ta := <-ready:
		require.Failf(t, "reconciliation dispatched directly", "unexpected action %s", ta.Action.Path)
	default:
	}

	batch := rt.resources.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.False(t, batch.FullRefresh)
}
