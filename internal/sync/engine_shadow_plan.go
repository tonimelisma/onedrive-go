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

// shadowPlanModels reports whether the identity-keyed shadow planner models a
// pass of this run: it plans every change in both directions, with no content
// filter's path rules and no shortcut roots. A download-only or upload-only
// watch, a filter that hides paths and a drive with shared-folder mounts
// would each make it report differences that are only what it leaves out.
func (flow *engineFlow) shadowPlanModels(mode syncdomain.SyncMode) bool {
	filter := flow.cfg.contentFilter
	switch {
	case mode != syncdomain.SyncBidirectional:
		return false
	case len(filter.IgnoredDirs) > 0 || len(filter.IncludedDirs) > 0 || len(filter.IgnoredPaths) > 0:
		return false
	case flow.roots != nil && len(flow.roots.current()) > 0:
		return false
	default:
		return true
	}
}

// shadowPlanNoteLimit bounds the differences one debug event spells out; its
// Count still says how many there are.
const shadowPlanNoteLimit = 20

// reportShadowPlan compares a watch replan's plan with the identity-keyed
// shadow plan and, when they differ or the shadow planner fails, reports it
// as a debug event. It changes nothing: the shadow plan is neither executed
// nor recorded. A pass whose inputs the shadow planner does not model is not
// compared (shadowPlanModels), so every difference it reports is one the
// shadow planner could have avoided.
func (rt *watchRuntime) reportShadowPlan(mode syncdomain.SyncMode, inputs *currentInputs, plan *syncdomain.ActionPlan) {
	if !rt.shadowPlanModels(mode) {
		return
	}
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
