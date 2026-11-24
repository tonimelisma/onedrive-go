package syncplan

import (
	"maps"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// moveEmitState tracks whether an item's target position is established for
// the items below it.
type moveEmitState int

const (
	moveEmitPending moveEmitState = iota
	// moveEmitSettled: the item is at its target on every side once the
	// actions of this plan that it waits for have run.
	moveEmitSettled
	moveEmitDeferred
)

type moveEmitter struct {
	*moveComposer
	state  map[string]moveEmitState
	result moveComposition
	// foldedByPath and foldedOccupied index the snapshot by folded path,
	// built on first use (occupantAt, occupiedFolded).
	foldedByPath   [moveCoordCount]map[namekey.PathKey]string
	foldedOccupied [moveCoordCount]map[namekey.PathKey]bool
	// undecidedRemoteFolders lists the folders OneDrive relocates that are
	// still pending, listed on first use and shrunk as they are decided
	// (remoteFolderUndecided).
	undecidedRemoteFolders []string
	remoteFoldersListed    bool
	// landings maps each key the plan moves on OneDrive into a folder made
	// here to the item OneDrive gives that folder's path (folderHolder), and
	// loopHeld maps each key that waits as the second of partner moves to the
	// first of them (remoteLandingLoops), or to the file still to be decided
	// it waits for, which may turn out to be the first. remoteHeld holds the
	// keys whose OneDrive move remoteLandingWaits holds, and takenBack the
	// keys whose move an earlier run took back (takeBackHeldLandings); each
	// waits for the next pass. parkedFirst holds the holders an earlier run
	// found nested below the move that lands in the folder at their path, which
	// move aside on OneDrive first (takeBackHeldLandings).
	landings    map[string]string
	loopHeld    map[string]string
	remoteHeld  map[string]bool
	takenBack   map[string]bool
	parkedFirst map[string]bool
}

// emit decides every item. A run that plans an OneDrive move into a folder
// whose holder ends the run without an OneDrive move of its own takes that
// move back and runs again with the item waiting, so every rule sees it stay
// and what was decided because it moved is decided again. Each run takes back
// items no earlier run did, so there are at most as many runs as items, and
// every run decides them in the same order.
func (c *moveComposer) emit() moveComposition {
	// Shallow targets first: a parent's decision is known before its
	// children's, and siblings keep a stable order.
	order := append([]string(nil), c.keys...)
	sort.SliceStable(order, func(i, j int) bool {
		pi, pj := c.targetPath(order[i]), c.targetPath(order[j])
		di, dj := strings.Count(pi, "/"), strings.Count(pj, "/")
		if di != dj {
			return di < dj
		}
		return pi < pj
	})

	takenBack, parkedFirst := map[string]bool{}, map[string]bool{}
	e := c.emitRun(order, takenBack, parkedFirst)
	for range 2 * len(order) {
		if !e.takeBackHeldLandings() {
			break
		}
		e = c.emitRun(order, takenBack, parkedFirst)
	}
	e.holdOwnLandings()
	e.result.notices = append(c.notices, e.result.notices...)

	return e.result
}

// emitRun decides every item in order, the items of takenBack waiting for the
// next pass and those of parkedFirst moving aside on OneDrive.
func (c *moveComposer) emitRun(order []string, takenBack, parkedFirst map[string]bool) *moveEmitter {
	e := &moveEmitter{
		moveComposer: c,
		state:        make(map[string]moveEmitState, len(c.keys)),
		result:       moveComposition{deferred: map[string]bool{}, deferredMoves: map[string][]syncdomain.ActionType{}},
		landings:     map[string]string{},
		loopHeld:     map[string]string{},
		remoteHeld:   map[string]bool{},
		takenBack:    takenBack,
		parkedFirst:  parkedFirst,
	}
	for range len(order) + 1 {
		e.resolvePending(order)
		if !e.parkOneOccupancyCycle(order) && !e.parkOneVacatingLoop(order) && !e.moveOneIntoItsWaitingFolder(order) {
			break
		}
	}
	for _, key := range order {
		if e.state[key] == moveEmitPending {
			e.deferNode(key)
		}
	}

	return e
}

// takeBackHeldLandings adds to takenBack each key whose planned OneDrive move
// lands in a folder made here at the path of an item (landings) that ends the
// run without an OneDrive move of its own: the item stays at the path this
// pass, so the move would land in it and stay there for as long as the item
// does. The partner moves of a folder that waits as the second of them keep
// theirs (isLoopPartner): that folder moves the next pass and carries them
// along.
//
// A holder whose own target lies below key's (targetHangsOff) can never be
// at its target before key has landed, so taking key's move back holds both
// for good: here H moved aside, Y moved into a new folder H as H/Y, and the
// old H into H/Y/f/a/H, while OneDrive kept H and Y. Such a holder moves
// aside on OneDrive first (parkedFirst, park), which frees its path for the
// folder made here; key's move then lands in that folder, which the plan
// makes on OneDrive once the holder has left (recreatedSourceCreates), and a
// later pass moves the holder below key. A holder that cannot be parked has
// key's move taken back. It reports whether it changed the decisions of the
// run.
func (e *moveEmitter) takeBackHeldLandings() bool {
	changed := false
	// Decided in key order, and a holder parked by this round keeps every
	// move landing in its path, so the outcome does not hang on the order
	// the map gives.
	parkedNow := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(e.landings)) {
		holder := e.landings[key]
		if (e.movedOnOneDrive(holder) && !e.landsBelowTheMoveItHolds(holder, key)) ||
			e.isLoopPartner(key, holder) || parkedNow[holder] {
			continue
		}
		if !e.parkedFirst[holder] && e.mustLeaveBeforeItsLanding(holder, key) {
			e.parkedFirst[holder] = true
			parkedNow[holder] = true
			changed = true
			continue
		}
		e.takenBack[key] = true
		changed = true
	}

	return changed
}

