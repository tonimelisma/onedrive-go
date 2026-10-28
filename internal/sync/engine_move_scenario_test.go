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

	h := newSyncHarness(t, remote)
	eng, syncRoot := h.eng, h.syncRoot
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "deep", "er", "still deeper", "leaf.md"))

	require.NoError(t, os.Rename(
		filepath.Join(syncRoot, "deep"),
		filepath.Join(syncRoot, "ünïcødé 日本語 🎉", "moved-deep"),
	))

	h.beginRound(t)
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
	assertSyncInvariants(t, h)

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

	h := newSyncHarness(t, remote)
	eng, syncRoot := h.eng, h.syncRoot
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "probe", "len", "f.txt"))

	// Deleted on OneDrive: the next delta reports only the folder's tombstone.
	remote.deleteRemotely("item-probe")
	h.beginRound(t)
	runScenarioPass(t, eng)

	assert.NoDirExists(t, filepath.Join(syncRoot, "probe"))
	var leftover int
	require.NoError(t, eng.baseline.rawDB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM remote_state WHERE path LIKE 'probe%'`).Scan(&leftover))
	assert.Zero(t, leftover, "no remote_state rows survive under a deleted folder")
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.2, R-2.15.3
//
// An item moved into a folder that is then deleted online, both between two
// observations, is in no delta window: delta reports the deleted folder
// alone, and the item's move is part of a subtree delta never repeats. Its
// remote_state row stayed at the old path as a ghost, so the synced local
// copy was never deleted (model test seed 28). The folder's tombstone makes
// the next pass a full refresh, whose orphans are confirmed by ID.
func TestRunOnce_ItemMovedIntoRemotelyDeletedFolderIsDeletedLocally(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-bin", "root", "bin")
	remote.addFile(t, "item-moved", "root", "moved.txt", "moved\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	h := newSyncHarness(t, remote)
	eng, syncRoot := h.eng, h.syncRoot
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "moved.txt"))

	remote.move("item-moved", "item-bin", "moved.txt")
	remote.deleteRemotely("item-bin")
	h.beginRound(t)
	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	assert.NoDirExists(t, filepath.Join(syncRoot, "bin"))
	assert.NoFileExists(t, filepath.Join(syncRoot, "moved.txt"), "the moved item was deleted with the folder")
	assert.FileExists(t, filepath.Join(syncRoot, "keep.txt"))
	var ghosts int
	require.NoError(t, eng.baseline.rawDB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM remote_state WHERE item_id = 'item-moved'`).Scan(&ghosts))
	assert.Zero(t, ghosts, "no remote_state row survives for an item deleted with its new parent")
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.4, R-2.17.10
//
// OneDrive replaced a file with a populated folder while the user edited the
// file (D-059). Both survive: the local edit as a conflict copy that is then
// uploaded, the new folder with its content locally. Before, local won and the
// folder's descendants wedged against the file still at its path.
func TestRunOnce_RemoteFileReplacedByNonEmptyFolderWithLocalEditKeepsBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-shape", "root", "shape", "file\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	h := newSyncHarness(t, remote)
	eng, syncRoot := h.eng, h.syncRoot
	runScenarioPass(t, eng)

	writeLocalFile(t, syncRoot, "shape", "edited locally\n")
	remote.deleteRemotely("item-shape")
	remote.addFolder("item-shape-dir", "root", "shape")
	remote.addFile(t, "item-inner", "item-shape-dir", "inner.txt", "inner\n")

	for range 3 {
		h.beginRound(t)
		runScenarioPass(t, eng)
	}

	tree := remote.tree()
	assert.Equal(t, "/", tree["shape"])
	assert.Equal(t, "inner\n", tree["shape/inner.txt"])
	assert.Contains(t, modelContents(tree), "edited locally\n", "the local edit survives")
	assert.FileExists(t, filepath.Join(syncRoot, "shape", "inner.txt"))
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.4, R-2.17.10
//
// OneDrive replaced a folder with a file while the user edited a file inside
// the folder: the local folder and its edit survive as a conflict copy, the
// new file takes the path.
func TestRunOnce_RemoteFolderReplacedByFileWithLocalChildEditsKeepsBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "shape")
	remote.addFile(t, "item-child", "item-dir", "child.txt", "child\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	writeLocalFile(t, syncRoot, filepath.Join("shape", "child.txt"), "edited child\n")
	remote.deleteRemotely("item-dir")
	remote.addFile(t, "item-shape-file", "root", "shape", "now a file\n")

	for range 3 {
		runScenarioPass(t, eng)
	}

	tree := remote.tree()
	assert.Equal(t, "now a file\n", tree["shape"])
	assert.Contains(t, modelContents(tree), "edited child\n", "the edit inside the replaced folder survives")
	data, err := os.ReadFile(filepath.Join(syncRoot, "shape")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "now a file\n", string(data))
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.4, R-2.17.10
//
// The user replaced a folder with a file while OneDrive edited a file inside
// the folder (GEN-079(b), P-7): the remote edit is not deleted with the
// folder. Local loses the path: its file becomes a conflict copy and the
// folder with the edit comes back.
func TestRunOnce_LocalFolderReplacedByFileWithRemoteChildEditKeepsBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "shape")
	remote.addFile(t, "item-child", "item-dir", "child.txt", "child\n")
	remote.addFile(t, "item-other", "item-dir", "other.txt", "other\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "shape")))
	writeLocalFile(t, syncRoot, "shape", "now a local file\n")
	remote.setContent(t, "item-child", "edited online\n")

	for range 3 {
		runScenarioPass(t, eng)
	}

	tree := remote.tree()
	assert.Equal(t, "edited online\n", tree["shape/child.txt"], "the remote edit is never deleted")
	assert.Equal(t, "other\n", tree["shape/other.txt"])
	assert.Contains(t, modelContents(tree), "now a local file\n", "the local file survives")
	assert.Empty(t, remote.deletes)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.3.1, R-2.17.10
//
// A create/create conflict between a local folder and a remote file (D-059a):
// the conflict copy of a directory is reserved as a directory, so the rename
// succeeds instead of failing with ENOTDIR on every pass.
func TestRunOnce_CreateConflictLocalDirRemoteFileKeepsBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	eng, syncRoot := newTestEngine(t, remote.client(t))

	writeLocalFile(t, syncRoot, filepath.Join("thing", "local.txt"), "local child\n")
	remote.addFile(t, "item-thing", "root", "thing", "remote file\n")

	for range 3 {
		runScenarioPass(t, eng)
	}

	tree := remote.tree()
	assert.Equal(t, "remote file\n", tree["thing"])
	assert.Contains(t, modelContents(tree), "local child\n")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.3.1, R-2.17.10
//
// The mirror: a local file against a remote folder. The local file is kept as
// a conflict copy and the folder is created locally, not "downloaded".
func TestRunOnce_CreateConflictLocalFileRemoteDirKeepsBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	eng, syncRoot := newTestEngine(t, remote.client(t))

	writeLocalFile(t, syncRoot, "thing", "local file\n")
	remote.addFolder("item-thing", "root", "thing")
	remote.addFile(t, "item-child", "item-thing", "child.txt", "remote child\n")

	for range 3 {
		runScenarioPass(t, eng)
	}

	tree := remote.tree()
	assert.Equal(t, "/", tree["thing"])
	assert.Equal(t, "remote child\n", tree["thing/child.txt"])
	assert.Contains(t, modelContents(tree), "local file\n")
	assert.FileExists(t, filepath.Join(syncRoot, "thing", "child.txt"))
	requireScenarioQuietPlan(t, eng)
}

