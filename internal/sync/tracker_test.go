package sync

import (
	"context"
	stdsync "sync"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestDepTracker_NoDeps(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case ta := <-dt.Interactive():
		if ta.LedgerID != 1 {
			t.Errorf("LedgerID = %d, want 1", ta.LedgerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for action on interactive channel")
	}
}

func TestDepTracker_DependencyChain(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	// Action 1: no deps.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "parent",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Action 2: depends on action 1.
	dt.Add(&Action{
		Type: ActionDownload, Path: "parent/child.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	// Only action 1 should be dispatched.
	select {
	case ta := <-dt.Interactive():
		if ta.LedgerID != 1 {
			t.Fatalf("expected action 1, got %d", ta.LedgerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for action 1")
	}

	// Action 2 should not be dispatched yet.
	select {
	case ta := <-dt.Interactive():
		t.Fatalf("action %d dispatched too early", ta.LedgerID)
	case <-time.After(50 * time.Millisecond):
		// Expected — action 2 still blocked.
	}

	// Complete action 1 — action 2 should become ready.
	dt.Complete(1)

	select {
	case ta := <-dt.Interactive():
		if ta.LedgerID != 2 {
			t.Fatalf("expected action 2, got %d", ta.LedgerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for action 2")
	}
}

func TestDepTracker_BulkLane(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	// Large download (above threshold) should go to bulk.
	dt.Add(&Action{
		Type: ActionDownload, Path: "big.bin",
		DriveID: driveid.New("d"), ItemID: "i1",
		View: &PathView{
			Remote: &RemoteState{Size: 20 * 1024 * 1024}, // 20 MB
		},
	}, 1, nil)

	select {
	case ta := <-dt.Bulk():
		if ta.LedgerID != 1 {
			t.Errorf("LedgerID = %d, want 1", ta.LedgerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for action on bulk channel")
	}
}

func TestDepTracker_SmallTransferInteractive(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	// Small upload (below threshold) should go to interactive.
	dt.Add(&Action{
		Type: ActionUpload, Path: "small.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
		View: &PathView{
			Local: &LocalState{Size: 100},
		},
	}, 1, nil)

	select {
	case ta := <-dt.Interactive():
		if ta.LedgerID != 1 {
			t.Errorf("LedgerID = %d, want 1", ta.LedgerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for action on interactive channel")
	}
}

func TestDepTracker_NonTransferAlwaysInteractive(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	// Delete action should always go to interactive regardless of view.
	dt.Add(&Action{
		Type: ActionLocalDelete, Path: "del.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case ta := <-dt.Interactive():
		if ta.LedgerID != 1 {
			t.Errorf("LedgerID = %d, want 1", ta.LedgerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for action on interactive channel")
	}
}

func TestDepTracker_DoneSignal(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "a",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	dt.Add(&Action{
		Type: ActionDownload, Path: "b",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	// Drain interactive.
	<-dt.Interactive()
	dt.Complete(1)
	<-dt.Interactive()
	dt.Complete(2)

	select {
	case <-dt.Done():
		// Success.
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for done signal")
	}
}

func TestDepTracker_CancelByPath(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	dt.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	ta := <-dt.Interactive()

	// Simulate a worker setting the cancel func.
	ctx, cancel := context.WithCancel(context.Background())
	ta.Cancel = cancel

	dt.CancelByPath("cancel-me.txt")

	select {
	case <-ctx.Done():
		// Context was canceled.
	case <-time.After(time.Second):
		t.Fatal("context should have been canceled")
	}
}

func TestDepTracker_ConcurrentComplete(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(100, 100)

	// Fan-out: action 0 has no deps; actions 1-49 depend on action 0.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "root",
		DriveID: driveid.New("d"), ItemID: "i0",
	}, 0, nil)

	for i := int64(1); i <= 49; i++ {
		dt.Add(&Action{
			Type: ActionDownload, Path: "file",
			DriveID: driveid.New("d"), ItemID: "i",
		}, i, []int64{0})
	}

	// Drain the root action.
	<-dt.Interactive()
	dt.Complete(0)

	// Concurrently drain and complete all dependents.
	var wg stdsync.WaitGroup

	for i := int64(1); i <= 49; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			select {
			case ta := <-dt.Interactive():
				dt.Complete(ta.LedgerID)
			case <-time.After(5 * time.Second):
				t.Error("timeout draining dependent action")
			}
		}()
	}

	wg.Wait()

	select {
	case <-dt.Done():
		// All complete.
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for done signal")
	}
}

func TestDepTracker_CompleteUnknownID(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	// Should not panic on unknown ID.
	dt.Complete(999)
}

func TestDepTracker_SkipCompletedDeps(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10)

	// Action 2 depends on action 1, but action 1 is not added to the tracker
	// (simulating it was already completed before tracker was populated).
	dt.Add(&Action{
		Type: ActionDownload, Path: "orphan.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	// Should dispatch immediately since dep 1 is unknown/completed.
	select {
	case ta := <-dt.Interactive():
		if ta.LedgerID != 2 {
			t.Errorf("LedgerID = %d, want 2", ta.LedgerID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for action with unknown dep")
	}
}
