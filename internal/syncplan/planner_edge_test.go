package syncplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// §3: Safety Invariant Tests
// ---------------------------------------------------------------------------

// Validates: R-6.2.1
// TestS1_NoRemoteDeleteWithoutBaseline validates Safety Invariant S1:
// a file absent locally with NO baseline entry must NOT produce a
// ActionRemoteDelete. Only baseline-tracked items can propagate deletes.
func TestS1_NoRemoteDeleteWithoutBaseline(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	// Remote has a file, local has nothing, NO baseline.
	// This should classify as EF14 (download), not a delete.
	changes := []synctypes.PathChanges{
		{
			Path: "remote-only.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "remote-only.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hash1",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	for _, a := range plan.Actions {
		assert.NotEqual(t, synctypes.ActionRemoteDelete, a.Type,
			"S1: must not produce remote delete without baseline")
	}

	// Should produce a download instead.
	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Len(t, downloads, 1)
}

// Validates: R-6.4.1
// TestS5_BigDeleteThresholdBoundary validates that big-delete protection
// uses a simple absolute count threshold with no percentage or per-folder checks.
func TestS5_BigDeleteThresholdBoundary(t *testing.T) {
	t.Run("above_threshold_blocked", func(t *testing.T) {
		// 11 deletes > threshold of 10 → triggered.
		assert.True(t, exceedsDeleteThreshold(11, 10))
	})

	t.Run("below_threshold_allowed", func(t *testing.T) {
		// 9 deletes < threshold of 10 → allowed.
		assert.False(t, exceedsDeleteThreshold(9, 10))
	})

	t.Run("exactly_at_threshold_allowed", func(t *testing.T) {
		// 10 deletes = threshold of 10 → NOT greater than, so allowed.
		assert.False(t, exceedsDeleteThreshold(10, 10))
	})

	t.Run("threshold_zero_disables", func(t *testing.T) {
		// Threshold of 0 disables protection entirely.
		assert.False(t, exceedsDeleteThreshold(99999, 0))
	})
}

// Validates: R-2.3
// TestS7_PartialFilesNeverUploaded validates Safety Invariant S7:
// .partial files are excluded by the local observer and should never
// appear in planner output. This test validates the exclusion function.
func TestS7_PartialFilesNeverUploaded(t *testing.T) {
	excludedNames := []string{
		"download.partial",
		"document.tmp",
		"editor.swp",
		"chrome.crdownload",
		"~backup.txt",
		".~lock.office",
	}

	for _, name := range excludedNames {
		assert.True(t, syncobserve.IsAlwaysExcluded(name),
			"S7: %q should be excluded", name)
	}
}

// Validates: R-2.3
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
		reason, _ := syncobserve.ValidateOneDriveName(name)
		excluded := syncobserve.IsAlwaysExcluded(name) || reason != ""
		assert.True(t, excluded,
			"S7: %q should be excluded from sync", name)
	}
}

// ---------------------------------------------------------------------------
// §4: Decision Matrix Edge Cases
// ---------------------------------------------------------------------------

// Validates: R-2.3
// TestEF6_LocalDeletedImpliesLocalChanged validates that a locally deleted
// file (baseline exists, local absent) is correctly classified as EF6
// (remote delete propagation), not stolen by EF3 (local changed).
func TestEF6_LocalDeletedImpliesLocalChanged(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "deleted.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	// No local events (file absent), no remote events (unchanged).
	changes := []synctypes.PathChanges{
		{
			Path: "deleted.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceLocal,
					Type:      synctypes.ChangeDelete,
					Path:      "deleted.txt",
					ItemType:  synctypes.ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	remoteDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1, "EF6: local delete should propagate as remote delete")
}

// Validates: R-2.3
// TestEF4_ConvergentEdit_NoTransfer validates that when both sides
// independently edit to the same hash, the result is ActionUpdateSynced
// with no data transfer.
func TestEF4_ConvergentEdit_NoTransfer(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "converge.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "oldHash",
		RemoteHash: "oldHash",
	})

	changes := []synctypes.PathChanges{
		{
			Path: "converge.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeModify,
					Path:     "converge.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "newHash",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "converge.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "newHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	synced := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpdateSynced)
	assert.Len(t, synced, 1, "EF4: convergent edit should produce update_synced")

	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	uploads := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpload)
	assert.Empty(t, downloads, "EF4: no download needed for convergent edit")
	assert.Empty(t, uploads, "EF4: no upload needed for convergent edit")
}

// Validates: R-2.3
// TestEF11_ConvergentCreate validates that files appearing on both sides
// with the same hash are adopted (ActionUpdateSynced) without transfer.
func TestEF11_ConvergentCreate(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "newfile.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "newfile.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "sameHash",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "newfile.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "sameHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	synced := synctest.ActionsOfType(plan.Actions, synctypes.ActionUpdateSynced)
	assert.Len(t, synced, 1, "EF11: convergent create should adopt")
}

