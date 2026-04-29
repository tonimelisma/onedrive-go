package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.10.10
func TestWatchRuntime_BeginWatchDrain_ObserverChannelsStayRuntimeOwned(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.localBatches = make(chan localObservationBatch)
	rt.remoteBatches = make(chan remoteObservationBatch)
	rt.skippedItems = make(chan []SkippedItem)
	rt.observerErrs = make(chan error)
	rt.activeObservers = 1

	p := &watchPipeline{
		runtime:      rt,
		replanReady:  make(chan dirtyBatch),
		maintenanceC: make(chan time.Time),
	}

	rt.beginWatchDrain(t.Context(), p)

	assert.Nil(t, p.replanReady)
	assert.Nil(t, p.maintenanceC)
	assert.Nil(t, rt.localBatches)
	assert.Nil(t, rt.remoteBatches)
	assert.Nil(t, rt.skippedItems)
	assert.NotNil(t, rt.observerErrs, "runtime must keep draining observer exits while observers are still active")
	assert.Equal(t, 1, rt.activeObservers)
}

func TestWatchRuntime_RunWatchLoop_BootstrapPhaseQuiescesAndReturnsToRunning(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.enterBootstrap()

	err := rt.runWatchLoop(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	assert.Equal(t, watchRuntimePhaseRunning, rt.phase())
}

func TestWatchRuntime_RunNonDrainingWatchStep_BootstrapDispatchUsesSharedHandler(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	action := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "bootstrap.txt",
		DriveID: eng.driveID,
		ItemID:  "bootstrap-item",
	}, 1, nil)
	require.NotNil(t, action)
	rt.replaceOutbox([]*TrackedAction{action})
	rt.enterBootstrap()

	done, err := rt.runNonDrainingWatchStep(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, 1, rt.runningCount)
	assert.Empty(t, rt.currentOutbox())
}

func TestWatchRuntime_RunNonDrainingWatchStep_ContextCancelStartsDrain(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done, err := rt.runNonDrainingWatchStep(ctx, &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	assert.False(t, done)
	assert.True(t, rt.isDraining())
}

func TestWatchRuntime_RunNonDrainingWatchStep_ConsumesReplanReady(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	replanReady := make(chan dirtyBatch, 1)
	replanReady <- dirtyBatch{}

	type idleResult struct {
		done bool
		err  error
	}
	doneCh := make(chan idleResult, 1)
	go func() {
		done, err := rt.runNonDrainingWatchStep(t.Context(), &watchPipeline{
			runtime:     rt,
			replanReady: replanReady,
		}, nil)
		doneCh <- idleResult{done: done, err: err}
	}()

	select {
	case result := <-doneCh:
		assert.False(t, result.done)
		require.ErrorContains(t, result.err, "steady-state replan requires loaded baseline")
	case <-time.After(time.Second):
		require.FailNow(t, "runNonDrainingWatchStep did not consume replanReady while idle")
	}
}

// Validates: R-2.8.6
func TestWatchRuntime_RunNonDrainingWatchStepPrioritizesReadyReplanOverDispatch(t *testing.T) {
	t.Parallel()

	for attempt := range 32 {
		eng := newSingleOwnerEngine(t)
		rt := testWatchRuntime(t, eng)
		replanReady := make(chan dirtyBatch, 1)
		replanReady <- dirtyBatch{}

		queued := rt.depGraph.Add(&Action{
			Type:    ActionDownload,
			Path:    "old.txt",
			DriveID: eng.driveID,
			ItemID:  "old-item",
		}, int64(attempt+1), nil)
		require.NotNil(t, queued)
		rt.markQueued(queued)
		rt.replaceOutbox([]*TrackedAction{queued})

		done, err := rt.runNonDrainingWatchStep(t.Context(), &watchPipeline{
			runtime:     rt,
			replanReady: replanReady,
		}, nil)

		require.NoError(t, err)
		assert.False(t, done)
		assert.True(t, rt.hasPendingReplan())
		assert.Empty(t, rt.currentOutbox())
		assert.Empty(t, rt.queuedByID)
		assert.Equal(t, 0, rt.runningCount)
		select {
		case dispatched := <-rt.dispatchCh:
			require.Failf(t, "old work dispatched after ready replan", "action=%+v", dispatched)
		default:
		}
	}
}

// Validates: R-2.8.6
func TestWatchRuntime_PendingReplanLocalObservationFailureReschedulesDirtySignal(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.dirtyBuf = NewDirtyBuffer(eng.logger)
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	queued := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "old.txt",
		DriveID: eng.driveID,
		ItemID:  "old-item",
	}, 1, nil)
	require.NotNil(t, queued)
	rt.markQueued(queued)
	rt.replaceOutbox([]*TrackedAction{queued})
	rt.queuePendingReplan(dirtyBatch{FullRefresh: true})
	require.Empty(t, rt.currentOutbox())
	require.Empty(t, rt.queuedByID)
	require.NoError(t, os.RemoveAll(syncRoot))

	replanned, err := rt.runPendingWatchReplan(t.Context(), &watchPipeline{
		runtime: rt,
		bl:      bl,
		mode:    SyncBidirectional,
	})

	require.NoError(t, err)
	assert.True(t, replanned)
	assert.False(t, rt.hasPendingReplan())
	assert.Empty(t, rt.currentOutbox())
	assert.Empty(t, rt.queuedByID)
	assert.Equal(t, 1, rt.depGraph.InFlightCount(), "retired outbox must stay retired after failed replacement replan")
	batch := rt.dirtyBuf.FlushImmediate()
	require.NotNil(t, batch)
	assert.True(t, batch.FullRefresh)
}

