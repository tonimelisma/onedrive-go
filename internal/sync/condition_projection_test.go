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
