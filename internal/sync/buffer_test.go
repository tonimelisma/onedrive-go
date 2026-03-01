package sync

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
		DriveID:  driveid.New(testDriveID),
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
		ItemID: "buf-a1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "buffer-beta.csv", Name: "buffer-beta.csv",
		ItemID: "", ItemType: ItemTypeFile,
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
		ItemID: "buf-r1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeModify,
		Path: "docs/buffer-report.pdf", Name: "buffer-report.pdf",
		ItemID: "buf-r1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
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
		ItemID: "buf-s1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
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
		ParentID: "buf-parent-1",
		DriveID:  driveid.New(testDriveID),
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

	if !synth.DriveID.Equal(driveid.New(testDriveID)) {
		t.Errorf("synthetic DriveID = %q, want %q", synth.DriveID, driveid.New(testDriveID))
	}

	if synth.ParentID != "buf-parent-1" {
		t.Errorf("synthetic ParentID = %q, want %q", synth.ParentID, "buf-parent-1")
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
		DriveID:  driveid.New(testDriveID),
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
			ItemID: "buf-ba1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
		},
		{
			Source: SourceLocal, Type: ChangeCreate,
			Path: "buffer-batch-b.txt", Name: "buffer-batch-b.txt",
			ItemType: ItemTypeFile,
		},
		{
			Source: SourceRemote, Type: ChangeModify,
			Path: "buffer-batch-a.txt", Name: "buffer-batch-a.txt",
			ItemID: "buf-ba1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
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
			ItemID: "buf-unr1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
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
		ItemID: "buf-ct1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
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
			ItemID: "buf-sort-" + p, DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
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
		ItemID: "buf-l1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
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

// ---------------------------------------------------------------------------
// FlushDebounced tests
// ---------------------------------------------------------------------------

func TestFlushDebounced_SingleBatch(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())

	debounce := 50 * time.Millisecond
	out := buf.FlushDebounced(ctx, debounce)

	// Add events, then wait for the debounce to flush.
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "debounce-a.txt", Name: "debounce-a.txt",
		ItemID: "d1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate,
		Path: "debounce-b.txt", Name: "debounce-b.txt",
		ItemType: ItemTypeFile,
	})

	select {
	case batch := <-out:
		if len(batch) != 2 {
			t.Errorf("batch len = %d, want 2", len(batch))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for debounced batch")
	}

	cancel()
	// Drain channel to ensure debounce goroutine exits before test ends.
	for range out {
	}
}

func TestFlushDebounced_MultipleWaves(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())

	debounce := 50 * time.Millisecond
	out := buf.FlushDebounced(ctx, debounce)

	// Wave 1.
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "wave1.txt", Name: "wave1.txt",
		ItemID: "w1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})

	select {
	case batch := <-out:
		if len(batch) != 1 {
			t.Errorf("wave 1 batch len = %d, want 1", len(batch))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for wave 1")
	}

	// Wave 2.
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "wave2.txt", Name: "wave2.txt",
		ItemType: ItemTypeFile,
	})

	select {
	case batch := <-out:
		if len(batch) != 1 {
			t.Errorf("wave 2 batch len = %d, want 1", len(batch))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for wave 2")
	}

	cancel()
	for range out {
	}
}

func TestFlushDebounced_DebounceResets(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())

	debounce := 100 * time.Millisecond
	out := buf.FlushDebounced(ctx, debounce)

	// Add first event.
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "reset-first.txt", Name: "reset-first.txt",
		ItemID: "r1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})

	// Wait less than debounce, then add second event — timer should reset.
	time.Sleep(50 * time.Millisecond)
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate,
		Path: "reset-second.txt", Name: "reset-second.txt",
		ItemType: ItemTypeFile,
	})

	// Both events should arrive in a single batch.
	select {
	case batch := <-out:
		if len(batch) != 2 {
			t.Errorf("batch len = %d, want 2 (debounce should have combined)", len(batch))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for debounced batch")
	}

	cancel()
	for range out {
	}
}

func TestFlushDebounced_ContextCancel(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())

	debounce := time.Hour // never fires naturally
	out := buf.FlushDebounced(ctx, debounce)

	// Add an event, then cancel before debounce expires.
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "cancel-drain.txt", Name: "cancel-drain.txt",
		ItemID: "cd1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})

	time.Sleep(20 * time.Millisecond) // let the signal propagate
	cancel()

	// The final drain should deliver the event.
	var gotBatch bool

	for batch := range out {
		if len(batch) > 0 {
			gotBatch = true
		}
	}

	if !gotBatch {
		t.Error("expected a final drain batch on context cancel")
	}
}

