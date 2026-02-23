package sync

import (
	"fmt"
	"sync"
	"testing"
)

// findPathChanges returns the PathChanges entry with the given path,
// or nil if not found.
func findPathChanges(changes []PathChanges, p string) *PathChanges {
	for i := range changes {
		if changes[i].Path == p {
			return &changes[i]
		}
	}

	return nil
}

func TestBuffer_AddSingle(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source:   SourceRemote,
		Type:     ChangeCreate,
		Path:     "buffer-notes.txt",
		Name:     "buffer-notes.txt",
		ItemID:   "buf-item-1",
		DriveID:  testDriveID,
		ItemType: ItemTypeFile,
		Size:     256,
		Hash:     "buf-hash-1",
	})

	result := buf.FlushImmediate()
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	pc := result[0]
	if pc.Path != "buffer-notes.txt" {
		t.Errorf("Path = %q, want %q", pc.Path, "buffer-notes.txt")
	}

	if len(pc.RemoteEvents) != 1 {
		t.Fatalf("len(RemoteEvents) = %d, want 1", len(pc.RemoteEvents))
	}

	if pc.RemoteEvents[0].Type != ChangeCreate {
		t.Errorf("RemoteEvents[0].Type = %v, want ChangeCreate", pc.RemoteEvents[0].Type)
	}

	if len(pc.LocalEvents) != 0 {
		t.Errorf("len(LocalEvents) = %d, want 0", len(pc.LocalEvents))
	}
}

func TestBuffer_AddMultiplePaths(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "buffer-alpha.txt", Name: "buffer-alpha.txt",
		ItemID: "buf-a1", DriveID: testDriveID, ItemType: ItemTypeFile,
	})
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "buffer-beta.csv", Name: "buffer-beta.csv",
		ItemID: "", DriveID: "", ItemType: ItemTypeFile,
	})

	result := buf.FlushImmediate()
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}

	alpha := findPathChanges(result, "buffer-alpha.txt")
	if alpha == nil {
		t.Fatal("buffer-alpha.txt not found")
	}

	if len(alpha.RemoteEvents) != 1 {
		t.Errorf("alpha RemoteEvents = %d, want 1", len(alpha.RemoteEvents))
	}

	beta := findPathChanges(result, "buffer-beta.csv")
	if beta == nil {
		t.Fatal("buffer-beta.csv not found")
	}

	if len(beta.LocalEvents) != 1 {
		t.Errorf("beta LocalEvents = %d, want 1", len(beta.LocalEvents))
	}
}

func TestBuffer_AddSamePathRemote(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "docs/buffer-report.pdf", Name: "buffer-report.pdf",
		ItemID: "buf-r1", DriveID: testDriveID, ItemType: ItemTypeFile,
	})
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeModify,
		Path: "docs/buffer-report.pdf", Name: "buffer-report.pdf",
		ItemID: "buf-r1", DriveID: testDriveID, ItemType: ItemTypeFile,
	})

	result := buf.FlushImmediate()
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	pc := result[0]
	if len(pc.RemoteEvents) != 2 {
		t.Errorf("RemoteEvents = %d, want 2", len(pc.RemoteEvents))
	}

	if pc.RemoteEvents[0].Type != ChangeCreate {
		t.Errorf("RemoteEvents[0].Type = %v, want ChangeCreate", pc.RemoteEvents[0].Type)
	}

	if pc.RemoteEvents[1].Type != ChangeModify {
		t.Errorf("RemoteEvents[1].Type = %v, want ChangeModify", pc.RemoteEvents[1].Type)
	}
}

func TestBuffer_AddSamePathLocal(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate,
		Path: "buffer-localdata.txt", Name: "buffer-localdata.txt",
		ItemType: ItemTypeFile, Size: 100,
	})
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "buffer-localdata.txt", Name: "buffer-localdata.txt",
		ItemType: ItemTypeFile, Size: 200,
	})

	result := buf.FlushImmediate()
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	if len(result[0].LocalEvents) != 2 {
		t.Errorf("LocalEvents = %d, want 2", len(result[0].LocalEvents))
	}
}

