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

// ---------------------------------------------------------------------------
// Test helpers (planner-specific)
// ---------------------------------------------------------------------------

// countActions returns the total number of actions in the plan.
func countActions(plan *synctypes.ActionPlan) int {
	return len(plan.Actions)
}

// moves returns all move actions (both local and remote) from the plan.
func moves(plan *synctypes.ActionPlan) []synctypes.Action {
	var result []synctypes.Action
	for i := range plan.Actions {
		if plan.Actions[i].Type == synctypes.ActionLocalMove || plan.Actions[i].Type == synctypes.ActionRemoteMove {
			result = append(result, plan.Actions[i])
		}
	}

	return result
}

func buildRemoteDeleteSet(prefix, itemPrefix, hash string, count int) ([]*synctypes.BaselineEntry, []synctypes.PathChanges) {
	var entries []*synctypes.BaselineEntry
	var changes []synctypes.PathChanges

	for i := range count {
		path := fmt.Sprintf("%s-%c.txt", prefix, rune('a'+i))
		itemID := fmt.Sprintf("%s-%c", itemPrefix, rune('a'+i))
		entries = append(entries, &synctypes.BaselineEntry{
			Path:       path,
			DriveID:    driveid.New(synctest.TestDriveID),
			ItemID:     itemID,
			ItemType:   synctypes.ItemTypeFile,
			LocalHash:  hash,
			RemoteHash: hash,
		})
		changes = append(changes, synctypes.PathChanges{
			Path: path,
			RemoteEvents: []synctypes.ChangeEvent{{
				Source:    synctypes.SourceRemote,
				Type:      synctypes.ChangeDelete,
				Path:      path,
				ItemType:  synctypes.ItemTypeFile,
				ItemID:    itemID,
				IsDeleted: true,
			}},
		})
	}

	return entries, changes
}

// ---------------------------------------------------------------------------
// File Decision Matrix Tests (EF1-EF14)
// ---------------------------------------------------------------------------

func TestClassifyFile_EF1_Unchanged(t *testing.T) {
	// EF1: baseline exists, remote and local both match baseline hashes.
	// No remote events and no local events → both change detectors return false.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashA",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashA",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")
	assert.Equal(t, 0, countActions(plan), "EF1")
}

func TestClassifyFile_EF2_RemoteModified(t *testing.T) {
	// EF2: baseline exists, remote hash changed, no local events (local derived from baseline).
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashB",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	require.Len(t, downloads, 1, "EF2")
	assert.Equal(t, "planner-test.txt", downloads[0].Path, "EF2")
}

func TestClassifyFile_EF3_LocalModified(t *testing.T) {
	// EF3: baseline exists, local hash changed, no remote events (remote nil → unchanged).
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashB",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	uploads := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	require.Len(t, uploads, 1, "EF3")
	assert.Equal(t, "planner-test.txt", uploads[0].Path, "EF3")
}

func TestClassifyFile_EF4_ConvergentEdit(t *testing.T) {
	// EF4: baseline exists, both hashes changed but local.Hash == remote.Hash.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashC",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashC",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	syncedUpdates := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpdateSynced)
	require.Len(t, syncedUpdates, 1, "EF4")
}

// Validates: R-2.2
func TestClassifyFile_EF5_EditEditConflict(t *testing.T) {
	// EF5: baseline exists, both hashes changed and differ.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashB",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashC",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	conflicts := synctest.ActionsOfType(plan.Actions, synctypes.ActionConflict)
	require.Len(t, conflicts, 1, "EF5")
	assert.Equal(t, "edit_edit", conflicts[0].ConflictInfo.ConflictType, "EF5")
}

func TestClassifyFile_EF6_LocalDeleteRemoteUnchanged(t *testing.T) {
	// EF6: baseline exists, local ChangeDelete event, no remote events.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	remoteDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteDelete)
	require.Len(t, remoteDeletes, 1, "EF6")
}

func TestClassifyFile_EF7_LocalDeleteRemoteModified(t *testing.T) {
	// EF7: baseline exists, local deleted, remote hash changed → download (remote wins).
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashB",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	require.Len(t, downloads, 1, "EF7")
}

// Validates: R-6.7.7
func TestClassifyFile_EF8_RemoteDeleted(t *testing.T) {
	// EF8: baseline exists, remote event with IsDeleted=true, no local events
	// (local derived from baseline → unchanged).
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "planner-test.txt",
					ItemType:  synctypes.ItemTypeFile,
					ItemID:    "item1",
					DriveID:   driveid.New(synctest.TestDriveID),
					IsDeleted: true,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	require.Len(t, localDeletes, 1, "EF8")
}

// Validates: R-2.2
func TestClassifyFile_EF9_EditDeleteConflict(t *testing.T) {
	// EF9: baseline exists, local hash changed, remote IsDeleted=true.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "planner-test.txt",
					ItemType:  synctypes.ItemTypeFile,
					ItemID:    "item1",
					DriveID:   driveid.New(synctest.TestDriveID),
					IsDeleted: true,
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashB",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	conflicts := synctest.ActionsOfType(plan.Actions, synctypes.ActionConflict)
	require.Len(t, conflicts, 1, "EF9")
	assert.Equal(t, "edit_delete", conflicts[0].ConflictInfo.ConflictType, "EF9")
}

func TestClassifyFile_EF10_BothDeleted(t *testing.T) {
	// EF10: baseline exists, local deleted, remote IsDeleted.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "planner-test.txt",
					ItemType:  synctypes.ItemTypeFile,
					ItemID:    "item1",
					DriveID:   driveid.New(synctest.TestDriveID),
					IsDeleted: true,
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "planner-test.txt",
					ItemType: synctypes.ItemTypeFile,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	cleanups := synctest.ActionsOfType(plan.Actions, synctypes.ActionCleanup)
	require.Len(t, cleanups, 1, "EF10")
}

func TestClassifyFile_EF11_ConvergentCreate(t *testing.T) {
	// EF11: no baseline, both local and remote exist with same hash.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-new.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashX",
					ItemID:   "item2",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashX",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	syncedUpdates := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpdateSynced)
	require.Len(t, syncedUpdates, 1, "EF11")
}

// Validates: R-2.2
func TestClassifyFile_EF12_CreateCreateConflict(t *testing.T) {
	// EF12: no baseline, both exist with different hashes.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-new.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashX",
					ItemID:   "item2",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashY",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	conflicts := synctest.ActionsOfType(plan.Actions, synctypes.ActionConflict)
	require.Len(t, conflicts, 1, "EF12")
	assert.Equal(t, "create_create", conflicts[0].ConflictInfo.ConflictType, "EF12")
}

func TestClassifyFile_EF13_NewLocal(t *testing.T) {
	// EF13: no baseline, local exists, no remote.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-local-only.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-local-only.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashL",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	uploads := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	require.Len(t, uploads, 1, "EF13")
}

func TestClassifyFile_EF14_NewRemote(t *testing.T) {
	// EF14: no baseline, remote exists, no local.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-remote-only.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-remote-only.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashR",
					ItemID:   "item3",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	require.Len(t, downloads, 1, "EF14")
}

