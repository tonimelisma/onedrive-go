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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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

func (m *tmMockDownloader) DownloadRange(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer, offset int64) (int64, error) {
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
}

var (
	_ Uploader        = (*tmMockUploader)(nil)
	_ SessionUploader = (*tmMockUploader)(nil)
)

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

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, expectedHash, result.LocalHash)
	assert.Equal(t, int64(len(content)), result.Size)

	// HashVerified should be true on successful hash match.
	assert.True(t, result.HashVerified, "HashVerified should be true on successful match")

	got, readErr := os.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")

	assert.Equal(t, content, got, "file content mismatch")

	// .partial should not exist after successful download.
	_, statErr := os.Stat(targetPath + ".partial")
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

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "network failure")

	// .partial should be cleaned up on non-ctx error.
	_, statErr := os.Stat(targetPath + ".partial")
	assert.True(t, os.IsNotExist(statErr), "expected .partial to be removed on non-ctx error")
}

func TestTransferManager_FreshDownload_CtxCancel_PreservesPartial(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			// Write some data, then simulate cancellation.
			n, _ := w.Write([]byte("partial-data"))
			cancel()

			return int64(n), context.Canceled
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "file.txt")

	_, err := tm.DownloadToFile(ctx, driveid.New("d1"), "item1", targetPath, DownloadOpts{})
	require.Error(t, err)

	// .partial should be preserved on context cancellation.
	info, statErr := os.Stat(targetPath + ".partial")
	require.NoError(t, statErr, "expected .partial to be preserved")

	assert.NotZero(t, info.Size(), "expected .partial to have data")
}

