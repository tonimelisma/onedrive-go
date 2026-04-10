package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
		done <- eng.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{
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
	eng.socketIOWakeSourceFactory = func(_ synctypes.SocketIOEndpointFetcher, _ driveid.ID, opts syncobserve.SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{
			started: started,
			runFn: func(ctx context.Context, _ chan<- struct{}) error {
				require.NotNil(t, opts.LifecycleHook)
				opts.LifecycleHook(syncobserve.SocketIOLifecycleEvent{
					Type:    syncobserve.SocketIOLifecycleEventStarted,
					DriveID: driveID.String(),
				})
				opts.LifecycleHook(syncobserve.SocketIOLifecycleEvent{
					Type:    syncobserve.SocketIOLifecycleEventConnected,
					DriveID: driveID.String(),
					SID:     "sid-1",
				})
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				opts.LifecycleHook(syncobserve.SocketIOLifecycleEvent{
					Type:    syncobserve.SocketIOLifecycleEventStopped,
					DriveID: driveID.String(),
				})
				return nil
			},
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
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
	eng.socketIOWakeSourceFactory = func(_ synctypes.SocketIOEndpointFetcher, _ driveid.ID, _ syncobserve.SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{started: started}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
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
	eng.rootItemID = "scoped-root"
	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ synctypes.SocketIOEndpointFetcher, _ driveid.ID, _ syncobserve.SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{started: started}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	select {
	case <-started:
		require.FailNow(t, "wake source should not start for scoped-root watch")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	require.NoError(t, <-done)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketFallback && event.Note == "scoped_root"
	}, "websocket scoped-root fallback")
}

// Validates: R-2.4.5, R-2.8.5
func TestRunWatch_SyncPathsScopedPollingDisablesWebsocket(t *testing.T) {
	t.Parallel()

	const docsPath = "Docs"

	mock := &engineMockClient{
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			if remotePath == docsPath {
				return &graph.Item{ID: "docs-id", Name: docsPath, IsFolder: true}, nil
			}

			return nil, graph.ErrNotFound
		},
		folderDeltaFn: func(_ context.Context, _ driveid.ID, folderID, token string) ([]graph.Item, string, error) {
			return nil, "docs-token", nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.driveType = driveid.DriveTypePersonal
	eng.enableWebsocket = true
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/" + docsPath},
	}

	recorder := attachDebugEventRecorder(eng)
	started := make(chan struct{}, 1)
	eng.socketIOWakeSourceFactory = func(_ synctypes.SocketIOEndpointFetcher, _ driveid.ID, _ syncobserve.SocketIOWakeSourceOptions) socketIOWakeSourceRunner {
		return &stubSocketIOWakeSource{started: started}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}, "remote observer started")

	select {
	case <-started:
		require.FailNow(t, "wake source should not start for sync_paths-scoped watch")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	require.NoError(t, <-done)
	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWebsocketFallback && event.Note == "sync_paths"
	}, "websocket sync_paths fallback")
}

