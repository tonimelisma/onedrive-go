package syncplan

import (
	"iter"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// buildDependencies computes dependency edges for a flat action list.
// Returns deps where deps[i] contains the indices that action i depends on.
// Rules: (1) folder create before any action in that subtree,
// (2) child delete/cleanup before parent folder delete,
// (3) moves order against each other and against the work around them
// (addMoveDeps), (4) a folder delete waits for same-side moves that carry
// descendants out of it, (5) the create half of a type-change replacement
// waits for the delete at the same path, (6) a remote create waits for the
// remote delete or move-away of a sibling whose name differs only in case,
// (7) a delete waits for the create on its side that writes the deleted
// file's content at another path, unless that closes a cycle
// (addContentCarrierDeps).
//
// Every rule relates an action to actions at its own path, at the folders
// above it, at the OneDrive name keys of its path and of those folders, or
// with the content it writes or removes, so the partners are found through
// indexes built once (dependencyIndex) and each rule costs the path's depth,
// not the plan's size (spec/design/performance-benchmarking.md).
func buildDependencies(actions []syncdomain.Action) [][]int {
	deps, _ := buildDependencyGraph(actions)

	return deps
}

// buildDependencyGraph is buildDependencies, and it also returns the content
// edges rule 7 leaves out as they would close a cycle (addContentCarrierDeps).
func buildDependencyGraph(actions []syncdomain.Action) (deps [][]int, leftOut []contentEdge) {
	deps = make([][]int, len(actions))
	index := newDependencyIndex(actions)

	for i := range actions {
		deps[i] = addParentFolderDep(deps[i], i, &actions[i], index.folderCreateIdx)
		deps[i] = addConflictCopyDep(deps[i], i, &actions[i], index.conflictCopyIdx)
		deps[i] = addChildDeleteDeps(deps[i], i, &actions[i], index)
		deps[i] = addSamePathReplacementDep(deps[i], i, &actions[i], index.deleteIdx)
		deps[i] = addMoveDeps(deps[i], i, &actions[i], actions, index)
		deps[i] = addMoveOutOfDeletedFolderDeps(deps[i], i, &actions[i], actions, index)
		deps[i] = addRemoteCaseVariantDeps(deps[i], i, &actions[i], actions, index)
		// Sort dependency indices for reproducible ordering (B-154).
		sort.Ints(deps[i])
	}
	leftOut = addContentCarrierDeps(deps, actions, index)

	return deps, leftOut
}

// dependencyIndex locates the actions a dependency rule relates an action
// to. The maps keyed by path hold the last action at a path, as the rules
// always did; the others hold every action. The per-action candidate lists
// are filled from the other end of the edge: an action is listed under each
// folder delete or move whose path lies above it, found by walking the
// folders above the action (pathAncestors), so the lists cost the depth of
// each path to build and hold each pair once.
type dependencyIndex struct {
	folderCreateIdx map[string]int
	conflictCopyIdx map[string]int
	deleteIdx       map[string]int

	movesByPath    map[string][]int // moves by destination
	movesByOldPath map[string][]int // moves by source
	// movesByOldNameKey lists the moves by the name key of their source: a
	// move into a folder below the path another move vacates finds that move
	// under OneDrive's name folding (movesIntoVacatedPath).
	movesByOldNameKey map[namekey.PathKey][]int
	actionsByPath     map[string][]int

	// childDeletes lists, per folder delete, the deletes below its path.
	childDeletes map[int][]int
	// movesBelowFolderDelete lists, per folder delete, the moves whose source
	// lies below its path.
	movesBelowFolderDelete map[int][]int
	// workBelowMoveSource lists, per move, the actions whose path or move
	// source lies below the move's source.
	workBelowMoveSource map[int][]int
	// vacatedByNameKey lists remote deletes by the name key of their path and
	// remote moves by the name key of their source: the actions that free a
	// OneDrive name (addRemoteCaseVariantDeps).
	vacatedByNameKey map[namekey.PathKey][]int
	// contentCarriers lists the uploads and downloads by the content they
	// write, and contentDeletes the deletes of a synced file, the two ends
	// of addContentCarrierDeps.
	contentCarriers map[contentKey][]int
	contentDeletes  []int
}

func newDependencyIndex(actions []syncdomain.Action) *dependencyIndex {
	index := &dependencyIndex{
		folderCreateIdx:        make(map[string]int),
		conflictCopyIdx:        make(map[string]int),
		deleteIdx:              make(map[string]int),
		movesByPath:            make(map[string][]int),
		movesByOldPath:         make(map[string][]int),
		movesByOldNameKey:      make(map[namekey.PathKey][]int),
		actionsByPath:          make(map[string][]int),
		childDeletes:           make(map[int][]int),
		movesBelowFolderDelete: make(map[int][]int),
		workBelowMoveSource:    make(map[int][]int),
		vacatedByNameKey:       make(map[namekey.PathKey][]int),
		contentCarriers:        make(map[contentKey][]int),
	}
	folderDeletesByPath := make(map[string][]int)
	for i := range actions {
		index.indexByPath(i, &actions[i], folderDeletesByPath)
	}
	index.linkWorkBelowFolders(actions, folderDeletesByPath)
	index.indexVacatedNames(actions)

	return index
}

func (index *dependencyIndex) indexByPath(i int, a *syncdomain.Action, folderDeletesByPath map[string][]int) {
	index.actionsByPath[a.Path] = append(index.actionsByPath[a.Path], i)
	switch a.Type {
	case syncdomain.ActionFolderCreate:
		index.folderCreateIdx[a.Path] = i
	case syncdomain.ActionConflictCopy:
		index.conflictCopyIdx[a.Path] = i
	case syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove:
		index.movesByPath[a.Path] = append(index.movesByPath[a.Path], i)
		index.movesByOldPath[a.OldPath] = append(index.movesByOldPath[a.OldPath], i)
		oldKey := namekey.OfPath(a.OldPath)
		index.movesByOldNameKey[oldKey] = append(index.movesByOldNameKey[oldKey], i)
	case syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete, syncdomain.ActionCleanup:
		index.deleteIdx[a.Path] = i
		if isSubtreeDelete(a) {
			folderDeletesByPath[a.Path] = append(folderDeletesByPath[a.Path], i)
		}
		if _, removes := deletedContentKey(a); removes {
			index.contentDeletes = append(index.contentDeletes, i)
		}
	case syncdomain.ActionDownload, syncdomain.ActionUpload:
		if key, writes := carriedContentKey(a); writes {
			index.contentCarriers[key] = append(index.contentCarriers[key], i)
		}
	case syncdomain.ActionBaselineUpdate:
	default:
		panic(syncdomain.OutsideTheEnum(a.Type))
	}
}

// linkWorkBelowFolders fills the per-action candidate lists by walking the
// folders above each action's path and move source.
func (index *dependencyIndex) linkWorkBelowFolders(actions []syncdomain.Action, folderDeletesByPath map[string][]int) {
	for childPath, childIdx := range index.deleteIdx {
		for parent := range pathAncestors(childPath) {
			for _, folderDeleteIdx := range folderDeletesByPath[parent] {
				index.childDeletes[folderDeleteIdx] = append(index.childDeletes[folderDeleteIdx], childIdx)
			}
		}
	}
	if len(index.movesByOldPath) == 0 && len(folderDeletesByPath) == 0 {
		return
	}

	for i := range actions {
		a := &actions[i]
		for parent := range pathAncestors(a.Path) {
			for _, moveIdx := range index.movesByOldPath[parent] {
				index.workBelowMoveSource[moveIdx] = append(index.workBelowMoveSource[moveIdx], i)
			}
		}
		if !isMoveAction(a) {
			continue
		}
		for parent := range pathAncestors(a.OldPath) {
			for _, moveIdx := range index.movesByOldPath[parent] {
				index.workBelowMoveSource[moveIdx] = append(index.workBelowMoveSource[moveIdx], i)
			}
			for _, folderDeleteIdx := range folderDeletesByPath[parent] {
				index.movesBelowFolderDelete[folderDeleteIdx] = append(index.movesBelowFolderDelete[folderDeleteIdx], i)
			}
		}
	}
}

// indexVacatedNames lists the remote deletes and remote moves by the name
// key of the path they free; a plan without either (a first upload) computes
// no keys, and addRemoteCaseVariantDeps computes none against an empty index.
func (index *dependencyIndex) indexVacatedNames(actions []syncdomain.Action) {
	for i := range actions {
		a := &actions[i]
		switch a.Type {
		case syncdomain.ActionRemoteDelete:
			key := namekey.OfPath(a.Path)
			index.vacatedByNameKey[key] = append(index.vacatedByNameKey[key], i)
		case syncdomain.ActionRemoteMove:
			key := namekey.OfPath(a.OldPath)
			index.vacatedByNameKey[key] = append(index.vacatedByNameKey[key], i)
		case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalDelete, syncdomain.ActionLocalMove,
			syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		default:
			panic(syncdomain.OutsideTheEnum(a.Type))
		}
	}
}

// pathAncestors yields every prefix p of path such that path lies below p,
// that is path has the prefix p + "/": the folders above the path, nearest
// last. It is the exact inverse of strings.HasPrefix(path, p+"/").
func pathAncestors[P ~string](path P) iter.Seq[P] {
	return func(yield func(P) bool) {
		for i := range len(path) {
			if path[i] == '/' && !yield(path[:i]) {
				return
			}
		}
	}
}

// isSubtreeDelete reports a local or remote delete of a folder: the deletes
// that wait for the work below their path (addChildDeleteDeps,
// addMoveOutOfDeletedFolderDeps). A cleanup removes only a baseline row.
func isSubtreeDelete(a *syncdomain.Action) bool {
	if a.Type != syncdomain.ActionLocalDelete && a.Type != syncdomain.ActionRemoteDelete {
		return false
	}

	return syncdomain.DeletedItemType(a) == syncdomain.ItemTypeFolder
}

// addParentFolderDep adds a dependency on a parent folder create if present.
// A delete or cleanup removes an entry of the old item at that path and
// never needs the parent created; waiting on a re-created parent closed a
// cycle with the move that vacates it (a folder moved and re-created).
func addParentFolderDep(deps []int, idx int, a *syncdomain.Action, folderCreateIdx map[string]int) []int {
	if isDeleteAction(a) {
		return deps
	}
	parentDir := filepath.Dir(a.Path)
	if parentDir == "." || parentDir == "" {
		return deps
	}

	parentDir = filepath.ToSlash(parentDir)

	if fcIdx, ok := folderCreateIdx[parentDir]; ok && fcIdx != idx {
		deps = append(deps, fcIdx)
	}

	return deps
}

// addConflictCopyDep makes a local create at a path wait for the conflict copy
// that moves the local entry there away.
func addConflictCopyDep(deps []int, idx int, a *syncdomain.Action, conflictCopyIdx map[string]int) []int {
	if !createsOnSide(a, moveCoordLocal) {
		return deps
	}

	if copyIdx, ok := conflictCopyIdx[a.Path]; ok && copyIdx != idx {
		deps = append(deps, copyIdx)
	}

	return deps
}

// addSamePathReplacementDep makes the create half of a type-change
// replacement wait for the delete of the old item at the same path.
func addSamePathReplacementDep(deps []int, idx int, a *syncdomain.Action, deleteIdx map[string]int) []int {
	if _, isCreate := createSide(a); !isCreate {
		return deps
	}
	if delIdx, ok := deleteIdx[a.Path]; ok && delIdx != idx {
		deps = append(deps, delIdx)
	}

	return deps
}

// addChildDeleteDeps makes folder deletes depend on child deletes at deeper paths.
func addChildDeleteDeps(deps []int, idx int, a *syncdomain.Action, index *dependencyIndex) []int {
	if !isSubtreeDelete(a) {
		return deps
	}

	for _, childIdx := range index.childDeletes[idx] {
		if childIdx != idx {
			deps = append(deps, childIdx)
		}
	}

	return deps
}

// addMoveOutOfDeletedFolderDeps makes a folder delete wait for moves that
// carry descendants out of it on the same side. Deleting a folder deletes its
// subtree, so a remote folder delete that ran before a remote move of one of
// its children would take the child with it (and a local folder delete would
// do the same to a child a remote move is relocating locally).
func addMoveOutOfDeletedFolderDeps(deps []int, idx int, a *syncdomain.Action, actions []syncdomain.Action, index *dependencyIndex) []int {
	var sameSideMove syncdomain.ActionType
	switch a.Type {
	case syncdomain.ActionRemoteDelete:
		sameSideMove = syncdomain.ActionRemoteMove
	case syncdomain.ActionLocalDelete:
		sameSideMove = syncdomain.ActionLocalMove
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove,
		syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return deps
	default:
		panic(syncdomain.OutsideTheEnum(a.Type))
	}
	if !isSubtreeDelete(a) {
		return deps
	}

	prefix := a.Path + "/"
	for _, moveIdx := range index.movesBelowFolderDelete[idx] {
		move := &actions[moveIdx]
		if moveIdx != idx && move.Type == sameSideMove && strings.HasPrefix(move.OldPath, prefix) {
			deps = append(deps, moveIdx)
		}
	}

	return deps
}

// addRemoteCaseVariantDeps orders a case-only rename on OneDrive, whose
// namespace treats case, Unicode form and emoji-only differences as one name
// (namekey). When the rename cannot be paired as a move
// (the content changed too), it plans a delete of "edit.txt" and an upload of
// "EDIT.txt". A create never replaces (R-5.6.14), so a create that ran first
// would fail with 409 against the old item and only succeed on a later pass.
// The create therefore waits for the old name to be deleted or moved away.
func addRemoteCaseVariantDeps(deps []int, idx int, a *syncdomain.Action, actions []syncdomain.Action, index *dependencyIndex) []int {
	switch a.Type {
	case syncdomain.ActionUpload, syncdomain.ActionRemoteMove:
	case syncdomain.ActionFolderCreate:
		if a.CreateSide != syncdomain.CreateRemote {
			return deps
		}
	case syncdomain.ActionDownload, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete, syncdomain.ActionLocalMove,
		syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return deps
	default:
		panic(syncdomain.OutsideTheEnum(a.Type))
	}
	if len(index.vacatedByNameKey) == 0 {
		return deps
	}

	for _, otherIdx := range index.vacatedByNameKey[namekey.OfPath(a.Path)] {
		if otherIdx != idx && vacatesCaseVariantOf(&actions[otherIdx], a) {
			deps = append(deps, otherIdx)
		}
	}

	return deps
}

// vacatesCaseVariantOf reports whether other frees a OneDrive name that a's
// path differs from only under name folding, so a must wait for it.
func vacatesCaseVariantOf(other, a *syncdomain.Action) bool {
	var vacated string
	switch other.Type {
	case syncdomain.ActionRemoteDelete:
		vacated = other.Path
	case syncdomain.ActionRemoteMove:
		if a.Type == syncdomain.ActionRemoteMove {
			// Move-to-move edges are skipped: two moves that trade
			// case-variant names would wait on each other. A blocked
			// rename fails on its name conflict and is retried instead.
			return false
		}
		vacated = other.OldPath
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalDelete, syncdomain.ActionLocalMove,
		syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return false
	default:
		panic(syncdomain.OutsideTheEnum(other.Type))
	}
	if vacated == a.Path || vacated == a.OldPath || !namekey.EqualPath(vacated, a.Path) {
		return false
	}
	if a.Type == syncdomain.ActionRemoteMove && strings.HasPrefix(a.OldPath, vacated+"/") {
		// The folder delete already waits for this move out of it.
		return false
	}

	return true
}

// addContentCarrierDeps makes a delete wait for each create on its side that
// writes the content of the synced file it deletes at another path
// (carriesContentOf): on OneDrive the upload of the file here that has the
// file's local identity, which SQLite plans when it cannot pair a rename
// because OneDrive keeps another item at the new path, and here the download
// of the file's item. Run before the create, the delete left that side
// without the content until a later pass whenever the create, or what it
// waits for, failed; run after it, the delete is skipped with the failed
// chain and planned again with it. An edge that would close a dependency
// cycle is left out (addContentEdges) and returned: there the create waits
// for the delete through the other rules, as the upload of a file edited in a
// folder renamed here waits for the folder's move, which waits for the delete
// of the file's old copy, and no order of this plan keeps the content
// (deletesAheadOfTheirContent holds some such deletes for the next pass).
func addContentCarrierDeps(deps [][]int, actions []syncdomain.Action, index *dependencyIndex) []contentEdge {
	if len(index.contentDeletes) == 0 || len(index.contentCarriers) == 0 {
		return nil
	}

	var edges []contentEdge
	for _, deleteIdx := range index.contentDeletes {
		key, _ := deletedContentKey(&actions[deleteIdx])
		for _, createIdx := range index.contentCarriers[key] {
			if carriesContentOf(&actions[createIdx], &actions[deleteIdx]) {
				edges = append(edges, contentEdge{deleteIdx: deleteIdx, createIdx: createIdx})
			}
		}
	}

	return addContentEdges(deps, edges)
}

// contentEdge is a delete that waits for a create carrying its content.
type contentEdge struct {
	deleteIdx, createIdx int
}

// addContentEdges adds each edge to deps, except one the delete already has
// and one whose create already waits for its delete, which would close a
// cycle, and returns the edges it leaves out. Edges are decided one at a time,
// each against the plan's other edges and the content edges kept before it:
// of two edges that close a cycle only together, the first is kept, so one
// of the deletes still waits for its content. Only an edge whose ends share a
// strongly connected component (dependencyComponents) once every edge is in
// can close a cycle, so the others are kept without a walk, and none at all
// is walked in a plan whose content edges lie on no cycle.
func addContentEdges(deps [][]int, edges []contentEdge) (leftOut []contentEdge) {
	added := edges[:0:0]
	for _, edge := range edges {
		if !slices.Contains(deps[edge.deleteIdx], edge.createIdx) {
			deps[edge.deleteIdx] = append(deps[edge.deleteIdx], edge.createIdx)
			added = append(added, edge)
		}
	}
	if len(added) == 0 {
		return nil
	}
	for _, edge := range added {
		sort.Ints(deps[edge.deleteIdx])
	}

	component, _ := dependencyComponents(deps)
	var onCycles []contentEdge
	for _, edge := range added {
		if component[edge.deleteIdx] == component[edge.createIdx] {
			deps[edge.deleteIdx] = withoutDep(deps[edge.deleteIdx], edge.createIdx)
			onCycles = append(onCycles, edge)
		}
	}
	for _, edge := range onCycles {
		if waitsFor(deps, edge.createIdx, edge.deleteIdx) {
			leftOut = append(leftOut, edge)
			continue
		}
		at, _ := slices.BinarySearch(deps[edge.deleteIdx], edge.createIdx)
		deps[edge.deleteIdx] = slices.Insert(deps[edge.deleteIdx], at, edge.createIdx)
	}

	return leftOut
}

// withoutDep removes dep from the sorted list deps.
func withoutDep(deps []int, dep int) []int {
	if at, found := slices.BinarySearch(deps, dep); found {
		return slices.Delete(deps, at, at+1)
	}

	return deps
}

// waitsFor reports whether action from waits for action to in deps, directly
// or through others. Each list of deps is sorted, so an action is asked for
// to before what it waits for is walked: through a folder's move, which waits
// for every delete below its source, the answer costs the depth of the walk,
// not the size of the folder.
func waitsFor(deps [][]int, from, to int) bool {
	seen := map[int]bool{from: true}
	stack := []int{from}
	for len(stack) > 0 {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, found := slices.BinarySearch(deps[v], to); found {
			return true
		}
		for _, w := range deps[v] {
			if !seen[w] {
				seen[w] = true
				stack = append(stack, w)
			}
		}
	}

	return false
}

// contentKey names the synced content a create writes and a delete removes:
// a local identity on OneDrive's side, an item ID on this computer's.
type contentKey struct {
	itemID        string
	device, inode uint64
}

// carriedContentKey reports the content an upload or a download writes: the
// local identity of the file an upload reads, the item a download fetches.
func carriedContentKey(create *syncdomain.Action) (contentKey, bool) {
	switch create.Type {
	case syncdomain.ActionUpload:
		if create.View == nil || create.View.Local == nil || !create.View.Local.LocalHasIdentity {
			return contentKey{}, false
		}

		return contentKey{device: create.View.Local.LocalDevice, inode: create.View.Local.LocalInode}, true
	case syncdomain.ActionDownload:
		return contentKey{itemID: create.ItemID}, create.ItemID != ""
	case syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete,
		syncdomain.ActionFolderCreate,
		syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return contentKey{}, false
	default:
		panic(syncdomain.OutsideTheEnum(create.Type))
	}
}

// deletedContentKey reports the synced file a delete removes from its side,
// under the key a create of that content on the same side carries: its
// synced local identity on OneDrive, its item ID here.
func deletedContentKey(deleted *syncdomain.Action) (contentKey, bool) {
	if deleted.View == nil || deleted.View.Baseline == nil || deleted.View.Baseline.ItemType != syncdomain.ItemTypeFile {
		return contentKey{}, false
	}
	synced := deleted.View.Baseline
	switch deleted.Type {
	case syncdomain.ActionRemoteDelete:
		return contentKey{device: synced.LocalDevice, inode: synced.LocalInode}, synced.LocalHasIdentity
	case syncdomain.ActionLocalDelete:
		return contentKey{itemID: synced.ItemID}, synced.ItemID != ""
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove,
		syncdomain.ActionFolderCreate,
		syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return contentKey{}, false
	default:
		panic(syncdomain.OutsideTheEnum(deleted.Type))
	}
}

// carriesContentOf reports whether create writes, at another path on the
// delete's side, the content of the synced file the delete deleted removes:
// an upload of the file here that has the file's local identity for a delete
// on OneDrive, a download of the file's item for a delete here.
func carriesContentOf(create, deleted *syncdomain.Action) bool {
	if create.Path == deleted.Path {
		return false
	}
	carried, writes := carriedContentKey(create)
	removed, removes := deletedContentKey(deleted)

	return writes && removes && carried == removed
}

// addMoveDeps orders moves (D-054). Move composition emits a move only when
// these rules give it a consistent place in the plan
// (planner_move_composition.go), and every rule is expressed in the paths the
// actions carry:
//   - work at or under a move's destination runs after the move: the target
//     parent must exist, and a move composed into a folder an earlier move
//     places waits for it;
//   - a move onto the path another move of the same side vacates waits for it,
//     and so does a move into a folder below that path, compared as OneDrive
//     compares names, from a source outside it (movesIntoVacatedPath): that
//     folder is a new one, which takes the path only after the move, so the
//     order holds whether or not the plan creates it;
//   - a move waits for the work under its source that does not follow it to
//     its destination (children leave first, and writes addressed at the old
//     path land before the folder moves away), except a same-side move from
//     its source into or below a folder the plan creates at the path it
//     vacates (movesIntoRecreatedSource): that move waits for the create
//     through its parent folders (addParentFolderDep), and the create for
//     this move (createsAtVacatedSource), so it runs after this move, from a
//     source this move took away, and the next pass moves it. Move
//     composition names such a source only for a move it neither rebases
//     through the folder's move nor holds for the next pass (vacatingFolder).
//
// The partners of these rules are the moves at or above a's path and source,
// the moves vacating a's path, and, for a move, the moves whose source's name
// key lies above its path's, the work below its source and the old occupant
// of its destination; moveDepCandidates gathers them and moveOrderDependency
// decides each pair.
func addMoveDeps(deps []int, idx int, a *syncdomain.Action, actions []syncdomain.Action, index *dependencyIndex) []int {
	candidates := moveDepCandidates(idx, a, index)
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)

	sourceRecreated := isMoveAction(a) && slices.ContainsFunc(index.actionsByPath[a.OldPath], func(otherIdx int) bool {
		return recreatesMoveSource(&actions[otherIdx], a)
	})
	for _, otherIdx := range candidates {
		if otherIdx != idx && moveOrderDependency(a, &actions[otherIdx], sourceRecreated) {
			deps = append(deps, otherIdx)
		}
	}

	return deps
}

