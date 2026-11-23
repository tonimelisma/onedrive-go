package sync

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// frontierRecorder collects what the recordFrontier hook is told, as
// "<step> <path>" lines. The hook runs on the engine's goroutine, which in
// these tests is the test's own.
type frontierRecorder struct {
	lines []string
}

func (r *frontierRecorder) record(step frontierStep, action syncdomain.Action) {
	r.lines = append(r.lines, fmt.Sprintf("%s %s", step, action.Path))
}

// Validates: R-6.10.31
func TestRecordFrontier_OneActionPassLeavesItsRecord(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	rt.initializeRuntimeState(&runtimePlan{})
	recorder := &frontierRecorder{}
	rt.hooks.recordFrontier = recorder.record

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	only := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "only.txt",
		DriveID: driveid.New(syncfixture.EngineTestDriveID),
		ItemID:  "only-item",
		View:    &syncdomain.PathView{Path: "only.txt"},
	}, 1, nil)
	require.NotNil(t, only)

	frontier, err := rt.reduceReadyFrontierStage(ctx, bl, []*syncdomain.TrackedAction{only})
	require.NoError(t, err)
	require.Len(t, frontier, 1)
	assert.Equal(t, []string{"dispatch only.txt"}, recorder.lines,
		"a frontier of one action is recorded, though no dispatch order is asked to sort it")
}

// Validates: R-6.10.31
func TestRecordFrontier_PublicationOnlyPassLeavesItsRecord(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.initializeRuntimeState(&runtimePlan{})
	recorder := &frontierRecorder{}
	rt.hooks.recordFrontier = recorder.record

	publication := rt.sched.AddForTest(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	frontier, err := rt.reduceReadyFrontierStage(ctx, bl, []*syncdomain.TrackedAction{publication})
	require.NoError(t, err)
	assert.Empty(t, frontier, "a publication-only pass dispatches nothing")
	assert.Equal(t, []string{"publish cleanup.txt"}, recorder.lines,
		"a pass that only publishes still leaves its record")
}

// Validates: R-6.10.31
func TestRecordFrontier_RecordsTheOrderTheFrontierLeavesIn(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	rt.initializeRuntimeState(&runtimePlan{})
	recorder := &frontierRecorder{}
	rt.hooks.dispatchOrder = dispatchOrderReverse
	rt.hooks.recordFrontier = recorder.record

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	var ready []*syncdomain.TrackedAction
	for i, name := range []string{"a.txt", "b.txt"} {
		ta := rt.sched.AddForTest(&syncdomain.Action{
			Type:    syncdomain.ActionDownload,
			Path:    name,
			DriveID: driveID,
			ItemID:  name + "-item",
			View:    &syncdomain.PathView{Path: name},
		}, int64(i+1), nil)
		require.NotNil(t, ta)
		ready = append(ready, ta)
	}

	frontier, err := rt.reduceReadyFrontierStage(ctx, bl, ready)
	require.NoError(t, err)
	require.Len(t, frontier, 2)
	assert.Equal(t, "b.txt", frontier[0].Action.Path)
	assert.Equal(t, "a.txt", frontier[1].Action.Path)
	assert.Equal(t, []string{"dispatch b.txt", "dispatch a.txt"}, recorder.lines,
		"the record is the order the frontier leaves in, after the dispatch order")
}
