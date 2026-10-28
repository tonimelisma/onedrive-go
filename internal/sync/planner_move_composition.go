package sync

import (
	"path"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
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
//     (P-6, D-051). Neither choice loses data; remote-wins needs no remote
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
// remote_state and the baseline follow every committed move). Anything else is
// deferred: the item stays where it is on both sides and the next pass, which
// observes the moves this one made, plans it simply. Every pass makes progress
// because the shallowest pending item is always ready.
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
	itemType ItemType
	paths    [moveCoordCount]string
	// displace says what a move of another item onto this node's local path
	// may do with it once OneDrive no longer holds this item (D-058).
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
	// moveDisplaceConflictCopy: gone online but edited locally; the edit is
	// renamed to a conflict copy first and uploaded as a new file.
	moveDisplaceConflictCopy
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
}

// moveOccupancy reports the paths each side's snapshot holds, so a move is
// never emitted onto an entry that is not one of the composed items.
type moveOccupancy struct {
	local  map[string]bool
	remote map[string]bool
	// remoteFiles holds the remote paths that are files, which nothing can
	// be created under.
	remoteFiles map[string]bool
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
	byTarget := make(map[string][]string, len(c.keys))
	hasLocalOrigin := false
	for _, key := range c.keys {
		// An item present on one side only is a create or delete SQLite
		// plans; where it sits is checked as occupancy when a move lands on
		// it, not as a competing claim.
		if c.unresolvable[key] || !c.isMoveParticipant(c.nodes[key]) {
			continue
		}
		folded := foldMovePath(c.targetPath(key))
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
			folded := foldMovePath(c.remoteTargetPath(occupied))
			if claims, ok := byTarget[folded]; ok {
				byTarget[folded] = append(claims, remoteOccupantClaim)
			}
		}
	}
	folded := make([]string, 0, len(byTarget))
	for p := range byTarget {
		folded = append(folded, p)
	}
	sort.Strings(folded)
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

// foldMovePath folds a path the way OneDrive compares names (namekey), so two
// targets that differ only in case collide as they would online.
func foldMovePath(p string) string {
	segments := strings.Split(p, "/")
	for i := range segments {
		segments[i] = namekey.Key(segments[i])
	}

	return strings.Join(segments, "/")
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
}

func (c *moveComposer) emit() moveComposition {
	e := &moveEmitter{
		moveComposer: c,
		state:        make(map[string]moveEmitState, len(c.keys)),
		result:       moveComposition{deferred: map[string]bool{}},
	}

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
			break
		}
	}
	for _, key := range order {
		if e.state[key] == moveEmitPending {
			e.deferNode(key)
		}
	}
	e.result.notices = append(c.notices, e.result.notices...)

	return e.result
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

