package sync

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
// docs is moved into its own former child, before one sync, here or on
// OneDrive: docs moved to tmp, a new docs made, tmp/sub moved into it and tmp
// moved to docs/sub/docs2. No order of plain moves exists: the new docs takes
// docs's path only once docs has left it, and docs lands below sub, which
// lands in the new docs. Both moves waited for each other's decision and were
// deferred on every pass. docs now moves aside first, carrying sub, and the
// next pass makes the new docs and moves sub into it and docs below sub. One
// sync finishes it: both sides hold the tree made, docs and sub are still the
// items they were, nothing is uploaded or downloaded again, and no plan drops
// an action as a dependency cycle.
func TestRunOnce_FolderMovedIntoItsOwnFormerChildConverges(t *testing.T) {
	t.Parallel()

	for _, onOneDrive := range []bool{false, true} {
		t.Run(fmt.Sprintf("on OneDrive: %v", onOneDrive), func(t *testing.T) {
			t.Parallel()

			remote := newSubfolderScenarioRemote(t)
			eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
			uploads, downloads := remote.uploads, remote.downloads
			if onOneDrive {
				remote.moveAt(t, "docs", "tmp")
				remote.mkdirAt(t, "docs")
				remote.moveAt(t, "tmp/sub", "docs/sub")
				remote.moveAt(t, "tmp", "docs/sub/docs2")
			} else {
				local := scenarioLocalPath(syncRoot)
				require.NoError(t, os.Rename(local("docs"), local("tmp")))
				require.NoError(t, os.Mkdir(local("docs"), 0o750))
				require.NoError(t, os.Rename(local("tmp/sub"), local("docs/sub")))
				require.NoError(t, os.Rename(local("tmp"), local("docs/sub/docs2")))
			}

			runs, _ := runScenarioUntilDone(t, eng)
			assert.Equal(t, 1, runs, "one sync finishes it")
			want := map[string]string{
				"docs": "/", "docs/sub": "/", "docs/sub/inner.txt": "inner\n",
				"docs/sub/docs2": "/", "docs/sub/docs2/kept.txt": "kept\n",
				"photos": "/", "photos/photo.jpg": "photo\n",
			}
			assert.Equal(t, want, remote.tree(), "OneDrive")
			assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
			assert.Equal(t, "docs/sub/docs2", remote.pathOf("item-docs"), "docs is still the folder it was")
			assert.Equal(t, "docs/sub", remote.pathOf("item-sub"), "sub is still the folder it was")
			assert.Equal(t, uploads, remote.uploads, "nothing is uploaded again")
			assert.Equal(t, downloads, remote.downloads, "nothing is downloaded again")
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.2, R-2.17.6
//
// docs is moved to archive, a new docs made, archive/sub/inner.txt moved into
// it and the emptied archive/sub removed, all before one sync, here or on
// OneDrive. Made here, SQLite deletes sub online at docs/sub: the delete
// waits for inner.txt to leave it, docs's move for the delete below its
// source, inner.txt's move for the new docs and the new docs for docs's move,
// a cycle the planner dropped on every pass, so nothing moved on OneDrive and
// every sync left work. The delete now waits a pass: docs's move carries sub
// with inner.txt in it, and the next pass moves inner.txt and then deletes
// sub. One sync finishes it on either side: both hold the tree made, inner.txt
// is still the item it was, nothing is uploaded again, and no content is lost
// to OneDrive's recycle bin or this computer's trash.
func TestRunOnce_FileMovedOutOfASubfolderIntoTheRemadeFolderWithTheSubfolderRemovedConverges(t *testing.T) {
	t.Parallel()

	for _, onOneDrive := range []bool{false, true} {
		t.Run(fmt.Sprintf("on OneDrive: %v", onOneDrive), func(t *testing.T) {
			t.Parallel()

			remote := newSubfolderScenarioRemote(t)
			eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
			if onOneDrive {
				remote.moveAt(t, "docs", "archive")
				remote.mkdirAt(t, "docs")
				remote.moveAt(t, "archive/sub/inner.txt", "docs/inner.txt")
				remote.deleteAt(t, "archive/sub")
			} else {
				local := scenarioLocalPath(syncRoot)
				require.NoError(t, os.Rename(local("docs"), local("archive")))
				require.NoError(t, os.Mkdir(local("docs"), 0o750))
				require.NoError(t, os.Rename(local("archive/sub/inner.txt"), local("docs/inner.txt")))
				require.NoError(t, os.Remove(local("archive/sub")))
			}

			runs, _ := runScenarioUntilDone(t, eng)
			assert.Equal(t, 1, runs, "one sync finishes it")
			want := map[string]string{
				"archive": "/", "archive/kept.txt": "kept\n", "docs": "/", "docs/inner.txt": "inner\n",
				"photos": "/", "photos/photo.jpg": "photo\n",
			}
			assert.Equal(t, want, remote.tree(), "OneDrive")
			assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
			assert.Equal(t, "docs/inner.txt", remote.pathOf("item-inner"), "inner.txt is still the item it was")
			assert.Zero(t, remote.uploads, "nothing is uploaded again")
			assert.NotContains(t, remote.retainedContents(), "inner\n", "inner.txt is not in OneDrive's recycle bin")
			assert.NotContains(t, harnessTrashContents(t, eng), "inner\n", "inner.txt is not in the trash here")
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
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

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
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

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
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

// nestingRotationCase is one rotation through nesting made on OneDrive: an
// entry moved into a folder, and the folder then moved to the entry's old
// path.
type nestingRotationCase struct {
	name        string
	entry, into string
	// folderTo is where the folder then moves.
	folderTo string
	want     map[string]string
}

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive docs is moved into archive and archive then to docs, or
// docs/a.txt into archive and archive onto docs/a.txt, or notes.txt into docs
// and docs onto notes.txt, before one sync. Here the entry still holds the
// path the folder's move lands on and was held for good with the folder's
// move. One sync moves the entry into the folder at the folder's path here
// and then the folder: both sides end with the same tree, nothing is moved
// back on OneDrive, uploaded or downloaded again, and no plan drops an
// action as a dependency cycle.
func TestRunOnce_ARotationThroughNestingMadeOnOneDriveConverges(t *testing.T) {
	t.Parallel()

	for _, tc := range []nestingRotationCase{
		{
			name: "a folder into a folder that takes its path", entry: "docs", into: "archive", folderTo: "docs",
			want: map[string]string{"docs": "/", "docs/docs": "/", "docs/docs/a.txt": "a\n", "notes.txt": "notes\n"},
		},
		{
			name: "a file into a folder moved onto its path", entry: "docs/a.txt", into: "archive", folderTo: "docs/a.txt",
			want: map[string]string{"docs": "/", "docs/a.txt": "/", "docs/a.txt/a.txt": "a\n", "notes.txt": "notes\n"},
		},
		{
			name: "a file into a folder moved onto its path at the top", entry: "notes.txt", into: "docs", folderTo: "notes.txt",
			want: map[string]string{
				"archive": "/", "notes.txt": "/", "notes.txt/a.txt": "a\n", "notes.txt/notes.txt": "notes\n",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runNestingRotationMadeOnOneDrive(t, &tc)
		})
	}
}

func runNestingRotationMadeOnOneDrive(t *testing.T, tc *nestingRotationCase) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFolder("item-archive", "root", "archive")
	remote.addFile(t, "item-notes", "root", "notes.txt", "notes\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.moveAt(t, tc.entry, path.Join(tc.into, path.Base(tc.entry)))
	remote.moveAt(t, tc.into, tc.folderTo)
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the rotation")
	assert.Zero(t, superseded, "no move names a source a folder's move took away")

	assert.Equal(t, tc.want, remote.tree(), "OneDrive")
	assert.Equal(t, tc.want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Empty(t, remote.moves, "nothing is moved back on OneDrive")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "the entries are moved on this computer, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// docs is moved into archive here while OneDrive moves archive into docs and
// notes.txt onto the path archive left: a rotation through nesting split
// across the two sides. OneDrive's moves are kept and this computer's undone
// (R-2.17.6), so here docs leaves archive first, archive then moves into
// docs, and notes.txt onto archive's old path, all in one plan. Before, the
// move of docs out of archive waited for the move onto archive's path, the
// path's own move waited for archive to leave, and archive's move waited for
// docs: a cycle dropped on every pass. One sync converges: both sides end
// with OneDrive's tree, nothing moved on OneDrive, uploaded or downloaded
// again, and no plan drops a cycle.
func TestRunOnce_ARotationThroughNestingSplitAcrossTheSidesKeepsOneDrivesNesting(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFolder("item-archive", "root", "archive")
	remote.addFile(t, "item-notes", "root", "notes.txt", "notes\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.moveAt(t, "archive", "docs/archive")
	remote.moveAt(t, "notes.txt", "archive")
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("docs"), local("archive/docs")))

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Zero(t, superseded)

	want := map[string]string{"docs": "/", "docs/a.txt": "a\n", "docs/archive": "/", "archive": "notes\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Empty(t, remote.moves, "nothing is moved on OneDrive")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "the entries are moved on this computer, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// An earlier sync parked d/c.md at d/c.swap-1.md for a swap on OneDrive, and
// OneDrive has since put that file at c.md, moved the file at c.md into d as
// d/c.md and moved d into e. Here the parked file leaves d for c.md, the file
// at c.md moves into d at d's path here, and d's move carries it into e, all
// in one plan. Before, the file at c.md waited for d's move, d for the parked
// file to leave, and the parked file for c.md: a cycle dropped on every pass
// (chaos seeds 189 and 232). One sync converges with nothing moved on
// OneDrive, uploaded or downloaded.
func TestRunOnce_AParkedFileLeavesItsFolderBeforeTheFolderMovesOnOneDrive(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-d", "root", "d")
	remote.addFile(t, "item-c", "item-d", "c.swap-1.md", "c\n")
	remote.addFolder("item-e", "root", "e")
	remote.addFile(t, "item-r", "root", "c.md", "r\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.moveAt(t, "c.md", "t.md")
	remote.moveAt(t, "d/c.swap-1.md", "c.md")
	remote.moveAt(t, "d", "e/d")
	remote.moveAt(t, "t.md", "e/d/c.md")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Zero(t, superseded)

	want := map[string]string{"c.md": "c\n", "e": "/", "e/d": "/", "e/d/c.md": "r\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Empty(t, remote.moves, "nothing is moved on OneDrive")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "the files are moved here, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.2
//
// Here x moved into Q as Q/renamed-x, with a file written into it, a new x
// was made and Q moved into it, while OneDrive moved Q into the old x.
// OneDrive's move wins, so the old x goes back onto x, which the new folder
// holds while holding the old x's source too. The new folder is renamed to
// a conflict copy and kept; the next pass moves the old x and Q out of it
// into place and uploads the new file into the old x. Before, the old x
// waited for good and every pass failed the create of the new file, whose
// parent the baseline does not have (chaos seed 275). One sync converges:
// every file once, at OneDrive's place, and the conflict copy, empty now,
// on both sides.
func TestRunOnce_AFolderMovedBackOutOfANewFolderAtItsPathKeepsTheNewFolderAsAConflictCopy(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-x", "root", "x")
	remote.addFile(t, "item-xf", "item-x", "xf.txt", "xf\n")
	remote.addFolder("item-Q", "root", "Q")
	remote.addFile(t, "item-qf", "item-Q", "qf.txt", "qf\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("x"), local("Q/renamed-x")))
	require.NoError(t, os.WriteFile(local("Q/renamed-x/new.txt"), []byte("new\n"), 0o600))
	require.NoError(t, os.Mkdir(local("x"), 0o750))
	require.NoError(t, os.Rename(local("Q"), local("x/Q")))
	remote.moveAt(t, "Q", "x/Q")

	runs, _ := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes")

	tree := remote.tree()
	assert.Equal(t, tree, harnessLocalTree(t, syncRoot), "both sides end equal")
	var copies []string
	for p := range tree {
		if strings.HasPrefix(p, "x.conflict-") {
			copies = append(copies, p)
			delete(tree, p)
		}
	}
	assert.Len(t, copies, 1, "the new folder is kept as one conflict copy")
	want := map[string]string{"x": "/", "x/xf.txt": "xf\n", "x/new.txt": "new\n", "x/Q": "/", "x/Q/qf.txt": "qf\n"}
	assert.Equal(t, want, tree, "every file once, at OneDrive's place")
	assert.Equal(t, "x", remote.pathOf("item-x"), "the old x is the folder at x")
	assert.NotContains(t, logs.String(), "has not been synced yet")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9, R-2.17.2
//
// OneDrive deleted the folder archive, which held a synced file, and moved
// notes.txt onto its path. The folder is unchanged here, so the plan deletes
// it here too, and the move of notes.txt onto archive waits for that delete
// and then runs here. Before, a folder at a move's destination made the move
// wait, while notes.txt was downloaded at archive and its copy here uploaded
// again: the file twice on both sides. One sync converges with notes.txt
// once, at archive, the folder's file in the recycle bin as OneDrive left it,
// and nothing uploaded or downloaded.
func TestRunOnce_AFileMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-archive", "root", "archive")
	remote.addFile(t, "item-old", "item-archive", "old.txt", "old\n")
	remote.addFile(t, "item-notes", "root", "notes.txt", "notes\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.deleteAt(t, "archive")
	remote.moveAt(t, "notes.txt", "archive")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes")
	assert.Zero(t, superseded)

	want := map[string]string{"archive": "notes\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "archive", remote.pathOf("item-notes"), "notes.txt is the item OneDrive moved")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Equal(t, downloadsBefore, remote.downloads, "notes.txt is moved here, not downloaded")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9, R-2.17.2
//
// OneDrive deleted the folder archive, which held a synced file, and moved
// the folder notes onto its path. SQLite reconciles archive as one folder
// replaced by another, so the plan deletes the old folder here (it is as
// synced) and the move of notes waits for that delete, and the delete of
// archive/old.txt stays at archive here, where the file is, rather than
// going into notes. Before, the delete was addressed at notes/old.txt and
// the move waited for good. One sync converges with notes at archive,
// nothing uploaded or downloaded.
func TestRunOnce_AFolderMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-archive", "root", "archive")
	remote.addFile(t, "item-old", "item-archive", "old.txt", "old\n")
	remote.addFolder("item-notes", "root", "notes")
	remote.addFile(t, "item-n", "item-notes", "n.txt", "n\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.deleteAt(t, "archive")
	remote.moveAt(t, "notes", "archive")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes")
	assert.Zero(t, superseded)

	want := map[string]string{"archive": "/", "archive/n.txt": "n\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "archive", remote.pathOf("item-notes"), "archive is the folder OneDrive moved")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Equal(t, downloadsBefore, remote.downloads, "notes is moved here, not downloaded")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9, R-2.17.6
//
// The folder docs was renamed docs2 here, carrying a.txt, while OneDrive
// deleted the folder archive and moved docs/a.txt onto its path. The row of
// a.txt is a local move, which names no OneDrive target, so the item ID
// places a.txt at archive on OneDrive: the moves compose, OneDrive's place
// wins, archive is deleted here and a.txt moved onto it, and docs is renamed
// docs2 on OneDrive. Before, a.txt had no OneDrive position: it was
// downloaded at archive while its copy in docs2 went back to OneDrive as a
// new file. One sync converges with a.txt once on each side, nothing
// uploaded or downloaded.
func TestRunOnce_AFileOfAFolderRenamedHereMovedOnOneDriveOntoAFolderItDeletedEndsOnce(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFolder("item-archive", "root", "archive")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("docs"), local("docs2")))
	remote.deleteAt(t, "archive")
	remote.moveAt(t, "docs/a.txt", "archive")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes")
	assert.Zero(t, superseded)

	want := map[string]string{"archive": "a\n", "docs2": "/"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "archive", remote.pathOf("item-a"), "a.txt is the item OneDrive moved")
	assert.Equal(t, "docs2", remote.pathOf("item-docs"), "docs is renamed on OneDrive")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Equal(t, downloadsBefore, remote.downloads, "a.txt is moved here, not downloaded")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// Here the folder docs was renamed shared, while OneDrive renamed the folder
// archive to shared and moved docs/a.txt onto archive's old path. The two
// shared folders merge as one made on both sides, so SQLite never read
// archive's move, and a.txt, which OneDrive moved onto a path another synced
// item had, read as a new file there: it was downloaded at archive while its
// copy here went up again at shared/a.txt. The item ID places it at archive
// on OneDrive, so its moves compose: OneDrive's place wins and a.txt moves
// there here. One sync converges with a.txt once, nothing uploaded or
// downloaded.
func TestRunOnce_AFileOneDriveMovedOntoAPathItsFolderLeftWhileTheFolderMergedHereEndsOnce(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFolder("item-archive", "root", "archive")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("docs"), local("shared")))
	remote.moveAt(t, "archive", "shared")
	remote.moveAt(t, "docs/a.txt", "archive")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes")
	assert.Zero(t, superseded)

	want := map[string]string{"archive": "a\n", "shared": "/"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "archive", remote.pathOf("item-a"), "a.txt is the item OneDrive moved")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Equal(t, downloadsBefore, remote.downloads, "a.txt is moved here, not downloaded")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// Here the folder archive was moved into docs, and docs then renamed to
// archive (a rotation through nesting), while OneDrive moved docs/a.txt into
// archive. SQLite pairs a local move only onto a path no synced item had, so
// it read docs as gone, deleted it on OneDrive, and a.txt, which this
// computer carried along in docs, went up again as a new file beside
// OneDrive's copy. The folder's identity places docs at archive here, and
// the rotation is replayed on OneDrive: archive moves into docs at the path
// docs has there, docs's move to archive then carries it, and a.txt moves
// here into archive/archive, where OneDrive keeps it. One sync converges
// with a.txt once, nothing uploaded or downloaded.
func TestRunOnce_AFolderRotatedThroughNestingHereIsRotatedOnOneDrive(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "a\n")
	remote.addFolder("item-archive", "root", "archive")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("archive"), local("docs/archive")))
	require.NoError(t, os.Rename(local("docs"), local("archive")))
	remote.moveAt(t, "docs/a.txt", "archive/a.txt")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.LessOrEqual(t, runs, 2, "the syncs finish")
	assert.Zero(t, superseded)

	want := map[string]string{"archive": "/", "archive/archive": "/", "archive/archive/a.txt": "a\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "archive", remote.pathOf("item-docs"), "docs is renamed on OneDrive")
	assert.Equal(t, "archive/archive", remote.pathOf("item-archive"), "archive is moved into docs on OneDrive")
	assert.Equal(t, "archive/archive/a.txt", remote.pathOf("item-a"), "a.txt is the item OneDrive moved")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Equal(t, downloadsBefore, remote.downloads, "a.txt is moved here, not downloaded")
	assert.Empty(t, remote.deletes, "nothing is deleted")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.8
//
// Here the folder archive was deleted and notes.txt moved onto its path,
// while OneDrive renamed notes.txt to final.txt. SQLite pairs a local move
// only onto a path no synced item had, so it read notes.txt at archive as
// new content and final.txt as a new file on OneDrive; the file's identity
// places it at archive here, so its moves compose: OneDrive's name wins,
// notes.txt is moved to final.txt here, and archive is deleted on OneDrive.
// Before, final.txt was downloaded and archive uploaded as a new file: the
// content twice on both sides. One sync converges with the file once,
// nothing uploaded or downloaded.
func TestRunOnce_AFileMovedHereOntoAFolderDeletedHereAndRenamedOnOneDriveEndsOnce(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-archive", "root", "archive")
	remote.addFile(t, "item-notes", "root", "notes.txt", "notes\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Remove(local("archive")))
	require.NoError(t, os.Rename(local("notes.txt"), local("archive")))
	remote.moveAt(t, "notes.txt", "final.txt")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes")
	assert.Zero(t, superseded)

	want := map[string]string{"final.txt": "notes\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "final.txt", remote.pathOf("item-notes"), "final.txt is the item OneDrive renamed")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Equal(t, downloadsBefore, remote.downloads, "notes.txt is moved here, not downloaded")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.9, R-2.17.10
//
// OneDrive deleted the folder archive and moved notes.txt onto its path,
// while a new file was saved in archive here. The folder holds more than was
// synced, so it is renamed to a conflict copy with everything in it, the
// move of notes.txt lands at archive here, and the conflict copy goes to
// OneDrive as a new folder. Before, notes.txt was downloaded at archive
// while its copy here went back to OneDrive as a new file: the file twice on
// both sides. One sync converges with notes.txt once, at archive, and the
// new file kept in the conflict copy on both sides.
func TestRunOnce_AFolderWrittenIntoHereThatOneDriveReplacedByAMovedFileIsKeptAsAConflictCopy(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-archive", "root", "archive")
	remote.addFile(t, "item-old", "item-archive", "old.txt", "old\n")
	remote.addFile(t, "item-notes", "root", "notes.txt", "notes\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.WriteFile(local("archive/new.txt"), []byte("new\n"), 0o600))
	remote.deleteAt(t, "archive")
	remote.moveAt(t, "notes.txt", "archive")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.LessOrEqual(t, runs, 2, "the conflict copy goes up in the sync after the rename")
	assert.Zero(t, superseded)

	tree := harnessLocalTree(t, syncRoot)
	assert.Equal(t, tree, remote.tree(), "both sides hold the same")
	assert.Equal(t, "notes\n", tree["archive"], "notes.txt is at archive")
	var copies []string
	for p, content := range tree {
		if content == "notes\n" && p != "archive" {
			copies = append(copies, p)
		}
		if strings.HasSuffix(p, "/new.txt") {
			assert.Equal(t, "new\n", content)
			assert.True(t, strings.HasPrefix(p, "archive.conflict"), "the new file is kept in the conflict copy: %s", p)
		}
	}
	assert.Empty(t, copies, "notes.txt is once on each side")
	assert.Len(t, tree, 4, "notes.txt and the conflict copy with its two files: %v", tree)
	assert.Equal(t, "archive", remote.pathOf("item-notes"), "notes.txt is the item OneDrive moved")
	assert.Equal(t, downloadsBefore, remote.downloads, "notes.txt is moved here, not downloaded")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.1.7
//
// Three folders moved into each other's new folders here: H moved aside,
// Y/f moved into a new Y/f/a/f, Y moved into a new folder H as H/Y, and the
// old H into H/Y/f/a/H, while OneDrive kept all three. Y's move on OneDrive
// lands in the new H, at the path OneDrive still gives the old H, whose own
// target lies below Y: the old H moves aside on OneDrive first, the new H is
// made there and Y moved into it, and later passes do the same for f below
// Y and then move f and the old H into the new f/a. Before, Y's move waited
// while the old H held the path and the old H waited for Y, so every sync
// left the moves for the next one (or, before that, moved Y into the old H
// on OneDrive). One sync converges: both sides hold the tree made here, each
// folder is the item that was moved there, so nothing was moved into or made
// inside an old folder on OneDrive, and nothing is uploaded or downloaded
// again.
func TestRunOnce_ThreeFoldersMovedIntoEachOthersNewFoldersConverge(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-H", "root", "H")
	remote.addFile(t, "item-h", "item-H", "h.txt", "h\n")
	remote.addFolder("item-Y", "root", "Y")
	remote.addFolder("item-f", "item-Y", "f")
	remote.addFile(t, "item-c", "item-f", "c.txt", "c\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("H"), local("H2")))
	require.NoError(t, os.Rename(local("Y/f"), local("tmp")))
	require.NoError(t, os.MkdirAll(local("Y/f/a"), 0o750))
	require.NoError(t, os.Rename(local("tmp"), local("Y/f/a/f")))
	require.NoError(t, os.Rename(local("Y"), local("tmp2")))
	require.NoError(t, os.Mkdir(local("H"), 0o750))
	require.NoError(t, os.Rename(local("tmp2"), local("H/Y")))
	require.NoError(t, os.Rename(local("H2"), local("H/Y/f/a/H")))
	want := harnessLocalTree(t, syncRoot)

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Zero(t, superseded)

	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	for id, at := range map[string]string{"item-H": "H/Y/f/a/H", "item-Y": "H/Y", "item-f": "H/Y/f/a/f"} {
		assert.Equal(t, at, remote.pathOf(id), "%s is the folder moved there", id)
	}
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "the folders are moved on OneDrive, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.1.7
//
// f is moved into a new folder at its own path on each side: here into
// f/a/f, on OneDrive into f/b/f. OneDrive's place wins (R-2.17.6), so f moves
// to f/b/f here, and the new f made here and the one made on OneDrive are
// one folder by path: the create of the one made here adopts OneDrive's,
// f/a is made on OneDrive and f/b here. Before, that create was planned
// without OneDrive's view of the path, which OneDrive's new f holds, so the
// worker's freshness check superseded it on every pass and f's move, waiting
// for f/b, never came. One sync converges: both sides hold both new folders
// and f inside f/b, nothing is moved on OneDrive, and nothing is uploaded or
// downloaded again.
func TestRunOnce_AFolderMovedIntoDifferentNewFoldersAtItsOwnPathOnEachSideConverges(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-f", "root", "f")
	remote.addFile(t, "item-c", "item-f", "c.txt", "c\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.moveAt(t, "f", "tmp")
	remote.mkdirAt(t, "f")
	remote.mkdirAt(t, "f/b")
	remote.moveAt(t, "tmp", "f/b/f")
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("f"), local("tmp")))
	require.NoError(t, os.MkdirAll(local("f/a"), 0o750))
	require.NoError(t, os.Rename(local("tmp"), local("f/a/f")))

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes")
	assert.Zero(t, superseded, "no create is superseded")

	want := map[string]string{"f": "/", "f/a": "/", "f/b": "/", "f/b/f": "/", "f/b/f/c.txt": "c\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Empty(t, remote.moves, "nothing is moved on OneDrive")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "f is moved on this computer, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.9
//
// notes.txt is moved into docs here and renamed g.txt on OneDrive, while a
// new g.txt is made here: two different files given one name, one on each
// side. OneDrive's name wins for the moved file, and the new file is kept
// under a conflict name and uploaded: both sides end with the same tree,
// each content exactly once, and the moved file is still the item OneDrive
// renamed. Before, the moved file was downloaded at g.txt beside its copy in
// docs, and every later sync planned the same again.
func TestRunOnce_AFileMadeHereUnderTheNameOneDriveGaveAMovedFileIsKeptAsAConflictCopy(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-notes", "root", "notes.txt", "notes\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads

	remote.moveAt(t, "notes.txt", "g.txt")
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("notes.txt"), local("docs/notes.txt")))
	require.NoError(t, os.WriteFile(local("g.txt"), []byte("new\n"), 0o600))

	runScenarioUntilDone(t, eng)

	tree := remote.tree()
	assert.Equal(t, tree, harnessLocalTree(t, syncRoot), "both sides end with the same tree")
	assert.Equal(t, "g.txt", remote.pathOf("item-notes"), "the moved file is the item OneDrive renamed")
	contents := map[string]int{}
	for p, content := range tree {
		if content != "/" {
			contents[content]++
			assert.NotEqual(t, "docs/notes.txt", p, "the moved file leaves docs")
		}
	}
	assert.Equal(t, map[string]int{"notes\n": 1, "new\n": 1}, contents, "each content once: the new file is kept, the moved one not copied")
	assert.Equal(t, 1, remote.uploads, "only the new file is uploaded")
	assert.Equal(t, downloadsBefore, remote.downloads, "the moved file is moved here, not downloaded again")
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

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
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

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
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

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive x is moved to x2, folders x and x/sub are made at the path it
// left, and folder f is moved into x/sub, before one sync. On this computer
// the new x is made only once x has left (SQLite plans no create at a path
// the baseline still gives x), so f's move waits for the next pass rather
// than run after x's and find no x/sub. The new x/sub is made in the first
// pass inside the old x, whose move waits for the work below it, so x carries
// it to x2/sub and the next pass moves it to x/sub. One sync finishes it with
// no action superseded (assertChainOfNewFoldersConverges): both sides hold
// the tree OneDrive has, and nothing is moved or uploaded on OneDrive.
func TestRunOnce_FolderMovedOnOneDriveIntoAChainOfNewFoldersConverges(t *testing.T) {
	t.Parallel()

	assertChainOfNewFoldersConverges(t, newFolderPairScenarioRemote(t), "x2", map[string]string{
		"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/sub": "/", "x/sub/f": "/", "x/sub/f/c.txt": "c\n",
	})
}

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive x is moved deeper, to zz/a/x2, folders x and x/sub are made at
// the path it left, and folder f is moved into x/sub, before one sync. On
// this computer f, decided before x, moves into the x/sub made inside the old
// x, and x waits for that move; x then moves, carrying x/sub and f, and the
// next pass moves x/sub, with f in it, to the new x. One sync finishes it with
// no action superseded (assertChainOfNewFoldersConverges): both sides hold
// the tree OneDrive has, and nothing is moved or uploaded on OneDrive.
func TestRunOnce_FolderMovedOnOneDriveIntoNewFoldersWhereAFolderMovedDeeperLeftConverges(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-zz", "root", "zz")
	remote.addFolder("item-a", "item-zz", "a")
	assertChainOfNewFoldersConverges(t, remote, "zz/a/x2", map[string]string{
		"zz": "/", "zz/a": "/", "zz/a/x2": "/", "zz/a/x2/keep.txt": "keep\n",
		"x": "/", "x/sub": "/", "x/sub/f": "/", "x/sub/f/c.txt": "c\n",
	})
}

// assertChainOfNewFoldersConverges syncs remote's tree, then on OneDrive moves
// x to movedTo, makes folders x and x/sub at the path it left and moves f into
// x/sub, and asserts that one sync finishes it with no action superseded,
// both sides holding want (assertOneSyncLeavesTree), and nothing moved on
// OneDrive.
func assertChainOfNewFoldersConverges(t *testing.T, remote *moveScenarioRemote, movedTo string, want map[string]string) {
	t.Helper()

	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	remote.moveAt(t, "x", movedTo)
	remote.mkdirAt(t, "x")
	remote.mkdirAt(t, "x/sub")
	remote.moveAt(t, "f", "x/sub/f")

	superseded := assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, want)
	assert.Zero(t, superseded, "no move is planned before its folder exists here")
	assert.Empty(t, remote.moves, "nothing is moved on OneDrive")
}

// Validates: R-2.17.6, R-2.17.1
//
// Here x is renamed x2, a new x is made and f is moved into it, while X2,
// which OneDrive takes for the same name as x2, is made on OneDrive. The
// rename collides with X2, so x stays where OneDrive has it and waits, its
// way back held by the new x; f's move, which would land in the old x on
// OneDrive, waits with it: the sync moves and makes nothing on OneDrive. Once
// X2 is gone, one sync finishes it with no action superseded: both sides hold
// the tree made here, x and f are each moved once on OneDrive and the new x
// is made there, and no plan drops an action as a dependency cycle.
func TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsTheOldFolderThere(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	moveFIntoANewXHere(t, syncRoot)
	remote.mkdirAt(t, "X2")

	assertHeldUntilTheCollisionClears(t, eng, remote, syncRoot, logs, &collisionHold{
		collision: "X2",
		want:      map[string]string{"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n"},
		moves:     []string{"item-x->x2", "item-f->f"},
		creates:   []string{"x"},
	})
}

// Validates: R-2.17.6, R-2.17.1
//
// Here x is renamed x2, a new x is made and f is moved into it, while a
// folder named exactly x2 is made on OneDrive, before one sync. The two x2
// are one path, and the sync makes them one folder: keep.txt, which x held,
// moves into the x2 OneDrive made, and x stays where OneDrive has it, holding
// f. One sync finishes it with no action superseded: both sides hold the same
// tree, x is still the item at x on OneDrive, and keep.txt and f are each
// moved once there.
func TestRunOnce_MoveIntoAFolderMadeHereWhileOneDriveMakesTheRenamedPathConverges(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	moveFIntoANewXHere(t, syncRoot)
	remote.mkdirAt(t, "x2")

	superseded := assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, map[string]string{
		"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n",
	})
	assert.Zero(t, superseded)
	assert.Equal(t, "x", remote.pathOf("item-x"), "x stays where OneDrive has it")
	assert.Equal(t, "x/f", remote.pathOf("item-f"))
	assert.Equal(t, "x2/keep.txt", remote.pathOf("item-keep"))
	assert.ElementsMatch(t, []string{"item-keep->keep.txt", "item-f->f"}, remote.moves)
	assert.Empty(t, remote.folderCreates)
}

// Validates: R-2.17.6, R-2.17.1
//
// Here x is renamed x2, a new x is made and the folder f is moved into it,
// while X2, which OneDrive takes for x2's name, and a file g.txt in f are made
// on OneDrive. f's move waits for x's, and so does g.txt's download, rebased
// to where f sits here: the plan holds every write rebased through a folder
// whose OneDrive move waits, and does not tell this one, a new file under a
// free name, from those the hold is for, such as a download that applies a
// move OneDrive made into the folder
// (TestRunOnce_AFileMovedOnOneDriveIntoAFolderWhoseMoveWaitsIsMovedHereOnce).
// This test pins the wait, which is the hold's cost, and its end: while X2
// lasts, the sync moves and makes nothing on OneDrive and g.txt waits there.
// Once X2 is gone, one sync finishes it: both sides hold the tree made here
// with g.txt in f, and f and g.txt are the items they were. The download may
// be superseded once, as a download below a folder that the same pass moves
// on OneDrive is without any collision, and is planned again in that sync.
func TestRunOnce_AFileMadeOnOneDriveInAFolderWhoseMoveWaitsIsDownloadedOnceItMoves(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	moveFIntoANewXHere(t, syncRoot)
	remote.mkdirAt(t, "X2")
	remote.addFile(t, "item-g", "item-f", "g.txt", "g\n")

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Superseded)
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.folderCreates)
	assert.NotContains(t, harnessLocalTree(t, syncRoot), "x/f/g.txt", "g.txt waits with f's move")

	remote.deleteAt(t, "X2")
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it once X2 is gone")
	assert.LessOrEqual(t, superseded, 1, "at most the download, planned again")
	want := map[string]string{
		"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n", "x/f/g.txt": "g\n",
	}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "x/f", remote.pathOf("item-f"), "f is still the item it was")
	assert.Equal(t, "x/f/g.txt", remote.pathOf("item-g"), "g.txt is still the item it was")
	assert.ElementsMatch(t, []string{"item-x->x2", "item-f->f"}, remote.moves)
	assert.Zero(t, remote.uploads)
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// Here x is renamed x2, a new x is made, the file pf.txt is moved out of the
// folder P into it and P too, while X2, which OneDrive takes for x2's name, is
// made on OneDrive, pf.txt is deleted there and z.txt is moved into P under
// pf.txt's name. P's move waits for x's, and so does z.txt's move here into
// x/P: the download OneDrive's move would otherwise be applied as waits with
// it. Run, it wrote a second copy of z.txt into x/P while z.txt stayed here,
// and z.txt went back to OneDrive as a new file. While X2 lasts, z.txt stays
// where it is here. Once X2 is gone, one sync finishes it with no action
// superseded: both sides hold the tree made here with z.txt's content once,
// at x/P/pf.txt, the item OneDrive moved.
func TestRunOnce_AFileMovedOnOneDriveIntoAFolderWhoseMoveWaitsIsMovedHereOnce(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-P", "root", "P")
	remote.addFile(t, "item-pf", "item-P", "pf.txt", "pf\n")
	remote.addFile(t, "item-z", "root", "z.txt", "z\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("x"), local("x2")))
	require.NoError(t, os.Mkdir(local("x"), 0o750))
	require.NoError(t, os.Rename(local("P/pf.txt"), local("x/pf.txt")))
	require.NoError(t, os.Rename(local("P"), local("x/P")))
	remote.mkdirAt(t, "X2")
	remote.deleteAt(t, "P/pf.txt")
	remote.moveAt(t, "z.txt", "P/pf.txt")

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Superseded)
	here := harnessLocalTree(t, syncRoot)
	assert.Equal(t, "z\n", here["z.txt"], "z.txt stays here while its move waits")
	assert.NotContains(t, here, "x/P/pf.txt", "no second copy of z.txt")

	remote.deleteAt(t, "X2")
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it once X2 is gone")
	assert.Zero(t, superseded)
	want := map[string]string{
		"x2": "/", "x2/keep.txt": "keep\n", "f": "/", "f/c.txt": "c\n",
		"x": "/", "x/pf.txt": "pf\n", "x/P": "/", "x/P/pf.txt": "z\n",
	}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "x/P/pf.txt", remote.pathOf("item-z"), "z.txt is the item OneDrive moved")
	assert.Equal(t, 1, remote.uploads, "only pf.txt, deleted there and kept here, is uploaded")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// moveFIntoANewXHere renames x to x2 on this computer, makes a new x and moves
// f into it.
func moveFIntoANewXHere(t *testing.T, syncRoot string) {
	t.Helper()

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("x"), local("x2")))
	require.NoError(t, os.Mkdir(local("x"), 0o750))
	require.NoError(t, os.Rename(local("f"), local("x/f")))
}

// Validates: R-2.17.6, R-2.17.1
//
// Here the synced file x2 is deleted, x is renamed x2, a new x is made and f
// is moved into it, before one sync. The sync makes x2 a new folder, as when
// OneDrive makes the renamed path itself
// (TestRunOnce_MoveIntoAFolderMadeHereWhileOneDriveMakesTheRenamedPathConverges):
// on OneDrive it deletes the file x2, makes the folder x2 and moves keep.txt,
// which x held, into it, and x stays where OneDrive has it, holding f. One
// sync finishes it with no action superseded: both sides hold the tree made
// here, x is still the item at x on OneDrive, and keep.txt and f are each
// moved once there.
func TestRunOnce_MoveIntoAFolderMadeHereAfterARenameOntoADeletedFileConverges(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFile(t, "item-x2", "root", "x2", "x2\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	require.NoError(t, os.Remove(scenarioLocalPath(syncRoot)("x2")))
	moveFIntoANewXHere(t, syncRoot)

	assertOneSyncSends(t, eng, remote, syncRoot, logs, &sentOnce{
		want:    map[string]string{"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n"},
		deletes: []string{"item-x2"},
		moves:   []string{"item-keep->keep.txt", "item-f->f"},
		creates: []string{"x2"},
	})
	assert.Equal(t, "x", remote.pathOf("item-x"), "x stays where OneDrive has it")
}

// Validates: R-2.17.6, R-2.17.1
//
// Here the synced file a/x is deleted, x is moved into a in its place, a is
// renamed b, a new x is made and f is moved into it, before one sync. On
// OneDrive x's move to b/x, which is a/x until a's move runs, waits for the
// file the plan deletes there, so x stays at x on OneDrive this pass, and f's
// move into the new x, decided while x waits, is taken back when x is
// deferred: run, it would land in the old x and be carried to b/x/f. One sync
// finishes it with no action superseded: both sides hold the tree made here,
// the file is deleted and the new x made on OneDrive, and a, x and f are each
// moved once there, f straight into the new x.
func TestRunOnce_MoveIntoAFolderMadeHereWhileTheOldFolderWaitsForADeleteConverges(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-a", "root", "a")
	remote.addFile(t, "item-w", "item-a", "x", "w\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Remove(local("a/x")))
	require.NoError(t, os.Rename(local("x"), local("a/x")))
	require.NoError(t, os.Rename(local("a"), local("b")))
	require.NoError(t, os.Mkdir(local("x"), 0o750))
	require.NoError(t, os.Rename(local("f"), local("x/f")))

	assertOneSyncSends(t, eng, remote, syncRoot, logs, &sentOnce{
		want: map[string]string{
			"b": "/", "b/x": "/", "b/x/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n",
		},
		deletes: []string{"item-w"},
		moves:   []string{"item-a->b", "item-x->x", "item-f->f"},
		creates: []string{"x"},
	})
	assert.Equal(t, "b/x", remote.pathOf("item-x"), "x is still the item it was")
}

// heldMoveOutOfADeletedFolderCase is a folder P deleted here after f, which
// it held, was moved into a new x, while a rename made here collides with an
// entry made on OneDrive, so f's move waits.
type heldMoveOutOfADeletedFolderCase struct {
	name string
	// setup adds to OneDrive what the steps need beside P.
	setup func(t *testing.T, remote *moveScenarioRemote)
	// steps are the changes made here, local giving a path in the sync root.
	steps     func(t *testing.T, local func(string) string)
	collision string
	want      map[string]string
	moves     []string
	creates   []string
	deletes   []string
}

// Validates: R-2.17.2, R-2.17.6
//
// Here f, which the folder P holds, is moved into a new x and P is deleted,
// while f's move waits: x is renamed x2 and the rename collides with X2, made
// on OneDrive, or x is moved into a new y whose rename collides with Y2, which
// takes f's move back. On OneDrive P still holds f, so P's delete waits with
// f's move, and so does the delete of every folder above f: run, it deleted f
// with P, and once the collision was gone f was made and its content uploaded
// again as new items. The sync deletes, moves and makes nothing on OneDrive.
// Once the collision is gone, one sync finishes it with no action superseded:
// both sides hold the tree made here, f is moved once on OneDrive and is
// still the item it was, P is deleted after it, and nothing reaches the
// recycle bin.
func TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaits(t *testing.T) {
	t.Parallel()

	xMovedTo := func(into string) map[string]string {
		return map[string]string{into: "/", into + "/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n"}
	}
	for _, tc := range []heldMoveOutOfADeletedFolderCase{
		{
			name: "f's move waits for x",
			steps: func(t *testing.T, local func(string) string) {
				t.Helper()
				moveFOutOfPIntoANewX(t, local, "P/f")
				require.NoError(t, os.Remove(local("P")))
			},
			collision: "X2", want: xMovedTo("x2"),
			moves: []string{"item-x->x2", "item-f->f"}, creates: []string{"x"}, deletes: []string{"item-P"},
		},
		{
			name: "f's move waits for x, two folders deep",
			setup: func(t *testing.T, remote *moveScenarioRemote) {
				t.Helper()
				remote.addFolder("item-Q", "item-P", "Q")
				remote.move("item-f", "item-Q", "f")
			},
			steps: func(t *testing.T, local func(string) string) {
				t.Helper()
				moveFOutOfPIntoANewX(t, local, "P/Q/f")
				require.NoError(t, os.Remove(local("P/Q")))
				require.NoError(t, os.Remove(local("P")))
			},
			collision: "X2", want: xMovedTo("x2"),
			moves: []string{"item-x->x2", "item-f->f"}, creates: []string{"x"}, deletes: []string{"item-Q", "item-P"},
		},
		{
			name: "f's move taken back while x waits in a new y",
			setup: func(t *testing.T, remote *moveScenarioRemote) {
				t.Helper()
				remote.addFolder("item-y", "root", "y")
				remote.addFile(t, "item-yfile", "item-y", "y.txt", "y\n")
			},
			steps: func(t *testing.T, local func(string) string) {
				t.Helper()
				require.NoError(t, os.Rename(local("y"), local("y2")))
				require.NoError(t, os.Mkdir(local("y"), 0o750))
				require.NoError(t, os.Rename(local("x"), local("y/x")))
				require.NoError(t, os.Mkdir(local("x"), 0o750))
				require.NoError(t, os.Rename(local("P/f"), local("x/f")))
				require.NoError(t, os.Remove(local("P")))
			},
			collision: "Y2",
			want: map[string]string{
				"y2": "/", "y2/y.txt": "y\n", "y": "/", "y/x": "/", "y/x/keep.txt": "keep\n",
				"x": "/", "x/f": "/", "x/f/c.txt": "c\n",
			},
			moves: []string{"item-y->y2", "item-x->x", "item-f->f"}, creates: []string{"y", "x"}, deletes: []string{"item-P"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runHeldMoveOutOfADeletedFolder(t, &tc)
		})
	}
}

// moveFOutOfPIntoANewX renames x to x2 on this computer, makes a new x and
// moves f from source into it.
func moveFOutOfPIntoANewX(t *testing.T, local func(string) string, source string) {
	t.Helper()

	require.NoError(t, os.Rename(local("x"), local("x2")))
	require.NoError(t, os.Mkdir(local("x"), 0o750))
	require.NoError(t, os.Rename(local(source), local("x/f")))
}

// runHeldMoveOutOfADeletedFolder syncs once while OneDrive holds
// tc.collision and asserts that nothing is deleted, moved or made there and
// that f stays in P. It then deletes the collision and asserts that one sync
// finishes it with no action superseded, both sides holding tc.want, exactly
// tc.moves, tc.creates and tc.deletes sent to OneDrive, f still the item it
// was, nothing uploaded or recycled, no plan dropping an action as a
// dependency cycle, and a quiet next plan.
func runHeldMoveOutOfADeletedFolder(t *testing.T, tc *heldMoveOutOfADeletedFolderCase) {
	t.Helper()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-P", "root", "P")
	remote.move("item-f", "item-P", "f")
	if tc.setup != nil {
		tc.setup(t, remote)
	}
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	heldAt := remote.pathOf("item-f")
	tc.steps(t, scenarioLocalPath(syncRoot))
	remote.mkdirAt(t, tc.collision)

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Superseded)
	assert.Empty(t, remote.deletes, "P is not deleted on OneDrive while it holds f")
	assert.Empty(t, remote.retainedContents(), "nothing reaches the recycle bin")
	assert.Equal(t, heldAt, remote.pathOf("item-f"), "f stays where OneDrive has it")
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.folderCreates)

	remote.deleteAt(t, tc.collision)
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it once %s is gone", tc.collision)
	assert.Zero(t, superseded)
	assert.Equal(t, tc.want, remote.tree(), "OneDrive")
	assert.Equal(t, tc.want, harnessLocalTree(t, syncRoot), "this computer")
	assert.ElementsMatch(t, tc.moves, remote.moves, "each move made here is one move on OneDrive")
	assert.ElementsMatch(t, tc.creates, remote.folderCreates)
	assert.Equal(t, tc.deletes, remote.deletes)
	assert.Equal(t, "x/f", remote.pathOf("item-f"), "f is still the item it was")
	assert.Empty(t, remote.retainedContents(), "nothing reaches the recycle bin")
	assert.Zero(t, remote.uploads)
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2, R-2.17.6
//
// On OneDrive x is renamed x2, a new x is made, f is moved out of the folder P
// into it and P is deleted, before one sync. On this computer f's move into
// the new x waits for the next pass, which sees x moved away and the new x
// made, so P, which still holds f here, waits with it: run, its delete failed
// on every pass of the sync, as the folder is not empty. One sync finishes it
// with no action superseded or failed: both sides hold OneDrive's tree, f is
// moved here and not downloaded again, and nothing is sent to OneDrive.
func TestRunOnce_AFolderDeletedOnOneDriveStaysHereWhileAMoveOutOfItWaits(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-P", "root", "P")
	remote.move("item-f", "item-P", "f")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads
	remote.moveAt(t, "x", "x2")
	remote.mkdirAt(t, "x")
	remote.moveAt(t, "P/f", "x/f")
	remote.deleteAt(t, "P")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it")
	assert.Zero(t, superseded)
	want := map[string]string{"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, downloadsBefore, remote.downloads, "nothing is downloaded again")
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.deletes)
	assert.Empty(t, remote.folderCreates)
	assert.Zero(t, remote.uploads)
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2, R-2.17.6
//
// Here the folder A is renamed B, the file a1.txt in it is edited and new.txt
// made, and f, which B/P holds, is moved into a new x while B/P is deleted; x
// is renamed x2 and the rename collides with X2, made on OneDrive, so f's
// move waits and so does P's delete. SQLite plans the edited a1.txt as a
// delete of A/a1.txt, which runs before A's rename, and an upload of
// B/a1.txt after it. The rename carries P, and f, along, so it does not wait
// with P's delete: every sync while the collision lasts leaves A renamed B on
// OneDrive with the edited a1.txt and new.txt in it and f still in P. Held
// with P's delete, the rename and both uploads waited for as long as the
// collision lasted, while the delete of a1.txt ran, so OneDrive held no
// a1.txt at all. Once X2 is gone, one sync finishes it with no action
// superseded: both sides hold the tree made here, f is moved once and is
// still the item it was, P is deleted after it, and only a1.txt's old version
// reaches the recycle bin.
func TestRunOnce_AFolderRenamedHereIsRenamedOnOneDriveWhileAMoveOutOfAFolderDeletedInItWaits(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-A", "root", "A")
	remote.addFolder("item-P", "item-A", "P")
	remote.move("item-f", "item-P", "f")
	remote.addFile(t, "item-a1", "item-A", "a1.txt", "a1\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("A"), local("B")))
	require.NoError(t, os.WriteFile(local("B/a1.txt"), []byte("a1 edited\n"), 0o600))
	require.NoError(t, os.WriteFile(local("B/new.txt"), []byte("new\n"), 0o600))
	moveFOutOfPIntoANewX(t, local, "B/P/f")
	require.NoError(t, os.Remove(local("B/P")))
	remote.mkdirAt(t, "X2")

	for sync := 1; sync <= 2; sync++ {
		report := runScenarioPass(t, eng)
		assert.Zero(t, report.Superseded, "sync %d", sync)
		assert.Equal(t, "B", remote.pathOf("item-A"), "sync %d renames A on OneDrive", sync)
		tree := remote.tree()
		assert.Equal(t, "a1 edited\n", tree["B/a1.txt"], "sync %d leaves the edited a1.txt on OneDrive", sync)
		assert.Equal(t, "new\n", tree["B/new.txt"], "sync %d", sync)
		assert.Equal(t, "B/P/f", remote.pathOf("item-f"), "sync %d keeps f in P on OneDrive", sync)
		assert.Equal(t, []string{"item-a1"}, remote.deletes, "sync %d deletes only a1.txt's old copy", sync)
		assert.Equal(t, []string{"a1\n"}, remote.recycled, "sync %d recycles only a1.txt's old copy", sync)
	}

	remote.deleteAt(t, "X2")
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it once X2 is gone")
	assert.Zero(t, superseded)
	want := map[string]string{
		"B": "/", "B/a1.txt": "a1 edited\n", "B/new.txt": "new\n",
		"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n",
	}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "x/f", remote.pathOf("item-f"), "f is still the item it was")
	assert.ElementsMatch(t, []string{"item-A->B", "item-x->x2", "item-f->f"}, remote.moves)
	assert.Equal(t, []string{"x"}, remote.folderCreates)
	assert.Equal(t, []string{"item-a1", "item-P"}, remote.deletes)
	assert.Equal(t, []string{"a1\n"}, remote.recycled, "only a1.txt's old copy is recycled")
	assert.Equal(t, 2, remote.uploads, "a1.txt and new.txt, once each")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2, R-2.17.6
//
// The same on OneDrive before one sync: A renamed B, B/a1.txt edited, x
// renamed x2, a new x made, B/P/f moved into it and B/P deleted. On this
// computer f's move into the new x waits a pass, and so does the delete of P,
// which holds f here, but A's rename here carries P and f along and does not
// wait with it. One sync finishes it with no action superseded or failed:
// both sides hold OneDrive's tree, only the edited a1.txt is downloaded, and
// nothing is sent to OneDrive.
func TestRunOnce_AFolderRenamedOnOneDriveIsRenamedHereWhileAMoveOutOfAFolderDeletedInItWaits(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-A", "root", "A")
	remote.addFolder("item-P", "item-A", "P")
	remote.move("item-f", "item-P", "f")
	remote.addFile(t, "item-a1", "item-A", "a1.txt", "a1\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads
	remote.moveAt(t, "A", "B")
	remote.putFileAt(t, "B/a1.txt", "a1 edited\n")
	remote.moveAt(t, "x", "x2")
	remote.mkdirAt(t, "x")
	remote.moveAt(t, "B/P/f", "x/f")
	remote.deleteAt(t, "B/P")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it")
	assert.Zero(t, superseded)
	want := map[string]string{
		"B": "/", "B/a1.txt": "a1 edited\n", "x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n",
	}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, downloadsBefore+1, remote.downloads, "only the edited a1.txt is downloaded")
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.deletes)
	assert.Empty(t, remote.folderCreates)
	assert.Zero(t, remote.uploads)
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2, R-2.17.6
//
// Here the file K is replaced by a folder K, f is moved out of the folder P
// into it and P is deleted, while K is edited on OneDrive. The plan keeps both
// K: the folder made here is renamed to a conflict copy, and the work below K
// waits for the next pass, f's move into it included, so P, which still holds
// f on OneDrive, waits with it: run, P's delete took f into the recycle bin
// and f and its file were made again as new items. One sync finishes it with
// no action superseded: f is moved once into the conflict copy and is still
// the item it was, P is deleted after it, OneDrive keeps its edited K with
// its old version in its history, and nothing reaches the recycle bin.
func TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaitsBelowAKeptConflict(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-P", "root", "P")
	remote.move("item-f", "item-P", "f")
	remote.addFile(t, "item-K", "root", "K", "k\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Remove(local("K")))
	require.NoError(t, os.Mkdir(local("K"), 0o750))
	require.NoError(t, os.Rename(local("P/f"), local("K/f")))
	require.NoError(t, os.Remove(local("P")))
	remote.putFileAt(t, "K", "k edited\n")

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it")
	assert.Zero(t, superseded)
	require.Len(t, remote.folderCreates, 1, "the conflict copy of the folder K")
	kept := remote.folderCreates[0]
	assert.True(t, strings.HasPrefix(kept, "K.conflict-"), "the folder made here is kept as %s", kept)
	want := map[string]string{
		"K": "k edited\n", kept: "/", kept + "/f": "/", kept + "/f/c.txt": "c\n", "x": "/", "x/keep.txt": "keep\n",
	}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, kept+"/f", remote.pathOf("item-f"), "f is still the item it was")
	assert.Equal(t, kept+"/f/c.txt", remote.pathOf("item-c"))
	assert.Equal(t, []string{"item-f->f"}, remote.moves, "f is moved once")
	assert.Equal(t, []string{"item-P"}, remote.deletes)
	assert.Empty(t, remote.recycled, "nothing reaches the recycle bin")
	assert.Equal(t, []string{"k\n"}, remote.retainedContents(), "K's old version stays in its history")
	assert.Zero(t, remote.uploads)
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2, R-2.17.6
//
// Here f, which the folder P holds, is moved into a new x whose rename
// collides with X2, made on OneDrive, so f's move waits, and the file z.txt is
// renamed onto a path that OneDrive keeps for f until f's move runs: P itself,
// once P is deleted here, or f's own old path in P. SQLite cannot pair the
// rename while OneDrive holds another item at that path, so it plans an upload
// there and z.txt's delete on OneDrive, and the upload waits with f's move. The
// delete waits with the upload: run, it left OneDrive without z.txt's content
// for as long as the collision lasted. While it lasts, the sync deletes,
// moves, makes and uploads nothing on OneDrive, and z.txt is still there. Once
// the collision is gone, one sync finishes it with no action superseded: both
// sides hold the tree made here, and f is moved once on OneDrive and is still
// the item it was.
func TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveUntilItsUploadRuns(t *testing.T) {
	t.Parallel()

	for _, tc := range renamedOntoAWaitingMovesPathCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newRenamedOntoAWaitingMovesPathRemote(t)
			eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
			tc.makeChangesHere(t, scenarioLocalPath(syncRoot))
			remote.mkdirAt(t, "X2")

			report := runScenarioPass(t, eng)
			assert.Zero(t, report.Superseded)
			assert.Equal(t, "z.txt", remote.pathOf("item-z"), "z.txt stays on OneDrive while its upload waits")
			assert.Empty(t, remote.deletes)
			assert.Empty(t, remote.moves)
			assert.Empty(t, remote.folderCreates)
			assert.Zero(t, remote.uploads)

			remote.deleteAt(t, "X2")
			runs, superseded := runScenarioUntilDone(t, eng)
			assert.Equal(t, 1, runs, "one sync finishes it once X2 is gone")
			assert.Zero(t, superseded)
			want := tc.wantTree()
			assert.Equal(t, want, remote.tree(), "OneDrive")
			assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
			assert.Equal(t, "x/f", remote.pathOf("item-f"), "f is still the item it was")
			assert.Equal(t, []string{"item-f->f"}, filterMoves(remote.moves, "item-f"), "f is moved once")
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.2, R-2.17.6
//
// The same two renames of z.txt, with X2 gone and OneDrive refusing, once,
// the create of the new x in the sync that plans f's move into it and the
// upload at the path f leaves. The move waits for the create, the upload for
// the move, and z.txt's delete for the upload, which carries its content, so
// none of them runs while the create fails: run before the upload, the
// delete left OneDrive with no copy of z.txt's content after that sync. After
// every sync z.txt's content is on OneDrive, and once the refused create's
// retry is due the syncs finish with both sides holding the tree made here.
func TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveWhileACreateBeforeItsUploadFails(t *testing.T) {
	t.Parallel()

	for _, tc := range renamedOntoAWaitingMovesPathCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newRenamedOntoAWaitingMovesPathRemote(t)
			logs := &syncfixture.SyncLockedBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
			eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
			clock := syncfixture.NewManualClock(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
			installManualClockForTestEngine(eng, clock)
			runScenarioPass(t, eng)
			tc.makeChangesHere(t, scenarioLocalPath(syncRoot))
			remote.mkdirAt(t, "X2")
			runScenarioPass(t, eng)
			assertOneDriveHolds(t, remote, "z\n", "while the collision lasts")

			remote.deleteAt(t, "X2")
			dropping := countDeletesDropping(remote, "z\n")
			var creates atomic.Int32
			remote.mu.Lock()
			remote.refuseCreate = func(_, name string) error {
				if name != "x" || creates.Add(1) > 1 {
					return nil
				}
				return &graph.GraphError{StatusCode: http.StatusBadRequest, Code: "invalidRequest", Err: graph.ErrBadRequest}
			}
			remote.mu.Unlock()

			report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, report.Failed, "the refused create: %v", report.Errors)
			assertOneDriveHolds(t, remote, "z\n", "after the sync whose create OneDrive refused")
			for run := 1; report.LeftWork().Any(); run++ {
				require.LessOrEqual(t, run, scenarioRunLimit, "the syncs never finish: %+v", report.LeftWork())
				clock.Advance(time.Hour)
				report = runScenarioPass(t, eng)
				assertOneDriveHolds(t, remote, "z\n", "after sync %d", run)
			}
			assert.Zero(t, dropping(), "no delete leaves OneDrive without z.txt's content")
			want := tc.wantTree()
			assert.Equal(t, want, remote.tree(), "OneDrive")
			assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
			assert.Equal(t, "x/f", remote.pathOf("item-f"), "f is still the item it was")
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.2, R-2.17.6
//
// Here the file f in P is deleted and z.txt renamed onto its path, while
// OneDrive renames P to q or moves it into x. SQLite cannot pair the rename,
// as OneDrive keeps f at that path, so it plans an upload there, which goes
// to where OneDrive has P, and z.txt's delete on OneDrive. The upload would
// land on f, which OneDrive keeps until f's delete runs, so it waits a pass,
// and z.txt's delete waits with it: run in the first pass, it left OneDrive
// without z.txt's content until the second. No delete the sync sends leaves
// OneDrive without z.txt's content, and one sync finishes it: both sides hold
// P where OneDrive put it, with z.txt's content at f.
func TestRunOnce_AFileRenamedHereOntoADeletedFileInAFolderMovedOnOneDriveStaysThereUntilItsUploadRuns(t *testing.T) {
	t.Parallel()

	for _, movedTo := range []string{"q", "x/P"} {
		t.Run("P moved to "+movedTo, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			remote.addFolder("item-x", "root", "x")
			remote.addFile(t, "item-keep", "item-x", "keep.txt", "keep\n")
			remote.addFolder("item-P", "root", "P")
			remote.addFile(t, "item-f", "item-P", "f", "f\n")
			remote.addFile(t, "item-z", "root", "z.txt", "z\n")
			eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
			local := scenarioLocalPath(syncRoot)
			require.NoError(t, os.Remove(local("P/f")))
			require.NoError(t, os.Rename(local("z.txt"), local("P/f")))
			remote.moveAt(t, "P", movedTo)
			dropping := countDeletesDropping(remote, "z\n")

			runs, _ := runScenarioUntilDone(t, eng)
			assert.Equal(t, 1, runs, "one sync finishes it")
			assert.Zero(t, dropping(), "no delete leaves OneDrive without z.txt's content")
			want := map[string]string{"x": "/", "x/keep.txt": "keep\n", movedTo: "/", movedTo + "/f": "z\n"}
			assert.Equal(t, want, remote.tree(), "OneDrive")
			assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
			assert.Equal(t, movedTo, remote.pathOf("item-P"), "P is still the item it was")
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// An item is moved into a new folder made at its own path, before one sync:
// the file g.txt into a new folder g.txt, or the folder f, with c.txt in it,
// into a new folder f, each here or on OneDrive. The side that has the item at
// that path keeps it there until it moves, and the item moves into the
// folder, so it first moves aside there to a free name beside its path, and
// a later pass makes the folder at the path it left and moves the item into
// it. Waiting for itself, the file stayed where it was on every sync, and the
// folder's move and the create of the folder it lands in waited for each
// other, a cycle dropped on every pass. One sync finishes it: both sides hold
// the tree the user made, the item is still the item it was, and nothing is
// uploaded or downloaded again.
func TestRunOnce_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, item, at, into string
		onOneDrive           bool
		want                 map[string]string
	}{
		{
			name: "a file, here", item: "item-g", at: "g.txt", into: "g.txt/g.txt",
			want: map[string]string{"x": "/", "x/keep.txt": "keep\n", "f": "/", "f/c.txt": "c\n", "g.txt": "/", "g.txt/g.txt": "g\n"},
		},
		{
			name: "a folder, here", item: "item-f", at: "f", into: "f/f",
			want: map[string]string{"x": "/", "x/keep.txt": "keep\n", "f": "/", "f/f": "/", "f/f/c.txt": "c\n", "g.txt": "g\n"},
		},
		{
			name: "a file, on OneDrive", item: "item-g", at: "g.txt", into: "g.txt/g.txt", onOneDrive: true,
			want: map[string]string{"x": "/", "x/keep.txt": "keep\n", "f": "/", "f/c.txt": "c\n", "g.txt": "/", "g.txt/g.txt": "g\n"},
		},
		{
			name: "a folder, on OneDrive", item: "item-f", at: "f", into: "f/f", onOneDrive: true,
			want: map[string]string{"x": "/", "x/keep.txt": "keep\n", "f": "/", "f/f": "/", "f/f/c.txt": "c\n", "g.txt": "g\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newFolderPairScenarioRemote(t)
			remote.addFile(t, "item-g", "root", "g.txt", "g\n")
			eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
			uploads, downloads := remote.uploads, remote.downloads
			if tc.onOneDrive {
				remote.moveAt(t, tc.at, "aside")
				remote.mkdirAt(t, tc.at)
				remote.moveAt(t, "aside", tc.into)
			} else {
				local := scenarioLocalPath(syncRoot)
				require.NoError(t, os.Rename(local(tc.at), local("aside")))
				require.NoError(t, os.Mkdir(local(tc.at), 0o750))
				require.NoError(t, os.Rename(local("aside"), local(tc.into)))
			}

			runs, superseded := runScenarioUntilDone(t, eng)
			assert.Equal(t, 1, runs, "one sync finishes it")
			assert.Zero(t, superseded)
			assert.Equal(t, tc.want, remote.tree(), "OneDrive")
			assert.Equal(t, tc.want, harnessLocalTree(t, syncRoot), "this computer")
			assert.Equal(t, tc.into, remote.pathOf(tc.item), "the item is still the item it was")
			assert.Equal(t, uploads, remote.uploads, "nothing is uploaded again")
			assert.Equal(t, downloads, remote.downloads, "nothing is downloaded again")
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// Here the folder P is moved into x, x is renamed P, so the old P sits at
// P/P, and the file f in it is edited. SQLite cannot pair x's rename while
// OneDrive keeps the old P at P, so it plans x's delete and a new folder P,
// and the old P's move lands in that folder at its own path: the old P moves
// aside on OneDrive first, and the upload of the edited f, below where the
// old P is going, waits with it. Run, the upload found no folder there and
// failed on every pass, and the delete of f's old copy and the old P's move
// waited for it. One sync finishes it: both sides hold the tree made here,
// the old P is still the folder it was, and f's edit is on OneDrive.
func TestRunOnce_AFolderMovedIntoTheFolderThatTakesItsPathConvergesWithAnEditInIt(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-x", "root", "x")
	remote.addFile(t, "item-keep", "item-x", "keep.txt", "keep\n")
	remote.addFolder("item-P", "root", "P")
	remote.addFile(t, "item-f", "item-P", "f", "f\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("P"), local("x/P")))
	require.NoError(t, os.Rename(local("x"), local("P")))
	require.NoError(t, os.WriteFile(local("P/P/f"), []byte("f edited\n"), 0o600))

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it")
	assert.Zero(t, superseded)
	want := map[string]string{"P": "/", "P/keep.txt": "keep\n", "P/P": "/", "P/P/f": "f edited\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "P/P", remote.pathOf("item-P"), "the old P is still the folder it was")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6
//
// Here the folder f, with c.txt in it, is moved into a new folder at its own
// path: deeper (f/a/f, with the new folder f/a between), beside a new file
// (f/f, with f/new.txt), or deeper below a folder Y renamed Z in the same
// sync (Z/f/a/f, or Z/P/f/a/f with P between), and OneDrive refuses, once or
// not at all, the move that takes the old f aside. Nothing is made at or
// below f's path, where the new folder goes, until the old f has left it: run
// with the move aside, the create of f/a or the upload of new.txt went into
// the old f while that move failed, and after f/a was made there the old f
// could never move into its own subfolder, so the syncs stalled with nothing
// above Debug; below the renamed Z, the create of the new f also collided with
// the old f that Z's move carried there. OneDrive gets nothing made inside the
// old f and holds nothing new in it after the refused sync, and one sync, the
// next one when the move was refused, finishes it with no action superseded:
// both sides hold the tree made here, and the old f, still the folder it was,
// is moved aside and into place. Below the renamed Z, Z's move comes first:
// the old f waits for it, and the next pass finds it in Z. The same rename
// and move made on OneDrive is held the same way on this computer: the create
// of Z/f/a, addressed here at Y/f/a until Y has moved, went into the old f,
// which could then never move into its own subfolder, and every later sync
// waited with only Debug lines. Nothing is sent to OneDrive then, the old f
// here is still the folder it was, and c.txt is not downloaded again. A move
// here cannot be refused by OneDrive, so that case runs without the refusal.
func TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath(t *testing.T) {
	t.Parallel()

	for _, tc := range ownLandingCases() {
		for _, refused := range []bool{false, true} {
			if refused && tc.onOneDrive {
				continue
			}
			t.Run(fmt.Sprintf("%s, moved aside refused once: %v", tc.name, refused), func(t *testing.T) {
				t.Parallel()

				runNothingMadeInTheOldF(t, &tc, refused)
			})
		}
	}
}

// ownLandingCase is one way to move the folder f, item-f with c.txt in it,
// into a new folder made here at its own path.
type ownLandingCase struct {
	name string
	// parents are the folders above f on OneDrive, outermost first ("Y/P");
	// renamed renames Y to Z here, or on OneDrive with onOneDrive, which
	// makes every change of the case there.
	parents    string
	renamed    bool
	onOneDrive bool
	// into is where f goes here, made the new folders it lands in, newFile a
	// file made here, want the tree both sides end with, and moves the moves
	// sent to OneDrive, in order.
	into, made, newFile string
	want                map[string]string
	moves               []string
}

func ownLandingCases() []ownLandingCase {
	parked := []string{"item-f->f.swap-1", "item-f->f"}
	renamedFirst := []string{"item-Y->Z", "item-f->f.swap-1", "item-f->f"}

	return []ownLandingCase{
		{
			name: "deeper", into: "f/a/f", made: "f/a", moves: parked,
			want: map[string]string{"f": "/", "f/a": "/", "f/a/f": "/", "f/a/f/c.txt": "c\n"},
		},
		{
			name: "beside a new file", into: "f/f", made: "f", newFile: "f/new.txt", moves: parked,
			want: map[string]string{"f": "/", "f/f": "/", "f/f/c.txt": "c\n", "f/new.txt": "new\n"},
		},
		{
			name: "deeper below its renamed folder", parents: "Y", renamed: true,
			into: "Z/f/a/f", made: "Z/f/a", moves: renamedFirst,
			want: map[string]string{"Z": "/", "Z/f": "/", "Z/f/a": "/", "Z/f/a/f": "/", "Z/f/a/f/c.txt": "c\n"},
		},
		{
			name: "deeper below its folder renamed on OneDrive", parents: "Y", renamed: true, onOneDrive: true,
			into: "Z/f/a/f", made: "Z/f/a",
			want: map[string]string{"Z": "/", "Z/f": "/", "Z/f/a": "/", "Z/f/a/f": "/", "Z/f/a/f/c.txt": "c\n"},
		},
		{
			name: "deeper below a folder in its renamed folder", parents: "Y/P", renamed: true,
			into: "Z/P/f/a/f", made: "Z/P/f/a", moves: renamedFirst,
			want: map[string]string{
				"Z": "/", "Z/P": "/", "Z/P/f": "/", "Z/P/f/a": "/", "Z/P/f/a/f": "/", "Z/P/f/a/f/c.txt": "c\n",
			},
		},
	}
}

// remote is OneDrive's tree for the case: f/c.txt below its parents.
func (tc *ownLandingCase) remote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	parentID := syncdomain.StrRoot
	if tc.parents != "" {
		for _, name := range strings.Split(tc.parents, "/") {
			remote.addFolder("item-"+name, parentID, name)
			parentID = "item-" + name
		}
	}
	remote.addFolder("item-f", parentID, "f")
	remote.addFile(t, "item-c", "item-f", "c.txt", "c\n")

	return remote
}

// makeChangesHere renames the outermost parent Z if the case does, then moves
// f aside, makes the new folders and moves f into them.
func (tc *ownLandingCase) makeChangesHere(t *testing.T, local func(string) string) {
	t.Helper()

	at := "f"
	if tc.parents != "" {
		at = tc.parents + "/f"
	}
	if tc.renamed {
		require.NoError(t, os.Rename(local("Y"), local("Z")))
		at = "Z" + strings.TrimPrefix(at, "Y")
	}
	require.NoError(t, os.Rename(local(at), local("aside")))
	require.NoError(t, os.MkdirAll(local(tc.made), 0o750))
	require.NoError(t, os.Rename(local("aside"), local(tc.into)))
	if tc.newFile != "" {
		require.NoError(t, os.WriteFile(local(tc.newFile), []byte("new\n"), 0o600))
	}
}

// makeChangesOnOneDrive is makeChangesHere made on OneDrive, for a case
// without a new file.
func (tc *ownLandingCase) makeChangesOnOneDrive(t *testing.T, remote *moveScenarioRemote) {
	t.Helper()

	at := "f"
	if tc.parents != "" {
		at = tc.parents + "/f"
	}
	if tc.renamed {
		remote.moveAt(t, "Y", "Z")
		at = "Z" + strings.TrimPrefix(at, "Y")
	}
	remote.moveAt(t, at, "aside")
	made := ""
	for _, name := range strings.Split(tc.made, "/") {
		made = joinScenarioPath(made, name)
		remote.mkdirAt(t, made)
	}
	remote.moveAt(t, "aside", tc.into)
}

// joinScenarioPath joins a slash-separated parent path and a name.
func joinScenarioPath(dir, name string) string {
	if dir == "" {
		return name
	}

	return dir + "/" + name
}

// runNothingMadeInTheOldF runs tc, with OneDrive refusing the old f's move
// aside once when refused.
func runNothingMadeInTheOldF(t *testing.T, tc *ownLandingCase, refused bool) {
	t.Helper()

	remote := tc.remote(t)
	logs := &syncfixture.SyncLockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
	clock := syncfixture.NewManualClock(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	runScenarioPass(t, eng)
	local := scenarioLocalPath(syncRoot)
	oldF, oldErr := os.Stat(local(path.Join(tc.parents, "f")))
	require.NoError(t, oldErr)
	downloads := remote.downloads
	if tc.onOneDrive {
		tc.makeChangesOnOneDrive(t, remote)
	} else {
		tc.makeChangesHere(t, local)
	}
	madeInTheOldF := watchCreatesIn(remote, "item-f")
	if refused {
		refuseMoveAsideOnce(remote, "item-f")
		report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, report.Failed, "the refused move: %v", report.Errors)
		remote.mu.Lock()
		assert.Equal(t, []string{"item-c"}, remote.childIDs("item-f"), "the old f holds nothing new after the refused sync")
		remote.mu.Unlock()
		clock.Advance(time.Hour)
	}

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes it")
	assert.Zero(t, superseded)
	assert.Empty(t, madeInTheOldF(), "nothing is made inside the old f on OneDrive")
	assert.Equal(t, tc.want, remote.tree(), "OneDrive")
	assert.Equal(t, tc.want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, tc.into, remote.pathOf("item-f"), "the old f is still the folder it was")
	assert.Equal(t, tc.moves, remote.moves, "the moves sent to OneDrive, in order")
	if tc.onOneDrive {
		movedF, statErr := os.Stat(local(tc.into))
		require.NoError(t, statErr)
		assert.True(t, os.SameFile(oldF, movedF), "the old f here is still the folder it was")
		assert.Equal(t, downloads, remote.downloads, "c.txt is not downloaded again")
	}
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// refuseMoveAsideOnce makes remote refuse the first move of itemID to a
// parking name.
func refuseMoveAsideOnce(remote *moveScenarioRemote, itemID string) {
	var parks atomic.Int32
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.refuseMove = func(id, newName string) error {
		if id != itemID || !strings.Contains(newName, syncplan.MoveSwapInfix) || parks.Add(1) > 1 {
			return nil
		}
		return &graph.GraphError{StatusCode: http.StatusBadRequest, Code: "invalidRequest", Err: graph.ErrBadRequest}
	}
}

// watchCreatesIn watches the creates the engine sends to remote, folders and
// files, and returns the names of those sent into the item parentID.
func watchCreatesIn(remote *moveScenarioRemote, parentID string) func() []string {
	var made []string
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.refuseCreate = func(parent, name string) error {
		if parent == parentID {
			remote.mu.Lock()
			made = append(made, name)
			remote.mu.Unlock()
		}
		return nil
	}

	return func() []string {
		remote.mu.Lock()
		defer remote.mu.Unlock()

		return slices.Clone(made)
	}
}

// Validates: R-2.17.2, R-2.17.6
//
// Here the folder P is renamed Q and the file f in it moved up to P, the path
// the folder left. SQLite cannot pair f's rename while OneDrive keeps the
// folder at P, so it plans an upload at P, which waits for P's move, and the
// delete of P/f, which P's move waits for, as children leave first: the
// delete cannot also wait for the upload. It waits for the next pass instead,
// so P's move carries f's old copy to Q/f, the upload runs after the move,
// and the next pass deletes Q/f. Run first, the delete left OneDrive without
// f's content until the upload ran, and for as long as the upload failed. No
// delete the sync sends leaves OneDrive without f's content, whether every
// create succeeds or OneDrive refuses the upload once; the syncs finish with
// both sides holding the tree made here. Refused, the upload is planned no
// more: with P moved, the next pass pairs f's rename and moves f's copy from
// Q/f to P, still the item it was.
func TestRunOnce_AFileMovedUpOntoItsRenamedFoldersPathStaysOnOneDriveUntilItsUploadRuns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		refuse  bool
		deletes []string
	}{
		{refuse: false, deletes: []string{"item-f"}},
		{refuse: true},
	} {
		refuse := tc.refuse
		t.Run(fmt.Sprintf("upload refused once: %v", refuse), func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			remote.addFolder("item-P", "root", "P")
			remote.addFile(t, "item-f", "item-P", "f", "f\n")
			logs := &syncfixture.SyncLockedBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
			eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
			clock := syncfixture.NewManualClock(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
			installManualClockForTestEngine(eng, clock)
			runScenarioPass(t, eng)
			local := scenarioLocalPath(syncRoot)
			require.NoError(t, os.Rename(local("P"), local("Q")))
			require.NoError(t, os.Rename(local("Q/f"), local("P")))
			dropping := countDeletesDropping(remote, "f\n")
			var uploads atomic.Int32
			remote.mu.Lock()
			remote.refuseCreate = func(_, name string) error {
				if !refuse || name != "P" || uploads.Add(1) > 1 {
					return nil
				}
				return &graph.GraphError{StatusCode: http.StatusBadRequest, Code: "invalidRequest", Err: graph.ErrBadRequest}
			}
			remote.mu.Unlock()

			report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			assertOneDriveHolds(t, remote, "f\n", "after the first sync")
			for run := 1; report.LeftWork().Any(); run++ {
				require.LessOrEqual(t, run, scenarioRunLimit, "the syncs never finish: %+v", report.LeftWork())
				clock.Advance(time.Hour)
				report = runScenarioPass(t, eng)
				assertOneDriveHolds(t, remote, "f\n", "after sync %d", run)
			}
			assert.Zero(t, dropping(), "no delete leaves OneDrive without f's content")
			want := map[string]string{"Q": "/", "P": "f\n"}
			assert.Equal(t, want, remote.tree(), "OneDrive")
			assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
			assert.Equal(t, "Q", remote.pathOf("item-P"), "Q is still the folder it was")
			assert.Equal(t, tc.deletes, remote.deletes)
			if refuse {
				assert.Equal(t, "P", remote.pathOf("item-f"), "f is moved, still the item it was")
			}
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.12, R-2.17.2
//
// Here the file f is moved up out of its folder P, P deleted and f moved onto
// P's path. SQLite plans the delete of P/f, the delete of P, and the upload of
// f at P, which waits for P's delete, which waits for P/f's: the delete of
// P/f cannot also wait for the upload. Run first, it left OneDrive without f's
// content for as long as the upload failed. The planner moves P/f aside on
// OneDrive instead, to a free temporary name above the deleted folder, and
// the next pass deletes it once the upload holds the content, or, with the
// upload refused once, moves it back to P, still the item it was. No delete
// the sync sends leaves OneDrive without f's content, and the syncs finish
// with both sides holding f at P.
func TestRunOnce_AFileMovedUpOntoItsDeletedFoldersPathStaysOnOneDriveUntilItsUploadRuns(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		refuse  bool
		deletes []string
	}{
		{refuse: false, deletes: []string{"item-P", "item-f"}},
		{refuse: true, deletes: []string{"item-P"}},
	} {
		refuse := tc.refuse
		t.Run(fmt.Sprintf("upload refused once: %v", refuse), func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			remote.addFolder("item-P", "root", "P")
			remote.addFile(t, "item-f", "item-P", "f", "f\n")
			logs := &syncfixture.SyncLockedBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
			eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
			clock := syncfixture.NewManualClock(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
			installManualClockForTestEngine(eng, clock)
			runScenarioPass(t, eng)
			local := scenarioLocalPath(syncRoot)
			require.NoError(t, os.Rename(local("P/f"), local("g")))
			require.NoError(t, os.Remove(local("P")))
			require.NoError(t, os.Rename(local("g"), local("P")))
			dropping := countDeletesDropping(remote, "f\n")
			var uploads atomic.Int32
			remote.mu.Lock()
			remote.refuseCreate = func(_, name string) error {
				if !refuse || name != "P" || uploads.Add(1) > 1 {
					return nil
				}
				return &graph.GraphError{StatusCode: http.StatusBadRequest, Code: "invalidRequest", Err: graph.ErrBadRequest}
			}
			remote.mu.Unlock()

			report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			assertOneDriveHolds(t, remote, "f\n", "after the first sync")
			for run := 1; report.LeftWork().Any(); run++ {
				require.LessOrEqual(t, run, scenarioRunLimit, "the syncs never finish: %+v", report.LeftWork())
				clock.Advance(time.Hour)
				report = runScenarioPass(t, eng)
				assertOneDriveHolds(t, remote, "f\n", "after sync %d", run)
			}
			assert.Zero(t, dropping(), "no delete leaves OneDrive without f's content")
			want := map[string]string{"P": "f\n"}
			assert.Equal(t, want, remote.tree(), "OneDrive")
			assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
			assert.ElementsMatch(t, tc.deletes, remote.deletes)
			if refuse {
				assert.Equal(t, "P", remote.pathOf("item-f"), "f is moved back, still the item it was")
			}
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.2, R-2.17.6
//
// Here the folder P is renamed Q, its subfolder S is renamed Q/T or moved out
// to R/S, and the file f in S is edited. SQLite plans the upload of the edit
// at f's new path and the delete of P/S/f, which P's move, or S's, waits for,
// as children leave first: the upload waits for the moves, so the delete
// cannot also wait for it. The delete keeps the old order, as the moves would
// carry the old copy to the very path the upload writes: followed through
// both moves, S's renamed below Q after P's, or S's leaving P first, the old
// copy lands at f's new path. Held instead, the upload landed on that copy
// and was superseded, and a second pass wrote it. One sync finishes it with no
// action superseded: both sides hold the tree made here, with the edit on
// OneDrive.
func TestRunOnce_AFileEditedInASubfolderMovedWithItsRenamedFolderIsUploadedOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, at string
		moves    []string
		want     map[string]string
	}{
		{
			name: "renamed in it", at: "Q/T", moves: []string{"item-P->Q", "item-S->T"},
			want: map[string]string{"Q": "/", "Q/T": "/", "Q/T/f": "edited\n"},
		},
		{
			name: "moved out of it", at: "R/S", moves: []string{"item-S->S", "item-P->Q"},
			want: map[string]string{"Q": "/", "R": "/", "R/S": "/", "R/S/f": "edited\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			remote.addFolder("item-P", "root", "P")
			remote.addFolder("item-S", "item-P", "S")
			remote.addFile(t, "item-f", "item-S", "f", "f\n")
			if _, inR := tc.want["R"]; inR {
				remote.addFolder("item-R", "root", "R")
			}
			eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
			local := scenarioLocalPath(syncRoot)
			require.NoError(t, os.Rename(local("P"), local("Q")))
			require.NoError(t, os.Rename(local("Q/S"), local(tc.at)))
			require.NoError(t, os.WriteFile(local(tc.at+"/f"), []byte("edited\n"), 0o600))

			runs, superseded := runScenarioUntilDone(t, eng)
			assert.Equal(t, 1, runs, "one sync finishes it")
			assert.Zero(t, superseded)
			assert.Equal(t, tc.want, remote.tree(), "OneDrive")
			assert.Equal(t, tc.want, harnessLocalTree(t, syncRoot), "this computer")
			assert.ElementsMatch(t, tc.moves, remote.moves)
			assert.Equal(t, []string{"item-f"}, remote.deletes, "the old copy is deleted")
			assert.Equal(t, 1, remote.uploads, "the edit is uploaded once")
			assert.NotContains(t, logs.String(), "dependency cycle")
			requireScenarioQuietPlan(t, eng)
		})
	}
}

// Validates: R-2.17.1, R-2.17.6
//
// Here a.txt and b.txt trade names in another case, a.txt becoming B.txt and
// b.txt A.txt. OneDrive takes each new name for the old name the other file
// still has, so a plan of uploads and deletes
// would hold two content edges that close a cycle only together, each upload
// waiting for the delete of the name it takes
// (TestBuildDependencies_OfTwoContentEdgesThatCloseACycleOnlyTogetherTheFirstIsKept).
// The renames are paired instead: each file is moved on OneDrive, one of them
// aside first, and nothing is uploaded or deleted. Two moves onto names that
// fold together have no order (addRemoteCaseVariantDeps), so a move may meet
// its name still taken and be retried by a later sync.
func TestRunOnce_NamesTradedHereInAnotherCaseAreMovedOnOneDrive(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-a", syncdomain.StrRoot, "a.txt", "a\n")
	remote.addFile(t, "item-b", syncdomain.StrRoot, "b.txt", "b\n")
	logs := &syncfixture.SyncLockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
	clock := syncfixture.NewManualClock(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	runScenarioPass(t, eng)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("a.txt"), local("t")))
	require.NoError(t, os.Rename(local("b.txt"), local("A.txt")))
	require.NoError(t, os.Rename(local("t"), local("B.txt")))

	for run := 1; ; run++ {
		require.LessOrEqual(t, run, scenarioRunLimit, "the syncs never finish")
		report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		if !report.LeftWork().Any() && report.Failed == 0 {
			break
		}
		clock.Advance(time.Hour)
	}
	want := map[string]string{"A.txt": "b\n", "B.txt": "a\n"}
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, "B.txt", remote.pathOf("item-a"), "a.txt is still the item it was")
	assert.Equal(t, "A.txt", remote.pathOf("item-b"), "b.txt is still the item it was")
	assert.Zero(t, remote.uploads, "nothing is uploaded")
	assert.Empty(t, remote.deletes, "nothing is deleted")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// countDeletesDropping watches the deletes the engine sends to remote and
// returns a count of those after which no file there holds content.
func countDeletesDropping(remote *moveScenarioRemote, content string) func() int {
	dropping := 0
	remote.mu.Lock()
	defer remote.mu.Unlock()
	remote.afterDelete = func(live map[string]string) {
		if !slices.Contains(slices.Collect(maps.Values(live)), content) {
			dropping++
		}
	}

	return func() int {
		remote.mu.Lock()
		defer remote.mu.Unlock()

		return dropping
	}
}

// renamedOntoAWaitingMovesPathCase is one path OneDrive keeps for f, which
// the folder P holds, until f's move into a new x runs: z.txt is renamed onto
// it here.
type renamedOntoAWaitingMovesPathCase struct {
	name string
	// steps rename z.txt here, local giving a path in the sync root.
	steps func(t *testing.T, local func(string) string)
	// want is the part of the tree made here that the steps change.
	want map[string]string
}

func renamedOntoAWaitingMovesPathCases() []renamedOntoAWaitingMovesPathCase {
	return []renamedOntoAWaitingMovesPathCase{
		{
			name: "onto P, deleted here",
			steps: func(t *testing.T, local func(string) string) {
				t.Helper()
				require.NoError(t, os.Remove(local("P")))
				require.NoError(t, os.Rename(local("z.txt"), local("P")))
			},
			want: map[string]string{"P": "z\n"},
		},
		{
			name: "onto f's old path in P",
			steps: func(t *testing.T, local func(string) string) {
				t.Helper()
				require.NoError(t, os.Rename(local("z.txt"), local("P/f")))
			},
			want: map[string]string{"P": "/", "P/f": "z\n"},
		},
	}
}

// makeChangesHere moves f out of P into a new x, x itself renamed x2, and
// renames z.txt as the case does.
func (tc *renamedOntoAWaitingMovesPathCase) makeChangesHere(t *testing.T, local func(string) string) {
	t.Helper()

	moveFOutOfPIntoANewX(t, local, "P/f")
	tc.steps(t, local)
}

// wantTree is the tree made here once every move has run.
func (tc *renamedOntoAWaitingMovesPathCase) wantTree() map[string]string {
	want := map[string]string{"x2": "/", "x2/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n"}
	maps.Copy(want, tc.want)

	return want
}

// newRenamedOntoAWaitingMovesPathRemote holds x/keep.txt, P/f/c.txt and z.txt.
func newRenamedOntoAWaitingMovesPathRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-P", "root", "P")
	remote.move("item-f", "item-P", "f")
	remote.addFile(t, "item-z", "root", "z.txt", "z\n")

	return remote
}

// assertOneDriveHolds asserts that a file on OneDrive holds content.
func assertOneDriveHolds(t *testing.T, remote *moveScenarioRemote, content string, msgAndArgs ...any) {
	t.Helper()

	assert.Contains(t, slices.Collect(maps.Values(remote.tree())), content, msgAndArgs...)
}

// filterMoves keeps the moves of one item.
func filterMoves(moves []string, itemID string) []string {
	var kept []string
	for _, move := range moves {
		if strings.HasPrefix(move, itemID+"->") {
			kept = append(kept, move)
		}
	}

	return kept
}

// sentOnce is the tree one sync leaves on both sides and what it sends to
// OneDrive.
type sentOnce struct {
	want    map[string]string
	deletes []string
	moves   []string
	creates []string
}

// assertOneSyncSends syncs until a run leaves no work and asserts that one
// sync did it with no action superseded, both sides holding sent.want, exactly
// sent.deletes, sent.moves and sent.creates sent to OneDrive, nothing
// uploaded, no plan dropping an action as a dependency cycle, and a quiet next
// plan.
func assertOneSyncSends(
	t *testing.T, eng *testEngine, remote *moveScenarioRemote, syncRoot string, logs *syncfixture.SyncLockedBuffer,
	sent *sentOnce,
) {
	t.Helper()

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Zero(t, superseded)
	assert.Equal(t, sent.want, remote.tree(), "OneDrive")
	assert.Equal(t, sent.want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, sent.deletes, remote.deletes)
	assert.ElementsMatch(t, sent.moves, remote.moves, "each move made here is one move on OneDrive")
	assert.Equal(t, sent.creates, remote.folderCreates)
	assert.Zero(t, remote.uploads)
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// Here y is renamed y2 and made again, x is moved into the new y and made
// again, and f is moved into the new x, while Y2, which OneDrive takes for the
// same name as y2, is made on OneDrive. The rename collides with Y2, so y
// stays where OneDrive has it and waits, x's move into the new y waits with
// it, and so does f's move, which would land in the old x on OneDrive and stay
// there for as long as the collision lasts, although f is decided before x:
// the sync moves and makes nothing on OneDrive. Once Y2 is gone, one sync
// finishes it with no action superseded: both sides hold the tree made here,
// y, x and f are each moved once on OneDrive, f straight into the new x, and
// the new y and x are made there.
func TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileTheOldFolderWaitsInANewFolder(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFolder("item-y", "root", "y")
	remote.addFile(t, "item-yfile", "item-y", "y.txt", "y\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local("y"), local("y2")))
	require.NoError(t, os.Mkdir(local("y"), 0o750))
	require.NoError(t, os.Rename(local("x"), local("y/x")))
	require.NoError(t, os.Mkdir(local("x"), 0o750))
	require.NoError(t, os.Rename(local("f"), local("x/f")))
	remote.mkdirAt(t, "Y2")

	assertHeldUntilTheCollisionClears(t, eng, remote, syncRoot, logs, &collisionHold{
		collision: "Y2",
		want: map[string]string{
			"y2": "/", "y2/y.txt": "y\n", "y": "/", "y/x": "/", "y/x/keep.txt": "keep\n",
			"x": "/", "x/f": "/", "x/f/c.txt": "c\n",
		},
		moves:   []string{"item-y->y2", "item-x->x", "item-f->f"},
		creates: []string{"y", "x"},
	})
}

// collisionHold is a scenario whose moves made here wait while OneDrive
// holds collision, an entry a rename made here collides with: want is the
// tree both sides hold once it is gone, moves and creates what OneDrive is
// then sent.
type collisionHold struct {
	collision string
	want      map[string]string
	moves     []string
	creates   []string
}

// assertHeldUntilTheCollisionClears syncs once while OneDrive holds
// hold.collision and asserts that nothing is moved or made there and that x
// and f stay where OneDrive has them, f out of the old x. It then deletes the
// collision and asserts that one sync finishes it with no action superseded,
// both sides holding hold.want, exactly hold.moves and hold.creates sent to
// OneDrive, nothing uploaded, no plan dropping an action as a dependency
// cycle, and a quiet next plan.
func assertHeldUntilTheCollisionClears(
	t *testing.T, eng *testEngine, remote *moveScenarioRemote, syncRoot string, logs *syncfixture.SyncLockedBuffer,
	hold *collisionHold,
) {
	t.Helper()

	runScenarioPass(t, eng)
	assert.Equal(t, "f", remote.pathOf("item-f"), "f is not moved into the old x on OneDrive")
	assert.Equal(t, "x", remote.pathOf("item-x"))
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.folderCreates)

	remote.deleteAt(t, hold.collision)
	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves once %s is gone", hold.collision)
	assert.Zero(t, superseded)
	assert.Equal(t, hold.want, remote.tree(), "OneDrive")
	assert.Equal(t, hold.want, harnessLocalTree(t, syncRoot), "this computer")
	assert.ElementsMatch(t, hold.moves, remote.moves, "each move made here is one move on OneDrive")
	assert.ElementsMatch(t, hold.creates, remote.folderCreates, "the new folders are made on OneDrive")
	assert.Zero(t, remote.uploads)
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.6, R-2.17.1
//
// Here x is moved into a new folder made at f's path and f into a new folder
// made at x's, before one sync. On OneDrive each move lands in a folder made
// at the path the other folder leaves, so planned together they wait for each
// other, a cycle dropped on every pass. x, decided first, moves into the old
// f and f waits; f then moves, carrying x, and x moves into the new f. One sync
// finishes it (assertOneSyncLeavesTree) with no action superseded: both sides
// hold the tree made here, x and f are still the items they were, x is moved
// twice and f once on OneDrive, both new folders are made there, and nothing
// is downloaded again.
func TestRunOnce_TwoFoldersMovedHereIntoNewFoldersAtEachOthersPathsConverge(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	downloadsBefore := remote.downloads
	moveEachIntoANewFolderAtTheOthersPath(t, syncRoot, "x", "f")

	superseded := assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, map[string]string{
		"f": "/", "f/x": "/", "f/x/keep.txt": "keep\n", "x": "/", "x/f": "/", "x/f/c.txt": "c\n",
	})
	assert.Zero(t, superseded, "each move is planned by the pass that runs it, never retried")
	assert.ElementsMatch(t, []string{"item-x->x", "item-f->f", "item-x->x"}, remote.moves,
		"x into the old f, f carrying x into the new x, then x into the new f")
	assert.Equal(t, "f/x", remote.pathOf("item-x"), "x is still the item it was")
	assert.Equal(t, "x/f", remote.pathOf("item-f"), "f is still the item it was")
	assert.ElementsMatch(t, []string{"x", "f"}, remote.folderCreates, "the two new folders are made on OneDrive")
	assert.Equal(t, downloadsBefore, remote.downloads, "nothing is downloaded again")
}

// Validates: R-2.17.6, R-2.17.1
//
// Here the file a is moved into a new folder made at f's path and f into a new
// folder made at a's, before one sync. On OneDrive f, decided first, waits for
// a, a file still to be decided; a then moves into the old f, which the next
// pass moves into the new a, carrying a, and a moves into the new f. Taken back
// instead, a's move would wait with f, and neither would ever move. One sync
// finishes it with no action superseded: both sides hold the tree made here,
// a is moved twice and f once on OneDrive, and both new folders are made
// there.
func TestRunOnce_FileAndFolderMovedHereIntoNewFoldersAtEachOthersPathsConverge(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFile(t, "item-a", "root", "a", "a\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	moveEachIntoANewFolderAtTheOthersPath(t, syncRoot, "a", "f")

	superseded := assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, map[string]string{
		"a": "/", "a/f": "/", "a/f/c.txt": "c\n", "f": "/", "f/a": "a\n", "x": "/", "x/keep.txt": "keep\n",
	})
	assert.Zero(t, superseded)
	assert.ElementsMatch(t, []string{"item-a->a", "item-f->f", "item-a->a"}, remote.moves)
	assert.Equal(t, "f/a", remote.pathOf("item-a"), "a is still the item it was")
	assert.ElementsMatch(t, []string{"a", "f"}, remote.folderCreates)
}

// Validates: R-2.17.6, R-2.17.1
//
// Here the file a is moved into a new folder made at b's path and the file b
// into a new folder made at a's, before one sync. On OneDrive neither new
// folder can be made while the file at its path stays, and neither file can
// leave its path before the other's folder is made, so both moves wait, and
// every sync says so. Planned anyway, a move into the file OneDrive still has
// at the other's path was refused on every pass. Each of two syncs supersedes
// nothing and sends nothing to OneDrive, which keeps both files where they
// were, and this computer keeps the tree made here.
func TestRunOnce_TwoFilesMovedHereIntoNewFoldersAtEachOthersPathsWait(t *testing.T) {
	t.Parallel()

	remote := newFolderPairScenarioRemote(t)
	remote.addFile(t, "item-a", "root", "a", "a\n")
	remote.addFile(t, "item-b", "root", "b", "b\n")
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	moveEachIntoANewFolderAtTheOthersPath(t, syncRoot, "a", "b")
	onOneDrive := map[string]string{"a": "a\n", "b": "b\n", "x": "/", "x/keep.txt": "keep\n", "f": "/", "f/c.txt": "c\n"}
	here := map[string]string{"a": "/", "a/b": "b\n", "b": "/", "b/a": "a\n", "x": "/", "x/keep.txt": "keep\n", "f": "/", "f/c.txt": "c\n"}

	for sync := 1; sync <= 2; sync++ {
		report := runScenarioPass(t, eng)
		assert.Zero(t, report.Superseded, "sync %d plans no move OneDrive refuses", sync)
		assert.True(t, report.LeftWork().Any(), "sync %d says the moves wait", sync)
	}
	assert.Empty(t, remote.moves)
	assert.Empty(t, remote.folderCreates)
	assert.Empty(t, remote.deletes)
	assert.Zero(t, remote.uploads)
	assert.Equal(t, onOneDrive, remote.tree(), "OneDrive")
	assert.Equal(t, here, harnessLocalTree(t, syncRoot), "this computer")
	assert.NotContains(t, logs.String(), "dependency cycle")
}

// moveEachIntoANewFolderAtTheOthersPath moves first into a new folder made at
// second's path and second into a new folder made at first's, on this
// computer.
func moveEachIntoANewFolderAtTheOthersPath(t *testing.T, syncRoot, first, second string) {
	t.Helper()

	local := scenarioLocalPath(syncRoot)
	require.NoError(t, os.Rename(local(first), local(first+"-moving")))
	require.NoError(t, os.Rename(local(second), local(second+"-moving")))
	require.NoError(t, os.Mkdir(local(first), 0o750))
	require.NoError(t, os.Mkdir(local(second), 0o750))
	require.NoError(t, os.Rename(local(first+"-moving"), local(second+"/"+first)))
	require.NoError(t, os.Rename(local(second+"-moving"), local(first+"/"+second)))
}

// Validates: R-2.17.6, R-2.17.1
//
// Here three folders are moved in a ring, each into a new folder made at the
// path the next one leaves: a into a new b, b into a new c and c into a new a,
// before one sync. On OneDrive c moves into the new a and a into the old b,
// while b, decided last, waits: its move lands where the moves of c and a
// leave. b then moves into the new c, carrying a, and a moves into the new b.
// One sync finishes it with no action superseded: both sides hold the tree
// made here, the three new folders are made on OneDrive, and a is moved twice
// there, b and c once.
func TestRunOnce_ThreeFoldersMovedHereInARingOfNewFoldersConverge(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	ring := [][2]string{{"a", "b/a"}, {"b", "c/b"}, {"c", "a/c"}}
	for _, move := range ring {
		remote.addFolder("item-"+move[0], "root", move[0])
		remote.addFile(t, "item-"+move[0]+"-file", "item-"+move[0], move[0]+".txt", move[0]+"\n")
	}
	eng, syncRoot, logs := newRecreateScenarioEngine(t, remote)
	local := scenarioLocalPath(syncRoot)
	for _, move := range ring {
		require.NoError(t, os.Rename(local(move[0]), local(move[0]+"-moving")))
		require.NoError(t, os.Mkdir(local(move[0]), 0o750))
	}
	for _, move := range ring {
		require.NoError(t, os.Rename(local(move[0]+"-moving"), local(move[1])))
	}

	superseded := assertOneSyncLeavesTree(t, eng, remote, syncRoot, logs, map[string]string{
		"a": "/", "a/c": "/", "a/c/c.txt": "c\n", "b": "/", "b/a": "/", "b/a/a.txt": "a\n",
		"c": "/", "c/b": "/", "c/b/b.txt": "b\n",
	})
	assert.Zero(t, superseded)
	assert.ElementsMatch(t, []string{"item-c->c", "item-a->a", "item-b->b", "item-a->a"}, remote.moves)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, remote.folderCreates)
}

// newFolderPairScenarioRemote holds x/keep.txt and f/c.txt.
func newFolderPairScenarioRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-x", "root", "x")
	remote.addFile(t, "item-keep", "item-x", "keep.txt", "keep\n")
	remote.addFolder("item-f", "root", "f")
	remote.addFile(t, "item-c", "item-f", "c.txt", "c\n")

	return remote
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

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
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

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
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
// action as a dependency cycle, and that the next plan is empty. It returns
// the actions the sync superseded.
func assertOneSyncLeavesTree(
	t *testing.T, eng *testEngine, remote *moveScenarioRemote, syncRoot string, logs *syncfixture.SyncLockedBuffer,
	want map[string]string,
) (superseded int) {
	t.Helper()

	runs, superseded := runScenarioUntilDone(t, eng)
	assert.Equal(t, 1, runs, "one sync finishes the moves")
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Zero(t, remote.uploads, "nothing is uploaded again")
	assert.Empty(t, remote.deletes, "nothing is deleted on OneDrive")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)

	return superseded
}

// scenarioLocalPath returns the local path of a slash-separated path relative
// to syncRoot.
func scenarioLocalPath(syncRoot string) func(rel string) string {
	return func(rel string) string { return filepath.Join(syncRoot, filepath.FromSlash(rel)) }
}

// newRecreateScenarioEngine syncs remote's tree to a new engine once and
// returns the engine, its sync root and the errors it logs.
func newRecreateScenarioEngine(t *testing.T, remote *moveScenarioRemote) (*testEngine, string, *syncfixture.SyncLockedBuffer) {
	t.Helper()

	logs := &syncfixture.SyncLockedBuffer{}
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

// savedAtTheFreedName is what another program saves at b.txt while a
// replacing move of a.txt onto it runs.
const savedAtTheFreedName = "saved meanwhile\n"

// Validates: R-2.17.9, R-6.5.2
//
// OneDrive moved a.txt onto b.txt, replacing b. The local replacing move took
// the old b.txt into the trash, and before it renamed a.txt there another
// program saved a file at b.txt: the rename refuses the taken name, the move
// is superseded, and the run says it left work. The next pass keeps the saved
// file as a conflict copy and finishes the move, instead of downloading the
// moved item beside its old copy; the old b.txt stays in the trash once. The
// pass after it uploads the conflict copy as a new file and nothing else:
// OneDrive never holds a.txt again, nothing is deleted there, and the moved
// item's content is unchanged.
func TestRunOnce_ReplacingMoveFindingANewFileAtTheFreedNameStillMoves(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFile(t, "item-b", "root", "b.txt", "bravo\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)
	moved := syncfixture.EntryIdentity(t, filepath.Join(h.syncRoot, "a.txt"))
	remote.deleteRemotely("item-b")
	remote.move("item-a", "root", "b.txt")

	// The executor reaches the crash point on a worker: the hook reports a
	// failed write as its error rather than failing the test from there.
	var saved atomic.Bool
	h.eng.armExecCrash(func(point string) error {
		if point != syncdomain.CrashPointLocalMoveDestinationTrashed || saved.Swap(true) {
			return nil
		}

		return os.WriteFile(filepath.Join(h.syncRoot, "b.txt"), []byte(savedAtTheFreedName), 0o600)
	})
	h.eng.hooks.oneShotPassLimit = 1

	first, err := h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.True(t, saved.Load(), "the replacing move reached the point between its two steps")
	assert.Equal(t, 1, first.Superseded, "the move found its destination taken")
	assert.True(t, first.LeftWork().Any(), "the run says it left the move: %+v", first.LeftWork())
	assert.Equal(t, map[string]string{"a.txt": "alpha\n", "b.txt": savedAtTheFreedName, "keep.txt": "keep\n"},
		harnessLocalTree(t, h.syncRoot))
	syncfixture.RequireTrashHoldsOnce(t, h.eng.upkeep.trash, "b.txt", "bravo\n")

	h.eng.armExecCrash(func(string) error { return nil })
	second := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, second.Moves, "the next pass finishes the move")
	assert.Equal(t, 1, second.ConflictCopies, "the saved file is kept as a conflict copy")
	assert.Zero(t, second.Downloads, "the moved item is not downloaded beside its old copy")
	assert.Zero(t, second.Uploads)
	assert.Zero(t, second.RemoteDeletes)
	local := harnessLocalTree(t, h.syncRoot)
	assert.NotContains(t, local, "a.txt", "the moved item left its old path")
	assert.Equal(t, "alpha\n", local["b.txt"])
	assert.Equal(t, moved, syncfixture.EntryIdentity(t, filepath.Join(h.syncRoot, "b.txt")), "b.txt is the moved file itself")
	assert.Contains(t, slices.Collect(maps.Values(local)), savedAtTheFreedName, "the saved file's content is kept: %v", local)
	syncfixture.RequireTrashHoldsOnce(t, h.eng.upkeep.trash, "b.txt", "bravo\n")

	h.eng.hooks.oneShotPassLimit = 0
	third := runScenarioPass(t, h.eng)

	assert.Equal(t, 1, third.Uploads, "only the conflict copy is uploaded")
	tree := h.remote.tree()
	assert.NotContains(t, tree, "a.txt", "nothing is put back at the moved item's old path on OneDrive")
	assert.Equal(t, "alpha\n", tree["b.txt"], "the moved item's content on OneDrive is unchanged")
	assert.Contains(t, slices.Collect(maps.Values(tree)), savedAtTheFreedName, "the saved file reaches OneDrive as a new file")
	assert.Equal(t, harnessLocalTree(t, h.syncRoot), tree, "both sides hold the same files")
	assert.Empty(t, h.remote.deletes, "nothing is deleted on OneDrive")
	requireScenarioQuietPlan(t, h.eng)
}
