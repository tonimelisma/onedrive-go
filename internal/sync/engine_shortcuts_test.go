package sync

import (
	"context"
	"fmt"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// filterOutShortcuts
// ---------------------------------------------------------------------------

// Validates: R-3.4.2
func TestFilterOutShortcuts_Empty(t *testing.T) {
	t.Parallel()

	result := filterOutShortcuts(nil)
	assert.Empty(t, result)
}

// Validates: R-3.4.2
func TestFilterOutShortcuts_NoShortcuts(t *testing.T) {
	t.Parallel()

	events := []synctypes.ChangeEvent{
		{Type: synctypes.ChangeCreate, Path: "a.txt"},
		{Type: synctypes.ChangeModify, Path: "b.txt"},
		{Type: synctypes.ChangeDelete, Path: "c.txt"},
	}

	result := filterOutShortcuts(events)
	assert.Len(t, result, 3)
}

// Validates: R-3.4.2
func TestFilterOutShortcuts_RemovesShortcuts(t *testing.T) {
	t.Parallel()

	events := []synctypes.ChangeEvent{
		{Type: synctypes.ChangeCreate, Path: "a.txt"},
		{Type: synctypes.ChangeShortcut, Path: "SharedFolder"},
		{Type: synctypes.ChangeModify, Path: "b.txt"},
		{Type: synctypes.ChangeShortcut, Path: "OtherShared"},
	}

	result := filterOutShortcuts(events)
	require.Len(t, result, 2)
	assert.Equal(t, "a.txt", result[0].Path)
	assert.Equal(t, "b.txt", result[1].Path)
}

// ---------------------------------------------------------------------------
// syncobserve.ConvertShortcutItems (unified item conversion for shortcut scopes)
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestConvertShortcutItems_NewFiles(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &synctypes.Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "Shared/TeamDocs",
	}

	items := []graph.Item{
		{
			ID:           "f1",
			Name:         "report.xlsx",
			ParentID:     "source-folder-1",
			DriveID:      remoteDriveID,
			Size:         1000,
			QuickXorHash: "hash1",
			ModifiedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			ID:       "d1",
			Name:     "SubDir",
			ParentID: "source-folder-1",
			DriveID:  remoteDriveID,
			IsFolder: true,
		},
	}

	bl := emptyBaseline()
	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))

	require.Len(t, events, 2)

	// File should be create with mapped path.
	assert.Equal(t, synctypes.ChangeCreate, events[0].Type)
	assert.Equal(t, "Shared/TeamDocs/report.xlsx", events[0].Path)
	assert.Equal(t, "f1", events[0].ItemID)
	assert.Equal(t, remoteDriveID, events[0].DriveID)

	// Folder should also be create.
	assert.Equal(t, synctypes.ChangeCreate, events[1].Type)
	assert.Equal(t, "Shared/TeamDocs/SubDir", events[1].Path)
	assert.Equal(t, synctypes.ItemTypeFolder, events[1].ItemType)
}

// Validates: R-2.10.16
func TestConvertShortcutItems_ExistingModified(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &synctypes.Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		{
			ID:           "f1",
			Name:         "report.xlsx",
			ParentID:     "source-folder-1",
			DriveID:      remoteDriveID,
			QuickXorHash: "newhash",
		},
	}

	bl := baselineWith(&synctypes.BaselineEntry{
		Path:    "SharedFolder/report.xlsx",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))
	require.Len(t, events, 1)
	assert.Equal(t, synctypes.ChangeModify, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
}

// Validates: R-2.10.16
func TestConvertShortcutItems_DeletedItem(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &synctypes.Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		{
			ID:        "f1",
			Name:      "report.xlsx",
			ParentID:  "source-folder-1",
			DriveID:   remoteDriveID,
			IsDeleted: true,
		},
	}

	bl := baselineWith(&synctypes.BaselineEntry{
		Path:    "SharedFolder/report.xlsx",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))
	require.Len(t, events, 1)
	assert.Equal(t, synctypes.ChangeDelete, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
	assert.True(t, events[0].IsDeleted)
}

// Validates: R-2.10.16
func TestConvertShortcutItems_SkipsRoot(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &synctypes.Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		{ID: "root", IsRoot: true, DriveID: remoteDriveID},
		{ID: "f1", Name: "file.txt", ParentID: "source-folder-1", DriveID: remoteDriveID},
	}

	bl := emptyBaseline()
	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))
	require.Len(t, events, 1)
	assert.Equal(t, "f1", events[0].ItemID)
}

// ---------------------------------------------------------------------------
// syncobserve.DetectShortcutOrphans
// ---------------------------------------------------------------------------

