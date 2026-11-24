package syncobserve

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// The fixture clients are pinned to the item read the observers make.
var (
	_ ItemReader = (*syncfixture.TestMockItemClient)(nil)
	_ ItemReader = (*syncfixture.EngineMockClient)(nil)
)

// ---------------------------------------------------------------------------
// Mock DeltaFetcher
// ---------------------------------------------------------------------------

type mockDeltaPage struct {
	page *graph.DeltaPage
	err  error
}

type mockDeltaFetcher struct {
	pages    []mockDeltaPage
	calls    int
	driveIDs []driveid.ID // records driveID passed to each Delta call
}

func twoPageDeltaPages(firstLinkType, firstLink, secondDeltaLink string) []mockDeltaPage {
	firstPage := &graph.DeltaPage{
		Items: []graph.Item{
			{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
			{
				ID:              "f1",
				Name:            "a.txt",
				ParentID:        "root",
				ParentPathKnown: true,
				DriveID:         driveid.New(synctest.TestDriveID),
			},
		},
	}
	if firstLinkType == "next" {
		firstPage.NextLink = firstLink
	} else {
		firstPage.DeltaLink = firstLink
	}

	return []mockDeltaPage{
		{page: firstPage},
		{page: &graph.DeltaPage{
			Items: []graph.Item{
				{
					ID:              "f2",
					Name:            "b.txt",
					ParentID:        "root",
					ParentPathKnown: true,
					DriveID:         driveid.New(synctest.TestDriveID),
				},
			},
			DeltaLink: secondDeltaLink,
		}},
	}
}

func (m *mockDeltaFetcher) Delta(_ context.Context, driveID driveid.ID, _ string) (*graph.DeltaPage, error) {
	m.driveIDs = append(m.driveIDs, driveID)

	if m.calls >= len(m.pages) {
		return nil, errors.New("mock: no more pages configured")
	}

	p := m.pages[m.calls]
	m.calls++

	return p.page, p.err
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// Validates: R-6.7.3
func TestFullDelta_NewFiles(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{
						ID: "f1", Name: "hello.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), Size: 100,
						QuickXorHash: "qxh1", ModifiedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					},
					{
						ID: "f2", Name: "world.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), Size: 200,
						QuickXorHash: "qxh2", ModifiedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
					},
				},
				DeltaLink: "https://graph.microsoft.com/delta?token=new-token",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, token, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	assert.Equal(t, "https://graph.microsoft.com/delta?token=new-token", token, "token should be delta link URL")

	require.Len(t, events, 2)

	// First file.
	e := events[0]
	assert.False(t, e.IsDeleted)
	assert.Empty(t, e.OldPath)
	assert.Equal(t, "hello.txt", e.Path)
	assert.Equal(t, "qxh1", e.Hash)
	assert.Equal(t, int64(100), e.Size)

	// Second file.
	assert.Equal(t, "world.txt", path.Base(events[1].Path))
}

// Validates: R-6.7.3
func TestFullDelta_ModifiedFile(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "docs/readme.md", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ParentID: "folder1", ItemType: syncdomain.ItemTypeFile, RemoteHash: "old-hash",
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "folder1", Name: "docs", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), IsFolder: true},
					{
						ID: "f1", Name: "readme.md", ParentID: "folder1", DriveID: driveid.New(synctest.TestDriveID),
						QuickXorHash: "new-hash", Size: 512,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "prev-token")
	require.NoError(t, err, "FullDelta")

	// Root and folder produce events too (folder is classified).
	// Find the file event.
	var fileEvent *syncdomain.RemoteItem
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]

			break
		}
	}

	require.NotNil(t, fileEvent, "file event not found")
	assert.False(t, fileEvent.IsDeleted)
	assert.Empty(t, fileEvent.OldPath)
	assert.Equal(t, "docs/readme.md", fileEvent.Path)
	assert.Equal(t, "new-hash", fileEvent.Hash)
}

// Validates: R-2.4.17
func TestFullDelta_DeletedFile(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "photos/cat.jpg", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ItemType: syncdomain.ItemTypeFile,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "cat.jpg", DriveID: driveid.New(synctest.TestDriveID), IsDeleted: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	e := events[0]
	assert.True(t, e.IsDeleted)
	assert.True(t, e.IsDeleted)
	assert.Equal(t, "photos/cat.jpg", e.Path)
}

// Validates: R-6.7.29
func TestFullDelta_ModifiedFile_SparseFieldsRecoveredFromBaseline(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "docs/readme.md", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ParentID: "folder1", ItemType: syncdomain.ItemTypeFile, RemoteHash: "old-hash",
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{
						ID: sharedTestFileItemID, Name: "", ParentID: "", DriveID: driveid.New(synctest.TestDriveID),
						QuickXorHash: "new-hash", Size: 512,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "prev-token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.False(t, events[0].IsDeleted)
	assert.Empty(t, events[0].OldPath)
	assert.Equal(t, "docs/readme.md", events[0].Path)
	assert.Equal(t, "readme.md", path.Base(events[0].Path))
	assert.Equal(t, "new-hash", events[0].Hash)
}

// Validates: R-6.7.3
func TestFullDelta_MovedFile(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "old-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-old",
			ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
		},
		&syncdomain.BaselineEntry{
			Path: "old-folder/doc.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "folder-old", ItemType: syncdomain.ItemTypeFile,
		},
		&syncdomain.BaselineEntry{
			Path: "new-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-new",
			ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
		},
	)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					// File moved from old-folder to new-folder.
					{ID: sharedTestFileItemID, Name: "doc.txt", ParentID: "folder-new", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var moveEvent *syncdomain.RemoteItem
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			moveEvent = &events[i]

			break
		}
	}

	require.NotNil(t, moveEvent, "move event not found")
	assert.NotEmpty(t, moveEvent.OldPath)
	assert.Equal(t, "new-folder/doc.txt", moveEvent.Path)
	assert.Equal(t, "old-folder/doc.txt", moveEvent.OldPath)
}

// Validates: R-6.7.29
func TestFullDelta_MovedFile_SparseNameRecoveredFromBaseline(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "old-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-old",
			ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
		},
		&syncdomain.BaselineEntry{
			Path: "old-folder/doc.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "folder-old", ItemType: syncdomain.ItemTypeFile,
		},
		&syncdomain.BaselineEntry{
			Path: "new-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-new",
			ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
		},
	)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: sharedTestFileItemID, Name: "", ParentID: "folder-new", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.NotEmpty(t, events[0].OldPath)
	assert.Equal(t, "new-folder/doc.txt", events[0].Path)
	assert.Equal(t, "old-folder/doc.txt", events[0].OldPath)
	assert.Equal(t, "doc.txt", path.Base(events[0].Path))
}

// Validates: R-6.7.29
func TestFullDelta_DescendantFollowsSparseRenamedParentWithOmittedParentReference(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "Team", DriveID: driveid.New(synctest.TestDriveID), ItemID: "team",
			ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
		},
		&syncdomain.BaselineEntry{
			Path: "Team/Reports", DriveID: driveid.New(synctest.TestDriveID), ItemID: "reports",
			ParentID: "team", ItemType: syncdomain.ItemTypeFolder,
		},
		&syncdomain.BaselineEntry{
			Path: "Team/Reports/q1.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "reports", ItemType: syncdomain.ItemTypeFile,
		},
	)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					// Descendant arrives in the same sparse batch as its renamed parent.
					{ID: sharedTestFileItemID, Name: "q1.txt", ParentID: "reports", DriveID: driveid.New(synctest.TestDriveID), QuickXorHash: "new-hash"},
					// Parent name changed, but Graph omits unchanged parentReference.
					{ID: "reports", Name: "Statements", ParentID: "", DriveID: driveid.New(synctest.TestDriveID), IsFolder: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var fileEvent *syncdomain.RemoteItem
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]
			break
		}
	}

	require.NotNil(t, fileEvent, "descendant event not found")
	assert.NotEmpty(t, fileEvent.OldPath)
	assert.Equal(t, "Team/Statements/q1.txt", fileEvent.Path,
		"descendant should keep the renamed parent's baseline ancestry when parentReference is omitted")
	assert.Equal(t, "Team/Reports/q1.txt", fileEvent.OldPath)
}

// Validates: R-2.1.2
func TestFullDelta_MultiPage(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: twoPageDeltaPages("next", "next-page-url", "final-delta-link"),
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, token, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 2)
	assert.Equal(t, "final-delta-link", token)
	assert.Equal(t, 2, fetcher.calls)
}

func TestFullDelta_EmptyDelta(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items:     nil,
				DeltaLink: "empty-delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, token, err := obs.FullDelta(t.Context(), "prev-token")
	require.NoError(t, err, "FullDelta")

	assert.Empty(t, events)
	assert.Equal(t, "empty-delta-link", token)
}

// Validates: R-6.7.5
func TestFullDelta_DeltaExpired(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: graph.ErrGone,
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	_, _, err := obs.FullDelta(t.Context(), "expired-token")

	assert.ErrorIs(t, err, syncdomain.ErrDeltaExpired)
}

func TestFullDelta_FetchError(t *testing.T) {
	t.Parallel()

	fetchErr := errors.New("network timeout")
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: fetchErr,
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	_, _, err := obs.FullDelta(t.Context(), "token")

	require.Error(t, err, "expected error")
	assert.ErrorIs(t, err, fetchErr)
}

