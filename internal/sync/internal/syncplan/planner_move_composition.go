package syncplan

import (
	"log/slog"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Move composition decides, once per synced item, where the item ends up and
// which moves get it there (D-050, D-051, D-053, D-054, D-055).
//
// SQLite keeps structural pairing: it proves which current local path holds
// a baseline item (filesystem identity plus content corroboration) and which
// remote path holds it (item ID). What it cannot do is compose the two sides,
// because an item's path is the composition of its parents' moves. Composing
// path by path planned a remote move whose source the local side had already
// moved, renamed onto itself, or moved two folders into each other.
//
// Composition therefore works on locations, not paths: an item sits on each
// side under a parent item with a name. An item whose location changed on a
// side was relocated there; one whose parent moved simply follows it. The
// target location is the side that relocated it, or, when both did:
//
//   - the same location on both sides: nothing moves, the baseline is re-keyed
//     (D-050);
//   - different locations: OneDrive's wins and the local copy is moved to it
//     (R-2.17.6, D-051). Neither choice loses data; remote-wins needs no remote
//     mutation, so the local rename is the cheapest correct repair, and it is
//     the same rule that breaks cycles;
//   - a cycle (local A→B/A while OneDrive moved B→A/B): the remote move is kept
//     and the local one undone, so neither tree nests inside the other (D-055);
//   - two items claiming one target: the local claim is undone.
//
// Emission is deliberately conservative: a move is emitted only when its
// target parent is already in place or is placed by a move earlier in the same
// plan, its destination is free or vacated by an earlier move, and its source
// path is not displaced by a move that runs first (except on OneDrive, where
// remote_state and the baseline follow every committed move). A folder
// OneDrive made at a path that, on this computer, a synced item leaving it
// still holds is not in place: a local move into it and that item's own move
// are never planned together (localFolderHolder). The one decided first
// moves, except that the move into the folder waits while the item is a file
// or frozen, which it cannot land in (cannotLandInTheHolder). A folder made
// here is created on OneDrive once the synced item OneDrive keeps at its path
// has left by its own move of the plan, so a move sent into it waits while
// that item stays at its target this pass or is a file or a frozen folder
// still to be decided (remoteLandingWaits), and also when the item is decided
// without an OneDrive move, before or after the move (takeBackHeldLandings).
// Of two items each moved into a new folder at the other's path, a folder
// waits and the other's move lands in it, which it carries along the next
// pass: of two folders, the one decided second waits, and two files both
// wait. On both sides the item is found at the first path above the new
// folders that a synced item holds (folderHolder). Anything else is deferred:
// the item stays where it is on both sides and the next pass, which observes
// the moves this one made, plans it simply. While an item's move waits so, a
// delete on that side waits while any work held for a later pass, that move
// or a create waiting with it, has its source at or below the deleted path
// (withholdDeletesOfHeldWork). An item is decided after the item its
// target hangs off, which is shallower, and, when its target lies at or below
// the path a folder's move vacates, after that folder (vacatingFolder), an
// ancestor of its source, and, as a file OneDrive moves, after every folder
// OneDrive relocates (waitsForVacatingFolder); each of these waits ends with
// the decision of the item waited for. An item also waits while another item
// holds its destination, whatever that item does (blockingPendingMove): until
// a move of the plan vacates the destination, parkOneOccupancyCycle parks an
// item of a swap or rotation, or the final sweep of emitRun defers the waiting
// item; a move that waits for the item at its folder's path on OneDrive is
// deferred at once instead (deferRefusedMove). So the pending items are
// decided in turn unless their waits loop: the final sweep defers every item
// on a loop, so the pass still ends. The loop of a folder whose target hangs
// off an entry that lands below the path the folder's move vacates is broken
// by moving the folder aside first (parkOneVacatingLoop), and the loop of an
// entry OneDrive moved into a folder that then took the entry's path, by
// moving the entry here into the folder where the folder is now
// (moveOneIntoItsWaitingFolder). Nothing breaks that
// of two files each
// moved here into a new folder at the other's path, nor a ring of such moves
// through a file: every later pass defers their items again, and a one-shot
// sync ends with them unfinished, after a pass that does nothing else or at
// its pass limit. The loop of a folder whose
// destination is held by a file that waits for it, as one of the folders
// OneDrive relocates, lasts until the move that vacates the file's target
// commits: a pass in which the file lands where no move vacates decides both.
//
// composeMoves is deterministic and performs no I/O.

// moveCoord names one of the three coordinate systems an item has a path in.
type moveCoord int

const (
	moveCoordBaseline moveCoord = iota
	moveCoordLocal
	moveCoordRemote
	moveCoordCount
)

// moveNode is one synced item as composition sees it: its baseline path and,
// when SQLite proved them, its current local and remote paths ("" = not
// present on that side as this item).
type moveNode struct {
	key      string
	itemType syncdomain.ItemType
	paths    [moveCoordCount]string
	// displace says what a move of another item onto this node's baseline
	// path may do with the entry there (D-058): this item's own copy once
	// OneDrive no longer holds it, or, when the node has no local position or
	// sits elsewhere here, the new file that took its baseline path
	// (localDisplacementAt).
	displace moveDisplacement
}

// moveDisplacement is how an entry at a local move's destination gives way.
type moveDisplacement int

const (
	// moveDisplaceNever: the entry is a live synced item; the move waits.
	moveDisplaceNever moveDisplacement = iota
	// moveDisplaceReplace: the item is gone online and its local copy is the
	// synced version, verified again (and kept in the trash) when replaced.
	moveDisplaceReplace
	// moveDisplaceConflictCopy: gone online but edited locally, or a new
	// file took its path; that file is renamed to a conflict copy first and
	// uploaded as a new file.
	moveDisplaceConflictCopy
	// moveDisplaceDeleteFirst: a synced folder OneDrive deleted, which the
	// plan deletes here too (deletedHereByThePlan): the move waits for that
	// delete, whose own checks keep the folder when it holds anything not as
	// synced, and the move then lands on the freed path.
	moveDisplaceDeleteFirst
)

func (n *moveNode) present(coord moveCoord) bool { return n.paths[coord] != "" }

// moveLocation is where an item sits: under a parent (a node key, a
// placeholder for a directory that is not a synced item, or "" for the sync
// root) with a name.
type moveLocation struct {
	parent string
	name   string
}

// unsyncedParentPrefix marks a parent that is a directory without a baseline
// row (a folder created on one side). Item IDs never contain NUL.
const unsyncedParentPrefix = "\x00"

type moveTargetOrigin int

const (
	moveTargetFromBaseline moveTargetOrigin = iota
	moveTargetFromLocal
	moveTargetFromRemote
	moveTargetAgreed
)

// composedMove is one emitted move. side is moveCoordLocal for a rename on
// disk (ActionLocalMove) and moveCoordRemote for a Graph move
// (ActionRemoteMove). For a remote move, localPath is where the item sits in
// the local snapshot, which is what freshness reads.
type composedMove struct {
	key       string
	side      moveCoord
	oldPath   string
	newPath   string
	localPath string
	// displaced is what happens to the entry the move lands on locally.
	displaced moveDisplacement
	// remotePath is set for a local move that runs before an ancestor's local
	// move: OneDrive holds the item at its final path, not at newPath.
	remotePath string
}

// composedRekey re-keys an item's baseline row to where both sides already
// hold it.
type composedRekey struct {
	key     string
	oldPath string
	newPath string
}

type moveNoticeKind int

const (
	moveNoticeDivergentRename moveNoticeKind = iota
	moveNoticeCycle
	moveNoticeCollision
	moveNoticeDeferred
)

// moveNotice reports a decision the user may want to know about: a local
// rename that lost to OneDrive's, or a move held for a later pass.
type moveNotice struct {
	kind         moveNoticeKind
	key          string
	baselinePath string
	localPath    string
	remotePath   string
	targetPath   string
}

type moveComposition struct {
	moves   []composedMove
	rekeys  []composedRekey
	notices []moveNotice
	// deferred holds the keys of relocated items whose moves wait for a
	// later pass.
	deferred map[string]bool
	// deferredMoves holds, for each deferred item, the moves its relocation
	// still needs: ActionLocalMove for this computer, ActionRemoteMove for
	// OneDrive, none when only the baseline is behind (a re-key).
	deferredMoves map[string][]syncdomain.ActionType
	// ownLandings holds, per side and under OneDrive's name folding
	// (namekey.OfPath), the path of each new folder a deferred item moves into
	// at its own path there (holdOwnLandings): a later pass makes it, once
	// the item has left.
	ownLandings map[moveCoord]map[namekey.PathKey]bool
	// asideFolders holds the local paths of new folders made here that hold
	// the source of a folder whose local move lands on their path
	// (newFolderHoldingItsSource): each is renamed to a conflict copy this
	// pass, and the folder moves out of it the next.
	asideFolders []string
}

// deferredRelocationsInMode counts the deferred relocations mode runs: one
// with a move the mode admits, or a re-key, which every mode runs. A move a
// one-way mode holds back waits for no later pass of that mode.
func (c *moveComposition) deferredRelocationsInMode(mode syncdomain.SyncMode) int {
	count := 0
	for key, moves := range c.deferredMoves {
		if len(moves) == 0 || c.holdsMoveInMode(key, mode) {
			count++
		}
	}

	return count
}

// holdsMoveInMode reports whether key's relocation has a move mode admits
// that waits for a later pass.
func (c *moveComposition) holdsMoveInMode(key string, mode syncdomain.SyncMode) bool {
	for _, moveType := range c.deferredMoves[key] {
		if actionAllowedInMode(&syncdomain.Action{Type: moveType}, mode) {
			return true
		}
	}

	return false
}

// moveOccupancy reports the paths each side's snapshot holds, so a move is
// never emitted onto an entry that is not one of the composed items.
type moveOccupancy struct {
	local  map[string]bool
	remote map[string]bool
	// remoteFiles holds the remote paths that are files, which nothing can
	// be created under.
	remoteFiles map[string]bool
	// localFiles holds the local paths that are files: a new one at a local
	// move's destination is renamed aside as a conflict copy
	// (localDisplacementAt).
	localFiles map[string]bool
}

type moveComposer struct {
	nodes     map[string]*moveNode
	keys      []string
	byPath    [moveCoordCount]map[string]string
	target    map[string]moveLocation
	origin    map[string]moveTargetOrigin
	pathMemo  map[string]string
	occupancy moveOccupancy
	notices   []moveNotice
	// unresolvable holds items whose target could not be made consistent
	// (defensive: undoing local relocations always yields the remote tree).
	// They keep their baseline path for composition and are deferred.
	unresolvable map[string]bool
}

// composeMoves is the move composition stage. nodes must hold every synced
// item that is present on both sides (moved or not), so parents resolve; only
// relocated items produce moves.
func composeMoves(nodes []moveNode, occupancy moveOccupancy, frozen map[string]bool) moveComposition {
	c := &moveComposer{
		nodes:        make(map[string]*moveNode, len(nodes)),
		target:       make(map[string]moveLocation, len(nodes)),
		origin:       make(map[string]moveTargetOrigin, len(nodes)),
		occupancy:    occupancy,
		unresolvable: make(map[string]bool, len(frozen)),
	}
	for key := range frozen {
		c.unresolvable[key] = true
	}
	for coord := range moveCoordCount {
		c.byPath[coord] = make(map[string]string, len(nodes))
	}
	for i := range nodes {
		n := nodes[i]
		if n.key == "" {
			continue
		}
		c.nodes[n.key] = &n
		c.keys = append(c.keys, n.key)
		for coord := range moveCoordCount {
			if n.present(coord) {
				c.byPath[coord][n.paths[coord]] = n.key
			}
		}
	}
	sort.Slice(c.keys, func(i, j int) bool {
		return c.nodes[c.keys[i]].paths[moveCoordBaseline] < c.nodes[c.keys[j]].paths[moveCoordBaseline]
	})

	for _, key := range c.keys {
		c.chooseTarget(c.nodes[key])
	}
	c.resolveConflicts()

	return c.emit()
}

func (c *moveComposer) location(n *moveNode, coord moveCoord) moveLocation {
	p := n.paths[coord]
	dir := path.Dir(p)
	if dir == "." || dir == "/" {
		return moveLocation{name: path.Base(p)}
	}
	if key, ok := c.byPath[coord][dir]; ok {
		return moveLocation{parent: key, name: path.Base(p)}
	}

	return moveLocation{parent: unsyncedParentPrefix + string(rune('0'+coord)) + dir, name: path.Base(p)}
}

// sameLocation compares locations across coordinates. Two unsynced parents
// are the same directory when their paths match: a folder both sides created
// at one path is one folder.
func sameLocation(a, b moveLocation) bool {
	if a.name != b.name {
		return false
	}
	if strings.HasPrefix(a.parent, unsyncedParentPrefix) && strings.HasPrefix(b.parent, unsyncedParentPrefix) {
		return unsyncedParentDir(a.parent) == unsyncedParentDir(b.parent)
	}

	return a.parent == b.parent
}

func unsyncedParentDir(parent string) string {
	return parent[len(unsyncedParentPrefix)+1:]
}

func unsyncedParentCoord(parent string) moveCoord {
	return moveCoord(parent[len(unsyncedParentPrefix)] - '0')
}

func (c *moveComposer) isMoveParticipant(n *moveNode) bool {
	return n.present(moveCoordLocal) && n.present(moveCoordRemote)
}

func (c *moveComposer) chooseTarget(n *moveNode) {
	baseline := c.location(n, moveCoordBaseline)
	if !c.isMoveParticipant(n) {
		c.target[n.key] = baseline
		c.origin[n.key] = moveTargetFromBaseline
		return
	}
	local := c.location(n, moveCoordLocal)
	remote := c.location(n, moveCoordRemote)
	localRelocated := !sameLocation(local, baseline)
	remoteRelocated := !sameLocation(remote, baseline)

	switch {
	case !localRelocated && !remoteRelocated:
		c.target[n.key], c.origin[n.key] = baseline, moveTargetFromBaseline
	case localRelocated && !remoteRelocated:
		c.target[n.key], c.origin[n.key] = local, moveTargetFromLocal
	case !localRelocated && remoteRelocated:
		c.target[n.key], c.origin[n.key] = remote, moveTargetFromRemote
	case sameLocation(local, remote):
		c.target[n.key], c.origin[n.key] = remote, moveTargetAgreed
	default:
		c.target[n.key], c.origin[n.key] = remote, moveTargetFromRemote
		c.notices = append(c.notices, c.notice(moveNoticeDivergentRename, n))
	}
}

// resolveConflicts undoes local relocations until the target tree is a tree
// with unique paths. Following only remote and baseline choices is the remote
// tree itself, so the loop ends.
func (c *moveComposer) resolveConflicts() {
	for range len(c.keys) + 1 {
		c.pathMemo = make(map[string]string, len(c.keys))
		if cycle := c.findTargetCycle(); len(cycle) > 0 {
			if !c.undoLocalRelocations(cycle, moveNoticeCycle) {
				c.markUnresolvable(cycle)
			}
			continue
		}
		if collision := c.findTargetCollision(); len(collision) > 0 {
			if !c.undoLocalRelocations(collision, moveNoticeCollision) {
				c.markUnresolvable(collision)
			}
			continue
		}

		return
	}
}

func (c *moveComposer) markUnresolvable(keys []string) {
	for _, key := range keys {
		if key != remoteOccupantClaim {
			c.unresolvable[key] = true
		}
	}
}

func (c *moveComposer) undoLocalRelocations(keys []string, kind moveNoticeKind) bool {
	undone := false
	for _, key := range keys {
		if key == remoteOccupantClaim || c.origin[key] != moveTargetFromLocal {
			continue
		}
		n := c.nodes[key]
		c.target[key] = c.location(n, moveCoordRemote)
		c.origin[key] = moveTargetFromRemote
		c.notices = append(c.notices, c.notice(kind, n))
		undone = true
	}

	return undone
}

// targetParentNode is the node an item's target position hangs off: its
// target parent, or for an unsynced parent directory the nearest synced
// ancestor of that directory on the side that holds it.
func (c *moveComposer) targetParentNode(key string) string {
	if c.unresolvable[key] {
		return ""
	}
	parent := c.target[key].parent
	if !strings.HasPrefix(parent, unsyncedParentPrefix) {
		return parent
	}
	anchor, _ := c.unsyncedAnchor(parent)

	return anchor
}

// unsyncedAnchor finds the deepest synced ancestor of an unsynced directory
// and the directory's path below it.
func (c *moveComposer) unsyncedAnchor(parent string) (string, string) {
	coord := unsyncedParentCoord(parent)
	dir := unsyncedParentDir(parent)
	for prefix := path.Dir(dir); prefix != "." && prefix != "/"; prefix = path.Dir(prefix) {
		if key, ok := c.byPath[coord][prefix]; ok {
			return key, strings.TrimPrefix(dir, prefix+"/")
		}
	}

	return "", dir
}

func (c *moveComposer) findTargetCycle() []string {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(c.keys))
	for _, start := range c.keys {
		if state[start] != unvisited {
			continue
		}
		var stack []string
		for key := start; key != ""; key = c.targetParentNode(key) {
			if _, known := c.nodes[key]; !known {
				break
			}
			if state[key] == done {
				break
			}
			if state[key] == visiting {
				for i := range stack {
					if stack[i] == key {
						return append([]string(nil), stack[i:]...)
					}
				}
				break
			}
			state[key] = visiting
			stack = append(stack, key)
		}
		for _, key := range stack {
			state[key] = done
		}
	}

	return nil
}