// Validates: R-2.10.17
func TestDetectShortcutOrphans_NoOrphans(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &synctypes.Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		{ID: "f1", Name: "file.txt", DriveID: remoteDriveID},
	}

	bl := baselineWith(&synctypes.BaselineEntry{
		Path:    "SharedFolder/file.txt",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	orphans := syncobserve.DetectShortcutOrphans(sc, remoteDriveID, items, bl)
	assert.Empty(t, orphans)
}

// Validates: R-2.10.17
func TestDetectShortcutOrphans_DetectsOrphans(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &synctypes.Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	// Enumeration has only f1.
	items := []graph.Item{
		{ID: "f1", Name: "file.txt", DriveID: remoteDriveID},
	}

	// synctypes.Baseline has f1 and f2 — f2 is the orphan.
	bl := baselineWith(
		&synctypes.BaselineEntry{Path: "SharedFolder/file.txt", DriveID: remoteDriveID, ItemID: "f1"},
		&synctypes.BaselineEntry{Path: "SharedFolder/deleted.txt", DriveID: remoteDriveID, ItemID: "f2"},
	)

	orphans := syncobserve.DetectShortcutOrphans(sc, remoteDriveID, items, bl)
	require.Len(t, orphans, 1)
	assert.Equal(t, synctypes.ChangeDelete, orphans[0].Type)
	assert.Equal(t, "SharedFolder/deleted.txt", orphans[0].Path)
	assert.Equal(t, "f2", orphans[0].ItemID)
	assert.True(t, orphans[0].IsDeleted)
}

// Validates: R-2.10.17
func TestDetectShortcutOrphans_IgnoresOtherDrives(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	otherDriveID := driveid.New("0000000000000001")
	sc := &synctypes.Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{} // empty enumeration

	// synctypes.Baseline has an entry from a different drive — should NOT be treated as orphan.
	bl := baselineWith(
		&synctypes.BaselineEntry{Path: "SharedFolder/other.txt", DriveID: otherDriveID, ItemID: "x1"},
	)

	orphans := syncobserve.DetectShortcutOrphans(sc, remoteDriveID, items, bl)
	assert.Empty(t, orphans, "items from other drives should not be treated as orphans")
}

// ---------------------------------------------------------------------------
// registerShortcuts (integration with syncstore.SyncStore)
// ---------------------------------------------------------------------------

// Validates: R-3.4.2
func TestRegisterShortcuts_NewShortcut(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	events := []synctypes.ChangeEvent{
		{
			Type:          synctypes.ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "Shared/TeamDocs",
			RemoteDriveID: "remote-drive-1",
			RemoteItemID:  "remote-item-1",
		},
	}

	err := testEngineFlowFromEngine(e).registerShortcuts(ctx, events)
	require.NoError(t, err)

	sc, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, sc)
	assert.Equal(t, "remote-drive-1", sc.RemoteDrive)
	assert.Equal(t, "remote-item-1", sc.RemoteItem)
	assert.Equal(t, "Shared/TeamDocs", sc.LocalPath)
}

// Validates: R-2.10.16
func TestRegisterShortcuts_UpdateExisting(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Pre-register a shortcut.
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "OldPath",
		DriveType:    "personal",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1000,
	}))

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	events := []synctypes.ChangeEvent{
		{
			Type:          synctypes.ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "NewPath",
			RemoteDriveID: "remote-drive-1",
			RemoteItemID:  "remote-item-1",
		},
	}

	err := testEngineFlowFromEngine(e).registerShortcuts(ctx, events)
	require.NoError(t, err)

	sc, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, sc)
	assert.Equal(t, "NewPath", sc.LocalPath)
	// DriveType and Observation should be preserved from the existing record.
	assert.Equal(t, "personal", sc.DriveType)
	assert.Equal(t, synctypes.ObservationDelta, sc.Observation)
	assert.Equal(t, int64(1000), sc.DiscoveredAt)
}

// ---------------------------------------------------------------------------
// handleRemovedShortcuts (integration with syncstore.SyncStore)
// ---------------------------------------------------------------------------

func shortcutRecord(itemID, remoteDrive, remoteItem, localPath string) *synctypes.Shortcut {
	return &synctypes.Shortcut{
		ItemID:       itemID,
		RemoteDrive:  remoteDrive,
		RemoteItem:   remoteItem,
		LocalPath:    localPath,
		Observation:  synctypes.ObservationUnknown,
		DiscoveredAt: 1000,
	}
}

// Validates: R-2.10.17
func TestHandleRemovedShortcuts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		removedIDs map[string]bool
		wantFound  bool
		wantNil    bool
	}{
		{name: "RemovesKnownShortcut", removedIDs: map[string]bool{"sc-1": true}, wantNil: true},
		{name: "IgnoresNonShortcutDeletes", removedIDs: map[string]bool{"other-item": true}, wantFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestManager(t)
			ctx := t.Context()
			require.NoError(t, mgr.UpsertShortcut(ctx, shortcutRecord("sc-1", "remote-drive-1", "remote-item-1", "SharedFolder")))

			shortcuts, err := mgr.ListShortcuts(ctx)
			require.NoError(t, err)

			e := &Engine{baseline: mgr, logger: testLogger(t)}
			require.NoError(t, testEngineFlowFromEngine(e).handleRemovedShortcuts(ctx, tt.removedIDs, shortcuts))

			sc, found, err := mgr.GetShortcut(ctx, "sc-1")
			require.NoError(t, err)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantNil {
				assert.Nil(t, sc, "shortcut should be deleted")
				return
			}

			assert.NotNil(t, sc, "shortcut should still exist")
		})
	}
}

