package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.45, R-2.10.47
func TestProjectStoredConditionGroups_MergesDurableAuthorities(t *testing.T) {
	t.Parallel()

	groups := ProjectStoredConditionGroups(&DriveStatusSnapshot{
		ObservationIssues: []ObservationIssueRow{
			{Path: "/bad:name.txt", IssueType: IssueInvalidFilename},
		},
		BlockScopes: []*BlockScope{
			{
				Key: SKPermRemoteWrite("Shared/Docs"),
			},
		},
		BlockedRetryWork: []RetryWorkRow{
			{Path: "Shared/Docs/b.txt", ScopeKey: SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/a.txt", ScopeKey: SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/a.txt", ScopeKey: SKPermRemoteWrite("Shared/Docs"), Blocked: true},
			{Path: "Shared/Docs/c.txt", ScopeKey: SKPermRemoteWrite("Shared/Docs"), Blocked: true},
		},
	})

	require.Len(t, groups, 2)
	assert.Equal(t, StoredConditionGroup{
		ConditionKey:  ConditionRemoteWriteDenied,
		ConditionType: IssueRemoteWriteDenied,
		ScopeKey:      SKPermRemoteWrite("Shared/Docs"),
		Count:         4,
		Paths:         []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt", "Shared/Docs/c.txt"},
	}, groups[0])
	assert.Equal(t, StoredConditionGroup{
		ConditionKey:  ConditionInvalidFilename,
		ConditionType: IssueInvalidFilename,
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
		RetryingWork:  []RetryWorkRow{{Path: "a.txt", ActionType: ActionUpload, AttemptCount: 3}},
		ActionableRetryWork: []RetryWorkRow{
			{Path: "deep", ActionType: ActionFolderCreate, ConditionType: IssuePathTooLong, AttemptCount: 1},
		},
		ObservationIssues: []ObservationIssueRow{
			{Path: "CON", IssueType: IssueInvalidFilename},
			{Path: "Shared/Gone", IssueType: IssueRemoteReadDenied, ScopeKey: SKPermRemoteRead("Shared/Gone")},
			{Path: "Docs/.interrupted-removal", IssueType: IssueLocalInterruptedRemoval},
		},
		BlockScopes:      []*BlockScope{{Key: SKPermRemoteWrite("Shared/Docs")}},
		BlockedRetryWork: []RetryWorkRow{{Path: "Shared/Docs/a.txt", ScopeKey: SKPermRemoteWrite("Shared/Docs"), Blocked: true}},
		HeldDeletes:      []HeldDeleteRow{{Path: "gone-1.txt", Side: string(DeleteSafetySideRemote)}},
	}

	assert.Equal(t, 2, snapshot.UnsyncedIssueCount())
	assert.Zero(t, (&DriveStatusSnapshot{}).UnsyncedIssueCount(), "an empty store holds no issue")

	// Every counted issue is one status lists in a condition group.
	listed := 0
	for _, group := range ProjectStoredConditionGroups(&DriveStatusSnapshot{ObservationIssues: snapshot.ObservationIssues}) {
		if IssueTypeLeavesFileUnsynced(group.ConditionType) {
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
		ObservationIssues: []ObservationIssueRow{{
			Path:      "Shared/Docs/file.txt",
			IssueType: IssueRemoteWriteDenied,
			ScopeKey:  SKPermRemoteWrite("Shared/Docs"),
		}},
		BlockScopes: []*BlockScope{{
			Key: SKPermRemoteWrite("Shared/Docs"),
		}},
	})

	require.Len(t, groups, 1)
	assert.Equal(t, StoredConditionGroup{
		ConditionKey:  ConditionRemoteWriteDenied,
		ConditionType: IssueRemoteWriteDenied,
		ScopeKey:      SKPermRemoteWrite("Shared/Docs"),
		Count:         1,
		Paths:         []string{"Shared/Docs/file.txt"},
	}, groups[0])
}
