package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// RunWatch tests
// ---------------------------------------------------------------------------

type stubSocketIOWakeSource struct {
	started chan struct{}
	runFn   func(context.Context, chan<- struct{}) error
}

func (s *stubSocketIOWakeSource) Run(ctx context.Context, wakes chan<- struct{}) error {
	if s.runFn != nil {
		return s.runFn(ctx, wakes)
	}

	select {
	case s.started <- struct{}{}:
	default:
	}

	<-ctx.Done()
	return nil
}

// Validates: R-2.8.3
func TestResolveDebounce_DefaultIsFiveSeconds(t *testing.T) {
	t.Parallel()

	eng := &Engine{}
	assert.Equal(t, 5*time.Second, eng.resolveDebounce(WatchOptions{}))
	assert.Equal(t, 1500*time.Millisecond, eng.resolveDebounce(WatchOptions{Debounce: 1500 * time.Millisecond}))
}

// Validates: R-2.8.3, R-6.10.10
// TestRunWatch_ContextCancel verifies that canceling the context causes
// RunWatch to return nil (clean shutdown), including during bootstrap.
func TestRunWatch_ContextCancel(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})

	mock := &engineMockClient{
		deltaFn: func(ctx context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncBidirectional, WatchOptions{
			// Use very long intervals so observers don't fire during test.
			PollInterval: 1 * time.Hour,
			Debounce:     1 * time.Hour,
		})
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not enter bootstrap observation before timeout")
	}

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunWatch")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout after context cancel")
	}

	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted
	}), "observers must not start after bootstrap cancellation")
}

// Validates: R-2.8.5
func TestRunWatch_WebsocketEnabledStartsWakeSource(t *testing.T) {
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
	eng.enableWebsocket = true
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ SocketIOEndpointFetcher, _ driveid.ID, opts SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{
			started: started,
			runFn: func(ctx context.Context, _ chan<- struct{}) error {
				require.NotNil(t, opts.LifecycleHook)
				opts.LifecycleHook(SocketIOLifecycleEvent{
					Type:    SocketIOLifecycleEventStarted,
					DriveID: driveID.String(),
				})
				opts.LifecycleHook(SocketIOLifecycleEvent{
					Type:    SocketIOLifecycleEventConnected,
					DriveID: driveID.String(),
					SID:     "sid-1",
				})
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				opts.LifecycleHook(SocketIOLifecycleEvent{
					Type:    SocketIOLifecycleEventStopped,
					DriveID: driveID.String(),
				})
				return nil
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
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "expected websocket wake source to start")
	}

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketWakeSourceStarted && event.DriveID == driveID.String()
	}, "websocket wake source started")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketConnected &&
			event.DriveID == driveID.String() &&
			event.Note == "sid=sid-1"
	}, "websocket connected")

	cancel()
	require.NoError(t, <-done)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketWakeSourceStopped && event.DriveID == driveID.String()
	}, "websocket wake source stopped")
}

// Validates: R-2.8.5
func TestRunWatch_WebsocketDisabledKeepsPollingOnly(t *testing.T) {
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
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ SocketIOEndpointFetcher, _ driveid.ID, _ SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{started: started}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncDownloadOnly, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	select {
	case <-started:
		require.FailNow(t, "wake source should not start when websocket is disabled")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	require.NoError(t, <-done)
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketWakeSourceStarted ||
			event.Type == engineDebugEventWebsocketConnected
	}), "disabled websocket mode must not emit websocket lifecycle events")
}

// Validates: R-2.8.5
func TestRunWatch_ScopedRootKeepsPollingOnly(t *testing.T) {
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
	eng.enableWebsocket = true
	eng.rootItemID = "shared-root"
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ SocketIOEndpointFetcher, _ driveid.ID, _ SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{started: started}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncDownloadOnly, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	select {
	case <-started:
		require.FailNow(t, "wake source should not start for shared-root watch")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	require.NoError(t, <-done)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketFallback && event.Note == "shared_root"
	}, "websocket shared-root fallback")
}

