package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// resolvePathDriveID tests
// ---------------------------------------------------------------------------

// Validates: R-6.7.21
func TestResolvePathDriveID_DirectMatch(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "docs/file.txt", DriveID: driveA, ItemID: "item1", ItemType: ItemTypeFile},
	})

	got := resolvePathDriveID("docs/file.txt", bl)
	assert.Equal(t, driveA, got)
}

// Validates: R-6.7.21
func TestResolvePathDriveID_ParentMatch(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "shared", DriveID: driveA, ItemID: "folder1", ItemType: ItemTypeFolder},
	})

	// The file itself has no baseline, but its parent folder does.
	got := resolvePathDriveID("shared/new-file.txt", bl)
	assert.Equal(t, driveA, got)
}

// Validates: R-6.7.21
func TestResolvePathDriveID_NoMatch(t *testing.T) {
	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "other/file.txt", DriveID: driveid.New("driveX"), ItemID: "x", ItemType: ItemTypeFile},
	})

	got := resolvePathDriveID("completely/different/path.txt", bl)
	assert.True(t, got.IsZero(), "no ancestry match should return zero ID")
}

// ---------------------------------------------------------------------------
// isCrossDriveLocalMove tests
// ---------------------------------------------------------------------------

// Validates: R-6.7.21
func TestIsCrossDriveLocalMove_SameDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := newBaselineForTest([]*BaselineEntry{
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

// Validates: R-6.7.21
func TestIsCrossDriveLocalMove_DifferentDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	driveB := driveid.New("driveB")
	bl := newBaselineForTest([]*BaselineEntry{
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

// Validates: R-6.7.21
func TestIsCrossDriveLocalMove_ZeroDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := newBaselineForTest([]*BaselineEntry{
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

// Validates: R-6.7.21
func TestIsCrossDriveRemoteMove_SameDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	view := &PathView{
		Baseline: &BaselineEntry{DriveID: driveA},
		Remote:   &RemoteState{DriveID: driveA},
	}

	assert.False(t, isCrossDriveRemoteMove(view))
}

// Validates: R-6.7.21
func TestIsCrossDriveRemoteMove_DifferentDrive(t *testing.T) {
	view := &PathView{
		Baseline: &BaselineEntry{DriveID: driveid.New("driveA")},
		Remote:   &RemoteState{DriveID: driveid.New("driveB")},
	}

	assert.True(t, isCrossDriveRemoteMove(view))
}

// Validates: R-6.7.21
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
	tests := []struct {
		name       string
		sourcePath string
		targetPath string
		sourceRoot string
		targetRoot string
		sourceID   string
		targetID   string
	}{
		{
			name:       "OwnDriveToShortcut",
			sourcePath: "own/file.txt",
			targetPath: "shared/file.txt",
			sourceRoot: "own",
			targetRoot: "shared",
			sourceID:   "sfolder",
			targetID:   "item1",
		},
		{
			name:       "ShortcutToOwnDrive",
			sourcePath: "shared/doc.txt",
			targetPath: "own/doc.txt",
			sourceRoot: "shared",
			targetRoot: "own",
			sourceID:   "ownfolder",
			targetID:   "item1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driveA := driveid.New("driveA")
			driveB := driveid.New("driveB")
			planner := NewPlanner(synctest.TestLogger(t))

			bl := newBaselineForTest([]*BaselineEntry{
				{Path: tt.sourcePath, DriveID: driveB, ItemID: tt.targetID, ItemType: ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
				{Path: tt.targetRoot, DriveID: driveA, ItemID: tt.sourceID, ItemType: ItemTypeFolder},
			})
			if tt.name == "OwnDriveToShortcut" {
				bl = newBaselineForTest([]*BaselineEntry{
					{Path: tt.sourcePath, DriveID: driveA, ItemID: tt.targetID, ItemType: ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
					{Path: tt.targetRoot, DriveID: driveB, ItemID: tt.sourceID, ItemType: ItemTypeFolder},
				})
			}

			changes := []PathChanges{
				{
					Path: tt.sourcePath,
					LocalEvents: []ChangeEvent{{
						Source:    SourceLocal,
						Type:      ChangeDelete,
						Path:      tt.sourcePath,
						ItemType:  ItemTypeFile,
						IsDeleted: true,
					}},
				},
				{
					Path: tt.targetPath,
					LocalEvents: []ChangeEvent{{
						Source:   SourceLocal,
						Type:     ChangeCreate,
						Path:     tt.targetPath,
						ItemType: ItemTypeFile,
						Name:     "file.txt",
						Hash:     "hash1",
					}},
				},
			}
			if tt.name == "ShortcutToOwnDrive" {
				changes[1].LocalEvents[0].Name = "doc.txt"
			}

			plan, err := planner.Plan(changes, bl, SyncBidirectional, DefaultSafetyConfig(), nil)
			require.NoError(t, err)
			assert.Empty(t, moves(plan), "cross-drive move should not produce a move action")

			var hasDelete, hasUpload bool
			for i := range plan.Actions {
				if plan.Actions[i].Type == ActionRemoteDelete && plan.Actions[i].Path == tt.sourcePath {
					hasDelete = true
				}
				if plan.Actions[i].Type == ActionUpload && plan.Actions[i].Path == tt.targetPath {
					hasUpload = true
				}
			}

			assert.True(t, hasDelete)
			assert.True(t, hasUpload)
		})
	}
}

// Validates: R-6.7.21
func TestPlan_LocalUploadUnderShortcutAncestor_HasTargetRootMetadata(t *testing.T) {
	t.Parallel()

	shortcutDriveID := driveid.New("driveB")
	planner := NewPlanner(synctest.TestLogger(t))

	bl := newBaselineForTest([]*BaselineEntry{
		{Path: "shortcut", DriveID: shortcutDriveID, ItemID: "shortcut-root-id", ItemType: ItemTypeFolder},
	})

	changes := []PathChanges{{
		Path: "shortcut/new-file.txt",
		LocalEvents: []ChangeEvent{{
			Source:   SourceLocal,
			Type:     ChangeCreate,
			Path:     "shortcut/new-file.txt",
			ItemType: ItemTypeFile,
			Name:     "new-file.txt",
			Hash:     "hash1",
		}},
	}}

	plan, err := planner.Plan(changes, bl, SyncBidirectional, DefaultSafetyConfig(), nil)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)

	action := plan.Actions[0]
	assert.Equal(t, ActionUpload, action.Type)
	assert.Equal(t, shortcutDriveID, action.TargetDriveID)
	assert.Equal(t, "shortcut-root-id", action.TargetRootItemID)
	assert.Equal(t, "shortcut", action.TargetRootLocalPath)
}

// Validates: R-6.7.21
func TestPlan_SameDriveMove_StillWorks(t *testing.T) {
	// Regression: same-drive moves should still be detected and produce
	// ActionRemoteMove actions.
	driveA := driveid.New("driveA")
	planner := NewPlanner(synctest.TestLogger(t))

	bl := newBaselineForTest([]*BaselineEntry{
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