func TestFullDelta_SkipsRootItem(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID), Name: "root"},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	assert.Empty(t, events, "root should be skipped")
}

// Validates: R-6.7.3
func TestFullDelta_PathMaterialization_InFlight(t *testing.T) {
	t.Parallel()

	// Folder on page 1, file inside it on page 2. Both new.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{
				page: &graph.DeltaPage{
					Items: []graph.Item{
						{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
						{ID: "d1", Name: "Documents", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), IsFolder: true},
					},
					NextLink: "next-page",
				},
			},
			{
				page: &graph.DeltaPage{
					Items: []graph.Item{
						{ID: sharedTestFileItemID, Name: "report.pdf", ParentID: "d1", DriveID: driveid.New(synctest.TestDriveID)},
					},
					DeltaLink: "delta-link",
				},
			},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	// Find file event.
	var fileEvent *syncdomain.RemoteItem
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]

			break
		}
	}

	require.NotNil(t, fileEvent, "file event not found")
	assert.Equal(t, "Documents/report.pdf", fileEvent.Path)
}

// Validates: R-6.7.3
func TestFullDelta_PathMaterialization_Baseline(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "Projects/GoApp", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder1",
		ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "main.go", ParentID: "folder1", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, "Projects/GoApp/main.go", events[0].Path)
}

// Validates: R-6.7.3
func TestFullDelta_PathMaterialization_Mixed(t *testing.T) {
	t.Parallel()

	// Existing folder in baseline, new subfolder in inflight.
	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "Documents", DriveID: driveid.New(synctest.TestDriveID), ItemID: "docs",
		ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					// New subfolder under existing Documents.
					{ID: "sub1", Name: "Reports", ParentID: "docs", DriveID: driveid.New(synctest.TestDriveID), IsFolder: true},
					// New file under new subfolder.
					{ID: "f1", Name: "q1.pdf", ParentID: "sub1", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var fileEvent *syncdomain.RemoteItem
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]

			break
		}
	}

	require.NotNil(t, fileEvent, "file event not found")
	assert.Equal(t, "Documents/Reports/q1.pdf", fileEvent.Path)
}

// Validates: R-2.4.17
func TestFullDelta_DeletedItem_MissingName(t *testing.T) {
	t.Parallel()

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "work/budget.xlsx", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ItemType: syncdomain.ItemTypeFile,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					// Business API: deleted item with empty Name.
					{ID: "f1", Name: "", DriveID: driveid.New(synctest.TestDriveID), IsDeleted: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, "budget.xlsx", path.Base(events[0].Path), "recovered from baseline")
	assert.Equal(t, "work/budget.xlsx", events[0].Path)
}

// Validates: R-6.7.2
func TestFullDelta_DriveIDNormalization(t *testing.T) {
	t.Parallel()

	// 15-char uppercase driveID — should be lowercased + zero-padded.
	rawDriveID := "ABC123DGH456789"

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(rawDriveID)},
					{ID: "f1", Name: "test.txt", ParentID: "root", DriveID: driveid.New(rawDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(rawDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	want := driveid.New("0abc123dgh456789") // lowercased + zero-padded to 16
	assert.True(t, events[0].DriveID.Equal(want), "DriveID = %q, want %q", events[0].DriveID, want)

	// Verify the fetcher was called with the normalized driveID, not the raw one.
	require.Len(t, fetcher.driveIDs, 1, "fetcher call count")
	assert.True(t, fetcher.driveIDs[0].Equal(want), "fetcher received driveID = %q, want %q (normalized)", fetcher.driveIDs[0], want)
}

// Validates: R-2.13.1
func TestFullDelta_NFCNormalization(t *testing.T) {
	t.Parallel()

	// NFD decomposed: e + combining acute accent (U+0301).
	nfd := "re\u0301sume\u0301.txt"
	// NFC composed: precomposed characters.
	nfc := "r\u00e9sum\u00e9.txt"

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "f1", Name: nfd, ParentID: "root", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, nfc, path.Base(events[0].Path), "NFC-normalized")
	assert.Equal(t, nfc, events[0].Path, "NFC-normalized")
}

// Validates: R-2.2.1
func TestFullDelta_HashSelection(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					// QuickXorHash preferred.
					{
						ID: "f1", Name: "a.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID),
						QuickXorHash: "qxh", SHA256Hash: "sha",
					},
					// SHA256 fallback.
					{
						ID: "f2", Name: "b.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID),
						SHA256Hash: "sha-only",
					},
					// Neither.
					{ID: "f3", Name: "c.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 3)
	assert.Equal(t, "qxh", events[0].Hash, "QuickXorHash preferred")
	// SHA digests cannot be compared with local QuickXor: hashless.
	assert.Empty(t, events[1].Hash, "no SHA256 fallback")
	assert.Empty(t, events[2].Hash)
}

// Validates: R-6.7.16, R-6.7.26
func TestFullDelta_TimestampConversion(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 15, 12, 30, 45, 123456789, time.UTC)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "f1", Name: "test.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), ModifiedAt: ts},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	assert.Equal(t, ts.UnixNano(), events[0].Mtime)
}

// Validates: R-6.7.16, R-6.7.26
func TestFullDelta_ZeroTimestampRemainsUnknown(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "f1", Name: "test.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), ModifiedAt: time.Time{}},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, int64(0), events[0].Mtime, "unknown remote timestamps should stay zero")
}

func TestFullDelta_FolderEvent(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "d1", Name: "Photos", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), IsFolder: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, syncdomain.ItemTypeFolder, events[0].ItemType)
	assert.Empty(t, events[0].Hash, "folders have no hash")
}

func TestFullDelta_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: ctx.Err(),
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	_, _, err := obs.FullDelta(ctx, "token")

	require.Error(t, err, "expected error")
	assert.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// Unit tests for helper functions
// ---------------------------------------------------------------------------

