package sync

import (
	"context"
	"fmt"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestDepTracker_NoDeps(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(1), ta.ID)
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for action on ready channel")
	}
}

func TestDepTracker_DependencyChain(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

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
	case ta := <-dt.Ready():
		require.Equal(t, int64(1), ta.ID, "expected action 1")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for action 1")
	}

	// Action 2 should not be dispatched yet.
	select {
	case ta := <-dt.Ready():
		require.Fail(t, fmt.Sprintf("action %d dispatched too early", ta.ID))
	case <-time.After(50 * time.Millisecond):
		// Expected — action 2 still blocked.
	}

	// Complete action 1 — action 2 should become ready.
	dt.Complete(1)

	select {
	case ta := <-dt.Ready():
		require.Equal(t, int64(2), ta.ID, "expected action 2")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for action 2")
	}
}

func TestDepTracker_DoneSignal(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "a",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	dt.Add(&Action{
		Type: ActionDownload, Path: "b",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	// Drain ready channel.
	<-dt.Ready()
	dt.Complete(1)
	<-dt.Ready()
	dt.Complete(2)

	select {
	case <-dt.Done():
		// Success.
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for done signal")
	}
}

func TestDepTracker_CancelByPath(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	dt.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	ta := <-dt.Ready()

	// Simulate a worker setting the cancel func.
	ctx, cancel := context.WithCancel(t.Context())
	ta.Cancel = cancel

	dt.CancelByPath("cancel-me.txt")

	select {
	case <-ctx.Done():
		// Context was canceled.
	case <-time.After(time.Second):
		require.Fail(t, "context should have been canceled")
	}
}

func TestDepTracker_ConcurrentComplete(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(100, testLogger(t))

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
	<-dt.Ready()
	dt.Complete(0)

	// Concurrently drain and complete all dependents.
	var wg stdsync.WaitGroup

	for i := int64(1); i <= 49; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			select {
			case ta := <-dt.Ready():
				dt.Complete(ta.ID)
			case <-time.After(5 * time.Second):
				assert.Fail(t, "timeout draining dependent action")
			}
		}()
	}

	wg.Wait()

	select {
	case <-dt.Done():
		// All complete.
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for done signal")
	}
}

// TestDepTracker_CompleteUnknownID verifies that calling Complete() with an
// unknown ID logs a warning and still increments the completed counter.
// This is a defensive guard against deadlock if the tracker population
// has a subtle bug. Regression test for: silent return without incrementing
// completed → done channel never closed → deadlock.
func TestDepTracker_CompleteUnknownID(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Add one real action so total=1.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "real",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Drain the dispatched action.
	<-dt.Ready()

	// Complete with an unknown ID — should log a warning and increment
	// completed. Since total=1 (from Add) and this increments completed to 1,
	// the done channel closes. This verifies the unknown-ID path still
	// advances the completion counter.
	dt.Complete(999)

	select {
	case <-dt.Done():
		// Success — the unknown-ID completion still incremented the counter.
	case <-time.After(time.Second):
		require.Fail(t, "done channel not closed after Complete with unknown ID — deadlock risk")
	}
}

