package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
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
//
// A folder replaced by a file: the folder's delete went through on OneDrive
// (its answer lost) and the file's create stopped after the empty item. The
// window that reports both still finds the folder's baseline row at the path,
// which only says what was synced, not what OneDrive holds. Treated as the
// path's occupant, it kept the placeholder from being adopted: the user's
// file became a conflict copy and the empty item was downloaded in its place
// (found by the chaos Graph layer, D-092).
func TestCommitObservation_PlaceholderReplacesTheBaselineOfAnItemOneDriveDeleted(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "folder", ParentID: "root", Path: "doc", ItemType: ItemTypeFolder, ETag: `"f,1"`,
	}}, "t1", driveID))
	_, err := mgr.rawDB().ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, etag) VALUES ('folder', 'doc', 'root', 'folder', '"f,1"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		{
			DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc", ItemType: ItemTypeFile,
			Hash: emptyQuickXor, Size: 0, ETag: `"p,1"`,
		},
		{DriveID: driveID, ItemID: "folder", ParentID: "root", Path: "doc", ItemType: ItemTypeFolder, IsDeleted: true},
	}, "t2", driveID))

	path, localSize, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found, "the placeholder is adopted")
	assert.Equal(t, "doc", path)
	assert.Zero(t, localSize)
	_, _, folderFound := readBaselineRow(t, mgr, "folder")
	assert.False(t, folderFound, "the deleted folder no longer holds the path")
}

// Validates: R-5.6.15
//
// A file renamed online while edited here: the edit is created as a new file
// at the old name, and the create stops after the empty item. The renamed
// item's baseline row still holds the old name, which OneDrive had freed;
// the placeholder is adopted there, and the renamed item stays in
// remote_state at its new name (chaos Graph layer, D-092, seed 15).
func TestCommitObservation_PlaceholderReplacesTheBaselineOfAnItemMovedAway(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "moved", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: "h", Size: 3, ETag: `"m,1"`,
	}}, "t1", driveID))
	_, err := mgr.rawDB().ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('moved', 'doc.txt', 'root', 'file', 'h', 'h', 3, 3, '"m,1"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{
		{
			DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
			Hash: emptyQuickXor, Size: 0, ETag: `"p,1"`,
		},
		{
			DriveID: driveID, ItemID: "moved", ParentID: "root", Path: "renamed.txt", ItemType: ItemTypeFile,
			Hash: "h", Size: 3, ETag: `"m,2"`,
		},
	}, "t2", driveID))

	path, _, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found, "the placeholder is adopted")
	assert.Equal(t, "doc.txt", path)
	_, _, movedFound := readBaselineRow(t, mgr, "moved")
	assert.False(t, movedFound, "the moved item no longer holds the old name")
}

// Validates: R-5.6.15
//
// A baseline row whose item OneDrive still holds keeps the path: the
// placeholder is not adopted over it.
func TestCommitObservation_PlaceholderDoesNotDisplaceALiveBaselineItem(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "live", ParentID: "root", Path: "doc", ItemType: ItemTypeFile,
		Hash: "h", Size: 3, ETag: `"l,1"`,
	}}, "t1", driveID))
	_, err := mgr.rawDB().ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('live', 'doc', 'root', 'file', 'h', 'h', 3, 3, '"l,1"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc", ParentID: "root", CreatedETag: `"p,1"`,
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc", ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: `"p,1"`,
	}}, "t2", driveID))

	_, _, found := readBaselineRow(t, mgr, "placeholder")
	assert.False(t, found)
	path, _, liveFound := readBaselineRow(t, mgr, "live")
	require.True(t, liveFound)
	assert.Equal(t, "doc", path)
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
//
// Opening and then canceling an upload session on the empty item moves its
// eTag although no content landed (live, e2e
// TestE2E_SyncWatch_FileModification). Matching on the creation eTag, sync
// stopped recognizing its own placeholder: observation dropped the record
// and the item read as someone else's file.
func TestCommitObservation_PlaceholderWhoseETagMovedWithoutContentIsStillAdopted(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: `"p,2"`,
	}}, "t1", driveID))

	path, localSize, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found, "still our empty placeholder: adopted")
	assert.Equal(t, "doc.txt", path)
	assert.Zero(t, localSize)
}

