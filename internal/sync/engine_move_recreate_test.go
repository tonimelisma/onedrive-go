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
// into destParent as "photos", a new docs made at its old path, and one file
// of the moved folder moved into the new docs.
type recreatedFolderMoveCase struct {
	name string
	// fileName is the file the second move takes into the new docs.
	fileName string
	// destParent is the folder the first move puts docs into.
	destParent string
	// oneRun makes both moves before a single run instead of a run after each.
	oneRun bool
	// keepName moves the file into the new docs under the name it had.
	keepName bool
	// onOneDrive makes the moves on OneDrive instead of on this computer.
	onOneDrive bool
}

// Validates: R-2.17.6, R-2.17.1
//
// docs is moved into photos as photos/photos and a new docs is made, then a
// file of the moved folder is moved into the new docs, on this computer or on
// OneDrive, with a sync after each step or one sync for both. One sync
// finishes the moves: both sides end with the same tree, the file is moved on
// the other side rather than sent again, and no plan drops an action as a
// dependency cycle.
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runMoveThroughRecreatedFolder(t, &tc)
		})
	}
}

// steps returns the two changes of the case: docs moved to moved and a new
// docs made, then the file moved from the moved folder to renamed.
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

	local := func(rel string) string { return filepath.Join(syncRoot, filepath.FromSlash(rel)) }

	return func() {
			require.NoError(t, os.Rename(local("docs"), local(moved)))
			require.NoError(t, os.Mkdir(local("docs"), 0o750))
		}, func() {
			require.NoError(t, os.Rename(local(path.Join(moved, tc.fileName)), local(renamed)))
		}
}

func runMoveThroughRecreatedFolder(t *testing.T, tc *recreatedFolderMoveCase) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-moved", "item-docs", tc.fileName, "moved\n")
	remote.addFile(t, "item-kept", "item-docs", "kept.txt", "kept\n")
	remote.addFolder("item-photos", "root", "photos")
	remote.addFile(t, "item-photo", "item-photos", "photo.jpg", "photo\n")
	want := map[string]string{"photos": "/", "photos/photo.jpg": "photo\n"}
	if tc.destParent != "photos" {
		remote.addFolder("item-album", "item-photos", path.Base(tc.destParent))
		want[tc.destParent] = "/"
	}

	logs := &syncLockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, syncRoot := newTestEngineWithLogger(t, remote.client(t), logger)
	runScenarioPass(t, eng)

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
	assert.Equal(t, 1, runScenarioUntilDone(t, eng), "one sync finishes the moves")

	want["docs"] = "/"
	want[renamed] = "moved\n"
	want[moved] = "/"
	want[path.Join(moved, "kept.txt")] = "kept\n"
	assert.Equal(t, want, remote.tree(), "OneDrive")
	assert.Equal(t, want, harnessLocalTree(t, syncRoot), "this computer")
	assert.Equal(t, renamed, remote.pathOf("item-moved"), "the file is moved on OneDrive, not uploaded again")
	assert.Zero(t, remote.uploads)
	assert.Equal(t, downloadsBefore, remote.downloads, "the file is moved on this computer, not downloaded again")
	assert.NotContains(t, logs.String(), "dependency cycle")
	requireScenarioQuietPlan(t, eng)
}

// runScenarioUntilDone syncs until a run leaves no work, at most
// scenarioRunLimit runs, and returns the runs it made. It fails the test when
// every run left some.
func runScenarioUntilDone(t *testing.T, eng *testEngine) int {
	t.Helper()

	var left LeftWork
	for run := 1; run <= scenarioRunLimit; run++ {
		left = runScenarioPass(t, eng).LeftWork()
		if !left.Any() {
			return run
		}
	}
	assert.Fail(t, "sync never finished", "each of %d runs left work, the last %+v", scenarioRunLimit, left)

	return scenarioRunLimit
}