// recreatesMoveSource reports whether create is a folder create at the path
// move vacates, on the side move vacates it: a create that waits for the move
// (createsAtVacatedSource).
func recreatesMoveSource(create, move *syncdomain.Action) bool {
	return create.Type == syncdomain.ActionFolderCreate && move.OldPath != "" && create.Path == move.OldPath &&
		createsAtVacatedSource(create, move)
}

// movesIntoRecreatedSource reports whether other is a move to a path in or
// below the folder created at the path move vacates, whose creation
// sourceRecreated reports (recreatesMoveSource).
func movesIntoRecreatedSource(other, move *syncdomain.Action, sourceRecreated bool) bool {
	return sourceRecreated && isMoveAction(other) && strings.HasPrefix(other.Path, move.OldPath+"/")
}

// moveDepCandidates lists every action that can satisfy moveOrderDependency
// with a, possibly more than once.
func moveDepCandidates(idx int, a *syncdomain.Action, index *dependencyIndex) []int {
	if len(index.movesByPath) == 0 {
		return nil
	}

	var candidates []int
	candidates = append(candidates, index.movesByPath[a.Path]...)
	candidates = append(candidates, index.movesByOldPath[a.Path]...)
	for parent := range pathAncestors(a.Path) {
		candidates = append(candidates, index.movesByPath[parent]...)
		candidates = append(candidates, index.movesByOldPath[parent]...)
	}
	if !isMoveAction(a) {
		return candidates
	}

	for parent := range pathAncestors(a.OldPath) {
		candidates = append(candidates, index.movesByPath[parent]...)
	}
	// A name key's segments are its path's, so the keys above it are the
	// keys of the folders above the path.
	for parentKey := range pathAncestors(namekey.OfPath(a.Path)) {
		candidates = append(candidates, index.movesByOldNameKey[parentKey]...)
	}
	candidates = append(candidates, index.workBelowMoveSource[idx]...)
	candidates = append(candidates, index.actionsByPath[a.Path]...)

	return candidates
}

