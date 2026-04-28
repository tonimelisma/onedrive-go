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
	rt.localEvents = make(chan ChangeEvent)
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
	assert.Nil(t, rt.localEvents)
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

	var published ShortcutChildRunnerPublication
	eng.shortcutChildRunnerSink = func(_ context.Context, publication ShortcutChildRunnerPublication) error {
		published = publication
		return nil
	}
	rt := testWatchRuntime(t, eng)

	done, err := rt.handleWatchProtectedRootEventSignal(t.Context(), &ProtectedRootEvent{}, true)

	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, "binding-1", moved.itemID)
	assert.Equal(t, "Renamed", moved.name)
	require.Len(t, published.RunnerWork.Children, 1)
	assert.Equal(t, "Shared/Renamed", published.RunnerWork.Children[0].RelativeLocalPath)
	roots, err := eng.baseline.listShortcutRoots(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, []string{"Shared/Renamed", "Shared/Docs"}, roots[0].ProtectedPaths)
}
