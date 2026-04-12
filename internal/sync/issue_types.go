// Package sync defines sync-domain issue type constants shared across the
// observer, engine, store, and CLI layers.
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

	// Delete safety threshold protection.
	IssueDeleteSafetyHeld = "delete_safety_held"

	// Runtime (execution-time).
	IssueUnauthorized          = "unauthorized"
	IssuePermissionDenied      = "permission_denied"
	IssueSharedFolderBlocked   = "shared_folder_write_blocked"
	IssueQuotaExceeded         = "quota_exceeded"
	IssueRateLimited           = "rate_limited"
	IssueLocalPermissionDenied = "local_permission_denied"
	IssueCaseCollision         = "case_collision"
	IssueDiskFull              = "disk_full"
	IssueServiceOutage         = "service_outage"
	IssueFileTooLargeForSpace  = "file_too_large_for_space"
)
