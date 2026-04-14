package sync

import (
	"context"
	"errors"
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
	"github.com/tonimelisma/onedrive-go/internal/synctest"
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
			{ID: "f1", Name: "a.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID)},
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
				{ID: "f2", Name: "b.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID)},
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, token, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	assert.Equal(t, "https://graph.microsoft.com/delta?token=new-token", token, "token should be delta link URL")

	require.Len(t, events, 2)

	// First file.
	e := events[0]
	assert.Equal(t, ChangeCreate, e.Type)
	assert.Equal(t, "hello.txt", e.Path)
	assert.Equal(t, "qxh1", e.Hash)
	assert.Equal(t, int64(100), e.Size)
	assert.Equal(t, SourceRemote, e.Source)

	// Second file.
	assert.Equal(t, "world.txt", events[1].Name)
}

func TestFullDelta_ModifiedFile(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(&BaselineEntry{
		Path: "docs/readme.md", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ParentID: "folder1", ItemType: ItemTypeFile, RemoteHash: "old-hash",
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "prev-token")
	require.NoError(t, err, "FullDelta")

	// Root and folder produce events too (folder is classified).
	// Find the file event.
	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]

			break
		}
	}

	require.NotNil(t, fileEvent, "file event not found")
	assert.Equal(t, ChangeModify, fileEvent.Type)
	assert.Equal(t, "docs/readme.md", fileEvent.Path)
	assert.Equal(t, "new-hash", fileEvent.Hash)
}

