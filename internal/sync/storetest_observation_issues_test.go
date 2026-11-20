package sync

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// seedObservationIssueRowForTest adds one issue to the stored set and keeps
// every other. A batch replaces all stored issues of the types it manages, so
// the batch reports the stored issues of the seeded type again next to the
// new one.
func seedObservationIssueRowForTest(t *testing.T, store *SyncStore, issue *ObservationIssue) {
	t.Helper()

	rows, err := store.ListObservationIssues(t.Context())
	require.NoError(t, err)
	batch := ObservationFindingsBatch{
		Issues:            []ObservationIssue{*issue},
		ManagedIssueTypes: []IssueType{issue.IssueType},
	}
	for i := range rows {
		if rows[i].IssueType != issue.IssueType || rows[i].Path == issue.Path {
			continue
		}
		batch.Issues = append(batch.Issues, ObservationIssue{
			Path:      rows[i].Path,
			DriveID:   rows[i].DriveID,
			IssueType: rows[i].IssueType,
			ScopeKey:  rows[i].ScopeKey,
		})
	}
	require.NoError(t, store.ReconcileObservationFindings(t.Context(), &batch))
}

func seedObservationIssueForTest(
	t *testing.T,
	store *SyncStore,
	path string,
	issueType IssueType,
	scopeKey ScopeKey,
) {
	t.Helper()

	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      path,
		DriveID:   driveid.New(testDriveID),
		IssueType: issueType,
		ScopeKey:  scopeKey,
	})
}

func observationIssuePathsForTest(t *testing.T, store *SyncStore) []string {
	t.Helper()

	rows, err := store.ListObservationIssues(t.Context())
	require.NoError(t, err)

	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}