// Validates: R-6.7.2
func TestDriveIDNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"15-char lowercase", "abc123def456789", "0abc123def456789"},
		{"16-char lowercase", "abc123def4567890", "abc123def4567890"},
		{"uppercase", "ABC123DGH4567890", "abc123dgh4567890"},
		{"15-char uppercase", "ABC123DGH456789", "0abc123dgh456789"},
		{"already normalized", "0000000000000001", "0000000000000001"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := driveid.New(tt.in)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func TestClassifyItemType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item graph.Item
		want syncdomain.ItemType
	}{
		{"file", graph.Item{}, syncdomain.ItemTypeFile},
		{"folder", graph.Item{IsFolder: true}, syncdomain.ItemTypeFolder},
		{"root", graph.Item{IsRoot: true}, syncdomain.ItemTypeRoot},
		{"root takes precedence", graph.Item{IsRoot: true, IsFolder: true}, syncdomain.ItemTypeRoot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyItemType(&tt.item)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-2.2.1
func TestSelectHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item graph.Item
		want string
	}{
		{"QuickXor present", graph.Item{QuickXorHash: "qxh", SHA256Hash: "sha"}, "qxh"},
		{"SHA256 only", graph.Item{SHA256Hash: "sha"}, ""},
		{"neither", graph.Item{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := driveops.SelectHash(&tt.item)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Validates: R-6.7.28
func TestFullDelta_OrphanedItem(t *testing.T) {
	t.Parallel()

	// Item whose parent is not in the inflight map or baseline.
	// Observer should fail the batch instead of fabricating a root-level path
	// or advancing the cursor past incomplete provider truth.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "orphan.txt", ParentID: "unknown-parent", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, token, err := obs.FullDelta(t.Context(), "")
	require.ErrorIs(t, err, syncdomain.ErrRemoteObservationIncomplete)

	assert.Empty(t, events)
	assert.Empty(t, token)
}

// Validates: R-6.7.3
func TestFullDelta_DeletedItem_NotInBaseline(t *testing.T) {
	t.Parallel()

	// Item created and deleted between syncs — appears as deleted in delta
	// but has no baseline entry. The observer should still materialize a path
	// from the delta payload instead of emitting an empty-path delete.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "ephemeral.txt", DriveID: driveid.New(synctest.TestDriveID), IsDeleted: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.True(t, events[0].IsDeleted)
	assert.Equal(t, "ephemeral.txt", events[0].Path)
	assert.Equal(t, "ephemeral.txt", path.Base(events[0].Path))
}

// Validates: R-6.7.3
func TestFullDelta_SkipsUnrecoverableDeletedItemAbsentFromBaseline(t *testing.T) {
	t.Parallel()

	// Unrelated live-drive activity can create and delete an item outside this
	// isolated store's baseline. If Graph omits both the deleted name and a
	// recoverable parent chain, there is no local path to mutate; the observer
	// should skip the delete without failing the whole delta batch.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{
						ID:        "deleted-untracked",
						ParentID:  "unknown-parent",
						DriveID:   driveid.New(synctest.TestDriveID),
						IsDeleted: true,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, token, err := obs.FullDelta(t.Context(), "saved-token")
	require.NoError(t, err, "FullDelta")

	assert.Empty(t, events)
	assert.Equal(t, "delta-link", token)
}

// Validates: R-6.7.28
func TestFullDelta_SkipsUntrackedDeletedItemWithKnownParentButEmptyName(t *testing.T) {
	t.Parallel()

	// A deleted item absent from baseline still needs its own name before the
	// observer can identify the child path. A known parent proves only where
	// the item used to live, not which child should be deleted.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "folder", Name: "Projects", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), IsFolder: true},
					{
						ID:        "deleted-child",
						ParentID:  "folder",
						DriveID:   driveid.New(synctest.TestDriveID),
						IsDeleted: true,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, token, err := obs.FullDelta(t.Context(), "saved-token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.False(t, events[0].IsDeleted)
	assert.Empty(t, events[0].OldPath)
	assert.Equal(t, "Projects", events[0].Path)
	assert.Equal(t, "delta-link", token)
}

func fullDeltaSingleItem(t *testing.T, item *graph.Item) []syncdomain.RemoteItem {
	t.Helper()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					*item,
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	return events
}

// Validates: R-6.7.28
func TestFullDelta_SkipsItemWithEmptyItemID(t *testing.T) {
	t.Parallel()

	events := fullDeltaSingleItem(t, &graph.Item{
		ID:       "",
		Name:     "bad.txt",
		ParentID: "root",
		DriveID:  driveid.New(synctest.TestDriveID),
	})

	assert.Empty(t, events, "items without ItemID should be skipped")
}

// Validates: R-6.7.28
func TestFullDelta_NonDeletedItem_MissingNameSkipped(t *testing.T) {
	t.Parallel()

	events := fullDeltaSingleItem(t, &graph.Item{
		ID:       "f1",
		Name:     "",
		ParentID: "root",
		DriveID:  driveid.New(synctest.TestDriveID),
	})

	assert.Empty(t, events, "non-deleted items without a name should be skipped")
}

// Validates: R-6.7.28
func TestFullDelta_DeletedItem_WithoutRecoverablePathSkipped(t *testing.T) {
	t.Parallel()

	events := fullDeltaSingleItem(t, &graph.Item{
		ID:        "gone",
		Name:      "",
		ParentID:  "",
		DriveID:   driveid.New(synctest.TestDriveID),
		IsDeleted: true,
	})

	assert.Empty(t, events, "deletes without recoverable path data should be skipped")
}

// Validates: R-2.4.3, R-2.4.8
func TestFullDeltaWithShortcutTopology_EmitsShortcutFactsAndSuppressesContent(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "shortcuts", Name: "Shortcuts", ParentID: "root", DriveID: driveID, IsFolder: true},
					{
						ID:             "binding-1",
						Name:           "Docs",
						ParentID:       "shortcuts",
						DriveID:        driveID,
						RemoteDriveID:  "remote-drive",
						RemoteItemID:   "remote-root",
						RemoteIsFolder: true,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:             fetcher,
		Baseline:            syncfixture.EmptyBaseline(),
		DriveID:             driveID,
		Logger:              synctest.TestLogger(t),
		ShortcutNamespaceID: "personal:owner@example.com",
		ProtectedRoots:      nil,
	})
	events, token, topology, err := obs.FullDeltaWithShortcutTopology(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, "delta-link", token)
	require.Len(t, events, 1)
	assert.Equal(t, "Shortcuts", events[0].Path)
	assert.Equal(t, "personal:owner@example.com", topology.NamespaceID)
	assert.Equal(t, syncshortcut.ShortcutTopologyObservationComplete, topology.Kind)
	require.Len(t, topology.Upserts, 1)
	assert.Equal(t, syncshortcut.ShortcutBindingUpsert{
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shortcuts/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     "remote-drive",
		RemoteItemID:      "remote-root",
		RemoteIsFolder:    true,
		Complete:          true,
	}, topology.Upserts[0])
}

// Validates: R-2.4.3, R-2.4.8
func TestFullDeltaWithShortcutTopology_EnrichesSparseKnownShortcutMove(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "container", Name: "Moved", ParentID: "root", DriveID: driveID, IsFolder: true},
					{
						ID:       "binding-1",
						ParentID: "container",
						DriveID:  driveID,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	itemClient := &syncfixture.TestMockItemClient{
		GetItemFn: func(_ context.Context, gotDriveID driveid.ID, itemID string) (*graph.Item, error) {
			assert.True(t, gotDriveID.Equal(driveID))
			assert.Equal(t, "binding-1", itemID)
			return &graph.Item{
				ID:             "binding-1",
				Name:           "Docs",
				ParentID:       "container",
				DriveID:        driveID,
				RemoteDriveID:  "remote-drive",
				RemoteItemID:   "remote-root",
				RemoteIsFolder: true,
			}, nil
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:             fetcher,
		Baseline:            syncfixture.EmptyBaseline(),
		DriveID:             driveID,
		Logger:              synctest.TestLogger(t),
		Items:               itemClient,
		ShortcutNamespaceID: "personal:owner@example.com",
		ProtectedRoots: []syncshortcut.ProtectedRoot{{
			Path:           "Docs",
			BindingID:      "binding-1",
			RemoteDriveID:  driveid.New("remote-drive"),
			RemoteItemID:   "remote-root",
			RemoteIsFolder: true,
		}},
	})
	events, token, topology, err := obs.FullDeltaWithShortcutTopology(t.Context(), "saved-token")
	require.NoError(t, err)
	assert.Equal(t, "delta-link", token)
	require.Len(t, events, 1)
	assert.Equal(t, "Moved", events[0].Path)
	require.Len(t, topology.Upserts, 1)
	assert.Equal(t, syncshortcut.ShortcutBindingUpsert{
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Moved/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     "remote-drive",
		RemoteItemID:      "remote-root",
		RemoteIsFolder:    true,
		Complete:          true,
	}, topology.Upserts[0])
	assert.Empty(t, topology.Unavailable)
}

// Validates: R-2.4.3, R-2.4.8
func TestWatch_TopologyApplyFailureDoesNotAdvanceCursor(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "shortcuts", Name: "Shortcuts", ParentID: "root", DriveID: driveID, IsFolder: true},
					{
						ID:             "binding-1",
						Name:           "Docs",
						ParentID:       "shortcuts",
						DriveID:        driveID,
						RemoteDriveID:  "remote-drive",
						RemoteItemID:   "remote-root",
						RemoteIsFolder: true,
					},
				},
				DeltaLink: "token-1",
			},
		}},
	}

	applyErr := errors.New("persist topology")
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:             fetcher,
		Baseline:            syncfixture.EmptyBaseline(),
		DriveID:             driveID,
		Logger:              synctest.TestLogger(t),
		ShortcutNamespaceID: "personal:owner@example.com",
		ProtectedRoots:      nil,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	batches := make(chan RemoteObservationBatch, 1)
	syncfixture.RunInBackground(t, ctx, func(consumerCtx context.Context) error {
		select {
		case batch := <-batches:
			assert.True(t, batch.ShortcutTopology.ShouldApply(), "expected applicable topology in watch batch")
			batch.FinishApplied(applyErr)
		case <-consumerCtx.Done():
		}
		return nil
	})

	err := syncfixture.RunWithin(t, ctx, "Watch to return the topology apply failure", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Millisecond, nil, testPrimaryWatchBatchHandler(driveID))
	})
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, obs.CurrentDeltaToken())
}

// Validates: R-2.4.3, R-2.4.8
func TestWatch_EmptyCompleteTopologyApplyFailureDoesNotAdvanceCursor(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
				},
				DeltaLink: "token-1",
			},
		}},
	}

	applyErr := errors.New("persist topology")
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:             fetcher,
		Baseline:            syncfixture.EmptyBaseline(),
		DriveID:             driveID,
		Logger:              synctest.TestLogger(t),
		ShortcutNamespaceID: "personal:owner@example.com",
		ProtectedRoots:      nil,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	batches := make(chan RemoteObservationBatch, 1)
	syncfixture.RunInBackground(t, ctx, func(consumerCtx context.Context) error {
		select {
		case batch := <-batches:
			assert.True(t, batch.ShortcutTopology.ShouldApply(), "expected empty complete topology batch to be emitted")
			batch.FinishApplied(applyErr)
		case <-consumerCtx.Done():
		}
		return nil
	})

	err := syncfixture.RunWithin(t, ctx, "Watch to return the topology apply failure", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Millisecond, nil, testPrimaryWatchBatchHandler(driveID))
	})
	require.ErrorIs(t, err, applyErr)
	assert.Empty(t, obs.CurrentDeltaToken())
}

// ---------------------------------------------------------------------------
// Watch tests
// ---------------------------------------------------------------------------

// noopSleep immediately returns nil. Tests use this to skip real delays.
func noopSleep(_ context.Context, _ time.Duration) error {
	return nil
}

// startTestRemoteBatchDrain plays the watch runtime for a remote observer's
// batches: it forwards each batch's changes, applies the batch with apply
// (nil applies nothing) and acknowledges it. It runs in the background until
// ctx ends (runInBackground), and its changes channel closes then.
func startTestRemoteBatchDrain(
	t *testing.T,
	ctx context.Context,
	batches <-chan RemoteObservationBatch,
	apply func(RemoteObservationBatch) error,
) <-chan RemoteChange {
	t.Helper()

	events := make(chan RemoteChange, 32)
	syncfixture.RunInBackground(t, ctx, func(drainCtx context.Context) error {
		defer close(events)

		for {
			select {
			case batch, ok := <-batches:
				if !ok {
					return nil
				}

				for i := range batch.Changes {
					select {
					case events <- batch.Changes[i]:
					case <-drainCtx.Done():
						return nil
					}
				}

				var err error
				if apply != nil {
					err = apply(batch)
				}
				batch.FinishApplied(err)
			case <-drainCtx.Done():
				return nil
			}
		}
	})

	return events
}

