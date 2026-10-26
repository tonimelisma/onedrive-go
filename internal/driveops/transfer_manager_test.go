package driveops

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// ---------------------------------------------------------------------------
// Mock types
// ---------------------------------------------------------------------------

// tmMockDownloader implements Downloader + RangeDownloader.
type tmMockDownloader struct {
	downloadFn      func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
	downloadRangeFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer, offset int64) (int64, error)
}

var (
	_ Downloader      = (*tmMockDownloader)(nil)
	_ RangeDownloader = (*tmMockDownloader)(nil)
)

func (m *tmMockDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

func (m *tmMockDownloader) DownloadRange(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer, offset int64, _ graph.DownloadVersion) (int64, error) {
	if m.downloadRangeFn != nil {
		return m.downloadRangeFn(ctx, driveID, itemID, w, offset)
	}

	return 0, fmt.Errorf("DownloadRange not mocked")
}

// tmMockUploader implements Uploader + SessionUploader.
type tmMockUploader struct {
	uploadFn              func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	createUploadSessionFn func(ctx context.Context, driveID driveid.ID, parentID, name string, size int64, mtime time.Time) (*graph.UploadSession, error)
	uploadFromSessionFn   func(ctx context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, progress graph.ProgressFunc) (*graph.Item, error)
	resumeUploadFn        func(ctx context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, progress graph.ProgressFunc) (*graph.Item, error)
	getItemFn             func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	listChildrenFn        func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	canceledSessions      []graph.UploadURL
}

func (m *tmMockUploader) CancelUploadSession(_ context.Context, session *graph.UploadSession) error {
	m.canceledSessions = append(m.canceledSessions, session.UploadURL)
	return nil
}

var (
	_ Uploader        = (*tmMockUploader)(nil)
	_ SessionUploader = (*tmMockUploader)(nil)
	_ ItemReader      = (*tmMockUploader)(nil)
)

func (m *tmMockUploader) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
}

func (m *tmMockUploader) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.listChildrenFn != nil {
		return m.listChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *tmMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return nil, fmt.Errorf("Upload not mocked")
}

func (m *tmMockUploader) CreateUploadSession(ctx context.Context, driveID driveid.ID, parentID, name string, size int64, mtime time.Time) (*graph.UploadSession, error) {
	if m.createUploadSessionFn != nil {
		return m.createUploadSessionFn(ctx, driveID, parentID, name, size, mtime)
	}

	return nil, fmt.Errorf("CreateUploadSession not mocked")
}

func (m *tmMockUploader) UploadFromSession(ctx context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFromSessionFn != nil {
		return m.uploadFromSessionFn(ctx, session, content, totalSize, progress)
	}

	return nil, fmt.Errorf("UploadFromSession not mocked")
}

func (m *tmMockUploader) ResumeUpload(ctx context.Context, session *graph.UploadSession, content io.ReaderAt, totalSize int64, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.resumeUploadFn != nil {
		return m.resumeUploadFn(ctx, session, content, totalSize, progress)
	}

	return nil, fmt.Errorf("ResumeUpload not mocked")
}

// tmSimpleDownloader implements only Downloader (no RangeDownloader), used to
// test the fresh-download-only path.
type tmSimpleDownloader struct {
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

var _ Downloader = (*tmSimpleDownloader)(nil)

func (m *tmSimpleDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func tmHashBytes(data []byte) string {
	h := quickxorhash.New()
	h.Write(data)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func newTestTM(dl Downloader, ul Uploader, store *SessionStore) *TransferManager {
	return NewTransferManager(dl, ul, store, slog.Default())
}

// newResumeMismatchDownloader models a resume attempt that yields mixed bytes,
// forcing TransferManager to discard the partial and retry from scratch.
func newResumeMismatchDownloader(
	t *testing.T,
	expectedOffset int,
	resumeBytes []byte,
	freshBytes []byte,
	rangeCalls *int,
	freshCalls *int,
) *tmMockDownloader {
	t.Helper()

	return &tmMockDownloader{
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer, offset int64) (int64, error) {
			*rangeCalls++
			assert.Equal(t, int64(expectedOffset), offset)

			written, err := w.Write(resumeBytes[offset:])
			return int64(written), err
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			*freshCalls++
			written, err := w.Write(freshBytes)
			return int64(written), err
		},
	}
}

// ---------------------------------------------------------------------------
// Download tests
// ---------------------------------------------------------------------------

// Validates: R-1.2, R-6.2.3
func TestTransferManager_FreshDownload_Success(t *testing.T) {
	t.Parallel()

	content := []byte("hello world download")
	expectedHash := tmHashBytes(content)

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "sub", "file.txt")

	opts := DownloadOpts{
		RemoteHash: expectedHash,
	}
	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, expectedHash, result.LocalHash)
	assert.Equal(t, int64(len(content)), result.Size)

	// HashVerified should be true on successful hash match.
	assert.True(t, result.HashVerified, "HashVerified should be true on successful match")

	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")

	assert.Equal(t, content, got, "file content mismatch")

	// .partial should not exist after successful download.
	_, statErr := os.Stat(DownloadPartialPath(targetPath, opts))
	assert.True(t, os.IsNotExist(statErr), "expected .partial to be removed")
}

func TestTransferManager_FreshDownload_Error(t *testing.T) {
	t.Parallel()

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, fmt.Errorf("network failure")
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "file.txt")

	opts := DownloadOpts{}
	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "network failure")

	// .partial should be cleaned up on non-ctx error.
	_, statErr := os.Stat(DownloadPartialPath(targetPath, opts))
	assert.True(t, os.IsNotExist(statErr), "expected .partial to be removed on non-ctx error")
}

func TestTransferManager_FreshDownload_CtxCancel_PreservesPartial(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			// Write some data, then simulate cancellation.
			n, err := w.Write([]byte("partial-data"))
			require.NoError(t, err)
			cancel()

			return int64(n), context.Canceled
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "file.txt")

	opts := DownloadOpts{}
	_, err := tm.DownloadToFile(ctx, driveid.New("d1"), "item1", targetPath, opts)
	require.Error(t, err)

	// .partial should be preserved on context cancellation.
	info, statErr := os.Stat(DownloadPartialPath(targetPath, opts))
	require.NoError(t, statErr, "expected .partial to be preserved")

	assert.NotZero(t, info.Size(), "expected .partial to have data")
}

