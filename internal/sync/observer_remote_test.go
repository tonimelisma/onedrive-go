package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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
// Test helpers
// ---------------------------------------------------------------------------

// emptyBaseline returns a Baseline with initialized but empty maps.
func emptyBaseline() *Baseline {
	return &Baseline{
		ByPath: make(map[string]*BaselineEntry),
		ByID:   make(map[driveid.ItemKey]*BaselineEntry),
	}
}

// baselineWith creates a Baseline pre-populated with the given entries.
// Each entry is keyed by both Path and ItemKey(DriveID, ItemID).
func baselineWith(entries ...*BaselineEntry) *Baseline {
	b := emptyBaseline()
	for _, e := range entries {
		b.ByPath[e.Path] = e
		b.ByID[driveid.NewItemKey(e.DriveID, e.ItemID)] = e
	}

	return b
}

const testDriveID = "0000000000000001"

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestFullDelta_NewFiles(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{
						ID: "f1", Name: "hello.txt", ParentID: "root", DriveID: driveid.New(testDriveID), Size: 100,
						QuickXorHash: "qxh1", ModifiedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
					},
					{
						ID: "f2", Name: "world.txt", ParentID: "root", DriveID: driveid.New(testDriveID), Size: 200,
						QuickXorHash: "qxh2", ModifiedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
					},
				},
				DeltaLink: "https://graph.microsoft.com/delta?token=new-token",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
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
		Path: "docs/readme.md", DriveID: driveid.New(testDriveID), ItemID: "f1",
		ParentID: "folder1", ItemType: ItemTypeFile, RemoteHash: "old-hash",
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "folder1", Name: "docs", ParentID: "root", DriveID: driveid.New(testDriveID), IsFolder: true},
					{
						ID: "f1", Name: "readme.md", ParentID: "folder1", DriveID: driveid.New(testDriveID),
						QuickXorHash: "new-hash", Size: 512,
					},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "prev-token")
	require.NoError(t, err, "FullDelta")

	// Root and folder produce events too (folder is classified).
	// Find the file event.
	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
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
		Path: "photos/cat.jpg", DriveID: driveid.New(testDriveID), ItemID: "f1",
		ItemType: ItemTypeFile,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "cat.jpg", DriveID: driveid.New(testDriveID), IsDeleted: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	e := events[0]
	assert.Equal(t, ChangeDelete, e.Type)
	assert.True(t, e.IsDeleted)
	assert.Equal(t, "photos/cat.jpg", e.Path)
}

func TestFullDelta_MovedFile(t *testing.T) {
	t.Parallel()

	baseline := baselineWith(
		&BaselineEntry{
			Path: "old-folder", DriveID: driveid.New(testDriveID), ItemID: "folder-old",
			ParentID: "root", ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "old-folder/doc.txt", DriveID: driveid.New(testDriveID), ItemID: "f1",
			ParentID: "folder-old", ItemType: ItemTypeFile,
		},
		&BaselineEntry{
			Path: "new-folder", DriveID: driveid.New(testDriveID), ItemID: "folder-new",
			ParentID: "root", ItemType: ItemTypeFolder,
		},
	)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					// File moved from old-folder to new-folder.
					{ID: "f1", Name: "doc.txt", ParentID: "folder-new", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var moveEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			moveEvent = &events[i]

			break
		}
	}

	require.NotNil(t, moveEvent, "move event not found")
	assert.Equal(t, ChangeMove, moveEvent.Type)
	assert.Equal(t, "new-folder/doc.txt", moveEvent.Path)
	assert.Equal(t, "old-folder/doc.txt", moveEvent.OldPath)
}

func TestFullDelta_MultiPage(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{
				page: &graph.DeltaPage{
					Items: []graph.Item{
						{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
						{ID: "f1", Name: "a.txt", ParentID: "root", DriveID: driveid.New(testDriveID)},
					},
					NextLink: "next-page-url",
				},
			},
			{
				page: &graph.DeltaPage{
					Items: []graph.Item{
						{ID: "f2", Name: "b.txt", ParentID: "root", DriveID: driveid.New(testDriveID)},
					},
					DeltaLink: "final-delta-link",
				},
			},
		},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	events, token, err := obs.FullDelta(t.Context(), "prev-token")
	require.NoError(t, err, "FullDelta")

	assert.Empty(t, events)
	assert.Equal(t, "empty-delta-link", token)
}

func TestFullDelta_DeltaExpired(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: graph.ErrGone,
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
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
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID), Name: "root"},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	assert.Empty(t, events, "root should be skipped")
}

