package syncfixture

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

type ExecutorMockItemClient = TestMockItemClient

type ExecutorMockDownloader struct {
	DownloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

func (m *ExecutorMockDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.DownloadFn != nil {
		return m.DownloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

type ExecutorMockUploader struct {
	// rawResponse returns uploadFn's item as is instead of a faithful one,
	// to model a server that stored something other than what was sent.
	RawResponse    bool
	UploadFn       func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	UploadToItemFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *ExecutorMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.UploadFn != nil && m.RawResponse {
		return m.UploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}
	if m.UploadFn != nil {
		return faithfulUploadResult(content, size)(m.UploadFn(ctx, driveID, parentID, name, content, size, mtime, progress))
	}

	return nil, fmt.Errorf("Upload not mocked")
}

func (m *ExecutorMockUploader) UploadToItem(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.UploadToItemFn != nil {
		return faithfulUploadResult(content, size)(m.UploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress))
	}
	if m.UploadFn != nil {
		return faithfulUploadResult(content, size)(m.UploadFn(ctx, driveID, "", "", content, size, mtime, progress))
	}

	return nil, fmt.Errorf("UploadToItem not mocked")
}

// faithfulUploadResult makes a mocked upload answer like a server that
// stored exactly the bytes it was sent: the returned item carries their size
// and QuickXorHash, as driveops verifies the stored item against them.
func faithfulUploadResult(content io.ReaderAt, size int64) func(*graph.Item, error) (*graph.Item, error) {
	return func(item *graph.Item, err error) (*graph.Item, error) {
		if err != nil || item == nil {
			return item, err
		}

		data := make([]byte, size)
		if _, readErr := content.ReadAt(data, 0); readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("reading mocked upload content: %w", readErr)
		}

		h := quickxorhash.New()
		h.Write(data)
		stored := *item
		stored.Size = size
		stored.QuickXorHash = base64.StdEncoding.EncodeToString(h.Sum(nil))

		return &stored, nil
	}
}

func WriteExecTestFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	absPath := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o700))
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o600))

	return absPath
}

func RequireOutcomeSuccess(t *testing.T, o *syncdomain.ActionOutcome) {
	t.Helper()

	require.True(t, o.Success, "expected success but got error: %v", o.Error)
}

func RequireOutcomeFailure(t *testing.T, o *syncdomain.ActionOutcome) {
	t.Helper()

	require.False(t, o.Success, "expected failure but got success")
}
