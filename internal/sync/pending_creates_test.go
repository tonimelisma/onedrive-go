package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// emptyQuickXor is the hash OneDrive gives an empty file (L-17, L-01).
const emptyQuickXor = "AAAAAAAAAAAAAAAAAAAAAAAAAAA="

func readBaselineRow(t *testing.T, mgr *SyncStore, itemID string) (path string, localSize int64, found bool) {
	t.Helper()

	err := mgr.rawDB().QueryRowContext(t.Context(),
		`SELECT path, coalesce(local_size, -1) FROM baseline WHERE item_id = ?`, itemID).Scan(&path, &localSize)
	if err != nil {
		return "", 0, false
	}

	return path, localSize, true
}

// Validates: R-5.6.15
func TestCommitObservation_OwnPlaceholderIsAdoptedAsEmptySyncedContent(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: `"p,1"`,
	}}, "t1", driveID))

	// Recorded as synced empty content: the local file reads as changed, so
	// the planner uploads by ID instead of a create_conflict that renamed
	// the user's file and downloaded the empty one.
	path, localSize, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found)
	assert.Equal(t, "doc.txt", path)
	assert.Zero(t, localSize)
}

// Validates: R-5.6.15
func TestCommitObservation_PlaceholderStateSeenAfterTheCommitIsNotARemoteChange(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "item", DriveID: driveID, Path: "doc.txt", CreatedETag: `"p,1"`,
	}))
	// The upload committed: baseline and remote_state hold the content.
	_, err := mgr.rawDB().ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('item', 'doc.txt', 'file', 'h', 'h', 7, 7, '"p,2"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "item", Path: "doc.txt", ItemType: ItemTypeFile, Hash: "h", Size: 7, ETag: `"p,2"`,
	}}, "t1", driveID))
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "item", DriveID: driveID, Path: "doc.txt", CreatedETag: `"p,1"`,
	}))

	// A delta page captured mid-upload, applied after the commit.
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "item", Path: "doc.txt", ItemType: ItemTypeFile, Hash: emptyQuickXor, Size: 0, ETag: `"p,1"`,
	}}, "t2", driveID))

	row := readRemoteStateRow(t, mgr.rawDB(), "item")
	require.NotNil(t, row)
	assert.Equal(t, int64(7), row.Size, "the stale empty state must not overwrite the committed content")
	assert.Equal(t, `"p,2"`, row.ETag)
}

// Validates: R-5.6.15
func TestCommitObservation_PendingCreateIsDroppedOnceTheItemChanges(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "item", DriveID: driveID, Path: "doc.txt", CreatedETag: `"p,1"`,
	}))
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "item", Path: "doc.txt", ItemType: ItemTypeFile, Hash: "h", Size: 7, ETag: `"p,2"`,
	}}, "t1", driveID))

	_, found, err := mgr.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.False(t, found)
}

// Validates: R-5.6.15
func TestRunOnce_InterruptedCreateFinishesIntoItsOwnPlaceholder(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("my words\n"), 0o600))

	// The process dies after the empty item exists, before any content.
	remote.mu.Lock()
	remote.interruptCreates = true
	remote.mu.Unlock()
	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"doc.txt": ""}, remote.tree(), "the placeholder is a real, empty OneDrive file")

	remote.mu.Lock()
	remote.interruptCreates = false
	remote.mu.Unlock()
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)

	// Before: a create_conflict. The user's file became a conflict copy and
	// the empty placeholder was downloaded as doc.txt.
	assert.Equal(t, map[string]string{"doc.txt": "my words\n"}, remote.tree())
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "doc.txt"))
	require.NoError(t, err)
	assert.Equal(t, "my words\n", string(data))
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), "conflict", "no conflict copy")
	}
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-5.6.15
func TestRunOnce_PlaceholderOfACreateWhoseFileWasDeletedIsRemoved(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	// A file both sides keep, so deleting doc.txt does not empty the local
	// side, which the delete-safety hold would hold for confirmation.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	local := filepath.Join(syncRoot, "doc.txt")
	require.NoError(t, os.WriteFile(local, []byte("draft\n"), 0o600))
	remote.mu.Lock()
	remote.interruptCreates = true
	remote.mu.Unlock()
	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	remote.mu.Lock()
	remote.interruptCreates = false
	remote.mu.Unlock()
	expireRetryWork(t, eng)
	require.NoError(t, os.Remove(local))
	runScenarioPass(t, eng)

	assert.Equal(t, map[string]string{"keep.txt": "keep\n"}, remote.tree(), "the empty placeholder is deleted, not downloaded")
	assert.NoFileExists(t, local)
}

// Validates: R-5.6.14
func TestRunOnce_UploadOnlyCreateOntoADifferentlySpelledRemoteNameIsADurableCollision(t *testing.T) {
	t.Parallel()

	// Upload-only never downloads straße.txt, so the scanner cannot see the
	// collision; OneDrive's 409 is the first evidence.
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "remote-strasse", "root", "straße.txt", "theirs\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "STRASSE.txt"), []byte("mine\n"), 0o600))

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Failed)
	joined := ""
	for _, e := range report.Errors {
		joined += e.Error() + "\n"
	}
	assert.True(t, strings.Contains(joined, "straße.txt") && strings.Contains(joined, "STRASSE.txt"),
		"the failure names both the local and the remote name: %s", joined)

	uploads := remote.uploads
	_, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, uploads, remote.uploads, "held with backoff, not retried as is on the next pass")
	assert.Equal(t, "theirs\n", remote.tree()["straße.txt"])
}

// expireRetryWork models the retry backoff of a failed action running out.
func expireRetryWork(t *testing.T, eng *testEngine) {
	t.Helper()

	_, err := eng.baseline.rawDB().ExecContext(t.Context(), `UPDATE retry_work SET next_retry_at = 1`)
	require.NoError(t, err)
}

// Validates: R-2.12.1, R-2.12.2
//
// A folder create onto a name OneDrive holds spelled differently is the same
// durable collision as an upload's: the user renamed nothing, so retrying it
// is pointless. It was a superseded precondition instead, so every pass
// planned and refused it again (model test seed 95: OneDrive re-created
// docs as Docs while the user kept editing inside docs).
func TestRunOnce_UploadOnlyFolderCreateOntoADifferentlySpelledRemoteNameIsADurableCollision(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("remote-docs", "root", "Docs")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "docs"), 0o750))

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Failed)
	joined := ""
	for _, e := range report.Errors {
		joined += e.Error() + "\n"
	}
	assert.True(t, strings.Contains(joined, `"Docs"`) && strings.Contains(joined, `"docs"`),
		"the failure names both the local and the remote name: %s", joined)

	creates := len(remote.folderCreates)
	report, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, creates, len(remote.folderCreates), "held with backoff, not retried as is on the next pass")
	assert.Zero(t, report.Failed+report.Succeeded, "nothing ran while the collision stands: %+v", report)
}
