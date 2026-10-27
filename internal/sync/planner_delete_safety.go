package sync

import (
	"path"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
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
//   - proportional: more deletes (files and folders) toward one side than
//     max(deleteSafetyMinFiles, deleteSafetyBaselinePercent of baseline items)
//     are held.
//
// Moves never count: a reorganization is not data loss. Everything else in
// the plan continues. A held delete, and any action that depends on one, is
// left out of this plan and recorded as held; it stays pending while the
// observed truth is unchanged and runs on the first pass started with
// `sync --confirm-deletes`, which releases exactly the recorded held set.
const (
	deleteSafetyMinFiles        = 50
	deleteSafetyBaselinePercent = 30
	percentDenominator          = 100
)

// deleteSafetyInput is the evidence the hold is decided from: the planner's
// baseline and the planner-visible current snapshots of both sides.
type deleteSafetyInput struct {
	Baseline   *Baseline
	LocalRows  []localStateRow
	RemoteRows []remoteStateRow
	// Approved is the held set the user confirmed with `sync
	// --confirm-deletes`: the deletes recorded as held before this pass.
	// Only those are released. A delete that appeared after the user looked
	// is judged by the rules again, so confirming one batch never waves
	// through the next.
	Approved []heldDelete
}

// applyDeleteSafetyHold removes held deletes, and every action that depends
// on one, from plan, rebuilding dependencies when anything was held. It is
// deterministic and performs no I/O.
func applyDeleteSafetyHold(plan *actionPlan, input *deleteSafetyInput) error {
	if plan == nil || input == nil || len(plan.Actions) == 0 {
		return nil
	}

	baselineItems := countBaselineItems(input.Baseline)
	if baselineItems == 0 {
		return nil
	}

	approved := approvedDeleteIndexes(plan.Actions, input.Approved)
	reasons := map[actionType]deleteSafetyReason{}
	if reason, held := deleteSafetyVerdict(
		plan.Actions, approved, ActionRemoteDelete, baselineItems, localSideIsEmpty(input.LocalRows),
	); held {
		reasons[ActionRemoteDelete] = reason
	}
	if reason, held := deleteSafetyVerdict(
		plan.Actions, approved, ActionLocalDelete, baselineItems, remoteSideIsEmpty(input.RemoteRows),
	); held {
		reasons[ActionLocalDelete] = reason
	}
	if len(reasons) == 0 {
		return nil
	}

	heldIdx := heldDeleteClosure(plan, reasons, approved)
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
				ItemID:   action.ItemID,
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

// approvedDeleteIndexes returns the delete actions the user's confirmation
// covers: same path, same side, and the same item when both record one.
func approvedDeleteIndexes(actions []Action, approved []heldDelete) map[int]struct{} {
	if len(approved) == 0 {
		return nil
	}
	type key struct {
		path string
		side deleteSafetySide
	}
	byKey := make(map[key]heldDelete, len(approved))
	for i := range approved {
		byKey[key{path: approved[i].Path, side: approved[i].Side}] = approved[i]
	}
	indexes := make(map[int]struct{})
	for i := range actions {
		if actions[i].Type != ActionLocalDelete && actions[i].Type != ActionRemoteDelete {
			continue
		}
		held, ok := byKey[key{path: actions[i].Path, side: deleteSafetySideFor(actions[i].Type)}]
		if !ok {
			continue
		}
		if held.ItemID != "" && actions[i].ItemID != "" && held.ItemID != actions[i].ItemID {
			continue
		}
		indexes[i] = struct{}{}
	}
	return indexes
}

// deleteSafetyVerdict decides whether the unapproved deletes of deleteType
// are held. Folder deletes count like file deletes: deleting a tree of
// folders is as much a deletion as deleting the files in them.
// oppositeSideEmpty is whether the side whose absence produced them has no
// entries at all.
func deleteSafetyVerdict(
	actions []Action,
	approved map[int]struct{},
	deleteType actionType,
	baselineItems int,
	oppositeSideEmpty bool,
) (deleteSafetyReason, bool) {
	deletes := 0
	for i := range actions {
		if actions[i].Type != deleteType {
			continue
		}
		if _, ok := approved[i]; ok {
			continue
		}
		deletes++
	}
	if deletes == 0 {
		return "", false
	}
	if oppositeSideEmpty {
		return deleteSafetyReasonSideVanished, true
	}
	if deletes > deleteSafetyThreshold(baselineItems) {
		return deleteSafetyReasonTooMany, true
	}

	return "", false
}

// deleteSafetyThreshold is the most file deletes toward one side a pass may
// run without confirmation.
func deleteSafetyThreshold(baselineFiles int) int {
	proportional := (baselineFiles*deleteSafetyBaselinePercent + percentDenominator - 1) / percentDenominator
	return max(deleteSafetyMinFiles, proportional)
}

// heldDeleteClosure returns the indexes of every held delete plus every action
// that depends on one, directly or transitively. A dependent cannot run
// safely alone: the create half of a type-change replacement would land on
// the item the held delete was to remove, and an upload waiting on the delete
// of a case variant would overwrite it in place.
func heldDeleteClosure(
	plan *actionPlan,
	reasons map[actionType]deleteSafetyReason,
	approved map[int]struct{},
) map[int]struct{} {
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
		if _, isApproved := approved[i]; isApproved {
			continue
		}
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

// countBaselineItems counts synced files and folders.
func countBaselineItems(baseline *Baseline) int {
	if baseline == nil {
		return 0
	}
	items := 0
	baseline.ForEachPath(func(_ string, entry *BaselineEntry) {
		if entry != nil && entry.ItemType != ItemTypeRoot {
			items++
		}
	})
	return items
}

// lostAndFoundName is the folder fsck keeps at the root of ext filesystems.
// A sync folder that is itself a mount point has one even when the disk is
// otherwise empty, so it is not evidence that the side still has content.
const lostAndFoundName = "lost+found"

func localSideIsEmpty(rows []localStateRow) bool {
	for i := range rows {
		if rows[i].ItemType == ItemTypeRoot || rows[i].Path == "" || localRowIsNotContent(rows[i].Path) {
			continue
		}
		return false
	}
	return true
}

// localRowIsNotContent reports rows that never make a local side non-empty:
// lost+found and the client's own transfer and set-aside artifacts.
func localRowIsNotContent(rowPath string) bool {
	if rowPath == lostAndFoundName || strings.HasPrefix(rowPath, lostAndFoundName+"/") {
		return true
	}
	return driveops.IsOwnedTransferArtifactName(path.Base(rowPath))
}

func remoteSideIsEmpty(rows []remoteStateRow) bool {
	for i := range rows {
		if rows[i].ItemType != ItemTypeRoot && rows[i].Path != "" {
			return false
		}
	}
	return true
}
