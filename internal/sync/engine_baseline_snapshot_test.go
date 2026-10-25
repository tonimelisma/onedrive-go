package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.17.2
func TestPlanning_UsesBaselineFromTheComparisonSnapshot(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	// The run's cached baseline is loaded first; a row then reaches SQLite
	// without passing through that cache (as when a commit lands between a
	// run's cached view and its planning transaction).
	cached, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	_, err = eng.baseline.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type) VALUES ('item-fm2', 'fm2', 'folder')`)
	require.NoError(t, err)
	_, found := cached.GetByPath("fm2")
	require.False(t, found)

	tx, err := beginPerfTx(ctx, eng.baseline.db)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, tx.Rollback())
	}()
	inputs, err := flow.loadCurrentInputsTx(ctx, eng.baseline, tx, eng.driveID)
	require.NoError(t, err)

	// Before: "sync: missing baseline entry for comparison path" tore down
	// the drive runner.
	plan, err := eng.buildCurrentActionPlanFromInputs(&inputs, cached, SyncBidirectional)
	require.NoError(t, err)
	assert.NotNil(t, plan)
}

// Validates: R-2.17.2
func TestCommitMutation_FolderMoveKeepsHeldBaselinePointerCurrent(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New("d")

	// Watch holds the pointer Load returned at startup for its whole run.
	held, err := mgr.Load(ctx)
	require.NoError(t, err)

	for _, m := range []BaselineMutation{
		{Action: ActionFolderCreate, Success: true, Path: "movesrc", DriveID: driveID, ItemID: "dir", ItemType: ItemTypeFolder},
		{Action: ActionUpload, Success: true, Path: "movesrc/a.txt", DriveID: driveID, ItemID: "a", ItemType: ItemTypeFile},
		{
			Action: ActionRemoteMove, Success: true, Path: "movedst", OldPath: "movesrc",
			DriveID: driveID, ItemID: "dir", ItemType: ItemTypeFolder,
		},
		// The commit after the folder move must still reach the held pointer.
		{Action: ActionFolderCreate, Success: true, Path: "fm2", DriveID: driveID, ItemID: "fm2", ItemType: ItemTypeFolder},
	} {
		require.NoError(t, mgr.CommitMutation(ctx, &m))
	}

	for _, p := range []string{"movedst", "movedst/a.txt", "fm2"} {
		_, ok := held.GetByPath(p)
		assert.True(t, ok, "held baseline lacks %s", p)
	}
	_, ok := held.GetByPath("movesrc/a.txt")
	assert.False(t, ok)
	current, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Same(t, held, current, "Load must keep returning the pointer callers hold")
}
