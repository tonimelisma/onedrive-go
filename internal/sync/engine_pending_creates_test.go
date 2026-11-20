package sync

import (
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
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

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
		{name: "path limit", message: scenarioPathLimitRefusal, condition: IssuePathTooLong},
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
	assert.Equal(t, IssueCaseCollision, rows[0].ConditionType)
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

	_, err := eng.baseline.RawDBForTest().ExecContext(t.Context(), `UPDATE retry_work SET next_retry_at = 1`)
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
