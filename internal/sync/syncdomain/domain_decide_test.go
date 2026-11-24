package syncdomain

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decisionFlagCount is the number of boolean fields of DecisionState.
const decisionFlagCount = 17

// decisionStateCount is the size of the normalized decision space: four side
// types on each of three sides times every flag combination.
const decisionStateCount = sideTypeCount * sideTypeCount * sideTypeCount << decisionFlagCount

// decisionStateAt decodes state number i: the flags from the low bits, then
// the three side types.
func decisionStateAt(i int) DecisionState {
	flag := func(bit int) bool { return i>>bit&1 == 1 }
	sides := i >> decisionFlagCount

	return DecisionState{
		Baseline:                       SideType(sides % sideTypeCount),
		Local:                          SideType(sides / sideTypeCount % sideTypeCount),
		Remote:                         SideType(sides / (sideTypeCount * sideTypeCount)),
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
func isRootCreate(s DecisionState) bool {
	if s.Baseline.Present() {
		return false
	}
	switch {
	case s.Local.Present() && !s.Remote.Present():
		return s.Local == SideRoot
	case s.Remote.Present() && !s.Local.Present():
		return s.Remote == SideRoot
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
func tablePropertyBroken(s DecisionState, d Decision) string {
	noSide := !s.Baseline.Present() && !s.Local.Present() && !s.Remote.Present()
	unclassified := d.Comparison == ComparisonUnclassified || d.Reconciliation == ReconcileUnclassified
	switch {
	case noSide && !unclassified:
		return "a path no side holds is classified"
	case unclassified && !noSide && !isRootCreate(s):
		return "a path a side holds is unclassified"
	case d.Reconciliation == ReconcileUpload && s.Local == SideFolder:
		return "upload of a local folder"
	case d.Reconciliation == ReconcileDownload && s.Remote == SideFolder:
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

	local := Decide(DecisionState{Local: SideRoot})
	assert.Equal(t, Decision{Comparison: ComparisonLocalOnlyCreate, Reconciliation: ReconcileUnclassified}, local)
	remote := Decide(DecisionState{Remote: SideRoot})
	assert.Equal(t, Decision{Comparison: ComparisonRemoteOnlyCreate, Reconciliation: ReconcileUnclassified}, remote)
	assert.Equal(t, Decision{Comparison: ComparisonLocalOnlyCreate, Reconciliation: ReconcileUpload},
		Decide(DecisionState{Local: SideFile}))
	assert.Equal(t, Decision{Comparison: ComparisonRemoteOnlyCreate, Reconciliation: ReconcileFolderCreateLocal},
		Decide(DecisionState{Remote: SideFolder, CurrentEqual: false}))
}

// Validates: R-6.2.16
//
// Valid names the rule a state breaks.
func TestDecisionState_ValidNamesTheBrokenRule(t *testing.T) {
	t.Parallel()

	require.NoError(t, DecisionState{Baseline: SideFile, Local: SideFile, Remote: SideFile}.Valid())
	err := DecisionState{Baseline: SideFolder, Local: SideFolder, Remote: SideFolder, LocalChanged: true, CurrentEqual: true}.Valid()
	require.ErrorIs(t, err, errInvalidDecisionState)
	require.ErrorContains(t, err, "has no local change")
	err = DecisionState{Local: SideFile, LocalChanged: true}.Valid()
	require.ErrorContains(t, err, "local_changed needs baseline and local")
}

// A valid state's check allocates nothing: the planner asserts it per row
// under the engine's invariant checks, and the exhaustive table test calls it
// for every state. Not parallel: AllocsPerRun counts every goroutine's
// allocations.
func TestDecisionState_ValidAllocatesNothing(t *testing.T) {
	state := DecisionState{Baseline: SideFile, Local: SideFile, Remote: SideFile}
	allocs := testing.AllocsPerRun(100, func() {
		if err := state.Valid(); err != nil {
			panic(err)
		}
	})
	assert.Zero(t, allocs)
}
