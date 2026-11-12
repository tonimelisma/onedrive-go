package sync

import "github.com/tonimelisma/onedrive-go/internal/errclass"

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
	ConditionLocalInterrupted       ConditionKey = "local_interrupted_removal"
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

// conditionKeyForStoredCondition maps one durable sync-condition record
// (observation issue, retry_work row, or block scope) into the shared
// condition family consumed by logs, watch summaries, and CLI status.
func conditionKeyForStoredCondition(conditionType IssueType, scopeKey ScopeKey) ConditionKey {
	return conditionKeyForIssueTypeOrScope(
		conditionType,
		scopeKey,
		describeScopeKey(scopeKey).DefaultConditionType,
	)
}

// conditionKeyForRuntimeResult maps one execution-time classified result into
// the shared condition family used by logs, watch summaries, and status. A
// transient runtime condition (a timeout, a conflict, a locked item) is an
// expected failure that is retried, so it has no family; it used to be filed
// as the unexpected condition. A failure with no condition at all is the
// unexpected condition, and a result that is not a failure has none.
func conditionKeyForRuntimeResult(class errclass.Class, conditionType IssueType) ConditionKey {
	if key, ok := conditionKeyForIssueType(conditionType); ok {
		return key
	}
	if conditionType != "" {
		// conditionKeyForIssueType names no family only for the transient
		// members and the empty value.
		return ""
	}

	switch class {
	case errclass.ClassRetryableTransient,
		errclass.ClassScopeBlockingTransient,
		errclass.ClassActionable,
		errclass.ClassFatal:
		return ConditionUnexpectedCondition
	case errclass.ClassInvalid,
		errclass.ClassSuccess,
		errclass.ClassShutdown,
		errclass.ClassSuperseded:
		return ""
	default:
		return ""
	}
}

// conditionClearsAutomatically reports whether a condition of this family
// clears without the user: OneDrive limiting requests or being unavailable,
// which sync waits out. Such a condition is said at Info and never makes the
// watch condition summary a warning. The switch names every ConditionKey (the
// exhaustive linter holds it to that), so a new family is decided on purpose;
// a key outside the set is not known to clear by itself, so it is reported as
// something the user may need to act on.
func conditionClearsAutomatically(key ConditionKey) bool {
	switch key {
	case ConditionRateLimited, ConditionServiceOutage:
		return true
	case ConditionAuthenticationRequired,
		ConditionQuotaExceeded,
		ConditionRemoteWriteDenied,
		ConditionRemoteReadDenied,
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
		ConditionLocalInterrupted,
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
	if key, ok := conditionKeyForIssueType(issueType); ok {
		return key
	}

	if key, ok := conditionKeyForIssueType(scopeFallbackIssueType); ok {
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
		ConditionLocalInterrupted,
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

// conditionKeyForIssueType is the condition family an issue type belongs to,
// in one switch that names every IssueType (the exhaustive linter holds it to
// that), so a new member gets a family, or none, on purpose. ok is false only
// where no family is named: for the transient runtime members, which no store
// keeps and whose results conditionKeyForRuntimeResult groups by their class,
// and for the empty value, which means "no condition". A value outside the
// enum, which the stores refuse and so only a programming error makes, is
// ConditionUnexpectedCondition: a lookup used to miss it and let a caller
// file it under whatever it tried next, a scope's condition included.
//
//nolint:gocyclo,funlen // one case per member: the family table of the enum
func conditionKeyForIssueType(issueType IssueType) (ConditionKey, bool) {
	switch issueType {
	case IssueInvalidFilename:
		return ConditionInvalidFilename, true
	case issuePathTooLong:
		return ConditionPathTooLong, true
	case issueFileTooLarge:
		return ConditionFileTooLarge, true
	case issueHashPanic:
		// The stored name is narrower than its use (issue_types.go).
		return ConditionHashError, true
	case IssueUnauthorized:
		return ConditionAuthenticationRequired, true
	case IssueRemoteWriteDenied:
		return ConditionRemoteWriteDenied, true
	case issueRemoteReadDenied:
		return ConditionRemoteReadDenied, true
	case IssueQuotaExceeded:
		return ConditionQuotaExceeded, true
	case issueRateLimited:
		return ConditionRateLimited, true
	case issueLocalReadDenied:
		return ConditionLocalReadDenied, true
	case issueLocalWriteDenied:
		return ConditionLocalWriteDenied, true
	case issueCaseCollision:
		return ConditionCaseCollision, true
	case issueRemoteNameTwin:
		return ConditionRemoteNameTwin, true
	case issueDiskFull:
		return ConditionDiskFull, true
	case issueServiceOutage:
		return ConditionServiceOutage, true
	case issueFileTooLargeForSpace:
		return ConditionFileTooLargeForSpace, true
	case issueLocalNameTooLong:
		return ConditionLocalNameTooLong, true
	case issueLocalReadOnly:
		return ConditionLocalReadOnly, true
	case issueDeletesHeld:
		return ConditionDeletesHeld, true
	case issueLocalUnsyncableReplacement:
		return ConditionLocalUnsyncable, true
	case issueLocalMountBoundary:
		return ConditionLocalMountBoundary, true
	case issueRemoteFolderUnverifiable:
		return ConditionRemoteFolderUnverified, true
	case issueLocalInterruptedRemoval:
		return ConditionLocalInterrupted, true
	case issueLocalFileTooLarge:
		return ConditionLocalFileTooLarge, true
	case issueLocalBusy:
		return ConditionLocalBusy, true
	case issueLocalCrossDevice:
		return ConditionLocalCrossDevice, true
	case issueUnsupportedOperation:
		return ConditionUnsupportedOperation, true
	case issueMalwareDetected:
		return ConditionMalwareDetected, true
	case issueLocalDatabaseOpen:
		return ConditionLocalDatabaseOpen, true
	case issueUploadVerification:
		return ConditionUploadVerification, true
	case issueDownloadIntegrity:
		return ConditionDownloadIntegrity, true
	case issueRequestTimeout,
		issueTransientConflict,
		issueTransientNotFound,
		issueResourceLocked,
		issueTransferURLRejected,
		"":
		return "", false
	default:
		return ConditionUnexpectedCondition, true
	}
}
