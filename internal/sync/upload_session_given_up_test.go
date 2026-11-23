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

// givenUpFileName is the large file whose interrupted upload sync gives up.
const givenUpFileName = "big.bin"

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
	remote.addFile(t, "item-big", "root", givenUpFileName, "synced\n")
	eng, syncRoot := newScenarioSessionEngine(t, remote.sessionClient(t))
	runScenarioPass(t, eng)

	local := strings.Repeat("a", scenarioLargeFileSize)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, givenUpFileName), []byte(local), 0o600))
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
	remote.putFileAt(t, givenUpFileName, theirs)
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	assert.Empty(t, uploadSessionRecords(t, eng), "the given-up upload's record is gone")
	assert.False(t, remote.holdsSession(urls[0]), "its server session is canceled")
	tree := remote.tree()
	assert.Equal(t, theirs, tree[givenUpFileName], "OneDrive's content stays")
	copies := 0
	for remotePath, content := range tree {
		if remotePath != givenUpFileName && content == local {
			copies++
		}
	}
	assert.Equal(t, 1, copies, "the conflict copy holds the local content on OneDrive")
	got, err := os.ReadFile(filepath.Join(syncRoot, givenUpFileName)) //nolint:gosec // the test reads its own sync folder
	require.NoError(t, err)
	assert.Equal(t, theirs, string(got))
	requireScenarioQuietPlan(t, eng)
}

// interruptLargeUpload writes content of scenarioLargeFileSize to rel and
// runs one pass whose upload fragment is interrupted: the pass fails that
// upload, keeps one session record to resume it, and OneDrive holds the
// session, whose URL it returns.
func interruptLargeUpload(t *testing.T, eng *testEngine, remote *moveScenarioRemote, syncRoot, rel, content string) graph.UploadURL {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, rel), []byte(content), 0o600))
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

	return urls[0]
}

// requireNoSessionOutlivesItsUpload checks that no session record is left
// and that OneDrive no longer holds the given-up upload's session.
func requireNoSessionOutlivesItsUpload(t *testing.T, eng *testEngine, remote *moveScenarioRemote, givenUp graph.UploadURL) {
	t.Helper()

	assert.Empty(t, uploadSessionRecords(t, eng), "no upload-session record is left")
	assert.False(t, remote.holdsSession(givenUp), "the given-up upload's server session is canceled")
}

// A new large file's interrupted upload holds its record under the create
// key (its absolute path). When OneDrive gets a file of its own at that path,
// the create becomes a conflict, sync gives the upload up, and the record and
// the server session go with it; the conflict copy holds the local content
// on both sides.
//
// Validates: R-5.6.13, R-5.2.4
func TestRunOnce_ConflictOverAnInterruptedLargeCreateDropsItsSession(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-other", "root", "other.txt", "other\n")
	eng, syncRoot := newScenarioSessionEngine(t, remote.sessionClient(t))
	runScenarioPass(t, eng)

	local := strings.Repeat("n", scenarioLargeFileSize)
	givenUp := interruptLargeUpload(t, eng, remote, syncRoot, givenUpFileName, local)

	const theirs = "theirs\n"
	remote.putFileAt(t, givenUpFileName, theirs)
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	requireNoSessionOutlivesItsUpload(t, eng, remote, givenUp)
	tree := remote.tree()
	assert.Equal(t, theirs, tree[givenUpFileName], "OneDrive's content stays")
	copies := 0
	for remotePath, content := range tree {
		if remotePath != givenUpFileName && content == local {
			copies++
		}
	}
	assert.Equal(t, 1, copies, "the conflict copy holds the local content on OneDrive")
	got, err := os.ReadFile(filepath.Join(syncRoot, givenUpFileName)) //nolint:gosec // the test reads its own sync folder
	require.NoError(t, err)
	assert.Equal(t, theirs, string(got))
	requireScenarioQuietPlan(t, eng)
}

// A synced file's interrupted large upload is given up when the user moves
// the file: the upload at the old path is no longer wanted, so its record
// and its server session go, and the content reaches OneDrive at the new
// path through an upload of its own, which leaves no record either.
//
// Validates: R-5.6.13
func TestRunOnce_LocalMoveOfAnInterruptedLargeUploadDropsItsSession(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-big", "root", givenUpFileName, "synced\n")
	eng, syncRoot := newScenarioSessionEngine(t, remote.sessionClient(t))
	runScenarioPass(t, eng)

	local := strings.Repeat("m", scenarioLargeFileSize)
	givenUp := interruptLargeUpload(t, eng, remote, syncRoot, givenUpFileName, local)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, givenUpFileName), filepath.Join(syncRoot, "moved.bin")))
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	requireNoSessionOutlivesItsUpload(t, eng, remote, givenUp)
	tree := remote.tree()
	assert.Equal(t, local, tree["moved.bin"], "the moved file's content reaches OneDrive at its new path")
	assert.NotContains(t, tree, givenUpFileName, "nothing is left at the old path")
	requireScenarioQuietPlan(t, eng)
}

// A synced file's interrupted large upload is given up when the user deletes
// the file: its record and its server session go, and the delete reaches
// OneDrive.
//
// Validates: R-5.6.13
func TestRunOnce_LocalDeleteOfAnInterruptedLargeUploadDropsItsSession(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-big", "root", givenUpFileName, "synced\n")
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newScenarioSessionEngine(t, remote.sessionClient(t))
	runScenarioPass(t, eng)

	givenUp := interruptLargeUpload(t, eng, remote, syncRoot, givenUpFileName, strings.Repeat("d", scenarioLargeFileSize))

	require.NoError(t, os.Remove(filepath.Join(syncRoot, givenUpFileName)))
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)
	runScenarioPass(t, eng)

	requireNoSessionOutlivesItsUpload(t, eng, remote, givenUp)
	tree := remote.tree()
	assert.NotContains(t, tree, givenUpFileName, "the local delete reaches OneDrive")
	assert.Equal(t, "keep\n", tree["keep.txt"])
	requireScenarioQuietPlan(t, eng)
}