func modelContents(tree map[string]string) []string {
	contents := make([]string, 0, len(tree))
	for _, content := range tree {
		contents = append(contents, content)
	}

	return contents
}

// Validates: R-2.17.4
//
// A populated folder replaced locally by a file converges (found by the
// model test's type-change operation): the deletes of the folder's children
// ran a local-absence check that failed on "parent folder is not a folder",
// so the folder delete waiting for them never ran. Below a local file nothing
// can exist, so the children are absent.
func TestRunOnce_LocalFolderWithChildrenReplacedByFileConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "photos")
	remote.addFile(t, "item-a", "item-dir", "a.txt", "a\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "photos")))
	writeLocalFile(t, syncRoot, "photos", "now a file\n")

	runScenarioPass(t, eng)

	assert.Equal(t, map[string]string{"photos": "now a file\n", "keep.txt": "keep\n"}, remote.tree())
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.1
//
// Online, b.txt moved into c/ and another file then took the path b.txt
// gave up. The local move of b.txt checked only that nothing was at b.txt on
// OneDrive any more, read the newcomer as the source moving back, and was
// superseded on every pass: the chain never converged (model test seed 28,
// batch 5). The source is stale only when the item at its old path is the
// moved item itself.
func TestRunOnce_RemoteMoveChainIntoVacatedPathConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-c", "root", "c")
	remote.addFile(t, "item-b", "root", "b.txt", "b\n")
	remote.addFile(t, "item-inside", "item-c", "inside.txt", "inside\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	h := newSyncHarness(t, remote)
	eng, syncRoot := h.eng, h.syncRoot
	runScenarioPass(t, eng)

	remote.move("item-b", "item-c", "renamed-b.txt")
	remote.move("item-inside", "root", "b.txt")
	h.beginRound(t)
	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	assert.Equal(t, map[string]string{
		"b.txt": "inside\n", "c": "/", "c/renamed-b.txt": "b\n", "keep.txt": "keep\n",
	}, remote.tree())
	requireScenarioLocalFile(t, syncRoot, "b.txt", "inside\n")
	requireScenarioLocalFile(t, syncRoot, "c/renamed-b.txt", "b\n")
	assert.NoFileExists(t, filepath.Join(syncRoot, "c", "inside.txt"))
	assert.Zero(t, remote.uploads, "nothing is uploaded back")
	assertSyncInvariants(t, h)
}

func requireScenarioLocalFile(t *testing.T, syncRoot, rel, want string) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(syncRoot, filepath.FromSlash(rel))) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, want, string(data), rel)
}