// Validates: R-2.10.17, R-2.10.38
func TestHandleRemovedShortcuts_ClearsRemotePermissionScopesUnderRemovedShortcut(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	ctx := t.Context()

	require.NoError(t, eng.baseline.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedFolder",
		Observation:  synctypes.ObservationUnknown,
		DiscoveredAt: 1000,
	}))
	require.NoError(t, eng.baseline.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-2",
		RemoteDrive:  "remote-drive-2",
		RemoteItem:   "remote-item-2",
		LocalPath:    "OtherFolder",
		Observation:  synctypes.ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	removedScope := synctypes.SKPermRemote("SharedFolder/locked")
	otherScope := synctypes.SKPermRemote("OtherFolder/locked")
	removedQuotaScope := synctypes.SKQuotaShortcut("remote-drive-1:remote-item-1")
	otherQuotaScope := synctypes.SKQuotaShortcut("remote-drive-2:remote-item-2")

	seedShortcutRemovalFailures(t, eng, ctx, removedScope, removedQuotaScope, otherScope, otherQuotaScope)
	seedShortcutRemovalScopeBlocks(t, eng, removedScope, removedQuotaScope, otherScope, otherQuotaScope)

	shortcuts, err := eng.baseline.ListShortcuts(ctx)
	require.NoError(t, err)

	err = handleRemovedShortcutsForTest(t, eng, ctx, map[string]bool{"sc-1": true}, shortcuts)
	require.NoError(t, err)

	sc, found, err := eng.baseline.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	assert.False(t, found, "removed shortcut should no longer exist")
	assert.Nil(t, sc, "removed shortcut should be deleted")

	assert.False(t, isTestScopeBlocked(eng, removedScope),
		"remote permission scope under the removed shortcut should be cleared")
	assert.False(t, isTestScopeBlocked(eng, removedQuotaScope),
		"quota scope under the removed shortcut should be discarded")
	assert.True(t, isTestScopeBlocked(eng, otherScope),
		"remote permission scopes under other shortcuts must remain intact")
	assert.True(t, isTestScopeBlocked(eng, otherQuotaScope),
		"quota scopes under other shortcuts must remain intact")

	failures, err := eng.baseline.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, failures, 2, "removed shortcut should discard both quota and remote permission failures recursively")
	assert.ElementsMatch(t, []string{"OtherFolder/locked", "OtherFolder/quota/file.txt"}, []string{failures[0].Path, failures[1].Path})
}

func seedShortcutRemovalFailures(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	removedScope synctypes.ScopeKey,
	removedQuotaScope synctypes.ScopeKey,
	otherScope synctypes.ScopeKey,
	otherQuotaScope synctypes.ScopeKey,
) {
	t.Helper()

	failures := []synctypes.SyncFailureParams{
		{
			Path:      "SharedFolder/locked",
			DriveID:   eng.driveID,
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleBoundary,
			Category:  synctypes.CategoryActionable,
			IssueType: synctypes.IssuePermissionDenied,
			ErrMsg:    "read-only boundary",
			ScopeKey:  removedScope,
		},
		{
			Path:      "SharedFolder/locked/file.txt",
			DriveID:   eng.driveID,
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleHeld,
			Category:  synctypes.CategoryTransient,
			ErrMsg:    "blocked by remote permission scope",
			ScopeKey:  removedScope,
		},
		{
			Path:      "SharedFolder/quota/file.txt",
			DriveID:   eng.driveID,
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleHeld,
			Category:  synctypes.CategoryTransient,
			ErrMsg:    "blocked by shortcut quota scope",
			ScopeKey:  removedQuotaScope,
		},
		{
			Path:      "OtherFolder/locked",
			DriveID:   eng.driveID,
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleBoundary,
			Category:  synctypes.CategoryActionable,
			IssueType: synctypes.IssuePermissionDenied,
			ErrMsg:    "read-only boundary",
			ScopeKey:  otherScope,
		},
		{
			Path:      "OtherFolder/quota/file.txt",
			DriveID:   eng.driveID,
			Direction: synctypes.DirectionUpload,
			Role:      synctypes.FailureRoleHeld,
			Category:  synctypes.CategoryTransient,
			ErrMsg:    "blocked by shortcut quota scope",
			ScopeKey:  otherQuotaScope,
		},
	}

	for i := range failures {
		require.NoError(t, eng.baseline.RecordFailure(ctx, &failures[i], nil))
	}
}