// Validates: R-2.2
// TestEF12_DivergentCreate validates that files appearing on both sides
// with different hashes produce a create-create conflict.
func TestEF12_DivergentCreate(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	changes := []synctypes.PathChanges{
		{
			Path: "newfile.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "newfile.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashA",
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
				},
			},
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "newfile.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "hashB",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, synctest.EmptyBaseline(), synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	conflicts := synctest.ActionsOfType(plan.Actions, synctypes.ActionConflict)
	assert.Len(t, conflicts, 1, "EF12: divergent create should conflict")
	assert.Equal(t, synctypes.ConflictCreateCreate, conflicts[0].ConflictInfo.ConflictType)
}

// Validates: R-2.2
// TestEF9_EditDeleteAutoResolve validates that local edit + remote delete
// produces a conflict with edit-delete type.
func TestEF9_EditDeleteAutoResolve(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "edited.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "oldHash",
		RemoteHash: "oldHash",
	})

	changes := []synctypes.PathChanges{
		{
			Path: "edited.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceRemote,
					Type:      synctypes.ChangeDelete,
					Path:      "edited.txt",
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
					Path:     "edited.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "newHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	conflicts := synctest.ActionsOfType(plan.Actions, synctypes.ActionConflict)
	assert.Len(t, conflicts, 1, "EF9: local edit + remote delete = conflict")
	assert.Equal(t, synctypes.ConflictEditDelete, conflicts[0].ConflictInfo.ConflictType)
}

// Validates: R-2.3
// TestUploadOnlyMode_ProducesRemoteDeletes validates that upload-only mode
// with locally deleted files produces ActionRemoteDelete (EF6), not
// EF10 cleanup.
func TestUploadOnlyMode_ProducesRemoteDeletes(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "gone.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "hashA",
		RemoteHash: "hashA",
	})

	changes := []synctypes.PathChanges{
		{
			Path: "gone.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceLocal,
					Type:      synctypes.ChangeDelete,
					Path:      "gone.txt",
					ItemType:  synctypes.ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncUploadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	remoteDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1,
		"upload-only: local delete should produce remote delete")
}

// Validates: R-2.3
// TestDownloadOnlyMode_SkipsLocalCorruption validates that download-only
// mode with a corrupted local file but unchanged remote produces no action.
func TestDownloadOnlyMode_SkipsLocalCorruption(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:       "corrupted.txt",
		DriveID:    driveid.New(synctest.TestDriveID),
		ItemID:     "item1",
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  "goodHash",
		RemoteHash: "goodHash",
	})

	// Local has different hash (corrupted), but in download-only mode
	// local changes are suppressed.
	changes := []synctypes.PathChanges{
		{
			Path: "corrupted.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeModify,
					Path:     "corrupted.txt",
					ItemType: synctypes.ItemTypeFile,
					Hash:     "corruptedHash",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncDownloadOnly, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// In download-only mode, local changes are suppressed. Since remote
	// didn't change, this is EF1 (no-op).
	assert.Empty(t, plan.Actions,
		"download-only: corrupted local with unchanged remote = no action")
}

// Validates: R-2.3
// TestED8_FolderModeFilteringRegression validates that folder classifiers
// use upfront mode filtering. ED8 (locally deleted folder, no remote observation,
// no remote deletion) should produce ActionRemoteDelete in bidirectional mode.
func TestED8_FolderModeFilteringRegression(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path:     "photos",
		DriveID:  driveid.New(synctest.TestDriveID),
		ItemID:   "folder1",
		ItemType: synctypes.ItemTypeFolder,
	})

	changes := []synctypes.PathChanges{
		{
			Path: "photos",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceLocal,
					Type:      synctypes.ChangeDelete,
					Path:      "photos",
					ItemType:  synctypes.ItemTypeFolder,
					IsDeleted: true,
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	remoteDeletes := synctest.ActionsOfType(plan.Actions, synctypes.ActionRemoteDelete)
	assert.Len(t, remoteDeletes, 1,
		"ED8: locally deleted folder should produce remote delete")
}

// ---------------------------------------------------------------------------
// §5: Move Detection Edge Cases
// ---------------------------------------------------------------------------

// Validates: R-2.3
// TestMoveDetection_AmbiguousSameHashMultipleDeletes validates that when
// two deleted files share the same hash, no move is detected (ambiguous).
func TestMoveDetection_AmbiguousSameHashMultipleDeletes(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path: "dir1/file.txt", DriveID: driveid.New(synctest.TestDriveID),
			ItemID: "item1", ItemType: synctypes.ItemTypeFile, LocalHash: "sameHash", RemoteHash: "sameHash",
		},
		&synctypes.BaselineEntry{
			Path: "dir2/file.txt", DriveID: driveid.New(synctest.TestDriveID),
			ItemID: "item2", ItemType: synctypes.ItemTypeFile, LocalHash: "sameHash", RemoteHash: "sameHash",
		},
	)

	// Both files deleted locally, one file created at new path.
	changes := []synctypes.PathChanges{
		{
			Path: "dir1/file.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceLocal, Type: synctypes.ChangeDelete, Path: "dir1/file.txt", ItemType: synctypes.ItemTypeFile, IsDeleted: true},
			},
		},
		{
			Path: "dir2/file.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceLocal, Type: synctypes.ChangeDelete, Path: "dir2/file.txt", ItemType: synctypes.ItemTypeFile, IsDeleted: true},
			},
		},
		{
			Path: "dir3/file.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "dir3/file.txt", ItemType: synctypes.ItemTypeFile, Hash: "sameHash"},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Ambiguous: two deletes with same hash → no move detected.
	mvs := moves(plan)
	assert.Empty(t, mvs, "ambiguous move: multiple deletes should prevent move detection")
}

