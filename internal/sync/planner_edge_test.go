package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// §3: Safety Invariant Tests
// ---------------------------------------------------------------------------

// TestS1_NoRemoteDeleteWithoutBaseline validates Safety Invariant S1:
// a file absent locally with NO baseline entry must NOT produce a
// ActionRemoteDelete. Only baseline-tracked items can propagate deletes.
func TestS1_NoRemoteDeleteWithoutBaseline(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	// Remote has a file, local has nothing, NO baseline.
	// This should classify as EF14 (download), not a delete.
	changes := []PathChanges{
		{
			Path: "remote-only.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "remote-only.txt",
					ItemType: ItemTypeFile,
					Hash:     "hash1",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	for _, a := range plan.Actions {
		assert.NotEqual(t, ActionRemoteDelete, a.Type,
			"S1: must not produce remote delete without baseline")
	}

	// Should produce a download instead.
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Len(t, downloads, 1)
}

// TestS5_BigDeleteThresholdBoundary validates Safety Invariant S5:
// big-delete protection triggers at the threshold boundary.
func TestS5_BigDeleteThresholdBoundary(t *testing.T) {
	// 10 items in baseline, 50% threshold = 5 deletes triggers protection.
	baseline := emptyBaseline()
	for i := range 10 {
		entry := &BaselineEntry{
			Path:      "file" + string(rune('a'+i)) + ".txt",
			DriveID:   driveid.New(testDriveID),
			ItemID:    "item-" + string(rune('a'+i)),
			ItemType:  ItemTypeFile,
			LocalHash: "hash-" + string(rune('a'+i)),
		}
		baseline.Put(entry)
	}

	config := DefaultSafetyConfig()

	t.Run("at_threshold_blocked", func(t *testing.T) {
		// 6 deletes out of 10 = 60% > 50% threshold.
		assert.True(t, bigDeleteTriggered(nil, 6, baseline, config))
	})

	t.Run("below_threshold_allowed", func(t *testing.T) {
		// 4 deletes out of 10 = 40% < 50% threshold.
		assert.False(t, bigDeleteTriggered(nil, 4, baseline, config))
	})

	t.Run("exactly_at_threshold_allowed", func(t *testing.T) {
		// 5 deletes out of 10 = exactly 50% — NOT greater than, so allowed.
		assert.False(t, bigDeleteTriggered(nil, 5, baseline, config))
	})
}

// TestS5_BigDeleteMinimumGuard validates that the big-delete check does
// not apply when the baseline has fewer items than the minimum threshold.
func TestS5_BigDeleteMinimumGuard(t *testing.T) {
	// 5 items in baseline (below default min of 10).
	baseline := emptyBaseline()
	for i := range 5 {
		entry := &BaselineEntry{
			Path:      "file" + string(rune('a'+i)) + ".txt",
			DriveID:   driveid.New(testDriveID),
			ItemID:    "item-" + string(rune('a'+i)),
			ItemType:  ItemTypeFile,
			LocalHash: "hash-" + string(rune('a'+i)),
		}
		baseline.Put(entry)
	}

	config := DefaultSafetyConfig()

	// 3 deletes out of 5 = 60%, but below minimum items threshold.
	assert.False(t, bigDeleteTriggered(nil, 3, baseline, config),
		"S5: big-delete should not trigger below minimum items")
}

// TestS5_BigDeleteMaxCount validates that the absolute count threshold
// blocks even when percentage is low.
func TestS5_BigDeleteMaxCount(t *testing.T) {
	config := &SafetyConfig{
		BigDeleteMinItems:   5,
		BigDeleteMaxCount:   10,
		BigDeleteMaxPercent: 90.0,
	}

	// 100 items in baseline, 11 deletes = 11% (under 90%), but exceeds max count of 10.
	baseline := emptyBaseline()
	for i := range 100 {
		entry := &BaselineEntry{
			Path:      "f" + string(rune(i)) + ".txt",
			DriveID:   driveid.New(testDriveID),
			ItemID:    "i" + string(rune(i)),
			ItemType:  ItemTypeFile,
			LocalHash: "h" + string(rune(i)),
		}
		baseline.Put(entry)
	}

	assert.True(t, bigDeleteTriggered(nil, 11, baseline, config),
		"S5: should trigger when exceeding max count")
	assert.False(t, bigDeleteTriggered(nil, 10, baseline, config),
		"S5: should not trigger at exactly max count")
}

// TestS7_PartialFilesNeverUploaded validates Safety Invariant S7:
// .partial files are excluded by the local observer and should never
// appear in planner output. This test validates the exclusion function.
func TestS7_PartialFilesNeverUploaded(t *testing.T) {
	excludedNames := []string{
		"download.partial",
		"document.tmp",
		"editor.swp",
		"chrome.crdownload",
		"data.db-wal",
		"data.db-shm",
		"sync.db",
		"~backup.txt",
		".~lock.office",
	}

	for _, name := range excludedNames {
		assert.True(t, isAlwaysExcluded(name),
			"S7: %q should be excluded", name)
	}
}

// TestS7_TempFilesFilteredSymmetrically validates that temp file exclusion
// applies to both potential upload and download paths.
func TestS7_TempFilesFilteredSymmetrically(t *testing.T) {
	// These should be excluded from both sides.
	names := []string{
		"~$document.docx",
		"file.tmp",
		"file.swp",
	}

	for _, name := range names {
		excluded := isAlwaysExcluded(name) || !isValidOneDriveName(name)
		assert.True(t, excluded,
			"S7: %q should be excluded from sync", name)
	}
}

// ---------------------------------------------------------------------------
// §4: Decision Matrix Edge Cases
// ---------------------------------------------------------------------------

// TestEF6_LocalDeletedImpliesLocalChanged validates that a locally deleted
// file (baseline exists, local absent) is correctly classified as EF6
// (remote delete propagation), not stolen by EF3 (local changed).
func TestEF6_LocalDeletedImpliesLocalChanged(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path:       "deleted.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	// No local events (file absent), no remote events (unchanged).
	changes := []PathChanges{
		{
			Path: "deleted.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "deleted.txt",
					ItemType:  ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1, "EF6: local delete should propagate as remote delete")
}

// TestEF4_ConvergentEdit_NoTransfer validates that when both sides
// independently edit to the same hash, the result is ActionUpdateSynced
// with no data transfer.
func TestEF4_ConvergentEdit_NoTransfer(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path:       "converge.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "oldHash",
		RemoteHash: "oldHash",
	})

	changes := []PathChanges{
		{
			Path: "converge.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeModify,
					Path:     "converge.txt",
					ItemType: ItemTypeFile,
					Hash:     "newHash",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "converge.txt",
					ItemType: ItemTypeFile,
					Hash:     "newHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	synced := ActionsOfType(plan.Actions, ActionUpdateSynced)
	assert.Len(t, synced, 1, "EF4: convergent edit should produce update_synced")

	downloads := ActionsOfType(plan.Actions, ActionDownload)
	uploads := ActionsOfType(plan.Actions, ActionUpload)
	assert.Empty(t, downloads, "EF4: no download needed for convergent edit")
	assert.Empty(t, uploads, "EF4: no upload needed for convergent edit")
}

// TestEF11_ConvergentCreate validates that files appearing on both sides
// with the same hash are adopted (ActionUpdateSynced) without transfer.
func TestEF11_ConvergentCreate(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "newfile.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "newfile.txt",
					ItemType: ItemTypeFile,
					Hash:     "sameHash",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "newfile.txt",
					ItemType: ItemTypeFile,
					Hash:     "sameHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	synced := ActionsOfType(plan.Actions, ActionUpdateSynced)
	assert.Len(t, synced, 1, "EF11: convergent create should adopt")
}

// TestEF12_DivergentCreate validates that files appearing on both sides
// with different hashes produce a create-create conflict.
func TestEF12_DivergentCreate(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	changes := []PathChanges{
		{
			Path: "newfile.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "newfile.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashA",
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
				},
			},
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "newfile.txt",
					ItemType: ItemTypeFile,
					Hash:     "hashB",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	assert.Len(t, conflicts, 1, "EF12: divergent create should conflict")
	assert.Equal(t, ConflictCreateCreate, conflicts[0].ConflictInfo.ConflictType)
}

// TestEF9_EditDeleteAutoResolve validates that local edit + remote delete
// produces a conflict with edit-delete type.
func TestEF9_EditDeleteAutoResolve(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path:       "edited.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "oldHash",
		RemoteHash: "oldHash",
	})

	changes := []PathChanges{
		{
			Path: "edited.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:    SourceRemote,
					Type:      ChangeDelete,
					Path:      "edited.txt",
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
					Path:     "edited.txt",
					ItemType: ItemTypeFile,
					Hash:     "newHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	conflicts := ActionsOfType(plan.Actions, ActionConflict)
	assert.Len(t, conflicts, 1, "EF9: local edit + remote delete = conflict")
	assert.Equal(t, ConflictEditDelete, conflicts[0].ConflictInfo.ConflictType)
}

// TestUploadOnlyMode_ProducesRemoteDeletes validates that upload-only mode
// with locally deleted files produces ActionRemoteDelete (EF6), not
// EF10 cleanup.
func TestUploadOnlyMode_ProducesRemoteDeletes(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path:       "gone.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	changes := []PathChanges{
		{
			Path: "gone.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "gone.txt",
					ItemType:  ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncUploadOnly, DefaultSafetyConfig())
	require.NoError(t, err)

	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1,
		"upload-only: local delete should produce remote delete")
}

// TestDownloadOnlyMode_SkipsLocalCorruption validates that download-only
// mode with a corrupted local file but unchanged remote produces no action.
func TestDownloadOnlyMode_SkipsLocalCorruption(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path:       "corrupted.txt",
		DriveID:    driveid.New(testDriveID),
		ItemID:     "item1",
		ItemType:   ItemTypeFile,
		LocalHash:  "goodHash",
		RemoteHash: "goodHash",
	})

	// Local has different hash (corrupted), but in download-only mode
	// local changes are suppressed.
	changes := []PathChanges{
		{
			Path: "corrupted.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeModify,
					Path:     "corrupted.txt",
					ItemType: ItemTypeFile,
					Hash:     "corruptedHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncDownloadOnly, DefaultSafetyConfig())
	require.NoError(t, err)

	// In download-only mode, local changes are suppressed. Since remote
	// didn't change, this is EF1 (no-op).
	assert.Empty(t, plan.Actions,
		"download-only: corrupted local with unchanged remote = no action")
}

// TestED8_FolderModeFilteringRegression validates that folder classifiers
// use upfront mode filtering. ED8 (locally deleted folder, no remote observation,
// no remote deletion) should produce ActionRemoteDelete in bidirectional mode.
func TestED8_FolderModeFilteringRegression(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path:     "photos",
		DriveID:  driveid.New(testDriveID),
		ItemID:   "folder1",
		ItemType: ItemTypeFolder,
	})

	changes := []PathChanges{
		{
			Path: "photos",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "photos",
					ItemType:  ItemTypeFolder,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	remoteDeletes := ActionsOfType(plan.Actions, ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1,
		"ED8: locally deleted folder should produce remote delete")
}

// ---------------------------------------------------------------------------
// §5: Move Detection Edge Cases
// ---------------------------------------------------------------------------

// TestMoveDetection_AmbiguousSameHashMultipleDeletes validates that when
// two deleted files share the same hash, no move is detected (ambiguous).
func TestMoveDetection_AmbiguousSameHashMultipleDeletes(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(
		&BaselineEntry{
			Path: "dir1/file.txt", DriveID: driveid.New(testDriveID),
			ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "sameHash", RemoteHash: "sameHash",
		},
		&BaselineEntry{
			Path: "dir2/file.txt", DriveID: driveid.New(testDriveID),
			ItemID: "item2", ItemType: ItemTypeFile, LocalHash: "sameHash", RemoteHash: "sameHash",
		},
	)

	// Both files deleted locally, one file created at new path.
	changes := []PathChanges{
		{
			Path: "dir1/file.txt",
			LocalEvents: []ChangeEvent{
				{Source: SourceLocal, Type: ChangeDelete, Path: "dir1/file.txt", ItemType: ItemTypeFile, IsDeleted: true},
			},
		},
		{
			Path: "dir2/file.txt",
			LocalEvents: []ChangeEvent{
				{Source: SourceLocal, Type: ChangeDelete, Path: "dir2/file.txt", ItemType: ItemTypeFile, IsDeleted: true},
			},
		},
		{
			Path: "dir3/file.txt",
			LocalEvents: []ChangeEvent{
				{Source: SourceLocal, Type: ChangeCreate, Path: "dir3/file.txt", ItemType: ItemTypeFile, Hash: "sameHash"},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	// Ambiguous: two deletes with same hash → no move detected.
	mvs := moves(plan)
	assert.Empty(t, mvs, "ambiguous move: multiple deletes should prevent move detection")
}

// TestMoveDetection_AmbiguousSameHashMultipleCreates validates that when
// two created files share the same hash as one deleted file, no move is detected.
func TestMoveDetection_AmbiguousSameHashMultipleCreates(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(
		&BaselineEntry{
			Path: "original.txt", DriveID: driveid.New(testDriveID),
			ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "sameHash", RemoteHash: "sameHash",
		},
	)

	changes := []PathChanges{
		{
			Path: "original.txt",
			LocalEvents: []ChangeEvent{
				{Source: SourceLocal, Type: ChangeDelete, Path: "original.txt", ItemType: ItemTypeFile, IsDeleted: true},
			},
		},
		{
			Path: "copy1.txt",
			LocalEvents: []ChangeEvent{
				{Source: SourceLocal, Type: ChangeCreate, Path: "copy1.txt", ItemType: ItemTypeFile, Hash: "sameHash"},
			},
		},
		{
			Path: "copy2.txt",
			LocalEvents: []ChangeEvent{
				{Source: SourceLocal, Type: ChangeCreate, Path: "copy2.txt", ItemType: ItemTypeFile, Hash: "sameHash"},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	// Ambiguous: one delete, two creates with same hash → no move detected.
	mvs := moves(plan)
	assert.Empty(t, mvs, "ambiguous move: multiple creates should prevent move detection")
}

// TestRemoteMove_WithRecreationAtSource validates that a remote move A→B
// combined with a new item at path A is handled correctly: B gets the
// move action, and A is classified as a new item.
func TestRemoteMove_WithRecreationAtSource(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	baseline := baselineWith(&BaselineEntry{
		Path: "source.txt", DriveID: driveid.New(testDriveID),
		ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "hashA", RemoteHash: "hashA",
	})

	changes := []PathChanges{
		{
			// The move event's Path is the destination.
			Path: "dest.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeMove,
					Path:     "dest.txt",
					OldPath:  "source.txt",
					ItemType: ItemTypeFile,
					ItemID:   "item1",
					DriveID:  driveid.New(testDriveID),
					Hash:     "hashA",
				},
			},
		},
		{
			// A new item appears at the old path.
			Path: "source.txt",
			RemoteEvents: []ChangeEvent{
				{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     "source.txt",
					ItemType: ItemTypeFile,
					ItemID:   "item2",
					DriveID:  driveid.New(testDriveID),
					Hash:     "hashB",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err)

	// Should have a local move for the renamed item.
	localMoves := ActionsOfType(plan.Actions, ActionLocalMove)
	assert.Len(t, localMoves, 1, "should have a local move action")

	if len(localMoves) > 0 {
		assert.Equal(t, "dest.txt", localMoves[0].Path)
		assert.Equal(t, "source.txt", localMoves[0].OldPath)
	}

	// The new item at source.txt should be classified as a download (EF14).
	downloads := ActionsOfType(plan.Actions, ActionDownload)
	assert.Len(t, downloads, 1, "new item at old path should be downloaded")
}

// ---------------------------------------------------------------------------
// §4: Dependency graph tests
// ---------------------------------------------------------------------------

// TestBuildDependencies_FolderCreateBeforeChild validates that a child
// download depends on its parent folder create.
func TestBuildDependencies_FolderCreateBeforeChild(t *testing.T) {
	actions := []Action{
		{Type: ActionFolderCreate, Path: "newdir", CreateSide: CreateLocal},
		{Type: ActionDownload, Path: "newdir/file.txt"},
	}

	deps := buildDependencies(actions)

	// Action 1 (download) should depend on action 0 (folder create).
	require.Len(t, deps, 2)
	assert.Contains(t, deps[1], 0, "download should depend on parent folder create")
	assert.Empty(t, deps[0], "folder create should have no dependencies")
}

// TestBuildDependencies_ChildDeleteBeforeParent validates that a parent
// folder delete depends on child deletes completing first.
func TestBuildDependencies_ChildDeleteBeforeParent(t *testing.T) {
	actions := []Action{
		{
			Type: ActionLocalDelete, Path: "dir",
			View: &PathView{
				Baseline: &BaselineEntry{ItemType: ItemTypeFolder},
			},
		},
		{
			Type: ActionLocalDelete, Path: "dir/file.txt",
			View: &PathView{
				Baseline: &BaselineEntry{ItemType: ItemTypeFile},
			},
		},
	}

	deps := buildDependencies(actions)

	// Action 0 (parent delete) should depend on action 1 (child delete).
	require.Len(t, deps, 2)
	assert.Contains(t, deps[0], 1, "parent folder delete should depend on child delete")
}

// ---------------------------------------------------------------------------
// §3: Planner-level safety check integration
// ---------------------------------------------------------------------------

// TestPlan_BigDeleteBlocked validates that the planner returns
// ErrBigDeleteTriggered when planned deletions exceed safety thresholds.
func TestPlan_BigDeleteBlocked(t *testing.T) {
	planner := NewPlanner(testLogger(t))

	// Create 10-item baseline.
	baseline := emptyBaseline()
	var changes []PathChanges

	for i := range 10 {
		path := "file" + string(rune('a'+i)) + ".txt"
		entry := &BaselineEntry{
			Path:       path,
			DriveID:    driveid.New(testDriveID),
			ItemID:     "item-" + string(rune('a'+i)),
			ItemType:   ItemTypeFile,
			LocalHash:  "hash",
			RemoteHash: "hash",
		}
		baseline.Put(entry)
	}

	// Delete 6 out of 10 (60% > 50% threshold).
	for i := range 6 {
		path := "file" + string(rune('a'+i)) + ".txt"
		changes = append(changes, PathChanges{
			Path: path,
			LocalEvents: []ChangeEvent{
				{Source: SourceLocal, Type: ChangeDelete, Path: path, ItemType: ItemTypeFile, IsDeleted: true},
			},
		})
	}

	_, err := planner.Plan(changes, baseline, SyncBidirectional, DefaultSafetyConfig())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBigDeleteTriggered)
}