// Validates: R-5.6.15
//
// The live failure end to end, in upload-only mode: the create stopped after
// the empty item, whose eTag then moved without content. Every retry got 409
// "name already exists" and the next plan deferred the item as a download
// that upload-only cannot do, so the user's file never reached OneDrive.
func TestRunOnce_UploadOnlyCreateFinishesIntoItsPlaceholderAfterItsETagMoved(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("version 2\n"), 0o600))
	remote.mu.Lock()
	remote.interruptCreates = true
	remote.churnInterruptedCreates = true
	remote.mu.Unlock()
	_, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"doc.txt": ""}, remote.tree())
	placeholder, ok := remote.itemAt("doc.txt")
	require.True(t, ok)

	remote.mu.Lock()
	remote.interruptCreates = false
	remote.mu.Unlock()
	expireRetryWork(t, eng)
	_, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"doc.txt": "version 2\n"}, remote.tree())
	finished, ok := remote.itemAt("doc.txt")
	require.True(t, ok)
	assert.Equal(t, placeholder.ID, finished.ID, "the content went into our own placeholder")
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
	clock := newManualClock(time.Now())
	installManualClockForTestEngine(eng, clock)
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
	clock.Advance(durableRecheckDelay / 2)
	report, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, uploads, remote.uploads, "held with backoff, not retried as is on the next pass")
	assert.Zero(t, report.Failed+report.Succeeded, "nothing ran while the collision stands: %+v", report)
	assert.Equal(t, "theirs\n", remote.tree()["straße.txt"])
}

// scenarioPathLimitRefusal is OneDrive's 400 for a path it counts as longer
// than its 400-character limit (live, 2026-11-02).
const scenarioPathLimitRefusal = "The specified file or folder name is too long. " +
	"The URL path for all files and folders must be 400 characters or less."

// Validates: R-2.10.71, R-2.11.7, R-5.6.15
//
// OneDrive can refuse a create with 400 invalidRequest, which live was
// retried for hours (2026-11-02). Refused through the scenario fake, with or
// without the path-limit message, the create made nothing: its intent is
// cleared, since a definite refusal says nothing was created; the work waits
// on the durable curve; and only the path-limit message names a condition.
func TestRunOnce_RefusedCreateWaitsOnTheDurableCurveAndClearsItsIntent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		message   string
		condition IssueType
	}{
		{name: "path limit", message: scenarioPathLimitRefusal, condition: issuePathTooLong},
		{name: "bare invalid request", message: "Invalid request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			var refusals atomic.Int32
			remote.refuseCreate = func(string, string) error {
				refusals.Add(1)
				return &graph.GraphError{
					StatusCode: http.StatusBadRequest, Code: "invalidRequest", Message: tc.message, Err: graph.ErrBadRequest,
				}
			}
			eng, syncRoot := newTestEngine(t, remote.client(t))
			clock := newManualClock(time.Date(2026, 11, 2, 22, 23, 0, 0, time.UTC))
			installManualClockForTestEngine(eng, clock)
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("my words\n"), 0o600))

			report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			require.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
			assert.Equal(t, int32(1), refusals.Load())
			assert.Empty(t, remote.tree(), "a refused create makes nothing")
			_, found, err := eng.baseline.PendingCreateAt(t.Context(), "doc.txt")
			require.NoError(t, err)
			assert.False(t, found, "a definite refusal clears the intent")

			rows, err := eng.baseline.ListRetryWork(t.Context())
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, ActionUpload, rows[0].ActionType)
			assert.Equal(t, tc.condition, rows[0].ConditionType)
			assert.GreaterOrEqual(t, rows[0].NextRetryAt-clock.Now().UnixNano(), int64(durableRecheckDelay),
				"a refusal waits on the durable curve")

			clock.Advance(durableRecheckDelay / 2)
			report, err = eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			assert.Zero(t, report.Failed+report.Succeeded, "held with backoff: %+v", report)
			assert.Equal(t, int32(1), refusals.Load(), "not retried before the row is due")
		})
	}
}

