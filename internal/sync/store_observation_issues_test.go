package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	})
	require.ErrorContains(t, err, "missing path")
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
	})
	require.ErrorContains(t, err, `unknown issue type "invalid_name"`)
}

// The write refuses an issue type the read would refuse, and the refused
// batch leaves the table as it was. The batch manages the type it reports,
// so the refusal is the write's, not the managed-type rule's.
// Validates: R-2.5.6
func TestSyncStore_ReconcileObservationFindings_RefusesToWriteAnUnknownIssueType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	unknown := IssueType("invalid_name")

	err := store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "renamed.txt",
			DriveID:   driveid.New(testDriveID),
			IssueType: unknown,
		}},
		ManagedIssueTypes: []IssueType{unknown},
	})
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
	}))

	assert.ElementsMatch(t, []string{"Private", "keep-invalid.txt"}, observationIssuePathsForTest(t, store))
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_FileReadDeniedDoesNotCreateReadBoundaryScopeRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "Private/file.txt",
			DriveID:   driveid.New(testDriveID),
			IssueType: issueLocalReadDenied,
		}},
		ManagedIssueTypes: []IssueType{issueLocalReadDenied},
	}))

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
	}))

	assert.ElementsMatch(t, []string{"/", "Private"}, observationIssuePathsForTest(t, store))
}

// interruptedRemovalStatusPaths returns the paths status lists under the
// interrupted-removal condition, read the way `status` reads them: the drive
// snapshot projected into condition groups.
func interruptedRemovalStatusPaths(t *testing.T, store *SyncStore) []string {
	t.Helper()

	snapshot, err := store.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	groups := ProjectStoredConditionGroups(&snapshot)
	for i := range groups {
		if groups[i].ConditionKey == ConditionLocalInterrupted {
			return groups[i].Paths
		}
	}

	return nil
}

// Every full scan that finds the leftover of an interrupted removal reports
// it, so the scan that no longer finds it has proven it gone: the local
// findings batch clears the condition and status stops listing it. A scan
// that still finds it keeps it, and that re-report is not a change that
// replans (R-2.8.21). The condition used to be left out of the batch's
// managed types and outlived the leftover for good.
//
// Validates: R-6.4.9, R-2.8.21
func TestSyncStore_ReconcileObservationFindings_InterruptedRemovalLastsAsLongAsTheLeftover(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	const leftoverPath = "Docs/.onedrive-go.0123456789abcdef.detach"
	leftover := skippedItem{
		Path:   leftoverPath,
		Reason: issueLocalInterruptedRemoval,
		Detail: "sync set this file aside while removing it and was interrupted",
	}
	reconcileScan := func(skipped ...skippedItem) bool {
		batch := localObservationFindingsBatchFromSkippedItems(driveid.New(testDriveID), skipped)
		changed, err := store.ReconcileObservationFindingsReportingChange(ctx, &batch)
		require.NoError(t, err)

		return changed
	}

	assert.True(t, reconcileScan(leftover), "the first scan that finds the leftover records it")
	assert.False(t, reconcileScan(leftover), "a scan that still finds it keeps it, and that is no change")
	assert.Equal(t, []string{leftoverPath}, interruptedRemovalStatusPaths(t, store))

	assert.True(t, reconcileScan(), "the scan that no longer finds the leftover clears its condition")
	assert.Empty(t, observationIssuePathsForTest(t, store))
	assert.Empty(t, interruptedRemovalStatusPaths(t, store), "status no longer lists it")
}

// A batch replaces only the rows of the types it manages, so a row it stored
// for a type it does not manage would outlive every scan that stops
// reporting it: that is how an interrupted removal's report outlived the
// file. Such a batch is refused naming the type, and the table is left as it
// was.
//
// Validates: R-2.10.10, R-6.4.9
func TestSyncStore_ReconcileObservationFindings_RefusesAnIssueOfATypeTheBatchDoesNotManage(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	seedObservationIssueForTest(t, store, "kept.txt", IssueInvalidFilename, ScopeKey{})

	err := store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "Docs/.onedrive-go.0123456789abcdef.detach",
			DriveID:   driveid.New(testDriveID),
			IssueType: issueLocalInterruptedRemoval,
		}},
		ManagedIssueTypes: []IssueType{IssueInvalidFilename},
	})
	require.ErrorContains(t, err, `"local_interrupted_removal"`)
	require.ErrorContains(t, err, "Docs/.onedrive-go.0123456789abcdef.detach")

	assert.Equal(t, []string{"kept.txt"}, observationIssuePathsForTest(t, store),
		"the refused batch neither stores its issue nor clears the managed row")
}

// A batch is a whole pass's findings for its managed types: a stored issue
// of a managed type it does not report again is cleared wherever it is, and
// one it reports again is kept.
//
// Validates: R-2.5.2, R-2.10.10
func TestSyncStore_ReconcileObservationFindings_ClearsEveryUnreportedIssueOfAManagedType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	seedObservationIssueForTest(t, store, "Private/file.txt", issueLocalReadDenied, ScopeKey{})
	seedObservationIssueForTest(t, store, "Other/file.txt", issueLocalReadDenied, ScopeKey{})
	require.ElementsMatch(t, []string{"Other/file.txt", "Private/file.txt"}, observationIssuePathsForTest(t, store),
		"seeding one issue keeps the stored issues of its type")

	require.NoError(t, store.ReconcileObservationFindings(ctx, &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "Other/file.txt",
			DriveID:   driveid.New(testDriveID),
			IssueType: issueLocalReadDenied,
		}},
		ManagedIssueTypes: []IssueType{issueLocalReadDenied},
	}))

	assert.Equal(t, []string{"Other/file.txt"}, observationIssuePathsForTest(t, store))
}
