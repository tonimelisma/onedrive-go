package syncplan

import (
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// MoveSwapInfix names the temporary path a parked item waits at.
const MoveSwapInfix = ".swap-"

// moveNameMaxBytes is the longest name a local filesystem stores (NAME_MAX).
// A parking name cuts its stem to fit the infix: a swap of two names at the
// limit otherwise parked at a name the rename refused.
const moveNameMaxBytes = 255

// truncateNameBytes cuts s to at most limit bytes on a character boundary.
func truncateNameBytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}

// parkOneOccupancyCycle breaks one cycle of moves that each wait for the
// next to vacate its destination: a swap of two names (a→b, b→a) or a longer
// rotation. Every such move waits for a free destination, so none was ever
// emitted and the items never converged. The first item of the cycle moves to
// a free temporary name beside its source, as a user swapping two names would;
// that vacates its path for the rest of the cycle, which then resolves in this
// plan, and the parked item reaches its target on the next pass. An item that
// holds the path of a folder a local move of the plan lands in is not parked
// on this computer (holdsALocalLanding): parked, it would leave that path to
// the next item of the cycle, which the move would land in. It reports
// whether it parked an item.
func (e *moveEmitter) parkOneOccupancyCycle(order []string) bool {
	for _, key := range order {
		if e.state[key] != moveEmitPending {
			continue
		}
		side, ok := e.occupancyCycleSide(key)
		if !ok || (side == moveCoordLocal && e.holdsALocalLanding(key)) {
			continue
		}
		if e.park(key, side) {
			return true
		}
	}

	return false
}

// parkOneVacatingLoop breaks one loop of a folder moved into its own former
// child: here docs moved to tmp, a new docs made, tmp/sub moved into it and
// tmp moved to docs/sub/docs2 (or the same on OneDrive). sub's move on the
// other side lands below the path docs's move vacates, so it is decided after
// docs (waitsForVacatingFolder), while docs's target hangs off sub, so docs is
// decided after sub: both stayed pending and were deferred on every pass, as
// no order of plain moves exists (sub's new folder takes docs's path only once
// docs has left it, and docs's destination lies below sub). The vacating
// folder moves to a free name beside its source on that side, as a swap's
// parked item does (park), carrying the child, and waits; the next pass makes
// the folder at the path it left and moves the child into it and then the
// folder below the child. A folder that holds the path of a folder a local
// move of the plan lands in is not parked on this computer
// (holdsALocalLanding), as in parkOneOccupancyCycle. It reports whether it
// parked a folder.
func (e *moveEmitter) parkOneVacatingLoop(order []string) bool {
	for _, key := range order {
		if e.state[key] != moveEmitPending || e.unresolvable[key] || e.parentState(key) != moveEmitSettled {
			continue
		}
		n := e.nodes[key]
		if !e.isMoveParticipant(n) {
			continue
		}
		for _, side := range []moveCoord{moveCoordLocal, moveCoordRemote} {
			if folder, ok := e.vacatingLoopFolder(key, n, side); ok && e.park(folder, side) {
				return true
			}
		}
	}

	return false
}

// vacatingLoopFolder is the folder parkOneVacatingLoop parks for key on side:
// the pending folder whose move vacates the path key lands below, when that
// folder's own target hangs off key.
func (e *moveEmitter) vacatingLoopFolder(key string, n *moveNode, side moveCoord) (string, bool) {
	if !n.present(side) || sameLocation(e.location(n, side), e.target[key]) {
		return "", false
	}
	folder, lands := e.vacatingFolder(n, side, e.targetPath(key))
	if !lands || e.state[folder] != moveEmitPending || e.unresolvable[folder] || !e.targetHangsOff(folder, key) {
		return "", false
	}
	if side == moveCoordLocal && e.holdsALocalLanding(folder) {
		return "", false
	}

	return folder, true
}

// targetHangsOff reports whether key's target position hangs, directly or
// through the items above it, off ancestor (targetParentNode).
func (e *moveEmitter) targetHangsOff(key, ancestor string) bool {
	current := key
	for range len(e.keys) {
		current = e.targetParentNode(current)
		if current == "" {
			return false
		}
		if current == ancestor {
			return true
		}
	}

	return false
}

// moveOneIntoItsWaitingFolder breaks one rotation through nesting made on
// one side: p moved into x, then x moved to p (or x moved into p and p to
// x, or p/f moved into x and x onto p/f). On the other side the entry still
// holds the path the folder's move lands on, so the folder waits for it to
// leave (blockingPendingMove), and the entry, whose target hangs off the
// folder, waits for the folder to be decided: both stayed pending and were
// deferred on every pass. Made on OneDrive, it is replayed here; made here
// (`mv x p/x, mv p x`), on OneDrive. A rotation is never one plain move, so
// on the side that follows the entry moves first into the folder at the path
// the folder has there now, which vacates the folder's destination, and the
// folder's move then carries the entry to its target in the same plan. The
// entry's move is progress whether or not the folder's move runs: it puts
// the entry under its target parent with its target name. It reports
// whether it planned such a move.
func (e *moveEmitter) moveOneIntoItsWaitingFolder(order []string) bool {
	for _, key := range order {
		for _, side := range []moveCoord{moveCoordLocal, moveCoordRemote} {
			if move, ok := e.moveIntoWaitingFolder(key, side); ok {
				e.result.moves = append(e.result.moves, move)
				e.state[key] = moveEmitSettled

				return true
			}
		}
	}

	return false
}

