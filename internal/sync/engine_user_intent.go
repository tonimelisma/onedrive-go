package sync

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"
)

const (
	plannerSafetyMax                = math.MaxInt32
	conflictResolutionBatchLimit    = 100
	staleConflictResolvingThreshold = time.Hour
)

type heldDeleteKey struct {
	driveID    string
	actionType ActionType
	path       string
	itemID     string
}

type heldDeletePathKey struct {
	driveID    string
	actionType ActionType
	path       string
}

func deleteKeyFromAction(action *Action) heldDeleteKey {
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

func deleteKeyFromRecord(record *HeldDeleteRecord) heldDeleteKey {
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

func plannedDeleteKeys(plan *ActionPlan) map[heldDeleteKey]struct{} {
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

func plannedDeleteItemsByPath(plan *ActionPlan) map[heldDeletePathKey]map[string]struct{} {
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
	plan *ActionPlan,
) (map[heldDeleteKey]struct{}, error) {
	approved, err := e.baseline.ListHeldDeletesByState(ctx, HeldDeleteStateApproved)
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
	plan *ActionPlan,
	keepAction func(*Action) bool,
) *ActionPlan {
	if plan == nil || len(plan.Actions) == 0 {
		return plan
	}

	kept := make([]Action, 0, len(plan.Actions))
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

	return &ActionPlan{Actions: kept, Deps: keptDeps}
}

func (e *Engine) holdDeleteActions(ctx context.Context, actions []Action) error {
	if len(actions) == 0 {
		return nil
	}

	now := e.nowFunc().UnixNano()
	records := make([]HeldDeleteRecord, 0, len(actions))
	for i := range actions {
		records = append(records, HeldDeleteRecord{
			DriveID:       actions[i].DriveID,
			ItemID:        actions[i].ItemID,
			Path:          actions[i].Path,
			ActionType:    actions[i].Type,
			State:         HeldDeleteStateHeld,
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
	plan *ActionPlan,
) (*ActionPlan, error) {
	if plan == nil {
		return &ActionPlan{}, nil
	}

	approved, err := e.approvedDeleteKeysForPlan(ctx, plan)
	if err != nil {
		return nil, fmt.Errorf("load approved deletes: %w", err)
	}

	if len(plan.Actions) == 0 || e.deleteSafetyThreshold <= 0 {
		return plan, nil
	}

	var unapprovedDeletes []Action
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

	return filterActionPlan(plan, func(action *Action) bool {
		if !isDeleteAction(action.Type) {
			return true
		}
		_, ok := approved[deleteKeyFromAction(action)]
		return ok
	}), nil
}

func (flow *engineFlow) collectApprovedDeleteChanges(ctx context.Context) []PathChanges {
	bl, err := flow.engine.baseline.Load(ctx)
	if err != nil {
		flow.engine.logger.Warn("load baseline for approved held deletes",
			slog.String("error", err.Error()),
		)
		return nil
	}

	records, err := flow.engine.baseline.ListHeldDeletesByState(ctx, HeldDeleteStateApproved)
	if err != nil {
		flow.engine.logger.Warn("load approved held deletes",
			slog.String("error", err.Error()),
		)
		return nil
	}

	var changes []PathChanges
	for i := range records {
		record := records[i]
		row := syncFailureRowFromHeldDelete(&record)
		rebuild := flow.buildRetryCandidate(ctx, bl, &row)
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

func syncFailureRowFromHeldDelete(record *HeldDeleteRecord) SyncFailureRow {
	return SyncFailureRow{
		Path:       record.Path,
		DriveID:    record.DriveID,
		Direction:  DirectionDelete,
		Role:       FailureRoleItem,
		Category:   CategoryActionable,
		IssueType:  IssueDeleteSafetyHeld,
		ItemID:     record.ItemID,
		ActionType: record.ActionType,
	}
}

func (flow *engineFlow) consumeHeldDelete(ctx context.Context, record *HeldDeleteRecord) {
	if err := flow.engine.baseline.DeleteHeldDelete(ctx, record.DriveID, record.ActionType, record.Path, record.ItemID); err != nil {
		flow.engine.logger.Warn("consume resolved held delete",
			slog.String("path", record.Path),
			slog.String("error", err.Error()),
		)
	}
}

func (flow *engineFlow) consumeHeldDeleteOnSuccess(ctx context.Context, r *WorkerResult) {
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

func (e *Engine) processQueuedConflictResolutions(ctx context.Context) ([]PathChanges, error) {
	cutoff := e.nowFunc().Add(-staleConflictResolvingThreshold)
	if _, err := e.baseline.ResetStaleResolvingConflicts(ctx, cutoff); err != nil {
		return nil, fmt.Errorf("sync: reset stale applying conflicts: %w", err)
	}

	var followUpChanges []PathChanges
	attempted := make(map[string]struct{})
	for {
		requests, err := e.baseline.ListRequestedConflictResolutions(ctx, conflictResolutionBatchLimit)
		if err != nil {
			return nil, fmt.Errorf("sync: list requested conflict resolutions: %w", err)
		}
		if len(requests) == 0 {
			return followUpChanges, nil
		}

		progressed := false
		for i := range requests {
			if _, seen := attempted[requests[i].ID]; seen {
				continue
			}

			claimed, ok, err := e.baseline.ClaimConflictResolution(ctx, requests[i].ID)
			if err != nil {
				return nil, fmt.Errorf("sync: claim conflict resolution %s: %w", requests[i].ID, err)
			}
			if !ok {
				continue
			}

			progressed = true
			attempted[claimed.ID] = struct{}{}
			if claimed.RequestedResolution == "" {
				markErr := fmt.Errorf("missing requested resolution")
				if err := e.baseline.MarkConflictResolutionFailed(ctx, claimed.ID, markErr); err != nil {
					return nil, fmt.Errorf("sync: mark missing conflict resolution failed: %w", err)
				}
				continue
			}

			affectedPaths, execErr := e.executeConflictResolution(ctx, &claimed.ConflictRecord, claimed.RequestedResolution)
			if execErr != nil {
				if err := e.baseline.MarkConflictResolutionFailed(ctx, claimed.ID, execErr); err != nil {
					return nil, fmt.Errorf("sync: return conflict resolution to queue: %w", err)
				}
				e.logger.Warn("conflict resolution attempt failed",
					slog.String("id", claimed.ID),
					slog.String("path", claimed.Path),
					slog.String("resolution", claimed.RequestedResolution),
					slog.String("error", execErr.Error()),
				)
				continue
			}

			if err := e.baseline.ResolveConflict(ctx, claimed.ID, claimed.RequestedResolution); err != nil {
				return nil, fmt.Errorf("sync: mark conflict resolved: %w", err)
			}

			followUpChanges = mergePathChangeBatches(
				followUpChanges,
				e.conflictResolutionFollowUpChanges(ctx, affectedPaths),
			)
		}

		if !progressed {
			return followUpChanges, nil
		}
	}
}

func (e *Engine) executeConflictResolution(
	ctx context.Context,
	c *ConflictRecord,
	resolution string,
) ([]string, error) {
	switch resolution {
	case ResolutionKeepBoth:
		return e.resolveKeepBoth(ctx, c)
	case ResolutionKeepLocal:
		return e.resolveKeepLocal(ctx, c)
	case ResolutionKeepRemote:
		return e.resolveKeepRemote(ctx, c)
	default:
		return nil, fmt.Errorf("sync: unknown resolution strategy %q", resolution)
	}
}

func (rt *watchRuntime) runUserIntentDispatch(
	ctx context.Context,
	bl *Baseline,
	mode Mode,
	safety *SafetyConfig,
) []*TrackedAction {
	if rt.depGraph.InFlightCount() > 0 {
		return nil
	}

	conflictChanges, err := rt.engine.processQueuedConflictResolutions(ctx)
	if err != nil {
		rt.engine.logger.Warn("process queued conflict resolutions",
			slog.String("error", err.Error()),
		)
	}

	changes := mergePathChangeBatches(conflictChanges, rt.collectApprovedDeleteChanges(ctx))
	if len(changes) == 0 {
		return nil
	}

	outbox, _ := rt.dispatchWorkRequest(ctx, engineWorkRequest{
		reason:  engineWorkRetry,
		changes: changes,
	}, bl, mode, safety)
	return outbox
}