// landsBelowTheMoveItHolds reports whether holder's OneDrive move of the
// plan lands at or below the destination of key's, which lands in the folder
// made here at holder's path: the folder is made once holder has left, key's
// move waits for it, and holder's for key's, a cycle dropped on every pass
// (from the held start, here the held x moved into p as p/f and p into the
// new x, while f, which OneDrive keeps at p/f, leaves p for the new x).
// Such a holder moves aside first instead (mustLeaveBeforeItsLanding).
func (e *moveEmitter) landsBelowTheMoveItHolds(holder, key string) bool {
	var keyTarget string
	for i := range e.result.moves {
		if move := &e.result.moves[i]; move.key == key && move.side == moveCoordRemote {
			keyTarget = move.newPath
		}
	}
	if keyTarget == "" {
		return false
	}
	for i := range e.result.moves {
		if move := &e.result.moves[i]; move.key == holder && move.side == moveCoordRemote && pathAtOrBelow(move.newPath, keyTarget) {
			return true
		}
	}

	return false
}

// mustLeaveBeforeItsLanding reports whether holder, which OneDrive keeps at
// the path of the folder key's OneDrive move lands in, has its own target
// below key's and can move aside on OneDrive: a participant there that is
// not frozen (unresolvable).
func (e *moveEmitter) mustLeaveBeforeItsLanding(holder, key string) bool {
	h := e.nodes[holder]

	return h != nil && !e.unresolvable[holder] && h.present(moveCoordRemote) &&
		e.isMoveParticipant(h) && !sameLocation(e.location(h, moveCoordRemote), e.target[holder]) &&
		e.targetHangsOff(holder, key)
}

// isLoopPartner reports whether key's move is one of the partner moves holder
// waits for as the second of them (loopHeld): the first, or a move the first
// lands in through the moves it waits for (remoteLandingLoops). Only a folder
// holder has partners: key's move lands in the holder on OneDrive this pass,
// and a file there refuses it on every pass.
func (e *moveEmitter) isLoopPartner(key, holder string) bool {
	first, held := e.loopHeld[holder]

	return held && e.nodes[holder].itemType == syncdomain.ItemTypeFolder && (key == first || e.remoteLandingLoops(first, key))
}

