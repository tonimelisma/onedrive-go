package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func kindTestObserver(t *testing.T, bl *Baseline, items []graph.Item, getItem func(string) (*graph.Item, error)) *remoteObserver {
	t.Helper()

	driveID := driveid.New(engineTestDriveID)
	fetcher := &mockDeltaFetcher{pages: []mockDeltaPage{{page: deltaPageWithItems(append([]graph.Item{
		{ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID},
	}, items...), "next-token")}}}
	obs := newRemoteObserver(fetcher, bl, driveID, testLogger(t))
	obs.SetItemClient(&engineMockClient{getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
		if getItem == nil {
			return nil, graph.ErrNotFound
		}
		return getItem(itemID)
	}})

	return obs
}

func kindTestEvent(t *testing.T, events []changeEvent, itemID string) *changeEvent {
	t.Helper()

	for i := range events {
		if events[i].ItemID == itemID {
			return &events[i]
		}
	}

	return nil
}

// Validates: R-2.4.16
func TestFullDelta_FacetlessUpdateOfABaselineFolderKeepsItsType(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	bl := baselineWith(&BaselineEntry{Path: "Photos", DriveID: driveID, ItemID: "folder-1", ParentID: "root", ItemType: ItemTypeFolder})
	obs := kindTestObserver(t, bl, []graph.Item{
		// A sparse update: name and parent, but no facet saying what it is.
		{ID: "folder-1", Name: "Photos", ParentID: "root", DriveID: driveID, TypeFacetMissing: true},
	}, nil)

	events, _, err := obs.FullDelta(t.Context(), "saved")
	require.NoError(t, err)

	ev := kindTestEvent(t, events, "folder-1")
	require.NotNil(t, ev)
	// As a "file" it planned a folder-to-file type change, which deletes
	// the local folder.
	assert.Equal(t, ItemTypeFolder, ev.ItemType)
}

// Validates: R-2.4.16
func TestFullDelta_FacetlessNewItemIsReadOnceOrSkipped(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	reads := map[string]int{}
	obs := kindTestObserver(t, emptyBaseline(), []graph.Item{
		{ID: "resolved", Name: "Docs", ParentID: "root", DriveID: driveID, TypeFacetMissing: true},
		{ID: "unknown", Name: "Mystery", ParentID: "root", DriveID: driveID, TypeFacetMissing: true},
	}, func(itemID string) (*graph.Item, error) {
		reads[itemID]++
		if itemID == "resolved" {
			return &graph.Item{ID: itemID, Name: "Docs", ParentID: "root", DriveID: driveID, IsFolder: true}, nil
		}
		return &graph.Item{ID: itemID, Name: "Mystery", ParentID: "root", DriveID: driveID, TypeFacetMissing: true}, nil
	})

	events, _, err := obs.FullDelta(t.Context(), "saved")
	require.NoError(t, err)

	resolved := kindTestEvent(t, events, "resolved")
	require.NotNil(t, resolved, "a re-read that names the kind is used")
	assert.Equal(t, ItemTypeFolder, resolved.ItemType)
	assert.Nil(t, kindTestEvent(t, events, "unknown"), "an item of unknown kind is never guessed to be a file")
	assert.Equal(t, map[string]int{"resolved": 1, "unknown": 1}, reads, "one re-read each")
}

// Validates: R-2.4.16
//
// A new facetless item whose one re-read fails for a passing reason used to
// be skipped for good: the window completed and the delta token was saved
// past it. Like a parentless item whose re-read fails, it now marks the
// observation incomplete so the window is replayed and the cursor stays.
func TestFullDelta_FacetlessNewItemReReadFailureReplaysWindow(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	obs := kindTestObserver(t, emptyBaseline(), []graph.Item{
		{ID: "flaky", Name: "Mystery", ParentID: "root", DriveID: driveID, TypeFacetMissing: true},
	}, func(string) (*graph.Item, error) {
		return nil, errors.New("503 service unavailable")
	})

	events, token, err := obs.FullDelta(t.Context(), "saved")
	require.ErrorIs(t, err, errRemoteObservationIncomplete)
	assert.Empty(t, token, "the cursor does not advance past a window that could not be classified")
	assert.Nil(t, kindTestEvent(t, events, "flaky"))
}

// Validates: R-2.4.16
//
// A re-read that finds no item is not a passing failure: the item is gone and
// its deletion arrives in a later window, so replaying this one would never
// change the answer. It keeps the skip.
func TestFullDelta_FacetlessNewItemGoneOnReReadIsSkipped(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	obs := kindTestObserver(t, emptyBaseline(), []graph.Item{
		{ID: "gone", Name: "Mystery", ParentID: "root", DriveID: driveID, TypeFacetMissing: true},
		{ID: "plain", Name: "b.jpg", ParentID: "root", DriveID: driveID, QuickXorHash: "h2", Size: 1},
	}, nil)

	events, token, err := obs.FullDelta(t.Context(), "saved")
	require.NoError(t, err)
	assert.Equal(t, "next-token", token)
	assert.Nil(t, kindTestEvent(t, events, "gone"))
	assert.NotNil(t, kindTestEvent(t, events, "plain"))
}

// Validates: R-2.4.16
func TestFullDelta_BundleAndItsChildrenAreSkipped(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	obs := kindTestObserver(t, emptyBaseline(), []graph.Item{
		{ID: "album", Name: "Holiday", ParentID: "root", DriveID: driveID, IsBundle: true},
		{ID: "in-album", Name: "a.jpg", ParentID: "album", DriveID: driveID, QuickXorHash: "h", Size: 1},
		{ID: "plain", Name: "b.jpg", ParentID: "root", DriveID: driveID, QuickXorHash: "h2", Size: 1},
	}, nil)

	events, _, err := obs.FullDelta(t.Context(), "saved")
	require.NoError(t, err)

	assert.Nil(t, kindTestEvent(t, events, "album"))
	assert.Nil(t, kindTestEvent(t, events, "in-album"))
	assert.NotNil(t, kindTestEvent(t, events, "plain"))
}

// Validates: R-2.4.16
func TestFullDelta_NegativeSizeIsUnknown(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	obs := kindTestObserver(t, emptyBaseline(), []graph.Item{
		{ID: "odd", Name: "odd.bin", ParentID: "root", DriveID: driveID, QuickXorHash: "h", Size: -1},
	}, nil)

	events, _, err := obs.FullDelta(t.Context(), "saved")
	require.NoError(t, err)

	ev := kindTestEvent(t, events, "odd")
	require.NotNil(t, ev)
	assert.Zero(t, ev.Size)
	assert.True(t, ev.SizeUnknown)

	mgr := newTestStore(t)
	require.NoError(t, mgr.CommitObservation(t.Context(), projectObservedItems(testLogger(t), events), "t", driveID))
	row := readRemoteStateRow(t, mgr.rawDB(), "odd")
	require.NotNil(t, row)
	assert.False(t, row.SizeKnown, "a negative size is stored as unknown, not as a size")
}
