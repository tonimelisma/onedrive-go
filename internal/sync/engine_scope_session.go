package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

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

type ObservationPlan struct {
	Mode             synctypes.ScopeObservationMode
	PrimaryScopes    []primaryObservationScope
	RootFallback     bool
	FullReconcile    bool
	WebsocketEnabled bool
	Reentry          ReentryPlan
	Hash             string
}

type ReentryPlan struct {
	Paths   []string
	Pending bool
	Kind    synctypes.ScopeReconcileKind
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

func (flow *engineFlow) BuildObservationPlan(
	ctx context.Context,
	session *ScopeSession,
	mode synctypes.SyncMode,
	fullReconcile bool,
) (ObservationPlan, error) {
	plan := ObservationPlan{
		Mode:             synctypes.ScopeObservationRootDelta,
		FullReconcile:    fullReconcile,
		WebsocketEnabled: flow.engine.enableWebsocket && flow.engine.socketIOFetcher != nil,
		Reentry: ReentryPlan{
			Kind: synctypes.ScopeReconcileNone,
		},
	}

	if mode != synctypes.SyncUploadOnly && session.Diff.HasEntered() {
		if hasObservedRoot(session.Diff.EnteredPaths) {
			plan.FullReconcile = true
			plan.Reentry.Kind = synctypes.ScopeReconcileFull
		} else {
			plan.Reentry.Paths = append([]string(nil), session.Diff.EnteredPaths...)
			plan.Reentry.Pending = true
			plan.Reentry.Kind = synctypes.ScopeReconcileEnteredPath
		}
	}

	if flow.engine.usesPrimaryPathScopes() {
		scopes, rootFallback, err := flow.engine.resolvePrimaryObservationScopes(ctx)
		if err != nil {
			return ObservationPlan{}, err
		}

		plan.PrimaryScopes = scopes
		plan.RootFallback = rootFallback
		if !rootFallback && len(scopes) > 0 {
			plan.Mode = scopeObservationModeForPrimary(scopes[0].mode)
			plan.WebsocketEnabled = false
		}
	} else if flow.engine.hasScopedRoot() {
		plan.Mode = scopeObservationModeForPrimary(flow.engine.primaryObservationMode())
		plan.WebsocketEnabled = false
	}

	planHashValue, err := planHash(plan, session.Current)
	if err != nil {
		return ObservationPlan{}, fmt.Errorf("build observation plan hash: %w", err)
	}

	plan.Hash = planHashValue
	return plan, nil
}

func hasObservedRoot(paths []string) bool {
	for _, path := range paths {
		if path == "" {
			return true
		}
	}

	return false
}

func (flow *engineFlow) scopeStateRecord(session *ScopeSession, plan ObservationPlan) (synctypes.ScopeStateRecord, error) {
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
		ObservationMode:       plan.Mode,
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

func planHash(plan ObservationPlan, snapshot syncscope.Snapshot) (string, error) {
	type planHashInput struct {
		Mode             synctypes.ScopeObservationMode `json:"mode"`
		PrimaryPaths     []string                       `json:"primary_paths,omitempty"`
		PrimaryFolderIDs []string                       `json:"primary_folder_ids,omitempty"`
		RootFallback     bool                           `json:"root_fallback"`
		FullReconcile    bool                           `json:"full_reconcile"`
		WebsocketEnabled bool                           `json:"websocket_enabled"`
		Snapshot         syncscope.PersistedSnapshot    `json:"snapshot"`
	}

	input := planHashInput{
		Mode:             plan.Mode,
		RootFallback:     plan.RootFallback,
		FullReconcile:    plan.FullReconcile,
		WebsocketEnabled: plan.WebsocketEnabled,
		Snapshot:         snapshot.Persisted(),
	}
	for _, scope := range plan.PrimaryScopes {
		input.PrimaryPaths = append(input.PrimaryPaths, scope.localPath)
		input.PrimaryFolderIDs = append(input.PrimaryFolderIDs, scope.folderID)
	}

	raw, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal plan hash input: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