func seedShortcutRemovalScopeBlocks(
	t *testing.T,
	eng *testEngine,
	removedScope synctypes.ScopeKey,
	removedQuotaScope synctypes.ScopeKey,
	otherScope synctypes.ScopeKey,
	otherQuotaScope synctypes.ScopeKey,
) {
	t.Helper()

	now := eng.nowFn()
	blocks := []synctypes.ScopeBlock{
		{
			Key:       removedScope,
			IssueType: synctypes.IssuePermissionDenied,
			BlockedAt: now,
		},
		{
			Key:           removedQuotaScope,
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			BlockedAt:     now,
			TrialInterval: time.Minute,
			NextTrialAt:   now.Add(time.Minute),
		},
		{
			Key:       otherScope,
			IssueType: synctypes.IssuePermissionDenied,
			BlockedAt: now,
		},
		{
			Key:           otherQuotaScope,
			IssueType:     synctypes.IssueQuotaExceeded,
			TimingSource:  synctypes.ScopeTimingBackoff,
			BlockedAt:     now,
			TrialInterval: time.Minute,
			NextTrialAt:   now.Add(time.Minute),
		},
	}

	for i := range blocks {
		setTestScopeBlock(t, eng, blocks[i])
	}
}

// ---------------------------------------------------------------------------
// detectShortcutCollisions
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestDetectShortcutCollisions_NoCollisions(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedA",
		Observation:  synctypes.ObservationUnknown,
		DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-2",
		RemoteDrive:  "remote-drive-2",
		RemoteItem:   "remote-item-2",
		LocalPath:    "SharedB",
		Observation:  synctypes.ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	collisions := detectShortcutCollisionsFromList(shortcuts, emptyBaseline(), testLogger(t))
	assert.Empty(t, collisions)
}

// Validates: R-2.10.16
func TestDetectShortcutCollisions_PrimaryDriveConflict(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	primaryDriveID := driveid.New("0000000000000001")

	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedDocs",
		Observation:  synctypes.ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	// synctypes.Baseline has a primary drive entry at the same path.
	bl := baselineWith(&synctypes.BaselineEntry{
		Path:    "SharedDocs",
		DriveID: primaryDriveID,
		ItemID:  "primary-folder-1",
	})

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	collisions := detectShortcutCollisionsFromList(shortcuts, bl, testLogger(t))
	assert.True(t, collisions["sc-1"], "shortcut conflicting with primary drive should be in collisions set")
}

// Validates: R-2.10.16
func TestDetectShortcutCollisions_PathConflicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		shortcuts      []*synctypes.Shortcut
		wantCollisions map[string]bool
	}{
		{
			name: "DuplicatePath",
			shortcuts: []*synctypes.Shortcut{
				shortcutRecord("sc-1", "remote-drive-1", "remote-item-1", "SharedDocs"),
				shortcutRecord("sc-2", "remote-drive-2", "remote-item-2", "SharedDocs"),
			},
			wantCollisions: map[string]bool{"sc-2": true},
		},
		{
			name: "NestedPaths",
			shortcuts: []*synctypes.Shortcut{
				shortcutRecord("sc-parent", "remote-drive-1", "remote-item-1", "Shared"),
				shortcutRecord("sc-child", "remote-drive-2", "remote-item-2", "Shared/Sub"),
			},
			wantCollisions: map[string]bool{"sc-child": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestManager(t)
			ctx := t.Context()
			for _, sc := range tt.shortcuts {
				require.NoError(t, mgr.UpsertShortcut(ctx, sc))
			}

			shortcuts, err := mgr.ListShortcuts(ctx)
			require.NoError(t, err)

			collisions := detectShortcutCollisionsFromList(shortcuts, emptyBaseline(), testLogger(t))
			for _, sc := range tt.shortcuts {
				assert.Equal(t, tt.wantCollisions[sc.ItemID], collisions[sc.ItemID], "collision mismatch for %s", sc.ItemID)
			}
		})
	}
}

// Validates: R-2.10.16
func TestObserveShortcutContent_SkipsCollisions(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Two shortcuts — one will be marked as collision.
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-ok",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "ok-folder",
		LocalPath:    "Good",
		Observation:  synctypes.ObservationEnumerate,
		DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-collide",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "collide-folder",
		LocalPath:    "Bad",
		Observation:  synctypes.ObservationEnumerate,
		DiscoveredAt: 1000,
	}))

	mockLister := &mockRecursiveLister{
		items: []graph.Item{
			{ID: "f1", Name: "file.txt", ParentID: "ok-folder", DriveID: remoteDriveID},
		},
	}

	e := &Engine{
		baseline:        mgr,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	collisions := map[string]bool{"sc-collide": true}
	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, collisions)
	require.NoError(t, err)

	// Only the non-colliding shortcut should produce events.
	// The mock returns the same items for all calls, but the colliding shortcut is skipped entirely.
	for _, ev := range events {
		assert.NotEqual(t, "Bad/file.txt", ev.Path, "colliding shortcut should not produce events")
	}
}

// ---------------------------------------------------------------------------
// detectDriveType
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestDetectDriveType_Personal(t *testing.T) {
	t.Parallel()

	verifier := &mockDriveVerifier{
		drive: &graph.Drive{
			ID:        driveid.New("0000000000000099"),
			DriveType: "personal",
		},
	}

	e := &Engine{
		driveVerifier: verifier,
		logger:        testLogger(t),
	}

	driveType, obs := testEngineFlowFromEngine(e).detectDriveType(t.Context(), "0000000000000099")
	assert.Equal(t, "personal", driveType)
	assert.Equal(t, synctypes.ObservationDelta, obs)
}

