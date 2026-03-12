package sync

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Constructor tests
// ---------------------------------------------------------------------------

func TestNewPrimaryConverter_EnablesVaultAndShortcutDetect(t *testing.T) {
	t.Parallel()

	bl := emptyBaseline()
	driveID := driveid.New(testDriveID)
	stats := &observerCounters{}

	c := newPrimaryConverter(bl, driveID, testLogger(t), stats)

	assert.True(t, c.enableVaultFilter, "primary converter should enable vault filter")
	assert.True(t, c.enableShortcutDetect, "primary converter should enable shortcut detect")
	assert.Empty(t, c.pathPrefix, "primary converter should have no path prefix")
	assert.Empty(t, c.scopeRootID, "primary converter should have no scope root")
	assert.False(t, c.skipNestedShortcuts, "primary converter should not skip nested shortcuts")
}

func TestNewShortcutConverter_EnablesShortcutBehavior(t *testing.T) {
	t.Parallel()

	bl := emptyBaseline()
	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "source-folder-1",
		LocalPath:   "Shared/TeamDocs",
	}

	c := newShortcutConverter(bl, remoteDriveID, testLogger(t), sc)

	assert.False(t, c.enableVaultFilter, "shortcut converter should not enable vault filter")
	assert.False(t, c.enableShortcutDetect, "shortcut converter should not enable shortcut detect")
	assert.Equal(t, "Shared/TeamDocs", c.pathPrefix, "shortcut converter should set path prefix")
	assert.Equal(t, "source-folder-1", c.scopeRootID, "shortcut converter should set scope root")
	assert.True(t, c.skipNestedShortcuts, "shortcut converter should skip nested shortcuts")
}

// ---------------------------------------------------------------------------
// Bug regression tests — these bugs existed in the old shortcut code path
// ---------------------------------------------------------------------------