// resolvePending resolves pending items until a sweep makes no progress.
func (e *moveEmitter) resolvePending(order []string) {
	for range len(order) + 1 {
		progressed := false
		for _, key := range order {
			if e.state[key] != moveEmitPending {
				continue
			}
			if e.resolve(key) {
				progressed = true
			}
		}
		if !progressed {
			return
		}
	}
}

// parentState is the emit state of whatever an item's target position hangs
// off. An unsynced directory is settled only while its anchor needs no move of
// its own: a directory that is not a synced item has no move to carry it.
func (e *moveEmitter) parentState(key string) moveEmitState {
	parent := e.target[key].parent
	if parent == "" {
		return moveEmitSettled
	}
	if strings.HasPrefix(parent, unsyncedParentPrefix) {
		anchor, _ := e.unsyncedAnchor(parent)
		if anchor == "" {
			return moveEmitSettled
		}
		if e.state[anchor] == moveEmitSettled && !e.hasOwnAction(anchor) {
			return e.parentState(anchor)
		}
		if e.state[anchor] == moveEmitPending {
			return moveEmitPending
		}
		return moveEmitDeferred
	}
	if _, known := e.nodes[parent]; !known {
		return moveEmitSettled
	}

	return e.state[parent]
}

func (e *moveEmitter) hasOwnAction(key string) bool {
	for i := range e.result.moves {
		if e.result.moves[i].key == key {
			return true
		}
	}
	for i := range e.result.rekeys {
		if e.result.rekeys[i].key == key {
			return true
		}
	}

	return false
}

func (e *moveEmitter) deferNode(key string) {
	e.state[key] = moveEmitDeferred
	n := e.nodes[key]
	if e.relocated(n) {
		e.result.deferred[key] = true
		e.result.deferredMoves[key] = e.relocationMoves(n)
		note := e.notice(moveNoticeDeferred, n)
		note.targetPath = e.targetPath(key)
		e.result.notices = append(e.result.notices, note)
	}
}

func (e *moveEmitter) relocated(n *moveNode) bool {
	target := e.target[n.key]
	for coord := range moveCoordCount {
		if n.present(coord) && !sameLocation(e.location(n, coord), target) {
			return true
		}
	}

	return false
}

// relocationMoves is the moves that bring n to its target: a local move
// when it is elsewhere on this computer, a remote move when it is elsewhere
// on OneDrive.
func (e *moveEmitter) relocationMoves(n *moveNode) []syncdomain.ActionType {
	target := e.target[n.key]
	var moves []syncdomain.ActionType
	if n.present(moveCoordLocal) && !sameLocation(e.location(n, moveCoordLocal), target) {
		moves = append(moves, syncdomain.ActionLocalMove)
	}
	if n.present(moveCoordRemote) && !sameLocation(e.location(n, moveCoordRemote), target) {
		moves = append(moves, syncdomain.ActionRemoteMove)
	}

	return moves
}

// resolve decides one item. It reports whether the item left the pending
// state.
func (e *moveEmitter) resolve(key string) bool {
	if e.parkedFirst[key] {
		// It moves aside whatever its target parent's decision: its target
		// lies below the move its leaving lets run.
		if !e.park(key, moveCoordRemote) {
			e.deferNode(key)
		}
		return true
	}
	switch e.parentState(key) {
	case moveEmitPending:
		return false
	case moveEmitDeferred:
		e.deferNode(key)
		return true
	case moveEmitSettled:
	}

	n := e.nodes[key]
	if e.unresolvable[key] || e.takenBack[key] {
		e.deferNode(key)
		return true
	}

	if !e.isMoveParticipant(n) {
		// Present on one side only: a create or delete that SQLite plans.
		e.state[key] = moveEmitSettled
		return true
	}
	target := e.target[key]
	newPath := e.targetPath(key)

	for _, side := range []moveCoord{moveCoordLocal, moveCoordRemote} {
		if n.present(side) && !sameLocation(e.location(n, side), target) {
			return e.resolveMove(n, side, newPath)
		}
	}

	if n.present(moveCoordBaseline) && !sameLocation(e.location(n, moveCoordBaseline), target) {
		e.result.rekeys = append(e.result.rekeys, composedRekey{
			key:     key,
			oldPath: n.paths[moveCoordBaseline],
			newPath: newPath,
		})
	}
	e.state[key] = moveEmitSettled

	return true
}

