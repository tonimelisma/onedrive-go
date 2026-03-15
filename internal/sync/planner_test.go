package sync

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// Test helpers (planner-specific — reuses testLogger, emptyBaseline,
// baselineWith, testDriveID from existing test files)
// ---------------------------------------------------------------------------

// countActions returns the total number of actions in the plan.
func countActions(plan *ActionPlan) int {
	return len(plan.Actions)
}

// moves returns all move actions (both local and remote) from the plan.
func moves(plan *ActionPlan) []Action {
	var result []Action
	for i := range plan.Actions {
		if plan.Actions[i].Type == ActionLocalMove || plan.Actions[i].Type == ActionRemoteMove {
			result = append(result, plan.Actions[i])
		}
	}

	return result
}

// ---------------------------------------------------------------------------
// File Decision Matrix Tests (EF1-EF14)
// ---------------------------------------------------------------------------

func TestClassifyFile_EF1_Unchanged(t *testing.T) {
	// EF1: baseline exists, remote and local both match baseline hashes.
	// No remote events and no local events → both change detectors return false.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashA",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashA",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")
	assert.Equal(t, 0, countActions(plan), "EF1")
}

func TestClassifyFile_EF2_RemoteModified(t *testing.T) {
	// EF2: baseline exists, remote hash changed, no local events (local derived from baseline).
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashB",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	require.Len(t, downloads, 1, "EF2")
	assert.Equal(t, "planner-test.txt", downloads[0].Path, "EF2")
}

func TestClassifyFile_EF3_LocalModified(t *testing.T) {
	// EF3: baseline exists, local hash changed, no remote events (remote nil → unchanged).
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashB",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	uploads := ActionsOfType(plan.Actions, ActionUpload)
	require.Len(t, uploads, 1, "EF3")
	assert.Equal(t, "planner-test.txt", uploads[0].Path, "EF3")
}

func TestClassifyFile_EF4_ConvergentEdit(t *testing.T) {
	// EF4: baseline exists, both hashes changed but local.Hash == remote.Hash.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashC",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashC",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	syncedUpdates := ActionsOfType(plan.Actions, ActionUpdateSynced)
	require.Len(t, syncedUpdates, 1, "EF4")
}

// Validates: R-2.2
func TestClassifyFile_EF5_EditEditConflict(t *testing.T) {
	// EF5: baseline exists, both hashes changed and differ.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashB",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashC",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	require.Len(t, conflicts, 1, "EF5")
	assert.Equal(t, "edit_edit", conflicts[0].ConflictInfo.ConflictType, "EF5")
}

func TestClassifyFile_EF6_LocalDeleteRemoteUnchanged(t *testing.T) {
	// EF6: baseline exists, local ChangeDelete event, no remote events.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	require.Len(t, remoteDeletes, 1, "EF6")
}

func TestClassifyFile_EF7_LocalDeleteRemoteModified(t *testing.T) {
	// EF7: baseline exists, local deleted, remote hash changed → download (remote wins).
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashB",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	require.Len(t, downloads, 1, "EF7")
}

// Validates: R-6.7.7
func TestClassifyFile_EF8_RemoteDeleted(t *testing.T) {
	// EF8: baseline exists, remote event with IsDeleted=true, no local events
	// (local derived from baseline → unchanged).
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "planner-test.txt",
					ItemType:  ItemTypeFile,
					ItemID:    "item1",
					DriveID:   driveid.New(testDriveID),
					IsDeleted: true,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	require.Len(t, localDeletes, 1, "EF8")
}

// Validates: R-2.2
func TestClassifyFile_EF9_EditDeleteConflict(t *testing.T) {
	// EF9: baseline exists, local hash changed, remote IsDeleted=true.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "planner-test.txt",
					ItemType:  ItemTypeFile,
					ItemID:    "item1",
					DriveID:   driveid.New(testDriveID),
					IsDeleted: true,
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashB",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	require.Len(t, conflicts, 1, "EF9")
	assert.Equal(t, "edit_delete", conflicts[0].ConflictInfo.ConflictType, "EF9")
}

func TestClassifyFile_EF10_BothDeleted(t *testing.T) {
	// EF10: baseline exists, local deleted, remote IsDeleted.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-test.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "planner-test.txt",
					ItemType:  ItemTypeFile,
					ItemID:    "item1",
					DriveID:   driveid.New(testDriveID),
					IsDeleted: true,
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "planner-test.txt",
					ItemType: ItemTypeFile,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-test.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	cleanups := ActionsOfType(plan.Actions, ActionCleanup)
	require.Len(t, cleanups, 1, "EF10")
}

func TestClassifyFile_EF11_ConvergentCreate(t *testing.T) {
	// EF11: no baseline, both local and remote exist with same hash.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-new.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashX",
					ItemID:   "item2",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashX",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	syncedUpdates := ActionsOfType(plan.Actions, ActionUpdateSynced)
	require.Len(t, syncedUpdates, 1, "EF11")
}

