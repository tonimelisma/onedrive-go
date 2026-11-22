package sync

import (
	"fmt"
	"strings"

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

// shadowPlanNoteLimit bounds the differences one debug event spells out; its
// Count still says how many there are.
const shadowPlanNoteLimit = 20

// reportShadowPlan compares a watch replan's plan with the identity-keyed
// shadow plan and, when they differ or the shadow planner fails, reports it
// as a debug event. It changes nothing: the shadow plan is neither executed
// nor recorded.
func (rt *watchRuntime) reportShadowPlan(inputs *currentInputs, plan *syncdomain.ActionPlan) {
	comparison := rt.compareShadowPlan(inputs, plan)
	if comparison.err == nil && len(comparison.differences) == 0 {
		return
	}

	event := syncdomain.EngineDebugEvent{
		Type:  syncdomain.EngineDebugEventShadowPlanDiffered,
		Count: len(comparison.differences),
		Note:  shadowPlanNote(&comparison),
	}
	if len(comparison.differences) > 0 {
		event.Path = comparison.differences[0].Path
	}
	if comparison.err != nil {
		event.Error = comparison.err.Error()
	}
	rt.deps.emit(event)
}

func shadowPlanNote(comparison *shadowPlanComparison) string {
	lines := make([]string, 0, min(len(comparison.differences), shadowPlanNoteLimit)+1)
	lines = append(lines, fmt.Sprintf("%d unexplained", comparison.unexplained))
	for i := range comparison.differences {
		if i == shadowPlanNoteLimit {
			lines = append(lines, fmt.Sprintf("and %d more", len(comparison.differences)-i))
			break
		}
		lines = append(lines, comparison.differences[i].String())
	}

	return strings.Join(lines, "; ")
}
