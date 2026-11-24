package syncplan

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// decisionFlagCount is the number of boolean fields of DecisionState.
const decisionFlagCount = 17

// decisionStateCount is the size of the normalized decision space: four side
// types on each of three sides times every flag combination.
const decisionStateCount = sideTypeCount * sideTypeCount * sideTypeCount << decisionFlagCount

// decisionStateAt decodes state number i: the flags from the low bits, then
// the three side types.
func decisionStateAt(i int) syncdomain.DecisionState {
	flag := func(bit int) bool { return i>>bit&1 == 1 }
	sides := i >> decisionFlagCount

	return syncdomain.DecisionState{
		Baseline:                       syncdomain.SideType(sides % sideTypeCount),
		Local:                          syncdomain.SideType(sides / sideTypeCount % sideTypeCount),
		Remote:                         syncdomain.SideType(sides / (sideTypeCount * sideTypeCount)),
		LocalMoveSource:                flag(0),
		LocalMoveDest:                  flag(1),
		RemoteMoveSource:               flag(2),
		RemoteMoveDest:                 flag(3),
		LocalItemReplaced:              flag(4),
		RemoteItemReplaced:             flag(5),
		DeleteAwaitsAncestorMove:       flag(6),
		LocalCopyAwaitsAncestorMove:    flag(7),
		RemoteCreateAwaitsAncestorMove: flag(8),
		KeptLocalFolder:                flag(9),
		RemoteMoveShadowed:             flag(10),
		CurrentEqual:                   flag(11),
		LocalChanged:                   flag(12),
		RemoteChanged:                  flag(13),
		LocalSubtreeChanged:            flag(14),
		RemoteSubtreeChanged:           flag(15),
		MetadataDrift:                  flag(16),
	}
}

// isRootCreate reports a path only one side holds, as a drive root: no
// observer records such an entry, and the decision refuses to transfer one.
func isRootCreate(s syncdomain.DecisionState) bool {
	if s.Baseline.Present() {
		return false
	}
	switch {
	case s.Local.Present() && !s.Remote.Present():
		return s.Local == syncdomain.SideRoot
	case s.Remote.Present() && !s.Local.Present():
		return s.Remote == syncdomain.SideRoot
	default:
		return false
	}
}

// Validates: R-6.2.16
//
// Every one of the 8,388,608 normalized states, with the properties the
// decision table must have over the states the comparison query can
// produce (Valid): a state is unclassified exactly when no side holds the
// path, or it would create a drive root; no state uploads a local folder or
// downloads a remote folder; every decision is reached by some valid state.
func TestDecide_EveryValidStateHasTheTableProperties(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive over 8,388,608 states; skipped in -short")
	}
	t.Parallel()

	valid := 0
	reached := map[ReconciliationKind]bool{}
	var violations []string
	for i := range decisionStateCount {
		s := decisionStateAt(i)
		if s.Valid() != nil {
			continue
		}
		valid++
		d := Decide(s)
		reached[d.Reconciliation] = true
		if what := tablePropertyBroken(s, d); what != "" && len(violations) < 10 {
			violations = append(violations, fmt.Sprintf("%s: %+v -> %s/%s", what, s, d.Comparison, d.Reconciliation))
		}
	}
	assert.Empty(t, violations)
	for _, kind := range allReconciliationKinds() {
		assert.True(t, reached[kind], "no valid state yields %s", kind)
	}
	t.Logf("%d of %d states are valid", valid, decisionStateCount)
}

// tablePropertyBroken names the table property decision d of valid state s
// breaks, or returns "".
func tablePropertyBroken(s syncdomain.DecisionState, d Decision) string {
	noSide := !s.Baseline.Present() && !s.Local.Present() && !s.Remote.Present()
	unclassified := d.Comparison == ComparisonUnclassified || d.Reconciliation == ReconcileUnclassified
	switch {
	case noSide && !unclassified:
		return "a path no side holds is classified"
	case unclassified && !noSide && !isRootCreate(s):
		return "a path a side holds is unclassified"
	case d.Reconciliation == ReconcileUpload && s.Local == syncdomain.SideFolder:
		return "upload of a local folder"
	case d.Reconciliation == ReconcileDownload && s.Remote == syncdomain.SideFolder:
		return "download onto a remote folder"
	default:
		return ""
	}
}

// Validates: R-6.2.16
//
// The (absent, root, absent) cell: a root-typed entry only the local tree
// holds. The SQL table uploaded it, as it uploads any non-folder; no
// observer records one, and a drive root is no file, so it is unclassified
// and planning refuses it. The same holds for one only OneDrive holds.
func TestDecide_ARootOnlyOneSideHoldsIsUnclassified(t *testing.T) {
	t.Parallel()

	local := Decide(syncdomain.DecisionState{Local: syncdomain.SideRoot})
	assert.Equal(t, Decision{Comparison: ComparisonLocalOnlyCreate, Reconciliation: ReconcileUnclassified}, local)
	remote := Decide(syncdomain.DecisionState{Remote: syncdomain.SideRoot})
	assert.Equal(t, Decision{Comparison: ComparisonRemoteOnlyCreate, Reconciliation: ReconcileUnclassified}, remote)
	assert.Equal(t, Decision{Comparison: ComparisonLocalOnlyCreate, Reconciliation: ReconcileUpload},
		Decide(syncdomain.DecisionState{Local: syncdomain.SideFile}))
	assert.Equal(t, Decision{Comparison: ComparisonRemoteOnlyCreate, Reconciliation: ReconcileFolderCreateLocal},
		Decide(syncdomain.DecisionState{Remote: syncdomain.SideFolder, CurrentEqual: false}))
}

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

func kindNames[K interface{ String() string }](kinds []K) []string {
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, kind.String())
	}

	return names
}