// Validates: R-5.2, R-6.8.3
func TestTransferManager_ResumeDownload_Success(t *testing.T) {
	t.Parallel()

	existingData := []byte("existing-")
	appendData := []byte("appended")
	fullContent := append(append([]byte(nil), existingData...), appendData...)
	expectedHash := tmHashBytes(fullContent)

	dl := &tmMockDownloader{
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer, offset int64) (int64, error) {
			if offset != int64(len(existingData)) {
				return 0, fmt.Errorf("unexpected offset %d", offset)
			}

			n, err := w.Write(appendData)

			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "file.txt")
	opts := DownloadOpts{
		RemoteHash: expectedHash,
	}
	partialPath := DownloadPartialPath(targetPath, opts)

	// Pre-create the .partial file with existing data.
	require.NoError(t, os.WriteFile(partialPath, existingData, 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, expectedHash, result.LocalHash)
	assert.Equal(t, int64(len(fullContent)), result.Size)

	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")

	assert.Equal(t, fullContent, got)
}

// Validates: R-5.2.2, R-5.5.1, R-6.8.3
func TestTransferManager_ResumeDownload_CorruptPartial_RetriesFresh(t *testing.T) {
	t.Parallel()

	remoteContent := []byte("correct-prefix-and-suffix")
	corruptPartial := append([]byte(nil), remoteContent...)
	copy(corruptPartial, "corrupt")

	expectedHash := tmHashBytes(remoteContent)
	var rangeCalls int
	var freshCalls int

	dl := newResumeMismatchDownloader(t, len(corruptPartial), remoteContent, remoteContent, &rangeCalls, &freshCalls)

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "file.txt")
	opts := DownloadOpts{
		RemoteHash:     expectedHash,
		MaxHashRetries: 1,
	}
	partialPath := DownloadPartialPath(targetPath, opts)
	require.NoError(t, os.WriteFile(partialPath, corruptPartial, 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, 1, rangeCalls, "expected one resume attempt before retrying fresh")
	assert.Equal(t, 1, freshCalls, "expected one fresh retry after corrupt partial mismatch")
	assert.Equal(t, expectedHash, result.LocalHash)
	assert.Equal(t, int64(len(remoteContent)), result.Size)

	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")
	assert.Equal(t, remoteContent, got)
}

// Validates: R-5.2.2, R-5.5.1, R-6.8.3
func TestTransferManager_ResumeDownload_RemoteChangedDuringResume_RetriesFresh(t *testing.T) {
	t.Parallel()

	oldContent := []byte("old-remote-version-content")
	newContent := []byte("new-remote-version-content")
	require.Len(t, newContent, len(oldContent))

	partialData := append([]byte(nil), oldContent[:12]...)
	expectedHash := tmHashBytes(newContent)
	var rangeCalls int
	var freshCalls int

	dl := newResumeMismatchDownloader(t, len(partialData), newContent, newContent, &rangeCalls, &freshCalls)

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "changed.txt")
	opts := DownloadOpts{
		RemoteHash:     expectedHash,
		MaxHashRetries: 1,
	}
	partialPath := DownloadPartialPath(targetPath, opts)
	require.NoError(t, os.WriteFile(partialPath, partialData, 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, 1, rangeCalls, "expected one resume attempt against stale partial data")
	assert.Equal(t, 1, freshCalls, "expected one fresh download after the remote changed")
	assert.Equal(t, expectedHash, result.LocalHash)
	assert.Equal(t, int64(len(newContent)), result.Size)

	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")
	assert.Equal(t, newContent, got)
}

// Validates: R-5.2.2, R-5.5.1, R-6.8.3
func TestTransferManager_ResumeDownload_OversizedPartial_RetriesFresh(t *testing.T) {
	t.Parallel()

	remoteContent := []byte("remote-content")
	oversizedPartial := append(append([]byte(nil), remoteContent...), []byte("-stale-tail")...)
	expectedHash := tmHashBytes(remoteContent)
	var rangeCalls int
	var freshCalls int

	dl := &tmMockDownloader{
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer, offset int64) (int64, error) {
			rangeCalls++
			assert.Greater(t, offset, int64(len(remoteContent)), "expected stale partial to exceed remote size")

			// Simulate a server that returns no bytes when the caller resumes past EOF.
			return 0, nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			freshCalls++
			written, err := w.Write(remoteContent)
			return int64(written), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "oversized-partial.txt")
	opts := DownloadOpts{
		RemoteHash:     expectedHash,
		MaxHashRetries: 1,
	}
	partialPath := DownloadPartialPath(targetPath, opts)
	require.NoError(t, os.WriteFile(partialPath, oversizedPartial, 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, 1, rangeCalls, "expected one resume attempt against oversized partial state")
	assert.Equal(t, 1, freshCalls, "expected a fresh retry after oversized partial mismatch")
	assert.Equal(t, expectedHash, result.LocalHash)
	assert.Equal(t, int64(len(remoteContent)), result.Size)

	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")
	assert.Equal(t, remoteContent, got)
}

func TestTransferManager_ResumeDownload_FallsBackToFreshDownload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		content     []byte
		partialData []byte
		rangeErr    string
	}{
		{
			name:        "RangeFail",
			content:     []byte("fresh-content"),
			partialData: []byte("old-data"),
			rangeErr:    "range not supported by server",
		},
		{
			name:        "CloseErrorPath",
			content:     []byte("fresh-after-close-error"),
			partialData: []byte("existing"),
			rangeErr:    "simulated range failure for close-error test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			expectedHash := tmHashBytes(tt.content)
			var freshDownloadCalled bool

			dl := &tmMockDownloader{
				downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer, _ int64) (int64, error) {
					return 0, fmt.Errorf("%s: %w", tt.rangeErr, graph.ErrRangeResumeRejected)
				},
				downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
					freshDownloadCalled = true
					written, err := w.Write(tt.content)
					return int64(written), err
				},
			}

			tm := newTestTM(dl, &tmMockUploader{}, nil)
			targetPath := filepath.Join(t.TempDir(), "file.txt")
			opts := DownloadOpts{
				RemoteHash: expectedHash,
			}
			partialPath := DownloadPartialPath(targetPath, opts)
			require.NoError(t, os.WriteFile(partialPath, tt.partialData, 0o600))

			result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
			require.NoError(t, err, "DownloadToFile")
			assert.True(t, freshDownloadCalled, "expected fresh Download() fallback")
			assert.Equal(t, expectedHash, result.LocalHash)
		})
	}
}

// Validates: R-5.5.1
func TestTransferManager_HashMismatch_Retry(t *testing.T) {
	t.Parallel()

	// First two downloads produce "wrong" content, third produces correct content.
	correctContent := []byte("correct-content")
	correctHash := tmHashBytes(correctContent)
	var downloadCount int

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadCount++
			if downloadCount <= 2 {
				data := fmt.Appendf(nil, "wrong-content-%d", downloadCount)
				n, err := w.Write(data)

				return int64(n), err
			}

			n, err := w.Write(correctContent)

			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "file.txt")

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash:     correctHash,
		MaxHashRetries: 3,
	})
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, 3, downloadCount)
	assert.Equal(t, correctHash, result.LocalHash)
}

// Validates: R-5.5.1, R-5.8.2
func TestTransferManager_DeterministicMismatchAtReportedSize_AcceptedOnce(t *testing.T) {
	t.Parallel()

	content := []byte("always-the-same-bytes")
	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "file.txt")

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash:     "reported-hash",
		RemoteSize:     int64(len(content)),
		MaxHashRetries: 1,
	})
	require.NoError(t, err, "DownloadToFile")

	// The same bytes at the reported size every time: what OneDrive serves
	// for this version. The remote hash recorded is OneDrive's, not the
	// local one (that made every later pass see a remote change).
	assert.True(t, result.AcceptedMismatch)
	assert.False(t, result.HashVerified)
	assert.Equal(t, "reported-hash", result.EffectiveRemoteHash)
	assert.Equal(t, tmHashBytes(content), result.LocalHash)
}

// Validates: R-5.5.1, R-5.8.2
func TestTransferManager_IntegrityFailureKeepsCanonicalFile(t *testing.T) {
	t.Parallel()

	const good = "the user's good local copy"
	errorBody := []byte(`{"error":{"code":"generalException","message":"try again"}}`)

	tests := []struct {
		name     string
		opts     DownloadOpts
		download func(attempt int, w io.Writer) (int64, error)
	}{
		{
			name: "200 with a JSON error body three times",
			opts: DownloadOpts{RemoteHash: "reported-hash", RemoteSize: 4096},
			download: func(_ int, w io.Writer) (int64, error) {
				n, err := w.Write(errorBody)
				return int64(n), err
			},
		},
		{
			name: "error document recognized by type",
			opts: DownloadOpts{RemoteHash: "reported-hash", RemoteSize: 4096},
			download: func(_ int, _ io.Writer) (int64, error) {
				return 0, fmt.Errorf("downloading: %w", graph.ErrUnexpectedDownloadBody)
			},
		},
		{
			name: "different wrong bytes each time",
			opts: DownloadOpts{RemoteHash: "reported-hash", RemoteSize: 5},
			download: func(attempt int, w io.Writer) (int64, error) {
				n, err := fmt.Fprintf(w, "try%d", attempt)
				return int64(n), err
			},
		},
		{
			name: "hashless short body",
			opts: DownloadOpts{RemoteSize: 4096},
			download: func(_ int, w io.Writer) (int64, error) {
				n, err := w.Write([]byte("short"))
				return int64(n), err
			},
		},
		{
			name: "hashless empty body for a non-empty item",
			opts: DownloadOpts{RemoteSize: 4096},
			download: func(_ int, _ io.Writer) (int64, error) {
				return 0, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts int
			dl := &tmSimpleDownloader{
				downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
					attempts++
					return tt.download(attempts, w)
				},
			}

			targetPath := filepath.Join(t.TempDir(), "report.bin")
			require.NoError(t, os.WriteFile(targetPath, []byte(good), 0o600))

			_, err := newTestTM(dl, &tmMockUploader{}, nil).DownloadToFile(
				t.Context(), driveid.New("d1"), "item1", targetPath, tt.opts)

			// These used to be renamed over the user's file: after three
			// attempts with a hash, at once without one.
			require.ErrorIs(t, err, ErrDownloadIntegrity)
			got, readErr := localpath.ReadFile(targetPath)
			require.NoError(t, readErr)
			assert.Equal(t, good, string(got))
			assert.Equal(t, defaultMaxHashRetries+1, attempts, "bounded retries")
			_, statErr := os.Stat(DownloadPartialPath(targetPath, tt.opts))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "no partial is left behind")
		})
	}
}

