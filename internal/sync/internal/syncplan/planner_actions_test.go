package syncplan

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.1.1, R-2.1.3, R-2.1.4
func TestPlannerPlanCurrentState_BuildsActionsFromSQLiteReconciliation(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 'old', 1, 1, 1, 1, 'etag-old'),
			('item-folder', 'folder', 'folder', '', '', NULL, NULL, NULL, NULL, NULL)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:     "upload.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "new-local",
			Size:     2,
			Mtime:    2,
		},
		{
			Path:     "new-folder",
			ItemType: syncdomain.ItemTypeFolder,
		},
	}))

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-upload",
			Path:     "upload.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "old",
			Size:     1,
			Mtime:    1,
			ETag:     "etag-old",
		},
		{
			DriveID:  driveID,
			ItemID:   "item-folder",
			Path:     "folder",
			ItemType: syncdomain.ItemTypeFolder,
		},
	}, "", driveID))

	planner := NewPlanner(syncfixture.TestLogger(t))
	plan, err := planner.PlanSnapshot(
		syncstoretest.ReadPlanningSnapshot(t, store),
		Decide,
		PlannerMountContext{DriveID: driveID},
		syncdomain.SyncBidirectional,
	)
	require.NoError(t, err)

	require.Len(t, plan.Actions, 3)
	byPath := make(map[string]syncdomain.Action, len(plan.Actions))
	for _, action := range plan.Actions {
		byPath[action.Path] = action
	}

	assert.Equal(t, syncdomain.ActionRemoteDelete, byPath["folder"].Type, "a locally deleted folder is deleted remotely")
	assert.Equal(t, syncdomain.ActionFolderCreate, byPath["new-folder"].Type)
	assert.Equal(t, syncdomain.CreateRemote, byPath["new-folder"].CreateSide)
	assert.Equal(t, syncdomain.ActionUpload, byPath["upload.txt"].Type)
}

// Validates: R-2.1.3, R-2.1.4
func TestPlannerPlanCurrentState_LocalFolderMoveByIdentityPlansSingleRemoteMove(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES
			('item-folder', 'Projects', 'folder', '', '', NULL, NULL, NULL, NULL, 11, 22, 1),
			('item-file', 'Projects/a.txt', 'file', 'hash-a', 'hash-a', 5, 5, 1, 1, 33, 44, 1)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:             "Renamed Projects",
			ItemType:         syncdomain.ItemTypeFolder,
			LocalDevice:      11,
			LocalInode:       22,
			LocalHasIdentity: true,
		},
		{
			Path:             "Renamed Projects/a.txt",
			ItemType:         syncdomain.ItemTypeFile,
			Hash:             "hash-a",
			Size:             5,
			Mtime:            1,
			LocalDevice:      33,
			LocalInode:       44,
			LocalHasIdentity: true,
		},
	}))

	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-folder', 'Projects', 'folder', '', NULL, NULL),
			('item-file', 'Projects/a.txt', 'file', 'hash-a', 5, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 1)
	assert.Equal(t, syncdomain.ActionRemoteMove, plan.Actions[0].Type)
	assert.Equal(t, "Projects", plan.Actions[0].OldPath)
	assert.Equal(t, "Renamed Projects", plan.Actions[0].Path)
}

// Validates: R-2.17.1
func TestPlannerPlanCurrentState_IdentityMatchedEditedFileIsDeleteAndCreate(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 'old-hash', 10, 10, 1, 1, 5, 6, 1)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:             "new.txt",
		ItemType:         syncdomain.ItemTypeFile,
		Hash:             "new-hash",
		Size:             20,
		Mtime:            2,
		LocalDevice:      5,
		LocalInode:       6,
		LocalHasIdentity: true,
	}}))

	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 10, 1)`)
	require.NoError(t, err)

	// Same inode but different size and content is indistinguishable from
	// inode reuse after delete + create, so it must not move old.txt's remote
	// content (and history) onto new.txt.
	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 2)
	byPath := make(map[string]syncdomain.Action, len(plan.Actions))
	for _, action := range plan.Actions {
		byPath[action.Path] = action
	}
	assert.Equal(t, syncdomain.ActionRemoteDelete, byPath["old.txt"].Type)
	assert.Equal(t, syncdomain.ActionUpload, byPath["new.txt"].Type)
}

// Validates: R-2.17.1, R-2.17.3
func TestPlannerPlanCurrentState_UnhashedMoveCarriesDestinationTruthAndReuploads(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 'old-hash', 10, 10, 1, 1, 5, 6, 1)`)
	require.NoError(t, err)

	destination := syncdomain.LocalStateRow{
		Path:             "new.txt",
		ItemType:         syncdomain.ItemTypeFile,
		Size:             10,
		Mtime:            1,
		LocalDevice:      5,
		LocalInode:       6,
		LocalHasIdentity: true,
	}
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{destination}))

	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 10, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 2)
	move := plan.Actions[0]
	assert.Equal(t, syncdomain.ActionRemoteMove, move.Type)
	assert.Equal(t, "old.txt", move.OldPath)
	assert.Equal(t, "new.txt", move.Path)
	require.NotNil(t, move.MovedLocal, "a local-origin move must carry the destination's observed local truth")
	assert.Empty(t, move.MovedLocal.Hash)
	assert.Equal(t, destination.Mtime, move.MovedLocal.Mtime)
	assert.Equal(t, syncdomain.ActionUpload, plan.Actions[1].Type)
	assert.Equal(t, "new.txt", plan.Actions[1].Path)
	assert.Equal(t, []int{0}, plan.Deps[1])
}