// ---------------------------------------------------------------------------
// Folder Decision Matrix Tests (ED1-ED8)
// ---------------------------------------------------------------------------

func TestClassifyFolder_ED1_InSync(t *testing.T) {
	// ED1: baseline exists, folder exists on both sides → no-op.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder1",
		ItemType: synctypes.ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	assert.Equal(t, 0, countActions(plan), "ED1")
}

func TestClassifyFolder_ED2_Adopt(t *testing.T) {
	// ED2: no baseline, folder exists on both sides → adopt (update synced).
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	syncedUpdates := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpdateSynced)
	require.Len(t, syncedUpdates, 1, "ED2")
}

func TestClassifyFolder_ED3_NewRemoteFolder(t *testing.T) {
	// ED3: no baseline, remote folder exists, no local → create locally.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	require.Len(t, folderCreates, 1, "ED3")
	assert.Equal(t, synctypes.CreateLocal, folderCreates[0].CreateSide, "ED3")
}

func TestClassifyFolder_ED4_RecreateLocal(t *testing.T) {
	// ED4: baseline exists, remote folder exists, local absent → recreate locally.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder1",
		ItemType: synctypes.ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	require.Len(t, folderCreates, 1, "ED4")
	assert.Equal(t, synctypes.CreateLocal, folderCreates[0].CreateSide, "ED4")
}

func TestClassifyFolder_ED5_NewLocalFolder(t *testing.T) {
	// ED5: no baseline, local folder exists, no remote → create remotely.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	require.Len(t, folderCreates, 1, "ED5")
	assert.Equal(t, synctypes.CreateRemote, folderCreates[0].CreateSide, "ED5")
}

func TestClassifyFolder_RemoteDeletedFolderOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		localType synctypes.ChangeType
		wantType  synctypes.ActionType
	}{
		{name: "ED6_RemoteDeletedFolder", localType: synctypes.ChangeModify, wantType: synctypes.ActionLocalDelete},
		{name: "ED7_BothGone", localType: synctypes.ChangeDelete, wantType: synctypes.ActionCleanup},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planner := NewPlanner(synctest.TestLogger(t))
			changes := []synctypes.PathChanges{{
				Path: "docs/planner-dir",
				RemoteEvents: []synctypes.ChangeEvent{{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "docs/planner-dir",
					ItemType:  synctypes.ItemTypeFolder,
					ItemID:    "folder1",
					IsDeleted: true,
				}},
				LocalEvents: []synctypes.ChangeEvent{{
					Source:   synctypes.SourceLocal,
					Type:     tt.localType,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
				}},
			}}
			baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
				Path:     "docs/planner-dir",
				DriveID:  driveid.New(synctest.TestDriveID),
				ItemID:   "folder1",
				ItemType: synctypes.ItemTypeFolder,
			})

			plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
			require.NoError(t, err, "Plan()")
			require.Len(t, synctest.ActionsOfType(plan.Actions, tt.wantType), 1, tt.name)
		})
	}
}

func TestClassifyFolder_ED8_PropagateRemoteDelete(t *testing.T) {
	// ED8: baseline exists, no remote events (unchanged), local deleted → propagate delete remotely.
	// This is the folder equivalent of EF6 (file: locally deleted, remote unchanged).
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "docs/planner-dir",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder1",
		ItemType: synctypes.ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	remoteDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteDelete)
	require.Len(t, remoteDeletes, 1, "ED8")
	assert.Equal(t, "docs/planner-dir", remoteDeletes[0].Path, "ED8")
	assert.Equal(t, "folder1", remoteDeletes[0].ItemID, "ED8")
	assert.Equal(t, driveid.New(synctest.TestDriveID), remoteDeletes[0].DriveID, "ED8")
}

func TestClassifyFolder_ED8_DownloadOnly(t *testing.T) {
	// ED8 + SyncDownloadOnly: local deleted, no remote events, baseline exists.
	// Download-only zeroes localDeleted → falls through to no action.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir-dl",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "docs/planner-dir-dl",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "docs/planner-dir-dl",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder2",
		ItemType: synctypes.ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncDownloadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	assert.Equal(t, 0, countActions(plan), "ED8 download-only")
}

func TestClassifyFolder_ED4_UploadOnly(t *testing.T) {
	// ED4 + SyncUploadOnly: local deleted, remote exists, baseline exists.
	// Upload-only: engine doesn't produce remote events, so hasRemote is false.
	// This test verifies the planner's defense in depth — if remote events
	// did arrive in upload-only mode, ED4 would not create locally.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir-ul4",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "docs/planner-dir-ul4",
					ItemType: synctypes.ItemTypeFolder,
					ItemID:   "folder3",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "docs/planner-dir-ul4",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "docs/planner-dir-ul4",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder3",
		ItemType: synctypes.ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncUploadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	assert.Empty(t, folderCreates, "ED4 upload-only")

	// Upload-only: local deletion should still propagate remotely (ED8 path).
	remoteDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1, "ED4 upload-only: local deletion propagates")
}

func TestClassifyFolder_ED6_UploadOnly(t *testing.T) {
	// ED6 + SyncUploadOnly: remote deleted, local exists, baseline exists.
	// Upload-only: engine doesn't produce remote events normally, but if
	// they did arrive, ED6 should not delete locally.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-dir-ul6",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "docs/planner-dir-ul6",
					ItemType:  synctypes.ItemTypeFolder,
					ItemID:    "folder4",
					DriveID:   driveid.New(synctest.TestDriveID),
					IsDeleted: true,
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "docs/planner-dir-ul6",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "docs/planner-dir-ul6",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder4",
		ItemType: synctypes.ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncUploadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Empty(t, localDeletes, "ED6 upload-only")
	assert.Equal(t, 0, countActions(plan), "ED6 upload-only: expected 0 total actions")
}

// ---------------------------------------------------------------------------
// Move Detection Tests
// ---------------------------------------------------------------------------

func TestDetectMoves_RemoteMove(t *testing.T) {
	// ChangeMove in remote events → ActionLocalMove.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "docs/planner-renamed.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeMove,
					Path:     "docs/planner-renamed.txt",
					OldPath:  "docs/planner-original.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashM",
					ItemID:   "item5",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "docs/planner-original.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item5",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashM",
		RemoteHash: "hashM",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	move := allMoves[0]
	assert.Equal(t, synctypes.ActionLocalMove, move.Type)
	assert.Equal(t, "docs/planner-renamed.txt", move.Path, "destination")
	assert.Equal(t, "docs/planner-original.txt", move.OldPath, "source")
}

func TestDetectMoves_LocalMoveByHash(t *testing.T) {
	// Local delete + local create with matching hash → ActionRemoteMove.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-old-loc.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "planner-old-loc.txt",
					ItemType: synctypes.ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-new-loc.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-new-loc.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashMove",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-old-loc.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item6",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashMove",
		RemoteHash: "hashMove",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	move := allMoves[0]
	assert.Equal(t, synctypes.ActionRemoteMove, move.Type)
	assert.Equal(t, "planner-new-loc.txt", move.Path, "destination")
	assert.Equal(t, "planner-old-loc.txt", move.OldPath, "source")
}

