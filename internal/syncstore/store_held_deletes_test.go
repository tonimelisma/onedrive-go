package syncstore

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.3.6
func TestSyncStore_UpsertHeldDeletesRequiresItemID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	err := store.UpsertHeldDeletes(t.Context(), []HeldDeleteRecord{{
		DriveID:       driveid.New("drive1"),
		ActionType:    synctypes.ActionRemoteDelete,
		Path:          "delete-me.txt",
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing item ID")
}

// Validates: R-2.3.6
func TestSyncStore_HeldDeleteConsumeRequiresMatchingItemID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New("drive1")
	require.NoError(t, store.UpsertHeldDeletes(ctx, []HeldDeleteRecord{
		{
			DriveID:       driveID,
			ActionType:    synctypes.ActionRemoteDelete,
			Path:          "reuse.txt",
			ItemID:        "old-item",
			State:         synctypes.HeldDeleteStateHeld,
			HeldAt:        1,
			LastPlannedAt: 1,
		},
		{
			DriveID:       driveID,
			ActionType:    synctypes.ActionRemoteDelete,
			Path:          "reuse.txt",
			ItemID:        "new-item",
			State:         synctypes.HeldDeleteStateHeld,
			HeldAt:        2,
			LastPlannedAt: 2,
		},
	}))
	require.NoError(t, store.ApproveHeldDeletes(ctx))

	require.NoError(t, store.ConsumeHeldDelete(ctx, driveID, synctypes.ActionRemoteDelete, "reuse.txt", "other-item"))
	approved, err := store.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 2)

	require.NoError(t, store.ConsumeHeldDelete(ctx, driveID, synctypes.ActionRemoteDelete, "reuse.txt", "old-item"))
	approved, err = store.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Equal(t, "new-item", approved[0].ItemID)
}

// Validates: R-2.3.6
func TestSyncStore_DeleteHeldDeleteRequiresMatchingItemID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New("drive1")
	require.NoError(t, store.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveID,
		ActionType:    synctypes.ActionRemoteDelete,
		Path:          "delete-me.txt",
		ItemID:        "item-1",
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))

	require.NoError(t, store.DeleteHeldDelete(ctx, driveID, synctypes.ActionRemoteDelete, "delete-me.txt", "other-item"))
	held, err := store.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, err)
	require.Len(t, held, 1)

	require.NoError(t, store.DeleteHeldDelete(ctx, driveID, synctypes.ActionRemoteDelete, "delete-me.txt", "item-1"))
	held, err = store.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, err)
	assert.Empty(t, held)
}

// Validates: R-2.3.12
func TestSyncStore_ApproveHeldDeletesConcurrentCallsAreIdempotent(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveid.New("drive1"),
		ActionType:    synctypes.ActionRemoteDelete,
		Path:          "delete-me.txt",
		ItemID:        "item-1",
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.ApproveHeldDeletes(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	held, err := store.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, err)
	assert.Empty(t, held)

	approved, err := store.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Positive(t, approved[0].ApprovedAt)
}
