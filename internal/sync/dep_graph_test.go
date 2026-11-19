package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// Add — no dependencies
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestDepGraph_Add_NoDeps(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

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

// Validates: R-2.10.5
func TestDepGraph_Add_WithDeps(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

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

// Validates: R-2.10.5
func TestDepGraph_Complete_ReturnsDependents(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

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
// Complete deletes finished actions from the tracked actions map.
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestDepGraph_Complete_DeletesFromActions(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

	dg.Add(&Action{
		Type: ActionFolderCreate, Path: "dir",
		DriveID: driveid.New("d"), ItemID: "i1",
	}, 1, nil)

	_, ok := dg.Complete(1)
	require.True(t, ok)

	// After Complete, action 1 should be gone from the actions map.
	// InFlightCount uses len(actions), so it should be 0.
	assert.Equal(t, 0, dg.InFlightCount(),
		"Complete must delete from actions map")

	// A new action depending on the completed ID should treat it as
	// satisfied (dep not found in actions → skip → depsLeft stays 0).
	ta := dg.Add(&Action{
		Type: ActionDownload, Path: "dir/file.txt",
		DriveID: driveid.New("d"), ItemID: "i2",
	}, 2, []int64{1})

	require.NotNil(t, ta,
		"action depending on completed (and deleted) ID should be immediately ready")
}

// ---------------------------------------------------------------------------
// Complete — unknown ID
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestDepGraph_Complete_UnknownID(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

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

// Validates: R-2.10.5
func TestDepGraph_Complete_UnknownID_NoPanic(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

	// Should not panic on unknown ID with zero tracked actions.
	ready, ok := dg.Complete(999)
	assert.False(t, ok)
	assert.Nil(t, ready)
}

// ---------------------------------------------------------------------------
// SkipCompletedDeps — dependency not in actions map
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestDepGraph_SkipCompletedDeps(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

	// Action 2 depends on action 1, but action 1 is not in the graph
	// (simulating it was already completed before graph was populated).
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

// Validates: R-2.10.5
func TestDepGraph_InFlightCount(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

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
// Regression: completed dependencies must stay satisfied for later actions.
// ---------------------------------------------------------------------------

// Validates: R-2.10.5
func TestDepGraph_D10_CompletedDepSatisfiedForNewAction(t *testing.T) {
	t.Parallel()

	dg := newDepGraph(synctest.TestLogger(t))

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
		"new action depending on completed ID must be immediately ready")
	assert.Equal(t, int64(2), ta.ID)
}