// Validates: R-3.4.2
func TestRunWatch_RootDeltaSteadyStateProcessesShortcutFollowUp(t *testing.T) {
	t.Parallel()

	const shortcutContent = "shortcut report"

	driveID := driveid.New(engineTestDriveID)
	remoteDriveID := driveid.New("0000000000000099")
	var deltaCalls atomic.Int32

	mock := &engineMockClient{
		deltaFn: func(ctx context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			switch deltaCalls.Add(1) {
			case 1:
				return deltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
				}, "token-bootstrap"), nil
			case 2:
				return deltaPageWithItems([]graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{
						ID:            "sc-1",
						Name:          "SharedDocs",
						ParentID:      "root",
						DriveID:       driveID,
						IsFolder:      true,
						RemoteDriveID: remoteDriveID.String(),
						RemoteItemID:  "remote-item-1",
					},
				}, "token-watch"), nil
			default:
				<-ctx.Done()
				return nil, ctx.Err()
			}
		},
		listChildrenRecursiveFn: func(_ context.Context, gotDriveID driveid.ID, folderID string) ([]graph.Item, error) {
			assert.Equal(t, remoteDriveID, gotDriveID)
			assert.Equal(t, "remote-item-1", folderID)
			return []graph.Item{{
				ID:           "shortcut-file",
				Name:         "report.txt",
				ParentID:     "remote-item-1",
				DriveID:      remoteDriveID,
				QuickXorHash: hashContentQuickXor(t, shortcutContent),
				Size:         int64(len(shortcutContent)),
			}}, nil
		},
		downloadFn: func(_ context.Context, gotDriveID driveid.ID, itemID string, w io.Writer) (int64, error) {
			assert.Equal(t, remoteDriveID, gotDriveID)
			assert.Equal(t, "shortcut-file", itemID)
			n, err := w.Write([]byte(shortcutContent))
			return int64(n), err
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	require.Eventually(t, func() bool {
		data, ok := readFileUnderRootIfExists(t, syncRoot, filepath.Join("SharedDocs", "report.txt"))
		return ok && string(data) == shortcutContent
	}, 10*time.Second, 25*time.Millisecond)

	cancel()
	require.NoError(t, <-done)

	shortcut, found, err := eng.baseline.GetShortcut(context.Background(), "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, shortcut)
	assert.Equal(t, "SharedDocs", shortcut.LocalPath)
	assert.GreaterOrEqual(t, deltaCalls.Load(), int32(2))
}

// Validates: R-2.4.5, R-3.4.2
func TestProcessCommittedScopedWatchBatch_ProcessesShortcutFollowUp(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remoteDriveID := driveid.New("0000000000000098")

	mock := &engineMockClient{
		listChildrenRecursiveFn: func(_ context.Context, gotDriveID driveid.ID, folderID string) ([]graph.Item, error) {
			assert.Equal(t, remoteDriveID, gotDriveID)
			assert.Equal(t, "remote-item-1", folderID)
			return []graph.Item{{
				ID:       "shortcut-file",
				Name:     "report.txt",
				ParentID: "remote-item-1",
				DriveID:  remoteDriveID,
			}}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	rt := newWatchRuntime(eng.Engine)
	rt.setScopeSnapshot(syncscope.Snapshot{}, 1)

	events, committed := rt.processCommittedScopedWatchBatch(t.Context(), emptyBaseline(), remoteFetchResult{
		events: []synctypes.ChangeEvent{{
			Source:        synctypes.SourceRemote,
			Type:          synctypes.ChangeShortcut,
			DriveID:       driveID,
			ItemID:        "sc-1",
			Path:          "SharedDocs",
			ItemType:      synctypes.ItemTypeFolder,
			RemoteDriveID: remoteDriveID.String(),
			RemoteItemID:  "remote-item-1",
		}},
	}, false)
	require.True(t, committed)
	require.Len(t, events, 1)
	assert.Equal(t, "SharedDocs/report.txt", events[0].Path)
}

// Validates: R-2.4.5, R-3.4.2
func TestRunWatch_SyncPathsSteadyStateProcessesShortcutFollowUp(t *testing.T) {
	t.Parallel()

	const (
		docsPath        = "Docs"
		shortcutContent = "sync paths shortcut report"
		tokenWatch      = "token-watch"
	)

	driveID := driveid.New(engineTestDriveID)
	remoteDriveID := driveid.New("0000000000000097")
	var folderDeltaCalls atomic.Int32

	mock := &engineMockClient{
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			switch remotePath {
			case docsPath:
				return &graph.Item{ID: "docs-id", Name: docsPath, IsFolder: true}, nil
			default:
				return nil, graph.ErrNotFound
			}
		},
		folderDeltaFn: func(_ context.Context, gotDriveID driveid.ID, folderID, _ string) ([]graph.Item, string, error) {
			assert.Equal(t, driveID, gotDriveID)
			assert.Equal(t, "docs-id", folderID)

			switch folderDeltaCalls.Add(1) {
			case 1:
				return nil, "token-bootstrap", nil
			case 2:
				return []graph.Item{
					{
						ID:            "sc-1",
						Name:          "SharedDocs",
						ParentID:      "docs-id",
						DriveID:       driveID,
						IsFolder:      true,
						RemoteDriveID: remoteDriveID.String(),
						RemoteItemID:  "remote-item-1",
					},
				}, tokenWatch, nil
			default:
				return nil, tokenWatch, nil
			}
		},
		listChildrenRecursiveFn: func(_ context.Context, gotDriveID driveid.ID, folderID string) ([]graph.Item, error) {
			assert.Equal(t, remoteDriveID, gotDriveID)
			assert.Equal(t, "remote-item-1", folderID)
			return []graph.Item{{
				ID:           "shortcut-file",
				Name:         "report.txt",
				ParentID:     "remote-item-1",
				DriveID:      remoteDriveID,
				QuickXorHash: hashContentQuickXor(t, shortcutContent),
				Size:         int64(len(shortcutContent)),
			}}, nil
		},
		downloadFn: func(_ context.Context, gotDriveID driveid.ID, itemID string, w io.Writer) (int64, error) {
			assert.Equal(t, remoteDriveID, gotDriveID)
			assert.Equal(t, "shortcut-file", itemID)
			n, err := w.Write([]byte(shortcutContent))
			return int64(n), err
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	eng.driveType = driveid.DriveTypePersonal
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/" + docsPath},
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
			PollInterval: time.Hour,
			Debounce:     5 * time.Millisecond,
		})
	}()

	require.Eventually(t, func() bool {
		data, ok := readFileUnderRootIfExists(t, syncRoot, filepath.Join("Docs", "SharedDocs", "report.txt"))
		return ok && string(data) == shortcutContent
	}, 10*time.Second, 25*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	assert.GreaterOrEqual(t, folderDeltaCalls.Load(), int32(2))
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
		done <- eng.RunWatch(ctx, synctypes.SyncUploadOnly, synctypes.WatchOpts{
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

// TestRunWatch_UploadOnly_SkipsRemoteObserver verifies that upload-only mode
// does not start a remote observer (no delta polling).
func TestRunWatch_UploadOnly_SkipsRemoteObserver(t *testing.T) {
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
		done <- eng.RunWatch(ctx, synctypes.SyncUploadOnly, synctypes.WatchOpts{
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

	assert.Zero(t, deltaCalls.Load(), "upload-only watch should not issue any delta calls")
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverRemote
	}), "upload-only watch must not start a remote observer")
}

// Validates: R-6.2.5, R-6.4.2, R-6.4.3
// TestRunWatch_ProcessBatch_DeleteSafety verifies that the rolling delete
// counter in watch mode holds delete actions when the threshold is exceeded,
// records them as actionable issues, and prevents dispatch.
func TestRunWatch_ProcessBatch_DeleteSafety(t *testing.T) {
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

	// Seed a large baseline so that a batch of deletes triggers delete safety.
	seedOutcomes := make([]synctypes.Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = synctypes.Outcome{
			Action:          synctypes.ActionDownload,
			Success:         true,
			Path:            fmt.Sprintf("file%02d.txt", i),
			DriveID:         driveID,
			ItemID:          fmt.Sprintf("item-%02d", i),
			ItemType:        synctypes.ItemTypeFile,
			RemoteHash:      fmt.Sprintf("hash%02d", i),
			LocalHash:       fmt.Sprintf("hash%02d", i),
			LocalSize:       100,
			LocalSizeKnown:  true,
			RemoteSize:      100,
			RemoteSizeKnown: true,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// Build a batch that would delete all 20 files.
	var batch []synctypes.PathChanges
	for _, o := range seedOutcomes {
		batch = append(batch, synctypes.PathChanges{
			Path: o.Path,
			RemoteEvents: []synctypes.ChangeEvent{{
				Source:    synctypes.SourceRemote,
				Type:      synctypes.ChangeDelete,
				Path:      o.Path,
				IsDeleted: true,
			}},
		})
	}

	setupWatchEngine(t, eng)

	// Install a rolling delete counter with threshold=10 on the engine. The
	// planner-level check is disabled so the engine can record durable
	// held-delete intent and keep non-delete work flowing.
	testWatchRuntime(t, eng).deleteCounter = syncdispatch.NewDeleteCounter(10, 5*time.Minute, time.Now)
	safety := &synctypes.SafetyConfig{DeleteSafetyThreshold: plannerSafetyMax}

	outbox := processBatchForTest(t, eng, ctx, batch, bl, safety)

	// Verify no actions were admitted into the watch loop outbox (all 20 are
	// deletes and the rolling counter held them as issues).
	assert.Empty(t, outbox)

	// Verify counter is now held.
	assert.True(t, testWatchRuntime(t, eng).deleteCounter.IsHeld(), "counter should be held")

	// Verify held deletes were recorded in the durable held-delete workflow.
	rows, listErr := eng.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, listErr, "ListHeldDeletesByState")
	assert.Len(t, rows, 20, "should have 20 held-delete entries")
}

// Validates: R-6.2.5, R-6.4.2
// TestRunWatch_ProcessBatch_DeleteSafety_NonDeletesFlow verifies that non-delete
// actions are dispatched even when the delete counter is held.
func TestRunWatch_ProcessBatch_DeleteSafety_NonDeletesFlow(t *testing.T) {
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

	// Seed baseline with files that will be "deleted" plus one path that
	// will produce a download (new remote file).
	seedOutcomes := make([]synctypes.Outcome, 15)
	for i := range 15 {
		seedOutcomes[i] = synctypes.Outcome{
			Action:          synctypes.ActionDownload,
			Success:         true,
			Path:            fmt.Sprintf("file%02d.txt", i),
			DriveID:         driveID,
			ItemID:          fmt.Sprintf("item-%02d", i),
			ItemType:        synctypes.ItemTypeFile,
			RemoteHash:      fmt.Sprintf("hash%02d", i),
			LocalHash:       fmt.Sprintf("hash%02d", i),
			LocalSize:       100,
			LocalSizeKnown:  true,
			RemoteSize:      100,
			RemoteSizeKnown: true,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// Build batch: 15 deletes + 1 new remote file (download).
	var batch []synctypes.PathChanges
	for _, o := range seedOutcomes {
		batch = append(batch, synctypes.PathChanges{
			Path: o.Path,
			RemoteEvents: []synctypes.ChangeEvent{{
				Source:    synctypes.SourceRemote,
				Type:      synctypes.ChangeDelete,
				Path:      o.Path,
				IsDeleted: true,
			}},
		})
	}

	// Add a new remote file that should produce a download.
	batch = append(batch, synctypes.PathChanges{
		Path: "newfile.txt",
		RemoteEvents: []synctypes.ChangeEvent{{
			Source:   synctypes.SourceRemote,
			Type:     synctypes.ChangeCreate,
			Path:     "newfile.txt",
			ItemID:   "item-new",
			DriveID:  driveID,
			Hash:     "newhash",
			Size:     50,
			ItemType: synctypes.ItemTypeFile,
		}},
	})

	setupWatchEngine(t, eng)

	// Install counter with threshold=10. 15 deletes > 10 → trips.
	testWatchRuntime(t, eng).deleteCounter = syncdispatch.NewDeleteCounter(10, 5*time.Minute, time.Now)
	safety := &synctypes.SafetyConfig{DeleteSafetyThreshold: plannerSafetyMax}

	outbox := processBatchForTest(t, eng, ctx, batch, bl, safety)

	// Counter should be held.
	assert.True(t, testWatchRuntime(t, eng).deleteCounter.IsHeld(), "counter should be held")

	require.Len(t, outbox, 1, "one non-delete action should be admitted into the watch loop outbox")
	assert.Equal(t, synctypes.ActionDownload, outbox[0].Action.Type)
	assert.Equal(t, "newfile.txt", outbox[0].Action.Path)

	// 15 held delete entries should exist.
	rows, listErr := eng.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, listErr, "ListHeldDeletesByState")
	assert.Len(t, rows, 15, "should have 15 held-delete entries")
}

// Validates: R-6.2.5, R-6.4.3
// TestRunWatch_ProcessBatch_DeleteSafety_BelowThreshold verifies that the
// rolling counter allows deletes through when below the threshold.
func TestRunWatch_ProcessBatch_DeleteSafety_BelowThreshold(t *testing.T) {
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

	// Seed baseline with 5 files.
	seedOutcomes := make([]synctypes.Outcome, 5)
	for i := range 5 {
		seedOutcomes[i] = synctypes.Outcome{
			Action:          synctypes.ActionDownload,
			Success:         true,
			Path:            fmt.Sprintf("file%02d.txt", i),
			DriveID:         driveID,
			ItemID:          fmt.Sprintf("item-%02d", i),
			ItemType:        synctypes.ItemTypeFile,
			RemoteHash:      fmt.Sprintf("hash%02d", i),
			LocalHash:       fmt.Sprintf("hash%02d", i),
			LocalSize:       100,
			LocalSizeKnown:  true,
			RemoteSize:      100,
			RemoteSizeKnown: true,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// Build batch: 5 deletes — below threshold of 10.
	var batch []synctypes.PathChanges
	for _, o := range seedOutcomes {
		batch = append(batch, synctypes.PathChanges{
			Path: o.Path,
			RemoteEvents: []synctypes.ChangeEvent{{
				Source:    synctypes.SourceRemote,
				Type:      synctypes.ChangeDelete,
				Path:      o.Path,
				IsDeleted: true,
			}},
		})
	}

	setupWatchEngine(t, eng)

	testWatchRuntime(t, eng).deleteCounter = syncdispatch.NewDeleteCounter(10, 5*time.Minute, time.Now)
	safety := &synctypes.SafetyConfig{DeleteSafetyThreshold: plannerSafetyMax}

	outbox := processBatchForTest(t, eng, ctx, batch, bl, safety)

	// Counter should NOT be held.
	assert.False(t, testWatchRuntime(t, eng).deleteCounter.IsHeld(), "counter should not trip at 5 < 10")

	require.Len(t, outbox, 5, "all 5 delete actions should be admitted into the watch loop outbox")
	for i := range outbox {
		assert.Equal(t, synctypes.ActionLocalDelete, outbox[i].Action.Type)
	}
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

// Validates: R-6.2.5, R-6.4.2
// TestEngine_HandleExternalChanges_DeleteSafetyClearance verifies that
// handleExternalChanges releases the delete counter when all held-delete rows
// have moved out of held state.
func TestEngine_HandleExternalChanges_DeleteSafetyClearance(t *testing.T) {
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

	// Install a held delete counter.
	testWatchRuntime(t, eng).deleteCounter = syncdispatch.NewDeleteCounter(10, 5*time.Minute, time.Now)
	testWatchRuntime(t, eng).deleteCounter.Add(15) // trips the counter
	require.True(t, testWatchRuntime(t, eng).deleteCounter.IsHeld())

	// Record held-delete rows.
	heldDeletes := []synctypes.HeldDeleteRecord{
		{Path: "file1.txt", DriveID: driveID, ItemID: "item-1", ActionType: synctypes.ActionRemoteDelete, State: synctypes.HeldDeleteStateHeld},
		{Path: "file2.txt", DriveID: driveID, ItemID: "item-2", ActionType: synctypes.ActionRemoteDelete, State: synctypes.HeldDeleteStateHeld},
	}
	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, heldDeletes))

	// handleExternalChanges should NOT release — rows still present.
	handleExternalChangesForTest(t, eng, ctx)
	assert.True(t, testWatchRuntime(t, eng).deleteCounter.IsHeld(), "should still be held with entries present")

	// Approve all held-delete entries (simulates `issues approve-deletes`).
	require.NoError(t, eng.baseline.ApproveHeldDeletes(ctx))

	// Now handleExternalChanges should release.
	handleExternalChangesForTest(t, eng, ctx)
	assert.False(t, testWatchRuntime(t, eng).deleteCounter.IsHeld(), "should be released after entries cleared")
}

// Validates: R-6.2.5, R-6.4.2
// TestEngine_HandleExternalChanges_PartialClear verifies that the counter
// stays held when only some held-delete entries leave held state.
func TestEngine_HandleExternalChanges_PartialClear(t *testing.T) {
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

	testWatchRuntime(t, eng).deleteCounter = syncdispatch.NewDeleteCounter(10, 5*time.Minute, time.Now)
	testWatchRuntime(t, eng).deleteCounter.Add(15)
	require.True(t, testWatchRuntime(t, eng).deleteCounter.IsHeld())

	// Record two held-delete entries.
	heldDeletes := []synctypes.HeldDeleteRecord{
		{Path: "file1.txt", DriveID: driveID, ItemID: "item-1", ActionType: synctypes.ActionRemoteDelete, State: synctypes.HeldDeleteStateHeld},
		{Path: "file2.txt", DriveID: driveID, ItemID: "item-2", ActionType: synctypes.ActionRemoteDelete, State: synctypes.HeldDeleteStateHeld},
	}
	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, heldDeletes))

	// Move only file1.txt out of held state — one entry remains held.
	require.NoError(t, eng.baseline.DeleteHeldDelete(ctx, driveID, synctypes.ActionRemoteDelete, "file1.txt", "item-1"))

	handleExternalChangesForTest(t, eng, ctx)
	assert.True(t, testWatchRuntime(t, eng).deleteCounter.IsHeld(), "should remain held with one entry still present")
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

	clearedScope := synctypes.SKPermRemote("Shared/TeamDocs")
	retainedScope := synctypes.SKPermRemote("Shared/Other")

	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:       clearedScope,
		IssueType: synctypes.IssueSharedFolderBlocked,
		BlockedAt: eng.nowFunc(),
	})
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:       retainedScope,
		IssueType: synctypes.IssueSharedFolderBlocked,
		BlockedAt: eng.nowFunc(),
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/TeamDocs/file.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ErrMsg:     "blocked by remote permission scope",
		ScopeKey:   clearedScope,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "Shared/Other/file.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ErrMsg:     "blocked by remote permission scope",
		ScopeKey:   retainedScope,
	}, nil))

	require.NoError(t, eng.baseline.ClearSyncFailure(ctx, "Shared/TeamDocs/file.txt", driveID))

	handleExternalChangesForTest(t, eng, ctx)

	assert.False(t, isTestScopeBlocked(eng, clearedScope),
		"clearing a remote permission issue externally should release that scope")
	assert.True(t, isTestScopeBlocked(eng, retainedScope),
		"unrelated remote permission scopes must remain blocked")

	retryable, err := eng.baseline.ListSyncFailuresForRetry(ctx, eng.nowFunc())
	require.NoError(t, err)
	assert.Empty(t, retryable, "clearing the last blocked write should forget the remote scope instead of retrying it")

	remainingIssues, err := eng.baseline.ListRemoteBlockedFailures(ctx)
	require.NoError(t, err)
	require.Len(t, remainingIssues, 1, "only the uncleared blocked write should remain")
	assert.Equal(t, "Shared/Other/file.txt", remainingIssues[0].Path)

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

	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed baseline with a synced file.
	seedOutcomes := []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
		Success:         true,
		Path:            "already-synced.txt",
		DriveID:         driveID,
		ItemID:          "item-as",
		ItemType:        synctypes.ItemTypeFile,
		RemoteHash:      "samehash",
		LocalHash:       "samehash",
		LocalSize:       5,
		LocalSizeKnown:  true,
		RemoteSize:      5,
		RemoteSizeKnown: true,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "")

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err, "Load")

	// A "change" that matches baseline exactly → planner produces empty plan.
	batch := []synctypes.PathChanges{{
		Path: "already-synced.txt",
		RemoteEvents: []synctypes.ChangeEvent{{
			Source:  synctypes.SourceRemote,
			Type:    synctypes.ChangeModify,
			Path:    "already-synced.txt",
			ItemID:  "item-as",
			DriveID: driveID,
			Hash:    "samehash",
			Size:    5,
		}},
	}}

	setupWatchEngine(t, eng)
	safety := synctypes.DefaultSafetyConfig()

	// Should return without error or dispatching actions.
	processBatchForTest(t, eng, ctx, batch, bl, safety)
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
	safety := synctypes.DefaultSafetyConfig()

	// First batch: download a file.
	batch1 := []synctypes.PathChanges{{
		Path: "overlapping.txt",
		RemoteEvents: []synctypes.ChangeEvent{{
			Source:  synctypes.SourceRemote,
			Type:    synctypes.ChangeCreate,
			Path:    "overlapping.txt",
			DriveID: driveID,
			ItemID:  "item-1",
			Hash:    "hash-v1",
			Size:    10,
		}},
	}}

	processBatchForTest(t, eng, ctx, batch1, bl, safety)

	// Verify the action is in-flight.
	require.True(t, testWatchRuntime(t, eng).depGraph.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after first batch")

	// Second batch: same path, different content. Should cancel the first.
	batch2 := []synctypes.PathChanges{{
		Path: "overlapping.txt",
		RemoteEvents: []synctypes.ChangeEvent{{
			Source:  synctypes.SourceRemote,
			Type:    synctypes.ChangeModify,
			Path:    "overlapping.txt",
			DriveID: driveID,
			ItemID:  "item-1",
			Hash:    "hash-v2",
			Size:    20,
		}},
	}}

	processBatchForTest(t, eng, ctx, batch2, bl, safety)

	// The second batch should have replaced the first.
	// We can't easily verify cancellation directly, but we can verify
	// the path is still tracked (new action replaced old one).
	assert.True(t, testWatchRuntime(t, eng).depGraph.HasInFlight("overlapping.txt"), "expected in-flight action for overlapping.txt after second batch")
}

