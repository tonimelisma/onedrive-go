package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

const plannerCascadeFolderPath = "folder"

// ---------------------------------------------------------------------------
// Folder Delete Cascade Expansion Tests
// ---------------------------------------------------------------------------

// TestCascade_BasicFolderDelete verifies that deleting a folder with 2 child
// files produces 3 total delete actions (folder + 2 children).
func TestCascade_BasicFolderDelete(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "folder", ItemType: ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&BaselineEntry{Path: "folder/a.txt", ItemType: ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "hashA", RemoteHash: "hashA"},
		&BaselineEntry{Path: "folder/b.txt", ItemType: ItemTypeFile, ItemID: "f3", DriveID: driveID, LocalHash: "hashB", RemoteHash: "hashB"},
	)

	// Delta reports only the parent folder as deleted — children NOT reported.
	changes := []PathChanges{
		{
			Path: "folder",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "folder",
					ItemType:  ItemTypeFolder,
					ItemID:    "f1",
					DriveID:   driveID,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should have 3 actions: folder delete + 2 cascaded child deletes.
	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Len(t, localDeletes, 3, "folder + 2 children = 3 local deletes")

	// Verify paths.
	deletePaths := make(map[string]bool)
	for _, a := range localDeletes {
		deletePaths[a.Path] = true
	}

	assert.True(t, deletePaths["folder"])
	assert.True(t, deletePaths["folder/a.txt"])
	assert.True(t, deletePaths["folder/b.txt"])
}

// TestCascade_NestedHierarchy verifies cascade for a deep a/b/c/deep.txt tree.
func TestCascade_NestedHierarchy(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "a", ItemType: ItemTypeFolder, ItemID: "a1", DriveID: driveID},
		&BaselineEntry{Path: "a/top.txt", ItemType: ItemTypeFile, ItemID: "a2", DriveID: driveID, LocalHash: "h1", RemoteHash: "h1"},
		&BaselineEntry{Path: "a/b", ItemType: ItemTypeFolder, ItemID: "a3", DriveID: driveID},
		&BaselineEntry{Path: "a/b/mid.txt", ItemType: ItemTypeFile, ItemID: "a4", DriveID: driveID, LocalHash: "h2", RemoteHash: "h2"},
		&BaselineEntry{Path: "a/b/c", ItemType: ItemTypeFolder, ItemID: "a5", DriveID: driveID},
		&BaselineEntry{Path: "a/b/c/deep.txt", ItemType: ItemTypeFile, ItemID: "a6", DriveID: driveID, LocalHash: "h3", RemoteHash: "h3"},
	)

	changes := []PathChanges{
		{
			Path: "a",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "a",
					ItemType:  ItemTypeFolder,
					ItemID:    "a1",
					DriveID:   driveID,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// All 6 items should be deleted (1 parent + 5 descendants).
	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Len(t, localDeletes, 6, "a + a/top.txt + a/b + a/b/mid.txt + a/b/c + a/b/c/deep.txt")

	// Dependency ordering: child deletes must complete before parent.
	deleteByPath := make(map[string]int)
	for i, a := range plan.Actions {
		if a.Type == ActionLocalDelete {
			deleteByPath[a.Path] = i
		}
	}

	// Verify a/b/c depends on a/b/c/deep.txt, a/b depends on a/b/c, etc.
	// The dependency graph ensures correct ordering.
	for i, deps := range plan.Deps {
		a := plan.Actions[i]
		if a.Type == ActionLocalDelete && a.View != nil &&
			a.View.Baseline != nil && a.View.Baseline.ItemType == ItemTypeFolder {
			// Folder deletes should depend on their children.
			if a.Path == "a" {
				assert.NotEmpty(t, deps, "folder 'a' should have child delete dependencies")
			}
		}
	}
}

// TestCascade_Deduplication verifies no duplicate actions when delta reports
// both parent and child.
func TestCascade_Deduplication(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "folder", ItemType: ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&BaselineEntry{Path: "folder/child.txt", ItemType: ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "hC", RemoteHash: "hC"},
	)

	// Delta reports BOTH parent folder AND child as deleted.
	changes := []PathChanges{
		{
			Path: "folder",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete,
					Path: "folder", ItemType: ItemTypeFolder,
					ItemID: "f1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
		{
			Path: "folder/child.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete,
					Path: "folder/child.txt", ItemType: ItemTypeFile,
					ItemID: "f2", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should have exactly 2 local deletes (no duplicates).
	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Len(t, localDeletes, 2, "no duplicates: folder + child")
}

func TestCascade_NestedDeletedFolders_DoesNotPanicOnOverlappingCascade(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "folder", ItemType: ItemTypeFolder, ItemID: "folder-id", DriveID: driveID},
		&BaselineEntry{Path: "folder/child", ItemType: ItemTypeFolder, ItemID: "child-id", DriveID: driveID},
		&BaselineEntry{Path: "folder/child/deep.txt", ItemType: ItemTypeFile, ItemID: "deep-id", DriveID: driveID, LocalHash: "h", RemoteHash: "h"},
	)

	changes := []PathChanges{
		{
			Path: "folder",
			RemoteEvents: []ChangeEvent{{
				Source:    SourceRemote,
				Type:      ChangeDelete,
				Path:      "folder",
				ItemType:  ItemTypeFolder,
				ItemID:    "folder-id",
				DriveID:   driveID,
				IsDeleted: true,
			}},
		},
		{
			Path: "folder/child",
			RemoteEvents: []ChangeEvent{{
				Source:    SourceRemote,
				Type:      ChangeDelete,
				Path:      "folder/child",
				ItemType:  ItemTypeFolder,
				ItemID:    "child-id",
				DriveID:   driveID,
				IsDeleted: true,
			}},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)
	require.NotNil(t, plan)

	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Len(t, localDeletes, 3)

	deletePaths := make(map[string]bool, len(localDeletes))
	for _, action := range localDeletes {
		deletePaths[action.Path] = true
	}

	assert.True(t, deletePaths["folder"])
	assert.True(t, deletePaths["folder/child"])
	assert.True(t, deletePaths["folder/child/deep.txt"])
}

// TestCascade_UploadOnlyMode verifies that upload-only still suppresses
// remote-originated folder deletes.
func TestCascade_UploadOnlyMode(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "folder", ItemType: ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&BaselineEntry{Path: "folder/child.txt", ItemType: ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "h", RemoteHash: "h"},
	)

	changes := []PathChanges{
		{
			Path: "folder",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete,
					Path: "folder", ItemType: ItemTypeFolder,
					ItemID: "f1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Upload-only mode suppresses remote deletions — no local deletes at all.
	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Empty(t, localDeletes, "upload-only mode: no local deletes")
}

func TestCascade_UploadOnlyPropagatesLocalFolderDeleteToRemoteDescendants(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "folder", ItemType: ItemTypeFolder, ItemID: "folder-id", DriveID: driveID},
		&BaselineEntry{Path: "folder/child.txt", ItemType: ItemTypeFile, ItemID: "child-id", DriveID: driveID, LocalHash: "h", RemoteHash: "h"},
	)

	changes := []PathChanges{
		{
			Path: "folder",
			LocalEvents: []ChangeEvent{{
				Source:    SourceLocal,
				Type:      ChangeDelete,
				Path:      "folder",
				ItemType:  ItemTypeFolder,
				IsDeleted: true,
			}},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	remoteDeletes := actionsOfType(plan.Actions, ActionRemoteDelete)
	require.Len(t, remoteDeletes, 2, "upload-only folder delete should cascade to remote descendants")

	deletePaths := make(map[string]bool, len(remoteDeletes))
	for _, action := range remoteDeletes {
		deletePaths[action.Path] = true
	}

	assert.True(t, deletePaths["folder"])
	assert.True(t, deletePaths["folder/child.txt"])
}

// TestCascade_RemoteDeletedFolderWithModifiedDescendant_ReclassifiesDescendant
// verifies that a parent-folder delete merged in via cascade can still
// reclassify an already-planned descendant local change as a remote delete,
// while keeping the parent folder alive remotely when the descendant must be
// preserved.
func TestCascade_RemoteDeletedFolderWithModifiedDescendant_ReclassifiesDescendant(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{
			Path:     "folder",
			ItemType: ItemTypeFolder,
			ItemID:   "folder-id",
			DriveID:  driveID,
		},
		&BaselineEntry{
			Path:       "folder/child.txt",
			ItemType:   ItemTypeFile,
			ItemID:     "child-id",
			DriveID:    driveID,
			LocalHash:  "old-local",
			RemoteHash: "old-remote",
		},
	)

	changes := []PathChanges{
		{
			Path: "folder",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "folder",
					ItemType:  ItemTypeFolder,
					ItemID:    "folder-id",
					DriveID:   driveID,
					IsDeleted: true,
				},
			},
		},
		{
			Path: "folder/child.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "folder/child.txt",
					ItemType: ItemTypeFile,
					Hash:     "new-local",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	folderCreates := actionsOfType(plan.Actions, ActionFolderCreate)
	require.Len(t, folderCreates, 1)
	assert.Equal(t, plannerCascadeFolderPath, folderCreates[0].Path)
	assert.Equal(t, CreateRemote, folderCreates[0].CreateSide)

	conflicts := actionsOfType(plan.Actions, ActionConflict)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "folder/child.txt", conflicts[0].Path)
	require.NotNil(t, conflicts[0].ConflictInfo)
	assert.Equal(t, ConflictEditDelete, conflicts[0].ConflictInfo.ConflictType)

	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Empty(t, localDeletes, "remote recreation should replace parent local delete when a descendant is preserved")

	var folderCreateIdx, conflictIdx int
	for i := range plan.Actions {
		switch {
		case plan.Actions[i].Type == ActionFolderCreate && plan.Actions[i].Path == plannerCascadeFolderPath:
			folderCreateIdx = i
		case plan.Actions[i].Type == ActionConflict && plan.Actions[i].Path == "folder/child.txt":
			conflictIdx = i
		}
	}
	assert.Contains(t, plan.Deps[conflictIdx], folderCreateIdx, "child conflict should wait for remote folder recreation")
}

func TestCascade_LocallyDeletedFolderWithChangedRemoteDescendant_ReclassifiesDescendant(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{
			Path:     "folder",
			ItemType: ItemTypeFolder,
			ItemID:   "folder-id",
			DriveID:  driveID,
		},
		&BaselineEntry{
			Path:       "folder/child.txt",
			ItemType:   ItemTypeFile,
			ItemID:     "child-id",
			DriveID:    driveID,
			LocalHash:  "old-local",
			RemoteHash: "old-remote",
		},
	)

	changes := []PathChanges{
		{
			Path: "folder",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "folder",
					ItemType:  ItemTypeFolder,
					IsDeleted: true,
				},
			},
		},
		{
			Path: "folder/child.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "folder/child.txt",
					ItemType: ItemTypeFile,
					ItemID:   "child-id",
					DriveID:  driveID,
					Hash:     "new-remote",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	folderCreates := actionsOfType(plan.Actions, ActionFolderCreate)
	require.Len(t, folderCreates, 1)
	assert.Equal(t, plannerCascadeFolderPath, folderCreates[0].Path)
	assert.Equal(t, CreateLocal, folderCreates[0].CreateSide)

	downloads := actionsOfType(plan.Actions, ActionDownload)
	require.Len(t, downloads, 1)
	assert.Equal(t, "folder/child.txt", downloads[0].Path)

	remoteDeletes := actionsOfType(plan.Actions, ActionRemoteDelete)
	assert.Empty(t, remoteDeletes, "local recreation should replace parent remote delete when a descendant is preserved")
}

