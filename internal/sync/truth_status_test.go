package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusByPath_ReturnsAvailableStatusForUnblockedPaths(t *testing.T) {
	t.Parallel()

	statuses := NewTruthAvailabilityIndex(nil, nil).StatusByPath([]string{"docs/readme.txt"})
	require.Len(t, statuses, 1)

	status, ok := statuses["docs/readme.txt"]
	require.True(t, ok)
	assert.True(t, status.Local.IsAvailable())
	assert.True(t, status.Remote.IsAvailable())
}

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusByPath_ReadScopesApplyToDescendants(t *testing.T) {
	t.Parallel()

	statuses := NewTruthAvailabilityIndex(
		nil,
		[]*BlockScope{
			{
				Key:           SKPermLocalRead("Private"),
				ConditionType: IssueLocalReadDenied,
				TimingSource:  ScopeTimingNone,
				BlockedAt:     time.Unix(100, 0),
			},
			{
				Key:           SKPermRemoteRead("Shared"),
				ConditionType: IssueRemoteReadDenied,
				TimingSource:  ScopeTimingNone,
				BlockedAt:     time.Unix(100, 0),
			},
		},
	).StatusByPath(
		[]string{"Private/sub/file.txt", "Shared/Docs/file.txt"},
	)

	localStatus, ok := statuses["Private/sub/file.txt"]
	require.True(t, ok)
	assert.Equal(t, TruthAvailabilityBlockedLocalReadScope, localStatus.Local.Availability)
	assert.Equal(t, PathTruthSourceReadScope, localStatus.Local.Source)
	assert.Equal(t, SKPermLocalRead("Private"), localStatus.Local.ScopeKey)
	assert.True(t, localStatus.Remote.IsAvailable())

	remoteStatus, ok := statuses["Shared/Docs/file.txt"]
	require.True(t, ok)
	assert.Equal(t, TruthAvailabilityBlockedRemoteReadScope, remoteStatus.Remote.Availability)
	assert.Equal(t, PathTruthSourceReadScope, remoteStatus.Remote.Source)
	assert.Equal(t, SKPermRemoteRead("Shared"), remoteStatus.Remote.ScopeKey)
	assert.True(t, remoteStatus.Local.IsAvailable())
}

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusForPath_UsesObservationAndReadScopeEvidence(t *testing.T) {
	t.Parallel()

	index := NewTruthAvailabilityIndex(
		[]ObservationIssueRow{
			{Path: "blocked-local.txt", IssueType: IssueInvalidFilename},
		},
		[]*BlockScope{
			{
				Key:           SKPermRemoteRead("Shared"),
				ConditionType: IssueRemoteReadDenied,
				TimingSource:  ScopeTimingNone,
				BlockedAt:     time.Unix(100, 0),
			},
		},
	)

	localStatus := index.StatusForPath("blocked-local.txt")
	assert.Equal(t, TruthAvailabilityBlockedObservationIssue, localStatus.Local.Availability)
	assert.Equal(t, PathTruthSourceObservationIssue, localStatus.Local.Source)
	assert.Equal(t, IssueInvalidFilename, localStatus.Local.IssueType)
	assert.True(t, localStatus.Remote.IsAvailable())

	remoteStatus := index.StatusForPath("Shared/Docs/file.txt")
	assert.Equal(t, TruthAvailabilityBlockedRemoteReadScope, remoteStatus.Remote.Availability)
	assert.Equal(t, PathTruthSourceReadScope, remoteStatus.Remote.Source)
	assert.Equal(t, SKPermRemoteRead("Shared"), remoteStatus.Remote.ScopeKey)
	assert.True(t, remoteStatus.Local.IsAvailable())
}
