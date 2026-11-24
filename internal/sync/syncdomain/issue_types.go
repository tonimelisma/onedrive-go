// The sync-domain issue type vocabulary, shared across the observer, engine,
// store, and CLI layers.

package syncdomain

import "fmt"

// IssueType names one sync condition: the issue_type of an observation issue,
// the condition_type of a retry_work row, a scope's default condition, and the
// condition of a runtime result decision. A member's value is the string the
// stores persist. The stores convert at their own boundary: a write refuses a
// value that is not a member and a read parses the stored string
// (ParseIssueType), so whatever a store hands back is a member, or empty where
// a field may hold no condition. Elsewhere a value becomes a plain string only
// on its way out, as a log attribute or in CLI output.
type IssueType string

// Issue type constants for sync conditions. Scanner-detectable issues
// (invalid_filename, path_too_long, file_too_large) are caught at
// observation time by shouldObserve / processEntry. Runtime issues
// are detected during execution by the engine. Every decision made per issue
// type is a switch that names every member, its default answering only a
// value outside the enum: membership (IsMember), truth
// (truthVerdictForIssueType), condition family (ConditionKeyForIssueType),
// retry curve, permission boundary scope, block-scope wording, the
// one-shot exit status (IssueTypeLeavesFileUnsynced) and the read failure of
// a whole tree (IssueTypeIsRootReadFailure). A new
// member does not pass the exhaustive linter until each of them decides it,
// and each has a test that reads the members from this file.
const (
	// Scanner-detectable (observation-time).
	IssueInvalidFilename IssueType = "invalid_filename"
	IssuePathTooLong     IssueType = "path_too_long"
	IssueFileTooLarge    IssueType = "file_too_large"

	// Scanner-detectable (hash phase): the scanner records it for a panic in
	// a hash worker and for any error but "not found" and "permission denied"
	// while reading a file to hash it (scanner_hash.go), so it means "the content
	// could not be read or checked". Users see it as the condition
	// ConditionHashError, of the same name (ConditionKeyForIssueType). Store
	// generation 29 gave the stored value this name; before, it named only
	// the panic.
	IssueHashError IssueType = "hash_error"

	// Runtime (execution-time).
	IssueUnauthorized      IssueType = "unauthorized"
	IssueRemoteWriteDenied IssueType = "remote_write_denied"
	IssueRemoteReadDenied  IssueType = "remote_read_denied"
	// IssueRemoteRootNotFound marks the root an engine syncs (a drive's own
	// root or a shared folder's) when OneDrive answered its read with "not
	// found"; a "forbidden" answer is IssueRemoteReadDenied
	// (rootReadFailureIssueType). Both sit at "/" under the root read scope,
	// so they block the same truth and every per-type switch except the
	// condition family decides them alike; each has its own condition
	// family, because what the user is told, and can do, differs.
	IssueRemoteRootNotFound IssueType = "remote_root_not_found"
	IssueQuotaExceeded      IssueType = "quota_exceeded"
	IssueRateLimited        IssueType = "rate_limited"
	IssueLocalReadDenied    IssueType = "local_read_denied"
	IssueLocalWriteDenied   IssueType = "local_permission_denied"
	IssueCaseCollision      IssueType = "case_collision"
	// IssueRemoteNameTwin marks a path where OneDrive keeps two or more
	// items that this computer stores under one name (store_remote_twins.go).
	IssueRemoteNameTwin       IssueType = "remote_name_twin"
	IssueDiskFull             IssueType = "disk_full"
	IssueServiceOutage        IssueType = "service_outage"
	IssueFileTooLargeForSpace IssueType = "file_too_large_for_space"
	IssueLocalNameTooLong     IssueType = "local_name_too_long"
	IssueLocalReadOnly        IssueType = "local_read_only"
	// IssueDeletesHeld marks deletes the delete-safety hold kept for
	// confirmation (planner_delete_safety.go).
	IssueDeletesHeld IssueType = "deletes_held"
	// IssueLocalUnsyncableReplacement marks a synced path whose item is now
	// something sync cannot represent: replaced by a symlink or special file,
	// or renamed to an invalid name or an excluded location. The synced path
	// is unavailable truth, never a delete (D-004, R-6.2.14).
	IssueLocalUnsyncableReplacement IssueType = "local_unsyncable_replacement"
	// IssueLocalMountBoundary marks a folder on another filesystem inside the
	// sync folder; sync does not cross into it (D-003, R-6.2.15).
	IssueLocalMountBoundary IssueType = "local_mount_boundary"
	// IssueLocalMountMissing marks a folder sync crossed into as the mount
	// point of another filesystem (cross_filesystems) that is not mounted
	// now. Its synced content is unavailable truth, never deleted, until the
	// disk is back or the user removes the folder; `drive rebind-root` keeps
	// the mount point (R-6.2.15).
	// Every per-type decision treats it as IssueLocalMountBoundary; only what
	// the user is told differs.
	IssueLocalMountMissing IssueType = "local_mount_missing"
	// IssueRemoteFolderUnverifiable marks a folder delete on OneDrive that
	// sync will not run because the folder is too large to list first.
	IssueRemoteFolderUnverifiable IssueType = "remote_folder_delete_unverifiable"
	// IssueLocalInterruptedRemoval marks a file sync set aside while removing
	// it and never moved out, because a run was interrupted. It is the
	// user's content, so it is reported, never deleted.
	IssueLocalInterruptedRemoval IssueType = "local_interrupted_removal"
	// IssueRemovedFolderKept marks an entry of a removed shared folder's
	// local copy that its release did not take (engine_shortcut_release.go):
	// content sync does not manage, a synced folder that cannot be written
	// to, a synced entry the local delete could not move to the trash or
	// delete, or a change that can no longer be sent to the shared folder.
	// It is the user's, so it is reported and kept.
	IssueRemovedFolderKept IssueType = "removed_folder_kept"
	IssueLocalFileTooLarge IssueType = "local_file_too_large"
	IssueLocalBusy         IssueType = "local_busy"
	// IssueLocalCallAbandoned marks a file whose last transfer gave up on a
	// read (upload) or write (download) of the local file that stopped
	// moving and left the stuck call behind, freeing its worker
	// (graph.ErrLocalCallAbandoned, R-6.8.32). It is retried as
	// IssueLocalBusy is; only what the user is told differs.
	IssueLocalCallAbandoned   IssueType = "local_call_abandoned"
	IssueLocalCrossDevice     IssueType = "local_cross_device"
	IssueUnsupportedOperation IssueType = "unsupported_operation"
	IssueMalwareDetected      IssueType = "malware_detected"
	IssueLocalDatabaseOpen    IssueType = "local_database_open"
	IssueUploadVerification   IssueType = "upload_verification_failed"
	IssueDownloadIntegrity    IssueType = "download_integrity"

	// Runtime transient (execution-time). These name a retryable failure in
	// the result decision, its retry delay and its log lines; no store keeps
	// them, because only an actionable condition is persisted.
	// IssueRequestTimeout also covers a transfer the stall watchdog abandoned.
	IssueRequestTimeout      IssueType = "request_timeout"
	IssueTransientConflict   IssueType = "transient_conflict"
	IssueTransientNotFound   IssueType = "transient_not_found"
	IssueResourceLocked      IssueType = "resource_locked"
	IssueTransferURLRejected IssueType = "transfer_url_rejected"
)

