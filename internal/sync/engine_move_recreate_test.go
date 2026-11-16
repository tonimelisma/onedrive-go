package sync

import (
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// scenarioRunLimit bounds the one-shot runs runScenarioUntilDone makes.
const scenarioRunLimit = 3

// recreatedFolderMoveCase is one way to reach the same end state: docs moved
// into destParent as "photos", a new docs made at its old path, and one entry
// of the moved folder moved into the new docs.
type recreatedFolderMoveCase struct {
	name string
	// fileName names the entry the second move takes into the new docs: a
	// file, or with folder set a folder holding inner.txt.
	fileName string
	// folder makes that entry a folder.
	folder bool
	// destParent is the folder the first move puts docs into.
	destParent string
	// oneRun makes both moves before a single run instead of a run after each.
	oneRun bool
	// keepName moves the entry into the new docs under the name it had.
	keepName bool
	// onOneDrive makes the moves on OneDrive instead of on this computer.
	onOneDrive bool
}

// Validates: R-2.17.6, R-2.17.1
//
// docs is moved into photos as photos/photos and a new docs is made, then a
// file or folder of the moved folder is moved into the new docs, under a new
// name or back under the one it had, on this computer or on OneDrive, with a
// sync after each step or one sync for both. One sync finishes the moves: both
// sides end with the tree the user left, each move is made once on the other
// side rather than sent again, no action is planned from a source the folder's
// move took away, and no plan drops an action as a dependency cycle.
func TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges(t *testing.T) {
	t.Parallel()

	for _, tc := range []recreatedFolderMoveCase{
		{name: "two runs", fileName: "50%.txt", destParent: "photos"},
		{name: "one run", fileName: "50%.txt", destParent: "photos", oneRun: true},
		{name: "two runs plain name", fileName: "plain.txt", destParent: "photos"},
		{name: "one run plain name", fileName: "plain.txt", destParent: "photos", oneRun: true},
		{name: "two runs two levels deep", fileName: "50%.txt", destParent: "photos/album"},
		{name: "one run two levels deep", fileName: "50%.txt", destParent: "photos/album", oneRun: true},
		{name: "two runs same name", fileName: "50%.txt", destParent: "photos", keepName: true},
		{name: "two runs on OneDrive", fileName: "50%.txt", destParent: "photos", onOneDrive: true},
		{name: "one run on OneDrive", fileName: "50%.txt", destParent: "photos", oneRun: true, onOneDrive: true},
		{
			name: "one run on OneDrive two levels deep", fileName: "plain.txt", destParent: "photos/album",
			oneRun: true, onOneDrive: true,
		},
		{name: "one run same name", fileName: "50%.txt", destParent: "photos", oneRun: true, keepName: true},
		{
			name: "one run same name on OneDrive", fileName: "50%.txt", destParent: "photos",
			oneRun: true, keepName: true, onOneDrive: true,
		},
		{name: "one run folder same name", fileName: "sub", folder: true, destParent: "photos", oneRun: true, keepName: true},
		{
			name: "one run folder same name on OneDrive", fileName: "sub", folder: true, destParent: "photos",
			oneRun: true, keepName: true, onOneDrive: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runMoveThroughRecreatedFolder(t, &tc)
		})
	}
}

// steps returns the two changes of the case: docs moved to moved and a new
// docs made, then the entry moved from the moved folder to renamed.
func (tc *recreatedFolderMoveCase) steps(
	t *testing.T, remote *moveScenarioRemote, syncRoot, moved, renamed string,
) (moveFolderAndRecreate, moveFile func()) {
	t.Helper()

	if tc.onOneDrive {
		return func() {
				remote.moveAt(t, "docs", moved)
				remote.mkdirAt(t, "docs")
			}, func() {
				remote.moveAt(t, path.Join(moved, tc.fileName), renamed)
			}
	}

	local := scenarioLocalPath(syncRoot)

	return func() {
			require.NoError(t, os.Rename(local("docs"), local(moved)))
			require.NoError(t, os.Mkdir(local("docs"), 0o750))
		}, func() {
			require.NoError(t, os.Rename(local(path.Join(moved, tc.fileName)), local(renamed)))
		}
}

