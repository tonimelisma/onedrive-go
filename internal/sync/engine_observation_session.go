package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type ObservationSession struct {
	Generation int64
}

type ReentryPlan struct {
	Paths   []string
	Pending bool
	Kind    ScopeReconcileKind
}

type ObservationPlanPurpose string

const (
	observationPlanPurposeOneShot ObservationPlanPurpose = "one_shot"
	observationPlanPurposeWatch   ObservationPlanPurpose = "watch"
)

type ObservationPhaseDriver string

const (
	observationPhaseDriverRootDelta    ObservationPhaseDriver = "root_delta"
	observationPhaseDriverScopedRoot   ObservationPhaseDriver = "scoped_root"
	observationPhaseDriverScopedTarget ObservationPhaseDriver = "scoped_targets"
)

type ObservationPhaseDispatchPolicy string

const (
	observationPhaseDispatchPolicySingleBatch       ObservationPhaseDispatchPolicy = "single_batch"
	observationPhaseDispatchPolicySequentialTargets ObservationPhaseDispatchPolicy = "sequential_targets"
	observationPhaseDispatchPolicyParallelTargets   ObservationPhaseDispatchPolicy = "parallel_targets"
)

type ObservationPhaseErrorPolicy string

const (
	observationPhaseErrorPolicyFailBatch     ObservationPhaseErrorPolicy = "fail_batch"
	observationPhaseErrorPolicyIsolateTarget ObservationPhaseErrorPolicy = "isolate_target"
)

type ObservationPhaseFallbackPolicy string

const (
	observationPhaseFallbackPolicyNone             ObservationPhaseFallbackPolicy = "no_fallback"
	observationPhaseFallbackPolicyDeltaToEnumerate ObservationPhaseFallbackPolicy = "delta_to_enumerate"
)

type ObservationPhaseTokenCommitPolicy string

const (
	observationPhaseTokenCommitPolicyAfterPhaseSuccess   ObservationPhaseTokenCommitPolicy = "commit_after_phase_success"
	observationPhaseTokenCommitPolicyAfterPlannerAccepts ObservationPhaseTokenCommitPolicy = "commit_after_planner_accepts"
)

type ObservationPhasePlan struct {
	Driver            ObservationPhaseDriver
	DispatchPolicy    ObservationPhaseDispatchPolicy
	Targets           []plannedObservationTarget
	ErrorPolicy       ObservationPhaseErrorPolicy
	FallbackPolicy    ObservationPhaseFallbackPolicy
	TokenCommitPolicy ObservationPhaseTokenCommitPolicy
}

func (plan ObservationPhasePlan) HasTargets() bool {
	return len(plan.Targets) > 0
}

type ObservationPlanRequest struct {
	Session       *ObservationSession
	SyncMode      Mode
	FullReconcile bool
	Purpose       ObservationPlanPurpose
}

type ObservationSessionPlan struct {
	PrimaryPhase ObservationPhasePlan
	Reentry      ReentryPlan
	Hash         string
}

type CursorCommitSet struct {
	Tokens []deferredDeltaToken
}

func (flow *engineFlow) BuildObservationSession(ctx context.Context) (ObservationSession, error) {
	_ = ctx
	return ObservationSession{Generation: 1}, nil
}

func (flow *engineFlow) BuildObservationSessionPlan(
	ctx context.Context,
	req ObservationPlanRequest,
) (ObservationSessionPlan, error) {
	if req.Session == nil {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan: missing session")
	}

	plan := flow.newObservationSessionPlan()
	plan.Reentry = buildReentryPlan(req)

	flow.buildPrimaryObservationPhase(ctx, &plan)

	planHashValue, err := flow.planHash(&plan, effectivePrimaryFullReconcile(req.FullReconcile, &plan))
	if err != nil {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan hash: %w", err)
	}

	plan.Hash = planHashValue

	if err := validateObservationSessionPlan(&plan, true); err != nil {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan: %w", err)
	}
	if err := flow.validatePrimaryObservationPersistence(&plan); err != nil {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan: %w", err)
	}
	return plan, nil
}

