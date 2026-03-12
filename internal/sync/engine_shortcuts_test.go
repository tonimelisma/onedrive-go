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

func TestFilterOutShortcuts_NoShortcuts(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Type: ChangeCreate, Path: "a.txt"},
		{Type: ChangeModify, Path: "b.txt"},
		{Type: ChangeDelete, Path: "c.txt"},
	}

	result := filterOutShortcuts(events)
	assert.Len(t, result, 3)
}

// Validates: R-3.4.2
func TestFilterOutShortcuts_RemovesShortcuts(t *testing.T) {
	t.Parallel()

	events := []ChangeEvent{
		{Type: ChangeCreate, Path: "a.txt"},
		{Type: ChangeShortcut, Path: "SharedFolder"},
		{Type: ChangeModify, Path: "b.txt"},
		{Type: ChangeShortcut, Path: "OtherShared"},
	}

	result := filterOutShortcuts(events)
	require.Len(t, result, 2)
	assert.Equal(t, "a.txt", result[0].Path)
	assert.Equal(t, "b.txt", result[1].Path)
}

// ---------------------------------------------------------------------------
// convertShortcutItems (unified item conversion for shortcut scopes)
// ---------------------------------------------------------------------------

func TestConvertShortcutItems_NewFiles(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
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
	events := convertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))

	require.Len(t, events, 2)

	// File should be create with mapped path.
	assert.Equal(t, ChangeCreate, events[0].Type)
	assert.Equal(t, "Shared/TeamDocs/report.xlsx", events[0].Path)
	assert.Equal(t, "f1", events[0].ItemID)
	assert.Equal(t, remoteDriveID, events[0].DriveID)

	// Folder should also be create.
	assert.Equal(t, ChangeCreate, events[1].Type)
	assert.Equal(t, "Shared/TeamDocs/SubDir", events[1].Path)
	assert.Equal(t, ItemTypeFolder, events[1].ItemType)
}

func TestConvertShortcutItems_ExistingModified(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
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

	bl := baselineWith(&BaselineEntry{
		Path:    "SharedFolder/report.xlsx",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	events := convertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))
	require.Len(t, events, 1)
	assert.Equal(t, ChangeModify, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
}

func TestConvertShortcutItems_DeletedItem(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
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

	bl := baselineWith(&BaselineEntry{
		Path:    "SharedFolder/report.xlsx",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	events := convertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))
	require.Len(t, events, 1)
	assert.Equal(t, ChangeDelete, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
	assert.True(t, events[0].IsDeleted)
}

func TestConvertShortcutItems_SkipsRoot(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
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
	events := convertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))
	require.Len(t, events, 1)
	assert.Equal(t, "f1", events[0].ItemID)
}

// ---------------------------------------------------------------------------
// detectShortcutOrphans
// ---------------------------------------------------------------------------

