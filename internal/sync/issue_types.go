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
	IssueUnauthorized      = "unauthorized"
	IssueRemoteWriteDenied = "remote_write_denied"
	issueRemoteReadDenied  = "remote_read_denied"
	IssueQuotaExceeded     = "quota_exceeded"
	issueRateLimited       = "rate_limited"
	issueLocalReadDenied   = "local_read_denied"
	issueLocalWriteDenied  = "local_permission_denied"
	issueCaseCollision     = "case_collision"
	// issueRemoteNameTwin marks a path where OneDrive keeps two or more
	// items that this computer stores under one name (store_remote_twins.go).
	issueRemoteNameTwin       = "remote_name_twin"
	issueDiskFull             = "disk_full"
	issueServiceOutage        = "service_outage"
	issueFileTooLargeForSpace = "file_too_large_for_space"
	issueLocalNameTooLong     = "local_name_too_long"
	issueLocalReadOnly        = "local_read_only"
	// issueDeletesHeld marks deletes the delete-safety hold kept for
	// confirmation (planner_delete_safety.go).
	issueDeletesHeld = "deletes_held"
	// issueLocalUnsyncableReplacement marks a synced path whose item is now
	// something sync cannot represent: replaced by a symlink or special file,
	// or renamed to an invalid name or an excluded location. The synced path
	// is unavailable truth, never a delete (D-004, P-5).
	issueLocalUnsyncableReplacement = "local_unsyncable_replacement"
	// issueLocalMountBoundary marks a folder on another filesystem inside the
	// sync folder; sync does not cross into it (D-003, P-11).
	issueLocalMountBoundary   = "local_mount_boundary"
	issueLocalFileTooLarge    = "local_file_too_large"
	issueLocalBusy            = "local_busy"
	issueLocalCrossDevice     = "local_cross_device"
	issueUnsupportedOperation = "unsupported_operation"
	issueMalwareDetected      = "malware_detected"
	issueLocalDatabaseOpen    = "local_database_open"
	issueUploadVerification   = "upload_verification_failed"
	issueDownloadIntegrity    = "download_integrity"
	// issueTransferStalled shares the request-timeout condition family.
	issueTransferStalled = "request_timeout"
)