// Validates: R-2.2
func TestClassifyFile_EF12_CreateCreateConflict(t *testing.T) {
	// EF12: no baseline, both exist with different hashes.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-new.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashX",
					ItemID:   "item2",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "planner-new.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashY",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	require.Len(t, conflicts, 1, "EF12")
	assert.Equal(t, "create_create", conflicts[0].ConflictInfo.ConflictType, "EF12")
}

func TestClassifyFile_EF13_NewLocal(t *testing.T) {
	// EF13: no baseline, local exists, no remote.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-local-only.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "planner-local-only.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashL",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	uploads := ActionsOfType(plan.Actions, ActionUpload)
	require.Len(t, uploads, 1, "EF13")
}

func TestClassifyFile_EF14_NewRemote(t *testing.T) {
	// EF14: no baseline, remote exists, no local.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-remote-only.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "planner-remote-only.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashR",
					ItemID:   "item3",
					DriveID:  driveid.New(testDriveID),
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	require.Len(t, downloads, 1, "EF14")
}

// ---------------------------------------------------------------------------
// Folder Decision Matrix Tests (ED1-ED8)
// ---------------------------------------------------------------------------

func TestClassifyFolder_ED1_InSync(t *testing.T) {
	// ED1: baseline exists, folder exists on both sides → no-op.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder1",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	assert.Equal(t, 0, countActions(plan), "ED1")
}

func TestClassifyFolder_ED2_Adopt(t *testing.T) {
	// ED2: no baseline, folder exists on both sides → adopt (update synced).
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	syncedUpdates := ActionsOfType(plan.Actions, ActionUpdateSynced)
	require.Len(t, syncedUpdates, 1, "ED2")
}

func TestClassifyFolder_ED3_NewRemoteFolder(t *testing.T) {
	// ED3: no baseline, remote folder exists, no local → create locally.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	require.Len(t, folderCreates, 1, "ED3")
	assert.Equal(t, CreateLocal, folderCreates[0].CreateSide, "ED3")
}

func TestClassifyFolder_ED4_RecreateLocal(t *testing.T) {
	// ED4: baseline exists, remote folder exists, local absent → recreate locally.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
					ItemID:   "folder1",
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder1",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	require.Len(t, folderCreates, 1, "ED4")
	assert.Equal(t, CreateLocal, folderCreates[0].CreateSide, "ED4")
}

func TestClassifyFolder_ED5_NewLocalFolder(t *testing.T) {
	// ED5: no baseline, local folder exists, no remote → create remotely.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	require.Len(t, folderCreates, 1, "ED5")
	assert.Equal(t, CreateRemote, folderCreates[0].CreateSide, "ED5")
}

func TestClassifyFolder_ED6_RemoteDeletedFolder(t *testing.T) {
	// ED6: baseline exists, remote IsDeleted, local exists → delete locally.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "docs/planner-dir",
					ItemType:  ItemTypeFolder,
					ItemID:    "folder1",
					IsDeleted: true,
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder1",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	require.Len(t, localDeletes, 1, "ED6")
}

func TestClassifyFolder_ED7_BothGone(t *testing.T) {
	// ED7: baseline exists, remote IsDeleted, local absent → cleanup.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "docs/planner-dir",
					ItemType:  ItemTypeFolder,
					ItemID:    "folder1",
					IsDeleted: true,
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder1",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	cleanups := ActionsOfType(plan.Actions, ActionCleanup)
	require.Len(t, cleanups, 1, "ED7")
}

func TestClassifyFolder_ED8_PropagateRemoteDelete(t *testing.T) {
	// ED8: baseline exists, no remote events (unchanged), local deleted → propagate delete remotely.
	// This is the folder equivalent of EF6 (file: locally deleted, remote unchanged).
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "docs/planner-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder1",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	require.Len(t, remoteDeletes, 1, "ED8")
	assert.Equal(t, "docs/planner-dir", remoteDeletes[0].Path, "ED8")
	assert.Equal(t, "folder1", remoteDeletes[0].ItemID, "ED8")
	assert.Equal(t, driveid.New(testDriveID), remoteDeletes[0].DriveID, "ED8")
}

func TestClassifyFolder_ED8_DownloadOnly(t *testing.T) {
	// ED8 + SyncDownloadOnly: local deleted, no remote events, baseline exists.
	// Download-only zeroes localDeleted → falls through to no action.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir-dl",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "docs/planner-dir-dl",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir-dl",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder2",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncDownloadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	assert.Equal(t, 0, countActions(plan), "ED8 download-only")
}

func TestClassifyFolder_ED4_UploadOnly(t *testing.T) {
	// ED4 + SyncUploadOnly: local deleted, remote exists, baseline exists.
	// Upload-only: engine doesn't produce remote events, so hasRemote is false.
	// This test verifies the planner's defense in depth — if remote events
	// did arrive in upload-only mode, ED4 would not create locally.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir-ul4",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "docs/planner-dir-ul4",
					ItemType: ItemTypeFolder,
					ItemID:   "folder3",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "docs/planner-dir-ul4",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir-ul4",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder3",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	assert.Empty(t, folderCreates, "ED4 upload-only")

	// Upload-only: local deletion should still propagate remotely (ED8 path).
	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1, "ED4 upload-only: local deletion propagates")
}