// IsMember reports whether t is one of the constants above. The exhaustive
// linter holds the switch to every member, so this is the list the store
// boundary trusts. The empty value is not a member: it means "no condition"
// where a field may hold none, and each store says whether it may.
func (t IssueType) IsMember() bool {
	switch t {
	case IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueHashError,
		IssueUnauthorized,
		IssueRemoteWriteDenied,
		IssueRemoteReadDenied,
		IssueRemoteRootNotFound,
		IssueQuotaExceeded,
		IssueRateLimited,
		IssueLocalReadDenied,
		IssueLocalWriteDenied,
		IssueCaseCollision,
		IssueRemoteNameTwin,
		IssueDiskFull,
		IssueServiceOutage,
		IssueFileTooLargeForSpace,
		IssueLocalNameTooLong,
		IssueLocalReadOnly,
		IssueDeletesHeld,
		IssueLocalUnsyncableReplacement,
		IssueLocalMountBoundary,
		IssueLocalMountMissing,
		IssueRemoteFolderUnverifiable,
		IssueLocalInterruptedRemoval,
		IssueRemovedFolderKept,
		IssueLocalFileTooLarge,
		IssueLocalBusy,
		IssueLocalCallAbandoned,
		IssueLocalCrossDevice,
		IssueUnsupportedOperation,
		IssueMalwareDetected,
		IssueLocalDatabaseOpen,
		IssueUploadVerification,
		IssueDownloadIntegrity,
		IssueRequestTimeout,
		IssueTransientConflict,
		IssueTransientNotFound,
		IssueResourceLocked,
		IssueTransferURLRejected:
		return true
	default:
		return false
	}
}

