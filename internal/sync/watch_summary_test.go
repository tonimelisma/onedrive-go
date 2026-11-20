package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// Validates: R-6.6.30
func TestWatchConditionCountTotal_SumsRawCounts(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 6, watchConditionCountTotal([]watchConditionCount{
		{Key: syncdomain.ConditionInvalidFilename, Count: 2},
		{Key: syncdomain.ConditionRemoteWriteDenied, Count: 3},
		{Key: syncdomain.ConditionAuthenticationRequired, Count: 1},
	}))
}

// Validates: R-6.6.30
func TestWatchConditionCounts_SortsAndAggregatesProjectedGroups(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []watchConditionCount{
		{Key: syncdomain.ConditionServiceOutage, Count: 3},
		{Key: syncdomain.ConditionDiskFull, Count: 4},
	}, watchConditionCounts([]syncstore.StoredConditionGroup{
		{ConditionKey: "", Count: 5},
		{ConditionKey: syncdomain.ConditionServiceOutage, Count: 0},
		{ConditionKey: syncdomain.ConditionServiceOutage, Count: 1},
		{ConditionKey: syncdomain.ConditionServiceOutage, Count: 2},
		{ConditionKey: syncdomain.ConditionDiskFull, Count: 4},
	}))
}

// Validates: R-6.6.30
func TestBuildWatchConditionSummary_AggregatesRawAuthorities(t *testing.T) {
	t.Parallel()

	summary := buildWatchConditionSummary(&syncstore.DriveStatusSnapshot{
		RetryingItems: 4,
		ObservationIssues: []syncdomain.ObservationIssueRow{
			{IssueType: syncdomain.IssueInvalidFilename},
			{IssueType: syncdomain.IssueRemoteReadDenied, ScopeKey: syncdomain.SKPermRemoteRead("Shared/Docs")},
		},
		BlockScopes: []*syncdomain.BlockScope{
			{
				Key:           syncdomain.SKPermRemoteWrite("Shared/Docs"),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
			{
				Key:           syncdomain.SKService(),
				TrialInterval: time.Minute,
				NextTrialAt:   time.Unix(0, 0).UTC().Add(time.Minute),
			},
		},
		BlockedRetryWork: []syncdomain.RetryWorkRow{
			{ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/a.txt"},
			{ScopeKey: syncdomain.SKPermRemoteWrite("Shared/Docs"), Path: "Shared/Docs/b.txt"},
		},
	})

	assert.Equal(t, 4, summary.Retrying)
	assert.Equal(t, 5, summary.ConditionTotal)
	assert.ElementsMatch(t, []watchConditionCount{
		{Key: syncdomain.ConditionInvalidFilename, Count: 1},
		{Key: syncdomain.ConditionRemoteReadDenied, Count: 1},
		{Key: syncdomain.ConditionRemoteWriteDenied, Count: 2},
		{Key: syncdomain.ConditionServiceOutage, Count: 1},
	}, summary.Counts)
	assert.Equal(t, []watchRemoteBlockedGroup{
		{
			ScopeKey:     syncdomain.SKPermRemoteWrite("Shared/Docs"),
			BlockedPaths: []string{"Shared/Docs/a.txt", "Shared/Docs/b.txt"},
		},
	}, summary.RemoteBlocked)
}

// Validates: R-6.6.30
func TestBuildWatchConditionSummary_RemoteBlockedGroupsOnlyTrackActiveScopes(t *testing.T) {
	t.Parallel()

	summary := buildWatchConditionSummary(&syncstore.DriveStatusSnapshot{
		ObservationIssues: []syncdomain.ObservationIssueRow{{
			Path:      "Shared/Docs/file.txt",
			IssueType: syncdomain.IssueRemoteWriteDenied,
			ScopeKey:  syncdomain.SKPermRemoteWrite("Shared/Docs"),
		}},
	})

	assert.Equal(t, 1, summary.ConditionTotal)
	assert.Equal(t, []watchConditionCount{{
		Key:   syncdomain.ConditionRemoteWriteDenied,
		Count: 1,
	}}, summary.Counts)
	assert.Empty(t, summary.RemoteBlocked)
}
