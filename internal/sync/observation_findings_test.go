package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.2, R-2.10.4
func TestLocalObservationFindingsBatchFromSkippedItems_UnreadableDirectoryCreatesBoundaryIssue(t *testing.T) {
	t.Parallel()

	batch := localObservationFindingsBatchFromSkippedItems(driveid.New(testDriveID), []skippedItem{{
		Path:               "Private",
		Reason:             issueLocalReadDenied,
		Detail:             "directory not accessible",
		BlocksReadBoundary: true,
	}})

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "Private", batch.Issues[0].Path)
	assert.Equal(t, issueLocalReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, sKPermLocalRead("Private"), batch.Issues[0].ScopeKey)
	assert.ElementsMatch(t, []string{
		IssueInvalidFilename,
		issuePathTooLong,
		issueFileTooLarge,
		issueCaseCollision,
		issueLocalReadDenied,
		issueHashPanic,
	}, batch.ManagedIssueTypes)
}

// Validates: R-2.1.2, R-2.10.4
func TestRootRemoteReadDeniedObservationFindingsBatch_CreatesBoundaryIssue(t *testing.T) {
	t.Parallel()

	batch := rootRemoteReadDeniedObservationFindingsBatch(driveid.New(testDriveID))

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "/", batch.Issues[0].Path)
	assert.Equal(t, issueRemoteReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, sKPermRemoteRead(""), batch.Issues[0].ScopeKey)
	assert.Equal(t, []string{issueRemoteReadDenied}, batch.ManagedIssueTypes)
}
