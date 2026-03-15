package sync

import (
	"context"
	"fmt"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// Add — no dependencies
// ---------------------------------------------------------------------------

func TestDepGraph_Add_NoDeps(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	ta := dg.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	require.NotNil(t, ta, "action with no deps should be returned as immediately ready")
	assert.Equal(t, int64(1), ta.ID)
}

// ---------------------------------------------------------------------------
// Add — with dependencies
// ---------------------------------------------------------------------------

func TestDepGraph_Add_WithDeps(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Action 1: no deps — should be returned as ready.
	ta1 := dg.Add(&Action{
		Type: ActionFolderCreate, Path: "parent",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)
	require.NotNil(t, ta1, "action 1 with no deps should be immediately ready")

	// Action 2: depends on action 1 — should NOT be returned (waiting).
	ta2 := dg.Add(&Action{
		Type: ActionDownload, Path: "parent/child.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})
	assert.Nil(t, ta2, "action 2 with unsatisfied dep should not be immediately ready")
}

// ---------------------------------------------------------------------------
// Complete — returns dependents
// ---------------------------------------------------------------------------

func TestDepGraph_Complete_ReturnsDependents(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Action 1: no deps.
	dg.Add(&Action{
		Type: ActionFolderCreate, Path: "parent",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Action 2: depends on action 1.
	dg.Add(&Action{
		Type: ActionDownload, Path: "parent/child.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	// Action 3: also depends on action 1.
	dg.Add(&Action{
		Type: ActionDownload, Path: "parent/other.txt",
		DriveID: driveid.New("d"), ItemID: "i3",
	}, 3, []int64{1})

	// Complete action 1 — should return actions 2 and 3.
	ready, ok := dg.Complete(1)
	require.True(t, ok, "Complete should return true for known ID")
	require.Len(t, ready, 2, "completing action 1 should release 2 dependents")

	ids := map[int64]bool{}
	for _, ta := range ready {
		ids[ta.ID] = true
	}
	assert.True(t, ids[2], "action 2 should be in ready list")
	assert.True(t, ids[3], "action 3 should be in ready list")
}

// ---------------------------------------------------------------------------
// Complete — deletes from actions map (D-10 fix)
// ---------------------------------------------------------------------------

func TestDepGraph_Complete_DeletesFromActions(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	dg.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	_, ok := dg.Complete(1)
	require.True(t, ok)

	// After Complete, action 1 should be gone from the actions map.
	// InFlightCount uses len(actions), so it should be 0.
	assert.Equal(t, 0, dg.InFlightCount(),
		"D-10: Complete must delete from actions map")

	// A new action depending on the completed ID should treat it as
	// satisfied (dep not found in actions → skip → depsLeft stays 0).
	ta := dg.Add(&Action{
		Type: ActionDownload, Path: "dir/file.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	require.NotNil(t, ta,
		"D-10: action depending on completed (and deleted) ID should be immediately ready")
}

// ---------------------------------------------------------------------------
// Complete — unknown ID
// ---------------------------------------------------------------------------

func TestDepGraph_Complete_UnknownID(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Add one real action.
	dg.Add(&Action{
		Type: ActionFolderCreate, Path: "real",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Complete with an unknown ID — should log warning and return (nil, false).
	ready, ok := dg.Complete(999)
	assert.False(t, ok, "Complete with unknown ID should return false")
	assert.Nil(t, ready, "Complete with unknown ID should return nil ready list")
}

func TestDepGraph_Complete_UnknownID_NoPanic(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Should not panic on unknown ID with zero tracked actions.
	ready, ok := dg.Complete(999)
	assert.False(t, ok)
	assert.Nil(t, ready)
}

// ---------------------------------------------------------------------------
// Complete — cleans byPath
// ---------------------------------------------------------------------------

func TestDepGraph_Complete_CleansByPath(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	dg.Add(&Action{
		Type: ActionDownload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	_, ok := dg.Complete(1)
	require.True(t, ok)

	// After Complete, HasInFlight should be false (byPath cleaned).
	assert.False(t, dg.HasInFlight("file.txt"),
		"byPath should be cleaned after Complete")
}

// ---------------------------------------------------------------------------
// Concurrent Complete
// ---------------------------------------------------------------------------

func TestDepGraph_ConcurrentComplete(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Fan-out: action 0 has no deps; actions 1-49 depend on action 0.
	dg.Add(&Action{
		Type: ActionFolderCreate, Path: "root",
		DriveID: driveid.New("d"), ItemID: "i0",
	}, 0, nil)

	for i := int64(1); i <= 49; i++ {
		dg.Add(&Action{
			Type: ActionDownload, Path: fmt.Sprintf("file-%d", i),
			DriveID: driveid.New("d"), ItemID: "i",
		}, i, []int64{0})
	}

	// Complete action 0 — should return 49 dependents.
	ready, ok := dg.Complete(0)
	require.True(t, ok)
	require.Len(t, ready, 49, "completing root should release 49 dependents")

	// Concurrently complete all dependents.
	var wg stdsync.WaitGroup
	for _, ta := range ready {
		wg.Add(1)
		go func(ta *TrackedAction) {
			defer wg.Done()
			dg.Complete(ta.ID)
		}(ta)
	}
	wg.Wait()

	assert.Equal(t, 0, dg.InFlightCount(), "all actions should be completed")
}

// ---------------------------------------------------------------------------
// Concurrent Add and Complete
// ---------------------------------------------------------------------------

func TestDepGraph_ConcurrentAddAndComplete(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Root action with no deps.
	dg.Add(&Action{
		Type: ActionFolderCreate, Path: "root",
		DriveID: driveid.New("d"), ItemID: "i0",
	}, 0, nil)

	// Seed some initial dependents.
	for i := int64(1); i <= 20; i++ {
		dg.Add(&Action{
			Type: ActionDownload, Path: fmt.Sprintf("file-%d", i),
			DriveID: driveid.New("d"), ItemID: fmt.Sprintf("i%d", i),
		}, i, []int64{0})
	}

	// Concurrently: Complete root (iterates dependents) while Add
	// appends more dependents to the root action's dependents slice.
	var wg stdsync.WaitGroup
	wg.Add(2)

	var readyFromComplete []*TrackedAction

	go func() {
		defer wg.Done()
		readyFromComplete, _ = dg.Complete(0)
	}()

	go func() {
		defer wg.Done()
		for i := int64(21); i <= 40; i++ {
			dg.Add(&Action{
				Type: ActionDownload, Path: fmt.Sprintf("file-%d", i),
				DriveID: driveid.New("d"), ItemID: fmt.Sprintf("i%d", i),
			}, i, []int64{0})
		}
	}()

	wg.Wait()

	// At least the initial 20 dependents should have been returned.
	require.GreaterOrEqual(t, len(readyFromComplete), 20,
		"expected at least 20 dependents from Complete")
}

// ---------------------------------------------------------------------------
// HasInFlight
// ---------------------------------------------------------------------------

func TestDepGraph_HasInFlight(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// No actions — HasInFlight should be false.
	assert.False(t, dg.HasInFlight("file.txt"))

	dg.Add(&Action{
		Type: ActionDownload, Path: "file.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	assert.True(t, dg.HasInFlight("file.txt"))

	dg.Complete(1)

	assert.False(t, dg.HasInFlight("file.txt"), "should be false after Complete")
}

// ---------------------------------------------------------------------------
// CancelByPath
// ---------------------------------------------------------------------------

func TestDepGraph_CancelByPath(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	dg.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	// Simulate a worker setting the cancel func.
	ctx, cancel := context.WithCancel(t.Context())

	// We need to access the TrackedAction to set Cancel — grab it via Add return.
	// Action 1 was returned by Add (no deps), so we already have it. But since
	// we didn't capture it, add a second and test with that.
	dg2 := NewDepGraph(testLogger(t))
	ta := dg2.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)
	require.NotNil(t, ta)
	ta.Cancel = cancel

	dg2.CancelByPath("cancel-me.txt")

	select {
	case <-ctx.Done():
		// Context was canceled.
	default:
		require.Fail(t, "context should have been canceled")
	}
}

func TestDepGraph_CancelByPath_CleansUp(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	ta := dg.Add(&Action{
		Type: ActionDownload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)
	require.NotNil(t, ta)

	ctx1, cancel1 := context.WithCancel(t.Context())
	ta.Cancel = cancel1

	dg.CancelByPath("cancel-me.txt")

	// Verify action 1 was canceled.
	select {
	case <-ctx1.Done():
		// Good.
	default:
		require.Fail(t, "action 1 context should have been canceled")
	}

	// byPath should be cleaned — HasInFlight returns false.
	assert.False(t, dg.HasInFlight("cancel-me.txt"),
		"byPath should be cleaned after CancelByPath")

	// Add a new action at the same path and cancel it.
	ta2 := dg.Add(&Action{
		Type: ActionUpload, Path: "cancel-me.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil)
	require.NotNil(t, ta2)

	ctx2, cancel2 := context.WithCancel(t.Context())
	ta2.Cancel = cancel2

	dg.CancelByPath("cancel-me.txt")

	select {
	case <-ctx2.Done():
		// Success — action 2's context was canceled.
	default:
		require.Fail(t, "action 2 context should have been canceled")
	}
}

// ---------------------------------------------------------------------------
// SkipCompletedDeps — dependency not in actions map
// ---------------------------------------------------------------------------

func TestDepGraph_SkipCompletedDeps(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Action 2 depends on action 1, but action 1 is not in the graph
	// (simulating it was already completed before tracker was populated).
	ta := dg.Add(&Action{
		Type: ActionDownload, Path: "orphan.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	require.NotNil(t, ta,
		"action with unknown dep should be immediately ready (dep treated as completed)")
	assert.Equal(t, int64(2), ta.ID)
}

// ---------------------------------------------------------------------------
// InFlightCount
// ---------------------------------------------------------------------------

func TestDepGraph_InFlightCount(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	assert.Equal(t, 0, dg.InFlightCount())

	dg.Add(&Action{
		Type: ActionDownload, Path: "a.txt",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	assert.Equal(t, 1, dg.InFlightCount())

	dg.Add(&Action{
		Type: ActionDownload, Path: "b.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, nil)

	assert.Equal(t, 2, dg.InFlightCount())

	dg.Complete(1)
	assert.Equal(t, 1, dg.InFlightCount(), "should decrease after Complete")

	dg.Complete(2)
	assert.Equal(t, 0, dg.InFlightCount(), "should be zero when all completed")
}

// ---------------------------------------------------------------------------
// Concurrent multi-add (race detector coverage)
// ---------------------------------------------------------------------------

func TestDepGraph_ConcurrentMultiAdd(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// 10 goroutines each add 50 independent actions (unique IDs).
	var wg stdsync.WaitGroup

	for g := range 10 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range 50 {
				id := int64(g*100 + i)
				dg.Add(&Action{
					Type:    ActionDownload,
					Path:    fmt.Sprintf("file-%d-%d", g, i),
					DriveID: driveid.New("d"),
					ItemID:  fmt.Sprintf("i%d", id),
				}, id, nil)
			}
		}()
	}

	wg.Wait()

	assert.Equal(t, 500, dg.InFlightCount(),
		"all 500 actions should be in-flight after concurrent adds")
}

// ---------------------------------------------------------------------------
// Concurrent HasInFlight during Complete (race detector coverage)
// ---------------------------------------------------------------------------

func TestDepGraph_ConcurrentHasInFlightDuringComplete(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Add 100 independent actions.
	for i := range int64(100) {
		dg.Add(&Action{
			Type:    ActionDownload,
			Path:    fmt.Sprintf("file-%d", i),
			DriveID: driveid.New("d"),
			ItemID:  fmt.Sprintf("i%d", i),
		}, i, nil)
	}

	var wg stdsync.WaitGroup
	wg.Add(2)

	// Goroutine 1: complete all actions sequentially.
	go func() {
		defer wg.Done()

		for i := range int64(100) {
			dg.Complete(i)
		}
	}()

	// Goroutine 2: call HasInFlight in a tight loop.
	go func() {
		defer wg.Done()

		for i := range int64(100) {
			dg.HasInFlight(fmt.Sprintf("file-%d", i))
		}
	}()

	wg.Wait()

	assert.Equal(t, 0, dg.InFlightCount(),
		"all actions should be completed")
}

// ---------------------------------------------------------------------------
// Concurrent CancelByPath and Complete (race detector coverage)
// ---------------------------------------------------------------------------

func TestDepGraph_ConcurrentCancelAndComplete(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Add 50 independent actions.
	for i := range int64(50) {
		ta := dg.Add(&Action{
			Type:    ActionDownload,
			Path:    fmt.Sprintf("file-%d", i),
			DriveID: driveid.New("d"),
			ItemID:  fmt.Sprintf("i%d", i),
		}, i, nil)

		// Set a Cancel func so CancelByPath has something to call.
		_, cancel := context.WithCancel(context.Background())
		ta.Cancel = cancel
	}

	var wg stdsync.WaitGroup
	wg.Add(2)

	// Goroutine 1: cancel by path.
	go func() {
		defer wg.Done()

		for i := range int64(50) {
			dg.CancelByPath(fmt.Sprintf("file-%d", i))
		}
	}()

	// Goroutine 2: complete by ID.
	go func() {
		defer wg.Done()

		for i := range int64(50) {
			dg.Complete(i)
		}
	}()

	wg.Wait()

	// Both goroutines race on the same set — some will be canceled first,
	// some completed first. Either way, the graph should be drained.
	assert.Equal(t, 0, dg.InFlightCount(),
		"all actions should be removed from the graph")
}

// ---------------------------------------------------------------------------
// D-10 regression: completed dep treated as satisfied for new action
// ---------------------------------------------------------------------------

func TestDepGraph_D10_CompletedDepSatisfiedForNewAction(t *testing.T) {
	t.Parallel()

	dg := NewDepGraph(testLogger(t))

	// Add and complete action 1.
	dg.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)
	dg.Complete(1)

	// Now add action 2 depending on action 1.
	// Since action 1 was completed AND deleted from actions, the dep
	// lookup should find nothing and treat it as satisfied.
	ta := dg.Add(&Action{
		Type: ActionDownload, Path: "dir/file.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	require.NotNil(t, ta,
		"D-10: new action depending on completed ID must be immediately ready")
	assert.Equal(t, int64(2), ta.ID)
}
