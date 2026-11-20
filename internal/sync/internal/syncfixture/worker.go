package syncfixture

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type WorkerMockItemClient = TestMockItemClient

type WorkerMockDownloader struct {
	DownloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

func (m *WorkerMockDownloader) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.DownloadFn != nil {
		return m.DownloadFn(ctx, driveID, itemID, w)
	}

	return 0, fmt.Errorf("Download not mocked")
}

type WorkerMockUploader struct {
	UploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *WorkerMockUploader) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.UploadFn != nil {
		return faithfulUploadResult(content, size)(m.UploadFn(ctx, driveID, parentID, name, content, size, mtime, progress))
	}

	return nil, fmt.Errorf("Upload not mocked")
}