// Validates: R-2.17.1
//
// Two synced files swapped names on one side (through a temporary name, as a
// user does). Online, each local move waited for the other to vacate its
// destination, so neither was ever planned and the swap never reached the
// local side (model test, remote swap operation). One item now parks at a
// temporary name beside its source, the other moves into the freed path, and
// the parked one follows on the next pass. A local swap is not paired as moves
// (a destination that is a synced path never pairs) and converges as two
// content uploads instead.
func TestRunOnce_SwapOfTwoNamesConverges(t *testing.T) {
	t.Parallel()

	for _, swapSide := range []string{modelLocalSide, modelRemoteSide} {
		t.Run(swapSide, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFile(t, "item-a", "root", "a.txt", "A\n")
			remote.addFile(t, "item-b", "root", "b.txt", "B\n")
			remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
			h := newSyncHarness(t, remote)
			runScenarioPass(t, h.eng)

			h.beginRound(t)
			if swapSide == modelLocalSide {
				for _, step := range [][2]string{{"a.txt", "swap.txt"}, {"b.txt", "a.txt"}, {"swap.txt", "b.txt"}} {
					require.NoError(t, os.Rename(filepath.Join(h.syncRoot, step[0]), filepath.Join(h.syncRoot, step[1])))
				}
			} else {
				remote.moveAt(t, "a.txt", "swap.txt")
				remote.moveAt(t, "b.txt", "a.txt")
				remote.moveAt(t, "swap.txt", "b.txt")
			}
			runScenarioPass(t, h.eng)
			runScenarioPass(t, h.eng)

			requireScenarioLocalFile(t, h.syncRoot, "a.txt", "B\n")
			requireScenarioLocalFile(t, h.syncRoot, "b.txt", "A\n")
			if swapSide == modelRemoteSide {
				assert.Zero(t, remote.uploads, "a swap online moves names locally, it transfers nothing")
			}
			assertSyncInvariants(t, h)
		})
	}
}