// Validates: R-2.8.3, R-6.8.9, R-6.10.10
func TestRunWatch_CancellationWinsOverFinalObserverExit(t *testing.T) {
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
	recorder := attachDebugEventRecorder(eng)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOptions{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunWatch should prefer graceful shutdown over all-observers-exited")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout after cancellation")
	}

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverExited && event.Observer == engineDebugObserverLocal
	}, "local observer exited")
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWatchStopped
	}, "watch stopped")
}

// TestRunWatch_UploadOnly_StartsRemoteObserver verifies that upload-only mode
// still observes remote truth via delta polling.
func TestRunWatch_UploadOnly_StartsRemoteObserver(t *testing.T) {
	t.Parallel()

	var deltaCalls atomic.Int32

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalls.Add(1)
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOptions{
			PollInterval: 50 * time.Millisecond,
			Debounce:     10 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunWatch")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout")
	}

	assert.Positive(t, deltaCalls.Load(), "upload-only watch should still issue delta calls")
	assert.True(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}), "upload-only watch must start a remote observer")
}

// TestEngine_ExternalDBChanged verifies the PRAGMA data_version detection.
func TestEngine_ExternalDBChanged(t *testing.T) {
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
	newTestWatchState(t, eng)

	// Seed the initial data_version.
	dv, err := eng.baseline.DataVersion(ctx)
	require.NoError(t, err)
	testWatchRuntime(t, eng).lastDataVersion = dv

	// No external changes yet — should return false.
	assert.False(t, externalDBChangedForTest(t, eng, ctx), "no external changes")

	// Engine's own writes don't change data_version, so repeated checks
	// should still return false.
	assert.False(t, externalDBChangedForTest(t, eng, ctx), "still no external changes")
}

// Validates: R-2.10.9, R-2.14.3
func TestEngine_HandleExternalChanges_RemotePermissionClearance(t *testing.T) {
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
	newTestWatchState(t, eng)

	clearedScope := SKPermRemoteWrite("Shared/TeamDocs")
	retainedScope := SKPermRemoteWrite("Shared/Other")

	setTestBlockScope(t, eng, &BlockScope{
		Key:       clearedScope,
		IssueType: IssueRemoteWriteDenied,
		BlockedAt: eng.nowFunc(),
	})
	setTestBlockScope(t, eng, &BlockScope{
		Key:       retainedScope,
		IssueType: IssueRemoteWriteDenied,
		BlockedAt: eng.nowFunc(),
	})

	_, err := eng.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "Shared/TeamDocs/file.txt",
		ActionType: ActionUpload,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   clearedScope,
		LastError:  "blocked by remote permission scope",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)
	_, err = eng.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "Shared/Other/file.txt",
		ActionType: ActionUpload,
		IssueType:  IssueRemoteWriteDenied,
		ScopeKey:   retainedScope,
		LastError:  "blocked by remote permission scope",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	require.NoError(t, eng.baseline.DiscardScope(ctx, clearedScope))

	handleExternalChangesForTest(t, eng, ctx)

	assert.False(t, isTestBlockScopeed(eng, clearedScope),
		"removing a remote permission scope externally should release that scope")
	assert.True(t, isTestBlockScopeed(eng, retainedScope),
		"unrelated remote permission scopes must remain blocked")

	retryable := readyRetryWorkForTest(t, eng.baseline, ctx, eng.nowFunc())
	assert.Empty(t, retryable, "clearing the last blocked write should forget the remote scope instead of retrying it")

	remainingBlocked := listRetryWorkForTest(t, eng.baseline, ctx)
	require.Len(t, remainingBlocked, 1, "only the uncleared blocked write should remain")
	assert.Equal(t, "Shared/Other/file.txt", remainingBlocked[0].Path)
	assert.True(t, remainingBlocked[0].Blocked)
	assert.Equal(t, retainedScope, remainingBlocked[0].ScopeKey)

	select {
	case <-testWatchRuntime(t, eng).retryTimerCh:
	default:
		require.Fail(t, "expected immediate retry wakeup after remote permission clearance")
	}
}

