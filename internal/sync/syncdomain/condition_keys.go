package syncdomain

// ConditionKey is the stable sync-domain grouping key consumed by store
// projections, logs, and CLI-owned rendering.
type ConditionKey string

const (
	ConditionAuthenticationRequired ConditionKey = "authentication_required"
	ConditionQuotaExceeded          ConditionKey = "quota_exceeded"
	ConditionServiceOutage          ConditionKey = "service_outage"
	ConditionRateLimited            ConditionKey = "rate_limited"
	ConditionRemoteWriteDenied      ConditionKey = "remote_write_denied"
	ConditionRemoteReadDenied       ConditionKey = "remote_read_denied"
	ConditionRemoteRootNotFound     ConditionKey = "remote_root_not_found"
	ConditionLocalReadDenied        ConditionKey = "local_read_denied"
	ConditionLocalWriteDenied       ConditionKey = "local_permission_denied"
	ConditionInvalidFilename        ConditionKey = "invalid_filename"
	ConditionPathTooLong            ConditionKey = "path_too_long"
	ConditionFileTooLarge           ConditionKey = "file_too_large"
	ConditionCaseCollision          ConditionKey = "case_collision"
	ConditionRemoteNameTwin         ConditionKey = "remote_name_twin"
	ConditionDiskFull               ConditionKey = "disk_full"
	ConditionHashError              ConditionKey = "hash_error"
	ConditionFileTooLargeForSpace   ConditionKey = "file_too_large_for_space"
	ConditionLocalNameTooLong       ConditionKey = "local_name_too_long"
	ConditionLocalReadOnly          ConditionKey = "local_read_only"
	ConditionDeletesHeld            ConditionKey = "deletes_held"
	ConditionLocalUnsyncable        ConditionKey = "local_unsyncable_replacement"
	ConditionLocalMountBoundary     ConditionKey = "local_mount_boundary"
	ConditionLocalMountMissing      ConditionKey = "local_mount_missing"
	ConditionLocalInterrupted       ConditionKey = "local_interrupted_removal"
	ConditionRemovedFolderKept      ConditionKey = "removed_folder_kept"
	ConditionRemoteFolderUnverified ConditionKey = "remote_folder_delete_unverifiable"
	ConditionLocalFileTooLarge      ConditionKey = "local_file_too_large"
	ConditionLocalBusy              ConditionKey = "local_busy"
	ConditionLocalCrossDevice       ConditionKey = "local_cross_device"
	ConditionUnsupportedOperation   ConditionKey = "unsupported_operation"
	ConditionMalwareDetected        ConditionKey = "malware_detected"
	ConditionLocalDatabaseOpen      ConditionKey = "local_database_open"
	ConditionUploadVerification     ConditionKey = "upload_verification_failed"
	ConditionDownloadIntegrity      ConditionKey = "download_integrity"
	ConditionUnexpectedCondition    ConditionKey = "unexpected_condition"
)

// ConditionKeyForStoredCondition maps one durable sync-condition record
// (observation issue, retry_work row, or block scope) into the shared
// condition family consumed by logs, watch summaries, and CLI status.
func ConditionKeyForStoredCondition(conditionType IssueType, scopeKey ScopeKey) ConditionKey {
	return conditionKeyForIssueTypeOrScope(
		conditionType,
		scopeKey,
		DescribeScopeKey(scopeKey).DefaultConditionType,
	)
}

