package sync

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.Len(t, result, 1)

	pc := result[0]
	assert.Equal(t, "buffer-notes.txt", pc.Path)
	require.Len(t, pc.RemoteEvents, 1)
	assert.Equal(t, ChangeCreate, pc.RemoteEvents[0].Type)
	assert.Empty(t, pc.LocalEvents)
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
	require.Len(t, result, 2)

	alpha := findPathChanges(result, "buffer-alpha.txt")
	require.NotNil(t, alpha, "buffer-alpha.txt not found")
	assert.Len(t, alpha.RemoteEvents, 1)

	beta := findPathChanges(result, "buffer-beta.csv")
	require.NotNil(t, beta, "buffer-beta.csv not found")
	assert.Len(t, beta.LocalEvents, 1)
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
	require.Len(t, result, 1)

	pc := result[0]
	assert.Len(t, pc.RemoteEvents, 2)
	assert.Equal(t, ChangeCreate, pc.RemoteEvents[0].Type)
	assert.Equal(t, ChangeModify, pc.RemoteEvents[1].Type)
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
	require.Len(t, result, 1)
	assert.Len(t, result[0].LocalEvents, 2)
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
	require.Len(t, result, 1)

	pc := result[0]
	assert.Len(t, pc.RemoteEvents, 1)
	assert.Len(t, pc.LocalEvents, 1)
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
	require.Len(t, result, 2)

	newPC := findPathChanges(result, "buffer-new-folder/moved.txt")
	require.NotNil(t, newPC, "new path not found")
	require.Len(t, newPC.RemoteEvents, 1)
	assert.Equal(t, ChangeMove, newPC.RemoteEvents[0].Type)

	oldPC := findPathChanges(result, "buffer-old-folder/moved.txt")
	require.NotNil(t, oldPC, "old path not found")
	require.Len(t, oldPC.RemoteEvents, 1)

	synth := oldPC.RemoteEvents[0]
	assert.Equal(t, ChangeDelete, synth.Type)
	assert.True(t, synth.IsDeleted)
	assert.Equal(t, "buf-m1", synth.ItemID)
	assert.Equal(t, "moved.txt", synth.Name)
	assert.True(t, synth.DriveID.Equal(driveid.New(testDriveID)))
	assert.Equal(t, "buf-parent-1", synth.ParentID)
	assert.Equal(t, ItemTypeFile, synth.ItemType)
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
	require.Len(t, result, 1)
	assert.Equal(t, "buffer-dest/file.txt", result[0].Path)
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

	require.Len(t, result, 2)

	a := findPathChanges(result, "buffer-batch-a.txt")
	require.NotNil(t, a, "buffer-batch-a.txt not found")
	assert.Len(t, a.RemoteEvents, 2)

	b := findPathChanges(result, "buffer-batch-b.txt")
	require.NotNil(t, b, "buffer-batch-b.txt not found")
	assert.Len(t, b.LocalEvents, 1)
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
	require.Len(t, result, 3)

	oldPC := findPathChanges(result, "buffer-mv-old/data.json")
	require.NotNil(t, oldPC, "old path not found for move in batch")
	require.Len(t, oldPC.LocalEvents, 1)
	assert.Equal(t, ChangeDelete, oldPC.LocalEvents[0].Type)
}

func TestBuffer_FlushEmpty(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	result := buf.FlushImmediate()

	assert.Nil(t, result)
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
	require.Len(t, first, 1)

	second := buf.FlushImmediate()
	assert.Nil(t, second)
	assert.Equal(t, 0, buf.Len())
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
	require.Len(t, result, 3)

	assert.Equal(t, "aaa/buffer-first.txt", result[0].Path)
	assert.Equal(t, "mmm/buffer-middle.txt", result[1].Path)
	assert.Equal(t, "zzz/buffer-last.txt", result[2].Path)
}