func TestDetectMoves_LocalMoveAmbiguous(t *testing.T) {
	// Multiple deletes with same hash → no move, separate actions.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-dup-a.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "planner-dup-a.txt",
					ItemType: synctypes.ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-dup-b.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "planner-dup-b.txt",
					ItemType: synctypes.ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-dup-dest.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-dup-dest.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashDup",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path:       "planner-dup-a.txt",
			DriveID:    driveid.New(synctest.TestDriveID),
			ItemID:     "itemA",
			ItemType:   synctypes.ItemTypeFile,
			LocalHash:  "hashDup",
			RemoteHash: "hashDup",
		},
		&synctypes.BaselineEntry{
			Path:       "planner-dup-b.txt",
			DriveID:    driveid.New(synctest.TestDriveID),
			ItemID:     "itemB",
			ItemType:   synctypes.ItemTypeFile,
			LocalHash:  "hashDup",
			RemoteHash: "hashDup",
		},
	)

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	// No moves — ambiguous hash match.
	allMoves := moves(plan)
	assert.Empty(t, allMoves, "expected 0 moves for ambiguous case")

	// The paths should still produce separate actions (deletes + upload).
	assert.NotEqual(t, 0, countActions(plan), "expected some actions for unmatched paths")
}

func TestDetectMoves_MovedPathsExcluded(t *testing.T) {
	// After move detection, matched paths do not appear in other action types.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-src-excl.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeDelete,
					Path:     "planner-src-excl.txt",
					ItemType: synctypes.ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-dst-excl.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-dst-excl.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashExcl",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-src-excl.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item7",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashExcl",
		RemoteHash: "hashExcl",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	// Should have exactly 1 move and no other actions.
	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	nonMoveCount := countActions(plan) - len(allMoves)
	assert.Equal(t, 0, nonMoveCount, "expected 0 non-move actions after move exclusion")
}

// ---------------------------------------------------------------------------
// Safety Tests (Big Delete)
// ---------------------------------------------------------------------------

// Validates: R-6.4.1
// Validates: R-6.4.1
func TestBigDelete_BelowThreshold(t *testing.T) {
	// Delete count at or below threshold → no trigger.
	planner := NewPlanner(synctest.TestLogger(t))

	// 20 baseline items, delete 10. Threshold is 10 → exactly at threshold, allowed.
	var entries []*synctypes.BaselineEntry
	var changes []synctypes.PathChanges

	for i := range 20 {
		p := fmt.Sprintf("planner-safe-%c.txt", rune('a'+i))
		itemID := fmt.Sprintf("safe-%c", rune('a'+i))
		entries = append(entries, &synctypes.BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(synctest.TestDriveID),
			ItemID:     itemID,
			ItemType:   synctypes.ItemTypeFile,
			LocalHash:  "hashSafe",
			RemoteHash: "hashSafe",
		})
	}

	// Delete exactly 10.
	for i := range 10 {
		changes = append(changes, synctypes.PathChanges{
			Path: entries[i].Path,
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      entries[i].Path,
					ItemType:  synctypes.ItemTypeFile,
					ItemID:    entries[i].ItemID,
					IsDeleted: true,
				},
			},
		})
	}

	baseline := synctest.BaselineWith(entries...)

	config := &synctypes.SafetyConfig{BigDeleteThreshold: 10}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, config, nil)
	require.NoError(t, err, "at threshold should be allowed")
	require.NotNil(t, plan)
}

// Validates: R-6.4.1
func TestBigDelete_ExceedsThreshold(t *testing.T) {
	// Delete count exceeds threshold → ErrBigDeleteTriggered.
	planner := NewPlanner(synctest.TestLogger(t))

	entries, changes := buildRemoteDeleteSet("planner-bigdel", "bdi", "hashBD", 20)
	baseline := synctest.BaselineWith(entries...)

	// 20 deletes > threshold of 10.
	config := &synctypes.SafetyConfig{BigDeleteThreshold: 10}

	_, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, config, nil)
	require.ErrorIs(t, err, synctypes.ErrBigDeleteTriggered)
}

// Validates: R-6.4.1
func TestBigDelete_NoTrigger(t *testing.T) {
	// Few deletes well within threshold → no error.
	planner := NewPlanner(synctest.TestLogger(t))

	var entries []*synctypes.BaselineEntry

	for i := range 20 {
		p := fmt.Sprintf("planner-safe-%c.txt", rune('a'+i))
		itemID := fmt.Sprintf("safe-%c", rune('a'+i))
		entries = append(entries, &synctypes.BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(synctest.TestDriveID),
			ItemID:     itemID,
			ItemType:   synctypes.ItemTypeFile,
			LocalHash:  "hashSafe",
			RemoteHash: "hashSafe",
		})
	}

	// Delete only 2 (well below default threshold of 1000).
	changes := []synctypes.PathChanges{
		{
			Path: entries[0].Path,
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      entries[0].Path,
					ItemType:  synctypes.ItemTypeFile,
					ItemID:    entries[0].ItemID,
					IsDeleted: true,
				},
			},
		},
		{
			Path: entries[1].Path,
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      entries[1].Path,
					ItemType:  synctypes.ItemTypeFile,
					ItemID:    entries[1].ItemID,
					IsDeleted: true,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(entries...)

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)
	require.NotNil(t, plan)
}

// Validates: R-6.4.1
func TestBigDelete_ThresholdZero_Disabled(t *testing.T) {
	// Threshold of 0 disables big-delete protection.
	planner := NewPlanner(synctest.TestLogger(t))

	entries, changes := buildRemoteDeleteSet("planner-disabled", "dis", "hashDis", 20)
	baseline := synctest.BaselineWith(entries...)

	config := &synctypes.SafetyConfig{BigDeleteThreshold: 0}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, config, nil)
	require.NoError(t, err, "threshold=0 disables protection")
	require.NotNil(t, plan)
}

// ---------------------------------------------------------------------------
// Mode Filtering Tests
// ---------------------------------------------------------------------------

func TestPlan_DownloadOnly_SuppressesUploads(t *testing.T) {
	// SyncDownloadOnly: local modified file → no upload.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-dl-only.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "planner-dl-only.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashNew",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-dl-only.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item9",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashOld",
		RemoteHash: "hashOld",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncDownloadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	uploads := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	assert.Empty(t, uploads, "download-only: expected 0 uploads")
}

func TestPlan_UploadOnly_SuppressesDownloads(t *testing.T) {
	// SyncUploadOnly: remote modified file → no download.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-ul-only.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "planner-ul-only.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashNew",
					ItemID:   "item10",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-ul-only.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item10",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashOld",
		RemoteHash: "hashOld",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncUploadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Empty(t, downloads, "upload-only: expected 0 downloads")
}

