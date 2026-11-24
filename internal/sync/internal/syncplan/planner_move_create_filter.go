package syncplan

import (
	"log/slog"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// dropCreatesOntoComposedItems removes creates of another item at a path a
// composed item still holds on that side this pass. Rebased remote-origin
// writes can target the local position of an item whose move waits for a
// later pass (or was undone to OneDrive's location); run anyway, the create
// failed on the occupied path, and a local folder move waiting for the writes
// under it never ran. Next pass the item has moved and the create is planned
// again. So is a create at or below the path of a new folder a deferred item
// moves into at its own path (ownLandings), which a later pass makes once the
// item has left: run, it went into the item, which still had the path, and a
// folder made so between the new folder and the item's target held the
// item's own landing, which the item cannot move into, so the syncs stalled
// once the move aside failed. A new item's create on OneDrive at a path onto
// which a folder move of the plan carries another OneDrive item waits too
// (carriedOntoByRemoteMove), with the creates below it.
//
// A create on OneDrive below such a path whose source here does not lie in
// the item holding it is removed too (belowAnotherItemsFolder). Here the
// planner keeps such a create: a move of the plan into the folder it makes
// relies on it, and the item's move carries it along, so the next pass moves
// it where it belongs (TestRunOnce_FolderMovedOnOneDriveIntoNewFoldersWhereAFolderMovedDeeperLeftConverges).
//
// It also removes a create here rebased from below a folder's path on
// OneDrive to where the folder sits here, while the folder's move on OneDrive
// waits for a later pass (heldFolderRebases), and logs each at Debug with the
// folder. The rebase writes into the folder here as if OneDrive had moved it,
// and two such writes went wrong: a download that applies a move OneDrive
// made into the folder wrote a second copy of the item while it stayed at its
// old path here, and went back to OneDrive as a new file, and a file OneDrive
// made in the folder under a name it takes for that of a file made here in it
// was downloaded beside that file, so the two sides never met. A file OneDrive
// made there under a free name waits too: it reaches this computer in the
// pass that moves the folder. A write SQLite reconciles at the folder's path
// here is not rebased and is not held. It returns the actions it keeps and
// the creates it removes.
//
// A create here at a name an item that stays put here holds under OneDrive's
// name comparison (namekey.OfPath), not as spelled, is removed too
// (foldedHeldPaths): a case-insensitive volume, as macOS's is by default,
// finds that item's entry under the new name, so the create adopted it, and
// a folder renamed here to x2 while OneDrive made X2 became X2, whose later
// rename on OneDrive carried x2's files away. A byte-exact volume would make
// a second entry that OneDrive takes for the first one's name, which syncs
// no better; the create runs once the item has left the name.
func dropCreatesOntoComposedItems(
	logger *slog.Logger, actions []syncdomain.Action, inputs *moveInputs, composition *moveComposition,
) (kept, dropped []syncdomain.Action) {
	heldBy := pathsHeldByUnmovedItems(inputs, composition)
	heldLocally := foldedHeldPaths(heldBy[moveCoordLocal])
	vacated := pathsVacatedByPlan(actions)
	remoteMoves := remoteMoveBoundaries(actions)
	heldRebases := heldFolderRebases(actions, inputs, composition)
	nodeByKey := make(map[string]*moveNode, len(inputs.nodes))
	for i := range inputs.nodes {
		nodeByKey[inputs.nodes[i].key] = &inputs.nodes[i]
	}

	heldRows := baselineRowsOfHeldItems(actions, inputs, composition)
	var ontoHeldNames []syncdomain.Action
	kept = actions[:0:0]
	for i := range actions {
		action := &actions[i]
		if movesOntoAHeldItemsRow(logger, action, heldRows) {
			dropped = append(dropped, *action)
			continue
		}
		side, isCreate := createSide(action)
		if !isCreate {
			kept = append(kept, *action)
			continue
		}
		if exact, onto := createsOntoAHeldPath(logger, action, heldBy[side], side, heldLocally); onto {
			if exact {
				dropped = append(dropped, *action)
			} else {
				ontoHeldNames = append(ontoHeldNames, *action)
			}
			continue
		}
		if folder, below := belowAnotherItemsFolder(action, side, heldBy[side], nodeByKey); below && side == moveCoordRemote {
			logCreateBelowAnotherItemsFolder(logger, action, folder)
			dropped = append(dropped, *action)
			continue
		}
		if atOrBelowOwnLanding(action.Path, composition.ownLandings[side]) ||
			slices.ContainsFunc(composition.asideFolders, func(aside string) bool {
				return pathAtOrBelow(syncdomain.ActionViewPath(action), aside)
			}) {
			// Below a folder renamed to a conflict copy this pass, the create
			// is planned again at its new path by the next.
			dropped = append(dropped, *action)
			continue
		}
		if meetsAnItemARemoteMoveCarries(logger, action, side, remoteMoves, &inputs.occupancy, vacated[side]) ||
			rebasedCreateWaits(logger, action, side, &inputs.occupancy, vacated[side], heldRebases) {
			dropped = append(dropped, *action)
			continue
		}
		kept = append(kept, *action)
	}

	kept, dropped = dropCreatesBelowDroppedCreates(kept, dropped)
	kept, ontoHeldNames = dropCreatesBelowSpelled(kept, ontoHeldNames)

	return kept, append(dropped, ontoHeldNames...)
}

// meetsAnItemARemoteMoveCarries reports, logging it at Debug, a new item's
// create on OneDrive at a path onto which a folder move of the plan carries
// another OneDrive item (carriedOntoByRemoteMove). A create with an item ID
// writes that item, wherever a move takes it, so it is never the case.
func meetsAnItemARemoteMoveCarries(
	logger *slog.Logger, action *syncdomain.Action, side moveCoord,
	remoteMoves []graphFolderMoveBoundary, occupancy *moveOccupancy, vacated map[string]bool,
) bool {
	if side != moveCoordRemote || action.ItemID != "" {
		return false
	}
	carried, onto := carriedOntoByRemoteMove(action.Path, remoteMoves, occupancy, vacated)
	if onto {
		logCreateOntoCarriedItem(logger, action, carried)
	}

	return onto
}

// rebasedCreateWaits reports a create a rebase moved off the path SQLite
// reconciled it at that waits for the next pass: onto a path the snapshot
// holds (rebasedCreateBlocked), or, here, through a folder whose move on
// OneDrive waits (rebasedThroughHeldFolder, logged at Debug).
func rebasedCreateWaits(
	logger *slog.Logger, action *syncdomain.Action, side moveCoord,
	occupancy *moveOccupancy, vacated map[string]bool, heldRebases []graphFolderMoveBoundary,
) bool {
	if action.Path == syncdomain.ActionViewPath(action) {
		return false
	}
	if rebasedCreateBlocked(action.Path, side, occupancy, vacated) {
		return true
	}
	if side != moveCoordLocal {
		return false
	}
	folder, through := rebasedThroughHeldFolder(action, heldRebases)
	if through {
		logRebasedCreateHeld(logger, action, folder)
	}

	return through
}

// dropCreatesBelowSpelled removes, with the creates already dropped, every
// create here below one of them as spelled. A create held off a name an item
// here holds (foldedHeldPaths) leaves its own children no folder, but a
// create into the holder, spelled as the holder is, has its folder: OneDrive's
// file in a folder whose move waits is downloaded where the folder sits here,
// which OneDrive's comparison would take for the held create's name.
func dropCreatesBelowSpelled(kept, dropped []syncdomain.Action) (stillKept, allDropped []syncdomain.Action) {
	if len(dropped) == 0 {
		return kept, dropped
	}

	stillKept = kept[:0:0]
	for i := range kept {
		_, isCreate := createSide(&kept[i])
		if isCreate && slices.ContainsFunc(dropped, func(held syncdomain.Action) bool {
			return pathAtOrBelow(kept[i].Path, held.Path) && kept[i].Path != held.Path
		}) {
			dropped = append(dropped, kept[i])
			continue
		}
		stillKept = append(stillKept, kept[i])
	}

	return stillKept, dropped
}

// movesOntoAHeldItemsRow reports a move of another item onto the baseline
// path of an item whose move waits (baselineRowsOfHeldItems), logging it at
// Debug.
func movesOntoAHeldItemsRow(logger *slog.Logger, action *syncdomain.Action, heldRows map[string]string) bool {
	holder, held := heldRows[action.Path]
	if !held || !isMoveAction(action) || holder == action.ItemID {
		return false
	}
	logMoveOntoAHeldItemsRow(logger, action, holder)

	return true
}

// baselineRowsOfHeldItems maps the baseline path of each item whose move
// the plan holds for a later pass, on either side, to that item, unless the
// plan moves the item too (a swap's item parked at a temporary name, whose
// move re-paths its row). A move of
// another item onto such a path waits for the next pass too
// (dropCreatesOntoComposedItems): its commit records the other item's row at
// the path, which the store keys by path as well as by item ID, so the held
// item lost its row, and the next pass read its two copies as two new files
// (from the held start, OneDrive moved p/f into X2 and z onto p/f while f's
// move here into the new x waited: f was downloaded at X2/f and uploaded
// again from x/f).
func baselineRowsOfHeldItems(actions []syncdomain.Action, inputs *moveInputs, composition *moveComposition) map[string]string {
	held := keysStillToMove(actions, composition)
	if len(held[moveCoordLocal]) == 0 && len(held[moveCoordRemote]) == 0 {
		return nil
	}
	moving := map[string]bool{}
	for i := range actions {
		if isMoveAction(&actions[i]) {
			moving[actions[i].ItemID] = true
		}
	}
	rows := map[string]string{}
	for i := range inputs.nodes {
		n := &inputs.nodes[i]
		if (held[moveCoordLocal][n.key] || held[moveCoordRemote][n.key]) && !moving[n.key] && n.paths[moveCoordBaseline] != "" {
			rows[n.paths[moveCoordBaseline]] = n.key
		}
	}

	return rows
}

// logMoveOntoAHeldItemsRow logs at Debug a move held for the next pass
// because it lands on the baseline path of an item whose own move waits.
func logMoveOntoAHeldItemsRow(logger *slog.Logger, action *syncdomain.Action, holder string) {
	if logger == nil {
		return
	}
	logger.Debug("move waits for the item whose synced path it lands on",
		slog.String("action", action.Type.String()),
		slog.String("path", action.Path),
		slog.String("held_item_id", holder))
}

// dropCreatesBelowDroppedCreates removes, with the creates already dropped,
// every create below one of them on the same side: a dropped folder create
// leaves its children no folder to land in. A new folder OneDrive made in a
// folder renamed differently on both sides, at the name of a new file made
// here, had its create here dropped (the file holds the path), while the
// download of its child, rebased below the file, was kept: it failed every
// pass and held the folder's move, so sync never converged (chaos seed 87).
// The next pass, which has moved the folder, plans both at one path.
func dropCreatesBelowDroppedCreates(kept, dropped []syncdomain.Action) (stillKept, allDropped []syncdomain.Action) {
	droppedAt := [moveCoordCount]map[namekey.PathKey]bool{}
	for i := range dropped {
		if side, isCreate := createSide(&dropped[i]); isCreate {
			if droppedAt[side] == nil {
				droppedAt[side] = map[namekey.PathKey]bool{}
			}
			droppedAt[side][namekey.OfPath(dropped[i].Path)] = true
		}
	}
	if droppedAt[moveCoordLocal] == nil && droppedAt[moveCoordRemote] == nil {
		return kept, dropped
	}

	stillKept = kept[:0:0]
	for i := range kept {
		side, isCreate := createSide(&kept[i])
		if isCreate && belowDroppedCreate(kept[i].Path, droppedAt[side]) {
			dropped = append(dropped, kept[i])
			continue
		}
		stillKept = append(stillKept, kept[i])
	}

	return stillKept, dropped
}

// belowDroppedCreate reports whether p lies, under OneDrive's name folding,
// below one of the paths in dropped.
func belowDroppedCreate(p string, dropped map[namekey.PathKey]bool) bool {
	if len(dropped) == 0 {
		return false
	}
	for dir := range pathAncestors(namekey.OfPath(p)) {
		if dropped[dir] {
			return true
		}
	}

	return false
}

// belowAnotherItemsFolder reports the nearest folder above a create on side
// that a synced item still holds there this pass (held, pathsHeldByUnmovedItems)
// when the entry the create comes from does not lie in that item on the other
// side: the create's source path (its view's) is not below where the item sits
// there. dropCreatesOntoComposedItems asks it for creates on OneDrive. The two are different folders at one path, told apart by the item:
// here x renamed x2, while OneDrive keeps it at x (it made X2, which it takes
// for x2's name), and a new x with a folder sub in it made here. The create of
// sub on OneDrive, sent, was made inside the old x, carried to x2/sub once x
// moved, and moved back by the next pass. It waits a pass, as a move into the
// new folder does, and is held work, so a delete of what it writes waits too.
// A create rebased into the folder here from below the item's path on OneDrive
// lies in the item on both sides and is kept.
func belowAnotherItemsFolder(
	action *syncdomain.Action, side moveCoord, held map[string]string, nodeByKey map[string]*moveNode,
) (string, bool) {
	if len(held) == 0 {
		return "", false
	}
	other := moveCoordLocal
	if side == moveCoordLocal {
		other = moveCoordRemote
	}
	source := syncdomain.ActionViewPath(action)
	for dir := path.Dir(action.Path); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		key, isHeld := held[dir]
		if !isHeld {
			continue
		}
		n := nodeByKey[key]
		if n == nil || n.itemType != syncdomain.ItemTypeFolder || pathAtOrBelow(source, n.paths[other]) {
			return "", false
		}

		return dir, true
	}

	return "", false
}