func TestFlushDebounced_ConcurrentAddAndFlush(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())

	debounce := 30 * time.Millisecond
	out := buf.FlushDebounced(ctx, debounce)

	// Concurrently add events from multiple goroutines.
	var wg sync.WaitGroup
	goroutines := 5
	eventsPerGoroutine := 10

	wg.Add(goroutines)

	for g := range goroutines {
		go func(id int) {
			defer wg.Done()

			for i := range eventsPerGoroutine {
				buf.Add(&ChangeEvent{
					Source:   SourceRemote,
					Type:     ChangeCreate,
					Path:     fmt.Sprintf("concurrent-g%d-e%d.txt", id, i),
					Name:     fmt.Sprintf("concurrent-g%d-e%d.txt", id, i),
					ItemType: ItemTypeFile,
				})
			}
		}(g)
	}

	wg.Wait()

	// Collect all batches until we have all events.
	totalPaths := 0
	timeout := time.After(5 * time.Second)

	for totalPaths < goroutines*eventsPerGoroutine {
		select {
		case batch := <-out:
			totalPaths += len(batch)
		case <-timeout:
			t.Fatalf("timeout: got %d paths, want %d", totalPaths, goroutines*eventsPerGoroutine)
		}
	}

	cancel()
	for range out {
	}
}

// TestBuffer_FlushDebounced_FinalDrainNoDeadlock verifies that canceling the
// context while the output channel is full does NOT deadlock. Before the B-103
// fix, the blocking `out <- batch` in debounceLoop would hang because the
// consumer had already stopped reading. The goroutine must exit within 5s.
func TestBuffer_FlushDebounced_FinalDrainNoDeadlock(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())

	debounce := time.Hour // never fires naturally
	out := buf.FlushDebounced(ctx, debounce)

	// Add an event so the final drain has something to send.
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "deadlock-test.txt", Name: "deadlock-test.txt",
		ItemID: "dl1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})

	time.Sleep(10 * time.Millisecond) // let the signal propagate

	// Cancel context — the debounce goroutine should exit via the
	// non-blocking send, discarding the final batch if the channel is full.
	cancel()

	// The goroutine must exit (channel closed) within 5 seconds.
	// If the old blocking send were still in place, this would deadlock
	// when the output channel (capacity 1) was already occupied.
	done := make(chan struct{})
	go func() {
		for range out {
		}
		close(done)
	}()

	select {
	case <-done:
		// Success — goroutine exited cleanly.
	case <-time.After(5 * time.Second):
		t.Fatal("debounce goroutine deadlocked on final drain (B-103)")
	}
}

// ---------------------------------------------------------------------------
// Buffer max paths tests (B-126)
// ---------------------------------------------------------------------------

func TestBuffer_MaxPaths_DropsNewPaths(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.SetMaxPaths(2)

	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "cap-a.txt", Name: "cap-a.txt", ItemType: ItemTypeFile,
	})
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "cap-b.txt", Name: "cap-b.txt", ItemType: ItemTypeFile,
	})

	// Third path should be dropped.
	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "cap-c.txt", Name: "cap-c.txt", ItemType: ItemTypeFile,
	})

	result := buf.FlushImmediate()
	if len(result) != 2 {
		t.Errorf("len(result) = %d, want 2 (third path should be dropped)", len(result))
	}
}

func TestBuffer_MaxPaths_AllowsExistingPaths(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.SetMaxPaths(1)

	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "only.txt", Name: "only.txt", ItemType: ItemTypeFile,
	})

	// Second event for the same path should be accepted even at capacity.
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "only.txt", Name: "only.txt", ItemType: ItemTypeFile,
	})

	result := buf.FlushImmediate()
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	total := len(result[0].RemoteEvents) + len(result[0].LocalEvents)
	if total != 2 {
		t.Errorf("total events = %d, want 2 (existing path should accept new events)", total)
	}
}

func TestBuffer_MaxPaths_ZeroUnlimited(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	buf.SetMaxPaths(0)

	for i := range 100 {
		buf.Add(&ChangeEvent{
			Source: SourceRemote, Type: ChangeCreate,
			Path: fmt.Sprintf("unlimited-%d.txt", i), Name: fmt.Sprintf("unlimited-%d.txt", i),
			ItemType: ItemTypeFile,
		})
	}

	result := buf.FlushImmediate()
	if len(result) != 100 {
		t.Errorf("len(result) = %d, want 100 (zero maxPaths = unlimited)", len(result))
	}
}