func TestBuffer_AddMixedSources(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeModify,
		Path: "buffer-shared.docx", Name: "buffer-shared.docx",
		ItemID: "buf-s1", DriveID: testDriveID, ItemType: ItemTypeFile,
	})
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "buffer-shared.docx", Name: "buffer-shared.docx",
		ItemType: ItemTypeFile,
	})

	result := buf.FlushImmediate()
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	pc := result[0]
	if len(pc.RemoteEvents) != 1 {
		t.Errorf("RemoteEvents = %d, want 1", len(pc.RemoteEvents))
	}

	if len(pc.LocalEvents) != 1 {
		t.Errorf("LocalEvents = %d, want 1", len(pc.LocalEvents))
	}
}

func TestBuffer_MoveDualKeying(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source:   SourceRemote,
		Type:     ChangeMove,
		Path:     "buffer-new-folder/moved.txt",
		OldPath:  "buffer-old-folder/moved.txt",
		Name:     "moved.txt",
		ItemID:   "buf-m1",
		DriveID:  testDriveID,
		ItemType: ItemTypeFile,
	})

	result := buf.FlushImmediate()

	// Two paths: the new location (move) and old location (synthetic delete).
	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}

	newPC := findPathChanges(result, "buffer-new-folder/moved.txt")
	if newPC == nil {
		t.Fatal("new path not found")
	}

	if len(newPC.RemoteEvents) != 1 {
		t.Fatalf("new path RemoteEvents = %d, want 1", len(newPC.RemoteEvents))
	}

	if newPC.RemoteEvents[0].Type != ChangeMove {
		t.Errorf("new path event Type = %v, want ChangeMove", newPC.RemoteEvents[0].Type)
	}

	oldPC := findPathChanges(result, "buffer-old-folder/moved.txt")
	if oldPC == nil {
		t.Fatal("old path not found")
	}

	if len(oldPC.RemoteEvents) != 1 {
		t.Fatalf("old path RemoteEvents = %d, want 1", len(oldPC.RemoteEvents))
	}

	synth := oldPC.RemoteEvents[0]
	if synth.Type != ChangeDelete {
		t.Errorf("synthetic Type = %v, want ChangeDelete", synth.Type)
	}

	if !synth.IsDeleted {
		t.Error("synthetic IsDeleted = false, want true")
	}

	if synth.ItemID != "buf-m1" {
		t.Errorf("synthetic ItemID = %q, want %q", synth.ItemID, "buf-m1")
	}

	if synth.Name != "moved.txt" {
		t.Errorf("synthetic Name = %q, want %q", synth.Name, "moved.txt")
	}

	if synth.DriveID != testDriveID {
		t.Errorf("synthetic DriveID = %q, want %q", synth.DriveID, testDriveID)
	}

	if synth.ItemType != ItemTypeFile {
		t.Errorf("synthetic ItemType = %v, want ItemTypeFile", synth.ItemType)
	}
}

func TestBuffer_MoveNoOldPath(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source:   SourceRemote,
		Type:     ChangeMove,
		Path:     "buffer-dest/file.txt",
		OldPath:  "", // no old path known
		Name:     "file.txt",
		ItemID:   "buf-no-old-1",
		DriveID:  testDriveID,
		ItemType: ItemTypeFile,
	})

	result := buf.FlushImmediate()

	// Only the new path — no synthetic delete because OldPath is empty.
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	if result[0].Path != "buffer-dest/file.txt" {
		t.Errorf("Path = %q, want %q", result[0].Path, "buffer-dest/file.txt")
	}
}