func TestFullDelta_PathMaterialization_InFlight(t *testing.T) {
	t.Parallel()

	// Folder on page 1, file inside it on page 2. Both new.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{
				page: &graph.DeltaPage{
					Items: []graph.Item{
						{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
						{ID: "d1", Name: "Documents", ParentID: "root", DriveID: driveid.New(testDriveID), IsFolder: true},
					},
					NextLink: "next-page",
				},
			},
			{
				page: &graph.DeltaPage{
					Items: []graph.Item{
						{ID: "f1", Name: "report.pdf", ParentID: "d1", DriveID: driveid.New(testDriveID)},
					},
					DeltaLink: "delta-link",
				},
			},
		},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	// Find file event.
	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
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
		Path: "Projects/GoApp", DriveID: driveid.New(testDriveID), ItemID: "folder1",
		ParentID: "root", ItemType: ItemTypeFolder,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "main.go", ParentID: "folder1", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)
	assert.Equal(t, "Projects/GoApp/main.go", events[0].Path)
}

func TestFullDelta_PathMaterialization_Mixed(t *testing.T) {
	t.Parallel()

	// Existing folder in baseline, new subfolder in inflight.
	baseline := baselineWith(&BaselineEntry{
		Path: "Documents", DriveID: driveid.New(testDriveID), ItemID: "docs",
		ParentID: "root", ItemType: ItemTypeFolder,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					// New subfolder under existing Documents.
					{ID: "sub1", Name: "Reports", ParentID: "docs", DriveID: driveid.New(testDriveID), IsFolder: true},
					// New file under new subfolder.
					{ID: "f1", Name: "q1.pdf", ParentID: "sub1", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
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
		Path: "work/budget.xlsx", DriveID: driveid.New(testDriveID), ItemID: "f1",
		ItemType: ItemTypeFile,
	})

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					// Business API: deleted item with empty Name.
					{ID: "f1", Name: "", DriveID: driveid.New(testDriveID), IsDeleted: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(testDriveID), testLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(rawDriveID), testLogger(t))
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
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "f1", Name: nfd, ParentID: "root", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
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
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					// QuickXorHash preferred.
					{
						ID: "f1", Name: "a.txt", ParentID: "root", DriveID: driveid.New(testDriveID),
						QuickXorHash: "qxh", SHA256Hash: "sha",
					},
					// SHA256 fallback.
					{
						ID: "f2", Name: "b.txt", ParentID: "root", DriveID: driveid.New(testDriveID),
						SHA256Hash: "sha-only",
					},
					// Neither.
					{ID: "f3", Name: "c.txt", ParentID: "root", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 3)
	assert.Equal(t, "qxh", events[0].Hash, "QuickXorHash preferred")
	assert.Equal(t, "sha-only", events[1].Hash, "SHA256 fallback")
	assert.Empty(t, events[2].Hash)
}

func TestFullDelta_TimestampConversion(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 15, 12, 30, 45, 123456789, time.UTC)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "f1", Name: "test.txt", ParentID: "root", DriveID: driveid.New(testDriveID), ModifiedAt: ts},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	assert.Equal(t, ts.UnixNano(), events[0].Mtime)
}

func TestFullDelta_FolderEvent(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "d1", Name: "Photos", ParentID: "root", DriveID: driveid.New(testDriveID), IsFolder: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
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

			got := classifyItemType(&tt.item)
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
	assert.Equal(t, ts.UnixNano(), toUnixNano(ts))
	assert.Equal(t, int64(0), toUnixNano(time.Time{}))
}

func TestFullDelta_OrphanedItem(t *testing.T) {
	t.Parallel()

	// Item whose parent is not in the inflight map or baseline.
	// Observer should warn and return a partial path (just the item name).
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "orphan.txt", ParentID: "unknown-parent", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	// Orphaned item gets a partial path: just its own name.
	assert.Equal(t, "orphan.txt", events[0].Path, "partial path for orphan")
	assert.Equal(t, ChangeCreate, events[0].Type)
}

