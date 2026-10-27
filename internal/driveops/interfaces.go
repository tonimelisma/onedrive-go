package driveops

import (
	"context"
	"io"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// The Graph client must keep satisfying the upload seams: they are
// type-asserted at runtime, so a signature drift would otherwise silently
// fall back to another route instead of failing to compile.
var (
	_ Uploader            = (*graph.Client)(nil)
	_ ItemUploader        = (*graph.Client)(nil)
	_ SessionUploader     = (*graph.Client)(nil)
	_ ItemSessionUploader = (*graph.Client)(nil)
	_ CreatingUploader    = (*graph.Client)(nil)
	_ PlaceholderRemover  = (*graph.Client)(nil)
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

// CreatingUploader creates a new file and uploads its content, calling
// onCreated after the empty item exists and before any content is sent.
// Satisfied by *graph.Client; type-asserted like the other optional seams.
type CreatingUploader interface {
	UploadCreating(
		ctx context.Context, driveID driveid.ID, parentID, name string,
		content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc, onCreated graph.CreatedFunc,
	) (*graph.Item, error)
}

// PlaceholderRemover deletes an item conditionally. Satisfied by
// *graph.Client.
type PlaceholderRemover interface {
	DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
}

// ItemUploader overwrites an existing item by item ID. A non-empty ifMatch
// makes the overwrite conditional on that eTag (412 otherwise).
type ItemUploader interface {
	UploadToItem(
		ctx context.Context, driveID driveid.ID, itemID, ifMatch string,
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
	sessionTransport
}

// ItemSessionUploader provides session-based overwrite methods for existing
// items identified by item ID.
type ItemSessionUploader interface {
	CreateUploadSessionForItem(
		ctx context.Context, driveID driveid.ID, itemID, ifMatch string,
		size int64, mtime time.Time,
	) (*graph.UploadSession, error)
	sessionTransport
}

// RangeDownloader downloads a file starting from a byte offset, bound to the
// remote version the partial holds. Satisfied by *graph.Client. Type-asserted
// at runtime to avoid breaking the Downloader interface (B-085).
type RangeDownloader interface {
	DownloadRange(
		ctx context.Context, driveID driveid.ID, itemID string,
		w io.Writer, offset int64, version graph.DownloadVersion,
	) (int64, error)
}

// PathConvergence owns post-success path settling for one mounted drive/root
// session. Callers use it after successful mutations when Graph can lag on
// follow-on path reads or path-authoritative delete routes.
type PathConvergence interface {
	WaitPathVisible(ctx context.Context, remotePath string) (*graph.Item, error)
	DeleteResolvedPath(ctx context.Context, remotePath, itemID string) error
	PermanentDeleteResolvedPath(ctx context.Context, remotePath, itemID string) error
}
