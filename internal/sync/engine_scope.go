package sync

import (
	"context"
	"fmt"
	"log/slog"
	"path"

	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type remoteScopeResult struct {
	observed []synctypes.ObservedItem
	emitted  []synctypes.ChangeEvent
}

func (e *Engine) buildScopeSnapshot(ctx context.Context) (syncscope.Snapshot, error) {
	snapshot, err := syncobserve.BuildScopeSnapshot(ctx, e.syncTree, e.syncScopeConfig, e.logger)
	if err != nil {
		return syncscope.Snapshot{}, fmt.Errorf("build scope snapshot: %w", err)
	}

	return snapshot, nil
}

func (flow *engineFlow) applyScopeState(
	ctx context.Context,
	dryRun bool,
	session *ScopeSession,
	plan *ObservationSessionPlan,
) error {
	if dryRun {
		return nil
	}

	state, err := flow.scopeStateRecord(session, plan)
	if err != nil {
		return err
	}

	if err := flow.engine.baseline.ApplyScopeState(ctx, synctypes.ScopeStateApplyRequest{
		State: state,
	}); err != nil {
		return fmt.Errorf("apply scope state: %w", err)
	}

	return nil
}

func applyRemoteScope(
	logger *slog.Logger,
	snapshot syncscope.Snapshot,
	generation int64,
	events []synctypes.ChangeEvent,
) remoteScopeResult {
	result := remoteScopeResult{
		observed: make([]synctypes.ObservedItem, 0, len(events)),
		emitted:  make([]synctypes.ChangeEvent, 0, len(events)),
	}

	for i := range events {
		ev := events[i]
		if ev.Source != synctypes.SourceRemote {
			result.emitted = append(result.emitted, ev)
			continue
		}

		switch ev.Type {
		case synctypes.ChangeMove:
			oldInScope := snapshot.AllowsPath(ev.OldPath)
			newReason := snapshot.ExclusionReason(ev.Path)
			newInScope := newReason == syncscope.ExclusionNone

			result.observed = appendObservedEvent(logger, result.observed, &ev, generation, newReason)

			switch {
			case oldInScope && newInScope:
				result.emitted = append(result.emitted, ev)
			case oldInScope && !newInScope:
				deleteEv := ev
				deleteEv.Type = synctypes.ChangeDelete
				deleteEv.Path = ev.OldPath
				deleteEv.OldPath = ""
				deleteEv.Name = path.Base(ev.OldPath)
				deleteEv.Hash = ""
				deleteEv.IsDeleted = true
				result.emitted = append(result.emitted, deleteEv)
			case !oldInScope && newInScope:
				createEv := ev
				createEv.Type = synctypes.ChangeCreate
				createEv.OldPath = ""
				createEv.IsDeleted = false
				result.emitted = append(result.emitted, createEv)
			}
		case synctypes.ChangeCreate, synctypes.ChangeModify, synctypes.ChangeDelete, synctypes.ChangeShortcut:
			reason := snapshot.ExclusionReason(ev.Path)
			result.observed = appendObservedEvent(logger, result.observed, &ev, generation, reason)
			if reason == syncscope.ExclusionNone {
				result.emitted = append(result.emitted, ev)
			}
		}
	}

	return result
}

func appendObservedEvent(
	logger *slog.Logger,
	items []synctypes.ObservedItem,
	ev *synctypes.ChangeEvent,
	generation int64,
	reason syncscope.ExclusionReason,
) []synctypes.ObservedItem {
	if ev.ItemID == "" {
		if logger != nil {
			logger.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
				slog.String("path", ev.Path),
			)
		}

		return items
	}

	filtered := reason != syncscope.ExclusionNone

	return append(items, synctypes.ObservedItem{
		DriveID:          ev.DriveID,
		ItemID:           ev.ItemID,
		ParentID:         ev.ParentID,
		Path:             ev.Path,
		ItemType:         ev.ItemType,
		Hash:             ev.Hash,
		Size:             ev.Size,
		Mtime:            ev.Mtime,
		ETag:             ev.ETag,
		IsDeleted:        ev.IsDeleted,
		Filtered:         filtered,
		FilterGeneration: generation,
		FilterReason:     mapFilterReason(reason),
	})
}

func mapFilterReason(reason syncscope.ExclusionReason) synctypes.RemoteFilterReason {
	switch reason {
	case syncscope.ExclusionNone:
		return synctypes.RemoteFilterNone
	case syncscope.ExclusionMarkerScope:
		return synctypes.RemoteFilterMarkerScope
	case syncscope.ExclusionPathScope:
		return synctypes.RemoteFilterPathScope
	default:
		panic(fmt.Sprintf("unknown exclusion reason %q", reason))
	}
}

func (rt *watchRuntime) currentScopeSnapshot() syncscope.Snapshot {
	rt.scopeMu.RLock()
	defer rt.scopeMu.RUnlock()

	return rt.scopeSnapshot
}

func (rt *watchRuntime) currentScopeGeneration() int64 {
	rt.scopeMu.RLock()
	defer rt.scopeMu.RUnlock()

	return rt.scopeGeneration
}

func (rt *watchRuntime) setScopeSnapshot(snapshot syncscope.Snapshot, generation int64) {
	rt.scopeMu.Lock()
	defer rt.scopeMu.Unlock()

	rt.scopeSnapshot = snapshot
	rt.scopeGeneration = generation
}

func (rt *watchRuntime) handleWatchScopeChange(
	ctx context.Context,
	p *watchPipeline,
	change *syncscope.Change,
) error {
	session := ScopeSession{
		Current:    change.New,
		Previous:   change.Old,
		Diff:       change.Diff,
		Generation: rt.currentScopeGeneration() + 1,
	}
	plan, err := rt.BuildObservationSessionPlan(ctx, ObservationPlanRequest{
		Session:  &session,
		SyncMode: p.mode,
		Purpose:  observationPlanPurposeWatch,
	})
	if err != nil {
		return err
	}

	rt.setScopeSnapshot(change.New, session.Generation)

	if p.mode == synctypes.SyncUploadOnly {
		plan.Reentry.Pending = false
		plan.Reentry.Kind = synctypes.ScopeReconcileNone
	}
	if err := rt.applyScopeState(ctx, false, &session, &plan); err != nil {
		return fmt.Errorf("sync: applying watch scope change: %w", err)
	}

	if plan.Reentry.Pending {
		rt.runEnteredScopeReconciliationAsync(ctx, p.bl, plan.Reentry.Paths)
	}

	return nil
}
