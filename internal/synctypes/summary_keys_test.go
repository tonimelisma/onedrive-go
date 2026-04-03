package synctypes

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/failures"
)

// Validates: R-6.8.16, R-6.6.11
func TestDescribeSummary_KnownKeys(t *testing.T) {
	t.Parallel()

	keys := []SummaryKey{
		SummaryConflictUnresolved,
		SummaryAuthenticationRequired,
		SummaryQuotaExceeded,
		SummaryServiceOutage,
		SummaryRateLimited,
		SummarySharedFolderWritesBlocked,
		SummaryLocalPermissionDenied,
		SummaryRemotePermissionDenied,
		SummaryInvalidFilename,
		SummaryPathTooLong,
		SummaryFileTooLarge,
		SummaryFileTooLargeForSpace,
		SummaryHeldDeletes,
		SummarySyncFailure,
	}

	for _, key := range keys {
		descriptor := DescribeSummary(key)
		assert.Equal(t, key, descriptor.Key)
		assert.NotEmpty(t, descriptor.Title)
		assert.NotEmpty(t, descriptor.Reason)
		assert.NotEmpty(t, descriptor.Action)
		assert.NotEmpty(t, descriptor.LogSummary)
	}
}

// Validates: R-6.8.16
func TestSummaryKeyForRuntime_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryAuthenticationRequired,
		SummaryKeyForRuntime(failures.ClassFatal, IssueUnauthorized))
	assert.Equal(t, SummaryServiceOutage,
		SummaryKeyForRuntime(failures.ClassRetryableTransient, IssueServiceOutage))
	assert.Equal(t, SummaryRateLimited,
		SummaryKeyForRuntime(failures.ClassScopeBlockingTransient, IssueRateLimited))
	assert.Equal(t, SummarySyncFailure,
		SummaryKeyForRuntime(failures.ClassActionable, ""))
}

// Validates: R-2.14.3, R-6.8.16
func TestSummaryKeyForPersistedFailure_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, SummaryInvalidFilename,
		SummaryKeyForPersistedFailure(IssueInvalidFilename, CategoryActionable, FailureRoleItem))
	assert.Equal(t, SummarySharedFolderWritesBlocked,
		SummaryKeyForPersistedFailure(IssueSharedFolderBlocked, CategoryTransient, FailureRoleHeld))
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