// TestShortcutConverter_NFCNormalization verifies that NFC normalization is
// applied to item names in shortcut scope. The old shortcutItemsToEvents used
// raw item.Name without normalization — Unicode file names with NFD encoding
// (macOS default) would not match baseline entries stored in NFC.
func TestShortcutConverter_NFCNormalization(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	// NFD decomposed: e + combining acute accent (U+0301).
	nfd := "re\u0301sume\u0301.txt"
	// NFC composed: precomposed characters.
	nfc := "r\u00e9sum\u00e9.txt"

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		{
			ID:       "f1",
			Name:     nfd,
			ParentID: "scope-root",
			DriveID:  remoteDriveID,
			Size:     100,
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	require.Len(t, events, 1)
	assert.Equal(t, nfc, events[0].Name, "shortcut items should be NFC-normalized")
	assert.Equal(t, "SharedFolder/"+nfc, events[0].Path, "shortcut paths should use NFC-normalized names")
}

// TestShortcutConverter_MoveDetection verifies that file renames inside shared
// folders produce ChangeMove events instead of ChangeModify. The old code path
// always classified existing items as ChangeModify, causing the planner to
// never create ActionLocalMove for shortcut-scoped files.
func TestShortcutConverter_MoveDetection(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "SharedFolder",
	}

	// File was at SharedFolder/old-name.txt, now renamed to new-name.txt.
	bl := baselineWith(&BaselineEntry{
		Path:    "SharedFolder/old-name.txt",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	items := []graph.Item{
		{
			ID:       "f1",
			Name:     "new-name.txt",
			ParentID: "scope-root",
			DriveID:  remoteDriveID,
			Size:     100,
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))

	require.Len(t, events, 1)
	assert.Equal(t, ChangeMove, events[0].Type, "renamed file in shortcut scope should be ChangeMove")
	assert.Equal(t, "SharedFolder/new-name.txt", events[0].Path, "new path")
	assert.Equal(t, "SharedFolder/old-name.txt", events[0].OldPath, "old path")
}

// TestShortcutConverter_DeletedItemNameRecovery verifies that deleted items in
// shortcut scope recover their Name from the baseline when the Graph API
// returns an empty Name (Business API behavior). The old code path did not
// attempt recovery, leaving the Name empty.
func TestShortcutConverter_DeletedItemNameRecovery(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "SharedFolder",
	}

	bl := baselineWith(&BaselineEntry{
		Path:    "SharedFolder/budget.xlsx",
		DriveID: remoteDriveID,
		ItemID:  "f1",
	})

	// Business API: deleted item with empty Name.
	items := []graph.Item{
		{
			ID:        "f1",
			Name:      "",
			ParentID:  "scope-root",
			DriveID:   remoteDriveID,
			IsDeleted: true,
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, bl, testLogger(t))

	require.Len(t, events, 1)
	assert.Equal(t, ChangeDelete, events[0].Type)
	assert.Equal(t, "budget.xlsx", events[0].Name, "name recovered from baseline")
	assert.Equal(t, "SharedFolder/budget.xlsx", events[0].Path, "path recovered from baseline")
}

// TestShortcutConverter_OrphanWarning verifies that items with missing parents
// in shortcut scope produce a warning log and a partial path (just the item
// name, prefixed). The old code silently fell back to the bare name without
// logging.
func TestShortcutConverter_OrphanWarning(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "SharedFolder",
	}

	// Item whose parent is not in the batch or baseline.
	items := []graph.Item{
		{
			ID:       "f1",
			Name:     "orphan.txt",
			ParentID: "unknown-parent",
			DriveID:  remoteDriveID,
			Size:     100,
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	require.Len(t, events, 1)
	assert.Equal(t, ChangeCreate, events[0].Type)
	// Orphan gets prefixed path with just its own name.
	assert.Equal(t, "SharedFolder/orphan.txt", events[0].Path, "orphan should get prefixed partial path")
}

// ---------------------------------------------------------------------------
// Shortcut-specific behavior tests
// ---------------------------------------------------------------------------

// TestShortcutConverter_PathPrefix verifies that all paths are prefixed with
// the shortcut's local path.
func TestShortcutConverter_PathPrefix(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "Deep/Nested/Path",
	}

	items := []graph.Item{
		{
			ID:       "d1",
			Name:     "SubDir",
			ParentID: "scope-root",
			DriveID:  remoteDriveID,
			IsFolder: true,
		},
		{
			ID:       "f1",
			Name:     "file.txt",
			ParentID: "d1",
			DriveID:  remoteDriveID,
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	require.Len(t, events, 2)
	assert.Equal(t, "Deep/Nested/Path/SubDir", events[0].Path)
	assert.Equal(t, "Deep/Nested/Path/SubDir/file.txt", events[1].Path)
}

// TestShortcutConverter_ScopeRootSkip verifies that the scope root item
// (the shortcut folder itself) is skipped and does not produce an event.
func TestShortcutConverter_ScopeRootSkip(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		// The scope root itself — should be skipped.
		{
			ID:       "scope-root",
			Name:     "SharedContent",
			ParentID: "",
			DriveID:  remoteDriveID,
			IsFolder: true,
		},
		// A child of the scope root — should produce an event.
		{
			ID:       "f1",
			Name:     "file.txt",
			ParentID: "scope-root",
			DriveID:  remoteDriveID,
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	require.Len(t, events, 1)
	assert.Equal(t, "f1", events[0].ItemID, "only the child should produce an event")
	assert.Equal(t, "SharedFolder/file.txt", events[0].Path)
}

// TestShortcutConverter_NestedShortcutSkip verifies that items with a
// RemoteItemID (nested shortcuts) are skipped in shortcut scope.
func TestShortcutConverter_NestedShortcutSkip(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "SharedFolder",
	}

	items := []graph.Item{
		// Normal file.
		{ID: "f1", Name: "normal.txt", ParentID: "scope-root", DriveID: remoteDriveID},
		// Nested shortcut — should be skipped.
		{
			ID: "nested-sc", Name: "NestedShared", ParentID: "scope-root",
			DriveID: remoteDriveID, IsFolder: true,
			RemoteItemID: "remote-nested", RemoteDriveID: "other-drive",
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	require.Len(t, events, 1)
	assert.Equal(t, "f1", events[0].ItemID)
}

// ---------------------------------------------------------------------------
// Primary drive behavior tests
// ---------------------------------------------------------------------------

// TestPrimaryConverter_VaultExclusion verifies that vault items are excluded
// by the primary converter.
func TestPrimaryConverter_VaultExclusion(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	bl := emptyBaseline()
	c := newPrimaryConverter(bl, driveID, testLogger(t), &observerCounters{})

	inflight := map[driveid.ItemKey]inflightParent{
		driveid.NewItemKey(driveID, "root"):         {name: "", isRoot: true},
		driveid.NewItemKey(driveID, "vault-folder"): {name: "Personal Vault", parentID: "root", isVault: true},
	}

	// Vault folder itself.
	vaultItem := &graph.Item{
		ID: "vault-folder", Name: "Personal Vault", ParentID: "root",
		DriveID: driveID, IsFolder: true, SpecialFolderName: "vault",
	}
	assert.Nil(t, c.classifyItem(vaultItem, inflight), "vault folder should be skipped")

	// Child of vault.
	vaultChild := &graph.Item{
		ID: "vault-child", Name: "secret.pdf", ParentID: "vault-folder",
		DriveID: driveID, Size: 1024,
	}
	assert.Nil(t, c.classifyItem(vaultChild, inflight), "vault child should be skipped")

	// Normal file outside vault.
	normalFile := &graph.Item{
		ID: "normal-file", Name: "readme.txt", ParentID: "root",
		DriveID: driveID, Size: 256,
	}
	ev := c.classifyItem(normalFile, inflight)
	assert.NotNil(t, ev, "normal file should produce an event")
	assert.Equal(t, "readme.txt", ev.Path)
}

// TestPrimaryConverter_ShortcutDetection verifies that shortcut items are
// classified as ChangeShortcut by the primary converter.
func TestPrimaryConverter_ShortcutDetection(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	bl := emptyBaseline()
	c := newPrimaryConverter(bl, driveID, testLogger(t), &observerCounters{})

	inflight := map[driveid.ItemKey]inflightParent{
		driveid.NewItemKey(driveID, "root"): {name: "", isRoot: true},
	}

	item := &graph.Item{
		ID: "sc-1", Name: "TeamDocs", ParentID: "root", DriveID: driveID,
		IsFolder: true, RemoteDriveID: "remote-drive", RemoteItemID: "remote-item",
	}

	ev := c.classifyItem(item, inflight)
	require.NotNil(t, ev)
	assert.Equal(t, ChangeShortcut, ev.Type)
	assert.Equal(t, "TeamDocs", ev.Path)
	assert.Equal(t, "remote-drive", ev.RemoteDriveID)
}

// TestPrimaryConverter_NilStatsIsSafe verifies that a nil stats pointer
// doesn't panic (shortcut converter has nil stats).
func TestPrimaryConverter_NilStatsIsSafe(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	bl := emptyBaseline()

	// Create converter with nil stats (like shortcut converter).
	c := &itemConverter{
		baseline: bl,
		driveID:  driveID,
		logger:   testLogger(t),
		stats:    nil,
	}

	inflight := map[driveid.ItemKey]inflightParent{
		driveid.NewItemKey(driveID, "root"): {name: "", isRoot: true},
	}

	item := &graph.Item{
		ID: "f1", Name: "file.txt", ParentID: "root", DriveID: driveID,
		QuickXorHash: "hash123",
	}

	// Should not panic with nil stats.
	ev := c.classifyItem(item, inflight)
	require.NotNil(t, ev)
	assert.Equal(t, "hash123", ev.Hash)
}

// ---------------------------------------------------------------------------
// ConvertItems tests
// ---------------------------------------------------------------------------

// TestConvertItems_TwoPassProcessing verifies that ConvertItems handles items
// in any order (child before parent) through two-pass processing.
func TestConvertItems_TwoPassProcessing(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "Shared",
	}

	// Child before parent in the array.
	items := []graph.Item{
		{ID: "f1", Name: "deep.txt", ParentID: "d1", DriveID: remoteDriveID},
		{ID: "d1", Name: "SubDir", ParentID: "scope-root", DriveID: remoteDriveID, IsFolder: true},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	require.Len(t, events, 2)

	// Find the file event regardless of output order.
	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			fileEvent = &events[i]

			break
		}
	}

	require.NotNil(t, fileEvent)
	assert.Equal(t, "Shared/SubDir/deep.txt", fileEvent.Path, "child-before-parent should resolve correctly")
}

// TestConvertItems_HashAndTimestamp verifies that hash and timestamp are
// correctly populated for shortcut items.
func TestConvertItems_HashAndTimestamp(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	ts := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)

	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "Shared",
	}

	items := []graph.Item{
		{
			ID: "f1", Name: "data.csv", ParentID: "scope-root", DriveID: remoteDriveID,
			QuickXorHash: "qxh123", Size: 500, ModifiedAt: ts,
		},
	}

	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), testLogger(t))

	require.Len(t, events, 1)
	assert.Equal(t, "qxh123", events[0].Hash)
	assert.Equal(t, int64(500), events[0].Size)
	assert.Equal(t, ts.UnixNano(), events[0].Mtime)
}

