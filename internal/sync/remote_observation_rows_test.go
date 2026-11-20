package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.7.28, R-2.15.3
//
// Every kind of item the converter and the orphan finder report becomes its
// whole stored row and the three facts of its change; an item that cannot be
// placed becomes neither.
func TestRemoteObservation_RowsAndChangesForEveryItemKind(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	modified := time.Date(2026, time.November, 19, 12, 0, 0, 0, time.UTC)
	mtime := storedTimeNano(modified)
	baseline := baselineWith(
		&BaselineEntry{Path: "old", DriveID: driveID, ItemID: "old", ParentID: "root", ItemType: ItemTypeFolder},
		&BaselineEntry{Path: "new", DriveID: driveID, ItemID: "new", ParentID: "root", ItemType: ItemTypeFolder},
		&BaselineEntry{Path: "doc.txt", DriveID: driveID, ItemID: "doc", ParentID: "root", ItemType: ItemTypeFile, RemoteHash: "h0"},
		&BaselineEntry{Path: "gone.txt", DriveID: driveID, ItemID: "gone", ParentID: "root", ItemType: ItemTypeFile},
		&BaselineEntry{Path: "old/m.txt", DriveID: driveID, ItemID: "m", ParentID: "old", ItemType: ItemTypeFile, RemoteHash: "hm"},
		&BaselineEntry{Path: "old/mc.txt", DriveID: driveID, ItemID: "mc", ParentID: "old", ItemType: ItemTypeFile, RemoteHash: "h0"},
		&BaselineEntry{Path: "orphan.txt", DriveID: driveID, ItemID: "orphan", ParentID: "root", ItemType: ItemTypeFile},
	)
	file := func(id, name, parent, hash string) graph.Item {
		return graph.Item{
			ID: id, Name: name, ParentID: parent, DriveID: driveID, Size: 7, QuickXorHash: hash,
			ModifiedAt: modified, ETag: "e-" + id,
		}
	}
	items := []graph.Item{
		{ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID},
		file("created", "created.txt", "root", "hc"),
		file("doc", "doc.txt", "root", "h1"),
		{ID: "gone", Name: "gone.txt", DriveID: driveID, IsDeleted: true},
		file("m", "m.txt", "new", "hm"),
		file("mc", "mc.txt", "new", "h2"),
		file("", "no-id.txt", "root", "hx"),
		file("lost", "", "", "hl"),
	}
	row := func(id, path, parent, hash string) observedItem {
		return observedItem{
			DriveID: driveID, ItemID: id, ParentID: parent, Path: path, ItemType: ItemTypeFile,
			Hash: hash, Size: 7, Mtime: mtime, ETag: "e-" + id,
		}
	}

	converter := newPrimaryConverter(baseline, driveID, synctest.TestLogger(t), nil, nil)
	events := converter.ConvertItems(t.Context(), items)
	events = append(events, findBaselineOrphans(baseline, map[string]struct{}{
		"old": {}, "new": {}, "doc": {}, "gone": {}, "m": {}, "mc": {},
	}, driveID, "")...)
	result := projectRemoteObservations(synctest.TestLogger(t), events)

	assert.Equal(t, []observedItem{
		row("created", "created.txt", "root", "hc"),
		row("doc", "doc.txt", "root", "h1"),
		{DriveID: driveID, ItemID: "gone", Path: "gone.txt", ItemType: ItemTypeFile, IsDeleted: true},
		row("m", "new/m.txt", "new", "hm"),
		row("mc", "new/mc.txt", "new", "h2"),
		{DriveID: driveID, ItemID: "orphan", Path: "orphan.txt", ItemType: ItemTypeFile, IsDeleted: true},
	}, result.observed)
	assert.Equal(t, []remoteChange{
		{Path: "created.txt", ItemType: ItemTypeFile},
		{Path: "doc.txt", ItemType: ItemTypeFile},
		{Path: "gone.txt", ItemType: ItemTypeFile},
		{Path: "new/m.txt", OldPath: "old/m.txt", ItemType: ItemTypeFile},
		{Path: "new/mc.txt", OldPath: "old/mc.txt", ItemType: ItemTypeFile},
		{Path: "orphan.txt", ItemType: ItemTypeFile},
	}, result.changes, "an item without an ID and one that cannot be placed are neither rows nor changes")
}

// Validates: R-2.8.8
//
// A change whose item has no ID stores no row, but it is still a change: the
// cursor advances over it, as over any non-empty window.
func TestRemoteBatch_AChangeWithoutAnItemIDStillCountsForTheCursor(t *testing.T) {
	t.Parallel()

	engine := &Engine{logger: synctest.TestLogger(t)}
	withoutID := buildRemoteObservationBatch(engine, remoteObservationModeDelta,
		[]remoteItem{{observedItem: observedItem{Path: "a.txt", ItemType: ItemTypeFile}}}, "token", false, ObservationFindingsBatch{})
	empty := buildRemoteObservationBatch(engine, remoteObservationModeDelta, nil, "token", false, ObservationFindingsBatch{})

	assert.Empty(t, withoutID.observed, "an item without an ID stores no row")
	assert.Equal(t, []remoteChange{{Path: "a.txt", ItemType: ItemTypeFile}}, withoutID.changes, "it is still a change")
	assert.Equal(t, "token", withoutID.cursorToken, "and the cursor advances over it")
	assert.Empty(t, empty.cursorToken, "a window with no change keeps the cursor where it was")
}
