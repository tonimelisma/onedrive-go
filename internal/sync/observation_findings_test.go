package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.2, R-2.10.4
func TestLocalObservationFindingsBatchFromSkippedItems_UnreadableDirectoryCreatesBoundaryIssueAndReadScope(t *testing.T) {
	t.Parallel()

	batch := localObservationFindingsBatchFromSkippedItems(driveid.New(testDriveID), []SkippedItem{{
		Path:            "Private",
		Reason:          IssueLocalReadDenied,
		Detail:          "directory not accessible",
		BlocksReadScope: true,
	}})

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "Private", batch.Issues[0].Path)
	assert.Equal(t, IssueLocalReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, SKPermLocalRead("Private"), batch.Issues[0].ScopeKey)
	assert.Equal(t, []ScopeKey{SKPermLocalRead("Private")}, batch.ReadScopes)
	assert.ElementsMatch(t, []string{
		IssueInvalidFilename,
		IssuePathTooLong,
		IssueFileTooLarge,
		IssueCaseCollision,
		IssueLocalReadDenied,
		IssueHashPanic,
	}, batch.ManagedIssueTypes)
	assert.Equal(t, []ScopeKeyKind{ScopePermDirRead}, batch.ManagedReadScopeKinds)
}

// Validates: R-2.1.2, R-2.10.4
func TestSinglePathObservationFindingsBatch_UnreadableDescendantKeepsBoundaryIssueDerived(t *testing.T) {
	t.Parallel()

	batch, ok := singlePathObservationFindingsBatch(
		driveid.New(testDriveID),
		"Private/sub/file.txt",
		&SinglePathObservation{
			Skipped: &SkippedItem{
				Path:            "Private",
				Reason:          IssueLocalReadDenied,
				Detail:          "directory not accessible",
				BlocksReadScope: true,
			},
		},
	)
	require.True(t, ok)

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "Private", batch.Issues[0].Path, "single-path observation should persist the denied boundary, not the descendant")
	assert.Equal(t, SKPermLocalRead("Private"), batch.Issues[0].ScopeKey)
	assert.Equal(t, []ScopeKey{SKPermLocalRead("Private")}, batch.ReadScopes)
	assert.ElementsMatch(t, []string{"Private/sub/file.txt", "Private"}, batch.ManagedPaths)
	assert.ElementsMatch(t, []ScopeKey{
		SKPermLocalRead("Private/sub/file.txt"),
		SKPermLocalRead("Private"),
	}, batch.ManagedReadScopes)
}

// Validates: R-2.1.2, R-2.10.4
func TestRootRemoteReadDeniedObservationFindingsBatch_CreatesBoundaryIssueAndReadScope(t *testing.T) {
	t.Parallel()

	batch := rootRemoteReadDeniedObservationFindingsBatch(driveid.New(testDriveID), assert.AnError)

	require.Len(t, batch.Issues, 1)
	assert.Equal(t, "/", batch.Issues[0].Path)
	assert.Equal(t, IssueRemoteReadDenied, batch.Issues[0].IssueType)
	assert.Equal(t, SKPermRemoteRead(""), batch.Issues[0].ScopeKey)
	assert.Equal(t, []ScopeKey{SKPermRemoteRead("")}, batch.ReadScopes)
	assert.Equal(t, []string{IssueRemoteReadDenied}, batch.ManagedIssueTypes)
	assert.Equal(t, []ScopeKeyKind{ScopePermRemoteRead}, batch.ManagedReadScopeKinds)
}
