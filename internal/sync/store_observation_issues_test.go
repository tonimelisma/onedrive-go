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
		ManagedIssueTypes: []string{issue.IssueType},
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
	issueType string,
	scopeKey ScopeKey,
) {
	t.Helper()

	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       path,
		DriveID:    driveid.New(testDriveID),
		ActionType: ActionUpload,
		IssueType:  issueType,
		Error:      issueType,
		ScopeKey:   scopeKey,
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
			DriveID:    driveid.New(testDriveID),
			ActionType: ActionUpload,
			IssueType:  IssueInvalidFilename,
		}},
		ManagedIssueTypes: []string{IssueInvalidFilename},
	}, time.Now().UTC())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing path")
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_ReplacesManagedIssueSet(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	seedObservationIssueForTest(t, store, "old-invalid.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "old-private", IssueLocalReadDenied, SKPermLocalRead("old-private"))

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{
			{
				Path:       "keep-invalid.txt",
				DriveID:    driveid.New(testDriveID),
				ActionType: ActionUpload,
				IssueType:  IssueInvalidFilename,
				Error:      "invalid",
			},
			{
				Path:       "Private",
				DriveID:    driveid.New(testDriveID),
				ActionType: ActionUpload,
				IssueType:  IssueLocalReadDenied,
				Error:      "directory not accessible",
				ScopeKey:   SKPermLocalRead("Private"),
			},
		},
		ManagedIssueTypes: []string{IssueInvalidFilename, IssueLocalReadDenied},
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
			Path:       "Private/file.txt",
			DriveID:    driveid.New(testDriveID),
			ActionType: ActionUpload,
			IssueType:  IssueLocalReadDenied,
			Error:      "file not accessible",
		}},
		ManagedIssueTypes: []string{IssueLocalReadDenied},
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

	seedObservationIssueForTest(t, store, "Private", IssueLocalReadDenied, SKPermLocalRead("Private"))
	seedObservationIssueForTest(t, store, "/", IssueRemoteReadDenied, SKPermRemoteRead(""))

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:       "/",
			DriveID:    driveid.New(testDriveID),
			ActionType: ActionDownload,
			IssueType:  IssueRemoteReadDenied,
			Error:      "remote root unreadable",
			ScopeKey:   SKPermRemoteRead(""),
		}},
		ManagedIssueTypes: []string{IssueRemoteReadDenied},
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

	seedObservationIssueForTest(t, store, "Private/file.txt", IssueLocalReadDenied, ScopeKey{})
	seedObservationIssueForTest(t, store, "Other/file.txt", IssueLocalReadDenied, ScopeKey{})

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		ManagedIssueTypes: []string{IssueLocalReadDenied},
		ManagedPaths:      []string{"Private/file.txt"},
	}, now))

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "Other/file.txt", rows[0].Path)
	assert.Equal(t, IssueLocalReadDenied, rows[0].IssueType)
}