// Validates: R-5.5.1
func TestTransferManager_EmptyItemWithoutDownloadURLCreatesEmptyFile(t *testing.T) {
	t.Parallel()

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			return 0, graph.ErrNoDownloadURL
		},
	}

	targetPath := filepath.Join(t.TempDir(), "empty.txt")
	result, err := newTestTM(dl, &tmMockUploader{}, nil).DownloadToFile(
		t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{RemoteSize: 0, RemoteSizeKnown: true})
	require.NoError(t, err)
	assert.Zero(t, result.Size)

	info, statErr := os.Stat(targetPath)
	require.NoError(t, statErr)
	assert.Zero(t, info.Size())
}

// Validates: R-5.2.2
func TestTransferManager_StalePartialFromOldVersionIsNotResumed(t *testing.T) {
	t.Parallel()

	newContent := []byte("version two bytes")
	var rangeCalls int
	dl := &tmMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(newContent)
			return int64(n), err
		},
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer, _ int64) (int64, error) {
			rangeCalls++
			return 0, nil
		},
	}

	targetPath := filepath.Join(t.TempDir(), "doc.bin")
	oldOpts := DownloadOpts{RemoteETag: "etag-v1", RemoteSize: int64(len(newContent))}
	newOpts := DownloadOpts{RemoteETag: "etag-v2", RemoteSize: int64(len(newContent))}
	require.NotEqual(t, DownloadPartialPath(targetPath, oldOpts), DownloadPartialPath(targetPath, newOpts))
	require.NoError(t, os.WriteFile(DownloadPartialPath(targetPath, oldOpts), []byte("version"), 0o600))

	_, err := newTestTM(dl, &tmMockUploader{}, nil).DownloadToFile(
		t.Context(), driveid.New("d1"), "item1", targetPath, newOpts)
	require.NoError(t, err)

	// A hashless item resumed across versions spliced old bytes onto new
	// ones with nothing to catch it.
	assert.Zero(t, rangeCalls)
	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr)
	assert.Equal(t, newContent, got)
}

// Validates: R-5.2.2
func TestTransferManager_PartialWithoutVersionIsNotResumed(t *testing.T) {
	t.Parallel()

	var rangeCalls int
	dl := &tmMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("whole"))
			return int64(n), err
		},
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer, _ int64) (int64, error) {
			rangeCalls++
			return 0, nil
		},
	}

	targetPath := filepath.Join(t.TempDir(), "doc.bin")
	require.NoError(t, os.WriteFile(DownloadPartialPath(targetPath, DownloadOpts{}), []byte("wh"), 0o600))

	_, err := newTestTM(dl, &tmMockUploader{}, nil).DownloadToFile(
		t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{})
	require.NoError(t, err)
	assert.Zero(t, rangeCalls, "nothing binds the partial to a version")
}

// Validates: R-5.8.1
//
// The documented OneDrive/iOS HEIC quirk serves a smaller post-processed file
// than the metadata describes. Re-downloading returns the same bytes, so the
// download is accepted on the first attempt (it used to be fetched three
// times), and the known, unactionable condition is logged at Debug only (it
// used to print four warnings per photo).
func TestTransferManager_HEICMetadataMismatch_AcceptsFirstDownloadQuietly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	content := []byte("heic-bytes-after-download")

	var downloadCount int
	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadCount++
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := NewTransferManager(dl, &tmMockUploader{}, nil, logger)
	targetPath := filepath.Join(t.TempDir(), "IMG_0001.HEIC")

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash:     "definitely-wrong-hash",
		RemoteSize:     int64(len(content) + 1234),
		MaxHashRetries: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, downloadCount, "re-downloading cannot fix the server-side HEIC quirk")
	assert.False(t, result.HashVerified)
	assert.True(t, result.AcceptedMismatch)
	// OneDrive's own hash is what the baseline records, so the next pass
	// sees no remote change and does not download the photo again.
	assert.Equal(t, "definitely-wrong-hash", result.EffectiveRemoteHash)
	assert.Contains(t, buf.String(), "known OneDrive/iOS metadata mismatch")
	assert.Contains(t, buf.String(), "accepting download")
	assert.NotContains(t, buf.String(), "level=WARN")
}

// Validates: R-5.5.1
//
// A HEIC whose size matches but whose hash does not is not the known quirk,
// so it keeps the normal hash retries.
func TestTransferManager_HEICSameSizeHashMismatch_StillRetries(t *testing.T) {
	t.Parallel()

	content := []byte("heic-bytes")

	var downloadCount int
	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadCount++
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "IMG_0002.heic")

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash:     "definitely-wrong-hash",
		RemoteSize:     int64(len(content)),
		MaxHashRetries: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, downloadCount)
}

// Validates: R-5.8.2
func TestTransferManager_AcceptedTransformDoesNotWarn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	content := []byte("always-the-same-bytes")

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := NewTransferManager(dl, &tmMockUploader{}, nil, logger)
	targetPath := filepath.Join(t.TempDir(), "file.txt")

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash:     "definitely-wrong-hash",
		RemoteSize:     int64(len(content)),
		MaxHashRetries: 2,
	})
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "level=WARN", buf.String())
	assert.Equal(t, 1, strings.Count(buf.String(), "level=INFO"), buf.String())
	assert.NotContains(t, buf.String(), "definitely-wrong-hash")
}

// Validates: R-5.5.1
//
// Items without any content hash (some SharePoint files) are downloaded on
// every pass; the unverifiable state is reported through HashVerified, not as
// a warning per file.
func TestTransferManager_NoRemoteHash_DoesNotWarn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("content"))
			return int64(n), err
		},
	}

	tm := NewTransferManager(dl, &tmMockUploader{}, nil, logger)
	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1",
		filepath.Join(t.TempDir(), "x.docx"), DownloadOpts{})
	require.NoError(t, err)
	assert.False(t, result.HashVerified)
	assert.Empty(t, buf.String())
}

// Validates: R-6.2.6
//
// A persistent statfs failure (an unusual FUSE mount) fails open for every
// download; it is announced once, not once per file.
func TestTransferManager_DiskCheckFailure_WarnsOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("content"))
			return int64(n), err
		},
	}

	tm := NewTransferManager(dl, &tmMockUploader{}, nil, logger,
		WithDiskCheck(1, func(string) (uint64, error) { return 0, errors.New("statfs: function not implemented") }))

	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item-"+name, filepath.Join(dir, name), DownloadOpts{})
		require.NoError(t, err)
	}

	assert.Equal(t, 1, strings.Count(buf.String(), "level=WARN"), buf.String())
}

// Validates: R-6.6.26
//
// A failed download names the file the user asked for, not the internal
// ".onedrive-go.<name>.partial" temp file.
func TestTransferManager_DownloadError_NamesTargetNotPartial(t *testing.T) {
	t.Parallel()

	dl := &tmSimpleDownloader{
		downloadFn: func(context.Context, driveid.ID, string, io.Writer) (int64, error) {
			return 0, errors.New("connection reset")
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "report.pdf")

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), targetPath)
	assert.NotContains(t, err.Error(), ".partial")
}

// ---------------------------------------------------------------------------
// Upload tests
// ---------------------------------------------------------------------------

func TestTransferManager_Upload_Success(t *testing.T) {
	t.Parallel()

	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return tmUploadedItem(t, "item-1", content, size), nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	// Create a temp file to upload.
	dir := t.TempDir()
	localPath := filepath.Join(dir, "upload.txt")

	require.NoError(t, os.WriteFile(localPath, []byte("upload data"), 0o600))

	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "upload.txt", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile")

	assert.Equal(t, "item-1", result.Item.ID)
	assert.NotEmpty(t, result.LocalHash, "LocalHash should not be empty")

	// Verify Size and Mtime are populated (fix #4).
	assert.Equal(t, int64(11), result.Size)
	assert.False(t, result.Mtime.IsZero(), "Mtime should not be zero")
}

// Validates: R-5.5.2
func TestTransferManager_Upload_PersonalHashMismatchFails(t *testing.T) {
	t.Parallel()

	var rereads int
	stored := &graph.Item{
		ID:              "item-1",
		Size:            int64(len("upload data")),
		QuickXorHash:    tmHashBytes([]byte("other data!")),
		ParentDriveType: "personal",
	}
	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return stored, nil
		},
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			rereads++
			assert.Equal(t, "item-1", itemID)
			return stored, nil
		},
	}

	localPath := filepath.Join(t.TempDir(), "report.docx")
	require.NoError(t, os.WriteFile(localPath, []byte("upload data"), 0o600))

	_, err := newTestTM(&tmSimpleDownloader{}, ul, nil).UploadFile(
		t.Context(), driveid.New("d1"), "parent1", "report.docx", localPath, UploadOpts{})

	// Logged at Info and reported as success before: the baseline then
	// recorded different bytes on each side as converged, forever.
	require.ErrorIs(t, err, ErrUploadVerificationFailed)
	assert.Equal(t, 1, rereads, "a mismatch is read back once before failing")
}