// newFolderHoldingItsSource reports the path of a folder made here, no synced
// item, that holds n's local move's destination while n's own source lies
// inside it: OneDrive's move of a folder wins over this computer's, which
// moved it into a folder that a new folder at its old path now holds (here x
// moved into Q as Q/renamed-x, a new x made and Q moved into it, while
// OneDrive moved Q into the old x). The move can never land while the new
// folder holds the path, and the folder cannot leave it before it lands, so
// the item waited for good while the creates below it failed on every pass.
// The new folder is renamed to a conflict copy, kept with everything in it,
// and the next pass moves the item out of it onto its path.
func (e *moveEmitter) newFolderHoldingItsSource(n *moveNode, side moveCoord, newPath string) (string, bool) {
	if side != moveCoordLocal || n.itemType != syncdomain.ItemTypeFolder {
		return "", false
	}
	at := e.snapshotPath(side, newPath)
	if at != newPath || !e.occupancy.local[at] || e.occupancy.localFiles[at] || !strings.HasPrefix(n.paths[side], at+"/") {
		return "", false
	}
	if _, isNode := e.occupantAt(side, at); isNode {
		return "", false
	}

	return at, true
}

// resolveMove decides n's move on side to newPath: it waits while the folder
// whose move vacates that path is still to be decided, moves n aside when it
// lands in a folder made at its own path (parkAwayFromItsOwnLanding), and
// otherwise plans the move or defers it. It reports whether n left the
// pending state.
func (e *moveEmitter) resolveMove(n *moveNode, side moveCoord, newPath string) bool {
	if e.landsPastARelocatedFolder(n, side, newPath) {
		e.deferNode(n.key)
		return true
	}
	if e.waitsForVacatingFolder(n, side, newPath) {
		return false
	}
	if aside, holds := e.newFolderHoldingItsSource(n, side, newPath); holds {
		e.result.asideFolders = append(e.result.asideFolders, aside)
		e.deferNode(n.key)
		return true
	}
	if holder, held := e.folderHolder(side, n.key, newPath); held && holder == n.key {
		return e.parkAwayFromItsOwnLanding(n.key, side)
	}
	if e.mergesWithANewFolderHere(n, side, newPath) {
		e.result.merges = append(e.result.merges, composedMerge{key: n.key, localPath: n.paths[side], newPath: newPath})
		e.state[n.key] = moveEmitSettled
		return true
	}
	move, ok := e.planMove(n, side, newPath)
	if !ok {
		return e.deferRefusedMove(n.key, side)
	}
	e.result.moves = append(e.result.moves, move)
	e.state[n.key] = moveEmitSettled

	return true
}

// mergesWithANewFolderHere reports whether n, a synced folder whose local
// move to newPath follows OneDrive (it sits there on OneDrive: OneDrive moved
// it, or this computer's own move of it was undone), lands on a folder made
// here that is no synced item, while n's own copy here holds nothing. The
// two folders at the path are one folder by path, as two folders made at one
// path on both sides are (R-2.17.13): the new folder takes n's item, which
// composition records at the path with the new folder's identity, and n's
// empty copy is deleted here, so nothing moves. Before, the move waited for
// good for the new folder to leave its destination (`rm x, mv p x` on
// OneDrive from the held start, where x here is the new folder made in the
// held one's place and p is empty here since f left it). The executor's
// delete still refuses a copy that gained an entry since the scan. No move of
// the plan here may carry n's copy or the destination, so both are snapshot
// paths.
func (e *moveEmitter) mergesWithANewFolderHere(n *moveNode, side moveCoord, newPath string) bool {
	source := n.paths[side]
	if side != moveCoordLocal || n.itemType != syncdomain.ItemTypeFolder || !n.present(moveCoordRemote) ||
		e.collisionUndone[n.key] ||
		!sameLocation(e.location(n, moveCoordRemote), e.target[n.key]) || pathAtOrBelow(newPath, source) ||
		pathAtOrBelow(source, newPath) || !e.occupancy.localFolders[newPath] {
		return false
	}
	if _, isNode := e.occupantAt(side, newPath); isNode {
		return false
	}
	if dir := path.Dir(newPath); dir != "." && !e.localParentPresent(n, dir) {
		return false
	}

	return !e.localMoveOfThePlanTouches(source, newPath) && !e.holdsAnythingHere(source)
}

