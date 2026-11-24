package syncplan

import (
	"log/slog"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// withholdDeletesOfHeldWork applies one rule on each side: a delete waits
// while work held for a later pass has its source at or below the deleted
// path there. Held work is an item's move that composition keeps for a later
// pass, or whose composed move the keep-both trim removed (keysStillToMove),
// what another step left for the next pass (held: a create that waits with
// such a move, work below a keep-both path, a create onto a path a composed
// item holds, an action caught in a dependency cycle), and what this rule
// withholds itself. The source of a move is the item's path on its side; the
// source of a create is the synced item whose content it writes at another
// path on that side (carriesContentOf): the file here that an upload carries
// when SQLite could not pair its rename, or the item OneDrive keeps that a
// download fetches. The delete is removed with everything that waits for it
// (buildDependencies), held work in turn, until no delete waits. Run, such a
// delete removed what the held work still needs: a folder delete on OneDrive
// took a held item into the recycle bin, a folder delete here failed on it,
// and the delete of a renamed file's old name left OneDrive without its
// content while its upload waited. The delete is released by the pass that
// plans the held work: nothing holds it then, and it waits for that work
// through the ordinary dependencies, a folder delete for the moves out of it
// (addMoveOutOfDeletedFolderDeps) and a delete for the create that writes its
// content (addContentCarrierDeps), so a failure of that work skips it too. A
// folder's move above held work is not removed with it
// (withoutWorkTheMoveCarries): the move carries the folder and the work
// along, so a folder renamed here is renamed on OneDrive in the same pass as
// the upload of a file edited inside it, whose old copy SQLite deletes before
// the rename. A delete the mode does not run is left to the mode partition.
// It logs each delete it holds at Debug and reports how many actions it
// removed, and how many of them the mode runs.
func withholdDeletesOfHeldWork(
	logger *slog.Logger, actions, held []syncdomain.Action, inputs *moveInputs, composition *moveComposition, mode syncdomain.SyncMode,
) (kept []syncdomain.Action, withheld, withheldInMode int) {
	sources := pathsAboveHeldMoves(actions, inputs, composition)
	held = slices.Clone(held)
	kept = actions
	for range len(actions) + 1 {
		roots := deletesOfHeldWork(logger, kept, held, sources, mode)
		for i := range deletesOfUncarriedContent(logger, kept, inputs.filesByContent, mode) {
			roots[i] = true
		}
		var carried [][]int
		if len(roots) > 0 || deletesContentACreateWrites(kept) {
			deps, leftOut := buildDependencyGraph(kept)
			carriedTo := carriedPaths(kept, deps, leftOut)
			carried = withoutWorkTheMoveCarries(kept, deps)
			deletesAheadOfTheirContent(logger, kept, leftOut, carried, carriedTo, mode, roots)
		}
		if len(roots) == 0 {
			break
		}
		dropped := withDependents(roots, carried)
		remaining := make([]syncdomain.Action, 0, len(kept)-len(dropped))
		for i := range kept {
			if !dropped[i] {
				remaining = append(remaining, kept[i])
				continue
			}
			held = append(held, kept[i])
			withheld++
			if actionAllowedInMode(&kept[i], mode) {
				withheldInMode++
			}
		}
		kept = remaining
	}

	return kept, withheld, withheldInMode
}

// withholdDeletesInsideFolderMoveCycles holds for the next pass each folder
// delete of a plan with a dependency cycle that closes the cycle through the
// move of a folder above it on its side (deletesInsideFolderMoveCycles), with
// what waits for it apart from such a move (withoutWorkTheMoveCarries). The
// shape: here docs moved to archive, a new docs made, archive/sub/inner.txt
// moved into it and archive/sub removed (or the same on OneDrive). SQLite
// deletes the one-sided sub at its baseline path docs/sub; the delete waits
// for inner.txt to leave it, docs's move waits for the delete below its
// source, and inner.txt's move waits for the new docs, which waits for docs's
// move. Dropped as a cycle, nothing ran on any pass. Held, docs's move
// carries sub with inner.txt in it, the new docs is made, inner.txt's move,
// whose source docs's move took away, is superseded, and the next pass moves
// inner.txt and then deletes the folder at its new path. Holding a delete
// never removes anything. It logs each delete it holds at Debug and returns
// the actions it keeps and those it holds.
func withholdDeletesInsideFolderMoveCycles(
	logger *slog.Logger, actions []syncdomain.Action, mode syncdomain.SyncMode,
) (kept, withheld []syncdomain.Action) {
	deps := buildDependencies(actions)
	onCycle := dependencyCycleMembers(deps)
	if len(onCycle) == 0 {
		return actions, nil
	}
	roots := deletesInsideFolderMoveCycles(actions, deps, onCycle, mode)
	if len(roots) == 0 {
		return actions, nil
	}
	for i := range roots {
		if logger != nil {
			logger.Debug("folder delete waits for the move of the folder above it",
				slog.String("action", actions[i].Type.String()),
				slog.String("path", actions[i].Path))
		}
	}
	dropped := withDependents(roots, withoutWorkTheMoveCarries(actions, deps))
	kept = make([]syncdomain.Action, 0, len(actions)-len(dropped))
	for i := range actions {
		if dropped[i] {
			withheld = append(withheld, actions[i])
			continue
		}
		kept = append(kept, actions[i])
	}

	return kept, withheld
}

// deletesInsideFolderMoveCycles lists the folder deletes on a cycle (onCycle)
// that a move of a folder above them on their side waits for, while they
// wait for a same-side move out of the deleted folder that waits, directly or
// through others, for that folder's move. deps are sorted per action, as
// buildDependencies leaves them.
func deletesInsideFolderMoveCycles(
	actions []syncdomain.Action, deps [][]int, onCycle map[int]bool, mode syncdomain.SyncMode,
) map[int]bool {
	roots := map[int]bool{}
	for d := range onCycle {
		deleted := &actions[d]
		if !isSubtreeDelete(deleted) || !actionAllowedInMode(deleted, mode) {
			continue
		}
		side, _ := deleteSide(deleted)
		if closesAFolderMoveCycle(actions, deps, onCycle, d, side) {
			roots[d] = true
		}
	}

	return roots
}

// closesAFolderMoveCycle reports whether the folder delete d on side is waited
// for by the move of a folder above it on side that a move out of d's folder,
// which d waits for, waits for in turn.
func closesAFolderMoveCycle(actions []syncdomain.Action, deps [][]int, onCycle map[int]bool, d int, side moveCoord) bool {
	deleted := &actions[d]
	for a := range onCycle {
		folderMove := &actions[a]
		if !isMoveAction(folderMove) || moveSide(folderMove.Type) != side ||
			!strings.HasPrefix(deleted.Path, folderMove.OldPath+"/") {
			continue
		}
		if _, waits := slices.BinarySearch(deps[a], d); !waits {
			continue
		}
		for _, m := range deps[d] {
			out := &actions[m]
			if isMoveAction(out) && moveSide(out.Type) == side && strings.HasPrefix(out.OldPath, deleted.Path+"/") &&
				waitsFor(deps, m, a) {
				return true
			}
		}
	}

	return false
}

// deletesOfHeldWork lists the deletes of actions that wait for held work
// (withholdDeletesOfHeldWork), logging each at Debug with the path of the
// work it waits for.
func deletesOfHeldWork(
	logger *slog.Logger, actions, held []syncdomain.Action, sources map[moveCoord]map[string]string, mode syncdomain.SyncMode,
) map[int]bool {
	roots := map[int]bool{}
	for i := range actions {
		action := &actions[i]
		side, deletes := deleteSide(action)
		if !deletes || !actionAllowedInMode(action, mode) {
			continue
		}
		heldPath, waits := sources[side][action.Path]
		if !waits {
			heldPath, waits = heldWorkFrom(action, side, held)
		}
		if !waits {
			continue
		}
		roots[i] = true
		if logger != nil {
			logger.Debug("delete waits for work held for the next pass",
				slog.String("action", action.Type.String()),
				slog.String("path", action.Path),
				slog.String("held_path", heldPath))
		}
	}

	return roots
}

// deletesContentACreateWrites reports whether a create of the plan writes the
// content of a synced file a delete of the plan removes, the two ends of a
// content edge (addContentCarrierDeps), without building the dependencies.
func deletesContentACreateWrites(actions []syncdomain.Action) bool {
	removed := map[contentKey]bool{}
	for i := range actions {
		if key, removes := deletedContentKey(&actions[i]); removes {
			removed[key] = true
		}
	}
	if len(removed) == 0 {
		return false
	}
	for i := range actions {
		if key, writes := carriedContentKey(&actions[i]); writes && removed[key] {
			return true
		}
	}

	return false
}

// deletesAheadOfTheirContent adds to roots each delete whose content a create
// of the plan writes at another path, where addContentCarrierDeps left that
// edge out because the create waits for the delete, when it waits only
// through the move of a folder above the deleted file (it does not reach the
// delete in carried, the dependencies without the work such moves carry) and
// lands elsewhere than where the plan's moves carry the file (carriedTo, by
// delete). Held for the next pass, the delete lets the moves carry the file
// along, the create runs after them, and the next pass deletes the file at
// its new path, so the content stays on that side when the create or what it
// waits for fails: run first, the delete left the side without it. A create
// at the path the moves carry the file to needs the file gone first, and one
// that waits for the delete otherwise, as the upload of a file moved up onto
// its folder's path waits for that folder's delete, would be held with it. It
// logs each at Debug.
func deletesAheadOfTheirContent(
	logger *slog.Logger, actions []syncdomain.Action, leftOut []contentEdge, carried [][]int, carriedTo map[int]string,
	mode syncdomain.SyncMode, roots map[int]bool,
) {
	for _, edge := range leftOut {
		deleted, create := &actions[edge.deleteIdx], &actions[edge.createIdx]
		if roots[edge.deleteIdx] || !actionAllowedInMode(deleted, mode) ||
			namekey.OfPath(carriedTo[edge.deleteIdx]) == namekey.OfPath(create.Path) ||
			waitsFor(carried, edge.createIdx, edge.deleteIdx) {
			continue
		}
		roots[edge.deleteIdx] = true
		if logger != nil {
			logger.Debug("delete waits for work held for the next pass",
				slog.String("action", deleted.Type.String()),
				slog.String("path", deleted.Path),
				slog.String("held_path", create.Path))
		}
	}
}

// carriedPaths maps the delete of each left-out content edge to where the
// plan's moves carry the deleted entry (carriedPath), finding the moves by
// their sources (movesBySource), so each costs the depth of the paths it
// walks, not the plan's size. deps are the plan's dependencies as built,
// before withoutWorkTheMoveCarries takes out a folder move's waits for the
// moves of subfolders below its source.
func carriedPaths(actions []syncdomain.Action, deps [][]int, leftOut []contentEdge) map[int]string {
	if len(leftOut) == 0 {
		return nil
	}
	bySource := movesBySource(actions)
	carriedTo := make(map[int]string, len(leftOut))
	for _, edge := range leftOut {
		if _, known := carriedTo[edge.deleteIdx]; !known {
			carriedTo[edge.deleteIdx] = carriedPath(actions, deps, bySource, &actions[edge.deleteIdx])
		}
	}

	return carriedTo
}

// movesBySource lists the moves of actions per side by their source path.
func movesBySource(actions []syncdomain.Action) map[moveCoord]map[string][]int {
	bySource := map[moveCoord]map[string][]int{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range actions {
		if move := &actions[i]; isMoveAction(move) {
			side := moveSide(move.Type)
			bySource[side][move.OldPath] = append(bySource[side][move.OldPath], i)
		}
	}

	return bySource
}

// carriedPath is where the plan's moves on deleted's side carry the deleted
// entry: the move of the nearest folder above it, then the move of the
// nearest folder above where that move puts it that waits for that move, and
// so on, or its own path when no move carries it. Each move's source is a
// path as the side has it when the move runs: on OneDrive a folder's move
// carries a subfolder whose own move, rebased below the folder's new path,
// runs after it, and here a subfolder moves first, from below the folder's
// old path, so the folder's move waits for it. A move above where an earlier
// one puts the entry that does not wait for it ran first, as the move of a
// folder whose path that earlier move takes, and carries the entry nowhere.
func carriedPath(actions []syncdomain.Action, deps [][]int, bySource map[moveCoord]map[string][]int, deleted *syncdomain.Action) string {
	side := contentRemovalSide(deleted)
	p, last := deleted.Path, -1
	applied := map[int]bool{}
	for {
		next := nearestMoveAbove(p, bySource[side], deps, last)
		if next < 0 || applied[next] {
			return p
		}
		applied[next] = true
		p = actions[next].Path + strings.TrimPrefix(p, actions[next].OldPath)
		last = next
	}
}

// nearestMoveAbove is the move whose source is the nearest folder above p
// that waits for the move after (any such move while after is -1), or -1.
func nearestMoveAbove(p string, bySource map[string][]int, deps [][]int, after int) int {
	var above []int
	for dir := range pathAncestors(p) {
		above = append(above, bySource[dir]...)
	}
	for i := len(above) - 1; i >= 0; i-- {
		if after < 0 {
			return above[i]
		}
		if _, waits := slices.BinarySearch(deps[above[i]], after); waits {
			return above[i]
		}
	}

	return -1
}

// heldWorkFrom reports the path of a held action whose source on side lies at
// or below the path of the delete deleted: a move out of there, or a create of
// content the deleted item holds.
func heldWorkFrom(deleted *syncdomain.Action, side moveCoord, held []syncdomain.Action) (string, bool) {
	for i := range held {
		work := &held[i]
		if isMoveAction(work) && moveSide(work.Type) == side && pathAtOrBelow(work.OldPath, deleted.Path) {
			return work.OldPath, true
		}
		if createsOnSide(work, side) && carriesContentOf(work, deleted) {
			return work.Path, true
		}
	}

	return "", false
}

// pathAtOrBelow reports whether p is dir or lies below it.
func pathAtOrBelow(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// withoutWorkTheMoveCarries removes from deps each edge by which a move waits
// for work strictly below its source on its side that does not follow it to
// its destination (the second rule of moveOrderDependency), so a held action
// below a folder's move does not hold the move. Run without it, the work is
// carried along and planned at its new path by the next pass. An edge that
// also orders the work first for another reason stays: a move landing at or
// below the destination of a move it waits for is held with that move, as it
// would land in what OneDrive still keeps at that path.
func withoutWorkTheMoveCarries(actions []syncdomain.Action, deps [][]int) [][]int {
	for v := range deps {
		move := &actions[v]
		if !isMoveAction(move) {
			continue
		}
		deps[v] = slices.DeleteFunc(deps[v], func(w int) bool {
			work := &actions[w]
			return underMoveSource(work, move) && !underMoveDestination(work, move) &&
				(!isMoveAction(work) || !movePrecedes(work, move))
		})
	}

	return deps
}

// pathsAboveHeldMoves maps, per side, the source there of each held move, the
// path of an item still to be moved on that side (keysStillToMove), and every
// folder above it, to the item's path, the first in path order where several
// share a folder: a delete of any of these paths has the source at or below
// it. A folder delete also waits for the deletes below it, so the folders
// above the parent hold by themselves only the delete of an ancestor whose
// folders between are not deleted, such as one that a folder between leaves
// with the item: that delete waits one pass, and the next plans it after the
// move.
func pathsAboveHeldMoves(actions []syncdomain.Action, inputs *moveInputs, composition *moveComposition) map[moveCoord]map[string]string {
	held := keysStillToMove(actions, composition)
	above := map[moveCoord]map[string]string{moveCoordLocal: {}, moveCoordRemote: {}}
	if len(held[moveCoordLocal]) == 0 && len(held[moveCoordRemote]) == 0 {
		return above
	}
	mark := func(side moveCoord, folder, itemPath string) {
		if first, marked := above[side][folder]; !marked || itemPath < first {
			above[side][folder] = itemPath
		}
	}
	for i := range inputs.nodes {
		n := &inputs.nodes[i]
		for _, side := range []moveCoord{moveCoordLocal, moveCoordRemote} {
			if !held[side][n.key] || !n.present(side) {
				continue
			}
			mark(side, n.paths[side], n.paths[side])
			for folder := range pathAncestors(n.paths[side]) {
				mark(side, folder, n.paths[side])
			}
		}
	}

	return above
}

// keysStillToMove lists, per side, the items still to be moved there after
// this plan: an item whose move there composition holds for the next pass
// (deferredMoves), and one whose composed move the plan no longer holds, as
// when the keep-both trim removes a move below a keep-both path.
func keysStillToMove(actions []syncdomain.Action, composition *moveComposition) map[moveCoord]map[string]bool {
	held := map[moveCoord]map[string]bool{moveCoordLocal: {}, moveCoordRemote: {}}
	for key, moves := range composition.deferredMoves {
		for _, moveType := range moves {
			held[moveSide(moveType)][key] = true
		}
	}
	if len(composition.moves) == 0 {
		return held
	}

	planned := map[moveCoord]map[string]bool{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range actions {
		if isMoveAction(&actions[i]) {
			planned[moveSide(actions[i].Type)][actions[i].ItemID] = true
		}
	}
	for i := range composition.moves {
		if move := &composition.moves[i]; !planned[move.side][move.key] {
			held[move.side][move.key] = true
		}
	}

	return held
}

// moveSide is the side a move of moveType, ActionLocalMove or
// ActionRemoteMove, moves an item on.
func moveSide(moveType syncdomain.ActionType) moveCoord {
	if moveType == syncdomain.ActionRemoteMove {
		return moveCoordRemote
	}

	return moveCoordLocal
}

// createSide reports which side an action creates an entry on.
func createSide(action *syncdomain.Action) (moveCoord, bool) {
	switch action.Type {
	case syncdomain.ActionDownload:
		return moveCoordLocal, true
	case syncdomain.ActionUpload:
		return moveCoordRemote, true
	case syncdomain.ActionFolderCreate:
		switch action.CreateSide {
		case syncdomain.CreateLocal:
			return moveCoordLocal, true
		case syncdomain.CreateRemote:
			return moveCoordRemote, true
		default:
			panic(syncdomain.OutsideTheEnum(action.CreateSide))
		}
	case syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete,
		syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return moveCoordBaseline, false
	default:
		panic(syncdomain.OutsideTheEnum(action.Type))
	}
}

// deleteSide reports which side a delete removes an entry on. A cleanup
// removes only a baseline row.
func deleteSide(action *syncdomain.Action) (moveCoord, bool) {
	switch action.Type {
	case syncdomain.ActionLocalDelete:
		return moveCoordLocal, true
	case syncdomain.ActionRemoteDelete:
		return moveCoordRemote, true
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove,
		syncdomain.ActionFolderCreate,
		syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return moveCoordBaseline, false
	default:
		panic(syncdomain.OutsideTheEnum(action.Type))
	}
}

// contentRemovalSide is the side a removal of synced content
// (deletedContentKey) acts on: a delete's, or here for a download that
// replaces the synced file at its path with another item.
func contentRemovalSide(action *syncdomain.Action) moveCoord {
	if side, deletes := deleteSide(action); deletes {
		return side
	}

	return moveCoordLocal
}

// createsOnSide reports whether an action creates an entry on side: a download
// or a local folder create locally, an upload or a remote folder create on
// OneDrive.
func createsOnSide(action *syncdomain.Action, side moveCoord) bool {
	created, isCreate := createSide(action)

	return isCreate && created == side
}