// Validates: R-5.5.2
func TestTransferManager_Upload_MismatchThatReadsBackVerifiedSucceeds(t *testing.T) {
	t.Parallel()

	data := []byte("upload data")
	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item-1", Size: 3, QuickXorHash: tmHashBytes([]byte("pre"))}, nil
		},
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			return &graph.Item{ID: "item-1", Size: int64(len(data)), QuickXorHash: tmHashBytes(data), ETag: "committed"}, nil
		},
	}

	localPath := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(localPath, data, 0o600))

	result, err := newTestTM(&tmSimpleDownloader{}, ul, nil).UploadFile(
		t.Context(), driveid.New("d1"), "parent1", "a.txt", localPath, UploadOpts{})
	require.NoError(t, err)
	assert.Equal(t, "committed", result.Item.ETag, "the verified read-back is the recorded item")
}

// Validates: R-5.5.2
func TestTransferManager_Upload_WarnsOnHashMismatch(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	enriched := &graph.Item{
		ID:              "item-1",
		Size:            4096,
		QuickXorHash:    base64.StdEncoding.EncodeToString([]byte("remote-hash")),
		ParentDriveType: "documentLibrary",
	}
	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return enriched, nil
		},
	}

	localPath := filepath.Join(t.TempDir(), "report.docx")
	require.NoError(t, os.WriteFile(localPath, []byte("upload data"), 0o600))

	tm := NewTransferManager(&tmSimpleDownloader{}, ul, nil, logger)

	// SharePoint rewrites Office documents on every upload: the stored item
	// is recorded as it is, without a failure or a warning.
	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "report.docx", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile")
	assert.Same(t, enriched, result.Item)
	assert.Contains(t, buf.String(), "OneDrive enriched the uploaded file")
	assert.NotContains(t, buf.String(), "level=WARN")
}

// Validates: R-5.5.2
func TestTransferManager_Upload_CompletionWithoutItemIsLookedUp(t *testing.T) {
	t.Parallel()

	data := []byte("data")
	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrUploadCompletedWithoutItem
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, parentID string) ([]graph.Item, error) {
			assert.Equal(t, "parent1", parentID)
			return []graph.Item{
				{ID: "folder", Name: "nil-item.txt", IsFolder: true},
				{ID: "found", Name: "nil-item.txt", Size: int64(len(data)), QuickXorHash: tmHashBytes(data)},
			}, nil
		},
	}

	localPath := filepath.Join(t.TempDir(), "nil-item.txt")
	require.NoError(t, os.WriteFile(localPath, data, 0o600))

	result, err := newTestTM(&tmSimpleDownloader{}, ul, nil).UploadFile(
		t.Context(), driveid.New("d1"), "parent1", "nil-item.txt", localPath, UploadOpts{})
	require.NoError(t, err)
	assert.Equal(t, "found", result.Item.ID, "an item without identity is never recorded")
}

// Validates: R-5.5.2
func TestTransferManager_Upload_NilItem(t *testing.T) {
	t.Parallel()

	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{}, nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	localPath := filepath.Join(t.TempDir(), "nil-item.txt")
	require.NoError(t, os.WriteFile(localPath, []byte("data"), 0o600))

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "nil-item.txt", localPath, UploadOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "looking it up failed")
}

func TestTransferManager_Upload_ErrorWrapping(t *testing.T) {
	t.Parallel()

	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, fmt.Errorf("server error")
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	dir := t.TempDir()
	localPath := filepath.Join(dir, "err-wrap.txt")

	require.NoError(t, os.WriteFile(localPath, []byte("data"), 0o600))

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "err-wrap.txt", localPath, UploadOpts{})
	require.Error(t, err)

	// Fix #13: simple upload errors should be wrapped with the local path.
	assert.Contains(t, err.Error(), "uploading")
	assert.Contains(t, err.Error(), localPath)
}

// Validates: R-5.7.1
func TestTransferManager_Upload_RejectsOversizedFileBeforeHashOrTransfer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	localPath := filepath.Join(dir, "oversized.bin")

	file, err := localpath.OpenFile(localPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(MaxOneDriveFileSize+1))
	require.NoError(t, file.Close())

	var hashCalled bool
	var uploadCalled bool

	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true
			return &graph.Item{ID: "unexpected"}, nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)
	tm.hashFunc = func(string) (string, error) {
		hashCalled = true
		return "unexpected", nil
	}

	_, err = tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "oversized.bin", localPath, UploadOpts{})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrFileExceedsOneDriveLimit)
	assert.Contains(t, err.Error(), "250 GB")
	assert.False(t, hashCalled, "oversized files should be rejected before hashing")
	assert.False(t, uploadCalled, "oversized files should be rejected before transfer")
}

