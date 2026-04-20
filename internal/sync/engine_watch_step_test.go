package sync

import (
	"testing"

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
	transition, handled, err := rt.transitionWatchDispatchEvent(ctx, p, &watchEvent{
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

	done, err := rt.applyWatchTransition(ctx, p, transition)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Empty(t, rt.currentOutbox(), "publication-only dependents should drain on the engine side")
	assert.Equal(t, 0, rt.depGraph.InFlightCount())

	_, found := bl.GetByPath("cleanup.txt")
	assert.False(t, found, "cleanup publication should commit immediately instead of waiting for worker dispatch")
}
