package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.5.2
func TestBuildObservationReconcilePlan_DeletesOnlyManagedCurrentIssues(t *testing.T) {
	t.Parallel()

	plan, err := buildObservationReconcilePlan(
		&ObservationFindingsBatch{
			ManagedIssueTypes: []IssueType{IssueLocalReadDenied},
		},
		observationReconcileStoreState{
			issues: []ObservationIssueRow{
				{Path: "Private/file.txt", IssueType: IssueLocalReadDenied},
				{Path: "Other/file.txt", IssueType: IssueLocalReadDenied},
				{Path: "other-issue.txt", IssueType: IssueInvalidFilename},
			},
		},
	)
	require.NoError(t, err)

	assert.Equal(t, []managedObservationIssueKey{
		{path: "Other/file.txt", issueType: IssueLocalReadDenied},
		{path: "Private/file.txt", issueType: IssueLocalReadDenied},
	}, plan.issueDeletes, "every stored issue of the managed type goes, the other type stays")
}

// Validates: R-2.5.2
func TestBuildObservationReconcilePlan_UpsertsCurrentIssuesOnly(t *testing.T) {
	t.Parallel()

	batch := &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:      "same.txt",
			DriveID:   driveid.New(testDriveID),
			IssueType: IssuePathTooLong,
		}},
		ManagedIssueTypes: []IssueType{IssuePathTooLong},
	}

	plan, err := buildObservationReconcilePlan(batch, observationReconcileStoreState{
		issues: []ObservationIssueRow{{
			Path:      "same.txt",
			IssueType: IssueInvalidFilename,
		}},
	})
	require.NoError(t, err)

	require.Len(t, plan.issueUpserts, 1)
	assert.Equal(t, batch.Issues[0], plan.issueUpserts[0])
	assert.Empty(t, plan.issueDeletes)
}

// Validates: R-2.5.2
func TestBuildObservationReconcilePlan_ReconcilesExactManagedIssueSet(t *testing.T) {
	t.Parallel()

	batch := &ObservationFindingsBatch{
		Issues: []ObservationIssue{
			{
				Path:      "same.txt",
				DriveID:   driveid.New(testDriveID),
				IssueType: IssuePathTooLong,
			},
			{
				Path:      "keep.txt",
				DriveID:   driveid.New(testDriveID),
				IssueType: IssueInvalidFilename,
			},
		},
		ManagedIssueTypes: []IssueType{IssuePathTooLong, IssueInvalidFilename},
	}

	plan, err := buildObservationReconcilePlan(batch, observationReconcileStoreState{
		issues: []ObservationIssueRow{
			{Path: "same.txt", IssueType: IssuePathTooLong},
			{Path: "removed.txt", IssueType: IssueInvalidFilename},
			{Path: "unmanaged.txt", IssueType: IssueLocalReadDenied},
		},
	})
	require.NoError(t, err)

	require.Len(t, plan.issueUpserts, 2)
	assert.ElementsMatch(t, batch.Issues, plan.issueUpserts)
	assert.Equal(t, []managedObservationIssueKey{{
		path:      "removed.txt",
		issueType: IssueInvalidFilename,
	}}, plan.issueDeletes)
}

// A batch deletes only stored rows of its managed types, so an issue of a type
// it does not manage would be stored for good. The plan refuses the batch,
// naming the type and the path, and plans nothing; a batch with no managed
// types at all is refused the same way.
//
// Validates: R-2.10.10, R-6.4.9
func TestBuildObservationReconcilePlan_RefusesAnIssueOfATypeTheBatchDoesNotManage(t *testing.T) {
	t.Parallel()

	leftover := ObservationIssue{
		Path:      "Docs/.onedrive-go.0123456789abcdef.detach",
		DriveID:   driveid.New(testDriveID),
		IssueType: IssueLocalInterruptedRemoval,
	}
	for _, managed := range [][]IssueType{{IssueInvalidFilename}, nil} {
		plan, err := buildObservationReconcilePlan(&ObservationFindingsBatch{
			Issues:            []ObservationIssue{leftover},
			ManagedIssueTypes: managed,
		}, observationReconcileStoreState{issues: []ObservationIssueRow{
			{Path: "bad:name.txt", IssueType: IssueInvalidFilename},
		}})

		require.ErrorContains(t, err, `issue type "local_interrupted_removal" at `+leftover.Path)
		assert.Empty(t, plan.issueUpserts, "managed %v", managed)
		assert.Empty(t, plan.issueDeletes, "managed %v", managed)
	}
}

// Validates: R-2.5.2
func TestSyncStore_ApplyObservationReconcilePlan_DeletesByPreviousIssueType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:      "same.txt",
		DriveID:   driveid.New(testDriveID),
		IssueType: IssueInvalidFilename,
	})

	tx, err := BeginPerfTx(ctx, store.rawDB())
	require.NoError(t, err)

	plan := observationReconcilePlan{
		issueUpserts: []ObservationIssue{{
			Path:      "same.txt",
			DriveID:   driveid.New(testDriveID),
			IssueType: IssuePathTooLong,
		}},
		issueDeletes: []managedObservationIssueKey{{
			path:      "same.txt",
			issueType: IssueInvalidFilename,
		}},
	}

	require.NoError(t, store.applyObservationFindingsReconcilePlanTx(ctx, tx, plan))
	require.NoError(t, tx.Commit())

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "same.txt", rows[0].Path)
	assert.Equal(t, IssuePathTooLong, rows[0].IssueType)
}
