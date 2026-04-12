package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type ScopeSession struct {
	Current        syncscope.Snapshot
	Previous       syncscope.Snapshot
	Persisted      syncstore.ScopeStateRecord
	PersistedFound bool
	Diff           syncscope.Diff
	Generation     int64
}

type ReentryPlan struct {
	Paths   []string
	Pending bool
	Kind    synctypes.ScopeReconcileKind
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
	Session                   *ScopeSession
	Baseline                  *syncstore.Baseline
	SyncMode                  Mode
	FullReconcile             bool
	Purpose                   ObservationPlanPurpose
	Shortcuts                 []synctypes.Shortcut
	ShortcutCollisions        map[string]bool
	SuppressedShortcutTargets map[string]struct{}
}

type ObservationSessionPlan struct {
	PrimaryPhase  ObservationPhasePlan
	ShortcutPhase ObservationPhasePlan
	Reentry       ReentryPlan
	Hash          string
}

type CursorCommitSet struct {
	Tokens []deferredDeltaToken
}

func (flow *engineFlow) BuildScopeSession(ctx context.Context, watch *watchRuntime) (ScopeSession, error) {
	current, err := flow.engine.buildScopeSnapshot(ctx)
	if err != nil {
		return ScopeSession{}, fmt.Errorf("sync: building scope snapshot: %w", err)
	}
	if watch != nil {
		watch.setScopeSnapshot(current, watch.currentScopeGeneration())
	}

	persisted, found, err := flow.engine.baseline.ReadScopeState(ctx)
	if err != nil {
		return ScopeSession{}, fmt.Errorf("sync: reading persisted scope state: %w", err)
	}

	previous, err := syncscope.UnmarshalSnapshot(persisted.EffectiveSnapshotJSON)
	if err != nil {
		return ScopeSession{}, fmt.Errorf("sync: decoding persisted scope snapshot: %w", err)
	}

	diff := syncscope.DiffSnapshots(previous, current)
	generation := persisted.Generation
	if generation <= 0 {
		generation = 1
	}
	if !found {
		generation = 1
	} else if diff.HasChanges() {
		generation++
	}

	return ScopeSession{
		Current:        current,
		Previous:       previous,
		Persisted:      persisted,
		PersistedFound: found,
		Diff:           diff,
		Generation:     generation,
	}, nil
}

func (flow *engineFlow) BuildObservationSessionPlan(
	ctx context.Context,
	req ObservationPlanRequest,
) (ObservationSessionPlan, error) {
	if req.Session == nil && len(req.Shortcuts) == 0 {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan: missing scope session")
	}

	plan := flow.newObservationSessionPlan()
	plan.Reentry = buildReentryPlan(req)

	if req.Session != nil {
		if err := flow.buildPrimaryObservationPhase(ctx, &plan); err != nil {
			return ObservationSessionPlan{}, err
		}
	}
	if err := flow.buildShortcutObservationPhase(&req, &plan); err != nil {
		return ObservationSessionPlan{}, err
	}

	if req.Session != nil {
		planHashValue, err := flow.planHash(&plan, req.Session.Current, effectivePrimaryFullReconcile(req.FullReconcile, &plan))
		if err != nil {
			return ObservationSessionPlan{}, fmt.Errorf("build observation session plan hash: %w", err)
		}

		plan.Hash = planHashValue
	}
	if err := validateObservationSessionPlan(&plan, req.Session != nil); err != nil {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan: %w", err)
	}
	if req.Session != nil {
		if err := flow.validatePrimaryScopePersistence(&plan); err != nil {
			return ObservationSessionPlan{}, fmt.Errorf("build observation session plan: %w", err)
		}
	}
	return plan, nil
}

func (flow *engineFlow) newObservationSessionPlan() ObservationSessionPlan {
	return ObservationSessionPlan{
		Reentry: ReentryPlan{
			Kind: synctypes.ScopeReconcileNone,
		},
	}
}

