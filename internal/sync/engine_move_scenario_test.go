package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// requireScenarioBaselineMoved asserts a folder move re-pathed the subtree's
// baseline rows: left at the old paths, every descendant replans as a move
// whose source is gone and is superseded forever.
func requireScenarioBaselineMoved(t *testing.T, eng *testEngine, oldPaths, newPaths []string) {
	t.Helper()

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	for _, p := range oldPaths {
		_, found := bl.GetByPath(p)
		assert.False(t, found, "baseline still holds pre-move path %s", p)
	}
	for _, p := range newPaths {
		_, found := bl.GetByPath(p)
		assert.True(t, found, "baseline lacks moved path %s", p)
	}
}

// requireScenarioQuietPlan asserts the next plan from current truth is empty.
func requireScenarioQuietPlan(t *testing.T, eng *testEngine) {
	t.Helper()

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates+report.Moves,
		"converged state must plan nothing: %+v", report)
}

func runScenarioPass(t *testing.T, eng *testEngine) *Report {
	t.Helper()

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 0, report.Failed, "errors: %v", report.Errors)

	return report
}

// Validates: R-2.17.1, R-2.17.2
func TestRunOnce_LocalFolderMoveBecomesOneRemoteMoveWithoutResurrectingSource(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-deep", "root", "deep")
	remote.addFolder("item-er", "item-deep", "er")
	remote.addFolder("item-still", "item-er", "still deeper")
	remote.addFile(t, "item-leaf", "item-still", "leaf.md", "leaf\n")
	remote.addFolder("item-target", "root", "ünïcødé 日本語 🎉")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "deep", "er", "still deeper", "leaf.md"))

	require.NoError(t, os.Rename(
		filepath.Join(syncRoot, "deep"),
		filepath.Join(syncRoot, "ünïcødé 日本語 🎉", "moved-deep"),
	))

	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-deep->moved-deep"}, remote.moves, "one remote folder move carries the subtree")
	assert.Empty(t, remote.folderCreates)
	assert.Empty(t, remote.deletes)
	assert.Zero(t, remote.uploads)
	assert.NoDirExists(t, filepath.Join(syncRoot, "deep"), "the moved source tree must not be re-created locally")
	assert.FileExists(t, filepath.Join(syncRoot, "ünïcødé 日本語 🎉", "moved-deep", "er", "still deeper", "leaf.md"))

	requireScenarioBaselineMoved(t, eng,
		[]string{"deep", "deep/er", "deep/er/still deeper", "deep/er/still deeper/leaf.md"},
		[]string{
			"ünïcødé 日本語 🎉/moved-deep", "ünïcødé 日本語 🎉/moved-deep/er",
			"ünïcødé 日本語 🎉/moved-deep/er/still deeper", "ünïcødé 日本語 🎉/moved-deep/er/still deeper/leaf.md",
		})
	requireScenarioQuietPlan(t, eng)

	// A converged pass afterwards must be quiet.
	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates)
	assert.NoDirExists(t, filepath.Join(syncRoot, "deep"))
}

// Validates: R-2.17.1
func TestRunOnce_LegacyFolderBaselineGainsIdentityAndMovesAsOne(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-old", "root", "old")
	remote.addFolder("item-sub", "item-old", "sub")
	remote.addFile(t, "item-leaf", "item-sub", "leaf.md", "leaf\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// Simulate a state database written before folder baselines recorded
	// their filesystem identity.
	ctx := t.Context()
	_, err := eng.baseline.rawDB().ExecContext(ctx, `
		UPDATE baseline SET local_device = 0, local_inode = 0, local_has_identity = 0
		WHERE item_type = 'folder'`)
	require.NoError(t, err)
	require.NoError(t, eng.baseline.reloadBaselineCache(ctx))

	// The next full scan backfills identity without changing anything.
	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "old"), filepath.Join(syncRoot, "new")))
	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-old->new"}, remote.moves, "a legacy folder must move as one item once its identity is known")
	assert.Empty(t, remote.folderCreates)
	assert.Empty(t, remote.deletes)
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2
func TestRunOnce_LocalFolderDeleteDeletesRemoteFolder(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-many", "root", "many")
	// A file outside the deleted folder keeps that side from being empty,
	// which the delete-safety hold would hold for confirmation instead.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	for i := range 3 {
		remote.addFile(t, fmt.Sprintf("item-f%03d", i), "item-many", fmt.Sprintf("f%03d.txt", i), fmt.Sprintf("file %d\n", i))
	}

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "many", "f000.txt"))

	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "many")))
	runScenarioPass(t, eng)

	assert.Contains(t, remote.deletes, "item-many", "the deleted folder must be deleted on OneDrive")
	assert.NoDirExists(t, filepath.Join(syncRoot, "many"), "the deleted folder must not be re-created locally")

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.FolderCreates)
	assert.NoDirExists(t, filepath.Join(syncRoot, "many"))
}

// Validates: R-2.17.3
func TestRunOnce_RemoteRenameAppliedLocallyDoesNotReupload(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "ünïcødé 日本語 🎉")
	remote.addFile(t, "item-renamed", "root", "renamed.txt", "ok")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "renamed.txt"))

	remote.move("item-renamed", "item-dir", "renamed-remotely.txt")
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "ünïcødé 日本語 🎉", "renamed-remotely.txt"))
	require.NoFileExists(t, filepath.Join(syncRoot, "renamed.txt"))

	report := runScenarioPass(t, eng)
	assert.Zero(t, remote.uploads, "unchanged content moved by the server must not be uploaded again")
	assert.Zero(t, report.Uploads+report.Downloads)
}

