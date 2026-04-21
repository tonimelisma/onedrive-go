package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestWatchDispatchEvent_ActionCompletionDrainsPublicationOnlyDependents(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	root := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "sync.txt",
		DriveID: driveID,
		ItemID:  "sync-item",
	}, 1, nil)
	require.NotNil(t, root)

	dependent := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 2, []int64{1})
	assert.Nil(t, dependent, "cleanup dependent should wait on its parent before completion")

	p := &watchPipeline{bl: bl}
	done, handled, err := rt.handleDispatchEvent(ctx, p, &watchEvent{
		kind: watchEventActionCompletion,
		completion: &ActionCompletion{
			Path:       "sync.txt",
			ItemID:     "sync-item",
			DriveID:    driveID,
			ActionType: ActionDownload,
			Success:    true,
			ActionID:   1,
		},
	})
	require.True(t, handled)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Empty(t, rt.currentOutbox(), "publication-only dependents should drain on the engine side")
	assert.Equal(t, 0, rt.depGraph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found, "cleanup publication should commit immediately instead of waiting for worker dispatch")
}

func TestReducePublicationFrontier_DoesNotReleaseUnrelatedHeldWork(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	rt := testWatchRuntime(t, eng)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	require.NoError(t, eng.baseline.CommitMutation(ctx, &BaselineMutation{
		Action:   ActionDownload,
		Success:  true,
		Path:     "cleanup.txt",
		DriveID:  driveID,
		ItemID:   "cleanup-item",
		ParentID: "root",
		ItemType: ItemTypeFile,
	}))

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	rt.initializePreparedRuntime(&PreparedCurrentPlan{})

	publication := rt.depGraph.Add(&Action{
		Type:    ActionCleanup,
		Path:    "cleanup.txt",
		DriveID: driveID,
		ItemID:  "cleanup-item",
	}, 1, nil)
	require.NotNil(t, publication)

	unlocked := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "after.txt",
		DriveID: driveID,
		ItemID:  "after-item",
	}, 2, []int64{1})
	assert.Nil(t, unlocked)

	held := rt.depGraph.Add(&Action{
		Type:    ActionDownload,
		Path:    "held.txt",
		DriveID: driveID,
		ItemID:  "held-item",
	}, 3, nil)
	require.NotNil(t, held)
	rt.holdAction(held, heldReasonRetry, ScopeKey{}, eng.nowFunc().Add(-time.Second))

	outbox, err := rt.reducePublicationFrontier(ctx, rt, bl, nil, []*TrackedAction{publication})
	require.NoError(t, err)
	require.Len(t, outbox, 1)
	assert.Equal(t, int64(2), outbox[0].ID, "publication reduction should only enqueue dependents unlocked by publication success")
	assert.Contains(t, rt.heldByKey, retryWorkKeyForAction(&held.Action), "unrelated held retry work should not be released by publication reduction")
}