// Validates: R-2.17.3
//
// A rename here and OneDrive's edit of the same file compose in one plan:
// the remote move, then the download of the edit at the new name, which
// waits for the move and may replace only the synced content found there.
func TestPlannerPlanCurrentState_LocalMoveComposesWithTheRemoteEdit(t *testing.T) {
	t.Parallel()

	s := newPMState(t)
	s.addSynced(matrixPath, false, matrixA.content())
	moveTo("doc2", matrixA)(s, true)
	moveTo(matrixPath, matrixC)(s, false)

	plan := s.plan()
	require.Len(t, plan.Actions, 2)
	moveIndex := slices.IndexFunc(plan.Actions, func(a syncdomain.Action) bool { return a.Type == syncdomain.ActionRemoteMove })
	downloadIndex := slices.IndexFunc(plan.Actions, func(a syncdomain.Action) bool { return a.Type == syncdomain.ActionDownload })
	require.NotEqual(t, -1, moveIndex)
	require.NotEqual(t, -1, downloadIndex)
	move, download := plan.Actions[moveIndex], plan.Actions[downloadIndex]
	assert.Equal(t, "doc2", move.Path)
	assert.Equal(t, "doc2", download.Path)
	assert.Equal(t, move.ItemID, download.ItemID, "the edit comes down onto the same item")
	assert.False(t, download.RequireMissingLocalTarget)
	require.NotNil(t, download.View.Local, "the download replaces only the verified local copy")
	require.NotNil(t, download.View.Baseline)
	assert.Equal(t, matrixPath, download.View.Baseline.Path)
	assert.Contains(t, plan.Deps[downloadIndex], moveIndex, "the download runs after the move")
}

