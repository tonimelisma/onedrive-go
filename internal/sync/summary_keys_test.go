package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.14.3, R-6.8.16
func TestSummaryKeyForObservationIssue_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryInvalidFilename,
		SummaryKeyForObservationIssue(IssueInvalidFilename, ScopeKey{}))
	assert.Equal(t, SummaryRemoteWriteDenied,
		SummaryKeyForObservationIssue(IssueRemoteWriteDenied, ScopeKey{}))
	assert.Equal(t, SummaryServiceOutage,
		SummaryKeyForObservationIssue("", SKService()))
	assert.Equal(t, SummaryQuotaExceeded,
		SummaryKeyForObservationIssue("custom_issue", SKQuotaOwn()))
	assert.Equal(t, SummaryUnexpectedCondition,
		SummaryKeyForObservationIssue("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, SummaryKeyForObservationIssue("", ScopeKey{}))
}

// Validates: R-2.14.3, R-6.8.16
func TestSummaryKeyForRetryWork_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryRateLimited,
		SummaryKeyForRetryWork(IssueRateLimited, ScopeKey{}))
	assert.Equal(t, SummaryQuotaExceeded,
		SummaryKeyForRetryWork("", SKQuotaOwn()))
	assert.Equal(t, SummaryServiceOutage,
		SummaryKeyForRetryWork("custom_issue", SKService()))
	assert.Equal(t, SummaryUnexpectedCondition,
		SummaryKeyForRetryWork("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, SummaryKeyForRetryWork("", ScopeKey{}))
}

// Validates: R-2.10.45, R-6.8.16
func TestSummaryKeyForBlockScope_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryServiceOutage,
		SummaryKeyForBlockScope(IssueServiceOutage, SKService()))
	assert.Equal(t, SummaryRateLimited,
		SummaryKeyForBlockScope("", SKThrottleDrive(driveid.New("0000000000000001"))))
	assert.Equal(t, SummaryQuotaExceeded,
		SummaryKeyForBlockScope("custom_issue", SKQuotaOwn()))
	assert.Equal(t, SummaryUnexpectedCondition,
		SummaryKeyForBlockScope("custom_issue", ScopeKey{Kind: ScopeKeyKind(99)}))
	assert.Empty(t, SummaryKeyForBlockScope("", ScopeKey{}))
}

// Validates: R-6.8.16
func TestSummaryKeyForIssueType_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryAuthenticationRequired, mustSummaryKeyForIssueType(t, IssueUnauthorized))
	assert.Equal(t, SummaryQuotaExceeded, mustSummaryKeyForIssueType(t, IssueQuotaExceeded))
	assert.Equal(t, SummaryRemoteReadDenied, mustSummaryKeyForIssueType(t, IssueRemoteReadDenied))
	assert.Equal(t, SummaryLocalReadDenied, mustSummaryKeyForIssueType(t, IssueLocalReadDenied))
	assert.Equal(t, SummaryLocalWriteDenied, mustSummaryKeyForIssueType(t, IssueLocalWriteDenied))
	assert.Equal(t, SummaryInvalidFilename, mustSummaryKeyForIssueType(t, IssueInvalidFilename))
	assert.Equal(t, SummaryPathTooLong, mustSummaryKeyForIssueType(t, IssuePathTooLong))
	assert.Equal(t, SummaryFileTooLarge, mustSummaryKeyForIssueType(t, IssueFileTooLarge))
	assert.Equal(t, SummaryCaseCollision, mustSummaryKeyForIssueType(t, IssueCaseCollision))
	assert.Equal(t, SummaryDiskFull, mustSummaryKeyForIssueType(t, IssueDiskFull))
	assert.Equal(t, SummaryFileTooLargeForSpace, mustSummaryKeyForIssueType(t, IssueFileTooLargeForSpace))

	key, ok := summaryKeyForIssueType("custom_issue")
	assert.False(t, ok)
	assert.Empty(t, key)
}

func mustSummaryKeyForIssueType(t *testing.T, issueType string) SummaryKey {
	t.Helper()

	key, ok := summaryKeyForIssueType(issueType)
	assert.True(t, ok)

	return key
}
