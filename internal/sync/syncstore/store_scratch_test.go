package syncstore

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "remote-1",
			Path:     "remote-one.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "remote-hash-1",
			Size:     11,
			Mtime:    101,
			ETag:     "etag-1",
		},
		{
			DriveID:  driveID,
			ItemID:   "remote-2",
			Path:     "folder/remote-two.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "remote-hash-2",
			Size:     22,
			Mtime:    202,
			ETag:     "etag-2",
		},
	}, "cursor-seeded", driveID))
	refreshAt := time.Unix(1700000000, 0).UTC()
	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, refreshAt, syncdomain.RemoteObservationModeDelta))
}

// seedScratchPlanningBaseline writes the baseline rows the scratch store must
// copy, straight into the database.
func seedScratchPlanningBaseline(t *testing.T, store *SyncStore, driveID driveid.ID) {
	t.Helper()

	for _, entry := range []*syncdomain.BaselineEntry{
		{
			Path:            "baseline.txt",
			DriveID:         driveID,
			ItemID:          "baseline-1",
			ParentID:        "root",
			ItemType:        syncdomain.ItemTypeFile,
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
			ItemType: syncdomain.ItemTypeFolder,
		},
	} {
		require.NoError(t, upsertBaselineRowTx(t.Context(), store.db, entry))
	}
}

// Validates: R-2.1.5
func TestCreateScratchPlanningStore_SeedsCommittedStateAndCleansUp(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)

	seedScratchPlanningSource(t, store, driveID)
	seedScratchPlanningBaseline(t, store, driveID)

	scratch, cleanup, err := store.CreateScratchPlanningStore(ctx)
	require.NoError(t, err)
	require.NotNil(t, scratch)
	require.NotNil(t, cleanup)

	scratchPath := scratchMainDBPath(t, scratch.db)
	require.NotEmpty(t, scratchPath)
	scratchDir := filepath.Dir(scratchPath)
	refreshAt := time.Unix(1700000000, 0).UTC()

	state, err := scratch.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, driveID, state.ContentDriveID)
	assert.Equal(t, "cursor-seeded", state.Cursor)
	assert.Equal(t, refreshAt.Add(syncdomain.FullRemoteRefreshInterval).UnixNano(), state.NextFullRemoteRefreshAt)

	remoteRows, err := listScratchRemoteStateRows(ctx, scratch.db)
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
	_, statErr := os.Stat(scratchDir)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// Validates: R-2.1.5, R-6.5.2
//
// A dry run plans from what is committed: the scratch store copies the
// baseline table, not the cache, so a cache that lags or leads the
// database cannot reach the plan.
func TestCreateScratchPlanningStore_CopiesTheDatabaseNotAStaleCache(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	seedScratchPlanningSource(t, store, driveID)
	_, err := store.Load(ctx)
	require.NoError(t, err)

	// One row reaches the database behind the cache, one the cache alone.
	require.NoError(t, upsertBaselineRowTx(ctx, store.db, &syncdomain.BaselineEntry{
		Path: "committed.txt", DriveID: driveID, ItemID: "item-committed", ParentID: "root", ItemType: syncdomain.ItemTypeFile,
	}))
	store.cachedBaseline().put(&syncdomain.BaselineEntry{
		Path: "cache-only.txt", DriveID: driveID, ItemID: "item-cache-only", ParentID: "root", ItemType: syncdomain.ItemTypeFile,
	})

	scratch, cleanup, err := store.CreateScratchPlanningStore(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, cleanup(ctx)) }()

	planned, err := scratch.Load(ctx)
	require.NoError(t, err)
	_, ok := planned.GetByPath("committed.txt")
	assert.True(t, ok, "the committed row is planned from")
	_, ok = planned.GetByPath("cache-only.txt")
	assert.False(t, ok, "a row only the cache holds is not")
}
