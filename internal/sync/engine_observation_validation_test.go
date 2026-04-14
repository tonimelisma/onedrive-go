package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.4.5, R-3.4.2
func TestValidateObservationSessionPlan_RequiresSessionBackedPrimaryPhase(t *testing.T) {
	t.Parallel()

	err := validateObservationSessionPlan(&ObservationSessionPlan{}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary")
}

// Validates: R-2.4.5, R-3.4.2
func TestValidateObservationPhasePlan_RejectsInvalidDispatchForDriver(t *testing.T) {
	t.Parallel()

	err := validateObservationPhasePlan(ObservationPhasePlan{
		Driver:         observationPhaseDriverRootDelta,
		DispatchPolicy: observationPhaseDispatchPolicySequentialTargets,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single_batch")
}

// Validates: R-2.4.5
func TestExecuteObservationPhase_RejectsInvalidPhase(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	flow := testEngineFlow(t, eng)

	_, err := flow.executeObservationPhase(t.Context(), emptyBaseline(), ObservationPhasePlan{
		Driver:            observationPhaseDriverScopedTarget,
		DispatchPolicy:    observationPhaseDispatchPolicySingleBatch,
		ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
		FallbackPolicy:    observationPhaseFallbackPolicyNone,
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
	}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires target dispatch")
}
