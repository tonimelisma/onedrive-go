package sync

import (
	"testing"
	"time"

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
func TestWatchConditionCounts_SortsAndAggregatesProjectedGroups(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []watchConditionCount{
		{Key: ConditionServiceOutage, Count: 3},
		{Key: ConditionDiskFull, Count: 4},
	}, watchConditionCounts([]StoredConditionGroup{
		{ConditionKey: "", Count: 5},
		{ConditionKey: ConditionServiceOutage, Count: 0},
		{ConditionKey: ConditionServiceOutage, Count: 1},
		{ConditionKey: ConditionServiceOutage, Count: 2},
		{ConditionKey: ConditionDiskFull, Count: 4},
	}))
}

// Validates: R-2.10.47
func TestBuildWatchConditionSummary_AggregatesRawAuthorities(t *testing.T) {
	t.Parallel()

	summary := buildWatchConditionSummary(&DriveStatusSnapshot{
		RetryingItems: 4,
		ObservationIssues: []ObservationIssueRow{
			{IssueType: IssueInvalidFilename},
			{IssueType: IssueRemoteReadDenied, ScopeKey: SKPermRemoteRead("Shared/Docs")},
		},
		BlockScopes: []*BlockScope{
			{
				Key:           SKPermRemoteWrite("Shared/Docs"),
				BlockedAt:     time.Unix(0, 0).UTC(),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
			{
				Key:           SKService(),
				BlockedAt:     time.Unix(0, 0).UTC(),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
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
			ScopeKey:     SKPermRemoteWrite("Shared/Docs"),
			BlockedPaths: []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"},
		},
	}, summary.RemoteBlocked)
}

// Validates: R-2.10.47
func TestBuildWatchConditionSummary_RemoteBlockedGroupsOnlyTrackActiveScopes(t *testing.T) {
	t.Parallel()

	summary := buildWatchConditionSummary(&DriveStatusSnapshot{
		ObservationIssues: []ObservationIssueRow{{
			Path:      "Shared/Docs/file.txt",
			IssueType: IssueRemoteWriteDenied,
			ScopeKey:  SKPermRemoteWrite("Shared/Docs"),
		}},
	})

	assert.Equal(t, 1, summary.ConditionTotal)
	assert.Equal(t, []watchConditionCount{{
		Key:   ConditionRemoteWriteDenied,
		Count: 1,
	}}, summary.Counts)
	assert.Empty(t, summary.RemoteBlocked)
}
