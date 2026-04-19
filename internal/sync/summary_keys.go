package sync

// SummaryKey is the stable sync-domain grouping key consumed by store
// projections, logs, and CLI-owned rendering.
type SummaryKey string

const (
	SummaryAuthenticationRequired SummaryKey = "authentication_required"
	SummaryQuotaExceeded          SummaryKey = "quota_exceeded"
	SummaryServiceOutage          SummaryKey = "service_outage"
	SummaryRateLimited            SummaryKey = "rate_limited"
	SummaryRemoteWriteDenied      SummaryKey = "remote_write_denied"
	SummaryRemoteReadDenied       SummaryKey = "remote_read_denied"
	SummaryLocalReadDenied        SummaryKey = "local_read_denied"
	SummaryLocalWriteDenied       SummaryKey = "local_permission_denied"
	SummaryInvalidFilename        SummaryKey = "invalid_filename"
	SummaryPathTooLong            SummaryKey = "path_too_long"
	SummaryFileTooLarge           SummaryKey = "file_too_large"
	SummaryCaseCollision          SummaryKey = "case_collision"
	SummaryDiskFull               SummaryKey = "disk_full"
	SummaryHashError              SummaryKey = "hash_error"
	SummaryFileTooLargeForSpace   SummaryKey = "file_too_large_for_space"
	SummaryUnexpectedCondition    SummaryKey = "unexpected_condition"
)

// SummaryKeyForObservationIssue maps a persisted observation issue to the
// shared summary family.
func SummaryKeyForObservationIssue(issueType string, scopeKey ScopeKey) SummaryKey {
	if key, ok := summaryKeyForIssueType(issueType); ok {
		return key
	}

	if key, ok := summaryKeyForIssueType(scopeKey.IssueType()); ok {
		return key
	}

	if !scopeKey.IsZero() {
		return SummaryUnexpectedCondition
	}

	return ""
}

// SummaryKeyForRetryWork maps persisted exact retry work to the shared summary
// family.
func SummaryKeyForRetryWork(issueType string, scopeKey ScopeKey) SummaryKey {
	if key, ok := summaryKeyForIssueType(issueType); ok {
		return key
	}

	if key, ok := summaryKeyForIssueType(scopeKey.IssueType()); ok {
		return key
	}

	if !scopeKey.IsZero() || issueType != "" {
		return SummaryUnexpectedCondition
	}

	return ""
}

// SummaryKeyForBlockScope maps a persisted block scope to the shared summary family.
func SummaryKeyForBlockScope(issueType string, scopeKey ScopeKey) SummaryKey {
	if key, ok := summaryKeyForIssueType(issueType); ok {
		return key
	}

	if key, ok := summaryKeyForIssueType(scopeKey.IssueType()); ok {
		return key
	}

	if !scopeKey.IsZero() {
		return SummaryUnexpectedCondition
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
	case IssueRemoteWriteDenied:
		return SummaryRemoteWriteDenied, true
	case IssueRemoteReadDenied:
		return SummaryRemoteReadDenied, true
	default:
		return "", false
	}
}

func summaryKeyForFilesystemIssueType(issueType string) (SummaryKey, bool) {
	switch issueType {
	case IssueLocalReadDenied:
		return SummaryLocalReadDenied, true
	case IssueLocalWriteDenied:
		return SummaryLocalWriteDenied, true
	case IssueInvalidFilename:
		return SummaryInvalidFilename, true
	case IssuePathTooLong:
		return SummaryPathTooLong, true
	case IssueFileTooLarge:
		return SummaryFileTooLarge, true
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
