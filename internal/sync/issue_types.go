// issue_types.go — Cross-cutting issue type constants and validation limits.
//
// These constants are used by the scanner (observation-time filtering),
// the engine (failure recording), the baseline (failure queries), and
// the CLI (issues display). Extracted from upload_validation.go to
// reflect their cross-cutting nature.
package sync

// Issue type constants for sync failures. Scanner-detectable issues
// (invalid_filename, path_too_long, file_too_large) are caught at
// observation time by shouldObserve / processEntry. Runtime issues
// are detected during execution by the engine.
const (
	// Scanner-detectable (observation-time).
	IssueInvalidFilename = "invalid_filename"
	IssuePathTooLong     = "path_too_long"
	IssueFileTooLarge    = "file_too_large"

	// Runtime (execution-time).
	IssuePermissionDenied      = "permission_denied"
	IssueQuotaExceeded         = "quota_exceeded"
	IssueLocalPermissionDenied = "local_permission_denied"
	IssueCaseCollision         = "case_collision"
	IssueDiskFull              = "disk_full"
	IssueServiceOutage         = "service_outage"
	IssueFileTooLargeForSpace  = "file_too_large_for_space"
)

// OneDrive validation limits. Both are direction-independent — OneDrive
// enforces them server-side, so files exceeding these limits cannot exist
// remotely either.
const (
	// maxOneDrivePathLength is the maximum total path length OneDrive allows.
	maxOneDrivePathLength = 400
	// maxOneDriveFileSize is the maximum file size OneDrive allows (250 GB).
	maxOneDriveFileSize = 250 * 1024 * 1024 * 1024 // 250 GB
)