func (flow *engineFlow) newObservationSessionPlan() ObservationSessionPlan {
	return ObservationSessionPlan{
		Reentry: ReentryPlan{
			Kind: ScopeReconcileNone,
		},
	}
}

func buildReentryPlan(req ObservationPlanRequest) ReentryPlan {
	_ = req
	return ReentryPlan{Kind: ScopeReconcileNone}
}

func (flow *engineFlow) buildPrimaryObservationPhase(
	ctx context.Context,
	plan *ObservationSessionPlan,
) {
	_ = ctx

	plan.PrimaryPhase = ObservationPhasePlan{
		Driver:            observationPhaseDriverRootDelta,
		DispatchPolicy:    observationPhaseDispatchPolicySingleBatch,
		ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
		FallbackPolicy:    observationPhaseFallbackPolicyNone,
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
	}

	if flow.engine.hasScopedRoot() {
		plan.PrimaryPhase.Driver = observationPhaseDriverScopedRoot
		plan.PrimaryPhase.FallbackPolicy = fallbackPolicyForPrimaryMode(flow.engine.primaryObservationMode(), flow.engine.recursiveLister != nil)
	}
}

func (flow *engineFlow) websocketEnabledForPrimaryPhase(phase ObservationPhasePlan) bool {
	return phase.Driver == observationPhaseDriverRootDelta &&
		flow.engine.enableWebsocket &&
		flow.engine.socketIOFetcher != nil
}

func scopeObservationModeForPrimary(mode primaryObservationMode) ScopeObservationMode {
	switch mode {
	case primaryObservationDelta:
		return ScopeObservationScopedDelta
	case primaryObservationEnumerate:
		return ScopeObservationScopedEnumerate
	default:
		panic(fmt.Sprintf("unknown primary observation mode %q", mode))
	}
}

func (flow *engineFlow) scopeObservationMode(plan *ObservationSessionPlan) ScopeObservationMode {
	switch {
	case plan.PrimaryPhase.Driver == observationPhaseDriverScopedTarget && plan.PrimaryPhase.HasTargets():
		return scopeObservationModeForPrimary(plan.PrimaryPhase.Targets[0].mode)
	case plan.PrimaryPhase.Driver == observationPhaseDriverScopedRoot:
		return scopeObservationModeForPrimary(flow.engine.primaryObservationMode())
	default:
		return ScopeObservationRootDelta
	}
}

func effectivePrimaryFullReconcile(fullReconcile bool, plan *ObservationSessionPlan) bool {
	_ = plan
	return fullReconcile
}

func (flow *engineFlow) planHash(
	plan *ObservationSessionPlan,
	fullReconcile bool,
) (string, error) {
	type planHashInput struct {
		Mode             ScopeObservationMode `json:"mode"`
		PrimaryPaths     []string             `json:"primary_paths,omitempty"`
		PrimaryFolderIDs []string             `json:"primary_folder_ids,omitempty"`
		FullReconcile    bool                 `json:"full_reconcile"`
		ReentryKind      ScopeReconcileKind   `json:"reentry_kind"`
		ReentryPaths     []string             `json:"reentry_paths,omitempty"`
		PendingReentry   bool                 `json:"pending_reentry"`
		WebsocketEnabled bool                 `json:"websocket_enabled"`
	}

	input := planHashInput{
		Mode:             flow.scopeObservationMode(plan),
		FullReconcile:    fullReconcile,
		ReentryKind:      plan.Reentry.Kind,
		ReentryPaths:     append([]string(nil), plan.Reentry.Paths...),
		PendingReentry:   plan.Reentry.Pending,
		WebsocketEnabled: flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase),
	}
	for _, scope := range plan.PrimaryPhase.Targets {
		input.PrimaryPaths = append(input.PrimaryPaths, scope.localPath)
		input.PrimaryFolderIDs = append(input.PrimaryFolderIDs, scope.scopeID)
	}

	raw, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal plan hash input: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