// Validates: R-2.1.7, R-2.8.7, R-5.6.14, R-5.6.15
//
// Graph's children route can lag behind an item (LI-20260405-09). In
// upload-only mode OneDrive's 409 is the first evidence of a differently
// spelled remote name, and the folder listing names it. When the listing does
// not show the name yet, the create comes back as a stale precondition: it is
// superseded and replanned in the same run, although its error still carries
// the 409, and the replanned create finds the collision and reports it naming
// both. The stale attempt leaves no retry work of its own, and the other
// device's file is never written to. Classified by its 409 first, the attempt
// was recorded as anonymous actionable retry work and waited on the durable
// curve before the collision was found.
func TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "remote-strasse", "root", "straße.txt", "theirs\n")
	remote.invisibleFor = map[string]int{"straße.txt": 1}
	var creates atomic.Int32
	remote.refuseCreate = func(string, string) error {
		creates.Add(1)
		return nil // counts the create, refuses nothing
	}
	eng, syncRoot := newTestEngine(t, remote.client(t))
	installManualClockForTestEngine(eng, newManualClock(time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "STRASSE.txt"), []byte("mine\n"), 0o600))

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	remote.mu.Lock()
	lagLeft := remote.invisibleFor["straße.txt"]
	remote.mu.Unlock()
	assert.Zero(t, lagLeft, "the 409's listing met the lag")
	assert.Equal(t, int32(2), creates.Load(), "the stale create was replanned in the same run")
	assert.Equal(t, 1, report.Superseded, "the stale 409 is superseded, not a failure")
	require.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	joined := ""
	for _, e := range report.Errors {
		joined += e.Error() + "\n"
	}
	assert.True(t, strings.Contains(joined, "straße.txt") && strings.Contains(joined, "STRASSE.txt"),
		"once listed, the collision names both: %s", joined)
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, issueCaseCollision, rows[0].ConditionType)
	assert.Equal(t, 1, rows[0].AttemptCount, "the stale attempt recorded no retry work of its own")
	assert.Equal(t, map[string]string{"straße.txt": "theirs\n"}, remote.tree())
}

// scenarioLargeFileSize is just above driveops' 4 MiB threshold for a
// recorded upload session.
const scenarioLargeFileSize = 4<<20 + 1