// Validates: R-2.3
// TestMoveDetection_AmbiguousSameHashMultipleCreates validates that when
// two created files share the same hash as one deleted file, no move is detected.
func TestMoveDetection_AmbiguousSameHashMultipleCreates(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(
		&synctypes.BaselineEntry{
			Path: "original.txt", DriveID: driveid.New(synctest.TestDriveID),
			ItemID: "item1", ItemType: synctypes.ItemTypeFile, LocalHash: "sameHash", RemoteHash: "sameHash",
		},
	)

	changes := []synctypes.PathChanges{
		{
			Path: "original.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceLocal, Type: synctypes.ChangeDelete, Path: "original.txt", ItemType: synctypes.ItemTypeFile, IsDeleted: true},
			},
		},
		{
			Path: "copy1.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "copy1.txt", ItemType: synctypes.ItemTypeFile, Hash: "sameHash"},
			},
		},
		{
			Path: "copy2.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceLocal, Type: synctypes.ChangeCreate, Path: "copy2.txt", ItemType: synctypes.ItemTypeFile, Hash: "sameHash"},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Ambiguous: one delete, two creates with same hash → no move detected.
	mvs := moves(plan)
	assert.Empty(t, mvs, "ambiguous move: multiple creates should prevent move detection")
}

// Validates: R-2.3
// TestRemoteMove_WithRecreationAtSource validates that a remote move A->B
// combined with a new item at path A is handled correctly: B gets the
// move action, and A is classified as a new item.
func TestRemoteMove_WithRecreationAtSource(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	baseline := synctest.BaselineWith(&synctypes.BaselineEntry{
		Path: "source.txt", DriveID: driveid.New(synctest.TestDriveID),
		ItemID: "item1", ItemType: synctypes.ItemTypeFile, LocalHash: "hashA", RemoteHash: "hashA",
	})

	changes := []synctypes.PathChanges{
		{
			// The move event's Path is the destination.
			Path: "dest.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeMove,
					Path:     "dest.txt",
					OldPath:  "source.txt",
					ItemType: synctypes.ItemTypeFile,
					ItemID:   "item1",
					DriveID:  driveid.New(synctest.TestDriveID),
					Hash:     "hashA",
				},
			},
		},
		{
			// A new item appears at the old path.
			Path: "source.txt",
			RemoteEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceRemote,
					Type:     synctypes.ChangeCreate,
					Path:     "source.txt",
					ItemType: synctypes.ItemTypeFile,
					ItemID:   "item2",
					DriveID:  driveid.New(synctest.TestDriveID),
					Hash:     "hashB",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should have a local move for the renamed item.
	localMoves := synctest.ActionsOfType(plan.Actions, synctypes.ActionLocalMove)
	assert.Len(t, localMoves, 1, "should have a local move action")

	if len(localMoves) > 0 {
		assert.Equal(t, "dest.txt", localMoves[0].Path)
		assert.Equal(t, "source.txt", localMoves[0].OldPath)
	}

	// The new item at source.txt should be classified as a download (EF14).
	downloads := synctest.ActionsOfType(plan.Actions, synctypes.ActionDownload)
	assert.Len(t, downloads, 1, "new item at old path should be downloaded")
}

// ---------------------------------------------------------------------------
// §4: Dependency graph tests
// ---------------------------------------------------------------------------

// Validates: R-5.1
// TestBuildDependencies_FolderCreateBeforeChild validates that a child
// download depends on its parent folder create.
func TestBuildDependencies_FolderCreateBeforeChild(t *testing.T) {
	actions := []synctypes.Action{
		{Type: synctypes.ActionFolderCreate, Path: "newdir", CreateSide: synctypes.CreateLocal},
		{Type: synctypes.ActionDownload, Path: "newdir/file.txt"},
	}

	deps := buildDependencies(actions)

	// Action 1 (download) should depend on action 0 (folder create).
	require.Len(t, deps, 2)
	assert.Contains(t, deps[1], 0, "download should depend on parent folder create")
	assert.Empty(t, deps[0], "folder create should have no dependencies")
}