// moveOrderDependency reports whether a must wait for other under the move
// rules: other is a move that precedes a; a is a move and other is work
// under its source that does not follow it to the destination, unless other
// moves into or below a folder the plan creates at a's vacated source
// (sourceRecreated, movesIntoRecreatedSource); or a is a move and other
// removes the old occupant of its destination.
func moveOrderDependency(a, other *syncdomain.Action, sourceRecreated bool) bool {
	if isMoveAction(other) && movePrecedes(other, a) {
		return true
	}
	if !isMoveAction(a) {
		return false
	}
	if underMoveSource(other, a) && !underMoveDestination(other, a) &&
		!movesIntoRecreatedSource(other, a, sourceRecreated) {
		return true
	}

	return (isDeleteAction(other) || other.Type == syncdomain.ActionConflictCopy) && other.Path == a.Path
}

// isDeleteAction reports an action that removes an entry: a local or remote
// delete, or a cleanup of a baseline row both sides lost.
func isDeleteAction(a *syncdomain.Action) bool {
	switch a.Type {
	case syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete, syncdomain.ActionCleanup:
		return true
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove,
		syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate:
		return false
	default:
		panic(syncdomain.OutsideTheEnum(a.Type))
	}
}

// isMoveAction reports a move on either side.
func isMoveAction(a *syncdomain.Action) bool {
	switch a.Type {
	case syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove:
		return true
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete,
		syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return false
	default:
		panic(syncdomain.OutsideTheEnum(a.Type))
	}
}

