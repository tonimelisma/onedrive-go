package sync

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-5.5.1, R-5.8.2
func TestExecutor_Download_HashMismatch_FailsWithoutWritingTarget(t *testing.T) {
	t.Parallel()

	callCount := 0
	dl := &syncfixture.ExecutorMockDownloader{
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			callCount++
			// An error page served with status 200, every time.
			n, err := w.Write([]byte(`<html>Service unavailable</html>`))
			return int64(n), err
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, dl, &syncfixture.ExecutorMockUploader{})
	e := syncexec.NewExecution(cfg, syncfixture.EmptyBaseline())

	action := &syncdomain.Action{
		Type:    syncdomain.ActionDownload,
		Path:    "hash-reject.txt",
		ItemID:  "item1",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Remote: &syncdomain.RemoteState{Hash: "remote-hash", Size: 4096, Mtime: 1}},
	}

	o := e.ExecuteDownload(t.Context(), action)

	// It used to be accepted after three attempts and renamed into place,
	// with the local hash recorded as the remote one.
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, driveops.ErrDownloadIntegrity)
	assert.Equal(t, 3, callCount, "1 initial + 2 retries")
	assert.Equal(t, syncdomain.IssueDownloadIntegrity, issueTypeForResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: o.Error})))

	assert.NoFileExists(t, filepath.Join(syncRoot, "hash-reject.txt"), "the error page is never renamed into place")
}

// Validates: R-5.5.2, R-2.5.7
func TestExecuteUpload_ResponseSizeMismatch_DoesNotCommitBaseline(t *testing.T) {
	t.Parallel()

	ul := &syncfixture.ExecutorMockUploader{
		RawResponse: true,
		UploadFn: func(_ context.Context, _ driveid.ID, _, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			// A Personal-drive answer that stored 2 bytes of a 5-byte file
			// and carries no hash: size is the only evidence, and it differs.
			return &graph.Item{ID: "short", Name: name, Size: 2, ParentDriveType: "personal"}, nil
		},
	}

	cfg, syncRoot := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, ul)
	e := syncexec.NewExecution(cfg, syncfixture.EmptyBaseline())
	syncfixture.WriteExecTestFile(t, syncRoot, "short.txt", "hello")

	o := e.ExecuteUpload(t.Context(), &syncdomain.Action{
		Type:    syncdomain.ActionUpload,
		Path:    "short.txt",
		DriveID: driveid.New(synctest.TestDriveID),
		View:    &syncdomain.PathView{Path: "short.txt"},
	})

	// Before: a success outcome recording local=5 bytes and remote=2 bytes
	// as one converged baseline row, never differing from either half again.
	syncfixture.RequireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, driveops.ErrUploadVerificationFailed)
	assert.Equal(t, syncdomain.IssueUploadVerification, issueTypeForResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: o.Error})))
}