// Validates: R-2.17.3
func TestPlannerPlanCurrentState_RemoteMoveCarriesSourceBaseline(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity, etag
		)
		VALUES ('item-file', 'renamed.txt', 'file', 'h', 'h', 2, 2, 3, 3, 5, 6, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:             "renamed.txt",
		ItemType:         syncdomain.ItemTypeFile,
		Hash:             "h",
		Size:             2,
		Mtime:            3,
		LocalDevice:      5,
		LocalInode:       6,
		LocalHasIdentity: true,
	}}))

	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag)
		VALUES ('item-file', 'sub/renamed-remotely.txt', 'file', 'h', 2, 3, 'etag-new')`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 1)
	move := plan.Actions[0]
	assert.Equal(t, syncdomain.ActionLocalMove, move.Type)
	assert.Equal(t, "renamed.txt", move.OldPath)
	assert.Equal(t, "sub/renamed-remotely.txt", move.Path)
	require.NotNil(t, move.View.Baseline, "a remote move must carry the source baseline")
	assert.Equal(t, "renamed.txt", move.View.Baseline.Path)
	assert.Equal(t, "h", move.View.Baseline.LocalHash)
}

// Validates: R-2.17.2
func TestPlannerPlanCurrentState_UnpairedLocalFolderMoveDeletesSourceAfterChildMoves(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	// A folder baseline without identity (written before folder identity was
	// recorded) cannot pair, so the folder move decomposes. The source folder
	// must be deleted on OneDrive, never re-created locally, and only after
	// the child it contains has been moved out of it.
	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES
			('item-deep', 'deep', 'folder', '', '', NULL, NULL, NULL, NULL, 0, 0, 0),
			('item-leaf', 'deep/leaf.md', 'file', 'hash-leaf', 'hash-leaf', 5, 5, 1, 1, 33, 44, 1)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:             "moved-deep",
			ItemType:         syncdomain.ItemTypeFolder,
			LocalDevice:      11,
			LocalInode:       22,
			LocalHasIdentity: true,
		},
		{
			Path:             "moved-deep/leaf.md",
			ItemType:         syncdomain.ItemTypeFile,
			Hash:             "hash-leaf",
			Size:             5,
			Mtime:            1,
			LocalDevice:      33,
			LocalInode:       44,
			LocalHasIdentity: true,
		},
	}))

	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-deep', 'deep', 'folder', '', NULL, NULL),
			('item-leaf', 'deep/leaf.md', 'file', 'hash-leaf', 5, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	indexByKey := make(map[string]int, len(plan.Actions))
	for i := range plan.Actions {
		action := plan.Actions[i]
		assert.False(t, action.Type == syncdomain.ActionFolderCreate && action.CreateSide == syncdomain.CreateLocal,
			"no local folder may be re-created: %+v", action)
		indexByKey[action.Type.String()+":"+action.Path] = i
	}
	deleteIdx, ok := indexByKey[syncdomain.ActionRemoteDelete.String()+":deep"]
	require.True(t, ok, "source folder must be deleted on OneDrive: %v", indexByKey)
	moveIdx, ok := indexByKey[syncdomain.ActionRemoteMove.String()+":moved-deep/leaf.md"]
	require.True(t, ok, "child must move on OneDrive: %v", indexByKey)
	createIdx, ok := indexByKey[syncdomain.ActionFolderCreate.String()+":moved-deep"]
	require.True(t, ok, "destination folder must be created on OneDrive: %v", indexByKey)
	assert.Equal(t, syncdomain.CreateRemote, plan.Actions[createIdx].CreateSide)
	assert.Contains(t, plan.Deps[deleteIdx], moveIdx, "folder delete must wait for the move out of it")
	assert.Len(t, plan.Actions, 3)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_ExpandsEditEditConflictIntoConcreteActions(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-conflict', 'conflict.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "conflict.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-conflict",
		Path:     "conflict.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	planner := NewPlanner(syncfixture.TestLogger(t))
	plan, err := planner.PlanSnapshot(
		syncstoretest.ReadPlanningSnapshot(t, store),
		Decide,
		PlannerMountContext{DriveID: driveID},
		syncdomain.SyncBidirectional,
	)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 2)

	assert.Equal(t, syncdomain.ActionConflictCopy, plan.Actions[0].Type)
	assert.Equal(t, "conflict.txt", plan.Actions[0].Path)
	assert.False(t, plan.Actions[0].RequireMissingLocalTarget)

	assert.Equal(t, syncdomain.ActionDownload, plan.Actions[1].Type)
	assert.Equal(t, "conflict.txt", plan.Actions[1].Path)
	assert.True(t, plan.Actions[1].RequireMissingLocalTarget)

	require.Len(t, plan.Deps, 2)
	assert.Equal(t, []int{0}, plan.Deps[1], "download should wait for the conflict copy")
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_ExpandsCreateCreateConflictIntoConcreteActions(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "new-collision.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "remote-new",
		Path:     "new-collision.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 2)

	assert.Equal(t, syncdomain.ActionConflictCopy, plan.Actions[0].Type)
	assert.Equal(t, "new-collision.txt", plan.Actions[0].Path)
	assert.False(t, plan.Actions[0].RequireMissingLocalTarget)

	assert.Equal(t, syncdomain.ActionDownload, plan.Actions[1].Type)
	assert.Equal(t, "new-collision.txt", plan.Actions[1].Path)
	assert.True(t, plan.Actions[1].RequireMissingLocalTarget)
	assert.Equal(t, []int{0}, plan.Deps[1], "download should wait for the conflict copy")
}

// Validates: R-2.3.6
func TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('deleted-item', 'edit-delete.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "edit-delete.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 1)

	action := plan.Actions[0]
	assert.Equal(t, syncdomain.ActionUpload, action.Type)
	assert.Equal(t, "edit-delete.txt", action.Path)
	assert.Empty(t, action.ItemID)
	assert.False(t, action.DriveID.IsZero())
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_RemoteParentDeletePlansDescendantLocalDeleteThroughSQLite(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "Projects", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: syncdomain.ItemTypeFile, Hash: "old-hash", Size: 1, Mtime: 1},
	}))
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStore(t, store)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", syncdomain.ActionLocalDelete)
	childIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", syncdomain.ActionLocalDelete)
	assert.Contains(t, plan.Deps[parentIdx], childIdx)
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "Projects", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: syncdomain.ItemTypeFile, Hash: "local-new", Size: 2, Mtime: 2},
	}))
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStore(t, store)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", syncdomain.ActionFolderCreate)
	uploadIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", syncdomain.ActionUpload)
	assert.Equal(t, syncdomain.CreateRemote, plan.Actions[parentIdx].CreateSide)
	assert.Empty(t, plan.Actions[uploadIdx].ItemID)
	assert.Contains(t, plan.Deps[uploadIdx], parentIdx)
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_DownloadOnlyKeepsParentDeleteWhenEditedChildUploadDeferred(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "Projects", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: syncdomain.ItemTypeFile, Hash: "local-new", Size: 2, Mtime: 2},
	}))
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStoreWithMode(t, store, syncdomain.SyncDownloadOnly)

	requireActionIndex(t, plan.Actions, "Projects", syncdomain.ActionLocalDelete)
	assertNoAction(t, plan.Actions, "Projects", syncdomain.ActionFolderCreate)
	assertNoAction(t, plan.Actions, "Projects/file.txt", syncdomain.ActionUpload)
	assert.Equal(t, 1, plan.DeferredByMode.Uploads)
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_UploadOnlyPreservesRemoteParentForAdmittedEditedChildUpload(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "Projects", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: syncdomain.ItemTypeFile, Hash: "local-new", Size: 2, Mtime: 2},
	}))
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStoreWithMode(t, store, syncdomain.SyncUploadOnly)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", syncdomain.ActionFolderCreate)
	uploadIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", syncdomain.ActionUpload)
	assert.Equal(t, syncdomain.CreateRemote, plan.Actions[parentIdx].CreateSide)
	assert.Empty(t, plan.Actions[uploadIdx].ItemID)
	assert.Contains(t, plan.Deps[uploadIdx], parentIdx)
}

// Validates: R-2.1.3, R-2.3.1
func TestPlannerPlanCurrentState_LocalParentDeleteCreatesParentForChangedRemoteChild(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "Projects/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
	}}))
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "folder-projects",
			Path:     "Projects",
			ItemType: syncdomain.ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "file-projects",
			Path:     "Projects/file.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "remote-new",
			Size:     2,
			Mtime:    2,
			ETag:     "etag-new",
		},
	}, "", driveID))

	plan := planCurrentStateForStore(t, store)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", syncdomain.ActionFolderCreate)
	downloadIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", syncdomain.ActionDownload)
	assert.Equal(t, syncdomain.CreateLocal, plan.Actions[parentIdx].CreateSide)
	assert.Contains(t, plan.Deps[downloadIdx], parentIdx)
}

// Validates: R-2.1.3, R-2.4.1
func TestPlannerPlanCurrentState_BothParentSidesDeletedCleansUpDescendantsThroughSQLite(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "Projects/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
	}}))
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveid.New(syncfixture.EngineTestDriveID)))

	plan := planCurrentStateForStore(t, store)

	requireActionIndex(t, plan.Actions, "Projects", syncdomain.ActionCleanup)
	requireActionIndex(t, plan.Actions, "Projects/file.txt", syncdomain.ActionCleanup)
}

// Validates: R-2.1.1, R-2.1.3
func TestPlannerPlanCurrentState_UsesRemoteRowDriveOwnershipForDownloadActions(t *testing.T) {
	t.Parallel()

	sharedDriveID := driveid.New("shared-drive-id")

	plan := planCurrentStateForInputs(
		t,
		[]DecidedComparison{{ComparisonRow: syncdomain.ComparisonRow{
			Path:       "Shared/report.txt",
			RemoteType: syncdomain.SideFile,
		}, ComparisonKind: ComparisonRemoteOnlyCreate}},
		[]ReconciliationRow{{
			Path:               "Shared/report.txt",
			ComparisonKind:     ComparisonRemoteOnlyCreate,
			ReconciliationKind: ReconcileDownload,
		}},
		nil,
		[]syncdomain.RemoteStateRow{{
			Path:     "Shared/report.txt",
			ItemID:   "remote-report",
			DriveID:  sharedDriveID,
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "remote-hash",
			Size:     12,
			Mtime:    123,
		}},
		nil,
		syncdomain.NewBaselineSnapshot(nil),
	)

	require.Len(t, plan.Actions, 1)
	assert.Equal(t, syncdomain.ActionDownload, plan.Actions[0].Type)
	assert.Equal(t, sharedDriveID, plan.Actions[0].DriveID)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_UploadOnlyDefersRemoteWinnerDownloadWithoutConflictCopy(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-conflict', 'conflict.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "conflict.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-conflict",
		Path:     "conflict.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	planner := NewPlanner(syncfixture.TestLogger(t))
	plan, err := planner.PlanSnapshot(
		syncstoretest.ReadPlanningSnapshot(t, store),
		Decide,
		PlannerMountContext{DriveID: driveID},
		syncdomain.SyncUploadOnly,
	)
	require.NoError(t, err)

	assert.Empty(t, plan.Actions)
	assert.Equal(t, 1, plan.DeferredByMode.Downloads)
	assert.Equal(t, 0, plan.DeferredByMode.Uploads)
}

func planCurrentStateForStore(t *testing.T, store *syncstore.SyncStore) *syncdomain.ActionPlan {
	t.Helper()

	return planCurrentStateForStoreWithMode(t, store, syncdomain.SyncBidirectional)
}

func planCurrentStateForStoreWithMode(t *testing.T, store *syncstore.SyncStore, mode syncdomain.SyncMode) *syncdomain.ActionPlan {
	t.Helper()

	return planCurrentStateForStoreWithLogger(t, store, mode, syncfixture.TestLogger(t))
}

// planCurrentStateForStoreWithLogger plans what store holds, as the engine
// reads it at the start of a pass, logging to logger.
func planCurrentStateForStoreWithLogger(t *testing.T, store *syncstore.SyncStore, mode syncdomain.SyncMode, logger *slog.Logger) *syncdomain.ActionPlan {
	t.Helper()

	plan, err := NewPlanner(logger).PlanSnapshot(
		syncstoretest.ReadPlanningSnapshot(t, store),
		Decide,
		PlannerMountContext{DriveID: driveid.New(syncfixture.EngineTestDriveID)},
		mode,
	)
	require.NoError(t, err)

	return plan
}

func planCurrentStateForInputs(
	t *testing.T,
	comparisons []DecidedComparison,
	reconciliations []ReconciliationRow,
	localRows []syncdomain.LocalStateRow,
	remoteRows []syncdomain.RemoteStateRow,
	observationIssues []syncdomain.ObservationIssueRow,
	baseline *syncdomain.BaselineSnapshot,
) *syncdomain.ActionPlan {
	t.Helper()

	planner := NewPlanner(syncfixture.TestLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		observationIssues,
		baseline,
		PlannerMountContext{DriveID: driveid.New(syncfixture.EngineTestDriveID)},
		syncdomain.SyncBidirectional,
	)
	require.NoError(t, err)

	return plan
}

func seedFolderCascadeBaseline(t *testing.T, store *syncstore.SyncStore) {
	t.Helper()

	_, err := syncstoretest.RawDB(t, store).ExecContext(t.Context(), `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime, etag
		)
		VALUES
			('folder-projects', 'Projects', 'folder', '', '', NULL, NULL, NULL, NULL, ''),
			('file-projects', 'Projects/file.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)
}

