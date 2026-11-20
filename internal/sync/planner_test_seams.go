package sync

// The planner's test seams: what the engine's tests need from the planner to
// build a plan by hand. A ...ForTest function is used only by tests and the
// sync test-helper packages.

// PlanActionsForTest plans reconciled as the planner's last step does, with
// no moves composed and no mount context (planBuiltActions).
func (p *Planner) PlanActionsForTest(reconciled []Action, mode SyncMode) (*ActionPlan, error) {
	return p.planBuiltActions(reconciled, nil, nil, &moveComposition{}, &moveInputs{}, PlannerMountContext{}, mode)
}

// DependencyCycleForTest is the error the planner reports when actions
// depend on each other in a cycle, nil when they do not.
func DependencyCycleForTest(actions []Action) error {
	return detectDependencyCycle(buildDependencies(actions))
}
