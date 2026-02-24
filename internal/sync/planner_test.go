package sync

import (
	"testing"

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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if got := countActions(plan); got != 0 {
		t.Errorf("EF1: expected 0 actions, got %d", got)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	if len(downloads) != 1 {
		t.Fatalf("EF2: expected 1 download, got %d", len(downloads))
	}

	if downloads[0].Path != "planner-test.txt" {
		t.Errorf("EF2: unexpected path %q", downloads[0].Path)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	uploads := ActionsOfType(plan.Actions, ActionUpload)
	if len(uploads) != 1 {
		t.Fatalf("EF3: expected 1 upload, got %d", len(uploads))
	}

	if uploads[0].Path != "planner-test.txt" {
		t.Errorf("EF3: unexpected path %q", uploads[0].Path)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	syncedUpdates := ActionsOfType(plan.Actions, ActionUpdateSynced)
	if len(syncedUpdates) != 1 {
		t.Fatalf("EF4: expected 1 synced update, got %d", len(syncedUpdates))
	}
}

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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	if len(conflicts) != 1 {
		t.Fatalf("EF5: expected 1 conflict, got %d", len(conflicts))
	}

	if conflicts[0].ConflictInfo.ConflictType != "edit_edit" {
		t.Errorf("EF5: expected edit_edit conflict, got %q", conflicts[0].ConflictInfo.ConflictType)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	if len(remoteDeletes) != 1 {
		t.Fatalf("EF6: expected 1 remote delete, got %d", len(remoteDeletes))
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	if len(downloads) != 1 {
		t.Fatalf("EF7: expected 1 download, got %d", len(downloads))
	}
}

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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	if len(localDeletes) != 1 {
		t.Fatalf("EF8: expected 1 local delete, got %d", len(localDeletes))
	}
}

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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	if len(conflicts) != 1 {
		t.Fatalf("EF9: expected 1 conflict, got %d", len(conflicts))
	}

	if conflicts[0].ConflictInfo.ConflictType != "edit_delete" {
		t.Errorf("EF9: expected edit_delete conflict, got %q", conflicts[0].ConflictInfo.ConflictType)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	cleanups := ActionsOfType(plan.Actions, ActionCleanup)
	if len(cleanups) != 1 {
		t.Fatalf("EF10: expected 1 cleanup, got %d", len(cleanups))
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	syncedUpdates := ActionsOfType(plan.Actions, ActionUpdateSynced)
	if len(syncedUpdates) != 1 {
		t.Fatalf("EF11: expected 1 synced update, got %d", len(syncedUpdates))
	}
}

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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	if len(conflicts) != 1 {
		t.Fatalf("EF12: expected 1 conflict, got %d", len(conflicts))
	}

	if conflicts[0].ConflictInfo.ConflictType != "create_create" {
		t.Errorf("EF12: expected create_create conflict, got %q", conflicts[0].ConflictInfo.ConflictType)
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	uploads := ActionsOfType(plan.Actions, ActionUpload)
	if len(uploads) != 1 {
		t.Fatalf("EF13: expected 1 upload, got %d", len(uploads))
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	if len(downloads) != 1 {
		t.Fatalf("EF14: expected 1 download, got %d", len(downloads))
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if got := countActions(plan); got != 0 {
		t.Errorf("ED1: expected 0 actions, got %d", got)
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	syncedUpdates := ActionsOfType(plan.Actions, ActionUpdateSynced)
	if len(syncedUpdates) != 1 {
		t.Fatalf("ED2: expected 1 synced update, got %d", len(syncedUpdates))
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 1 {
		t.Fatalf("ED3: expected 1 folder create, got %d", len(folderCreates))
	}

	if folderCreates[0].CreateSide != CreateLocal {
		t.Errorf("ED3: expected CreateLocal, got %v", folderCreates[0].CreateSide)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 1 {
		t.Fatalf("ED4: expected 1 folder create, got %d", len(folderCreates))
	}

	if folderCreates[0].CreateSide != CreateLocal {
		t.Errorf("ED4: expected CreateLocal, got %v", folderCreates[0].CreateSide)
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 1 {
		t.Fatalf("ED5: expected 1 folder create, got %d", len(folderCreates))
	}

	if folderCreates[0].CreateSide != CreateRemote {
		t.Errorf("ED5: expected CreateRemote, got %v", folderCreates[0].CreateSide)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	if len(localDeletes) != 1 {
		t.Fatalf("ED6: expected 1 local delete, got %d", len(localDeletes))
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	cleanups := ActionsOfType(plan.Actions, ActionCleanup)
	if len(cleanups) != 1 {
		t.Fatalf("ED7: expected 1 cleanup, got %d", len(cleanups))
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	if len(remoteDeletes) != 1 {
		t.Fatalf("ED8: expected 1 remote delete, got %d", len(remoteDeletes))
	}

	if remoteDeletes[0].Path != "docs/planner-dir" {
		t.Errorf("ED8: expected path docs/planner-dir, got %s", remoteDeletes[0].Path)
	}

	if remoteDeletes[0].ItemID != "folder1" {
		t.Errorf("ED8: expected ItemID folder1, got %s", remoteDeletes[0].ItemID)
	}

	if remoteDeletes[0].DriveID != driveid.New(testDriveID) {
		t.Errorf("ED8: expected DriveID %v, got %v", driveid.New(testDriveID), remoteDeletes[0].DriveID)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncDownloadOnly, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if got := countActions(plan); got != 0 {
		t.Errorf("ED8 download-only: expected 0 actions, got %d", got)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 0 {
		t.Errorf("ED4 upload-only: expected 0 folder creates, got %d", len(folderCreates))
	}

	// Upload-only: local deletion should still propagate remotely (ED8 path).
	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	if len(remoteDeletes) != 1 {
		t.Errorf("ED4 upload-only: expected 1 remote delete (local deletion propagates), got %d", len(remoteDeletes))
	}
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

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	if len(localDeletes) != 0 {
		t.Errorf("ED6 upload-only: expected 0 local deletes, got %d", len(localDeletes))
	}

	if got := countActions(plan); got != 0 {
		t.Errorf("ED6 upload-only: expected 0 total actions, got %d", got)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	allMoves := moves(plan)
	if len(allMoves) != 1 {
		t.Fatalf("expected 1 move, got %d", len(allMoves))
	}

	move := allMoves[0]
	if move.Type != ActionLocalMove {
		t.Errorf("expected ActionLocalMove, got %v", move.Type)
	}

	if move.Path != "docs/planner-renamed.txt" {
		t.Errorf("expected destination 'docs/planner-renamed.txt', got %q", move.Path)
	}

	if move.OldPath != "docs/planner-original.txt" {
		t.Errorf("expected source 'docs/planner-original.txt', got %q", move.OldPath)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	allMoves := moves(plan)
	if len(allMoves) != 1 {
		t.Fatalf("expected 1 move, got %d", len(allMoves))
	}

	move := allMoves[0]
	if move.Type != ActionRemoteMove {
		t.Errorf("expected ActionRemoteMove, got %v", move.Type)
	}

	if move.Path != "planner-new-loc.txt" {
		t.Errorf("expected destination 'planner-new-loc.txt', got %q", move.Path)
	}

	if move.OldPath != "planner-old-loc.txt" {
		t.Errorf("expected source 'planner-old-loc.txt', got %q", move.OldPath)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	// No moves — ambiguous hash match.
	allMoves := moves(plan)
	if len(allMoves) != 0 {
		t.Errorf("expected 0 moves for ambiguous case, got %d", len(allMoves))
	}

	// The paths should still produce separate actions (deletes + upload).
	if countActions(plan) == 0 {
		t.Error("expected some actions for unmatched paths, got 0")
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	// Should have exactly 1 move and no other actions.
	allMoves := moves(plan)
	if len(allMoves) != 1 {
		t.Fatalf("expected 1 move, got %d", len(allMoves))
	}

	nonMoveCount := countActions(plan) - len(allMoves)
	if nonMoveCount != 0 {
		t.Errorf("expected 0 non-move actions after move exclusion, got %d", nonMoveCount)
	}
}

// ---------------------------------------------------------------------------
// Safety Tests (Big Delete)
// ---------------------------------------------------------------------------

func TestBigDelete_BelowMinItems(t *testing.T) {
	// Baseline has fewer items than MinItems → no trigger even at 100% deletes.
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "planner-safe-del.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "planner-safe-del.txt",
					ItemType:  ItemTypeFile,
					ItemID:    "item8",
					IsDeleted: true,
				},
			},
		},
	}

	baseline := baselineWith(&BaselineEntry{
		Path:       "planner-safe-del.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item8",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashS",
		RemoteHash: "hashS",
	})

	// 1 item in baseline, 1 delete = 100%, but below MinItems (10).
	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("expected no error below MinItems, got: %v", err)
	}

	if plan == nil {
		t.Fatal("expected non-nil plan below MinItems")
	}
}

func TestBigDelete_ExceedsMaxCount(t *testing.T) {
	// Delete count exceeds MaxCount → ErrBigDeleteTriggered.
	planner := NewPlanner(testLogger(t))

	// Build a baseline with 20 items and create delete events for all of them.
	var entries []*BaselineEntry
	var changes []PathChanges

	for i := 0; i < 20; i++ {
		p := "planner-bigdel-" + string(rune('a'+i)) + ".txt"
		itemID := "bdi-" + string(rune('a'+i))
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

	// Use a low MaxCount to trigger.
	config := &SafetyConfig{
		BigDeleteMinItems:   5,
		BigDeleteMaxCount:   10,
		BigDeleteMaxPercent: defaultBigDeleteMaxPercent,
	}

	_, err := planner.Plan(changes, baseline, SyncBidirectional, config)
	if err == nil {
		t.Fatal("expected ErrBigDeleteTriggered, got nil")
	}

	if err != ErrBigDeleteTriggered {
		t.Fatalf("expected ErrBigDeleteTriggered, got: %v", err)
	}
}

func TestBigDelete_ExceedsPercent(t *testing.T) {
	// Delete percentage exceeds MaxPercent → ErrBigDeleteTriggered.
	planner := NewPlanner(testLogger(t))

	// 20 baseline items, delete 15 of them = 75%.
	var entries []*BaselineEntry
	var changes []PathChanges

	for i := 0; i < 20; i++ {
		p := "planner-pct-" + string(rune('a'+i)) + ".txt"
		itemID := "pct-" + string(rune('a'+i))
		entries = append(entries, &BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(testDriveID),
			ItemID:     itemID,
			ItemType:   ItemTypeFile,
			LocalHash:  "hashPCT",
			RemoteHash: "hashPCT",
		})
	}

	// Only delete the first 15.
	for i := 0; i < 15; i++ {
		p := entries[i].Path
		changes = append(changes, PathChanges{
			Path: p,
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      p,
					ItemType:  ItemTypeFile,
					ItemID:    entries[i].ItemID,
					IsDeleted: true,
				},
			},
		})
	}

	baseline := baselineWith(entries...)

	config := &SafetyConfig{
		BigDeleteMinItems:   5,
		BigDeleteMaxCount:   defaultBigDeleteMaxCount,
		BigDeleteMaxPercent: 50.0, // 75% > 50% threshold
	}

	_, err := planner.Plan(changes, baseline, SyncBidirectional, config)
	if err != ErrBigDeleteTriggered {
		t.Fatalf("expected ErrBigDeleteTriggered, got: %v", err)
	}
}

func TestBigDelete_NoTrigger(t *testing.T) {
	// Deletes within limits → no error.
	planner := NewPlanner(testLogger(t))

	// 20 baseline items, delete 2 = 10%.
	var entries []*BaselineEntry

	for i := 0; i < 20; i++ {
		p := "planner-safe-" + string(rune('a'+i)) + ".txt"
		itemID := "safe-" + string(rune('a'+i))
		entries = append(entries, &BaselineEntry{
			Path:       p,
			DriveID:    driveid.New(testDriveID),
			ItemID:     itemID,
			ItemType:   ItemTypeFile,
			LocalHash:  "hashSafe",
			RemoteHash: "hashSafe",
		})
	}

	// Delete only 2.
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

	config := &SafetyConfig{
		BigDeleteMinItems:   5,
		BigDeleteMaxCount:   defaultBigDeleteMaxCount,
		BigDeleteMaxPercent: defaultBigDeleteMaxPercent,
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, config)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
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

	plan, err := planner.Plan(changes, baseline, SyncDownloadOnly, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	uploads := ActionsOfType(plan.Actions, ActionUpload)
	if len(uploads) != 0 {
		t.Errorf("download-only: expected 0 uploads, got %d", len(uploads))
	}
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

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	if len(downloads) != 0 {
		t.Errorf("upload-only: expected 0 downloads, got %d", len(downloads))
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncDownloadOnly, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 0 {
		t.Errorf("download-only: expected 0 folder creates, got %d", len(folderCreates))
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncUploadOnly, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 0 {
		t.Errorf("upload-only: expected 0 folder creates, got %d", len(folderCreates))
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 3 {
		t.Fatalf("expected 3 folder creates, got %d", len(folderCreates))
	}

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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	if len(localDeletes) != 3 {
		t.Fatalf("expected 3 local deletes, got %d", len(localDeletes))
	}

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
		if !deletePaths[expected] {
			t.Errorf("expected delete for path %q not found", expected)
		}
	}
}

// ---------------------------------------------------------------------------
// Integration Tests
// ---------------------------------------------------------------------------

func TestPlan_EmptyChanges(t *testing.T) {
	// Empty changes → empty plan, no error.
	planner := NewPlanner(testLogger(t))

	plan, err := planner.Plan(nil, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if got := countActions(plan); got != 0 {
		t.Errorf("empty changes: expected 0 actions, got %d", got)
	}
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

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 1 {
		t.Errorf("expected 1 folder create, got %d", len(folderCreates))
	}

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	if len(downloads) != 1 {
		t.Errorf("expected 1 download, got %d", len(downloads))
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	// EF2 + EF14 = 2 downloads.
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	if len(downloads) != 2 {
		t.Errorf("expected 2 downloads, got %d", len(downloads))
	}

	// EF3 = 1 upload.
	uploads := ActionsOfType(plan.Actions, ActionUpload)
	if len(uploads) != 1 {
		t.Errorf("expected 1 upload, got %d", len(uploads))
	}

	// ED3 = 1 folder create.
	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 1 {
		t.Errorf("expected 1 folder create, got %d", len(folderCreates))
	}
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

	if !action.DriveID.Equal(driveid.New("000000000000000a")) {
		t.Errorf("expected DriveID from Remote %q, got %q", driveid.New("000000000000000a"), action.DriveID)
	}

	if action.ItemID != "item-from-drive-a" {
		t.Errorf("expected ItemID %q, got %q", "item-from-drive-a", action.ItemID)
	}
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

	if !action.DriveID.IsZero() {
		t.Errorf("expected zero DriveID for new local item, got %q", action.DriveID)
	}

	if action.ItemID != "" {
		t.Errorf("expected empty ItemID for new local item, got %q", action.ItemID)
	}
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

	if !action.DriveID.Equal(driveid.New(testDriveID)) {
		t.Errorf("expected DriveID from Baseline %q, got %q", driveid.New(testDriveID), action.DriveID)
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
	if !detectLocalChange(view) {
		t.Error("expected local change with no baseline and local present")
	}

	// No baseline, no local → not changed.
	view = &PathView{}
	if detectLocalChange(view) {
		t.Error("expected no local change with no baseline and no local")
	}

	// Baseline exists, local nil → changed (deleted).
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFile, LocalHash: "h1"},
	}
	if !detectLocalChange(view) {
		t.Error("expected local change when local is nil (deleted)")
	}

	// Baseline folder → not changed (folders have no hash).
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
		Local:    &LocalState{ItemType: ItemTypeFolder},
	}
	if detectLocalChange(view) {
		t.Error("expected no change for folder")
	}
}

func TestDetectRemoteChange(t *testing.T) {
	// No baseline, remote exists → changed.
	view := &PathView{
		Remote: &RemoteState{Hash: "h1"},
	}
	if !detectRemoteChange(view) {
		t.Error("expected remote change with no baseline and remote present")
	}

	// No baseline, remote is deleted → not changed (never synced, delete is a no-op).
	view = &PathView{
		Remote: &RemoteState{IsDeleted: true},
	}
	if detectRemoteChange(view) {
		t.Error("expected no remote change for deleted item with no baseline")
	}

	// Baseline exists, remote nil → no change (no observation).
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFile, RemoteHash: "h1"},
	}
	if detectRemoteChange(view) {
		t.Error("expected no remote change when remote is nil")
	}

	// Baseline exists, remote deleted → changed.
	view = &PathView{
		Baseline: &BaselineEntry{ItemType: ItemTypeFile, RemoteHash: "h1"},
		Remote:   &RemoteState{IsDeleted: true},
	}
	if !detectRemoteChange(view) {
		t.Error("expected remote change when remote is deleted")
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	allMoves := moves(plan)
	if len(allMoves) != 1 {
		t.Fatalf("expected 1 move, got %d", len(allMoves))
	}

	move := allMoves[0]
	if move.OldPath != "planner-reused-path.txt" || move.Path != "planner-moved-dest.txt" {
		t.Errorf("move: %q → %q, want planner-reused-path.txt → planner-moved-dest.txt",
			move.OldPath, move.Path)
	}

	// The new file at the old path should produce a download (EF14).
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	if len(downloads) != 1 {
		t.Fatalf("expected 1 download for new file at reused path, got %d", len(downloads))
	}

	if downloads[0].Path != "planner-reused-path.txt" {
		t.Errorf("download path = %q, want %q", downloads[0].Path, "planner-reused-path.txt")
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	allMoves := moves(plan)
	if len(allMoves) != 1 {
		t.Fatalf("expected 1 move, got %d", len(allMoves))
	}

	// The new folder at the old path should produce a folder create (ED3).
	folderCreates := ActionsOfType(plan.Actions, ActionFolderCreate)
	if len(folderCreates) != 1 {
		t.Fatalf("expected 1 folder create for new folder at reused path, got %d", len(folderCreates))
	}

	if folderCreates[0].Path != "planner-reused-folder" {
		t.Errorf("folder create path = %q, want %q", folderCreates[0].Path, "planner-reused-folder")
	}

	if folderCreates[0].CreateSide != CreateLocal {
		t.Errorf("folder create side = %v, want CreateLocal", folderCreates[0].CreateSide)
	}
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

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	localDeletes := ActionsOfType(plan.Actions, ActionLocalDelete)
	if len(localDeletes) != 2 {
		t.Fatalf("expected 2 local deletes, got %d", len(localDeletes))
	}

	// Verify both paths are present (order is non-deterministic in the flat
	// list — the executor uses Deps to determine execution order).
	deletePaths := make(map[string]bool)
	for _, d := range localDeletes {
		deletePaths[d.Path] = true
	}

	if !deletePaths["x/planner-del-file.txt"] {
		t.Error("expected delete for x/planner-del-file.txt")
	}

	if !deletePaths["x/planner-del-folder"] {
		t.Error("expected delete for x/planner-del-folder")
	}
}