// localMoveOfThePlanTouches reports whether a local move planned so far
// leaves from or lands at or above one of paths.
func (e *moveEmitter) localMoveOfThePlanTouches(paths ...string) bool {
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if earlier.side != moveCoordLocal {
			continue
		}
		for _, p := range paths {
			if pathAtOrBelow(p, earlier.oldPath) || pathAtOrBelow(p, earlier.newPath) {
				return true
			}
		}
	}

	return false
}

// holdsAnythingHere reports whether the local snapshot holds an entry below
// folder.
func (e *moveEmitter) holdsAnythingHere(folder string) bool {
	for held := range e.occupancy.local {
		if strings.HasPrefix(held, folder+"/") {
			return true
		}
	}

	return false
}

// deferRefusedMove decides key, whose move on side planMove refused. It stays
// pending while another item holds its destination, which a move of its own
// may still vacate (parkOneOccupancyCycle), unless the move waits for the item
// at the path of the folder it lands in on OneDrive (remoteHeld): nothing this
// run decides later lets it run, so it is deferred at once, and no item is
// parked to free its destination for a move that waits anyway. It reports
// whether key left the pending state.
func (e *moveEmitter) deferRefusedMove(key string, side moveCoord) bool {
	if _, waits := e.blockingPendingMove(key, side); waits && !e.remoteHeld[key] {
		return false
	}
	e.deferNode(key)

	return true
}

// planMove builds the move of n on side, or reports that it must wait for a
// later pass. For an OneDrive move into a folder made here it records the
// item that holds the folder's path (recordLanding).
func (e *moveEmitter) planMove(n *moveNode, side moveCoord, newPath string) (composedMove, bool) {
	oldPath := n.paths[side]
	remotePath := ""
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if before, carriedRemotePath, lands := e.landsInTheMovingFolderFirst(n, side, oldPath, newPath, earlier); lands {
			newPath, remotePath = before, carriedRemotePath
			continue
		}
		if earlier.side != side || !strings.HasPrefix(oldPath, earlier.oldPath+"/") {
			continue
		}
		if !strings.HasPrefix(newPath, earlier.newPath+"/") {
			if e.leavesBeforeItsFolder(n, side, oldPath, newPath, earlier) {
				// The earlier move waits for this one (children leave first).
				continue
			}
			// The item lands at or below the path the earlier move vacates (a
			// folder made there, or that path itself), so it moves after that
			// move. On OneDrive a file's source is rebased through it, as for
			// an ancestor's move below. A local source rebased so is not in
			// local truth before the next scan. Work below a folder addressed
			// at its old paths lies below both the earlier move's source and
			// this move's destination, so each move would wait for it. Those
			// moves wait for the next pass, which observes the folder moved,
			// and so does a file whose rebased source the dependency rules
			// would misread (rebasedSourceMisread).
			if side == moveCoordLocal || n.itemType != syncdomain.ItemTypeFile {
				return composedMove{}, false
			}
			oldPath = earlier.newPath + strings.TrimPrefix(oldPath, earlier.oldPath)
			if e.rebasedSourceMisread(oldPath, newPath) {
				return composedMove{}, false
			}
			continue
		}
		// An ancestor's move carries both this item's source and its target.
		// On OneDrive this move runs after it, with the source rebased:
		// remote_state and the baseline follow a committed move. local_state
		// is observation-owned and does not, so locally this move runs first,
		// inside the folder before it moves, when its target parent is
		// already there; otherwise it waits a pass.
		if side == moveCoordLocal {
			before := earlier.oldPath + strings.TrimPrefix(newPath, earlier.newPath)
			if !e.localParentPresent(n, path.Dir(before)) {
				return composedMove{}, false
			}
			newPath = before
			remotePath = n.paths[moveCoordRemote]
			continue
		}
		oldPath = earlier.newPath + strings.TrimPrefix(oldPath, earlier.oldPath)
	}
	// The holder is looked up at the final newPath: a local move that runs
	// inside a folder before the folder moves lands where that folder is now.
	if e.waitsForTheNextPass(n.key, side, oldPath, newPath) {
		return composedMove{}, false
	}
	if oldPath == newPath {
		return composedMove{}, false
	}
	free, displaced := e.destinationFree(n, side, newPath)
	if !free {
		return composedMove{}, false
	}
	e.recordLanding(n.key, side, newPath)

	return composedMove{
		key:        n.key,
		side:       side,
		oldPath:    oldPath,
		newPath:    newPath,
		localPath:  n.paths[moveCoordLocal],
		remotePath: remotePath,
		displaced:  displaced,
	}, true
}

