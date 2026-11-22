package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// shadowPlanComparison is one pass's plan compared with the identity-keyed
// shadow plan of the same inputs: where they differ, how many of those
// differences no known reason covers, and the shadow planner's error. The
// shadow plan is never executed and never recorded.
type shadowPlanComparison struct {
	differences []syncplan.PlanDifference
	unexplained int
	err         error
}

// compareShadowPlan plans inputs with the identity-keyed planner and
// compares its plan with plan, the pass's own (after the delete-safety hold).
func (flow *engineFlow) compareShadowPlan(inputs *currentInputs, plan *syncdomain.ActionPlan) shadowPlanComparison {
	diffs, unexplained, err := syncplan.ShadowPlanDifferences(&syncplan.IdentityInputs{
		Baseline: inputs.baseline,
		Local:    inputs.localRows,
		Remote:   inputs.remoteRows,
		DriveID:  flow.cfg.driveID,

		ObservationIssues: inputs.observationIssues,
	}, plan)

	return shadowPlanComparison{differences: diffs, unexplained: unexplained, err: err}
}
