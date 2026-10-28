package sync

import (
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// planner is a pure decision engine that transforms SQLite-owned comparison
// and reconciliation rows plus baseline state into an ordered ActionPlan. It
// performs no I/O.
type planner struct {
	logger *slog.Logger
}

type plannerMountContext struct {
	DriveID driveid.ID
}

// newPlanner creates a Planner with the given logger.
func newPlanner(logger *slog.Logger) *planner {
	return &planner{logger: logger}
}

func actionAllowedInMode(action *Action, mode SyncMode) bool {
	switch action.Type {
	case ActionDownload:
		return mode != SyncUploadOnly
	case ActionUpload:
		return mode != SyncDownloadOnly
	case ActionLocalDelete:
		return mode != SyncUploadOnly
	case ActionRemoteDelete:
		return mode != SyncDownloadOnly
	case ActionLocalMove:
		return mode != SyncUploadOnly
	case ActionRemoteMove:
		return mode != SyncDownloadOnly
	case ActionFolderCreate:
		if action.CreateSide == createLocal {
			return mode != SyncUploadOnly
		}
		if action.CreateSide == CreateRemote {
			return mode != SyncDownloadOnly
		}
		return true
	case ActionConflictCopy:
		// Conflict-copy is only safe when the paired remote-winner download
		// can also run. In upload-only mode that download is deferred, so keeping
		// the rename would mutate local truth into a fake delete/create sequence.
		return mode != SyncUploadOnly
	case ActionBaselineUpdate, ActionCleanup:
		return true
	default:
		return true
	}
}

// resolveItemType determines the item type by checking Remote, Local,
// then Baseline. Defaults to ItemTypeFile if none provide a type.
//
// Special case: when the delta API reports a deleted item, it may strip
// the folder facet — making item.IsFolder=false even for folders. When
// Remote is deleted and its ItemType is the default (ItemTypeFile), we
// fall through to Baseline which has the correct type from when the item
// was alive. This ensures folder deletes are correctly identified for
// dependency ordering in buildDependencies/addChildDeleteDeps.
func resolveItemType(view *pathView) ItemType {
	if view == nil {
		return ItemTypeFile
	}

	if view.Remote != nil {
		// When the remote item is deleted, the delta API may omit the folder
		// facet, causing ItemType to default to ItemTypeFile. If a baseline
		// exists with a non-file type, prefer it — the baseline recorded the
		// correct type when the item was still alive.
		if view.Remote.IsDeleted &&
			view.Remote.ItemType == ItemTypeFile &&
			view.Baseline != nil &&
			view.Baseline.ItemType != ItemTypeFile {
			return view.Baseline.ItemType
		}

		return view.Remote.ItemType
	}

	if view.Local != nil {
		return view.Local.ItemType
	}

	if view.Baseline != nil {
		return view.Baseline.ItemType
	}

	return ItemTypeFile
}

// makeAction constructs an Action with type, path, and IDs populated from
// the PathView.
//
// DriveID propagation contract:
//   - Remote.DriveID is authoritative for cross-drive items (shared folders
//     from Drive A appearing in Drive B's delta carry Drive A's DriveID).
//   - Baseline.DriveID is the fallback for items with no remote observation.
//   - Empty DriveID for brand-new local work — planner fills this from the
//     mounted engine drive before execution.
//   - Empty ItemID for new items — assigned by the API on creation.
func makeAction(actionType actionType, view *pathView) Action {
	a := Action{
		Type: actionType,
		Path: view.Path,
		View: view,
	}

	// Remote provides ItemID and DriveID.
	if view.Remote != nil {
		a.ItemID = view.Remote.ItemID
	}

	// DriveID: prefer Remote (handles cross-drive items correctly),
	// fall back to Baseline (for items with no remote observation).
	if view.Remote != nil && !view.Remote.DriveID.IsZero() {
		a.DriveID = view.Remote.DriveID
	}

	if a.DriveID.IsZero() && view.Baseline != nil {
		a.DriveID = view.Baseline.DriveID
	}

	// Baseline provides a fallback ItemID when Remote is absent.
	if a.ItemID == "" && view.Baseline != nil {
		a.ItemID = view.Baseline.ItemID
	}

	return a
}

func bindMountContext(actions []Action, mount plannerMountContext) {
	for i := range actions {
		if actions[i].DriveID.IsZero() {
			actions[i].DriveID = mount.DriveID
		}
	}
}

func makeConflictCopyAction(view *pathView) Action {
	return makeAction(ActionConflictCopy, view)
}

func makeDownloadAfterConflictCopyAction(view *pathView) Action {
	action := makeAction(ActionDownload, view)
	action.RequireMissingLocalTarget = true
	return action
}

func makeCreateUploadAction(view *pathView) Action {
	action := makeAction(ActionUpload, view)
	action.ItemID = ""
	return action
}

// makeFolderCreate constructs an ActionFolderCreate action with the
// specified creation side (local or remote).
func makeFolderCreate(view *pathView, side folderCreateSide) Action {
	a := makeAction(ActionFolderCreate, view)
	a.CreateSide = side

	return a
}

func preserveParentFoldersForRunnableDescendants(actions []Action, mode SyncMode) []Action {
	preserved := append([]Action(nil), actions...)
	for i := range preserved {
		action := &preserved[i]
		if !isFolderDelete(action) || !runnableSubtreeActionRequiresParent(preserved, action.Path, mode) {
			continue
		}

		switch action.Type {
		case ActionLocalDelete:
			preserved[i] = makeFolderCreate(action.View, CreateRemote)
		case ActionRemoteDelete:
			preserved[i] = makeFolderCreate(action.View, createLocal)
		case ActionCleanup:
		case ActionDownload,
			ActionUpload,
			ActionLocalMove,
			ActionRemoteMove,
			ActionFolderCreate,
			ActionConflictCopy,
			ActionBaselineUpdate:
		}
	}

	return preserved
}

func isFolderDelete(action *Action) bool {
	if action == nil {
		return false
	}

	isDelete := action.Type == ActionLocalDelete ||
		action.Type == ActionRemoteDelete ||
		action.Type == ActionCleanup
	return isDelete && deletedItemType(action) == ItemTypeFolder
}

// deletedItemType is the type of the item a delete removes: the side it
// deletes from, when that side is known. It differs from resolveItemType only
// across a type change, where the two sides disagree and the view's preferred
// (remote) type would misdescribe a local delete.
func deletedItemType(action *Action) ItemType {
	if action == nil || action.View == nil {
		return resolveItemType(nil)
	}
	switch action.Type {
	case ActionLocalDelete:
		if action.View.Local != nil {
			return action.View.Local.ItemType
		}
	case ActionRemoteDelete:
		if action.View.Remote != nil {
			return action.View.Remote.ItemType
		}
	case ActionDownload, ActionUpload, ActionLocalMove, ActionRemoteMove,
		ActionFolderCreate, ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
	}

	return resolveItemType(action.View)
}

func runnableSubtreeActionRequiresParent(actions []Action, parentPath string, mode SyncMode) bool {
	prefix := parentPath + "/"

	for i := range actions {
		action := &actions[i]
		if !strings.HasPrefix(action.Path, prefix) || !actionAllowedInMode(action, mode) {
			continue
		}
		if actionRequiresParentFolder(action.Type) {
			return true
		}
	}

	return false
}

func actionRequiresParentFolder(actionType actionType) bool {
	return actionType != ActionLocalDelete &&
		actionType != ActionCleanup &&
		actionType != ActionRemoteDelete
}

// buildDependencies computes dependency edges for a flat action list.
// Returns deps where deps[i] contains the indices that action i depends on.
// Rules: (1) folder create before any action in that subtree,
// (2) child delete/cleanup before parent folder delete,
// (3) moves order against each other and against the work around them
// (addMoveDeps), (4) a folder delete waits for same-side moves that carry
// descendants out of it, (5) the create half of a type-change replacement
// waits for the delete at the same path, (6) a remote create waits for the
// remote delete or move-away of a sibling whose name differs only in case.
func buildDependencies(actions []Action) [][]int {
	deps := make([][]int, len(actions))

	// Index folder creates by path for quick lookup.
	folderCreateIdx := make(map[string]int)
	conflictCopyIdx := make(map[string]int)
	// Index all deletes by path for child→parent edges.
	deleteIdx := make(map[string]int)

	for i := range actions {
		if actions[i].Type == ActionFolderCreate {
			folderCreateIdx[actions[i].Path] = i
		}
		if actions[i].Type == ActionConflictCopy {
			conflictCopyIdx[actions[i].Path] = i
		}

		isDelete := actions[i].Type == ActionLocalDelete ||
			actions[i].Type == ActionRemoteDelete ||
			actions[i].Type == ActionCleanup
		if isDelete {
			deleteIdx[actions[i].Path] = i
		}
	}

	for i := range actions {
		deps[i] = addParentFolderDep(deps[i], i, &actions[i], folderCreateIdx)
		deps[i] = addConflictCopyDep(deps[i], i, &actions[i], conflictCopyIdx)
		deps[i] = addChildDeleteDeps(deps[i], i, &actions[i], deleteIdx)
		deps[i] = addSamePathReplacementDep(deps[i], i, &actions[i], deleteIdx)
		deps[i] = addMoveDeps(deps[i], i, &actions[i], actions)
		deps[i] = addMoveOutOfDeletedFolderDeps(deps[i], i, &actions[i], actions)
		deps[i] = addRemoteCaseVariantDeps(deps[i], i, &actions[i], actions)
		// Sort dependency indices for reproducible ordering (B-154).
		sort.Ints(deps[i])
	}

	return deps
}

// addParentFolderDep adds a dependency on a parent folder create if present.
func addParentFolderDep(deps []int, idx int, a *Action, folderCreateIdx map[string]int) []int {
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
func addChildDeleteDeps(deps []int, idx int, a *Action, deleteIdx map[string]int) []int {
	if a.Type != ActionLocalDelete && a.Type != ActionRemoteDelete {
		return deps
	}

	if deletedItemType(a) != ItemTypeFolder {
		return deps
	}

	prefix := a.Path + "/"

	for childPath, childIdx := range deleteIdx {
		if childIdx != idx && strings.HasPrefix(childPath, prefix) {
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
func addMoveOutOfDeletedFolderDeps(deps []int, idx int, a *Action, actions []Action) []int {
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
	if deletedItemType(a) != ItemTypeFolder {
		return deps
	}

	prefix := a.Path + "/"
	for moveIdx := range actions {
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
func addRemoteCaseVariantDeps(deps []int, idx int, a *Action, actions []Action) []int {
	switch {
	case a.Type == ActionUpload, a.Type == ActionRemoteMove:
	case a.Type == ActionFolderCreate && a.CreateSide == CreateRemote:
	default:
		return deps
	}

	for otherIdx := range actions {
		other := &actions[otherIdx]
		if otherIdx == idx {
			continue
		}
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
			continue
		}
		if vacated == a.Path || vacated == a.OldPath || !namekey.EqualPath(vacated, a.Path) {
			continue
		}
		if a.Type == ActionRemoteMove && strings.HasPrefix(a.OldPath, vacated+"/") {
			// The folder delete already waits for this move out of it.
			continue
		}
		deps = append(deps, otherIdx)
	}

	return deps
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
func addMoveDeps(deps []int, idx int, a *Action, actions []Action) []int {
	for otherIdx := range actions {
		if otherIdx == idx {
			continue
		}
		other := &actions[otherIdx]
		if isMoveAction(other) && movePrecedes(other, a) {
			deps = append(deps, otherIdx)
			continue
		}
		if isMoveAction(a) && underMoveSource(other, a) && !underMoveDestination(other, a) {
			deps = append(deps, otherIdx)
			continue
		}
		if isMoveAction(a) && (isDeleteAction(other) || other.Type == ActionConflictCopy) && other.Path == a.Path {
			// The old occupant of the destination leaves first.
			deps = append(deps, otherIdx)
		}
	}

	return deps
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
func underMoveSource(a, move *Action) bool {
	if move.OldPath == "" || !strings.HasPrefix(movedSourcePath(a, move), move.OldPath+"/") {
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
// vacates on the create's side: it must wait for the move (a new local entry
// at a remotely moved item's old path, D-056).
func createsAtVacatedSource(a, move *Action) bool {
	if a.Path != move.OldPath || move.OldPath == "" {
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

// countByType counts actions grouped by ActionType. Exported for use by the
// sync engine when building pass reports from plan counts.
func countByType(actions []Action) map[actionType]int {
	counts := make(map[actionType]int)
	for i := range actions {
		counts[actions[i].Type]++
	}

	return counts
}

// detectDependencyCycle performs a DFS to check for cycles in the dependency
// graph. Returns ErrDependencyCycle if any cycle is found. Uses standard
// white/gray/black three-color marking (B-313).
func detectDependencyCycle(deps [][]int) error {
	const (
		white = 0 // unvisited
		gray  = 1 // in current DFS path
		black = 2 // fully explored, no cycle
	)

	color := make([]int, len(deps))

	var dfs func(node int) bool
	dfs = func(node int) bool {
		color[node] = gray

		for _, neighbor := range deps[node] {
			switch color[neighbor] {
			case gray:
				return true // back edge → cycle
			case white:
				if dfs(neighbor) {
					return true
				}
			}
		}

		color[node] = black

		return false
	}

	for i := range deps {
		if color[i] == white {
			if dfs(i) {
				return errDependencyCycle
			}
		}
	}

	return nil
}