// TestRunWatch_DownloadOnly_SkipsLocalObserver verifies that download-only mode
// does not start a local observer (no fsnotify watcher, no local change detection).
func TestRunWatch_DownloadOnly_SkipsLocalObserver(t *testing.T) {
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
		done <- eng.RunWatch(ctx, synctypes.SyncDownloadOnly, synctypes.WatchOpts{
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

	// Create a local file. If a local observer were running, it would detect
	// this and eventually produce a sync action. In download-only mode, the
	// local observer is skipped, so this file should be invisible to sync.
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
	assert.False(t, recorder.findEvent(func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}), "download-only watch must not start a local observer")
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
		done <- eng.RunWatch(t.Context(), synctypes.SyncUploadOnly, synctypes.WatchOpts{
			PollInterval: 1 * time.Hour,
			Debounce:     10 * time.Millisecond,
		})
	}()

	select {
	case err := <-done:
		require.Error(t, err, "RunWatch returned nil, want error indicating all observers exited")

		if !errors.Is(err, synctypes.ErrNosyncGuard) {
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
	eng.localWatcherFactory = func() (syncobserve.FsWatcher, error) {
		return watcher, nil
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncUploadOnly, synctypes.WatchOpts{
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
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:           synctypes.SKService(),
		IssueType:     synctypes.IssueServiceOutage,
		TimingSource:  synctypes.ScopeTimingServerRetryAfter,
		BlockedAt:     eng.nowFunc(),
		TrialInterval: 5 * time.Second,
		NextTrialAt:   eng.nowFunc().Add(5 * time.Second),
	})

	require.NoError(t, eng.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "retry.txt",
		DriveID:    eng.driveID,
		Direction:  synctypes.DirectionDownload,
		ActionType: synctypes.ActionDownload,
		Role:       synctypes.FailureRoleItem,
		Category:   synctypes.CategoryTransient,
		ErrMsg:     "retry later",
	}, func(_ int) time.Duration {
		return 5 * time.Second
	}))
	watchCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(watchCtx, synctypes.SyncUploadOnly, synctypes.WatchOpts{
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
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
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
	eng.localWatcherFactory = func() (syncobserve.FsWatcher, error) {
		return watcher, nil
	}
	reconcileTickerCreated := installTickerCreatedSignal(eng, 15*time.Minute)

	watchCtx, cancel := context.WithCancel(t.Context())
	eng.watchRuntimeHook = func(rt *watchRuntime) {
		rt.afterReconcileCommit = func() {
			cancel()
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(watchCtx, synctypes.SyncUploadOnly, synctypes.WatchOpts{
			PollInterval:      1 * time.Hour,
			Debounce:          5 * time.Millisecond,
			ReconcileInterval: 15 * time.Minute,
		})
	}()

	recorder.waitUntilSeen(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverStarted && event.Observer == engineDebugObserverLocal
	}, "local observer started")
	waitForSignal(t, watcher.Added(), "local watch setup did not add any watcher")
	waitForSignal(t, reconcileTickerCreated, "reconcile ticker was not created")

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
	eng.localWatcherFactory = func() (syncobserve.FsWatcher, error) {
		return watcher, nil
	}
	tickerCreated := installTickerCreatedSignal(eng, 1*time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- eng.RunWatch(ctx, synctypes.SyncUploadOnly, synctypes.WatchOpts{
			PollInterval: 1 * time.Second,
			Debounce:     5 * time.Millisecond,
		})
	}()

	recorder.waitForEvent(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventObserverFallbackStarted
	}, "fallback started")
	waitForSignal(t, tickerCreated, "fallback ticker was not created")
	clock.Advance(1 * time.Second)
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

func TestResolveConflict_KeepLocal_TransferFails(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, fmt.Errorf("upload failed: network error")
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "fail-upload.txt",
		DriveID:      driveID,
		ItemID:       "item-fu",
		ItemType:     synctypes.ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that would be uploaded.
	writeLocalFile(t, syncRoot, "fail-upload.txt", "local-data")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal))

	// Conflict should remain visible with a durable failure state. Resolution
	// execution is engine-owned and failure is persisted for retry/review,
	// rather than returned as an ad hoc CLI-side operation error.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after failed resolve")
	require.Len(t, remaining, 1, "expected 1 unresolved conflict")
	request, err := eng.baseline.GetConflictRequest(ctx, conflicts[0].ID)
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictStateResolveFailed, request.State)
	assert.Contains(t, request.ResolutionError, "upload failed")
}

