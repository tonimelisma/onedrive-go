package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// resolvePathDriveID tests
// ---------------------------------------------------------------------------

func TestResolvePathDriveID_DirectMatch(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "docs/file.txt", DriveID: driveA, ItemID: "item1", ItemType: ItemTypeFile},
	})

	got := resolvePathDriveID("docs/file.txt", bl)
	assert.Equal(t, driveA, got)
}

func TestResolvePathDriveID_ParentMatch(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "shared", DriveID: driveA, ItemID: "folder1", ItemType: ItemTypeFolder},
	})

	// The file itself has no baseline, but its parent folder does.
	got := resolvePathDriveID("shared/new-file.txt", bl)
	assert.Equal(t, driveA, got)
}

func TestResolvePathDriveID_NoMatch(t *testing.T) {
	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "other/file.txt", DriveID: driveid.New("driveX"), ItemID: "x", ItemType: ItemTypeFile},
	})

	got := resolvePathDriveID("completely/different/path.txt", bl)
	assert.True(t, got.IsZero(), "no ancestry match should return zero ID")
}

// ---------------------------------------------------------------------------
// isCrossDriveLocalMove tests
// ---------------------------------------------------------------------------

func TestIsCrossDriveLocalMove_SameDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "old.txt", DriveID: driveA, ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "hash1"},
		{Path: "docs", DriveID: driveA, ItemID: "folder1", ItemType: ItemTypeFolder},
	})
	views := map[string]*PathView{
		"old.txt":          {Path: "old.txt", Baseline: bl.ByPath["old.txt"]},
		"docs/renamed.txt": {Path: "docs/renamed.txt", Local: &LocalState{Hash: "hash1"}},
	}

	got := isCrossDriveLocalMove("old.txt", "docs/renamed.txt", views, bl)
	assert.False(t, got, "same drive should not be detected as cross-drive")
}

func TestIsCrossDriveLocalMove_DifferentDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	driveB := driveid.New("driveB")
	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "own/file.txt", DriveID: driveA, ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "hash1"},
		{Path: "shared", DriveID: driveB, ItemID: "folder1", ItemType: ItemTypeFolder},
	})
	views := map[string]*PathView{
		"own/file.txt":    {Path: "own/file.txt", Baseline: bl.ByPath["own/file.txt"]},
		"shared/file.txt": {Path: "shared/file.txt", Local: &LocalState{Hash: "hash1"}},
	}

	got := isCrossDriveLocalMove("own/file.txt", "shared/file.txt", views, bl)
	assert.True(t, got, "different drives should be detected as cross-drive")
}

func TestIsCrossDriveLocalMove_ZeroDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "old.txt", DriveID: driveA, ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "hash1"},
	})
	views := map[string]*PathView{
		"old.txt":         {Path: "old.txt", Baseline: bl.ByPath["old.txt"]},
		"unknown/new.txt": {Path: "unknown/new.txt", Local: &LocalState{Hash: "hash1"}},
	}

	// Destination has no baseline ancestry → zero drive → conservative false.
	got := isCrossDriveLocalMove("old.txt", "unknown/new.txt", views, bl)
	assert.False(t, got, "unknown destination drive should return false (conservative)")
}

// ---------------------------------------------------------------------------
// isCrossDriveRemoteMove tests
// ---------------------------------------------------------------------------

func TestIsCrossDriveRemoteMove_SameDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	view := &PathView{
		Baseline: &BaselineEntry{DriveID: driveA},
		Remote:   &RemoteState{DriveID: driveA},
	}

	assert.False(t, isCrossDriveRemoteMove(view))
}

func TestIsCrossDriveRemoteMove_DifferentDrive(t *testing.T) {
	view := &PathView{
		Baseline: &BaselineEntry{DriveID: driveid.New("driveA")},
		Remote:   &RemoteState{DriveID: driveid.New("driveB")},
	}

	assert.True(t, isCrossDriveRemoteMove(view))
}

func TestIsCrossDriveRemoteMove_NoBaseline(t *testing.T) {
	view := &PathView{
		Remote: &RemoteState{DriveID: driveid.New("driveA")},
	}

	assert.False(t, isCrossDriveRemoteMove(view), "no baseline → false")
}

// ---------------------------------------------------------------------------
// Integration tests via Plan()
// ---------------------------------------------------------------------------

