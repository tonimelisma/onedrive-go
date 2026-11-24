package syncplan

import (
	"path"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// recordLanding records, for a planned OneDrive move of key into a folder made
// here, the item that holds the folder's path (landings).
func (e *moveEmitter) recordLanding(key string, side moveCoord, newPath string) {
	if side != moveCoordRemote {
		return
	}
	if holder, held := e.folderHolder(side, key, newPath); held {
		e.landings[key] = holder
	}
}

// folderHolder reports the synced item that holds, in side's snapshot, the
// path of the folder a move of key to targetPath lands in, when that folder is
// not a synced item: a folder made on the other side, alone or below other
// folders made there. Those folders are walked up from the deepest to the
// first path the snapshot gives a synced item, which a move into the deepest
// would otherwise land in or be carried by. Paths are compared as OneDrive
// compares names (occupantAt): a case-insensitive volume resolves a new folder
// X to the item at x. On each side a path is looked up where the moves the
// plan makes before this one on that side leave it (snapshotPath): a local
// move that runs inside a folder before the folder moves is given the path
// where it lands by planMove, which no earlier move maps, and one that runs
// after a folder above it moved finds what that folder carried. An item
// present on one side only is a create or a delete SQLite plans and holds
// nothing.
func (e *moveEmitter) folderHolder(side moveCoord, key, targetPath string) (string, bool) {
	holder, _, held := e.landingFolderHolder(side, key, targetPath)

	return holder, held
}

// landingFolderHolder is folderHolder, and it also returns the path of the new
// folder whose path the holder has, as the plan's creates on side address it:
// on OneDrive the move's target path, as creates there are rebased through
// the local moves of the plan; on this computer the snapshot path, as writes
// here are rebased to where their folders sit here now
// (rebaseLocalWritesToLocalFolderPositions).
func (e *moveEmitter) landingFolderHolder(side moveCoord, key, targetPath string) (holder, folder string, held bool) {
	parent := e.target[key].parent
	if !strings.HasPrefix(parent, unsyncedParentPrefix) {
		return "", "", false
	}
	_, made := e.unsyncedAnchor(parent)
	dir := path.Dir(targetPath)
	for range strings.Count(made, "/") + 1 {
		at := e.snapshotPath(side, dir)
		if occupant, isNode := e.occupantAt(side, at); isNode {
			folder = dir
			if side == moveCoordLocal {
				folder = at
			}
			return occupant, folder, e.isMoveParticipant(e.nodes[occupant])
		}
		dir = path.Dir(dir)
	}

	return "", "", false
}

// localFolderHolder reports the item that holds, on this computer, the path of
// the folder a local move of key to targetPath lands in (folderHolder), when
// that folder is one OneDrive made and the item is relocated on this computer.
// That item leaves the path, and the folder the move needs is made there only
// by a later pass: SQLite plans no create at a path the baseline still gives
// an item OneDrive moved away. Planned with the item's own move, the move
// would land in the item before it leaves, which carries it along, or find no
// folder after it left.
func (e *moveEmitter) localFolderHolder(key, targetPath string) (string, bool) {
	holder, held := e.folderHolder(moveCoordLocal, key, targetPath)
	if !held || sameLocation(e.location(e.nodes[holder], moveCoordLocal), e.target[holder]) {
		return "", false
	}

	return holder, true
}

// waitsForAFolderHolder reports whether key's move on side to targetPath
// waits for the next pass because the folder it lands in was made on the other
// side at a path a synced item holds on side (folderHolder): on this computer,
// the move lands where a holder is that it cannot land in, or it would vacate
// the path a planned move lands in; on OneDrive, the holder stays at the path
// this pass (remoteLandingWaits), and key is recorded in remoteHeld.
func (e *moveEmitter) waitsForAFolderHolder(key string, side moveCoord, targetPath string) bool {
	if side == moveCoordRemote {
		if !e.remoteLandingWaits(key, targetPath) {
			return false
		}
		e.remoteHeld[key] = true

		return true
	}

	return e.cannotLandInTheHolder(key, targetPath) || e.holdsALocalLanding(key)
}

// remoteLandingWaits reports whether key's OneDrive move to targetPath waits
// for the next pass because the folder it lands in, made here, cannot exist on
// OneDrive when the move runs: OneDrive gives the path to a synced item
// (folderHolder) that stays there this pass. The plan creates such a folder on
// OneDrive only after the item's own move of the plan has left the path
// (recreatedSourceCreates, buildDependencies), so the move waits while the
// item is at its target there, and while it is a file or a frozen folder
// still to be decided, a file even when it then moves. Run anyway, the move
// would land in the item and stay in it for as long as the item does. A
// folder still to be decided, and an item decided without an OneDrive move
// (held for the next pass, or parked on this computer), hold the move
// through the take-back: the move is planned, and when the run ends without
// an OneDrive move of the item, it is taken back, unless the item's own
// target lies below the move and it moves aside on OneDrive first instead
// (takeBackHeldLandings). An item the plan moves away
// on OneDrive holds it only when that move, directly or through the moves it
// waits for, lands in a folder made at key's own OneDrive path
// (remoteLandingLoops): two folders each moved here into a new folder at the
// other's path would wait for each other, so the one decided second waits for
// the next pass (loopHeld), and the move of the first lands in it. A file
// still to be decided is recorded the same way, as the first, so that when
// it moves into a folder made at key's path, its move lands in key, which
// carries it along the next pass. Only a folder key carries the first along
// (isLoopPartner): when key is a file, the move of the first, which would
// land in that file on OneDrive, is taken back, and both wait.
func (e *moveEmitter) remoteLandingWaits(key, targetPath string) bool {
	holder, held := e.folderHolder(moveCoordRemote, key, targetPath)
	if !held {
		return false
	}
	h := e.nodes[holder]
	switch {
	case sameLocation(e.location(h, moveCoordRemote), e.target[holder]):
		return true
	case e.state[holder] == moveEmitPending:
		if h.itemType == syncdomain.ItemTypeFile {
			e.loopHeld[key] = holder
		}

		return h.itemType != syncdomain.ItemTypeFolder || e.unresolvable[holder]
	case e.remoteLandingLoops(holder, key):
		e.loopHeld[key] = holder
		return true
	default:
		return false
	}
}

// remoteLandingLoops reports whether holder's planned OneDrive move lands,
// directly or through the moves it waits for, in a folder made here at key's
// OneDrive path (landings): planned with them, key's move would wait for that
// chain and the chain for key's, a cycle buildDependencies cannot order.
func (e *moveEmitter) remoteLandingLoops(holder, key string) bool {
	seen := map[string]bool{}
	for current := holder; !seen[current]; {
		seen[current] = true
		next, lands := e.landings[current]
		if !lands {
			return false
		}
		if next == key {
			return true
		}
		current = next
	}

	return false
}

// movedOnOneDrive reports whether the plan moves key on OneDrive: settled
// with an OneDrive move, or parked there (parkedFirst), which frees its path
// as well.
func (e *moveEmitter) movedOnOneDrive(key string) bool {
	if e.state[key] != moveEmitSettled && !e.parkedFirst[key] {
		return false
	}
	for i := range e.result.moves {
		if move := &e.result.moves[i]; move.key == key && move.side == moveCoordRemote {
			return true
		}
	}

	return false
}

// cannotLandInTheHolder reports whether key's local move to targetPath lands
// in a folder whose path an item holds (localFolderHolder) that the move
// cannot land in, so the move waits for the next pass: an item already
// decided (moved away by this plan, or held for the next pass, it leaves the
// path before the folder is made), a file, or a frozen folder, below which
// nothing moves this pass. A pending folder that is not frozen waits instead
// (holdsALocalLanding): the move lands in it and the folder moves a pass
// later, carrying it. Whichever composition decides first moves, so two
// folders that each land where the other leaves converge.
func (e *moveEmitter) cannotLandInTheHolder(key, targetPath string) bool {
	holder, held := e.localFolderHolder(key, targetPath)

	return held && (e.state[holder] != moveEmitPending || e.nodes[holder].itemType != syncdomain.ItemTypeFolder || e.unresolvable[holder])
}

// holdsALocalLanding reports whether a local move this plan makes lands in a
// folder at key's local path (localFolderHolder): key's own local move waits
// for the next pass, which sees that move made.
func (e *moveEmitter) holdsALocalLanding(key string) bool {
	for i := range e.result.moves {
		move := &e.result.moves[i]
		if move.side != moveCoordLocal || e.state[move.key] != moveEmitSettled {
			continue
		}
		if holder, held := e.localFolderHolder(move.key, move.newPath); held && holder == key {
			return true
		}
	}

	return false
}

// vacatingFolder finds the folder whose move on side vacates the path n's
// target lies at or below: the nearest folder above n on side that is
// relocated there, when newPath is that folder's path on side or lies below
// it. A directory that is not a synced item moves with the folder above it on
// side, so the search passes over it; a synced folder present on one side
// only (one SQLite deletes or re-creates) ends it with none. With none, n's
// move keeps its source, and buildDependencies orders it after a folder move
// whose vacated path the plan creates a folder at (movesIntoRecreatedSource).
// When the plan deletes the one-sided folder instead, that delete waits for
// the move and the folder move for the delete, a cycle the delete waits a
// pass to break (withholdDeletesInsideFolderMoveCycles). A nearer relocated
// folder n does not land in is returned with lands false; when n lands below
// a farther one, n waits for the next pass (landsPastARelocatedFolder).
func (e *moveEmitter) vacatingFolder(n *moveNode, side moveCoord, newPath string) (string, bool) {
	for dir := path.Dir(n.paths[side]); dir != "." && dir != "/"; dir = path.Dir(dir) {
		key, isNode := e.byPath[side][dir]
		if !isNode {
			continue
		}
		if !e.isMoveParticipant(e.nodes[key]) {
			return "", false
		}
		if sameLocation(e.location(e.nodes[key], side), e.target[key]) {
			continue
		}

		return key, newPath == dir || strings.HasPrefix(newPath, dir+"/")
	}

	return "", false
}

// landsPastARelocatedFolder reports whether n's target lies at or below the
// path a folder's move on side vacates, past a nearer folder above n that is
// relocated on side and that the target does not land below: the search of
// vacatingFolder walked on past that nearer folder. n's move then waits for
// the next pass, which finds it where both folders' moves carried it. Planned
// now, from a source below both moving folders, it closed a cycle on every
// pass wherever nothing else ordered it: onto the vacated path itself or into
// a folder moved onto it (`docs/sub` renamed `docs/sub2`, `docs` moved to
// `archive`, and `archive/sub2/inner.txt` moved onto `docs` or into `photos`
// moved there), the folder move waited for n to leave first and n for the
// folder move, directly or through the folder moved onto the path; and where
// the nearer folder moves out of the moving one (`archive/sub` moved to
// `photos/sub` and `photos/sub/inner.txt` into a new `docs`), that folder's
// move waited for n to leave it, n for the new folder, the new folder for
// the folder move and the folder move for the subfolder. The walk stops, as
// vacatingFolder's does, at a synced folder present on one side only.
func (e *moveEmitter) landsPastARelocatedFolder(n *moveNode, side moveCoord, newPath string) bool {
	passedARelocatedFolder := false
	for dir := path.Dir(n.paths[side]); dir != "." && dir != "/"; dir = path.Dir(dir) {
		key, isNode := e.byPath[side][dir]
		if !isNode {
			continue
		}
		if !e.isMoveParticipant(e.nodes[key]) {
			return false
		}
		if sameLocation(e.location(e.nodes[key], side), e.target[key]) {
			continue
		}
		if pathAtOrBelow(newPath, dir) {
			return passedARelocatedFolder
		}
		passedARelocatedFolder = true
	}

	return false
}

// waitsForVacatingFolder reports whether n's move on side lands where a
// folder's move vacates (vacatingFolder) while that move is still to be
// decided: n is decided after it, so planMove can order n after it. A file
// whose OneDrive source planMove rebases through that move is also decided
// after every folder OneDrive relocates, so rebasedSourceMisread sees the
// source of each folder move the plan makes.
func (e *moveEmitter) waitsForVacatingFolder(n *moveNode, side moveCoord, newPath string) bool {
	folder, lands := e.vacatingFolder(n, side, newPath)
	if !lands {
		return false
	}
	if e.state[folder] == moveEmitPending {
		return true
	}

	return side == moveCoordRemote && n.itemType == syncdomain.ItemTypeFile && e.remoteFolderUndecided()
}

// remoteFolderUndecided reports whether a folder OneDrive relocates is still
// pending.
func (e *moveEmitter) remoteFolderUndecided() bool {
	if !e.remoteFoldersListed {
		e.remoteFoldersListed = true
		for _, key := range e.keys {
			n := e.nodes[key]
			if n.itemType == syncdomain.ItemTypeFolder && e.isMoveParticipant(n) &&
				!sameLocation(e.location(n, moveCoordRemote), e.target[key]) {
				e.undecidedRemoteFolders = append(e.undecidedRemoteFolders, key)
			}
		}
	}
	undecided := e.undecidedRemoteFolders[:0]
	for _, key := range e.undecidedRemoteFolders {
		if e.state[key] == moveEmitPending {
			undecided = append(undecided, key)
		}
	}
	e.undecidedRemoteFolders = undecided

	return len(undecided) > 0
}

// rebasedSourceMisread reports whether a file's OneDrive source, rebased
// through the move of the folder that vacates the path its target lies at or
// below, lies below the source of another folder move of the plan that does
// not carry target too. That folder's path is then filled again (the vacating
// folder moves into a folder made there or moved onto it, or onto the path
// itself), and the source exists only after it moved away; but the
// dependency rules read work below a folder move's source as leaving the
// folder first (children leave first), so the folder move would wait for the
// file, which waits for the vacating folder's move, which waits for the path
// to be filled after the folder move: a cycle dropped on every pass. Such a
// file waits for the next pass, which observes both folders moved.
func (e *moveEmitter) rebasedSourceMisread(source, target string) bool {
	for i := range e.result.moves {
		move := &e.result.moves[i]
		if move.side != moveCoordRemote || e.nodes[move.key].itemType != syncdomain.ItemTypeFolder {
			continue
		}
		if strings.HasPrefix(source, move.oldPath+"/") && !strings.HasPrefix(target, move.newPath+"/") {
			return true
		}
	}

	return false
}

// localParentPresent reports whether dir, in the local snapshot, is the
// directory n's target hangs off.
func (e *moveEmitter) localParentPresent(n *moveNode, dir string) bool {
	parent := e.target[n.key].parent
	if strings.HasPrefix(parent, unsyncedParentPrefix) {
		return e.occupancy.local[dir]
	}
	key, ok := e.byPath[moveCoordLocal][dir]

	return ok && key == parent
}

// destinationFree reports whether newPath is free on side when the move runs:
// empty in the snapshot, or vacated by a move earlier in this plan.
//
// A local destination may also hold a synced item OneDrive no longer has (it
// moved a.txt over b.txt, replacing b): that entry gives way as its
// displacement says, and the move is planned with it.
func (e *moveEmitter) destinationFree(n *moveNode, side moveCoord, newPath string) (bool, moveDisplacement) {
	occupied := e.occupancy.local
	if side == moveCoordRemote {
		occupied = e.occupancy.remote
	}
	snapshotPath := e.snapshotPath(side, newPath)
	occupant, isNode := e.occupantAt(side, snapshotPath)
	if !isNode && !e.occupiedFolded(side, occupied, snapshotPath) {
		return true, moveDisplaceNever
	}
	if isNode && occupant == n.key {
		return true, moveDisplaceNever
	}
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if earlier.side == side && namekey.OfPath(e.nodes[earlier.key].paths[side]) == namekey.OfPath(snapshotPath) {
			return true, moveDisplaceNever
		}
	}
	if side == moveCoordLocal && snapshotPath == newPath {
		if displace := e.localDisplacementAt(occupant, isNode, snapshotPath); displace != moveDisplaceNever {
			return true, displace
		}
		if e.newFolderMeetsAFile(n, isNode, snapshotPath) {
			return true, moveDisplaceConflictCopy
		}
		if e.emptyNewFolderGivesWay(n, isNode, snapshotPath) {
			return true, moveDisplaceEmptyFolder
		}
	}

	return false, moveDisplaceNever
}

