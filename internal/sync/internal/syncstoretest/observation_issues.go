package syncstoretest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// SeedObservationIssueRowForTest adds one issue to the stored set and keeps
// every other. A batch replaces all stored issues of the types it manages, so
// the batch reports the stored issues of the seeded type again next to the
// new one.
func SeedObservationIssueRowForTest(t *testing.T, store *syncstore.SyncStore, issue *syncdomain.ObservationIssue) {
	t.Helper()

	rows, err := store.ListObservationIssues(t.Context())
	require.NoError(t, err)
	batch := syncdomain.ObservationFindingsBatch{
		Issues:            []syncdomain.ObservationIssue{*issue},
		ManagedIssueTypes: []syncdomain.IssueType{issue.IssueType},
	}
	for i := range rows {
		if rows[i].IssueType != issue.IssueType || rows[i].Path == issue.Path {
			continue
		}
		batch.Issues = append(batch.Issues, syncdomain.ObservationIssue{
			Path:      rows[i].Path,
			DriveID:   rows[i].DriveID,
			IssueType: rows[i].IssueType,
			ScopeKey:  rows[i].ScopeKey,
		})
	}
	require.NoError(t, store.ReconcileObservationFindings(t.Context(), &batch))
}

func SeedObservationIssueForTest(
	t *testing.T,
	store *syncstore.SyncStore,
	path string,
	issueType syncdomain.IssueType,
	scopeKey syncdomain.ScopeKey,
) {
	t.Helper()

	SeedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
		Path:      path,
		DriveID:   driveid.New(syncfixture.TestDriveID),
		IssueType: issueType,
		ScopeKey:  scopeKey,
	})
}

func ObservationIssuePathsForTest(t *testing.T, store *syncstore.SyncStore) []string {
	t.Helper()

	rows, err := store.ListObservationIssues(t.Context())
	require.NoError(t, err)

	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}

	return paths
}
