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
)

// Validates: R-2.17.4
//
// A folder renamed differently on both sides keeps the OneDrive name and
// moves the local copy. Inside it, a new local file and a new OneDrive folder
// took one name (chaos seed 87). The download of the folder's child was
// planned at the local folder's name, below the local file, failed there
// every pass, and held the move: sync never converged. Both are kept, as at
// a path no rename touched (TestRunOnce_CreateConflictLocalFileRemoteDirKeepsBoth).
func TestRunOnce_CreateConflictInsideADivergentlyRenamedFolderKeepsBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-x", "root", "x")
	remote.addFile(t, "item-keep", "item-x", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "x"), filepath.Join(syncRoot, "docs")))
	syncfixture.WriteLocalFile(t, syncRoot, filepath.Join("docs", "thing"), "local file\n")
	remote.moveAt(t, "x", "remote-docs")
	remote.mkdirAt(t, "remote-docs/thing")
	remote.putFileAt(t, "remote-docs/thing/inside.txt", "remote child\n")

	for range modelMaxSyncPasses {
		report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		if modelPassChanges(report) == 0 && report.Failed == 0 {
			break
		}
	}

	tree := remote.tree()
	assert.Equal(t, "remote child\n", tree["remote-docs/thing/inside.txt"])
	assert.Equal(t, "keep\n", tree["remote-docs/keep.txt"])
	assert.Contains(t, modelContents(tree), "local file\n", "the local file is kept beside OneDrive's folder")
	assert.FileExists(t, filepath.Join(syncRoot, "remote-docs", "thing", "inside.txt"))
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.4, R-2.17.9
//
// A file moved here, with a new file saved at its old path, while OneDrive
// swapped it with another synced file: the item keeps OneDrive's name, so its
// copy here moves onto the other file, whose own copy moves onto the old
// path the new file holds. The new file is not the item, so it is kept as a
// conflict copy and the moves land. Waiting for it to leave held both moves
// for good (chaos seeds 162 and 385).
func TestRunOnce_ANewFileAtAMovedFilesOldPathGivesWayToASwapOnOneDrive(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-x", "root", "x")
	remote.addFolder("item-docs", "item-x", "Docs")
	remote.addFile(t, "item-a", "item-x", "A.txt", "item a\n")
	remote.addFile(t, "item-c", "root", "c.md", "item c\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "x", "A.txt"), filepath.Join(syncRoot, "x", "Docs", "b.txt")))
	syncfixture.WriteLocalFile(t, syncRoot, filepath.Join("x", "A.txt"), "a new file\n")
	remote.moveAt(t, "x/A.txt", "swap.tmp")
	remote.moveAt(t, "c.md", "x/A.txt")
	remote.moveAt(t, "swap.tmp", "c.md")

	converged := false
	for range modelMaxSyncPasses {
		report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		if modelPassChanges(report) == 0 && report.Failed == 0 {
			converged = true
			break
		}
	}
	require.True(t, converged, "the moves land instead of waiting on each other")

	tree := remote.tree()
	assert.Equal(t, "item a\n", tree["c.md"], "the item keeps OneDrive's name")
	assert.Equal(t, "item c\n", tree["x/A.txt"])
	assert.Contains(t, modelContents(tree), "a new file\n", "the new file is kept beside them")
	requireScenarioLocalFile(t, syncRoot, "c.md", "item a\n")
	requireScenarioLocalFile(t, syncRoot, "x/A.txt", "item c\n")
	requireScenarioQuietPlan(t, eng)
}
