package syncplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// resolvePathDriveID tests
// ---------------------------------------------------------------------------

// Validates: R-6.7.21
func TestResolvePathDriveID_DirectMatch(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
		{Path: "docs/file.txt", DriveID: driveA, ItemID: "item1", ItemType: synctypes.ItemTypeFile},
	})

	got := resolvePathDriveID("docs/file.txt", bl)
	assert.Equal(t, driveA, got)
}

// Validates: R-6.7.21
func TestResolvePathDriveID_ParentMatch(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
		{Path: "shared", DriveID: driveA, ItemID: "folder1", ItemType: synctypes.ItemTypeFolder},
	})

	// The file itself has no baseline, but its parent folder does.
	got := resolvePathDriveID("shared/new-file.txt", bl)
	assert.Equal(t, driveA, got)
}

// Validates: R-6.7.21
func TestResolvePathDriveID_NoMatch(t *testing.T) {
	bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
		{Path: "other/file.txt", DriveID: driveid.New("driveX"), ItemID: "x", ItemType: synctypes.ItemTypeFile},
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
	bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
		{Path: "old.txt", DriveID: driveA, ItemID: "item1", ItemType: synctypes.ItemTypeFile, LocalHash: "hash1"},
		{Path: "docs", DriveID: driveA, ItemID: "folder1", ItemType: synctypes.ItemTypeFolder},
	})
	views := map[string]*synctypes.PathView{
		"old.txt":          {Path: "old.txt", Baseline: bl.ByPath["old.txt"]},
		"docs/renamed.txt": {Path: "docs/renamed.txt", Local: &synctypes.LocalState{Hash: "hash1"}},
	}

	got := isCrossDriveLocalMove("old.txt", "docs/renamed.txt", views, bl)
	assert.False(t, got, "same drive should not be detected as cross-drive")
}

// Validates: R-6.7.21
func TestIsCrossDriveLocalMove_DifferentDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	driveB := driveid.New("driveB")
	bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
		{Path: "own/file.txt", DriveID: driveA, ItemID: "item1", ItemType: synctypes.ItemTypeFile, LocalHash: "hash1"},
		{Path: "shared", DriveID: driveB, ItemID: "folder1", ItemType: synctypes.ItemTypeFolder},
	})
	views := map[string]*synctypes.PathView{
		"own/file.txt":    {Path: "own/file.txt", Baseline: bl.ByPath["own/file.txt"]},
		"shared/file.txt": {Path: "shared/file.txt", Local: &synctypes.LocalState{Hash: "hash1"}},
	}

	got := isCrossDriveLocalMove("own/file.txt", "shared/file.txt", views, bl)
	assert.True(t, got, "different drives should be detected as cross-drive")
}

// Validates: R-6.7.21
func TestIsCrossDriveLocalMove_ZeroDrive(t *testing.T) {
	driveA := driveid.New("driveA")
	bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
		{Path: "old.txt", DriveID: driveA, ItemID: "item1", ItemType: synctypes.ItemTypeFile, LocalHash: "hash1"},
	})
	views := map[string]*synctypes.PathView{
		"old.txt":         {Path: "old.txt", Baseline: bl.ByPath["old.txt"]},
		"unknown/new.txt": {Path: "unknown/new.txt", Local: &synctypes.LocalState{Hash: "hash1"}},
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
	view := &synctypes.PathView{
		Baseline: &synctypes.BaselineEntry{DriveID: driveA},
		Remote:   &synctypes.RemoteState{DriveID: driveA},
	}

	assert.False(t, isCrossDriveRemoteMove(view))
}

// Validates: R-6.7.21
func TestIsCrossDriveRemoteMove_DifferentDrive(t *testing.T) {
	view := &synctypes.PathView{
		Baseline: &synctypes.BaselineEntry{DriveID: driveid.New("driveA")},
		Remote:   &synctypes.RemoteState{DriveID: driveid.New("driveB")},
	}

	assert.True(t, isCrossDriveRemoteMove(view))
}