// TestDepTracker_CompleteUnknownID_NoPanic verifies the basic no-panic case
// with zero tracked actions.
func TestDepTracker_CompleteUnknownID_NoPanic(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

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
	dt := NewDepTracker(200, testLogger(t))

	// Root action with no deps — will be completed while Add appends dependents.
	dt.Add(&Action{
		Type: ActionFolderCreate, Path: "root",
		DriveID: driveid.New("d"), ItemID: "i0",
	}, 0, nil)

	// Seed some initial dependents so Complete has a non-empty slice to iterate.
	for i := int64(1); i <= 20; i++ {
		dt.Add(&Action{
			Type: ActionDownload, Path: fmt.Sprintf("file-%d", i),
			DriveID: driveid.New("d"), ItemID: fmt.Sprintf("i%d", i),
		}, i, []int64{0})
	}

	// Concurrently: Complete root (iterates dependents) while Add
	// appends more dependents to the root action's dependents slice.
	// Under -race, a data race on the slice would be detected here.
	var wg stdsync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-dt.Ready() // drain root
		dt.Complete(0)
	}()

	go func() {
		defer wg.Done()
		for i := int64(21); i <= 40; i++ {
			dt.Add(&Action{
				Type: ActionDownload, Path: fmt.Sprintf("file-%d", i),
				DriveID: driveid.New("d"), ItemID: fmt.Sprintf("i%d", i),
			}, i, []int64{0})
		}
	}()

	wg.Wait()

	// Drain whatever was dispatched (at least the initial 20 dependents).
	// Actions added after Complete's copy won't be dispatched — that's by
	// design (the tracker is populated before workers start in RunOnce).
	drained := 0

	for {
		select {
		case ta := <-dt.Ready():
			dt.Complete(ta.ID)
			drained++
		case <-time.After(200 * time.Millisecond):
			// No more actions to drain.
			require.GreaterOrEqual(t, drained, 20,
				"expected at least 20 dispatched actions")

			return
		}
	}
}