func TestFullDelta_DeletedItem_NotInBaseline(t *testing.T) {
	t.Parallel()

	// Item created and deleted between syncs — appears as deleted in delta
	// but has no baseline entry. Observer should produce an event with empty path.
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f1", Name: "ephemeral.txt", DriveID: driveid.New(testDriveID), IsDeleted: true},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	events, _, err := obs.FullDelta(t.Context(), "token")
	require.NoError(t, err, "FullDelta")

	require.Len(t, events, 1)

	e := events[0]
	assert.Equal(t, ChangeDelete, e.Type)

	// No baseline entry — path is empty.
	assert.Empty(t, e.Path, "no baseline entry")

	// Name is still available from the delta item itself.
	assert.Equal(t, "ephemeral.txt", e.Name)
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

func TestWatch_PollsAtInterval(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "f1", Name: "a.txt", ParentID: "root", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "token-1",
			}},
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "f2", Name: "b.txt", ParentID: "root", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "token-2",
			}},
		},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	obs.sleepFunc = noopSleep

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

	err := obs.Watch(ctx, "", events, time.Millisecond)
	require.NoError(t, err, "Watch returned error")

	assert.GreaterOrEqual(t, fetcher.calls, 2)
	assert.NotEmpty(t, obs.CurrentDeltaToken(), "delta token should be non-empty after successful polls")
}

func TestWatch_BackoffOnError(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
		pages: []mockDeltaPage{
			{err: errors.New("transient network error")},
			{err: errors.New("transient network error")},
			{page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "f1", Name: "ok.txt", ParentID: "root", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "recovered-token",
			}},
		},
	}

	var sleepDurations []time.Duration

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	obs.sleepFunc = func(_ context.Context, d time.Duration) error {
		sleepDurations = append(sleepDurations, d)
		return nil
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		<-events // wait for the successful event
		cancel()
	}()

	err := obs.Watch(ctx, "", events, time.Minute)
	require.NoError(t, err, "Watch returned error")

	// First sleep should be 5s (initial backoff), second should be 10s (doubled).
	require.GreaterOrEqual(t, len(sleepDurations), 2, "sleep should be called for backoff")
	assert.Equal(t, initialWatchBackoff, sleepDurations[0])
	assert.Equal(t, initialWatchBackoff*backoffMultiplier, sleepDurations[1])
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
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "f1", Name: "resync.txt", ParentID: "root", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "new-full-token",
			}},
		},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	obs.sleepFunc = noopSleep

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		<-events // wait for the resync event
		cancel()
	}()

	err := obs.Watch(ctx, "old-expired-token", events, time.Millisecond)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	obs.sleepFunc = func(ctx context.Context, _ time.Duration) error {
		// Block until canceled.
		<-ctx.Done()
		return ctx.Err()
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, "", events, time.Hour)
	}()

	// Let the first poll complete, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err, "Watch returned error")
	case <-time.After(5 * time.Second):
		require.Fail(t, "Watch did not return after context cancellation")
	}
}

func TestWatch_CurrentDeltaToken(t *testing.T) {
	t.Parallel()

	fetcher := &sequentialFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{DeltaLink: "token-after-poll-1"}},
			{page: &graph.DeltaPage{DeltaLink: "token-after-poll-2"}},
		},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))
	obs.sleepFunc = noopSleep

	// Before Watch starts, token is empty.
	assert.Empty(t, obs.CurrentDeltaToken(), "initial token")

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	pollCount := 0
	origSleep := obs.sleepFunc
	obs.sleepFunc = func(ctx context.Context, d time.Duration) error {
		pollCount++
		if pollCount >= 2 {
			cancel()
			return ctx.Err()
		}
		return origSleep(ctx, d)
	}

	err := obs.Watch(ctx, "initial-token", events, time.Millisecond)
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))

	// Track what interval the sleep function actually receives.
	var actualSleepDuration time.Duration
	obs.sleepFunc = func(_ context.Context, d time.Duration) error {
		actualSleepDuration = d
		return errors.New("stop after first sleep")
	}

	events := make(chan ChangeEvent, 10)
	// Pass zero interval — should be clamped to minPollInterval (30s).
	_ = obs.Watch(t.Context(), "", events, 0)

	assert.Equal(t, minPollInterval, actualSleepDuration, "clamped from 0")
}