func requireActionIndex(t *testing.T, actions []syncdomain.Action, path string, actionType syncdomain.ActionType) int {
	t.Helper()

	for i := range actions {
		if actions[i].Path == path && actions[i].Type == actionType {
			return i
		}
	}

	require.Failf(t, "missing action", "no %s action for %s in %#v", actionType.String(), path, actions)
	return -1
}

func assertNoAction(t *testing.T, actions []syncdomain.Action, path string, actionType syncdomain.ActionType) {
	t.Helper()

	for i := range actions {
		assert.Falsef(
			t,
			actions[i].Path == path && actions[i].Type == actionType,
			"unexpected %s action for %s in %#v",
			actionType.String(),
			path,
			actions,
		)
	}
}

func planForUnavailableLocalReadBoundaryDescendant(t *testing.T) *syncdomain.ActionPlan {
	t.Helper()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private', 'Private/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-private",
		Path:     "Private/sub/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-private",
	}}, "", driveID))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Private",
		DriveID:   driveID,
		IssueType: syncdomain.IssueLocalReadDenied,
		ScopeKey:  syncdomain.SKPermLocalRead("Private"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableRemoteReadBoundaryDescendant(t *testing.T) *syncdomain.ActionPlan {
	t.Helper()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared', 'Shared/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "Shared/sub/file.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
	}}))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Shared",
		DriveID:   driveID,
		IssueType: syncdomain.IssueRemoteReadDenied,
		ScopeKey:  syncdomain.SKPermRemoteRead("Shared"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableLocalReadBoundaryCleanupCandidate(t *testing.T) *syncdomain.ActionPlan {
	t.Helper()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private-dir', 'Private/sub', 'folder', '', '', 0, 0, 1, 1, ''),
		       ('item-private-file', 'Private/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Private",
		DriveID:   driveid.New(syncfixture.EngineTestDriveID),
		IssueType: syncdomain.IssueLocalReadDenied,
		ScopeKey:  syncdomain.SKPermLocalRead("Private"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableRemoteReadBoundaryCleanupCandidate(t *testing.T) *syncdomain.ActionPlan {
	t.Helper()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared-dir', 'Shared/sub', 'folder', '', '', 0, 0, 1, 1, ''),
		       ('item-shared-file', 'Shared/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:     "Shared/sub",
			ItemType: syncdomain.ItemTypeFolder,
		},
		{
			Path:     "Shared/sub/file.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash",
			Size:     10,
			Mtime:    1,
		},
	}))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Shared",
		DriveID:   driveID,
		IssueType: syncdomain.IssueRemoteReadDenied,
		ScopeKey:  syncdomain.SKPermRemoteRead("Shared"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableLocalMoveSource(t *testing.T) *syncdomain.ActionPlan {
	t.Helper()

	return planCurrentStateForInputs(
		t,
		[]DecidedComparison{{ComparisonRow: syncdomain.ComparisonRow{
			Path:          "docs/source.txt",
			BaselineType:  syncdomain.SideFile,
			LocalType:     syncdomain.SideFile,
			RemoteType:    syncdomain.SideFile,
			LocalChanged:  true,
			RemoteChanged: false,
		}, ComparisonKind: ComparisonLocalMoveSource}},
		[]ReconciliationRow{{
			Path:               "docs/source.txt",
			ComparisonKind:     ComparisonLocalMoveSource,
			ReconciliationKind: ReconcileLocalMove,
			LocalMoveTarget:    "docs/dest.txt",
		}},
		[]syncdomain.LocalStateRow{{
			Path:     "docs/source.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "local",
		}},
		[]syncdomain.RemoteStateRow{{
			Path:    "docs/source.txt",
			ItemID:  "remote-source",
			DriveID: driveid.New(syncfixture.EngineTestDriveID),
		}},
		[]syncdomain.ObservationIssueRow{{
			Path:      "docs/source.txt",
			IssueType: syncdomain.IssueLocalReadDenied,
		}},
		syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{{
			Path:     "docs/source.txt",
			DriveID:  driveid.New(syncfixture.EngineTestDriveID),
			ItemID:   "baseline-source",
			ItemType: syncdomain.ItemTypeFile,
		}}),
	)
}

func planForUnavailableRemoteMoveDestination(t *testing.T) *syncdomain.ActionPlan {
	t.Helper()

	return planCurrentStateForInputs(
		t,
		[]DecidedComparison{{ComparisonRow: syncdomain.ComparisonRow{
			Path:          "Shared/dest.txt",
			BaselineType:  syncdomain.SideFile,
			LocalType:     syncdomain.SideFile,
			RemoteType:    syncdomain.SideFile,
			LocalChanged:  false,
			RemoteChanged: true,
		}, ComparisonKind: ComparisonRemoteMoveDest}},
		[]ReconciliationRow{{
			Path:               "Shared/dest.txt",
			ComparisonKind:     ComparisonRemoteMoveDest,
			ReconciliationKind: ReconcileRemoteMove,
			RemoteMoveSource:   "Shared/source.txt",
		}},
		[]syncdomain.LocalStateRow{{
			Path:     "Shared/dest.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "local",
		}},
		[]syncdomain.RemoteStateRow{{
			Path:    "Shared/dest.txt",
			ItemID:  "remote-dest",
			DriveID: driveid.New(syncfixture.EngineTestDriveID),
		}},
		[]syncdomain.ObservationIssueRow{{
			Path:      "Shared",
			IssueType: syncdomain.IssueRemoteReadDenied,
			ScopeKey:  syncdomain.SKPermRemoteRead("Shared"),
		}},
		syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{{
			Path:     "Shared/dest.txt",
			DriveID:  driveid.New(syncfixture.EngineTestDriveID),
			ItemID:   "baseline-dest",
			ItemType: syncdomain.ItemTypeFile,
		}}),
	)
}

func assertNoActionForPath(t *testing.T, plan *syncdomain.ActionPlan, path string, actionType syncdomain.ActionType) {
	t.Helper()

	for i := range plan.Actions {
		assert.Falsef(
			t,
			plan.Actions[i].Path == path && plan.Actions[i].Type == actionType,
			"unexpected %s for %s",
			actionType,
			path,
		)
	}
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadDeniedDoesNotDeleteRemoteData(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-danger', 'danger.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-danger')`)
	require.NoError(t, err)

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-danger",
		Path:     "danger.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-danger",
	}}, "", driveID))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "danger.txt",
		DriveID:   driveID,
		IssueType: syncdomain.IssueLocalReadDenied,
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "danger.txt", syncdomain.ActionRemoteDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundaryDoesNotDeleteLocalData(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared', 'Shared/a.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "Shared/a.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
	}}))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Shared",
		DriveID:   driveid.New(syncfixture.EngineTestDriveID),
		IssueType: syncdomain.IssueRemoteReadDenied,
		ScopeKey:  syncdomain.SKPermRemoteRead("Shared"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Shared/a.txt", syncdomain.ActionLocalDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadBoundaryBlocksRemoteDeletesForDescendants(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private', 'Private/a.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-private",
		Path:     "Private/a.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-private",
	}}, "", driveID))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Private",
		DriveID:   driveID,
		IssueType: syncdomain.IssueLocalReadDenied,
		ScopeKey:  syncdomain.SKPermLocalRead("Private"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Private/a.txt", syncdomain.ActionRemoteDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundaryBlocksLocalDeletesForDescendants(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-team-a', 'Team/a.txt', 'file', 'hash-a', 'hash-a', 10, 10, 1, 1, 'etag-team-a'),
			('item-team-b', 'Team/sub/b.txt', 'file', 'hash-b', 'hash-b', 11, 11, 2, 2, 'etag-team-b')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:     "Team/a.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-a",
			Size:     10,
			Mtime:    1,
		},
		{
			Path:     "Team/sub/b.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-b",
			Size:     11,
			Mtime:    2,
		},
	}))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Team",
		DriveID:   driveid.New(syncfixture.EngineTestDriveID),
		IssueType: syncdomain.IssueRemoteReadDenied,
		ScopeKey:  syncdomain.SKPermRemoteRead("Team"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Team/a.txt", syncdomain.ActionLocalDelete)
	assertNoActionForPath(t, plan, "Team/sub/b.txt", syncdomain.ActionLocalDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadBoundarySuppressesRemoteOnlySubtreeActions(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, store.CommitObservation(ctx, []syncdomain.ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-private-dir",
			Path:     "Private",
			ItemType: syncdomain.ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "item-private-subdir",
			Path:     "Private/sub",
			ItemType: syncdomain.ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "item-private-file",
			Path:     "Private/sub/file.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-private",
			Size:     10,
			Mtime:    1,
			ETag:     "etag-private",
		},
	}, "", driveID))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Private",
		DriveID:   driveID,
		IssueType: syncdomain.IssueLocalReadDenied,
		ScopeKey:  syncdomain.SKPermLocalRead("Private"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Private", syncdomain.ActionFolderCreate)
	assertNoActionForPath(t, plan, "Private/sub", syncdomain.ActionFolderCreate)
	assertNoActionForPath(t, plan, "Private/sub/file.txt", syncdomain.ActionDownload)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundarySuppressesLocalOnlySubtreeActions(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:     "Shared",
			ItemType: syncdomain.ItemTypeFolder,
		},
		{
			Path:     "Shared/sub",
			ItemType: syncdomain.ItemTypeFolder,
		},
		{
			Path:     "Shared/sub/file.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-shared",
			Size:     10,
			Mtime:    1,
		},
	}))
	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "Shared",
		DriveID:   driveid.New(syncfixture.EngineTestDriveID),
		IssueType: syncdomain.IssueRemoteReadDenied,
		ScopeKey:  syncdomain.SKPermRemoteRead("Shared"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Shared", syncdomain.ActionFolderCreate)
	assertNoActionForPath(t, plan, "Shared/sub", syncdomain.ActionFolderCreate)
	assertNoActionForPath(t, plan, "Shared/sub/file.txt", syncdomain.ActionUpload)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_NewUnreadableLocalPathProducesNoActions(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	syncstoretest.SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      "blocked/new.txt",
		DriveID:   driveID,
		IssueType: syncdomain.IssueLocalReadDenied,
	})

	plan := planCurrentStateForStore(t, store)
	assert.Empty(t, plan.Actions)

	observationIssues, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, observationIssues, 1)
	assert.Equal(t, "blocked/new.txt", observationIssues[0].Path)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalMoveSourceWithUnavailableTruthProducesNoActions(t *testing.T) {
	t.Parallel()

	plan := planForUnavailableLocalMoveSource(t)

	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteMoveDestinationWithUnavailableTruthProducesNoActions(t *testing.T) {
	t.Parallel()

	plan := planForUnavailableRemoteMoveDestination(t)

	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_UnavailableTruthNeverDeletesData(t *testing.T) {
	t.Parallel()

	t.Run("local read boundary keeps remote descendants from looking deleted", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableLocalReadBoundaryDescendant(t)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", syncdomain.ActionRemoteDelete)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", syncdomain.ActionDownload)
		assert.Empty(t, plan.Actions)
	})

	t.Run("remote read boundary keeps local descendants from looking deleted", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteReadBoundaryDescendant(t)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", syncdomain.ActionLocalDelete)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", syncdomain.ActionUpload)
		assert.Empty(t, plan.Actions)
	})

	t.Run("local unreadable subtree suppresses cleanup for descendants", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableLocalReadBoundaryCleanupCandidate(t)
		assertNoActionForPath(t, plan, "Private/sub", syncdomain.ActionCleanup)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", syncdomain.ActionCleanup)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", syncdomain.ActionRemoteDelete)
		assert.Empty(t, plan.Actions)
	})

	t.Run("remote unreadable subtree suppresses cleanup for descendants", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteReadBoundaryCleanupCandidate(t)
		assertNoActionForPath(t, plan, "Shared/sub", syncdomain.ActionCleanup)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", syncdomain.ActionCleanup)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", syncdomain.ActionLocalDelete)
		assert.Empty(t, plan.Actions)
	})

	t.Run("unavailable move source produces no remote move", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableLocalMoveSource(t)

		assert.Empty(t, plan.Actions)
	})

	t.Run("unavailable remote move destination produces no local move", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteMoveDestination(t)

		assert.Empty(t, plan.Actions)
	})
}

