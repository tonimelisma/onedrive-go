package graph

// ChunkAlignment is the required alignment for upload chunk sizes (320 KiB).
// All chunks except the final one must be a multiple of this value.
const ChunkAlignment = 320 * 1024

// SimpleUploadMaxSize is the maximum file size for simple (single-request) upload (4 MiB).
// Files larger than this must use resumable upload sessions.
const SimpleUploadMaxSize = 4 * 1024 * 1024

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
