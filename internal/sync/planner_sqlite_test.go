package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.1, R-2.1.3, R-2.1.4
func TestPlannerPlanCurrentState_BuildsActionsFromSQLiteReconciliation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 'old', 1, 1, 1, 1, 'etag-old'),
			('item-folder', 'folder', 'folder', '', '', NULL, NULL, NULL, NULL, NULL)`)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:            "upload.txt",
			ItemType:        ItemTypeFile,
			Hash:            "new-local",
			Size:            2,
			Mtime:           2,
			ContentIdentity: "new-local",
			ObservedAt:      1,
		},
		{
			Path:       "new-folder",
			ItemType:   ItemTypeFolder,
			ObservedAt: 1,
		},
	}))

	require.NoError(t, store.CommitObservation(ctx, []ObservedItem{
		{
			DriveID:  driveID,
			ItemID:   "item-upload",
			Path:     "upload.txt",
			ItemType: ItemTypeFile,
			Hash:     "old",
			Size:     1,
			Mtime:    1,
			ETag:     "etag-old",
		},
		{
			DriveID:  driveID,
			ItemID:   "item-folder",
			Path:     "folder",
			ItemType: ItemTypeFolder,
		},
	}, "", driveID))

	bl, err := store.Load(ctx)
	require.NoError(t, err)

	comparisons, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	reconciliations, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	localRows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	remoteRows, err := store.ListRemoteState(ctx)
	require.NoError(t, err)

	planner := NewPlanner(testLogger(t))
	plan, err := planner.PlanCurrentState(
		comparisons,
		reconciliations,
		localRows,
		remoteRows,
		bl,
		SyncBidirectional,
		&SafetyConfig{},
	)
	require.NoError(t, err)

	require.Len(t, plan.Actions, 3)
	byPath := make(map[string]Action, len(plan.Actions))
	for _, action := range plan.Actions {
		byPath[action.Path] = action
	}

	assert.Equal(t, ActionFolderCreate, byPath["folder"].Type)
	assert.Equal(t, CreateLocal, byPath["folder"].CreateSide)
	assert.Equal(t, ActionFolderCreate, byPath["new-folder"].Type)
	assert.Equal(t, CreateRemote, byPath["new-folder"].CreateSide)
	assert.Equal(t, ActionUpload, byPath["upload.txt"].Type)
}