func TestFullDelta_DeletedFile(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(&BaselineEntry{
		Path: "photos/cat.jpg", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ItemType: ItemTypeFile,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	e := events[0]
	assert.Equal(t, ChangeDelete, e.Type)
	assert.True(t, e.IsDeleted)
	assert.Equal(t, "photos/cat.jpg", e.Path)
}

// Validates: R-6.7.29
func TestFullDelta_ModifiedFile_SparseFieldsRecoveredFromBaseline(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(&BaselineEntry{
		Path: "docs/readme.md", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ParentID: "folder1", ItemType: ItemTypeFile, RemoteHash: "old-hash",
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "prev-token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, ChangeModify, events[0].Type)
	assert.Equal(t, "docs/readme.md", events[0].Path)
	assert.Equal(t, "readme.md", events[0].Name)
	assert.Equal(t, "new-hash", events[0].Hash)
}

func TestFullDelta_MovedFile(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(
		&BaselineEntry{
			Path: "old-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-old",
			ParentID: "root", ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "old-folder/doc.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "folder-old", ItemType: ItemTypeFile,
		},
		&BaselineEntry{
			Path: "new-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-new",
			ParentID: "root", ItemType: ItemTypeFolder,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var moveEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			moveEvent = &events[i]

			break
		}
	}

	require.NotNil(t, moveEvent, "move event not found")
	assert.Equal(t, ChangeMove, moveEvent.Type)
	assert.Equal(t, "new-folder/doc.txt", moveEvent.Path)
	assert.Equal(t, "old-folder/doc.txt", moveEvent.OldPath)
}

// Validates: R-6.7.29
func TestFullDelta_MovedFile_SparseNameRecoveredFromBaseline(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(
		&BaselineEntry{
			Path: "old-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-old",
			ParentID: "root", ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "old-folder/doc.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "folder-old", ItemType: ItemTypeFile,
		},
		&BaselineEntry{
			Path: "new-folder", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder-new",
			ParentID: "root", ItemType: ItemTypeFolder,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, ChangeMove, events[0].Type)
	assert.Equal(t, "new-folder/doc.txt", events[0].Path)
	assert.Equal(t, "old-folder/doc.txt", events[0].OldPath)
	assert.Equal(t, "doc.txt", events[0].Name)
}

// Validates: R-6.7.29
func TestFullDelta_DescendantFollowsSparseRenamedParentWithOmittedParentReference(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(
		&BaselineEntry{
			Path: "Team", DriveID: driveid.New(synctest.TestDriveID), ItemID: "team",
			ParentID: "root", ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "Team/Reports", DriveID: driveid.New(synctest.TestDriveID), ItemID: "reports",
			ParentID: "team", ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "Team/Reports/q1.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "reports", ItemType: ItemTypeFile,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]
			break
		}
	}

	require.NotNil(t, fileEvent, "descendant event not found")
	assert.Equal(t, ChangeMove, fileEvent.Type)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	_, _, err := obs.FullDelta(t.Context(), "expired-token")

	assert.ErrorIs(t, err, ErrDeltaExpired)
}

func TestFullDelta_FetchError(t *testing.T) {
	t.Parallel()

	fetchErr := errors.New("network timeout")
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: fetchErr,
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	// Find file event.
	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]

			break
		}
	}

	require.NotNil(t, fileEvent, "file event not found")
	assert.Equal(t, "Documents/report.pdf", fileEvent.Path)
}

func TestFullDelta_PathMaterialization_Baseline(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(&BaselineEntry{
		Path: "Projects/GoApp", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder1",
		ParentID: "root", ItemType: ItemTypeFolder,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, "Projects/GoApp/main.go", events[0].Path)
}

func TestFullDelta_PathMaterialization_Mixed(t *testing.T) {
	t.Parallel()

	// Existing folder in baseline, new subfolder in inflight.
	baseline := baselineWith(&BaselineEntry{
		Path: "Documents", DriveID: driveid.New(synctest.TestDriveID), ItemID: "docs",
		ParentID: "root", ItemType: ItemTypeFolder,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == sharedTestFileItemID {
			fileEvent = &events[i]

			break
		}
	}

	require.NotNil(t, fileEvent, "file event not found")
	assert.Equal(t, "Documents/Reports/q1.pdf", fileEvent.Path)
}

func TestFullDelta_DeletedItem_MissingName(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(&BaselineEntry{
		Path: "work/budget.xlsx", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
		ItemType: ItemTypeFile,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, "budget.xlsx", events[0].Name, "recovered from baseline")
	assert.Equal(t, "work/budget.xlsx", events[0].Path)
}

func TestFullDelta_DriveIDNormalization(t *testing.T) {
	t.Parallel()

	// 15-char uppercase driveID — should be lowercased + zero-padded.
	rawDriveID := "ABC123DEF456789"

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(rawDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	want := driveid.New("0abc123def456789") // lowercased + zero-padded to 16
	assert.True(t, events[0].DriveID.Equal(want), "DriveID = %q, want %q", events[0].DriveID, want)

	// Verify the fetcher was called with the normalized driveID, not the raw one.
	require.Len(t, fetcher.driveIDs, 1, "fetcher call count")
	assert.True(t, fetcher.driveIDs[0].Equal(want), "fetcher received driveID = %q, want %q (normalized)", fetcher.driveIDs[0], want)
}

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, nfc, events[0].Name, "NFC-normalized")
	assert.Equal(t, nfc, events[0].Path, "NFC-normalized")
}

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 3)
	assert.Equal(t, "qxh", events[0].Hash, "QuickXorHash preferred")
	assert.Equal(t, "sha-only", events[1].Hash, "SHA256 fallback")
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, ItemTypeFolder, events[0].ItemType)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	_, _, err := obs.FullDelta(ctx, "token")

	require.Error(t, err, "expected error")
	assert.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// Unit tests for helper functions
// ---------------------------------------------------------------------------

func TestDriveIDNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"15-char lowercase", "abc123def456789", "0abc123def456789"},
		{"16-char lowercase", "abc123def4567890", "abc123def4567890"},
		{"uppercase", "ABC123DEF4567890", "abc123def4567890"},
		{"15-char uppercase", "ABC123DEF456789", "0abc123def456789"},
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
		want ItemType
	}{
		{"file", graph.Item{}, ItemTypeFile},
		{"folder", graph.Item{IsFolder: true}, ItemTypeFolder},
		{"root", graph.Item{IsRoot: true}, ItemTypeRoot},
		{"root takes precedence", graph.Item{IsRoot: true, IsFolder: true}, ItemTypeRoot},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ClassifyItemType(&tt.item)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSelectHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		item graph.Item
		want string
	}{
		{"QuickXor present", graph.Item{QuickXorHash: "qxh", SHA256Hash: "sha"}, "qxh"},
		{"SHA256 only", graph.Item{SHA256Hash: "sha"}, "sha"},
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

func TestToUnixNano(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, ts.UnixNano(), ToUnixNano(ts))
	assert.Equal(t, int64(0), ToUnixNano(time.Time{}))
}