// landsInTheMovingFolderFirst reports where n's move on side lands when it
// runs inside the folder whose earlier move of the plan carries its target,
// at the path that folder has there now (landsInsideBeforeItMoves here,
// landsInTheFolderItMakesWayFor on OneDrive), and, for a local move, where
// OneDrive holds the item (the move's remotePath).
func (e *moveEmitter) landsInTheMovingFolderFirst(
	n *moveNode, side moveCoord, oldPath, newPath string, earlier *composedMove,
) (before, remotePath string, lands bool) {
	if before, lands = e.landsInsideBeforeItMoves(n, side, oldPath, newPath, earlier); lands {
		return before, n.paths[moveCoordRemote], true
	}
	before, lands = e.landsInTheFolderItMakesWayFor(n, side, oldPath, newPath, earlier)

	return before, "", lands
}

// waitsForTheNextPass reports whether key's move on side from oldPath to
// newPath waits for the next pass: for the item holding the folder it lands
// in (waitsForAFolderHolder), or for a folder above it to leave its
// destination (landsWhereAFolderAboveItLeaves).
func (e *moveEmitter) waitsForTheNextPass(key string, side moveCoord, oldPath, newPath string) bool {
	return e.waitsForAFolderHolder(key, side, newPath) || e.landsWhereAFolderAboveItLeaves(side, oldPath, newPath)
}

// landsWhereAFolderAboveItLeaves reports whether a local move from oldPath
// lands on the path an earlier local move of the plan vacates whose source
// holds oldPath: that folder's move waits for the entry to leave it first
// (children leave first) and the entry's for the folder to leave its
// destination, a cycle dropped on every pass. The entry waits for the next
// pass, which finds it where the folder's move carried it. Here p moved into
// x while OneDrive moved x into p and p/f onto x: p leaves x, x moves into p,
// and f, carried to p/f, moves onto x the next pass.
func (e *moveEmitter) landsWhereAFolderAboveItLeaves(side moveCoord, oldPath, newPath string) bool {
	if side != moveCoordLocal {
		return false
	}
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if earlier.side == side && earlier.oldPath == newPath && strings.HasPrefix(oldPath, earlier.oldPath+"/") {
			return true
		}
	}

	return false
}

// landsInsideBeforeItMoves reports where n's local move lands when its
// target lies below the destination of an earlier local folder move of the
// plan and its source outside that folder: inside the folder at the path the
// folder has here now, where the folder's move then carries it
// (landsInTheFolderMoveCarries orders the folder's move after it), when its
// parent there is already present. Ordered after the folder's move, it waited
// for it, while the folder waited for a child to leave, which waited for the
// path n vacates: here d/c.swap-1.md, parked by an earlier pass of a swap on
// OneDrive, moving to c.md, c.md moving into d's destination, and d moving.
func (e *moveEmitter) landsInsideBeforeItMoves(n *moveNode, side moveCoord, oldPath, newPath string, earlier *composedMove) (string, bool) {
	if side != moveCoordLocal || earlier.side != moveCoordLocal || e.nodes[earlier.key].itemType != syncdomain.ItemTypeFolder ||
		!strings.HasPrefix(newPath, earlier.newPath+"/") || pathAtOrBelow(oldPath, earlier.oldPath) ||
		pathAtOrBelow(earlier.oldPath, oldPath) {
		// Inside n itself, the folder's path here now is no place n can
		// move to: n waits for that folder to leave it first.
		return "", false
	}
	if !e.childLeavesForPath(earlier.oldPath, oldPath) {
		return "", false
	}
	before := earlier.oldPath + strings.TrimPrefix(newPath, earlier.newPath)
	if !e.localParentPresent(n, path.Dir(before)) {
		return "", false
	}

	return before, true
}

