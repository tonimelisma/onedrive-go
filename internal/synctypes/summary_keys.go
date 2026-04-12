package synctypes

// SummaryKey is the shared sync-domain rendering key consumed by sync logs,
// store-derived summaries, and CLI issue/status presentation.
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

// SummaryDescriptor is the shared rendering payload for a sync-domain
// failure/issue family.
type SummaryDescriptor struct {
	Key        SummaryKey
	Title      string
	Reason     string
	Action     string
	LogSummary string
}

// DescribeSummary returns the shared descriptor for a normalized summary key.
func DescribeSummary(key SummaryKey) SummaryDescriptor {
	if descriptor, ok := describeCoreSummary(key); ok {
		return descriptor
	}
	if descriptor, ok := describeFilesystemSummary(key); ok {
		return descriptor
	}
	return defaultSummaryDescriptor(key)
}

// SummaryKeyForPersistedFailure maps a persisted sync_failures row to the
// shared summary/rendering family.
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

// SummaryKeyForScopeBlock maps a persisted scope block to the shared
// summary/rendering family.
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

func describeCoreSummary(key SummaryKey) (SummaryDescriptor, bool) {
	switch string(key) {
	case string(SummaryConflictUnresolved):
		return SummaryDescriptor{
			Key:        SummaryConflictUnresolved,
			Title:      "UNRESOLVED CONFLICT",
			Reason:     "Local and remote changes diverged and both versions were preserved.",
			Action:     "Review the conflict copies, then run 'onedrive-go resolve local|remote|both <path>'.",
			LogSummary: "unresolved conflict",
		}, true
	case string(SummaryAuthenticationRequired):
		return SummaryDescriptor{
			Key:        SummaryAuthenticationRequired,
			Title:      "AUTHENTICATION REQUIRED",
			Reason:     "The last sync attempt for this account was rejected by OneDrive.",
			Action:     "Run 'onedrive-go whoami' to re-check access, or 'onedrive-go login' to sign in again.",
			LogSummary: "authentication required",
		}, true
	case string(SummaryQuotaExceeded):
		return SummaryDescriptor{
			Key:        SummaryQuotaExceeded,
			Title:      "QUOTA EXCEEDED",
			Reason:     "The OneDrive storage quota for this sync scope is full.",
			Action:     "Free up space in the owning drive, or ask the shared-folder owner to do so.",
			LogSummary: "quota exceeded",
		}, true
	case string(SummaryServiceOutage):
		return SummaryDescriptor{
			Key:        SummaryServiceOutage,
			Title:      "SERVICE OUTAGE",
			Reason:     "OneDrive service is temporarily unavailable.",
			Action:     "Wait for the service to recover (automatic retry in progress).",
			LogSummary: "service outage",
		}, true
	case string(SummaryRateLimited):
		return SummaryDescriptor{
			Key:        SummaryRateLimited,
			Title:      "RATE LIMITED",
			Reason:     "OneDrive asked this remote location to slow down.",
			Action:     "Wait for the retry window to expire (automatic retry in progress).",
			LogSummary: "rate limited",
		}, true
	case string(SummarySharedFolderWritesBlocked):
		return SummaryDescriptor{
			Key:        SummarySharedFolderWritesBlocked,
			Title:      "SHARED FOLDER WRITES BLOCKED",
			Reason:     "This shared folder is read-only for your current write attempts. Downloads continue normally.",
			Action:     "Remove or ignore local write changes here, or ask the owner for edit permissions if the write was intended.",
			LogSummary: "shared-folder writes blocked",
		}, true
	case string(SummaryRemotePermissionDenied):
		return SummaryDescriptor{
			Key:        SummaryRemotePermissionDenied,
			Title:      "PERMISSION DENIED",
			Reason:     "You don't have write access to this location.",
			Action:     "Ask the drive owner to grant you edit permissions.",
			LogSummary: "remote permission denied",
		}, true
	case string(SummarySyncFailure):
		return defaultSummaryDescriptor(SummarySyncFailure), true
	default:
		return SummaryDescriptor{}, false
	}
}

func describeFilesystemSummary(key SummaryKey) (SummaryDescriptor, bool) {
	switch string(key) {
	case string(SummaryLocalPermissionDenied):
		return SummaryDescriptor{
			Key:        SummaryLocalPermissionDenied,
			Title:      "LOCAL PERMISSION DENIED",
			Reason:     "The local directory or file is not accessible.",
			Action:     "Check filesystem permissions (e.g., chmod +r).",
			LogSummary: "local permission denied",
		}, true
	case string(SummaryInvalidFilename):
		return SummaryDescriptor{
			Key:        SummaryInvalidFilename,
			Title:      "INVALID FILENAME",
			Reason:     "The filename contains characters not allowed by OneDrive.",
			Action:     "Rename the file to remove invalid characters.",
			LogSummary: "invalid filename",
		}, true
	case string(SummaryPathTooLong):
		return SummaryDescriptor{
			Key:        SummaryPathTooLong,
			Title:      "PATH TOO LONG",
			Reason:     "The full path exceeds OneDrive's 400-character limit.",
			Action:     "Shorten the path by renaming files or folders.",
			LogSummary: "path too long",
		}, true
	case string(SummaryFileTooLarge):
		return SummaryDescriptor{
			Key:        SummaryFileTooLarge,
			Title:      "FILE TOO LARGE",
			Reason:     "The file exceeds the maximum upload size.",
			Action:     "Reduce the file size or exclude it via skip_files.",
			LogSummary: "file too large",
		}, true
	case string(SummaryHeldDeletes):
		return SummaryDescriptor{
			Key:        SummaryHeldDeletes,
			Title:      "HELD DELETES",
			Reason:     "Delete safety threshold triggered - too many deletes in one batch.",
			Action:     "Run `resolve deletes` to approve, or investigate first.",
			LogSummary: "held deletes",
		}, true
	case string(SummaryCaseCollision):
		return SummaryDescriptor{
			Key:        SummaryCaseCollision,
			Title:      "CASE COLLISION",
			Reason:     "Two files differ only in letter case, which OneDrive cannot distinguish.",
			Action:     "Rename one of the conflicting files.",
			LogSummary: "case collision",
		}, true
	case string(SummaryDiskFull):
		return SummaryDescriptor{
			Key:        SummaryDiskFull,
			Title:      "DISK FULL",
			Reason:     "Local disk space is insufficient for downloads.",
			Action:     "Free up local disk space.",
			LogSummary: "disk full",
		}, true
	case string(SummaryHashError):
		return SummaryDescriptor{
			Key:        SummaryHashError,
			Title:      "HASH ERROR",
			Reason:     "File hashing failed unexpectedly.",
			Action:     "Check file integrity and retry.",
			LogSummary: "hash error",
		}, true
	case string(SummaryFileTooLargeForSpace):
		return SummaryDescriptor{
			Key:        SummaryFileTooLargeForSpace,
			Title:      "FILE TOO LARGE FOR SPACE",
			Reason:     "The file is larger than available local disk space.",
			Action:     "Free up local disk space to fit this file.",
			LogSummary: "file too large for space",
		}, true
	default:
		return SummaryDescriptor{}, false
	}
}

func defaultSummaryDescriptor(key SummaryKey) SummaryDescriptor {
	return SummaryDescriptor{
		Key:        key,
		Title:      "SYNC FAILURE",
		Reason:     "An unexpected sync error occurred.",
		Action:     "Check logs for details or retry.",
		LogSummary: "sync failure",
	}
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