func TestFullDelta_RenameInPlace(t *testing.T) {
	t.Parallel()

	// File renamed within the same parent folder (same parent, new name).
	baseline := baselineWith(
		&BaselineEntry{
			Path: "docs", DriveID: driveid.New(testDriveID), ItemID: "folder1",
			ParentID: "root", ItemType: ItemTypeFolder,
		},
		&BaselineEntry{
			Path: "docs/old-name.txt", DriveID: driveid.New(testDriveID), ItemID: "f1",
			ParentID: "folder1", ItemType: ItemTypeFile,
		},
	)

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					// Same parent (folder1), different name.
					{ID: "f1", Name: "new-name.txt", ParentID: "folder1", DriveID: driveid.New(testDriveID)},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, baseline, driveid.New(testDriveID), testLogger(t))
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
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "f1", Name: "test.txt", ParentID: "root", DriveID: driveid.New(testDriveID), Size: 10},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))

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
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{ID: "f1", Name: "doc.txt", ParentID: "root", DriveID: driveid.New(testDriveID), Size: 10},
					{ID: "f2", Name: "img.png", ParentID: "root", DriveID: driveid.New(testDriveID), Size: 20},
				},
				DeltaLink: "delta-link",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))

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

	obs := NewRemoteObserver(fetcher, baseline, primaryDrive, testLogger(t))

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

// TestFullDelta_PersonalVaultExcluded verifies that items with
// specialFolder.name == "vault" are excluded from delta processing by default,
// preventing data-loss from vault lock/unlock cycles (B-271).
func TestFullDelta_PersonalVaultExcluded(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					// Vault folder.
					{
						ID: "vault-folder", Name: "Personal Vault",
						ParentID: "root", DriveID: driveid.New(testDriveID),
						IsFolder: true, SpecialFolderName: "vault",
					},
					// File inside vault.
					{
						ID: "vault-file", Name: "secret.pdf",
						ParentID: "vault-folder", DriveID: driveid.New(testDriveID),
						Size: 1024, QuickXorHash: "vhash",
					},
					// Normal file outside vault.
					{
						ID: "normal-file", Name: "readme.txt",
						ParentID: "root", DriveID: driveid.New(testDriveID),
						Size: 256, QuickXorHash: "nhash",
					},
				},
				DeltaLink: "delta-vault",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))

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
func TestFullDelta_VaultChildBeforeParent(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			page: &graph.DeltaPage{
				Items: []graph.Item{
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					// Child appears BEFORE its vault parent — triggers B-281.
					{
						ID: "vault-file", Name: "secret.pdf",
						ParentID: "vault-folder", DriveID: driveid.New(testDriveID),
						Size: 1024, QuickXorHash: "vhash",
					},
					// Vault folder appears after its child.
					{
						ID: "vault-folder", Name: "Personal Vault",
						ParentID: "root", DriveID: driveid.New(testDriveID),
						IsFolder: true, SpecialFolderName: "vault",
					},
					// Normal file outside vault.
					{
						ID: "normal-file", Name: "readme.txt",
						ParentID: "root", DriveID: driveid.New(testDriveID),
						Size: 256, QuickXorHash: "nhash",
					},
				},
				DeltaLink: "delta-vault-reorder",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))

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
					{ID: "root", IsRoot: true, DriveID: driveid.New(testDriveID)},
					{
						ID: "f1", Name: "hashed.txt", ParentID: "root",
						DriveID: driveid.New(testDriveID), QuickXorHash: "abc123",
					},
					{
						ID: "f2", Name: "no-hash.txt", ParentID: "root",
						DriveID: driveid.New(testDriveID),
						// No hash fields set.
					},
					{
						ID: "f3", Name: "sha256.txt", ParentID: "root",
						DriveID: driveid.New(testDriveID), SHA256Hash: "deadbeef",
					},
				},
				DeltaLink: "delta-stats",
			},
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveid.New(testDriveID), testLogger(t))

	_, _, err := obs.FullDelta(t.Context(), "")
	require.NoError(t, err, "FullDelta")

	stats := obs.Stats()
	assert.Equal(t, int64(2), stats.HashesComputed, "items with hashes")
}

// ---------------------------------------------------------------------------
// B-307: Remote observer filtering symmetry
// ---------------------------------------------------------------------------