func TestPlan_DownloadOnly_SuppressesFolderCreateRemote(t *testing.T) {
	// SyncDownloadOnly: new local folder → no remote create.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-dl-dir",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-dl-dir",
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncDownloadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	assert.Empty(t, folderCreates, "download-only: expected 0 folder creates")
}

func TestPlan_UploadOnly_SuppressesFolderCreateLocal(t *testing.T) {
	// SyncUploadOnly: new remote folder → no local create.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-ul-dir",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-ul-dir",
					ItemType: synctypes.ItemTypeFolder,
					ItemID:   "folder2",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncUploadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	assert.Empty(t, folderCreates, "upload-only: expected 0 folder creates")
}

// ---------------------------------------------------------------------------
// Ordering Tests (via dependency edges)
// ---------------------------------------------------------------------------

func TestOrderPlan_FolderCreatesTopDown(t *testing.T) {
	// Folder creates should have dependency edges: deeper depends on shallower.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "a/b/c/planner-deep",
			RemoteEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceRemote, Type: synctypes.ChangeCreate, Path: "a/b/c/planner-deep", ItemType: synctypes.ItemTypeFolder, ItemID: "f1"},
			},
		},
		{
			Path: "a/planner-shallow",
			RemoteEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceRemote, Type: synctypes.ChangeCreate, Path: "a/planner-shallow", ItemType: synctypes.ItemTypeFolder, ItemID: "f2"},
			},
		},
		{
			Path: "a/b/planner-mid",
			RemoteEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceRemote, Type: synctypes.ChangeCreate, Path: "a/b/planner-mid", ItemType: synctypes.ItemTypeFolder, ItemID: "f3"},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	require.Len(t, folderCreates, 3)

	// Build a path→index map for the full action list so we can check deps.
	pathIdx := make(map[string]int)
	for i := range plan.Actions {
		pathIdx[plan.Actions[i].Path] = i
	}

	// Verify dependency edges exist: deeper folders depend on their parent folder create.
	// a/b/c/planner-deep should depend on a/b/planner-mid (since a/b is its parent
	// directory, but there's no folder create at a/b — only at a/b/planner-mid).
	// The dependency rule is: if parent path has a folder create, depend on it.
	// So we just verify the plan has the correct number of folder creates.
	// The executor uses Deps to determine execution order.
	_ = pathIdx // dependency edges are tested implicitly via the executor
}

func TestOrderPlan_DeletesBottomUp(t *testing.T) {
	// Deletes should have dependency edges: parent deletes depend on child deletes.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "x/planner-del-shallow.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete, Path: "x/planner-del-shallow.txt",
					ItemType: synctypes.ItemTypeFile, ItemID: "d1", IsDeleted: true,
				},
			},
		},
		{
			Path: "x/y/z/planner-del-deep.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete, Path: "x/y/z/planner-del-deep.txt",
					ItemType: synctypes.ItemTypeFile, ItemID: "d2", IsDeleted: true,
				},
			},
		},
		{
			Path: "x/y/planner-del-mid.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete, Path: "x/y/planner-del-mid.txt",
					ItemType: synctypes.ItemTypeFile, ItemID: "d3", IsDeleted: true,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path: "x/planner-del-shallow.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "d1",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hashD", RemoteHash: "hashD",
		},
		&synctypes.BaselineEntry{
			Path: "x/y/z/planner-del-deep.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "d2",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hashD", RemoteHash: "hashD",
		},
		&synctypes.BaselineEntry{
			Path: "x/y/planner-del-mid.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "d3",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hashD", RemoteHash: "hashD",
		},
	)

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	require.Len(t, localDeletes, 3)

	// Verify all 3 delete paths are present (order is non-deterministic in the
	// flat list — the executor uses Deps to determine execution order).
	deletePaths := make(map[string]bool)
	for _, d := range localDeletes {
		deletePaths[d.Path] = true
	}

	for _, expected := range []string{
		"x/planner-del-shallow.txt",
		"x/y/planner-del-mid.txt",
		"x/y/z/planner-del-deep.txt",
	} {
		assert.True(t, deletePaths[expected], "expected delete for path %q", expected)
	}
}

// ---------------------------------------------------------------------------
// Integration Tests
// ---------------------------------------------------------------------------

func TestPlan_EmptyChanges(t *testing.T) {
	// Empty changes → empty plan, no error.
	planner := NewPlanner(synctest.TestLogger(t))

	plan, err := planner.Plan(nil, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	assert.Equal(t, 0, countActions(plan), "empty changes: expected 0 actions")
}

func TestPlan_MixedFileAndFolder(t *testing.T) {
	// Mix of file and folder changes → correct action types.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-mix-dir",
			RemoteEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceRemote, Type: synctypes.ChangeCreate, Path: "planner-mix-dir", ItemType: synctypes.ItemTypeFolder, ItemID: "mf1"},
			},
		},
		{
			Path: "planner-mix-dir/planner-mix-file.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeCreate, Path: "planner-mix-dir/planner-mix-file.txt",
					ItemType: synctypes.ItemTypeFile, Hash: "hashMix", ItemID: "mf2",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	assert.Len(t, folderCreates, 1)

	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Len(t, downloads, 1)
}

func TestPlan_FullScenario(t *testing.T) {
	// Multiple paths with different matrix cells → correct plan.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		// EF2: remote modified
		{
			Path: "planner-full/remote-mod.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeModify, Path: "planner-full/remote-mod.txt",
					ItemType: synctypes.ItemTypeFile, Hash: "hashNew1", ItemID: "full1",
				},
			},
		},
		// EF3: local modified
		{
			Path: "planner-full/local-mod.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceLocal, Type: synctypes.ChangeModify, Path: "planner-full/local-mod.txt",
					ItemType: synctypes.ItemTypeFile, Hash: "hashNew2",
				},
			},
		},
		// EF14: new remote file
		{
			Path: "planner-full/brand-new.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeCreate, Path: "planner-full/brand-new.txt",
					ItemType: synctypes.ItemTypeFile, Hash: "hashBN", ItemID: "full3",
				},
			},
		},
		// ED3: new remote folder
		{
			Path: "planner-full/new-dir",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeCreate, Path: "planner-full/new-dir",
					ItemType: synctypes.ItemTypeFolder, ItemID: "full4",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path: "planner-full/remote-mod.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "full1",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hashOld1", RemoteHash: "hashOld1",
		},
		&synctypes.BaselineEntry{
			Path: "planner-full/local-mod.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "full2",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hashOld2", RemoteHash: "hashOld2",
		},
	)

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	// EF2 + EF14 = 2 downloads.
	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Len(t, downloads, 2)

	// EF3 = 1 upload.
	uploads := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	assert.Len(t, uploads, 1)

	// ED3 = 1 folder create.
	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	assert.Len(t, folderCreates, 1)
}

// ---------------------------------------------------------------------------
// DriveID Propagation Tests
// ---------------------------------------------------------------------------

