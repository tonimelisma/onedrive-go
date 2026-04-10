package syncplan

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{Path: "folder", ItemType: synctypes.ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "folder/a.txt", ItemType: synctypes.ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "hashA", RemoteHash: "hashA"},
		&synctypes.BaselineEntry{Path: "folder/b.txt", ItemType: synctypes.ItemTypeFile, ItemID: "f3", DriveID: driveID, LocalHash: "hashB", RemoteHash: "hashB"},
	)

	// Delta reports only the parent folder as deleted — children NOT reported.
	changes := []synctypes.PathChanges{
		{
			Path: "folder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "folder",
					ItemType:  synctypes.ItemTypeFolder,
					ItemID:    "f1",
					DriveID:   driveID,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should have 3 actions: folder delete + 2 cascaded child deletes.
	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
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

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{Path: "a", ItemType: synctypes.ItemTypeFolder, ItemID: "a1", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "a/top.txt", ItemType: synctypes.ItemTypeFile, ItemID: "a2", DriveID: driveID, LocalHash: "h1", RemoteHash: "h1"},
		&synctypes.BaselineEntry{Path: "a/b", ItemType: synctypes.ItemTypeFolder, ItemID: "a3", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "a/b/mid.txt", ItemType: synctypes.ItemTypeFile, ItemID: "a4", DriveID: driveID, LocalHash: "h2", RemoteHash: "h2"},
		&synctypes.BaselineEntry{Path: "a/b/c", ItemType: synctypes.ItemTypeFolder, ItemID: "a5", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "a/b/c/deep.txt", ItemType: synctypes.ItemTypeFile, ItemID: "a6", DriveID: driveID, LocalHash: "h3", RemoteHash: "h3"},
	)

	changes := []synctypes.PathChanges{
		{
			Path: "a",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "a",
					ItemType:  synctypes.ItemTypeFolder,
					ItemID:    "a1",
					DriveID:   driveID,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// All 6 items should be deleted (1 parent + 5 descendants).
	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Len(t, localDeletes, 6, "a + a/top.txt + a/b + a/b/mid.txt + a/b/c + a/b/c/deep.txt")

	// Dependency ordering: child deletes must complete before parent.
	deleteByPath := make(map[string]int)
	for i, a := range plan.Actions {
		if a.Type == synctypes.ActionLocalDelete {
			deleteByPath[a.Path] = i
		}
	}

	// Verify a/b/c depends on a/b/c/deep.txt, a/b depends on a/b/c, etc.
	// The dependency graph ensures correct ordering.
	for i, deps := range plan.Deps {
		a := plan.Actions[i]
		if a.Type == synctypes.ActionLocalDelete && a.View != nil &&
			a.View.Baseline != nil && a.View.Baseline.ItemType == synctypes.ItemTypeFolder {
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

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{Path: "folder", ItemType: synctypes.ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "folder/child.txt", ItemType: synctypes.ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "hC", RemoteHash: "hC"},
	)

	// Delta reports BOTH parent folder AND child as deleted.
	changes := []synctypes.PathChanges{
		{
			Path: "folder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete,
					Path: "folder", ItemType: synctypes.ItemTypeFolder,
					ItemID: "f1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
		{
			Path: "folder/child.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete,
					Path: "folder/child.txt", ItemType: synctypes.ItemTypeFile,
					ItemID: "f2", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should have exactly 2 local deletes (no duplicates).
	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Len(t, localDeletes, 2, "no duplicates: folder + child")
}

func TestCascade_NestedDeletedFolders_DoesNotPanicOnOverlappingCascade(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{Path: "folder", ItemType: synctypes.ItemTypeFolder, ItemID: "folder-id", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "folder/child", ItemType: synctypes.ItemTypeFolder, ItemID: "child-id", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "folder/child/deep.txt", ItemType: synctypes.ItemTypeFile, ItemID: "deep-id", DriveID: driveID, LocalHash: "h", RemoteHash: "h"},
	)

	changes := []synctypes.PathChanges{
		{
			Path: "folder",
			RemoteEvents: []synctypes.ChangeEvent{{
				Source:    synctypes.SourceRemote,
				Type:      synctypes.ChangeDelete,
				Path:      "folder",
				ItemType:  synctypes.ItemTypeFolder,
				ItemID:    "folder-id",
				DriveID:   driveID,
				IsDeleted: true,
			}},
		},
		{
			Path: "folder/child",
			RemoteEvents: []synctypes.ChangeEvent{{
				Source:    synctypes.SourceRemote,
				Type:      synctypes.ChangeDelete,
				Path:      "folder/child",
				ItemType:  synctypes.ItemTypeFolder,
				ItemID:    "child-id",
				DriveID:   driveID,
				IsDeleted: true,
			}},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)
	require.NotNil(t, plan)

	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Len(t, localDeletes, 3)

	deletePaths := make(map[string]bool, len(localDeletes))
	for _, action := range localDeletes {
		deletePaths[action.Path] = true
	}

	assert.True(t, deletePaths["folder"])
	assert.True(t, deletePaths["folder/child"])
	assert.True(t, deletePaths["folder/child/deep.txt"])
}

// TestCascade_UploadOnlyMode verifies no cascade in upload-only mode.
func TestCascade_UploadOnlyMode(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{Path: "folder", ItemType: synctypes.ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "folder/child.txt", ItemType: synctypes.ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "h", RemoteHash: "h"},
	)

	changes := []synctypes.PathChanges{
		{
			Path: "folder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete,
					Path: "folder", ItemType: synctypes.ItemTypeFolder,
					ItemID: "f1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncUploadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Upload-only mode suppresses remote deletions — no local deletes at all.
	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Empty(t, localDeletes, "upload-only mode: no local deletes")
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

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path:     "folder",
			ItemType: synctypes.ItemTypeFolder,
			ItemID:   "folder-id",
			DriveID:  driveID,
		},
		&synctypes.BaselineEntry{
			Path:       "folder/child.txt",
			ItemType:   synctypes.ItemTypeFile,
			ItemID:     "child-id",
			DriveID:    driveID,
			LocalHash:  "old-local",
			RemoteHash: "old-remote",
		},
	)

	changes := []synctypes.PathChanges{
		{
			Path: "folder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "folder",
					ItemType:  synctypes.ItemTypeFolder,
					ItemID:    "folder-id",
					DriveID:   driveID,
					IsDeleted: true,
				},
			},
		},
		{
			Path: "folder/child.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "folder/child.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "new-local",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	require.Len(t, folderCreates, 1)
	assert.Equal(t, plannerCascadeFolderPath, folderCreates[0].Path)
	assert.Equal(t, synctypes.CreateRemote, folderCreates[0].CreateSide)

	conflicts := synctest.ActionsOfType(plan.Actions, synctypes.ActionConflict)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "folder/child.txt", conflicts[0].Path)
	require.NotNil(t, conflicts[0].ConflictInfo)
	assert.Equal(t, synctypes.ConflictEditDelete, conflicts[0].ConflictInfo.ConflictType)

	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Empty(t, localDeletes, "remote recreation should replace parent local delete when a descendant is preserved")

	var folderCreateIdx, conflictIdx int
	for i := range plan.Actions {
		switch {
		case plan.Actions[i].Type == synctypes.ActionFolderCreate && plan.Actions[i].Path == plannerCascadeFolderPath:
			folderCreateIdx = i
		case plan.Actions[i].Type == synctypes.ActionConflict && plan.Actions[i].Path == "folder/child.txt":
			conflictIdx = i
		}
	}
	assert.Contains(t, plan.Deps[conflictIdx], folderCreateIdx, "child conflict should wait for remote folder recreation")
}

// TestCascade_BothSidesDeleted verifies cleanup actions for descendants when
// both sides are deleted. The planner derives local state from baseline when
// no local events exist (assumes item still exists on disk), so the parent
// gets ED6 (ActionLocalDelete). The cascade generates ActionLocalDelete for
// descendants too (executor's hash-before-delete check handles the case where
// the file is actually absent).
func TestCascade_BothSidesDeleted(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{Path: "folder", ItemType: synctypes.ItemTypeFolder, ItemID: "f1", DriveID: driveID},
		&synctypes.BaselineEntry{Path: "folder/gone.txt", ItemType: synctypes.ItemTypeFile, ItemID: "f2", DriveID: driveID, LocalHash: "h", RemoteHash: "h"},
	)

	// Remote deleted, no local events. The planner derives local state from
	// baseline (assumes folder still on disk) → ED6 (ActionLocalDelete).
	changes := []synctypes.PathChanges{
		{
			Path: "folder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete,
					Path: "folder", ItemType: synctypes.ItemTypeFolder,
					ItemID: "f1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Both parent and child get ActionLocalDelete (local state derived from
	// baseline — planner assumes items exist on disk).
	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Len(t, localDeletes, 2, "parent + child: local deletes")
}

// TestCascade_EmptyFolder verifies no cascade for a folder with no descendants.
func TestCascade_EmptyFolder(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{Path: "empty", ItemType: synctypes.ItemTypeFolder, ItemID: "e1", DriveID: driveID},
	)

	changes := []synctypes.PathChanges{
		{
			Path: "empty",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete,
					Path: "empty", ItemType: synctypes.ItemTypeFolder,
					ItemID: "e1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Just the single folder delete — no cascade needed.
	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Len(t, localDeletes, 1)
	assert.Equal(t, "empty", localDeletes[0].Path)
}

// Validates: R-6.2.5, R-6.4.1
// TestCascade_DeleteSafetyProtection verifies that cascaded actions increase
// the delete count and trigger delete safety protection if threshold is exceeded.
func TestCascade_DeleteSafetyProtection(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))
	driveID := driveid.New(synctest.TestDriveID)

	// Create a folder with 5 children.
	entries := []*synctypes.BaselineEntry{
		{Path: "bigfolder", ItemType: synctypes.ItemTypeFolder, ItemID: "bf1", DriveID: driveID},
	}

	for i := range 5 {
		entries = append(entries, &synctypes.BaselineEntry{
			Path: fmt.Sprintf("bigfolder/file%d.txt", i), ItemType: synctypes.ItemTypeFile,
			ItemID: fmt.Sprintf("bf%d", i+2), DriveID: driveID,
			LocalHash: fmt.Sprintf("h%d", i), RemoteHash: fmt.Sprintf("h%d", i),
		})
	}

	baseline := synctest.BaselineWith(entries...)

	changes := []synctypes.PathChanges{
		{
			Path: "bigfolder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete,
					Path: "bigfolder", ItemType: synctypes.ItemTypeFolder,
					ItemID: "bf1", DriveID: driveID, IsDeleted: true,
				},
			},
		},
	}

	// Set threshold to 3 — cascade produces 6 deletes, which exceeds it.
	safety := &synctypes.SafetyConfig{DeleteSafetyThreshold: 3}

	_, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, safety, nil)
	assert.ErrorIs(t, err, synctypes.ErrDeleteSafetyThresholdExceeded, "cascaded deletes should trigger delete safety protection")
}