// Validates: R-2.17.2
func TestPlannerPlanCurrentState_LocalFolderDeletePropagatesAfterChildDeletes(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	// rm -rf many/ while watch mode runs: every baseline row under many is
	// gone locally and still present remotely.
	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime
		)
		VALUES
			('item-many', 'many', 'folder', '', '', NULL, NULL, NULL, NULL),
			('item-f000', 'many/f000.txt', 'file', 'h0', 'h0', 1, 1, 1, 1),
			('item-f001', 'many/f001.txt', 'file', 'h1', 'h1', 1, 1, 1, 1),
			('item-sub', 'many/sub', 'folder', '', '', NULL, NULL, NULL, NULL),
			('item-f002', 'many/sub/f002.txt', 'file', 'h2', 'h2', 1, 1, 1, 1)`)
	require.NoError(t, err)

	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-many', 'many', 'folder', '', NULL, NULL),
			('item-f000', 'many/f000.txt', 'file', 'h0', 1, 1),
			('item-f001', 'many/f001.txt', 'file', 'h1', 1, 1),
			('item-sub', 'many/sub', 'folder', '', NULL, NULL),
			('item-f002', 'many/sub/f002.txt', 'file', 'h2', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 5)
	indexByPath := make(map[string]int, len(plan.Actions))
	for i := range plan.Actions {
		assert.Equal(t, syncdomain.ActionRemoteDelete, plan.Actions[i].Type,
			"%s: a locally deleted tree is deleted remotely, never re-created locally", plan.Actions[i].Path)
		indexByPath[plan.Actions[i].Path] = i
	}
	assert.ElementsMatch(t,
		[]int{indexByPath["many/f000.txt"], indexByPath["many/f001.txt"], indexByPath["many/sub"], indexByPath["many/sub/f002.txt"]},
		plan.Deps[indexByPath["many"]],
		"the folder delete runs only after everything under it")
}

// The folder delete that a local rm -rf now propagates must never take a file
// added on OneDrive after the last sync with it: that file is downloaded and
// the folder is re-created locally instead.
//
// Validates: R-2.17.2
func TestPlannerPlanCurrentState_LocalFolderDeleteKeepsFolderForNewRemoteChild(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime
		)
		VALUES
			('item-many', 'many', 'folder', '', '', NULL, NULL, NULL, NULL),
			('item-f000', 'many/f000.txt', 'file', 'h0', 'h0', 1, 1, 1, 1)`)
	require.NoError(t, err)

	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-many', 'many', 'folder', '', NULL, NULL),
			('item-f000', 'many/f000.txt', 'file', 'h0', 1, 1),
			('item-new', 'many/added-remotely.txt', 'file', 'hn', 1, 2)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)

	assert.Contains(t, index, "remote_delete:many/f000.txt", "the file the user deleted is still deleted")
	assert.Contains(t, index, "download:many/added-remotely.txt", "the new remote file is kept")
	assert.NotContains(t, index, "remote_delete:many", "the folder holding the new file is not deleted")
	folderIdx, ok := index["folder_create:many"]
	require.True(t, ok, "the folder is re-created locally for the new file")
	assert.Equal(t, syncdomain.CreateLocal, plan.Actions[folderIdx].CreateSide)
}

