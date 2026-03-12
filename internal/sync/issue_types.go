// issue_types.go — Cross-cutting issue type constants.
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

	// Scanner-detectable (hash phase).
	IssueHashPanic = "hash_panic"

	// Big-delete protection (watch-mode).
	IssueBigDeleteHeld = "big_delete_held"

	// Runtime (execution-time).
	IssuePermissionDenied      = "permission_denied"
	IssueQuotaExceeded         = "quota_exceeded"
	IssueLocalPermissionDenied = "local_permission_denied"
	IssueCaseCollision         = "case_collision"
	IssueDiskFull              = "disk_full"
	IssueServiceOutage         = "service_outage"
	IssueFileTooLargeForSpace  = "file_too_large_for_space"
)