func testPrimaryWatchBatchHandler(driveID driveid.ID) remoteWatchBatchHandler {
	return func(
		_ context.Context,
		events []syncdomain.RemoteItem,
		newToken string,
		topology syncshortcut.ShortcutTopologyBatch,
	) (RemoteObservationBatch, error) {
		projected := ProjectRemoteObservations(nil, events)
		return RemoteObservationBatch{
			Source:           RemoteObservationBatchPrimaryWatch,
			ObservationMode:  syncdomain.RemoteObservationModeDelta,
			Observed:         projected.Observed,
			Changes:          projected.Changes,
			CursorToken:      newToken,
			ShortcutTopology: topology,
			ApplyAck:         make(chan error, 1),
		}, nil
	}
}

// sequentialFetcher returns a different DeltaPage for each call, allowing
// tests to script multi-poll watch scenarios.
type sequentialFetcher struct {
	pages []mockDeltaPage
	calls int
}

func (f *sequentialFetcher) Delta(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
	if f.calls >= len(f.pages) {
		return nil, errors.New("mock: no more pages configured")
	}

	p := f.pages[f.calls]
	f.calls++

	return p.page, p.err
}

type signalingFetcher struct {
	mu     sync.Mutex
	pages  []mockDeltaPage
	calls  int
	called chan int
}

func newSignalingFetcher(pages []mockDeltaPage) *signalingFetcher {
	return &signalingFetcher{
		pages:  pages,
		called: make(chan int, len(pages)+1),
	}
}

func (f *signalingFetcher) Delta(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.calls >= len(f.pages) {
		f.calls++
		select {
		case f.called <- f.calls:
		default:
		}

		return nil, errors.New("mock: no more pages configured")
	}

	p := f.pages[f.calls]
	f.calls++
	select {
	case f.called <- f.calls:
	default:
	}

	return p.page, p.err
}

func (f *signalingFetcher) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func tryWake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Validates: R-2.1.2
func TestWatch_PollsAtInterval(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
		pages: twoPageDeltaPages("delta", "token-1", "token-2"),
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: noopSleep}

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	events := startTestRemoteBatchDrain(t, ctx, batches, nil)
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Millisecond, nil, nil)
	})

	syncfixture.ReceiveWithin(t, events, "the first poll's event")
	syncfixture.ReceiveWithin(t, events, "the second poll's event")
	syncfixture.StopWatch(t, cancel, done)

	assert.GreaterOrEqual(t, fetcher.calls, 2)
	assert.NotEmpty(t, obs.CurrentDeltaToken(), "delta token should be non-empty after successful polls")
}

// Validates: R-2.8.5
func TestWatch_WakeSignalTriggersImmediatePoll(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := newSignalingFetcher([]mockDeltaPage{
		{page: &graph.DeltaPage{
			Items: []graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{ID: "f1", Name: "first.txt", ParentID: "root", DriveID: driveID, Size: 10},
			},
			DeltaLink: "token-1",
		}},
		{page: &graph.DeltaPage{
			Items: []graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{ID: "f2", Name: "second.txt", ParentID: "root", DriveID: driveID, Size: 20},
			},
			DeltaLink: "token-2",
		}},
	})

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	wakeCh := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	events := startTestRemoteBatchDrain(t, ctx, batches, nil)
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Hour, wakeCh, nil)
	})

	syncfixture.ReceiveWithin(t, events, "the first poll's event")
	tryWake(wakeCh)
	syncfixture.ReceiveWithin(t, events, "the event of the poll the wake started")
	syncfixture.StopWatch(t, cancel, done)
	assert.Equal(t, 2, fetcher.CallCount(), "wake should trigger a second delta cycle before fallback polling")
}

// Validates: R-2.8.5
//
// Wakes that pile up while a poll is pending collapse into that poll. Both
// wakes of the burst arrive while the first batch is being applied, which the
// watch waits for: the first ends the wait after that poll, and the second,
// still waiting when the next poll begins, is drained into it and reported.
// Nothing is then left to wake the watch, whose interval is an hour, so the
// second poll is the last.
func TestWatch_WakeSignalsCoalesce(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := newSignalingFetcher([]mockDeltaPage{
		{page: &graph.DeltaPage{
			Items: []graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{ID: "f1", Name: "first.txt", ParentID: "root", DriveID: driveID, Size: 10},
			},
			DeltaLink: "token-1",
		}},
		{page: &graph.DeltaPage{
			Items: []graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{ID: "f2", Name: "second.txt", ParentID: "root", DriveID: driveID, Size: 20},
			},
			DeltaLink: "token-2",
		}},
	})

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	recorder := syncfixture.NewDebugEventRecorder()
	obs.debugEvents = recorder.Record
	const burst = 2
	wakeCh := make(chan struct{}, burst)

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	applied := 0
	events := startTestRemoteBatchDrain(t, ctx, batches, func(RemoteObservationBatch) error {
		applied++
		if applied == 1 {
			for range burst {
				tryWake(wakeCh)
			}
		}
		return nil
	})
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Hour, wakeCh, testPrimaryWatchBatchHandler(driveID))
	})

	syncfixture.ReceiveWithin(t, events, "the first poll's event")
	recorder.WaitUntilSeen(t, func(event syncdomain.EngineDebugEvent) bool {
		return event.Type == syncdomain.EngineDebugEventRemoteWakesCoalesced && event.Count == burst-1
	}, "the second poll draining the wake left from the burst")
	syncfixture.ReceiveWithin(t, events, "the second poll's event")
	assert.Empty(t, wakeCh, "no wake is left to start a third poll")
	assert.Equal(t, 2, fetcher.CallCount(), "burst wakes should collapse into one pending delta cycle")
	syncfixture.StopWatch(t, cancel, done)
}

// fetcherFailingTwiceThenRecovering answers two polls with a transient failure
// and the third with one file.
func fetcherFailingTwiceThenRecovering() *sequentialFetcher {
	return &sequentialFetcher{
		pages: []mockDeltaPage{
			{err: errors.New("transient network error")},
			{err: errors.New("transient network error")},
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "f1", Name: "ok.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "recovered-token",
			}},
		},
	}
}

// Validates: R-6.8.2, R-6.8.29
func TestWatch_BackoffWithAWakeChannelWaitsOnTheRunClock(t *testing.T) {
	t.Parallel()

	fetcher := fetcherFailingTwiceThenRecovering()

	// The clock fires the two backoff waits at once and never fires a later
	// one, so the test does not wait the backoffs out. The delays it records
	// are the proof: a backoff armed on wall time records nothing here.
	var waits []time.Duration

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, afterFunc: func(d time.Duration, fn func()) syncdomain.SyncTimer {
		waits = append(waits, d)
		if len(waits) <= 2 {
			fn()
		}

		return noopSyncTimer{}
	}}

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	events := startTestRemoteBatchDrain(t, ctx, batches, nil)
	wakeCh := make(chan struct{}, 1)
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Minute, wakeCh, nil)
	})

	syncfixture.ReceiveWithin(t, events, "the event of the poll that succeeded")
	syncfixture.StopWatch(t, cancel, done)

	require.GreaterOrEqual(t, len(waits), 2, "each backoff is armed on the run's clock")
	assert.Equal(t, retry.WatchRemotePolicy().Base, waits[0])
	assert.Equal(t, time.Duration(float64(retry.WatchRemotePolicy().Base)*retry.WatchRemotePolicy().Multiplier), waits[1])
}

// Validates: R-6.8.2
func TestWatch_BackoffOnError(t *testing.T) {
	t.Parallel()

	fetcher := fetcherFailingTwiceThenRecovering()

	var sleepDurations []time.Duration

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(_ context.Context, d time.Duration) error {
		sleepDurations = append(sleepDurations, d)
		return nil
	}}

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	events := startTestRemoteBatchDrain(t, ctx, batches, nil)
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Minute, nil, nil)
	})

	syncfixture.ReceiveWithin(t, events, "the event of the poll that succeeded")
	syncfixture.StopWatch(t, cancel, done)

	// First sleep should be 5s (initial backoff), second should be 10s (doubled).
	require.GreaterOrEqual(t, len(sleepDurations), 2, "sleep should be called for backoff")
	assert.Equal(t, retry.WatchRemotePolicy().Base, sleepDurations[0])
	assert.Equal(t, time.Duration(float64(retry.WatchRemotePolicy().Base)*retry.WatchRemotePolicy().Multiplier), sleepDurations[1])
}