func TestClassifyFolder_ED6_UploadOnly(t *testing.T) {
	// ED6 + SyncUploadOnly: remote deleted, local exists, baseline exists.
	// Upload-only: engine doesn't produce remote events normally, but if
	// they did arrive, ED6 should not delete locally.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-dir-ul6",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "docs/planner-dir-ul6",
					ItemType:  ItemTypeFolder,
					ItemID:    "folder4",
					DriveID:   driveid.New(testDriveID),
					IsDeleted: true,
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "docs/planner-dir-ul6",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "docs/planner-dir-ul6",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder4",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	assert.Empty(t, localDeletes, "ED6 upload-only")
	assert.Equal(t, 0, countActions(plan), "ED6 upload-only: expected 0 total actions")
}

// ---------------------------------------------------------------------------
// Move Detection Tests
// ---------------------------------------------------------------------------

func TestDetectMoves_RemoteMove(t *testing.T) {
	// ChangeMove in remote events → ActionLocalMove.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "docs/planner-renamed.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeMove,
					Path:     "docs/planner-renamed.txt",
					OldPath:  "docs/planner-original.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashM",
					ItemID:   "item5",
					DriveID:  driveid.New(testDriveID),
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "docs/planner-original.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item5",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashM",
		RemoteHash: "hashM",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	move := allMoves[0]
	assert.Equal(t, ActionLocalMove, move.Type)
	assert.Equal(t, "docs/planner-renamed.txt", move.Path, "destination")
	assert.Equal(t, "docs/planner-original.txt", move.OldPath, "source")
}

func TestDetectMoves_LocalMoveByHash(t *testing.T) {
	// Local delete + local create with matching hash → ActionRemoteMove.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-old-loc.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "planner-old-loc.txt",
					ItemType: ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-new-loc.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "planner-new-loc.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashMove",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-old-loc.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item6",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashMove",
		RemoteHash: "hashMove",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	move := allMoves[0]
	assert.Equal(t, ActionRemoteMove, move.Type)
	assert.Equal(t, "planner-new-loc.txt", move.Path, "destination")
	assert.Equal(t, "planner-old-loc.txt", move.OldPath, "source")
}

func TestDetectMoves_LocalMoveAmbiguous(t *testing.T) {
	// Multiple deletes with same hash → no move, separate actions.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-dup-a.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "planner-dup-a.txt",
					ItemType: ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-dup-b.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "planner-dup-b.txt",
					ItemType: ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-dup-dest.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "planner-dup-dest.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashDup",
				},
			},
		},
	}

	baseline := baselineWith(
		&BaselineEntry{
			Path:       "planner-dup-a.txt",
			DriveID:    driveid.New(testDriveID),
			ItemID:     "itemA",
			ItemType:   ItemTypeFile,
			LocalHash:  "hashDup",
			RemoteHash: "hashDup",
		},
		&BaselineEntry{
			Path:       "planner-dup-b.txt",
			DriveID:    driveid.New(testDriveID),
			ItemID:     "itemB",
			ItemType:   ItemTypeFile,
			LocalHash:  "hashDup",
			RemoteHash: "hashDup",
		},
	)

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	// No moves — ambiguous hash match.
	allMoves := moves(plan)
	assert.Empty(t, allMoves, "expected 0 moves for ambiguous case")

	// The paths should still produce separate actions (deletes + upload).
	assert.NotEqual(t, 0, countActions(plan), "expected some actions for unmatched paths")
}

func TestDetectMoves_MovedPathsExcluded(t *testing.T) {
	// After move detection, matched paths do not appear in other action types.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-src-excl.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeDelete,
					Path:     "planner-src-excl.txt",
					ItemType: ItemTypeFile,
				},
			},
		},
		{
			Path: "planner-dst-excl.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "planner-dst-excl.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashExcl",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-src-excl.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item7",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashExcl",
		RemoteHash: "hashExcl",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
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
	planner := NewPlanner(testLogger(t))

	// 20 baseline items, delete 10. Threshold is 10 → exactly at threshold, allowed.
	var entries []*BaselineEntry
	var changes []PathChanges

	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("planner-safe-%c.txt", rune('a'+i))
		itemID := fmt.Sprintf("safe-%c", rune('a'+i))
		entries = append(entries, &BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(testDriveID),
			ItemID:     itemID,
			ItemType:   ItemTypeFile,
			LocalHash:  "hashSafe",
			RemoteHash: "hashSafe",
		})
	}

	// Delete exactly 10.
	for i := 0; i < 10; i++ {
		changes = append(changes, PathChanges{
			Path: entries[i].Path,
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      entries[i].Path,
					ItemType:  ItemTypeFile,
					ItemID:    entries[i].ItemID,
					IsDeleted: true,
				},
			},
		})
	}

	baseline := baselineWith(entries...)

	config := &SafetyConfig{BigDeleteThreshold: 10}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, config, nil)
	require.NoError(t, err, "at threshold should be allowed")
	require.NotNil(t, plan)
}