// TestRunWatch_ProcessBatch_EmptyPlan verifies that an empty plan (all
// changes classify to no-op) is handled gracefully.
func TestRunWatch_ProcessBatch_EmptyPlan(t *testing.T) {
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
	content := "hello"
	contentHash := hashContentQuickXor(t, content)

	// Seed baseline with a synced file.
	seedOutcomes := []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "already-synced.txt",
		DriveID:         driveID,
		ItemID:          "item-as",
		ItemType:        ItemTypeFile,
		RemoteHash:      contentHash,
		LocalHash:       contentHash,
		LocalSize:       5,
		LocalSizeKnown:  true,
		RemoteSize:      5,
		RemoteSizeKnown: true,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")
	writeLocalFile(t, syncRoot, "already-synced.txt", content)

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	setupWatchEngine(t, eng)
	safety := DefaultSafetyConfig()

	require.NoError(t, testWatchRuntime(t, eng).commitObservedItems(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-as",
		Path:     "already-synced.txt",
		ItemType: ItemTypeFile,
		Hash:     contentHash,
		Size:     5,
	}}, ""))

	dispatch := testWatchRuntime(t, eng).processDirtyBatch(ctx, DirtyBatch{
		Paths: []string{"already-synced.txt"},
	}, bl, SyncBidirectional, safety)
	assert.Nil(t, dispatch)
}

// TestRunWatch_Deduplication verifies that processBatch cancels in-flight
// actions for paths that appear in a new batch (B-122).
func TestRunWatch_Deduplication(t *testing.T) {
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

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	setupWatchEngine(t, eng)
	safety := DefaultSafetyConfig()

	require.NoError(t, testWatchRuntime(t, eng).commitObservedItems(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-1",
		Path:     "overlapping.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-v1",
		Size:     10,
	}}, ""))

	dispatch := testWatchRuntime(t, eng).processDirtyBatch(ctx, DirtyBatch{
		Paths: []string{"overlapping.txt"},
	}, bl, SyncBidirectional, safety)
	assert.NotNil(t, dispatch)

	// Verify the action is in-flight.
	require.True(t, testWatchRuntime(t, eng).depGraph.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after first batch")

	require.NoError(t, testWatchRuntime(t, eng).commitObservedItems(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-1",
		Path:     "overlapping.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash-v2",
		Size:     20,
	}}, ""))

	dispatch = testWatchRuntime(t, eng).processDirtyBatch(ctx, DirtyBatch{
		Paths: []string{"overlapping.txt"},
	}, bl, SyncBidirectional, safety)
	assert.NotNil(t, dispatch)

	// The second batch should have replaced the first.
	// We can't easily verify cancellation directly, but we can verify
	// the path is still tracked (new action replaced old one).
	assert.True(t, testWatchRuntime(t, eng).depGraph.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after second batch")
}

// TestRunWatch_DownloadOnly_StartsLocalObserver verifies that download-only
// mode still observes local truth even though uploads stay suppressed.
func TestRunWatch_DownloadOnly_StartsLocalObserver(t *testing.T) {
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
	recorder := attachDebugEventRecorder(eng)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncDownloadOnly, WatchOptions{
			PollInterval: 1 * time.Hour,
			Debounce:     10 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventBootstrapQuiesced
	}, "bootstrap quiesced")
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	// Create a local file. Download-only still observes it, but upload actions
	// remain suppressed by mode admission.
	writeLocalFile(t, syncRoot, "local-only.txt", "should-be-ignored")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunWatch")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout")
	}

	bl, err := eng.baseline.Load(context.Background())
	require.NoError(t, err)
	_, found := bl.GetByPath("local-only.txt")
	assert.False(t, found, "download-only watch mode must ignore local-only files created after bootstrap")
	assert.True(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}), "download-only watch must start a local observer")
}