// targetPath composes an item's target path. Callers ensure the target tree
// is acyclic first.
func (c *moveComposer) targetPath(key string) string {
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, unsyncedParentPrefix) {
		anchor, rest := c.unsyncedAnchor(key)
		return joinMovePath(c.targetPath(anchor), rest)
	}
	if p, ok := c.pathMemo[key]; ok {
		return p
	}
	if c.unresolvable[key] {
		return c.nodes[key].paths[moveCoordBaseline]
	}
	loc := c.target[key]
	p := joinMovePath(c.targetPath(loc.parent), loc.name)
	c.pathMemo[key] = p

	return p
}

func joinMovePath(dir, name string) string {
	if dir == "" {
		return name
	}

	return dir + "/" + name
}

// remoteOccupantClaim marks a collision group's claim by an OneDrive entry
// that is not a composed item (a file created online). It is never undone.
const remoteOccupantClaim = unsyncedParentPrefix + "occupant"

func (c *moveComposer) findTargetCollision() []string {
	byTarget := make(map[namekey.PathKey][]string, len(c.keys))
	hasLocalOrigin := false
	for _, key := range c.keys {
		// An item present on one side only is a create or delete SQLite
		// plans; where it sits is checked as occupancy when a move lands on
		// it, not as a competing claim.
		if c.unresolvable[key] || !c.isMoveParticipant(c.nodes[key]) {
			continue
		}
		folded := namekey.OfPath(c.targetPath(key))
		byTarget[folded] = append(byTarget[folded], key)
		hasLocalOrigin = hasLocalOrigin || c.origin[key] == moveTargetFromLocal
	}
	if hasLocalOrigin {
		// A local move composed through a parent can land where OneDrive
		// created something new (the user renamed a file to b.txt inside a
		// folder OneDrive moved, and OneDrive gained its own b.txt there).
		// SQLite checks only the direct destination.
		for occupied := range c.occupancy.remote {
			if _, isNode := c.byPath[moveCoordRemote][occupied]; isNode {
				continue
			}
			folded := namekey.OfPath(c.remoteTargetPath(occupied))
			if claims, ok := byTarget[folded]; ok {
				byTarget[folded] = append(claims, remoteOccupantClaim)
			}
		}
	}
	folded := make([]namekey.PathKey, 0, len(byTarget))
	for p := range byTarget {
		folded = append(folded, p)
	}
	slices.Sort(folded)
	for _, p := range folded {
		if len(byTarget[p]) > 1 {
			return byTarget[p]
		}
	}

	return nil
}

