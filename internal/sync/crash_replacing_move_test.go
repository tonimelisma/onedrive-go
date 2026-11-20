package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// requireBaselineItemAt asserts that the baseline holds itemID at path with
// the local hash want.
func requireBaselineItemAt(t *testing.T, bl *syncdomain.Baseline, path, itemID, localHash string) {
	t.Helper()

	entry, ok := bl.GetByPath(path)
	require.True(t, ok, "baseline row for %s", path)
	assert.Equal(t, itemID, entry.ItemID)
	assert.Equal(t, localHash, entry.LocalHash)
}

// Validates: R-6.5.2, R-2.17.9
//
// A crash between the two steps of a replacing local move: OneDrive moved
// a.txt over b.txt, and the move here took the old b.txt into the trash but
// did not rename a.txt. The crash leaves b.txt absent, its old content in
// the trash, a.txt at its old path and the state database as it was before
// the pass. The next plan finishes the move, deletes nothing on OneDrive,
// makes no conflict copy and uploads nothing, and the old b.txt stays in the
// trash, once.
func TestCrashRecovery_ReplacingMoveBetweenItsTwoStepsFinishesTheMove(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "root", "b.txt", "bravo\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)
	before, err := h.eng.baseline.Load(t.Context())
	require.NoError(t, err)
	aRow, ok := before.GetByPath("a.txt")
	require.True(t, ok)
	bRow, ok := before.GetByPath("b.txt")
	require.True(t, ok)

	remote.deleteRemotely("item-b")
	remote.move("item-a", "root", "b.txt")
	capture := &crashCapture{
		point:  syncdomain.CrashPointLocalMoveDestinationTrashed,
		srcDir: filepath.Dir(h.syncRoot), dstDir: t.TempDir(), remote: remote,
	}
	h.eng.execCfg.SetCrashHookForTest(capture.hook)
	_, err = h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	recovered := capture.reopen(t)

	assert.Equal(t, map[string]string{"a.txt": "alpha\n", "keep.txt": "keep\n"}, harnessLocalTree(t, recovered.syncRoot))
	syncfixture.RequireTrashHoldsOnce(t, recovered.eng.trash, "b.txt", "bravo\n")
	crashed, err := recovered.eng.baseline.Load(t.Context())
	require.NoError(t, err)
	requireBaselineItemAt(t, crashed, "a.txt", aRow.ItemID, aRow.LocalHash)
	requireBaselineItemAt(t, crashed, "b.txt", bRow.ItemID, bRow.LocalHash)

	recovered.eng.oneShotPassLimit = 1
	report := runScenarioPass(t, recovered.eng)

	assert.Equal(t, 1, report.Moves, "the next plan finishes the move")
	assert.Zero(t, report.RemoteDeletes, "nothing is deleted on OneDrive")
	assert.Zero(t, report.ConflictCopies)
	assert.Zero(t, report.Uploads)
	assert.Zero(t, report.Downloads)
	assert.Zero(t, report.Failed)
	assert.NotContains(t, recovered.remote.deletes, "item-a")
	assert.Equal(t, map[string]string{"b.txt": "alpha\n", "keep.txt": "keep\n"}, recovered.remote.tree())
	assert.Equal(t, map[string]string{"b.txt": "alpha\n", "keep.txt": "keep\n"}, harnessLocalTree(t, recovered.syncRoot))
	syncfixture.RequireTrashHoldsOnce(t, recovered.eng.trash, "b.txt", "bravo\n")
	after, err := recovered.eng.baseline.Load(t.Context())
	require.NoError(t, err)
	requireBaselineItemAt(t, after, "b.txt", aRow.ItemID, aRow.LocalHash)
	requireScenarioQuietPlan(t, recovered.eng)
}
