package syncdomain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