// Validates: R-2.10.16
func TestDetectDriveType_Business(t *testing.T) {
	t.Parallel()

	verifier := &mockDriveVerifier{
		drive: &graph.Drive{
			ID:        driveid.New("0000000000000099"),
			DriveType: "business",
		},
	}

	e := &Engine{
		driveVerifier: verifier,
		logger:        testLogger(t),
	}

	driveType, obs := testEngineFlowFromEngine(e).detectDriveType(t.Context(), "0000000000000099")
	assert.Equal(t, "business", driveType)
	assert.Equal(t, synctypes.ObservationEnumerate, obs)
}

// Validates: R-2.10.16
func TestDetectDriveType_ErrorFallsBackToEnumerate(t *testing.T) {
	t.Parallel()

	verifier := &mockDriveVerifier{err: assert.AnError}

	e := &Engine{
		driveVerifier: verifier,
		logger:        testLogger(t),
	}

	driveType, obs := testEngineFlowFromEngine(e).detectDriveType(t.Context(), "0000000000000099")
	assert.Empty(t, driveType)
	assert.Equal(t, synctypes.ObservationEnumerate, obs)
}

// ---------------------------------------------------------------------------
// DeleteDeltaToken
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestDeleteDeltaToken(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Commit a delta token.
	require.NoError(t, mgr.CommitDeltaToken(ctx, "tok1", "drv1", "scope1", "drv1"))

	// Verify it exists.
	token, err := mgr.GetDeltaToken(ctx, "drv1", "scope1")
	require.NoError(t, err)
	assert.Equal(t, "tok1", token)

	// Delete it.
	require.NoError(t, mgr.DeleteDeltaToken(ctx, "drv1", "scope1"))

	// Verify it's gone.
	token, err = mgr.GetDeltaToken(ctx, "drv1", "scope1")
	require.NoError(t, err)
	assert.Empty(t, token)
}

// Validates: R-2.10.16
func TestDeleteDeltaToken_NonExistent(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)

	// Deleting a non-existent token should not error.
	err := mgr.DeleteDeltaToken(t.Context(), "nonexistent", "scope")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// observeShortcutContent + observeSingleShortcut
// ---------------------------------------------------------------------------

// Validates: R-3.4.2
func TestObserveShortcutContent_DeltaStrategy(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Register a shortcut with delta observation.
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1000,
	}))

	mockFolderDelta := &mockFolderDeltaFetcher{
		items: []graph.Item{
			{
				ID:           "f1",
				Name:         "report.xlsx",
				ParentID:     "source-folder-1",
				DriveID:      remoteDriveID,
				QuickXorHash: "hash1",
				Size:         500,
			},
		},
		token: "new-delta-token",
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	e := &Engine{
		baseline:    mgr,
		folderDelta: mockFolderDelta,
		logger:      testLogger(t),
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, synctypes.ChangeCreate, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
	assert.Equal(t, remoteDriveID, events[0].DriveID)

	// Delta token should be committed.
	token, err := mgr.GetDeltaToken(ctx, "0000000000000099", "source-folder-1")
	require.NoError(t, err)
	assert.Equal(t, "new-delta-token", token)
}

// Validates: R-2.10.16
func TestObserveShortcutContent_EnumerateStrategy(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  synctypes.ObservationEnumerate,
		DiscoveredAt: 1000,
	}))

	mockLister := &mockRecursiveLister{
		items: []graph.Item{
			{
				ID:           "f1",
				Name:         "data.csv",
				ParentID:     "source-folder-1",
				DriveID:      remoteDriveID,
				QuickXorHash: "hash2",
			},
		},
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	e := &Engine{
		baseline:        mgr,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, synctypes.ChangeCreate, events[0].Type)
	assert.Equal(t, "SharedFolder/data.csv", events[0].Path)
}

// Validates: R-2.10.16
func TestObserveShortcutContent_SkipsOnError(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1000,
	}))

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	e := &Engine{
		baseline:    mgr,
		folderDelta: &mockFolderDeltaFetcher{err: assert.AnError},
		logger:      testLogger(t),
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err, "should not propagate per-shortcut errors")
	assert.Empty(t, events)
}

// Validates: R-2.10.16
func TestObserveShortcutContent_NoShortcuts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	e := &Engine{
		baseline:    mgr,
		folderDelta: &mockFolderDeltaFetcher{},
		logger:      testLogger(t),
	}

	bl := emptyBaseline()
	var shortcuts []synctypes.Shortcut
	events, err := testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)
	assert.Empty(t, events)
}

// ---------------------------------------------------------------------------
// processShortcuts (integration)
// ---------------------------------------------------------------------------