// remoteTargetPath is where a remote path that is not a composed item ends up:
// below its deepest composed remote ancestor's target.
func (c *moveComposer) remoteTargetPath(remotePath string) string {
	for prefix := path.Dir(remotePath); prefix != "." && prefix != "/"; prefix = path.Dir(prefix) {
		if key, ok := c.byPath[moveCoordRemote][prefix]; ok {
			return joinMovePath(c.targetPath(key), strings.TrimPrefix(remotePath, prefix+"/"))
		}
	}

	return remotePath
}

func (c *moveComposer) notice(kind moveNoticeKind, n *moveNode) moveNotice {
	return moveNotice{
		kind:         kind,
		key:          n.key,
		baselinePath: n.paths[moveCoordBaseline],
		localPath:    n.paths[moveCoordLocal],
		remotePath:   n.paths[moveCoordRemote],
	}
}

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
		if e.movedOnOneDrive(holder) || e.isLoopPartner(key, holder) || parkedNow[holder] {
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
// OneDrive: p moved into x, then x moved to p (or x moved into p and p to
// x, or p/f moved into x and x onto p/f). Here the entry still holds the
// path the folder's move lands on, so the folder waits for it to leave
// (blockingPendingMove), and the entry, whose target hangs off the folder,
// waits for the folder to be decided: both stayed pending and were deferred
// on every pass. OneDrive moves by item ID, so the same rotation made here
// converges; on this computer the entry moves first into the folder at the
// path the folder has now, which vacates the folder's destination, and the
// folder's move then carries the entry to its target in the same plan. The
// entry's move is progress whether or not the folder's move runs: it puts
// the entry under its target parent with its target name. It reports
// whether it planned such a move.
func (e *moveEmitter) moveOneIntoItsWaitingFolder(order []string) bool {
	for _, key := range order {
		if move, ok := e.moveIntoWaitingFolder(key); ok {
			e.result.moves = append(e.result.moves, move)
			e.state[key] = moveEmitSettled

			return true
		}
	}

	return false
}

// moveIntoWaitingFolder builds key's local move into its target folder at
// that folder's current path here, when the folder waits for key alone
// (waitsOnlyForItsFolder). No move of the plan may carry key's source, the
// folder or the destination, so the move runs in snapshot coordinates.
func (e *moveEmitter) moveIntoWaitingFolder(key string) (composedMove, bool) {
	folderKey, ok := e.waitsOnlyForItsFolder(key)
	if !ok {
		return composedMove{}, false
	}
	n := e.nodes[key]
	source, folderPath := n.paths[moveCoordLocal], e.nodes[folderKey].paths[moveCoordLocal]
	if pathAtOrBelow(folderPath, source) {
		return composedMove{}, false
	}
	newPath := joinMovePath(folderPath, e.target[key].name)
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if earlier.side == moveCoordLocal && (pathAtOrBelow(source, earlier.oldPath) ||
			pathAtOrBelow(folderPath, earlier.oldPath) || pathAtOrBelow(newPath, earlier.newPath)) {
			return composedMove{}, false
		}
	}
	if free, displaced := e.destinationFree(n, moveCoordLocal, newPath); !free || displaced != moveDisplaceNever {
		return composedMove{}, false
	}

	return composedMove{
		key:        key,
		side:       moveCoordLocal,
		oldPath:    source,
		newPath:    newPath,
		localPath:  source,
		remotePath: n.paths[moveCoordRemote],
	}, true
}

