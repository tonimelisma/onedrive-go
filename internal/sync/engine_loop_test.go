package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
	seedBaseline(t, eng.baseline, ctx, []ExecutionResult{{
		Action:          ActionDownload,
		Success:         true,
		Path:            path,
		DriveID:         driveID,
		ItemID:          itemID,
		ItemType:        synctypes.ItemTypeFile,
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
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, eng.baseline.ApproveHeldDeletes(ctx))

	return testWorkDispatchState(t, eng, ctx)
}

func dispatchBusyWatchAction(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	path string,
	actionID int64,
) *TrackedAction {
	t.Helper()

	rt := testWatchRuntime(t, eng)
	busy := rt.depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    path,
		DriveID: driveid.New(engineTestDriveID),
	}, actionID, nil)
	require.NotNil(t, busy)

	dispatchCh := make(chan *TrackedAction, 1)
	rt.dispatchCh = dispatchCh
	rt.replaceOutbox([]*TrackedAction{busy})

	shuttingDown, err := rt.runWatchStep(ctx, &watchPipeline{})
	require.NoError(t, err)
	assert.False(t, shuttingDown)

	dispatched := <-dispatchCh
	assert.Same(t, busy, dispatched)
	assert.Empty(t, rt.currentOutbox())

	return busy
}

func assertQueuedHeldDeleteAction(t *testing.T, action *TrackedAction, path string) {
	t.Helper()
	require.NotNil(t, action)
	assert.Equal(t, ActionRemoteDelete, action.Action.Type)
	assert.Equal(t, path, action.Action.Path)
}

// Validates: R-2.3.6, R-2.9.3
func TestRunWatchStep_UserIntentWakeStaysPendingUntilOutboxDrains(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	bl, safety := seedApprovedHeldDelete(t, eng, ctx, "delete-me.txt", "item-delete")
	userIntentC := make(chan struct{}, 1)
	userIntentC <- struct{}{}
	p := &watchPipeline{
		bl:          bl,
		safety:      safety,
		mode:        SyncBidirectional,
		userIntentC: userIntentC,
	}

	busy := testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "busy.txt",
		DriveID: driveid.New(engineTestDriveID),
	}, 99, nil)
	require.NotNil(t, busy)
	testWatchRuntime(t, eng).dispatchCh = nil
	testWatchRuntime(t, eng).replaceOutbox([]*TrackedAction{busy})

	shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	outbox := testWatchRuntime(t, eng).currentOutbox()
	require.Len(t, outbox, 1, "wake should stay pending while existing outbox work is still draining")
	assert.Equal(t, "busy.txt", outbox[0].Action.Path)

	dispatchCh := make(chan *TrackedAction, 1)
	testWatchRuntime(t, eng).dispatchCh = dispatchCh

	p.userIntentC = nil
	shuttingDown, err = testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)

	dispatched := <-dispatchCh
	assert.Equal(t, "busy.txt", dispatched.Action.Path)
	outbox = testWatchRuntime(t, eng).currentOutbox()
	assert.Empty(t, outbox, "pending user intent should stay queued until the in-flight action completes")
	assert.True(t, testWatchRuntime(t, eng).userIntentPending)

	ready, ok := testWatchRuntime(t, eng).depGraph.Complete(busy.ID)
	require.True(t, ok)
	assert.Empty(t, ready)
	testWatchRuntime(t, eng).settleWatchAdmission(ctx, p)
	outbox = testWatchRuntime(t, eng).currentOutbox()

	require.Len(t, outbox, 1, "pending user intent should dispatch as soon as the in-flight action completes")
	assert.Equal(t, ActionRemoteDelete, outbox[0].Action.Type)
	assert.Equal(t, "delete-me.txt", outbox[0].Action.Path)
	assert.False(t, testWatchRuntime(t, eng).userIntentPending)
}

