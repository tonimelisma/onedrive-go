package sync

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	plannerSafetyMax                = math.MaxInt32
	conflictResolutionBatchLimit    = 100
	staleConflictResolvingThreshold = time.Hour
)

type heldDeleteKey struct {
	driveID    string
	actionType synctypes.ActionType
	path       string
	itemID     string
}

type heldDeletePathKey struct {
	driveID    string
	actionType synctypes.ActionType
	path       string
}

func deleteKeyFromAction(action *synctypes.Action) heldDeleteKey {
	if action == nil {
		return heldDeleteKey{}
	}

	return heldDeleteKey{
		driveID:    action.DriveID.String(),
		actionType: action.Type,
		path:       action.Path,
		itemID:     action.ItemID,
	}
}

func deleteKeyFromRecord(record *synctypes.HeldDeleteRecord) heldDeleteKey {
	if record == nil {
		return heldDeleteKey{}
	}

	return heldDeleteKey{
		driveID:    record.DriveID.String(),
		actionType: record.ActionType,
		path:       record.Path,
		itemID:     record.ItemID,
	}
}

func pathKeyFromDeleteKey(key heldDeleteKey) heldDeletePathKey {
	return heldDeletePathKey{
		driveID:    key.driveID,
		actionType: key.actionType,
		path:       key.path,
	}
}

func plannedDeleteKeys(plan *synctypes.ActionPlan) map[heldDeleteKey]struct{} {
	if plan == nil {
		return map[heldDeleteKey]struct{}{}
	}

	keys := make(map[heldDeleteKey]struct{})
	for i := range plan.Actions {
		action := &plan.Actions[i]
		if isDeleteAction(action.Type) {
			keys[deleteKeyFromAction(action)] = struct{}{}
		}
	}

	return keys
}

func plannedDeleteItemsByPath(plan *synctypes.ActionPlan) map[heldDeletePathKey]map[string]struct{} {
	itemsByPath := make(map[heldDeletePathKey]map[string]struct{})
	for key := range plannedDeleteKeys(plan) {
		if key.itemID == "" {
			continue
		}
		pathKey := pathKeyFromDeleteKey(key)
		if itemsByPath[pathKey] == nil {
			itemsByPath[pathKey] = make(map[string]struct{})
		}
		itemsByPath[pathKey][key.itemID] = struct{}{}
	}

	return itemsByPath
}

func (e *Engine) approvedDeleteKeysForPlan(
	ctx context.Context,
	plan *synctypes.ActionPlan,
) (map[heldDeleteKey]struct{}, error) {
	approved, err := e.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
	if err != nil {
		return nil, fmt.Errorf("sync: list approved held deletes: %w", err)
	}

	planned := plannedDeleteKeys(plan)
	plannedItemsByPath := plannedDeleteItemsByPath(plan)
	keys := make(map[heldDeleteKey]struct{}, len(approved))
	prunedCount := 0
	for i := range approved {
		record := &approved[i]
		key := deleteKeyFromRecord(record)
		if _, ok := planned[key]; ok {
			keys[key] = struct{}{}
			continue
		}

		plannedItems := plannedItemsByPath[pathKeyFromDeleteKey(key)]
		if len(plannedItems) == 0 || key.itemID == "" {
			continue
		}
		if _, pathStillUsesSameItem := plannedItems[key.itemID]; pathStillUsesSameItem {
			continue
		}

		if err := e.baseline.DeleteHeldDelete(ctx, record.DriveID, record.ActionType, record.Path, record.ItemID); err != nil {
			return nil, fmt.Errorf("sync: prune stale approved held delete %s: %w", record.Path, err)
		}
		prunedCount++
		e.logger.Info("pruned stale approved held delete",
			slog.String("path", record.Path),
			slog.String("item_id", record.ItemID),
		)
	}
	if err := e.baseline.RecordStaleHeldDeletePrune(ctx, prunedCount, e.nowFunc()); err != nil {
		return nil, fmt.Errorf("sync: record stale approved held delete prune: %w", err)
	}

	return keys, nil
}

func filterActionPlan(
	plan *synctypes.ActionPlan,
	keepAction func(*synctypes.Action) bool,
) *synctypes.ActionPlan {
	if plan == nil || len(plan.Actions) == 0 {
		return plan
	}

	kept := make([]synctypes.Action, 0, len(plan.Actions))
	keptDeps := make([][]int, 0, len(plan.Deps))
	oldToNew := make(map[int]int, len(plan.Actions))

	for i := range plan.Actions {
		if !keepAction(&plan.Actions[i]) {
			continue
		}

		oldToNew[i] = len(kept)
		kept = append(kept, plan.Actions[i])
		keptDeps = append(keptDeps, nil)
	}

	for oldIdx, newIdx := range oldToNew {
		for _, depOld := range plan.Deps[oldIdx] {
			if depNew, ok := oldToNew[depOld]; ok {
				keptDeps[newIdx] = append(keptDeps[newIdx], depNew)
			}
		}
	}

	return &synctypes.ActionPlan{Actions: kept, Deps: keptDeps}
}