// moveIntoWaitingFolder builds key's move on side into its target folder at
// that folder's current path there, when the folder waits for key alone
// (waitsOnlyForItsFolder). No move of the plan on side may carry key's
// source, the folder or the destination, so the move runs in snapshot
// coordinates.
func (e *moveEmitter) moveIntoWaitingFolder(key string, side moveCoord) (composedMove, bool) {
	folderKey, ok := e.waitsOnlyForItsFolder(key, side)
	if !ok {
		return composedMove{}, false
	}
	n := e.nodes[key]
	source, folderPath := n.paths[side], e.nodes[folderKey].paths[side]
	if pathAtOrBelow(folderPath, source) {
		return composedMove{}, false
	}
	newPath := joinMovePath(folderPath, e.target[key].name)
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if earlier.side == side && (pathAtOrBelow(source, earlier.oldPath) ||
			pathAtOrBelow(folderPath, earlier.oldPath) || pathAtOrBelow(newPath, earlier.newPath)) {
			return composedMove{}, false
		}
	}
	if free, displaced := e.destinationFree(n, side, newPath); !free || displaced != moveDisplaceNever {
		return composedMove{}, false
	}

	move := composedMove{
		key:       key,
		side:      side,
		oldPath:   source,
		newPath:   newPath,
		localPath: n.paths[moveCoordLocal],
	}
	if side == moveCoordLocal {
		move.remotePath = n.paths[moveCoordRemote]
	}

	return move, true
}

// waitsOnlyForItsFolder reports key's target folder when key is pending and
// relocated on side only (the other side keeps it at its target), and that
// folder, a pending folder present on side, waits for key alone: key holds
// the folder's destination there (blockingPendingMove).
func (e *moveEmitter) waitsOnlyForItsFolder(key string, side moveCoord) (string, bool) {
	other := moveCoordRemote
	if side == moveCoordRemote {
		other = moveCoordLocal
	}
	n := e.nodes[key]
	if e.state[key] != moveEmitPending || e.unresolvable[key] || e.takenBack[key] || !e.isMoveParticipant(n) ||
		sameLocation(e.location(n, side), e.target[key]) ||
		!sameLocation(e.location(n, other), e.target[key]) {
		return "", false
	}
	folderKey := e.target[key].parent
	folder, known := e.nodes[folderKey]
	if !known || e.state[folderKey] != moveEmitPending || e.unresolvable[folderKey] ||
		folder.itemType != syncdomain.ItemTypeFolder || !folder.present(side) {
		return "", false
	}
	if blocker, blocked := e.blockingPendingMove(folderKey, side); blocked && blocker == key {
		return folderKey, true
	}

	return folderKey, e.holdsTheLandingOfAFolderAbove(key, folderKey, side)
}

// holdsTheLandingOfAFolderAbove reports whether key holds on side the
// destination of a pending folder that folderKey's target hangs off: the
// folder waits for that folder's decision, which waits for key to leave, so
// key waits for its folder alone through it. Here x moved into p and f out
// of p onto x, while OneDrive moved p into x: the cycle keeps OneDrive's
// nesting, so x goes back to its path, which f holds, and p moves into x,
// while f's own claim on x is undone; f moves first into p where p is here
// now, then x out of p onto its path, then p into it.
func (e *moveEmitter) holdsTheLandingOfAFolderAbove(key, folderKey string, side moveCoord) bool {
	for _, other := range e.keys {
		if other == folderKey || e.state[other] != moveEmitPending || !e.targetHangsOff(folderKey, other) {
			continue
		}
		if blocker, blocked := e.blockingPendingMove(other, side); blocked && blocker == key {
			return true
		}
	}

	return false
}

// parkAwayFromItsOwnLanding decides key, whose move on side lands in a folder
// made on the other side at key's own path on side (folderHolder): the folder
// takes that path only once key has left it, and key's move lands in the
// folder, so neither ever ran, and the item waited, or the plan dropped both
// as a dependency cycle, on every pass. key moves to a free name beside its
// source, as a swap's parked item does (park), and the next pass makes the
// folder at the path it left and moves key into it. A move of the plan that
// carries key's source, or no free name, leaves key waiting for a later pass.
// Either way key is deferred, so nothing is made at or below the folder's
// path this pass (holdOwnLandings). It reports that key left the pending
// state.
func (e *moveEmitter) parkAwayFromItsOwnLanding(key string, side moveCoord) bool {
	source := e.nodes[key].paths[side]
	for i := range e.result.moves {
		if earlier := &e.result.moves[i]; earlier.side == side && strings.HasPrefix(source, earlier.oldPath+"/") {
			e.deferNode(key)
			return true
		}
	}
	if !e.park(key, side) {
		e.deferNode(key)
	}

	return true
}

