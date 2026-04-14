package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.3.6, R-2.9.3
func TestHandleWatchMutationRequest_ApproveHeldDeletesReleasesDeleteCounter(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	seedBaseline(t, eng.baseline, ctx, []ActionOutcome{{
		Action:          ActionDownload,
		Success:         true,
		Path:            "delete-me.txt",
		DriveID:         driveID,
		ItemID:          "item-delete",
		ItemType:        ItemTypeFile,
		RemoteHash:      "hash-delete",
		LocalHash:       "hash-delete",
		LocalSize:       10,
		LocalSizeKnown:  true,
		RemoteSize:      10,
		RemoteSizeKnown: true,
	}}, "")
	require.NoError(t, eng.baseline.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveID,
		ItemID:        "item-delete",
		Path:          "delete-me.txt",
		ActionType:    ActionRemoteDelete,
		State:         HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))

	rt := testWatchRuntime(t, eng)
	rt.deleteCounter = NewDeleteCounter(1, time.Minute, time.Now)
	assert.True(t, rt.deleteCounter.Add(2))
	assert.True(t, rt.deleteCounter.IsHeld())

	request := &WatchMutationRequest{
		Kind:     WatchMutationApproveHeldDeletes,
		Response: make(chan WatchMutationResponse, 1),
	}

	transition := rt.handleWatchMutationRequest(ctx, request)
	response := <-request.Response
	require.NoError(t, response.Err)
	assert.True(t, transition.markUserIntentPending)
	assert.False(t, rt.deleteCounter.IsHeld())

	approved, err := eng.baseline.ListHeldDeletesByState(ctx, HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Equal(t, "delete-me.txt", approved[0].Path)
}

// Validates: R-2.3.12, R-2.9.3
func TestHandleWatchMutationRequest_ConflictResolutionQueuesDurableIntent(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, nil)
	newTestWatchState(t, eng)
	ctx := t.Context()

	_, err := eng.baseline.DB().ExecContext(ctx, `
		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES
			('conflict-1', ?, 'item-1', 'conflict.txt', 'edit_edit', 1, 'unresolved')`,
		engineTestDriveID,
	)
	require.NoError(t, err)

	request := &WatchMutationRequest{
		Kind:       WatchMutationRequestConflictResolution,
		ConflictID: "conflict-1",
		Resolution: ResolutionKeepLocal,
		Response:   make(chan WatchMutationResponse, 1),
	}

	transition := testWatchRuntime(t, eng).handleWatchMutationRequest(ctx, request)
	response := <-request.Response
	require.NoError(t, response.Err)
	assert.Equal(t, ConflictRequestQueued, response.ConflictRequestResult.Status)
	assert.True(t, transition.markUserIntentPending)

	queued, err := eng.baseline.GetConflictRequest(ctx, "conflict-1")
	require.NoError(t, err)
	assert.Equal(t, ConflictStateQueued, queued.State)
	assert.Equal(t, ResolutionKeepLocal, queued.RequestedResolution)
}
