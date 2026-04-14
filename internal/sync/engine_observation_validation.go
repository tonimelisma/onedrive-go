package sync

import "fmt"

func validateObservationSessionPlan(plan *ObservationSessionPlan, sessionBacked bool) error {
	if plan == nil {
		return fmt.Errorf("observation session plan is required")
	}
	if sessionBacked {
		if err := validatePrimaryObservationPhase(plan.PrimaryPhase); err != nil {
			return fmt.Errorf("invalid primary observation phase: %w", err)
		}
	} else if err := validateObservationPhasePlan(plan.PrimaryPhase); err != nil {
		return fmt.Errorf("invalid primary observation phase: %w", err)
	}

	return nil
}

func validateObservationPhasePlan(phase ObservationPhasePlan) error {
	if phase.Driver == "" {
		if len(phase.Targets) > 0 {
			return fmt.Errorf("empty driver cannot carry targets")
		}
		if phase.DispatchPolicy != "" || phase.ErrorPolicy != "" || phase.FallbackPolicy != "" || phase.TokenCommitPolicy != "" {
			return fmt.Errorf("empty driver cannot carry dispatch or policy state")
		}
		return nil
	}

	switch phase.Driver {
	case observationPhaseDriverRootDelta, observationPhaseDriverScopedRoot:
		if len(phase.Targets) > 0 {
			return fmt.Errorf("%s cannot carry explicit targets", phase.Driver)
		}
		if phase.DispatchPolicy != observationPhaseDispatchPolicySingleBatch {
			return fmt.Errorf("%s requires single_batch dispatch", phase.Driver)
		}
	case observationPhaseDriverScopedTarget:
		switch phase.DispatchPolicy {
		case observationPhaseDispatchPolicySequentialTargets, observationPhaseDispatchPolicyParallelTargets:
		case observationPhaseDispatchPolicySingleBatch:
			return fmt.Errorf("%s requires target dispatch", phase.Driver)
		default:
			return fmt.Errorf("%s requires target dispatch", phase.Driver)
		}
	default:
		return fmt.Errorf("unknown driver %q", phase.Driver)
	}

	return nil
}

func validatePrimaryObservationPhase(phase ObservationPhasePlan) error {
	if phase.Driver == "" {
		return fmt.Errorf("session-backed plans require a primary phase")
	}
	if err := validateObservationPhasePlan(phase); err != nil {
		return err
	}
	if phase.ErrorPolicy != observationPhaseErrorPolicyFailBatch {
		return fmt.Errorf("primary phase requires fail_batch error policy")
	}
	if phase.TokenCommitPolicy != observationPhaseTokenCommitPolicyAfterPlannerAccepts {
		return fmt.Errorf("primary phase requires commit_after_planner_accepts token policy")
	}

	switch phase.Driver {
	case observationPhaseDriverRootDelta:
		if phase.FallbackPolicy != observationPhaseFallbackPolicyNone {
			return fmt.Errorf("root_delta primary phase cannot fall back")
		}
	case observationPhaseDriverScopedRoot:
		switch phase.FallbackPolicy {
		case observationPhaseFallbackPolicyNone, observationPhaseFallbackPolicyDeltaToEnumerate:
		default:
			return fmt.Errorf("scoped_root primary phase has invalid fallback policy %q", phase.FallbackPolicy)
		}
	case observationPhaseDriverScopedTarget:
		if phase.DispatchPolicy != observationPhaseDispatchPolicySequentialTargets {
			return fmt.Errorf("scoped_targets primary phase requires sequential target dispatch")
		}
		switch phase.FallbackPolicy {
		case observationPhaseFallbackPolicyNone, observationPhaseFallbackPolicyDeltaToEnumerate:
		default:
			return fmt.Errorf("scoped_targets primary phase has invalid fallback policy %q", phase.FallbackPolicy)
		}
	}

	return nil
}

func (flow *engineFlow) validatePrimaryObservationPersistence(plan *ObservationSessionPlan) error {
	if err := validatePrimaryObservationPhase(plan.PrimaryPhase); err != nil {
		return fmt.Errorf("primary persistence requires valid primary phase: %w", err)
	}
	if plan.Hash == "" {
		return fmt.Errorf("primary persistence requires a non-empty plan hash")
	}

	return nil
}