func (e *Engine) holdDeleteActions(ctx context.Context, actions []synctypes.Action) error {
	if len(actions) == 0 {
		return nil
	}

	now := e.nowFunc().UnixNano()
	records := make([]synctypes.HeldDeleteRecord, 0, len(actions))
	for i := range actions {
		records = append(records, synctypes.HeldDeleteRecord{
			DriveID:       actions[i].DriveID,
			ItemID:        actions[i].ItemID,
			Path:          actions[i].Path,
			ActionType:    actions[i].Type,
			State:         synctypes.HeldDeleteStateHeld,
			HeldAt:        now,
			LastPlannedAt: now,
			LastError:     fmt.Sprintf("held by delete safety threshold (threshold: %d)", e.deleteSafetyThreshold),
		})
	}

	if err := e.baseline.UpsertHeldDeletes(ctx, records); err != nil {
		return fmt.Errorf("sync: record held deletes: %w", err)
	}

	return nil
}

func (e *Engine) applyOneShotDeleteProtection(
	ctx context.Context,
	plan *synctypes.ActionPlan,
) (*synctypes.ActionPlan, error) {
	if plan == nil {
		return &synctypes.ActionPlan{}, nil
	}

	approved, err := e.approvedDeleteKeysForPlan(ctx, plan)
	if err != nil {
		return nil, fmt.Errorf("load approved deletes: %w", err)
	}

	if len(plan.Actions) == 0 || e.deleteSafetyThreshold <= 0 {
		return plan, nil
	}

	var unapprovedDeletes []synctypes.Action
	for i := range plan.Actions {
		action := &plan.Actions[i]
		if !isDeleteAction(action.Type) {
			continue
		}
		if _, ok := approved[deleteKeyFromAction(action)]; ok {
			continue
		}
		unapprovedDeletes = append(unapprovedDeletes, *action)
	}

	if len(unapprovedDeletes) <= e.deleteSafetyThreshold {
		return plan, nil
	}

	if err := e.holdDeleteActions(ctx, unapprovedDeletes); err != nil {
		return nil, err
	}

	e.logger.Warn("delete safety threshold held delete actions",
		slog.Int("delete_count", len(unapprovedDeletes)),
		slog.Int("threshold", e.deleteSafetyThreshold),
	)

	return filterActionPlan(plan, func(action *synctypes.Action) bool {
		if !isDeleteAction(action.Type) {
			return true
		}
		_, ok := approved[deleteKeyFromAction(action)]
		return ok
	}), nil
}

func (flow *engineFlow) collectApprovedDeleteChanges(ctx context.Context) []synctypes.PathChanges {
	records, err := flow.engine.baseline.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
	if err != nil {
		flow.engine.logger.Warn("load approved held deletes",
			slog.String("error", err.Error()),
		)
		return nil
	}

	var changes []synctypes.PathChanges
	for i := range records {
		record := records[i]
		row := syncFailureRowFromHeldDelete(&record)
		rebuild := flow.rebuildFailureWork(ctx, &row)
		switch {
		case rebuild.err != nil:
			flow.engine.logger.Warn("rebuild approved held delete",
				slog.String("path", record.Path),
				slog.String("error", rebuild.err.Error()),
			)
		case rebuild.resolved:
			flow.consumeHeldDelete(ctx, &record)
		case rebuild.skipped != nil:
			flow.engine.logger.Warn("approved held delete path is no longer usable",
				slog.String("path", record.Path),
				slog.String("reason", rebuild.skipped.Reason),
			)
		case rebuild.event != nil:
			changes = mergePathChangeBatches(changes, pathChangesFromEvent(rebuild.event))
		}
	}

	return changes
}

func syncFailureRowFromHeldDelete(record *synctypes.HeldDeleteRecord) synctypes.SyncFailureRow {
	return synctypes.SyncFailureRow{
		Path:       record.Path,
		DriveID:    record.DriveID,
		Direction:  synctypes.DirectionDelete,
		Role:       synctypes.FailureRoleItem,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssueDeleteSafetyHeld,
		ItemID:     record.ItemID,
		ActionType: record.ActionType,
	}
}