// Validates: R-2.17.1, R-2.17.2
//
// The user deleted f/x.txt while OneDrive renamed f to g. x.txt only followed
// its folder online, so its local delete must reach OneDrive once the folder
// rename is applied locally. Its remote path no longer matched the baseline
// path, so it was read as a new file at g/x.txt and downloaded again: the
// delete was undone (model test, case-only folder rename on OneDrive with a
// local delete below it).
func TestRunOnce_LocalDeleteInsideFolderRenamedOnlineIsNotUndone(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-f", "root", "f")
	remote.addFile(t, "item-x", "item-f", "x.txt", "X\n")
	remote.addFile(t, "item-y", "item-f", "y.txt", "Y\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, "f", "x.txt")))
	remote.moveAt(t, "f", "g")
	runScenarioPass(t, h.eng)
	runScenarioPass(t, h.eng)

	assert.NoFileExists(t, filepath.Join(h.syncRoot, "g", "x.txt"), "the local delete is not undone")
	assert.NotContains(t, remote.tree(), "g/x.txt", "the local delete reaches OneDrive")
	requireScenarioLocalFile(t, h.syncRoot, "g/y.txt", "Y\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.1
//
// The user rebuilt a folder (same names and content, new inodes) while
// OneDrive renamed a file in it. The file's new inode is not a change
// (D-057), yet the local move applying the rename checked the source by
// inode alone, read it as a different item, and was superseded on every
// pass (model test, folder recreate with a remote swap).
func TestRunOnce_RemoteRenameInsideLocallyRecreatedFolderConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-c", "item-docs", "c.md", "C\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	docs := filepath.Join(h.syncRoot, "docs")
	require.NoError(t, os.RemoveAll(docs))
	require.NoError(t, os.Mkdir(docs, 0o750))
	writeLocalFile(t, h.syncRoot, filepath.Join("docs", "c.md"), "C\n")
	remote.moveAt(t, "docs/c.md", "docs/d.md")
	runScenarioPass(t, h.eng)
	runScenarioPass(t, h.eng)

	requireScenarioLocalFile(t, h.syncRoot, "docs/d.md", "C\n")
	assert.NoFileExists(t, filepath.Join(docs, "c.md"))
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.4, R-2.17.10
//
// Both sides replaced a synced folder with a file, each with its own
// content. The plan read it as a local type change that wins (delete the
// OneDrive item, upload the local file), but the remote delete refuses to
// run while the path exists locally, so it was superseded on every pass (model
// test: local replace and remote replace of one folder). Two files that both
// replaced the folder are a create/create conflict: both are kept.
func TestRunOnce_FolderReplacedByFilesOnBothSidesKeepsBoth(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-in", "item-docs", "in.txt", "in\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	require.NoError(t, os.RemoveAll(filepath.Join(h.syncRoot, "docs")))
	writeLocalFile(t, h.syncRoot, "docs", "local file\n")
	remote.deleteAt(t, "docs")
	remote.putFileAt(t, "docs", "remote file\n")
	for range 3 {
		runScenarioPass(t, h.eng)
	}

	contents := modelContents(harnessLocalTree(t, h.syncRoot))
	assert.Contains(t, contents, "local file\n")
	assert.Contains(t, contents, "remote file\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.4
//
// Both sides replaced a synced file with a folder: the two folders are one,
// and what each side put in it syncs to the other.
func TestRunOnce_FileReplacedByFoldersOnBothSidesMergesThem(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-docs", "root", "docs", "was a file\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, "docs")))
	writeLocalFile(t, h.syncRoot, filepath.Join("docs", "local.txt"), "local\n")
	remote.deleteAt(t, "docs")
	remote.mkdirAt(t, "docs")
	remote.putFileAt(t, "docs/remote.txt", "remote\n")
	for range 3 {
		runScenarioPass(t, h.eng)
	}

	requireScenarioLocalFile(t, h.syncRoot, "docs/local.txt", "local\n")
	requireScenarioLocalFile(t, h.syncRoot, "docs/remote.txt", "remote\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.8.10, R-2.17.10
//
// OneDrive replaced a synced folder with a file while the user re-created the
// folder locally (same name, new inode). The plan read the local folder as
// unchanged and deleted it to make way for the file, but a local delete
// refuses a folder whose identity changed (R-2.8.10), so it was superseded
// on every pass (model test: local recreate folder, remote replace). A
// re-created entry is a new local item: both are kept.
func TestRunOnce_RemoteReplaceOfALocallyRecreatedFolderKeepsBoth(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	docs := filepath.Join(h.syncRoot, "docs")
	require.NoError(t, os.Remove(docs))
	require.NoError(t, os.Mkdir(docs, 0o750))
	remote.deleteAt(t, "docs")
	remote.putFileAt(t, "docs", "remote file\n")
	for range 3 {
		runScenarioPass(t, h.eng)
	}

	requireScenarioLocalFile(t, h.syncRoot, "docs", "remote file\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.1, R-2.8.10
//
// OneDrive moved f/x.txt out of f while the user rebuilt f locally with a
// different x.txt (new inode, new content). Move composition took the entry at
// the old path for the synced item and planned its local move, which the
// source check rightly refused as a different item, on every pass (model
// test: local folder recreate, then remote move while local edits). The
// entry is a new local file: OneDrive's moved item downloads at its new path
// and the local file uploads at the old one.
func TestRunOnce_RemoteMoveOfAFileTheUserReplacedLocallyKeepsBoth(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-f", "root", "f")
	remote.addFile(t, "item-x", "item-f", "x.txt", "synced\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	// Written beside it and renamed over it, so the new file cannot reuse the
	// old inode.
	writeLocalFile(t, h.syncRoot, filepath.Join("f", "x.new"), "a new local file\n")
	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "f", "x.new"), filepath.Join(h.syncRoot, "f", "x.txt")))
	remote.moveAt(t, "f/x.txt", "x.txt")
	for range 3 {
		runScenarioPass(t, h.eng)
	}

	requireScenarioLocalFile(t, h.syncRoot, "x.txt", "synced\n")
	requireScenarioLocalFile(t, h.syncRoot, "f/x.txt", "a new local file\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.1, R-2.17.6
//
// The user renamed p.txt to r.txt while OneDrive swapped p.txt and q.txt.
// OneDrive's name wins a divergent rename, so r.txt should become q.txt, but
// q.txt was the baseline path of the other swapped file, and composition
// accepted an item's remote position only at a path no synced item held. The
// renamed file had no remote position, nothing moved it, and the trees never
// converged (model test seed 27, with a case-only local rename). The item ID
// proves where OneDrive holds the item; a path another synced item is moving
// away from is a valid position.
func TestRunOnce_LocalRenameWhileOneDriveSwapsItConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-p", "root", "p.txt", "P\n")
	remote.addFile(t, "item-q", "root", "q.txt", "Q\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "p.txt"), filepath.Join(h.syncRoot, "r.txt")))
	remote.moveAt(t, "p.txt", "swap.txt")
	remote.moveAt(t, "q.txt", "p.txt")
	remote.moveAt(t, "swap.txt", "q.txt")
	for range 3 {
		runScenarioPass(t, h.eng)
	}

	requireScenarioLocalFile(t, h.syncRoot, "q.txt", "P\n")
	requireScenarioLocalFile(t, h.syncRoot, "p.txt", "Q\n")
	assert.NoFileExists(t, filepath.Join(h.syncRoot, "r.txt"))
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.1, R-2.17.2
//
// The user renamed folder d to e while OneDrive deleted d/x.txt. x.txt only
// followed its folder locally, so OneDrive's delete must reach e/x.txt once
// the folder rename reaches OneDrive. Its baseline path held nothing on
// either side, so the baseline row was dropped as gone from both, and the
// local copy then read as a new file and was uploaded: the delete was undone
// (model test seed 31, where OneDrive replaced the file with a folder).
func TestRunOnce_RemoteDeleteInsideFolderRenamedLocallyIsNotUndone(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-d", "root", "d")
	remote.addFile(t, "item-x", "item-d", "x.txt", "X\n")
	remote.addFile(t, "item-y", "item-d", "y.txt", "Y\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "d"), filepath.Join(h.syncRoot, "e")))
	remote.deleteAt(t, "d/x.txt")
	for range 3 {
		runScenarioPass(t, h.eng)
	}

	assert.NoFileExists(t, filepath.Join(h.syncRoot, "e", "x.txt"), "OneDrive's delete is not undone")
	assert.NotContains(t, remote.tree(), "e/x.txt", "the file is not uploaded again")
	requireScenarioLocalFile(t, h.syncRoot, "e/y.txt", "Y\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.1, R-2.17.4
//
// As above, but OneDrive also renamed d to f (its name wins) and replaced
// x.txt with a folder holding a file. The new folder's download was rebased into the local folder before it moved,
// onto the old x.txt still in the way, failed, and held the folder move
// that would have resolved it, on every pass (model test seed 31). It now
// waits for the folder move too, and the next pass replaces the unchanged
// local file with OneDrive's folder.
func TestRunOnce_RemoteReplaceInsideFolderRenamedLocallyConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-d", "root", "d")
	remote.addFile(t, "item-x", "item-d", "x.txt", "X\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "d"), filepath.Join(h.syncRoot, "e")))
	remote.moveAt(t, "d", "f")
	remote.deleteAt(t, "f/x.txt")
	remote.mkdirAt(t, "f/x.txt")
	remote.putFileAt(t, "f/x.txt/inside.txt", "inside\n")
	for range 4 {
		runScenarioPass(t, h.eng)
	}

	requireScenarioLocalFile(t, h.syncRoot, "f/x.txt/inside.txt", "inside\n")
	assertSyncInvariants(t, h)
}

// Validates: R-2.8.10, R-2.17.2
//
// OneDrive deleted folder q/x while the user re-created it locally (new
// inode). The plan read the local folder as unchanged and deleted it, but a
// local folder delete refuses a folder whose identity changed (R-2.8.10), so
// it was superseded on every pass (model test seed 99). A re-created folder
// is a new local entry: it is created on OneDrive again.
func TestRunOnce_RemoteDeleteOfALocallyRecreatedFolderKeepsIt(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-q", "root", "q")
	remote.addFolder("item-x", "item-q", "x")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	x := filepath.Join(h.syncRoot, "q", "x")
	require.NoError(t, os.Remove(x))
	require.NoError(t, os.Mkdir(x, 0o750))
	remote.deleteAt(t, "q/x")
	for range 3 {
		runScenarioPass(t, h.eng)
	}

	assert.DirExists(t, x)
	assert.Equal(t, modelFolderSentinel, remote.tree()["q/x"], "the re-created folder is on OneDrive again")
	assertSyncInvariants(t, h)
}