// ConditionClearsAutomatically reports whether a condition of this family
// clears without the user: OneDrive limiting requests or being unavailable,
// which sync waits out. Such a condition is said at Info and never makes the
// watch condition summary a warning. The switch names every ConditionKey (the
// exhaustive linter holds it to that), so a new family is decided on purpose;
// a key outside the set is not known to clear by itself, so it is reported as
// something the user may need to act on.
func ConditionClearsAutomatically(key ConditionKey) bool {
	switch key {
	case ConditionRateLimited, ConditionServiceOutage:
		return true
	case ConditionAuthenticationRequired,
		ConditionQuotaExceeded,
		ConditionRemoteWriteDenied,
		ConditionRemoteReadDenied,
		ConditionRemoteRootNotFound,
		ConditionLocalReadDenied,
		ConditionLocalWriteDenied,
		ConditionInvalidFilename,
		ConditionPathTooLong,
		ConditionFileTooLarge,
		ConditionCaseCollision,
		ConditionRemoteNameTwin,
		ConditionDiskFull,
		ConditionHashError,
		ConditionFileTooLargeForSpace,
		ConditionLocalNameTooLong,
		ConditionLocalReadOnly,
		ConditionDeletesHeld,
		ConditionLocalUnsyncable,
		ConditionLocalMountBoundary,
		ConditionLocalMountMissing,
		ConditionLocalInterrupted,
		ConditionRemovedFolderKept,
		ConditionRemoteFolderUnverified,
		ConditionLocalFileTooLarge,
		ConditionLocalBusy,
		ConditionLocalCrossDevice,
		ConditionUnsupportedOperation,
		ConditionMalwareDetected,
		ConditionLocalDatabaseOpen,
		ConditionUploadVerification,
		ConditionDownloadIntegrity,
		ConditionUnexpectedCondition:
		return false
	default:
		return false
	}
}

// ConditionKeyLess reports whether left should sort before right in stable
// user-facing condition output. Known condition families use a fixed sync-owned
// display order; unknown keys fall back to lexical order after the known set.
func ConditionKeyLess(left, right ConditionKey) bool {
	leftRank := conditionKeyRank(left)
	rightRank := conditionKeyRank(right)
	if leftRank != rightRank {
		return leftRank < rightRank
	}

	return string(left) < string(right)
}

// conditionKeyForIssueTypeOrScope is the family of a record's own condition,
// or, when that names none (the empty value or a transient member), the
// family of its scope's default condition. A record that still has none is
// the unexpected condition; only a record with neither a condition nor a
// scope has no key.
func conditionKeyForIssueTypeOrScope(
	issueType IssueType,
	scopeKey ScopeKey,
	scopeFallbackIssueType IssueType,
) ConditionKey {
	if key, ok := ConditionKeyForIssueType(issueType); ok {
		return key
	}

	if key, ok := ConditionKeyForIssueType(scopeFallbackIssueType); ok {
		return key
	}

	if !scopeKey.IsZero() || issueType != "" {
		return ConditionUnexpectedCondition
	}

	return ""
}

func conditionKeyRank(key ConditionKey) int {
	ordered := [...]ConditionKey{
		ConditionDeletesHeld,
		ConditionAuthenticationRequired,
		ConditionQuotaExceeded,
		ConditionServiceOutage,
		ConditionRateLimited,
		ConditionRemoteWriteDenied,
		ConditionRemoteReadDenied,
		ConditionRemoteRootNotFound,
		ConditionLocalReadDenied,
		ConditionLocalWriteDenied,
		ConditionInvalidFilename,
		ConditionPathTooLong,
		ConditionFileTooLarge,
		ConditionCaseCollision,
		ConditionRemoteNameTwin,
		ConditionDiskFull,
		ConditionHashError,
		ConditionFileTooLargeForSpace,
		ConditionLocalNameTooLong,
		ConditionLocalReadOnly,
		ConditionLocalUnsyncable,
		ConditionLocalMountBoundary,
		ConditionLocalMountMissing,
		ConditionLocalInterrupted,
		ConditionRemovedFolderKept,
		ConditionRemoteFolderUnverified,
		ConditionLocalFileTooLarge,
		ConditionLocalBusy,
		ConditionLocalCrossDevice,
		ConditionUnsupportedOperation,
		ConditionMalwareDetected,
		ConditionLocalDatabaseOpen,
		ConditionUploadVerification,
		ConditionDownloadIntegrity,
		ConditionUnexpectedCondition,
	}

	for i := range ordered {
		if ordered[i] == key {
			return i
		}
	}
	if key == "" {
		return len(ordered) + 1
	}

	return len(ordered)
}

