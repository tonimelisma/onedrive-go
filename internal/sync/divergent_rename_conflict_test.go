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