// Validates: R-2.3.6, R-2.9.3
func TestRunWatchStep_RecheckWakeStaysPendingUntilOutboxDrains(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)
	seedBaseline(t, eng.baseline, ctx, []ExecutionResult{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "recheck-delete.txt",
		DriveID:         driveID,
		ItemID:          "recheck-item",
		ItemType:        synctypes.ItemTypeFile,
		RemoteHash:      "hash-recheck",
		LocalHash:       "hash-recheck",
		LocalSize:       10,
		LocalSizeKnown:  true,
		RemoteSize:      10,
		RemoteSizeKnown: true,
	}}, "")
	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveID,
		ItemID:        "recheck-item",
		Path:          "recheck-delete.txt",
		ActionType:    ActionRemoteDelete,
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))

	currentVersion, err := eng.baseline.DataVersion(ctx)
	require.NoError(t, err)
	testWatchRuntime(t, eng).lastDataVersion = currentVersion

	dbPath := syncStorePathForTest(t, eng)
	externalStore, err := syncstore.NewSyncStore(ctx, filepath.Clean(dbPath), testLogger(t))
	require.NoError(t, err)
	require.NoError(t, externalStore.ApproveHeldDeletes(ctx))
	require.NoError(t, externalStore.Close(ctx))

	bl, safety := testWorkDispatchState(t, eng, ctx)
	recheckC := make(chan time.Time, 1)
	recheckC <- time.Now()
	p := &watchPipeline{
		bl:       bl,
		safety:   safety,
		mode:     SyncBidirectional,
		recheckC: recheckC,
	}

	busy := testWatchRuntime(t, eng).depGraph.Add(&Action{
		Type:    ActionUpload,
		Path:    "busy-recheck.txt",
		DriveID: driveID,
	}, 100, nil)
	require.NotNil(t, busy)
	testWatchRuntime(t, eng).dispatchCh = nil
	testWatchRuntime(t, eng).replaceOutbox([]*TrackedAction{busy})

	shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	outbox := testWatchRuntime(t, eng).currentOutbox()
	require.Len(t, outbox, 1, "recheck-triggered user intent should stay pending while outbox work remains")
	assert.Equal(t, "busy-recheck.txt", outbox[0].Action.Path)

	dispatchCh := make(chan *TrackedAction, 1)
	testWatchRuntime(t, eng).dispatchCh = dispatchCh

	p.recheckC = nil
	shuttingDown, err = testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)

	dispatched := <-dispatchCh
	assert.Equal(t, "busy-recheck.txt", dispatched.Action.Path)
	outbox = testWatchRuntime(t, eng).currentOutbox()
	assert.Empty(t, outbox, "pending recheck-triggered intent should stay queued while work remains in flight")
	assert.True(t, testWatchRuntime(t, eng).userIntentPending)

	ready, ok := testWatchRuntime(t, eng).depGraph.Complete(busy.ID)
	require.True(t, ok)
	assert.Empty(t, ready)
	testWatchRuntime(t, eng).settleWatchAdmission(ctx, p)
	outbox = testWatchRuntime(t, eng).currentOutbox()

	require.Len(t, outbox, 1)
	assert.Equal(t, ActionRemoteDelete, outbox[0].Action.Type)
	assert.Equal(t, "recheck-delete.txt", outbox[0].Action.Path)
	assert.False(t, testWatchRuntime(t, eng).userIntentPending)
}

