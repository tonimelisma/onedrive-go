package sync

// SummaryKey is the stable sync-domain grouping key consumed by store
// projections, logs, and CLI-owned rendering.
type SummaryKey string

const (
	SummaryConflictUnresolved        SummaryKey = "conflict_unresolved"
	SummaryAuthenticationRequired    SummaryKey = "authentication_required"
	SummaryQuotaExceeded             SummaryKey = "quota_exceeded"
	SummaryServiceOutage             SummaryKey = "service_outage"
	SummaryRateLimited               SummaryKey = "rate_limited"
	SummarySharedFolderWritesBlocked SummaryKey = "shared_folder_writes_blocked"
	SummaryLocalPermissionDenied     SummaryKey = "local_permission_denied"
	SummaryRemotePermissionDenied    SummaryKey = "remote_permission_denied"
	SummaryInvalidFilename           SummaryKey = "invalid_filename"
	SummaryPathTooLong               SummaryKey = "path_too_long"
	SummaryFileTooLarge              SummaryKey = "file_too_large"
	SummaryHeldDeletes               SummaryKey = "held_deletes"
	SummaryCaseCollision             SummaryKey = "case_collision"
	SummaryDiskFull                  SummaryKey = "disk_full"
	SummaryHashError                 SummaryKey = "hash_error"
	SummaryFileTooLargeForSpace      SummaryKey = "file_too_large_for_space"
	SummarySyncFailure               SummaryKey = "sync_failure"
)

// SummaryKeyForPersistedFailure maps a persisted sync_failures row to the
// shared summary family.
func SummaryKeyForPersistedFailure(
	issueType string,
	category FailureCategory,
	role FailureRole,
) SummaryKey {
	if key, ok := summaryKeyForIssueType(issueType); ok {
		return key
	}

	if category == CategoryActionable || role != FailureRoleItem {
		return SummarySyncFailure
	}

	return SummarySyncFailure
}

// SummaryKeyForScopeBlock maps a persisted scope block to the shared summary family.
func SummaryKeyForScopeBlock(issueType string, scopeKey ScopeKey) SummaryKey {
	if key, ok := summaryKeyForIssueType(issueType); ok {
		return key
	}

	if key, ok := summaryKeyForIssueType(scopeKey.IssueType()); ok {
		return key
	}

	if !scopeKey.IsZero() {
		return SummarySyncFailure
	}

	return ""
}

func summaryKeyForIssueType(issueType string) (SummaryKey, bool) {
	if key, ok := summaryKeyForCoreIssueType(issueType); ok {
		return key, true
	}
	return summaryKeyForFilesystemIssueType(issueType)
}

func summaryKeyForCoreIssueType(issueType string) (SummaryKey, bool) {
	switch issueType {
	case IssueUnauthorized:
		return SummaryAuthenticationRequired, true
	case IssueQuotaExceeded:
		return SummaryQuotaExceeded, true
	case IssueServiceOutage:
		return SummaryServiceOutage, true
	case IssueRateLimited:
		return SummaryRateLimited, true
	case IssueSharedFolderBlocked:
		return SummarySharedFolderWritesBlocked, true
	case IssuePermissionDenied:
		return SummaryRemotePermissionDenied, true
	default:
		return "", false
	}
}

func summaryKeyForFilesystemIssueType(issueType string) (SummaryKey, bool) {
	switch issueType {
	case IssueLocalPermissionDenied:
		return SummaryLocalPermissionDenied, true
	case IssueInvalidFilename:
		return SummaryInvalidFilename, true
	case IssuePathTooLong:
		return SummaryPathTooLong, true
	case IssueFileTooLarge:
		return SummaryFileTooLarge, true
	case IssueDeleteSafetyHeld:
		return SummaryHeldDeletes, true
	case IssueCaseCollision:
		return SummaryCaseCollision, true
	case IssueDiskFull:
		return SummaryDiskFull, true
	case IssueHashPanic:
		return SummaryHashError, true
	case IssueFileTooLargeForSpace:
		return SummaryFileTooLargeForSpace, true
	default:
		return "", false
	}
}