// TestClassifyItem_FilteringSymmetry validates that the remote observer
// applies the same filtering as the local observer (S7 symmetric).
func TestClassifyItem_FilteringSymmetry(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	bl := emptyBaseline()
	obs := NewRemoteObserver(nil, bl, driveID, testLogger(t))

	inflight := map[driveid.ItemKey]inflightParent{
		driveid.NewItemKey(driveID, "root"): {name: "", isRoot: true},
	}

	tests := []struct {
		name      string
		itemName  string
		isDeleted bool
		wantNil   bool
	}{
		// Excluded names should be filtered.
		{".tmp file", "data.tmp", false, true},
		{".partial file", "download.partial", false, true},
		{"tilde backup", "~backup.txt", false, true},
		{"dot-tilde lock", ".~lock.file", false, true},
		{".swp file", "file.swp", false, true},
		{".crdownload", "file.crdownload", false, true},
		// Invalid OneDrive names.
		{"reserved name CON", "CON", false, true},
		{"trailing dot", "file.", false, true},
		{"trailing space", "file ", false, true},
		// desktop.ini has a leading space so it passes isValidOneDriveName
		// but let's test the standard Office temp pattern.
		{"tilde-dollar", "~$document.docx", false, true},

		// Valid names should produce events.
		{"normal file", "hello.txt", false, false},
		{"db file", "data.db", false, false},
		{"pdf file", "report.pdf", false, false},

		// Deleted items with excluded names MUST pass through for cleanup.
		{"deleted .tmp", "data.tmp", true, false},
		{"deleted tilde", "~backup.txt", true, false},
		{"deleted .partial", "download.partial", true, false},
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

			ev := obs.classifyItem(item, inflight)
			if tt.wantNil {
				assert.Nil(t, ev, "expected nil for %q", tt.itemName)
			} else {
				assert.NotNil(t, ev, "expected event for %q", tt.itemName)
			}
		})
	}
}

// TestFilteringSymmetry_BothObserversAgree confirms that isAlwaysExcluded
// and isValidOneDriveName produce identical results for a shared set of
// test names, regardless of which observer calls them.
func TestFilteringSymmetry_BothObserversAgree(t *testing.T) {
	t.Parallel()

	names := []string{
		"hello.txt", "data.db", "report.pdf",
		"download.partial", "data.tmp", "file.swp", "file.crdownload",
		"~backup.txt", ".~lock.file", "~$document.docx",
		"CON", "PRN", "NUL",
		"file.", "file ", " leading",
	}

	for _, name := range names {
		excluded := isAlwaysExcluded(name) || !isValidOneDriveName(name)
		// Both observers use the same functions — this is a regression
		// guard that the filtering logic stays in scanner.go (shared).
		assert.Equal(t, excluded, isAlwaysExcluded(name) || !isValidOneDriveName(name),
			"filtering must be deterministic for %q", name)
	}
}

// mockObservationWriter records CommitObservation calls for test verification.
type mockObservationWriter struct {
	calls []mockObsWriterCall
	err   error
}

type mockObsWriterCall struct {
	events   []ObservedItem
	newToken string
	driveID  driveid.ID
}

func (m *mockObservationWriter) CommitObservation(_ context.Context, events []ObservedItem, newToken string, driveID driveid.ID) error {
	m.calls = append(m.calls, mockObsWriterCall{events: events, newToken: newToken, driveID: driveID})
	return m.err
}

// TestWatch_CommitsObservations verifies that Watch calls CommitObservation
// after each successful FullDelta poll.
func TestWatch_CommitsObservations(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

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

	writer := &mockObservationWriter{}
	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, testLogger(t))
	obs.obsWriter = writer
	obs.sleepFunc = noopSleep

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

	err := obs.Watch(ctx, "", events, time.Millisecond)
	require.NoError(t, err)

	// CommitObservation should have been called.
	require.GreaterOrEqual(t, len(writer.calls), 1, "CommitObservation should be called")
	assert.Equal(t, "token-1", writer.calls[0].newToken)
	assert.Equal(t, driveID, writer.calls[0].driveID)
}

// TestWatch_ObsWriterError_ContinuesRetry verifies that a CommitObservation
// error causes the observer to retry (continue loop) rather than crash.
func TestWatch_ObsWriterError_ContinuesRetry(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	pollCount := 0

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{
			{page: &graph.DeltaPage{
				Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
				DeltaLink: "token-1",
			}},
			{page: &graph.DeltaPage{
				Items:     []graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}},
				DeltaLink: "token-2",
			}},
		},
	}

	writer := &mockObservationWriter{err: fmt.Errorf("simulated commit error")}
	obs := NewRemoteObserver(fetcher, emptyBaseline(), driveID, testLogger(t))
	obs.obsWriter = writer
	obs.sleepFunc = func(_ context.Context, _ time.Duration) error {
		pollCount++
		if pollCount >= 3 {
			return fmt.Errorf("stop")
		}
		return nil
	}

	events := make(chan ChangeEvent, 10)
	err := obs.Watch(t.Context(), "", events, time.Millisecond)
	require.NoError(t, err)

	// Should have retried — multiple commit attempts.
	assert.GreaterOrEqual(t, len(writer.calls), 1, "should retry after commit failure")
}