func TestCascade_RemotelyDeletedFolderWithNewLocalDescendant_RecreatesParentRemotely(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{
			Path:     plannerCascadeFolderPath,
			ItemType: ItemTypeFolder,
			ItemID:   "folder-id",
			DriveID:  driveID,
		},
	)

	changes := []PathChanges{
		{
			Path: plannerCascadeFolderPath,
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      plannerCascadeFolderPath,
					ItemType:  ItemTypeFolder,
					ItemID:    "folder-id",
					DriveID:   driveID,
					IsDeleted: true,
				},
			},
		},
		{
			Path: "folder/local-only",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "folder/local-only",
					ItemType: ItemTypeFolder,
				},
			},
		},
		{
			Path: "folder/local-only/stuff.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "folder/local-only/stuff.txt",
					ItemType: ItemTypeFile,
					Hash:     "local-hash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	folderCreates := actionsOfType(plan.Actions, ActionFolderCreate)
	require.Len(t, folderCreates, 2)

	folderCreateByPath := make(map[string]Action, len(folderCreates))
	for i := range folderCreates {
		folderCreateByPath[folderCreates[i].Path] = folderCreates[i]
	}

	parentCreate, ok := folderCreateByPath[plannerCascadeFolderPath]
	require.True(t, ok, "deleted parent should be recreated remotely when new local descendants need it")
	assert.Equal(t, CreateRemote, parentCreate.CreateSide)

	childCreate, ok := folderCreateByPath["folder/local-only"]
	require.True(t, ok, "new local child folder should still upload")
	assert.Equal(t, CreateRemote, childCreate.CreateSide)

	uploads := actionsOfType(plan.Actions, ActionUpload)
	require.Len(t, uploads, 1)
	assert.Equal(t, "folder/local-only/stuff.txt", uploads[0].Path)

	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Empty(t, localDeletes, "preserved new local subtree should replace parent local delete")

	var parentCreateIdx, childCreateIdx, uploadIdx int
	for i := range plan.Actions {
		switch {
		case plan.Actions[i].Type == ActionFolderCreate && plan.Actions[i].Path == plannerCascadeFolderPath:
			parentCreateIdx = i
		case plan.Actions[i].Type == ActionFolderCreate && plan.Actions[i].Path == "folder/local-only":
			childCreateIdx = i
		case plan.Actions[i].Type == ActionUpload && plan.Actions[i].Path == "folder/local-only/stuff.txt":
			uploadIdx = i
		}
	}

	assert.Contains(t, plan.Deps[childCreateIdx], parentCreateIdx, "child folder create should wait for parent recreation")
	assert.Contains(t, plan.Deps[uploadIdx], childCreateIdx, "upload should wait for child folder create")
}

