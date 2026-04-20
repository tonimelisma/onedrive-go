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
		ConditionKeyForStoredCondition(IssueInvalidFilename, ScopeKey{}))
	assert.Equal(t, ConditionRemoteWriteDenied,
		ConditionKeyForStoredCondition(IssueRemoteWriteDenied, ScopeKey{}))
	assert.Equal(t, ConditionServiceOutage,
		ConditionKeyForStoredCondition("", SKService()))
	assert.Equal(t, ConditionQuotaExceeded,
		ConditionKeyForStoredCondition("custom_issue", SKQuotaOwn()))
	assert.Equal(t, ConditionUnexpectedCondition,
		ConditionKeyForStoredCondition("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Equal(t, ConditionRateLimited,
		ConditionKeyForStoredCondition("", SKThrottleDrive(driveid.New("0000000000000001"))))
	assert.Equal(t, ConditionQuotaExceeded,
		ConditionKeyForStoredCondition("custom_issue", SKQuotaOwn()))
	assert.Empty(t, ConditionKeyForStoredCondition("", ScopeKey{}))
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
	assert.Equal(t, ConditionRemoteReadDenied, mustConditionKeyForIssueType(t, IssueRemoteReadDenied))
	assert.Equal(t, ConditionLocalReadDenied, mustConditionKeyForIssueType(t, IssueLocalReadDenied))
	assert.Equal(t, ConditionLocalWriteDenied, mustConditionKeyForIssueType(t, IssueLocalWriteDenied))
	assert.Equal(t, ConditionInvalidFilename, mustConditionKeyForIssueType(t, IssueInvalidFilename))
	assert.Equal(t, ConditionPathTooLong, mustConditionKeyForIssueType(t, IssuePathTooLong))
	assert.Equal(t, ConditionFileTooLarge, mustConditionKeyForIssueType(t, IssueFileTooLarge))
	assert.Equal(t, ConditionCaseCollision, mustConditionKeyForIssueType(t, IssueCaseCollision))
	assert.Equal(t, ConditionDiskFull, mustConditionKeyForIssueType(t, IssueDiskFull))
	assert.Equal(t, ConditionFileTooLargeForSpace, mustConditionKeyForIssueType(t, IssueFileTooLargeForSpace))

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