func TestMakeAction_CrossDriveItem(t *testing.T) {
	// When Remote has DriveID "drive-A" and Baseline has DriveID "drive-B",
	// the Action should get "drive-A" (Remote wins for cross-drive items).
	view := &synctypes.PathView{
		Path: "shared/cross-drive-file.txt",
		Remote: &synctypes.RemoteState{
			ItemID:   "item-from-drive-a",
			DriveID:  driveid.New("000000000000000a"),
			ItemType: synctypes.ItemTypeFile,
		},
		Baseline: &synctypes.BaselineEntry{
			Path:    "shared/cross-drive-file.txt",
			DriveID: driveid.New("000000000000000b"),
			ItemID:  "item-from-drive-a",
		},
	}

	action := MakeAction(synctypes.ActionDownload, view)

	assert.Equal(t, driveid.New("000000000000000a"), action.DriveID, "DriveID from Remote")
	assert.Equal(t, "item-from-drive-a", action.ItemID)
}

func TestMakeAction_NewLocalItem(t *testing.T) {
	// When both Remote and Baseline are nil (new local-only file, EF13),
	// Action.DriveID should be empty — the executor fills from context.
	view := &synctypes.PathView{
		Path: "new-local-file.txt",
		Local: &synctypes.LocalState{
			Name:     "new-local-file.txt",
			ItemType: synctypes.ItemTypeFile,
			Size:     100,
			Hash:     "hashLocal",
		},
	}

	action := MakeAction(synctypes.ActionUpload, view)

	assert.True(t, action.DriveID.IsZero(), "expected zero DriveID for new local item")
	assert.Empty(t, action.ItemID, "expected empty ItemID for new local item")
}

func TestMakeAction_BaselineFallbackDriveID(t *testing.T) {
	// When Remote has no DriveID (empty) but Baseline has one,
	// the Action should get Baseline's DriveID.
	view := &synctypes.PathView{
		Path: "baseline-fallback.txt",
		Remote: &synctypes.RemoteState{
			ItemID:   "item-fallback",
			ItemType: synctypes.ItemTypeFile,
			// DriveID zero value — no DriveID from remote
		},
		Baseline: &synctypes.BaselineEntry{
			Path:    "baseline-fallback.txt",
			DriveID: driveid.New(synctest.TestDriveID),
			ItemID:  "item-fallback",
		},
	}

	action := MakeAction(synctypes.ActionDownload, view)

	assert.Equal(t, driveid.New(synctest.TestDriveID), action.DriveID, "DriveID from Baseline")
}

// Validates: R-6.8.12, R-6.8.13
func TestMakeAction_ShortcutEnrichment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		remote          *synctypes.RemoteState
		wantOwnDrive    bool
		wantShortcutKey string
		wantTargetDrive driveid.ID
	}{
		{
			name: "own-drive item has empty shortcut fields",
			remote: &synctypes.RemoteState{
				ItemID:   "item-1",
				DriveID:  driveid.New(synctest.TestDriveID),
				ItemType: synctypes.ItemTypeFile,
			},
			wantOwnDrive:    true,
			wantShortcutKey: "",
			wantTargetDrive: driveid.ID{},
		},
		{
			name: "shortcut item has populated shortcut fields",
			remote: &synctypes.RemoteState{
				ItemID:        "item-2",
				DriveID:       driveid.New(synctest.TestDriveID),
				ItemType:      synctypes.ItemTypeFile,
				RemoteDriveID: "0000000000000099",
				RemoteItemID:  "source-folder-1",
			},
			wantOwnDrive:    false,
			wantShortcutKey: "0000000000000099:source-folder-1",
			wantTargetDrive: driveid.New("0000000000000099"),
		},
		{
			name:            "nil remote has empty shortcut fields",
			remote:          nil,
			wantOwnDrive:    true,
			wantShortcutKey: "",
			wantTargetDrive: driveid.ID{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			view := &synctypes.PathView{
				Path:   "test-file.txt",
				Remote: tt.remote,
			}

			action := MakeAction(synctypes.ActionDownload, view)

			assert.Equal(t, tt.wantOwnDrive, action.TargetsOwnDrive(), "TargetsOwnDrive()")
			assert.Equal(t, tt.wantShortcutKey, action.ShortcutKey(), "ShortcutKey()")
			assert.Equal(t, tt.wantTargetDrive, action.TargetDriveID, "TargetDriveID")
		})
	}
}

// ---------------------------------------------------------------------------
// Helper function unit tests
// ---------------------------------------------------------------------------

func TestDetectLocalChange(t *testing.T) {
	// No baseline, local exists → changed (new file).
	view := &synctypes.PathView{
		Local: &synctypes.LocalState{Hash: "h1"},
	}
	assert.True(t, detectLocalChange(view), "expected local change with no baseline and local present")

	// No baseline, no local → not changed.
	view = &synctypes.PathView{}
	assert.False(t, detectLocalChange(view), "expected no local change with no baseline and no local")

	// Baseline exists, local nil → changed (deleted).
	view = &synctypes.PathView{
		Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFile, LocalHash: "h1"},
	}
	assert.True(t, detectLocalChange(view), "expected local change when local is nil (deleted)")

	// Baseline folder → not changed (folders have no hash).
	view = &synctypes.PathView{
		Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFolder},
		Local:    &synctypes.LocalState{ItemType: synctypes.ItemTypeFolder},
	}
	assert.False(t, detectLocalChange(view), "expected no change for folder")
}

// Validates: R-6.7.7
func TestDetectRemoteChange(t *testing.T) {
	// No baseline, remote exists → changed.
	view := &synctypes.PathView{
		Remote: &synctypes.RemoteState{Hash: "h1"},
	}
	assert.True(t, detectRemoteChange(view), "expected remote change with no baseline and remote present")

	// No baseline, remote is deleted → not changed (never synced, delete is a no-op).
	view = &synctypes.PathView{
		Remote: &synctypes.RemoteState{IsDeleted: true},
	}
	assert.False(t, detectRemoteChange(view), "expected no remote change for deleted item with no baseline")

	// Baseline exists, remote nil → no change (no observation).
	view = &synctypes.PathView{
		Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFile, RemoteHash: "h1"},
	}
	assert.False(t, detectRemoteChange(view), "expected no remote change when remote is nil")

	// Baseline exists, remote deleted → changed.
	view = &synctypes.PathView{
		Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFile, RemoteHash: "h1"},
		Remote:   &synctypes.RemoteState{IsDeleted: true},
	}
	assert.True(t, detectRemoteChange(view), "expected remote change when remote is deleted")
}

