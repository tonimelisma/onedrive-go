package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func seedObservationIssueForTest(
	t *testing.T,
	store *SyncStore,
	path string,
	issueType string,
	scopeKey ScopeKey,
) {
	t.Helper()

	require.NoError(t, store.UpsertObservationIssue(t.Context(), &ObservationIssue{
		Path:       path,
		DriveID:    driveid.New(testDriveID),
		ActionType: ActionUpload,
		IssueType:  issueType,
		Error:      issueType,
		ScopeKey:   scopeKey,
	}))
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
func TestSyncStore_UpsertObservationIssue_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	err := store.UpsertObservationIssue(t.Context(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil issue")

	err = store.UpsertObservationIssue(t.Context(), &ObservationIssue{
		DriveID:    driveid.New(testDriveID),
		ActionType: ActionUpload,
		IssueType:  IssueInvalidFilename,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing path")
}

// Validates: R-2.5.2
func TestSyncStore_ClearObservationIssuesByPaths(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	seedObservationIssueForTest(t, store, "a.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "b.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "c.txt", IssuePathTooLong, ScopeKey{})

	require.NoError(t, store.ClearObservationIssuesByPaths(
		t.Context(),
		IssueInvalidFilename,
		[]string{"a.txt", "missing.txt"},
	))

	assert.ElementsMatch(t, []string{"b.txt", "c.txt"}, observationIssuePathsForTest(t, store))
}

// Validates: R-2.5.2
func TestSyncStore_ClearResolvedObservationIssues(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	seedObservationIssueForTest(t, store, "keep.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "drop.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "other.txt", IssuePathTooLong, ScopeKey{})

	require.NoError(t, store.ClearResolvedObservationIssues(
		t.Context(),
		IssueInvalidFilename,
		[]string{"keep.txt"},
	))

	assert.ElementsMatch(t, []string{"keep.txt", "other.txt"}, observationIssuePathsForTest(t, store))

	require.NoError(t, store.ClearResolvedObservationIssues(t.Context(), IssueInvalidFilename, nil))
	assert.ElementsMatch(t, []string{"other.txt"}, observationIssuePathsForTest(t, store))
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_ReplacesManagedIssueSet(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	seedObservationIssueForTest(t, store, "old-invalid.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "old-private", IssueLocalReadDenied, SKPermLocalRead("old-private"))

	require.NoError(t, store.ReconcileObservationFindings(ctx, ObservationFindingsBatch{
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
		ReadScopes: []ScopeKey{SKPermLocalRead("Private")},
	}, now))

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"Private", "keep-invalid.txt"}, observationIssuePathsForTest(t, store))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, SKPermLocalRead("Private"), blocks[0].Key)
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_ReleasesMissingReadScopesWithoutBlockedRetryWork(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 5, 0, 0, time.UTC)

	require.NoError(t, store.UpsertBlockScope(ctx, &BlockScope{
		Key:          SKPermLocalRead("Private"),
		IssueType:    IssueLocalReadDenied,
		TimingSource: ScopeTimingNone,
		BlockedAt:    now.Add(-time.Minute),
	}))

	require.NoError(t, store.ReconcileObservationFindings(ctx, ObservationFindingsBatch{}, now))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_FileReadDeniedDoesNotCreateReadScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 19, 10, 10, 0, 0, time.UTC)

	require.NoError(t, store.ReconcileObservationFindings(ctx, ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:       "Private/file.txt",
			DriveID:    driveid.New(testDriveID),
			ActionType: ActionUpload,
			IssueType:  IssueLocalReadDenied,
			Error:      "file not accessible",
		}},
	}, now))

	blocks, err := store.ListBlockScopes(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].ScopeKey.IsZero())
}