// Validates: R-6.7.21
func TestIsCrossDriveRemoteMove_NoBaseline(t *testing.T) {
	view := &synctypes.PathView{
		Remote: &synctypes.RemoteState{DriveID: driveid.New("driveA")},
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

			bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
				{Path: tt.sourcePath, DriveID: driveB, ItemID: tt.targetID, ItemType: synctypes.ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
				{Path: tt.targetRoot, DriveID: driveA, ItemID: tt.sourceID, ItemType: synctypes.ItemTypeFolder},
			})
			if tt.name == "OwnDriveToShortcut" {
				bl = synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
					{Path: tt.sourcePath, DriveID: driveA, ItemID: tt.targetID, ItemType: synctypes.ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
					{Path: tt.targetRoot, DriveID: driveB, ItemID: tt.sourceID, ItemType: synctypes.ItemTypeFolder},
				})
			}

			changes := []synctypes.PathChanges{
				{
					Path: tt.sourcePath,
					LocalEvents: []synctypes.ChangeEvent{{
						Source:    synctypes.SourceLocal,
						Type:      synctypes.ChangeDelete,
						Path:      tt.sourcePath,
						ItemType:  synctypes.ItemTypeFile,
						IsDeleted: true,
					}},
				},
				{
					Path: tt.targetPath,
					LocalEvents: []synctypes.ChangeEvent{{
						Source:   synctypes.SourceLocal,
						Type:     synctypes.ChangeCreate,
						Path:     tt.targetPath,
						ItemType: synctypes.ItemTypeFile,
						Name:     "file.txt",
						Hash:     "hash1",
					}},
				},
			}
			if tt.name == "ShortcutToOwnDrive" {
				changes[1].LocalEvents[0].Name = "doc.txt"
			}

			plan, err := planner.Plan(changes, bl, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
			require.NoError(t, err)
			assert.Empty(t, moves(plan), "cross-drive move should not produce a move action")

			var hasDelete, hasUpload bool
			for i := range plan.Actions {
				if plan.Actions[i].Type == synctypes.ActionRemoteDelete && plan.Actions[i].Path == tt.sourcePath {
					hasDelete = true
				}
				if plan.Actions[i].Type == synctypes.ActionUpload && plan.Actions[i].Path == tt.targetPath {
					hasUpload = true
				}
			}

			assert.True(t, hasDelete)
			assert.True(t, hasUpload)
		})
	}
}

// Validates: R-6.7.21
func TestPlan_SameDriveMove_StillWorks(t *testing.T) {
	// Regression: same-drive moves should still be detected and produce
	// ActionRemoteMove actions.
	driveA := driveid.New("driveA")
	planner := NewPlanner(synctest.TestLogger(t))

	bl := synctypes.NewBaselineForTest([]*synctypes.BaselineEntry{
		{Path: "old-name.txt", DriveID: driveA, ItemID: "item1", ItemType: synctypes.ItemTypeFile, LocalHash: "hash1", RemoteHash: "hash1"},
	})

	changes := []synctypes.PathChanges{
		{
			Path: "old-name.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:    synctypes.SourceLocal,
					Type:      synctypes.ChangeDelete,
					Path:      "old-name.txt",
					ItemType:  synctypes.ItemTypeFile,
					IsDeleted: true,
				},
			},
		},
		{
			Path: "new-name.txt",
			LocalEvents: []synctypes.ChangeEvent{
				{
					Source:   synctypes.SourceLocal,
					Type:     synctypes.ChangeCreate,
					Path:     "new-name.txt",
					ItemType: synctypes.ItemTypeFile,
					Name:     "new-name.txt",
					Hash:     "hash1",
				},
			},
		},
	}

	plan, err := planner.Plan(changes, bl, synctypes.SyncBidirectional, synctypes.DefaultSafetyConfig(), nil)
	require.NoError(t, err)

	mvs := moves(plan)
	require.Len(t, mvs, 1, "same-drive rename should produce a move action")
	assert.Equal(t, synctypes.ActionRemoteMove, mvs[0].Type)
	assert.Equal(t, "old-name.txt", mvs[0].OldPath)
	assert.Equal(t, "new-name.txt", mvs[0].Path)
}
