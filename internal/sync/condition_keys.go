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

// ConditionKeyForStoredCondition maps one durable sync-condition record
// (observation issue, retry_work row, or block scope) into the shared
// condition family consumed by logs, watch summaries, and CLI status.
func ConditionKeyForStoredCondition(conditionType string, scopeKey ScopeKey) ConditionKey {
	return conditionKeyForIssueTypeOrScope(
		conditionType,
		scopeKey,
		DescribeScopeKey(scopeKey).DefaultConditionType,
	)
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

func conditionKeyForIssueTypeOrScope(
	issueType string,
	scopeKey ScopeKey,
	scopeFallbackIssueType string,
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
		ConditionDiskFull,
		ConditionHashError,
		ConditionFileTooLargeForSpace,
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

func conditionKeyForIssueType(issueType string) (ConditionKey, bool) {
	// Keep the condition-family classifier exhaustive in one local table so
	// callers do not have to think in separate "core" and "filesystem"
	// taxonomies.
	for _, mapping := range [...]struct {
		issueType string
		key       ConditionKey
	}{
		{issueType: IssueUnauthorized, key: ConditionAuthenticationRequired},
		{issueType: IssueQuotaExceeded, key: ConditionQuotaExceeded},
		{issueType: IssueServiceOutage, key: ConditionServiceOutage},
		{issueType: IssueRateLimited, key: ConditionRateLimited},
		{issueType: IssueRemoteWriteDenied, key: ConditionRemoteWriteDenied},
		{issueType: IssueRemoteReadDenied, key: ConditionRemoteReadDenied},
		{issueType: IssueLocalReadDenied, key: ConditionLocalReadDenied},
		{issueType: IssueLocalWriteDenied, key: ConditionLocalWriteDenied},
		{issueType: IssueInvalidFilename, key: ConditionInvalidFilename},
		{issueType: IssuePathTooLong, key: ConditionPathTooLong},
		{issueType: IssueFileTooLarge, key: ConditionFileTooLarge},
		{issueType: IssueCaseCollision, key: ConditionCaseCollision},
		{issueType: IssueDiskFull, key: ConditionDiskFull},
		{issueType: IssueHashPanic, key: ConditionHashError},
		{issueType: IssueFileTooLargeForSpace, key: ConditionFileTooLargeForSpace},
	} {
		if mapping.issueType == issueType {
			return mapping.key, true
		}
	}

	return "", false
}