// ConditionKeyForIssueType is the condition family an issue type belongs to,
// in one switch that names every IssueType (the exhaustive linter holds it to
// that), so a new member gets a family, or none, on purpose. ok is false only
// where no family is named: for the transient runtime members, which no store
// keeps and whose results the engine's conditionKeyForRuntimeResult groups by their class,
// and for the empty value, which means "no condition". A value outside the
// enum, which the stores refuse and so only a programming error makes, is
// ConditionUnexpectedCondition: a lookup used to miss it and let a caller
// file it under whatever it tried next, a scope's condition included.
//
//nolint:gocyclo,funlen // one case per member: the family table of the enum
func ConditionKeyForIssueType(issueType IssueType) (ConditionKey, bool) {
	switch issueType {
	case IssueInvalidFilename:
		return ConditionInvalidFilename, true
	case IssuePathTooLong:
		return ConditionPathTooLong, true
	case IssueFileTooLarge:
		return ConditionFileTooLarge, true
	case IssueHashError:
		// The stored name is narrower than its use (issue_types.go).
		return ConditionHashError, true
	case IssueUnauthorized:
		return ConditionAuthenticationRequired, true
	case IssueRemoteWriteDenied:
		return ConditionRemoteWriteDenied, true
	case IssueRemoteReadDenied:
		return ConditionRemoteReadDenied, true
	case IssueRemoteRootNotFound:
		return ConditionRemoteRootNotFound, true
	case IssueQuotaExceeded:
		return ConditionQuotaExceeded, true
	case IssueRateLimited:
		return ConditionRateLimited, true
	case IssueLocalReadDenied:
		return ConditionLocalReadDenied, true
	case IssueLocalWriteDenied:
		return ConditionLocalWriteDenied, true
	case IssueCaseCollision:
		return ConditionCaseCollision, true
	case IssueRemoteNameTwin:
		return ConditionRemoteNameTwin, true
	case IssueDiskFull:
		return ConditionDiskFull, true
	case IssueServiceOutage:
		return ConditionServiceOutage, true
	case IssueFileTooLargeForSpace:
		return ConditionFileTooLargeForSpace, true
	case IssueLocalNameTooLong:
		return ConditionLocalNameTooLong, true
	case IssueLocalReadOnly:
		return ConditionLocalReadOnly, true
	case IssueDeletesHeld:
		return ConditionDeletesHeld, true
	case IssueLocalUnsyncableReplacement:
		return ConditionLocalUnsyncable, true
	case IssueLocalMountBoundary:
		return ConditionLocalMountBoundary, true
	case IssueLocalMountMissing:
		return ConditionLocalMountMissing, true
	case IssueRemoteFolderUnverifiable:
		return ConditionRemoteFolderUnverified, true
	case IssueLocalInterruptedRemoval:
		return ConditionLocalInterrupted, true
	case IssueRemovedFolderKept:
		return ConditionRemovedFolderKept, true
	case IssueLocalFileTooLarge:
		return ConditionLocalFileTooLarge, true
	case IssueLocalBusy:
		return ConditionLocalBusy, true
	case IssueLocalCrossDevice:
		return ConditionLocalCrossDevice, true
	case IssueUnsupportedOperation:
		return ConditionUnsupportedOperation, true
	case IssueMalwareDetected:
		return ConditionMalwareDetected, true
	case IssueLocalDatabaseOpen:
		return ConditionLocalDatabaseOpen, true
	case IssueUploadVerification:
		return ConditionUploadVerification, true
	case IssueDownloadIntegrity:
		return ConditionDownloadIntegrity, true
	case IssueRequestTimeout,
		IssueTransientConflict,
		IssueTransientNotFound,
		IssueResourceLocked,
		IssueTransferURLRejected,
		"":
		return "", false
	default:
		return ConditionUnexpectedCondition, true
	}
}
