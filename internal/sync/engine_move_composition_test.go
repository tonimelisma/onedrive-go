package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Validates: R-2.17.6
//
// The same rename on both sides (or a crash after the move ran on one side
// and before its outcome was recorded) leaves baseline P with the item at L
// on both sides. Paired as a remote move P→L, freshness found L occupied and
// superseded it on every pass (D-050).
func TestRunOnce_BothSidesRenamedIdenticallyConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-child", "item-dir", "child.txt", "child\n")
	remote.addFile(t, "item-file", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	require.NoError(t, os.Rename(filepath.Join(syncRoot, "dir"), filepath.Join(syncRoot, "renamed-dir")))
	remote.move("item-file", "root", "b.txt")
	remote.move("item-dir", "root", "renamed-dir")

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates)
	assert.Empty(t, remote.moves, "the item is already where both sides want it")
	assert.Zero(t, remote.uploads)

	requireScenarioBaselineMoved(t, eng,
		[]string{"a.txt", "dir", "dir/child.txt"},
		[]string{"b.txt", "renamed-dir", "renamed-dir/child.txt"})
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.1.7
//
// Both sides renamed a folder the same way and a new folder was made here at
// its old path. The plan re-keys the renamed folder's baseline row and makes
// the new folder on OneDrive; the re-key is publication-only, committed
// through the store before the create is dispatched, so the create records
// the new folder at the path the row left and the children's rows follow
// the renamed folder. One pass converges, and the next plan is empty.
func TestRunOnce_AFolderRenamedTheSameOnBothSidesWithANewFolderAtItsOldPathConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-child", "item-dir", "child.txt", "child\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "dir"), filepath.Join(syncRoot, "renamed-dir")))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "dir"), 0o700))
	remote.move("item-dir", "root", "renamed-dir")

	eng.hooks.oneShotPassLimit = 1
	report := runScenarioPass(t, eng)
	assert.Equal(t, 1, report.FolderCreates, "the new folder is made on OneDrive")
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes)
	assert.Equal(t, "item-dir", requireScenarioRemoteID(t, remote, "renamed-dir"))
	assert.NotEqual(t, "item-dir", requireScenarioRemoteID(t, remote, "dir"))

	requireScenarioBaselineMoved(t, eng, []string{"dir/child.txt"}, []string{"dir", "renamed-dir", "renamed-dir/child.txt"})
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.1.7
//
// Both sides renamed the file to the same name, and OneDrive also edited it.
// Re-keying the baseline must publish only the path: the remote edit stays
// pending and is downloaded, not recorded as already synced. The download is
// planned by the pass after the re-key, which the same sync runs.
func TestRunOnce_BothSidesRenamedIdenticallyKeepsTheRemoteEditPending(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-file", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	remote.move("item-file", "root", "b.txt")
	remote.setContent(t, "item-file", "edited online\n")

	runScenarioPass(t, eng)

	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "edited online\n", string(data), "the remote edit must reach the local copy")
	assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// OneDrive renamed a folder and then created a new folder inside it. The new