// Validates: R-1.3, R-5.2.1
func TestTransferManager_SessionUpload_Success(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	ul := &tmMockUploader{
		createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
			return &graph.UploadSession{UploadURL: "https://upload.example.com/session"}, nil
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, content io.ReaderAt, size int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return tmUploadedItem(t, "session-item", content, size), nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	// Create a file larger than persistedSessionMinSize.
	localPath := filepath.Join(dir, "large.bin")
	largeData := make([]byte, persistedSessionMinSize+1)

	require.NoError(t, os.WriteFile(localPath, largeData, 0o600))

	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "large.bin", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile")

	assert.Equal(t, "session-item", result.Item.ID)
}

// Validates: R-5.2.3, R-5.2.4
func TestTransferManager_SessionUpload_Resume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	// Create a file larger than persistedSessionMinSize.
	localPath := filepath.Join(dir, "resume.bin")
	largeData := make([]byte, persistedSessionMinSize+1)

	require.NoError(t, os.WriteFile(localPath, largeData, 0o600))

	// Pre-compute hash to match the session record.
	fileHash, hashErr := ComputeQuickXorHash(localPath)
	require.NoError(t, hashErr, "ComputeQuickXorHash")

	// Pre-save a session record.
	driveStr := driveid.New("d1").String()
	saveErr := store.Save(driveStr, localPath, &SessionRecord{
		SessionURL: "https://upload.example.com/existing",
		FileHash:   fileHash,
		FileSize:   int64(len(largeData)),
	})
	require.NoError(t, saveErr, "Save")

	var resumeCalled bool

	ul := &tmMockUploader{
		resumeUploadFn: func(_ context.Context, session *graph.UploadSession, content io.ReaderAt, size int64, _ graph.ProgressFunc) (*graph.Item, error) {
			resumeCalled = true

			if session.UploadURL != "https://upload.example.com/existing" {
				return nil, fmt.Errorf("unexpected session URL: %s", session.UploadURL)
			}

			return tmUploadedItem(t, "resumed-item", content, size), nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "resume.bin", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile")

	assert.True(t, resumeCalled, "expected ResumeUpload to be called")
	assert.Equal(t, "resumed-item", result.Item.ID)
}

func TestTransferManager_SessionUpload_ExpiredFallback(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	// Create a file larger than persistedSessionMinSize.
	localPath := filepath.Join(dir, "expired.bin")
	largeData := make([]byte, persistedSessionMinSize+1)

	require.NoError(t, os.WriteFile(localPath, largeData, 0o600))

	fileHash, hashErr := ComputeQuickXorHash(localPath)
	require.NoError(t, hashErr, "ComputeQuickXorHash")

	driveStr := driveid.New("d1").String()
	saveErr := store.Save(driveStr, localPath, &SessionRecord{
		SessionURL: "https://upload.example.com/expired",
		FileHash:   fileHash,
		FileSize:   int64(len(largeData)),
	})
	require.NoError(t, saveErr, "Save")

	var createCalled, uploadFromCalled bool

	ul := &tmMockUploader{
		resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrUploadSessionExpired
		},
		createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
			createCalled = true

			return &graph.UploadSession{UploadURL: "https://upload.example.com/fresh"}, nil
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, content io.ReaderAt, size int64, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadFromCalled = true

			return tmUploadedItem(t, "fresh-item", content, size), nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "expired.bin", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile")

	assert.True(t, createCalled, "expected CreateUploadSession to be called after expiration")
	assert.True(t, uploadFromCalled, "expected UploadFromSession to be called after fresh session creation")
	assert.Equal(t, "fresh-item", result.Item.ID)
}

func TestTransferManager_DriveID_InLogs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	content := []byte("log-test")
	expectedHash := tmHashBytes(content)

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := NewTransferManager(dl, &tmMockUploader{}, nil, logger)
	targetPath := filepath.Join(t.TempDir(), "log-test.txt")
	driveID := driveid.New("test-drive-id-123")

	_, err := tm.DownloadToFile(t.Context(), driveID, "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	logOutput := buf.String()
	assert.Contains(t, logOutput, "test-drive-id-123", "expected drive_id in log output")
}

// ---------------------------------------------------------------------------
// B-208: session delete on any resume failure
// ---------------------------------------------------------------------------

func TestSessionUpload_NonExpiredResumeError_DeletesSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	localPath := filepath.Join(dir, "network-err.bin")
	largeData := make([]byte, persistedSessionMinSize+1)

	require.NoError(t, os.WriteFile(localPath, largeData, 0o600))

	fileHash, hashErr := ComputeQuickXorHash(localPath)
	require.NoError(t, hashErr, "ComputeQuickXorHash")

	driveStr := driveid.New("d1").String()
	saveErr := store.Save(driveStr, localPath, &SessionRecord{
		SessionURL: "https://upload.example.com/broken",
		FileHash:   fileHash,
		FileSize:   int64(len(largeData)),
	})
	require.NoError(t, saveErr, "Save")

	ul := &tmMockUploader{
		resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, fmt.Errorf("network error")
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "network-err.bin", localPath, UploadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "network error")

	// B-208: session should be deleted even for non-expired errors.
	rec, found, loadErr := store.Load(driveStr, localPath)
	require.NoError(t, loadErr, "Load")
	assert.False(t, found, "expected session to be deleted after non-expired resume error")
	assert.Nil(t, rec, "expected session to be deleted after non-expired resume error")
}

func TestSessionUpload_NonExpiredResumeError_FreshOnRetry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	localPath := filepath.Join(dir, "retry.bin")
	largeData := make([]byte, persistedSessionMinSize+1)

	require.NoError(t, os.WriteFile(localPath, largeData, 0o600))

	fileHash, hashErr := ComputeQuickXorHash(localPath)
	require.NoError(t, hashErr, "ComputeQuickXorHash")

	driveStr := driveid.New("d1").String()
	saveErr := store.Save(driveStr, localPath, &SessionRecord{
		SessionURL: "https://upload.example.com/broken-retry",
		FileHash:   fileHash,
		FileSize:   int64(len(largeData)),
	})
	require.NoError(t, saveErr, "Save")

	callCount := 0

	ul := &tmMockUploader{
		resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			callCount++
			return nil, fmt.Errorf("rate limited")
		},
		createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
			return &graph.UploadSession{UploadURL: "https://upload.example.com/fresh-retry"}, nil
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, content io.ReaderAt, size int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return tmUploadedItem(t, "fresh-retry-item", content, size), nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	// First call fails (resume returns non-expired error).
	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "retry.bin", localPath, UploadOpts{})
	require.Error(t, err)

	assert.Equal(t, 1, callCount)

	// Second call should NOT attempt resume (session was deleted), should create fresh.
	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "retry.bin", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile retry")

	// Resume should not be called again (session deleted, so no match).
	assert.Equal(t, 1, callCount, "resumeUpload should not be called again after session deleted")

	assert.Equal(t, "fresh-retry-item", result.Item.ID)
}

// ---------------------------------------------------------------------------
// Empty-string validation tests
// ---------------------------------------------------------------------------

func TestDownloadToFile_EmptyTargetPath(t *testing.T) {
	t.Parallel()

	tm := newTestTM(&tmSimpleDownloader{}, &tmMockUploader{}, nil)

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", "", DownloadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "target path must not be empty")
}

func TestDownloadToFile_EmptyItemID(t *testing.T) {
	t.Parallel()

	tm := newTestTM(&tmSimpleDownloader{}, &tmMockUploader{}, nil)

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "", "/tmp/file.txt", DownloadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "item ID must not be empty")
}

func TestUploadFile_EmptyName(t *testing.T) {
	t.Parallel()

	tm := newTestTM(&tmSimpleDownloader{}, &tmMockUploader{}, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "", "/tmp/file.txt", UploadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "file name must not be empty")
}

func TestUploadFile_EmptyParentID(t *testing.T) {
	t.Parallel()

	tm := newTestTM(&tmSimpleDownloader{}, &tmMockUploader{}, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "", "file.txt", "/tmp/file.txt", UploadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "parent ID must not be empty")
}

func TestUploadFile_EmptyLocalPath(t *testing.T) {
	t.Parallel()

	tm := newTestTM(&tmSimpleDownloader{}, &tmMockUploader{}, nil)

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "file.txt", "", UploadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "local path must not be empty")
}

// ---------------------------------------------------------------------------
// File permission tests
// ---------------------------------------------------------------------------

func TestRemovePartialIfNotCanceled(t *testing.T) {
	t.Parallel()

	tm := newTestTM(&tmSimpleDownloader{}, nil, nil)

	t.Run("removes file when context is active", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "test.partial")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))

		tm.removePartialIfNotCanceled(t.Context(), LocalTarget(), path)

		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err), "expected file to be removed")
	})

	t.Run("preserves file when context is canceled", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "test.partial")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		tm.removePartialIfNotCanceled(ctx, LocalTarget(), path)

		_, err := os.Stat(path)
		assert.NoError(t, err, "expected file to be preserved")
	})

	t.Run("no panic for nonexistent file", func(t *testing.T) {
		t.Parallel()

		// Should not panic when file doesn't exist.
		tm.removePartialIfNotCanceled(t.Context(), LocalTarget(), "/nonexistent/path.partial")
	})
}

// ---------------------------------------------------------------------------
// B-214: rename failure preserves .partial for future resume
// ---------------------------------------------------------------------------

func TestDownloadToFile_RenameFailure_PreservesPartial(t *testing.T) {
	t.Parallel()

	content := []byte("data-to-preserve")
	expectedHash := tmHashBytes(content)

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()

	// Make the target path a directory so os.Rename fails with EISDIR when
	// trying to rename .partial (a file) over it.
	targetPath := filepath.Join(dir, "target_is_dir")
	require.NoError(t, os.Mkdir(targetPath, 0o700))

	opts := DownloadOpts{
		RemoteHash: expectedHash,
	}
	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.Error(t, err)

	assert.Contains(t, err.Error(), "renaming partial")

	// .partial should still exist with correct content.
	partialPath := DownloadPartialPath(targetPath, opts)

	got, readErr := localpath.ReadFile(partialPath)
	require.NoError(t, readErr, "expected .partial to be preserved")

	assert.Equal(t, content, got, "partial content mismatch")
}

// Validates: R-2.8.10
func TestDownloadToFile_TargetPreconditionBeforeRenamePreservesPartial(t *testing.T) {
	t.Parallel()

	content := []byte("data-to-preserve")
	expectedHash := tmHashBytes(content)
	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "target.txt")

	opts := DownloadOpts{
		RemoteHash: expectedHash,
		ValidateTargetBeforeRename: func() error {
			return assert.AnError
		},
	}
	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.ErrorIs(t, err, assert.AnError)

	assert.NoFileExists(t, targetPath)
	got, readErr := localpath.ReadFile(DownloadPartialPath(targetPath, opts))
	require.NoError(t, readErr)
	assert.Equal(t, content, got)
}

// Validates: R-2.8.10
func TestUploadFile_SourcePreconditionRunsBeforeUpload(t *testing.T) {
	t.Parallel()

	ul := &tmMockUploader{
		uploadFn: func(context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc) (*graph.Item, error) {
			require.FailNow(t, "upload should not run after source precondition failure")
			return nil, assert.AnError
		},
	}
	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)
	localPath := filepath.Join(t.TempDir(), "preflight.txt")
	require.NoError(t, os.WriteFile(localPath, []byte("hello"), 0o600))

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "preflight.txt", localPath, UploadOpts{
		ValidateSourceBeforeRead: func() error {
			return assert.AnError
		},
	})
	require.ErrorIs(t, err, assert.AnError)
}