func TestDetectShortcutOrphans_NoOrphans(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		{ID: "f1", Name: "file.txt", DriveID: remoteDriveID},
	}

	bl := baselineWith(&BaselineEntry{
		Path:    "SharedFolder/file.txt",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	orphans := detectShortcutOrphans(sc, remoteDriveID, items, bl)
	assert.Empty(t, orphans)
}

func TestDetectShortcutOrphans_DetectsOrphans(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	// Enumeration has only f1.
	items := []graph.Item{
		{ID: "f1", Name: "file.txt", DriveID: remoteDriveID},
	}

	// Baseline has f1 and f2 — f2 is the orphan.
	bl := baselineWith(
		&BaselineEntry{Path: "SharedFolder/file.txt", DriveID: remoteDriveID, ItemID: "f1"},
		&BaselineEntry{Path: "SharedFolder/deleted.txt", DriveID: remoteDriveID, ItemID: "f2"},
	)

	orphans := detectShortcutOrphans(sc, remoteDriveID, items, bl)
	require.Len(t, orphans, 1)
	assert.Equal(t, ChangeDelete, orphans[0].Type)
	assert.Equal(t, "SharedFolder/deleted.txt", orphans[0].Path)
	assert.Equal(t, "f2", orphans[0].ItemID)
	assert.True(t, orphans[0].IsDeleted)
}

func TestDetectShortcutOrphans_IgnoresOtherDrives(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	otherDriveID := driveid.New("0000000000000001")
	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{} // empty enumeration

	// Baseline has an entry from a different drive — should NOT be treated as orphan.
	bl := baselineWith(
		&BaselineEntry{Path: "SharedFolder/other.txt", DriveID: otherDriveID, ItemID: "x1"},
	)

	orphans := detectShortcutOrphans(sc, remoteDriveID, items, bl)
	assert.Empty(t, orphans, "items from other drives should not be treated as orphans")
}

// ---------------------------------------------------------------------------
// registerShortcuts (integration with SyncStore)
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

	events := []ChangeEvent{
		{
			Type:          ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "Shared/TeamDocs",
			RemoteDriveID: "remote-drive-1",
			RemoteItemID:  "remote-item-1",
		},
	}

	err := e.registerShortcuts(ctx, events)
	require.NoError(t, err)

	sc, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.NotNil(t, sc)
	assert.Equal(t, "remote-drive-1", sc.RemoteDrive)
	assert.Equal(t, "remote-item-1", sc.RemoteItem)
	assert.Equal(t, "Shared/TeamDocs", sc.LocalPath)
}

func TestRegisterShortcuts_UpdateExisting(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Pre-register a shortcut.
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "OldPath",
		DriveType:    "personal",
		Observation:  ObservationDelta,
		DiscoveredAt: 1000,
	}))

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	events := []ChangeEvent{
		{
			Type:          ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "NewPath",
			RemoteDriveID: "remote-drive-1",
			RemoteItemID:  "remote-item-1",
		},
	}

	err := e.registerShortcuts(ctx, events)
	require.NoError(t, err)

	sc, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.NotNil(t, sc)
	assert.Equal(t, "NewPath", sc.LocalPath)
	// DriveType and Observation should be preserved from the existing record.
	assert.Equal(t, "personal", sc.DriveType)
	assert.Equal(t, ObservationDelta, sc.Observation)
	assert.Equal(t, int64(1000), sc.DiscoveredAt)
}

// ---------------------------------------------------------------------------
// handleRemovedShortcuts (integration with SyncStore)
// ---------------------------------------------------------------------------

func TestHandleRemovedShortcuts_RemovesKnownShortcut(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedFolder",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	err = e.handleRemovedShortcuts(ctx, map[string]bool{"sc-1": true}, shortcuts)
	require.NoError(t, err)

	sc, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	assert.Nil(t, sc, "shortcut should be deleted")
}

func TestHandleRemovedShortcuts_IgnoresNonShortcutDeletes(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedFolder",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	// Delete a different item ID — shortcut should remain.
	err = e.handleRemovedShortcuts(ctx, map[string]bool{"other-item": true}, shortcuts)
	require.NoError(t, err)

	sc, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	assert.NotNil(t, sc, "shortcut should still exist")
}

// ---------------------------------------------------------------------------
// detectShortcutCollisions
// ---------------------------------------------------------------------------

func TestDetectShortcutCollisions_NoCollisions(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedA",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-2",
		RemoteDrive:  "remote-drive-2",
		RemoteItem:   "remote-item-2",
		LocalPath:    "SharedB",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	collisions := detectShortcutCollisionsFromList(shortcuts, emptyBaseline(), testLogger(t))
	assert.Empty(t, collisions)
}

func TestDetectShortcutCollisions_DuplicatePath(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Two shortcuts at the same local path.
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedDocs",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-2",
		RemoteDrive:  "remote-drive-2",
		RemoteItem:   "remote-item-2",
		LocalPath:    "SharedDocs",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	collisions := detectShortcutCollisionsFromList(shortcuts, emptyBaseline(), testLogger(t))
	assert.True(t, collisions["sc-2"], "later duplicate should be in collisions set")
	assert.False(t, collisions["sc-1"], "first shortcut should be kept")
}