func TestBuffer_AddAll(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	events := []ChangeEvent{
		{
			Source: SourceRemote, Type: ChangeCreate,
			Path: "buffer-batch-a.txt", Name: "buffer-batch-a.txt",
			ItemID: "buf-ba1", DriveID: testDriveID, ItemType: ItemTypeFile,
		},
		{
			Source: SourceLocal, Type: ChangeCreate,
			Path: "buffer-batch-b.txt", Name: "buffer-batch-b.txt",
			ItemType: ItemTypeFile,
		},
		{
			Source: SourceRemote, Type: ChangeModify,
			Path: "buffer-batch-a.txt", Name: "buffer-batch-a.txt",
			ItemID: "buf-ba1", DriveID: testDriveID, ItemType: ItemTypeFile,
		},
	}

	buf.AddAll(events)
	result := buf.FlushImmediate()

	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}

	a := findPathChanges(result, "buffer-batch-a.txt")
	if a == nil {
		t.Fatal("buffer-batch-a.txt not found")
	}

	if len(a.RemoteEvents) != 2 {
		t.Errorf("buffer-batch-a.txt RemoteEvents = %d, want 2", len(a.RemoteEvents))
	}

	b := findPathChanges(result, "buffer-batch-b.txt")
	if b == nil {
		t.Fatal("buffer-batch-b.txt not found")
	}

	if len(b.LocalEvents) != 1 {
		t.Errorf("buffer-batch-b.txt LocalEvents = %d, want 1", len(b.LocalEvents))
	}
}

func TestBuffer_AddAllWithMoves(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	events := []ChangeEvent{
		{
			Source: SourceLocal, Type: ChangeMove,
			Path: "buffer-mv-new/data.json", OldPath: "buffer-mv-old/data.json",
			Name: "data.json", ItemType: ItemTypeFile,
		},
		{
			Source: SourceRemote, Type: ChangeCreate,
			Path: "buffer-mv-unrelated.txt", Name: "buffer-mv-unrelated.txt",
			ItemID: "buf-unr1", DriveID: testDriveID, ItemType: ItemTypeFile,
		},
	}

	buf.AddAll(events)
	result := buf.FlushImmediate()

	// 3 paths: new path, old path (synthetic delete), unrelated.
	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(result))
	}

	oldPC := findPathChanges(result, "buffer-mv-old/data.json")
	if oldPC == nil {
		t.Fatal("old path not found for move in batch")
	}

	if len(oldPC.LocalEvents) != 1 {
		t.Fatalf("old path LocalEvents = %d, want 1", len(oldPC.LocalEvents))
	}

	if oldPC.LocalEvents[0].Type != ChangeDelete {
		t.Errorf("old path Type = %v, want ChangeDelete", oldPC.LocalEvents[0].Type)
	}
}

func TestBuffer_FlushEmpty(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	result := buf.FlushImmediate()

	if result != nil {
		t.Errorf("FlushImmediate() = %v, want nil", result)
	}
}

func TestBuffer_FlushClears(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "buffer-cleartest.txt", Name: "buffer-cleartest.txt",
		ItemID: "buf-ct1", DriveID: testDriveID, ItemType: ItemTypeFile,
	})

	first := buf.FlushImmediate()
	if len(first) != 1 {
		t.Fatalf("first flush: len = %d, want 1", len(first))
	}

	second := buf.FlushImmediate()
	if second != nil {
		t.Errorf("second flush: got %v, want nil", second)
	}

	if buf.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after flush", buf.Len())
	}
}

func TestBuffer_FlushSorted(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	// Add in reverse alphabetical order.
	paths := []string{
		"zzz/buffer-last.txt",
		"mmm/buffer-middle.txt",
		"aaa/buffer-first.txt",
	}

	for _, p := range paths {
		buf.Add(&ChangeEvent{
			Source: SourceRemote, Type: ChangeCreate,
			Path: p, Name: p,
			ItemID: "buf-sort-" + p, DriveID: testDriveID, ItemType: ItemTypeFile,
		})
	}

	result := buf.FlushImmediate()
	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(result))
	}

	if result[0].Path != "aaa/buffer-first.txt" {
		t.Errorf("result[0].Path = %q, want %q", result[0].Path, "aaa/buffer-first.txt")
	}

	if result[1].Path != "mmm/buffer-middle.txt" {
		t.Errorf("result[1].Path = %q, want %q", result[1].Path, "mmm/buffer-middle.txt")
	}

	if result[2].Path != "zzz/buffer-last.txt" {
		t.Errorf("result[2].Path = %q, want %q", result[2].Path, "zzz/buffer-last.txt")
	}
}