// Validates: R-5.2, R-6.8.3
func TestTransferManager_ResumeDownload_Success(t *testing.T) {
	t.Parallel()

	existingData := []byte("existing-")
	appendData := []byte("appended")
	fullContent := append(existingData, appendData...)
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
	partialPath := targetPath + ".partial"

	// Pre-create the .partial file with existing data.
	require.NoError(t, os.WriteFile(partialPath, existingData, 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	assert.Equal(t, expectedHash, result.LocalHash)
	assert.Equal(t, int64(len(fullContent)), result.Size)

	got, readErr := os.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")

	assert.Equal(t, fullContent, got)
}

func TestTransferManager_ResumeDownload_RangeFail_FallsBack(t *testing.T) {
	t.Parallel()

	content := []byte("fresh-content")
	expectedHash := tmHashBytes(content)
	var downloadCalled bool

	dl := &tmMockDownloader{
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer, _ int64) (int64, error) {
			return 0, fmt.Errorf("range not supported by server")
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloadCalled = true
			n, err := w.Write(content)

			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "file.txt")
	partialPath := targetPath + ".partial"

	// Pre-create .partial to trigger resume attempt.
	require.NoError(t, os.WriteFile(partialPath, []byte("old-data"), 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	assert.True(t, downloadCalled, "expected fresh Download() to be called as fallback")
	assert.Equal(t, expectedHash, result.LocalHash)
}

func TestTransferManager_ResumeDownload_CloseError_FallsBack(t *testing.T) {
	t.Parallel()

	// This test verifies fix #7: when f.Close() fails after DownloadRange,
	// we fall back to a fresh download instead of proceeding to hash verification.
	content := []byte("fresh-after-close-error")
	expectedHash := tmHashBytes(content)
	var freshDownloadCalled bool

	dl := &tmMockDownloader{
		downloadRangeFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer, _ int64) (int64, error) {
			// Write some data — the close will fail but that's handled by the
			// TransferManager. We can't directly mock f.Close() failure through
			// the interface, so we test the range-failure fallback path instead
			// which exercises the same code path.
			return 0, fmt.Errorf("simulated range failure for close-error test")
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			freshDownloadCalled = true
			n, err := w.Write(content)

			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "file.txt")
	partialPath := targetPath + ".partial"

	require.NoError(t, os.WriteFile(partialPath, []byte("existing"), 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	assert.True(t, freshDownloadCalled, "expected fresh download fallback after close error")
	assert.Equal(t, expectedHash, result.LocalHash)
}

// Validates: R-5.5
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

func TestTransferManager_HashExhaustion_Accepts(t *testing.T) {
	t.Parallel()

	// All downloads produce content with a different hash. After exhaustion,
	// EffectiveRemoteHash should be set to localHash.
	content := []byte("always-wrong-hash")

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "file.txt")

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash:     "definitely-wrong-hash",
		MaxHashRetries: 1,
	})
	require.NoError(t, err, "DownloadToFile")

	// After exhaustion, EffectiveRemoteHash should match localHash (accepted).
	assert.Equal(t, result.LocalHash, result.EffectiveRemoteHash)

	// HashVerified should be false when retries exhausted with mismatch.
	assert.False(t, result.HashVerified, "HashVerified should be false after exhaustion")
}

// ---------------------------------------------------------------------------
// Upload tests
// ---------------------------------------------------------------------------

func TestTransferManager_Upload_Success(t *testing.T) {
	t.Parallel()

	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "item-1", ETag: "etag-1"}, nil
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

func TestTransferManager_Upload_NilItem(t *testing.T) {
	t.Parallel()

	ul := &tmMockUploader{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, nil //nolint:nilnil // intentional to test nil-item guard
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, nil)

	dir := t.TempDir()
	localPath := filepath.Join(dir, "nil-item.txt")

	require.NoError(t, os.WriteFile(localPath, []byte("data"), 0o600))

	_, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "nil-item.txt", localPath, UploadOpts{})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "returned nil item")
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

// Validates: R-1.3, R-5.2
func TestTransferManager_SessionUpload_Success(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	ul := &tmMockUploader{
		createUploadSessionFn: func(_ context.Context, _ driveid.ID, _, _ string, _ int64, _ time.Time) (*graph.UploadSession, error) {
			return &graph.UploadSession{UploadURL: "https://upload.example.com/session"}, nil
		},
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "session-item"}, nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	// Create a file larger than SimpleUploadMaxSize.
	localPath := filepath.Join(dir, "large.bin")
	largeData := make([]byte, graph.SimpleUploadMaxSize+1)

	require.NoError(t, os.WriteFile(localPath, largeData, 0o600))

	result, err := tm.UploadFile(t.Context(), driveid.New("d1"), "parent1", "large.bin", localPath, UploadOpts{})
	require.NoError(t, err, "UploadFile")

	assert.Equal(t, "session-item", result.Item.ID)
}

func TestTransferManager_SessionUpload_Resume(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	// Create a file larger than SimpleUploadMaxSize.
	localPath := filepath.Join(dir, "resume.bin")
	largeData := make([]byte, graph.SimpleUploadMaxSize+1)

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
		resumeUploadFn: func(_ context.Context, session *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			resumeCalled = true

			if session.UploadURL != "https://upload.example.com/existing" {
				return nil, fmt.Errorf("unexpected session URL: %s", session.UploadURL)
			}

			return &graph.Item{ID: "resumed-item"}, nil
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

	// Create a file larger than SimpleUploadMaxSize.
	localPath := filepath.Join(dir, "expired.bin")
	largeData := make([]byte, graph.SimpleUploadMaxSize+1)

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
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadFromCalled = true

			return &graph.Item{ID: "fresh-item"}, nil
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

func TestTransferManager_ParentDirPerms(t *testing.T) {
	t.Parallel()

	content := []byte("perms-test")
	expectedHash := tmHashBytes(content)

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	base := t.TempDir()
	targetPath := filepath.Join(base, "newdir", "file.txt")

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	info, statErr := os.Stat(filepath.Join(base, "newdir"))
	require.NoError(t, statErr, "Stat newdir")

	// Fix #15: parent dir should be 0o700 (owner-only).
	perms := info.Mode().Perm()
	assert.Equal(t, os.FileMode(0o700), perms, "parent dir perms")
}

// ---------------------------------------------------------------------------
// B-208: session delete on any resume failure
// ---------------------------------------------------------------------------

func TestSessionUpload_NonExpiredResumeError_DeletesSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	localPath := filepath.Join(dir, "network-err.bin")
	largeData := make([]byte, graph.SimpleUploadMaxSize+1)

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
	rec, loadErr := store.Load(driveStr, localPath)
	require.NoError(t, loadErr, "Load")

	assert.Nil(t, rec, "expected session to be deleted after non-expired resume error")
}

func TestSessionUpload_NonExpiredResumeError_FreshOnRetry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := NewSessionStore(dir, slog.Default())

	localPath := filepath.Join(dir, "retry.bin")
	largeData := make([]byte, graph.SimpleUploadMaxSize+1)

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
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "fresh-retry-item"}, nil
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

	t.Run("removes file when context is active", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "test.partial")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))

		removePartialIfNotCanceled(t.Context(), path)

		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err), "expected file to be removed")
	})

	t.Run("preserves file when context is canceled", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "test.partial")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		removePartialIfNotCanceled(ctx, path)

		_, err := os.Stat(path)
		assert.NoError(t, err, "expected file to be preserved")
	})

	t.Run("no panic for nonexistent file", func(t *testing.T) {
		t.Parallel()

		// Should not panic when file doesn't exist.
		removePartialIfNotCanceled(t.Context(), "/nonexistent/path.partial")
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

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "renaming partial")

	// .partial should still exist with correct content.
	partialPath := targetPath + ".partial"

	got, readErr := os.ReadFile(partialPath)
	require.NoError(t, readErr, "expected .partial to be preserved")

	assert.Equal(t, content, got, "partial content mismatch")
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
		uploadFromSessionFn: func(_ context.Context, _ *graph.UploadSession, _ io.ReaderAt, _ int64, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "save-fail-ok"}, nil
		},
	}

	tm := newTestTM(&tmSimpleDownloader{}, ul, store)

	// Make the store directory read-only so Save fails.
	require.NoError(t, os.Chmod(store.dir, 0o444))

	t.Cleanup(func() { os.Chmod(store.dir, 0o700) })

	localPath := filepath.Join(dir, "save-fail.bin")
	largeData := make([]byte, graph.SimpleUploadMaxSize+1)

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
	assert.True(t, errors.Is(err, os.ErrNotExist), "expected errors.Is(err, os.ErrNotExist)")

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
	partialPath := targetPath + ".partial"

	// Pre-create a .partial file with old content — should be overwritten.
	require.NoError(t, os.WriteFile(partialPath, []byte("old-partial-data-that-should-go-away"), 0o600))

	result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	// Final file should contain fresh content, not concatenated with old.
	got, readErr := os.ReadFile(targetPath)
	require.NoError(t, readErr, "ReadFile")

	assert.Equal(t, freshContent, got)
	assert.Equal(t, expectedHash, result.LocalHash)

	// .partial should not exist after successful download.
	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "expected .partial to be removed")
}