// Validates: R-2.3.6, R-2.9.3
func TestRunWatchStep_UserIntentWakeStaysPendingUntilInFlightCompletes(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	bl, safety := seedApprovedHeldDelete(t, eng, ctx, "delete-after-inflight.txt", "item-inflight")
	busy := dispatchBusyWatchAction(t, eng, ctx, "busy-inflight.txt", 101)
	userIntentC := make(chan struct{}, 1)
	userIntentC <- struct{}{}
	p := &watchPipeline{
		bl:          bl,
		safety:      safety,
		mode:        SyncBidirectional,
		userIntentC: userIntentC,
	}

	shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	assert.Empty(t, testWatchRuntime(t, eng).currentOutbox(), "pending intent should not admit while a dispatched action is still in flight")
	assert.True(t, testWatchRuntime(t, eng).userIntentPending)

	ready, ok := testWatchRuntime(t, eng).depGraph.Complete(busy.ID)
	require.True(t, ok)
	assert.Empty(t, ready)
	testWatchRuntime(t, eng).settleWatchAdmission(ctx, p)

	outbox := testWatchRuntime(t, eng).currentOutbox()
	require.Len(t, outbox, 1)
	assertQueuedHeldDeleteAction(t, outbox[0], "delete-after-inflight.txt")
	assert.False(t, testWatchRuntime(t, eng).userIntentPending)
}

// Validates: R-2.3.6, R-2.9.3
func TestRunWatchStep_RepeatedUserIntentWakesCoalesceWhileInFlight(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	bl, safety := seedApprovedHeldDelete(t, eng, ctx, "delete-coalesced.txt", "item-coalesced")
	busy := dispatchBusyWatchAction(t, eng, ctx, "busy-coalesced.txt", 102)
	userIntentC := make(chan struct{}, 2)
	userIntentC <- struct{}{}
	userIntentC <- struct{}{}
	p := &watchPipeline{
		bl:          bl,
		safety:      safety,
		mode:        SyncBidirectional,
		userIntentC: userIntentC,
	}

	for range 2 {
		shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
		require.NoError(t, err)
		assert.False(t, shuttingDown)
		assert.Empty(t, testWatchRuntime(t, eng).currentOutbox())
		assert.True(t, testWatchRuntime(t, eng).userIntentPending)
	}

	ready, ok := testWatchRuntime(t, eng).depGraph.Complete(busy.ID)
	require.True(t, ok)
	assert.Empty(t, ready)
	testWatchRuntime(t, eng).settleWatchAdmission(ctx, p)

	outbox := testWatchRuntime(t, eng).currentOutbox()
	require.Len(t, outbox, 1, "multiple wakes should still collapse into one durable-intent dispatch")
	assertQueuedHeldDeleteAction(t, outbox[0], "delete-coalesced.txt")
	assert.False(t, testWatchRuntime(t, eng).userIntentPending)
}

// Validates: R-2.3.6, R-2.9.3
func TestRunWatchStep_RecheckAndUserIntentWakeCoalesceWhenBothReady(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	bl, safety := seedApprovedHeldDelete(t, eng, ctx, "delete-dual-wake.txt", "item-dual-wake")
	busy := dispatchBusyWatchAction(t, eng, ctx, "busy-dual-wake.txt", 103)
	recheckC := make(chan time.Time, 1)
	recheckC <- time.Now()
	userIntentC := make(chan struct{}, 1)
	userIntentC <- struct{}{}
	p := &watchPipeline{
		bl:          bl,
		safety:      safety,
		mode:        SyncBidirectional,
		recheckC:    recheckC,
		userIntentC: userIntentC,
	}

	for range 2 {
		shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p)
		require.NoError(t, err)
		assert.False(t, shuttingDown)
		assert.Empty(t, testWatchRuntime(t, eng).currentOutbox())
		assert.True(t, testWatchRuntime(t, eng).userIntentPending)
	}

	ready, ok := testWatchRuntime(t, eng).depGraph.Complete(busy.ID)
	require.True(t, ok)
	assert.Empty(t, ready)
	testWatchRuntime(t, eng).settleWatchAdmission(ctx, p)

	outbox := testWatchRuntime(t, eng).currentOutbox()
	require.Len(t, outbox, 1, "recheck and live wake should still produce one durable-intent dispatch")
	assertQueuedHeldDeleteAction(t, outbox[0], "delete-dual-wake.txt")
	assert.False(t, testWatchRuntime(t, eng).userIntentPending)
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
