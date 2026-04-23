package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.33
func TestWatchRuntime_RunNonDrainingWatchStep_BootstrapRetryTickReducesReleasedPublicationRetryOnEngineSide(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	now := eng.nowFunc()

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	row := RetryWorkRow{
		Path:         "cleanup.txt",
		ActionType:   ActionCleanup,
		AttemptCount: 1,
		NextRetryAt:  now.Add(-time.Second).UnixNano(),
	}
	require.NoError(t, eng.baseline.UpsertRetryWork(ctx, &row))
	rt.initializeRuntimeState(&runtimePlan{RetryRows: []RetryWorkRow{row}})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)
	rt.holdAction(publication, heldReasonRetry, ScopeKey{}, now.Add(-time.Second))
	rt.kickRetryHeldReleaseNow()
	rt.enterBootstrap()

	done, err := rt.runNonDrainingWatchStep(ctx, &watchPipeline{bl: bl}, nil)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Empty(t, rt.currentOutbox(), "bootstrap retry release must re-enter publication drain before worker dispatch")
	assert.Empty(t, rt.heldByKey)
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

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  eng.driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	root := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "sync.txt",
		DriveID: eng.driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, root)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: eng.driveID,
		ItemID:  "cleanup-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent, "cleanup dependent should wait on its parent before bootstrap completion")
	rt.enterBootstrap()

	err = rt.handleWatchActionCompletion(ctx, &watchPipeline{
		bl: bl,
	}, &ActionCompletion{
		Path:       "sync.txt",
		ItemID:     "sync-item",
		DriveID:    eng.driveID,
		ActionType: ActionDownload,
		Success:    true,
		ActionID:   1,
	})
	require.NoError(t, err)
	assert.Empty(t, rt.currentOutbox(), "bootstrap completion should drain publication-only dependents on the engine side")
	assert.Equal(t, 0, rt.depGraph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found, "cleanup publication should commit immediately during bootstrap completion")
}
