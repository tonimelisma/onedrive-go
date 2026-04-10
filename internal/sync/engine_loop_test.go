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
) (*synctypes.Baseline, *synctypes.SafetyConfig) {
	t.Helper()

	driveID := driveid.New(engineTestDriveID)
	seedBaseline(t, eng.baseline, ctx, []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
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

	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, []synctypes.HeldDeleteRecord{{
		DriveID:       driveID,
		ItemID:        itemID,
		Path:          path,
		ActionType:    synctypes.ActionRemoteDelete,
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, eng.baseline.ApproveHeldDeletes(ctx))

	return testWorkDispatchState(t, eng, ctx)
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
		mode:        synctypes.SyncBidirectional,
		userIntentC: userIntentC,
	}

	busy := &synctypes.TrackedAction{
		ID: 99,
		Action: synctypes.Action{
			Type:    synctypes.ActionUpload,
			Path:    "busy.txt",
			DriveID: driveid.New(engineTestDriveID),
		},
	}
	testWatchRuntime(t, eng).dispatchCh = nil

	outbox, shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p, []*synctypes.TrackedAction{busy})
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	require.Len(t, outbox, 1, "wake should stay pending while existing outbox work is still draining")
	assert.Equal(t, "busy.txt", outbox[0].Action.Path)

	dispatchCh := make(chan *synctypes.TrackedAction, 1)
	testWatchRuntime(t, eng).dispatchCh = dispatchCh

	p.userIntentC = nil
	outbox, shuttingDown, err = testWatchRuntime(t, eng).runWatchStep(ctx, p, outbox)
	require.NoError(t, err)
	assert.False(t, shuttingDown)

	dispatched := <-dispatchCh
	assert.Equal(t, "busy.txt", dispatched.Action.Path)

	require.Len(t, outbox, 1, "pending user intent should dispatch as soon as the outbox drains")
	assert.Equal(t, synctypes.ActionRemoteDelete, outbox[0].Action.Type)
	assert.Equal(t, "delete-me.txt", outbox[0].Action.Path)
}

// Validates: R-2.3.6, R-2.9.3
func TestRunWatchStep_RecheckWakeStaysPendingUntilOutboxDrains(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)
	seedBaseline(t, eng.baseline, ctx, []synctypes.Outcome{{
		Action:          synctypes.ActionDownload,
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
	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, []synctypes.HeldDeleteRecord{{
		DriveID:       driveID,
		ItemID:        "recheck-item",
		Path:          "recheck-delete.txt",
		ActionType:    synctypes.ActionRemoteDelete,
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
		mode:     synctypes.SyncBidirectional,
		recheckC: recheckC,
	}

	busy := &synctypes.TrackedAction{
		ID: 100,
		Action: synctypes.Action{
			Type:    synctypes.ActionUpload,
			Path:    "busy-recheck.txt",
			DriveID: driveID,
		},
	}
	testWatchRuntime(t, eng).dispatchCh = nil

	outbox, shuttingDown, err := testWatchRuntime(t, eng).runWatchStep(ctx, p, []*synctypes.TrackedAction{busy})
	require.NoError(t, err)
	assert.False(t, shuttingDown)
	require.Len(t, outbox, 1, "recheck-triggered user intent should stay pending while outbox work remains")
	assert.Equal(t, "busy-recheck.txt", outbox[0].Action.Path)

	dispatchCh := make(chan *synctypes.TrackedAction, 1)
	testWatchRuntime(t, eng).dispatchCh = dispatchCh

	p.recheckC = nil
	outbox, shuttingDown, err = testWatchRuntime(t, eng).runWatchStep(ctx, p, outbox)
	require.NoError(t, err)
	assert.False(t, shuttingDown)

	dispatched := <-dispatchCh
	assert.Equal(t, "busy-recheck.txt", dispatched.Action.Path)

	require.Len(t, outbox, 1)
	assert.Equal(t, synctypes.ActionRemoteDelete, outbox[0].Action.Type)
	assert.Equal(t, "recheck-delete.txt", outbox[0].Action.Path)
}