// movePrecedes reports whether move must run before a.
func movePrecedes(move, a *syncdomain.Action) bool {
	if underMoveDestination(a, move) || createsAtVacatedSource(a, move) {
		return true
	}
	return isMoveAction(a) && a.Type == move.Type && (a.Path == move.OldPath || movesIntoVacatedPath(a, move))
}

// movesIntoVacatedPath reports whether a, a move of move's side, lands
// strictly below the path move vacates from a source outside it. What a lands
// in there is a new folder that takes the path once move has left it, so a
// runs after move, whether or not the plan also creates that folder.
// Unordered, a would land, run first, in the item move carries away, and find
// no folder, run second. A move from below the path is work that leaves first
// (underMoveSource). Both paths are compared as OneDrive compares names
// (namekey.OfPath): a new folder X takes the path x leaves there and on a volume
// that folds case.
func movesIntoVacatedPath(a, move *syncdomain.Action) bool {
	if move.OldPath == "" {
		return false
	}
	vacated := namekey.OfPath(move.OldPath)

	return belowPathKey(namekey.OfPath(a.Path), vacated) && !belowPathKey(namekey.OfPath(a.OldPath), vacated)
}

// belowPathKey reports whether the path of k lies below the path of root,
// not at it.
func belowPathKey(k, root namekey.PathKey) bool {
	return k != root && k.HasPrefix(root)
}