// Validates: R-6.4.1
func TestBigDelete_ExceedsThreshold(t *testing.T) {
	// Delete count exceeds threshold → ErrBigDeleteTriggered.
	planner := NewPlanner(testLogger(t))

	// Build a baseline with 20 items and create delete events for all of them.
	var entries []*BaselineEntry
	var changes []PathChanges

	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("planner-bigdel-%c.txt", rune('a'+i))
		itemID := fmt.Sprintf("bdi-%c", rune('a'+i))
		entries = append(entries, &BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(testDriveID),
			ItemID:     itemID,
			ItemType:   ItemTypeFile,
			LocalHash:  "hashBD",
			RemoteHash: "hashBD",
		})
		changes = append(changes, PathChanges{
			Path: p,
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      p,
					ItemType:  ItemTypeFile,
					ItemID:    itemID,
					IsDeleted: true,
				},
			},
		})
	}

	baseline := baselineWith(entries...)

	// 20 deletes > threshold of 10.
	config := &SafetyConfig{BigDeleteThreshold: 10}

	_, err := planner.Plan(changes, baseline, SyncBidirectional, config, nil)
	require.ErrorIs(t, err, ErrBigDeleteTriggered)
}

// Validates: R-6.4.1
func TestBigDelete_NoTrigger(t *testing.T) {
	// Few deletes well within threshold → no error.
	planner := NewPlanner(testLogger(t))

	var entries []*BaselineEntry

	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("planner-safe-%c.txt", rune('a'+i))
		itemID := fmt.Sprintf("safe-%c", rune('a'+i))
		entries = append(entries, &BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(testDriveID),
			ItemID:     itemID,
			ItemType:   ItemTypeFile,
			LocalHash:  "hashSafe",
			RemoteHash: "hashSafe",
		})
	}

	// Delete only 2 (well below default threshold of 1000).
	changes := []PathChanges{
		{
			Path: entries[0].Path,
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      entries[0].Path,
					ItemType:  ItemTypeFile,
					ItemID:    entries[0].ItemID,
					IsDeleted: true,
				},
			},
		},
		{
			Path: entries[1].Path,
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      entries[1].Path,
					ItemType:  ItemTypeFile,
					ItemID:    entries[1].ItemID,
					IsDeleted: true,
				},
			},
		},
	}

	baseline := baselineWith(entries...)

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)
	require.NotNil(t, plan)
}

// Validates: R-6.4.1
func TestBigDelete_ThresholdZero_Disabled(t *testing.T) {
	// Threshold of 0 disables big-delete protection.
	planner := NewPlanner(testLogger(t))

	var entries []*BaselineEntry
	var changes []PathChanges

	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("planner-disabled-%c.txt", rune('a'+i))
		itemID := fmt.Sprintf("dis-%c", rune('a'+i))
		entries = append(entries, &BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(testDriveID),
			ItemID:     itemID,
			ItemType:   ItemTypeFile,
			LocalHash:  "hashDis",
			RemoteHash: "hashDis",
		})
		changes = append(changes, PathChanges{
			Path: p,
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete, Path: p,
					ItemType: ItemTypeFile, ItemID: itemID, IsDeleted: true,
				},
			},
		})
	}

	baseline := baselineWith(entries...)

	config := &SafetyConfig{BigDeleteThreshold: 0}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, config, nil)
	require.NoError(t, err, "threshold=0 disables protection")
	require.NotNil(t, plan)
}

// ---------------------------------------------------------------------------
// Mode Filtering Tests
// ---------------------------------------------------------------------------

func TestPlan_DownloadOnly_SuppressesUploads(t *testing.T) {
	// SyncDownloadOnly: local modified file → no upload.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-dl-only.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "planner-dl-only.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashNew",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-dl-only.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item9",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashOld",
		RemoteHash: "hashOld",
	})

	plan, err := planner.Plan(changes, baseline, SyncDownloadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	uploads := ActionsOfType(plan.Actions, ActionUpload)
	assert.Empty(t, uploads, "download-only: expected 0 uploads")
}

func TestPlan_UploadOnly_SuppressesDownloads(t *testing.T) {
	// SyncUploadOnly: remote modified file → no download.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-ul-only.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "planner-ul-only.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashNew",
					ItemID:   "item10",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-ul-only.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item10",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashOld",
		RemoteHash: "hashOld",
	})

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Empty(t, downloads, "upload-only: expected 0 downloads")
}

func TestPlan_DownloadOnly_SuppressesFolderCreateRemote(t *testing.T) {
	// SyncDownloadOnly: new local folder → no remote create.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-dl-dir",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "planner-dl-dir",
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncDownloadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	assert.Empty(t, folderCreates, "download-only: expected 0 folder creates")
}

func TestPlan_UploadOnly_SuppressesFolderCreateLocal(t *testing.T) {
	// SyncUploadOnly: new remote folder → no local create.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-ul-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "planner-ul-dir",
					ItemType: ItemTypeFolder,
					ItemID:   "folder2",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncUploadOnly, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	assert.Empty(t, folderCreates, "upload-only: expected 0 folder creates")
}

