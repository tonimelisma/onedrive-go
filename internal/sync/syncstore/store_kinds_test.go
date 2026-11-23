package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.2.16
//
// A side type the enum does not know means the SQL and the Go vocabulary
// drifted apart: the scan fails and names the path.
func TestQueryPlannerRows_UnknownSideTypeFailsTheScanNamingThePath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	_, err := queryComparisonRows(t.Context(), store.RawDBForTest(),
		`SELECT 'docs/compared.txt', '', '', 0, 0, 0, '', '', 0, '', '',
			'file', 'bogus', 'file', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0`)
	require.Error(t, err)
	require.ErrorContains(t, err, `"docs/compared.txt"`)
	require.ErrorContains(t, err, `"bogus"`)
}

// Validates: R-6.2.16
//
// One query reads the inputs and Go decides: the comparison and
// reconciliation rows of a path carry the same kinds, the decider's, and an
// unclassified row carries the inputs the plan error prints.
func TestQueryPlannerRows_GoDecidesEveryRowOfOneQuery(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	_, err := store.RawDBForTest().ExecContext(ctx, `INSERT INTO local_state (path, item_type, hash, size, mtime)
		VALUES ('new.txt', 'file', 'h', 1, 1), ('other.txt', 'file', 'h', 1, 1)`)
	require.NoError(t, err)

	comparisons, reconciliations, err := QueryPlannerRows(ctx, store.RawDBForTest(), "local_state", "remote_state", syncdomain.Decide)
	require.NoError(t, err)
	require.Len(t, comparisons, 2)
	require.Len(t, reconciliations, 2)
	for i := range comparisons {
		assert.Equal(t, comparisons[i].Path, reconciliations[i].Path)
		assert.Equal(t, syncdomain.ComparisonLocalOnlyCreate, comparisons[i].ComparisonKind)
		assert.Equal(t, syncdomain.ComparisonLocalOnlyCreate, reconciliations[i].ComparisonKind)
		assert.Equal(t, syncdomain.ReconcileUpload, reconciliations[i].ReconciliationKind)
		assert.Nil(t, reconciliations[i].DecisionInputs)
	}

	refuseNew := func(_ syncdomain.DecisionState) syncdomain.Decision {
		return syncdomain.Decision{Comparison: syncdomain.ComparisonLocalOnlyCreate, Reconciliation: syncdomain.ReconcileUnclassified}
	}
	_, reconciliations, err = QueryPlannerRows(ctx, store.RawDBForTest(), "local_state", "remote_state", refuseNew)
	require.NoError(t, err)
	require.NotEmpty(t, reconciliations[0].DecisionInputs)
	assert.Equal(t, syncdomain.PlannerDecisionInput{Column: "local_present", Value: "1"}, reconciliations[0].DecisionInputs[1])
	assert.Equal(t, syncdomain.PlannerDecisionInput{Column: "local_item_type", Value: "file"}, reconciliations[0].DecisionInputs[4])
}