// Validates: R-2.1.2, R-2.8.5
func TestWatch_ZeroEvents_NoTokenAdvanceAfterWake(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := newSignalingFetcher([]mockDeltaPage{
		{page: &graph.DeltaPage{
			Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
			DeltaLink: "new-token-1",
		}},
		{page: &graph.DeltaPage{
			Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
			DeltaLink: "new-token-2",
		}},
	})

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	store := syncstoretest.NewTestStore(t)
	wakeCh := make(chan struct{}, 1)

	ctx, cancel := context.WithTimeout(t.Context(), syncfixture.EventWait)
	defer cancel()

	batches := make(chan RemoteObservationBatch, 1)
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "old-token", batches, time.Hour, wakeCh, testPrimaryWatchBatchHandler(driveID))
	})
	_ = startTestRemoteBatchDrain(t, ctx, batches, func(batch RemoteObservationBatch) error {
		return store.CommitObservation(context.WithoutCancel(ctx), batch.Observed, batch.CursorToken, driveID)
	})

	require.Eventually(t, func() bool {
		return fetcher.CallCount() >= 1
	}, syncfixture.EventWait, 10*time.Millisecond)
	tryWake(wakeCh)
	require.Eventually(t, func() bool {
		return fetcher.CallCount() >= 2
	}, syncfixture.EventWait, 10*time.Millisecond)
	syncfixture.StopWatch(t, cancel, done)

	assert.Empty(t, syncfixture.ReadObservationCursor(t, syncstoretest.RawDB(t, store), driveID.String()), "zero-event wake polls must not commit observations")
	assert.Equal(t, "old-token", obs.CurrentDeltaToken(), "zero-event wake polls must not advance the token")
}

// Validates: R-2.15.3, R-6.7.5
func TestWatch_DeltaExpiredResets(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
		pages: []mockDeltaPage{
			// First call: delta expired.
			{err: graph.ErrGone},
			// Second call (after reset): success with full data.
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "f1", Name: "resync.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "new-full-token",
			}},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: noopSleep}

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	events := startTestRemoteBatchDrain(t, ctx, batches, nil)
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "old-expired-token", batches, time.Millisecond, nil, nil)
	})

	syncfixture.ReceiveWithin(t, events, "the resync event")
	syncfixture.StopWatch(t, cancel, done)

	// After delta expired, token should have been reset to "" for resync,
	// then updated to the new token from the successful full resync.
	assert.Equal(t, "new-full-token", obs.CurrentDeltaToken())
}

// Validates: R-2.8.3, R-6.10.10
func TestWatch_ContextCancellation(t *testing.T) {
	t.Parallel()

	// Fetcher that always succeeds with empty delta (no events to send).
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{DeltaLink: "token"},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	pollStarted := make(chan struct{})
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(ctx context.Context, _ time.Duration) error {
		select {
		case <-pollStarted:
		default:
			close(pollStarted)
		}
		// Block until canceled.
		<-ctx.Done()
		return ctx.Err()
	}}

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	_ = startTestRemoteBatchDrain(t, ctx, batches, nil)
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Hour, nil, nil)
	})

	syncfixture.ReceiveWithin(t, pollStarted, "the remote watch to sleep between polls")
	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.1.2
func TestWatch_CurrentDeltaToken(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)

	// Pages must include a non-root item so events > 0 (the zero-event
	// guard skips token advancement when events are empty).
	fetcher := &sequentialFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file.txt", ParentID: "root", DriveID: driveID, Size: 10},
				},
				DeltaLink: "token-after-poll-1",
			}},
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file.txt", ParentID: "root", DriveID: driveID, Size: 10},
				},
				DeltaLink: "token-after-poll-2",
			}},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: noopSleep}

	// Before Watch starts, token is empty.
	assert.Empty(t, obs.CurrentDeltaToken(), "initial token")

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	_ = startTestRemoteBatchDrain(t, ctx, batches, nil)

	pollCount := 0
	origSleep := obs.clock.Sleep
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(ctx context.Context, d time.Duration) error {
		pollCount++
		if pollCount >= 2 {
			cancel()
			return ctx.Err()
		}
		return origSleep(ctx, d)
	}}

	err := syncfixture.RunWithin(t, ctx, "the second poll's sleep to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "initial-token", batches, time.Millisecond, nil, nil)
	})
	require.NoError(t, err, "Watch returned error")

	// Token should have been updated by successful polls.
	token := obs.CurrentDeltaToken()
	assert.True(t, token == "token-after-poll-1" || token == "token-after-poll-2",
		"token = %q, want one of the poll tokens", token)
}

// TestWatch_IntervalClamping verifies that zero/negative poll intervals are
// clamped to the minimum (30s) instead of causing a tight polling loop.
func TestWatch_IntervalClamping(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{DeltaLink: "token-1"}},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})

	// Track what interval the sleep function actually receives.
	var actualSleepDuration time.Duration
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(_ context.Context, d time.Duration) error {
		actualSleepDuration = d
		return errors.New("stop after first sleep")
	}}

	batches := make(chan RemoteObservationBatch, 10)
	// Pass zero interval — should be clamped to MinPollInterval (30s).
	err := syncfixture.RunWithin(t, t.Context(), "the first sleep's error to end the watch", func(ctx context.Context) error {
		return obs.Watch(ctx, "", batches, 0, nil, nil)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stop after first sleep")

	assert.Equal(t, syncdomain.MinPollInterval, actualSleepDuration, "clamped from 0")
}

// Validates: R-6.7.24
func TestFullDelta_RenameInPlace(t *testing.T) {
	t.Parallel()

	// File renamed within the same parent folder (same parent, new name).
	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "docs", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder1",
			ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
		},
		&syncdomain.BaselineEntry{
			Path: "docs/old-name.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "folder1", ItemType: syncdomain.ItemTypeFile,
		},
	)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					// Same parent (folder1), different name.
					{ID: "f1", Name: "new-name.txt", ParentID: "folder1", DriveID: driveid.New(synctest.TestDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var renameEvent *syncdomain.RemoteItem
	for i := range events {
		if events[i].ItemID == "f1" {
			renameEvent = &events[i]

			break
		}
	}

	require.NotNil(t, renameEvent, "rename event not found")
	assert.NotEmpty(t, renameEvent.OldPath)
	assert.Equal(t, "docs/new-name.txt", renameEvent.Path)
	assert.Equal(t, "docs/old-name.txt", renameEvent.OldPath)
	assert.Equal(t, "new-name.txt", path.Base(renameEvent.Path))
}

// Validates: R-2.2.1
// TestSelectHash_FallbackChain verifies the hash priority: QuickXorHash >
// SHA256Hash > SHA1Hash > empty (B-021).
func TestSelectHash_FallbackChain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item graph.Item
		want string
	}{
		{
			name: "QuickXorHash preferred",
			item: graph.Item{QuickXorHash: "qxor", SHA256Hash: "sha256", SHA1Hash: "sha1"},
			want: "qxor",
		},
		{
			name: "SHA256Hash is not comparable",
			item: graph.Item{SHA256Hash: "sha256", SHA1Hash: "sha1"},
			want: "",
		},
		{
			name: "SHA1Hash is not comparable",
			item: graph.Item{SHA1Hash: "sha1"},
			want: "",
		},
		{
			name: "no hash returns empty",
			item: graph.Item{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := driveops.SelectHash(&tt.item)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRemoteObserver_Stats verifies that Stats() returns non-zero counters
// after processing delta events (B-127).
func TestRemoteObserver_Stats(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "f1", Name: "doc.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), Size: 10},
					{ID: "f2", Name: "img.png", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), Size: 20},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})

	_, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	stats := obs.Stats()

	assert.NotZero(t, stats.EventsEmitted, "EventsEmitted should be > 0 after processing items")
	assert.NotZero(t, stats.PollsCompleted, "PollsCompleted should be > 0 after a successful FullDelta")
}

// Validates: R-6.7.21
// TestFullDelta_CrossDriveItems verifies path resolution for items that
// reference a different drive than the observer's primary drive, e.g., shared
// items where the item's DriveID differs from the observer's driveID (B-007).
func TestFullDelta_CrossDriveItems(t *testing.T) {
	t.Parallel()

	primaryDrive := driveid.New("0000000000000001")
	sharedDrive := driveid.New("0000000000000099")

	// Baseline has the root of the primary drive.
	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "shared-folder", DriveID: primaryDrive, ItemID: "sf1",
			ParentID: "root", ItemType: syncdomain.ItemTypeFolder,
		},
	)

	// Delta response includes an item from sharedDrive appearing under
	// the primary drive's folder. The item's DriveID differs from the
	// observer's driveID, and its ParentDriveID points back to the primary.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: primaryDrive},
					// Cross-drive item: lives on sharedDrive but parent is on primaryDrive.
					{
						ID: "shared-f1", Name: "shared-doc.txt",
						DriveID:       sharedDrive,
						ParentID:      "sf1",
						ParentDriveID: primaryDrive,
						Size:          512,
						QuickXorHash:  "xhash-shared",
					},
				},
				DeltaLink: "delta-cross-drive",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: baseline,
		DriveID:  primaryDrive,
		Logger:   synctest.TestLogger(t),
	})

	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	ev := events[0]

	// The item should use sharedDrive as its DriveID.
	assert.True(t, ev.DriveID.Equal(sharedDrive), "DriveID = %v, want %v (shared drive)", ev.DriveID, sharedDrive)

	// Path should be resolved through the primary drive's baseline.
	assert.Equal(t, "shared-folder/shared-doc.txt", ev.Path)

	// Since the item is new (not in baseline), it should be a Create.
	assert.False(t, ev.IsDeleted)
	assert.Empty(t, ev.OldPath)
}

