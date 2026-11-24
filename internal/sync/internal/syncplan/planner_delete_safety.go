package syncplan

import (
	"path"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/synctree"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The delete-safety hold is a planner post-pass over the admitted action set.
// Per-item checks (hash-verified local deletes, eTag-checked remote deletes)
// prove each delete matches what was synced; they cannot tell an intended
// mass delete from a wrong tree that passed every identity check: an fscrypt
// folder whose key is not loaded, a sync folder restored without its
// contents, or a complete but anomalous remote enumeration. Such a tree reads
// as "every file was deleted" and every per-item check passes (D-002).
//
// Two rules, applied to each direction separately (R-6.2.5):
//   - structural: a side with no entries at all while the baseline holds files
//     has not been emptied by a person deleting things one by one; the side
//     vanished, and every delete toward the other side is held;
//   - proportional: more deletes (files and folders) toward one side than
//     max(DeleteSafetyMinItems, deleteSafetyBaselinePercent of baseline items)
//     are held. The unconfirmed deletes sync already ran toward that side
//     within DeleteSafetyWindow count too, against the baseline as it stood
//     before them, so a deletion spread over many small passes is judged as
//     the one deletion it is.
//
// A third rule holds single deletes rather than a direction: a delete toward
// OneDrive of a recorded mount point whose folder is missing locally, or of
// anything below it, is held however few such deletes there are. While that
// folder is there, a disk that is not mounted plans no deletes (a read
// boundary) and a mounted disk the user emptied is an ordinary delete; a
// missing folder is either a disk the user lets go of or a restored or
// re-pointed sync folder without it, and only a confirmation tells them apart.
//
// Moves never count: a reorganization is not data loss. Everything else in
// the plan continues. A held delete, and any action that depends on one, is
// left out of this plan and recorded as held; it stays pending while the
// observed truth is unchanged and runs on the first pass started with
// `sync --confirm-deletes`, which releases exactly the recorded held set.
const (
	DeleteSafetyMinItems        = 50
	deleteSafetyBaselinePercent = 30
	percentDenominator          = 100
)

// DeleteSafetyInput is the evidence the hold is decided from: the planner's
// baseline and the planner-visible current snapshots of both sides.
type DeleteSafetyInput struct {
	Baseline   *syncdomain.BaselineSnapshot
	LocalRows  []syncdomain.LocalStateRow
	RemoteRows []syncdomain.RemoteStateRow
	// Approved is the held set the user confirmed with `sync
	// --confirm-deletes`: the deletes recorded as held before this pass.
	// Only those are released. A delete that appeared after the user looked
	// is judged by the rules again, so confirming one batch never waves
	// through the next.
	Approved []syncdomain.HeldDelete
	// RecentDeletes counts, per side, the unconfirmed deletes sync ran
	// toward that side within DeleteSafetyWindow before this plan (the
	// store's delete ledger). They count toward the proportional rule.
	RecentDeletes map[syncdomain.DeleteSafetySide]int
	// MountBoundaries are the folders recorded as crossed mount points
	// (mount_boundaries). A delete toward OneDrive of one whose folder is not
	// in LocalRows, or of anything below it, is held whatever the count.
	MountBoundaries []string
}

// ApplyDeleteSafetyHold removes held deletes, and every action that depends
// on one, from plan, rebuilding dependencies when anything was held. It is
// deterministic and performs no I/O.
func ApplyDeleteSafetyHold(plan *syncdomain.ActionPlan, input *DeleteSafetyInput) error {
	if plan == nil || input == nil || len(plan.Actions) == 0 {
		return nil
	}

	baselineItems := countBaselineItems(input.Baseline)
	if baselineItems == 0 {
		return nil
	}

	approved := approvedDeleteIndexes(plan.Actions, input.Approved)
	for i := range approved {
		plan.Actions[i].DeleteConfirmed = true
	}
	reasons := map[syncdomain.ActionType]syncdomain.DeleteSafetyReason{}
	if reason, held := deleteSafetyVerdict(
		plan.Actions, approved, syncdomain.ActionRemoteDelete, baselineItems,
		input.RecentDeletes[syncdomain.DeleteSafetySideRemote], localSideIsEmpty(input.LocalRows),
	); held {
		reasons[syncdomain.ActionRemoteDelete] = reason
	}
	if reason, held := deleteSafetyVerdict(
		plan.Actions, approved, syncdomain.ActionLocalDelete, baselineItems,
		input.RecentDeletes[syncdomain.DeleteSafetySideLocal], remoteSideIsEmpty(input.RemoteRows),
	); held {
		reasons[syncdomain.ActionLocalDelete] = reason
	}
	seeds := heldDeleteSeeds(plan.Actions, approved, reasons, missingMountPoints(input))
	if len(seeds) == 0 {
		return nil
	}

	heldIdx := heldDeleteClosure(plan, seeds)
	kept := make([]syncdomain.Action, 0, len(plan.Actions)-len(heldIdx))
	var held []syncdomain.HeldDelete
	for i := range plan.Actions {
		action := &plan.Actions[i]
		if _, isHeld := heldIdx[i]; !isHeld {
			kept = append(kept, *action)
			continue
		}
		side, isDelete := syncdomain.DeleteSafetySideOf(action.Type)
		if reason, isSeed := seeds[i]; isDelete && isSeed {
			held = append(held, syncdomain.HeldDelete{
				Path:     action.Path,
				Side:     side,
				Reason:   reason,
				ItemType: syncdomain.DeletedItemType(action),
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

// heldDeleteSeeds returns the unapproved deletes the hold keeps, with the
// rule that holds each: every delete of a direction a rule held, and every
// delete toward OneDrive under a missing mount point.
func heldDeleteSeeds(
	actions []syncdomain.Action,
	approved map[int]struct{},
	reasons map[syncdomain.ActionType]syncdomain.DeleteSafetyReason,
	missingMounts []string,
) map[int]syncdomain.DeleteSafetyReason {
	seeds := make(map[int]syncdomain.DeleteSafetyReason)
	for i := range actions {
		if _, isApproved := approved[i]; isApproved {
			continue
		}
		if reason, ok := reasons[actions[i].Type]; ok {
			seeds[i] = reason
			continue
		}
		if actions[i].Type == syncdomain.ActionRemoteDelete && underAnyFolder(actions[i].Path, missingMounts) {
			seeds[i] = syncdomain.DeleteSafetyReasonMountPointGone
		}
	}
	return seeds
}

// missingMountPoints returns the recorded mount points whose folder the
// local snapshot does not hold. A file at a mount point's path does not
// count as its folder.
func missingMountPoints(input *DeleteSafetyInput) []string {
	if len(input.MountBoundaries) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(input.LocalRows))
	for i := range input.LocalRows {
		if input.LocalRows[i].ItemType == syncdomain.ItemTypeFolder {
			present[input.LocalRows[i].Path] = struct{}{}
		}
	}
	var missing []string
	for _, boundary := range input.MountBoundaries {
		if _, ok := present[boundary]; !ok {
			missing = append(missing, boundary)
		}
	}
	return missing
}

// underAnyFolder reports whether itemPath is one of folders or lies below one.
func underAnyFolder(itemPath string, folders []string) bool {
	for _, folder := range folders {
		if itemPath == folder || strings.HasPrefix(itemPath, folder+"/") {
			return true
		}
	}
	return false
}

// approvedDeleteIndexes returns the delete actions the user's confirmation
// covers: same path, same side, and the same item when both record one.
func approvedDeleteIndexes(actions []syncdomain.Action, approved []syncdomain.HeldDelete) map[int]struct{} {
	if len(approved) == 0 {
		return nil
	}
	type key struct {
		path string
		side syncdomain.DeleteSafetySide
	}
	byKey := make(map[key]syncdomain.HeldDelete, len(approved))
	for i := range approved {
		byKey[key{path: approved[i].Path, side: approved[i].Side}] = approved[i]
	}
	indexes := make(map[int]struct{})
	for i := range actions {
		side, isDelete := syncdomain.DeleteSafetySideOf(actions[i].Type)
		if !isDelete {
			continue
		}
		held, ok := byKey[key{path: actions[i].Path, side: side}]
		if !ok {
			continue
		}
		// A confirmation names an item. When the delete knows its item, the
		// held row must name the same one; a row without an ID approves only
		// a delete that has none either.
		if actions[i].ItemID != "" && held.ItemID != actions[i].ItemID {
			continue
		}
		indexes[i] = struct{}{}
	}
	return indexes
}

// deleteSafetyVerdict decides whether the unapproved deletes of deleteType
// are held. Folder deletes count like file deletes: deleting a tree of
// folders is as much a deletion as deleting the files in them. recentDeletes
// are the unconfirmed deletes already run toward the same side within the
// window; they count with this plan's, against the baseline as it stood
// before them. oppositeSideEmpty is whether the side whose absence produced
// them has no entries at all.
func deleteSafetyVerdict(
	actions []syncdomain.Action,
	approved map[int]struct{},
	deleteType syncdomain.ActionType,
	baselineItems int,
	recentDeletes int,
	oppositeSideEmpty bool,
) (syncdomain.DeleteSafetyReason, bool) {
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
		return syncdomain.DeleteSafetyReasonSideVanished, true
	}
	if deletes+recentDeletes > deleteSafetyThreshold(baselineItems+recentDeletes) {
		return syncdomain.DeleteSafetyReasonTooMany, true
	}

	return "", false
}

// deleteSafetyThreshold is the most deletes (files and folders) toward one
// side a pass may run without confirmation, given the synced items they are
// judged against.
func deleteSafetyThreshold(baselineItems int) int {
	proportional := (baselineItems*deleteSafetyBaselinePercent + percentDenominator - 1) / percentDenominator
	return max(DeleteSafetyMinItems, proportional)
}

// heldDeleteClosure returns the indexes of every held delete plus every action
// that depends on one, directly or transitively. A dependent cannot run
// safely alone: the create half of a type-change replacement would land on
// the item the held delete was to remove, and an upload waiting on the delete
// of a case variant would overwrite it in place.
func heldDeleteClosure(plan *syncdomain.ActionPlan, seeds map[int]syncdomain.DeleteSafetyReason) map[int]struct{} {
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
		if _, ok := seeds[i]; ok {
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
func countBaselineItems(baseline syncdomain.BaselineView) int {
	if baseline == nil {
		return 0
	}
	items := 0
	baseline.ForEachPath(func(_ string, entry syncdomain.BaselineEntry) {
		if entry.ItemType != syncdomain.ItemTypeRoot {
			items++
		}
	})
	return items
}

// lostAndFoundName is the folder fsck keeps at the root of ext filesystems.
// A sync folder that is itself a mount point has one even when the disk is
// otherwise empty, so it is not evidence that the side still has content.
const lostAndFoundName = "lost+found"

func localSideIsEmpty(rows []syncdomain.LocalStateRow) bool {
	for i := range rows {
		if rows[i].ItemType == syncdomain.ItemTypeRoot || rows[i].Path == "" || localRowIsNotContent(rows[i].Path) {
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
	return synctree.IsOwnedTransferArtifactName(path.Base(rowPath))
}

func remoteSideIsEmpty(rows []syncdomain.RemoteStateRow) bool {
	for i := range rows {
		if rows[i].ItemType != syncdomain.ItemTypeRoot && rows[i].Path != "" {
			return false
		}
	}
	return true
}