// ---------------------------------------------------------------------------
// Regression: B-091 — resolveTransfer success path commits to baseline
// ---------------------------------------------------------------------------

// TestResolveConflict_KeepLocal_CommitsToBaseline verifies that after a
// successful keep_local resolution (upload), the baseline contains an updated
// entry with the new ItemID and hash from the upload response.
func TestResolveConflict_KeepLocal_CommitsToBaseline(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{
				ID:   "resolved-item-id",
				Name: name,
				ETag: "etag-resolved",
				// Empty hash = skip server-side verification (consistent with B-153).
				QuickXorHash: "",
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict.
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "baseline-commit.txt",
		DriveID:      driveID,
		ItemID:       "original-item-id",
		ItemType:     synctypes.ItemTypeFile,
		LocalHash:    "old-local-h",
		RemoteHash:   "old-remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that will be uploaded.
	writeLocalFile(t, syncRoot, "baseline-commit.txt", "resolved local")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal), "ResolveConflict")

	// Verify the baseline was updated with the new item from the upload.
	bl, loadErr := eng.baseline.Load(ctx)
	require.NoError(t, loadErr, "baseline.Load")

	entry, ok := bl.GetByPath("baseline-commit.txt")
	require.True(t, ok, "baseline entry not found after resolve")

	assert.Equal(t, "resolved-item-id", entry.ItemID)
	assert.Equal(t, "etag-resolved", entry.ETag)
	assert.NotEmpty(t, entry.LocalHash, "baseline LocalHash should be set (computed from local file)")

	// RemoteHash comes from the upload response's QuickXorHash, which is empty
	// in this mock (skip-verification pattern), so it should be empty.
	assert.Empty(t, entry.RemoteHash, "mock returns no hash")

	// "resolved local" is 14 bytes.
	assert.Equal(t, int64(14), entry.LocalSize)
	assert.True(t, entry.LocalSizeKnown)
}

