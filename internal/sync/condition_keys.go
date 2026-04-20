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
	ConditionDiskFull               ConditionKey = "disk_full"
	ConditionHashError              ConditionKey = "hash_error"
	ConditionFileTooLargeForSpace   ConditionKey = "file_too_large_for_space"
	ConditionUnexpectedCondition    ConditionKey = "unexpected_condition"
)

// ConditionKeyForObservationIssue maps a persisted observation issue to the
// shared condition family.
func ConditionKeyForObservationIssue(issueType string, scopeKey ScopeKey) ConditionKey {
	if key, ok := conditionKeyForIssueType(issueType); ok {
		return key
	}

	if key, ok := conditionKeyForIssueType(scopeKey.ConditionType()); ok {
		return key
	}

	if !scopeKey.IsZero() {
		return ConditionUnexpectedCondition
	}

	return ""
}

// ConditionKeyForRetryWork maps persisted exact retry work to the shared condition
// family.
func ConditionKeyForRetryWork(conditionType string, scopeKey ScopeKey) ConditionKey {
	if key, ok := conditionKeyForIssueType(conditionType); ok {
		return key
	}

	if key, ok := conditionKeyForIssueType(scopeKey.ConditionType()); ok {
		return key
	}

	if !scopeKey.IsZero() || conditionType != "" {
		return ConditionUnexpectedCondition
	}

	return ""
}

// ConditionKeyForBlockScope maps a persisted block scope to the shared condition family.
func ConditionKeyForBlockScope(conditionType string, scopeKey ScopeKey) ConditionKey {
	if key, ok := conditionKeyForIssueType(conditionType); ok {
		return key
	}

	if key, ok := conditionKeyForIssueType(DescribeScopeKey(scopeKey).DefaultConditionType); ok {
		return key
	}

	if !scopeKey.IsZero() {
		return ConditionUnexpectedCondition
	}

	return ""
}

// ConditionKeyForRuntimeResult maps one execution-time classified result into
// the shared condition family used by logs, watch summaries, and status.
func ConditionKeyForRuntimeResult(class errclass.Class, conditionType string) ConditionKey {
	if key, ok := conditionKeyForIssueType(conditionType); ok {
		return key
	}

	if class == errclass.ClassRetryableTransient ||
		class == errclass.ClassBlockScopeingTransient ||
		class == errclass.ClassActionable ||
		class == errclass.ClassFatal {
		return ConditionUnexpectedCondition
	}

	return ""
}

func conditionKeyForIssueType(issueType string) (ConditionKey, bool) {
	if key, ok := conditionKeyForCoreIssueType(issueType); ok {
		return key, true
	}

	return conditionKeyForFilesystemIssueType(issueType)
}

func conditionKeyForCoreIssueType(issueType string) (ConditionKey, bool) {
	switch issueType {
	case IssueUnauthorized:
		return ConditionAuthenticationRequired, true
	case IssueQuotaExceeded:
		return ConditionQuotaExceeded, true
	case IssueServiceOutage:
		return ConditionServiceOutage, true
	case IssueRateLimited:
		return ConditionRateLimited, true
	case IssueRemoteWriteDenied:
		return ConditionRemoteWriteDenied, true
	case IssueRemoteReadDenied:
		return ConditionRemoteReadDenied, true
	default:
		return "", false
	}
}

func conditionKeyForFilesystemIssueType(issueType string) (ConditionKey, bool) {
	switch issueType {
	case IssueLocalReadDenied:
		return ConditionLocalReadDenied, true
	case IssueLocalWriteDenied:
		return ConditionLocalWriteDenied, true
	case IssueInvalidFilename:
		return ConditionInvalidFilename, true
	case IssuePathTooLong:
		return ConditionPathTooLong, true
	case IssueFileTooLarge:
		return ConditionFileTooLarge, true
	case IssueCaseCollision:
		return ConditionCaseCollision, true
	case IssueDiskFull:
		return ConditionDiskFull, true
	case IssueHashPanic:
		return ConditionHashError, true
	case IssueFileTooLargeForSpace:
		return ConditionFileTooLargeForSpace, true
	default:
		return "", false
	}
}