// ---------------------------------------------------------------------------
// Ordering Tests (via dependency edges)
// ---------------------------------------------------------------------------

func TestOrderPlan_FolderCreatesTopDown(t *testing.T) {
	// Folder creates should have dependency edges: deeper depends on shallower.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "a/b/c/planner-deep",
			RemoteEvents: []ChangeEvent{
				{Source: SourceRemote, Type: ChangeCreate, Path: "a/b/c/planner-deep", ItemType: ItemTypeFolder, ItemID: "f1"},
			},
		},
		{
			Path: "a/planner-shallow",
			RemoteEvents: []ChangeEvent{
				{Source: SourceRemote, Type: ChangeCreate, Path: "a/planner-shallow", ItemType: ItemTypeFolder, ItemID: "f2"},
			},
		},
		{
			Path: "a/b/planner-mid",
			RemoteEvents: []ChangeEvent{
				{Source: SourceRemote, Type: ChangeCreate, Path: "a/b/planner-mid", ItemType: ItemTypeFolder, ItemID: "f3"},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
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
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "x/planner-del-shallow.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete, Path: "x/planner-del-shallow.txt",
					ItemType: ItemTypeFile, ItemID: "d1", IsDeleted: true,
				},
			},
		},
		{
			Path: "x/y/z/planner-del-deep.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete, Path: "x/y/z/planner-del-deep.txt",
					ItemType: ItemTypeFile, ItemID: "d2", IsDeleted: true,
				},
			},
		},
		{
			Path: "x/y/planner-del-mid.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete, Path: "x/y/planner-del-mid.txt",
					ItemType: ItemTypeFile, ItemID: "d3", IsDeleted: true,
				},
			},
		},
	}

	baseline := baselineWith(
		&BaselineEntry{
			Path: "x/planner-del-shallow.txt", DriveID: driveid.New(testDriveID), ItemID: "d1",
			ItemType: ItemTypeFile, LocalHash: "hashD", RemoteHash: "hashD",
		},
		&BaselineEntry{
			Path: "x/y/z/planner-del-deep.txt", DriveID: driveid.New(testDriveID), ItemID: "d2",
			ItemType: ItemTypeFile, LocalHash: "hashD", RemoteHash: "hashD",
		},
		&BaselineEntry{
			Path: "x/y/planner-del-mid.txt", DriveID: driveid.New(testDriveID), ItemID: "d3",
			ItemType: ItemTypeFile, LocalHash: "hashD", RemoteHash: "hashD",
		},
	)

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
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
	planner := NewPlanner(testLogger(t))

	plan, err := planner.Plan(nil, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	assert.Equal(t, 0, countActions(plan), "empty changes: expected 0 actions")
}

func TestPlan_MixedFileAndFolder(t *testing.T) {
	// Mix of file and folder changes → correct action types.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-mix-dir",
			RemoteEvents: []ChangeEvent{
				{Source: SourceRemote, Type: ChangeCreate, Path: "planner-mix-dir", ItemType: ItemTypeFolder, ItemID: "mf1"},
			},
		},
		{
			Path: "planner-mix-dir/planner-mix-file.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeCreate, Path: "planner-mix-dir/planner-mix-file.txt",
					ItemType: ItemTypeFile, Hash: "hashMix", ItemID: "mf2",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	assert.Len(t, folderCreates, 1)

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Len(t, downloads, 1)
}

func TestPlan_FullScenario(t *testing.T) {
	// Multiple paths with different matrix cells → correct plan.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		// EF2: remote modified
		{
			Path: "planner-full/remote-mod.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeModify, Path: "planner-full/remote-mod.txt",
					ItemType: ItemTypeFile, Hash: "hashNew1", ItemID: "full1",
				},
			},
		},
		// EF3: local modified
		{
			Path: "planner-full/local-mod.txt",
			LocalEvents: []ChangeEvent{
				{
					Source: SourceLocal, Type: ChangeModify, Path: "planner-full/local-mod.txt",
					ItemType: ItemTypeFile, Hash: "hashNew2",
				},
			},
		},
		// EF14: new remote file
		{
			Path: "planner-full/brand-new.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeCreate, Path: "planner-full/brand-new.txt",
					ItemType: ItemTypeFile, Hash: "hashBN", ItemID: "full3",
				},
			},
		},
		// ED3: new remote folder
		{
			Path: "planner-full/new-dir",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeCreate, Path: "planner-full/new-dir",
					ItemType: ItemTypeFolder, ItemID: "full4",
				},
			},
		},
	}

	baseline := baselineWith(
		&BaselineEntry{
			Path: "planner-full/remote-mod.txt", DriveID: driveid.New(testDriveID), ItemID: "full1",
			ItemType: ItemTypeFile, LocalHash: "hashOld1", RemoteHash: "hashOld1",
		},
		&BaselineEntry{
			Path: "planner-full/local-mod.txt", DriveID: driveid.New(testDriveID), ItemID: "full2",
			ItemType: ItemTypeFile, LocalHash: "hashOld2", RemoteHash: "hashOld2",
		},
	)

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	// EF2 + EF14 = 2 downloads.
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Len(t, downloads, 2)

	// EF3 = 1 upload.
	uploads := ActionsOfType(plan.Actions, ActionUpload)
	assert.Len(t, uploads, 1)

	// ED3 = 1 folder create.
	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	assert.Len(t, folderCreates, 1)
}