// Validates: R-2.4.7
// TestFullDelta_PersonalVaultExcluded verifies that items with
// specialFolder.name == "vault" are excluded from delta processing by default,
// preventing data-loss from vault lock/unlock transitions (B-271).
func TestFullDelta_PersonalVaultExcluded(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					// Vault folder.
					{
						ID: "vault-folder", Name: "Personal Vault",
						ParentID: "root", DriveID: driveid.New(synctest.TestDriveID),
						IsFolder: true, SpecialFolderName: "vault",
					},
					// File inside vault.
					{
						ID: "vault-file", Name: "secret.pdf",
						ParentID: "vault-folder", DriveID: driveid.New(synctest.TestDriveID),
						Size: 1024, QuickXorHash: "vhash",
					},
					// Normal file outside vault.
					{
						ID: "normal-file", Name: "readme.txt",
						ParentID: "root", DriveID: driveid.New(synctest.TestDriveID),
						Size: 256, QuickXorHash: "nhash",
					},
				},
				DeltaLink: "delta-vault",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})

	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	// Only the normal file should produce an event — vault items are excluded.
	require.Len(t, events, 1, "vault excluded")
	assert.Equal(t, "readme.txt", path.Base(events[0].Path))
}

// TestFullDelta_VaultChildBeforeParent verifies that vault descendants are
// correctly skipped even when the Graph API returns the child BEFORE the
// vault parent in the same delta page. Without two-pass processing, the
// child's parent is not yet in the inflight map when isDescendantOfVault
// runs, causing it to be emitted as a normal file — data loss when the
// vault re-locks (B-281).
// Validates: R-2.4.7, R-6.7.1
func TestFullDelta_VaultChildBeforeParent(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					// Child appears BEFORE its vault parent — triggers B-281.
					{
						ID: "vault-file", Name: "secret.pdf",
						ParentID: "vault-folder", DriveID: driveid.New(synctest.TestDriveID),
						Size: 1024, QuickXorHash: "vhash",
					},
					// Vault folder appears after its child.
					{
						ID: "vault-folder", Name: "Personal Vault",
						ParentID: "root", DriveID: driveid.New(synctest.TestDriveID),
						IsFolder: true, SpecialFolderName: "vault",
					},
					// Normal file outside vault.
					{
						ID: "normal-file", Name: "readme.txt",
						ParentID: "root", DriveID: driveid.New(synctest.TestDriveID),
						Size: 256, QuickXorHash: "nhash",
					},
				},
				DeltaLink: "delta-vault-reorder",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})

	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	// Only the normal file should produce an event — vault child must be
	// skipped regardless of ordering within the delta page.
	require.Len(t, events, 1, "vault child skipped despite ordering")
	assert.Equal(t, "readme.txt", path.Base(events[0].Path))
}

// TestObserverStats_HashesComputed verifies that the HashesComputed counter
// increments for each item that has a non-empty content hash (B-282).
func TestObserverStats_HashesComputed(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{
						ID: "f1", Name: "hashed.txt", ParentID: "root",
						DriveID: driveid.New(synctest.TestDriveID), QuickXorHash: "abc123",
					},
					{
						ID: "f2", Name: "no-hash.txt", ParentID: "root",
						DriveID: driveid.New(synctest.TestDriveID),
						// No hash fields set.
					},
					{
						ID: "f3", Name: "sha256.txt", ParentID: "root",
						DriveID: driveid.New(synctest.TestDriveID), SHA256Hash: "deadbeef",
					},
				},
				DeltaLink: "delta-stats",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
	})

	_, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	stats := obs.Stats()
	assert.Equal(t, int64(1), stats.HashesComputed, "items with a comparable (QuickXor) hash")
}

// ---------------------------------------------------------------------------
// Remote observer trusts server — no name filtering
// ---------------------------------------------------------------------------

// Validates: R-2.4.2
// TestClassifyItem_RemoteDoesNotApplyProductJunkFiltering validates that raw
// remote observation keeps manageable provider rows; sync visibility filtering
// happens later at the planner view boundary.
func TestClassifyItem_RemoteDoesNotApplyProductJunkFiltering(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	bl := syncfixture.EmptyBaseline()
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  nil,
		Baseline: bl,
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})

	inflight := map[string]inflightParent{
		"root": {Name: "", IsRoot: true},
	}

	tests := []struct {
		name      string
		itemName  string
		isDeleted bool
		wantEvent bool
	}{
		// Product junk policy is not applied inside raw remote observation.
		{".tmp file", "data.tmp", false, true},
		{".partial file", "download.partial", false, true},
		{"tilde backup", "~backup.txt", false, true},
		{"dot-tilde lock", ".~lock.file", false, true},
		{".swp file", "file.swp", false, true},
		{".crdownload", "file.crdownload", false, true},
		{"reserved name CON", "CON", false, true},
		{"trailing dot", "file.", false, true},
		{"trailing space", "file ", false, true},
		{"tilde-dollar", "~$document.docx", false, true},

		// Valid names produce events as before.
		{"normal file", "hello.txt", false, true},
		{"db file", "data.db", false, true},
		{"pdf file", "report.pdf", false, true},

		// Deleted junk items are still raw remote facts.
		{"deleted .tmp", "data.tmp", true, true},
		{"deleted tilde", "~backup.txt", true, true},
		{"deleted .partial", "download.partial", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			item := &graph.Item{
				ID:        "item-" + tt.itemName,
				Name:      tt.itemName,
				ParentID:  "root",
				DriveID:   driveID,
				IsDeleted: tt.isDeleted,
			}

			ev := obs.Converter.ClassifyItem(item, inflight)
			if tt.wantEvent {
				assert.NotNil(t, ev, "remote observer must produce event for %q", tt.itemName)
				return
			}

			assert.Nil(t, ev, "remote observer unexpectedly produced event for %q", tt.itemName)
		})
	}
}

// Validates: R-2.1.2, R-2.4.7
func TestClassifyItem_SkipsPackageItemsForSync(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  nil,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	inflight := map[string]inflightParent{
		"root": {Name: "", IsRoot: true},
	}

	ev := obs.Converter.ClassifyItem(&graph.Item{
		ID:        "notebook-1",
		Name:      "Notebook",
		ParentID:  "root",
		DriveID:   driveID,
		IsPackage: true,
	}, inflight)

	assert.Nil(t, ev)
}

// Validates: R-2.1.2, R-2.4.2
// TestWatch_CommitsObservations verifies that Watch commits observations
// after each successful FullDelta poll.
func TestWatch_CommitsObservations(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)

	fetcher := &sequentialFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "a.txt", ParentID: "root", DriveID: driveID, Size: 10, QuickXorHash: "hash1"},
				},
				DeltaLink: "token-1",
			}},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	store := syncstoretest.NewTestStore(t)
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: noopSleep}

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 10)
	committed := make(chan struct{})
	var committedOnce sync.Once
	_ = startTestRemoteBatchDrain(t, ctx, batches, func(batch RemoteObservationBatch) error {
		err := store.CommitObservation(context.WithoutCancel(ctx), batch.Observed, batch.CursorToken, driveID)
		committedOnce.Do(func() { close(committed) })
		return err
	})

	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Millisecond, nil, testPrimaryWatchBatchHandler(driveID))
	})

	syncfixture.ReceiveWithin(t, committed, "the watch's first batch to be committed")
	syncfixture.StopWatch(t, cancel, done)

	assert.Equal(t, "token-1", syncfixture.ReadObservationCursor(t, syncstoretest.RawDB(t, store), driveID.String()))
	row := syncstoretest.ReadRemoteStateRow(t, store, "f1")
	require.NotNil(t, row, "remote observation should persist mirrored state")
	assert.Equal(t, driveID, row.DriveID)
	assert.Equal(t, "a.txt", row.Path)
}

// TestWatch_BatchHandlerError_ReturnsError verifies that watch-owned batch
// policy errors stop the watch loop instead of being retried inside the
// observer.
func TestWatch_BatchHandlerError_ReturnsError(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)

	// Pages must include a non-root item so events > 0 (the zero-event
	// guard skips the batch handler entirely when events are empty).
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file.txt", ParentID: "root", DriveID: driveID, Size: 10},
				},
				DeltaLink: "token-1",
			}},
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "file.txt", ParentID: "root", DriveID: driveID, Size: 10},
				},
				DeltaLink: "token-2",
			}},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	batches := make(chan RemoteObservationBatch, 10)
	_ = startTestRemoteBatchDrain(t, ctx, batches, nil)
	err := syncfixture.RunWithin(t, ctx, "the batch policy failure to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "", batches, time.Millisecond, nil, func(
			context.Context,
			[]syncdomain.RemoteItem,
			string,
			syncshortcut.ShortcutTopologyBatch,
		) (RemoteObservationBatch, error) {
			return RemoteObservationBatch{}, filepath.ErrBadPattern
		})
	})
	require.Error(t, err)
	require.ErrorIs(t, err, filepath.ErrBadPattern)
	assert.Equal(t, 1, fetcher.calls, "watch should stop on batch policy failure")
}

