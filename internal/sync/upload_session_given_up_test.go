package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// uploadSessionRecords lists the upload-session records the engine keeps.
func uploadSessionRecords(t *testing.T, eng *testEngine) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(eng.dataDir, "upload-sessions"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)

	return entries
}

// openSessionURLs returns the URLs of the sessions the scenario remote holds.
func openSessionURLs(remote *moveScenarioRemote) []graph.UploadURL {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	urls := make([]graph.UploadURL, 0, len(remote.sessions))
	for url := range remote.sessions {
		urls = append(urls, url)
	}

	return urls
}

// A large upload interrupted mid-fragment keeps its session record so that
// its retry resumes. When sync then gives the upload up (OneDrive changed the
// file, so the path becomes a conflict), the record goes and the server
// session is canceled: no session outlives its upload. The conflict copy
// holds the local content on both sides, and the file holds OneDrive's.
//
// Validates: R-5.6.13, R-5.2.4
func TestRunOnce_ConflictOverAnInterruptedLargeUploadDropsItsSession(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-big", "root", "big.bin", "synced\n")
	eng, syncRoot := newScenarioSessionEngine(t, remote.sessionClient(t))
	runScenarioPass(t, eng)

	local := strings.Repeat("a", scenarioLargeFileSize)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "big.bin"), []byte(local), 0o600))
	remote.mu.Lock()
	remote.interruptFragmentAt = scenarioSessionFragmentSize
	remote.mu.Unlock()
	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Len(t, report.Errors, 1, "the interrupted fragment fails the upload")
	require.Len(t, uploadSessionRecords(t, eng), 1, "the interrupted upload keeps its record to resume")
	urls := openSessionURLs(remote)
	require.Len(t, urls, 1)
	require.True(t, remote.holdsSession(urls[0]))

	const theirs = "theirs\n"
	remote.putFileAt(t, "big.bin", theirs)
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	assert.Empty(t, uploadSessionRecords(t, eng), "the given-up upload's record is gone")
	assert.False(t, remote.holdsSession(urls[0]), "its server session is canceled")
	tree := remote.tree()
	assert.Equal(t, theirs, tree["big.bin"], "OneDrive's content stays")
	copies := 0
	for remotePath, content := range tree {
		if remotePath != "big.bin" && content == local {
			copies++
		}
	}
	assert.Equal(t, 1, copies, "the conflict copy holds the local content on OneDrive")
	got, err := os.ReadFile(filepath.Join(syncRoot, "big.bin"))
	require.NoError(t, err)
	assert.Equal(t, theirs, string(got))
	requireScenarioQuietPlan(t, eng)
}