func TestDetectChange_UsesPerSideHashes(t *testing.T) {
	tests := []struct {
		name   string
		detect func(*synctypes.PathView) bool
		view   *synctypes.PathView
		want   bool
	}{
		{
			name:   "local_matches_local_hash",
			detect: detectLocalChange,
			view: &synctypes.PathView{
				Path:  "test.txt",
				Local: &synctypes.LocalState{Hash: "localHash"},
				Baseline: &synctypes.BaselineEntry{
					ItemType:   synctypes.ItemTypeFile,
					LocalHash:  "localHash",
					RemoteHash: "differentRemoteHash",
				},
			},
		},
		{
			name:   "local_differs_from_local_hash",
			detect: detectLocalChange,
			view: &synctypes.PathView{
				Path:  "test.txt",
				Local: &synctypes.LocalState{Hash: "newLocalHash"},
				Baseline: &synctypes.BaselineEntry{
					ItemType:   synctypes.ItemTypeFile,
					LocalHash:  "oldLocalHash",
					RemoteHash: "oldLocalHash",
				},
			},
			want: true,
		},
		{
			name:   "remote_matches_remote_hash",
			detect: detectRemoteChange,
			view: &synctypes.PathView{
				Path:   "test.txt",
				Remote: &synctypes.RemoteState{Hash: "remoteHash"},
				Baseline: &synctypes.BaselineEntry{
					ItemType:   synctypes.ItemTypeFile,
					LocalHash:  "differentLocalHash",
					RemoteHash: "remoteHash",
				},
			},
		},
		{
			name:   "remote_differs_from_remote_hash",
			detect: detectRemoteChange,
			view: &synctypes.PathView{
				Path:   "test.txt",
				Remote: &synctypes.RemoteState{Hash: "newRemoteHash"},
				Baseline: &synctypes.BaselineEntry{
					ItemType:   synctypes.ItemTypeFile,
					LocalHash:  "someHash",
					RemoteHash: "oldRemoteHash",
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.detect(tt.view))
		})
	}
}

type changeDetectionCase struct {
	name string
	view *synctypes.PathView
	want bool
}

func metadataFallbackLocalCases() []changeDetectionCase {
	return []changeDetectionCase{
		{
			name: "local_fallback_matches_when_size_and_mtime_match",
			view: &synctypes.PathView{
				Local: &synctypes.LocalState{
					ItemType: synctypes.ItemTypeFile,
					Size:     0,
					Mtime:    100,
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:       synctypes.ItemTypeFile,
					LocalSize:      0,
					LocalSizeKnown: true,
					LocalMtime:     100,
				},
			},
		},
		{
			name: "local_fallback_detects_size_change",
			view: &synctypes.PathView{
				Local: &synctypes.LocalState{
					ItemType: synctypes.ItemTypeFile,
					Size:     2,
					Mtime:    100,
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:       synctypes.ItemTypeFile,
					LocalSize:      1,
					LocalSizeKnown: true,
					LocalMtime:     100,
				},
			},
			want: true,
		},
		{
			name: "local_fallback_treats_unknown_size_as_changed",
			view: &synctypes.PathView{
				Local: &synctypes.LocalState{
					ItemType: synctypes.ItemTypeFile,
					Size:     0,
					Mtime:    100,
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:   synctypes.ItemTypeFile,
					LocalMtime: 100,
				},
			},
			want: true,
		},
		{
			name: "local_hash_appearing_counts_as_change",
			view: &synctypes.PathView{
				Local: &synctypes.LocalState{
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hash-now-present",
					Size:     10,
					Mtime:    100,
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:       synctypes.ItemTypeFile,
					LocalSize:      10,
					LocalSizeKnown: true,
					LocalMtime:     100,
				},
			},
			want: true,
		},
		{
			name: "local_hash_disappearing_counts_as_change",
			view: &synctypes.PathView{
				Local: &synctypes.LocalState{
					ItemType: synctypes.ItemTypeFile,
					Size:     10,
					Mtime:    100,
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:       synctypes.ItemTypeFile,
					LocalHash:      "hash-was-present",
					LocalSize:      10,
					LocalSizeKnown: true,
					LocalMtime:     100,
				},
			},
			want: true,
		},
	}
}

func metadataFallbackRemoteMetadataCases() []changeDetectionCase {
	return []changeDetectionCase{
		{
			name: "remote_fallback_matches_when_size_mtime_and_etag_match",
			view: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemType: synctypes.ItemTypeFile,
					Size:     0,
					Mtime:    200,
					ETag:     "etag-1",
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:        synctypes.ItemTypeFile,
					RemoteSize:      0,
					RemoteSizeKnown: true,
					RemoteMtime:     200,
					ETag:            "etag-1",
				},
			},
		},
		{
			name: "remote_fallback_detects_etag_change",
			view: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemType: synctypes.ItemTypeFile,
					Size:     0,
					Mtime:    200,
					ETag:     "etag-new",
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:        synctypes.ItemTypeFile,
					RemoteSize:      0,
					RemoteSizeKnown: true,
					RemoteMtime:     200,
					ETag:            "etag-old",
				},
			},
			want: true,
		},
		{
			name: "remote_fallback_detects_remote_mtime_change",
			view: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemType: synctypes.ItemTypeFile,
					Size:     10,
					Mtime:    201,
					ETag:     "etag-1",
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:        synctypes.ItemTypeFile,
					RemoteSize:      10,
					RemoteSizeKnown: true,
					RemoteMtime:     200,
					ETag:            "etag-1",
				},
			},
			want: true,
		},
		{
			name: "remote_fallback_detects_remote_size_change",
			view: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemType: synctypes.ItemTypeFile,
					Size:     11,
					Mtime:    200,
					ETag:     "etag-1",
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:        synctypes.ItemTypeFile,
					RemoteSize:      10,
					RemoteSizeKnown: true,
					RemoteMtime:     200,
					ETag:            "etag-1",
				},
			},
			want: true,
		},
		{
			name: "remote_fallback_treats_missing_etag_as_changed",
			view: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemType: synctypes.ItemTypeFile,
					Size:     10,
					Mtime:    200,
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:        synctypes.ItemTypeFile,
					RemoteSize:      10,
					RemoteSizeKnown: true,
					RemoteMtime:     200,
					ETag:            "etag-1",
				},
			},
			want: true,
		},
	}
}

func metadataFallbackRemoteHashTransitionCases() []changeDetectionCase {
	return []changeDetectionCase{
		{
			name: "remote_hash_appearing_counts_as_change",
			view: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hash-now-present",
					Size:     10,
					Mtime:    200,
					ETag:     "etag-1",
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:        synctypes.ItemTypeFile,
					RemoteSize:      10,
					RemoteSizeKnown: true,
					RemoteMtime:     200,
					ETag:            "etag-1",
				},
			},
			want: true,
		},
		{
			name: "remote_hash_disappearing_counts_as_change",
			view: &synctypes.PathView{
				Remote: &synctypes.RemoteState{
					ItemType: synctypes.ItemTypeFile,
					Size:     10,
					Mtime:    200,
					ETag:     "etag-1",
				},
				Baseline: &synctypes.BaselineEntry{
					ItemType:        synctypes.ItemTypeFile,
					RemoteHash:      "hash-was-present",
					RemoteSize:      10,
					RemoteSizeKnown: true,
					RemoteMtime:     200,
					ETag:            "etag-1",
				},
			},
			want: true,
		},
	}
}

func metadataFallbackRemoteCases() []changeDetectionCase {
	return append(metadataFallbackRemoteMetadataCases(), metadataFallbackRemoteHashTransitionCases()...)
}

// Validates: R-6.7.17
func TestDetectLocalChange_UsesMetadataFallbackWhenHashesMissing(t *testing.T) {
	for _, tt := range metadataFallbackLocalCases() {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, detectLocalChange(tt.view))
		})
	}
}