// Validates: R-2.17.5
func TestRunOnce_LocalRenameKeepsTheRemoteModificationTime(t *testing.T) {
	t.Parallel()

	// Manual test D-150: renaming a synced file locally left its OneDrive
	// copy with the rename's time as its modification time, because a
	// rename PATCH that does not carry fileSystemInfo resets it (L-09).
	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-old", "root", "old-name.txt", "keep my time\n")
	before, err := remote.getItem("item-old")
	require.NoError(t, err)

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "old-name.txt"), filepath.Join(syncRoot, "new-name.txt")))
	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-old->new-name.txt"}, remote.moves)
	after, err := remote.getItem("item-old")
	require.NoError(t, err)
	assert.True(t, after.ModifiedAt.Equal(before.ModifiedAt),
		"the rename must keep the modification time: was %s, now %s", before.ModifiedAt, after.ModifiedAt)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.4
func TestRunOnce_LocalFileReplacedByFolderConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-lower", "root", "lowercase.txt", "x\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "lowercase.txt"))

	require.NoError(t, os.Remove(filepath.Join(syncRoot, "lowercase.txt")))
	writeLocalFile(t, syncRoot, filepath.Join("lowercase.txt", "inside.txt"), "\n")
	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-lower"}, remote.deletes, "the old file is replaced, not overwritten with a directory")
	assert.Equal(t, []string{"lowercase.txt"}, remote.folderCreates)
	assert.Equal(t, 1, remote.uploads)

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates,
		"a replaced item must converge, not stall or churn")
}

// Validates: R-2.17.1
func TestRunOnce_LocalFolderMoveSurvivesFolderETagChurn(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	eng, syncRoot := newTestEngine(t, remote.client(t))

	// Created locally, so the folders are created remotely before their
	// children are uploaded into them: the recorded folder eTags go stale.
	writeLocalFile(t, syncRoot, filepath.Join("movesrc", "inner", "a.txt"), "a\n")
	runScenarioPass(t, eng)
	require.Len(t, remote.folderCreates, 2)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "movesrc"), filepath.Join(syncRoot, "movedst")))
	runScenarioPass(t, eng)

	assert.Equal(t, []string{"created-folder-1->movedst"}, remote.moves)
	assert.Empty(t, remote.deletes)
	assert.Equal(t, 1, remote.uploads)
	requireScenarioBaselineMoved(t, eng,
		[]string{"movesrc", "movesrc/inner", "movesrc/inner/a.txt"},
		[]string{"movedst", "movedst/inner", "movedst/inner/a.txt"})

	// Delta now reports the children under movedst/ and local agrees: the
	// next passes must converge without planning a child move.
	requireScenarioQuietPlan(t, eng)
	runScenarioPass(t, eng)
	assert.Equal(t, []string{"created-folder-1->movedst"}, remote.moves, "no descendant move after the folder move")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.2.3
func TestRunOnce_TimestampOnlyLocalChangeRefreshesBaselineWithoutTransfer(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-touch", "root", "touch.txt", "touch me\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// touch -d 2020-01-01: same content, new mtime.
	old := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(syncRoot, "touch.txt"), old, old))

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads, "a timestamp-only change transfers nothing")
	assert.Zero(t, remote.uploads)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByPath("touch.txt")
	require.True(t, ok)
	assert.Equal(t, old.UnixNano(), entry.LocalMtime,
		"the baseline must learn the new mtime, or every scan re-hashes the file")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.3
func TestRunOnce_RemoteFolderRenameMovesExistingChildrenOnce(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-links", "root", "links")
	remote.addFolder("item-sub", "item-links", "sub")
	remote.addFile(t, "item-a", "item-links", "a.txt", "a\n")
	remote.addFile(t, "item-b", "item-sub", "b.txt", "b\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "links", "sub", "b.txt"))

	// Renamed on OneDrive: the next delta reports only the folder.
	remote.move("item-links", "root", "links-renamed")
	runScenarioPass(t, eng)

	assert.NoDirExists(t, filepath.Join(syncRoot, "links"), "the old folder must not come back")
	assert.FileExists(t, filepath.Join(syncRoot, "links-renamed", "a.txt"))
	assert.FileExists(t, filepath.Join(syncRoot, "links-renamed", "sub", "b.txt"))
	assert.Empty(t, remote.folderCreates, "nothing is re-created on OneDrive")
	assert.Empty(t, remote.moves, "the engine moves nothing back")
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2
func TestRunOnce_RemoteFolderDeleteRemovesLocalTreeAndLeavesNoDrift(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-probe", "root", "probe")
	remote.addFolder("item-len", "item-probe", "len")
	remote.addFile(t, "item-f", "item-len", "f.txt", "f\n")
	// A file outside the deleted folder keeps that side from being empty,
	// which the delete-safety hold would hold for confirmation instead.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "probe", "len", "f.txt"))

	// Deleted on OneDrive: the next delta reports only the folder's tombstone.
	remote.deleteRemotely("item-probe")
	runScenarioPass(t, eng)

	assert.NoDirExists(t, filepath.Join(syncRoot, "probe"))
	var leftover int
	require.NoError(t, eng.baseline.rawDB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM remote_state WHERE path LIKE 'probe%'`).Scan(&leftover))
	assert.Zero(t, leftover, "no remote_state rows survive under a deleted folder")
	requireScenarioQuietPlan(t, eng)
}