// Validates: R-2.8.6
func TestWatchRuntime_IdleReplanLocalObservationFailureReschedulesDirtySignal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		fullRefresh bool
	}{
		{name: "dirty", fullRefresh: false},
		{name: "full_refresh", fullRefresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			eng, syncRoot := newTestEngine(t, &engineMockClient{})
			setupWatchEngine(t, eng)
			rt := testWatchRuntime(t, eng)
			rt.dirtyBuf = NewDirtyBuffer(eng.logger)
			bl, err := eng.baseline.Load(t.Context())
			require.NoError(t, err)
			require.NoError(t, os.RemoveAll(syncRoot))

			err = rt.handleWatchReplanReady(t.Context(), &watchPipeline{
				runtime: rt,
				bl:      bl,
				mode:    SyncBidirectional,
			}, dirtyBatch{FullRefresh: tc.fullRefresh})

			require.NoError(t, err)
			assert.False(t, rt.hasPendingReplan())
			assert.Empty(t, rt.currentOutbox())
			assert.Empty(t, rt.queuedByID)
			assert.Equal(t, 0, rt.runningCount)
			assert.Equal(t, 0, rt.depGraph.InFlightCount())
			batch := rt.dirtyBuf.FlushImmediate()
			require.NotNil(t, batch)
			assert.Equal(t, tc.fullRefresh, batch.FullRefresh)
		})
	}
}

// Validates: R-2.8.6
func TestWatchRuntime_QueuePendingReplanRetiresOldOutbox(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.transferWorkers = 2
	recorder := attachDebugEventRecorder(eng)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)

	queued := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "old.txt",
		DriveID: eng.driveID,
		ItemID:  "old-item",
	}, 1, nil)
	require.NotNil(t, queued)
	rt.markQueued(queued)
	rt.replaceOutbox([]*TrackedAction{queued})

	rt.queuePendingReplan(dirtyBatch{})

	assert.True(t, rt.hasPendingReplan())
	assert.True(t, rt.canPrepareNow())
	assert.Empty(t, rt.currentOutbox())
	assert.Empty(t, rt.queuedByID)
	assert.Equal(t, 1, rt.depGraph.InFlightCount(), "retiring old outbox must not complete dependency nodes as success")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventOldOutboxRetired && event.Count == 1
	}, 1, "pending replan should emit retired outbox count")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventDispatchPausedForReplan &&
			!event.At.IsZero() &&
			event.Count == 1 &&
			event.Outbox == 1 &&
			event.Running == 0 &&
			event.IdleWorkers == 2
	}, 1, "pending replan should record queue and idle-worker counters before retiring old outbox")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventOldOutboxRetired &&
			!event.At.IsZero() &&
			event.Delay == 0 &&
			event.Count == 1 &&
			event.Outbox == 0 &&
			event.Running == 0 &&
			event.IdleWorkers == 2
	}, 1, "retired outbox instrumentation should record timestamped post-retirement counters")
}