// waitsOnlyForItsFolder reports key's target folder when key is pending and
// relocated here only (OneDrive keeps it at its target), and that folder, a
// pending folder present here, waits for key alone: key holds the folder's
// local destination (blockingPendingMove).
func (e *moveEmitter) waitsOnlyForItsFolder(key string) (string, bool) {
	n := e.nodes[key]
	if e.state[key] != moveEmitPending || e.unresolvable[key] || e.takenBack[key] || !e.isMoveParticipant(n) ||
		sameLocation(e.location(n, moveCoordLocal), e.target[key]) ||
		!sameLocation(e.location(n, moveCoordRemote), e.target[key]) {
		return "", false
	}
	folderKey := e.target[key].parent
	folder, known := e.nodes[folderKey]
	if !known || e.state[folderKey] != moveEmitPending || e.unresolvable[folderKey] ||
		folder.itemType != syncdomain.ItemTypeFolder || !folder.present(moveCoordLocal) {
		return "", false
	}
	blocker, blocked := e.blockingPendingMove(folderKey, moveCoordLocal)

	return folderKey, blocked && blocker == key
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
	move, ok := e.planMove(n, side, newPath)
	if !ok {
		return e.deferRefusedMove(n.key, side)
	}
	e.result.moves = append(e.result.moves, move)
	e.state[n.key] = moveEmitSettled

	return true
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
		if before, lands := e.landsInsideBeforeItMoves(n, side, oldPath, newPath, earlier); lands {
			newPath = before
			remotePath = n.paths[moveCoordRemote]
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
	if e.waitsForAFolderHolder(n.key, side, newPath) {
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
		!strings.HasPrefix(newPath, earlier.newPath+"/") || pathAtOrBelow(oldPath, earlier.oldPath) {
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
// only (one SQLite deletes or re-creates) ends it with none. With none, or
// with a nearer relocated folder n does not land in, n's move keeps its
// source, and buildDependencies orders it after a folder move whose vacated
// path the plan creates a folder at (movesIntoRecreatedSource), unless that
// nearer folder moves out of the moving one: its own move then waits for n
// to leave it first, a cycle through the folder made there. When the plan
// deletes the one-sided folder instead, that delete waits for the move and
// the folder move for the delete, a cycle the delete waits a pass to break
// (withholdDeletesInsideFolderMoveCycles). When n lands on the vacated path
// itself or in a folder moved onto it, n's move and the folder move wait for
// each other: a cycle the planner drops on every pass (sync-planning.md, Move
// Composition).
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
	}

	return false, moveDisplaceNever
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

// moveCompositionInputs adapts the SQLite comparison rows to composition
// nodes. Positions come only from SQLite's pairing, so composition never
// moves an item SQLite did not prove is the same item:
//   - local: the unique identity-paired destination of a local move, or the
//     baseline path while it still holds an entry of the same type;
//   - remote: the item's remote path when it is the baseline path, the paired
//     remote-move destination, or a destination shadowed by (or equal to) the
//     local move.
//
// A node whose paths touch unavailable truth is frozen: it keeps its baseline
// position and nothing below it moves this pass.
type moveInputs struct {
	nodes     []moveNode
	occupancy moveOccupancy
	frozen    map[string]bool
	// recreated maps an item's key to the baseline path it moved away from
	// while a new local entry took that path (rename and recreate, D-056).
	recreated map[string]string
	// filesByContent locates each file of the snapshots by the key a delete
	// of its synced content carries (contentKey): a file here by its local
	// identity, an item on OneDrive by its ID (withholdDeletesOfUncarriedContent).
	filesByContent map[contentKey]string
}

func moveCompositionInputs(
	comparisons []syncdomain.SQLiteComparisonRow,
	views map[string]*syncdomain.PathView,
	remoteRows []syncdomain.RemoteStateRow,
	localRows []syncdomain.LocalStateRow,
) moveInputs {
	cmpByPath := make(map[string]*syncdomain.SQLiteComparisonRow, len(comparisons))
	for i := range comparisons {
		cmpByPath[comparisons[i].Path] = &comparisons[i]
	}
	remoteByID, occupancy := indexMoveSnapshot(remoteRows, localRows)

	var nodes []moveNode
	frozen := map[string]bool{}
	recreated := map[string]string{}
	for i := range comparisons {
		cmp := &comparisons[i]
		view := views[cmp.Path]
		if !cmp.BaselineType.Present() || view == nil || view.Baseline == nil || view.Baseline.ItemID == "" {
			continue
		}
		entry := view.Baseline
		node := moveNode{key: entry.ItemID, itemType: entry.ItemType}
		node.paths[moveCoordBaseline] = cmp.Path
		node.paths[moveCoordLocal] = composedLocalPosition(cmp, view)
		node.paths[moveCoordRemote] = composedRemotePosition(cmp, entry, remoteByID, cmpByPath, node.paths[moveCoordLocal])
		node.displace = composedDisplacement(&node, entry, view, remoteByID)
		if moveNodeTouchesUnavailableTruth(&node, views) {
			frozen[node.key] = true
		}
		if cmp.ComparisonKind == syncdomain.ComparisonLocalMoveSource && cmp.LocalType.Present() && cmp.LocalMoveTarget != "" {
			recreated[node.key] = cmp.Path
		}
		nodes = append(nodes, node)
	}

	return moveInputs{
		nodes: nodes, occupancy: occupancy, frozen: frozen, recreated: recreated,
		filesByContent: filesByContent(remoteRows, localRows),
	}
}

func indexMoveSnapshot(
	remoteRows []syncdomain.RemoteStateRow, localRows []syncdomain.LocalStateRow,
) (map[string]*syncdomain.RemoteStateRow, moveOccupancy) {
	remoteByID := make(map[string]*syncdomain.RemoteStateRow, len(remoteRows))
	occupancy := moveOccupancy{
		local:       make(map[string]bool, len(localRows)),
		remote:      make(map[string]bool, len(remoteRows)),
		remoteFiles: make(map[string]bool, len(remoteRows)),
		localFiles:  make(map[string]bool, len(localRows)),
	}
	for i := range remoteRows {
		remoteByID[remoteRows[i].ItemID] = &remoteRows[i]
		occupancy.remote[remoteRows[i].Path] = true
		if remoteRows[i].ItemType != syncdomain.ItemTypeFolder {
			occupancy.remoteFiles[remoteRows[i].Path] = true
		}
	}
	for i := range localRows {
		occupancy.local[localRows[i].Path] = true
		if localRows[i].ItemType == syncdomain.ItemTypeFile {
			occupancy.localFiles[localRows[i].Path] = true
		}
	}

	return remoteByID, occupancy
}

func moveNodeTouchesUnavailableTruth(node *moveNode, views map[string]*syncdomain.PathView) bool {
	for coord := range moveCoordCount {
		if p := node.paths[coord]; p != "" && views[p] != nil && plannerSuppressesUnavailableTruth(&views[p].TruthStatus) {
			return true
		}
	}

	return false
}

// composedDisplacement decides whether the entry at the baseline path of a
// synced file may give way to another item moved onto that path. The file's
// own local copy is replaced when it is provably the synced version and
// renamed to a conflict copy when it was edited, both only once OneDrive no
// longer has the item. A new file that took the path (the entry there is not
// the baseline item: the node has no local position, or the synced copy moved
// elsewhere here) is renamed to a conflict copy too, whatever OneDrive holds:
// it is kept and uploaded later as a new file, and the move still lands, so
// the moved item is neither downloaded beside its old copy nor uploaded
// again. Never otherwise: a folder at either end, unknown content, or the
// item's own copy while it still exists online somewhere. When the synced
// copy moved elsewhere here, the displacement is the new entry's only: the
// node's own copy, at its new path, never gives way (localDisplacementAt).
func composedDisplacement(
	node *moveNode, entry *syncdomain.BaselineEntry, view *syncdomain.PathView, remoteByID map[string]*syncdomain.RemoteStateRow,
) moveDisplacement {
	if entry.ItemType != syncdomain.ItemTypeFile || view.Local == nil {
		return moveDisplaceNever
	}
	if movedAwayHere(node) {
		// The synced copy left the path here and a new file took it: that
		// file is not the item, whatever OneDrive holds, so it is kept as a
		// conflict copy. Waiting for it to leave left two moves waiting on
		// each other's destinations for good (chaos seeds 162 and 385).
		if view.Local.ItemType == syncdomain.ItemTypeFile {
			return moveDisplaceConflictCopy
		}
		return moveDisplaceNever
	}
	if _, stillOnline := remoteByID[entry.ItemID]; stillOnline {
		return moveDisplaceNever
	}
	if !node.present(moveCoordLocal) {
		if view.Local.ItemType == syncdomain.ItemTypeFile {
			return moveDisplaceConflictCopy
		}
		return moveDisplaceNever
	}
	if view.Local.Hash == "" || entry.LocalHash == "" {
		return moveDisplaceNever
	}
	if view.Local.Hash != entry.LocalHash {
		return moveDisplaceConflictCopy
	}

	return moveDisplaceReplace
}

func composedLocalPosition(cmp *syncdomain.SQLiteComparisonRow, view *syncdomain.PathView) string {
	if cmp.ComparisonKind == syncdomain.ComparisonLocalMoveSource && cmp.LocalMoveTarget != "" {
		return cmp.LocalMoveTarget
	}
	if view.Local != nil && localEntryIsBaselineItem(view.Local, view.Baseline) {
		return cmp.Path
	}

	return ""
}

// localEntryIsBaselineItem applies the source check a local move's freshness
// and executor make (LocalRowIsBaselineItem) to a planner view, so
// composition never plans a move of an entry they would refuse: a file
// replaced at the path by a different one (new identity, other content) is a
// new local entry, and its move was superseded on every pass.
func localEntryIsBaselineItem(local *syncdomain.LocalState, baseline *syncdomain.BaselineEntry) bool {
	return syncdomain.LocalRowIsBaselineItem(&syncdomain.LocalStateRow{
		ItemType:         local.ItemType,
		Hash:             local.Hash,
		Size:             local.Size,
		LocalDevice:      local.LocalDevice,
		LocalInode:       local.LocalInode,
		LocalBirthTime:   local.LocalBirthTime,
		LocalHasIdentity: local.LocalHasIdentity,
	}, baseline)
}

func composedRemotePosition(
	cmp *syncdomain.SQLiteComparisonRow,
	entry *syncdomain.BaselineEntry,
	remoteByID map[string]*syncdomain.RemoteStateRow,
	cmpByPath map[string]*syncdomain.SQLiteComparisonRow,
	localPosition string,
) string {
	row, ok := remoteByID[entry.ItemID]
	if !ok || row.ItemType != entry.ItemType {
		return ""
	}
	if row.Path == cmp.Path || row.Path == cmp.RemoteMoveTarget {
		return row.Path
	}
	at := cmpByPath[row.Path]
	if at == nil {
		return ""
	}
	switch {
	case at.ComparisonKind == syncdomain.ComparisonRemoteMoveShadowed:
		return row.Path
	// The move OneDrive made is shadowed by this item's local move, and a
	// new local file took its destination here (z moved into p here and to
	// g on OneDrive, a new g made here): the path is a create conflict, but
	// the item ID proves the item sits there, so the moves compose and the
	// new file is kept as a conflict copy (localDisplacementAt). Without it
	// the item was downloaded at the path beside its moved copy here. Only
	// a file meeting a new file: two folders at one path merge as a create
	// conflict of folders, and a folder composed so would undo that merge.
	// Only a shadowed move: one OneDrive made alone, SQLite reconciles as a
	// delete here at the item's path and a create conflict at the new one,
	// which no composed move takes back.
	case at.RemoteMoveShadowed && !at.BaselineType.Present() &&
		entry.ItemType == syncdomain.ItemTypeFile && at.LocalType == syncdomain.SideFile:
		return row.Path
	case sitsAtAnotherSyncedItemsPath(at, entry):
		return row.Path
	case at.ComparisonKind == syncdomain.ComparisonLocalMoveDest && row.Path == localPosition:
		return row.Path
	default:
		return ""
	}
}

// sitsAtAnotherSyncedItemsPath reports whether OneDrive holds the item of
// entry at the path of the comparison row at, a path that is another synced
// item's: the item ID proves this item now sits there. It does when
// OneDrive moved the path's own item away (a swap or a chain), which is
// composed too; without that a local rename of an item OneDrive swapped had
// no remote position and was copied, not moved. It does when the path's own
// item is gone here (deleted here, or moved away from a swap with this one)
// and no entry here holds the path; without that the item was downloaded at
// the path and its renamed copy here uploaded as a new file, one content at
// two paths, or, where OneDrive still held the path's own item elsewhere,
// the download waited as a removal of that item's content and sync never
// converged (chaos seed 55).
func sitsAtAnotherSyncedItemsPath(at *syncdomain.SQLiteComparisonRow, entry *syncdomain.BaselineEntry) bool {
	if at.RemoteItemID != entry.ItemID {
		return false
	}

	return at.ComparisonKind == syncdomain.ComparisonRemoteMoveSource ||
		(at.BaselineType.Present() && !at.LocalType.Present())
}

// remoteToLocalFolderPrefixes maps each composed folder's remote path to where
// the same folder sits locally now, deepest first. Remote-origin local writes
// carry remote paths; rebased through this map they address the local tree as
// it is, and moves wait for writes under their source (buildDependencies).
func remoteToLocalFolderPrefixes(nodes []moveNode) []graphFolderMoveBoundary {
	var prefixes []graphFolderMoveBoundary
	for i := range nodes {
		n := &nodes[i]
		if n.itemType != syncdomain.ItemTypeFolder || !n.present(moveCoordLocal) || !n.present(moveCoordRemote) {
			continue
		}
		if n.paths[moveCoordLocal] == n.paths[moveCoordRemote] {
			continue
		}
		prefixes = append(prefixes, graphFolderMoveBoundary{
			oldPrefix: n.paths[moveCoordRemote] + "/",
			newPrefix: n.paths[moveCoordLocal] + "/",
		})
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i].oldPrefix) > len(prefixes[j].oldPrefix)
	})

	return prefixes
}

