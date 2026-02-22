package sync

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// --- Mock DeltaFetcher ---

// mockDeltaFetcher returns pre-configured pages in sequence.
// Each call to Delta pops the next page; an error can be injected
// at a specific call index.
type mockDeltaFetcher struct {
	pages    []*graph.DeltaPage
	callIdx  int
	errAtIdx int         // inject error at this call index (-1 = never)
	err      error       // the error to inject
	calls    []deltaCall // records all calls for verification
}

type deltaCall struct {
	DriveID string
	Token   string
}

func newMockFetcher(pages ...*graph.DeltaPage) *mockDeltaFetcher {
	return &mockDeltaFetcher{
		pages:    pages,
		errAtIdx: -1,
	}
}

func (m *mockDeltaFetcher) Delta(_ context.Context, driveID, token string) (*graph.DeltaPage, error) {
	m.calls = append(m.calls, deltaCall{DriveID: driveID, Token: token})

	if m.errAtIdx >= 0 && m.callIdx == m.errAtIdx {
		m.callIdx++
		return nil, m.err
	}

	if m.callIdx >= len(m.pages) {
		return nil, errors.New("no more pages configured in mock")
	}

	page := m.pages[m.callIdx]
	m.callIdx++

	return page, nil
}

// --- Mock Store ---

// mockStore implements DeltaStore for delta processor tests.
type mockStore struct {
	// Delta token state
	deltaToken    string
	deltaComplete bool
	tokenDeleted  bool

	// Item storage keyed by "driveID/itemID"
	items map[string]*Item

	// Call recordings
	upsertCalls        []*Item
	markDeletedCalls   []markDeletedCall
	materializeResults map[string]string // "driveID/itemID" -> path
	cascadeCalls       []cascadeCall
	savedTokens        []savedToken
	deltaCompleteCalls []bool

	// Error injection
	getItemErr        error
	upsertErr         error
	markDeletedErr    error
	materializeErr    error
	cascadeErr        error
	getDeltaTokenErr  error
	saveDeltaTokenErr error
	setCompleteErr    error
	deleteDeltaErr    error
}

type markDeletedCall struct {
	DriveID   string
	ItemID    string
	DeletedAt int64
}

type cascadeCall struct {
	OldPrefix string
	NewPrefix string
}

type savedToken struct {
	DriveID string
	Token   string
}

func newMockStore() *mockStore {
	return &mockStore{
		items:              make(map[string]*Item),
		materializeResults: make(map[string]string),
	}
}

func (s *mockStore) itemKey(driveID, itemID string) string {
	return driveID + "/" + itemID
}

func (s *mockStore) GetDeltaToken(_ context.Context, _ string) (string, error) {
	return s.deltaToken, s.getDeltaTokenErr
}

func (s *mockStore) SaveDeltaToken(_ context.Context, driveID, token string) error {
	s.savedTokens = append(s.savedTokens, savedToken{DriveID: driveID, Token: token})
	s.deltaToken = token
	return s.saveDeltaTokenErr
}

func (s *mockStore) DeleteDeltaToken(_ context.Context, _ string) error {
	s.deltaToken = ""
	s.tokenDeleted = true
	return s.deleteDeltaErr
}

func (s *mockStore) SetDeltaComplete(_ context.Context, _ string, complete bool) error {
	s.deltaCompleteCalls = append(s.deltaCompleteCalls, complete)
	s.deltaComplete = complete
	return s.setCompleteErr
}

func (s *mockStore) GetItem(_ context.Context, driveID, itemID string) (*Item, error) {
	if s.getItemErr != nil {
		return nil, s.getItemErr
	}
	return s.items[s.itemKey(driveID, itemID)], nil
}

func (s *mockStore) UpsertItem(_ context.Context, item *Item) error {
	s.upsertCalls = append(s.upsertCalls, item)
	s.items[s.itemKey(item.DriveID, item.ItemID)] = item
	return s.upsertErr
}

func (s *mockStore) MarkDeleted(_ context.Context, driveID, itemID string, deletedAt int64) error {
	s.markDeletedCalls = append(s.markDeletedCalls, markDeletedCall{
		DriveID:   driveID,
		ItemID:    itemID,
		DeletedAt: deletedAt,
	})

	if item, ok := s.items[s.itemKey(driveID, itemID)]; ok {
		item.IsDeleted = true
		item.DeletedAt = Int64Ptr(deletedAt)
	}

	return s.markDeletedErr
}

func (s *mockStore) MaterializePath(_ context.Context, driveID, itemID string) (string, error) {
	if s.materializeErr != nil {
		return "", s.materializeErr
	}

	if path, ok := s.materializeResults[s.itemKey(driveID, itemID)]; ok {
		return path, nil
	}

	return "/default/" + itemID, nil
}