// logCreateBelowAnotherItemsFolder logs, at Debug, a create that waits for the
// item holding a folder's path above it to leave (belowAnotherItemsFolder).
func logCreateBelowAnotherItemsFolder(logger *slog.Logger, action *syncdomain.Action, folder string) {
	if logger == nil {
		return
	}
	logger.Debug("create waits for another item to leave the folder path above it",
		slog.String("action", action.Type.String()),
		slog.String("path", action.Path),
		slog.String("folder", folder))
}

// atOrBelowOwnLanding reports whether p is, under OneDrive's name folding,
// the path of a new folder a deferred item moves into at its own path on
// that side (ownLandings), or lies below one.
func atOrBelowOwnLanding(p string, landings map[namekey.PathKey]bool) bool {
	if len(landings) == 0 {
		return false
	}
	key := namekey.OfPath(p)
	if landings[key] {
		return true
	}
	for dir := range pathAncestors(key) {
		if landings[dir] {
			return true
		}
	}

	return false
}

// logRebasedCreateHeld logs, at Debug, a create here that waits for the move
// on OneDrive of the folder it was rebased through: the folder's path here and
// the one OneDrive keeps until the move runs.
func logRebasedCreateHeld(logger *slog.Logger, action *syncdomain.Action, folder graphFolderMoveBoundary) {
	if logger == nil {
		return
	}
	logger.Debug("create waits for its folder's move on OneDrive",
		slog.String("action", action.Type.String()),
		slog.String("path", action.Path),
		slog.String("folder", strings.TrimSuffix(folder.newPrefix, "/")),
		slog.String("folder_on_onedrive", strings.TrimSuffix(folder.oldPrefix, "/")))
}

