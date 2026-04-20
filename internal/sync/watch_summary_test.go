package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-2.10.47
func TestWatchConditionCountTotal_SumsRawCounts(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 6, watchConditionCountTotal([]watchConditionCount{
		{Key: ConditionInvalidFilename, Count: 2},
		{Key: ConditionRemoteWriteDenied, Count: 3},
		{Key: ConditionAuthenticationRequired, Count: 1},
	}))
}

// Validates: R-2.10.47
func TestWatchConditionCountAccumulator_AddAndCounts_SortsAndAggregates(t *testing.T) {
	t.Parallel()

	accumulator := make(watchConditionCountAccumulator)
	accumulator.Add("", 5)
	accumulator.Add(ConditionServiceOutage, 0)
	accumulator.Add(ConditionServiceOutage, 1)
	accumulator.Add(ConditionServiceOutage, 2)
	accumulator.Add(ConditionDiskFull, 4)

	assert.Equal(t, []watchConditionCount{
		{Key: ConditionServiceOutage, Count: 3},
		{Key: ConditionDiskFull, Count: 4},
	}, accumulator.Counts())
}

// Validates: R-2.10.47
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

	assert.Equal(t, 4, summary.Retrying)
	assert.Equal(t, 5, summary.ConditionTotal)
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