// Validates: R-2.8.10
func TestUploadFile_SourcePreconditionRunsDuringSessionRead(t *testing.T) {
	t.Parallel()

	validateCalls := 0
	ul := &tmMockUploader{
		createUploadSessionFn: func(context.Context, driveid.ID, string, string, int64, time.Time) (*graph.UploadSession, error) {
			return &graph.UploadSession{UploadURL: "https://upload.example.com/live-precondition"}, nil
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, content io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			buf := make([]byte, 1)
			_, err := content.ReadAt(buf, 0)
			return nil, fmt.Errorf("reading session content: %w", err)
		},
	}

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	tm := newTestTM(&tmSimpleDownloader{}, ul, store)
	localPath := filepath.Join(dir, "large.bin")
	require.NoError(t, os.WriteFile(localPath, make([]byte, persistedSessionMinSize+1), 0o600))

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "large.bin", localPath, UploadOpts{
		ValidateSourceBeforeRead: func() error {
			validateCalls++
			if validateCalls >= 3 {
				return assert.AnError
			}
			return nil
		},
	})
	require.ErrorIs(t, err, assert.AnError)
	assert.GreaterOrEqual(t, validateCalls, 3)
}

// ---------------------------------------------------------------------------
// B-215: session save failure still completes upload
// ---------------------------------------------------------------------------

func TestSessionUpload_SaveFailure_StillCompletes(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	// Pre-create the upload-sessions directory so we can make it read-only.
	require.NoError(t, os.MkdirAll(store.dir, 0o700))

	ul := &tmMockUploader{
		createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
			return &graph.UploadSession{UploadURL: "https://upload.example.com/save-fail"}, nil
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, content io.ReaderAt, size int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return tmUploadedItem(t, "save-fail-ok", content, size), nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	// Make the store directory read-only so Save fails.
	setTestDirPermissions(t, store.dir, 0o444)

	t.Cleanup(func() { setTestDirPermissions(t, store.dir, 0o700) })

	localPath := filepath.Join(dir, "save-fail.bin")
	largeData := make([]byte, persistedSessionMinSize+1)

	require.NoError(t, os.WriteFile(localPath, largeData, 0o600))

	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "save-fail.bin", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile should succeed despite Save failure")

	assert.Equal(t, "save-fail-ok", result.Item.ID)
}

// ---------------------------------------------------------------------------
// B-216: UploadFile stat failure wraps error correctly
// ---------------------------------------------------------------------------

func TestUploadFile_StatFailure_WrapsError(t *testing.T) {
	t.Parallel()

	tm := newTestTM(&tmSimpleDownloader{}, &tmMockUploader{}, nil)

	_, err := tm.UploadFile(
		t.Context(), driveid.New("d1"), "parent1", "file.txt",
		"/nonexistent/path/file.txt", UploadOpts{},
	)
	require.Error(t, err)

	// Error should wrap os.ErrNotExist and be discoverable via errors.Is.
	require.ErrorIs(t, err, os.ErrNotExist, "expected errors.Is(err, os.ErrNotExist)")

	// Error should contain "stat" context.
	assert.Contains(t, err.Error(), "stat")
}

// ---------------------------------------------------------------------------
// B-217: non-RangeDownloader with existing .partial starts fresh
// ---------------------------------------------------------------------------

func TestDownloadToFile_SimpleDownloader_OverwritesPartial(t *testing.T) {
	t.Parallel()

	freshContent := []byte("fresh-content-overwrites")
	expectedHash := tmHashBytes(freshContent)

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(freshContent)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "file.txt")
	opts := DownloadOpts{
		RemoteHash: expectedHash,
	}
	partialPath := DownloadPartialPath(targetPath, opts)

	// Pre-create a .partial file with old content — should be overwritten.
	require.NoError(t, os.WriteFile(partialPath, []byte("old-partial-data-that-should-go-away"), 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.NoError(t, err, "DownloadToFile")

	// Final file should contain fresh content, not concatenated with old.
	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")

	assert.Equal(t, freshContent, got)
	assert.Equal(t, expectedHash, result.LocalHash)

	// .partial should not exist after successful download.
	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "expected .partial to be removed")
}

// TestTransferManager_ResumeDownload_PartialDeletedBeforeOpen verifies that
// when a .partial file is deleted between the existence check and open
// (TOCTOU race), downloadToPartial falls back to a fresh download (B-211).
func TestTransferManager_ResumeDownload_PartialDeletedBeforeOpen(t *testing.T) {
	t.Parallel()

	content := []byte("fresh-download-content")
	expectedHash := tmHashBytes(content)
	var freshCalled bool

	dl := &tmMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			freshCalled = true
			n, err := w.Write(content)

			return int64(n), err
		},
		// DownloadRange is set so the downloader satisfies RangeDownloader,
		// but should NOT be called since the .partial doesn't exist.
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer, _ int64) (int64, error) {
			require.Fail(t, "DownloadRange should not be called when .partial is absent")
			return 0, nil
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "file.txt")

	// Do NOT create a .partial file — simulates it being deleted before open.

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	assert.True(t, freshCalled, "expected fresh download to be called")
	assert.Equal(t, expectedHash, result.LocalHash)
}

// TestDownloadToFile_EmptyRemoteHash_HashVerifiedFalse verifies that when
// the remote item has no hash (common on some SharePoint files), the download
// succeeds but HashVerified is false (B-021).
func TestDownloadToFile_EmptyRemoteHash_HashVerifiedFalse(t *testing.T) {
	t.Parallel()

	content := []byte("no-hash file content")

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "nohash.txt")

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: "", // empty — no hash available from remote
	})
	require.NoError(t, err, "DownloadToFile")

	// Download should succeed.
	assert.NotEmpty(t, result.LocalHash, "LocalHash should not be empty")

	// HashVerified should be false — no verification occurred.
	assert.False(t, result.HashVerified, "HashVerified should be false when remote hash is empty")

	// File should exist with correct content.
	got, readErr := localpath.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")

	assert.Equal(t, content, got, "file content mismatch")
}

// ---------------------------------------------------------------------------
// Disk space pre-check tests (R-2.10.43, R-2.10.44, R-6.2.6, R-6.4.7)
// ---------------------------------------------------------------------------

// Validates: R-2.10.43, R-2.10.44, R-6.2.6, R-6.4.7
func TestTransferManager_DiskSpaceCheck(t *testing.T) {
	t.Parallel()

	content := []byte("disk-check-content")
	expectedHash := tmHashBytes(content)
	downloader := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			written, err := w.Write(content)
			return int64(written), err
		},
	}
	newManager := func(opts ...Option) *TransferManager {
		return NewTransferManager(downloader, &tmMockUploader{}, nil, slog.Default(), opts...)
	}

	tests := []struct {
		name       string
		newTM      func() *TransferManager
		remoteSize int64
		wantErr    error
	}{
		{
			name:       "DiskFull",
			remoteSize: 100,
			wantErr:    ErrDiskFull,
			newTM: func() *TransferManager {
				return newManager(WithDiskCheck(1000, func(string) (uint64, error) { return 500, nil }))
			},
		},
		{
			name:       "FileTooLarge",
			remoteSize: 1500,
			wantErr:    ErrFileTooLargeForSpace,
			newTM: func() *TransferManager {
				return newManager(WithDiskCheck(1000, func(string) (uint64, error) { return 2000, nil }))
			},
		},
		{
			name: "Disabled_NilFunc",
			newTM: func() *TransferManager {
				return newManager()
			},
		},
		{
			name: "Disabled_ZeroMinFreeSpace",
			newTM: func() *TransferManager {
				return newManager(WithDiskCheck(0, func(string) (uint64, error) { return 100, nil }))
			},
		},
		{
			name: "StatfsError_FailOpen",
			newTM: func() *TransferManager {
				return newManager(WithDiskCheck(1000, func(string) (uint64, error) {
					return 0, fmt.Errorf("simulated statfs error")
				}))
			},
		},
		{
			name:       "SufficientSpace",
			remoteSize: 2000,
			newTM: func() *TransferManager {
				return newManager(WithDiskCheck(1000, func(string) (uint64, error) { return 5000, nil }))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			targetPath := filepath.Join(t.TempDir(), "file.txt")
			result, err := tt.newTM().DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
				RemoteHash: expectedHash,
				RemoteSize: tt.remoteSize,
			})
			if tt.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, expectedHash, result.LocalHash)
		})
	}
}

