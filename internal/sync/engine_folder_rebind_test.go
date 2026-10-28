package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// A folder deleted and re-created online at the same path is a new item. The
// baseline must follow it, or every upload into the folder targets the dead
// item ID forever.
//
// Validates: R-2.10.49
func TestRunOnce_FolderRecreatedOnlineRebindsAndUploadsIntoTheNewItem(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("photos-1", "root", "Photos")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.Equal(t, 1, runScenarioPass(t, eng).FolderCreates)

	remote.deleteRemotely("photos-1")
	remote.addFolder("photos-2", "root", "Photos")
	runScenarioPass(t, eng)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	entry, found := bl.GetByPath("Photos")
	require.True(t, found, "the folder is still synced")
	assert.Equal(t, "photos-2", entry.ItemID, "the baseline follows the folder's new item")

	writeTestFile(t, syncRoot, "Photos/new.txt", "new local file")
	report := runScenarioPass(t, eng)
	require.Equal(t, 1, report.Uploads, "%+v", report)

	uploaded, ok := remote.itemAt("Photos/new.txt")
	require.True(t, ok, "the upload landed in the live folder")
	assert.Equal(t, "photos-2", uploaded.ParentID)
}

// Validates: R-2.10.49
func TestRunOnce_FullRescanWithEveryFolderReboundPlansNoTransfers(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("docs-1", "root", "Docs")
	remote.addFolder("deep-1", "docs-1", "Deep")
	remote.addFile(t, "file-1", "deep-1", "a.txt", "content")
	eng, _ := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// Every item comes back with a new ID at the same path (a restore from a
	// backup, or a migrated library), and the old ones are gone.
	remote.removeUnreported("docs-1")
	remote.addFolder("docs-2", "root", "Docs")
	remote.addFolder("deep-2", "docs-2", "Deep")
	remote.addFile(t, "file-2", "deep-2", "a.txt", "content")

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{FullReconcile: true})
	require.NoError(t, err)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates,
		"same content at the same paths moves nothing: %+v", report)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	for p, want := range map[string]string{"Docs": "docs-2", "Docs/Deep": "deep-2", "Docs/Deep/a.txt": "file-2"} {
		entry, found := bl.GetByPath(p)
		require.True(t, found, p)
		assert.Equal(t, want, entry.ItemID, p)
	}
}

// Validates: R-2.10.49
func TestCommitMutation_FolderReboundAtSamePathRebindsItsChildren(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	_, err := mgr.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, parent_id, item_type) VALUES
			('photos-1', 'Photos', 'root', 'folder'),
			('pic-1', 'Photos/a.jpg', 'photos-1', 'file')`)
	require.NoError(t, err)

	require.NoError(t, mgr.CommitMutation(ctx, &BaselineMutation{
		Action: ActionBaselineUpdate, Success: true, Path: "Photos", DriveID: driveID,
		ItemID: "photos-2", ParentID: "root", ItemType: ItemTypeFolder,
	}))

	var parentID string
	require.NoError(t, mgr.rawDB().QueryRowContext(ctx,
		`SELECT parent_id FROM baseline WHERE item_id = 'pic-1'`).Scan(&parentID))
	assert.Equal(t, "photos-2", parentID)
}

// Validates: R-2.10.49
func TestQueryReconciliationState_FolderWithNewItemIDAtSamePathIsBaselineUpdate(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	_, err := mgr.rawDB().ExecContext(ctx,
		`INSERT INTO baseline (item_id, path, parent_id, item_type) VALUES ('id1', 'X', 'root', 'folder')`)
	require.NoError(t, err)
	require.NoError(t, mgr.ReplaceLocalState(ctx, []localStateRow{{Path: "X", ItemType: ItemTypeFolder}}))
	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{
		{DriveID: driveID, ItemID: "id2", Path: "X", ItemType: ItemTypeFolder},
	}, "", driveID))

	rows, err := mgr.QueryReconciliationState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "baseline_update", rows[0].ReconciliationKind)
}

// Validates: R-2.10.49
func TestCommitMutation_FolderRebindUpdatesCachedChildren(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	_, err := mgr.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, parent_id, item_type) VALUES
			('photos-1', 'Photos', 'root', 'folder'),
			('pic-1', 'Photos/a.jpg', 'photos-1', 'file')`)
	require.NoError(t, err)
	bl, err := mgr.Load(ctx)
	require.NoError(t, err)

	require.NoError(t, mgr.CommitMutation(ctx, &BaselineMutation{
		Action: ActionBaselineUpdate, Success: true, Path: "Photos", DriveID: driveID,
		ItemID: "photos-2", ParentID: "root", ItemType: ItemTypeFolder,
	}))

	child, found := bl.GetByPath("Photos/a.jpg")
	require.True(t, found)
	assert.Equal(t, "photos-2", child.ParentID, "the cached child follows its folder's new ID")
	folder, found := bl.GetByID("photos-2")
	require.True(t, found)
	assert.Equal(t, "Photos", folder.Path)
	_, stale := bl.GetByID("photos-1")
	assert.False(t, stale, "the dead ID is gone from the cache")
}
