package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func scratchMainDBPath(t *testing.T, db *sql.DB) string {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), `PRAGMA database_list`)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, rows.Close())
	}()

	for rows.Next() {
		var (
			seq  int
			name string
			file string
		)
		require.NoError(t, rows.Scan(&seq, &name, &file))
		if name == "main" {
			require.NotEmpty(t, file)
			return file
		}
	}

	require.NoError(t, rows.Err())
	require.FailNow(t, "main SQLite database entry not found")

	return ""
}

func seedScratchPlanningSource(t *testing.T, store *SyncStore, driveID driveid.ID) {
	t.Helper()

	ctx := t.Context()

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:         driveID,
			ItemID:          "remote-1",
			ParentID:        "root",
			Path:            "remote-one.txt",
			ItemType:        ItemTypeFile,
			Hash:            "remote-hash-1",
			Size:            11,
			Mtime:           101,
			ETag:            "etag-1",
			ContentIdentity: "cid-1",
		},
		{
			DriveID:         driveID,
			ItemID:          "remote-2",
			ParentID:        "root",
			Path:            "folder/remote-two.txt",
			ItemType:        ItemTypeFile,
			Hash:            "remote-hash-2",
			Size:            22,
			Mtime:           202,
			ETag:            "etag-2",
			ContentIdentity: "cid-2",
		},
	}, "cursor-seeded", driveID))
	require.NoError(t, store.MarkFullRemoteReconcile(ctx, driveID, time.Unix(1700000000, 0).UTC()))
	require.NoError(t, store.MarkFullLocalRefresh(ctx, driveID, time.Unix(1700003600, 0).UTC(), localRefreshModeWatchDegraded))
}

func scratchPlanningBaseline(driveID driveid.ID) *Baseline {
	return newBaselineForTest([]*BaselineEntry{
		{
			Path:            "baseline.txt",
			DriveID:         driveID,
			ItemID:          "baseline-1",
			ParentID:        "root",
			ItemType:        ItemTypeFile,
			LocalHash:       "local-hash",
			RemoteHash:      "remote-hash",
			LocalSize:       33,
			LocalSizeKnown:  true,
			RemoteSize:      44,
			RemoteSizeKnown: true,
			LocalMtime:      303,
			RemoteMtime:     404,
			ETag:            "etag-baseline",
		},
		{
			Path:     "folder",
			DriveID:  driveID,
			ItemID:   "baseline-folder",
			ParentID: "root",
			ItemType: ItemTypeFolder,
		},
	})
}

// Validates: R-2.1.5
func TestCreateScratchPlanningStore_SeedsCommittedStateAndCleansUp(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	seedScratchPlanningSource(t, store, driveID)

	scratch, cleanup, err := store.createScratchPlanningStore(ctx, scratchPlanningBaseline(driveID))
	require.NoError(t, err)
	require.NotNil(t, scratch)
	require.NotNil(t, cleanup)

	scratchPath := scratchMainDBPath(t, scratch.rawDB())

	state, err := scratch.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, driveID, state.ConfiguredDriveID)
	assert.Equal(t, "cursor-seeded", state.Cursor)
	assert.Equal(t, localRefreshModeWatchDegraded, state.LocalRefreshMode)
	assert.Equal(t, remoteRefreshModeDeltaHealthy, state.RemoteRefreshMode)

	remoteRows, err := listScratchRemoteStateRows(ctx, scratch.rawDB())
	require.NoError(t, err)
	require.Len(t, remoteRows, 2)
	assert.Equal(t, "folder/remote-two.txt", remoteRows[0].Path)
	assert.Equal(t, "remote-one.txt", remoteRows[1].Path)
	assert.Equal(t, "remote-2", remoteRows[0].ItemID)
	assert.Equal(t, "remote-1", remoteRows[1].ItemID)

	loadedBaseline, err := scratch.Load(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, loadedBaseline.Len())

	entry, ok := loadedBaseline.GetByPath("baseline.txt")
	require.True(t, ok)
	assert.Equal(t, "baseline-1", entry.ItemID)
	assert.Equal(t, "etag-baseline", entry.ETag)
	assert.Equal(t, int64(33), entry.LocalSize)
	assert.True(t, entry.LocalSizeKnown)
	assert.Equal(t, int64(44), entry.RemoteSize)
	assert.True(t, entry.RemoteSizeKnown)

	require.NoError(t, cleanup(ctx))

	_, statErr := os.Stat(filepath.Dir(scratchPath))
	require.Error(t, statErr)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// Validates: R-2.1.5
func TestCreateScratchPlanningStore_RequiresBaseline(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	scratch, cleanup, err := store.createScratchPlanningStore(t.Context(), nil)
	require.Error(t, err)
	assert.Nil(t, scratch)
	assert.Nil(t, cleanup)
	assert.Contains(t, err.Error(), "scratch planning store requires baseline")
}

// Validates: R-2.1.5
func TestCloneBaselineEntries_ReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	assert.Nil(t, cloneBaselineEntries(nil))

	original := newBaselineForTest([]*BaselineEntry{{
		Path:            "copied.txt",
		ItemID:          "item-1",
		ParentID:        "root",
		ItemType:        ItemTypeFile,
		LocalHash:       "local-hash",
		RemoteHash:      "remote-hash",
		LocalSize:       10,
		LocalSizeKnown:  true,
		RemoteSize:      12,
		RemoteSizeKnown: true,
		LocalMtime:      100,
		RemoteMtime:     200,
		ETag:            "etag-1",
	}})

	cloned := cloneBaselineEntries(original)
	require.Len(t, cloned, 1)

	cloned[0].Path = "changed.txt"
	cloned[0].ItemID = "changed-item"
	cloned[0].LocalHash = "changed-hash"

	entry, ok := original.GetByPath("copied.txt")
	require.True(t, ok)
	assert.Equal(t, "item-1", entry.ItemID)
	assert.Equal(t, "local-hash", entry.LocalHash)
	assert.Equal(t, "copied.txt", entry.Path)
}
