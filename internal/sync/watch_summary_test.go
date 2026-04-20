package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWatchConditionSummary_CountHelpers(t *testing.T) {
	t.Parallel()

	summary := watchConditionSummary{
		Counts: []watchConditionCount{
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

func TestWatchConditionCountAccumulator_AddAndCounts_SortsAndAggregates(t *testing.T) {
	t.Parallel()

	accumulator := make(watchConditionCountAccumulator)
	accumulator.Add("", 5)
	accumulator.Add(ConditionServiceOutage, 0)
	accumulator.Add(ConditionServiceOutage, 1)
	accumulator.Add(ConditionServiceOutage, 2)
	accumulator.Add(ConditionDiskFull, 4)

	assert.Equal(t, []watchConditionCount{
		{Key: ConditionDiskFull, Count: 4},
		{Key: ConditionServiceOutage, Count: 3},
	}, accumulator.Counts())
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
	assert.ElementsMatch(t, []watchConditionCount{
		{Key: ConditionInvalidFilename, Count: 1},
		{Key: ConditionRemoteReadDenied, Count: 1},
		{Key: ConditionRemoteWriteDenied, Count: 2},
		{Key: ConditionServiceOutage, Count: 1},
	}, summary.Counts)
	assert.Equal(t, []watchRemoteBlockedGroup{
		{
			ConditionKey: ConditionRemoteWriteDenied,
			ScopeKey:     SKPermRemoteWrite("Shared/Docs"),
			BlockedPaths: []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"},
		},
	}, groups)
}
