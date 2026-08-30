package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.14.3, R-6.8.16
func TestConditionKeyForStoredCondition_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionInvalidFilename,
		conditionKeyForStoredCondition(IssueInvalidFilename, ScopeKey{}))
	assert.Equal(t, ConditionRemoteWriteDenied,
		conditionKeyForStoredCondition(IssueRemoteWriteDenied, ScopeKey{}))
	assert.Equal(t, ConditionServiceOutage,
		conditionKeyForStoredCondition("", SKService()))
	assert.Equal(t, ConditionQuotaExceeded,
		conditionKeyForStoredCondition("custom_issue", SKQuotaOwn()))
	assert.Equal(t, ConditionUnexpectedCondition,
		conditionKeyForStoredCondition("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Equal(t, ConditionRateLimited,
		conditionKeyForStoredCondition("", SKThrottleDrive(driveid.New("0000000000000001"))))
	assert.Equal(t, ConditionQuotaExceeded,
		conditionKeyForStoredCondition("custom_issue", SKQuotaOwn()))
	assert.Empty(t, conditionKeyForStoredCondition("", ScopeKey{}))
}

// Validates: R-2.10.47, R-6.8.16
func TestConditionKeyLess_UsesCanonicalDisplayOrder(t *testing.T) {
	t.Parallel()

	assert.True(t, ConditionKeyLess(ConditionAuthenticationRequired, ConditionRemoteWriteDenied))
	assert.True(t, ConditionKeyLess(ConditionRemoteWriteDenied, ConditionInvalidFilename))
	assert.True(t, ConditionKeyLess(ConditionUnexpectedCondition, ConditionKey("zzz_custom")))
	assert.True(t, ConditionKeyLess(ConditionKey("aaa_custom"), ConditionKey("zzz_custom")))
	assert.False(t, ConditionKeyLess(ConditionKey("zzz_custom"), ConditionUnexpectedCondition))
}

// Validates: R-6.8.16
func TestConditionKeyForIssueType_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionAuthenticationRequired, mustConditionKeyForIssueType(t, IssueUnauthorized))
	assert.Equal(t, ConditionQuotaExceeded, mustConditionKeyForIssueType(t, IssueQuotaExceeded))
	assert.Equal(t, ConditionRemoteReadDenied, mustConditionKeyForIssueType(t, issueRemoteReadDenied))
	assert.Equal(t, ConditionLocalReadDenied, mustConditionKeyForIssueType(t, issueLocalReadDenied))
	assert.Equal(t, ConditionLocalWriteDenied, mustConditionKeyForIssueType(t, issueLocalWriteDenied))
	assert.Equal(t, ConditionInvalidFilename, mustConditionKeyForIssueType(t, IssueInvalidFilename))
	assert.Equal(t, ConditionPathTooLong, mustConditionKeyForIssueType(t, issuePathTooLong))
	assert.Equal(t, ConditionFileTooLarge, mustConditionKeyForIssueType(t, issueFileTooLarge))
	assert.Equal(t, ConditionCaseCollision, mustConditionKeyForIssueType(t, issueCaseCollision))
	assert.Equal(t, ConditionDiskFull, mustConditionKeyForIssueType(t, issueDiskFull))
	assert.Equal(t, ConditionFileTooLargeForSpace, mustConditionKeyForIssueType(t, issueFileTooLargeForSpace))

	key, ok := conditionKeyForIssueType("custom_issue")
	assert.False(t, ok)
	assert.Empty(t, key)
}

func mustConditionKeyForIssueType(t *testing.T, issueType string) ConditionKey {
	t.Helper()

	key, ok := conditionKeyForIssueType(issueType)
	assert.True(t, ok)

	return key
}
