package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-5.8.1, R-5.8.2
func TestRunOnce_AcceptedServerTransformIsNotDownloadedAgain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileName string
		reported string // the content OneDrive's size and hash describe
		served   string // the bytes it actually serves
	}{
		// iOS HEIC: metadata describes the original, OneDrive serves a
		// smaller post-processed file that is still a HEIF container
		// (an ISO BMFF `ftyp` box); anything else is an integrity failure.
		{name: "HEIC served smaller", fileName: "IMG_0001.HEIC", reported: "original heic bytes, much larger than the served file", served: "\x00\x00\x00\x18ftypheicprocessed"},
		// Deterministic same-size transform (AIP-protected Office files).
		{name: "same bytes at the reported size", fileName: "plan.docx", reported: "unprotected", served: "protected!!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFile(t, "item-1", "root", tt.fileName, tt.reported)
			remote.contents["item-1"] = tt.served

			eng, syncRoot := newTestEngine(t, remote.client(t))
			first := runScenarioPass(t, eng)
			require.Equal(t, 1, first.Downloads)

			got, err := localpath.ReadFile(filepath.Join(syncRoot, tt.fileName))
			require.NoError(t, err)
			assert.Equal(t, tt.served, string(got))

			// The baseline used to record the local hash as the remote one
			// while remote_state kept OneDrive's, so every pass saw a remote
			// change and downloaded the file again (three times each).
			second := runScenarioPass(t, eng)
			assert.Zero(t, second.Downloads+second.Uploads, "an accepted transform is converged")
		})
	}
}

// Validates: R-2.2.1, R-2.2.3, R-5.8.2
//
// After an accepted server transform the baseline holds the served bytes'
// hash on the local side and OneDrive's reported hash on the remote side.
// When the local file then comes to hold exactly the content OneDrive reports,
// local changed against the baseline and OneDrive did not, but the two sides
// are proven equal: nothing transfers. The pass used to upload the file, a new
// version online for bytes OneDrive already holds. The baseline learns the
// new local hash, size, mtime and identity beside the remote facts it had,
// and planning the same state again is a no-op.
func TestRunOnce_LocalEditEqualToUnchangedRemoteUploadsNothing(t *testing.T) {
	t.Parallel()

	const (
		fileName = "plan.docx"
		reported = "unprotected" // the content OneDrive's size and hash describe
		served   = "protected!!" // the same-size bytes it serves
	)
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-1", "root", fileName, reported)
	remote.contents["item-1"] = served

	eng, syncRoot := newTestEngine(t, remote.client(t))
	first := runScenarioPass(t, eng)
	require.Equal(t, 1, first.Downloads)
	ctx := t.Context()
	accepted, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	acceptedEntry, ok := accepted.GetByPath(fileName)
	require.True(t, ok)
	before := *acceptedEntry
	require.Equal(t, hashContentQuickXor(t, served), before.LocalHash, "the setup: the transform is accepted")
	require.Equal(t, hashContentQuickXor(t, reported), before.RemoteHash)

	writeLocalFile(t, syncRoot, fileName, reported)
	second := runScenarioPass(t, eng)
	assert.Zero(t, second.Uploads+second.Downloads, "equal content transfers nothing")
	assert.Zero(t, remote.uploads)

	var observed struct {
		hash                      string
		size, mtime, ctime, birth int64
		device, inode             uint64
	}
	require.NoError(t, eng.baseline.rawDB().QueryRowContext(ctx,
		`SELECT hash, size, mtime, ctime, local_birth_time, local_device, local_inode
		FROM local_state WHERE path = ?`, fileName).Scan(
		&observed.hash, &observed.size, &observed.mtime, &observed.ctime, &observed.birth, &observed.device, &observed.inode))
	require.Equal(t, hashContentQuickXor(t, reported), observed.hash)

	updated, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	after, ok := updated.GetByPath(fileName)
	require.True(t, ok)
	assert.Equal(t, observed.hash, after.LocalHash, "the baseline learns the local hash")
	assert.Equal(t, observed.size, after.LocalSize)
	assert.Equal(t, observed.mtime, after.LocalMtime)
	assert.Equal(t, observed.ctime, after.LocalCtime)
	assert.Equal(t, observed.birth, after.LocalBirthTime)
	assert.Equal(t, observed.device, after.LocalDevice)
	assert.Equal(t, observed.inode, after.LocalInode)
	assert.True(t, after.LocalHasIdentity)
	assert.Equal(t, before.ItemID, after.ItemID, "the remote side is unchanged")
	assert.Equal(t, before.RemoteHash, after.RemoteHash)
	assert.Equal(t, before.RemoteSize, after.RemoteSize)
	assert.Equal(t, before.RemoteMtime, after.RemoteMtime)
	assert.Equal(t, before.ETag, after.ETag)

	assert.Empty(t, planCurrentStateForStore(t, eng.baseline).Actions, "the same state plans nothing, not even a baseline update")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-5.6.12
func TestUploadTransferError_InvalidatedSessionIsAStalePlan(t *testing.T) {
	t.Parallel()

	err := uploadTransferError(fmt.Errorf("uploading a.bin: %w", driveops.ErrUploadSessionInvalidated))

	// Another writer changed the item mid-upload: re-observe and replan,
	// never upload over the change.
	require.ErrorIs(t, err, errActionPreconditionChanged)
	assert.Equal(t, resultSuperseded, classifyResult(&actionCompletion{Err: err}).Class)
}

// Validates: R-6.7.34
func TestExecutor_Download_RecordsObservedLocalMtime(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	// No remote mtime to apply: the file keeps the time it was written.
	o := e.ExecuteDownload(t.Context(), &Action{
		Type:    ActionDownload,
		Path:    "observed.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{}},
	})
	requireOutcomeSuccess(t, &o)

	// The outcome used to record the intended (here: no) time instead of
	// what the filesystem holds, so the next scan saw a local change.
	info, err := os.Stat(filepath.Join(syncRoot, "observed.txt"))
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), o.LocalMtime)
}

// Validates: R-6.8.20
func TestClassifyResult_StalledTransferIsRetryableNotShutdown(t *testing.T) {
	t.Parallel()

	// What the graph boundary returns when its watchdog abandons a body:
	// even if a caller wrapped it with a cancellation, it is retry work.
	stalled := fmt.Errorf("downloading a.bin: %w: %w", graph.ErrTransferStalled, context.Canceled)

	decision := classifyResult(&actionCompletion{Err: stalled})

	// A shutdown classification dropped the action and its dependents
	// without recording anything to retry.
	assert.Equal(t, resultRequeue, decision.Class)
	assert.Equal(t, persistRetryWork, decision.Persistence)
}

// Validates: R-5.5.1
func TestExecutor_Download_KnownEmptyItemRejectsNonEmptyBody(t *testing.T) {
	t.Parallel()

	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("<html>not the empty file</html>"))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())

	// remote_state holds a real size of 0 (not NULL) for this hashless item.
	o := e.ExecuteDownload(t.Context(), &Action{
		Type:    ActionDownload,
		Path:    "empty.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &pathView{Remote: &remoteState{Size: 0, SizeKnown: true, Mtime: 1}},
	})

	// A size of 0 used to read as "unknown", so any body was accepted.
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, driveops.ErrDownloadIntegrity)
	assert.NoFileExists(t, filepath.Join(syncRoot, "empty.txt"))
}