// ---------------------------------------------------------------------------
// Regression: B-077 — resolveTransfer with minimal conflict record (no panic)
// ---------------------------------------------------------------------------

// TestResolveConflict_KeepLocal_MinimalRecord_NoPanic verifies that calling
// ResolveConflict with a sparse synctypes.ConflictRecord (only mandatory fields) does
// not cause a nil-pointer panic. The original bug was a nil-map panic when
// called without prior Execute().
func TestResolveConflict_KeepLocal_MinimalRecord_NoPanic(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{
				ID:   "minimal-resolved",
				Name: name,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Seed a conflict with only the mandatory fields — no hashes, no etag.
	outcomes := []synctypes.Outcome{{
		Action:       synctypes.ActionConflict,
		Success:      true,
		Path:         "minimal-conflict.txt",
		DriveID:      driveID,
		ItemID:       "item-min",
		ItemType:     synctypes.ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file.
	writeLocalFile(t, syncRoot, "minimal-conflict.txt", "minimal data")

	conflicts, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts")
	require.Len(t, conflicts, 1)

	// This must not panic. The original bug was a nil-map access in resolveTransfer.
	require.NoError(t, eng.ResolveConflict(ctx, conflicts[0].ID, synctypes.ResolutionKeepLocal), "ResolveConflict")

	// Verify the conflict is resolved.
	remaining, err := eng.ListConflicts(ctx)
	require.NoError(t, err, "ListConflicts after resolve")
	assert.Empty(t, remaining, "expected 0 unresolved conflicts")
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
	plan := &synctypes.ActionPlan{
		Actions: []synctypes.Action{
			{Type: synctypes.ActionDownload, Path: "file.txt"},
			{Type: synctypes.ActionDownload, Path: "file2.txt"},
		},
		Deps: [][]int{{1}}, // only 1 dep entry for 2 actions
	}

	report := &synctypes.SyncReport{}

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

	eng, err := NewEngine(t.Context(), &synctypes.EngineConfig{
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
	testWatchRuntime(t, testEng).remoteObs = &syncobserve.RemoteObserver{}
	testWatchRuntime(t, testEng).localObs = &syncobserve.LocalObserver{}

	// First Close should succeed.
	require.NoError(t, eng.Close(t.Context()))

	// Second Close must not panic (idempotency). The baseline DB is already
	// closed. A second Close should still be a clean no-op.
	assert.NotPanics(t, func() {
		assert.NoError(t, eng.Close(t.Context()))
	}, "second Close must not panic")
}
