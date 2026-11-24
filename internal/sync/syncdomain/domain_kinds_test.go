package syncdomain

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.2.16
func TestPlannerKinds_EveryMemberHasItsOwnName(t *testing.T) {
	t.Parallel()

	comparisons := allComparisonKinds()
	assert.Equal(t, ComparisonUnclassified, comparisons[0], "the zero value is the unclassified member")
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(kindNames(comparisons)))), len(comparisons))
	for _, kind := range comparisons {
		assert.NotContains(t, kind.String(), "ComparisonKind(", "every member has a name")
	}

	reconciliations := allReconciliationKinds()
	assert.Equal(t, ReconcileUnclassified, reconciliations[0], "the zero value is the unclassified member")
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(kindNames(reconciliations)))), len(reconciliations))
	for _, kind := range reconciliations {
		assert.NotContains(t, kind.String(), "ReconciliationKind(", "every member has a name")
	}

	// A value outside either enum prints under the exported type's name.
	assert.Equal(t, "ComparisonKind(200)", ComparisonKind(200).String())
	assert.Equal(t, "ReconciliationKind(200)", ReconciliationKind(200).String())
}

// allComparisonKinds lists every member in declaration order, the
// unclassified one first.
func allComparisonKinds() []ComparisonKind {
	kinds := make([]ComparisonKind, 0, int(ComparisonDiverged)+1)
	for k := ComparisonUnclassified; k <= ComparisonDiverged; k++ {
		kinds = append(kinds, k)
	}

	return kinds
}

// allReconciliationKinds lists every member in declaration order, the
// unclassified one first.
func allReconciliationKinds() []ReconciliationKind {
	kinds := make([]ReconciliationKind, 0, int(ReconcileReplaceLocal)+1)
	for k := ReconcileUnclassified; k <= ReconcileReplaceLocal; k++ {
		kinds = append(kinds, k)
	}

	return kinds
}