func buildReentryPlan(req ObservationPlanRequest) ReentryPlan {
	plan := ReentryPlan{
		Kind: synctypes.ScopeReconcileNone,
	}
	if req.Session == nil {
		return plan
	}
	if req.SyncMode == SyncUploadOnly || !req.Session.Diff.HasEntered() {
		return plan
	}
	if hasObservedRoot(req.Session.Diff.EnteredPaths) {
		plan.Kind = synctypes.ScopeReconcileFull
		return plan
	}

	plan.Paths = append([]string(nil), req.Session.Diff.EnteredPaths...)
	plan.Pending = true
	plan.Kind = synctypes.ScopeReconcileEnteredPath
	return plan
}

func (flow *engineFlow) buildPrimaryObservationPhase(
	ctx context.Context,
	plan *ObservationSessionPlan,
) error {
	plan.PrimaryPhase = ObservationPhasePlan{
		Driver:            observationPhaseDriverRootDelta,
		DispatchPolicy:    observationPhaseDispatchPolicySingleBatch,
		ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
		FallbackPolicy:    observationPhaseFallbackPolicyNone,
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
	}

	if flow.engine.usesPrimaryPathScopes() {
		scopes, rootFallback, err := flow.engine.resolvePrimaryObservationScopes(ctx)
		if err != nil {
			return err
		}

		plan.PrimaryPhase.Targets = scopes
		switch {
		case !rootFallback && plan.PrimaryPhase.HasTargets():
			plan.PrimaryPhase.Driver = observationPhaseDriverScopedTarget
			plan.PrimaryPhase.DispatchPolicy = observationPhaseDispatchPolicySequentialTargets
			if plan.primaryPhaseUsesDelta() {
				plan.PrimaryPhase.FallbackPolicy = fallbackPolicyForPrimaryMode(primaryObservationDelta, flow.engine.recursiveLister != nil)
			}
		case flow.engine.hasScopedRoot():
			plan.PrimaryPhase.Driver = observationPhaseDriverScopedRoot
			plan.PrimaryPhase.FallbackPolicy = fallbackPolicyForPrimaryMode(flow.engine.primaryObservationMode(), flow.engine.recursiveLister != nil)
		}
		return nil
	}

	if flow.engine.hasScopedRoot() {
		plan.PrimaryPhase.Driver = observationPhaseDriverScopedRoot
		plan.PrimaryPhase.FallbackPolicy = fallbackPolicyForPrimaryMode(flow.engine.primaryObservationMode(), flow.engine.recursiveLister != nil)
	}

	return nil
}

func (flow *engineFlow) buildShortcutObservationPhase(
	req *ObservationPlanRequest,
	plan *ObservationSessionPlan,
) error {
	if len(req.Shortcuts) == 0 {
		return nil
	}
	if req.Baseline == nil {
		return fmt.Errorf("build observation session plan: missing baseline for shortcut planning")
	}

	plan.ShortcutPhase = ObservationPhasePlan{
		Driver:            observationPhaseDriverScopedTarget,
		DispatchPolicy:    observationPhaseDispatchPolicyParallelTargets,
		ErrorPolicy:       observationPhaseErrorPolicyIsolateTarget,
		FallbackPolicy:    observationPhaseFallbackPolicyNone,
		TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPhaseSuccess,
	}

	collisions := req.ShortcutCollisions
	if collisions == nil {
		collisions = detectShortcutCollisionsFromList(req.Shortcuts, req.Baseline, flow.engine.logger)
	}

	targets := make([]plannedObservationTarget, 0, len(req.Shortcuts))
	for i := range req.Shortcuts {
		sc := &req.Shortcuts[i]
		if collisions[sc.ItemID] {
			continue
		}
		if len(req.SuppressedShortcutTargets) > 0 {
			shortcutKey := sc.RemoteDrive + ":" + sc.RemoteItem
			if _, suppressed := req.SuppressedShortcutTargets[shortcutKey]; suppressed {
				flow.engine.logger.Debug(
					"suppressing shortcut observation target — target rate limited",
					slog.String("shortcut_key", shortcutKey),
					slog.String("local_path", sc.LocalPath),
				)
				continue
			}
		}

		targets = append(targets, shortcutObservationTarget(sc))
	}
	plan.ShortcutPhase.Targets = targets
	return nil
}

func (plan *ObservationSessionPlan) primaryPhaseUsesDelta() bool {
	for _, target := range plan.PrimaryPhase.Targets {
		if target.mode == primaryObservationDelta {
			return true
		}
	}

	return false
}