// underMoveDestination reports whether a works at or below move's
// destination.
func underMoveDestination(a, move *syncdomain.Action) bool {
	if a.Path == move.Path && !isMoveAction(a) && !isDeleteAction(a) && a.Type != syncdomain.ActionConflictCopy {
		return true
	}
	if strings.HasPrefix(a.Path, move.Path+"/") {
		return true
	}

	return isMoveAction(a) && strings.HasPrefix(a.OldPath, move.Path+"/")
}

// underMoveSource reports whether a works strictly below move's source in
// the coordinates the move vacates: a move on the same side by its source; for
// a local move, local writes (rebased to where the folder sits locally now);
// for a remote move, remote deletes addressed at the item's old remote path.
// Local creates under a remote move's old path belong to a new local entry
// there (rename and recreate), not to the moved item, so they do not hold it.
//
// A moved file has nothing below it: work under its old path belongs to the
// folder that takes the path, and waits for the move (createsAtVacatedSource).
func underMoveSource(a, move *syncdomain.Action) bool {
	if move.OldPath == "" || !movedItemIsFolder(move) ||
		!strings.HasPrefix(movedSourcePath(a, move), move.OldPath+"/") {
		return false
	}
	switch move.Type {
	case syncdomain.ActionLocalMove:
		// Local moves by their source and local writes: writesLocally is
		// true for a local move and false for a remote one.
		return writesLocally(a)
	case syncdomain.ActionRemoteMove:
		return a.Type == syncdomain.ActionRemoteMove || a.Type == syncdomain.ActionRemoteDelete || a.Type == syncdomain.ActionCleanup
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete,
		syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		// Not a move: it vacates no source for anything to lie below.
		return false
	default:
		panic(syncdomain.OutsideTheEnum(move.Type))
	}
}