// Validates: R-5.2.5
func TestDownloadPartialName_FitsLocalNameLimit(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ".onedrive-go.report.pdf.partial", downloadPartialName("report.pdf", ""))

	long := strings.Repeat("w", 246) + ".txt" // 250 bytes: valid locally
	partial := downloadPartialName(long, "")
	assert.LessOrEqual(t, len(partial), maxLocalNameBytes)
	assert.True(t, IsOwnedTransferArtifactName(partial))
	assert.Equal(t, partial, downloadPartialName(long, ""), "deterministic, so a resume finds the same partial")
	assert.NotEqual(t, partial, downloadPartialName(strings.Repeat("w", 246)+".TXT", ""), "different names never share a partial")

	japanese := strings.Repeat("日本", 42) // 252 bytes of multi-byte runes
	partial = downloadPartialName(japanese, "")
	assert.LessOrEqual(t, len(partial), maxLocalNameBytes)
	assert.True(t, utf8.ValidString(partial), "truncation keeps whole runes")
}

// Validates: R-5.6.5
func TestUploadFile_ZeroByteMtimeNotAppliedStillSucceeds(t *testing.T) {
	t.Parallel()

	localPath := filepath.Join(t.TempDir(), "empty.txt")
	require.NoError(t, os.WriteFile(localPath, nil, 0o600))

	var uploads int
	uploaded := &graph.Item{ID: "empty-item", Name: "empty.txt"}
	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploads++
			return uploaded, &graph.MtimeNotAppliedError{Item: uploaded, Err: errors.New("patch failed")}
		},
	}

	result, err := newTestTM(&tmMockDownloader{}, ul, nil).UploadFile(
		t.Context(), driveid.New("d"), "parent", "empty.txt", localPath, UploadOpts{},
	)

	// The content is on OneDrive: failing here made the caller upload it
	// again, one more version per attempt, and still never set the mtime.
	require.NoError(t, err)
	assert.Same(t, uploaded, result.Item)
	assert.True(t, result.RemoteMtimePending)
	assert.Equal(t, 1, uploads)
}

// tmUploadedItem is the item OneDrive returns for a faithful upload: its
// QuickXorHash and size match what was sent.
func tmUploadedItem(t *testing.T, id string, content io.ReaderAt, size int64) *graph.Item {
	t.Helper()

	data := make([]byte, size)
	_, err := content.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		require.NoError(t, err)
	}

	return &graph.Item{ID: id, Size: size, QuickXorHash: tmHashBytes(data)}
}

// tmDownloadWriting returns a downloader that serves content.
func tmDownloadWriting(content []byte) *tmSimpleDownloader {
	return &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}
}

// Validates: R-5.10.2
func TestDownloadToFile_NewFileAndParentHonourUmask(t *testing.T) {
	t.Parallel()

	content := []byte("new file")
	umask := probeUmask()
	targetPath := filepath.Join(t.TempDir(), "newdir", "file.txt")

	_, err := newTestTM(tmDownloadWriting(content), &tmMockUploader{}, nil).DownloadToFile(
		t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{RemoteHash: tmHashBytes(content)})
	require.NoError(t, err)

	// Downloads used to be 0600 and new parents 0700 whatever the umask,
	// which broke group sharing (Samba, shared folders) for every file.
	fileInfo, err := os.Stat(targetPath)
	require.NoError(t, err)
	assert.Equal(t, 0o666&^umask, fileInfo.Mode().Perm())

	dirInfo, err := os.Stat(filepath.Dir(targetPath))
	require.NoError(t, err)
	assert.Equal(t, 0o777&^umask, dirInfo.Mode().Perm())
}

// Validates: R-5.10.2
func TestDownloadToFile_PreservesExistingTargetMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing os.FileMode
		want     os.FileMode
	}{
		{name: "executable script keeps +x", existing: 0o755, want: 0o755},
		{name: "group-readable file stays group-readable", existing: 0o640, want: 0o640},
		{name: "setuid is never handed to new bytes", existing: 0o755 | os.ModeSetuid, want: 0o755},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content := []byte("new remote version")
			targetPath := filepath.Join(t.TempDir(), "tool.sh")
			require.NoError(t, os.WriteFile(targetPath, []byte("old"), 0o600))
			require.NoError(t, os.Chmod(targetPath, tt.existing))

			_, err := newTestTM(tmDownloadWriting(content), &tmMockUploader{}, nil).DownloadToFile(
				t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{RemoteHash: tmHashBytes(content)})
			require.NoError(t, err)

			info, err := os.Stat(targetPath)
			require.NoError(t, err)
			assert.Equal(t, tt.want, info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
		})
	}
}

// recordingTarget records the order of the filesystem steps a download takes.
type recordingTarget struct {
	TargetFS
	steps []string
}

func (r *recordingTarget) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	step := "open-partial"
	if flag == os.O_WRONLY {
		step = "finalize(chmod,fsync)"
	}
	r.steps = append(r.steps, step)

	return r.TargetFS.OpenFile(name, flag, perm) //nolint:wrapcheck // test pass-through
}

func (r *recordingTarget) Chtimes(name string, atime, mtime time.Time) error {
	r.steps = append(r.steps, "chtimes")
	return r.TargetFS.Chtimes(name, atime, mtime) //nolint:wrapcheck // test pass-through
}

func (r *recordingTarget) Rename(oldName, newName string) error {
	r.steps = append(r.steps, "rename")
	return r.TargetFS.Rename(oldName, newName) //nolint:wrapcheck // test pass-through
}

func (r *recordingTarget) SyncDir(dir string) error {
	r.steps = append(r.steps, "syncdir:"+filepath.Base(dir))
	return r.TargetFS.SyncDir(dir) //nolint:wrapcheck // test pass-through
}

// Validates: R-5.10.1
func TestDownloadToFile_FsyncsBeforeRenameAndSyncsDirAfter(t *testing.T) {
	t.Parallel()

	content := []byte("durable")
	dir := filepath.Join(t.TempDir(), "folder")
	require.NoError(t, os.Mkdir(dir, 0o700))
	target := &recordingTarget{TargetFS: LocalTarget()}

	_, err := newTestTM(tmDownloadWriting(content), &tmMockUploader{}, nil).DownloadToFile(
		t.Context(), driveid.New("d1"), "item1", filepath.Join(dir, "f.txt"),
		DownloadOpts{RemoteHash: tmHashBytes(content), RemoteMtime: time.Now().UnixNano(), Target: target})
	require.NoError(t, err)

	// The baseline commit follows success, so the bytes and the directory
	// entry must be on disk first: a power cut used to leave an empty file
	// the next pass trusted or uploaded over the good remote copy.
	assert.Equal(t, []string{"open-partial", "finalize(chmod,fsync)", "chtimes", "rename", "syncdir:folder"}, target.steps)
}

// refusingParentTarget is a TargetFS whose parent check fails, as sync's
// does for a parent the plan did not observe.
type refusingParentTarget struct{ TargetFS }

var errParentGone = errors.New("parent folder is gone")

func (refusingParentTarget) PrepareParent(string) error { return errParentGone }

// Validates: R-5.10.3
func TestDownloadToFile_ParentRefusalStopsBeforeAnyWrite(t *testing.T) {
	t.Parallel()

	var downloads int
	dl := &tmSimpleDownloader{downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
		downloads++
		return 0, nil
	}}
	targetPath := filepath.Join(t.TempDir(), "moved-away", "f.txt")

	_, err := newTestTM(dl, &tmMockUploader{}, nil).DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath,
		DownloadOpts{Target: refusingParentTarget{TargetFS: LocalTarget()}})
	require.ErrorIs(t, err, errParentGone)
	assert.Zero(t, downloads)
	assert.NoDirExists(t, filepath.Dir(targetPath), "a parent the plan did not observe is never created")
}

// sessionTestFile writes a file large enough for a persisted session and
// returns its path, bytes and QuickXorHash.
func sessionTestFile(t *testing.T, dir string) (string, []byte, string) {
	t.Helper()

	data := bytes.Repeat([]byte("s"), persistedSessionMinSize+1)
	localPath := filepath.Join(dir, "big.bin")
	require.NoError(t, os.WriteFile(localPath, data, 0o600))

	return localPath, data, tmHashBytes(data)
}

// Validates: R-5.6.12
func TestSessionUpload_FinalFragmentFailureAdoptsCommittedItem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)

	var creates int
	ul := &tmMockUploader{
		createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
			creates++
			return &graph.UploadSession{UploadURL: "https://upload.example.com/s1", ExpirationTime: time.Now().Add(time.Hour)}, nil
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			// The commit outlasted the response timeout.
			return nil, fmt.Errorf("upload: %w", &graph.FinalFragmentError{Err: context.DeadlineExceeded})
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "landed", Name: "big.bin", Size: int64(len(data)), QuickXorHash: hash}}, nil
		},
	}

	result, err := newTestTM(&tmSimpleDownloader{}, ul, store).UploadFile(
		t.Context(), driveid.New("d1"), "parent1", "big.bin", localPath, UploadOpts{})

	// It used to fail, and the next attempt's resume got 404 and uploaded
	// the whole file again, possibly forever for a slow commit.
	require.NoError(t, err)
	assert.Equal(t, "landed", result.Item.ID)
	assert.Equal(t, 1, creates, "no second session")
	_, found, loadErr := store.Load(driveid.New("d1").String(), localPath)
	require.NoError(t, loadErr)
	assert.False(t, found, "the finished session's record is gone")
}

