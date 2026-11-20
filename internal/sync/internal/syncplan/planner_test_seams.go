package syncplan

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The planner's test seams: what the engine's tests need from the planner to
// build a plan by hand. A ...ForTest function is used only by tests and the
// sync test-helper packages.

// PlanActionsForTest plans reconciled as the planner's last step does, with
// no moves composed and no mount context (planBuiltActions).
func (p *Planner) PlanActionsForTest(reconciled []syncdomain.Action, mode syncdomain.SyncMode) (*syncdomain.ActionPlan, error) {
	return p.planBuiltActions(reconciled, nil, nil, &moveComposition{}, &moveInputs{}, PlannerMountContext{}, mode)
}

// DependencyCycleForTest is the error the planner reports when actions
// depend on each other in a cycle, nil when they do not.
func DependencyCycleForTest(actions []syncdomain.Action) error {
	return detectDependencyCycle(buildDependencies(actions))
}
