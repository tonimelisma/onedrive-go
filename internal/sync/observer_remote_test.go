package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, token, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if token != "https://graph.microsoft.com/delta?token=new-token" {
		t.Errorf("token = %q, want delta link URL", token)
	}

	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}

	// First file.
	e := events[0]
	if e.Type != ChangeCreate {
		t.Errorf("events[0].Type = %v, want ChangeCreate", e.Type)
	}

	if e.Path != "hello.txt" {
		t.Errorf("events[0].Path = %q, want %q", e.Path, "hello.txt")
	}

	if e.Hash != "qxh1" {
		t.Errorf("events[0].Hash = %q, want %q", e.Hash, "qxh1")
	}

	if e.Size != 100 {
		t.Errorf("events[0].Size = %d, want 100", e.Size)
	}

	if e.Source != SourceRemote {
		t.Errorf("events[0].Source = %v, want SourceRemote", e.Source)
	}

	// Second file.
	if events[1].Name != "world.txt" {
		t.Errorf("events[1].Name = %q, want %q", events[1].Name, "world.txt")
	}
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

	obs := NewRemoteObserver(fetcher, baseline, testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "prev-token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	// Root and folder produce events too (folder is classified).
	// Find the file event.
	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			fileEvent = &events[i]

			break
		}
	}

	if fileEvent == nil {
		t.Fatal("file event not found")
	}

	if fileEvent.Type != ChangeModify {
		t.Errorf("Type = %v, want ChangeModify", fileEvent.Type)
	}

	if fileEvent.Path != "docs/readme.md" {
		t.Errorf("Path = %q, want %q", fileEvent.Path, "docs/readme.md")
	}

	if fileEvent.Hash != "new-hash" {
		t.Errorf("Hash = %q, want %q", fileEvent.Hash, "new-hash")
	}
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

	obs := NewRemoteObserver(fetcher, baseline, testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	e := events[0]
	if e.Type != ChangeDelete {
		t.Errorf("Type = %v, want ChangeDelete", e.Type)
	}

	if !e.IsDeleted {
		t.Error("IsDeleted = false, want true")
	}

	if e.Path != "photos/cat.jpg" {
		t.Errorf("Path = %q, want %q", e.Path, "photos/cat.jpg")
	}
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

	obs := NewRemoteObserver(fetcher, baseline, testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	var moveEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			moveEvent = &events[i]

			break
		}
	}

	if moveEvent == nil {
		t.Fatal("move event not found")
	}

	if moveEvent.Type != ChangeMove {
		t.Errorf("Type = %v, want ChangeMove", moveEvent.Type)
	}

	if moveEvent.Path != "new-folder/doc.txt" {
		t.Errorf("Path = %q, want %q", moveEvent.Path, "new-folder/doc.txt")
	}

	if moveEvent.OldPath != "old-folder/doc.txt" {
		t.Errorf("OldPath = %q, want %q", moveEvent.OldPath, "old-folder/doc.txt")
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, token, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}

	if token != "final-delta-link" {
		t.Errorf("token = %q, want %q", token, "final-delta-link")
	}

	if fetcher.calls != 2 {
		t.Errorf("fetcher.calls = %d, want 2", fetcher.calls)
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, token, err := obs.FullDelta(context.Background(), "prev-token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}

	if token != "empty-delta-link" {
		t.Errorf("token = %q, want %q", token, "empty-delta-link")
	}
}

func TestFullDelta_DeltaExpired(t *testing.T) {
	t.Parallel()

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: graph.ErrGone,
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	_, _, err := obs.FullDelta(context.Background(), "expired-token")

	if !errors.Is(err, ErrDeltaExpired) {
		t.Errorf("err = %v, want ErrDeltaExpired", err)
	}
}

func TestFullDelta_FetchError(t *testing.T) {
	t.Parallel()

	fetchErr := errors.New("network timeout")
	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: fetchErr,
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	_, _, err := obs.FullDelta(context.Background(), "token")

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, fetchErr) {
		t.Errorf("err = %v, want wrapped %v", err, fetchErr)
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0 (root should be skipped)", len(events))
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	// Find file event.
	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			fileEvent = &events[i]

			break
		}
	}

	if fileEvent == nil {
		t.Fatal("file event not found")
	}

	if fileEvent.Path != "Documents/report.pdf" {
		t.Errorf("Path = %q, want %q", fileEvent.Path, "Documents/report.pdf")
	}
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

	obs := NewRemoteObserver(fetcher, baseline, testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].Path != "Projects/GoApp/main.go" {
		t.Errorf("Path = %q, want %q", events[0].Path, "Projects/GoApp/main.go")
	}
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

	obs := NewRemoteObserver(fetcher, baseline, testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	var fileEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			fileEvent = &events[i]

			break
		}
	}

	if fileEvent == nil {
		t.Fatal("file event not found")
	}

	if fileEvent.Path != "Documents/Reports/q1.pdf" {
		t.Errorf("Path = %q, want %q", fileEvent.Path, "Documents/Reports/q1.pdf")
	}
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

	obs := NewRemoteObserver(fetcher, baseline, testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].Name != "budget.xlsx" {
		t.Errorf("Name = %q, want %q (recovered from baseline)", events[0].Name, "budget.xlsx")
	}

	if events[0].Path != "work/budget.xlsx" {
		t.Errorf("Path = %q, want %q", events[0].Path, "work/budget.xlsx")
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), rawDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	want := driveid.New("0abc123def456789") // lowercased + zero-padded to 16
	if !events[0].DriveID.Equal(want) {
		t.Errorf("DriveID = %q, want %q", events[0].DriveID, want)
	}

	// Verify the fetcher was called with the normalized driveID, not the raw one.
	if len(fetcher.driveIDs) != 1 {
		t.Fatalf("fetcher called %d times, want 1", len(fetcher.driveIDs))
	}

	if !fetcher.driveIDs[0].Equal(want) {
		t.Errorf("fetcher received driveID = %q, want %q (normalized)", fetcher.driveIDs[0], want)
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].Name != nfc {
		t.Errorf("Name = %q, want %q (NFC-normalized)", events[0].Name, nfc)
	}

	if events[0].Path != nfc {
		t.Errorf("Path = %q, want %q (NFC-normalized)", events[0].Path, nfc)
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}

	if events[0].Hash != "qxh" {
		t.Errorf("events[0].Hash = %q, want %q (QuickXorHash preferred)", events[0].Hash, "qxh")
	}

	if events[1].Hash != "sha-only" {
		t.Errorf("events[1].Hash = %q, want %q (SHA256 fallback)", events[1].Hash, "sha-only")
	}

	if events[2].Hash != "" {
		t.Errorf("events[2].Hash = %q, want empty", events[2].Hash)
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if events[0].Mtime != ts.UnixNano() {
		t.Errorf("Mtime = %d, want %d", events[0].Mtime, ts.UnixNano())
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if events[0].ItemType != ItemTypeFolder {
		t.Errorf("ItemType = %v, want ItemTypeFolder", events[0].ItemType)
	}

	if events[0].Hash != "" {
		t.Errorf("Hash = %q, want empty (folders have no hash)", events[0].Hash)
	}
}

func TestFullDelta_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	fetcher := &mockDeltaFetcher{
		pages: []mockDeltaPage{{
			err: ctx.Err(),
		}},
	}

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	_, _, err := obs.FullDelta(ctx, "token")

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
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
		{"empty", "", "0000000000000000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := driveid.New(tt.in)
			if got.String() != tt.want {
				t.Errorf("driveid.New(%q) = %q, want %q", tt.in, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("classifyItemType() = %v, want %v", got, tt.want)
			}
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

			got := selectHash(&tt.item)
			if got != tt.want {
				t.Errorf("selectHash() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToUnixNano(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := toUnixNano(ts); got != ts.UnixNano() {
		t.Errorf("toUnixNano(ts) = %d, want %d", got, ts.UnixNano())
	}

	if got := toUnixNano(time.Time{}); got != 0 {
		t.Errorf("toUnixNano(zero) = %d, want 0", got)
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	// Orphaned item gets a partial path: just its own name.
	if events[0].Path != "orphan.txt" {
		t.Errorf("Path = %q, want %q (partial path for orphan)", events[0].Path, "orphan.txt")
	}

	if events[0].Type != ChangeCreate {
		t.Errorf("Type = %v, want ChangeCreate", events[0].Type)
	}
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

	obs := NewRemoteObserver(fetcher, emptyBaseline(), testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	e := events[0]
	if e.Type != ChangeDelete {
		t.Errorf("Type = %v, want ChangeDelete", e.Type)
	}

	// No baseline entry — path is empty.
	if e.Path != "" {
		t.Errorf("Path = %q, want empty (no baseline entry)", e.Path)
	}

	// Name is still available from the delta item itself.
	if e.Name != "ephemeral.txt" {
		t.Errorf("Name = %q, want %q", e.Name, "ephemeral.txt")
	}
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

	obs := NewRemoteObserver(fetcher, baseline, testDriveID, testLogger(t))
	events, _, err := obs.FullDelta(context.Background(), "token")
	if err != nil {
		t.Fatalf("FullDelta: %v", err)
	}

	var renameEvent *ChangeEvent
	for i := range events {
		if events[i].ItemID == "f1" {
			renameEvent = &events[i]

			break
		}
	}

	if renameEvent == nil {
		t.Fatal("rename event not found")
	}

	if renameEvent.Type != ChangeMove {
		t.Errorf("Type = %v, want ChangeMove", renameEvent.Type)
	}

	if renameEvent.Path != "docs/new-name.txt" {
		t.Errorf("Path = %q, want %q", renameEvent.Path, "docs/new-name.txt")
	}

	if renameEvent.OldPath != "docs/old-name.txt" {
		t.Errorf("OldPath = %q, want %q", renameEvent.OldPath, "docs/old-name.txt")
	}

	if renameEvent.Name != "new-name.txt" {
		t.Errorf("Name = %q, want %q", renameEvent.Name, "new-name.txt")
	}
}