// Validates: R-2.1.2
func TestWatch_ZeroEvents_NoTokenAdvance(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	pollCount := 0

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			// First poll: 0 events (only root, which is skipped).
			{page: &graph.DeltaPage{
				Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
				DeltaLink: "new-token-should-not-be-saved",
			}},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	polledAt := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	obs.clock = &observerTestClock{SyncClock: syncfixture.NewManualClock(polledAt), sleep: func(ctx context.Context, _ time.Duration) error {
		pollCount++
		if pollCount >= 1 {
			cancel()
			return ctx.Err()
		}

		return nil
	}}

	// Nothing drains the channel: a batch the watch handed over would still
	// be in it after the watch returns.
	batches := make(chan RemoteObservationBatch, 10)
	err := syncfixture.RunWithin(t, ctx, "the first poll's sleep to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "old-token", batches, time.Millisecond, nil, testPrimaryWatchBatchHandler(driveID))
	})
	require.NoError(t, err)

	require.Len(t, batches, 1, "a poll that returned no events hands over only the time it began")
	handed := <-batches
	assert.True(t, handed.PollOnly)
	assert.Equal(t, polledAt, handed.PollStartedAt)
	assert.Empty(t, handed.Observed, "nothing to commit")
	assert.Empty(t, handed.Changes, "nothing to replan")
	assert.Empty(t, handed.CursorToken, "no token to save")
	assert.Equal(t, "old-token", obs.CurrentDeltaToken(), "token should not advance when 0 events returned")
}

// Validates: R-5.6.15, R-2.1.2
//
// The watch loop drops the create intent of a file that is gone once a poll
// that began after the attempt ended has been applied, so every batch a poll
// hands over says when it began, on the observer's clock, before its first
// page was asked for: a poll with items as well as one that returned none.
func TestWatch_EveryPollHandsOverTheTimeItBegan(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &sequentialFetcher{pages: []mockDeltaPage{
		{page: &graph.DeltaPage{
			Items: []graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{ID: "f1", Name: "a.txt", ParentID: "root", DriveID: driveID, Size: 10, QuickXorHash: "hash1"},
			},
			DeltaLink: "token-1",
		}},
		{page: &graph.DeltaPage{Items: []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}}, DeltaLink: "token-2"}},
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	firstPoll := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	clock := syncfixture.NewManualClock(firstPoll)
	sleeps := 0
	obs.clock = &observerTestClock{SyncClock: clock, sleep: func(ctx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps > 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		clock.Advance(time.Minute)
		return nil
	}}

	ctx, cancel := context.WithCancel(t.Context())
	batches := make(chan RemoteObservationBatch, 2)
	handed := make(chan RemoteObservationBatch, 2)
	_ = startTestRemoteBatchDrain(t, ctx, batches, func(batch RemoteObservationBatch) error {
		handed <- batch
		return nil
	})
	_, done := syncfixture.RunInBackground(t, ctx, func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "old-token", batches, time.Millisecond, nil, testPrimaryWatchBatchHandler(driveID))
	})

	withItems := syncfixture.ReceiveWithin(t, handed, "the first poll's batch")
	zeroEvent := syncfixture.ReceiveWithin(t, handed, "the second poll's batch")
	syncfixture.StopWatch(t, cancel, done)

	assert.Equal(t, firstPoll, withItems.PollStartedAt)
	assert.False(t, withItems.PollOnly)
	assert.NotEmpty(t, withItems.Observed)
	assert.Equal(t, firstPoll.Add(time.Minute), zeroEvent.PollStartedAt)
	assert.True(t, zeroEvent.PollOnly)
	assert.Equal(t, "token-1", obs.CurrentDeltaToken(), "the poll that returned nothing keeps the token")
}

// Regression: a delta window whose only items are deletes of never-synced
// items converts to zero events. Treating that like an empty window kept the
// old token, so every poll replayed the same deletes forever (and warned about
// them each time). The window did carry items, so the token must advance.
//
// Validates: R-2.1.2
func TestWatch_OnlyUnrecoverableDeletes_AdvancesToken(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	pollCount := 0

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "gone-1", IsDeleted: true, DriveID: driveID},
					{ID: "gone-2", IsDeleted: true, DriveID: driveID},
				},
				DeltaLink: "token-after-deletes",
			}},
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	store := syncstoretest.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(ctx context.Context, _ time.Duration) error {
		pollCount++
		if pollCount >= 1 {
			cancel()
			return ctx.Err()
		}

		return nil
	}}

	batches := make(chan RemoteObservationBatch, 10)
	_ = startTestRemoteBatchDrain(t, ctx, batches, func(batch RemoteObservationBatch) error {
		return store.CommitObservation(context.WithoutCancel(ctx), batch.Observed, batch.CursorToken, driveID)
	})
	err := syncfixture.RunWithin(t, ctx, "the first poll's sleep to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "old-token", batches, time.Millisecond, nil, testPrimaryWatchBatchHandler(driveID))
	})
	require.NoError(t, err)

	assert.Equal(t, "token-after-deletes", obs.CurrentDeltaToken(),
		"a window that returned items must advance the token even when none converted to events")
	assert.Equal(t, "token-after-deletes", syncfixture.ReadObservationCursor(t, syncstoretest.RawDB(t, store), driveID.String()),
		"the advanced token must be committed so a restart does not replay the window")
}

// Regression: an incremental window can carry a change inside a folder that
// is neither in the window nor in the baseline (a suppressed OneNote package,
// or a folder whose own download has not succeeded yet). That orphan used to
// mark the window incomplete on every poll, so the cursor never advanced and
// remote sync stalled forever. The observer now reads the missing ancestor by
// ID, places the item, and advances.
//
// Validates: R-2.1.2
func TestWatch_OrphanWithReadableParent_AdvancesToken(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "notes.txt", ParentID: "unsynced-folder", DriveID: driveID},
				},
				DeltaLink: "token-after-orphan",
			}},
		},
	}

	var fetchedIDs []string
	itemClient := &syncfixture.TestMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			fetchedIDs = append(fetchedIDs, itemID)
			require.Equal(t, "unsynced-folder", itemID)

			return &graph.Item{ID: "unsynced-folder", Name: "Docs", ParentID: "root", DriveID: driveID, IsFolder: true}, nil
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
		Items:    itemClient,
	})
	token, emitted := runSingleWatchPoll(t, obs, driveID)

	assert.Equal(t, []string{"unsynced-folder"}, fetchedIDs, "only the missing parent is read")
	assert.Equal(t, "token-after-orphan", token, "a resolved orphan must not hold the cursor")
	require.Len(t, emitted, 1, "the fetched parent is path context, not an event")
	assert.Equal(t, "Docs/notes.txt", emitted[0].Path)
}

// Safety: resolving a missing parent must not smuggle Personal Vault content
// into sync. A change inside the vault arrives without the (suppressed) vault
// folder; once the fetched ancestor shows it is the vault, the child stays
// excluded exactly as if the vault had been in the window (R-2.4.7).
//
// Validates: R-2.4.7, R-2.1.2
func TestWatch_OrphanUnderFetchedVaultStaysExcluded(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "secret", Name: "passport.pdf", ParentID: "vault-folder", DriveID: driveID},
				},
				DeltaLink: "token-after-vault",
			}},
		},
	}

	itemClient := &syncfixture.TestMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			require.Equal(t, "vault-folder", itemID)

			return &graph.Item{
				ID: "vault-folder", Name: "Personal Vault", ParentID: "root", DriveID: driveID,
				IsFolder: true, SpecialFolderName: specialFolderVault,
			}, nil
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
		Items:    itemClient,
	})
	token, emitted := runSingleWatchPoll(t, obs, driveID)

	assert.Equal(t, "token-after-vault", token, "the window still advances")
	assert.Empty(t, emitted, "vault content must never become a sync event")
}

// Safety: a change inside a OneNote notebook (a package, which sync skips)
// must stay skipped once its parent package is fetched to place it.
//
// Validates: R-2.1.2
func TestWatch_OrphanUnderFetchedPackageStaysExcluded(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "section", Name: "Meetings.one", ParentID: "notebook", DriveID: driveID},
				},
				DeltaLink: "token-after-package",
			}},
		},
	}

	itemClient := &syncfixture.TestMockItemClient{
		GetItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			require.Equal(t, "notebook", itemID)

			return &graph.Item{ID: "notebook", Name: "Work Notebook", ParentID: "root", DriveID: driveID, IsPackage: true}, nil
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
		Items:    itemClient,
	})
	token, emitted := runSingleWatchPoll(t, obs, driveID)

	assert.Equal(t, "token-after-package", token)
	assert.Empty(t, emitted, "a notebook's sections must not sync as ordinary files")
}

// Regression: when Graph says the orphan's parent no longer exists (or is not
// readable), no replay can ever place the item. It is skipped and the window
// advances instead of stalling every later poll.
//
// Validates: R-2.1.2
func TestWatch_OrphanWithUnreachableParent_AdvancesToken(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "f1", Name: "notes.txt", ParentID: "gone-folder", DriveID: driveID},
					{ID: "f2", Name: "kept.txt", ParentID: "root", DriveID: driveID},
				},
				DeltaLink: "token-after-orphan",
			}},
		},
	}

	itemClient := &syncfixture.TestMockItemClient{
		GetItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
			return nil, graph.ErrNotFound
		},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
		Items:    itemClient,
	})
	token, emitted := runSingleWatchPoll(t, obs, driveID)

	assert.Equal(t, "token-after-orphan", token, "an unplaceable orphan must not hold the cursor")
	require.Len(t, emitted, 1, "the rest of the window still syncs")
	assert.Equal(t, "kept.txt", emitted[0].Path)
}

