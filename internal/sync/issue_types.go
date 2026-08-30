// Package sync defines sync-domain issue type constants shared across the
// observer, engine, store, and CLI layers.
package sync

// Issue type constants for sync conditions. Scanner-detectable issues
// (invalid_filename, path_too_long, file_too_large) are caught at
// observation time by shouldObserve / processEntry. Runtime issues
// are detected during execution by the engine.
const (
	// Scanner-detectable (observation-time).
	IssueInvalidFilename = "invalid_filename"
	issuePathTooLong     = "path_too_long"
	issueFileTooLarge    = "file_too_large"

	// Scanner-detectable (hash phase).
	issueHashPanic = "hash_panic"

	// Runtime (execution-time).
	IssueUnauthorized         = "unauthorized"
	IssueRemoteWriteDenied    = "remote_write_denied"
	issueRemoteReadDenied     = "remote_read_denied"
	IssueQuotaExceeded        = "quota_exceeded"
	issueRateLimited          = "rate_limited"
	issueLocalReadDenied      = "local_read_denied"
	issueLocalWriteDenied     = "local_permission_denied"
	issueCaseCollision        = "case_collision"
	issueDiskFull             = "disk_full"
	issueServiceOutage        = "service_outage"
	issueFileTooLargeForSpace = "file_too_large_for_space"
)
