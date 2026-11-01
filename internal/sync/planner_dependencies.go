package sync

import (
	"iter"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// buildDependencies computes dependency edges for a flat action list.
// Returns deps where deps[i] contains the indices that action i depends on.
// Rules: (1) folder create before any action in that subtree,
// (2) child delete/cleanup before parent folder delete,
// (3) moves order against each other and against the work around them
// (addMoveDeps), (4) a folder delete waits for same-side moves that carry
// descendants out of it, (5) the create half of a type-change replacement
// waits for the delete at the same path, (6) a remote create waits for the
// remote delete or move-away of a sibling whose name differs only in case.
//
// Every rule relates an action to actions at its own path, at the folders
// above it, or at the OneDrive name key of its path, so the partners are
// found through indexes built once (dependencyIndex) and each rule costs the
// path's depth, not the plan's size. The rules used to scan the whole list
// per action, which was 91 % of a 34-minute first plan of 154k actions
// (spec/design/performance-benchmarking.md).
func buildDependencies(actions []Action) [][]int {
	deps := make([][]int, len(actions))
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

	return deps
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
	actionsByPath  map[string][]int

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
	vacatedByNameKey map[string][]int
}

func newDependencyIndex(actions []Action) *dependencyIndex {
	index := &dependencyIndex{
		folderCreateIdx:        make(map[string]int),
		conflictCopyIdx:        make(map[string]int),
		deleteIdx:              make(map[string]int),
		movesByPath:            make(map[string][]int),
		movesByOldPath:         make(map[string][]int),
		actionsByPath:          make(map[string][]int),
		childDeletes:           make(map[int][]int),
		movesBelowFolderDelete: make(map[int][]int),
		workBelowMoveSource:    make(map[int][]int),
		vacatedByNameKey:       make(map[string][]int),
	}
	folderDeletesByPath := make(map[string][]int)
	for i := range actions {
		index.indexByPath(i, &actions[i], folderDeletesByPath)
	}
	index.linkWorkBelowFolders(actions, folderDeletesByPath)
	index.indexVacatedNames(actions)

	return index
}

func (index *dependencyIndex) indexByPath(i int, a *Action, folderDeletesByPath map[string][]int) {
	index.actionsByPath[a.Path] = append(index.actionsByPath[a.Path], i)
	switch {
	case a.Type == ActionFolderCreate:
		index.folderCreateIdx[a.Path] = i
	case a.Type == ActionConflictCopy:
		index.conflictCopyIdx[a.Path] = i
	case isMoveAction(a):
		index.movesByPath[a.Path] = append(index.movesByPath[a.Path], i)
		index.movesByOldPath[a.OldPath] = append(index.movesByOldPath[a.OldPath], i)
	case isDeleteAction(a):
		index.deleteIdx[a.Path] = i
		if isSubtreeDelete(a) {
			folderDeletesByPath[a.Path] = append(folderDeletesByPath[a.Path], i)
		}
	}
}

// linkWorkBelowFolders fills the per-action candidate lists by walking the
// folders above each action's path and move source.
func (index *dependencyIndex) linkWorkBelowFolders(actions []Action, folderDeletesByPath map[string][]int) {
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
func (index *dependencyIndex) indexVacatedNames(actions []Action) {
	for i := range actions {
		a := &actions[i]
		switch a.Type {
		case ActionRemoteDelete:
			key := pathNameKey(a.Path)
			index.vacatedByNameKey[key] = append(index.vacatedByNameKey[key], i)
		case ActionRemoteMove:
			key := pathNameKey(a.OldPath)
			index.vacatedByNameKey[key] = append(index.vacatedByNameKey[key], i)
		case ActionDownload, ActionUpload, ActionLocalDelete, ActionLocalMove,
			ActionFolderCreate, ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		}
	}
}

// pathAncestors yields every prefix p of path such that path lies below p,
// that is path has the prefix p + "/": the folders above the path, nearest
// last. It is the exact inverse of strings.HasPrefix(path, p+"/").
func pathAncestors(path string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for i := range len(path) {
			if path[i] == '/' && !yield(path[:i]) {
				return
			}
		}
	}
}

// pathNameKey is the key under which OneDrive's namespace holds a path: the
// name key of each segment (namekey.Key). Two paths that namekey.EqualPath
// reports equal have the same key, so a lookup by key finds every candidate
// and the rule then applies EqualPath itself.
func pathNameKey(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = namekey.Key(segment)
	}

	return strings.Join(segments, "/")
}

