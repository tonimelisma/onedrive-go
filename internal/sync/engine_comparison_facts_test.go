package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// renumberedDeviceOffset fabricates a device number no test host uses.
const renumberedDeviceOffset = 1 << 40

// Validates: R-2.2.6
//
// A device renumbered across a reboot (this has happened on real hosts whose
// disks re-letter) gives every synced entry a new filesystem identity. That is
// not an edit: nothing transfers, and the baseline learns the new identity so
// later renames still pair.
func TestRunOnce_RenumberedDeviceTransfersNothing(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "root", "b.txt", "bravo\n")

	eng, _ := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	ctx := t.Context()
	var observedDevice int64
	require.NoError(t, eng.baseline.rawDB().QueryRowContext(ctx,
		`SELECT local_device FROM baseline WHERE path = 'b.txt'`).Scan(&observedDevice))
	_, err := eng.baseline.rawDB().ExecContext(ctx,
		`UPDATE baseline SET local_device = ? WHERE local_has_identity = 1`, observedDevice+renumberedDeviceOffset)
	require.NoError(t, err)
	require.NoError(t, eng.baseline.reloadBaselineCache(ctx))

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.FolderCreates+report.LocalDeletes+report.RemoteDeletes,
		"a new device number is not a content change")
	assert.Zero(t, remote.uploads)

	var stale int
	require.NoError(t, eng.baseline.rawDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM baseline WHERE local_has_identity = 1 AND local_device <> ?`, observedDevice).Scan(&stale))
	assert.Zero(t, stale, "the baseline must learn the observed identity")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.2.6
//
// rm -rf docs followed by restoring the same tree (from a backup, a git
// checkout) gives the folder and its files new inodes. The folder used to be
// planned as an "upload" of a directory and superseded forever.
func TestRunOnce_FolderDeletedAndRestoredLocallyTransfersNothing(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "alpha\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "docs")))
	writeLocalFile(t, syncRoot, filepath.Join("docs", "a.txt"), "alpha\n")

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.FolderCreates+report.LocalDeletes+report.RemoteDeletes,
		"a restored identical tree transfers nothing")
	assert.Zero(t, remote.uploads)
	assert.Empty(t, remote.deletes)
	assert.Empty(t, remote.folderCreates)
	requireScenarioQuietPlan(t, eng)
}