func planActionIndex(t *testing.T, plan *syncdomain.ActionPlan) map[string]int {
	t.Helper()

	index := make(map[string]int, len(plan.Actions))
	for i := range plan.Actions {
		key := plan.Actions[i].Type.String() + ":" + plan.Actions[i].Path
		_, dup := index[key]
		require.False(t, dup, "duplicate action %s", key)
		index[key] = i
	}

	return index
}

// Validates: R-2.17.4
func TestPlannerPlanCurrentState_LocalFileReplacedByFolderReplacesRemoteItem(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	// rm lowercase.txt; mkdir lowercase.txt; echo > lowercase.txt/inside.txt
	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-lower', 'lowercase.txt', 'file', 'h', 'h', 1, 1, 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "lowercase.txt", ItemType: syncdomain.ItemTypeFolder},
		{Path: "lowercase.txt/inside.txt", ItemType: syncdomain.ItemTypeFile, Hash: "inside", Size: 1, Mtime: 2},
	}))
	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-lower', 'lowercase.txt', 'file', 'h', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 3, "%v", index)

	deleteIdx, ok := index[syncdomain.ActionRemoteDelete.String()+":lowercase.txt"]
	require.True(t, ok, "%v", index)
	assert.Equal(t, "item-lower", plan.Actions[deleteIdx].ItemID)
	createIdx, ok := index[syncdomain.ActionFolderCreate.String()+":lowercase.txt"]
	require.True(t, ok, "%v", index)
	assert.Equal(t, syncdomain.CreateRemote, plan.Actions[createIdx].CreateSide)
	uploadIdx, ok := index[syncdomain.ActionUpload.String()+":lowercase.txt/inside.txt"]
	require.True(t, ok, "%v", index)

	assert.Contains(t, plan.Deps[createIdx], deleteIdx, "the new folder is created only after the old file is gone")
	assert.Contains(t, plan.Deps[uploadIdx], createIdx)
}