// Validates: R-2.8.6
func TestWatchRuntime_PendingReplanRetiresDependentsReleasedByRunningAction(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.transferWorkers = 2
	recorder := attachDebugEventRecorder(eng)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	root := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "parent.txt",
		DriveID: eng.driveID,
		ItemID:  "parent-item",
	}, 1, nil)
	require.NotNil(t, root)
	child := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "child.txt",
		DriveID: eng.driveID,
		ItemID:  "child-item",
	}, 2, []int64{1})
	require.Nil(t, child)
	rt.markRunning(root)
	rt.queuePendingReplan(dirtyBatch{})

	err = rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &ActionCompletion{
		Path:       "parent.txt",
		ItemID:     "parent-item",
		DriveID:    eng.driveID,
		ActionType: ActionDownload,
		Success:    true,
		ActionID:   1,
	})

	require.NoError(t, err)
	assert.True(t, rt.hasPendingReplan())
	assert.True(t, rt.canPrepareNow())
	assert.Empty(t, rt.currentOutbox(), "ready dependents from old graph should not be appended while replan is pending")
	assert.Empty(t, rt.queuedByID)
	assert.Equal(t, 1, rt.depGraph.InFlightCount(), "released child should remain uncompleted until replacement runtime is installed")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventWaitingForRunningActions &&
			!event.At.IsZero() &&
			event.Running == 1 &&
			event.IdleWorkers == 1
	}, 1, "pending replan should record that old running work is still draining")
	recorder.requireEventCount(t, func(event engineDebugEvent) bool {
		return event.Type == engineDebugEventOldOutboxRetired &&
			event.Note == "released_ready_frontier" &&
			!event.At.IsZero() &&
			event.Count == 1 &&
			event.Outbox == 0 &&
			event.Running == 0 &&
			event.IdleWorkers == 2
	}, 1, "ready dependents released by old running work should be timestamped as retired frontier")
}

// Validates: R-2.8.6
func TestWatchRuntime_InitWatchInfraUsesUnbufferedDispatch(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	eng.transferWorkers = 1
	rt := newWatchRuntime(eng.Engine)
	ctx, cancel := context.WithCancel(t.Context())

	pipe, err := rt.initWatchInfra(ctx, SyncBidirectional, WatchOptions{})
	require.NoError(t, err)
	require.NotNil(t, pipe)
	defer func() {
		cancel()
		pipe.cleanup()
	}()

	assert.Equal(t, watchDispatchBuf, cap(rt.dispatchCh))
	assert.Equal(t, 0, cap(rt.dispatchCh), "watch dispatch must stay unbuffered so queued work remains engine-owned")
	assert.Equal(t, watchCompletionBuf, cap(pipe.completions))
}

// Validates: R-2.4.8
func TestWatchRuntime_HandleProtectedRootEventOwnsLocalAliasRename(t *testing.T) {
	t.Parallel()

	var moved struct {
		itemID string
		name   string
	}
	mock := &engineMockClient{
		moveItemFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
			moved.itemID = itemID
			moved.name = newName
			assert.Empty(t, newParentID)
			return &graph.Item{ID: itemID, Name: newName}, nil
		},
	}
	eng, syncRoot := newTestEngine(t, mock)
	setupWatchEngine(t, eng)
	eng.shortcutNamespaceID = shortcutNamespaceTestID

	aliasRoot := filepath.Join(syncRoot, "Shared", "Docs")
	renamedRoot := filepath.Join(syncRoot, "Shared", "Renamed")
	require.NoError(t, os.MkdirAll(aliasRoot, 0o700))
	identity, err := eng.syncTree.IdentityNoFollow(filepath.Join("Shared", "Docs"))
	require.NoError(t, err)
	require.NoError(t, eng.baseline.replaceShortcutRoots(t.Context(), []ShortcutRootRecord{{
		NamespaceID:       shortcutNamespaceTestID,
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("drive-1"),
		RemoteItemID:      "target-1",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
		LocalRootIdentity: &identity,
	}}))
	require.NoError(t, os.Rename(aliasRoot, renamedRoot))

	var published ShortcutChildWorkSnapshot
	eng.shortcutChildWorkSink = func(_ context.Context, publication ShortcutChildWorkSnapshot) error {
		published = publication
		return nil
	}
	rt := testWatchRuntime(t, eng)

	done, err := rt.handleWatchProtectedRootEventSignal(t.Context(), &ProtectedRootEvent{}, true)

	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, "binding-1", moved.itemID)
	assert.Equal(t, "Renamed", moved.name)
	require.Len(t, published.RunCommands, 1)
	assert.Equal(t, filepath.Join(syncRoot, "Shared", "Renamed"), published.RunCommands[0].Engine.LocalRoot)
	roots, err := eng.baseline.listShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs"}, roots[0].ProtectedPaths)
}
