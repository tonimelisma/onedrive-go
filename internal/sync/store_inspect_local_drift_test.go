package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Live incident, 2026-11-01: 300 local edits sat committed in local_state for
// an hour with their uploads never dispatched, and `status` said "Up to date"
// because it counted only remote drift.
//
// Validates: R-2.10.50
func TestReadDriveStatusSnapshot_LocalDriftUsesPlannerContentTest(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitMutation(t.Context(), &BaselineMutation{
		Action:   ActionFolderCreate,
		Success:  true,
		Path:     "Docs",
		DriveID:  driveID,
		ItemID:   "item-docs",
		ItemType: ItemTypeFolder,
	}))
	for _, file := range []struct{ path, id string }{
		{path: "same.txt", id: "item-same"},
		{path: "edited.txt", id: "item-edited"},
		{path: "gone.txt", id: "item-gone"},
	} {
		require.NoError(t, store.CommitMutation(t.Context(), &BaselineMutation{
			Action:          ActionDownload,
			Success:         true,
			Path:            file.path,
			DriveID:         driveID,
			ItemID:          file.id,
			ItemType:        ItemTypeFile,
			LocalHash:       "abc",
			RemoteHash:      "abc",
			LocalSize:       3,
			LocalSizeKnown:  true,
			RemoteSize:      3,
			RemoteSizeKnown: true,
			LocalMtime:      1700000000,
			RemoteMtime:     1700000000,
		}))
	}

	// What the local observer reports: the folder as it is, same.txt touched
	// but with the same content, edited.txt with new content, gone.txt
	// deleted, and new.txt not yet uploaded.
	require.NoError(t, store.ReplaceLocalState(t.Context(), []localStateRow{
		{Path: "Docs", ItemType: ItemTypeFolder, Mtime: 1777605693000000000},
		{Path: "same.txt", ItemType: ItemTypeFile, Hash: "abc", Size: 3, Mtime: 1700000999},
		{Path: "edited.txt", ItemType: ItemTypeFile, Hash: "def", Size: 4, Mtime: 1700000999},
		{Path: "new.txt", ItemType: ItemTypeFile, Hash: "ghi", Size: 5, Mtime: 1700000999},
	}))

	snapshot, err := ReadDriveStatusSnapshot(t.Context(), syncStorePathForStoreScopeTest(t, store), testLogger(t))
	require.NoError(t, err)
	// edited.txt, gone.txt and new.txt: the folder's mtime and same.txt's
	// mtime are not changes when the planner would not act on them.
	assert.Equal(t, 3, snapshot.LocalDriftItems)

	// Once everything is uploaded and local_state matches, nothing drifts.
	require.NoError(t, store.ReplaceLocalState(t.Context(), []localStateRow{
		{Path: "Docs", ItemType: ItemTypeFolder},
		{Path: "same.txt", ItemType: ItemTypeFile, Hash: "abc", Size: 3, Mtime: 1700000000},
		{Path: "edited.txt", ItemType: ItemTypeFile, Hash: "abc", Size: 3, Mtime: 1700000000},
		{Path: "gone.txt", ItemType: ItemTypeFile, Hash: "abc", Size: 3, Mtime: 1700000000},
	}))
	snapshot, err = ReadDriveStatusSnapshot(t.Context(), syncStorePathForStoreScopeTest(t, store), testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 0, snapshot.LocalDriftItems)
}
