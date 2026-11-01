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