// wantTree is the tree both sides hold once the moves are done: docs at
// moved, the new docs, and the entry at renamed.
func (tc *recreatedFolderMoveCase) wantTree(moved, renamed string) map[string]string {
	want := map[string]string{
		"photos": "/", "photos/photo.jpg": "photo\n", "docs": "/",
		moved: "/", path.Join(moved, "kept.txt"): "kept\n", tc.destParent: "/",
	}
	if tc.folder {
		want[renamed] = "/"
		want[path.Join(renamed, "inner.txt")] = "inner\n"
	} else {
		want[renamed] = "moved\n"
	}

	return want
}

func runMoveThroughRecreatedFolder(t *testing.T, tc *recreatedFolderMoveCase) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	if tc.folder {
		remote.addFolder("item-moved", "item-docs", tc.fileName)
		remote.addFile(t, "item-inner", "item-moved", "inner.txt", "inner\n")
	} else {
		remote.addFile(t, "item-moved", "item-docs", tc.fileName, "moved\n")
	}
	remote.addFile(t, "item-kept", "item-docs", "kept.txt", "kept\n")
	remote.addFolder("item-photos", "root", "photos")
	remote.addFile(t, "item-photo", "item-photos", "photo.jpg", "photo\n")
	if tc.destParent != "photos" {
		remote.addFolder("item-album", "item-photos", path.Base(tc.destParent))
	}
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)

	moved := path.Join(tc.destParent, "photos")
	renamed := path.Join("docs", "renamed-"+tc.fileName)
	if tc.keepName {
		renamed = path.Join("docs", tc.fileName)
	}
	downloadsBefore := remote.downloads
	moveFolderAndRecreate, moveFile := tc.steps(t, remote, syncRoot, moved, renamed)
	moveFolderAndRecreate()
	if !tc.oneRun {
		runScenarioPass(t, eng)
	}
	moveFile()
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Zero(t, superseded, "no move names a source the folder's move took away")

	want := tc.wantTree(moved, renamed)
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, renamed, remote.pathOf("item-moved"), "the entry is moved on OneDrive, not uploaded again")
	wantGraphMoves := []string{"item-docs->photos", "item-moved->" + path.Base(renamed)}
	if tc.onOneDrive {
		wantGraphMoves = nil
	}
	assert.Equal(t, wantGraphMoves, remote.moves, "each move made here is one move on OneDrive")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "the entry is moved on this computer, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive, docs is moved to archive and its subfolder sub is then moved to
// docs, the path its parent left, before one sync. One sync finishes both
// moves on this computer: both sides end with the same tree, nothing is
// downloaded again, and no plan drops an action as a dependency cycle.
func TestRunOnce_SubfolderMovedOnOneDriveToThePathItsMovedParentLeftConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFolder("item-sub", "item-docs", "sub")
	remote.addFile(t, "item-inner", "item-sub", "inner.txt", "inner\n")
	remote.addFile(t, "item-kept", "item-docs", "kept.txt", "kept\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.moveAt(t, "docs", "archive")
	remote.moveAt(t, "archive/sub", "docs")
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Zero(t, superseded, "no move names a source the folder's move took away")

	want := map[string]string{"archive": "/", "archive/kept.txt": "kept\n", "docs": "/", "docs/inner.txt": "inner\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Empty(t, remote.moves, "nothing is moved back on OneDrive")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "the folders are moved on this computer, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// folderMadeHereCase is one way OneDrive takes a.txt out of docs while this
// computer moved it into a new folder docs/new.
type folderMadeHereCase struct {
	name string
	// movedTo is where OneDrive moves docs.
	movedTo string
	// recreate makes a new docs on OneDrive before a.txt is moved.
	recreate bool
	// target is where OneDrive then moves a.txt.
	target string
}

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive docs is moved away and its a.txt moved to the path docs left,
// into a new docs made there or onto that path itself, while on this computer
// a.txt was moved into a new folder docs/new before the sync. One sync
// finishes it, and no move is superseded: both sides end with the same tree,
// a.txt once, where OneDrive put it, and docs/new inside the folder it was
// made in, made on OneDrive; nothing is moved, deleted, uploaded or
// downloaded again, and no plan drops an action as a dependency cycle. The
// into case holds that the moves converge, with or without the wait that
// orders a.txt's move after docs's; the onto case holds that wait too
// (TestComposeMoves_FileMovedOnOneDriveOutOfAFolderMadeHereWaitsForItsFolder):
// without it, every pass drops a cycle.
func TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges(t *testing.T) {
	t.Parallel()

	for _, tc := range []folderMadeHereCase{
		{name: "into the folder made at its folder's path", movedTo: "photos/photos", recreate: true, target: "docs/a.txt"},
		{name: "onto its folder's path", movedTo: "archive", target: "docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runFileMovedOutOfAFolderMadeHere(t, &tc)
		})
	}
}

func runFileMovedOutOfAFolderMadeHere(t *testing.T, tc *folderMadeHereCase) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-moved", "item-docs", "a.txt", "moved\n")
	remote.addFile(t, "item-kept", "item-docs", "kept.txt", "kept\n")
	remote.addFolder("item-photos", "root", "photos")
	remote.addFile(t, "item-photo", "item-photos", "photo.jpg", "photo\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.moveAt(t, "docs", tc.movedTo)
	if tc.recreate {
		remote.mkdirAt(t, "docs")
	}
	remote.moveAt(t, path.Join(tc.movedTo, "a.txt"), tc.target)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Mkdir(local("docs/new"), 0o750))
	require.NoError(t, os.Rename(local("docs/a.txt"), local("docs/new/a.txt")))

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Zero(t, superseded, "no move names a source a folder's move took away")

	want := map[string]string{
		"photos": "/", "photos/photo.jpg": "photo\n", tc.movedTo: "/", path.Join(tc.movedTo, "kept.txt"): "kept\n",
		path.Join(tc.movedTo, "new"): "/", tc.target: "moved\n",
	}
	if tc.recreate {
		want[path.Dir(tc.target)] = "/"
	}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, tc.target, remote.pathOf("item-moved"), "a.txt is still the item OneDrive moved")
	assert.Empty(t, remote.moves, "nothing is moved on OneDrive")
	assert.Equal(t, []string{"new"}, remote.folderCreates, "the folder made here is made on OneDrive")
	assert.Empty(t, remote.deletes)
	assert.Empty(t, remote.retainedContents(), "nothing goes to OneDrive's recycle bin")
	assert.Empty(t, harnessTrashContents(t, eng), "nothing goes to the local trash")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "a.txt is moved on this computer, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// docs/sub is renamed docs/sub2, docs is moved into photos as photos/photos, a