// Validates: R-2.17.4
func TestPlannerPlanCurrentState_LocalFolderReplacedByFileReplacesRemoteItem(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES
			('item-dir', 'thing', 'folder', NULL, NULL, NULL, NULL, NULL, NULL),
			('item-child', 'thing/child.txt', 'file', 'c', 'c', 1, 1, 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "thing", ItemType: syncdomain.ItemTypeFile, Hash: "now-a-file", Size: 3, Mtime: 4},
	}))
	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-dir', 'thing', 'folder', NULL, NULL, NULL),
			('item-child', 'thing/child.txt', 'file', 'c', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 3, "%v", index)

	childDeleteIdx, ok := index[syncdomain.ActionRemoteDelete.String()+":thing/child.txt"]
	require.True(t, ok, "%v", index)
	folderDeleteIdx, ok := index[syncdomain.ActionRemoteDelete.String()+":thing"]
	require.True(t, ok, "%v", index)
	uploadIdx, ok := index[syncdomain.ActionUpload.String()+":thing"]
	require.True(t, ok, "%v", index)
	assert.Empty(t, plan.Actions[uploadIdx].ItemID, "the file is new content, not an overwrite of the folder item")

	assert.Contains(t, plan.Deps[folderDeleteIdx], childDeleteIdx)
	assert.Contains(t, plan.Deps[uploadIdx], folderDeleteIdx)
}

// Validates: R-2.17.4
func TestPlannerPlanCurrentState_RemoteFileReplacedByFolderReplacesLocalItem(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-old-file', 'shape', 'file', 'h', 'h', 1, 1, 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "shape", ItemType: syncdomain.ItemTypeFile, Hash: "h", Size: 1, Mtime: 1},
	}))
	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-new-folder', 'shape', 'folder', NULL, NULL, NULL),
			('item-inner', 'shape/inner.txt', 'file', 'i', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 3, "%v", index)

	deleteIdx, ok := index[syncdomain.ActionLocalDelete.String()+":shape"]
	require.True(t, ok, "%v", index)
	createIdx, ok := index[syncdomain.ActionFolderCreate.String()+":shape"]
	require.True(t, ok, "%v", index)
	assert.Equal(t, syncdomain.CreateLocal, plan.Actions[createIdx].CreateSide)
	downloadIdx, ok := index[syncdomain.ActionDownload.String()+":shape/inner.txt"]
	require.True(t, ok, "%v", index)

	assert.Contains(t, plan.Deps[createIdx], deleteIdx)
	assert.Contains(t, plan.Deps[downloadIdx], createIdx)
}

// Validates: R-2.17.4
func TestPlannerPlanCurrentState_RemoteFolderReplacedByFileReplacesLocalItem(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-old-dir', 'shape', 'folder', NULL, NULL, NULL, NULL, NULL, NULL)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "shape", ItemType: syncdomain.ItemTypeFolder},
	}))
	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-new-file', 'shape', 'file', 'f', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 2, "%v", index)

	deleteIdx, ok := index[syncdomain.ActionLocalDelete.String()+":shape"]
	require.True(t, ok, "%v", index)
	downloadIdx, ok := index[syncdomain.ActionDownload.String()+":shape"]
	require.True(t, ok, "%v", index)
	assert.True(t, plan.Actions[downloadIdx].RequireMissingLocalTarget)
	assert.Equal(t, "item-new-file", plan.Actions[downloadIdx].ItemID)
	assert.Contains(t, plan.Deps[downloadIdx], deleteIdx)
}

// Validates: R-2.12.1, R-2.17.1
func TestPlannerPlanCurrentState_CaseOnlyRenameWithEditUploadsAfterOldNameIsDeleted(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()

	// mv edit.txt EDIT.txt; then rewrite it, so no move can be proven.
	_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-edit', 'edit.txt', 'file', 'old', 'old', 3, 3, 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "EDIT.txt", ItemType: syncdomain.ItemTypeFile, Hash: "new", Size: 30, Mtime: 2},
	}))
	_, err = syncstoretest.RawDB(t, store).ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-edit', 'edit.txt', 'file', 'old', 3, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 2, "%v", index)

	deleteIdx, ok := index[syncdomain.ActionRemoteDelete.String()+":edit.txt"]
	require.True(t, ok, "%v", index)
	uploadIdx, ok := index[syncdomain.ActionUpload.String()+":EDIT.txt"]
	require.True(t, ok, "%v", index)
	assert.Contains(t, plan.Deps[uploadIdx], deleteIdx,
		"OneDrive names are case-insensitive: an upload that ran first would replace the item the delete then removes")
}

// Validates: R-2.12.1
func TestBuildDependencies_CaseVariantMovesDoNotWaitOnEachOther(t *testing.T) {
	t.Parallel()

	// Two files trade case-variant names: x -> Y and y -> X.
	actions := []syncdomain.Action{
		{Type: syncdomain.ActionRemoteMove, OldPath: "x", Path: "Y"},
		{Type: syncdomain.ActionRemoteMove, OldPath: "y", Path: "X"},
		{Type: syncdomain.ActionUpload, Path: "dir/A.txt"},
		{Type: syncdomain.ActionRemoteMove, OldPath: "dir/a.txt", Path: "dir/b.txt"},
	}

	deps := buildDependencies(actions)
	require.NoError(t, detectDependencyCycle(deps))
	assert.Empty(t, deps[0])
	assert.Empty(t, deps[1])
	assert.Equal(t, []int{3}, deps[2], "a create waits for a move that vacates its case-variant name")
}