// Validates: R-5.2.1, R-5.2.4, R-5.6.15
//
// The large-file route, through the scenario fake's sessions: the session
// request of a new file above 4 MiB fails after its create, and the empty
// item stays on OneDrive with its eTag moved (the cleanup of 2026-11-10 was
// refused, LI-20261110-01). The item the failure names is recorded, so the
// next pass adopts it as this client's placeholder and sends the content into
// it by ID through a session on that item: no conflict copy, no download of
// the empty file.
func TestRunOnce_LargeFileWhoseSessionRequestFailedFinishesIntoItsPlaceholder(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newScenarioSessionEngine(t, remote.sessionClient(t))
	runScenarioPass(t, eng)

	content := strings.Repeat("x", scenarioLargeFileSize)
	local := filepath.Join(syncRoot, "big.bin")
	require.NoError(t, os.WriteFile(local, []byte(content), 0o600))
	remote.mu.Lock()
	remote.interruptCreates = true
	remote.churnInterruptedCreates = true
	remote.mu.Unlock()
	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0].Error(), "creating upload session", "the create took the session route")

	placeholder, ok := remote.itemAt("big.bin")
	require.True(t, ok, "the empty item stayed")
	require.Zero(t, placeholder.Size)
	pending, found, err := eng.baseline.PendingCreateAt(t.Context(), "big.bin")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, placeholder.ID, pending.ItemID, "the item the failure named is recorded")

	remote.mu.Lock()
	remote.interruptCreates = false
	remote.mu.Unlock()
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)

	finished, ok := remote.itemAt("big.bin")
	require.True(t, ok)
	assert.Equal(t, placeholder.ID, finished.ID, "the content went into our own placeholder")
	assert.Equal(t, int64(scenarioLargeFileSize), finished.Size)
	assert.Equal(t, hashContentQuickXor(t, content), finished.QuickXorHash)
	remote.mu.Lock()
	sessionsOpened, sessionsOpen := remote.sessionSeq, len(remote.sessions)
	remote.mu.Unlock()
	assert.Equal(t, 1, sessionsOpened, "the content went through a session on the item")
	assert.Zero(t, sessionsOpen)
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), "conflict", "no conflict copy")
	}
	info, err := os.Stat(local)
	require.NoError(t, err)
	assert.Equal(t, int64(scenarioLargeFileSize), info.Size(), "the local file is untouched")
	requireScenarioQuietPlan(t, eng)
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
	clock := newManualClock(time.Now())
	installManualClockForTestEngine(eng, clock)
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
	// A pass a few seconds later, however long the first one took (over a
	// second under -race, which outlasted the transient retry curve's first
	// step and retried the collision).
	clock.Advance(durableRecheckDelay / 2)
	report, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Len(t, remote.folderCreates, creates, "held with backoff, not retried as is on the next pass")
	assert.Zero(t, report.Failed+report.Succeeded, "nothing ran while the collision stands: %+v", report)
}

// Validates: R-5.6.15
//
// The creating request was in flight when the process died: OneDrive made
// the empty item after the client was gone, so no hook recorded it (live,
// 2026-11-01). Only the intent, written before the request, knows the empty
// file at that path is ours; observation claims it and adopts it.
func TestCommitObservation_IntentClaimsTheEmptyFileACrashLeftUnrecorded(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: driveID, Path: "doc.txt", ParentID: "root", LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "orphan", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: `"o,1"`,
	}}, "t1", driveID))

	path, localSize, found := readBaselineRow(t, mgr, "orphan")
	require.True(t, found, "the unrecorded placeholder is adopted as synced empty content")
	assert.Equal(t, "doc.txt", path)
	assert.Zero(t, localSize)
	pending, found, err := mgr.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "orphan", pending.ItemID, "the record now names the item")
	assert.Equal(t, `"o,1"`, pending.CreatedETag)
}

// Validates: R-5.6.15
//
// An intent claims only an empty file, and only while fresh: a file with
// content at the path is someone's real file, and an intent older than the
// TTL could meet a stranger's empty file long after the attempt. Both drop
// the intent.
func TestCommitObservation_IntentDoesNotClaimContentOrOutliveItsTTL(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)

	withContent := newTestStore(t)
	require.NoError(t, withContent.RecordPendingCreateIntent(t.Context(), &PendingCreate{DriveID: driveID, Path: "doc.txt", ParentID: "root"}))
	require.NoError(t, withContent.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "theirs", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: "their-hash", Size: 9, ETag: `"t,1"`,
	}}, "t1", driveID))
	_, _, found := readBaselineRow(t, withContent, "theirs")
	assert.False(t, found, "a file with content is not adopted")
	_, found, err := withContent.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.False(t, found, "the intent is dropped: the path holds real content")

	expired := newTestStore(t)
	base := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	expired.setNowFunc(func() time.Time { return base })
	require.NoError(t, expired.RecordPendingCreateIntent(t.Context(), &PendingCreate{DriveID: driveID, Path: "doc.txt", ParentID: "root"}))
	expired.setNowFunc(func() time.Time { return base.Add(pendingCreateIntentTTL + time.Minute) })
	require.NoError(t, expired.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "late", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: `"l,1"`,
	}}, "t1", driveID))
	_, _, found = readBaselineRow(t, expired, "late")
	assert.False(t, found, "an expired intent claims nothing")
	_, found, err = expired.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.False(t, found, "and is dropped")
}