func TestDetectShortcutCollisions_PrimaryDriveConflict(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	primaryDriveID := driveid.New("0000000000000001")

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedDocs",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	// Baseline has a primary drive entry at the same path.
	bl := baselineWith(&BaselineEntry{
		Path:    "SharedDocs",
		DriveID: primaryDriveID,
		ItemID:  "primary-folder-1",
	})

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	collisions := detectShortcutCollisionsFromList(shortcuts, bl, testLogger(t))
	assert.True(t, collisions["sc-1"], "shortcut conflicting with primary drive should be in collisions set")
}

func TestDetectShortcutCollisions_NestedPaths(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-parent",
		RemoteDrive:  "remote-drive-1",
		RemoteItem:   "remote-item-1",
		LocalPath:    "Shared",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-child",
		RemoteDrive:  "remote-drive-2",
		RemoteItem:   "remote-item-2",
		LocalPath:    "Shared/Sub",
		Observation:  ObservationUnknown,
		DiscoveredAt: 1000,
	}))

	shortcuts, err := mgr.ListShortcuts(ctx)
	require.NoError(t, err)

	collisions := detectShortcutCollisionsFromList(shortcuts, emptyBaseline(), testLogger(t))
	assert.True(t, collisions["sc-child"], "child nested under parent shortcut should be in collisions set")
	assert.False(t, collisions["sc-parent"], "parent shortcut should be kept")
}

func TestObserveShortcutContent_SkipsCollisions(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Two shortcuts — one will be marked as collision.
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-ok",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "ok-folder",
		LocalPath:    "Good",
		Observation:  ObservationEnumerate,
		DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-collide",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "collide-folder",
		LocalPath:    "Bad",
		Observation:  ObservationEnumerate,
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
	events, err := e.observeShortcutContentFromList(ctx, shortcuts, bl, collisions)
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

	driveType, obs := e.detectDriveType(t.Context(), "0000000000000099")
	assert.Equal(t, "personal", driveType)
	assert.Equal(t, ObservationDelta, obs)
}

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

	driveType, obs := e.detectDriveType(t.Context(), "0000000000000099")
	assert.Equal(t, "business", driveType)
	assert.Equal(t, ObservationEnumerate, obs)
}

func TestDetectDriveType_ErrorFallsBackToEnumerate(t *testing.T) {
	t.Parallel()

	verifier := &mockDriveVerifier{err: assert.AnError}

	e := &Engine{
		driveVerifier: verifier,
		logger:        testLogger(t),
	}

	driveType, obs := e.detectDriveType(t.Context(), "0000000000000099")
	assert.Equal(t, "", driveType)
	assert.Equal(t, ObservationEnumerate, obs)
}

// ---------------------------------------------------------------------------
// DeleteDeltaToken
// ---------------------------------------------------------------------------

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
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  ObservationDelta,
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
	events, err := e.observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, ChangeCreate, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
	assert.Equal(t, remoteDriveID, events[0].DriveID)

	// Delta token should be committed.
	token, err := mgr.GetDeltaToken(ctx, "0000000000000099", "source-folder-1")
	require.NoError(t, err)
	assert.Equal(t, "new-delta-token", token)
}

func TestObserveShortcutContent_EnumerateStrategy(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  ObservationEnumerate,
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
	events, err := e.observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, ChangeCreate, events[0].Type)
	assert.Equal(t, "SharedFolder/data.csv", events[0].Path)
}