// new docs is made, and photos/photos/sub2/inner.txt is moved into the new
// docs, on this computer or on OneDrive, before one sync. One sync finishes
// the moves (assertOneSyncLeavesTree), each move made here is one move on
// OneDrive, and inner.txt is not downloaded again.
func TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		onOneDrive bool
	}{
		{name: "made here"},
		{name: "made on OneDrive", onOneDrive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newSubfolderScenarioRemote(t)
			eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
			downloadsBefore := remote.downloads
			var wantGraphMoves []string
			if tc.onOneDrive {
				remote.moveAt(t, "docs/sub", "docs/sub2")
				remote.moveAt(t, "docs", "photos/photos")
				remote.mkdirAt(t, "docs")
				remote.moveAt(t, "photos/photos/sub2/inner.txt", "docs/inner.txt")
			} else {
				local := scenarioLocalPath(syncRoot)
				require.NoError(t, os.Rename(local("docs/sub"), local("docs/sub2")))
				require.NoError(t, os.Rename(local("docs"), local("photos/photos")))
				require.NoError(t, os.Mkdir(local("docs"), 0o750))
				require.NoError(t, os.Rename(local("photos/photos/sub2/inner.txt"), local("docs/inner.txt")))
				wantGraphMoves = []string{"item-docs->photos", "item-sub->sub2", "item-inner->inner.txt"}
			}

			assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, map[string]string{
				"photos": "/", "photos/photo.jpg": "photo\n", "photos/photos": "/",
				"photos/photos/kept.txt": "kept\n", "photos/photos/sub2": "/", "docs": "/", "docs/inner.txt": "inner\n",
			})
			assert.ElementsMatch(t, wantGraphMoves, remote.moves, "each move made here is one move on OneDrive")
			assert.Equal(t, downloadsBefore, remote.downloads, "inner.txt is moved, not downloaded again")
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// On this computer docs is moved to archive, a new docs is made, inner.txt is
// moved from archive/sub into it and the emptied archive/sub is removed, while
// a new file is added to docs/sub on OneDrive, before one sync. One sync
// finishes it (assertOneSyncLeavesTree): inner.txt ends in the new docs and
// the new file in archive/sub on both sides, and each move made here is one
// move on OneDrive.
func TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges(t *testing.T) {
	t.Parallel()

	remote := newSubfolderScenarioRemote(t)
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("docs"), local("archive")))
	require.NoError(t, os.Mkdir(local("docs"), 0o750))
	require.NoError(t, os.Rename(local("archive/sub/inner.txt"), local("docs/inner.txt")))
	require.NoError(t, os.Remove(local("archive/sub")))
	remote.addFile(t, "item-new", "item-sub", "new.txt", "new\n")

	assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, map[string]string{
		"photos": "/", "photos/photo.jpg": "photo\n", "archive": "/", "archive/kept.txt": "kept\n",
		"archive/sub": "/", "archive/sub/new.txt": "new\n", "docs": "/", "docs/inner.txt": "inner\n",
	})
	assert.ElementsMatch(t, []string{"item-docs->archive", "item-inner->inner.txt"}, remote.moves,
		"each move made here is one move on OneDrive")
}

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive the file x is moved to zz/a/x2, a folder x is made at the path
// it left and folder f is moved into the new x, before one sync. On this
// computer a file cannot carry f, so f's move waits for x's: one sync finishes
// it, with both sides holding the tree OneDrive has, and nothing moved or
// uploaded on OneDrive.
func TestRunOnce_FolderMovedOnOneDriveIntoAFolderMadeWhereAFileLeftConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-x", "root", "x", "x\n")
	remote.addFolder("item-f", "root", "f")
	remote.addFile(t, "item-c", "item-f", "c.txt", "c\n")
	remote.addFolder("item-zz", "root", "zz")
	remote.addFolder("item-a", "item-zz", "a")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	remote.moveAt(t, "x", "zz/a/x2")
	remote.mkdirAt(t, "x")
	remote.moveAt(t, "f", "x/f")

	assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, map[string]string{
		"zz": "/", "zz/a": "/", "zz/a/x2": "x\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n",
	})
	assert.Empty(t, remote.moves, "nothing is moved on OneDrive")
}

