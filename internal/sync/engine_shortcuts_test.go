package sync

import (
	"context"
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
// mapShortcutPath
// ---------------------------------------------------------------------------

func TestMapShortcutPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		shortcut string
		relPath  string
		wantPath string
	}{
		{"direct child", "Shared/TeamDocs", "report.xlsx", "Shared/TeamDocs/report.xlsx"},
		{"nested child", "SharedFolder", "sub/file.txt", "SharedFolder/sub/file.txt"},
		{"deep nesting", "A/B", "C/D/E.txt", "A/B/C/D/E.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mapShortcutPath(tt.shortcut, tt.relPath)
			assert.Equal(t, tt.wantPath, got)
		})
	}
}

// ---------------------------------------------------------------------------
// buildShortcutRelPath
// ---------------------------------------------------------------------------

func TestBuildShortcutRelPath_DirectChild(t *testing.T) {
	t.Parallel()

	item := &graph.Item{
		ID:       "file-1",
		Name:     "report.xlsx",
		ParentID: "root-folder",
	}

	parentMap := map[string]shortcutParent{}

	got := buildShortcutRelPath(item, parentMap, "root-folder")
	assert.Equal(t, "report.xlsx", got)
}

func TestBuildShortcutRelPath_NestedChild(t *testing.T) {
	t.Parallel()

	item := &graph.Item{
		ID:       "file-1",
		Name:     "report.xlsx",
		ParentID: "subfolder-1",
	}

	parentMap := map[string]shortcutParent{
		"subfolder-1": {name: "Documents", parentID: "root-folder"},
	}

	got := buildShortcutRelPath(item, parentMap, "root-folder")
	assert.Equal(t, "Documents/report.xlsx", got)
}

func TestBuildShortcutRelPath_DeeplyNested(t *testing.T) {
	t.Parallel()

	item := &graph.Item{
		ID:       "file-1",
		Name:     "data.csv",
		ParentID: "folder-c",
	}

	parentMap := map[string]shortcutParent{
		"folder-c": {name: "C", parentID: "folder-b"},
		"folder-b": {name: "B", parentID: "root-folder"},
	}

	got := buildShortcutRelPath(item, parentMap, "root-folder")
	assert.Equal(t, "B/C/data.csv", got)
}

// ---------------------------------------------------------------------------
// shortcutItemsToEvents
// ---------------------------------------------------------------------------

func TestShortcutItemsToEvents_NewFiles(t *testing.T) {
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
	events := shortcutItemsToEvents(items, sc, remoteDriveID, bl)

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

func TestShortcutItemsToEvents_ExistingModified(t *testing.T) {
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

	events := shortcutItemsToEvents(items, sc, remoteDriveID, bl)
	require.Len(t, events, 1)
	assert.Equal(t, ChangeModify, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
}

func TestShortcutItemsToEvents_DeletedItem(t *testing.T) {
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

	events := shortcutItemsToEvents(items, sc, remoteDriveID, bl)
	require.Len(t, events, 1)
	assert.Equal(t, ChangeDelete, events[0].Type)
	assert.Equal(t, "SharedFolder/report.xlsx", events[0].Path)
	assert.True(t, events[0].IsDeleted)
}

func TestShortcutItemsToEvents_SkipsRoot(t *testing.T) {
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
	events := shortcutItemsToEvents(items, sc, remoteDriveID, bl)
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

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	err := e.handleRemovedShortcuts(ctx, map[string]bool{"sc-1": true})
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

	e := &Engine{
		baseline: mgr,
		logger:   testLogger(t),
	}

	// Delete a different item ID — shortcut should remain.
	err := e.handleRemovedShortcuts(ctx, map[string]bool{"other-item": true})
	require.NoError(t, err)

	sc, err := mgr.GetShortcut(ctx, "sc-1")
	require.NoError(t, err)
	assert.NotNil(t, sc, "shortcut should still exist")
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

	e := &Engine{
		baseline:    mgr,
		folderDelta: mockFolderDelta,
		logger:      testLogger(t),
	}

	bl := emptyBaseline()
	events, err := e.observeShortcutContent(ctx, bl)
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

	e := &Engine{
		baseline:        mgr,
		recursiveLister: mockLister,
		logger:          testLogger(t),
	}

	bl := emptyBaseline()
	events, err := e.observeShortcutContent(ctx, bl)
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

	e := &Engine{
		baseline:    mgr,
		folderDelta: &mockFolderDeltaFetcher{err: assert.AnError},
		logger:      testLogger(t),
	}

	bl := emptyBaseline()
	events, err := e.observeShortcutContent(ctx, bl)
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
	events, err := e.observeShortcutContent(ctx, bl)
	require.NoError(t, err)
	assert.Empty(t, events)
}

// ---------------------------------------------------------------------------
// processShortcuts (integration)
// ---------------------------------------------------------------------------

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

type mockRecursiveLister struct {
	items []graph.Item
	err   error
}

func (m *mockRecursiveLister) ListChildrenRecursive(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
	return m.items, m.err
}