func (s *mockStore) CascadePathUpdate(_ context.Context, oldPrefix, newPrefix string) error {
	s.cascadeCalls = append(s.cascadeCalls, cascadeCall{OldPrefix: oldPrefix, NewPrefix: newPrefix})
	return s.cascadeErr
}

// --- Test helper ---

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testWriter adapts testing.T to io.Writer for slog output.
type testWriter struct {
	t *testing.T
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

// --- Tests ---

func TestFetchAndApply_SinglePage(t *testing.T) {
	store := newMockStore()
	// buildNewItemPath calls MaterializePath with the *parent* ID, then appends
	// the item's name. Both items have ParentID "root".
	store.materializeResults["d/root"] = "docs"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "item-1", Name: "file1.txt", DriveID: "d", ParentID: "root", Size: 100,
				QuickXorHash: "abc123", ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			{
				ID: "item-2", Name: "folder1", DriveID: "d", ParentID: "root", IsFolder: true,
				ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "https://graph.microsoft.com/v1.0/drives/d/root/delta?token=newtoken",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Verify items were upserted.
	assert.Len(t, store.upsertCalls, 2)
	assert.Equal(t, "item-1", store.upsertCalls[0].ItemID)
	assert.Equal(t, "docs/file1.txt", store.upsertCalls[0].Path)
	assert.Equal(t, ItemTypeFile, store.upsertCalls[0].ItemType)
	assert.Equal(t, "item-2", store.upsertCalls[1].ItemID)
	assert.Equal(t, ItemTypeFolder, store.upsertCalls[1].ItemType)

	// Verify token saved and delta marked complete.
	require.Len(t, store.savedTokens, 1)
	assert.Contains(t, store.savedTokens[0].Token, "newtoken")
	assert.True(t, store.deltaComplete)
}

func TestFetchAndApply_MultiPage(t *testing.T) {
	store := newMockStore()
	store.materializeResults["d/item-1"] = "file1.txt"
	store.materializeResults["d/item-2"] = "file2.txt"

	fetcher := newMockFetcher(
		&graph.DeltaPage{
			Items: []graph.Item{
				{
					ID: "item-1", Name: "file1.txt", DriveID: "d", ParentID: "root", Size: 50,
					ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			NextLink: "https://graph.microsoft.com/v1.0/drives/d/root/delta?token=page2",
		},
		&graph.DeltaPage{
			Items: []graph.Item{
				{
					ID: "item-2", Name: "file2.txt", DriveID: "d", ParentID: "root", Size: 75,
					ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			DeltaLink: "https://graph.microsoft.com/v1.0/drives/d/root/delta?token=final",
		},
	)

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Both items should be upserted.
	assert.Len(t, store.upsertCalls, 2)

	// Second call should use the nextLink as token.
	require.Len(t, fetcher.calls, 2)
	assert.Equal(t, "", fetcher.calls[0].Token)
	assert.Contains(t, fetcher.calls[1].Token, "page2")

	// Token saved from deltaLink.
	require.Len(t, store.savedTokens, 1)
	assert.Contains(t, store.savedTokens[0].Token, "final")
}

func TestFetchAndApply_EmptyDelta(t *testing.T) {
	store := newMockStore()
	fetcher := newMockFetcher(&graph.DeltaPage{
		Items:     []graph.Item{},
		DeltaLink: "https://graph.microsoft.com/v1.0/drives/d/root/delta?token=unchanged",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// No items upserted, but token still saved.
	assert.Empty(t, store.upsertCalls)
	require.Len(t, store.savedTokens, 1)
	assert.True(t, store.deltaComplete)
}

func TestFetchAndApply_DeletedItem_ExistsInStore(t *testing.T) {
	store := newMockStore()
	store.items["d/item-1"] = &Item{
		DriveID: "d", ItemID: "item-1", Name: "old.txt",
		ItemType: ItemTypeFile, IsDeleted: false,
	}

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{ID: "item-1", Name: "old.txt", DriveID: "d", ParentID: "root", IsDeleted: true},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Item should be marked deleted (tombstoned), not hard-deleted.
	require.Len(t, store.markDeletedCalls, 1)
	assert.Equal(t, "item-1", store.markDeletedCalls[0].ItemID)
	assert.Greater(t, store.markDeletedCalls[0].DeletedAt, int64(0))

	// No upsert — deletion is via MarkDeleted, not UpsertItem.
	assert.Empty(t, store.upsertCalls)
}

func TestFetchAndApply_DeletedItem_NotInStore(t *testing.T) {
	store := newMockStore()
	// No items in store — deletion of unknown item should be a no-op.

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{ID: "item-1", Name: "phantom.txt", DriveID: "d", ParentID: "root", IsDeleted: true},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// No calls to MarkDeleted or UpsertItem.
	assert.Empty(t, store.markDeletedCalls)
	assert.Empty(t, store.upsertCalls)
}

func TestFetchAndApply_DeletedItem_AlreadyTombstoned(t *testing.T) {
	store := newMockStore()
	deletedAt := NowNano()
	store.items["d/item-1"] = &Item{
		DriveID: "d", ItemID: "item-1", Name: "old.txt",
		ItemType: ItemTypeFile, IsDeleted: true, DeletedAt: Int64Ptr(deletedAt),
	}

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{ID: "item-1", Name: "old.txt", DriveID: "d", ParentID: "root", IsDeleted: true},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// No additional calls — already tombstoned.
	assert.Empty(t, store.markDeletedCalls)
	assert.Empty(t, store.upsertCalls)
}

func TestFetchAndApply_NewItem(t *testing.T) {
	store := newMockStore()
	// buildNewItemPath calls MaterializePath on the parent, then appends item name.
	store.materializeResults["d/docs-folder"] = "documents"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "item-1", Name: "report.docx", DriveID: "d", ParentID: "docs-folder",
				Size: 2048, QuickXorHash: "hash123", ETag: "etag1", CTag: "ctag1",
				ModifiedAt: time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	require.Len(t, store.upsertCalls, 1)
	item := store.upsertCalls[0]
	assert.Equal(t, "item-1", item.ItemID)
	assert.Equal(t, "d", item.DriveID)
	assert.Equal(t, "report.docx", item.Name)
	assert.Equal(t, "documents/report.docx", item.Path)
	assert.Equal(t, ItemTypeFile, item.ItemType)
	assert.Equal(t, "hash123", item.QuickXorHash)
	assert.Equal(t, "etag1", item.ETag)
	assert.Equal(t, "ctag1", item.CTag)
	require.NotNil(t, item.Size)
	assert.Equal(t, int64(2048), *item.Size)
	require.NotNil(t, item.RemoteMtime)
}

func TestFetchAndApply_Resurrection(t *testing.T) {
	store := newMockStore()
	deletedAt := NowNano()
	store.items["d/item-1"] = &Item{
		DriveID: "d", ItemID: "item-1", Name: "lazarus.txt",
		ItemType: ItemTypeFile, IsDeleted: true, DeletedAt: Int64Ptr(deletedAt),
		Path: "old/lazarus.txt",
	}
	store.materializeResults["d/item-1"] = "new/lazarus.txt"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "item-1", Name: "lazarus.txt", DriveID: "d", ParentID: "new-folder",
				Size: 512, QuickXorHash: "newhash",
				ModifiedAt: time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	require.Len(t, store.upsertCalls, 1)
	item := store.upsertCalls[0]
	assert.False(t, item.IsDeleted, "tombstone should be cleared")
	assert.Nil(t, item.DeletedAt, "DeletedAt should be nil after resurrection")
	assert.Equal(t, "new/lazarus.txt", item.Path)
	assert.Equal(t, "newhash", item.QuickXorHash)
}

func TestFetchAndApply_UpdateExisting(t *testing.T) {
	store := newMockStore()
	store.items["d/item-1"] = &Item{
		DriveID: "d", ItemID: "item-1", Name: "file.txt",
		ItemType: ItemTypeFile, ParentID: "root", Path: "file.txt",
		QuickXorHash: "oldhash", ETag: "old-etag",
	}

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "item-1", Name: "file.txt", DriveID: "d", ParentID: "root",
				Size: 999, QuickXorHash: "newhash", ETag: "new-etag",
				ModifiedAt: time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	require.Len(t, store.upsertCalls, 1)
	item := store.upsertCalls[0]
	assert.Equal(t, "newhash", item.QuickXorHash)
	assert.Equal(t, "new-etag", item.ETag)
	// Path unchanged because parent and name did not change.
	assert.Equal(t, "file.txt", item.Path)
}

func TestFetchAndApply_MoveRename(t *testing.T) {
	store := newMockStore()
	store.items["d/item-1"] = &Item{
		DriveID: "d", ItemID: "item-1", Name: "old-name.txt",
		ItemType: ItemTypeFile, ParentID: "folder-a", Path: "folder-a/old-name.txt",
	}
	store.materializeResults["d/item-1"] = "folder-b/new-name.txt"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "item-1", Name: "new-name.txt", DriveID: "d", ParentID: "folder-b",
				Size: 100, ModifiedAt: time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	require.Len(t, store.upsertCalls, 1)
	item := store.upsertCalls[0]
	assert.Equal(t, "new-name.txt", item.Name)
	assert.Equal(t, "folder-b", item.ParentID)
	assert.Equal(t, "folder-b/new-name.txt", item.Path)
}

func TestFetchAndApply_FolderMove_CascadesPath(t *testing.T) {
	store := newMockStore()
	store.items["d/folder-1"] = &Item{
		DriveID: "d", ItemID: "folder-1", Name: "myfolder",
		ItemType: ItemTypeFolder, ParentID: "root", Path: "myfolder",
	}
	store.materializeResults["d/folder-1"] = "archive/myfolder"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "folder-1", Name: "myfolder", DriveID: "d", ParentID: "archive-folder",
				IsFolder: true, ModifiedAt: time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Verify cascade was called with old and new path prefixes.
	require.Len(t, store.cascadeCalls, 1)
	assert.Equal(t, "myfolder", store.cascadeCalls[0].OldPrefix)
	assert.Equal(t, "archive/myfolder", store.cascadeCalls[0].NewPrefix)
}

func TestFetchAndApply_HTTP410_Recovery(t *testing.T) {
	store := newMockStore()
	store.deltaToken = "expired-token"
	store.materializeResults["d/item-1"] = "file.txt"

	// First call returns 410, subsequent calls succeed with fresh data.
	fetcher := &mockDeltaFetcher{
		pages: []*graph.DeltaPage{
			nil, // placeholder for the error call
			{
				Items: []graph.Item{
					{
						ID: "item-1", Name: "file.txt", DriveID: "d", ParentID: "root", Size: 42,
						ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
					},
				},
				DeltaLink: "token:fresh",
			},
		},
		errAtIdx: 0,
		err:      &graph.GraphError{StatusCode: 410, Err: graph.ErrGone, Message: "resyncRequired"},
	}

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Token should have been deleted then re-saved.
	assert.True(t, store.tokenDeleted, "expired token should be deleted")
	require.Len(t, store.savedTokens, 1)
	assert.Contains(t, store.savedTokens[0].Token, "fresh")

	// Delta complete should have been set false (during 410 handling) then true (after re-fetch).
	assert.True(t, store.deltaComplete)

	// Verify item was processed after re-enumeration.
	require.Len(t, store.upsertCalls, 1)
	assert.Equal(t, "item-1", store.upsertCalls[0].ItemID)
}

func TestFetchAndApply_BatchBoundary(t *testing.T) {
	store := newMockStore()

	// Create enough items to trigger a batch flush.
	// We use defaultBatchSize + 1 items across two pages.
	items := make([]graph.Item, defaultBatchSize+1)
	for i := range items {
		id := "item-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		items[i] = graph.Item{
			ID: id, Name: id + ".txt", DriveID: "d", ParentID: "root", Size: 10,
			ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		store.materializeResults["d/"+id] = id + ".txt"
	}

	// Split across two pages: first page has defaultBatchSize items, second has 1.
	fetcher := newMockFetcher(
		&graph.DeltaPage{
			Items:    items[:defaultBatchSize],
			NextLink: "page2",
		},
		&graph.DeltaPage{
			Items:     items[defaultBatchSize:],
			DeltaLink: "token:batched",
		},
	)

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// All items should be upserted.
	assert.Len(t, store.upsertCalls, defaultBatchSize+1)
	assert.True(t, store.deltaComplete)
}

func TestFetchAndApply_ExistingToken(t *testing.T) {
	store := newMockStore()
	store.deltaToken = "existing-token-123"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items:     []graph.Item{},
		DeltaLink: "token:incremental",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Verify the existing token was passed to the fetcher.
	require.Len(t, fetcher.calls, 1)
	assert.Equal(t, "existing-token-123", fetcher.calls[0].Token)
}

func TestFetchAndApply_FetchError(t *testing.T) {
	store := newMockStore()
	fetcher := &mockDeltaFetcher{
		errAtIdx: 0,
		err:      errors.New("network error"),
	}

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network error")

	// Token should NOT be saved on error.
	assert.Empty(t, store.savedTokens)
}

func TestFetchAndApply_SkipsPackages(t *testing.T) {
	store := newMockStore()
	store.materializeResults["d/file-1"] = "normal.txt"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{ID: "pkg-1", Name: "Notebook", DriveID: "d", ParentID: "root", IsPackage: true},
			{
				ID: "file-1", Name: "normal.txt", DriveID: "d", ParentID: "root", Size: 100,
				ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Only the regular file should be upserted, not the package.
	require.Len(t, store.upsertCalls, 1)
	assert.Equal(t, "file-1", store.upsertCalls[0].ItemID)
}

func TestFetchAndApply_DeltaCompleteness(t *testing.T) {
	tests := []struct {
		name     string
		pages    []*graph.DeltaPage
		wantErr  bool
		complete bool
	}{
		{
			name: "complete delta sets true",
			pages: []*graph.DeltaPage{
				{Items: []graph.Item{}, DeltaLink: "token:done"},
			},
			complete: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockStore()
			fetcher := newMockFetcher(tc.pages...)

			dp := NewDeltaProcessor(fetcher, store, testLogger(t))
			err := dp.FetchAndApply(context.Background(), "d")

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tc.complete, store.deltaComplete)
		})
	}
}

func TestConvertGraphItem_FieldMapping(t *testing.T) {
	mtime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	gItem := &graph.Item{
		ID:            "abc-123",
		Name:          "report.pdf",
		DriveID:       "drive-x",
		ParentID:      "parent-456",
		ParentDriveID: "drive-x",
		Size:          4096,
		ETag:          "etag-val",
		CTag:          "ctag-val",
		QuickXorHash:  "qxh-base64",
		SHA256Hash:    "sha256-hex",
		ModifiedAt:    mtime,
	}

	item := convertGraphItem(gItem, "fallback-drive")
	require.NotNil(t, item)

	assert.Equal(t, "abc-123", item.ItemID)
	assert.Equal(t, "report.pdf", item.Name)
	assert.Equal(t, "drive-x", item.DriveID, "should use graph item's DriveID when present")
	assert.Equal(t, "parent-456", item.ParentID)
	assert.Equal(t, "drive-x", item.ParentDriveID)
	assert.Equal(t, ItemTypeFile, item.ItemType)
	assert.Equal(t, "etag-val", item.ETag)
	assert.Equal(t, "ctag-val", item.CTag)
	assert.Equal(t, "qxh-base64", item.QuickXorHash)
	assert.Equal(t, "sha256-hex", item.SHA256Hash)
	require.NotNil(t, item.Size)
	assert.Equal(t, int64(4096), *item.Size)
	require.NotNil(t, item.RemoteMtime)
	assert.Equal(t, mtime.UnixNano(), *item.RemoteMtime)
	assert.Greater(t, item.CreatedAt, int64(0))
	assert.Greater(t, item.UpdatedAt, int64(0))
}

func TestConvertGraphItem_FallbackDriveID(t *testing.T) {
	gItem := &graph.Item{
		ID:       "item-1",
		Name:     "file.txt",
		ParentID: "root",
		// DriveID is empty — should use the fallback.
	}

	item := convertGraphItem(gItem, "my-drive")
	require.NotNil(t, item)
	assert.Equal(t, "my-drive", item.DriveID)
}

func TestConvertGraphItem_Folder(t *testing.T) {
	gItem := &graph.Item{
		ID:       "folder-1",
		Name:     "Documents",
		DriveID:  "d",
		ParentID: "root",
		IsFolder: true,
	}

	item := convertGraphItem(gItem, "d")
	require.NotNil(t, item)
	assert.Equal(t, ItemTypeFolder, item.ItemType)
}

func TestConvertGraphItem_RootFolder(t *testing.T) {
	gItem := &graph.Item{
		ID:       "root-id",
		Name:     "root",
		DriveID:  "d",
		ParentID: "",
		IsFolder: true,
		IsRoot:   true,
	}

	item := convertGraphItem(gItem, "d")
	require.NotNil(t, item)
	assert.Equal(t, ItemTypeRoot, item.ItemType, "root folder should be classified as ItemTypeRoot")
}

func TestConvertGraphItem_RootVsRegularFolder(t *testing.T) {
	// Root folder: both IsFolder and IsRoot are true.
	root := convertGraphItem(&graph.Item{
		ID: "root-id", Name: "root", DriveID: "d",
		IsFolder: true, IsRoot: true,
	}, "d")
	require.NotNil(t, root)
	assert.Equal(t, ItemTypeRoot, root.ItemType)

	// Regular folder: only IsFolder is true.
	folder := convertGraphItem(&graph.Item{
		ID: "folder-id", Name: "Documents", DriveID: "d", ParentID: "root-id",
		IsFolder: true, IsRoot: false,
	}, "d")
	require.NotNil(t, folder)
	assert.Equal(t, ItemTypeFolder, folder.ItemType)
}

func TestConvertGraphItem_Package_ReturnsNil(t *testing.T) {
	gItem := &graph.Item{
		ID:        "pkg-1",
		Name:      "Notebook",
		DriveID:   "d",
		ParentID:  "root",
		IsPackage: true,
	}

	item := convertGraphItem(gItem, "d")
	assert.Nil(t, item, "packages should be skipped")
}

func TestConvertGraphItem_DeletedItem_NilSize(t *testing.T) {
	gItem := &graph.Item{
		ID:        "item-1",
		Name:      "deleted.txt",
		DriveID:   "d",
		ParentID:  "root",
		IsDeleted: true,
		Size:      0, // Personal deleted items lack size.
	}

	item := convertGraphItem(gItem, "d")
	require.NotNil(t, item)
	assert.True(t, item.IsDeleted)
	assert.Nil(t, item.Size, "deleted item with zero size should have nil Size")
}

func TestConvertGraphItem_ZeroModifiedAt(t *testing.T) {
	gItem := &graph.Item{
		ID:       "item-1",
		Name:     "file.txt",
		DriveID:  "d",
		ParentID: "root",
		Size:     100,
		// ModifiedAt is zero time.
	}

	item := convertGraphItem(gItem, "d")
	require.NotNil(t, item)
	assert.Nil(t, item.RemoteMtime, "zero ModifiedAt should result in nil RemoteMtime")
}

// --- DriveID case normalization tests ---

// testCanonicalDriveID is the canonical (uppercase) drive ID used by config/engine.
const testCanonicalDriveID = "BD50CF43646E28E6"

func TestConvertGraphItem_DriveID_CaseNormalization(t *testing.T) {
	// Graph API returns lowercase DriveID for same-drive items.
	// The converter should use the canonical (caller-provided) ID.
	gItem := &graph.Item{
		ID:      "item-1",
		Name:    "file.txt",
		DriveID: "bd50cf43646e28e6", // lowercase from Graph API
		Size:    100,
	}

	item := convertGraphItem(gItem, testCanonicalDriveID)
	require.NotNil(t, item)
	assert.Equal(t, testCanonicalDriveID, item.DriveID,
		"same-drive items should use canonical DriveID, not API's lowercase version")
}

func TestConvertGraphItem_DriveID_CrossDrivePreserved(t *testing.T) {
	crossDriveID := "AABBCCDD11223344"

	// Cross-drive items (shared from a different drive) should keep the API's ID.
	gItem := &graph.Item{
		ID:      "item-1",
		Name:    "shared.txt",
		DriveID: crossDriveID, // different drive entirely
		Size:    100,
	}

	item := convertGraphItem(gItem, testCanonicalDriveID)
	require.NotNil(t, item)
	assert.Equal(t, crossDriveID, item.DriveID,
		"cross-drive items should preserve the API's DriveID")
}

func TestConvertGraphItem_DriveID_EmptyFallsBackToCanonical(t *testing.T) {
	// Items without a DriveID (common for same-drive delta responses)
	// should use the canonical ID.
	gItem := &graph.Item{
		ID:   "item-1",
		Name: "file.txt",
		Size: 100,
	}

	item := convertGraphItem(gItem, testCanonicalDriveID)
	require.NotNil(t, item)
	assert.Equal(t, testCanonicalDriveID, item.DriveID,
		"items without DriveID should use canonical ID")
}

// --- buildNewItemPath tests ---

func TestBuildNewItemPath_RootItem(t *testing.T) {
	store := newMockStore()
	dp := NewDeltaProcessor(nil, store, testLogger(t))

	item := &Item{DriveID: "d", ItemID: "root-id", Name: "root", ItemType: ItemTypeRoot}
	path, err := dp.buildNewItemPath(context.Background(), item)
	require.NoError(t, err)
	assert.Equal(t, "", path, "root items should have empty path")
}

func TestBuildNewItemPath_EmptyParentID(t *testing.T) {
	store := newMockStore()
	dp := NewDeltaProcessor(nil, store, testLogger(t))

	item := &Item{DriveID: "d", ItemID: "item-1", Name: "file.txt", ItemType: ItemTypeFile, ParentID: ""}
	path, err := dp.buildNewItemPath(context.Background(), item)
	require.NoError(t, err)
	assert.Equal(t, "file.txt", path, "items with no parent should use just their name")
}

func TestBuildNewItemPath_ParentIsRoot(t *testing.T) {
	store := newMockStore()
	// Root's MaterializePath returns "" (empty path).
	store.materializeResults["d/root-id"] = ""
	dp := NewDeltaProcessor(nil, store, testLogger(t))

	item := &Item{DriveID: "d", ItemID: "item-1", Name: "file.txt", ItemType: ItemTypeFile, ParentID: "root-id", ParentDriveID: "d"}
	path, err := dp.buildNewItemPath(context.Background(), item)
	require.NoError(t, err)
	assert.Equal(t, "file.txt", path, "child of root should have just the file name")
}

func TestBuildNewItemPath_NestedItem(t *testing.T) {
	store := newMockStore()
	store.materializeResults["d/folder-id"] = "Documents/Work"
	dp := NewDeltaProcessor(nil, store, testLogger(t))

	item := &Item{DriveID: "d", ItemID: "item-1", Name: "report.pdf", ItemType: ItemTypeFile, ParentID: "folder-id", ParentDriveID: "d"}
	path, err := dp.buildNewItemPath(context.Background(), item)
	require.NoError(t, err)
	assert.Equal(t, "Documents/Work/report.pdf", path)
}

func TestBuildNewItemPath_FallbackDriveID(t *testing.T) {
	store := newMockStore()
	store.materializeResults["d/parent-id"] = "shared"
	dp := NewDeltaProcessor(nil, store, testLogger(t))

	// ParentDriveID is empty — should fall back to item's own DriveID.
	item := &Item{DriveID: "d", ItemID: "item-1", Name: "file.txt", ItemType: ItemTypeFile, ParentID: "parent-id", ParentDriveID: ""}
	path, err := dp.buildNewItemPath(context.Background(), item)
	require.NoError(t, err)
	assert.Equal(t, "shared/file.txt", path)
}

func TestReorderDeletions(t *testing.T) {
	tests := []struct {
		name    string
		items   []*Item
		wantIDs []string
	}{
		{
			name:    "empty slice",
			items:   []*Item{},
			wantIDs: []string{},
		},
		{
			name: "all non-deleted",
			items: []*Item{
				{ItemID: "a", IsDeleted: false},
				{ItemID: "b", IsDeleted: false},
			},
			wantIDs: []string{"a", "b"},
		},
		{
			name: "all deleted",
			items: []*Item{
				{ItemID: "a", IsDeleted: true},
				{ItemID: "b", IsDeleted: true},
			},
			wantIDs: []string{"a", "b"},
		},
		{
			name: "mixed - deletions moved first",
			items: []*Item{
				{ItemID: "create-1", IsDeleted: false},
				{ItemID: "delete-1", IsDeleted: true},
				{ItemID: "create-2", IsDeleted: false},
				{ItemID: "delete-2", IsDeleted: true},
			},
			wantIDs: []string{"delete-1", "delete-2", "create-1", "create-2"},
		},
		{
			name: "already ordered",
			items: []*Item{
				{ItemID: "delete-1", IsDeleted: true},
				{ItemID: "create-1", IsDeleted: false},
			},
			wantIDs: []string{"delete-1", "create-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reorderDeletions(tc.items)

			gotIDs := make([]string, len(tc.items))
			for i, item := range tc.items {
				gotIDs[i] = item.ItemID
			}

			assert.Equal(t, tc.wantIDs, gotIDs)
		})
	}
}

func TestUpdateRemoteFields(t *testing.T) {
	existing := &Item{
		DriveID:      "d",
		ItemID:       "item-1",
		Name:         "old-name.txt",
		ParentID:     "old-parent",
		ItemType:     ItemTypeFile,
		QuickXorHash: "old-hash",
		ETag:         "old-etag",
		CTag:         "old-ctag",
		// Local fields should be preserved.
		LocalSize:  Int64Ptr(500),
		LocalMtime: Int64Ptr(1234567890),
		LocalHash:  "local-hash",
	}

	remoteMtime := int64(9999999999)
	incoming := &Item{
		Name:          "new-name.txt",
		ParentID:      "new-parent",
		ParentDriveID: "drive-y",
		ItemType:      ItemTypeFile,
		Size:          Int64Ptr(1024),
		QuickXorHash:  "new-hash",
		SHA256Hash:    "new-sha256",
		ETag:          "new-etag",
		CTag:          "new-ctag",
		RemoteMtime:   &remoteMtime,
	}

	updateRemoteFields(existing, incoming)

	// Remote fields should be updated.
	assert.Equal(t, "new-name.txt", existing.Name)
	assert.Equal(t, "new-parent", existing.ParentID)
	assert.Equal(t, "drive-y", existing.ParentDriveID)
	assert.Equal(t, "new-hash", existing.QuickXorHash)
	assert.Equal(t, "new-sha256", existing.SHA256Hash)
	assert.Equal(t, "new-etag", existing.ETag)
	assert.Equal(t, "new-ctag", existing.CTag)
	require.NotNil(t, existing.Size)
	assert.Equal(t, int64(1024), *existing.Size)
	require.NotNil(t, existing.RemoteMtime)
	assert.Equal(t, int64(9999999999), *existing.RemoteMtime)

	// Local fields should be preserved.
	require.NotNil(t, existing.LocalSize)
	assert.Equal(t, int64(500), *existing.LocalSize)
	require.NotNil(t, existing.LocalMtime)
	assert.Equal(t, int64(1234567890), *existing.LocalMtime)
	assert.Equal(t, "local-hash", existing.LocalHash)
}

func TestFetchAndApply_GetDeltaTokenError(t *testing.T) {
	store := newMockStore()
	store.getDeltaTokenErr = errors.New("db read error")

	fetcher := newMockFetcher()

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get delta token")
}

// --- Error path tests for handleFetchError, finalizeDelta, applyBatch ---

func TestHandleFetchError_DeleteTokenError(t *testing.T) {
	// When a 410 Gone arrives but DeleteDeltaToken fails, the error should propagate.
	store := newMockStore()
	store.deltaToken = "stale-token"
	store.deleteDeltaErr = errors.New("disk full")

	fetcher := &mockDeltaFetcher{
		errAtIdx: 0,
		err:      &graph.GraphError{StatusCode: 410, Err: graph.ErrGone, Message: "resyncRequired"},
	}

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete expired delta token")
	assert.Contains(t, err.Error(), "disk full")
}

func TestHandleFetchError_SetCompleteError(t *testing.T) {
	// When a 410 Gone arrives and token deletion succeeds but SetDeltaComplete fails,
	// the error should propagate.
	store := newMockStore()
	store.deltaToken = "expired-complete-token"
	store.setCompleteErr = errors.New("db locked")

	fetcher := &mockDeltaFetcher{
		errAtIdx: 0,
		err:      &graph.GraphError{StatusCode: 410, Err: graph.ErrGone, Message: "resyncRequired"},
	}

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set delta incomplete after 410")
	assert.Contains(t, err.Error(), "db locked")
}

func TestFinalizeDelta_FlushBatchError(t *testing.T) {
	// When the final page has items and the batch flush (UpsertItem) fails,
	// finalizeDelta should return the error.
	store := newMockStore()
	store.upsertErr = errors.New("constraint violation")
	store.materializeResults["d/item-1"] = "flush-err.txt"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "item-1", Name: "flush-err.txt", DriveID: "d", ParentID: "root", Size: 42,
				ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:final",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "constraint violation")
}

func TestFinalizeDelta_SaveTokenError(t *testing.T) {
	// When the delta completes with an empty final page but SaveDeltaToken fails,
	// finalizeDelta should return the error.
	store := newMockStore()
	store.saveDeltaTokenErr = errors.New("io error")

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items:     []graph.Item{},
		DeltaLink: "token:final",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "save delta token")
	assert.Contains(t, err.Error(), "io error")
}

func TestApplyBatch_ItemError(t *testing.T) {
	// When GetItem fails during applyDeltaItem, the error should propagate.
	store := newMockStore()
	store.getItemErr = errors.New("db corruption")
	store.materializeResults["d/item-1"] = "apply-err.txt"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			{
				ID: "item-1", Name: "apply-err.txt", DriveID: "d", ParentID: "root", Size: 42,
				ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db corruption")
}

func TestFetchAndApply_DeletionReorderedBeforeCreation(t *testing.T) {
	// Verify that when a deletion and creation arrive in the same page
	// (creation first, deletion second — the API bug), the deletion is
	// processed first, preventing the create-then-delete data loss.
	store := newMockStore()
	store.items["d/item-1"] = &Item{
		DriveID: "d", ItemID: "item-1", Name: "conflict.txt",
		ItemType: ItemTypeFile, ParentID: "root", Path: "conflict.txt",
	}
	store.materializeResults["d/item-2"] = "conflict.txt"

	fetcher := newMockFetcher(&graph.DeltaPage{
		Items: []graph.Item{
			// API delivers creation before deletion (the bug).
			{
				ID: "item-2", Name: "conflict.txt", DriveID: "d", ParentID: "root", Size: 100,
				ModifiedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			{ID: "item-1", Name: "conflict.txt", DriveID: "d", ParentID: "root", IsDeleted: true},
		},
		DeltaLink: "token:done",
	})

	dp := NewDeltaProcessor(fetcher, store, testLogger(t))
	err := dp.FetchAndApply(context.Background(), "d")
	require.NoError(t, err)

	// Deletion should be processed first (item-1 marked deleted),
	// then creation (item-2 upserted).
	require.Len(t, store.markDeletedCalls, 1)
	assert.Equal(t, "item-1", store.markDeletedCalls[0].ItemID)
	require.Len(t, store.upsertCalls, 1)
	assert.Equal(t, "item-2", store.upsertCalls[0].ItemID)
}