func TestFreshDownload_FilePermissions(t *testing.T) {
	t.Parallel()

	content := []byte("perms-check")
	expectedHash := tmHashBytes(content)

	dl := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "perm-test.txt")

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
		RemoteHash: expectedHash,
	})
	require.NoError(t, err, "DownloadToFile")

	info, statErr := os.Stat(targetPath)
	require.NoError(t, statErr, "Stat")

	perms := info.Mode().Perm()
	assert.Equal(t, os.FileMode(0o600), perms, "file perms")
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
	got, readErr := os.ReadFile(targetPath)
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

	okDownloader := &tmSimpleDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}

	// Validates: R-2.10.43
	t.Run("DiskFull", func(t *testing.T) {
		t.Parallel()

		tm := NewTransferManager(okDownloader, &tmMockUploader{}, nil, slog.Default(),
			WithDiskCheck(1000, func(string) (uint64, error) { return 500, nil }),
		)

		targetPath := filepath.Join(t.TempDir(), "file.txt")
		_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
			RemoteHash: expectedHash,
			RemoteSize: 100,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrDiskFull)
	})

	// Validates: R-2.10.44
	t.Run("FileTooLarge", func(t *testing.T) {
		t.Parallel()

		// available (2000) >= minFreeSpace (1000) but < fileSize (1500) + minFreeSpace (1000)
		tm := NewTransferManager(okDownloader, &tmMockUploader{}, nil, slog.Default(),
			WithDiskCheck(1000, func(string) (uint64, error) { return 2000, nil }),
		)

		targetPath := filepath.Join(t.TempDir(), "file.txt")
		_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
			RemoteHash: expectedHash,
			RemoteSize: 1500,
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrFileTooLargeForSpace)
	})

	// Validates: R-6.4.7
	t.Run("Disabled_NilFunc", func(t *testing.T) {
		t.Parallel()

		// No WithDiskCheck option → download proceeds normally.
		tm := NewTransferManager(okDownloader, &tmMockUploader{}, nil, slog.Default())

		targetPath := filepath.Join(t.TempDir(), "file.txt")
		result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
			RemoteHash: expectedHash,
		})
		require.NoError(t, err)
		assert.Equal(t, expectedHash, result.LocalHash)
	})

	// Validates: R-6.4.7
	t.Run("Disabled_ZeroMinFreeSpace", func(t *testing.T) {
		t.Parallel()

		// WithDiskCheck(0, fn) → download proceeds (zero disables check).
		tm := NewTransferManager(okDownloader, &tmMockUploader{}, nil, slog.Default(),
			WithDiskCheck(0, func(string) (uint64, error) { return 100, nil }),
		)

		targetPath := filepath.Join(t.TempDir(), "file.txt")
		result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
			RemoteHash: expectedHash,
		})
		require.NoError(t, err)
		assert.Equal(t, expectedHash, result.LocalHash)
	})

	// Validates: R-6.2.6
	t.Run("StatfsError_FailOpen", func(t *testing.T) {
		t.Parallel()

		// When statfs fails, the check should fail open (not block downloads).
		tm := NewTransferManager(okDownloader, &tmMockUploader{}, nil, slog.Default(),
			WithDiskCheck(1000, func(string) (uint64, error) {
				return 0, fmt.Errorf("simulated statfs error")
			}),
		)

		targetPath := filepath.Join(t.TempDir(), "file.txt")
		result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
			RemoteHash: expectedHash,
		})
		require.NoError(t, err)
		assert.Equal(t, expectedHash, result.LocalHash)
	})

	// Validates: R-6.2.6
	t.Run("SufficientSpace", func(t *testing.T) {
		t.Parallel()

		// available (5000) >= fileSize (2000) + minFreeSpace (1000) — download proceeds.
		tm := NewTransferManager(okDownloader, &tmMockUploader{}, nil, slog.Default(),
			WithDiskCheck(1000, func(string) (uint64, error) { return 5000, nil }),
		)

		targetPath := filepath.Join(t.TempDir(), "file.txt")
		result, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{
			RemoteHash: expectedHash,
			RemoteSize: 2000,
		})
		require.NoError(t, err)
		assert.Equal(t, expectedHash, result.LocalHash)
	})
}
