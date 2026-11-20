package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.12.6
func TestRunOnce_RemoteTwinsAreHeldAndTheDriveKeepsSyncing(t *testing.T) {
	t.Parallel()

	// Live: the installed build looped "UNIQUE constraint failed:
	// remote_state.path" for the whole drive after probe L-33 left these two
	// items in one folder.
	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "twin-composed", "root", "שׁ.txt", "composed\n")
	remote.addFile(t, "twin-decomposed", "root", "שׁ.txt", "decomposed\n")
	remote.addFile(t, "other", "root", "other.txt", "other\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	assert.FileExists(t, filepath.Join(syncRoot, "other.txt"), "one remote fact must not stop the drive")
	assert.NoFileExists(t, filepath.Join(syncRoot, syncfixture.RemoteTwinPath), "neither twin syncs")
	requireScenarioIssue(t, eng, syncfixture.RemoteTwinPath, syncdomain.IssueRemoteNameTwin)

	// Deleting one twin on OneDrive lets the survivor sync.
	remote.mu.Lock()
	remote.remove("twin-composed")
	remote.mu.Unlock()
	runScenarioPass(t, eng)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, syncfixture.RemoteTwinPath))
	require.NoError(t, err)
	assert.Equal(t, "decomposed\n", string(data))
	assert.Empty(t, syncstoretest.TwinIssuePaths(t, eng.baseline))
}

// Validates: R-2.12.6
func TestRunOnce_RemoteTwinOfASyncedFileKeepsTheLocalFile(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "synced", "root", "שׁ.txt", "synced\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, syncfixture.RemoteTwinPath))

	remote.mu.Lock()
	remote.addFile(t, "newcomer", "root", "שׁ.txt", "newcomer\n")
	remote.mu.Unlock()
	runScenarioPass(t, eng)

	data, err := localpath.ReadFile(filepath.Join(syncRoot, syncfixture.RemoteTwinPath))
	require.NoError(t, err, "a twin being held must never delete the synced file")
	assert.Equal(t, "synced\n", string(data))
	assert.Empty(t, remote.deletes)
	requireScenarioIssue(t, eng, syncfixture.RemoteTwinPath, syncdomain.IssueRemoteNameTwin)
}
