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

	batch := localObservationFindingsBatchFromSkippedItems(driveid.New(testDriveID), []SkippedItem{{
		Path:               "Private",
		Reason:             IssueLocalReadDenied,
		Detail:             "directory not accessible",
		BlocksReadBoundary: true,
	}})

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "Private", batch.Issues[0].Path)
	assert.Equal(t, IssueLocalReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, SKPermLocalRead("Private"), batch.Issues[0].ScopeKey)
	assert.ElementsMatch(t, []string{
		IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueCaseCollision,
		IssueLocalReadDenied,
		IssueHashPanic,
	}, batch.ManagedIssueTypes)
}

// Validates: R-2.1.2, R-2.10.4
func TestSinglePathObservationFindingsBatch_UnreadableDescendantKeepsBoundaryIssueDerived(t *testing.T) {
	t.Parallel()

	batch, ok := singlePathObservationFindingsBatch(
		driveid.New(testDriveID),
		"Private/sub/file.txt",
		&SinglePathObservation{
			Skipped: &SkippedItem{
				Path:               "Private",
				Reason:             IssueLocalReadDenied,
				Detail:             "directory not accessible",
				BlocksReadBoundary: true,
			},
		},
	)
	require.True(t, ok)

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "Private", batch.Issues[0].Path, "single-path observation should persist the denied boundary, not the descendant")
	assert.Equal(t, SKPermLocalRead("Private"), batch.Issues[0].ScopeKey)
	assert.ElementsMatch(t, []string{"Private/sub/file.txt", "Private"}, batch.ManagedPaths)
}

// Validates: R-2.1.2, R-2.10.4
func TestRootRemoteReadDeniedObservationFindingsBatch_CreatesBoundaryIssue(t *testing.T) {
	t.Parallel()

	batch := rootRemoteReadDeniedObservationFindingsBatch(driveid.New(testDriveID), assert.AnError)

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "/", batch.Issues[0].Path)
	assert.Equal(t, IssueRemoteReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, SKPermRemoteRead(""), batch.Issues[0].ScopeKey)
	assert.Equal(t, []string{IssueRemoteReadDenied}, batch.ManagedIssueTypes)
}