func TestFullDelta_OrphanedItem(t *testing.T) {
	t.Parallel()

	// Item whose parent is not in the inflight map or baseline.
	// Observer should warn and return a partial path (just the item name).
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	// Orphaned item gets a partial path: just its own name.
	assert.Equal(t, "orphan.txt", events[0].Path, "partial path for orphan")
	assert.Equal(t, ChangeCreate, events[0].Type)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, ChangeDelete, events[0].Type)
	assert.Equal(t, "ephemeral.txt", events[0].Path)
	assert.Equal(t, "ephemeral.txt", events[0].Name)
}

func fullDeltaSingleItemEvents(t *testing.T, item *graph.Item) []ChangeEvent {
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	return events
}

// Validates: R-6.7.28
func TestFullDelta_SkipsItemWithEmptyItemID(t *testing.T) {
	t.Parallel()

	events := fullDeltaSingleItemEvents(t, &graph.Item{
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

	events := fullDeltaSingleItemEvents(t, &graph.Item{
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

	events := fullDeltaSingleItemEvents(t, &graph.Item{
		ID:        "gone",
		Name:      "",
		ParentID:  "",
		DriveID:   driveid.New(synctest.TestDriveID),
		IsDeleted: true,
	})

	assert.Empty(t, events, "deletes without recoverable path data should be skipped")
}

// ---------------------------------------------------------------------------
// Watch tests
// ---------------------------------------------------------------------------

// noopSleep immediately returns nil. Tests use this to skip real delays.
func noopSleep(_ context.Context, _ time.Duration) error {
	return nil
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

func TestWatch_PollsAtInterval(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
		pages: twoPageDeltaPages("delta", "token-1", "token-2"),
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	obs.SleepFunc = noopSleep

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		// Wait for at least 2 events then cancel.
		received := 0
		for range events {
			received++
			if received >= 2 {
				cancel()
				return
			}
		}
	}()

	err := obs.Watch(ctx, "", events, time.Millisecond, nil, nil)
	require.NoError(t, err, "Watch returned error")

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, synctest.TestLogger(t))
	wakeCh := make(chan struct{}, 1)

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	go func() {
		received := 0
		for range events {
			received++
			if received == 1 {
				tryWake(wakeCh)
			}
			if received >= 2 {
				cancel()
				return
			}
		}
	}()

	err := obs.Watch(ctx, "", events, time.Hour, wakeCh, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, fetcher.CallCount(), "wake should trigger a second delta cycle before fallback polling")
}

// Validates: R-2.8.5
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, synctest.TestLogger(t))
	wakeCh := make(chan struct{}, 1)

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	go func() {
		received := 0
		for range events {
			received++
			if received == 1 {
				tryWake(wakeCh)
				tryWake(wakeCh)
			}
			if received == 2 {
				time.AfterFunc(50*time.Millisecond, cancel)
				return
			}
		}
	}()

	err := obs.Watch(ctx, "", events, time.Hour, wakeCh, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, fetcher.CallCount(), "burst wakes should collapse into one pending delta cycle")
}

func TestWatch_BackoffOnError(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
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

	var sleepDurations []time.Duration

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	obs.SleepFunc = func(_ context.Context, d time.Duration) error {
		sleepDurations = append(sleepDurations, d)
		return nil
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		<-events // wait for the successful event
		cancel()
	}()

	err := obs.Watch(ctx, "", events, time.Minute, nil, nil)
	require.NoError(t, err, "Watch returned error")

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, synctest.TestLogger(t))
	store := newTestStore(t)
	wakeCh := make(chan struct{}, 1)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(
			ctx,
			"old-token",
			make(chan ChangeEvent, 1),
			time.Hour,
			wakeCh,
			func(ctx context.Context, events []ChangeEvent, newToken string) ([]ChangeEvent, error) {
				return events, store.CommitObservation(ctx, changeEventsToObservedItems(obs.logger, events), newToken, driveID)
			},
		)
	}()

	require.Eventually(t, func() bool {
		return fetcher.CallCount() >= 1
	}, time.Second, 10*time.Millisecond)
	tryWake(wakeCh)
	require.Eventually(t, func() bool {
		return fetcher.CallCount() >= 2
	}, time.Second, 10*time.Millisecond)
	cancel()

	err := <-done
	require.NoError(t, err)
	assert.Empty(t, readDeltaToken(t, store.DB(), driveID.String()), "zero-event wake polls must not commit observations")
	assert.Equal(t, "old-token", obs.CurrentDeltaToken(), "zero-event wake polls must not advance the token")
}

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	obs.SleepFunc = noopSleep

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		<-events // wait for the resync event
		cancel()
	}()

	err := obs.Watch(ctx, "old-expired-token", events, time.Millisecond, nil, nil)
	require.NoError(t, err, "Watch returned error")

	// After delta expired, token should have been reset to "" for resync,
	// then updated to the new token from the successful full resync.
	assert.Equal(t, "new-full-token", obs.CurrentDeltaToken())
}