// Validates: R-2.4.8
//
// A plan that releases its local tree leaves out only what is new to it: three
// new nested folders stay, because a synced file OneDrive moved into the
// deepest one needs them, and the new file beside it is left out. The count
// is logged once, at Debug.
func TestLeaveOutNewLocalItems_KeepsTheNewFoldersAnAdmittedMoveNeeds(t *testing.T) {
	t.Parallel()

	newRemote := func(storedPath string, itemType syncdomain.ItemType) *syncdomain.PathView {
		return &syncdomain.PathView{Path: storedPath, Remote: &syncdomain.RemoteState{ItemID: "item-" + storedPath, ItemType: itemType}}
	}
	folders := []string{"New", "New/Deeper", "New/Deeper/Deepest"}
	var actions []syncdomain.Action
	for _, folder := range folders {
		actions = append(actions, makeFolderCreate(newRemote(folder, syncdomain.ItemTypeFolder), syncdomain.CreateLocal))
	}
	moved := makeAction(syncdomain.ActionLocalMove, &syncdomain.PathView{
		Path:     "New/Deeper/Deepest/moved.txt",
		Baseline: &syncdomain.BaselineEntry{Path: "moved.txt", ItemID: "item-moved", ItemType: syncdomain.ItemTypeFile},
		Remote:   &syncdomain.RemoteState{ItemID: "item-moved", ItemType: syncdomain.ItemTypeFile},
	})
	moved.OldPath = "moved.txt"
	actions = append(actions, moved, makeAction(syncdomain.ActionDownload, newRemote("New/Deeper/Deepest/beside.txt", syncdomain.ItemTypeFile)))
	capture, logger := synctest.NewLogCapture()

	kept := NewPlanner(logger).leaveOutNewLocalItems(actions, syncdomain.SyncBidirectional)

	paths := make([]string, 0, len(kept))
	for i := range kept {
		paths = append(paths, kept[i].Type.String()+" "+kept[i].Path)
	}
	assert.Equal(t, []string{
		"folder_create New",
		"folder_create New/Deeper",
		"folder_create New/Deeper/Deepest",
		"local_move New/Deeper/Deepest/moved.txt",
	}, paths, "only the new file beside the moved one is left out")
	var leftOut []synctest.CapturedRecord
	for _, record := range capture.Records() {
		if record.Message == "plan of a tree being released leaves out items new to it" {
			leftOut = append(leftOut, record)
		}
	}
	require.Len(t, leftOut, 1)
	assert.Equal(t, slog.LevelDebug, leftOut[0].Level)
	assert.Equal(t, "1", leftOut[0].Attrs["left_out"])
}

// Validates: R-6.2.16
//
// The planner's decisions on the action type panic with OutsideTheEnum on a
// value outside the enum, like the vocabulary's
// (TestActionTypeDecisions_PanicOnATypeOutsideTheEnum): not admitted, not a
// local write, creates nothing.
func TestPlannerActionTypeDecisions_PanicOnATypeOutsideTheEnum(t *testing.T) {
	t.Parallel()

	const (
		unlisted     = syncdomain.ActionCleanup + 1
		unlistedSide = syncdomain.CreateRemote + 1
	)
	outside := func() *syncdomain.Action {
		return &syncdomain.Action{
			Type:    unlisted,
			Path:    "docs/a.txt",
			OldPath: "docs",
			ItemID:  "item-a",
			View:    &syncdomain.PathView{Path: "docs/a.txt", Baseline: &syncdomain.BaselineEntry{Path: "docs", ItemType: syncdomain.ItemTypeFolder}},
		}
	}
	upload := func(p string) *syncdomain.Action { return &syncdomain.Action{Type: syncdomain.ActionUpload, Path: p} }
	movedLocally := &moveComposition{moves: []composedMove{{key: "item-a", side: moveCoordLocal}}}
	mode := syncdomain.SyncBidirectional

	decisions := map[string]func(){
		"actionAllowedInMode":               func() { actionAllowedInMode(outside(), syncdomain.SyncBidirectional) },
		"writesLocally":                     func() { writesLocally(outside()) },
		"countRenames":                      func() { countRenames([]syncdomain.Action{*outside()}) },
		"indexByPath":                       func() { newDependencyIndex([]syncdomain.Action{*outside()}) },
		"indexVacatedNames":                 func() { (&dependencyIndex{}).indexVacatedNames([]syncdomain.Action{*outside()}) },
		"isDeleteAction":                    func() { isDeleteAction(outside()) },
		"isMoveAction":                      func() { isMoveAction(outside()) },
		"addMoveOutOfDeletedFolderDeps":     func() { addMoveOutOfDeletedFolderDeps(nil, 0, outside(), nil, &dependencyIndex{}) },
		"addRemoteCaseVariantDeps":          func() { addRemoteCaseVariantDeps(nil, 0, outside(), nil, &dependencyIndex{}) },
		"vacatesCaseVariantOf":              func() { vacatesCaseVariantOf(outside(), upload("docs/A.txt")) },
		"underMoveSource":                   func() { underMoveSource(upload("docs/x.txt"), outside()) },
		"createsAtVacatedSource":            func() { createsAtVacatedSource(upload("docs"), outside()) },
		"pathsVacatedByPlan":                func() { pathsVacatedByPlan([]syncdomain.Action{*outside()}) },
		"createSide":                        func() { createSide(outside()) },
		"deleteSide":                        func() { deleteSide(outside()) },
		"dropReconciliationOfComposedMoves": func() { dropReconciliationOfComposedMoves([]syncdomain.Action{*outside()}, movedLocally, nil, mode) },
	}
	for name, decide := range decisions {
		assert.PanicsWithValue(t, syncdomain.OutsideTheEnum(unlisted), decide, name)
	}
	assert.PanicsWithValue(t, syncdomain.OutsideTheEnum(unlistedSide), func() {
		actionAllowedInMode(&syncdomain.Action{Type: syncdomain.ActionFolderCreate, CreateSide: unlistedSide}, syncdomain.SyncBidirectional)
	}, "actionAllowedInMode on a folder create side outside its enum")
}
