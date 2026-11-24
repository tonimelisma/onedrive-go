package syncplan

import (
	"path"
	"slices"
	"sort"
	"strings"

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
// the decision of the item waited for. An item whose target lies at or below
// the path a farther folder's move vacates, past a nearer relocated folder it
// does not land below, waits for the next pass (landsPastARelocatedFolder).
// An item also waits while another item
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
	// moveDisplaceEmptyFolder: an empty folder made here, no synced item,
	// at a synced folder's local move destination: it is deleted first, the
	// executor's delete refusing it if it gained an entry since the scan, and
	// the folder lands in its place, the one folder at the path
	// (emptyNewFolderGivesWay).
	moveDisplaceEmptyFolder
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

// composedMerge binds a new folder made here at newPath to the synced folder
// key, which OneDrive holds there, and deletes key's own copy here, empty at
// localPath (mergeWithANewFolderHere).
type composedMerge struct {
	key       string
	localPath string
	newPath   string
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
	merges  []composedMerge
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
	// localFolders holds the local paths that are folders: a new one at a
	// file's local move destination is renamed aside as a conflict copy
	// (newFolderMeetsAFile).
	localFolders map[string]bool
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
	// collisionUndone holds items whose local relocation was undone because
	// it claimed the path of another item (findTargetCollision): their move
	// back never takes the place of a folder made here (mergesWithANewFolderHere,
	// emptyNewFolderGivesWay), which would undo what the user did here twice.
	collisionUndone map[string]bool
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

		collisionUndone: map[string]bool{},
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
		if kind == moveNoticeCollision {
			c.collisionUndone[key] = true
		}
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