// buildComposedMoveActions turns composed moves and re-keys into actions. It
// also returns the creates that wait with a move held for a later pass
// (recreatedSourceCreates).
func buildComposedMoveActions(
	composition *moveComposition,
	inputs *moveInputs,
	views map[string]*syncdomain.PathView,
	baseline syncdomain.BaselineView,
) (actions, waiting []syncdomain.Action) {
	nodes := inputs.nodes
	nodeByKey := make(map[string]*moveNode, len(nodes))
	for i := range nodes {
		nodeByKey[nodes[i].key] = &nodes[i]
	}
	viewAt := func(p string) *syncdomain.PathView {
		if v := views[p]; v != nil {
			return v
		}
		return &syncdomain.PathView{Path: p}
	}

	for i := range composition.moves {
		move := &composition.moves[i]
		node := nodeByKey[move.key]
		entry, ok := baseline.GetByID(move.key)
		if node == nil || !ok {
			continue
		}
		remote := viewAt(node.paths[moveCoordRemote]).Remote
		if move.side == moveCoordLocal {
			actions = append(actions, composedLocalMoveActions(move, &entry, remote, viewAt)...)
			continue
		}
		actions = append(actions, composedRemoteMoveActions(move, &entry, remote, viewAt)...)
	}

	creates, waiting := recreatedSourceCreates(composition, inputs, nodeByKey, viewAt)
	actions = append(actions, creates...)
	for _, aside := range composition.asideFolders {
		copyView := *viewAt(aside)
		actions = append(actions, makeConflictCopyAction(&copyView))
	}

	for i := range composition.rekeys {
		rekey := &composition.rekeys[i]
		node := nodeByKey[rekey.key]
		entry, ok := baseline.GetByID(rekey.key)
		if node == nil || !ok {
			continue
		}
		view := &syncdomain.PathView{
			Path:        rekey.newPath,
			Remote:      viewAt(node.paths[moveCoordRemote]).Remote,
			Local:       viewAt(node.paths[moveCoordLocal]).Local,
			Baseline:    &entry,
			TruthStatus: viewAt(rekey.newPath).TruthStatus,
		}
		action := makeAction(syncdomain.ActionBaselineUpdate, view)
		action.OldPath = rekey.oldPath
		actions = append(actions, action)
	}

	return actions, waiting
}

