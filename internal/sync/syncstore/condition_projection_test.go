package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.45, R-2.10.47
func TestProjectStoredConditionGroups_MergesDurableAuthorities(t *testing.T) {
	t.Parallel()

	groups := ProjectStoredConditionGroups(&DriveStatusSnapshot{
		ObservationIssues: []syncdomain.ObservationIssueRow{
			{Path: "/bad:name.txt", IssueType: syncdomain.IssueInvalidFilename},
		},
		BlockScopes: []*syncdomain.BlockScope{
			{
				Key: syncdomain.SKPermRemoteWrite("Shared/Docs"),
			},
		},
		BlockedRetryWork: []syncdomain.RetryWorkRow{
			{Path: "Shared/Docs/b.txt", ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/a.txt", ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/a.txt", ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/c.txt", ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Blocked: true},
		},
	})

	require.Len(t, groups, 2)
	assert.Equal(t, StoredConditionGroup{
		ConditionKey:  syncdomain.ConditionRemoteWriteDenied,
		ConditionType: syncdomain.IssueRemoteWriteDenied,
		ScopeKey:      syncdomain.SKPermRemoteWrite("Shared/Docs"),
		Count:         4,
		Paths:         []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt", "Shared/Docs/c.txt"},
	}, groups[0])
	assert.Equal(t, StoredConditionGroup{
		ConditionKey:  syncdomain.ConditionInvalidFilename,
		ConditionType: syncdomain.IssueInvalidFilename,
		Count:         1,
		Paths:         []string{"/bad:name.txt"},
	}, groups[1])
}

// Validates: R-6.6.28
//
// The issues a one-shot run counts as left are the observation issues status
// lists, through status's own filter, less those whose type keeps no file
// unsynced. Retry work and held deletes in the same snapshot are not read:
// the run's own counts hold the retry work, and held deletes are a
// deliberate hold.
func TestDriveStatusSnapshot_UnsyncedIssueCountIsWhatStatusListsLessDeliberateHolds(t *testing.T) {
	t.Parallel()

	snapshot := &DriveStatusSnapshot{
		RetryingItems: 1,
		RetryingWork:  []syncdomain.RetryWorkRow{{Path: "a.txt", ActionType: syncdomain.ActionUpload, AttemptCount: 3}},
		ActionableRetryWork: []syncdomain.RetryWorkRow{
			{Path: "deep", ActionType: syncdomain.ActionFolderCreate, ConditionType: syncdomain.IssuePathTooLong, AttemptCount: 1},
		},
		ObservationIssues: []syncdomain.ObservationIssueRow{
			{Path: "CON", IssueType: syncdomain.IssueInvalidFilename},
			{Path: "Shared/Gone", IssueType: syncdomain.IssueRemoteReadDenied, ScopeKey: syncdomain.SKPermRemoteRead("Shared/Gone")},
			{Path: "Docs/.interrupted-removal", IssueType: syncdomain.IssueLocalInterruptedRemoval},
		},
		BlockScopes:      []*syncdomain.BlockScope{{Key: syncdomain.SKPermRemoteWrite("Shared/Docs")}},
		BlockedRetryWork: []syncdomain.RetryWorkRow{{Path: "Shared/Docs/a.txt", ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Blocked: true}},
		HeldDeletes:      []HeldDeleteRow{{Path: "gone-1.txt", Side: string(syncdomain.DeleteSafetySideRemote)}},
	}

	assert.Equal(t, 2, snapshot.UnsyncedIssueCount())
	assert.Zero(t, (&DriveStatusSnapshot{}).UnsyncedIssueCount(), "an empty store holds no issue")

	// Every counted issue is one status lists in a condition group.
	listed := 0
	for _, group := range ProjectStoredConditionGroups(&DriveStatusSnapshot{ObservationIssues: snapshot.ObservationIssues}) {
		if syncdomain.IssueTypeLeavesFileUnsynced(group.ConditionType) {
			listed += group.Count
		}
	}
	assert.Equal(t, listed, snapshot.UnsyncedIssueCount())
}

// Validates: R-2.10.47
func TestProjectStoredConditionGroups_NilSnapshotReturnsNil(t *testing.T) {
	t.Parallel()

	assert.Nil(t, ProjectStoredConditionGroups(nil))
}

// Validates: R-2.10.45, R-2.10.47
func TestProjectStoredConditionGroups_DoesNotDoubleCountObservationBackedScope(t *testing.T) {
	t.Parallel()

	groups := ProjectStoredConditionGroups(&DriveStatusSnapshot{
		ObservationIssues: []syncdomain.ObservationIssueRow{{
			Path:      "Shared/Docs/file.txt",
			IssueType: syncdomain.IssueRemoteWriteDenied,
			ScopeKey:  syncdomain.SKPermRemoteWrite("Shared/Docs"),
		}},
		BlockScopes: []*syncdomain.BlockScope{{
			Key: syncdomain.SKPermRemoteWrite("Shared/Docs"),
		}},
	})

	require.Len(t, groups, 1)
	assert.Equal(t, StoredConditionGroup{
		ConditionKey:  syncdomain.ConditionRemoteWriteDenied,
		ConditionType: syncdomain.IssueRemoteWriteDenied,
		ScopeKey:      syncdomain.SKPermRemoteWrite("Shared/Docs"),
		Count:         1,
		Paths:         []string{"Shared/Docs/file.txt"},
	}, groups[0])
}
