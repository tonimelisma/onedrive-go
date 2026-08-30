package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusByPath_ReturnsAvailableStatusForUnblockedPaths(t *testing.T) {
	t.Parallel()

	statuses := newTruthAvailabilityIndex(nil).StatusByPath([]string{"docs/readme.txt"})
	require.Len(t, statuses, 1)

	status, ok := statuses["docs/readme.txt"]
	require.True(t, ok)
	assert.True(t, status.Local.IsAvailable())
	assert.True(t, status.Remote.IsAvailable())
}

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusByPath_ReadBoundariesApplyToDescendants(t *testing.T) {
	t.Parallel()

	statuses := newTruthAvailabilityIndex(
		[]ObservationIssueRow{
			{Path: "Private", IssueType: issueLocalReadDenied, ScopeKey: sKPermLocalRead("Private")},
			{Path: "Shared", IssueType: issueRemoteReadDenied, ScopeKey: sKPermRemoteRead("Shared")},
		},
	).StatusByPath(
		[]string{"Private/sub/file.txt", "Shared/Docs/file.txt"},
	)

	localStatus, ok := statuses["Private/sub/file.txt"]
	require.True(t, ok)
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, localStatus.Local.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, localStatus.Local.Source)
	assert.Equal(t, sKPermLocalRead("Private"), localStatus.Local.ScopeKey)
	assert.True(t, localStatus.Remote.IsAvailable())

	remoteStatus, ok := statuses["Shared/Docs/file.txt"]
	require.True(t, ok)
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, remoteStatus.Remote.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, remoteStatus.Remote.Source)
	assert.Equal(t, sKPermRemoteRead("Shared"), remoteStatus.Remote.ScopeKey)
	assert.True(t, remoteStatus.Local.IsAvailable())
}

// Validates: R-2.1.3, R-2.10.4
func TestTruthAvailabilityIndex_StatusForPath_UsesObservationEvidence(t *testing.T) {
	t.Parallel()

	index := newTruthAvailabilityIndex(
		[]ObservationIssueRow{
			{Path: "blocked-local.txt", IssueType: IssueInvalidFilename},
			{Path: "Shared", IssueType: issueRemoteReadDenied, ScopeKey: sKPermRemoteRead("Shared")},
		},
	)

	localStatus := index.StatusForPath("blocked-local.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, localStatus.Local.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, localStatus.Local.Source)
	assert.Equal(t, IssueInvalidFilename, localStatus.Local.IssueType)
	assert.True(t, localStatus.Remote.IsAvailable())

	remoteStatus := index.StatusForPath("Shared/Docs/file.txt")
	assert.Equal(t, truthAvailabilityBlockedObservationIssue, remoteStatus.Remote.Availability)
	assert.Equal(t, pathTruthSourceObservationIssue, remoteStatus.Remote.Source)
	assert.Equal(t, sKPermRemoteRead("Shared"), remoteStatus.Remote.ScopeKey)
	assert.True(t, remoteStatus.Local.IsAvailable())
}
