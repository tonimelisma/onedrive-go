package sync

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func seedObservationIssueRowForTest(t *testing.T, store *SyncStore, issue *ObservationIssue) {
	t.Helper()

	batch := ObservationFindingsBatch{
		Issues:            []ObservationIssue{*issue},
		ManagedIssueTypes: []IssueType{issue.IssueType},
		ManagedPaths:      []string{issue.Path},
	}
	slices.Sort(batch.ManagedIssueTypes)
	slices.Sort(batch.ManagedPaths)
	require.NoError(t, store.ReconcileObservationFindings(t.Context(), &batch, time.Now().UTC()))
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

// Validates: R-2.5.2
func TestSyncStore_ReconcileObservationFindings_RejectsInvalidIssueInput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	err := store.ReconcileObservationFindings(t.Context(), &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			DriveID:   driveid.New(testDriveID),
			IssueType: IssueInvalidFilename,
		}},
		ManagedIssueTypes: []IssueType{IssueInvalidFilename},
	}, time.Now().UTC())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing path")
}

// A stored issue type no member has fails the read naming the row, instead
// of reaching truth availability, which has no verdict for it. The reconcile
// reads the same rows, so it refuses as well rather than reconciling around a
// row it cannot classify.
// Validates: R-2.5.6
func TestSyncStore_ListObservationIssues_RefusesUnknownStoredIssueType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	seedObservationIssueForTest(t, store, "kept.txt", IssueInvalidFilename, ScopeKey{})
	_, err := store.rawDB().ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key) VALUES (?, ?, ?)`,
		"renamed.txt", "invalid_name", "")
	require.NoError(t, err)

	_, err = store.ListObservationIssues(ctx)
	require.ErrorContains(t, err, `unknown issue type "invalid_name"`)
	require.ErrorContains(t, err, "renamed.txt")

	err = store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		ManagedIssueTypes: []IssueType{IssueInvalidFilename},
	}, time.Now().UTC())
	require.ErrorContains(t, err, `unknown issue type "invalid_name"`)
}

// The write refuses an issue type the read would refuse, and the refused
// batch leaves the table as it was.
// Validates: R-2.5.6
func TestSyncStore_ReconcileObservationFindings_RefusesToWriteAnUnknownIssueType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	err := store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "renamed.txt",
			DriveID:   driveid.New(testDriveID),
			IssueType: IssueType("invalid_name"),
		}},
		ManagedIssueTypes: []IssueType{IssueInvalidFilename},
	}, time.Now().UTC())
	require.ErrorContains(t, err, `unknown issue type "invalid_name"`)

	var count int
	require.NoError(t, store.rawDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM observation_issues`).Scan(&count))
	assert.Zero(t, count)
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_ReplacesManagedIssueSet(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	seedObservationIssueForTest(t, store, "old-invalid.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "old-private", issueLocalReadDenied, sKPermLocalRead("old-private"))

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{
			{
				Path:      "keep-invalid.txt",
				DriveID:   driveid.New(testDriveID),
				IssueType: IssueInvalidFilename,
			},
			{
				Path:      "Private",
				DriveID:   driveid.New(testDriveID),
				IssueType: issueLocalReadDenied,
				ScopeKey:  sKPermLocalRead("Private"),
			},
		},
		ManagedIssueTypes: []IssueType{IssueInvalidFilename, issueLocalReadDenied},
	}, now))

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"Private", "keep-invalid.txt"}, observationIssuePathsForTest(t, store))
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_FileReadDeniedDoesNotCreateReadBoundaryScopeRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 10, 0, 0, time.UTC)

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "Private/file.txt",
			DriveID:   driveid.New(testDriveID),
			IssueType: issueLocalReadDenied,
		}},
		ManagedIssueTypes: []IssueType{issueLocalReadDenied},
	}, now))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].ScopeKey.IsZero())
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_OnlyClearsManagedFamilies(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 15, 0, 0, time.UTC)

	seedObservationIssueForTest(t, store, "Private", issueLocalReadDenied, sKPermLocalRead("Private"))
	seedObservationIssueForTest(t, store, "/", issueRemoteReadDenied, sKPermRemoteRead(""))

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "/",
			DriveID:   driveid.New(testDriveID),
			IssueType: issueRemoteReadDenied,
			ScopeKey:  sKPermRemoteRead(""),
		}},
		ManagedIssueTypes: []IssueType{issueRemoteReadDenied},
	}, now))

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"/", "Private"}, []string{rows[0].Path, rows[1].Path})
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_ManagedPathsOnlyTouchTargetedPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 20, 0, 0, time.UTC)

	seedObservationIssueForTest(t, store, "Private/file.txt", issueLocalReadDenied, ScopeKey{})
	seedObservationIssueForTest(t, store, "Other/file.txt", issueLocalReadDenied, ScopeKey{})

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		ManagedIssueTypes: []IssueType{issueLocalReadDenied},
		ManagedPaths:      []string{"Private/file.txt"},
	}, now))

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Other/file.txt", rows[0].Path)
	assert.Equal(t, issueLocalReadDenied, rows[0].IssueType)
}