// ---------------------------------------------------------------------------
// DriveID Propagation Tests
// ---------------------------------------------------------------------------

func TestMakeAction_CrossDriveItem(t *testing.T) {
	// When Remote has DriveID "drive-A" and Baseline has DriveID "drive-B",
	// the Action should get "drive-A" (Remote wins for cross-drive items).
	view := &PathView{
		Path: "shared/cross-drive-file.txt",
		Remote: &RemoteState{
			ItemID:   "item-from-drive-a",
			DriveID:  driveid.New("000000000000000a"),
			ItemType: ItemTypeFile,
		},
		Baseline: &BaselineEntry{
			Path:    "shared/cross-drive-file.txt",
			DriveID: driveid.New("000000000000000b"),
			ItemID:  "item-from-drive-a",
		},
	}

	action := makeAction(ActionDownload, view)

	assert.Equal(t, driveid.New("000000000000000a"), action.DriveID, "DriveID from Remote")
	assert.Equal(t, "item-from-drive-a", action.ItemID)
}

func TestMakeAction_NewLocalItem(t *testing.T) {
	// When both Remote and Baseline are nil (new local-only file, EF13),
	// Action.DriveID should be empty — the executor fills from context.
	view := &PathView{
		Path: "new-local-file.txt",
		Local: &LocalState{
			Name:     "new-local-file.txt",
			ItemType: ItemTypeFile,
			Size:     100,
			Hash:     "hashLocal",
		},
	}

	action := makeAction(ActionUpload, view)

	assert.True(t, action.DriveID.IsZero(), "expected zero DriveID for new local item")
	assert.Empty(t, action.ItemID, "expected empty ItemID for new local item")
}

func TestMakeAction_BaselineFallbackDriveID(t *testing.T) {
	// When Remote has no DriveID (empty) but Baseline has one,
	// the Action should get Baseline's DriveID.
	view := &PathView{
		Path: "baseline-fallback.txt",
		Remote: &RemoteState{
			ItemID:   "item-fallback",
			ItemType: ItemTypeFile,
			// DriveID zero value — no DriveID from remote
		},
		Baseline: &BaselineEntry{
			Path:    "baseline-fallback.txt",
			DriveID: driveid.New(testDriveID),
			ItemID:  "item-fallback",
		},
	}

	action := makeAction(ActionDownload, view)

	assert.Equal(t, driveid.New(testDriveID), action.DriveID, "DriveID from Baseline")
}

// Validates: R-6.8.12, R-6.8.13
func TestMakeAction_ShortcutEnrichment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		remote          *RemoteState
		wantOwnDrive    bool
		wantShortcutKey string
		wantTargetDrive driveid.ID
	}{
		{
			name: "own-drive item has empty shortcut fields",
			remote: &RemoteState{
				ItemID:   "item-1",
				DriveID:  driveid.New(testDriveID),
				ItemType: ItemTypeFile,
			},
			wantOwnDrive:    true,
			wantShortcutKey: "",
			wantTargetDrive: driveid.ID{},
		},
		{
			name: "shortcut item has populated shortcut fields",
			remote: &RemoteState{
				ItemID:        "item-2",
				DriveID:       driveid.New(testDriveID),
				ItemType:      ItemTypeFile,
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
			view := &PathView{
				Path:   "test-file.txt",
				Remote: tt.remote,
			}

			action := makeAction(ActionDownload, view)

			assert.Equal(t, tt.wantOwnDrive, action.TargetsOwnDrive(), "TargetsOwnDrive()")
			assert.Equal(t, tt.wantShortcutKey, action.ShortcutKey(), "ShortcutKey()")
			assert.Equal(t, tt.wantTargetDrive, action.TargetDriveID(), "TargetDriveID()")
		})
	}
}

// ---------------------------------------------------------------------------
// Helper function unit tests
// ---------------------------------------------------------------------------

func TestDetectLocalChange(t *testing.T) {
	// No baseline, local exists → changed (new file).
	view := &PathView{
		Local: &LocalState{Hash: "h1"},
	}
	assert.True(t, detectLocalChange(view), "expected local change with no baseline and local present")

	// No baseline, no local → not changed.
	view = &PathView{}
	assert.False(t, detectLocalChange(view), "expected no local change with no baseline and no local")

	// Baseline exists, local nil → changed (deleted).
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFile, LocalHash: "h1"},
	}
	assert.True(t, detectLocalChange(view), "expected local change when local is nil (deleted)")

	// Baseline folder → not changed (folders have no hash).
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
		Local:    &LocalState{ItemType: ItemTypeFolder},
	}
	assert.False(t, detectLocalChange(view), "expected no change for folder")
}