// A transient failure reading the missing parent keeps today's safe
// behavior: the window is incomplete and is retried, never committed with
// the orphan's create silently dropped.
//
// Validates: R-2.1.2
func TestFullDelta_OrphanParentReadTransientFailure_StaysIncomplete(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "notes.txt", ParentID: "unsynced-folder", DriveID: driveID},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
		Items: &syncfixture.TestMockItemClient{
			GetItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
				return nil, errors.New("connection reset")
			},
		},
	})
	events, token, err := obs.FullDelta(t.Context(), "saved-token")
	require.ErrorIs(t, err, syncdomain.ErrRemoteObservationIncomplete)
	assert.Empty(t, events)
	assert.Empty(t, token)
}

// runSingleWatchPoll runs one Watch poll against a fresh store, commits the
// batch it produces, and returns the committed cursor and emitted events.
func runSingleWatchPoll(t *testing.T, obs *RemoteObserver, driveID driveid.ID) (string, []RemoteChange) {
	t.Helper()

	store := syncstoretest.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}

	var emitted []RemoteChange
	batches := make(chan RemoteObservationBatch, 10)
	_ = startTestRemoteBatchDrain(t, ctx, batches, func(batch RemoteObservationBatch) error {
		emitted = append(emitted, batch.Changes...)
		return store.CommitObservation(context.WithoutCancel(ctx), batch.Observed, batch.CursorToken, driveID)
	})
	err := syncfixture.RunWithin(t, ctx, "the poll's sleep to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "old-token", batches, time.Millisecond, nil, testPrimaryWatchBatchHandler(driveID))
	})
	require.NoError(t, err)

	assert.Equal(t, obs.CurrentDeltaToken(), syncfixture.ReadObservationCursor(t, syncstoretest.RawDB(t, store), driveID.String()),
		"the in-memory cursor must match the committed one")

	return obs.CurrentDeltaToken(), emitted
}

// ---------------------------------------------------------------------------
// Embedded shared-folder link handling
// ---------------------------------------------------------------------------

// Validates: R-2.4.3
func TestClassifyItem_RegularFileNoRemoteDriveID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	bl := syncfixture.EmptyBaseline()
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  nil,
		Baseline: bl,
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})

	inflight := map[string]inflightParent{
		"root": {Name: "", IsRoot: true},
	}

	item := &graph.Item{
		ID:           "regular-file-1",
		Name:         "document.docx",
		ParentID:     "root",
		DriveID:      driveID,
		IsFolder:     false,
		QuickXorHash: "hash123",
	}

	ev := obs.Converter.ClassifyItem(item, inflight)
	require.NotNil(t, ev, "regular file should produce an event")

	assert.False(t, ev.IsDeleted, "regular file without RemoteDriveID should be ChangeCreate")
	assert.Empty(t, ev.OldPath, "regular file without RemoteDriveID should be ChangeCreate")
}

// Validates: R-2.4.3
func TestClassifyItem_EmbeddedSharedPlaceholdersIgnored(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  nil,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})

	inflight := map[string]inflightParent{
		"root": {Name: "", IsRoot: true},
	}

	tests := []struct {
		name string
		item graph.Item
	}{
		{
			name: "top level folder-shaped shared link",
			item: graph.Item{
				ID:            "shared-link-1",
				Name:          "TeamDocs",
				ParentID:      "root",
				DriveID:       driveID,
				IsFolder:      true,
				IsDeleted:     true,
				RemoteDriveID: "source-drive-abc",
				RemoteItemID:  "source-item-123",
			},
		},
		{
			name: "remote folder shortcut placeholder",
			item: graph.Item{
				ID:             "shortcut-placeholder-1",
				Name:           "TeamDocs",
				ParentID:       "root",
				DriveID:        driveID,
				IsFolder:       false,
				RemoteIsFolder: true,
				RemoteDriveID:  "source-drive-abc",
				RemoteItemID:   "source-item-123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := tt.item
			ev := obs.Converter.ClassifyItem(&item, inflight)
			assert.Nil(t, ev, "embedded shared placeholders should be ignored inside the content engine")
		})
	}
}

// Validates: R-2.4.3
func TestFullDelta_EmbeddedSharedLinksIgnored(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{
						ID: "f1", Name: "myfile.txt", ParentID: "root", DriveID: driveID,
						QuickXorHash: "hash1", Size: 100,
					},
					{
						ID: "sc-1", Name: "SharedFolder", ParentID: "root", DriveID: driveID,
						IsFolder: true, RemoteDriveID: "remote-drive-1", RemoteItemID: "remote-item-1",
					},
				},
				DeltaLink: "https://graph.microsoft.com/delta?token=tok",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, "f1", events[0].ItemID)
	assert.Equal(t, "myfile.txt", events[0].Path)
}

// Validates: R-2.11.9
func TestFullDelta_RejectsUnmaterializableRemoteNames(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "dotdot", Name: "..", ParentID: "root", DriveID: driveID, IsFolder: true},
					{ID: "escaped-child", Name: "secret.txt", ParentID: "dotdot", DriveID: driveID},
					{ID: "dot", Name: ".", ParentID: "root", DriveID: driveID},
					{ID: "slash", Name: "x/y.txt", ParentID: "root", DriveID: driveID},
					{ID: "nul", Name: "a\x00b.txt", ParentID: "root", DriveID: driveID},
					{ID: "badutf8", Name: "caf\xe9.txt", ParentID: "root", DriveID: driveID},
					{ID: "ok", Name: "a%2Fb.txt", ParentID: "root", DriveID: driveID},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	events, token, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "an unmaterializable name must not stall the window")
	assert.Equal(t, "delta-link", token)

	require.Len(t, events, 1, "only the literal percent name is materializable")
	assert.Equal(t, "ok", events[0].ItemID)
	assert.Equal(t, "a%2Fb.txt", events[0].Path)
}

// Validates: R-2.11.9
func TestFullDelta_ItemWithoutParentReferenceIsNotPlacedAtRoot(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "parentless", Name: "stray.txt", DriveID: driveID},
					{ID: "placed", Name: "kept.txt", ParentID: "root", DriveID: driveID},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, "kept.txt", events[0].Path)
}

// Validates: R-6.7.3
func TestFullDelta_ParentIDChainWinsOverParentReferencePath(t *testing.T) {
	t.Parallel()

	// parentReference.path is URL-encoded, so a folder literally named
	// "a%20b" decodes to "a b" there. The ID chain names it exactly.
	driveID := driveid.New(synctest.TestDriveID)
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveID},
					{ID: "d1", Name: "a%20b", ParentID: "root", DriveID: driveID, IsFolder: true},
					{
						ID: sharedTestFileItemID, Name: "c.txt", ParentID: "d1", DriveID: driveID,
						ParentPath: "a b", ParentPathKnown: true,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveID,
		Logger:   synctest.TestLogger(t),
	})
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err)

	var child *syncdomain.RemoteItem
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			child = &events[i]
		}
	}
	require.NotNil(t, child)
	assert.Equal(t, "a%20b/c.txt", child.Path)
}

// Validates: R-2.11.9, R-6.7.23
func TestFullDelta_ParentReferencePathFallbackChecksEverySegment(t *testing.T) {
	t.Parallel()

	// The parent chain cannot be resolved here (the parent is in neither the
	// window nor the baseline), so placement falls back to the verbatim
	// parentReference.path. A ".." segment there must not climb out, and a
	// folder literally named "a%20b" keeps its name (L-32).
	driveID := driveid.New(synctest.TestDriveID)
	place := func(parentPath string) ([]syncdomain.RemoteItem, error) {
		fetcher := &mockDeltaFetcher{pages: []mockDeltaPage{{page: &graph.DeltaPage{
			Items: []graph.Item{{
				ID: "child", Name: "x.txt", ParentID: "unknown-parent", DriveID: driveID,
				ParentPath: parentPath, ParentPathKnown: true,
			}},
			DeltaLink: "delta-link",
		}}}}
		events, _, err := NewRemoteObserver(&RemoteObserverInputs{
			Fetcher:  fetcher,
			Baseline: syncfixture.EmptyBaseline(),
			DriveID:  driveID,
			Logger:   synctest.TestLogger(t),
		}).FullDelta(t.Context(), "")
		return events, err
	}

	events, err := place("docs/../..")
	if err != nil {
		require.ErrorIs(t, err, syncdomain.ErrRemoteObservationIncomplete)
	}
	assert.Empty(t, events)

	events, err = place("a%20b")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "a%20b/x.txt", events[0].Path)
}

// A tombstone with neither name nor parent for an item the baseline does not
// hold is placed by the path the client's own remote_state row stores, so the
// row is retired instead of lingering (R-2.4.17).
//
// Validates: R-2.4.17
func TestFullDelta_PlacesNamelessUntrackedDeleteByItsRemoteStatePath(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "deleted-child", DriveID: driveid.New(synctest.TestDriveID), IsDeleted: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(synctest.TestDriveID),
		Logger:   synctest.TestLogger(t),
		RemotePathLookup: func(itemID string) (string, bool) {
			if itemID == "deleted-child" {
				return "Projects/old.txt", true
			}
			return "", false
		},
	})
	events, _, err := obs.FullDelta(t.Context(), "saved-token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.True(t, events[0].IsDeleted)
	assert.Equal(t, "Projects/old.txt", events[0].Path)
	assert.Equal(t, "old.txt", path.Base(events[0].Path))
}
