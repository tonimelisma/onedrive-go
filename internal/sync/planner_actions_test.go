package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.1, R-2.1.3, R-2.1.4
func TestPlannerPlanCurrentState_BuildsActionsFromSQLiteReconciliation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 'old', 1, 1, 1, 1, 'etag-old'),
			('item-folder', 'folder', 'folder', '', '', NULL, NULL, NULL, NULL, NULL)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{
			Path:     "upload.txt",
			ItemType: ItemTypeFile,
			Hash:     "new-local",
			Size:     2,
			Mtime:    2,
		},
		{
			Path:     "new-folder",
			ItemType: ItemTypeFolder,
		},
	}))

	require.NoError(t, store.CommitObservation(ctx, []observedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-upload",
			Path:     "upload.txt",
			ItemType: ItemTypeFile,
			Hash:     "old",
			Size:     1,
			Mtime:    1,
			ETag:     "etag-old",
		},
		{
			DriveID:  driveID,
			ItemID:   "item-folder",
			Path:     "folder",
			ItemType: ItemTypeFolder,
		},
	}, "", driveID))

	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)

	planner := newPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		nil,
		bl,
		plannerMountContext{DriveID: driveID},
		SyncBidirectional,
	)
	require.NoError(t, err)

	require.Len(t, plan.Actions, 3)
	byPath := make(map[string]Action, len(plan.Actions))
	for _, action := range plan.Actions {
		byPath[action.Path] = action
	}

	assert.Equal(t, ActionRemoteDelete, byPath["folder"].Type, "a locally deleted folder is deleted remotely")
	assert.Equal(t, ActionFolderCreate, byPath["new-folder"].Type)
	assert.Equal(t, CreateRemote, byPath["new-folder"].CreateSide)
	assert.Equal(t, ActionUpload, byPath["upload.txt"].Type)
}

// Validates: R-2.1.3, R-2.1.4
func TestPlannerPlanCurrentState_LocalFolderMoveByIdentityPlansSingleRemoteMove(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES
			('item-folder', 'Projects', 'folder', '', '', NULL, NULL, NULL, NULL, 11, 22, 1),
			('item-file', 'Projects/a.txt', 'file', 'hash-a', 'hash-a', 5, 5, 1, 1, 33, 44, 1)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{
			Path:             "Renamed Projects",
			ItemType:         ItemTypeFolder,
			LocalDevice:      11,
			LocalInode:       22,
			LocalHasIdentity: true,
		},
		{
			Path:             "Renamed Projects/a.txt",
			ItemType:         ItemTypeFile,
			Hash:             "hash-a",
			Size:             5,
			Mtime:            1,
			LocalDevice:      33,
			LocalInode:       44,
			LocalHasIdentity: true,
		},
	}))

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-folder', 'Projects', 'folder', '', NULL, NULL),
			('item-file', 'Projects/a.txt', 'file', 'hash-a', 5, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 1)
	assert.Equal(t, ActionRemoteMove, plan.Actions[0].Type)
	assert.Equal(t, "Projects", plan.Actions[0].OldPath)
	assert.Equal(t, "Renamed Projects", plan.Actions[0].Path)
}

// Validates: R-2.1.3, R-2.1.4
func TestOmitDescendantRemoteMovesCoveredByFolderMoves_KeepsDescendantRename(t *testing.T) {
	t.Parallel()

	actions := []Action{
		{
			Type:    ActionRemoteMove,
			OldPath: "Projects",
			Path:    "Archive",
			View: &pathView{
				Local: &localState{ItemType: ItemTypeFolder},
			},
		},
		{
			Type:    ActionRemoteMove,
			OldPath: "Projects/kept.txt",
			Path:    "Archive/kept.txt",
			View: &pathView{
				Local: &localState{ItemType: ItemTypeFile},
			},
		},
		{
			Type:    ActionRemoteMove,
			OldPath: "Projects/old.txt",
			Path:    "Archive/new.txt",
			View: &pathView{
				Local: &localState{ItemType: ItemTypeFile},
			},
		},
	}

	normalized := omitDescendantRemoteMoveActionsCoveredByGraphFolderMoves(actions)

	require.Len(t, normalized, 2)
	assert.Equal(t, "Projects", normalized[0].OldPath)
	assert.Equal(t, "Projects/old.txt", normalized[1].OldPath)
	assert.Equal(t, "Archive/new.txt", normalized[1].Path)
	assert.Len(t, actions, 3, "normalization should not mutate the caller's action slice")
}