// isSubtreeDelete reports a local or remote delete of a folder: the deletes
// that wait for the work below their path (addChildDeleteDeps,
// addMoveOutOfDeletedFolderDeps). A cleanup removes only a baseline row.
func isSubtreeDelete(a *Action) bool {
	if a.Type != ActionLocalDelete && a.Type != ActionRemoteDelete {
		return false
	}

	return deletedItemType(a) == ItemTypeFolder
}

// addParentFolderDep adds a dependency on a parent folder create if present.
// A delete or cleanup removes an entry of the old item at that path and
// never needs the parent created; waiting on a re-created parent closed a
// cycle with the move that vacates it (a folder moved and re-created).
func addParentFolderDep(deps []int, idx int, a *Action, folderCreateIdx map[string]int) []int {
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

func addConflictCopyDep(deps []int, idx int, a *Action, conflictCopyIdx map[string]int) []int {
	if a.Type != ActionDownload && (a.Type != ActionFolderCreate || a.CreateSide != createLocal) {
		return deps
	}

	if copyIdx, ok := conflictCopyIdx[a.Path]; ok && copyIdx != idx {
		deps = append(deps, copyIdx)
	}

	return deps
}

// addSamePathReplacementDep makes the create half of a type-change
// replacement wait for the delete of the old item at the same path.
func addSamePathReplacementDep(deps []int, idx int, a *Action, deleteIdx map[string]int) []int {
	if a.Type != ActionUpload && a.Type != ActionDownload && a.Type != ActionFolderCreate {
		return deps
	}
	if delIdx, ok := deleteIdx[a.Path]; ok && delIdx != idx {
		deps = append(deps, delIdx)
	}

	return deps
}

// addChildDeleteDeps makes folder deletes depend on child deletes at deeper paths.
func addChildDeleteDeps(deps []int, idx int, a *Action, index *dependencyIndex) []int {
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
func addMoveOutOfDeletedFolderDeps(deps []int, idx int, a *Action, actions []Action, index *dependencyIndex) []int {
	var sameSideMove actionType
	switch a.Type {
	case ActionRemoteDelete:
		sameSideMove = ActionRemoteMove
	case ActionLocalDelete:
		sameSideMove = ActionLocalMove
	case ActionDownload, ActionUpload, ActionLocalMove, ActionRemoteMove,
		ActionFolderCreate, ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		return deps
	default:
		return deps
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
func addRemoteCaseVariantDeps(deps []int, idx int, a *Action, actions []Action, index *dependencyIndex) []int {
	switch {
	case a.Type == ActionUpload, a.Type == ActionRemoteMove:
	case a.Type == ActionFolderCreate && a.CreateSide == CreateRemote:
	default:
		return deps
	}
	if len(index.vacatedByNameKey) == 0 {
		return deps
	}

	for _, otherIdx := range index.vacatedByNameKey[pathNameKey(a.Path)] {
		if otherIdx != idx && vacatesCaseVariantOf(&actions[otherIdx], a) {
			deps = append(deps, otherIdx)
		}
	}

	return deps
}

// vacatesCaseVariantOf reports whether other frees a OneDrive name that a's
// path differs from only under name folding, so a must wait for it.
func vacatesCaseVariantOf(other, a *Action) bool {
	var vacated string
	switch {
	case other.Type == ActionRemoteDelete:
		vacated = other.Path
	case other.Type == ActionRemoteMove && a.Type != ActionRemoteMove:
		// Move-to-move edges are skipped: two moves that trade
		// case-variant names would wait on each other. A blocked rename
		// fails on its name conflict and is retried instead.
		vacated = other.OldPath
	default:
		return false
	}
	if vacated == a.Path || vacated == a.OldPath || !namekey.EqualPath(vacated, a.Path) {
		return false
	}
	if a.Type == ActionRemoteMove && strings.HasPrefix(a.OldPath, vacated+"/") {
		// The folder delete already waits for this move out of it.
		return false
	}

	return true
}

// addMoveDeps orders moves (D-054). Move composition emits a move only when
// these rules give it a consistent place in the plan
// (planner_move_composition.go), and every rule is expressed in the paths the
// actions carry:
//   - work at or under a move's destination runs after the move: the target
//     parent must exist, and a move composed into a folder an earlier move
//     places waits for it;
//   - a move onto the path another move of the same side vacates waits for it;
//   - a move waits for the work under its source that does not follow it to
//     its destination (children leave first, and writes addressed at the old
//     path land before the folder moves away).
//
// Before, only remote moves ordered anything, local moves fabricated their
// missing parents with MkdirAll, and a child moved into a folder renamed in
// the same plan created a stray copy of the folder.
//
// The partners of these rules are the moves at or above a's path and source,
// the moves vacating a's path, and, for a move, the work below its source
// and the old occupant of its destination; moveDepCandidates gathers them
// and moveOrderDependency decides each pair.
func addMoveDeps(deps []int, idx int, a *Action, actions []Action, index *dependencyIndex) []int {
	candidates := moveDepCandidates(idx, a, index)
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)

	for _, otherIdx := range candidates {
		if otherIdx != idx && moveOrderDependency(a, &actions[otherIdx]) {
			deps = append(deps, otherIdx)
		}
	}

	return deps
}

// moveDepCandidates lists every action that can satisfy moveOrderDependency
// with a, possibly more than once.
func moveDepCandidates(idx int, a *Action, index *dependencyIndex) []int {
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
	candidates = append(candidates, index.workBelowMoveSource[idx]...)
	candidates = append(candidates, index.actionsByPath[a.Path]...)

	return candidates
}

// moveOrderDependency reports whether a must wait for other under the move
// rules: other is a move that precedes a; a is a move and other is work
// under its source that does not follow it to the destination; or a is a
// move and other removes the old occupant of its destination.
func moveOrderDependency(a, other *Action) bool {
	if isMoveAction(other) && movePrecedes(other, a) {
		return true
	}
	if !isMoveAction(a) {
		return false
	}
	if underMoveSource(other, a) && !underMoveDestination(other, a) {
		return true
	}

	return (isDeleteAction(other) || other.Type == ActionConflictCopy) && other.Path == a.Path
}

func isDeleteAction(a *Action) bool {
	return a.Type == ActionLocalDelete || a.Type == ActionRemoteDelete || a.Type == ActionCleanup
}

func isMoveAction(a *Action) bool {
	return a.Type == ActionLocalMove || a.Type == ActionRemoteMove
}

// movePrecedes reports whether move must run before a.
func movePrecedes(move, a *Action) bool {
	if underMoveDestination(a, move) || createsAtVacatedSource(a, move) {
		return true
	}
	return isMoveAction(a) && a.Type == move.Type && a.Path == move.OldPath
}

// underMoveDestination reports whether a works at or below move's
// destination.
func underMoveDestination(a, move *Action) bool {
	if a.Path == move.Path && !isMoveAction(a) && !isDeleteAction(a) && a.Type != ActionConflictCopy {
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
func underMoveSource(a, move *Action) bool {
	if move.OldPath == "" || !movedItemIsFolder(move) ||
		!strings.HasPrefix(movedSourcePath(a, move), move.OldPath+"/") {
		return false
	}
	switch {
	case a.Type == move.Type:
		return true
	case isMoveAction(a):
		return false
	case move.Type == ActionLocalMove:
		return writesLocally(a)
	default:
		return a.Type == ActionRemoteDelete || a.Type == ActionCleanup
	}
}

func movedSourcePath(a, move *Action) string {
	if a.Type == move.Type {
		return a.OldPath
	}

	return a.Path
}

// createsAtVacatedSource reports a create of a new item at the path a move
// vacates on the create's side, or below it when the moved item is a file and
// a new folder takes its path: it must wait for the move (a new local entry
// at a remotely moved item's old path, D-056).
func createsAtVacatedSource(a, move *Action) bool {
	if move.OldPath == "" {
		return false
	}
	if a.Path != move.OldPath && (movedItemIsFolder(move) || !strings.HasPrefix(a.Path, move.OldPath+"/")) {
		return false
	}
	switch move.Type {
	case ActionRemoteMove:
		return a.Type == ActionUpload || (a.Type == ActionFolderCreate && a.CreateSide == CreateRemote)
	case ActionLocalMove:
		return a.Type == ActionDownload || (a.Type == ActionFolderCreate && a.CreateSide == createLocal)
	case ActionDownload, ActionUpload, ActionLocalDelete, ActionRemoteDelete,
		ActionFolderCreate, ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		return false
	default:
		return false
	}
}

// movedItemIsFolder reports whether a move carries a folder, by the synced
// item it moves.
func movedItemIsFolder(move *Action) bool {
	if move.View != nil && move.View.Baseline != nil {
		return move.View.Baseline.ItemType == ItemTypeFolder
	}

	return actionItemType(move) == ItemTypeFolder
}