// heldFolderRebases lists the folders still to be moved on OneDrive
// (keysStillToMove) that sit elsewhere here, as the rebase of writes here maps
// them (remoteToLocalFolderPrefixes).
func heldFolderRebases(actions []syncdomain.Action, inputs *moveInputs, composition *moveComposition) []graphFolderMoveBoundary {
	held := keysStillToMove(actions, composition)[moveCoordRemote]
	if len(held) == 0 {
		return nil
	}
	var nodes []moveNode
	for i := range inputs.nodes {
		if held[inputs.nodes[i].key] {
			nodes = append(nodes, inputs.nodes[i])
		}
	}

	return remoteToLocalFolderPrefixes(nodes)
}

// rebasedThroughHeldFolder reports the held folder a create here was rebased
// through: from below its path on OneDrive to below where it sits here.
func rebasedThroughHeldFolder(action *syncdomain.Action, held []graphFolderMoveBoundary) (graphFolderMoveBoundary, bool) {
	viewPath := syncdomain.ActionViewPath(action)
	for _, folder := range held {
		if strings.HasPrefix(viewPath, folder.oldPrefix) && strings.HasPrefix(action.Path, folder.newPrefix) {
			return folder, true
		}
	}

	return graphFolderMoveBoundary{}, false
}

// pathsHeldByUnmovedItems maps, per side, the paths composed items still hold
// this pass (no move of theirs on that side) to their keys.
func pathsHeldByUnmovedItems(inputs *moveInputs, composition *moveComposition) map[moveCoord]map[string]string {
	movedOn := map[moveCoord]map[string]bool{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range composition.moves {
		movedOn[composition.moves[i].side][composition.moves[i].key] = true
	}
	heldBy := map[moveCoord]map[string]string{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range inputs.nodes {
		n := &inputs.nodes[i]
		if !n.present(moveCoordLocal) || !n.present(moveCoordRemote) {
			// Not composed: present on one side only, a delete or a
			// replacement SQLite plans together with its create.
			continue
		}
		for _, side := range []moveCoord{moveCoordLocal, moveCoordRemote} {
			if !movedOn[side][n.key] {
				heldBy[side][n.paths[side]] = n.key
			}
		}
	}

	return heldBy
}

// heldName is a path an item holds and that item's key.
type heldName struct {
	key  string
	path string
}

// foldedHeldPaths indexes held (pathsHeldByUnmovedItems, one side) by
// OneDrive's comparison of paths, keeping the path each item holds as
// spelled.
func foldedHeldPaths(held map[string]string) map[namekey.PathKey]heldName {
	folded := make(map[namekey.PathKey]heldName, len(held))
	for p, key := range held {
		folded[namekey.OfPath(p)] = heldName{key: key, path: p}
	}

	return folded
}

// createsOntoAHeldPath reports a create on side onto a path another composed
// item still holds there (held, pathsHeldByUnmovedItems), and whether the two
// are spelled alike (exact). On this computer a name the item holds under
// OneDrive's name comparison counts too (createsOntoAHeldName).
func createsOntoAHeldPath(
	logger *slog.Logger, action *syncdomain.Action, held map[string]string, side moveCoord, heldLocally map[namekey.PathKey]heldName,
) (exact, onto bool) {
	if holder, isHeld := held[action.Path]; isHeld && holder != action.ItemID {
		return true, true
	}
	if side != moveCoordLocal {
		return false, false
	}

	return false, createsOntoAHeldName(logger, action, heldLocally)
}

// createsOntoAHeldName reports a create here at a name another item that
// stays put here holds under OneDrive's name comparison (foldedHeldPaths),
// logging it at Debug.
func createsOntoAHeldName(logger *slog.Logger, action *syncdomain.Action, heldLocally map[namekey.PathKey]heldName) bool {
	holder, held := heldLocally[namekey.OfPath(action.Path)]
	if !held || holder.key == action.ItemID {
		return false
	}
	if logger != nil {
		logger.Debug("create waits for an item here to leave its name",
			slog.String("action", action.Type.String()),
			slog.String("path", action.Path),
			slog.String("held_by", holder.path))
	}

	return true
}

// pathsVacatedByPlan reports, per side, the paths an action of the plan frees.
func pathsVacatedByPlan(actions []syncdomain.Action) map[moveCoord]map[string]bool {
	vacated := map[moveCoord]map[string]bool{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range actions {
		switch actions[i].Type {
		case syncdomain.ActionLocalMove:
			vacated[moveCoordLocal][actions[i].OldPath] = true
		case syncdomain.ActionConflictCopy, syncdomain.ActionLocalDelete:
			vacated[moveCoordLocal][actions[i].Path] = true
		case syncdomain.ActionRemoteMove:
			vacated[moveCoordRemote][actions[i].OldPath] = true
		case syncdomain.ActionRemoteDelete:
			vacated[moveCoordRemote][actions[i].Path] = true
		case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionFolderCreate, syncdomain.ActionBaselineUpdate,
			syncdomain.ActionCleanup:
		default:
			panic(syncdomain.OutsideTheEnum(actions[i].Type))
		}
	}

	return vacated
}

// rebasedCreateBlocked reports a create that a rebase moved onto a path the
// snapshot already holds on that side (and that nothing in the plan vacates),
// or below a remote file. Its SQLite row reconciled other coordinates: an item
// that only followed a folder moved on the other side, a new local folder in
// place of a file OneDrive kept, ... Run, it failed on every pass and held the
// folder move behind it; the next pass, with the folder moved, reconciles the
// path as one.
func rebasedCreateBlocked(p string, side moveCoord, occupancy *moveOccupancy, vacated map[string]bool) bool {
	occupied := occupancy.local
	if side == moveCoordRemote {
		occupied = occupancy.remote
		for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if occupancy.remoteFiles[dir] {
				return true
			}
		}
	}

	return occupied[p] && !vacated[p]
}

// remoteMoveBoundaries maps the subtree of each OneDrive move of the plan
// from its destination back to its source, longest destination first.
func remoteMoveBoundaries(actions []syncdomain.Action) []graphFolderMoveBoundary {
	var moves []graphFolderMoveBoundary
	for i := range actions {
		if actions[i].Type != syncdomain.ActionRemoteMove || actions[i].OldPath == "" {
			continue
		}
		moves = append(moves, graphFolderMoveBoundary{oldPrefix: actions[i].Path + "/", newPrefix: actions[i].OldPath + "/"})
	}
	sort.Slice(moves, func(i, j int) bool { return len(moves[i].oldPrefix) > len(moves[j].oldPrefix) })

	return moves
}

// carriedOntoByRemoteMove reports the snapshot path of another OneDrive item
// that a folder move of the plan carries to p, the path of a new item's create
// there (remoteMoves, remoteMoveBoundaries), unless the plan moves or deletes
// that item away first. SQLite reconciles the create where the entry sits
// here and the carried item where it sat on OneDrive, two coordinates of one
// path: a folder renamed here with a file inside it replaced by a folder,
// while OneDrive edited (or made anew) that file. The rename lands OneDrive's
// file at the new folder's path, so the create met it and failed: OneDrive
// refuses a create onto a taken name (R-5.6.14), and the uploads below the
// refused folder found no parent. The planner model, which let the folder
// create through, recorded the file's item as the new folder and lost
// OneDrive's edit (planner model seeds 502 and 793). The next pass, with the
// folder moved, reconciles the path as one (a keep-both conflict, R-2.17.10).
func carriedOntoByRemoteMove(
	p string, remoteMoves []graphFolderMoveBoundary, occupancy *moveOccupancy, vacated map[string]bool,
) (string, bool) {
	source := rebaseThroughFolderMoves(p, remoteMoves)
	if source == p || !occupancy.remote[source] || vacated[source] {
		return "", false
	}

	return source, true
}

// logCreateOntoCarriedItem logs, at Debug, a create on OneDrive that waits
// because a folder move of the plan carries another item to its path
// (carriedOntoByRemoteMove).
func logCreateOntoCarriedItem(logger *slog.Logger, action *syncdomain.Action, carried string) {
	if logger == nil {
		return
	}
	logger.Debug("create waits for an item a folder move carries to its path",
		slog.String("action", action.Type.String()),
		slog.String("path", action.Path),
		slog.String("carried_from", carried))
}