// ---------------------------------------------------------------------------
// FlushDebounced double-call panic test (B-111)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// B-115: Conflicting local change types from watch + safety scan
// ---------------------------------------------------------------------------

// TestBuffer_WatchAndSafetyScanConflictingTypes verifies that when a file
// watch produces ChangeCreate and a safety scan produces ChangeModify for the
// same path, the Buffer groups them under the same PathChanges entry. The
// planner should classify a single action — not duplicate or conflict (B-115).
func TestBuffer_WatchAndSafetyScanConflictingTypes(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))

	// Simulate a file watch event (ChangeCreate) arriving first.
	buf.Add(&ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeCreate,
		Path:     "docs/new-file.txt",
		Name:     "new-file.txt",
		ItemType: ItemTypeFile,
		Size:     100,
		Hash:     "localhash1",
	})

	// Safety scan sees the same file and reports ChangeModify (different type).
	buf.Add(&ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeModify,
		Path:     "docs/new-file.txt",
		Name:     "new-file.txt",
		ItemType: ItemTypeFile,
		Size:     100,
		Hash:     "localhash1",
	})

	// Buffer should group both under a single PathChanges entry.
	result := buf.FlushImmediate()
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1 (same path)", len(result))
	}

	if len(result[0].LocalEvents) != 2 {
		t.Fatalf("LocalEvents = %d, want 2", len(result[0].LocalEvents))
	}

	// Planner should produce a single action (upload) for a new local file
	// with no baseline and no remote state. The planner takes the last local
	// event (ChangeModify), which gives a non-nil LocalState.
	planner := NewPlanner(testLogger(t))

	plan, err := planner.Plan(result, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	if err != nil {
		t.Fatalf("Plan() error: %v", err)
	}

	if len(plan.Actions) != 1 {
		t.Fatalf("actions = %d, want 1 (single upload for local-only file)", len(plan.Actions))
	}

	if plan.Actions[0].Type != ActionUpload {
		t.Errorf("action type = %v, want ActionUpload", plan.Actions[0].Type)
	}
}

// ---------------------------------------------------------------------------
// B-128: Debounce semantics under load — slow consumer
// ---------------------------------------------------------------------------

// TestFlushDebounced_SlowConsumer verifies that when events arrive rapidly
// while the consumer is slow (not reading from the output channel), events
// accumulate in the buffer and are delivered as a batch when the consumer
// unblocks. The blocking send on the output channel is intentional
// backpressure — it prevents event loss (B-128).
func TestFlushDebounced_SlowConsumer(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(context.Background())

	debounce := 30 * time.Millisecond
	out := buf.FlushDebounced(ctx, debounce)

	// Inject first wave — this will flush and fill the output channel (cap 1).
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate,
		Path: "slow-a.txt", Name: "slow-a.txt", ItemType: ItemTypeFile,
	})

	// Wait for the first batch to be flushed into the channel.
	time.Sleep(debounce * 3)

	// Inject second wave while the first batch is still unread (slow consumer).
	// These events accumulate in the buffer because the debounce loop is
	// blocked trying to send the first batch.
	for i := range 5 {
		buf.Add(&ChangeEvent{
			Source: SourceLocal, Type: ChangeCreate,
			Path: fmt.Sprintf("slow-b%d.txt", i), Name: fmt.Sprintf("slow-b%d.txt", i),
			ItemType: ItemTypeFile,
		})
	}

	// Now consume the first batch (unblock the debounce loop).
	select {
	case batch1 := <-out:
		if len(batch1) == 0 {
			t.Fatal("first batch is empty")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first batch")
	}

	// The second wave should arrive as an accumulated batch.
	select {
	case batch2 := <-out:
		if len(batch2) != 5 {
			t.Errorf("second batch len = %d, want 5 (accumulated while consumer was slow)", len(batch2))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for accumulated batch")
	}

	cancel()
	for range out {
	}
}

// ---------------------------------------------------------------------------
// FlushDebounced double-call panic test (B-111)
// ---------------------------------------------------------------------------

// TestFlushDebounced_PanicsOnDoubleCall verifies that calling FlushDebounced
// twice on the same Buffer panics (B-111).
func TestFlushDebounced_PanicsOnDoubleCall(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out := buf.FlushDebounced(ctx, time.Hour)
	defer func() {
		cancel()
		for range out {
		}
	}()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on second FlushDebounced call")
		}

		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic, got %T: %v", r, r)
		}

		if msg != "sync: FlushDebounced called twice on the same Buffer" {
			t.Errorf("unexpected panic message: %s", msg)
		}
	}()

	_ = buf.FlushDebounced(ctx, time.Hour)
}