// composedLocalMoveActions builds a remote-origin local move, preceded by the
// conflict copy of an edited file it displaces (buildDependencies orders the
// move after the copy at its destination).
func composedLocalMoveActions(
	move *composedMove, entry *syncdomain.BaselineEntry, remote *syncdomain.RemoteState, viewAt func(string) *syncdomain.PathView,
) []syncdomain.Action {
	view := &syncdomain.PathView{
		Path:        move.newPath,
		Remote:      remote,
		Local:       viewAt(move.newPath).Local,
		Baseline:    entry,
		TruthStatus: viewAt(move.newPath).TruthStatus,
	}
	action := makeAction(syncdomain.ActionLocalMove, view)
	action.OldPath = move.oldPath
	action.MovedRemotePath = move.remotePath

	switch move.displaced {
	case moveDisplaceDeleteFirst:
		// The move waits for the delete at its destination
		// (moveOrderDependency) and lands where nothing is any more; its
		// view keeps the folder there as planned, which the worker's
		// freshness check reads in the snapshot.
	case moveDisplaceReplace:
		action.ReplaceLocalTarget = true
	case moveDisplaceConflictCopy:
		copyView := *viewAt(move.newPath)
		return []syncdomain.Action{makeConflictCopyAction(&copyView), action}
	case moveDisplaceNever:
	}

	return []syncdomain.Action{action}
}

