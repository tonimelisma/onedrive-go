package sync

import (
	"context"
	"fmt"
	stdsync "sync"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestDepTracker_NoDeps(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

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

	dt := NewDepTracker(10, 10, testLogger(t))

	// Action 1: no deps.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "parent",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

	// Action 2: depends on action 1.
	dt.Add(&Action{
		Type: ActionDownload, Path: "parent/child.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1}, "")

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

	dt := NewDepTracker(10, 10, testLogger(t))

	// Large download (above threshold) should go to bulk.
	dt.Add(&Action{
		Type: ActionDownload, Path: "big.bin",
		DriveID: driveid.New("d"), ItemID: "i1",
		View: &PathView{
			Remote: &RemoteState{Size: 20 * 1024 * 1024}, // 20 MB
		},
	}, 1, nil, "")

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

	dt := NewDepTracker(10, 10, testLogger(t))

	// Small upload (below threshold) should go to interactive.
	dt.Add(&Action{
		Type: ActionUpload, Path: "small.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
		View: &PathView{
			Local: &LocalState{Size: 100},
		},
	}, 1, nil, "")

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

	dt := NewDepTracker(10, 10, testLogger(t))

	// Delete action should always go to interactive regardless of view.
	dt.Add(&Action{
		Type: ActionLocalDelete, Path: "del.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

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

	dt := NewDepTracker(10, 10, testLogger(t))

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "a",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

	dt.Add(&Action{
		Type: ActionDownload, Path: "b",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1}, "")

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

	dt := NewDepTracker(10, 10, testLogger(t))

	dt.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

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

	dt := NewDepTracker(100, 100, testLogger(t))

	// Fan-out: action 0 has no deps; actions 1-49 depend on action 0.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "root",
		DriveID: driveid.New("d"), ItemID: "i0",
	}, 0, nil, "")

	for i := int64(1); i <= 49; i++ {
		dt.Add(&Action{
			Type: ActionDownload, Path: "file",
			DriveID: driveid.New("d"), ItemID: "i",
		}, i, []int64{0}, "")
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

// TestDepTracker_CompleteUnknownID verifies that calling Complete() with an
// unknown ledger ID logs a warning and still increments the completed counter.
// This is a defensive guard against deadlock if the tracker/ledger population
// has a subtle bug. Regression test for: silent return without incrementing
// completed → done channel never closed → deadlock.
func TestDepTracker_CompleteUnknownID(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	// Add one real action so total=1.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "real",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

	// Drain the dispatched action.
	<-dt.Interactive()

	// Complete with an unknown ID — should log a warning and increment
	// completed. Since total=1 (from Add) and this increments completed to 1,
	// the done channel closes. This verifies the unknown-ID path still
	// advances the completion counter.
	dt.Complete(999)

	select {
	case <-dt.Done():
		// Success — the unknown-ID completion still incremented the counter.
	case <-time.After(time.Second):
		t.Fatal("done channel not closed after Complete with unknown ID — deadlock risk")
	}
}

// TestDepTracker_CompleteUnknownID_NoPanic verifies the basic no-panic case
// with zero tracked actions.
func TestDepTracker_CompleteUnknownID_NoPanic(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	// Should not panic on unknown ID with zero total.
	dt.Complete(999)
}

// TestDepTracker_ConcurrentAddAndComplete verifies that Complete() safely
// copies the dependents slice before iterating, preventing a data race with
// Add() appending to the same slice. Under -race this would fail if
// Complete() iterated the original slice without copying.
// Regression test for: Complete() read ta.dependents under lock, released
// lock, then iterated — racing with Add() appending to the same slice.
func TestDepTracker_ConcurrentAddAndComplete(t *testing.T) {
	t.Parallel()

	// Use large buffer to prevent dispatch blocking during the race window.
	dt := NewDepTracker(200, 200, testLogger(t))

	// Root action with no deps — will be completed while Add appends dependents.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "root",
		DriveID: driveid.New("d"), ItemID: "i0",
	}, 0, nil, "")

	// Seed some initial dependents so Complete has a non-empty slice to iterate.
	for i := int64(1); i <= 20; i++ {
		dt.Add(&Action{
			Type: ActionDownload, Path: fmt.Sprintf("file-%d", i),
			DriveID: driveid.New("d"), ItemID: fmt.Sprintf("i%d", i),
		}, i, []int64{0}, "")
	}

	// Concurrently: Complete root (iterates dependents) while Add
	// appends more dependents to the root action's dependents slice.
	// Under -race, a data race on the slice would be detected here.
	var wg stdsync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-dt.Interactive() // drain root
		dt.Complete(0)
	}()

	go func() {
		defer wg.Done()
		for i := int64(21); i <= 40; i++ {
			dt.Add(&Action{
				Type: ActionDownload, Path: fmt.Sprintf("file-%d", i),
				DriveID: driveid.New("d"), ItemID: fmt.Sprintf("i%d", i),
			}, i, []int64{0}, "")
		}
	}()

	wg.Wait()

	// Drain whatever was dispatched (at least the initial 20 dependents).
	// Actions added after Complete's copy won't be dispatched — that's by
	// design (the tracker is populated before workers start in RunOnce).
	drained := 0

	for {
		select {
		case ta := <-dt.Interactive():
			dt.Complete(ta.LedgerID)
			drained++
		case <-time.After(200 * time.Millisecond):
			// No more actions to drain.
			if drained < 20 {
				t.Fatalf("expected at least 20 dispatched actions, got %d", drained)
			}

			return
		}
	}
}

// TestDepTracker_CompleteCleansByPath verifies that Complete() removes the
// byPath entry so a subsequent CancelByPath on the same path is a no-op.
// Regression test for B-095: stale byPath entries in long-lived trackers.
func TestDepTracker_CompleteCleansByPath(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	dt.Add(&Action{
		Type: ActionDownload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

	<-dt.Interactive()
	dt.Complete(1)

	// After Complete, CancelByPath should be a no-op (byPath entry removed).
	// Verify by adding a new action at the same path — if byPath was NOT
	// cleaned, CancelByPath would have stale reference to action 1.
	dt.Add(&Action{
		Type: ActionUpload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil, "")

	ta := <-dt.Interactive()

	// Set up cancel on the new action.
	ctx, cancel := context.WithCancel(context.Background())
	ta.Cancel = cancel

	// CancelByPath should cancel action 2 (the new one), not be a no-op
	// from a stale action 1 entry.
	dt.CancelByPath("file.txt")

	select {
	case <-ctx.Done():
		// Success — the new action's context was canceled.
	case <-time.After(time.Second):
		t.Fatal("CancelByPath should cancel the new action, not be a no-op")
	}
}

// TestDepTracker_CancelByPathCleansUp verifies that CancelByPath removes
// the byPath entry so a subsequent Add at the same path gets a fresh entry.
// Regression test for B-095.
func TestDepTracker_CancelByPathCleansUp(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	dt.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

	ta := <-dt.Interactive()
	ctx1, cancel1 := context.WithCancel(context.Background())
	ta.Cancel = cancel1

	dt.CancelByPath("cancel-me.txt")

	// Verify action 1 was canceled.
	select {
	case <-ctx1.Done():
		// Good.
	case <-time.After(time.Second):
		t.Fatal("action 1 context should have been canceled")
	}

	// Add a new action at the same path. If byPath was cleaned, this works
	// correctly — CancelByPath on the new action should cancel action 2.
	dt.Add(&Action{
		Type: ActionUpload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil, "")

	ta2 := <-dt.Interactive()
	ctx2, cancel2 := context.WithCancel(context.Background())
	ta2.Cancel = cancel2

	dt.CancelByPath("cancel-me.txt")

	select {
	case <-ctx2.Done():
		// Success — action 2's context was canceled.
	case <-time.After(time.Second):
		t.Fatal("action 2 context should have been canceled")
	}
}

func TestDepTracker_SkipCompletedDeps(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	// Action 2 depends on action 1, but action 1 is not added to the tracker
	// (simulating it was already completed before tracker was populated).
	dt.Add(&Action{
		Type: ActionDownload, Path: "orphan.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1}, "")

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

// ---------------------------------------------------------------------------
// HasInFlight tests (B-122)
// ---------------------------------------------------------------------------

func TestDepTracker_HasInFlight(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	// No actions — HasInFlight should be false.
	if dt.HasInFlight("file.txt") {
		t.Error("HasInFlight returned true for empty tracker")
	}

	dt.Add(&Action{
		Type: ActionDownload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

	// Action added — HasInFlight should be true.
	if !dt.HasInFlight("file.txt") {
		t.Error("HasInFlight returned false for in-flight path")
	}

	// Drain and complete the action.
	<-dt.Interactive()
	dt.Complete(1)

	// After Complete, HasInFlight should be false (byPath cleaned up).
	if dt.HasInFlight("file.txt") {
		t.Error("HasInFlight returned true after Complete")
	}
}

// ---------------------------------------------------------------------------
// Persistent mode tests
// ---------------------------------------------------------------------------

func TestDepTracker_PersistentMode(t *testing.T) {
	t.Parallel()

	dt := NewPersistentDepTracker(testLogger(t))

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil, "")

	<-dt.Interactive()
	dt.Complete(1)

	// In persistent mode, Done() should NOT fire even though all actions
	// are complete. Workers exit via context cancellation instead.
	select {
	case <-dt.Done():
		t.Fatal("Done() fired in persistent mode — should never close")
	case <-time.After(100 * time.Millisecond):
		// Expected — Done() never fires in persistent mode.
	}
}

// ---------------------------------------------------------------------------
// Per-cycle completion tracking tests (B-121)
// ---------------------------------------------------------------------------

func TestDepTracker_CycleDone(t *testing.T) {
	t.Parallel()

	dt := NewPersistentDepTracker(testLogger(t))

	// Cycle A: two actions.
	dt.Add(&Action{
		Type: ActionDownload, Path: "a1.txt",
		DriveID: driveid.New("d"), ItemID: "a1",
	}, 1, nil, "cycle-a")
	dt.Add(&Action{
		Type: ActionDownload, Path: "a2.txt",
		DriveID: driveid.New("d"), ItemID: "a2",
	}, 2, nil, "cycle-a")

	// Cycle B: one action.
	dt.Add(&Action{
		Type: ActionUpload, Path: "b1.txt",
		DriveID: driveid.New("d"), ItemID: "b1",
	}, 3, nil, "cycle-b")

	cycleADone := dt.CycleDone("cycle-a")
	cycleBDone := dt.CycleDone("cycle-b")

	// Drain all actions.
	<-dt.Interactive()
	<-dt.Interactive()
	<-dt.Interactive()

	// Complete cycle A actions.
	dt.Complete(1)

	// Cycle A should NOT be done yet (1 of 2 complete).
	select {
	case <-cycleADone:
		t.Fatal("cycle A done too early")
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}

	dt.Complete(2)

	// Now cycle A should be done.
	select {
	case <-cycleADone:
		// Success.
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for cycle A done")
	}

	// Cycle B should NOT be done yet.
	select {
	case <-cycleBDone:
		t.Fatal("cycle B done before its actions completed")
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}

	dt.Complete(3)

	// Now cycle B should be done.
	select {
	case <-cycleBDone:
		// Success.
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for cycle B done")
	}
}

func TestDepTracker_CleanupCycle(t *testing.T) {
	t.Parallel()

	dt := NewPersistentDepTracker(testLogger(t))

	// Add and complete a cycle.
	dt.Add(&Action{
		Type: ActionDownload, Path: "cleanup.txt",
		DriveID: driveid.New("d"), ItemID: "c1",
	}, 1, nil, "cycle-cleanup")

	<-dt.Interactive()
	dt.Complete(1)

	// Wait for cycle done.
	select {
	case <-dt.CycleDone("cycle-cleanup"):
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for cycle done")
	}

	// Cleanup should remove the cycle from the map.
	dt.CleanupCycle("cycle-cleanup")

	// CycleDone for a cleaned-up cycle should return a closed channel
	// (unknown cycle → defensive closed channel).
	select {
	case <-dt.CycleDone("cycle-cleanup"):
		// Success — closed channel returns immediately.
	case <-time.After(time.Second):
		t.Fatal("CycleDone for cleaned-up cycle should not block")
	}
}

func TestDepTracker_CycleDone_UnknownCycle(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, 10, testLogger(t))

	// CycleDone for an unknown cycle should return a closed channel
	// (defensive: prevents callers from blocking forever).
	select {
	case <-dt.CycleDone("nonexistent"):
		// Success — closed channel returns immediately.
	case <-time.After(time.Second):
		t.Fatal("CycleDone for unknown cycle should not block")
	}
}