// Validates: R-5.6.15
//
// A zero-byte file is complete as created, so its create hook never runs and
// only the intent names the path. When that intent outlives the create (the
// create succeeded as a shutdown canceled the attempt, which keeps a canceled
// attempt's intent, or the clear failed), the next observation of the file
// claimed it as a placeholder although the baseline already held it as
// synced: the record then named an empty file that stays empty, and nothing
// ever dropped it. A file the baseline holds is not a lost create's, and the
// intent is dropped instead.
func TestCommitObservation_IntentDoesNotClaimAFileTheBaselineHolds(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &PendingCreate{DriveID: driveID, Path: "empty.txt", ParentID: "root"}))
	_, err := mgr.rawDB().ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('created', 'empty.txt', 'root', 'file', ?, ?, 0, 0, '"c,1"')`, emptyQuickXor, emptyQuickXor)
	require.NoError(t, err)

	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "created", ParentID: "root", Path: "empty.txt", ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: `"c,1"`,
	}}, "t1", driveID))

	_, found, err := mgr.PendingCreateAt(t.Context(), "empty.txt")
	require.NoError(t, err)
	assert.False(t, found, "the intent of a create that completed is dropped, not turned into a placeholder record")
	path, _, synced := readBaselineRow(t, mgr, "created")
	require.True(t, synced)
	assert.Equal(t, "empty.txt", path)
}

// createIntentPruneCase is one intent and what the pass that prunes it saw.
type createIntentPruneCase struct {
	name          string
	path          string
	localFile     bool          // the local scan found a file at path
	readDenied    string        // the scan could not read this folder
	remoteDenied  string        // OneDrive refused to list this folder
	suspect       bool          // local truth was marked suspect after the scan
	unscanned     bool          // the content filter or a shortcut root keeps path out of the scan
	localOnlyPass bool          // a watch replan: local truth refreshed, OneDrive not observed
	namesItem     bool          // the row records the item the create made
	prunedAfter   time.Duration // since the intent was recorded
	wantKept      bool
	why           string
}

// Validates: R-5.6.15
//
// An intent without an item that no create will ever retry stayed for good:
// the create ended without an answer, the user renamed the file away, and no
// item appeared at the path (model test, chaos seeds 9 and 11). While fresh
// it could still claim an empty file a stranger made there, and with the
// local file gone the next pass deleted that file. Pruned after planning, such
// an intent is dropped once a pass that observed OneDrive and then a complete
// local scan finds nothing at its path, and any intent past its TTL is dropped
// whatever the pass saw. A path the scan could not see or OneDrive would not
// list, local truth marked suspect, a pass that did not observe OneDrive
// (whose next change batch may still show the create's empty file), and a row
// that names an item all keep the row.
func TestPruneAbandonedCreateIntents_DropsWhatNoCreateWillClaim(t *testing.T) {
	t.Parallel()

	const deniedFolder = "Private"
	fresh, expired := time.Minute, pendingCreateIntentTTL+time.Minute
	for _, tc := range []createIntentPruneCase{
		{
			name: "no file at its path", path: "doc.txt", prunedAfter: fresh,
			why: "nothing at the path will retry the create",
		},
		{
			name: "past its TTL with its file still there and local truth suspect", path: "doc.txt",
			localFile: true, suspect: true, prunedAfter: expired, why: "an expired intent claims nothing",
		},
		{
			name: "past its TTL in a pass that did not observe OneDrive", path: "doc.txt",
			localOnlyPass: true, prunedAfter: expired, why: "expiry needs no observation",
		},
		{
			name: "its file still there", path: "doc.txt", localFile: true, prunedAfter: fresh,
			wantKept: true, why: "the create of that file is retried",
		},
		{
			name: "below a folder the scan could not read", path: deniedFolder + "/doc.txt", readDenied: deniedFolder,
			prunedAfter: fresh, wantKept: true, why: "unreadable is not absent",
		},
		{
			name: "below a folder OneDrive would not list", path: deniedFolder + "/doc.txt", remoteDenied: deniedFolder,
			prunedAfter: fresh, wantKept: true, why: "an unobservable remote path may hold the create's empty file",
		},
		{
			name: "a pass that did not observe OneDrive", path: "doc.txt", localOnlyPass: true, prunedAfter: fresh,
			wantKept: true, why: "the change batch that shows a landed create claims it",
		},
		{
			name: "local truth suspect", path: "doc.txt", suspect: true, prunedAfter: fresh,
			wantKept: true, why: "a suspect scan proves no absence",
		},
		{
			name: "a path the scan does not cover", path: "doc.txt", unscanned: true, prunedAfter: fresh,
			wantKept: true, why: "a path the scan never visits proves no absence",
		},
		{
			name: "a row that names its item", path: "doc.txt", namesItem: true, prunedAfter: expired,
			wantKept: true, why: "a recorded item is dropped by observation only",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runCreateIntentPruneCase(t, &tc)
		})
	}
}

// runCreateIntentPruneCase records tc's intent, the local and remote truth
// its pass saw, prunes, and checks whether the row is still there.
func runCreateIntentPruneCase(t *testing.T, tc *createIntentPruneCase) {
	t.Helper()

	intendedAt := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	store.setNowFunc(func() time.Time { return intendedAt })
	pending := &PendingCreate{DriveID: driveID, Path: tc.path, ParentID: "root"}
	require.NoError(t, store.RecordPendingCreateIntent(ctx, pending))
	if tc.namesItem {
		pending.ItemID, pending.CreatedETag = "placeholder", `"p,1"`
		require.NoError(t, store.RecordPendingCreate(ctx, pending))
	}
	scanned := []localStateRow{{Path: "keep.txt", ItemType: ItemTypeFile, Hash: "keep-hash", Size: 5}}
	if tc.localFile {
		scanned = append(scanned, localStateRow{Path: tc.path, ItemType: ItemTypeFile, Hash: "my-hash", Size: 9})
	}
	require.NoError(t, store.ReplaceLocalState(ctx, scanned))
	if tc.readDenied != "" {
		seedObservationIssueRowForTest(t, store, &ObservationIssue{
			Path: tc.readDenied, DriveID: driveID, IssueType: issueLocalReadDenied, ScopeKey: sKPermLocalRead(tc.readDenied),
		})
	}
	if tc.remoteDenied != "" {
		seedObservationIssueRowForTest(t, store, &ObservationIssue{
			Path: tc.remoteDenied, DriveID: driveID, IssueType: issueRemoteReadDenied, ScopeKey: sKPermRemoteRead(tc.remoteDenied),
		})
	}
	if tc.suspect {
		require.NoError(t, store.MarkLocalTruthSuspect(ctx, "watch overflow"))
	}
	store.setNowFunc(func() time.Time { return intendedAt.Add(tc.prunedAfter) })
	observed := passObservedRemoteThenLocal
	if tc.localOnlyPass {
		observed = passRefreshedLocalOnly
	}

	require.NoError(t, store.pruneAbandonedCreateIntents(ctx, observed, func(path string) bool {
		return !tc.unscanned || path != tc.path
	}))

	kept, found, err := store.PendingCreateAt(ctx, tc.path)
	require.NoError(t, err)
	require.Equal(t, tc.wantKept, found, tc.why)
	if tc.namesItem {
		assert.Equal(t, "placeholder", kept.ItemID)
	}
}

// Validates: R-5.6.15
//
// An attempt that ends without creating anything (the create itself failed)
// clears its intent, and an intent at the path of a placeholder already
// recorded keeps the placeholder's ID.
func TestPendingCreateIntent_ClearedWhenNothingWasCreatedAndKeepsAKnownItem(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &PendingCreate{DriveID: driveID, Path: "a.txt", ParentID: "root"}))
	require.NoError(t, mgr.ClearPendingCreateIntent(t.Context(), "a.txt"))
	_, found, err := mgr.PendingCreateAt(t.Context(), "a.txt")
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "b.txt", ParentID: "root", CreatedETag: `"p,1"`,
	}))
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &PendingCreate{DriveID: driveID, Path: "b.txt", ParentID: "root"}))
	pending, found, err := mgr.PendingCreateAt(t.Context(), "b.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "placeholder", pending.ItemID, "a retry's intent does not forget the placeholder it will upload into")
	require.NoError(t, mgr.ClearPendingCreateIntent(t.Context(), "b.txt"))
	_, found, err = mgr.PendingCreateAt(t.Context(), "b.txt")
	require.NoError(t, err)
	assert.True(t, found, "clearing an intent never drops a recorded item")
}

// creatingTestUploader models graph.Client's two-step create for executor
// tests: create runs where the empty item is made, with the record hook the
// client calls between the item and its content.
type creatingTestUploader struct {
	executorMockUploader
	create func(ctx context.Context, onCreated graph.CreatedFunc) (*graph.Item, error)
}

func (u *creatingTestUploader) UploadCreating(
	ctx context.Context, _ driveid.ID, _, _ string,
	_ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc, onCreated graph.CreatedFunc,
) (*graph.Item, error) {
	return u.create(ctx, onCreated)
}

// newPendingCreateTestExecutor returns an executor that uploads through ul and
// records its pending creates in a real store, plus that store and the sync
// root.
func newPendingCreateTestExecutor(t *testing.T, ul driveops.Uploader, logger *slog.Logger) (*executor, *SyncStore, string) {
	t.Helper()

	syncRoot := t.TempDir()
	syncTree, err := synctree.Open(syncRoot)
	require.NoError(t, err)
	dl := &executorMockDownloader{}
	cfg := newExecutorConfig(&executorMockItemClient{}, dl, ul, syncTree, driveid.New(testDriveID), logger, nil)
	cfg.SetTransferMgr(driveops.NewTransferManager(dl, ul, nil, logger))
	store := newTestStore(t)
	cfg.SetPendingCreates(store)

	return newExecution(cfg, emptyBaseline()), store, syncRoot
}

// Validates: R-5.6.15
//
// The item exists on OneDrive from the moment the create answers, whatever
// happens to the action after. A shutdown landing between the answer and the
// record (live, 2026-11-10, e2e TestE2E_SyncWatch_PauseResume) failed the
// write on the canceled action context, so only the intent was left.
func TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled(t *testing.T) {
	t.Parallel()

	actionCtx, cancelAction := context.WithCancel(t.Context())
	defer cancelAction()
	created := &graph.Item{ID: "new-item", ETag: `"c,1"`}
	ul := &creatingTestUploader{create: func(uploadCtx context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
		cancelAction() // the shutdown, right after the create answered
		if hookErr := onCreated(created); hookErr != nil {
			return nil, &graph.CreatedItemError{Item: created, Err: hookErr}
		}
		return nil, &graph.CreatedItemError{Item: created, Err: uploadCtx.Err()}
	}}
	_, logger := newDebugLogBuffer()
	e, store, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
	local := writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")

	_, uploadErr := e.uploadNewFile(actionCtx, &Action{Type: ActionUpload, Path: "doc.txt"},
		driveid.New(testDriveID), graphRootID, "doc.txt", local)
	require.ErrorIs(t, uploadErr, context.Canceled)

	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "new-item", pending.ItemID, "the item is recorded, not only the intent")
	assert.Equal(t, `"c,1"`, pending.CreatedETag)
}

// Validates: R-5.6.15, R-6.6.21
//
// A create that a shutdown canceled before it answered can still land on
// OneDrive after the client is gone, which is what the intent is for.
// Clearing it on the canceled context failed and warned at a clean stop; the
// intent is kept instead, quietly, and expires on its own.
func TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly(t *testing.T) {
	t.Parallel()

	actionCtx, cancelAction := context.WithCancel(t.Context())
	defer cancelAction()
	ul := &creatingTestUploader{create: func(uploadCtx context.Context, _ graph.CreatedFunc) (*graph.Item, error) {
		cancelAction() // the shutdown, while the create is in flight
		return nil, fmt.Errorf("graph: creating file: %w", uploadCtx.Err())
	}}
	logs, logger := newDebugLogBuffer()
	e, store, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
	local := writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")

	_, uploadErr := e.uploadNewFile(actionCtx, &Action{Type: ActionUpload, Path: "doc.txt"},
		driveid.New(testDriveID), graphRootID, "doc.txt", local)
	require.ErrorIs(t, uploadErr, context.Canceled)
	assert.NotContains(t, logs.String(), "level=WARN", "a clean shutdown warns about nothing")

	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "the intent stays for a create that may still land")
	assert.Empty(t, pending.ItemID)
}

// Validates: R-5.6.15
//
// On a drive that gives empty files no hash (zero-byte files on Business),
// the eTag and modification time are all the comparison has for an adopted
// placeholder. Both move without content when an upload session is opened
// and canceled on the item, and with the local file changed that read as an
// edit on both sides: the user's file renamed to a conflict copy and the
// empty file downloaded in its place. The adopted row follows them while the
// item stays empty, so the plan is still the upload into it by ID.
func TestCommitObservation_AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))
	createdAt := time.Date(2026, 11, 10, 18, 0, 48, 0, time.UTC)
	placeholder := observedItem{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Size: 0, ETag: `"p,1"`, Mtime: createdAt.UnixNano(),
	}
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{placeholder}, "t1", driveID))
	_, err := mgr.rawDB().ExecContext(t.Context(), `INSERT INTO local_state (path, item_type, hash, size, mtime)
		VALUES ('doc.txt', 'file', 'local-hash', 7, 7)`)
	require.NoError(t, err)
	// Cached now, so the check at the end reads the in-memory copy.
	_, err = mgr.Load(t.Context())
	require.NoError(t, err)

	moved := placeholder
	moved.ETag = `"p,2"`
	touched := moved
	touched.ETag, touched.Mtime = `"p,3"`, createdAt.Add(time.Second).UnixNano()
	for i, observed := range []observedItem{moved, touched} {
		require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{observed}, fmt.Sprintf("t%d", i+2), driveID))
		rows, queryErr := mgr.QueryReconciliationState(t.Context())
		require.NoError(t, queryErr)
		assert.Equal(t, "upload", reconciliationKindsByPath(rows)["doc.txt"], "observation %d is no remote edit", i)
	}

	bl, err := mgr.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByID("placeholder")
	require.True(t, ok)
	assert.Equal(t, `"p,3"`, entry.ETag, "the in-memory baseline follows the row")
	assert.Zero(t, entry.RemoteSize)
}

// Validates: R-5.6.15, R-2.4.16
//
// Following an adopted placeholder's eTag says "still empty at this eTag",
// and the next upload into it is conditional on exactly that eTag. An
// observation whose size Graph did not report cannot say that: the item may
// hold someone's content by now, so its row keeps the eTag it was adopted
// with and the moved eTag stays visible as a remote change.
func TestCommitObservation_AdoptedPlaceholderOfUnknownSizeIsNotFollowed(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
	}))
	placeholder := observedItem{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: ItemTypeFile,
		Size: 0, ETag: `"p,1"`,
	}
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{placeholder}, "t1", driveID))

	sizeless := placeholder
	sizeless.ETag, sizeless.SizeUnknown = `"p,moved"`, true
	require.NoError(t, mgr.CommitObservation(t.Context(), []observedItem{sizeless}, "t2", driveID))

	bl, err := mgr.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByID("placeholder")
	require.True(t, ok)
	assert.Equal(t, `"p,1"`, entry.ETag, "an item not known to be empty keeps the eTag it was adopted with")
}