func TestBuffer_Len(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))

	if buf.Len() != 0 {
		t.Errorf("initial Len() = %d, want 0", buf.Len())
	}

	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "buffer-len-one.txt", Name: "buffer-len-one.txt",
		ItemID: "buf-l1", DriveID: testDriveID, ItemType: ItemTypeFile,
	})

	if buf.Len() != 1 {
		t.Errorf("Len() after 1 add = %d, want 1", buf.Len())
	}

	// Same path again — Len should not increase.
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "buffer-len-one.txt", Name: "buffer-len-one.txt",
		ItemType: ItemTypeFile,
	})

	if buf.Len() != 1 {
		t.Errorf("Len() after same-path add = %d, want 1", buf.Len())
	}

	// Different path.
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate,
		Path: "buffer-len-two.txt", Name: "buffer-len-two.txt",
		ItemType: ItemTypeFile,
	})

	if buf.Len() != 2 {
		t.Errorf("Len() after new-path add = %d, want 2", buf.Len())
	}
}

func TestBuffer_ThreadSafety_DifferentPaths(t *testing.T) {
	// Exercises concurrent map insertions to distinct paths (the existing
	// TestBuffer_ThreadSafety only tests concurrent appends to one path).
	t.Parallel()

	buf := NewBuffer(testLogger(t))

	goroutines := 10
	eventsPerGoroutine := 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for g := range goroutines {
		go func(id int) {
			defer wg.Done()

			for i := range eventsPerGoroutine {
				// Path includes goroutine ID and event index for uniqueness.
				p := fmt.Sprintf("buffer-mt-g%d-e%d.txt", id, i)
				buf.Add(&ChangeEvent{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     p,
					Name:     p,
					ItemType: ItemTypeFile,
					Size:     int64(id*eventsPerGoroutine + i),
				})
			}
		}(g)
	}

	wg.Wait()

	result := buf.FlushImmediate()

	// Each goroutine adds 50 events to unique paths (goroutine ID + event index).
	wantPaths := goroutines * eventsPerGoroutine
	if len(result) != wantPaths {
		t.Errorf("len(result) = %d, want %d", len(result), wantPaths)
	}

	// Validate each path has exactly 1 event.
	for _, pc := range result {
		total := len(pc.RemoteEvents) + len(pc.LocalEvents)
		if total != 1 {
			t.Errorf("path %q has %d events, want 1", pc.Path, total)
		}
	}
}

func TestBuffer_ThreadSafety(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	goroutines := 20
	eventsPerGoroutine := 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for g := range goroutines {
		go func(id int) {
			defer wg.Done()

			for i := range eventsPerGoroutine {
				// Half remote, half local — mix sources.
				source := SourceRemote
				if i%2 == 0 {
					source = SourceLocal
				}

				buf.Add(&ChangeEvent{
					Source:   source,
					Type:     ChangeModify,
					Path:     "buffer-concurrent.dat",
					Name:     "buffer-concurrent.dat",
					ItemType: ItemTypeFile,
					Size:     int64(id*eventsPerGoroutine + i),
				})
			}
		}(g)
	}

	wg.Wait()

	// Verify buffer state is consistent.
	if buf.Len() != 1 {
		t.Errorf("Len() = %d, want 1 (all events target same path)", buf.Len())
	}

	result := buf.FlushImmediate()
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	totalEvents := len(result[0].RemoteEvents) + len(result[0].LocalEvents)
	wantTotal := goroutines * eventsPerGoroutine

	if totalEvents != wantTotal {
		t.Errorf("total events = %d, want %d", totalEvents, wantTotal)
	}
}