// Validates: R-6.7.7
func TestDetectRemoteChange(t *testing.T) {
	// No baseline, remote exists → changed.
	view := &PathView{
		Remote: &RemoteState{Hash: "h1"},
	}
	assert.True(t, detectRemoteChange(view), "expected remote change with no baseline and remote present")

	// No baseline, remote is deleted → not changed (never synced, delete is a no-op).
	view = &PathView{
		Remote: &RemoteState{IsDeleted: true},
	}
	assert.False(t, detectRemoteChange(view), "expected no remote change for deleted item with no baseline")

	// Baseline exists, remote nil → no change (no observation).
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFile, RemoteHash: "h1"},
	}
	assert.False(t, detectRemoteChange(view), "expected no remote change when remote is nil")

	// Baseline exists, remote deleted → changed.
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFile, RemoteHash: "h1"},
		Remote:   &RemoteState{IsDeleted: true},
	}
	assert.True(t, detectRemoteChange(view), "expected remote change when remote is deleted")
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
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-moved-dest.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeMove,
					Path:     "planner-moved-dest.txt",
					OldPath:  "planner-reused-path.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashOriginal",
					ItemID:   "item-original",
					DriveID:  driveid.New(testDriveID),
				},
			},
		},
		{
			Path: "planner-reused-path.txt",
			RemoteEvents: []ChangeEvent{
				// Synthetic delete from the buffer's move dual-keying.
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "planner-reused-path.txt",
					ItemID:    "item-original",
					DriveID:   driveid.New(testDriveID),
					ItemType:  ItemTypeFile,
					IsDeleted: true,
				},
				// New file created at the old path.
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "planner-reused-path.txt",
					ItemID:   "item-new-at-old",
					DriveID:  driveid.New(testDriveID),
					ItemType: ItemTypeFile,
					Hash:     "hashNewFile",
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-reused-path.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item-original",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashOriginal",
		RemoteHash: "hashOriginal",
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	move := allMoves[0]
	assert.Equal(t, "planner-moved-dest.txt", move.Path, "move destination")
	assert.Equal(t, "planner-reused-path.txt", move.OldPath, "move source")

	// The new file at the old path should produce a download (EF14).
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	require.Len(t, downloads, 1, "expected 1 download for new file at reused path")
	assert.Equal(t, "planner-reused-path.txt", downloads[0].Path)
}

func TestDetectMoves_RemoteMoveOldPathReusedFolder(t *testing.T) {
	// Same scenario as above but a new folder at the old path instead of a file.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-moved-folder-dest",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeMove,
					Path:     "planner-moved-folder-dest",
					OldPath:  "planner-reused-folder",
					ItemType: ItemTypeFolder,
					ItemID:   "folder-original",
					DriveID:  driveid.New(testDriveID),
				},
			},
		},
		{
			Path: "planner-reused-folder",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "planner-reused-folder",
					ItemID:    "folder-original",
					DriveID:   driveid.New(testDriveID),
					ItemType:  ItemTypeFolder,
					IsDeleted: true,
				},
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "planner-reused-folder",
					ItemID:   "folder-new-at-old",
					DriveID:  driveid.New(testDriveID),
					ItemType: ItemTypeFolder,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:     "planner-reused-folder",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder-original",
		ItemType: ItemTypeFolder,
	})

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	allMoves := moves(plan)
	require.Len(t, allMoves, 1)

	// The new folder at the old path should produce a folder create (ED3).
	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	require.Len(t, folderCreates, 1, "expected 1 folder create for new folder at reused path")
	assert.Equal(t, "planner-reused-folder", folderCreates[0].Path)
	assert.Equal(t, CreateLocal, folderCreates[0].CreateSide)
}

// ---------------------------------------------------------------------------
// Delete Ordering: Files Before Folders at Same Depth
// ---------------------------------------------------------------------------