// Validates: R-2.17.1
func TestPlannerPlanCurrentState_IdentityMatchedEditedFileIsDeleteAndCreate(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 'old-hash', 10, 10, 1, 1, 5, 6, 1)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:             "new.txt",
		ItemType:         ItemTypeFile,
		Hash:             "new-hash",
		Size:             20,
		Mtime:            2,
		LocalDevice:      5,
		LocalInode:       6,
		LocalHasIdentity: true,
	}}))

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 10, 1)`)
	require.NoError(t, err)

	// Same inode but different size and content is indistinguishable from
	// inode reuse after delete + create, so it must not move old.txt's remote
	// content (and history) onto new.txt.
	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 2)
	byPath := make(map[string]Action, len(plan.Actions))
	for _, action := range plan.Actions {
		byPath[action.Path] = action
	}
	assert.Equal(t, ActionRemoteDelete, byPath["old.txt"].Type)
	assert.Equal(t, ActionUpload, byPath["new.txt"].Type)
}

// Validates: R-2.17.1, R-2.17.3
func TestPlannerPlanCurrentState_UnhashedMoveCarriesDestinationTruthAndReuploads(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 'old-hash', 10, 10, 1, 1, 5, 6, 1)`)
	require.NoError(t, err)

	destination := localStateRow{
		Path:             "new.txt",
		ItemType:         ItemTypeFile,
		Size:             10,
		Mtime:            1,
		LocalDevice:      5,
		LocalInode:       6,
		LocalHasIdentity: true,
	}
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{destination}))

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-file', 'old.txt', 'file', 'old-hash', 10, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 2)
	move := plan.Actions[0]
	assert.Equal(t, ActionRemoteMove, move.Type)
	assert.Equal(t, "old.txt", move.OldPath)
	assert.Equal(t, "new.txt", move.Path)
	require.NotNil(t, move.MovedLocal, "a local-origin move must carry the destination's observed local truth")
	assert.Empty(t, move.MovedLocal.Hash)
	assert.Equal(t, destination.Mtime, move.MovedLocal.Mtime)
	assert.Equal(t, ActionUpload, plan.Actions[1].Type)
	assert.Equal(t, "new.txt", plan.Actions[1].Path)
	assert.Equal(t, []int{0}, plan.Deps[1])
}

