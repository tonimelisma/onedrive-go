package syncplan

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// dropReconciliationOfComposedMoves removes reconciliation work for an item
// that composition moves this pass. OneDrive moved it onto a path that held
// another synced item, so SQLite reconciles the destination as that item
// changed: a download (or a conflict) when the content differs, which left the
// source behind to be uploaded again as a duplicate (D-058), or, when the
// content is identical and only the eTag moved, a metadata baseline update that
// recorded the moved item at the destination with the replaced file's local
// identity. Composition's move publishes the destination itself.
//
// It also removes the baseline update of an item that has a move mode runs
// and composition holds for the next pass. The item keeps its baseline row
// as the plan's moves leave it: a folder move of this plan can carry the
// item (the baseline follows a committed move), while the update, addressed
// at the path the item had and waiting for a folder made there, would record
// it at that path again; the next pass would read the item as moved on the
// other side and move it back into the folder.
func dropReconciliationOfComposedMoves(
	actions []syncdomain.Action, composition *moveComposition, identified map[string]string, mode syncdomain.SyncMode,
) []syncdomain.Action {
	moved := map[moveCoord]map[string]bool{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range composition.moves {
		moved[composition.moves[i].side][composition.moves[i].key] = true
	}
	// An item found here by its identity (identified) that both sides hold
	// at one place is re-keyed there: the copy here is the item, so what
	// SQLite planned for the path as two files made there (a conflict copy
	// of the edit and a download of OneDrive's version beside it) would split
	// it in two, as a move of it would.
	for i := range composition.rekeys {
		if key := composition.rekeys[i].key; identifiedKey(identified, key) {
			moved[moveCoordLocal][key] = true
		}
	}
	merged := map[string]bool{}
	for i := range composition.merges {
		merged[composition.merges[i].key] = true
	}
	if len(moved[moveCoordLocal]) == 0 && len(moved[moveCoordRemote]) == 0 && len(composition.deferredMoves) == 0 &&
		len(merged) == 0 {
		return actions
	}

	// A merged folder's composed baseline update records it where it is; the
	// one SQLite planned there would only record it again.
	movedOrHeld := func(key string) bool {
		return moved[moveCoordLocal][key] || moved[moveCoordRemote][key] || merged[key] ||
			composition.holdsMoveInMode(key, mode)
	}
	kept := actions[:0:0]
	for i := range actions {
		if !replacedByAComposedMove(&actions[i], moved, movedOrHeld, identified) {
			kept = append(kept, actions[i])
		}
	}

	return kept
}

// downloadsOntoItsOwnRow reports a download at a path whose baseline row is
// the downloaded item's own: new content for the item where it is synced.
func downloadsOntoItsOwnRow(action *syncdomain.Action) bool {
	return action.View != nil && action.View.Baseline != nil && action.View.Baseline.ItemID == action.ItemID
}

// replacedByAComposedMove reports a reconciliation action the plan's moves
// replace (dropReconciliationOfComposedMoves). moved holds the keys each side
// moves; movedOrHeld also answers a move held for a later pass in the mode.
func replacedByAComposedMove(
	action *syncdomain.Action, moved map[moveCoord]map[string]bool, movedOrHeld func(string) bool, identified map[string]string,
) bool {
	switch action.Type {
	case syncdomain.ActionBaselineUpdate:
		return movedOrHeld(action.ItemID)
	case syncdomain.ActionConflictCopy:
		return moved[moveCoordLocal][action.ItemID]
	case syncdomain.ActionDownload:
		// A download of an item the plan moves on either side writes a
		// second copy of it: the move puts the copy here where OneDrive
		// holds it, or OneDrive's copy where this one is (p renamed q on
		// both sides, q/f moved up to p here: f moves to p on OneDrive, and
		// its download at q/f made it twice). So does one at a path the item
		// never had while its move waits for a later pass: OneDrive moved z
		// into x while this computer moved z elsewhere and x's move here
		// waited, and z was downloaded into x beside its copy here.
		return moved[moveCoordLocal][action.ItemID] || moved[moveCoordRemote][action.ItemID] ||
			(movedOrHeld(action.ItemID) && !downloadsOntoItsOwnRow(action))
	case syncdomain.ActionUpload:
		// The file SQLite read as new content at a path another item had
		// is a synced item found by its identity; its moves carry it
		// (movedOntoAnotherItemsPath).
		key, ok := identified[action.Path]
		return ok && movedOrHeld(key)
	case syncdomain.ActionCleanup:
		// The baseline row of an item the plan moves is re-pathed by the
		// move's commit; removed first, the move found no row to move.
		return movedOrHeld(action.ItemID)
	case syncdomain.ActionRemoteDelete:
		// A folder this computer moved onto a path its item left, which
		// SQLite read as gone (folderMovedOntoAPathItsItemLeft): its moves
		// carry it, and the delete would take it from OneDrive.
		return action.ItemID != "" && identifiedKey(identified, action.ItemID) && movedOrHeld(action.ItemID)
	case syncdomain.ActionFolderCreate:
		return makesHereAFolderTheMoveBrings(action, moved)
	case syncdomain.ActionLocalDelete, syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove:
		return false
	default:
		panic(syncdomain.OutsideTheEnum(action.Type))
	}
}

// makesHereAFolderTheMoveBrings reports OneDrive's folder made here at a
// path SQLite reconciled as a type change (OneDrive deleted the synced file
// there and moved the folder onto it, while this computer moved it
// elsewhere): the plan moves the folder's copy here there instead, so the
// folder is made only once.
func makesHereAFolderTheMoveBrings(action *syncdomain.Action, moved map[moveCoord]map[string]bool) bool {
	return action.CreateSide == syncdomain.CreateLocal && action.ItemID != "" && moved[moveCoordLocal][action.ItemID]
}

// identifiedKey reports whether key is the item of a path identified gives.
func identifiedKey(identified map[string]string, key string) bool {
	for _, found := range identified {
		if found == key {
			return true
		}
	}

	return false
}
