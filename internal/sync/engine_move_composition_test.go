package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.17.6
//
// The same rename on both sides (or a crash after the move ran on one side
// and before its outcome was recorded) leaves baseline P with the item at L
// on both sides. Paired as a remote move P→L, freshness found L occupied and
// superseded it on every pass (D-050).
func TestRunOnce_BothSidesRenamedIdenticallyConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
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

// Validates: R-2.17.6
//
// Both sides renamed the file to the same name, and OneDrive also edited it.
// Re-keying the baseline must publish only the path: the remote edit stays
// pending and is downloaded, not recorded as already synced.
func TestRunOnce_BothSidesRenamedIdenticallyKeepsTheRemoteEditPending(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-file", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	remote.move("item-file", "root", "b.txt")
	remote.setContent(t, "item-file", "edited online\n")

	runScenarioPass(t, eng)
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

	driveID := driveid.New(engineTestDriveID)
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

	driveID := driveid.New(engineTestDriveID)
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

// Validates: R-2.17.8
//
// mv a.txt renamed.txt followed by a new a.txt: one remote move carries the
// synced item, and the new file is uploaded once the move has vacated the
// old name, so it does not become a new version of the moved item.
func TestRunOnce_RenameAndRecreateMovesThenCreates(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-child", "item-dir", "child.txt", "child\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "renamed.txt")))
	writeLocalFile(t, syncRoot, "a.txt", "fresh\n")
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
// Local a→b while OneDrive renamed a→c (D-051). OneDrive's name wins (P-6):
// the local copy is renamed to c, nothing is uploaded or deleted, and the next
// plan is quiet. Before, the plan moved the remote item from a, where it no
// longer was, and was superseded on every pass.
func TestRunOnce_DivergentRenameConvergesInTwoPasses(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	remote.move("item-a", "root", "c.txt")

	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	assert.NoFileExists(t, filepath.Join(syncRoot, "b.txt"))
	assert.FileExists(t, filepath.Join(syncRoot, "c.txt"))
	assert.Equal(t, map[string]string{"c.txt": "alpha\n"}, remote.tree())
	assert.Empty(t, remote.moves, "OneDrive's name is kept")
	assert.Zero(t, remote.uploads)
	assert.Empty(t, remote.deletes)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.7
//
// OneDrive moved a file the user had edited locally (D-053). The local copy
// follows the move with its edit, and the edit is uploaded at the new name as
// a new version of the same item, never stranded at the old name.
func TestRunOnce_RemoteMoveWithLocalEditMovesThenUploads(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "dir")
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	writeLocalFile(t, syncRoot, "a.txt", "edited locally\n")
	remote.move("item-a", "item-dir", "moved.txt")

	runScenarioPass(t, eng)
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

	driveID := driveid.New(engineTestDriveID)
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

	driveID := driveid.New(engineTestDriveID)
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

	driveID := driveid.New(engineTestDriveID)
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

	driveID := driveid.New(engineTestDriveID)
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

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "b.txt")))
	remote.setContent(t, "item-a", "edited online\n")

	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "edited online\n", string(data))
	assert.Equal(t, map[string]string{"b.txt": "edited online\n"}, remote.tree())
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9
//
// OneDrive moved a.txt over b.txt, replacing it (D-058). The local copy of
// a.txt moves onto b.txt, whose old local copy is unchanged and therefore
// replaced; nothing is uploaded, so a.txt is not resurrected online as a
// spurious duplicate.
func TestRunOnce_RemoteMoveOverExistingFileReplacesWithoutResurrecting(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "root", "b.txt", "bravo\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	remote.deleteRemotely("item-b")
	remote.move("item-a", "root", "b.txt")

	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	assert.Equal(t, map[string]string{"b.txt": "alpha\n", "keep.txt": "keep\n"}, remote.tree())
	assert.NoFileExists(t, filepath.Join(syncRoot, "a.txt"))
	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "alpha\n", string(data))
	assert.Zero(t, remote.uploads, "the moved file must not be uploaded again")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9
//
// The same replacing move when the user had edited the replaced b.txt: the
// edit is kept (as a conflict copy) instead of being overwritten.
func TestRunOnce_RemoteMoveOverLocallyEditedFileKeepsTheEdit(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "root", "b.txt", "bravo\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	writeLocalFile(t, syncRoot, "b.txt", "edited bravo\n")
	remote.deleteRemotely("item-b")
	remote.move("item-a", "root", "b.txt")

	for range 3 {
		runScenarioPass(t, eng)
	}

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