// TestRunWatch_AllObserversDead_ReturnsError verifies that RunWatch returns an
// error (not nil) when all observers exit. Uses upload-only mode with a .nosync
// guard file so the local observer fails immediately.
func TestRunWatch_AllObserversDead_ReturnsError(t *testing.T) {
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

	// Create .nosync guard file so local observer exits immediately with error.
	writeLocalFile(t, syncRoot, ".nosync", "")

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(t.Context(), SyncUploadOnly, WatchOptions{
			PollInterval: 1 * time.Hour,
			Debounce:     10 * time.Millisecond,
		})
	}()

	select {
	case err := <-done:
		require.Error(t, err, "RunWatch returned nil, want error indicating all observers exited")

		if !errors.Is(err, ErrNosyncGuard) {
			// Should be the "all observers exited" wrapper, but the observer error
			// should be logged as a warning. Check it's not a random error.
			assert.Equal(t, "sync: all observers exited", err.Error())
		}
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout (should exit when all observers die)")
	}
}

// TestRunWatch_WatchLimitExhausted_FallsBackToPolling verifies that when the
// local observer returns ErrWatchLimitExhausted, the engine does NOT consider
// the observer dead. Instead it falls back to periodic full scanning and
// RunWatch continues until the context is canceled.
func TestRunWatch_WatchLimitExhausted_FallsBackToPolling(t *testing.T) {
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

	// Create a subdirectory so the ENOSPC watcher has something to fail on.
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "subdir"), 0o750))

	// Inject a watcher factory that returns ENOSPC after the first Add (root).
	watcher := newEnospcWatcher(1)
	eng.localWatcherFactory = func() (FsWatcher, error) {
		return watcher, nil
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOptions{
			PollInterval: 100 * time.Millisecond, // short for fast test
			Debounce:     10 * time.Millisecond,
		})
	}()

	select {
	case <-watcher.Failures():
	case <-time.After(10 * time.Second):
		require.Fail(t, "local watcher did not hit ENOSPC before timeout")
	}

	cancel()

	select {
	case err := <-done:
		// RunWatch should return nil (clean shutdown), NOT an "all observers exited" error.
		require.NoError(t, err, "RunWatch should return nil on clean shutdown with fallback polling")
	case <-time.After(10 * time.Second):
		require.Fail(t, "RunWatch did not return within timeout")
	}
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_ShutdownStopsRetryAndTrialTimers(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)

	clock := newManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	installManualClock(eng.Engine, clock)

	ctx := t.Context()
	setTestBlockScope(t, eng, &BlockScope{
		Key:           SKService(),
		IssueType:     IssueServiceOutage,
		TimingSource:  ScopeTimingServerRetryAfter,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   eng.nowFunc().Add(5 * time.Second),
	})
	_, err := eng.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "held.txt",
		ActionType: ActionUpload,
		IssueType:  IssueServiceOutage,
		ScopeKey:   SKService(),
		LastError:  "held by service scope",
		Blocked:    true,
	}, nil)
	require.NoError(t, err)

	_, err = eng.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:       "retry.txt",
		ActionType: ActionDownload,
		IssueType:  IssueServiceOutage,
		LastError:  "retry later",
	}, func(_ int) time.Duration {
		return 5 * time.Second
	})
	require.NoError(t, err)
	watchCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(watchCtx, SyncUploadOnly, WatchOptions{
			PollInterval: 1 * time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventTrialTimerArmed
	}, "trial timer armed")
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventRetrySweepCompleted
	}, "initial retry sweep completed")

	cancel()
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventShutdownStarted
	}, "shutdown started")

	clock.Advance(10 * time.Second)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "watch loop did not exit after timer shutdown test")
	}

	recorder.requireOrderedSubsequence(t, []func(engineDebugEvent) bool{
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventShutdownStarted
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventWatchStopped
		},
	}, "local observer start, shutdown, and watch stop should occur in order")
	for _, forbidden := range []engineDebugEventType{
		engineDebugEventRetryTimerFired,
		engineDebugEventRetrySweepStarted,
		engineDebugEventTrialTimerFired,
		engineDebugEventTrialSweepStarted,
	} {
		recorder.requireNoEventAfter(t, func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventShutdownStarted
		}, func(event engineDebugEvent) bool {
			return event.Type == forbidden
		}, fmt.Sprintf("%s must not occur after shutdown starts", forbidden))
	}
}