// emptyNewFolderGivesWay reports a synced folder n moving onto p here, where
// the entry is an empty folder that is no synced item: two folders at one
// path are one folder by path (R-2.17.13), and with nothing in the new one,
// n takes the path whole. From the synced tree, p moved into x and a new p
// made here while OneDrive moved x into p: OneDrive's nesting wins, p goes
// back to its path, where the new p is, and the move waited for good for it
// to leave. A new folder that holds anything is not given way so.
func (e *moveEmitter) emptyNewFolderGivesWay(n *moveNode, isNode bool, p string) bool {
	return n.itemType == syncdomain.ItemTypeFolder && !isNode && !e.collisionUndone[n.key] && e.occupancy.localFolders[p] &&
		!e.holdsAnythingHere(p)
}

// localDisplacementAt is how the local entry at p gives way to a move onto
// it. A composed occupant gives way as its own displacement says. An entry
// that is no composed item may be a new file at the path of a synced file
// OneDrive no longer has, saved there after a replacing move (D-058) freed
// the name: that baseline item, absent here as itself, carries how the entry
// at its path gives way (composedDisplacement). A new file at a path no
// synced item had is the user's new content under the name OneDrive gave
// the moving item (z renamed g on OneDrive while a new g was made here): it
// is renamed to a conflict copy first and uploaded later as a new file, so
// the move runs and neither copy is lost. Left in place, the move waited
// for good while the reconciliation of the path downloaded the item a
// second time beside its copy here.
func (e *moveEmitter) localDisplacementAt(occupant string, isNode bool, p string) moveDisplacement {
	if isNode {
		if movedAwayHere(e.nodes[occupant]) {
			// Its displacement is that of the entry at its baseline path;
			// its own copy at its new path stays.
			return moveDisplaceNever
		}
		return e.nodes[occupant].displace
	}
	if !e.occupancy.local[p] {
		return moveDisplaceNever
	}
	key, ok := e.byPath[moveCoordBaseline][p]
	if !ok {
		if e.occupancy.localFiles[p] {
			return moveDisplaceConflictCopy
		}
		return moveDisplaceNever
	}

	return e.nodes[key].displace
}

