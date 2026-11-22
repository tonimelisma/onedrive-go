package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.33
func TestWatchRuntime_RunNonDrainingWatchStep_BootstrapRetryTickReducesReleasedPublicationRetryOnEngineSide(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	row := syncdomain.RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   syncdomain.ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []syncdomain.RetryWorkRow{row}})

	publication := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, syncdomain.ScopeKey{}, now.Add(-time.Second))
	rt.kickRetryHeldReleaseNow()
	setWatchPhase(rt, watchPhaseBootstrap)

	done, err := rt.runNonDrainingWatchStep(ctx, &watchPipeline{bl: bl}, nil)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Empty(t, rt.currentOutbox(), "bootstrap retry release must re-enter publication drain before worker dispatch")
	assert.Empty(t, rt.retries.heldByKey)
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, ctx))

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found)
}

// Validates: R-2.10.33
func TestWatchRuntime_HandleWatchActionCompletion_DrainsPublicationOnlyDependentsDuringBootstrap(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &syncdomain.BaselineMutation{
		Action:   syncdomain.ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: syncdomain.ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	root := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "sync.txt",
		DriveID: eng.driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, root)

	dependent := rt.sched.graph.Add(&syncdomain.Action{
		Type:    syncdomain.ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent, "cleanup dependent should wait on its parent before bootstrap completion")
	setWatchPhase(rt, watchPhaseBootstrap)

	err = rt.handleWatchActionCompletion(ctx, &watchPipeline{
		bl: bl,
	}, &syncdomain.ActionCompletion{
		Path:       "sync.txt",
		ItemID:     "sync-item",
		DriveID:    eng.driveID,
		ActionType: syncdomain.ActionDownload,
		Success:    true,
		ActionID:   1,
	})
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox(), "bootstrap completion should drain publication-only dependents on the engine side")
	assert.Equal(t, 0, rt.sched.graph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found, "cleanup publication should commit immediately during bootstrap completion")
}