func TestObserveShortcutContent_SkipsOnError(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  ObservationDelta,
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
	events, err := e.observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err, "should not propagate per-shortcut errors")
	assert.Empty(t, events)
}

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
	var shortcuts []Shortcut
	events, err := e.observeShortcutContentFromList(ctx, shortcuts, bl, nil)
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

	remoteEvents := []ChangeEvent{
		{
			Type:          ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "SharedDocs",
			RemoteDriveID: "0000000000000099",
			RemoteItemID:  "remote-item-1",
		},
	}

	bl := emptyBaseline()
	events, err := e.processShortcuts(ctx, remoteEvents, bl, false)
	require.NoError(t, err)

	// Should have registered the shortcut.
	sc, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.NotNil(t, sc)
	assert.Equal(t, "SharedDocs", sc.LocalPath)

	// Should have observed content.
	require.Len(t, events, 1)
	assert.Equal(t, "SharedDocs/file.txt", events[0].Path)
}

func TestProcessShortcuts_DryRunSkipsObservation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	e := &Engine{
		baseline:        mgr,
		recursiveLister: &mockRecursiveLister{},
		logger:          testLogger(t),
	}

	remoteEvents := []ChangeEvent{
		{
			Type:          ChangeShortcut,
			ItemID:        "sc-1",
			Path:          "SharedDocs",
			RemoteDriveID: "0000000000000099",
			RemoteItemID:  "remote-item-1",
		},
	}

	bl := emptyBaseline()
	events, err := e.processShortcuts(ctx, remoteEvents, bl, true)
	require.NoError(t, err)
	assert.Nil(t, events, "dry-run should skip observation")

	// Shortcut should still be registered.
	sc, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	require.NotNil(t, sc)
}

func TestProcessShortcuts_NilCapabilities(t *testing.T) {
	t.Parallel()

	e := &Engine{
		logger: testLogger(t),
	}

	events, err := e.processShortcuts(t.Context(), nil, emptyBaseline(), false)
	require.NoError(t, err)
	assert.Nil(t, events, "should return nil when no shortcut capabilities configured")
}

// ---------------------------------------------------------------------------
// Integration: observeChanges produces both primary and shortcut events
// ---------------------------------------------------------------------------

func TestProcessShortcuts_ShortcutAndPrimaryEventsCoexist(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Pre-register a shortcut so observation finds it.
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "remote-item-1",
		LocalPath:    "SharedDocs",
		Observation:  ObservationEnumerate,
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
	remoteEvents := []ChangeEvent{
		{Type: ChangeCreate, Path: "primary/newfile.txt", ItemID: "p1", Source: SourceRemote},
		{
			Type: ChangeShortcut, Path: "SharedDocs", ItemID: "sc-1",
			RemoteDriveID: "0000000000000099", RemoteItemID: "remote-item-1",
		},
	}

	bl := emptyBaseline()
	shortcutEvents, err := e.processShortcuts(ctx, remoteEvents, bl, false)
	require.NoError(t, err)

	// Shortcut events should contain the shared file.
	require.Len(t, shortcutEvents, 1)
	assert.Equal(t, "SharedDocs/shared.txt", shortcutEvents[0].Path)

	// Filter out shortcuts from primary events.
	filtered := filterOutShortcuts(remoteEvents)
	require.Len(t, filtered, 1)
	assert.Equal(t, "primary/newfile.txt", filtered[0].Path)

	// Both sets of events should coexist in a buffer.
	buf := NewBuffer(testLogger(t))
	buf.AddAll(filtered)
	buf.AddAll(shortcutEvents)
	batch := buf.FlushImmediate()
	assert.Len(t, batch, 2, "buffer should contain both primary and shortcut events")
}

// ---------------------------------------------------------------------------
// Concurrent multi-shortcut observation
// ---------------------------------------------------------------------------