// Validates: R-5.1
// TestBuildDependencies_ChildDeleteBeforeParent validates that a parent
// folder delete depends on child deletes completing first.
func TestBuildDependencies_ChildDeleteBeforeParent(t *testing.T) {
	actions := []synctypes.Action{
		{
			Type: synctypes.ActionLocalDelete, Path: "dir",
			View: &synctypes.PathView{
				Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFolder},
			},
		},
		{
			Type: synctypes.ActionLocalDelete, Path: "dir/file.txt",
			View: &synctypes.PathView{
				Baseline: &synctypes.BaselineEntry{ItemType: synctypes.ItemTypeFile},
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

// Validates: R-6.4.1
// TestPlan_BigDeleteBlocked validates that the planner returns
// ErrBigDeleteTriggered when planned deletions exceed the threshold.
func TestPlan_BigDeleteBlocked(t *testing.T) {
	planner := NewPlanner(synctest.TestLogger(t))

	// Create 10-item baseline.
	baseline := synctest.EmptyBaseline()
	var changes []synctypes.PathChanges

	for i := range 10 {
		path := "file" + string(rune('a'+i)) + ".txt"
		entry := &synctypes.BaselineEntry{
			Path:       path,
			DriveID:    driveid.New(synctest.TestDriveID),
			ItemID:     "item-" + string(rune('a'+i)),
			ItemType:   synctypes.ItemTypeFile,
			LocalHash:  "hash",
			RemoteHash: "hash",
		}
		baseline.Put(entry)
	}

	// Delete 6 out of 10. Threshold is 5 → 6 > 5 → triggered.
	for i := range 6 {
		path := "file" + string(rune('a'+i)) + ".txt"
		changes = append(changes, synctypes.PathChanges{
			Path: path,
			LocalEvents: []synctypes.ChangeEvent{
				{Source: synctypes.SourceLocal, Type: synctypes.ChangeDelete, Path: path, ItemType: synctypes.ItemTypeFile, IsDeleted: true},
			},
		})
	}

	config := &synctypes.SafetyConfig{BigDeleteThreshold: 5}

	_, err := planner.Plan(changes, baseline, synctypes.SyncBidirectional, config, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, synctypes.ErrBigDeleteTriggered)
}

// ---------------------------------------------------------------------------
// §6: DAG Cycle Detection
// ---------------------------------------------------------------------------

// Validates: R-2.3
func TestDetectCycle_NoCycle(t *testing.T) {
	t.Parallel()

	// Linear chain: 0 → 1 → 2.
	deps := [][]int{
		{},  // 0 depends on nothing
		{0}, // 1 depends on 0
		{1}, // 2 depends on 1
	}

	err := detectDependencyCycle(deps)
	assert.NoError(t, err)
}

// Validates: R-2.3
func TestDetectCycle_SelfLoop(t *testing.T) {
	t.Parallel()

	// Node 1 depends on itself.
	deps := [][]int{
		{},  // 0
		{1}, // 1 → 1
		{},  // 2
	}

	err := detectDependencyCycle(deps)
	require.Error(t, err)
	assert.ErrorIs(t, err, synctypes.ErrDependencyCycle)
}

// Validates: R-2.3
func TestDetectCycle_MutualDependency(t *testing.T) {
	t.Parallel()

	// 0 → 1 → 0.
	deps := [][]int{
		{1}, // 0 depends on 1
		{0}, // 1 depends on 0
	}

	err := detectDependencyCycle(deps)
	require.Error(t, err)
	assert.ErrorIs(t, err, synctypes.ErrDependencyCycle)
}

// Validates: R-2.3
func TestDetectCycle_IndirectCycle(t *testing.T) {
	t.Parallel()

	// 0 → 1 → 2 → 0.
	deps := [][]int{
		{2}, // 0 depends on 2
		{0}, // 1 depends on 0
		{1}, // 2 depends on 1
	}

	err := detectDependencyCycle(deps)
	require.Error(t, err)
	assert.ErrorIs(t, err, synctypes.ErrDependencyCycle)
}

// Validates: R-2.3
func TestDetectCycle_DiamondNoCycle(t *testing.T) {
	t.Parallel()

	// Diamond: 0 → 1, 0 → 2, 1 → 3, 2 → 3.
	deps := [][]int{
		{1, 2}, // 0 depends on 1 and 2
		{3},    // 1 depends on 3
		{3},    // 2 depends on 3
		{},     // 3 depends on nothing
	}

	err := detectDependencyCycle(deps)
	assert.NoError(t, err)
}