// Validates: R-6.7.21
func TestPlan_CrossDriveLocalMove_DecomposesToDeleteAndUpload(t *testing.T) {
	// Move from own drive (driveA) to shared folder (driveB).
	// Should NOT produce a move action — should decompose to delete + upload.
	driveA := driveid.New("driveA")
	driveB := driveid.New("driveB")
	planner := NewPlanner(testLogger(t))

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "own/file.txt", DriveID: driveA, ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
		{Path: "shared", DriveID: driveB, ItemID: "sfolder", ItemType: ItemTypeFolder},
	})

	changes := []PathChanges{
		{
			// Delete from own drive (local file disappeared).
			Path: "own/file.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "own/file.txt",
					ItemType:  ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
		{
			// Create in shared folder (local file appeared).
			Path: "shared/file.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "shared/file.txt",
					ItemType: ItemTypeFile,
					Name:     "file.txt",
					Hash:     "hash1",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, bl, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	// Should have NO move actions.
	mvs := moves(plan)
	assert.Empty(t, mvs, "cross-drive move should not produce a move action")

	// Should have a remote delete (for the deleted path on driveA) and
	// an upload (for the new file in driveB).
	var hasDelete, hasUpload bool
	for i := range plan.Actions {
		if plan.Actions[i].Type == ActionRemoteDelete && plan.Actions[i].Path == "own/file.txt" {
			hasDelete = true
		}
		if plan.Actions[i].Type == ActionUpload && plan.Actions[i].Path == "shared/file.txt" {
			hasUpload = true
		}
	}

	assert.True(t, hasDelete, "should have ActionRemoteDelete for own/file.txt")
	assert.True(t, hasUpload, "should have ActionUpload for shared/file.txt")
}

// Validates: R-6.7.21
func TestPlan_CrossDriveLocalMove_ShortcutToOwnDrive(t *testing.T) {
	// Reverse direction: move from shared folder (driveB) to own drive (driveA).
	driveA := driveid.New("driveA")
	driveB := driveid.New("driveB")
	planner := NewPlanner(testLogger(t))

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "shared/doc.txt", DriveID: driveB, ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
		{Path: "own", DriveID: driveA, ItemID: "ownfolder", ItemType: ItemTypeFolder},
	})

	changes := []PathChanges{
		{
			Path: "shared/doc.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "shared/doc.txt",
					ItemType:  ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
		{
			Path: "own/doc.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "own/doc.txt",
					ItemType: ItemTypeFile,
					Name:     "doc.txt",
					Hash:     "hash1",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, bl, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	mvs := moves(plan)
	assert.Empty(t, mvs, "cross-drive move should not produce a move action")

	// Verify decomposition.
	var hasDelete, hasUpload bool
	for i := range plan.Actions {
		if plan.Actions[i].Type == ActionRemoteDelete && plan.Actions[i].Path == "shared/doc.txt" {
			hasDelete = true
		}
		if plan.Actions[i].Type == ActionUpload && plan.Actions[i].Path == "own/doc.txt" {
			hasUpload = true
		}
	}

	assert.True(t, hasDelete, "should have ActionRemoteDelete for shared/doc.txt")
	assert.True(t, hasUpload, "should have ActionUpload for own/doc.txt")
}

func TestPlan_SameDriveMove_StillWorks(t *testing.T) {
	// Regression: same-drive moves should still be detected and produce
	// ActionRemoteMove actions.
	driveA := driveid.New("driveA")
	planner := NewPlanner(testLogger(t))

	bl := NewBaselineForTest([]*BaselineEntry{
		{Path: "old-name.txt", DriveID: driveA, ItemID: "item1", ItemType: ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
	})

	changes := []PathChanges{
		{
			Path: "old-name.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:    SourceLocal,
					Type:      ChangeDelete,
					Path:      "old-name.txt",
					ItemType:  ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
		{
			Path: "new-name.txt",
			LocalEvents: []ChangeEvent{
				{
					Source:   SourceLocal,
					Type:     ChangeCreate,
					Path:     "new-name.txt",
					ItemType: ItemTypeFile,
					Name:     "new-name.txt",
					Hash:     "hash1",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, bl, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	mvs := moves(plan)
	require.Len(t, mvs, 1, "same-drive rename should produce a move action")
	assert.Equal(t, ActionRemoteMove, mvs[0].Type)
	assert.Equal(t, "old-name.txt", mvs[0].OldPath)
	assert.Equal(t, "new-name.txt", mvs[0].Path)
}