// IssueTypeLeavesFileUnsynced reports whether an observation issue of this
// type keeps a file from syncing, so that a one-shot sync ending with it in
// the store has work left and exits 1 (R-6.6.28). A type answers no only when
// it records a deliberate hold or a fact that keeps no file from syncing:
// deletes held for `sync --confirm-deletes`, the leftover of an interrupted
// removal, which sits at an internal name sync never syncs and is reported
// for the user to look at, and what a removed shared folder's release kept:
// content sync does not sync by design, a synced folder that cannot be
// written to or a synced entry the local delete could not remove, both
// already synced, or a change that can no longer be sent to the shared
// folder. Each of them keeps the folder's local copy through the release's
// own report (Report.LocalTreeHolds), not through this count. The change that
// could not be sent is still work the run left, and fails it: the release's
// report counts it (LeftWork.NotUploaded), so counting its row here would
// count it twice.
// A value outside the enum, which the stores refuse, is not known to be done,
// so it counts.
func IssueTypeLeavesFileUnsynced(issueType IssueType) bool {
	switch issueType {
	case IssueDeletesHeld,
		IssueLocalInterruptedRemoval,
		IssueRemovedFolderKept:
		return false
	case IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueHashError,
		IssueUnauthorized,
		IssueRemoteWriteDenied,
		IssueRemoteReadDenied,
		IssueRemoteRootNotFound,
		IssueQuotaExceeded,
		IssueRateLimited,
		IssueLocalReadDenied,
		IssueLocalWriteDenied,
		IssueCaseCollision,
		IssueRemoteNameTwin,
		IssueDiskFull,
		IssueServiceOutage,
		IssueFileTooLargeForSpace,
		IssueLocalNameTooLong,
		IssueLocalReadOnly,
		IssueLocalUnsyncableReplacement,
		IssueLocalMountBoundary,
		IssueLocalMountMissing,
		IssueRemoteFolderUnverifiable,
		IssueLocalFileTooLarge,
		IssueLocalBusy,
		IssueLocalCallAbandoned,
		IssueLocalCrossDevice,
		IssueUnsupportedOperation,
		IssueMalwareDetected,
		IssueLocalDatabaseOpen,
		IssueUploadVerification,
		IssueDownloadIntegrity,
		IssueRequestTimeout,
		IssueTransientConflict,
		IssueTransientNotFound,
		IssueResourceLocked,
		IssueTransferURLRejected:
		return true
	default:
		return true
	}
}

// IssueTypeIsRootReadFailure reports whether an issue of this type, held at
// the root read scope, is OneDrive's refusal to read the root an engine
// syncs (rootReadFailureIssueType): "forbidden" or "not found", which makes
// the whole tree unreadable. A value outside the enum is not.
func IssueTypeIsRootReadFailure(issueType IssueType) bool {
	switch issueType {
	case IssueRemoteReadDenied,
		IssueRemoteRootNotFound:
		return true
	case IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueHashError,
		IssueUnauthorized,
		IssueRemoteWriteDenied,
		IssueQuotaExceeded,
		IssueRateLimited,
		IssueLocalReadDenied,
		IssueLocalWriteDenied,
		IssueCaseCollision,
		IssueRemoteNameTwin,
		IssueDiskFull,
		IssueServiceOutage,
		IssueFileTooLargeForSpace,
		IssueLocalNameTooLong,
		IssueLocalReadOnly,
		IssueDeletesHeld,
		IssueLocalUnsyncableReplacement,
		IssueLocalMountBoundary,
		IssueLocalMountMissing,
		IssueRemoteFolderUnverifiable,
		IssueLocalInterruptedRemoval,
		IssueRemovedFolderKept,
		IssueLocalFileTooLarge,
		IssueLocalBusy,
		IssueLocalCallAbandoned,
		IssueLocalCrossDevice,
		IssueUnsupportedOperation,
		IssueMalwareDetected,
		IssueLocalDatabaseOpen,
		IssueUploadVerification,
		IssueDownloadIntegrity,
		IssueRequestTimeout,
		IssueTransientConflict,
		IssueTransientNotFound,
		IssueResourceLocked,
		IssueTransferURLRejected:
		return false
	default:
		return false
	}
}

// ParseIssueType reads an issue type a store holds. Only a member parses.
// Anything else (an empty issue_type, a name no member has) was not written
// by this binary: a store from a build with other members, or a damaged row.
// The read fails naming the value instead of guessing what the row means;
// `drive reset-sync-state` rebuilds the store from truth.
func ParseIssueType(stored string) (IssueType, error) {
	issueType := IssueType(stored)
	if !issueType.IsMember() {
		return "", fmt.Errorf("syncdomain: unknown issue type %q", stored)
	}

	return issueType, nil
}