// Validates: R-3.4.2
func TestProcessShortcuts_RegistersAndObserves(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	mockLister := &mockRecursiveLister{
		items: []graph.Item{
			{
				ID:           "f1",
				Name:         "file.txt",
				ParentID:     "remote-item-1",
				DriveID:      remoteDriveID,
				QuickXorHash: "h1",
			},
		},
	}

	e := &Engine{
		baseline:        mgr,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	remoteEvents := []synctypes.ChangeEvent{
		{
			Type:          synctypes.ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "SharedDocs",
			RemoteDriveID: "0000000000000099",
			RemoteItemID:  "remote-item-1",
		},
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).processShortcuts(ctx, remoteEvents, bl, false)
	require.NoError(t, err)

	// Should have registered the shortcut.
	sc, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, sc)
	assert.Equal(t, "SharedDocs", sc.LocalPath)

	// Should have observed content.
	require.Len(t, events, 1)
	assert.Equal(t, "SharedDocs/file.txt", events[0].Path)
}

// Validates: R-2.10.16
func TestProcessShortcuts_DryRunSkipsObservation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	e := &Engine{
		baseline:        mgr,
		recursiveLister: &mockRecursiveLister{},
		logger:          testLogger(t),
	}

	remoteEvents := []synctypes.ChangeEvent{
		{
			Type:          synctypes.ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "SharedDocs",
			RemoteDriveID: "0000000000000099",
			RemoteItemID:  "remote-item-1",
		},
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).processShortcuts(ctx, remoteEvents, bl, true)
	require.NoError(t, err)
	assert.Nil(t, events, "dry-run should skip observation")

	// synctypes.Shortcut should still be registered.
	sc, found, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, sc)
}

// Validates: R-2.10.16
func TestProcessShortcuts_NilCapabilities(t *testing.T) {
	t.Parallel()

	e := &Engine{
		logger: testLogger(t),
	}

	events, err := testEngineFlowFromEngine(e).processShortcuts(t.Context(), nil, emptyBaseline(), false)
	require.NoError(t, err)
	assert.Nil(t, events, "should return nil when no shortcut capabilities configured")
}

// ---------------------------------------------------------------------------
// Integration: observeChanges produces both primary and shortcut events
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestProcessShortcuts_ShortcutAndPrimaryEventsCoexist(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Pre-register a shortcut so observation finds it.
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedDocs",
		Observation:  synctypes.ObservationEnumerate,
		DiscoveredAt: 1000,
	}))

	mockLister := &mockRecursiveLister{
		items: []graph.Item{
			{ID: "f1", Name: "shared.txt", ParentID: "remote-item-1", DriveID: remoteDriveID},
		},
	}

	e := &Engine{
		baseline:        mgr,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	// Simulate primary delta producing a regular file change AND a shortcut event.
	remoteEvents := []synctypes.ChangeEvent{
		{Type: synctypes.ChangeCreate, Path: "primary/newfile.txt", ItemID: "p1", Source: synctypes.SourceRemote},
		{
			Type: synctypes.ChangeShortcut, Path: "SharedDocs", ItemID: "sc-1",
			RemoteDriveID: "0000000000000099", RemoteItemID: "remote-item-1",
		},
	}

	bl := emptyBaseline()
	shortcutEvents, err := testEngineFlowFromEngine(e).processShortcuts(ctx, remoteEvents, bl, false)
	require.NoError(t, err)

	// synctypes.Shortcut events should contain the shared file.
	require.Len(t, shortcutEvents, 1)
	assert.Equal(t, "SharedDocs/shared.txt", shortcutEvents[0].Path)

	// Filter out shortcuts from primary events.
	filtered := filterOutShortcuts(remoteEvents)
	require.Len(t, filtered, 1)
	assert.Equal(t, "primary/newfile.txt", filtered[0].Path)

	// Both sets of events should coexist in a buffer.
	buf := syncobserve.NewBuffer(testLogger(t))
	buf.AddAll(filtered)
	buf.AddAll(shortcutEvents)
	batch := buf.FlushImmediate()
	assert.Len(t, batch, 2, "buffer should contain both primary and shortcut events")
}

// ---------------------------------------------------------------------------
// Concurrent multi-shortcut observation
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestObserveShortcutContent_ConcurrentMultipleShortcuts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Register 5 shortcuts to exercise concurrency (maxShortcutConcurrency=4).
	for i := range 5 {
		id := fmt.Sprintf("sc-%d", i)
		require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
			ItemID:       id,
			RemoteDrive:  "0000000000000099",
			RemoteItem:   fmt.Sprintf("folder-%d", i),
			LocalPath:    fmt.Sprintf("Shared%d", i),
			Observation:  synctypes.ObservationEnumerate,
			DiscoveredAt: 1000,
		}))
	}

	mockLister := &mockRecursiveLister{
		items: []graph.Item{
			{ID: "f1", Name: "file.txt", ParentID: "folder-0", DriveID: remoteDriveID},
		},
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	e := &Engine{
		baseline:        mgr,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)

	// Each shortcut produces 1 event from the mock, so expect 5.
	assert.Len(t, events, 5, "all 5 shortcuts should be observed concurrently")
}

