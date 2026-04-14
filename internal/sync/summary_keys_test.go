package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.14.3, R-6.8.16
func TestSummaryKeyForPersistedFailure_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryInvalidFilename,
		SummaryKeyForPersistedFailure(IssueInvalidFilename, CategoryActionable, FailureRoleItem))
	assert.Equal(t, SummaryRemoteWriteDenied,
		SummaryKeyForPersistedFailure(IssueRemoteWriteDenied, CategoryTransient, FailureRoleHeld))
	assert.Equal(t, SummarySyncFailure,
		SummaryKeyForPersistedFailure("", CategoryActionable, FailureRoleItem))
}

// Validates: R-2.10.45, R-6.8.16
func TestSummaryKeyForScopeBlock_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryAuthenticationRequired,
		SummaryKeyForScopeBlock(IssueUnauthorized, SKAuthAccount()))
	assert.Equal(t, SummaryServiceOutage,
		SummaryKeyForScopeBlock(IssueServiceOutage, SKService()))
	assert.Equal(t, SummaryRateLimited,
		SummaryKeyForScopeBlock("", SKThrottleAccount()))
}

// Validates: R-6.8.16
func TestSummaryKeyForIssueType_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryAuthenticationRequired, mustSummaryKeyForIssueType(t, IssueUnauthorized))
	assert.Equal(t, SummaryQuotaExceeded, mustSummaryKeyForIssueType(t, IssueQuotaExceeded))
	assert.Equal(t, SummaryInvalidFilename, mustSummaryKeyForIssueType(t, IssueInvalidFilename))
	assert.Equal(t, SummaryDiskFull, mustSummaryKeyForIssueType(t, IssueDiskFull))
}

func mustSummaryKeyForIssueType(t *testing.T, issueType string) SummaryKey {
	t.Helper()

	key, ok := summaryKeyForIssueType(issueType)
	assert.True(t, ok)

	return key
}