// landsInTheFolderItMakesWayFor reports where n's OneDrive move lands when
// n holds there the path of the new folder, made here, that the earlier
// OneDrive move of a folder lands in (landings), and n's target lies below
// that folder's destination: inside the folder at the path it has on
// OneDrive now, where the folder's move then carries n
// (landsInTheFolderMoveCarries orders the folder's move after n's, as n's
// local position is where that move carries the path). Moved to its target
// itself, n waited for the folder's move, which waited for the new folder,
// which waited for n to leave its path, a cycle dropped on every pass: from
// the held start, here p moved into the new x and the held x into p, while
// OneDrive still holds x at the new folder's path. n's move vacates the path
// first, the new folder is made there, and p's move lands in it.
func (e *moveEmitter) landsInTheFolderItMakesWayFor(
	n *moveNode, side moveCoord, oldPath, newPath string, earlier *composedMove,
) (string, bool) {
	if side != moveCoordRemote || earlier.side != moveCoordRemote || e.landings[earlier.key] != n.key ||
		e.nodes[earlier.key].itemType != syncdomain.ItemTypeFolder || !strings.HasPrefix(newPath, earlier.newPath+"/") ||
		pathAtOrBelow(oldPath, earlier.oldPath) || pathAtOrBelow(earlier.oldPath, oldPath) {
		return "", false
	}
	before := earlier.oldPath + strings.TrimPrefix(newPath, earlier.newPath)
	if n.paths[moveCoordLocal] != newPath || path.Dir(before) != earlier.oldPath ||
		e.occupiedFolded(moveCoordRemote, e.occupancy.remote, before) {
		// An entry OneDrive keeps at that path leaves it only by a move of
		// its own, which may itself wait for the new folder: n moves aside
		// first instead (takeBackHeldLandings).
		return "", false
	}

	return before, true
}

// childLeavesForPath reports whether an item below folder here has p as its
// target: that item leaves the folder before the folder moves, so the move
// that vacates p must not wait for the folder's move.
func (e *moveEmitter) childLeavesForPath(folder, p string) bool {
	for _, key := range e.keys {
		m := e.nodes[key]
		if m.present(moveCoordLocal) && strings.HasPrefix(m.paths[moveCoordLocal], folder+"/") && e.targetPath(key) == p {
			return true
		}
	}

	return false
}

// leavesBeforeItsFolder reports whether n, leaving the folder whose move on
// side earlier is, moves before that move: unless it lands at or below the
// path the folder's move vacates, or leaves after the folder on OneDrive
// (leavesAfterItsFolder).
func (e *moveEmitter) leavesBeforeItsFolder(n *moveNode, side moveCoord, oldPath, newPath string, earlier *composedMove) bool {
	if folder, lands := e.vacatingFolder(n, side, newPath); lands && folder == earlier.key {
		return false
	}

	return !leavesAfterItsFolder(n, side, oldPath, earlier)
}

// leavesAfterItsFolder reports whether n, a file directly in the folder whose
// move on OneDrive earlier is, leaves the folder after that move, from its
// source rebased below the folder's new path, rather than first. OneDrive
// moves an item into its parent by ID, so either order lands it; leaving
// first, its move into a folder made here at a path another move of the
// plan vacates closed a cycle when that other move lands below the folder's
// new path (from the held start, here p renamed q and the held x moved into
// it as q/x2 while f left p for the new x: x's move waited for p's, p's for
// f leaving, f's for the new x, and the new x for x's move). A file further
// down leaves first: the folders between it and the moved folder may be
// deleted or moved on their own, and their deletes wait for it at their
// paths as the plan has them.
func leavesAfterItsFolder(n *moveNode, side moveCoord, oldPath string, earlier *composedMove) bool {
	return side == moveCoordRemote && n.itemType == syncdomain.ItemTypeFile && path.Dir(oldPath) == earlier.oldPath
}
