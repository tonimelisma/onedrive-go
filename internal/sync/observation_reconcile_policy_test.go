package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestBuildObservationReconcilePlan_DeletesOnlyManagedCurrentIssues(t *testing.T) {
	t.Parallel()

	plan := buildObservationReconcilePlan(
		&ObservationFindingsBatch{
			ManagedIssueTypes: []string{IssueLocalReadDenied},
			ManagedPaths:      []string{"Private/file.txt"},
		},
		observationReconcileState{
			issues: []ObservationIssueRow{
				{Path: "Private/file.txt", IssueType: IssueLocalReadDenied},
				{Path: "Other/file.txt", IssueType: IssueLocalReadDenied},
				{Path: "other-issue.txt", IssueType: IssueInvalidFilename},
			},
		},
	)

	require.Len(t, plan.issueDeletes, 1)
	assert.Equal(t, observationIssueDelete{
		path:      "Private/file.txt",
		issueType: IssueLocalReadDenied,
	}, plan.issueDeletes[0])
}

func TestBuildObservationReconcilePlan_UpsertsCurrentIssuesOnly(t *testing.T) {
	t.Parallel()

	batch := &ObservationFindingsBatch{
		Issues: []ObservationIssue{{
			Path:       "same.txt",
			DriveID:    driveid.New(testDriveID),
			ActionType: ActionUpload,
			IssueType:  IssuePathTooLong,
			Error:      "new",
		}},
		ManagedIssueTypes: []string{IssuePathTooLong},
		ManagedPaths:      []string{"same.txt"},
	}

	plan := buildObservationReconcilePlan(batch, observationReconcileState{
		issues: []ObservationIssueRow{{
			Path:      "same.txt",
			IssueType: IssueInvalidFilename,
		}},
	})

	require.Len(t, plan.issueUpserts, 1)
	assert.Equal(t, batch.Issues[0], plan.issueUpserts[0])
	assert.Empty(t, plan.issueDeletes)
}

func TestSyncStore_ApplyObservationReconcilePlan_DeletesByPreviousIssueType(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)

	seedObservationIssueRowForTest(t, store, &ObservationIssue{
		Path:       "same.txt",
		DriveID:    driveid.New(testDriveID),
		ActionType: ActionUpload,
		IssueType:  IssueInvalidFilename,
		Error:      "old",
	})

	tx, err := beginPerfTx(ctx, store.rawDB())
	require.NoError(t, err)

	plan := observationReconcilePlan{
		issueUpserts: []ObservationIssue{{
			Path:       "same.txt",
			DriveID:    driveid.New(testDriveID),
			ActionType: ActionUpload,
			IssueType:  IssuePathTooLong,
			Error:      "new",
		}},
		issueDeletes: []observationIssueDelete{{
			path:      "same.txt",
			issueType: IssueInvalidFilename,
		}},
	}

	require.NoError(t, store.applyObservationFindingsReconcilePlanTx(ctx, tx, plan, now.UnixNano()))
	require.NoError(t, tx.Commit())

	rows, err := store.ListObservationIssues(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "same.txt", rows[0].Path)
	assert.Equal(t, IssuePathTooLong, rows[0].IssueType)
}