// resolve decides one item. It reports whether the item left the pending
// state.
func (e *moveEmitter) resolve(key string) bool {
	switch e.parentState(key) {
	case moveEmitPending:
		return false
	case moveEmitDeferred:
		e.deferNode(key)
		return true
	case moveEmitSettled:
	}

	n := e.nodes[key]
	if e.unresolvable[key] {
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
		if !n.present(side) || sameLocation(e.location(n, side), target) {
			continue
		}
		move, ok := e.planMove(n, side, newPath)
		if !ok {
			e.deferNode(key)
			return true
		}
		e.result.moves = append(e.result.moves, move)
		e.state[key] = moveEmitSettled
		return true
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

// planMove builds the move of n on side, or reports that it must wait for a
// later pass.
func (e *moveEmitter) planMove(n *moveNode, side moveCoord, newPath string) (composedMove, bool) {
	oldPath := n.paths[side]
	remotePath := ""
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if earlier.side != side || !strings.HasPrefix(oldPath, earlier.oldPath+"/") {
			continue
		}
		if !strings.HasPrefix(newPath, earlier.newPath+"/") {
			// The earlier move waits for this one (children leave first).
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
	if oldPath == newPath {
		return composedMove{}, false
	}
	free, displaced := e.destinationFree(n, side, newPath)
	if !free {
		return composedMove{}, false
	}

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
	occupant, isNode := e.byPath[side][snapshotPath]
	if !isNode && !occupied[snapshotPath] {
		return true, moveDisplaceNever
	}
	if isNode && occupant == n.key {
		return true, moveDisplaceNever
	}
	for i := range e.result.moves {
		earlier := &e.result.moves[i]
		if earlier.side == side && e.nodes[earlier.key].paths[side] == snapshotPath {
			return true, moveDisplaceNever
		}
	}
	if isNode && side == moveCoordLocal && snapshotPath == newPath {
		if displace := e.nodes[occupant].displace; displace != moveDisplaceNever {
			return true, displace
		}
	}

	return false, moveDisplaceNever
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
}

func moveCompositionInputs(
	comparisons []sQLiteComparisonRow,
	views map[string]*pathView,
	remoteRows []remoteStateRow,
	localRows []localStateRow,
) moveInputs {
	cmpByPath := make(map[string]*sQLiteComparisonRow, len(comparisons))
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
		if !cmp.BaselinePresent || view == nil || view.Baseline == nil || view.Baseline.ItemID == "" {
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
		if cmp.ComparisonKind == comparisonKindLocalMoveSource && cmp.LocalPresent && cmp.LocalMoveTarget != "" {
			recreated[node.key] = cmp.Path
		}
		nodes = append(nodes, node)
	}

	return moveInputs{nodes: nodes, occupancy: occupancy, frozen: frozen, recreated: recreated}
}

// comparisonKindLocalMoveSource is SQLite's comparison kind for a baseline path
// whose item was renamed away locally.
const comparisonKindLocalMoveSource = "local_move_source"

func indexMoveSnapshot(remoteRows []remoteStateRow, localRows []localStateRow) (map[string]*remoteStateRow, moveOccupancy) {
	remoteByID := make(map[string]*remoteStateRow, len(remoteRows))
	occupancy := moveOccupancy{
		local:       make(map[string]bool, len(localRows)),
		remote:      make(map[string]bool, len(remoteRows)),
		remoteFiles: make(map[string]bool, len(remoteRows)),
	}
	for i := range remoteRows {
		remoteByID[remoteRows[i].ItemID] = &remoteRows[i]
		occupancy.remote[remoteRows[i].Path] = true
		if remoteRows[i].ItemType != ItemTypeFolder {
			occupancy.remoteFiles[remoteRows[i].Path] = true
		}
	}
	for i := range localRows {
		occupancy.local[localRows[i].Path] = true
	}

	return remoteByID, occupancy
}

func moveNodeTouchesUnavailableTruth(node *moveNode, views map[string]*pathView) bool {
	for coord := range moveCoordCount {
		if p := node.paths[coord]; p != "" && views[p] != nil && plannerSuppressesUnavailableTruth(&views[p].TruthStatus) {
			return true
		}
	}

	return false
}

// composedDisplacement decides whether a synced file OneDrive no longer has
// may give way to another item moved onto its local path: replaced when its
// local copy is provably the synced version, renamed to a conflict copy when
// it was edited, never otherwise (a folder, unknown content, or an item that
// still exists online somewhere).
func composedDisplacement(node *moveNode, entry *BaselineEntry, view *pathView, remoteByID map[string]*remoteStateRow) moveDisplacement {
	if node.paths[moveCoordLocal] != node.paths[moveCoordBaseline] || entry.ItemType != ItemTypeFile || view.Local == nil {
		return moveDisplaceNever
	}
	if _, stillOnline := remoteByID[entry.ItemID]; stillOnline {
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

func composedLocalPosition(cmp *sQLiteComparisonRow, view *pathView) string {
	if cmp.ComparisonKind == comparisonKindLocalMoveSource && cmp.LocalMoveTarget != "" {
		return cmp.LocalMoveTarget
	}
	if view.Local != nil && view.Local.ItemType == view.Baseline.ItemType {
		return cmp.Path
	}

	return ""
}

func composedRemotePosition(
	cmp *sQLiteComparisonRow,
	entry *BaselineEntry,
	remoteByID map[string]*remoteStateRow,
	cmpByPath map[string]*sQLiteComparisonRow,
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
	case at.ComparisonKind == "remote_move_shadowed":
		return row.Path
	case at.ComparisonKind == "local_move_dest" && row.Path == localPosition:
		return row.Path
	default:
		return ""
	}
}

// remoteToLocalFolderPrefixes maps each composed folder's remote path to where
// the same folder sits locally now, deepest first. Remote-origin local writes
// carry remote paths; rebased through this map they address the local tree as
// it is, and moves wait for writes under their source (buildDependencies).
func remoteToLocalFolderPrefixes(nodes []moveNode) []graphFolderMoveBoundary {
	var prefixes []graphFolderMoveBoundary
	for i := range nodes {
		n := &nodes[i]
		if n.itemType != ItemTypeFolder || !n.present(moveCoordLocal) || !n.present(moveCoordRemote) {
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

// buildComposedMoveActions turns composed moves and re-keys into actions.
func buildComposedMoveActions(
	composition *moveComposition,
	inputs *moveInputs,
	views map[string]*pathView,
	baseline *Baseline,
) []Action {
	nodes := inputs.nodes
	nodeByKey := make(map[string]*moveNode, len(nodes))
	for i := range nodes {
		nodeByKey[nodes[i].key] = &nodes[i]
	}
	viewAt := func(p string) *pathView {
		if v := views[p]; v != nil {
			return v
		}
		return &pathView{Path: p}
	}

	var actions []Action
	for i := range composition.moves {
		move := &composition.moves[i]
		node := nodeByKey[move.key]
		entry, ok := baseline.GetByID(move.key)
		if node == nil || !ok {
			continue
		}
		remote := viewAt(node.paths[moveCoordRemote]).Remote
		if move.side == moveCoordLocal {
			actions = append(actions, composedLocalMoveActions(move, entry, remote, viewAt)...)
			continue
		}
		actions = append(actions, composedRemoteMoveActions(move, entry, remote, viewAt)...)
	}

	actions = append(actions, recreatedSourceCreates(composition, inputs, nodeByKey, viewAt)...)

	for i := range composition.rekeys {
		rekey := &composition.rekeys[i]
		node := nodeByKey[rekey.key]
		entry, ok := baseline.GetByID(rekey.key)
		if node == nil || !ok {
			continue
		}
		view := &pathView{
			Path:        rekey.newPath,
			Remote:      viewAt(node.paths[moveCoordRemote]).Remote,
			Local:       viewAt(node.paths[moveCoordLocal]).Local,
			Baseline:    entry,
			TruthStatus: viewAt(rekey.newPath).TruthStatus,
		}
		action := makeAction(ActionBaselineUpdate, view)
		action.OldPath = rekey.oldPath
		actions = append(actions, action)
	}

	return actions
}

func localFileDiffersFromBaseline(local *localState, baseline *BaselineEntry) bool {
	if local == nil || baseline == nil {
		return false
	}
	if baseline.ItemType != ItemTypeFile || local.ItemType != ItemTypeFile {
		return false
	}
	if baseline.LocalHash != "" || local.Hash != "" {
		return baseline.LocalHash != local.Hash
	}
	if baseline.LocalSizeKnown && baseline.LocalSize != local.Size {
		return true
	}

	return baseline.LocalMtime != local.Mtime
}

// composedLocalMoveActions builds a remote-origin local move, preceded by the
// conflict copy of an edited file it displaces (buildDependencies orders the
// move after the copy at its destination).
func composedLocalMoveActions(move *composedMove, entry *BaselineEntry, remote *remoteState, viewAt func(string) *pathView) []Action {
	view := &pathView{
		Path:        move.newPath,
		Remote:      remote,
		Local:       viewAt(move.newPath).Local,
		Baseline:    entry,
		TruthStatus: viewAt(move.newPath).TruthStatus,
	}
	action := makeAction(ActionLocalMove, view)
	action.OldPath = move.oldPath
	action.MovedRemotePath = move.remotePath

	switch move.displaced {
	case moveDisplaceReplace:
		action.ReplaceLocalTarget = true
	case moveDisplaceConflictCopy:
		copyView := *viewAt(move.newPath)
		return []Action{makeConflictCopyAction(&copyView), action}
	case moveDisplaceNever:
	}

	return []Action{action}
}

// composedRemoteMoveActions builds a local-origin remote move. A move whose
// content could not be proven unchanged (no hash) also uploads what now sits
// at the destination, but only when the item is already there locally: the
// upload reads local truth at its path.
func composedRemoteMoveActions(move *composedMove, entry *BaselineEntry, remote *remoteState, viewAt func(string) *pathView) []Action {
	movedLocal := viewAt(move.localPath).Local
	view := &pathView{
		Path:        move.oldPath,
		Remote:      remote,
		Local:       viewAt(move.oldPath).Local,
		Baseline:    entry,
		TruthStatus: viewAt(move.oldPath).TruthStatus,
	}
	action := makeAction(ActionRemoteMove, view)
	action.OldPath = move.oldPath
	action.Path = move.newPath
	action.MovedLocal = movedLocal
	if move.localPath != move.newPath {
		action.MovedLocalPath = move.localPath
	}
	actions := []Action{action}

	if move.localPath == move.newPath && localFileDiffersFromBaseline(movedLocal, entry) {
		uploadView := &pathView{
			Path:        move.newPath,
			Local:       movedLocal,
			Remote:      remote,
			Baseline:    entry,
			TruthStatus: viewAt(move.newPath).TruthStatus,
		}
		actions = append(actions, makeAction(ActionUpload, uploadView))
	}

	return actions
}

// recreatedSourceCreates plans the new local entry at a path the synced item
// was renamed away from (D-056). It is created once OneDrive no longer holds
// the moved item there: already moved elsewhere online, or moved by a remote
// move of this plan, which the create waits for (buildDependencies). While
// the move waits for a later pass, so does the create: uploading at the old
// path first would write a new version of the moved item instead.
func recreatedSourceCreates(
	composition *moveComposition,
	inputs *moveInputs,
	nodeByKey map[string]*moveNode,
	viewAt func(string) *pathView,
) []Action {
	keys := make([]string, 0, len(inputs.recreated))
	for key := range inputs.recreated {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var actions []Action
	for _, key := range keys {
		source := inputs.recreated[key]
		node := nodeByKey[key]
		if node == nil || inputs.frozen[key] || composition.deferred[key] {
			continue
		}
		vacated := node.paths[moveCoordRemote] != source
		for i := range composition.moves {
			move := &composition.moves[i]
			if move.key == key && move.side == moveCoordRemote && move.oldPath == source {
				vacated = true
			}
		}
		sourceView := viewAt(source)
		if !vacated || sourceView.Local == nil || plannerSuppressesUnavailableTruth(&sourceView.TruthStatus) {
			continue
		}
		createView := &pathView{Path: source, Local: sourceView.Local, TruthStatus: sourceView.TruthStatus}
		if sourceView.Local.ItemType == ItemTypeFolder {
			actions = append(actions, makeFolderCreate(createView, CreateRemote))
			continue
		}
		actions = append(actions, makeCreateUploadAction(createView))
	}

	return actions
}

// dropCreatesOntoComposedItems removes creates of another item at a path a
// composed item still holds on that side this pass. Rebased remote-origin
// writes can target the local position of an item whose move waits for a
// later pass (or was undone to OneDrive's location); run anyway, the create
// failed on the occupied path, and a local folder move waiting for the writes
// under it never ran. Next pass the item has moved and the create is planned
// again.
func dropCreatesOntoComposedItems(actions []Action, inputs *moveInputs, composition *moveComposition) []Action {
	heldBy := pathsHeldByUnmovedItems(inputs, composition)
	vacated := pathsVacatedByPlan(actions)

	kept := actions[:0:0]
	for i := range actions {
		action := &actions[i]
		side, isCreate := createSide(action)
		if !isCreate {
			kept = append(kept, *action)
			continue
		}
		if holder, held := heldBy[side][action.Path]; held && holder != action.ItemID {
			continue
		}
		if action.Path != actionViewPath(action) && rebasedCreateBlocked(action.Path, side, &inputs.occupancy, vacated[side]) {
			continue
		}
		kept = append(kept, *action)
	}

	return kept
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
func pathsVacatedByPlan(actions []Action) map[moveCoord]map[string]bool {
	vacated := map[moveCoord]map[string]bool{moveCoordLocal: {}, moveCoordRemote: {}}
	for i := range actions {
		switch actions[i].Type {
		case ActionLocalMove:
			vacated[moveCoordLocal][actions[i].OldPath] = true
		case ActionConflictCopy, ActionLocalDelete:
			vacated[moveCoordLocal][actions[i].Path] = true
		case ActionRemoteMove:
			vacated[moveCoordRemote][actions[i].OldPath] = true
		case ActionRemoteDelete:
			vacated[moveCoordRemote][actions[i].Path] = true
		case ActionDownload, ActionUpload, ActionFolderCreate, ActionBaselineUpdate, ActionCleanup:
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
func createSide(action *Action) (moveCoord, bool) {
	switch action.Type {
	case ActionDownload:
		return moveCoordLocal, true
	case ActionUpload:
		return moveCoordRemote, true
	case ActionFolderCreate:
		if action.CreateSide == createLocal {
			return moveCoordLocal, true
		}
		return moveCoordRemote, true
	case ActionLocalMove, ActionRemoteMove, ActionLocalDelete, ActionRemoteDelete,
		ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		return moveCoordBaseline, false
	default:
		return moveCoordBaseline, false
	}
}

// dropTransfersOfComposedMoves removes reconciliation transfers of an item
// that composition moves: OneDrive moved it onto a path that held another
// synced item, so SQLite sees a changed item at the destination and plans a
// download (or a conflict) there. Composition moves the local copy instead;
// downloading as well left the source behind, and the next pass uploaded it
// as a spurious duplicate (D-058).
func dropTransfersOfComposedMoves(actions []Action, composition *moveComposition) []Action {
	movedLocally := map[string]bool{}
	for i := range composition.moves {
		if composition.moves[i].side == moveCoordLocal {
			movedLocally[composition.moves[i].key] = true
		}
	}
	if len(movedLocally) == 0 {
		return actions
	}

	kept := actions[:0:0]
	for i := range actions {
		action := &actions[i]
		if movedLocally[action.ItemID] && (action.Type == ActionDownload || action.Type == ActionConflictCopy) {
			continue
		}
		kept = append(kept, *action)
	}

	return kept
}