// Validates: R-5.6.12
func TestSessionUpload_GoneSessionWithLandedItemIsAdopted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)
	require.NoError(t, store.Save(driveid.New("d1").String(), localPath, &SessionRecord{
		SessionURL: "https://upload.example.com/old", FileHash: hash, FileSize: int64(len(data)),
		ExpiresAt: time.Now().Add(time.Hour),
	}))

	var creates int
	ul := &tmMockUploader{
		resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrUploadSessionExpired // a crash after the final fragment
		},
		createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
			creates++
			return &graph.UploadSession{UploadURL: "https://upload.example.com/new"}, nil
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "landed", Name: "big.bin", Size: int64(len(data)), QuickXorHash: hash}}, nil
		},
	}

	result, err := newTestTM(&tmSimpleDownloader{}, ul, store).UploadFile(
		t.Context(), driveid.New("d1"), "parent1", "big.bin", localPath, UploadOpts{})
	require.NoError(t, err)
	assert.Equal(t, "landed", result.Item.ID)
	assert.Zero(t, creates, "a committed upload is never sent again")
}

// Validates: R-5.6.12
func TestSessionUpload_GoneSessionBeforeExpiryIsInvalidation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())
	localPath, data, hash := sessionTestFile(t, dir)
	require.NoError(t, store.Save(driveid.New("d1").String(), existingItemSessionKey(localPath, "item-1"), &SessionRecord{
		SessionURL: "https://upload.example.com/old", FileHash: hash, FileSize: int64(len(data)),
		ExpiresAt: time.Now().Add(time.Hour),
	}))

	var creates int
	ul := &tmItemSessionUploader{tmMockUploader: tmMockUploader{
		resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, graph.ErrUploadSessionExpired
		},
		getItemFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.Item, error) {
			// Another writer changed the item: its bytes are not ours.
			return &graph.Item{ID: "item-1", Size: 7, QuickXorHash: tmHashBytes([]byte("theirs!"))}, nil
		},
	}, createForItem: func() { creates++ }}

	_, err := newTestTM(&tmSimpleDownloader{}, ul, store).UploadFileToItem(
		t.Context(), driveid.New("d1"), "item-1", localPath, UploadOpts{})

	// Live probe L-02: a session opened before another writer's change
	// answers 404. Restarting the upload overwrote that change.
	require.ErrorIs(t, err, ErrUploadSessionInvalidated)
	assert.Zero(t, creates, "no blind restart")
}

// Validates: R-5.6.13
func TestSessionUpload_DroppedSessionsAreCancelled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		recHash   func(hash string) string
		resumeErr error
	}{
		{name: "record for different content", recHash: func(string) string { return "stale-hash" }},
		{
			name:      "resume failure",
			recHash:   func(hash string) string { return hash },
			resumeErr: &graph.GraphError{StatusCode: 403, Message: "forbidden"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			store := NewSessionStore(dir, slog.Default())
			localPath, data, hash := sessionTestFile(t, dir)
			require.NoError(t, store.Save(driveid.New("d1").String(), localPath, &SessionRecord{
				SessionURL: "https://upload.example.com/old", FileHash: tt.recHash(hash), FileSize: int64(len(data)),
			}))

			ul := &tmMockUploader{
				resumeUploadFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
					return nil, tt.resumeErr
				},
				createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
					return &graph.UploadSession{UploadURL: "https://upload.example.com/new"}, nil
				},
				uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, content io.ReaderAt, size int64, _ graph.ProgressFunc) (*graph.Item, error) {
					return tmUploadedItem(t, "new", content, size), nil
				},
			}

			_, err := newTestTM(&tmSimpleDownloader{}, ul, store).UploadFile(
				t.Context(), driveid.New("d1"), "parent1", "big.bin", localPath, UploadOpts{})
			if tt.resumeErr == nil {
				require.NoError(t, err, "a fresh session follows the dropped one")
			}

			// Dropping only the local record leaked the server session,
			// holding quota and, on Business, blocking a new session.
			assert.Equal(t, []graph.UploadURL{"https://upload.example.com/old"}, ul.canceledSessions)
		})
	}
}

// Validates: R-5.6.12
func TestUploadFile_SmallFileFinalFragmentFailureAdoptsCommittedItem(t *testing.T) {
	t.Parallel()

	data := []byte("small file")
	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, &graph.FinalFragmentError{Err: io.ErrUnexpectedEOF}
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, _ string) ([]graph.Item, error) {
			return []graph.Item{{ID: "landed", Name: "a.txt", Size: int64(len(data)), QuickXorHash: tmHashBytes(data)}}, nil
		},
	}

	localPath := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(localPath, data, 0o600))

	result, err := newTestTM(&tmSimpleDownloader{}, ul, nil).UploadFile(
		t.Context(), driveid.New("d1"), "parent1", "a.txt", localPath, UploadOpts{})
	require.NoError(t, err)
	assert.Equal(t, "landed", result.Item.ID)
}

// tmItemSessionUploader adds the item-ID session methods to tmMockUploader.
type tmItemSessionUploader struct {
	tmMockUploader
	createForItem func()
}

func (m *tmItemSessionUploader) CreateUploadSessionForItem(
	_ context.Context, _ driveid.ID, _ string, _ int64, _ time.Time,
) (*graph.UploadSession, error) {
	if m.createForItem != nil {
		m.createForItem()
	}

	return &graph.UploadSession{UploadURL: "https://upload.example.com/new"}, nil
}

func (m *tmItemSessionUploader) UploadToItem(
	_ context.Context, _ driveid.ID, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc,
) (*graph.Item, error) {
	return nil, errors.New("UploadToItem not mocked")
}

// Validates: R-6.8.20
func TestDownloadToFile_StalledTransferKeepsPartialForResume(t *testing.T) {
	t.Parallel()

	dl := &tmSimpleDownloader{downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
		n, err := w.Write([]byte("first half"))
		if err != nil {
			return int64(n), err
		}

		return int64(n), fmt.Errorf("download: %w", graph.ErrTransferStalled)
	}}

	targetPath := filepath.Join(t.TempDir(), "big.bin")
	opts := DownloadOpts{RemoteETag: "v1", RemoteSize: 100}
	_, err := newTestTM(dl, &tmMockUploader{}, nil).DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)
	require.ErrorIs(t, err, graph.ErrTransferStalled)

	got, readErr := localpath.ReadFile(DownloadPartialPath(targetPath, opts))
	require.NoError(t, readErr, "the retry resumes from the bytes that arrived")
	assert.Equal(t, "first half", string(got))
}

// Validates: R-6.8.20, R-5.2.2
func TestTransferManager_StalledResumeKeepsPartial(t *testing.T) {
	t.Parallel()

	for _, rangeErr := range []error{
		fmt.Errorf("range download: %w", graph.ErrTransferStalled),
		fmt.Errorf("range download: %w", io.ErrUnexpectedEOF),
	} {
		t.Run(rangeErr.Error(), func(t *testing.T) {
			t.Parallel()

			var fresh int
			dl := &tmMockDownloader{
				downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer, _ int64) (int64, error) {
					n, err := w.Write([]byte(" more"))
					if err != nil {
						return int64(n), err
					}
					return int64(n), rangeErr
				},
				downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
					fresh++
					return 0, nil
				},
			}

			targetPath := filepath.Join(t.TempDir(), "big.bin")
			opts := DownloadOpts{RemoteETag: "v1", RemoteSize: 100}
			require.NoError(t, os.WriteFile(DownloadPartialPath(targetPath, opts), []byte("resumed"), 0o600))

			_, err := newTestTM(dl, &tmMockUploader{}, nil).DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, opts)

			// Starting over threw the resumed bytes away, so a link that
			// stalls before a file finishes never finished it.
			require.ErrorIs(t, err, rangeErr)
			assert.Zero(t, fresh, "no fresh download after a stalled resume")
			got, readErr := localpath.ReadFile(DownloadPartialPath(targetPath, opts))
			require.NoError(t, readErr)
			assert.Equal(t, "resumed more", string(got))
		})
	}
}
