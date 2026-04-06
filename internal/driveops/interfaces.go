package driveops

import (
	"context"
	"io"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Downloader streams a remote file by item ID.
type Downloader interface {
	Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)
}

// Uploader uploads a local file, encapsulating the simple-vs-chunked decision
// and upload session lifecycle. content must be an io.ReaderAt for retry safety.
type Uploader interface {
	Upload(
		ctx context.Context, driveID driveid.ID, parentID, name string,
		content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error)
}

// ItemUploader overwrites an existing item by item ID.
type ItemUploader interface {
	UploadToItem(
		ctx context.Context, driveID driveid.ID, itemID string,
		content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc,
	) (*graph.Item, error)
}

// SessionUploader provides session-based upload methods for resumable transfers.
// Satisfied by *graph.Client. Type-asserted at runtime to avoid breaking the
// Uploader interface. When available alongside a SessionStore, the executor
// uses session-based uploads for large files and persists session state for
// cross-crash resume.
type SessionUploader interface {
	CreateUploadSession(
		ctx context.Context, driveID driveid.ID, parentID, name string,
		size int64, mtime time.Time,
	) (*graph.UploadSession, error)
	UploadFromSession(
		ctx context.Context, session *graph.UploadSession,
		content io.ReaderAt, totalSize int64, progress graph.ProgressFunc,
	) (*graph.Item, error)
	ResumeUpload(
		ctx context.Context, session *graph.UploadSession,
		content io.ReaderAt, totalSize int64, progress graph.ProgressFunc,
	) (*graph.Item, error)
}

// ItemSessionUploader provides session-based overwrite methods for existing
// items identified by item ID.
type ItemSessionUploader interface {
	CreateUploadSessionForItem(
		ctx context.Context, driveID driveid.ID, itemID string,
		size int64, mtime time.Time,
	) (*graph.UploadSession, error)
	UploadFromSession(
		ctx context.Context, session *graph.UploadSession,
		content io.ReaderAt, totalSize int64, progress graph.ProgressFunc,
	) (*graph.Item, error)
	ResumeUpload(
		ctx context.Context, session *graph.UploadSession,
		content io.ReaderAt, totalSize int64, progress graph.ProgressFunc,
	) (*graph.Item, error)
}

// RangeDownloader downloads a file starting from a byte offset. Satisfied by
// *graph.Client. Type-asserted at runtime to avoid breaking the Downloader
// interface (B-085).
type RangeDownloader interface {
	DownloadRange(
		ctx context.Context, driveID driveid.ID, itemID string,
		w io.Writer, offset int64,
	) (int64, error)
}

// PathConvergence owns post-success path settling for one resolved drive
// session. Callers use it after successful mutations when Graph can lag on
// follow-on path reads or path-authoritative delete routes.
type PathConvergence interface {
	WaitPathVisible(ctx context.Context, remotePath string) (*graph.Item, error)
	DeleteResolvedPath(ctx context.Context, remotePath, itemID string) error
	PermanentDeleteResolvedPath(ctx context.Context, remotePath, itemID string) error
}