// folder is created where the renamed folder still sits locally, and the
// local rename carries it along: one pass, no stray copy.
func TestRunOnce_RemoteFolderRenameWithNewSubfolderConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-x", "root", "x")
	remote.addFolder("item-photos", "item-x", "photos")
	remote.addFile(t, "item-b", "item-photos", "b.txt", "b\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	remote.move("item-photos", "item-x", "x")
	remote.addFolder("item-new", "item-photos", "photos")

	runScenarioPass(t, eng)
	assert.DirExists(t, filepath.Join(syncRoot, "x", "x", "photos"))
	assert.FileExists(t, filepath.Join(syncRoot, "x", "x", "b.txt"))
	assert.NoDirExists(t, filepath.Join(syncRoot, "x", "photos"))
	requireScenarioBaselineMoved(t, eng,
		[]string{"x/photos", "x/photos/photos", "x/photos/b.txt"},
		[]string{"x/x", "x/x/photos", "x/x/b.txt"})
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.8
//
// A hard link is a second name for the synced file, not a move of it
// (D-056). Paired as a move, the synced name looked moved away while it was
// still there, and the new name was never uploaded.
func TestRunOnce_HardLinkUploadsAsANewFile(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Link(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "link.txt")))
	runScenarioPass(t, eng)

	assert.Empty(t, remote.moves)
	assert.Equal(t, 1, remote.uploads)
	assert.Equal(t, map[string]string{"a.txt": "alpha\n", "link.txt": "alpha\n"}, remote.tree())
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.8, R-2.17.6
//
// A hard link synced as its own file shares the inode of the synced file it
// names. When the link's name is deleted here and OneDrive moves the link's
// item, the inode is still at the other file's own path, where that file's
// baseline holds the same identity. That is the other file in its place, not
// the link's item moved onto a path its owner freed: paired as such a move,
// the divergent rename moved the other file here to OneDrive's name of the
// link and the next pass deleted the other file on OneDrive (model test seed
// 328009, where the link's name went with its folder replaced by a file and
// OneDrive renamed the link's folders in case only).
func TestRunOnce_AHardLinkDeletedHereAndMovedOnOneDriveLeavesTheOtherNameInPlace(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	h.beginRound(t)
	require.NoError(t, os.Link(filepath.Join(h.syncRoot, "a.txt"), filepath.Join(h.syncRoot, "link.txt")))
	runScenarioPass(t, h.eng)
	require.Equal(t, map[string]string{"a.txt": "alpha\n", "link.txt": "alpha\n"}, remote.tree())

	h.beginRound(t)
	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, "link.txt")))
	remote.moveAt(t, "link.txt", "moved.txt")
	runScenarioPass(t, h.eng)
	runScenarioPass(t, h.eng)

	requireScenarioLocalFile(t, h.syncRoot, "a.txt", "alpha\n")
	assert.Equal(t, "item-a", requireScenarioRemoteID(t, remote, "a.txt"))
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.8
//
// mv a.txt renamed.txt followed by a new a.txt: one remote move carries the
// synced item, and the new file is uploaded once the move has vacated the
// old name, so it does not become a new version of the moved item.
func TestRunOnce_RenameAndRecreateMovesThenCreates(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-child", "item-dir", "child.txt", "child\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "renamed.txt")))
	syncfixture.WriteLocalFile(t, syncRoot, "a.txt", "fresh\n")
	require.NoError(t, os.Rename(filepath.Join(syncRoot, "dir"), filepath.Join(syncRoot, "renamed-dir")))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "dir"), 0o750))

	runScenarioPass(t, eng)

	assert.ElementsMatch(t, []string{"item-a->renamed.txt", "item-dir->renamed-dir"}, remote.moves)
	assert.Equal(t, map[string]string{
		"a.txt":                 "fresh\n",
		"dir":                   "/",
		"renamed.txt":           "alpha\n",
		"renamed-dir":           "/",
		"renamed-dir/child.txt": "child\n",
	}, remote.tree())
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// Local a→b while OneDrive renamed a→c (D-051). OneDrive's name wins (R-2.17.6):
// the local copy is renamed to c, nothing is uploaded or deleted, and the next
// plan is quiet. Before, the plan moved the remote item from a, where it no
// longer was, and was superseded on every pass.
func TestRunOnce_DivergentRenameConvergesInOneSync(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	remote.move("item-a", "root", "c.txt")

	runScenarioPass(t, eng)

	assert.NoFileExists(t, filepath.Join(syncRoot, "b.txt"))
	assert.FileExists(t, filepath.Join(syncRoot, "c.txt"))
	assert.Equal(t, map[string]string{"c.txt": "alpha\n"}, remote.tree())
	assert.Empty(t, remote.moves, "OneDrive's name is kept")
	assert.Zero(t, remote.uploads)
	assert.Empty(t, remote.deletes)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.7, R-2.1.7
//
// OneDrive moved a file the user had edited locally (D-053). The local copy
// follows the move with its edit, and the edit is uploaded at the new name as
// a new version of the same item, never stranded at the old name. The upload
// is planned by the pass after the move, which the same sync runs.
func TestRunOnce_RemoteMoveWithLocalEditMovesThenUploads(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	syncfixture.WriteLocalFile(t, syncRoot, "a.txt", "edited locally\n")
	remote.move("item-a", "item-dir", "moved.txt")

	runScenarioPass(t, eng)

	assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
	data, err := os.ReadFile(filepath.Join(syncRoot, "dir", "moved.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "edited locally\n", string(data))
	assert.Equal(t, map[string]string{"dir": "/", "dir/moved.txt": "edited locally\n"}, remote.tree(),
		"the edit reaches OneDrive as the same item at its new name")
	assert.Empty(t, remote.deletes)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// Local A→B/A while OneDrive moved B→A/B (D-055). Applied independently,
// Graph rejected moving a folder into its own descendant and the local side
// nested the trees. The OneDrive move is kept and the local one undone: A at
// the top with B inside, every file kept, no folder nested in itself.
func TestRunOnce_CrossMoveCycleKeepsBothTreesWithoutNesting(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-A", "root", "A")
	remote.addFolder("item-B", "root", "B")
	remote.addFile(t, "item-fa", "item-A", "fa.txt", "in A\n")
	remote.addFile(t, "item-fb", "item-B", "fb.txt", "in B\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "A"), filepath.Join(syncRoot, "B", "A")))
	remote.move("item-B", "item-A", "B")

	runScenarioPass(t, eng)

	want := map[string]string{
		"A":          "/",
		"A/fa.txt":   "in A\n",
		"A/B":        "/",
		"A/B/fb.txt": "in B\n",
	}
	assert.Equal(t, want, remote.tree())
	assert.FileExists(t, filepath.Join(syncRoot, "A", "fa.txt"))
	assert.FileExists(t, filepath.Join(syncRoot, "A", "B", "fb.txt"))
	assert.NoDirExists(t, filepath.Join(syncRoot, "B"))
	assert.NoDirExists(t, filepath.Join(syncRoot, "A", "B", "A"))
	assert.Empty(t, remote.moves, "the OneDrive move is kept; nothing is moved online")
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// OneDrive inverted a hierarchy (A/B → B, then A → B/A). Locally B must leave
// A before A can move into B; a local move never fabricates a missing parent
// (D-054).
func TestRunOnce_RemoteHierarchyInversionConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-A", "root", "A")
	remote.addFolder("item-B", "item-A", "B")
	remote.addFile(t, "item-fa", "item-A", "fa.txt", "in A\n")
	remote.addFile(t, "item-fb", "item-B", "fb.txt", "in B\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	remote.move("item-B", "root", "B")
	remote.move("item-A", "item-B", "A")

	runScenarioPass(t, eng)

	assert.FileExists(t, filepath.Join(syncRoot, "B", "fb.txt"))
	assert.FileExists(t, filepath.Join(syncRoot, "B", "A", "fa.txt"))
	assert.NoDirExists(t, filepath.Join(syncRoot, "A"))
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.folderCreates)
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// OneDrive renamed a populated folder and a file inside it in one batch, and
// moved another file into it (D-054): the folder moves once, the child
// rename and the move-in follow inside it, nothing is re-created.
func TestRunOnce_RemoteFolderRenameWithChildRenameAndMoveInConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-old", "item-docs", "old.txt", "old\n")
	remote.addFile(t, "item-kept", "item-docs", "kept.txt", "kept\n")
	remote.addFile(t, "item-out", "root", "outside.txt", "outside\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	remote.move("item-docs", "root", "archive")
	remote.move("item-old", "item-docs", "new.txt")
	remote.move("item-out", "item-docs", "inside.txt")

	runScenarioPass(t, eng)

	assert.Equal(t, map[string]string{
		"archive":            "/",
		"archive/new.txt":    "old\n",
		"archive/kept.txt":   "kept\n",
		"archive/inside.txt": "outside\n",
	}, remote.tree())
	for p, content := range remote.tree() {
		if content == "/" {
			assert.DirExists(t, filepath.Join(syncRoot, filepath.FromSlash(p)))
			continue
		}
		assert.FileExists(t, filepath.Join(syncRoot, filepath.FromSlash(p)))
	}
	assert.NoDirExists(t, filepath.Join(syncRoot, "docs"))
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.folderCreates)
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// The user renamed a populated folder and a file inside it (D-054): one
// remote folder move, then the child's rename addressed through the folder's
// new path in the same pass.
func TestRunOnce_LocalFolderRenameWithChildRenameConvergesInOnePass(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-old", "item-docs", "old.txt", "old\n")
	remote.addFile(t, "item-kept", "item-docs", "kept.txt", "kept\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "docs"), filepath.Join(syncRoot, "archive")))
	require.NoError(t, os.Rename(filepath.Join(syncRoot, "archive", "old.txt"), filepath.Join(syncRoot, "archive", "new.txt")))

	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-docs->archive", "item-old->new.txt"}, remote.moves)
	assert.Equal(t, map[string]string{
		"archive":          "/",
		"archive/new.txt":  "old\n",
		"archive/kept.txt": "kept\n",
	}, remote.tree())
	assert.Zero(t, remote.uploads)
	assert.Empty(t, remote.folderCreates)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.3
//
// A local rename while OneDrive edits the same file (D-052): the move
// publishes only the path, so the remote edit stays pending and is downloaded
// at the new name instead of being recorded as synced over the old bytes.
func TestRunOnce_LocalRenameWhileRemoteEditsDownloadsTheEdit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	remote.setContent(t, "item-a", "edited online\n")

	runScenarioPass(t, eng)

	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "edited online\n", string(data))
	assert.Equal(t, map[string]string{"b.txt": "edited online\n"}, remote.tree())
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.3, R-2.1.7
//
// The rename here and OneDrive's edit of the same file compose in one pass:
// the plan moves the item on OneDrive and downloads the edit at the new name
// after the move, and the download is not superseded by the move's new eTag.
func TestRunOnce_LocalRenameAndRemoteEditComposeInOnePass(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	remote.setContent(t, "item-a", "edited online\n")

	planned, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, 1, planned.Moves, "the rename goes up")
	assert.Equal(t, 1, planned.Downloads, "the edit comes down in the same pass")

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Superseded, "the download runs after the move without going stale")
	assert.Equal(t, 1, report.Downloads)

	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "edited online\n", string(data))
	assert.Equal(t, map[string]string{"b.txt": "edited online\n"}, remote.tree())
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.3, R-2.17.6, R-2.1.7
//
// OneDrive moved and edited a file: its move and its edit compose in one
// pass, whether the copy here stayed put or was renamed differently here
// (OneDrive's name wins). The local copy moves to OneDrive's place and the
// edit comes down onto it after the move; the download is not superseded by
// the rename's new inode change time, and nothing goes up.
func TestRunOnce_RemoteMoveAndEditComposeInOnePass(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		localAfter string
	}{
		{name: "kept here", localAfter: "a.txt"},
		{name: "renamed here", localAfter: "b.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			driveID := driveid.New(syncfixture.EngineTestDriveID)
			remote := newMoveScenarioRemote(driveID)
			remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

			eng, syncRoot := newTestEngine(t, remote.client(t))
			runScenarioPass(t, eng)

			if tc.localAfter != "a.txt" {
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, tc.localAfter)))
			}
			remote.move("item-a", "root", "c.txt")
			remote.setContent(t, "item-a", "edited online\n")

			planned, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{DryRun: true})
			require.NoError(t, err)
			assert.Equal(t, 1, planned.Moves, "the copy here follows OneDrive's move")
			assert.Equal(t, 1, planned.Downloads, "the edit comes down in the same pass")

			report := runScenarioPass(t, eng)
			assert.Zero(t, report.Superseded, "the download runs after the move without going stale")
			assert.Equal(t, 1, report.Downloads)

			data, err := os.ReadFile(filepath.Join(syncRoot, "c.txt")) //nolint:gosec // test reads its own temp tree
			require.NoError(t, err)
			assert.Equal(t, "edited online\n", string(data))
			assert.NoFileExists(t, filepath.Join(syncRoot, tc.localAfter))
			assert.Equal(t, map[string]string{"c.txt": "edited online\n"}, remote.tree())
			assert.Zero(t, remote.uploads)
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.9
//
// OneDrive moved a.txt over b.txt, replacing it (D-058). The local copy of
// a.txt moves onto b.txt, whose old local copy is unchanged and therefore
// replaced; nothing is uploaded, so a.txt is not resurrected online as a
// spurious duplicate. The old b.txt is in the trash exactly once, as itself,
// and the rename replaced nothing: b.txt is the moved file itself.
func TestRunOnce_RemoteMoveOverExistingFileReplacesWithoutResurrecting(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "root", "b.txt", "bravo\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	moved, displaced := syncfixture.EntryIdentity(t, filepath.Join(syncRoot, "a.txt")), syncfixture.EntryIdentity(t, filepath.Join(syncRoot, "b.txt"))

	remote.deleteRemotely("item-b")
	remote.move("item-a", "root", "b.txt")

	runScenarioPass(t, eng)

	assert.Equal(t, map[string]string{"b.txt": "alpha\n", "keep.txt": "keep\n"}, remote.tree())
	assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "alpha\n", string(data))
	assert.Zero(t, remote.uploads, "the moved file must not be uploaded again")
	assert.Equal(t, moved, syncfixture.EntryIdentity(t, filepath.Join(syncRoot, "b.txt")), "b.txt is the moved file itself")
	kept := syncfixture.RequireTrashHoldsOnce(t, eng.upkeep.trash, "b.txt", "bravo\n")
	assert.Equal(t, displaced, syncfixture.EntryIdentity(t, kept.ItemPath), "the trash holds the old b.txt itself, not a copy")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9
//
// The same replacing move when the user had edited the replaced b.txt: the
// edit is kept (as a conflict copy) instead of being overwritten.
func TestRunOnce_RemoteMoveOverLocallyEditedFileKeepsTheEdit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "root", "b.txt", "bravo\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	syncfixture.WriteLocalFile(t, syncRoot, "b.txt", "edited bravo\n")
	remote.deleteRemotely("item-b")
	remote.move("item-a", "root", "b.txt")

	runScenarioPass(t, eng)

	tree := remote.tree()
	var contents []string
	for _, content := range tree {
		contents = append(contents, content)
	}
	assert.Contains(t, contents, "edited bravo\n", "the local edit survives online")
	assert.Contains(t, contents, "alpha\n")
	assert.Equal(t, "alpha\n", tree["b.txt"])
	assert.NotContains(t, tree, "a.txt")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9
//
// A replacing remote move onto a file with identical content: SQLite sees the
// destination unchanged with a new eTag and plans a metadata baseline update
// for it, recording the moved item at the destination with the replaced
// file's local identity. Composition moves the item there itself; the
// competing publication is dropped, so one pass leaves the baseline exactly
// as the disk is.
func TestRunOnce_RemoteMoveOverIdenticalFileConvergesInOnePass(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "same\n")
	remote.addFile(t, "item-b", "root", "b.txt", "same\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	moved := syncfixture.EntryIdentity(t, filepath.Join(syncRoot, "a.txt"))

	remote.deleteRemotely("item-b")
	remote.move("item-a", "root", "b.txt")

	// The move pass alone: a follow-up pass would refresh the baseline from
	// the disk and hide what the move published.
	eng.hooks.oneShotPassLimit = 1
	report := runScenarioPass(t, eng)
	assert.Equal(t, 1, report.Moves)
	assert.Zero(t, report.BaselineUpdates, "no competing publication for the destination")
	assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
	assert.Equal(t, map[string]string{"b.txt": "same\n", "keep.txt": "keep\n"}, remote.tree())

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByPath("b.txt")
	require.True(t, ok)
	assert.Equal(t, "item-a", entry.ItemID)
	info, err := os.Lstat(filepath.Join(syncRoot, "b.txt"))
	require.NoError(t, err)
	identity, ok := synctree.IdentityFromFileInfo(info)
	require.True(t, ok)
	assert.Equal(t, identity.Inode, entry.LocalInode, "the baseline records the moved file's identity")
	assert.Equal(t, moved.Inode, identity.Inode, "b.txt is the moved file itself")
	syncfixture.RequireTrashHoldsOnce(t, eng.upkeep.trash, "b.txt", "same\n")
	requireScenarioQuietPlan(t, eng)
	assert.Zero(t, remote.uploads)
}