func installTickerCreatedSignal(eng *testEngine, interval time.Duration) <-chan struct{} {
	tickerCreated := make(chan struct{})
	var tickerCreatedOnce atomic.Bool
	origNewTicker := eng.newTicker
	eng.newTicker = func(nextInterval time.Duration) syncTicker {
		ticker := origNewTicker(nextInterval)
		if nextInterval == interval && tickerCreatedOnce.CompareAndSwap(false, true) {
			close(tickerCreated)
		}

		return ticker
	}

	return tickerCreated
}

func installAfterFuncCreatedSignal(eng *testEngine, delay time.Duration) <-chan struct{} {
	timerCreated := make(chan struct{})
	var timerCreatedOnce atomic.Bool
	origAfterFunc := eng.afterFunc
	eng.afterFunc = func(nextDelay time.Duration, fn func()) syncTimer {
		timer := origAfterFunc(nextDelay, fn)
		if nextDelay == delay && timerCreatedOnce.CompareAndSwap(false, true) {
			close(timerCreated)
		}

		return timer
	}

	return timerCreated
}

func waitForSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		require.FailNow(t, description)
	}
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_ShutdownDropsReconcileResult(t *testing.T) {
	t.Parallel()

	reconcileStarted := make(chan struct{})
	reconcileReleased := make(chan struct{})
	var reconcileStartedOnce atomic.Bool
	var deltaCalls atomic.Int32
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			call := deltaCalls.Add(1)
			if call == 1 {
				return deltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
				}, "bootstrap-token"), nil
			}
			if reconcileStartedOnce.CompareAndSwap(false, true) {
				close(reconcileStarted)
			}
			<-reconcileReleased
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
				{ID: "late-item", Name: "late.txt", ParentID: "root", DriveID: driveid.New(engineTestDriveID), Size: 10, QuickXorHash: "late-hash"},
			}, "reconcile-token"), nil
		},
	}
	eng, _ := newTestEngine(t, mock)
	recorder := attachDebugEventRecorder(eng)
	clock := newManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	installManualClock(eng.Engine, clock)
	watcher := newSignalingWatcher()
	eng.localWatcherFactory = func() (FsWatcher, error) {
		return watcher, nil
	}
	reconcileTimerCreated := installAfterFuncCreatedSignal(eng, 15*time.Minute)
	saveObservationCursorForTest(t, eng.baseline, t.Context(), engineTestDriveID, "seed-token")
	require.NoError(t, eng.baseline.MarkFullRemoteReconcile(
		t.Context(),
		driveid.New(engineTestDriveID),
		clock.Now().Add(-fullRemoteReconcileInterval+15*time.Minute),
	))

	watchCtx, cancel := context.WithCancel(t.Context())
	eng.watchRuntimeHook = func(rt *watchRuntime) {
		rt.afterReconcileCommit = func() {
			cancel()
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(watchCtx, SyncUploadOnly, WatchOptions{
			PollInterval: 1 * time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")
	waitForSignal(t, watcher.Added(), "local watch setup did not add any watcher")
	waitForSignal(t, reconcileTimerCreated, "reconcile timer was not created")

	clock.Advance(15 * time.Minute)
	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventReconcileStarted
	}, "reconcile started")
	waitForSignal(t, reconcileStarted, "reconcile delta fetch did not start")

	close(reconcileReleased)

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventShutdownStarted
	}, "shutdown started")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "watch loop did not exit after dropping reconcile result")
	}

	recorder.requireOrderedSubsequence(t, []func(engineDebugEvent) bool{
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventReconcileStarted
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventShutdownStarted
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventReconcileDroppedOnShutdown
		},
		func(event engineDebugEvent) bool {
			return event.Type == engineDebugEventWatchStopped
		},
	}, "reconcile should be dropped after shutdown starts")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventReconcileDroppedOnShutdown
	}, 1, "expected exactly one reconcile_dropped_on_shutdown event")
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventReconcileApplied
	}), "reconcile result should not be applied after shutdown starts")
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatch_FallbackSleepHonorsCancellation(t *testing.T) {
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
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "subdir"), 0o750))
	recorder := attachDebugEventRecorder(eng)

	clock := newManualClock(time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC))
	clock.SetJitter(5 * time.Second)
	installManualClock(eng.Engine, clock)

	sleepStarted := make(chan struct{})
	var sleepStartedOnce atomic.Bool
	origSleep := eng.sleepFn
	eng.sleepFn = func(ctx context.Context, delay time.Duration) error {
		if sleepStartedOnce.CompareAndSwap(false, true) {
			close(sleepStarted)
		}
		return origSleep(ctx, delay)
	}

	watcher := newEnospcWatcher(1)
	eng.localWatcherFactory = func() (FsWatcher, error) {
		return watcher, nil
	}
	degradedInterval := localRefreshIntervalForMode(localRefreshModeWatchDegraded)
	tickerCreated := installTickerCreatedSignal(eng, degradedInterval)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, SyncUploadOnly, WatchOptions{
			PollInterval: 1 * time.Second,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started")
	waitForSignal(t, tickerCreated, "fallback ticker was not created")
	clock.Advance(degradedInterval)
	waitForSignal(t, sleepStarted, "fallback jitter sleep did not start")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not exit promptly after fallback cancellation")
	}

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStopped
	}, "fallback stopped")
}