func TestBuffer_Len(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))

	assert.Equal(t, 0, buf.Len())

	buf.Add(&ChangeEvent{
		Source: SourceRemote, Type: ChangeCreate,
		Path: "buffer-len-one.txt", Name: "buffer-len-one.txt",
		ItemID: "buf-l1", DriveID: driveid.New(testDriveID), ItemType: ItemTypeFile,
	})

	assert.Equal(t, 1, buf.Len())

	// Same path again — Len should not increase.
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "buffer-len-one.txt", Name: "buffer-len-one.txt",
		ItemType: ItemTypeFile,
	})

	assert.Equal(t, 1, buf.Len())

	// Different path.
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate,
		Path: "buffer-len-two.txt", Name: "buffer-len-two.txt",
		ItemType: ItemTypeFile,
	})

	assert.Equal(t, 2, buf.Len())
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
	assert.Len(t, result, wantPaths)

	// Validate each path has exactly 1 event.
	for _, pc := range result {
		total := len(pc.RemoteEvents) + len(pc.LocalEvents)
		assert.Equal(t, 1, total, "path %q should have exactly 1 event", pc.Path)
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
	assert.Equal(t, 1, buf.Len(), "all events target same path")

	result := buf.FlushImmediate()
	require.Len(t, result, 1)

	totalEvents := len(result[0].RemoteEvents) + len(result[0].LocalEvents)
	wantTotal := goroutines * eventsPerGoroutine

	assert.Equal(t, wantTotal, totalEvents)
}

// ---------------------------------------------------------------------------
// FlushDebounced tests
// ---------------------------------------------------------------------------

func TestFlushDebounced_SingleBatch(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())

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
		assert.Len(t, batch, 2)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for debounced batch")
	}

	cancel()
	// Drain channel to ensure debounce goroutine exits before test ends.
	for range out {
	}
}

func TestFlushDebounced_MultipleWaves(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())

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
		assert.Len(t, batch, 1)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for wave 1")
	}

	// Wave 2.
	buf.Add(&ChangeEvent{
		Source: SourceLocal, Type: ChangeModify,
		Path: "wave2.txt", Name: "wave2.txt",
		ItemType: ItemTypeFile,
	})

	select {
	case batch := <-out:
		assert.Len(t, batch, 1)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for wave 2")
	}

	cancel()
	for range out {
	}
}

func TestFlushDebounced_DebounceResets(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())

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
		assert.Len(t, batch, 2, "debounce should have combined")
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for debounced batch")
	}

	cancel()
	for range out {
	}
}

func TestFlushDebounced_ContextCancel(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())

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

	assert.True(t, gotBatch, "expected a final drain batch on context cancel")
}

func TestFlushDebounced_ConcurrentAddAndFlush(t *testing.T) {
	t.Parallel()

	buf := NewBuffer(testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())

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
			require.Fail(t, fmt.Sprintf("timeout: got %d paths, want %d", totalPaths, goroutines*eventsPerGoroutine))
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
	ctx, cancel := context.WithCancel(t.Context())

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
		require.Fail(t, "debounce goroutine deadlocked on final drain (B-103)")
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
	assert.Len(t, result, 2, "third path should be dropped")
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
	require.Len(t, result, 1)

	total := len(result[0].RemoteEvents) + len(result[0].LocalEvents)
	assert.Equal(t, 2, total, "existing path should accept new events")
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
	assert.Len(t, result, 100, "zero maxPaths = unlimited")
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
	require.Len(t, result, 1, "same path")
	require.Len(t, result[0].LocalEvents, 2)

	// Planner should produce a single action (upload) for a new local file
	// with no baseline and no remote state. The planner takes the last local
	// event (ChangeModify), which gives a non-nil LocalState.
	planner := NewPlanner(testLogger(t))

	plan, err := planner.Plan(result, emptyBaseline(), SyncBidirectional, DefaultSafetyConfig())
	require.NoError(t, err, "Plan()")
	require.Len(t, plan.Actions, 1, "single upload for local-only file")
	assert.Equal(t, ActionUpload, plan.Actions[0].Type)
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
	ctx, cancel := context.WithCancel(t.Context())

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
		require.NotEmpty(t, batch1, "first batch is empty")
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for first batch")
	}

	// The second wave should arrive as an accumulated batch.
	select {
	case batch2 := <-out:
		assert.Len(t, batch2, 5, "accumulated while consumer was slow")
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for accumulated batch")
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
		require.NotNil(t, r, "expected panic on second FlushDebounced call")

		msg, ok := r.(string)
		require.True(t, ok, "expected string panic, got %T: %v", r, r)
		assert.Equal(t, "sync: FlushDebounced called twice on the same Buffer", msg)
	}()

	_ = buf.FlushDebounced(ctx, time.Hour)
}