func TestWatch_ContextCancellation(t *testing.T) {
	t.Parallel()

	// Fetcher that always succeeds with empty delta (no events to send).
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{DeltaLink: "token"},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	pollStarted := make(chan struct{})
	obs.SleepFunc = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-pollStarted:
		default:
			close(pollStarted)
		}
		// Block until canceled.
		<-ctx.Done()
		return ctx.Err()
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, "", events, time.Hour, nil, nil)
	}()

	select {
	case <-pollStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for remote watch sleep")
	}
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "Watch returned error")
	case <-time.After(5 * time.Second):
		require.Fail(t, "Watch did not return after context cancellation")
	}
}

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	obs.SleepFunc = noopSleep

	// Before Watch starts, token is empty.
	assert.Empty(t, obs.CurrentDeltaToken(), "initial token")

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	pollCount := 0
	origSleep := obs.SleepFunc
	obs.SleepFunc = func(ctx context.Context, d time.Duration) error {
		pollCount++
		if pollCount >= 2 {
			cancel()
			return ctx.Err()
		}
		return origSleep(ctx, d)
	}

	err := obs.Watch(ctx, "initial-token", events, time.Millisecond, nil, nil)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))

	// Track what interval the sleep function actually receives.
	var actualSleepDuration time.Duration
	obs.SleepFunc = func(_ context.Context, d time.Duration) error {
		actualSleepDuration = d
		return errors.New("stop after first sleep")
	}

	events := make(chan ChangeEvent, 10)
	// Pass zero interval — should be clamped to MinPollInterval (30s).
	err := obs.Watch(t.Context(), "", events, 0, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stop after first sleep")

	assert.Equal(t, MinPollInterval, actualSleepDuration, "clamped from 0")
}

// Validates: R-6.7.24
func TestFullDelta_RenameInPlace(t *testing.T) {
	t.Parallel()

	// File renamed within the same parent folder (same parent, new name).
	baseline := baselineWith(
		&BaselineEntry{
			Path: "docs", DriveID: driveid.New(synctest.TestDriveID), ItemID: "folder1",
			ParentID: "root", ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "docs/old-name.txt", DriveID: driveid.New(synctest.TestDriveID), ItemID: "f1",
			ParentID: "folder1", ItemType: ItemTypeFile,
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

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(synctest.TestDriveID), synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var renameEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			renameEvent = &events[i]

			break
		}
	}

	require.NotNil(t, renameEvent, "rename event not found")
	assert.Equal(t, ChangeMove, renameEvent.Type)
	assert.Equal(t, "docs/new-name.txt", renameEvent.Path)
	assert.Equal(t, "docs/old-name.txt", renameEvent.OldPath)
	assert.Equal(t, "new-name.txt", renameEvent.Name)
}

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
			name: "SHA256Hash fallback",
			item: graph.Item{SHA256Hash: "sha256", SHA1Hash: "sha1"},
			want: "sha256",
		},
		{
			name: "SHA1Hash fallback",
			item: graph.Item{SHA1Hash: "sha1"},
			want: "sha1",
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

// TestRemoteObserver_LastActivity verifies that LastActivity() is updated
// after a successful FullDelta poll, providing a liveness signal for the
// engine to detect stalled observers (B-125).
func TestRemoteObserver_LastActivity(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(synctest.TestDriveID)},
					{ID: "f1", Name: "test.txt", ParentID: "root", DriveID: driveid.New(synctest.TestDriveID), Size: 10},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))

	// Before any poll, LastActivity should be zero.
	assert.True(t, obs.LastActivity().IsZero(), "LastActivity before poll should be zero")

	before := time.Now()

	_, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	after := time.Now()
	activity := obs.LastActivity()

	assert.False(t, activity.Before(before), "LastActivity = %v, want >= %v", activity, before)
	assert.False(t, activity.After(after), "LastActivity = %v, want <= %v", activity, after)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))

	_, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	stats := obs.Stats()

	assert.NotZero(t, stats.EventsEmitted, "EventsEmitted should be > 0 after processing items")
	assert.NotZero(t, stats.PollsCompleted, "PollsCompleted should be > 0 after a successful FullDelta")
}