// composedRemoteMoveActions builds a local-origin remote move. A move whose
// content could not be proven unchanged (no hash) also uploads what now sits
// at the destination, but only when the item is already there locally: the
// upload reads local truth at its path.
func composedRemoteMoveActions(
	move *composedMove, entry *syncdomain.BaselineEntry, remote *syncdomain.RemoteState, viewAt func(string) *syncdomain.PathView,
) []syncdomain.Action {
	movedLocal := viewAt(move.localPath).Local
	view := &syncdomain.PathView{
		Path:        move.oldPath,
		Remote:      remote,
		Local:       viewAt(move.oldPath).Local,
		Baseline:    entry,
		TruthStatus: viewAt(move.oldPath).TruthStatus,
	}
	action := makeAction(syncdomain.ActionRemoteMove, view)
	action.OldPath = move.oldPath
	action.Path = move.newPath
	action.MovedLocal = movedLocal
	if move.localPath != move.newPath {
		action.MovedLocalPath = move.localPath
	}
	actions := []syncdomain.Action{action}

	if move.localPath == move.newPath && syncdomain.LocalFileDiffersFromBaseline(movedLocal, entry) {
		uploadView := &syncdomain.PathView{
			Path:        move.newPath,
			Local:       movedLocal,
			Remote:      remote,
			Baseline:    entry,
			TruthStatus: viewAt(move.newPath).TruthStatus,
		}
		actions = append(actions, makeAction(syncdomain.ActionUpload, uploadView))
	}

	return actions
}

// recreatedSourceCreates plans the new local entry at a path the synced item
// was renamed away from (D-056). It is created once OneDrive no longer holds
// the moved item there: already moved elsewhere online, or moved by a remote
// move of this plan, which the create waits for (buildDependencies). While
// the move waits for a later pass, so does the create: uploading at the old
// path first would write a new version of the moved item instead. Such a
// create is returned as waiting, work held for a later pass
// (withholdDeletesOfHeldWork).
func recreatedSourceCreates(
	composition *moveComposition,
	inputs *moveInputs,
	nodeByKey map[string]*moveNode,
	viewAt func(string) *syncdomain.PathView,
) (creates, waiting []syncdomain.Action) {
	keys := make([]string, 0, len(inputs.recreated))
	for key := range inputs.recreated {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		source := inputs.recreated[key]
		node := nodeByKey[key]
		sourceView := viewAt(source)
		if node == nil || sourceView.Local == nil || plannerSuppressesUnavailableTruth(&sourceView.TruthStatus) {
			continue
		}
		createView := &syncdomain.PathView{Path: source, Local: sourceView.Local, TruthStatus: sourceView.TruthStatus}
		create := makeCreateUploadAction(createView)
		if sourceView.Local.ItemType == syncdomain.ItemTypeFolder {
			createView.Remote = remoteFolderOfAnotherItem(sourceView.Remote, key)
			create = makeFolderCreate(createView, syncdomain.CreateRemote)
		}
		vacated := recreatedSourceVacated(composition, key, node, source)
		if inputs.frozen[key] || (composition.deferred[key] && !movedAwayOnOneDrive(composition, key, source)) || !vacated {
			waiting = append(waiting, create)
			continue
		}
		creates = append(creates, create)
	}

	return creates, waiting
}