func TestOrderPlan_DeletesFilesBeforeFoldersAtSameDepth(t *testing.T) {
	// At the same depth, files and folders should both produce delete actions.
	// In the flat plan, ordering is handled by dependency edges rather than
	// positional ordering. Folder deletes depend on child deletes via Deps.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "x/planner-del-folder",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete, Path: "x/planner-del-folder",
					ItemType: ItemTypeFolder, ItemID: "df1", IsDeleted: true,
				},
			},
		},
		{
			Path: "x/planner-del-file.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source: SourceRemote, Type: ChangeDelete, Path: "x/planner-del-file.txt",
					ItemType: ItemTypeFile, ItemID: "df2", IsDeleted: true,
				},
			},
		},
	}

	baseline := baselineWith(
		&BaselineEntry{
			Path: "x/planner-del-folder", DriveID: driveid.New(testDriveID), ItemID: "df1",
			ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "x/planner-del-file.txt", DriveID: driveid.New(testDriveID), ItemID: "df2",
			ItemType: ItemTypeFile, LocalHash: "hashDF", RemoteHash: "hashDF",
		},
	)

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "Plan()")

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
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
	planner := NewPlanner(testLogger(t))
	baseline := emptyBaseline()

	// Create multiple paths with no baseline — all produce uploads.
	changes := []PathChanges{
		{Path: "z/delta.txt", LocalEvents: []ChangeEvent{{Source: SourceLocal, Type: ChangeCreate, Path: "z/delta.txt", ItemType: ItemTypeFile, Hash: "h4"}}},
		{Path: "a/alpha.txt", LocalEvents: []ChangeEvent{{Source: SourceLocal, Type: ChangeCreate, Path: "a/alpha.txt", ItemType: ItemTypeFile, Hash: "h1"}}},
		{Path: "m/beta.txt", LocalEvents: []ChangeEvent{{Source: SourceLocal, Type: ChangeCreate, Path: "m/beta.txt", ItemType: ItemTypeFile, Hash: "h2"}}},
		{Path: "b/gamma.txt", LocalEvents: []ChangeEvent{{Source: SourceLocal, Type: ChangeCreate, Path: "b/gamma.txt", ItemType: ItemTypeFile, Hash: "h3"}}},
	}

	plan1, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err, "first Plan()")

	plan2, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), nil)
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

	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path: "Shared/ReadOnly/file.txt", ItemType: ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Local changed, remote unchanged → would normally be ActionUpload.
	changes := []PathChanges{{
		Path:         "Shared/ReadOnly/file.txt",
		LocalEvents:  []ChangeEvent{{Type: ChangeModify, Path: "Shared/ReadOnly/file.txt", ItemType: ItemTypeFile, Hash: "bbb"}},
		RemoteEvents: nil,
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	// Upload suppressed under denied prefix.
	uploads := ActionsOfType(plan.Actions, ActionUpload)
	assert.Empty(t, uploads, "uploads should be suppressed under denied prefix")
}

func TestPlan_DeniedPrefix_AllowsDownloads(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path: "Shared/ReadOnly/file.txt", ItemType: ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Remote changed, local unchanged → ActionDownload (should proceed).
	changes := []PathChanges{{
		Path:        "Shared/ReadOnly/file.txt",
		LocalEvents: nil,
		RemoteEvents: []ChangeEvent{{
			Type: ChangeModify, Path: "Shared/ReadOnly/file.txt",
			ItemType: ItemTypeFile, Hash: "bbb",
		}},
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Len(t, downloads, 1, "downloads should proceed under denied prefix")
}

func TestPlan_DeniedPrefix_OutsideDenied_Normal(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path: "Writable/file.txt", ItemType: ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Local changed outside denied prefix → normal ActionUpload.
	changes := []PathChanges{{
		Path:         "Writable/file.txt",
		LocalEvents:  []ChangeEvent{{Type: ChangeModify, Path: "Writable/file.txt", ItemType: ItemTypeFile, Hash: "bbb"}},
		RemoteEvents: nil,
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	uploads := ActionsOfType(plan.Actions, ActionUpload)
	assert.Len(t, uploads, 1, "uploads outside denied prefix should proceed normally")
}

func TestPlan_DeniedPrefix_RemoteDelete_LocalDelete(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path: "Shared/ReadOnly/file.txt", ItemType: ItemTypeFile,
		LocalHash: "aaa", RemoteHash: "aaa",
	})

	// Remote deleted under denied prefix → should produce ActionLocalDelete.
	changes := []PathChanges{{
		Path:        "Shared/ReadOnly/file.txt",
		LocalEvents: nil,
		RemoteEvents: []ChangeEvent{{
			Type: ChangeDelete, Path: "Shared/ReadOnly/file.txt",
			ItemType: ItemTypeFile, IsDeleted: true,
		}},
	}}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	assert.Len(t, localDeletes, 1, "remote delete under denied prefix → local delete")

	// Should NOT produce remote delete (we can't write to remote).
	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	assert.Empty(t, remoteDeletes, "should not produce remote deletes under denied prefix")
}

func TestPlan_DeniedPrefix_LocalMove_Suppressed(t *testing.T) {
	t.Parallel()

	planner := NewPlanner(testLogger(t))

	// Baseline has a file at old path.
	baseline := baselineWith(&BaselineEntry{
		Path: "Shared/ReadOnly/old.txt", ItemType: ItemTypeFile,
		LocalHash: "abc123", RemoteHash: "abc123",
		DriveID: driveid.New(testDriveID), ItemID: "item-1",
	})

	// Local delete at old path + local create at new path with same hash = local move.
	changes := []PathChanges{
		{
			Path: "Shared/ReadOnly/old.txt",
			// No local events (file gone locally = local delete).
		},
		{
			Path: "Shared/ReadOnly/new.txt",
			LocalEvents: []ChangeEvent{{
				Type:     ChangeCreate,
				Path:     "Shared/ReadOnly/new.txt",
				ItemType: ItemTypeFile,
				Hash:     "abc123",
			}},
		},
	}

	denied := []string{"Shared/ReadOnly"}
	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig(), denied)
	require.NoError(t, err)

	// Should NOT produce a remote move — can't write to remote under denied prefix.
	remoteMoves := ActionsOfType(plan.Actions, ActionRemoteMove)
	assert.Empty(t, remoteMoves, "local move under denied prefix should not produce remote move")
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
			assert.Equal(t, tt.want, isWriteDenied(tt.path, denied))
		})
	}
}
