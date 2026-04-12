package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.16
func TestUpsertShortcut_Insert(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	sc := Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "Shared/TeamDocs",
		DriveType:    "personal",
		Observation:  ObservationDelta,
		DiscoveredAt: 1000,
	}

	err := mgr.UpsertShortcut(ctx, &sc)
	require.NoError(t, err)

	got, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, got)

	assert.Equal(t, "sc-1", got.ItemID)
	assert.Equal(t, "remote-drive-1", got.RemoteDrive)
	assert.Equal(t, "remote-item-1", got.RemoteItem)
	assert.Equal(t, "Shared/TeamDocs", got.LocalPath)
	assert.Equal(t, "personal", got.DriveType)
	assert.Equal(t, ObservationDelta, got.Observation)
	assert.Equal(t, int64(1000), got.DiscoveredAt)
}

// Validates: R-2.10.16
func TestUpsertShortcut_Update(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	sc := Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "Shared/OldName",
		DriveType:    "personal",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}
	require.NoError(t, mgr.UpsertShortcut(ctx, &sc))

	// Update with new path and observation.
	sc.LocalPath = "Shared/NewName"
	sc.Observation = ObservationDelta
	require.NoError(t, mgr.UpsertShortcut(ctx, &sc))

	got, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "Shared/NewName", got.LocalPath)
	assert.Equal(t, ObservationDelta, got.Observation)
}

// Validates: R-2.10.16
func TestUpsertShortcut_PreservesDiscoveredAt(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	sc := Shortcut{
		ItemID: "sc-1", RemoteDrive: "d1", RemoteItem: "i1",
		LocalPath: "path1", Observation: ObservationDelta, DiscoveredAt: 1000,
	}
	require.NoError(t, mgr.UpsertShortcut(ctx, &sc))

	// Upsert again with a new path — should preserve discovered_at.
	sc.LocalPath = "path2"
	sc.DiscoveredAt = 9999 // caller passes a new timestamp, but ON CONFLICT should ignore it
	require.NoError(t, mgr.UpsertShortcut(ctx, &sc))

	got, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "path2", got.LocalPath)
	assert.Equal(t, int64(1000), got.DiscoveredAt, "discovered_at should be preserved across upserts")
}

// Validates: R-2.10.16
func TestGetShortcut_NotFound(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	got, found, err := mgr.GetShortcut(ctx, "nonexistent")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, got)
}

// Validates: R-2.10.16
func TestListShortcuts_Empty(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)
	assert.Empty(t, shortcuts)
}

// Validates: R-2.10.16
func TestListShortcuts_Multiple(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	for _, sc := range []Shortcut{
		{ItemID: "sc-1", RemoteDrive: "d1", RemoteItem: "i1", LocalPath: "path1", Observation: ObservationDelta, DiscoveredAt: 1000},
		{ItemID: "sc-2", RemoteDrive: "d2", RemoteItem: "i2", LocalPath: "path2", Observation: ObservationEnumerate, DiscoveredAt: 2000},
	} {
		require.NoError(t, mgr.UpsertShortcut(ctx, &sc))
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)
	assert.Len(t, shortcuts, 2)
}

// Validates: R-2.10.16
func TestDeleteShortcut(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	sc := Shortcut{
		ItemID: "sc-1", RemoteDrive: "d1", RemoteItem: "i1",
		LocalPath: "path1", Observation: ObservationDelta, DiscoveredAt: 1000,
	}
	require.NoError(t, mgr.UpsertShortcut(ctx, &sc))

	err := mgr.DeleteShortcut(ctx, "sc-1")
	require.NoError(t, err)

	got, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, got)
}

// Validates: R-2.10.16
func TestDeleteShortcut_NotFound(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	// Deleting a nonexistent shortcut should not error.
	err := mgr.DeleteShortcut(ctx, "nonexistent")
	require.NoError(t, err)
}
