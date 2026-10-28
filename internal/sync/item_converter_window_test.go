package sync

import (
	"context"
	"fmt"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.15.4
func TestFullDelta_CrossPageChildBeforeParentResolvesWithoutFetch(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	const orphans = 250
	var children, parents []graph.Item
	for i := range orphans {
		parentID := fmt.Sprintf("folder-%03d", i)
		children = append(children, graph.Item{
			ID: fmt.Sprintf("file-%03d", i), Name: "f.txt", ParentID: parentID, DriveID: driveID,
			QuickXorHash: "h", Size: 1,
		})
		parents = append(parents, graph.Item{
			ID: parentID, Name: parentID, ParentID: "root", DriveID: driveID, IsFolder: true,
		})
	}
	fetcher := &mockDeltaFetcher{pages: []mockDeltaPage{
		// Every child arrives a page before its folder.
		{page: &graph.DeltaPage{Items: append([]graph.Item{{ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID}}, children...), NextLink: "page-2"}},
		{page: &graph.DeltaPage{Items: parents, DeltaLink: "done"}},
	}}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveID, testLogger(t))
	obs.SetItemClient(&engineMockClient{getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
		require.Failf(t, "unexpected read", "the folder %s arrives later in the window", itemID)
		return nil, graph.ErrNotFound
	}})

	events, token, err := obs.FullDelta(t.Context(), "saved")

	require.NoError(t, err)
	assert.Equal(t, "done", token)
	files := 0
	for i := range events {
		if strings.HasPrefix(events[i].ItemID, "file-") {
			files++
			assert.Equal(t, strings.Replace(events[i].ItemID, "file", "folder", 1)+"/f.txt", events[i].Path)
		}
	}
	assert.Equal(t, orphans, files)
}

// Validates: R-2.15.4
func TestFullDelta_ManyOrphansCompleteAcrossReplaysWithoutRefetching(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	const orphans = maxAncestorFetchesPerWindow + 1
	items := []graph.Item{{ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID}}
	for i := range orphans {
		items = append(items, graph.Item{
			ID: fmt.Sprintf("file-%03d", i), Name: "f.txt", ParentID: fmt.Sprintf("folder-%03d", i), DriveID: driveID,
			QuickXorHash: "h", Size: 1,
		})
	}
	page := mockDeltaPage{page: &graph.DeltaPage{Items: items, DeltaLink: "done"}}
	fetcher := &mockDeltaFetcher{pages: []mockDeltaPage{page, page}}

	var mu stdsync.Mutex
	reads := map[string]int{}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveID, testLogger(t))
	obs.SetItemClient(&engineMockClient{getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
		mu.Lock()
		reads[itemID]++
		mu.Unlock()
		return &graph.Item{ID: itemID, Name: itemID, ParentID: "root", DriveID: driveID, IsFolder: true}, nil
	}})

	// The first attempt spends the window's budget and stays incomplete.
	_, _, err := obs.FullDelta(t.Context(), "saved")
	require.ErrorIs(t, err, errRemoteObservationIncomplete)

	// The replay reuses what it already read and spends the budget only on
	// the rest; it used to start from zero and fail the same way forever.
	events, _, err := obs.FullDelta(t.Context(), "saved")
	require.NoError(t, err)
	assert.Len(t, events, orphans)
	for id, n := range reads {
		assert.Equal(t, 1, n, "%s read once across replays", id)
	}
	assert.Len(t, reads, orphans)
}