// newFolderMeetsAFile reports a file n moving onto p here, where the entry is
// a folder that is no synced item at its place (one made here, at a path
// whose synced folder moved elsewhere here or none had): a file and a folder
// cannot share the path, and the reconciliation of the path keeps both, the
// folder as a conflict copy. The move is composed with that copy, so the
// file lands here as itself; left waiting, the path's reconciliation
// downloaded it beside its copy here, which went back to OneDrive as a new
// file.
func (e *moveEmitter) newFolderMeetsAFile(n *moveNode, isNode bool, p string) bool {
	return n.itemType == syncdomain.ItemTypeFile && !isNode && e.occupancy.localFolders[p]
}

// movedAwayHere reports a node whose synced copy sits here at another path
// than its baseline path.
func movedAwayHere(n *moveNode) bool {
	return n.present(moveCoordLocal) && n.paths[moveCoordLocal] != n.paths[moveCoordBaseline]
}

// occupantAt is the composed item holding p on side, compared as OneDrive
// compares names: one folder cannot hold two names that fold together, so an
// item at d/50%.Txt occupies d/50%.txt too. An exact match wins.
func (e *moveEmitter) occupantAt(side moveCoord, p string) (string, bool) {
	if key, ok := e.byPath[side][p]; ok {
		return key, true
	}
	if e.foldedByPath[side] == nil {
		e.foldedByPath[side] = make(map[namekey.PathKey]string, len(e.byPath[side]))
		for held, key := range e.byPath[side] {
			e.foldedByPath[side][namekey.OfPath(held)] = key
		}
	}
	key, ok := e.foldedByPath[side][namekey.OfPath(p)]

	return key, ok
}

// occupiedFolded reports whether the snapshot holds p on side under
// OneDrive's name folding.
func (e *moveEmitter) occupiedFolded(side moveCoord, occupied map[string]bool, p string) bool {
	if occupied[p] {
		return true
	}
	if e.foldedOccupied[side] == nil {
		e.foldedOccupied[side] = make(map[namekey.PathKey]bool, len(occupied))
		for held := range occupied {
			e.foldedOccupied[side][namekey.OfPath(held)] = true
		}
	}

	return e.foldedOccupied[side][namekey.OfPath(p)]
}

// snapshotPath maps a path as it will be when a move runs back to the
// snapshot, undoing the earlier moves of this plan on the same side.
func (e *moveEmitter) snapshotPath(side moveCoord, p string) string {
	for i := len(e.result.moves) - 1; i >= 0; i-- {
		earlier := &e.result.moves[i]
		if earlier.side != side {
			continue
		}
		switch {
		case p == earlier.newPath:
			p = earlier.oldPath
		case strings.HasPrefix(p, earlier.newPath+"/"):
			p = earlier.oldPath + strings.TrimPrefix(p, earlier.newPath)
		}
	}

	return p
}
