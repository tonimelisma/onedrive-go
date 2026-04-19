package sync

import (
	"testing"

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

// Validates: R-2.5.2
func TestSyncStore_DeleteObservationIssuesByPrefixAndScope(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	scopeKey := SKPermRemote("Shared/Docs")
	seedObservationIssueForTest(t, store, "folder/a.txt", IssueInvalidFilename, ScopeKey{})
	seedObservationIssueForTest(t, store, "folder/sub/b.txt", IssueInvalidFilename, scopeKey)
	seedObservationIssueForTest(t, store, "other/c.txt", IssueInvalidFilename, scopeKey)

	require.NoError(t, store.ClearObservationIssuesByPrefix(t.Context(), "folder", IssueInvalidFilename))
	assert.ElementsMatch(t, []string{"other/c.txt"}, observationIssuePathsForTest(t, store))

	seedObservationIssueForTest(t, store, "scoped/a.txt", IssueInvalidFilename, scopeKey)
	seedObservationIssueForTest(t, store, "scoped/b.txt", IssuePathTooLong, scopeKey)

	require.NoError(t, store.DeleteObservationIssuesByScope(t.Context(), scopeKey))
	assert.Empty(t, observationIssuePathsForTest(t, store))
}