// TestFullDelta_CrossDriveItems verifies path resolution for items that
// reference a different drive than the observer's primary drive, e.g., shared
// items where the item's DriveID differs from the observer's driveID (B-007).
func TestFullDelta_CrossDriveItems(t *testing.T) {
	t.Parallel()

	primaryDrive := driveid.New("0000000000000001")
	sharedDrive := driveid.New("0000000000000099")

	// Baseline has the root of the primary drive.
	baseline := baselineWith(
		&BaselineEntry{
			Path: "shared-folder", DriveID: primaryDrive, ItemID: "sf1",
			ParentID: "root", ItemType: ItemTypeFolder,
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

	obs := NewRemoteObserver(fetcher, baseline, primaryDrive, synctest.TestLogger(t))

	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	ev := events[0]

	// The item should use sharedDrive as its DriveID.
	assert.True(t, ev.DriveID.Equal(sharedDrive), "DriveID = %v, want %v (shared drive)", ev.DriveID, sharedDrive)

	// Path should be resolved through the primary drive's baseline.
	assert.Equal(t, "shared-folder/shared-doc.txt", ev.Path)

	// Since the item is new (not in baseline), it should be a Create.
	assert.Equal(t, ChangeCreate, ev.Type)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))

	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	// Only the normal file should produce an event — vault items are excluded.
	require.Len(t, events, 1, "vault excluded")
	assert.Equal(t, "readme.txt", events[0].Name)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))

	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	// Only the normal file should produce an event — vault child must be
	// skipped regardless of ordering within the delta page.
	require.Len(t, events, 1, "vault child skipped despite ordering")
	assert.Equal(t, "readme.txt", events[0].Name)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(synctest.TestDriveID), synctest.TestLogger(t))

	_, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	stats := obs.Stats()
	assert.Equal(t, int64(2), stats.HashesComputed, "items with hashes")
}

// ---------------------------------------------------------------------------
// Remote observer trusts server — no name filtering
// ---------------------------------------------------------------------------

