package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func seedApprovedHeldDelete(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	path string,
	itemID string,
) (*Baseline, *SafetyConfig) {
	t.Helper()

	driveID := driveid.New(engineTestDriveID)
	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            path,
		DriveID:         driveID,
		ItemID:          itemID,
		ItemType:        ItemTypeFile,
		RemoteHash:      "hash-" + itemID,
		LocalHash:       "hash-" + itemID,
		LocalSize:       10,
		LocalSizeKnown:  true,
		RemoteSize:      10,
		RemoteSizeKnown: true,
	}}, "")

	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveID,
		ItemID:        itemID,
		Path:          path,
		ActionType:    ActionRemoteDelete,
		State:         HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, eng.baseline.ApproveHeldDeletes(ctx))

	return testWorkDispatchState(t, eng, ctx)
}

func assertQueuedHeldDeleteAction(t *testing.T, action *TrackedAction, path string) {
	t.Helper()
	require.NotNil(t, action)
	assert.Equal(t, ActionRemoteDelete, action.Action.Type)
	assert.Equal(t, path, action.Action.Path)
}

// Validates: R-2.3.6, R-2.8.3
func TestRunWatchStep_PendingUserIntentStaysQueuedUntilOutboxDrains(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	bl, safety := seedApprovedHeldDelete(t, eng, ctx, "delete-pending.txt", "item-pending")
	busy := testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "busy-pending.txt",
		DriveID: driveid.New(engineTestDriveID),
	}, 103, nil)
	require.NotNil(t, busy)

	dispatchCh := make(chan *TrackedAction, 1)
	testWatchRuntime(t, eng).dispatchCh = dispatchCh
	testWatchRuntime(t, eng).replaceOutbox([]*TrackedAction{busy})
	testWatchRuntime(t, eng).queueUserIntentDispatch()

	p := &watchPipeline{
		bl:     bl,
		safety: safety,
		mode:   SyncBidirectional,
	}

	shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	assert.True(t, testWatchRuntime(t, eng).userIntentPending, "user intent must remain pending while ordinary outbox work is still in flight")
	assert.Empty(t, testWatchRuntime(t, eng).currentOutbox(), "the busy action should dispatch, but no durable-intent action should be admitted yet")
	assert.Equal(t, 1, testWatchRuntime(t, eng).depGraph.InFlightCount(), "the in-flight busy action should still seal admission")

	dispatched := <-dispatchCh
	assert.Same(t, busy, dispatched)
}

// Validates: R-2.3.6, R-2.9.3
func TestRunWatchStep_WorkerResultCompletionAdmitsPendingUserIntent(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	bl, safety := seedApprovedHeldDelete(t, eng, ctx, "delete-on-result.txt", "item-result")
	busy := testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "busy-result.txt",
		DriveID: driveid.New(engineTestDriveID),
	}, 104, nil)
	require.NotNil(t, busy)

	results := make(chan WorkerResult, 1)
	results <- WorkerResult{
		ActionID:   busy.ID,
		ActionType: busy.Action.Type,
		Path:       busy.Action.Path,
		DriveID:    busy.Action.DriveID,
		Success:    true,
	}
	testWatchRuntime(t, eng).queueUserIntentDispatch()
	p := &watchPipeline{
		bl:      bl,
		safety:  safety,
		mode:    SyncBidirectional,
		results: results,
	}

	shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	assert.False(t, testWatchRuntime(t, eng).userIntentPending)

	outbox := testWatchRuntime(t, eng).currentOutbox()
	require.Len(t, outbox, 1, "worker completion should trigger admission when it frees the final capacity edge")
	assertQueuedHeldDeleteAction(t, outbox[0], "delete-on-result.txt")
	assert.Equal(t, 1, testWatchRuntime(t, eng).depGraph.InFlightCount(), "the completed action should be replaced by the newly admitted durable-intent action")
}

// Validates: R-2.8.3, R-6.10.10
func TestRunWatchStep_DrainDoesNotAdmitPendingUserIntent(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	bl, safety := seedApprovedHeldDelete(t, eng, t.Context(), "delete-drain.txt", "item-drain")
	testWatchRuntime(t, eng).queueUserIntentDispatch()
	p := &watchPipeline{
		bl:     bl,
		safety: safety,
		mode:   SyncBidirectional,
	}

	shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	assert.True(t, testWatchRuntime(t, eng).isDraining())
	assert.Empty(t, testWatchRuntime(t, eng).currentOutbox(), "shutdown must seal admission even when user intent is already pending")
	assert.True(t, testWatchRuntime(t, eng).userIntentPending)
}
