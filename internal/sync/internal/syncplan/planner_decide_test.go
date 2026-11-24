package syncplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.2.16
//
// The store reads the inputs and the planner decides in Go: the comparison
// and reconciliation rows of a path carry the same kinds, the decider's; an
// unclassified row carries the inputs the plan error prints.
func TestDecideComparisons_GoDecidesEveryRowOfOneRead(t *testing.T) {
	t.Parallel()

	read := []syncdomain.ComparisonRow{
		{Path: "new.txt", LocalType: syncdomain.SideFile, LocalChanged: true},
		{Path: "other.txt", LocalType: syncdomain.SideFile, LocalChanged: true},
	}

	comparisons, reconciliations := DecideComparisons(read, syncdomain.Decide)
	require.Len(t, comparisons, 2)
	require.Len(t, reconciliations, 2)
	for i := range comparisons {
		assert.Equal(t, read[i].Path, comparisons[i].Path)
		assert.Equal(t, comparisons[i].Path, reconciliations[i].Path)
		assert.Equal(t, syncdomain.ItemTypeFile, reconciliations[i].ItemType)
		assert.Equal(t, syncdomain.ComparisonLocalOnlyCreate, comparisons[i].ComparisonKind)
		assert.Equal(t, syncdomain.ComparisonLocalOnlyCreate, reconciliations[i].ComparisonKind)
		assert.Equal(t, syncdomain.ReconcileUpload, reconciliations[i].ReconciliationKind)
		assert.Nil(t, reconciliations[i].DecisionInputs)
	}

	refuseNew := func(_ syncdomain.DecisionState) syncdomain.Decision {
		return syncdomain.Decision{Comparison: syncdomain.ComparisonLocalOnlyCreate, Reconciliation: syncdomain.ReconcileUnclassified}
	}
	_, reconciliations = DecideComparisons(read, refuseNew)
	require.NotEmpty(t, reconciliations[0].DecisionInputs)
	assert.Equal(t, syncdomain.PlannerDecisionInput{Column: "local_present", Value: "1"}, reconciliations[0].DecisionInputs[1])
	assert.Equal(t, syncdomain.PlannerDecisionInput{Column: "local_item_type", Value: "file"}, reconciliations[0].DecisionInputs[4])
}