// movedBackCase is one way to take b.txt back out of folder f after f was
// moved into the path folder x left: x moved to x2, a new x made there or
// folder g moved onto it, f moved into x, a new f made, and x/f/b.txt moved
// into the new f.
type movedBackCase struct {
	name string
	// back is where b.txt goes in the new f.
	back string
	// onto moves g onto x instead of making a new x.
	onto bool
	// onOneDrive makes the steps on OneDrive instead of on this computer.
	onOneDrive bool
}

// Validates: R-2.17.6, R-2.17.1
//
// x is moved to x2 and a new x is made, or folder g is moved onto x, f is
// moved into x, a new f is made, and x/f/b.txt is moved into the new f, under
// a new name or under the one it had, on this computer or on OneDrive, before
// one sync. One sync finishes it: both sides end with the tree the user left,
// b.txt where the user put it and still the item it was, nothing is uploaded
// or downloaded again, and no plan drops an action as a dependency cycle.
// Made here, no action is superseded, and the Graph calls are the moves of
// x, f and b.txt and the two new folders; a g moved onto x reaches OneDrive
// as the new x holding g.txt, and g is deleted there. The cases made here
// hold the wait of b.txt
// (TestComposeMoves_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathWaits);
// the cases made on OneDrive hold that the steps converge, with or without
// it.
func TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges(t *testing.T) {
	t.Parallel()

	for _, tc := range []movedBackCase{
		{name: "renamed", back: "f/d.txt"},
		{name: "same name", back: "f/b.txt"},
		{name: "same name, a folder moved onto x", back: "f/b.txt", onto: true},
		{name: "renamed on OneDrive", back: "f/d.txt", onOneDrive: true},
		{name: "same name on OneDrive", back: "f/b.txt", onOneDrive: true},
		{name: "same name, a folder moved onto x on OneDrive", back: "f/b.txt", onto: true, onOneDrive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runFileMovedBackOutOfAMovedFolder(t, &tc)
		})
	}
}

// steps makes the case's changes.
func (tc *movedBackCase) steps(t *testing.T, remote *moveScenarioRemote, syncRoot string) {
	t.Helper()

	if tc.onOneDrive {
		remote.moveAt(t, "x", "x2")
		if tc.onto {
			remote.moveAt(t, "g", "x")
		} else {
			remote.mkdirAt(t, "x")
		}
		remote.moveAt(t, "f", "x/f")
		remote.mkdirAt(t, "f")
		remote.moveAt(t, "x/f/b.txt", tc.back)

		return
	}
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("x"), local("x2")))
	if tc.onto {
		require.NoError(t, os.Rename(local("g"), local("x")))
	} else {
		require.NoError(t, os.Mkdir(local("x"), 0o750))
	}
	require.NoError(t, os.Rename(local("f"), local("x/f")))
	require.NoError(t, os.Mkdir(local("f"), 0o750))
	require.NoError(t, os.Rename(local("x/f/b.txt"), local(tc.back)))
}