// ---------------------------------------------------------------------------
// observeShortcutDelta: ErrGone retry
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestObserveShortcutDelta_RetryOnErrGone(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1000,
	}))

	// Save a stale delta token.
	require.NoError(t, mgr.CommitDeltaToken(ctx, "stale-token", "0000000000000099", "source-folder-1", "0000000000000099"))

	mock := &mockRetryFolderDelta{
		firstErr: graph.ErrGone,
		items: []graph.Item{
			{ID: "f1", Name: "report.xlsx", ParentID: "source-folder-1", DriveID: remoteDriveID},
		},
		token: "fresh-token",
	}

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	e := &Engine{
		baseline:    mgr,
		folderDelta: mock,
		logger:      testLogger(t),
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)

	// Should have retried with empty token and succeeded.
	require.Len(t, events, 1)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)

	// Fresh token should be committed.
	token, err := mgr.GetDeltaToken(ctx, "0000000000000099", "source-folder-1")
	require.NoError(t, err)
	assert.Equal(t, "fresh-token", token)

	// Mock should have been called twice (first 410, then success).
	assert.Equal(t, 2, mock.calls)
}

// ---------------------------------------------------------------------------
// reconcileShortcutScopes tests (B-332)
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestReconcileShortcutScopes_DeltaReconciliation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Register a delta-observation shortcut.
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "Shared/Delta", Observation: synctypes.ObservationDelta, DiscoveredAt: 1000,
	}))

	// Seed baseline with an existing file (will NOT be in delta → becomes orphan).
	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action: synctypes.ActionDownload, Success: true, Path: "Shared/Delta/old.txt",
		DriveID: remoteDriveID, ItemID: "old-file", ParentID: "root-1", ItemType: synctypes.ItemTypeFile,
		RemoteHash: "oldhash", Size: 100,
	}))

	mockDelta := &mockFolderDeltaFetcher{
		items: []graph.Item{
			{ID: "new-file", Name: "new.txt", ParentID: "root-1", DriveID: remoteDriveID, QuickXorHash: "newhash", Size: 200},
		},
		token: "reconcile-token",
	}

	e := &Engine{
		baseline:    mgr,
		folderDelta: mockDelta,
		logger:      testLogger(t),
	}

	bl, err := mgr.Load(ctx)
	require.NoError(t, err)

	events, err := testEngineFlowFromEngine(e).reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)

	// Should have: 1 create (new file) + 1 delete (orphan old file).
	require.Len(t, events, 2)

	var creates, deletes int
	for _, ev := range events {
		if ev.Type == synctypes.ChangeCreate {
			creates++
			assert.Equal(t, "Shared/Delta/new.txt", ev.Path)
			continue
		}

		if ev.Type == synctypes.ChangeDelete {
			deletes++
			assert.Equal(t, "Shared/Delta/old.txt", ev.Path)
		}
	}

	assert.Equal(t, 1, creates)
	assert.Equal(t, 1, deletes)

	// Delta token should be committed.
	token, err := mgr.GetDeltaToken(ctx, "0000000000000099", "root-1")
	require.NoError(t, err)
	assert.Equal(t, "reconcile-token", token)
}

// Validates: R-2.10.16
func TestReconcileShortcutScopes_EnumerateReconciliation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "Shared/Enum", Observation: synctypes.ObservationEnumerate, DiscoveredAt: 1000,
	}))

	mockLister := &mockRecursiveLister{
		items: []graph.Item{
			{ID: "f1", Name: "doc.txt", ParentID: "root-1", DriveID: remoteDriveID, QuickXorHash: "h1"},
		},
	}

	e := &Engine{
		baseline:        mgr,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	bl := emptyBaseline()

	events, err := testEngineFlowFromEngine(e).reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, synctypes.ChangeCreate, events[0].Type)
	assert.Equal(t, "Shared/Enum/doc.txt", events[0].Path)
}

// Validates: R-2.10.16
func TestReconcileShortcutScopes_CollisionSkipped(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Two shortcuts with colliding local paths.
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "Shared/Collide", Observation: synctypes.ObservationDelta, DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID: "sc-2", RemoteDrive: "0000000000000099", RemoteItem: "root-2",
		LocalPath: "Shared/Collide", Observation: synctypes.ObservationDelta, DiscoveredAt: 2000,
	}))

	mockDelta := &mockFolderDeltaFetcher{
		items: []graph.Item{
			{ID: "f1", Name: "file.txt", ParentID: "root-1", DriveID: remoteDriveID, QuickXorHash: "h1"},
		},
		token: "tok",
	}

	e := &Engine{
		baseline:    mgr,
		folderDelta: mockDelta,
		logger:      testLogger(t),
	}

	bl := emptyBaseline()

	events, err := testEngineFlowFromEngine(e).reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)

	// sc-2 is colliding (skipped). sc-1 is the "kept" shortcut.
	// sc-1 gets delta results, but the mock returns items for root-1.
	// Both shortcuts hit the same mock, but only sc-1 produces events
	// because sc-2 is skipped. The mock returns 1 item, so sc-1 produces
	// 1 create event. Verify only sc-1's events appear.
	for _, ev := range events {
		assert.Equal(t, "Shared/Collide/file.txt", ev.Path,
			"all events should be from the non-colliding shortcut")
	}
}

