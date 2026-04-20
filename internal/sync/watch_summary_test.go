package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWatchConditionSummary_CountHelpers(t *testing.T) {
	t.Parallel()

	summary := watchConditionSummary{
		Groups: []watchConditionGroupCount{
			{Key: ConditionInvalidFilename, Count: 2},
			{Key: ConditionRemoteWriteDenied, Count: 3},
			{Key: ConditionAuthenticationRequired, Count: 1},
		},
		Retrying: 4,
	}

	assert.Equal(t, 6, summary.VisibleTotal())
	assert.Equal(t, 0, summary.ConflictCount())
	assert.Equal(t, 2, summary.ActionableCount())
	assert.Equal(t, 3, summary.RemoteBlockedCount())
	assert.Equal(t, 1, summary.AuthRequiredCount())
	assert.Equal(t, 4, summary.RetryingCount())
}

func TestWatchConditionGroupAccumulator_AddAndGroups_SortsAndAggregates(t *testing.T) {
	t.Parallel()

	accumulator := make(watchConditionGroupAccumulator)
	accumulator.Add("", 5, "", "")
	accumulator.Add(ConditionServiceOutage, 0, "drive", "ignored")
	accumulator.Add(ConditionServiceOutage, 1, "drive", "Drive B")
	accumulator.Add(ConditionServiceOutage, 2, "drive", "Drive A")
	accumulator.Add(ConditionServiceOutage, 3, "drive", "Drive A")
	accumulator.Add(ConditionDiskFull, 4, "disk", "Mac SSD")

	assert.Equal(t, []watchConditionGroupCount{
		{Key: ConditionDiskFull, Count: 4, ScopeKind: "disk", Scope: "Mac SSD"},
		{Key: ConditionServiceOutage, Count: 5, ScopeKind: "drive", Scope: "Drive A"},
		{Key: ConditionServiceOutage, Count: 1, ScopeKind: "drive", Scope: "Drive B"},
	}, accumulator.Groups())
}

func TestBuildWatchConditionSummary_AggregatesRawAuthorities(t *testing.T) {
	t.Parallel()

	summary, groups := buildWatchConditionSummary(&DriveStatusSnapshot{
		RetryingItems: 4,
		ObservationIssues: []ObservationIssueRow{
			{IssueType: IssueInvalidFilename},
			{IssueType: IssueRemoteReadDenied, ScopeKey: SKPermRemoteRead("Shared/Docs")},
		},
		BlockScopes: []*BlockScope{
			{Key: SKPermRemoteWrite("Shared/Docs"), ConditionType: IssueRemoteWriteDenied},
			{Key: SKService(), ConditionType: IssueServiceOutage},
		},
		BlockedRetryWork: []RetryWorkRow{
			{ScopeKey: SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/a.txt"},
			{ScopeKey: SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/b.txt"},
		},
	})

	assert.Equal(t, 4, summary.RetryingCount())
	assert.ElementsMatch(t, []watchConditionGroupCount{
		{Key: ConditionInvalidFilename, Count: 1},
		{Key: ConditionRemoteReadDenied, Count: 1},
		{Key: ConditionRemoteWriteDenied, Count: 2},
		{Key: ConditionServiceOutage, Count: 1},
	}, summary.Groups)
	assert.Equal(t, []watchRemoteBlockedGroup{
		{
			ScopeKey:     SKPermRemoteWrite("Shared/Docs"),
			BoundaryPath: "Shared/Docs",
			BlockedPaths: []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"},
		},
	}, groups)
}
