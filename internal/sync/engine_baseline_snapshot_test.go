package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.17.2
func TestPlanning_UsesBaselineFromTheComparisonSnapshot(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	flow := testEngineFlow(t, eng)
	ctx := t.Context()

	// The run's cached baseline is loaded first; a row then reaches SQLite
	// without passing through that cache (as when a commit lands between a
	// run's cached view and its planning transaction).
	cached, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	_, err = eng.baseline.RawDBForTest().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type) VALUES ('item-fm2', 'fm2', 'folder')`)
	require.NoError(t, err)
	_, found := cached.GetByPath("fm2")
	require.False(t, found)

	tx, err := eng.baseline.BeginPerfTx(ctx)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, tx.Rollback())
	}()
	inputs, err := flow.loadCurrentInputsTx(ctx, tx, eng.driveID)
	require.NoError(t, err)

	// The planner takes the snapshot type alone, so the cached view cannot
	// be handed to it. Before: "sync: missing baseline entry for comparison
	// path" tore down the drive runner.
	plan, err := eng.buildCurrentActionPlanFromInputs(&inputs, syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.NotNil(t, plan)
}

// Validates: R-2.17.2, R-6.3.4
//
// Watch holds the view Load returned at startup for its whole run: every
// commit, a folder move's reload included, must reach it. What a lookup
// returned is a value, so a commit never changes an entry a reader already
// holds.
func TestCommitMutation_FolderMoveKeepsHeldBaselineViewCurrent(t *testing.T) {
	t.Parallel()

	mgr := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	driveID := driveid.New("d")

	held, err := mgr.Load(ctx)
	require.NoError(t, err)
	commit := func(m syncdomain.BaselineMutation) {
		t.Helper()
		require.NoError(t, mgr.CommitMutation(ctx, &m))
	}

	commit(syncdomain.BaselineMutation{Action: syncdomain.ActionFolderCreate, Success: true, Path: "movesrc", DriveID: driveID, ItemID: "dir", ItemType: syncdomain.ItemTypeFolder})
	commit(syncdomain.BaselineMutation{Action: syncdomain.ActionUpload, Success: true, Path: "movesrc/a.txt", DriveID: driveID, ItemID: "a", ItemType: syncdomain.ItemTypeFile})
	beforeMove, ok := held.GetByID("a")
	require.True(t, ok)

	commit(syncdomain.BaselineMutation{
		Action: syncdomain.ActionRemoteMove, Success: true, Path: "movedst", OldPath: "movesrc",
		DriveID: driveID, ItemID: "dir", ItemType: syncdomain.ItemTypeFolder,
	})
	// The commit after the folder move must still reach the held view.
	commit(syncdomain.BaselineMutation{Action: syncdomain.ActionFolderCreate, Success: true, Path: "fm2", DriveID: driveID, ItemID: "fm2", ItemType: syncdomain.ItemTypeFolder})

	for _, p := range []string{"movedst", "movedst/a.txt", "fm2"} {
		_, found := held.GetByPath(p)
		assert.True(t, found, "held baseline lacks %s", p)
	}
	_, found := held.GetByPath("movesrc/a.txt")
	assert.False(t, found)
	afterMove, ok := held.GetByID("a")
	require.True(t, ok)
	assert.Equal(t, "movedst/a.txt", afterMove.Path)
	assert.Equal(t, "movesrc/a.txt", beforeMove.Path, "a value read before the move is not rewritten under its reader")

	current, err := mgr.Load(ctx)
	require.NoError(t, err)
	assert.Same(t, held, current, "Load must keep returning the view callers hold")
}