func (flow *engineFlow) consumeHeldDelete(ctx context.Context, record *synctypes.HeldDeleteRecord) {
	if err := flow.engine.baseline.DeleteHeldDelete(ctx, record.DriveID, record.ActionType, record.Path, record.ItemID); err != nil {
		flow.engine.logger.Warn("consume resolved held delete",
			slog.String("path", record.Path),
			slog.String("error", err.Error()),
		)
	}
}

func (flow *engineFlow) consumeHeldDeleteOnSuccess(ctx context.Context, r *synctypes.WorkerResult) {
	if !isDeleteAction(r.ActionType) {
		return
	}

	driveID := r.DriveID
	if driveID.IsZero() {
		driveID = flow.engine.driveID
	}

	if err := flow.engine.baseline.ConsumeHeldDelete(ctx, driveID, r.ActionType, r.Path, r.ItemID); err != nil {
		flow.engine.logger.Warn("consume approved held delete",
			slog.String("path", r.Path),
			slog.String("error", err.Error()),
		)
	}
}

func (e *Engine) processQueuedConflictResolutions(ctx context.Context) error {
	cutoff := e.nowFunc().Add(-staleConflictResolvingThreshold)
	if _, err := e.baseline.ResetStaleResolvingConflicts(ctx, cutoff); err != nil {
		return fmt.Errorf("sync: reset stale resolving conflicts: %w", err)
	}

	for {
		requests, err := e.baseline.ListRequestedConflictResolutions(ctx, conflictResolutionBatchLimit)
		if err != nil {
			return fmt.Errorf("sync: list requested conflict resolutions: %w", err)
		}
		if len(requests) == 0 {
			return nil
		}

		for i := range requests {
			claimed, ok, err := e.baseline.ClaimConflictResolution(ctx, requests[i].ID)
			if err != nil {
				return fmt.Errorf("sync: claim conflict resolution %s: %w", requests[i].ID, err)
			}
			if !ok {
				continue
			}
			if claimed.RequestedResolution == "" {
				markErr := fmt.Errorf("missing requested resolution")
				if err := e.baseline.MarkConflictResolutionFailed(ctx, claimed.ID, markErr); err != nil {
					return fmt.Errorf("sync: mark missing conflict resolution failed: %w", err)
				}
				continue
			}

			execErr := e.executeConflictResolution(ctx, &claimed.ConflictRecord, claimed.RequestedResolution)
			if execErr != nil {
				if err := e.baseline.MarkConflictResolutionFailed(ctx, claimed.ID, execErr); err != nil {
					return fmt.Errorf("sync: mark conflict resolution failed: %w", err)
				}
				e.logger.Warn("conflict resolution failed",
					slog.String("id", claimed.ID),
					slog.String("path", claimed.Path),
					slog.String("resolution", claimed.RequestedResolution),
					slog.String("error", execErr.Error()),
				)
				continue
			}

			if err := e.baseline.ResolveConflict(ctx, claimed.ID, claimed.RequestedResolution); err != nil {
				return fmt.Errorf("sync: mark conflict resolved: %w", err)
			}
		}
	}
}

func (e *Engine) executeConflictResolution(
	ctx context.Context,
	c *synctypes.ConflictRecord,
	resolution string,
) error {
	switch resolution {
	case synctypes.ResolutionKeepBoth:
		return e.resolveKeepBoth(ctx, c)
	case synctypes.ResolutionKeepLocal:
		return e.resolveKeepLocal(ctx, c)
	case synctypes.ResolutionKeepRemote:
		return e.resolveKeepRemote(ctx, c)
	default:
		return fmt.Errorf("sync: unknown resolution strategy %q", resolution)
	}
}

func (rt *watchRuntime) runUserIntentDispatch(
	ctx context.Context,
	bl *synctypes.Baseline,
	mode synctypes.SyncMode,
	safety *synctypes.SafetyConfig,
) []*synctypes.TrackedAction {
	if rt.depGraph.InFlightCount() > 0 {
		return nil
	}

	if err := rt.engine.processQueuedConflictResolutions(ctx); err != nil {
		rt.engine.logger.Warn("process queued conflict resolutions",
			slog.String("error", err.Error()),
		)
	}

	changes := rt.collectApprovedDeleteChanges(ctx)
	if len(changes) == 0 {
		return nil
	}

	outbox, _ := rt.dispatchWorkRequest(ctx, engineWorkRequest{
		reason:  engineWorkRetry,
		changes: changes,
	}, bl, mode, safety)
	return outbox
}
