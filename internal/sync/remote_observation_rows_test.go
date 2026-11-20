package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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
	mtime := syncdomain.StoredTimeNano(modified)
	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{Path: "old", DriveID: driveID, ItemID: "old", ParentID: "root", ItemType: syncdomain.ItemTypeFolder},
		&syncdomain.BaselineEntry{Path: "new", DriveID: driveID, ItemID: "new", ParentID: "root", ItemType: syncdomain.ItemTypeFolder},
		&syncdomain.BaselineEntry{Path: "doc.txt", DriveID: driveID, ItemID: "doc", ParentID: "root", ItemType: syncdomain.ItemTypeFile, RemoteHash: "h0"},
		&syncdomain.BaselineEntry{Path: "gone.txt", DriveID: driveID, ItemID: "gone", ParentID: "root", ItemType: syncdomain.ItemTypeFile},
		&syncdomain.BaselineEntry{Path: "old/m.txt", DriveID: driveID, ItemID: "m", ParentID: "old", ItemType: syncdomain.ItemTypeFile, RemoteHash: "hm"},
		&syncdomain.BaselineEntry{Path: "old/mc.txt", DriveID: driveID, ItemID: "mc", ParentID: "old", ItemType: syncdomain.ItemTypeFile, RemoteHash: "h0"},
		&syncdomain.BaselineEntry{Path: "orphan.txt", DriveID: driveID, ItemID: "orphan", ParentID: "root", ItemType: syncdomain.ItemTypeFile},
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
	row := func(id, path, parent, hash string) syncdomain.ObservedItem {
		return syncdomain.ObservedItem{
			DriveID: driveID, ItemID: id, ParentID: parent, Path: path, ItemType: syncdomain.ItemTypeFile,
			Hash: hash, Size: 7, Mtime: mtime, ETag: "e-" + id,
		}
	}

	converter := syncobserve.NewPrimaryConverter(baseline, driveID, synctest.TestLogger(t), nil, nil)
	events := converter.ConvertItems(t.Context(), items)
	events = append(events, syncstore.FindBaselineOrphans(baseline, map[string]struct{}{
		"old": {}, "new": {}, "doc": {}, "gone": {}, "m": {}, "mc": {},
	}, driveID, "")...)
	result := syncobserve.ProjectRemoteObservations(synctest.TestLogger(t), events)

	assert.Equal(t, []syncdomain.ObservedItem{
		row("created", "created.txt", "root", "hc"),
		row("doc", "doc.txt", "root", "h1"),
		{DriveID: driveID, ItemID: "gone", Path: "gone.txt", ItemType: syncdomain.ItemTypeFile, IsDeleted: true},
		row("m", "new/m.txt", "new", "hm"),
		row("mc", "new/mc.txt", "new", "h2"),
		{DriveID: driveID, ItemID: "orphan", Path: "orphan.txt", ItemType: syncdomain.ItemTypeFile, IsDeleted: true},
	}, result.Observed)
	assert.Equal(t, []syncobserve.RemoteChange{
		{Path: "created.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "doc.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "gone.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "new/m.txt", OldPath: "old/m.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "new/mc.txt", OldPath: "old/mc.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "orphan.txt", ItemType: syncdomain.ItemTypeFile},
	}, result.Changes, "an item without an ID and one that cannot be placed are neither rows nor changes")
}

// Validates: R-2.8.8
//
// A change whose item has no ID stores no row, but it is still a change: the
// cursor advances over it, as over any non-empty window.
func TestRemoteBatch_AChangeWithoutAnItemIDStillCountsForTheCursor(t *testing.T) {
	t.Parallel()

	engine := &Engine{logger: synctest.TestLogger(t)}
	withoutID := buildRemoteObservationBatch(engine, syncdomain.RemoteObservationModeDelta,
		[]syncdomain.RemoteItem{{ObservedItem: syncdomain.ObservedItem{Path: "a.txt", ItemType: syncdomain.ItemTypeFile}}}, "token", false, syncdomain.ObservationFindingsBatch{})
	empty := buildRemoteObservationBatch(engine, syncdomain.RemoteObservationModeDelta, nil, "token", false, syncdomain.ObservationFindingsBatch{})

	assert.Empty(t, withoutID.Observed, "an item without an ID stores no row")
	assert.Equal(t, []syncobserve.RemoteChange{{Path: "a.txt", ItemType: syncdomain.ItemTypeFile}}, withoutID.Changes, "it is still a change")
	assert.Equal(t, "token", withoutID.CursorToken, "and the cursor advances over it")
	assert.Empty(t, empty.CursorToken, "a window with no change keeps the cursor where it was")
}