func TestObserveShortcutContent_ConcurrentMultipleShortcuts(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Register 5 shortcuts to exercise concurrency (maxShortcutConcurrency=4).
	for i := range 5 {
		id := fmt.Sprintf("sc-%d", i)
		require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
			ItemID:       id,
			RemoteDrive:  "0000000000000099",
			RemoteItem:   fmt.Sprintf("folder-%d", i),
			LocalPath:    fmt.Sprintf("Shared%d", i),
			Observation:  ObservationEnumerate,
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
	events, err := e.observeShortcutContentFromList(ctx, shortcuts, bl, nil)
	require.NoError(t, err)

	// Each shortcut produces 1 event from the mock, so expect 5.
	assert.Len(t, events, 5, "all 5 shortcuts should be observed concurrently")
}

// ---------------------------------------------------------------------------
// observeShortcutDelta: ErrGone retry
// ---------------------------------------------------------------------------

func TestObserveShortcutDelta_RetryOnErrGone(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "sc-1",
		RemoteDrive:  "0000000000000099",
		RemoteItem:   "source-folder-1",
		LocalPath:    "SharedFolder",
		Observation:  ObservationDelta,
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
	events, err := e.observeShortcutContentFromList(ctx, shortcuts, bl, nil)
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

func TestReconcileShortcutScopes_DeltaReconciliation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Register a delta-observation shortcut.
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "Shared/Delta", Observation: ObservationDelta, DiscoveredAt: 1000,
	}))

	// Seed baseline with an existing file (will NOT be in delta → becomes orphan).
	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionDownload, Success: true, Path: "Shared/Delta/old.txt",
		DriveID: remoteDriveID, ItemID: "old-file", ParentID: "root-1", ItemType: ItemTypeFile,
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

	events, err := e.reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)

	// Should have: 1 create (new file) + 1 delete (orphan old file).
	require.Len(t, events, 2)

	var creates, deletes int
	for _, ev := range events {
		switch ev.Type { //nolint:exhaustive // only create and delete are relevant
		case ChangeCreate:
			creates++
			assert.Equal(t, "Shared/Delta/new.txt", ev.Path)
		case ChangeDelete:
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

func TestReconcileShortcutScopes_EnumerateReconciliation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "Shared/Enum", Observation: ObservationEnumerate, DiscoveredAt: 1000,
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

	events, err := e.reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, ChangeCreate, events[0].Type)
	assert.Equal(t, "Shared/Enum/doc.txt", events[0].Path)
}

func TestReconcileShortcutScopes_CollisionSkipped(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Two shortcuts with colliding local paths.
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "Shared/Collide", Observation: ObservationDelta, DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID: "sc-2", RemoteDrive: "0000000000000099", RemoteItem: "root-2",
		LocalPath: "Shared/Collide", Observation: ObservationDelta, DiscoveredAt: 2000,
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

	events, err := e.reconcileShortcutScopes(ctx, bl)
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

func TestReconcileShortcutScopes_PerScopeErrorIsolation(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	remoteDriveID := driveid.New("0000000000000099")

	// Two shortcuts: one with failing delta, one with working lister.
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID: "sc-fail", RemoteDrive: "0000000000000099", RemoteItem: "root-fail",
		LocalPath: "Shared/Fail", Observation: ObservationDelta, DiscoveredAt: 1000,
	}))
	require.NoError(t, mgr.UpsertShortcut(ctx, &Shortcut{
		ItemID: "sc-ok", RemoteDrive: "0000000000000099", RemoteItem: "root-ok",
		LocalPath: "Shared/Ok", Observation: ObservationEnumerate, DiscoveredAt: 2000,
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

	events, err := e.reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err, "overall reconciliation should succeed even if one scope fails")

	// Only the successful scope's events should appear.
	require.Len(t, events, 1)
	assert.Equal(t, "Shared/Ok/ok.txt", events[0].Path)
}

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
	events, err := e.reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestReconcileShortcutScopes_NilFetchersReturnsNil(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	bl := emptyBaseline()
	events, err := e.reconcileShortcutScopes(ctx, bl)
	require.NoError(t, err)
	assert.Nil(t, events)
}

// ---------------------------------------------------------------------------
// Source dedup tests
// ---------------------------------------------------------------------------

func TestDetectShortcutCollisions_DuplicateSourceFolder(t *testing.T) {
	t.Parallel()

	shortcuts := []Shortcut{
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

	sc := &Shortcut{
		ItemID: "sc-1", RemoteDrive: "0000000000000099", RemoteItem: "root-1",
		LocalPath: "SharedFolder",
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

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