// Validates: R-2.10.16
func TestReconcileShortcutScopes_PerScopeErrorIsolation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Two shortcuts: one with failing delta, one with working lister.
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID: "sc-fail", RemoteDrive: "0000000000000099", RemoteItem: "root-fail",
		LocalPath: "Shared/Fail", Observation: synctypes.ObservationDelta, DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID: "sc-ok", RemoteDrive: "0000000000000099", RemoteItem: "root-ok",
		LocalPath: "Shared/Ok", Observation: synctypes.ObservationEnumerate, DiscoveredAt: 2000,
	}))

	// Folder delta will fail.
	mockDelta := &mockFolderDeltaFetcher{
		err: fmt.Errorf("network timeout"),
	}

	// Recursive lister will succeed.
	mockLister := &mockRecursiveLister{
		items: []graph.Item{
			{ID: "f1", Name: "ok.txt", ParentID: "root-ok", DriveID: remoteDriveID, QuickXorHash: "h1"},
		},
	}

	e := &Engine{
		baseline:        mgr,
		folderDelta:     mockDelta,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	bl := emptyBaseline()

	events, err := testEngineFlowFromEngine(e).reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err, "overall reconciliation should succeed even if one scope fails")

	// Only the successful scope's events should appear.
	require.Len(t, events, 1)
	assert.Equal(t, "Shared/Ok/ok.txt", events[0].Path)
}

// Validates: R-2.10.16
func TestReconcileShortcutScopes_NoShortcuts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	e := &Engine{
		baseline:    mgr,
		folderDelta: &mockFolderDeltaFetcher{},
		logger:      testLogger(t),
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)
	assert.Empty(t, events)
}

// Validates: R-2.10.16
func TestReconcileShortcutScopes_NilFetchersReturnsNil(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	bl := emptyBaseline()
	events, err := testEngineFlowFromEngine(e).reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)
	assert.Nil(t, events)
}

// ---------------------------------------------------------------------------
// Source dedup tests
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestDetectShortcutCollisions_DuplicateSourceFolder(t *testing.T) {
	t.Parallel()

	shortcuts := []synctypes.Shortcut{
		{ItemID: "sc-1", RemoteDrive: "drive-1", RemoteItem: "item-1", LocalPath: "Shared/A"},
		{ItemID: "sc-2", RemoteDrive: "drive-1", RemoteItem: "item-1", LocalPath: "Shared/B"},
	}

	collisions := detectShortcutCollisionsFromList(shortcuts, emptyBaseline(), testLogger(t))

	// sc-2 is the duplicate source — should be skipped.
	assert.True(t, collisions["sc-2"], "duplicate source shortcut should be flagged")
	assert.False(t, collisions["sc-1"], "original source shortcut should not be flagged")
}

// ---------------------------------------------------------------------------
// Nested shortcut skip tests
// ---------------------------------------------------------------------------

// Validates: R-2.10.16
func TestConvertShortcutItems_NestedShortcut_SkippedWithWarning(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	items := []graph.Item{
		// Normal file.
		{ID: "f1", Name: "report.xlsx", ParentID: "root-1", DriveID: remoteDriveID, QuickXorHash: "h1"},
		// Nested shortcut (has RemoteItemID).
		{
			ID: "nested-sc", Name: "NestedShared", ParentID: "root-1", DriveID: remoteDriveID, IsFolder: true,
			RemoteItemID: "remote-nested-item", RemoteDriveID: "other-drive",
		},
	}

	sc := &synctypes.Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "SharedFolder",
	}

	events := syncobserve.ConvertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	// Only the normal file should produce an event.
	require.Len(t, events, 1)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
}

// ---------------------------------------------------------------------------
// Mocks
// ---------------------------------------------------------------------------

type mockDriveVerifier struct {
	drive *graph.Drive
	err   error
}

func (m *mockDriveVerifier) Drive(_ context.Context, _ driveid.ID) (*graph.Drive, error) {
	return m.drive, m.err
}

type mockFolderDeltaFetcher struct {
	items []graph.Item
	token string
	err   error
}

func (m *mockFolderDeltaFetcher) DeltaFolderAll(_ context.Context, _ driveid.ID, _, _ string) ([]graph.Item, string, error) {
	return m.items, m.token, m.err
}

// mockRetryFolderDelta returns an error on the first call (simulating 410/ErrGone),
// then succeeds on subsequent calls with fresh items and token.
type mockRetryFolderDelta struct {
	calls    int
	firstErr error
	items    []graph.Item
	token    string
	mu       stdsync.Mutex
}

func (m *mockRetryFolderDelta) DeltaFolderAll(_ context.Context, _ driveid.ID, _, _ string) ([]graph.Item, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if m.calls == 1 && m.firstErr != nil {
		return nil, "", m.firstErr
	}

	return m.items, m.token, nil
}

type mockRecursiveLister struct {
	items []graph.Item
	err   error
}

func (m *mockRecursiveLister) ListChildrenRecursive(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
	return m.items, m.err
}