func runFileMovedBackOutOfAMovedFolder(t *testing.T, tc *movedBackCase) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-x", "root", "x")
	remote.addFile(t, "item-keep", "item-x", "keep.txt", "keep\n")
	remote.addFolder("item-f", "root", "f")
	remote.addFile(t, "item-b", "item-f", "b.txt", "b\n")
	remote.addFile(t, "item-c", "item-f", "c.txt", "c\n")
	if tc.onto {
		remote.addFolder("item-g", "root", "g")
		remote.addFile(t, "item-g-file", "item-g", "g.txt", "g\n")
	}
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	tc.steps(t, remote, syncRoot)
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")

	want := map[string]string{
		"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n", "f": "/", tc.back: "b\n",
	}
	if tc.onto {
		want["x/g.txt"] = "g\n"
	}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, tc.back, remote.pathOf("item-b"), "b.txt is still the item it was")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "b.txt is moved, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
	if tc.onOneDrive {
		assert.Empty(t, remote.moves, "nothing is moved on OneDrive")
		assert.Empty(t, remote.deletes)
		return
	}

	assert.Zero(t, superseded, "no move names a source a folder's move took away")
	wantGraphMoves := []string{"item-x->x2", "item-f->f", "item-b->" + path.Base(tc.back)}
	var wantDeletes []string
	if tc.onto {
		wantGraphMoves = append(wantGraphMoves, "item-g-file->g.txt")
		wantDeletes = []string{"item-g"}
	}
	assert.ElementsMatch(t, wantGraphMoves, remote.moves, "the Graph moves")
	assert.Equal(t, wantDeletes, remote.deletes)
	assert.ElementsMatch(t, []string{"x", "f"}, remote.folderCreates, "the two new folders are made on OneDrive")
}

// newSubfolderScenarioRemote holds docs/sub/inner.txt, docs/kept.txt and
// photos/photo.jpg.
func newSubfolderScenarioRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFolder("item-sub", "item-docs", "sub")
	remote.addFile(t, "item-inner", "item-sub", "inner.txt", "inner\n")
	remote.addFile(t, "item-kept", "item-docs", "kept.txt", "kept\n")
	remote.addFolder("item-photos", "root", "photos")
	remote.addFile(t, "item-photo", "item-photos", "photo.jpg", "photo\n")

	return remote
}

// assertOneSyncLeavesTree syncs until a run leaves no work and asserts that
// one sync did it, that OneDrive and this computer both hold exactly want,
// that nothing was uploaded or deleted on OneDrive, that no plan dropped an
// action as a dependency cycle, and that the next plan is empty.
func assertOneSyncLeavesTree(
	t *testing.T, eng *testEngine, remote *moveScenarioRemote, syncRoot string, logs *syncLockedBuffer,
	want map[string]string,
) {
	t.Helper()

	runs, _ := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Empty(t, remote.deletes, "nothing is deleted on OneDrive")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// scenarioLocalPath returns the local path of a slash-separated path relative
// to syncRoot.
func scenarioLocalPath(syncRoot string) func(rel string) string {
	return func(rel string) string { return filepath.Join(syncRoot, filepath.FromSlash(rel)) }
}

// newRecreateScenarioEngine syncs remote's tree to a new engine once and
// returns the engine, its sync root and the errors it logs.
func newRecreateScenarioEngine(t *testing.T, remote *moveScenarioRemote) (*testEngine, string, *syncLockedBuffer) {
	t.Helper()

	logs := &syncLockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
	runScenarioPass(t, eng)

	return eng, syncRoot, logs
}

// runScenarioUntilDone syncs until a run leaves no work, at most
// scenarioRunLimit runs, and returns the runs it made and the actions their
// passes superseded. It fails the test when every run left some.
func runScenarioUntilDone(t *testing.T, eng *testEngine) (runs, superseded int) {
	t.Helper()

	var left LeftWork
	for run := 1; run <= scenarioRunLimit; run++ {
		report := runScenarioPass(t, eng)
		superseded += report.Superseded
		left = report.LeftWork()
		if !left.Any() {
			return run, superseded
		}
	}
	assert.Fail(t, "sync never finished", "each of %d runs left work, the last %+v", scenarioRunLimit, left)

	return scenarioRunLimit, superseded
}