// TestCascade_BothSidesDeleted verifies cleanup actions for descendants when
// both sides are deleted.
func TestCascade_BothSidesDeleted(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "folder", ItemType: ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&BaselineEntry{Path: "folder/gone.txt", ItemType: ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "h", RemoteHash: "h"},
	)

	changes := []PathChanges{
		{
			Path: "folder",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "folder",
					ItemType:  ItemTypeFolder,
					IsDeleted: true,
				},
			},
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete,
					Path: "folder", ItemType: ItemTypeFolder,
					ItemID: "f1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	cleanups := actionsOfType(plan.Actions, ActionCleanup)
	assert.Len(t, cleanups, 2, "parent + child: cleanup actions")
	assert.Empty(t, actionsOfType(plan.Actions, ActionLocalDelete))
}

// TestCascade_EmptyFolder verifies no cascade for a folder with no descendants.
func TestCascade_EmptyFolder(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := baselineWith(
		&BaselineEntry{Path: "empty", ItemType: ItemTypeFolder, ItemID: "e1", DriveID: driveID},
	)

	changes := []PathChanges{
		{
			Path: "empty",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete,
					Path: "empty", ItemType: ItemTypeFolder,
					ItemID: "e1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Just the single folder delete — no cascade needed.
	localDeletes := actionsOfType(plan.Actions, ActionLocalDelete)
	assert.Len(t, localDeletes, 1)
	assert.Equal(t, "empty", localDeletes[0].Path)
}
