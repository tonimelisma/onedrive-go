package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConditionSummary_CountHelpers(t *testing.T) {
	t.Parallel()

	summary := ConditionSummary{
		Groups: []ConditionGroupCount{
			{Key: SummaryInvalidFilename, Count: 2},
			{Key: SummaryRemoteWriteDenied, Count: 3},
			{Key: SummaryAuthenticationRequired, Count: 1},
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

func TestConditionGroupAccumulator_AddAndGroups_SortsAndAggregates(t *testing.T) {
	t.Parallel()

	accumulator := make(conditionGroupAccumulator)
	accumulator.Add("", 5, "", "")
	accumulator.Add(SummaryServiceOutage, 0, "drive", "ignored")
	accumulator.Add(SummaryServiceOutage, 1, "drive", "Drive B")
	accumulator.Add(SummaryServiceOutage, 2, "drive", "Drive A")
	accumulator.Add(SummaryServiceOutage, 3, "drive", "Drive A")
	accumulator.Add(SummaryDiskFull, 4, "disk", "Mac SSD")

	assert.Equal(t, []ConditionGroupCount{
		{Key: SummaryDiskFull, Count: 4, ScopeKind: "disk", Scope: "Mac SSD"},
		{Key: SummaryServiceOutage, Count: 5, ScopeKind: "drive", Scope: "Drive A"},
		{Key: SummaryServiceOutage, Count: 1, ScopeKind: "drive", Scope: "Drive B"},
	}, accumulator.Groups())
}