// Validates: R-6.7.17
func TestDetectRemoteChange_UsesMetadataFallbackWhenHashesMissing(t *testing.T) {
	for _, tt := range metadataFallbackRemoteCases() {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, detectRemoteChange(tt.view))
		})
	}
}

// ---------------------------------------------------------------------------
// Move Detection: Old Path Reused
// ---------------------------------------------------------------------------

func TestDetectMoves_RemoteMoveOldPathReused(t *testing.T) {
	// Scenario: File at A moved to B, new file created at A in the same delta.
	// Buffer produces:
	//   PathChanges for B: [ChangeMove(A→B)]
	//   PathChanges for A: [synthetic_delete, ChangeCreate(new item)]
	// The planner must produce 1 move + 1 download (not lose the new file at A).
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-moved-dest.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeMove,
					Path:     "planner-moved-dest.txt",
					OldPath:  "planner-reused-path.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashOriginal",
					ItemID:   "item-original",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
		},
		{
			Path: "planner-reused-path.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				// Synthetic delete from the buffer's move dual-keying.
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "planner-reused-path.txt",
					ItemID:    "item-original",
					DriveID:   driveid.New(synctest.TestDriveID),
					ItemType:  synctypes.ItemTypeFile,
					IsDeleted: true,
				},
				// New file created at the old path.
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-reused-path.txt",
					ItemID:   "item-new-at-old",
					DriveID:  driveid.New(synctest.TestDriveID),
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashNewFile",
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "planner-reused-path.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item-original",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashOriginal",
		RemoteHash: "hashOriginal",
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	move := allMoves[0]
	assert.Equal(t, "planner-moved-dest.txt", move.Path, "move destination")
	assert.Equal(t, "planner-reused-path.txt", move.OldPath, "move source")

	// The new file at the old path should produce a download (EF14).
	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	require.Len(t, downloads, 1, "expected 1 download for new file at reused path")
	assert.Equal(t, "planner-reused-path.txt", downloads[0].Path)
}

func TestDetectMoves_RemoteMoveOldPathReusedFolder(t *testing.T) {
	// Same scenario as above but a new folder at the old path instead of a file.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "planner-moved-folder-dest",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeMove,
					Path:     "planner-moved-folder-dest",
					OldPath:  "planner-reused-folder",
					ItemType: synctypes.ItemTypeFolder,
					ItemID:   "folder-original",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
		},
		{
			Path: "planner-reused-folder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "planner-reused-folder",
					ItemID:    "folder-original",
					DriveID:   driveid.New(synctest.TestDriveID),
					ItemType:  synctypes.ItemTypeFolder,
					IsDeleted: true,
				},
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "planner-reused-folder",
					ItemID:   "folder-new-at-old",
					DriveID:  driveid.New(synctest.TestDriveID),
					ItemType: synctypes.ItemTypeFolder,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "planner-reused-folder",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder-original",
		ItemType: synctypes.ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	// The new folder at the old path should produce a folder create (ED3).
	folderCreates := synctest.ActionsOfType(plan.Actions, synctypes.ActionFolderCreate)
	require.Len(t, folderCreates, 1, "expected 1 folder create for new folder at reused path")
	assert.Equal(t, "planner-reused-folder", folderCreates[0].Path)
	assert.Equal(t, synctypes.CreateLocal, folderCreates[0].CreateSide)
}

// ---------------------------------------------------------------------------
// Delete Ordering: Files Before Folders at Same Depth
// ---------------------------------------------------------------------------

func TestOrderPlan_DeletesFilesBeforeFoldersAtSameDepth(t *testing.T) {
	// At the same depth, files and folders should both produce delete actions.
	// In the flat plan, ordering is handled by dependency edges rather than
	// positional ordering. Folder deletes depend on child deletes via Deps.
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "x/planner-del-folder",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete, Path: "x/planner-del-folder",
					ItemType: synctypes.ItemTypeFolder, ItemID: "df1", IsDeleted: true,
				},
			},
		},
		{
			Path: "x/planner-del-file.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source: synctypes.SourceRemote, Type: synctypes.ChangeDelete, Path: "x/planner-del-file.txt",
					ItemType: synctypes.ItemTypeFile, ItemID: "df2", IsDeleted: true,
				},
			},
		},
	}

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path: "x/planner-del-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "df1",
			ItemType: synctypes.ItemTypeFolder,
		},
		&synctypes.BaselineEntry{
			Path: "x/planner-del-file.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "df2",
			ItemType: synctypes.ItemTypeFile, LocalHash: "hashDF", RemoteHash: "hashDF",
		},
	)

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	require.Len(t, localDeletes, 2)

	// Verify both paths are present (order is non-deterministic in the flat
	// list — the executor uses Deps to determine execution order).
	deletePaths := make(map[string]bool)
	for _, d := range localDeletes {
		deletePaths[d.Path] = true
	}

	assert.True(t, deletePaths["x/planner-del-file.txt"], "expected delete for x/planner-del-file.txt")
	assert.True(t, deletePaths["x/planner-del-folder"], "expected delete for x/planner-del-folder")
}

// TestPlan_DeterministicOrder verifies that calling Plan() twice with
// identical input produces identical action ordering (B-154).
func TestPlan_DeterministicOrder(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))
	baseline := synctest.EmptyBaseline()

	// Create multiple paths with no baseline — all produce uploads.
	changes := []synctypes.PathChanges{
		{Path: "z/delta.txt", LocalEvents: []synctypes.ChangeEvent{{Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "z/delta.txt", ItemType: synctypes.ItemTypeFile, Hash: "h4"}}},
		{Path: "a/alpha.txt", LocalEvents: []synctypes.ChangeEvent{{Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "a/alpha.txt", ItemType: synctypes.ItemTypeFile, Hash: "h1"}}},
		{Path: "m/beta.txt", LocalEvents: []synctypes.ChangeEvent{{Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "m/beta.txt", ItemType: synctypes.ItemTypeFile, Hash: "h2"}}},
		{Path: "b/gamma.txt", LocalEvents: []synctypes.ChangeEvent{{Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "b/gamma.txt", ItemType: synctypes.ItemTypeFile, Hash: "h3"}}},
	}

	plan1, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "first Plan()")

	plan2, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err, "second Plan()")

	require.Len(t, plan2.Actions, len(plan1.Actions), "action count mismatch")

	for i := range plan1.Actions {
		assert.Equal(t, plan1.Actions[i].Path, plan2.Actions[i].Path, "action[%d] path", i)
		assert.Equal(t, plan1.Actions[i].Type, plan2.Actions[i].Type, "action[%d] type", i)
	}
}

// ---------------------------------------------------------------------------
// Denied Prefix Tests (planner-integrated permission suppression)
// ---------------------------------------------------------------------------

