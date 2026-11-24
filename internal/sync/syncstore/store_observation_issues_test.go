package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.5.2
func TestSyncStore_ReconcileObservationFindings_RejectsInvalidIssueInput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)

	err := store.ReconcileObservationFindings(t.Context(), &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{{
			DriveID:   driveid.New(syncfixture.TestDriveID),
			IssueType: syncdomain.IssueInvalidFilename,
		}},
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueInvalidFilename},
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

	seedObservationIssueForTest(t, store, "kept.txt", syncdomain.IssueInvalidFilename, syncdomain.ScopeKey{})
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key) VALUES (?, ?, ?)`,
		"renamed.txt", "invalid_name", "")
	require.NoError(t, err)

	_, err = store.ListObservationIssues(ctx)
	require.ErrorContains(t, err, `unknown issue type "invalid_name"`)
	require.ErrorContains(t, err, "renamed.txt")

	err = store.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueInvalidFilename},
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
	unknown := syncdomain.IssueType("invalid_name")

	err := store.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{{
			Path:      "renamed.txt",
			DriveID:   driveid.New(syncfixture.TestDriveID),
			IssueType: unknown,
		}},
		ManagedIssueTypes: []syncdomain.IssueType{unknown},
	})
	require.ErrorContains(t, err, `unknown issue type "invalid_name"`)

	var count int
	require.NoError(t, store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM observation_issues`).Scan(&count))
	assert.Zero(t, count)
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_ReplacesManagedIssueSet(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	seedObservationIssueForTest(t, store, "old-invalid.txt", syncdomain.IssueInvalidFilename, syncdomain.ScopeKey{})
	seedObservationIssueForTest(t, store, "old-private", syncdomain.IssueLocalReadDenied, syncdomain.SKPermLocalRead("old-private"))

	require.NoError(t, store.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{
			{
				Path:      "keep-invalid.txt",
				DriveID:   driveid.New(syncfixture.TestDriveID),
				IssueType: syncdomain.IssueInvalidFilename,
			},
			{
				Path:      "Private",
				DriveID:   driveid.New(syncfixture.TestDriveID),
				IssueType: syncdomain.IssueLocalReadDenied,
				ScopeKey:  syncdomain.SKPermLocalRead("Private"),
			},
		},
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueInvalidFilename, syncdomain.IssueLocalReadDenied},
	}))

	assert.ElementsMatch(t, []string{"Private", "keep-invalid.txt"}, observationIssuePathsForTest(t, store))
}

// Validates: R-2.5.2, R-2.10.4
func TestSyncStore_ReconcileObservationFindings_FileReadDeniedDoesNotCreateReadBoundaryScopeRow(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{{
			Path:      "Private/file.txt",
			DriveID:   driveid.New(syncfixture.TestDriveID),
			IssueType: syncdomain.IssueLocalReadDenied,
		}},
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueLocalReadDenied},
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

	seedObservationIssueForTest(t, store, "Private", syncdomain.IssueLocalReadDenied, syncdomain.SKPermLocalRead("Private"))
	seedObservationIssueForTest(t, store, "/", syncdomain.IssueRemoteReadDenied, syncdomain.SKPermRemoteRead(""))

	require.NoError(t, store.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{{
			Path:      "/",
			DriveID:   driveid.New(syncfixture.TestDriveID),
			IssueType: syncdomain.IssueRemoteReadDenied,
			ScopeKey:  syncdomain.SKPermRemoteRead(""),
		}},
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueRemoteReadDenied},
	}))

	assert.ElementsMatch(t, []string{"/", "Private"}, observationIssuePathsForTest(t, store))
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
	seedObservationIssueForTest(t, store, "kept.txt", syncdomain.IssueInvalidFilename, syncdomain.ScopeKey{})

	err := store.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{{
			Path:      "Docs/.onedrive-go.0123456789abcdef.detach",
			DriveID:   driveid.New(syncfixture.TestDriveID),
			IssueType: syncdomain.IssueLocalInterruptedRemoval,
		}},
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueInvalidFilename},
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

	seedObservationIssueForTest(t, store, "Private/file.txt", syncdomain.IssueLocalReadDenied, syncdomain.ScopeKey{})
	seedObservationIssueForTest(t, store, "Other/file.txt", syncdomain.IssueLocalReadDenied, syncdomain.ScopeKey{})
	require.ElementsMatch(t, []string{"Other/file.txt", "Private/file.txt"}, observationIssuePathsForTest(t, store),
		"seeding one issue keeps the stored issues of its type")

	require.NoError(t, store.ReconcileObservationFindings(ctx, &syncdomain.ObservationFindingsBatch{
		Issues: []syncdomain.ObservationIssue{{
			Path:      "Other/file.txt",
			DriveID:   driveid.New(syncfixture.TestDriveID),
			IssueType: syncdomain.IssueLocalReadDenied,
		}},
		ManagedIssueTypes: []syncdomain.IssueType{syncdomain.IssueLocalReadDenied},
	}))

	assert.Equal(t, []string{"Other/file.txt"}, observationIssuePathsForTest(t, store))
}
