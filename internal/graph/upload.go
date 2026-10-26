package graph

import (
	"log/slog"
	"time"
)

// ChunkAlignment is the required alignment for upload chunk sizes (320 KiB).
// All chunks except the final one must be a multiple of this value.
const ChunkAlignment = 320 * 1024

// ChunkedUploadChunkSize is the chunk size for resumable uploads (10 MiB, aligned to 320 KiB).
const ChunkedUploadChunkSize = 10 * 1024 * 1024

// ProgressFunc is a callback invoked after each chunk upload completes.
// bytesUploaded is cumulative; totalBytes is the full file size.
type ProgressFunc func(bytesUploaded, totalBytes int64)

// Upload session request/response types for Graph API JSON serialization.
type createUploadSessionRequest struct {
	Item uploadSessionItem `json:"item"`
}

type uploadSessionItem struct {
	ConflictBehavior string          `json:"@microsoft.graph.conflictBehavior"`
	FileSystemInfo   *fileSystemInfo `json:"fileSystemInfo,omitempty"`
}

// fileSystemInfo preserves local timestamps on upload, preventing OneDrive
// from overwriting them with server-side receipt time (double-versioning).
type fileSystemInfo struct {
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
}

// newFileSystemInfo is the fileSystemInfo to send for mtime: nil for the
// zero time, which OneDrive silently replaces with the current time, and
// clamped to EarliestFileSystemTime, before which it answers a misleading
// 404 itemNotFound (live probe L-26). The relabelling of that 404 stays as
// a defense for any other sender.
func (c *Client) newFileSystemInfo(mtime time.Time) *fileSystemInfo {
	if mtime.IsZero() {
		return nil
	}

	return &fileSystemInfo{LastModifiedDateTime: c.sendableMtime(mtime).UTC().Format(time.RFC3339)}
}

// sendableMtime clamps mtime to EarliestFileSystemTime, warning once per
// client.
func (c *Client) sendableMtime(mtime time.Time) time.Time {
	if !mtime.Before(EarliestFileSystemTime()) {
		return mtime
	}

	logAt := c.logger.Debug
	if c.clampWarned.CompareAndSwap(false, true) {
		logAt = c.logger.Warn
	}
	logAt("OneDrive doesn't accept modification times before 1900; using 1900-01-01 instead",
		slog.Time("mtime", mtime),
	)

	return EarliestFileSystemTime()
}

type uploadSessionResponse struct {
	UploadURL          string `json:"uploadUrl"`
	ExpirationDateTime string `json:"expirationDateTime"`
}

// uploadSessionStatusResponse is the JSON shape returned when querying an upload session.
type uploadSessionStatusResponse struct {
	UploadURL          string   `json:"uploadUrl"`
	ExpirationDateTime string   `json:"expirationDateTime"`
	NextExpectedRanges []string `json:"nextExpectedRanges"`
}
