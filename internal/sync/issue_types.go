// Package sync defines sync-domain issue type constants shared across the
// observer, engine, store, and CLI layers.
package sync

import "fmt"

// IssueType names one sync condition: the issue_type of an observation issue,
// the condition_type of a retry_work row, a scope's default condition, and the
// condition of a runtime result decision. A member's value is the string the
// stores persist. The stores convert at their own boundary: a write refuses a
// value that is not a member and a read parses the stored string
// (parseIssueType), so whatever a store hands back is a member, or empty where
// a field may hold no condition. Elsewhere a value becomes a plain string only
// on its way out, as a log attribute or in CLI output.
type IssueType string

// Issue type constants for sync conditions. Scanner-detectable issues
// (invalid_filename, path_too_long, file_too_large) are caught at
// observation time by shouldObserve / processEntry. Runtime issues
// are detected during execution by the engine. A new member must be added to
// isMember and given a verdict in observationIssueBlocksLocalTruth; the
// exhaustive linter fails until both name it.
const (
	// Scanner-detectable (observation-time).
	IssueInvalidFilename IssueType = "invalid_filename"
	issuePathTooLong     IssueType = "path_too_long"
	issueFileTooLarge    IssueType = "file_too_large"

	// Scanner-detectable (hash phase).
	issueHashPanic IssueType = "hash_panic"

	// Runtime (execution-time).
	IssueUnauthorized      IssueType = "unauthorized"
	IssueRemoteWriteDenied IssueType = "remote_write_denied"
	issueRemoteReadDenied  IssueType = "remote_read_denied"
	IssueQuotaExceeded     IssueType = "quota_exceeded"
	issueRateLimited       IssueType = "rate_limited"
	issueLocalReadDenied   IssueType = "local_read_denied"
	issueLocalWriteDenied  IssueType = "local_permission_denied"
	issueCaseCollision     IssueType = "case_collision"
	// issueRemoteNameTwin marks a path where OneDrive keeps two or more
	// items that this computer stores under one name (store_remote_twins.go).
	issueRemoteNameTwin       IssueType = "remote_name_twin"
	issueDiskFull             IssueType = "disk_full"
	issueServiceOutage        IssueType = "service_outage"
	issueFileTooLargeForSpace IssueType = "file_too_large_for_space"
	issueLocalNameTooLong     IssueType = "local_name_too_long"
	issueLocalReadOnly        IssueType = "local_read_only"
	// issueDeletesHeld marks deletes the delete-safety hold kept for
	// confirmation (planner_delete_safety.go).
	issueDeletesHeld IssueType = "deletes_held"
	// issueLocalUnsyncableReplacement marks a synced path whose item is now
	// something sync cannot represent: replaced by a symlink or special file,
	// or renamed to an invalid name or an excluded location. The synced path
	// is unavailable truth, never a delete (D-004, P-5).
	issueLocalUnsyncableReplacement IssueType = "local_unsyncable_replacement"
	// issueLocalMountBoundary marks a folder on another filesystem inside the
	// sync folder; sync does not cross into it (D-003, P-11).
	issueLocalMountBoundary IssueType = "local_mount_boundary"
	// issueRemoteFolderUnverifiable marks a folder delete on OneDrive that
	// sync will not run because the folder is too large to list first.
	issueRemoteFolderUnverifiable IssueType = "remote_folder_delete_unverifiable"
	// issueLocalInterruptedRemoval marks a file sync set aside while removing
	// it and never moved out, because a run was interrupted. It is the
	// user's content, so it is reported, never deleted.
	issueLocalInterruptedRemoval IssueType = "local_interrupted_removal"
	issueLocalFileTooLarge       IssueType = "local_file_too_large"
	issueLocalBusy               IssueType = "local_busy"
	issueLocalCrossDevice        IssueType = "local_cross_device"
	issueUnsupportedOperation    IssueType = "unsupported_operation"
	issueMalwareDetected         IssueType = "malware_detected"
	issueLocalDatabaseOpen       IssueType = "local_database_open"
	issueUploadVerification      IssueType = "upload_verification_failed"
	issueDownloadIntegrity       IssueType = "download_integrity"

	// Runtime transient (execution-time). These name a retryable failure in
	// the result decision, its retry delay and its log lines; no store keeps
	// them, because only an actionable condition is persisted.
	// issueRequestTimeout also covers a transfer the stall watchdog abandoned.
	issueRequestTimeout      IssueType = "request_timeout"
	issueTransientConflict   IssueType = "transient_conflict"
	issueTransientNotFound   IssueType = "transient_not_found"
	issueResourceLocked      IssueType = "resource_locked"
	issueTransferURLRejected IssueType = "transfer_url_rejected"
)

// isMember reports whether t is one of the constants above. The exhaustive
// linter holds the switch to every member, so this is the list the store
// boundary trusts. The empty value is not a member: it means "no condition"
// where a field may hold none, and each store says whether it may.
func (t IssueType) isMember() bool {
	switch t {
	case IssueInvalidFilename,
		issuePathTooLong,
		issueFileTooLarge,
		issueHashPanic,
		IssueUnauthorized,
		IssueRemoteWriteDenied,
		issueRemoteReadDenied,
		IssueQuotaExceeded,
		issueRateLimited,
		issueLocalReadDenied,
		issueLocalWriteDenied,
		issueCaseCollision,
		issueRemoteNameTwin,
		issueDiskFull,
		issueServiceOutage,
		issueFileTooLargeForSpace,
		issueLocalNameTooLong,
		issueLocalReadOnly,
		issueDeletesHeld,
		issueLocalUnsyncableReplacement,
		issueLocalMountBoundary,
		issueRemoteFolderUnverifiable,
		issueLocalInterruptedRemoval,
		issueLocalFileTooLarge,
		issueLocalBusy,
		issueLocalCrossDevice,
		issueUnsupportedOperation,
		issueMalwareDetected,
		issueLocalDatabaseOpen,
		issueUploadVerification,
		issueDownloadIntegrity,
		issueRequestTimeout,
		issueTransientConflict,
		issueTransientNotFound,
		issueResourceLocked,
		issueTransferURLRejected:
		return true
	default:
		return false
	}
}

// parseIssueType reads an issue type a store holds. Only a member parses.
// Anything else (an empty issue_type, a name no member has) was not written
// by this binary: a store from a build with other members, or a damaged row.
// The read fails naming the value instead of guessing what the row means;
// `drive reset-sync-state` rebuilds the store from truth.
func parseIssueType(stored string) (IssueType, error) {
	issueType := IssueType(stored)
	if !issueType.isMember() {
		return "", fmt.Errorf("sync: unknown issue type %q", stored)
	}

	return issueType, nil
}
