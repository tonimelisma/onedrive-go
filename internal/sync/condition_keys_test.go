package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.14.3, R-6.8.16
func TestConditionKeyForObservationIssue_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionInvalidFilename,
		ConditionKeyForObservationIssue(IssueInvalidFilename, ScopeKey{}))
	assert.Equal(t, ConditionRemoteWriteDenied,
		ConditionKeyForObservationIssue(IssueRemoteWriteDenied, ScopeKey{}))
	assert.Equal(t, ConditionServiceOutage,
		ConditionKeyForObservationIssue("", SKService()))
	assert.Equal(t, ConditionQuotaExceeded,
		ConditionKeyForObservationIssue("custom_issue", SKQuotaOwn()))
	assert.Equal(t, ConditionUnexpectedCondition,
		ConditionKeyForObservationIssue("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, ConditionKeyForObservationIssue("", ScopeKey{}))
}

// Validates: R-2.14.3, R-6.8.16
func TestConditionKeyForRetryWork_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionRateLimited,
		ConditionKeyForRetryWork(IssueRateLimited, ScopeKey{}))
	assert.Equal(t, ConditionQuotaExceeded,
		ConditionKeyForRetryWork("", SKQuotaOwn()))
	assert.Equal(t, ConditionServiceOutage,
		ConditionKeyForRetryWork("custom_issue", SKService()))
	assert.Equal(t, ConditionUnexpectedCondition,
		ConditionKeyForRetryWork("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, ConditionKeyForRetryWork("", ScopeKey{}))
}

// Validates: R-2.10.45, R-6.8.16
func TestConditionKeyForBlockScope_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionServiceOutage,
		ConditionKeyForBlockScope(IssueServiceOutage, SKService()))
	assert.Equal(t, ConditionRateLimited,
		ConditionKeyForBlockScope("", SKThrottleDrive(driveid.New("0000000000000001"))))
	assert.Equal(t, ConditionQuotaExceeded,
		ConditionKeyForBlockScope("custom_issue", SKQuotaOwn()))
	assert.Equal(t, ConditionUnexpectedCondition,
		ConditionKeyForBlockScope("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, ConditionKeyForBlockScope("", ScopeKey{}))
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
