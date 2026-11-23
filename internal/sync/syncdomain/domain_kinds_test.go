package syncdomain

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.16
func TestPlannerKinds_ParseRoundTripsEveryMemberAndRefusesOthers(t *testing.T) {
	t.Parallel()

	comparisons := AllComparisonKinds()
	assert.Equal(t, ComparisonUnclassified, comparisons[0], "the zero value is the unclassified member")
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(kindNames(comparisons)))), len(comparisons))
	for _, kind := range comparisons {
		parsed, err := ParseComparisonKind(kind.String())
		require.NoError(t, err)
		assert.Equal(t, kind, parsed)
	}
	_, err := ParseComparisonKind("bogus")
	require.Error(t, err)

	reconciliations := AllReconciliationKinds()
	assert.Equal(t, ReconcileUnclassified, reconciliations[0], "the zero value is the unclassified member")
	assert.Len(t, slices.Compact(slices.Sorted(slices.Values(kindNames(reconciliations)))), len(reconciliations))
	for _, kind := range reconciliations {
		parsed, parseErr := ParseReconciliationKind(kind.String())
		require.NoError(t, parseErr)
		assert.Equal(t, kind, parsed)
	}
	_, err = ParseReconciliationKind("bogus")
	require.Error(t, err)

	// A value outside either enum prints under the exported type's name.
	assert.Equal(t, "ComparisonKind(200)", ComparisonKind(200).String())
	assert.Equal(t, "ReconciliationKind(200)", ReconciliationKind(200).String())
}