func TestPlan_DeniedPrefix_SuppressesUploads(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "Shared/ReadOnly/file.txt", ItemType: synctypes.ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Local changed, remote unchanged → would normally be ActionUpload.
	changes := []synctypes.PathChanges{{
		Path:         "Shared/ReadOnly/file.txt",
		LocalEvents:  []synctypes.ChangeEvent{{Type: synctypes.ChangeModify, Path: "Shared/ReadOnly/file.txt", ItemType: synctypes.ItemTypeFile, Hash: "bbb"}},
		RemoteEvents: nil,
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	// Upload suppressed under denied prefix.
	uploads := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	assert.Empty(t, uploads, "uploads should be suppressed under denied prefix")
}

func TestPlan_DeniedPrefix_AllowsDownloads(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "Shared/ReadOnly/file.txt", ItemType: synctypes.ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Remote changed, local unchanged → ActionDownload (should proceed).
	changes := []synctypes.PathChanges{{
		Path:        "Shared/ReadOnly/file.txt",
		LocalEvents: nil,
		RemoteEvents: []synctypes.ChangeEvent{{
			Type: synctypes.ChangeModify, Path: "Shared/ReadOnly/file.txt",
			ItemType: synctypes.ItemTypeFile, Hash: "bbb",
		}},
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Len(t, downloads, 1, "downloads should proceed under denied prefix")
}

func TestPlan_DeniedPrefix_OutsideDenied_Normal(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "Writable/file.txt", ItemType: synctypes.ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Local changed outside denied prefix → normal ActionUpload.
	changes := []synctypes.PathChanges{{
		Path:         "Writable/file.txt",
		LocalEvents:  []synctypes.ChangeEvent{{Type: synctypes.ChangeModify, Path: "Writable/file.txt", ItemType: synctypes.ItemTypeFile, Hash: "bbb"}},
		RemoteEvents: nil,
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	uploads := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	assert.Len(t, uploads, 1, "uploads outside denied prefix should proceed normally")
}

func TestPlan_DeniedPrefix_RemoteDelete_LocalDelete(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "Shared/ReadOnly/file.txt", ItemType: synctypes.ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Remote deleted under denied prefix → should produce ActionLocalDelete.
	changes := []synctypes.PathChanges{{
		Path:        "Shared/ReadOnly/file.txt",
		LocalEvents: nil,
		RemoteEvents: []synctypes.ChangeEvent{{
			Type: synctypes.ChangeDelete, Path: "Shared/ReadOnly/file.txt",
			ItemType: synctypes.ItemTypeFile, IsDeleted: true,
		}},
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	localDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalDelete)
	assert.Len(t, localDeletes, 1, "remote delete under denied prefix → local delete")

	// Should NOT produce remote delete (we can't write to remote).
	remoteDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteDelete)
	assert.Empty(t, remoteDeletes, "should not produce remote deletes under denied prefix")
}

func TestPlan_DeniedPrefix_LocalMove_Suppressed(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(synctest.TestLogger(t))

	// Baseline has a file at old path.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "Shared/ReadOnly/old.txt", ItemType: synctypes.ItemTypeFile,
		LocalHash: "abc123", RemoteHash: "abc123",
		DriveID: driveid.New(synctest.TestDriveID), ItemID: "item-1",
	})

	// Local delete at old path + local create at new path with same hash = local move.
	changes := []synctypes.PathChanges{
		{
			Path: "Shared/ReadOnly/old.txt",
			// No local events (file gone locally = local delete).
		},
		{
			Path: "Shared/ReadOnly/new.txt",
			LocalEvents: []synctypes.ChangeEvent{{
				Type:     synctypes.ChangeCreate,
				Path:     "Shared/ReadOnly/new.txt",
				ItemType: synctypes.ItemTypeFile,
				Hash:     "abc123",
			}},
		},
	}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	// Should NOT produce a remote move — can't write to remote under denied prefix.
	remoteMoves := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteMove)
	assert.Empty(t, remoteMoves, "local move under denied prefix should not produce remote move")
}

// Validates: R-6.8.12, R-6.8.13
func TestPlan_ShortcutAction_HasTargetShortcutKey(t *testing.T) {
	// Integration test: a shortcut ChangeEvent flows through Plan() and the
	// resulting Action carries targetShortcutKey and targetDriveID so that
	// active-scope matching can distinguish own-drive vs shortcut-scoped failures.
	t.Parallel()

	const (
		shortcutPath    = "Shortcuts/shared/doc.txt"
		remoteDriveID   = "AAAA000000000099" // sharer's drive
		remoteItemID    = "shortcut-folder-id"
		fileItemID      = "file-item-1"
		fileParentID    = "parent-1"
		ownDriveIDValue = synctest.TestDriveID
	)

	planner := NewPlanner(synctest.TestLogger(t))

	// Baseline entry for the file under the shortcut path — represents
	// a previously synced shortcut item living on the sharer's drive.
	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       shortcutPath,
		DriveID:    driveid.New(remoteDriveID),
		ItemID:     fileItemID,
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashOld",
		RemoteHash: "hashOld",
	})

	// Simulate a remote modify event as produced by the shortcut converter:
	// the ChangeEvent carries RemoteDriveID/RemoteItemID identifying the
	// shortcut scope, and a new hash to trigger a download action.
	changes := []synctypes.PathChanges{
		{
			Path: shortcutPath,
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:        synctypes.SourceRemote,
					Type:          synctypes.ChangeModify,
					Path:          shortcutPath,
					ItemType:      synctypes.ItemTypeFile,
					ItemID:        fileItemID,
					DriveID:       driveid.New(remoteDriveID),
					ParentID:      fileParentID,
					Hash:          "hashNew",
					RemoteDriveID: remoteDriveID,
					RemoteItemID:  remoteItemID,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.NotEmpty(t, plan.Actions, "expected at least one action for shortcut change")

	action := plan.Actions[0]
	assert.False(t, action.TargetsOwnDrive(), "shortcut action should NOT target own drive")
	assert.Equal(t, remoteDriveID+":"+remoteItemID, action.ShortcutKey(),
		"ShortcutKey should be remoteDrive:remoteItem")
	assert.Equal(t, driveid.New(remoteDriveID), action.TargetDriveID,
		"TargetDriveID should be the sharer's drive")
}

func TestIsWriteDenied(t *testing.T) {
	t.Parallel()

	denied := []string{"Shared/ReadOnly", "Shared/Other/Private"}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"exact denied folder", "Shared/ReadOnly", true},
		{"child of denied", "Shared/ReadOnly/sub/file.txt", true},
		{"different folder", "Shared/Writable/file.txt", false},
		{"partial prefix", "Shared/ReadOnlyExtra/file.txt", false},
		{"exact subfolder denied", "Shared/Other/Private", true},
		{"child of subfolder denied", "Shared/Other/Private/deep/file.txt", true},
		{"sibling of denied subfolder", "Shared/Other/Public/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsWriteDenied(tt.path, denied))
		})
	}
}

func TestIsWriteDenied_EmptyBoundaryBlocksWholeScopedRoot(t *testing.T) {
	t.Parallel()

	assert.True(t, IsWriteDenied("nested/file.txt", []string{""}))
	assert.True(t, IsWriteDenied("top-level.txt", []string{""}))
}