func movedSourcePath(a, move *syncdomain.Action) string {
	if a.Type == move.Type {
		return a.OldPath
	}

	return a.Path
}

// createsAtVacatedSource reports a create of a new item at the path a move
// vacates on the create's side, or below it when the moved item is a file and
// a new folder takes its path: it must wait for the move (a new local entry
// at a remotely moved item's old path, D-056).
func createsAtVacatedSource(a, move *syncdomain.Action) bool {
	if move.OldPath == "" {
		return false
	}
	if a.Path != move.OldPath && (movedItemIsFolder(move) || !strings.HasPrefix(a.Path, move.OldPath+"/")) {
		return false
	}
	switch move.Type {
	case syncdomain.ActionRemoteMove:
		return createsOnSide(a, moveCoordRemote)
	case syncdomain.ActionLocalMove:
		return createsOnSide(a, moveCoordLocal)
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete,
		syncdomain.ActionFolderCreate, syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return false
	default:
		panic(syncdomain.OutsideTheEnum(move.Type))
	}
}

// movedItemIsFolder reports whether a move carries a folder, by the synced
// item it moves.
func movedItemIsFolder(move *syncdomain.Action) bool {
	if move.View != nil && move.View.Baseline != nil {
		return move.View.Baseline.ItemType == syncdomain.ItemTypeFolder
	}

	return syncdomain.ActionItemType(move) == syncdomain.ItemTypeFolder
}