// TestConvertShortcutItems_NilLogger verifies that passing a nil logger
// doesn't panic — the function should fall back to slog.Default().
func TestConvertShortcutItems_NilLogger(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "Shared",
	}

	items := []graph.Item{
		{ID: "f1", Name: "file.txt", ParentID: "scope-root", DriveID: remoteDriveID},
	}

	// Should not panic with nil logger.
	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), nil)
	require.Len(t, events, 1)
}

// TestConvertShortcutItems_NilLoggerPassedDefault verifies that the nil-safe
// logger fallback actually produces a working logger.
func TestConvertShortcutItems_NilLoggerPassedDefault(t *testing.T) {
	t.Parallel()

	remoteDriveID := driveid.New("0000000000000099")
	sc := &Shortcut{
		ItemID:      "sc-1",
		RemoteDrive: "0000000000000099",
		RemoteItem:  "scope-root",
		LocalPath:   "Shared",
	}

	// Item with unknown parent to trigger orphan warning log.
	items := []graph.Item{
		{ID: "f1", Name: "orphan.txt", ParentID: "unknown", DriveID: remoteDriveID},
	}

	// nil logger falls back to slog.Default(), should not panic.
	events := convertShortcutItems(items, sc, remoteDriveID, emptyBaseline(), (*slog.Logger)(nil))
	require.Len(t, events, 1)
}
