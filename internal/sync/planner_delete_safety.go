package sync

import (
	"sort"
)

// The delete-safety hold is a planner post-pass over the admitted action set.
// Per-item checks (hash-verified local deletes, eTag-checked remote deletes)
// prove each delete matches what was synced; they cannot tell an intended
// mass delete from a wrong tree that passed every identity check: an fscrypt
// folder whose key is not loaded, a sync folder restored without its
// contents, or a complete but anomalous remote enumeration. Such a tree reads
// as "every file was deleted" and every per-item check passes (D-002).
//
// Two rules, applied to each direction separately (P-1):
//   - structural: a side with no entries at all while the baseline holds files
//     has not been emptied by a person deleting things one by one; the side
//     vanished, and every delete toward the other side is held;
//   - proportional: more file deletes toward one side than
//     max(deleteSafetyMinFiles, deleteSafetyBaselinePercent of baseline files)
//     are held.
//
// Moves never count: a reorganization is not data loss. Everything else in
// the plan continues. A held delete, and any action that depends on one, is
// left out of this plan and recorded as held; it stays pending while the
// observed truth is unchanged and runs on the first pass started with
// `sync --confirm-deletes`.
const (
	deleteSafetyMinFiles        = 50
	deleteSafetyBaselinePercent = 30
)

// deleteSafetyInput is the evidence the hold is decided from: the planner's
// baseline and the planner-visible current snapshots of both sides.
type deleteSafetyInput struct {
	Baseline   *Baseline
	LocalRows  []localStateRow
	RemoteRows []remoteStateRow
	// Confirmed is the user's `sync --confirm-deletes` for this pass.
	Confirmed bool
}

// applyDeleteSafetyHold removes held deletes, and every action that depends
// on one, from plan, rebuilding dependencies when anything was held. It is
// deterministic and performs no I/O.
func applyDeleteSafetyHold(plan *actionPlan, input *deleteSafetyInput) error {
	if plan == nil || input == nil || input.Confirmed || len(plan.Actions) == 0 {
		return nil
	}

	baselineFiles := countBaselineFiles(input.Baseline)
	if baselineFiles == 0 {
		return nil
	}

	reasons := map[actionType]deleteSafetyReason{}
	if reason, held := deleteSafetyVerdict(
		plan.Actions, ActionRemoteDelete, baselineFiles, localSideIsEmpty(input.LocalRows),
	); held {
		reasons[ActionRemoteDelete] = reason
	}
	if reason, held := deleteSafetyVerdict(
		plan.Actions, ActionLocalDelete, baselineFiles, remoteSideIsEmpty(input.RemoteRows),
	); held {
		reasons[ActionLocalDelete] = reason
	}
	if len(reasons) == 0 {
		return nil
	}

	heldIdx := heldDeleteClosure(plan, reasons)
	kept := make([]Action, 0, len(plan.Actions)-len(heldIdx))
	var held []heldDelete
	for i := range plan.Actions {
		action := &plan.Actions[i]
		if _, isHeld := heldIdx[i]; !isHeld {
			kept = append(kept, *action)
			continue
		}
		if reason, isDelete := reasons[action.Type]; isDelete {
			held = append(held, heldDelete{
				Path:     action.Path,
				Side:     deleteSafetySideFor(action.Type),
				Reason:   reason,
				ItemType: deletedItemType(action),
			})
		}
	}

	deps := buildDependencies(kept)
	if err := detectDependencyCycle(deps); err != nil {
		return err
	}

	sort.Slice(held, func(i, j int) bool {
		if held[i].Side != held[j].Side {
			return held[i].Side < held[j].Side
		}
		return held[i].Path < held[j].Path
	})
	plan.Actions = kept
	plan.Deps = deps
	plan.HeldDeletes = held

	return nil
}

// deleteSafetyVerdict decides whether deletes of deleteType are held.
// oppositeSideEmpty is whether the side whose absence produced them has no
// entries at all.
func deleteSafetyVerdict(
	actions []Action,
	deleteType actionType,
	baselineFiles int,
	oppositeSideEmpty bool,
) (deleteSafetyReason, bool) {
	fileDeletes := 0
	anyDelete := false
	for i := range actions {
		if actions[i].Type != deleteType {
			continue
		}
		anyDelete = true
		if deletedItemType(&actions[i]) == ItemTypeFile {
			fileDeletes++
		}
	}
	if !anyDelete {
		return "", false
	}
	if oppositeSideEmpty {
		return deleteSafetyReasonSideVanished, true
	}
	if fileDeletes > deleteSafetyThreshold(baselineFiles) {
		return deleteSafetyReasonTooMany, true
	}

	return "", false
}

// deleteSafetyThreshold is the most file deletes toward one side a pass may
// run without confirmation.
func deleteSafetyThreshold(baselineFiles int) int {
	proportional := (baselineFiles*deleteSafetyBaselinePercent + 99) / 100
	return max(deleteSafetyMinFiles, proportional)
}

// heldDeleteClosure returns the indexes of every held delete plus every action
// that depends on one, directly or transitively. A dependent cannot run
// safely alone: the create half of a type-change replacement would land on
// the item the held delete was to remove, and an upload waiting on the delete
// of a case variant would overwrite it in place.
func heldDeleteClosure(plan *actionPlan, reasons map[actionType]deleteSafetyReason) map[int]struct{} {
	deps := plan.Deps
	if len(deps) != len(plan.Actions) {
		deps = buildDependencies(plan.Actions)
	}
	dependents := make([][]int, len(plan.Actions))
	for i := range deps {
		for _, dep := range deps[i] {
			dependents[dep] = append(dependents[dep], i)
		}
	}

	held := make(map[int]struct{})
	var queue []int
	for i := range plan.Actions {
		if _, ok := reasons[plan.Actions[i].Type]; ok {
			held[i] = struct{}{}
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, dependent := range dependents[next] {
			if _, seen := held[dependent]; seen {
				continue
			}
			held[dependent] = struct{}{}
			queue = append(queue, dependent)
		}
	}

	return held
}

func countBaselineFiles(baseline *Baseline) int {
	if baseline == nil {
		return 0
	}
	files := 0
	baseline.ForEachPath(func(_ string, entry *BaselineEntry) {
		if entry != nil && entry.ItemType == ItemTypeFile {
			files++
		}
	})
	return files
}

func localSideIsEmpty(rows []localStateRow) bool {
	for i := range rows {
		if rows[i].ItemType != ItemTypeRoot && rows[i].Path != "" {
			return false
		}
	}
	return true
}

func remoteSideIsEmpty(rows []remoteStateRow) bool {
	for i := range rows {
		if rows[i].ItemType != ItemTypeRoot && rows[i].Path != "" {
			return false
		}
	}
	return true
}