// remoteFolderOfAnotherItem is the folder OneDrive holds at a recreated
// source when it is not the moved item key: both sides moved the item away
// and each made a new folder at its path, which are one folder by path. The
// folder create made here carries it as its OneDrive truth, so the worker's
// freshness check reads that folder by item ID instead of superseding the
// create on every pass because the path is no longer free, and the executor
// adopts it by name (adoptExistingRemoteFolder). Whichever of the create and
// the moved item's move commits first, the baseline ends right: run first,
// the create's row replaces the moved item's at the path and the move
// records the item where it went; run second, the move carries the rows
// below the path with it, and the next pass re-keys the new folder's back.
// A file there, or the moved item itself, is no such folder.
func remoteFolderOfAnotherItem(remote *syncdomain.RemoteState, key string) *syncdomain.RemoteState {
	if remote == nil || remote.IsDeleted || remote.ItemType != syncdomain.ItemTypeFolder || remote.ItemID == key {
		return nil
	}

	return remote
}

// movedAwayOnOneDrive reports whether the plan moves key away from source on
// OneDrive: a deferred item parked there frees the path in this pass
// (parkedFirst), so the folder made here at it is made on OneDrive after the
// move, and the move that lands in it after that.
func movedAwayOnOneDrive(composition *moveComposition, key, source string) bool {
	for i := range composition.moves {
		move := &composition.moves[i]
		if move.key == key && move.side == moveCoordRemote && move.oldPath == source {
			return true
		}
	}

	return false
}

// displaceFoldersDeletedHere marks a synced folder OneDrive deleted as giving
// way to a move onto its path when the plan deletes it here (localDeletes,
// the paths SQLite reconciles as a local delete): the folder is unmoved
// here and gone online. Before, such a folder made the move wait, while the
// moved item was downloaded at the folder's path and its copy here uploaded
// again: OneDrive replacing a deleted folder by a moved file (`rm x, mv z
// x`) left the file twice on both sides.
func displaceFoldersDeletedHere(inputs *moveInputs, localDeletes map[string]bool) {
	for i := range inputs.nodes {
		n := &inputs.nodes[i]
		if n.itemType == syncdomain.ItemTypeFolder && n.displace == moveDisplaceNever && !n.present(moveCoordRemote) &&
			n.paths[moveCoordLocal] == n.paths[moveCoordBaseline] && localDeletes[n.paths[moveCoordBaseline]] {
			n.displace = moveDisplaceDeleteFirst
		}
	}
}

// recreatedSourceVacated reports whether OneDrive no longer holds key at
// source once the plan's moves have run.
func recreatedSourceVacated(composition *moveComposition, key string, node *moveNode, source string) bool {
	return node.paths[moveCoordRemote] != source || movedAwayOnOneDrive(composition, key, source)
}

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
// once the move aside failed.
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
func dropCreatesOntoComposedItems(
	logger *slog.Logger, actions []syncdomain.Action, inputs *moveInputs, composition *moveComposition,
) (kept, dropped []syncdomain.Action) {
	heldBy := pathsHeldByUnmovedItems(inputs, composition)
	vacated := pathsVacatedByPlan(actions)
	heldRebases := heldFolderRebases(actions, inputs, composition)
	nodeByKey := make(map[string]*moveNode, len(inputs.nodes))
	for i := range inputs.nodes {
		nodeByKey[inputs.nodes[i].key] = &inputs.nodes[i]
	}

	kept = actions[:0:0]
	for i := range actions {
		action := &actions[i]
		side, isCreate := createSide(action)
		if !isCreate {
			kept = append(kept, *action)
			continue
		}
		if holder, held := heldBy[side][action.Path]; held && holder != action.ItemID {
			dropped = append(dropped, *action)
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
		if action.Path == syncdomain.ActionViewPath(action) {
			kept = append(kept, *action)
			continue
		}
		if rebasedCreateBlocked(action.Path, side, &inputs.occupancy, vacated[side]) {
			dropped = append(dropped, *action)
			continue
		}
		if side == moveCoordLocal {
			if folder, through := rebasedThroughHeldFolder(action, heldRebases); through {
				logRebasedCreateHeld(logger, action, folder)
				dropped = append(dropped, *action)
				continue
			}
		}
		kept = append(kept, *action)
	}

	return dropCreatesBelowDroppedCreates(kept, dropped)
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
	actions []syncdomain.Action, composition *moveComposition, mode syncdomain.SyncMode,
) []syncdomain.Action {
	moved := map[moveCoord]map[string]bool{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range composition.moves {
		moved[composition.moves[i].side][composition.moves[i].key] = true
	}
	if len(moved[moveCoordLocal]) == 0 && len(moved[moveCoordRemote]) == 0 && len(composition.deferredMoves) == 0 {
		return actions
	}

	kept := actions[:0:0]
	for i := range actions {
		action := &actions[i]
		switch action.Type {
		case syncdomain.ActionBaselineUpdate:
			if moved[moveCoordLocal][action.ItemID] || moved[moveCoordRemote][action.ItemID] ||
				composition.holdsMoveInMode(action.ItemID, mode) {
				continue
			}
		case syncdomain.ActionDownload, syncdomain.ActionConflictCopy:
			if moved[moveCoordLocal][action.ItemID] {
				continue
			}
		case syncdomain.ActionUpload, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete, syncdomain.ActionLocalMove,
			syncdomain.ActionRemoteMove,
			syncdomain.ActionFolderCreate, syncdomain.ActionCleanup:
		default:
			panic(syncdomain.OutsideTheEnum(action.Type))
		}
		kept = append(kept, *action)
	}

	return kept
}