// Validates: R-2.17.3
func TestPlannerPlanCurrentState_RemoteMoveCarriesSourceBaseline(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity, etag
		)
		VALUES ('item-file', 'renamed.txt', 'file', 'h', 'h', 2, 2, 3, 3, 5, 6, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:             "renamed.txt",
		ItemType:         ItemTypeFile,
		Hash:             "h",
		Size:             2,
		Mtime:            3,
		LocalDevice:      5,
		LocalInode:       6,
		LocalHasIdentity: true,
	}}))

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag)
		VALUES ('item-file', 'sub/renamed-remotely.txt', 'file', 'h', 2, 3, 'etag-new')`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 1)
	move := plan.Actions[0]
	assert.Equal(t, ActionLocalMove, move.Type)
	assert.Equal(t, "renamed.txt", move.OldPath)
	assert.Equal(t, "sub/renamed-remotely.txt", move.Path)
	require.NotNil(t, move.View.Baseline, "a remote move must carry the source baseline")
	assert.Equal(t, "renamed.txt", move.View.Baseline.Path)
	assert.Equal(t, "h", move.View.Baseline.LocalHash)
}

// Validates: R-2.17.2
func TestPlannerPlanCurrentState_UnpairedLocalFolderMoveDeletesSourceAfterChildMoves(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	// A folder baseline without identity (written before folder identity was
	// recorded) cannot pair, so the folder move decomposes. The source folder
	// must be deleted on OneDrive, never re-created locally, and only after
	// the child it contains has been moved out of it.
	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES
			('item-deep', 'deep', 'folder', '', '', NULL, NULL, NULL, NULL, 0, 0, 0),
			('item-leaf', 'deep/leaf.md', 'file', 'hash-leaf', 'hash-leaf', 5, 5, 1, 1, 33, 44, 1)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{
			Path:             "moved-deep",
			ItemType:         ItemTypeFolder,
			LocalDevice:      11,
			LocalInode:       22,
			LocalHasIdentity: true,
		},
		{
			Path:             "moved-deep/leaf.md",
			ItemType:         ItemTypeFile,
			Hash:             "hash-leaf",
			Size:             5,
			Mtime:            1,
			LocalDevice:      33,
			LocalInode:       44,
			LocalHasIdentity: true,
		},
	}))

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-deep', 'deep', 'folder', '', NULL, NULL),
			('item-leaf', 'deep/leaf.md', 'file', 'hash-leaf', 5, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	indexByKey := make(map[string]int, len(plan.Actions))
	for i := range plan.Actions {
		action := plan.Actions[i]
		assert.False(t, action.Type == ActionFolderCreate && action.CreateSide == createLocal,
			"no local folder may be re-created: %+v", action)
		indexByKey[action.Type.String()+":"+action.Path] = i
	}
	deleteIdx, ok := indexByKey[ActionRemoteDelete.String()+":deep"]
	require.True(t, ok, "source folder must be deleted on OneDrive: %v", indexByKey)
	moveIdx, ok := indexByKey[ActionRemoteMove.String()+":moved-deep/leaf.md"]
	require.True(t, ok, "child must move on OneDrive: %v", indexByKey)
	createIdx, ok := indexByKey[ActionFolderCreate.String()+":moved-deep"]
	require.True(t, ok, "destination folder must be created on OneDrive: %v", indexByKey)
	assert.Equal(t, CreateRemote, plan.Actions[createIdx].CreateSide)
	assert.Contains(t, plan.Deps[deleteIdx], moveIdx, "folder delete must wait for the move out of it")
	assert.Len(t, plan.Actions, 3)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_ExpandsEditEditConflictIntoConcreteActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-conflict', 'conflict.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "conflict.txt",
		ItemType: ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item-conflict",
		Path:     "conflict.txt",
		ItemType: ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)

	planner := newPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		nil,
		bl,
		plannerMountContext{DriveID: driveID},
		SyncBidirectional,
	)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 2)

	assert.Equal(t, ActionConflictCopy, plan.Actions[0].Type)
	assert.Equal(t, "conflict.txt", plan.Actions[0].Path)
	assert.False(t, plan.Actions[0].RequireMissingLocalTarget)

	assert.Equal(t, ActionDownload, plan.Actions[1].Type)
	assert.Equal(t, "conflict.txt", plan.Actions[1].Path)
	assert.True(t, plan.Actions[1].RequireMissingLocalTarget)

	require.Len(t, plan.Deps, 2)
	assert.Equal(t, []int{0}, plan.Deps[1], "download should wait for the conflict copy")
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_ExpandsCreateCreateConflictIntoConcreteActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "new-collision.txt",
		ItemType: ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "remote-new",
		Path:     "new-collision.txt",
		ItemType: ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 2)

	assert.Equal(t, ActionConflictCopy, plan.Actions[0].Type)
	assert.Equal(t, "new-collision.txt", plan.Actions[0].Path)
	assert.False(t, plan.Actions[0].RequireMissingLocalTarget)

	assert.Equal(t, ActionDownload, plan.Actions[1].Type)
	assert.Equal(t, "new-collision.txt", plan.Actions[1].Path)
	assert.True(t, plan.Actions[1].RequireMissingLocalTarget)
	assert.Equal(t, []int{0}, plan.Deps[1], "download should wait for the conflict copy")
}

// Validates: R-2.3.6
func TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('deleted-item', 'edit-delete.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "edit-delete.txt",
		ItemType: ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	plan := planCurrentStateForStore(t, store)
	require.Len(t, plan.Actions, 1)

	action := plan.Actions[0]
	assert.Equal(t, ActionUpload, action.Type)
	assert.Equal(t, "edit-delete.txt", action.Path)
	assert.Empty(t, action.ItemID)
	assert.False(t, action.DriveID.IsZero())
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_RemoteParentDeletePlansDescendantLocalDeleteThroughSQLite(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "Projects", ItemType: ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: ItemTypeFile, Hash: "old-hash", Size: 1, Mtime: 1},
	}))
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStore(t, store)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", ActionLocalDelete)
	childIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", ActionLocalDelete)
	assert.Contains(t, plan.Deps[parentIdx], childIdx)
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "Projects", ItemType: ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: ItemTypeFile, Hash: "local-new", Size: 2, Mtime: 2},
	}))
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStore(t, store)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", ActionFolderCreate)
	uploadIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", ActionUpload)
	assert.Equal(t, CreateRemote, plan.Actions[parentIdx].CreateSide)
	assert.Empty(t, plan.Actions[uploadIdx].ItemID)
	assert.Contains(t, plan.Deps[uploadIdx], parentIdx)
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_DownloadOnlyKeepsParentDeleteWhenEditedChildUploadDeferred(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "Projects", ItemType: ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: ItemTypeFile, Hash: "local-new", Size: 2, Mtime: 2},
	}))
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStoreWithMode(t, store, SyncDownloadOnly)

	requireActionIndex(t, plan.Actions, "Projects", ActionLocalDelete)
	assertNoAction(t, plan.Actions, "Projects", ActionFolderCreate)
	assertNoAction(t, plan.Actions, "Projects/file.txt", ActionUpload)
	assert.Equal(t, 1, plan.DeferredByMode.Uploads)
}

// Validates: R-2.1.3, R-2.3.6
func TestPlannerPlanCurrentState_UploadOnlyPreservesRemoteParentForAdmittedEditedChildUpload(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "Projects", ItemType: ItemTypeFolder},
		{Path: "Projects/file.txt", ItemType: ItemTypeFile, Hash: "local-new", Size: 2, Mtime: 2},
	}))
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveID))

	plan := planCurrentStateForStoreWithMode(t, store, SyncUploadOnly)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", ActionFolderCreate)
	uploadIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", ActionUpload)
	assert.Equal(t, CreateRemote, plan.Actions[parentIdx].CreateSide)
	assert.Empty(t, plan.Actions[uploadIdx].ItemID)
	assert.Contains(t, plan.Deps[uploadIdx], parentIdx)
}

// Validates: R-2.1.3, R-2.3.1
func TestPlannerPlanCurrentState_LocalParentDeleteCreatesParentForChangedRemoteChild(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "Projects/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
	}}))
	require.NoError(t, store.CommitObservation(ctx, []observedItem{
		{
			DriveID:  driveID,
			ItemID:   "folder-projects",
			Path:     "Projects",
			ItemType: ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "file-projects",
			Path:     "Projects/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "remote-new",
			Size:     2,
			Mtime:    2,
			ETag:     "etag-new",
		},
	}, "", driveID))

	plan := planCurrentStateForStore(t, store)

	parentIdx := requireActionIndex(t, plan.Actions, "Projects", ActionFolderCreate)
	downloadIdx := requireActionIndex(t, plan.Actions, "Projects/file.txt", ActionDownload)
	assert.Equal(t, createLocal, plan.Actions[parentIdx].CreateSide)
	assert.Contains(t, plan.Deps[downloadIdx], parentIdx)
}

// Validates: R-2.1.3, R-2.4.1
func TestPlannerPlanCurrentState_BothParentSidesDeletedCleansUpDescendantsThroughSQLite(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	seedFolderCascadeBaseline(t, store)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "Projects/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
	}}))
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveid.New(engineTestDriveID),
		ItemID:   "file-projects",
		Path:     "Projects/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "old-hash",
		Size:     1,
		Mtime:    1,
		ETag:     "etag-old",
	}}, "", driveid.New(engineTestDriveID)))

	plan := planCurrentStateForStore(t, store)

	requireActionIndex(t, plan.Actions, "Projects", ActionCleanup)
	requireActionIndex(t, plan.Actions, "Projects/file.txt", ActionCleanup)
}

// Validates: R-2.1.1, R-2.1.3
func TestPlannerPlanCurrentState_UsesRemoteRowDriveOwnershipForDownloadActions(t *testing.T) {
	t.Parallel()

	sharedDriveID := driveid.New("shared-drive-id")

	plan := planCurrentStateForInputs(
		t,
		[]sQLiteComparisonRow{{
			Path:           "Shared/report.txt",
			RemotePresent:  true,
			ComparisonKind: "remote_only_create",
		}},
		[]sQLiteReconciliationRow{{
			Path:               "Shared/report.txt",
			ComparisonKind:     "remote_only_create",
			ReconciliationKind: strDownload,
		}},
		nil,
		[]remoteStateRow{{
			Path:     "Shared/report.txt",
			ItemID:   "remote-report",
			DriveID:  sharedDriveID,
			ItemType: ItemTypeFile,
			Hash:     "remote-hash",
			Size:     12,
			Mtime:    123,
		}},
		nil,
		NewBaselineForTest(nil),
	)

	require.Len(t, plan.Actions, 1)
	assert.Equal(t, ActionDownload, plan.Actions[0].Type)
	assert.Equal(t, sharedDriveID, plan.Actions[0].DriveID)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_UploadOnlyDefersRemoteWinnerDownloadWithoutConflictCopy(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-conflict', 'conflict.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "conflict.txt",
		ItemType: ItemTypeFile,
		Hash:     "local-new",
		Size:     2,
		Mtime:    2,
	}}))

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item-conflict",
		Path:     "conflict.txt",
		ItemType: ItemTypeFile,
		Hash:     "remote-new",
		Size:     3,
		Mtime:    3,
		ETag:     "etag-remote",
	}}, "", driveID))

	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)

	planner := newPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		nil,
		bl,
		plannerMountContext{DriveID: driveID},
		SyncUploadOnly,
	)
	require.NoError(t, err)

	assert.Empty(t, plan.Actions)
	assert.Equal(t, 1, plan.DeferredByMode.Downloads)
	assert.Equal(t, 0, plan.DeferredByMode.Uploads)
}

func planCurrentStateForStore(t *testing.T, store *SyncStore) *actionPlan {
	t.Helper()

	return planCurrentStateForStoreWithMode(t, store, SyncBidirectional)
}

func planCurrentStateForStoreWithMode(t *testing.T, store *SyncStore, mode SyncMode) *actionPlan {
	t.Helper()

	ctx := t.Context()
	bl, err := store.Load(ctx)
	require.NoError(t, err)

	tx, err := beginPerfTx(ctx, store.rawDB())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	rawLocalRows, err := listLocalStateRows(ctx, tx)
	require.NoError(t, err)

	observationState, err := store.readObservationStateTx(ctx, tx)
	require.NoError(t, err)
	contentDriveID := observationState.ContentDriveID
	if contentDriveID.IsZero() {
		contentDriveID = driveid.New(engineTestDriveID)
	}

	rawRemoteRows, err := queryRemoteStateRowsWithRunner(
		ctx,
		tx,
		`SELECT `+sqlSelectRemoteStateCols+` FROM remote_state`,
		contentDriveID,
	)
	require.NoError(t, err)

	visibleRows, err := replacePlannerVisibleStateTx(ctx, tx, rawLocalRows, rawRemoteRows, contentDriveID)
	require.NoError(t, err)

	comparisons, err := queryComparisonStateWithRunnerForTables(
		ctx,
		tx,
		plannerVisibleLocalStateTable,
		plannerVisibleRemoteStateTable,
	)
	require.NoError(t, err)
	reconciliations, err := queryReconciliationStateWithRunnerForTables(
		ctx,
		tx,
		plannerVisibleLocalStateTable,
		plannerVisibleRemoteStateTable,
	)
	require.NoError(t, err)
	observationIssues, err := queryObservationIssueRowsWithRunner(ctx, tx)
	require.NoError(t, err)

	planner := newPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		visibleRows.Local,
		visibleRows.Remote,
		observationIssues,
		bl,
		plannerMountContext{DriveID: driveid.New(engineTestDriveID)},
		mode,
	)
	require.NoError(t, err)

	return plan
}

func planCurrentStateForInputs(
	t *testing.T,
	comparisons []sQLiteComparisonRow,
	reconciliations []sQLiteReconciliationRow,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
	observationIssues []ObservationIssueRow,
	baseline *Baseline,
) *actionPlan {
	t.Helper()

	planner := newPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		observationIssues,
		baseline,
		plannerMountContext{DriveID: driveid.New(engineTestDriveID)},
		SyncBidirectional,
	)
	require.NoError(t, err)

	return plan
}

func seedFolderCascadeBaseline(t *testing.T, store *SyncStore) {
	t.Helper()

	_, err := store.rawDB().ExecContext(t.Context(), `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime, etag
		)
		VALUES
			('folder-projects', 'Projects', 'folder', '', '', NULL, NULL, NULL, NULL, ''),
			('file-projects', 'Projects/file.txt', 'file', 'old-hash', 'old-hash', 1, 1, 1, 1, 'etag-old')`)
	require.NoError(t, err)
}

func requireActionIndex(t *testing.T, actions []Action, path string, actionType actionType) int {
	t.Helper()

	for i := range actions {
		if actions[i].Path == path && actions[i].Type == actionType {
			return i
		}
	}

	require.Failf(t, "missing action", "no %s action for %s in %#v", actionType.String(), path, actions)
	return -1
}

func assertNoAction(t *testing.T, actions []Action, path string, actionType actionType) {
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

func planForUnavailableLocalReadBoundaryDescendant(t *testing.T) *actionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private', 'Private/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item-private",
		Path:     "Private/sub/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-private",
	}}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Private",
		DriveID:   driveID,
		IssueType: issueLocalReadDenied,
		ScopeKey:  sKPermLocalRead("Private"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableRemoteReadBoundaryDescendant(t *testing.T) *actionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared', 'Shared/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "Shared/sub/file.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
	}}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Shared",
		DriveID:   driveID,
		IssueType: issueRemoteReadDenied,
		ScopeKey:  sKPermRemoteRead("Shared"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableLocalReadBoundaryCleanupCandidate(t *testing.T) *actionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private-dir', 'Private/sub', 'folder', '', '', 0, 0, 1, 1, ''),
		       ('item-private-file', 'Private/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Private",
		DriveID:   driveid.New(engineTestDriveID),
		IssueType: issueLocalReadDenied,
		ScopeKey:  sKPermLocalRead("Private"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableRemoteReadBoundaryCleanupCandidate(t *testing.T) *actionPlan {
	t.Helper()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared-dir', 'Shared/sub', 'folder', '', '', 0, 0, 1, 1, ''),
		       ('item-shared-file', 'Shared/sub/file.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{
			Path:     "Shared/sub",
			ItemType: ItemTypeFolder,
		},
		{
			Path:     "Shared/sub/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
			Size:     10,
			Mtime:    1,
		},
	}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Shared",
		DriveID:   driveID,
		IssueType: issueRemoteReadDenied,
		ScopeKey:  sKPermRemoteRead("Shared"),
	})

	return planCurrentStateForStore(t, store)
}

func planForUnavailableLocalMoveSource(t *testing.T) *actionPlan {
	t.Helper()

	return planCurrentStateForInputs(
		t,
		[]sQLiteComparisonRow{{
			Path:            "docs/source.txt",
			BaselinePresent: true,
			LocalPresent:    true,
			RemotePresent:   true,
			LocalChanged:    true,
			RemoteChanged:   false,
			ComparisonKind:  "local_move_source",
		}},
		[]sQLiteReconciliationRow{{
			Path:               "docs/source.txt",
			ComparisonKind:     "local_move_source",
			ReconciliationKind: strLocalMove,
			LocalMoveTarget:    "docs/dest.txt",
		}},
		[]localStateRow{{
			Path:     "docs/source.txt",
			ItemType: ItemTypeFile,
			Hash:     "local",
		}},
		[]remoteStateRow{{
			Path:    "docs/source.txt",
			ItemID:  "remote-source",
			DriveID: driveid.New(engineTestDriveID),
		}},
		[]ObservationIssueRow{{
			Path:      "docs/source.txt",
			IssueType: issueLocalReadDenied,
		}},
		NewBaselineForTest([]*BaselineEntry{{
			Path:     "docs/source.txt",
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "baseline-source",
			ItemType: ItemTypeFile,
		}}),
	)
}

func planForUnavailableRemoteMoveDestination(t *testing.T) *actionPlan {
	t.Helper()

	return planCurrentStateForInputs(
		t,
		[]sQLiteComparisonRow{{
			Path:            "Shared/dest.txt",
			BaselinePresent: true,
			LocalPresent:    true,
			RemotePresent:   true,
			LocalChanged:    false,
			RemoteChanged:   true,
			ComparisonKind:  "remote_move_dest",
		}},
		[]sQLiteReconciliationRow{{
			Path:               "Shared/dest.txt",
			ComparisonKind:     "remote_move_dest",
			ReconciliationKind: strRemoteMove,
			RemoteMoveSource:   "Shared/source.txt",
		}},
		[]localStateRow{{
			Path:     "Shared/dest.txt",
			ItemType: ItemTypeFile,
			Hash:     "local",
		}},
		[]remoteStateRow{{
			Path:    "Shared/dest.txt",
			ItemID:  "remote-dest",
			DriveID: driveid.New(engineTestDriveID),
		}},
		[]ObservationIssueRow{{
			Path:      "Shared",
			IssueType: issueRemoteReadDenied,
			ScopeKey:  sKPermRemoteRead("Shared"),
		}},
		NewBaselineForTest([]*BaselineEntry{{
			Path:     "Shared/dest.txt",
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "baseline-dest",
			ItemType: ItemTypeFile,
		}}),
	)
}

func assertNoActionForPath(t *testing.T, plan *actionPlan, path string, actionType actionType) {
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

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-danger', 'danger.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-danger')`)
	require.NoError(t, err)

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item-danger",
		Path:     "danger.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-danger",
	}}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "danger.txt",
		DriveID:   driveID,
		IssueType: issueLocalReadDenied,
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "danger.txt", ActionRemoteDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundaryDoesNotDeleteLocalData(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-shared', 'Shared/a.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-shared')`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path:     "Shared/a.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
	}}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Shared",
		DriveID:   driveid.New(engineTestDriveID),
		IssueType: issueRemoteReadDenied,
		ScopeKey:  sKPermRemoteRead("Shared"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Shared/a.txt", ActionLocalDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadBoundaryBlocksRemoteDeletesForDescendants(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES ('item-private', 'Private/a.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-private')`)
	require.NoError(t, err)
	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item-private",
		Path:     "Private/a.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash",
		Size:     10,
		Mtime:    1,
		ETag:     "etag-private",
	}}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Private",
		DriveID:   driveID,
		IssueType: issueLocalReadDenied,
		ScopeKey:  sKPermLocalRead("Private"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Private/a.txt", ActionRemoteDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundaryBlocksLocalDeletesForDescendants(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-team-a', 'Team/a.txt', 'file', 'hash-a', 'hash-a', 10, 10, 1, 1, 'etag-team-a'),
			('item-team-b', 'Team/sub/b.txt', 'file', 'hash-b', 'hash-b', 11, 11, 2, 2, 'etag-team-b')`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{
			Path:     "Team/a.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-a",
			Size:     10,
			Mtime:    1,
		},
		{
			Path:     "Team/sub/b.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-b",
			Size:     11,
			Mtime:    2,
		},
	}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Team",
		DriveID:   driveid.New(engineTestDriveID),
		IssueType: issueRemoteReadDenied,
		ScopeKey:  sKPermRemoteRead("Team"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Team/a.txt", ActionLocalDelete)
	assertNoActionForPath(t, plan, "Team/sub/b.txt", ActionLocalDelete)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_LocalReadBoundarySuppressesRemoteOnlySubtreeActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, store.CommitObservation(ctx, []observedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-private-dir",
			Path:     "Private",
			ItemType: ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "item-private-subdir",
			Path:     "Private/sub",
			ItemType: ItemTypeFolder,
		},
		{
			DriveID:  driveID,
			ItemID:   "item-private-file",
			Path:     "Private/sub/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-private",
			Size:     10,
			Mtime:    1,
			ETag:     "etag-private",
		},
	}, "", driveID))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Private",
		DriveID:   driveID,
		IssueType: issueLocalReadDenied,
		ScopeKey:  sKPermLocalRead("Private"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Private", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Private/sub", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionDownload)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_RemoteReadBoundarySuppressesLocalOnlySubtreeActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{
			Path:     "Shared",
			ItemType: ItemTypeFolder,
		},
		{
			Path:     "Shared/sub",
			ItemType: ItemTypeFolder,
		},
		{
			Path:     "Shared/sub/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-shared",
			Size:     10,
			Mtime:    1,
		},
	}))
	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "Shared",
		DriveID:   driveid.New(engineTestDriveID),
		IssueType: issueRemoteReadDenied,
		ScopeKey:  sKPermRemoteRead("Shared"),
	})

	plan := planCurrentStateForStore(t, store)
	assertNoActionForPath(t, plan, "Shared", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Shared/sub", ActionFolderCreate)
	assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionUpload)
	assert.Empty(t, plan.Actions)
}

// Validates: R-2.1.3, R-2.10.4
func TestPlannerPlanCurrentState_NewUnreadableLocalPathProducesNoActions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "blocked/new.txt",
		DriveID:   driveID,
		IssueType: issueLocalReadDenied,
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
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionRemoteDelete)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionDownload)
		assert.Empty(t, plan.Actions)
	})

	t.Run("remote read boundary keeps local descendants from looking deleted", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteReadBoundaryDescendant(t)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionLocalDelete)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionUpload)
		assert.Empty(t, plan.Actions)
	})

	t.Run("local unreadable subtree suppresses cleanup for descendants", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableLocalReadBoundaryCleanupCandidate(t)
		assertNoActionForPath(t, plan, "Private/sub", ActionCleanup)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionCleanup)
		assertNoActionForPath(t, plan, "Private/sub/file.txt", ActionRemoteDelete)
		assert.Empty(t, plan.Actions)
	})

	t.Run("remote unreadable subtree suppresses cleanup for descendants", func(t *testing.T) {
		t.Parallel()

		plan := planForUnavailableRemoteReadBoundaryCleanupCandidate(t)
		assertNoActionForPath(t, plan, "Shared/sub", ActionCleanup)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionCleanup)
		assertNoActionForPath(t, plan, "Shared/sub/file.txt", ActionLocalDelete)
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

	store := newTestStore(t)
	ctx := t.Context()

	// rm -rf many/ while watch mode runs: every baseline row under many is
	// gone locally and still present remotely.
	_, err := store.rawDB().ExecContext(ctx, `
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

	_, err = store.rawDB().ExecContext(ctx, `
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
		assert.Equal(t, ActionRemoteDelete, plan.Actions[i].Type,
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

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime
		)
		VALUES
			('item-many', 'many', 'folder', '', '', NULL, NULL, NULL, NULL),
			('item-f000', 'many/f000.txt', 'file', 'h0', 'h0', 1, 1, 1, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
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
	assert.Equal(t, createLocal, plan.Actions[folderIdx].CreateSide)
}

func planActionIndex(t *testing.T, plan *actionPlan) map[string]int {
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

	store := newTestStore(t)
	ctx := t.Context()

	// rm lowercase.txt; mkdir lowercase.txt; echo > lowercase.txt/inside.txt
	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-lower', 'lowercase.txt', 'file', 'h', 'h', 1, 1, 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "lowercase.txt", ItemType: ItemTypeFolder},
		{Path: "lowercase.txt/inside.txt", ItemType: ItemTypeFile, Hash: "inside", Size: 1, Mtime: 2},
	}))
	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-lower', 'lowercase.txt', 'file', 'h', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 3, "%v", index)

	deleteIdx, ok := index[ActionRemoteDelete.String()+":lowercase.txt"]
	require.True(t, ok, "%v", index)
	assert.Equal(t, "item-lower", plan.Actions[deleteIdx].ItemID)
	createIdx, ok := index[ActionFolderCreate.String()+":lowercase.txt"]
	require.True(t, ok, "%v", index)
	assert.Equal(t, CreateRemote, plan.Actions[createIdx].CreateSide)
	uploadIdx, ok := index[ActionUpload.String()+":lowercase.txt/inside.txt"]
	require.True(t, ok, "%v", index)

	assert.Contains(t, plan.Deps[createIdx], deleteIdx, "the new folder is created only after the old file is gone")
	assert.Contains(t, plan.Deps[uploadIdx], createIdx)
}

// Validates: R-2.17.4
func TestPlannerPlanCurrentState_LocalFolderReplacedByFileReplacesRemoteItem(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES
			('item-dir', 'thing', 'folder', NULL, NULL, NULL, NULL, NULL, NULL),
			('item-child', 'thing/child.txt', 'file', 'c', 'c', 1, 1, 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "thing", ItemType: ItemTypeFile, Hash: "now-a-file", Size: 3, Mtime: 4},
	}))
	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-dir', 'thing', 'folder', NULL, NULL, NULL),
			('item-child', 'thing/child.txt', 'file', 'c', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 3, "%v", index)

	childDeleteIdx, ok := index[ActionRemoteDelete.String()+":thing/child.txt"]
	require.True(t, ok, "%v", index)
	folderDeleteIdx, ok := index[ActionRemoteDelete.String()+":thing"]
	require.True(t, ok, "%v", index)
	uploadIdx, ok := index[ActionUpload.String()+":thing"]
	require.True(t, ok, "%v", index)
	assert.Empty(t, plan.Actions[uploadIdx].ItemID, "the file is new content, not an overwrite of the folder item")

	assert.Contains(t, plan.Deps[folderDeleteIdx], childDeleteIdx)
	assert.Contains(t, plan.Deps[uploadIdx], folderDeleteIdx)
}

// Validates: R-2.17.4
func TestPlannerPlanCurrentState_RemoteFileReplacedByFolderReplacesLocalItem(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-old-file', 'shape', 'file', 'h', 'h', 1, 1, 1, 1)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "shape", ItemType: ItemTypeFile, Hash: "h", Size: 1, Mtime: 1},
	}))
	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-new-folder', 'shape', 'folder', NULL, NULL, NULL),
			('item-inner', 'shape/inner.txt', 'file', 'i', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 3, "%v", index)

	deleteIdx, ok := index[ActionLocalDelete.String()+":shape"]
	require.True(t, ok, "%v", index)
	createIdx, ok := index[ActionFolderCreate.String()+":shape"]
	require.True(t, ok, "%v", index)
	assert.Equal(t, createLocal, plan.Actions[createIdx].CreateSide)
	downloadIdx, ok := index[ActionDownload.String()+":shape/inner.txt"]
	require.True(t, ok, "%v", index)

	assert.Contains(t, plan.Deps[createIdx], deleteIdx)
	assert.Contains(t, plan.Deps[downloadIdx], createIdx)
}

// Validates: R-2.17.4
func TestPlannerPlanCurrentState_RemoteFolderReplacedByFileReplacesLocalItem(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-old-dir', 'shape', 'folder', NULL, NULL, NULL, NULL, NULL, NULL)`)
	require.NoError(t, err)
	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{
		{Path: "shape", ItemType: ItemTypeFolder},
	}))
	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-new-file', 'shape', 'file', 'f', 1, 1)`)
	require.NoError(t, err)

	plan := planCurrentStateForStore(t, store)
	index := planActionIndex(t, plan)
	require.Len(t, plan.Actions, 2, "%v", index)

	deleteIdx, ok := index[ActionLocalDelete.String()+":shape"]
	require.True(t, ok, "%v", index)
	downloadIdx, ok := index[ActionDownload.String()+":shape"]
	require.True(t, ok, "%v", index)
	assert.True(t, plan.Actions[downloadIdx].RequireMissingLocalTarget)
	assert.Equal(t, "item-new-file", plan.Actions[downloadIdx].ItemID)
	assert.Contains(t, plan.Deps[downloadIdx], deleteIdx)
}