// TestClassifyItem_RemoteTrustsServer validates that the remote observer does
// NOT filter items by name validity. If OneDrive sends an item in a delta
// response, it exists on OneDrive — filtering it would be silent data loss.
// Only root and vault items are filtered.
func TestClassifyItem_RemoteTrustsServer(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	bl := emptyBaseline()
	obs := NewRemoteObserver(nil, bl, driveID, synctest.TestLogger(t))

	inflight := map[driveid.ItemKey]InflightParent{
		driveid.NewItemKey(driveID, "root"): {Name: "", IsRoot: true},
	}

	tests := []struct {
		name      string
		itemName  string
		isDeleted bool
	}{
		// Names that the local observer would exclude — remote observer
		// must still produce events for these because they exist on OneDrive.
		{".tmp file", "data.tmp", false},
		{".partial file", "download.partial", false},
		{"tilde backup", "~backup.txt", false},
		{"dot-tilde lock", ".~lock.file", false},
		{".swp file", "file.swp", false},
		{".crdownload", "file.crdownload", false},
		{"reserved name CON", "CON", false},
		{"trailing dot", "file.", false},
		{"trailing space", "file ", false},
		{"tilde-dollar", "~$document.docx", false},

		// Valid names produce events as before.
		{"normal file", "hello.txt", false},
		{"db file", "data.db", false},
		{"pdf file", "report.pdf", false},

		// Deleted items also pass through.
		{"deleted .tmp", "data.tmp", true},
		{"deleted tilde", "~backup.txt", true},
		{"deleted .partial", "download.partial", true},
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
			assert.NotNil(t, ev, "remote observer must produce event for %q (server data is authoritative)", tt.itemName)
		})
	}
}

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, synctest.TestLogger(t))
	store := newTestStore(t)
	obs.SleepFunc = noopSleep

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		// Receive events then cancel.
		received := 0
		for range events {
			received++
			if received >= 1 {
				cancel()
				return
			}
		}
	}()

	err := obs.Watch(
		ctx,
		"",
		events,
		time.Millisecond,
		nil,
		func(ctx context.Context, events []ChangeEvent, newToken string) ([]ChangeEvent, error) {
			return events, store.CommitObservation(ctx, changeEventsToObservedItems(obs.logger, events), newToken, driveID)
		},
	)
	require.NoError(t, err)

	assert.Equal(t, "token-1", readDeltaToken(t, store.DB(), driveID.String()))
	row := readRemoteStateRow(t, store.DB(), "f1")
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, synctest.TestLogger(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	events := make(chan ChangeEvent, 10)
	err := obs.Watch(ctx, "", events, time.Millisecond, nil, func(context.Context, []ChangeEvent, string) ([]ChangeEvent, error) {
		return nil, filepath.ErrBadPattern
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, synctest.TestLogger(t))
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	obs.SleepFunc = func(ctx context.Context, _ time.Duration) error {
		pollCount++
		if pollCount >= 1 {
			cancel()
			return ctx.Err()
		}

		return nil
	}

	events := make(chan ChangeEvent, 10)
	err := obs.Watch(
		ctx,
		"old-token",
		events,
		time.Millisecond,
		nil,
		func(ctx context.Context, events []ChangeEvent, newToken string) ([]ChangeEvent, error) {
			return events, store.CommitObservation(ctx, changeEventsToObservedItems(obs.logger, events), newToken, driveID)
		},
	)
	require.NoError(t, err)

	assert.Empty(t, readDeltaToken(t, store.DB(), driveID.String()), "should not commit observations when 0 events returned")
	assert.Equal(t, "old-token", obs.CurrentDeltaToken(), "token should not advance when 0 events returned")
}

// ---------------------------------------------------------------------------
// Embedded shared-folder link handling
// ---------------------------------------------------------------------------

func TestClassifyItem_RegularFileNoRemoteDriveID(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	bl := emptyBaseline()
	obs := NewRemoteObserver(nil, bl, driveID, synctest.TestLogger(t))

	inflight := map[driveid.ItemKey]InflightParent{
		driveid.NewItemKey(driveID, "root"): {Name: "", IsRoot: true},
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

	assert.Equal(t, ChangeCreate, ev.Type, "regular file without RemoteDriveID should be ChangeCreate")
}

func TestClassifyItem_EmbeddedSharedLinkIgnored(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(synctest.TestDriveID)
	obs := NewRemoteObserver(nil, emptyBaseline(), driveID, synctest.TestLogger(t))

	inflight := map[driveid.ItemKey]InflightParent{
		driveid.NewItemKey(driveID, "root"): {Name: "", IsRoot: true},
	}

	item := &graph.Item{
		ID:            "shared-link-1",
		Name:          "TeamDocs",
		ParentID:      "root",
		DriveID:       driveID,
		IsFolder:      true,
		IsDeleted:     true,
		RemoteDriveID: "source-drive-abc",
		RemoteItemID:  "source-item-123",
	}

	ev := obs.Converter.ClassifyItem(item, inflight)
	assert.Nil(t, ev, "embedded shared-folder links should be ignored in a normal drive")
}

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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, synctest.TestLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err)

	require.Len(t, events, 1)
	assert.Equal(t, "f1", events[0].ItemID)
	assert.Equal(t, "myfile.txt", events[0].Path)
}