// holdOwnLandings records in ownLandings, for each item the plan defers whose
// move on a side lands in a new folder made on the other side at the item's
// own path there (landingFolderHolder), that folder's path as the move's
// target path names it. A later pass makes the folder, once the item has left
// the path. Made this pass, the folder would collide with the item, and what
// is made below it would go into the item: nothing orders such a create
// after the item's move aside (park), which may fail, nor after a move of a
// folder above that carries the item to that path. On both sides the
// folder's path is looked up back through the plan's earlier moves on that
// side (snapshotPath): with Y renamed Z on OneDrive and Z/f moved there into
// a new Z/f/a/f, the old f is found at Y/f here, where the create of Z/f/a,
// rebased to where Y sits here, would land inside it before Y moves.
func (e *moveEmitter) holdOwnLandings() {
	for key := range e.result.deferred {
		n := e.nodes[key]
		targetPath := e.targetPath(key)
		for _, side := range []moveCoord{moveCoordLocal, moveCoordRemote} {
			if !n.present(side) || sameLocation(e.location(n, side), e.target[key]) {
				continue
			}
			holder, folder, held := e.landingFolderHolder(side, key, targetPath)
			if !held || holder != key {
				continue
			}
			if e.result.ownLandings == nil {
				e.result.ownLandings = map[moveCoord]map[namekey.PathKey]bool{}
			}
			if e.result.ownLandings[side] == nil {
				e.result.ownLandings[side] = map[namekey.PathKey]bool{}
			}
			e.result.ownLandings[side][namekey.OfPath(folder)] = true
		}
	}
}

// park moves key on side to a free name beside its source (freeParkingPath)
// and defers it, so it reaches its target the next pass. It reports whether a
// free name was found.
func (e *moveEmitter) park(key string, side moveCoord) bool {
	n := e.nodes[key]
	parking := e.freeParkingPath(n.paths[side])
	if parking == "" {
		return false
	}
	move := composedMove{
		key:       key,
		side:      side,
		oldPath:   n.paths[side],
		newPath:   parking,
		localPath: n.paths[moveCoordLocal],
	}
	if side == moveCoordLocal {
		move.remotePath = n.paths[moveCoordRemote]
	}
	e.result.moves = append(e.result.moves, move)
	e.deferNode(key)

	return true
}

// occupancyCycleSide reports the side on which key's move waits on a cycle of
// pending moves, each blocked by the next one's source.
func (e *moveEmitter) occupancyCycleSide(key string) (moveCoord, bool) {
	if e.parentState(key) != moveEmitSettled || e.unresolvable[key] {
		return 0, false
	}
	for _, side := range []moveCoord{moveCoordLocal, moveCoordRemote} {
		seen := map[string]bool{}
		for current := key; !seen[current]; {
			seen[current] = true
			next, blocked := e.blockingPendingMove(current, side)
			if !blocked {
				break
			}
			if next == key {
				return side, true
			}
			current = next
		}
	}

	return 0, false
}

// blockingPendingMove reports the item that holds, in side's snapshot, the
// destination of key's move on side, while key is pending and not yet at its
// target there. It names any item there but key, whatever the occupant's
// state and whether it moves at all: occupancyCycleSide follows these names
// and stops at an occupant that is no longer pending or needs no move on
// side, and an entry whose occupant never leaves stays pending until the
// final sweep of emitRun defers it.
func (e *moveEmitter) blockingPendingMove(key string, side moveCoord) (string, bool) {
	n := e.nodes[key]
	if n == nil || e.state[key] != moveEmitPending || !n.present(side) ||
		sameLocation(e.location(n, side), e.target[key]) {
		return "", false
	}
	occupant, isNode := e.occupantAt(side, e.snapshotPath(side, e.targetPath(key)))
	if !isNode || occupant == key {
		return "", false
	}

	return occupant, true
}

// freeParkingPath is a sibling of p that no side holds and no move of this
// plan targets, compared as OneDrive compares names.
func (e *moveEmitter) freeParkingPath(p string) string {
	dir, name := path.Dir(p), path.Base(p)
	if dir == "." {
		dir = ""
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	taken := e.takenPathsFolded()
	for i := 1; i <= len(e.keys)+1; i++ {
		suffix := MoveSwapInfix + strconv.Itoa(i) + ext
		candidate := joinMovePath(dir, truncateNameBytes(stem, moveNameMaxBytes-len(suffix))+suffix)
		if !taken[namekey.OfPath(candidate)] {
			return candidate
		}
	}

	return ""
}

func (e *moveEmitter) takenPathsFolded() map[namekey.PathKey]bool {
	taken := make(map[namekey.PathKey]bool, len(e.occupancy.local)+len(e.occupancy.remote))
	for _, occupied := range []map[string]bool{e.occupancy.local, e.occupancy.remote} {
		for p := range occupied {
			taken[namekey.OfPath(p)] = true
		}
	}
	for coord := range moveCoordCount {
		for p := range e.byPath[coord] {
			taken[namekey.OfPath(p)] = true
		}
	}
	for i := range e.result.moves {
		taken[namekey.OfPath(e.result.moves[i].newPath)] = true
	}

	return taken
}