// ---------------------------------------------------------------------------
// Plan invariant guard tests
// ---------------------------------------------------------------------------

// TestExecutePlan_ActionsDepsLengthMismatch verifies that executePlan returns
// cleanly (no panic) when plan.Actions and plan.Deps have mismatched lengths.
func TestExecutePlan_ActionsDepsLengthMismatch(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)

	// Create a plan with mismatched Actions and Deps.
	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionDownload, Path: "file.txt"},
			{Type: ActionDownload, Path: "file2.txt"},
		},
		Deps: [][]int{{1}}, // only 1 dep entry for 2 actions
	}

	report := &Report{}

	// Should return cleanly without panic.
	require.NoError(t, newOneShotRunner(eng.Engine).executePlan(t.Context(), plan, report, nil))

	// Invariant violation should surface in the report.
	assert.Equal(t, len(plan.Actions), report.Failed)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0].Error(), "invariant violation")
}

// ---------------------------------------------------------------------------
// Close() cleanup and idempotency
// ---------------------------------------------------------------------------

// TestEngine_Close_CleansStaleAndIsIdempotent verifies that Close() cleans
// stale session files and remains idempotent even when a test runtime exists.
func TestEngine_Close_CleansStaleAndIsIdempotent(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")
	dataDir := filepath.Join(tmpDir, "data")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	require.NoError(t, os.MkdirAll(dataDir, 0o750))

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DataDir:   dataDir,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err)
	testEng := newFlowBackedTestEngine(eng)

	// Register a test runtime to prove Close operates on engine-owned resources
	// only; watch observers now belong to the runtime and are cleaned up by the
	// watch coordinator, not by Engine.Close.
	setupWatchEngine(t, testEng)
	testWatchRuntime(t, testEng).remoteObs = &RemoteObserver{}
	testWatchRuntime(t, testEng).localObs = &LocalObserver{}

	// First Close should succeed.
	require.NoError(t, eng.Close(t.Context()))

	// Second Close must not panic (idempotency). The baseline DB is already
	// closed. A second Close should still be a clean no-op.
	assert.NotPanics(t, func() {
		assert.NoError(t, eng.Close(t.Context()))
	}, "second Close must not panic")
}