func (flow *engineFlow) websocketEnabledForPrimaryPhase(phase ObservationPhasePlan) bool {
	return phase.Driver == observationPhaseDriverRootDelta &&
		flow.engine.enableWebsocket &&
		flow.engine.socketIOFetcher != nil
}

func hasObservedRoot(paths []string) bool {
	for _, path := range paths {
		if path == "" {
			return true
		}
	}

	return false
}

func (flow *engineFlow) scopeStateRecord(session *ScopeSession, plan *ObservationSessionPlan) (syncstore.ScopeStateRecord, error) {
	if err := flow.validatePrimaryScopePersistence(plan); err != nil {
		return syncstore.ScopeStateRecord{}, fmt.Errorf("validate primary scope persistence: %w", err)
	}

	snapshotJSON, err := syncscope.MarshalSnapshot(session.Current)
	if err != nil {
		return syncstore.ScopeStateRecord{}, fmt.Errorf("marshal scope snapshot: %w", err)
	}

	lastKind := plan.Reentry.Kind
	if !plan.Reentry.Pending {
		lastKind = synctypes.ScopeReconcileNone
	}

	return syncstore.ScopeStateRecord{
		Generation:            session.Generation,
		EffectiveSnapshotJSON: snapshotJSON,
		ObservationPlanHash:   plan.Hash,
		ObservationMode:       flow.scopeObservationMode(plan),
		WebsocketEnabled:      flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase),
		PendingReentry:        plan.Reentry.Pending,
		LastReconcileKind:     lastKind,
		UpdatedAt:             flow.engine.nowFunc().UnixNano(),
	}, nil
}

func scopeObservationModeForPrimary(mode primaryObservationMode) synctypes.ScopeObservationMode {
	switch mode {
	case primaryObservationDelta:
		return synctypes.ScopeObservationScopedDelta
	case primaryObservationEnumerate:
		return synctypes.ScopeObservationScopedEnumerate
	default:
		panic(fmt.Sprintf("unknown primary observation mode %q", mode))
	}
}

func (flow *engineFlow) scopeObservationMode(plan *ObservationSessionPlan) synctypes.ScopeObservationMode {
	switch {
	case plan.PrimaryPhase.Driver == observationPhaseDriverScopedTarget && plan.PrimaryPhase.HasTargets():
		return scopeObservationModeForPrimary(plan.PrimaryPhase.Targets[0].mode)
	case plan.PrimaryPhase.Driver == observationPhaseDriverScopedRoot:
		return scopeObservationModeForPrimary(flow.engine.primaryObservationMode())
	default:
		return synctypes.ScopeObservationRootDelta
	}
}

func effectivePrimaryFullReconcile(fullReconcile bool, plan *ObservationSessionPlan) bool {
	return fullReconcile || plan.Reentry.Kind == synctypes.ScopeReconcileFull
}

func (flow *engineFlow) planHash(
	plan *ObservationSessionPlan,
	snapshot syncscope.Snapshot,
	fullReconcile bool,
) (string, error) {
	type planHashInput struct {
		Mode             synctypes.ScopeObservationMode `json:"mode"`
		PrimaryPaths     []string                       `json:"primary_paths,omitempty"`
		PrimaryFolderIDs []string                       `json:"primary_folder_ids,omitempty"`
		FullReconcile    bool                           `json:"full_reconcile"`
		ReentryKind      synctypes.ScopeReconcileKind   `json:"reentry_kind"`
		ReentryPaths     []string                       `json:"reentry_paths,omitempty"`
		PendingReentry   bool                           `json:"pending_reentry"`
		WebsocketEnabled bool                           `json:"websocket_enabled"`
		Snapshot         syncscope.PersistedSnapshot    `json:"snapshot"`
	}

	input := planHashInput{
		Mode:             flow.scopeObservationMode(plan),
		FullReconcile:    fullReconcile,
		ReentryKind:      plan.Reentry.Kind,
		ReentryPaths:     append([]string(nil), plan.Reentry.Paths...),
		PendingReentry:   plan.Reentry.Pending,
		WebsocketEnabled: flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase),
		Snapshot:         snapshot.Persisted(),
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