// TestDepTracker_CompleteCleansByPath verifies that Complete() removes the
// byPath entry so a subsequent CancelByPath on the same path is a no-op.
// Regression test for B-095: stale byPath entries in long-lived trackers.
func TestDepTracker_CompleteCleansByPath(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	dt.Add(&Action{
		Type: ActionDownload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	<-dt.Ready()
	dt.Complete(1)

	// After Complete, CancelByPath should be a no-op (byPath entry removed).
	// Verify by adding a new action at the same path — if byPath was NOT
	// cleaned, CancelByPath would have stale reference to action 1.
	dt.Add(&Action{
		Type: ActionUpload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil)

	ta := <-dt.Ready()

	// Set up cancel on the new action.
	ctx, cancel := context.WithCancel(t.Context())
	ta.Cancel = cancel

	// CancelByPath should cancel action 2 (the new one), not be a no-op
	// from a stale action 1 entry.
	dt.CancelByPath("file.txt")

	select {
	case <-ctx.Done():
		// Success — the new action's context was canceled.
	case <-time.After(time.Second):
		require.Fail(t, "CancelByPath should cancel the new action, not be a no-op")
	}
}

// TestDepTracker_CancelByPathCleansUp verifies that CancelByPath removes
// the byPath entry so a subsequent Add at the same path gets a fresh entry.
// Regression test for B-095.
func TestDepTracker_CancelByPathCleansUp(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	dt.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	ta := <-dt.Ready()
	ctx1, cancel1 := context.WithCancel(t.Context())
	ta.Cancel = cancel1

	dt.CancelByPath("cancel-me.txt")

	// Verify action 1 was canceled.
	select {
	case <-ctx1.Done():
		// Good.
	case <-time.After(time.Second):
		require.Fail(t, "action 1 context should have been canceled")
	}

	// Add a new action at the same path. If byPath was cleaned, this works
	// correctly — CancelByPath on the new action should cancel action 2.
	dt.Add(&Action{
		Type: ActionUpload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil)

	ta2 := <-dt.Ready()
	ctx2, cancel2 := context.WithCancel(t.Context())
	ta2.Cancel = cancel2

	dt.CancelByPath("cancel-me.txt")

	select {
	case <-ctx2.Done():
		// Success — action 2's context was canceled.
	case <-time.After(time.Second):
		require.Fail(t, "action 2 context should have been canceled")
	}
}

func TestDepTracker_SkipCompletedDeps(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Action 2 depends on action 1, but action 1 is not added to the tracker
	// (simulating it was already completed before tracker was populated).
	dt.Add(&Action{
		Type: ActionDownload, Path: "orphan.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	// Should dispatch immediately since dep 1 is unknown/completed.
	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(2), ta.ID)
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for action with unknown dep")
	}
}

// ---------------------------------------------------------------------------
// HasInFlight tests (B-122)
// ---------------------------------------------------------------------------

func TestDepTracker_HasInFlight(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// No actions — HasInFlight should be false.
	assert.False(t, dt.HasInFlight("file.txt"), "HasInFlight returned true for empty tracker")

	dt.Add(&Action{
		Type: ActionDownload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Action added — HasInFlight should be true.
	assert.True(t, dt.HasInFlight("file.txt"), "HasInFlight returned false for in-flight path")

	// Drain and complete the action.
	<-dt.Ready()
	dt.Complete(1)

	// After Complete, HasInFlight should be false (byPath cleaned up).
	assert.False(t, dt.HasInFlight("file.txt"), "HasInFlight returned true after Complete")
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
	}, 1, nil)

	<-dt.Ready()
	dt.Complete(1)

	// In persistent mode, Done() should NOT fire even though all actions
	// are complete. Workers exit via context cancellation instead.
	select {
	case <-dt.Done():
		require.Fail(t, "Done() fired in persistent mode — should never close")
	case <-time.After(100 * time.Millisecond):
		// Expected — Done() never fires in persistent mode.
	}
}

// TestDepTracker_SuppressedDepFilteredByEngine verifies that when a dependency
// index is omitted (because the engine suppressed it), the dependent action
// dispatches immediately. Before the fix, the engine passed phantom dep IDs
// for suppressed actions — the tracker silently ignored them, causing dependents
// to dispatch without waiting for (non-existent) completions.
func TestDepTracker_SuppressedDepFilteredByEngine(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Simulate engine's processBatch: action 0 is suppressed (not added),
	// action 1 depends on action 0 but the engine filters out the suppressed
	// dep ID before calling Add.
	dt.Add(&Action{
		Type: ActionDownload, Path: "child.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 1, nil) // no deps — the suppressed dep was filtered out

	// Action 1 should dispatch immediately (no dependencies).
	select {
	case ta := <-dt.Ready():
		require.Equal(t, int64(1), ta.ID, "expected action 1")
	case <-time.After(time.Second):
		require.Fail(t, "action with filtered-out suppressed dep should dispatch immediately")
	}

	dt.Complete(1)
}

// ---------------------------------------------------------------------------
// Scope gating tests (R-2.10.11, R-2.10.15, R-2.10.5)
// ---------------------------------------------------------------------------

// Validates: R-2.10.11, R-2.10.15
func TestScopeGating_BlockedActionsHeld(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Set up a throttle:account scope block — blocks ALL actions.
	block := &ScopeBlock{
		Key:       "throttle:account",
		IssueType: "rate_limited",
		BlockedAt: time.Now(),
	}
	dt.HoldScope("throttle:account", block)

	// Add an action — it should be diverted to the held queue, not ready.
	dt.Add(&Action{
		Type: ActionUpload, Path: "blocked.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// The ready channel should be empty because the action is held.
	select {
	case <-dt.Ready():
		require.Fail(t, "action matching blocked scope should not appear on ready channel")
	case <-time.After(100 * time.Millisecond):
		// Expected — action is in the held queue.
	}
}

// Validates: R-2.10.15
func TestScopeGating_UnblockedPassthrough(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// No scope blocks registered — all actions should pass through.
	dt.Add(&Action{
		Type: ActionUpload, Path: "free.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	select {
	case ta := <-dt.Ready():
		assert.Equal(t, int64(1), ta.ID, "unblocked action should pass through to ready")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for unblocked action on ready channel")
	}
}

// Validates: R-2.10.11
func TestReleaseScope_DispatchesAllHeld(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block the throttle:account scope.
	block := &ScopeBlock{
		Key:       "throttle:account",
		IssueType: "rate_limited",
		BlockedAt: time.Now(),
	}
	dt.HoldScope("throttle:account", block)

	// Add three actions — all should be held.
	for i := int64(1); i <= 3; i++ {
		dt.Add(&Action{
			Type:    ActionUpload,
			Path:    fmt.Sprintf("held-%d.txt", i),
			DriveID: driveid.New("d"),
			ItemID:  fmt.Sprintf("i%d", i),
		}, i, nil)
	}

	// Confirm nothing on ready channel.
	select {
	case <-dt.Ready():
		require.Fail(t, "actions should be held, not ready")
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}

	// Release the scope — all held actions should be dispatched.
	dt.ReleaseScope("throttle:account")

	dispatched := make(map[int64]bool)
	for i := 0; i < 3; i++ {
		select {
		case ta := <-dt.Ready():
			dispatched[ta.ID] = true
		case <-time.After(time.Second):
			require.Fail(t, fmt.Sprintf("timeout waiting for held action %d", i+1))
		}
	}

	assert.Len(t, dispatched, 3, "all three held actions should be dispatched")
	assert.True(t, dispatched[1], "action 1 should be dispatched")
	assert.True(t, dispatched[2], "action 2 should be dispatched")
	assert.True(t, dispatched[3], "action 3 should be dispatched")
}

// Validates: R-2.10.5
func TestDispatchTrial_MarksIsTrial(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block the throttle:account scope.
	block := &ScopeBlock{
		Key:       "throttle:account",
		IssueType: "rate_limited",
		BlockedAt: time.Now(),
	}
	dt.HoldScope("throttle:account", block)

	// Add an action — it goes to the held queue.
	dt.Add(&Action{
		Type: ActionUpload, Path: "trial.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Confirm it's held, not ready.
	select {
	case <-dt.Ready():
		require.Fail(t, "action should be held")
	case <-time.After(50 * time.Millisecond):
		// Expected.
	}

	// DispatchTrial should pop from held and mark IsTrial.
	ok := dt.DispatchTrial("throttle:account")
	require.True(t, ok, "DispatchTrial should return true when held queue is non-empty")

	select {
	case ta := <-dt.Ready():
		assert.True(t, ta.IsTrial, "dispatched trial action should have IsTrial=true")
		assert.Equal(t, int64(1), ta.ID)
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for trial action on ready channel")
	}

	// After popping, the held queue should be empty.
	ok = dt.DispatchTrial("throttle:account")
	assert.False(t, ok, "DispatchTrial should return false when held queue is empty")
}

// ---------------------------------------------------------------------------
// Trial scope key and trial methods (R-2.10.5)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestDispatchTrial_SetsTrialScopeKey(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	block := &ScopeBlock{
		Key:       "quota:own",
		IssueType: "quota_exceeded",
		BlockedAt: time.Now(),
	}
	dt.HoldScope("quota:own", block)

	dt.Add(&Action{
		Type: ActionUpload, Path: "big.zip",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	ok := dt.DispatchTrial("quota:own")
	require.True(t, ok)

	select {
	case ta := <-dt.Ready():
		assert.True(t, ta.IsTrial, "should be marked as trial")
		assert.Equal(t, "quota:own", ta.TrialScopeKey, "should carry scope key")
	case <-time.After(time.Second):
		require.Fail(t, "timeout waiting for trial action")
	}
}

// Validates: R-2.10.5
func TestNextDueTrial(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	// No scope blocks — no due trials.
	key, _, ok := dt.NextDueTrial(now)
	assert.False(t, ok, "no scope blocks → no due trials")
	assert.Empty(t, key)

	// Add a scope block with NextTrialAt in the past.
	block := &ScopeBlock{
		Key:         "throttle:account",
		IssueType:   "rate_limited",
		BlockedAt:   now.Add(-time.Minute),
		NextTrialAt: now.Add(-time.Second),
	}
	dt.HoldScope("throttle:account", block)

	// Add a held action (NextDueTrial requires a non-empty held queue).
	dt.Add(&Action{
		Type: ActionUpload, Path: "test.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	key, trialAt, ok := dt.NextDueTrial(now)
	assert.True(t, ok, "past NextTrialAt with held actions → due trial")
	assert.Equal(t, "throttle:account", key)
	assert.Equal(t, block.NextTrialAt, trialAt)

	// NextTrialAt in the future — not due.
	block.NextTrialAt = now.Add(time.Hour)
	key, _, ok = dt.NextDueTrial(now)
	assert.False(t, ok, "future NextTrialAt → not due")
	assert.Empty(t, key)
}

// Validates: R-2.10.5
func TestExtendTrial(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	block := &ScopeBlock{
		Key:           "throttle:account",
		IssueType:     "rate_limited",
		BlockedAt:     now,
		NextTrialAt:   now.Add(10 * time.Second),
		TrialCount:    0,
		TrialInterval: 10 * time.Second,
	}
	dt.HoldScope("throttle:account", block)

	newAt := now.Add(30 * time.Second)
	dt.ExtendTrial("throttle:account", newAt)

	// Verify the block was updated.
	dt.mu.Lock()
	updated := dt.scopeBlocks["throttle:account"]
	dt.mu.Unlock()

	assert.Equal(t, newAt, updated.NextTrialAt, "NextTrialAt should be extended")
	assert.Equal(t, 1, updated.TrialCount, "TrialCount should be incremented")
}

func TestExtendTrial_UnknownScope(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Should not panic on unknown scope.
	dt.ExtendTrial("nonexistent", time.Now().Add(time.Minute))
}

// ---------------------------------------------------------------------------
// EarliestTrialAt (R-2.10.5)
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestEarliestTrialAt_ReturnsEarliest(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	// No blocks → no earliest.
	_, ok := dt.EarliestTrialAt()
	assert.False(t, ok, "no scope blocks → no earliest trial")

	// Add two blocks with different NextTrialAt and held actions.
	dt.HoldScope("service", &ScopeBlock{
		Key:         "service",
		IssueType:   "service_outage",
		NextTrialAt: now.Add(5 * time.Minute),
	})
	dt.Add(&Action{Type: ActionDownload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)

	dt.HoldScope("throttle:account", &ScopeBlock{
		Key:         "throttle:account",
		IssueType:   "rate_limited",
		NextTrialAt: now.Add(2 * time.Minute),
	})
	dt.Add(&Action{Type: ActionUpload, Path: "b.txt", DriveID: driveid.New("d"), ItemID: "i2"}, 2, nil)

	earliest, ok := dt.EarliestTrialAt()
	assert.True(t, ok)
	assert.Equal(t, now.Add(2*time.Minute), earliest, "should return the earlier of the two")
}

// Validates: R-2.10.5
func TestEarliestTrialAt_SkipsEmptyHeldQueue(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))
	now := time.Now()

	// Block exists but no held actions.
	dt.HoldScope("service", &ScopeBlock{
		Key:         "service",
		IssueType:   "service_outage",
		NextTrialAt: now.Add(time.Minute),
	})

	_, ok := dt.EarliestTrialAt()
	assert.False(t, ok, "block with no held actions should be skipped")
}

// Validates: R-2.10.5
func TestEarliestTrialAt_SkipsZeroNextTrialAt(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Block with zero NextTrialAt.
	dt.HoldScope("service", &ScopeBlock{
		Key:       "service",
		IssueType: "service_outage",
	})
	dt.Add(&Action{Type: ActionDownload, Path: "a.txt", DriveID: driveid.New("d"), ItemID: "i1"}, 1, nil)

	_, ok := dt.EarliestTrialAt()
	assert.False(t, ok, "zero NextTrialAt should be skipped")
}

// ---------------------------------------------------------------------------
// GetScopeBlock
// ---------------------------------------------------------------------------

func TestGetScopeBlock(t *testing.T) {
	t.Parallel()

	dt := NewDepTracker(10, testLogger(t))

	// Unknown key → not found.
	_, ok := dt.GetScopeBlock("nonexistent")
	assert.False(t, ok)

	// Add a block and retrieve it.
	block := &ScopeBlock{
		Key:           "quota:own",
		IssueType:     "quota_exceeded",
		TrialInterval: 5 * time.Minute,
	}
	dt.HoldScope("quota:own", block)

	got, ok := dt.GetScopeBlock("quota:own")
	require.True(t, ok)
	assert.Equal(t, block, got)
}
