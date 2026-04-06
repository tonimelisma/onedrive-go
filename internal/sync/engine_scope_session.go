package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type ScopeSession struct {
	Current        syncscope.Snapshot
	Previous       syncscope.Snapshot
	Persisted      synctypes.ScopeStateRecord
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

type WatchStrategy string

const (
	watchStrategyRootDelta    WatchStrategy = "root_delta"
	watchStrategyScopedRoot   WatchStrategy = "scoped_root"
	watchStrategyScopedTarget WatchStrategy = "scoped_targets"
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
	Baseline                  *synctypes.Baseline
	SyncMode                  synctypes.SyncMode
	FullReconcile             bool
	Purpose                   ObservationPlanPurpose
	Shortcuts                 []synctypes.Shortcut
	ShortcutCollisions        map[string]bool
	SuppressedShortcutTargets map[string]struct{}
}

type ObservationSessionPlan struct {
	PrimaryPhase     ObservationPhasePlan
	ShortcutPhase    ObservationPhasePlan
	WatchStrategy    WatchStrategy
	WebsocketEnabled bool
	Reentry          ReentryPlan
	Hash             string
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
	if req.Session == nil {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan: missing scope session")
	}

	plan := flow.newObservationSessionPlan()
	plan.Reentry = buildReentryPlan(req)

	if err := flow.buildPrimaryObservationPhase(ctx, &plan); err != nil {
		return ObservationSessionPlan{}, err
	}
	if err := flow.buildShortcutObservationPhase(&req, &plan); err != nil {
		return ObservationSessionPlan{}, err
	}

	if req.Purpose == observationPlanPurposeWatch && plan.WatchStrategy != watchStrategyRootDelta {
		plan.WebsocketEnabled = false
	}

	planHashValue, err := flow.planHash(&plan, req.Session.Current, effectivePrimaryFullReconcile(req.FullReconcile, &plan))
	if err != nil {
		return ObservationSessionPlan{}, fmt.Errorf("build observation session plan hash: %w", err)
	}

	plan.Hash = planHashValue
	return plan, nil
}

func (flow *engineFlow) newObservationSessionPlan() ObservationSessionPlan {
	return ObservationSessionPlan{
		PrimaryPhase: ObservationPhasePlan{
			ErrorPolicy:       observationPhaseErrorPolicyFailBatch,
			FallbackPolicy:    observationPhaseFallbackPolicyNone,
			TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPlannerAccepts,
		},
		ShortcutPhase: ObservationPhasePlan{
			ErrorPolicy:       observationPhaseErrorPolicyIsolateTarget,
			FallbackPolicy:    observationPhaseFallbackPolicyNone,
			TokenCommitPolicy: observationPhaseTokenCommitPolicyAfterPhaseSuccess,
		},
		WatchStrategy:    watchStrategyRootDelta,
		WebsocketEnabled: flow.engine.enableWebsocket && flow.engine.socketIOFetcher != nil,
		Reentry: ReentryPlan{
			Kind: synctypes.ScopeReconcileNone,
		},
	}
}

func buildReentryPlan(req ObservationPlanRequest) ReentryPlan {
	plan := ReentryPlan{
		Kind: synctypes.ScopeReconcileNone,
	}
	if req.SyncMode == synctypes.SyncUploadOnly || !req.Session.Diff.HasEntered() {
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
	if flow.engine.usesPrimaryPathScopes() {
		scopes, rootFallback, err := flow.engine.resolvePrimaryObservationScopes(ctx)
		if err != nil {
			return err
		}

		plan.PrimaryPhase.Targets = scopes
		switch {
		case !rootFallback && plan.PrimaryPhase.HasTargets():
			plan.WatchStrategy = watchStrategyScopedTarget
			plan.WebsocketEnabled = false
			if flow.engine.recursiveLister != nil && plan.primaryPhaseUsesDelta() {
				plan.PrimaryPhase.FallbackPolicy = observationPhaseFallbackPolicyDeltaToEnumerate
			}
		case flow.engine.hasScopedRoot():
			plan.WatchStrategy = watchStrategyScopedRoot
			plan.WebsocketEnabled = false
		}
		return nil
	}

	if flow.engine.hasScopedRoot() {
		plan.WatchStrategy = watchStrategyScopedRoot
		plan.WebsocketEnabled = false
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

func hasObservedRoot(paths []string) bool {
	for _, path := range paths {
		if path == "" {
			return true
		}
	}

	return false
}

func (flow *engineFlow) scopeStateRecord(session *ScopeSession, plan *ObservationSessionPlan) (synctypes.ScopeStateRecord, error) {
	snapshotJSON, err := syncscope.MarshalSnapshot(session.Current)
	if err != nil {
		return synctypes.ScopeStateRecord{}, fmt.Errorf("marshal scope snapshot: %w", err)
	}

	lastKind := plan.Reentry.Kind
	if !plan.Reentry.Pending {
		lastKind = synctypes.ScopeReconcileNone
	}

	return synctypes.ScopeStateRecord{
		Generation:            session.Generation,
		EffectiveSnapshotJSON: snapshotJSON,
		ObservationPlanHash:   plan.Hash,
		ObservationMode:       flow.scopeObservationMode(plan),
		WebsocketEnabled:      plan.WebsocketEnabled,
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
	case plan.PrimaryPhase.HasTargets():
		return scopeObservationModeForPrimary(plan.PrimaryPhase.Targets[0].mode)
	case plan.WatchStrategy == watchStrategyScopedRoot:
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
		WebsocketEnabled: plan.WebsocketEnabled,
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
