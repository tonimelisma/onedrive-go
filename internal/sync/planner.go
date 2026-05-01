package sync

import (
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Planner is a pure decision engine that transforms SQLite-owned comparison
// and reconciliation rows plus baseline state into an ordered ActionPlan. It
// performs no I/O.
type Planner struct {
	logger *slog.Logger
}

type plannerMountContext struct {
	DriveID driveid.ID
}

// NewPlanner creates a Planner with the given logger.
func NewPlanner(logger *slog.Logger) *Planner {
	return &Planner{logger: logger}
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
		if action.CreateSide == CreateLocal {
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
func resolveItemType(view *PathView) ItemType {
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

// MakeAction constructs an Action with type, path, and IDs populated from
// the PathView.
//
// DriveID propagation contract:
//   - Remote.DriveID is authoritative for cross-drive items (shared folders
//     from Drive A appearing in Drive B's delta carry Drive A's DriveID).
//   - Baseline.DriveID is the fallback for items with no remote observation.
//   - Empty DriveID for brand-new local work — planner fills this from the
//     mounted engine drive before execution.
//   - Empty ItemID for new items — assigned by the API on creation.
func MakeAction(actionType ActionType, view *PathView) Action {
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

func makeConflictCopyAction(view *PathView) Action {
	return MakeAction(ActionConflictCopy, view)
}

func makeDownloadAfterConflictCopyAction(view *PathView) Action {
	action := MakeAction(ActionDownload, view)
	action.RequireMissingLocalTarget = true
	return action
}

func makeCreateUploadAction(view *PathView) Action {
	action := MakeAction(ActionUpload, view)
	action.ItemID = ""
	return action
}

// makeFolderCreate constructs an ActionFolderCreate action with the
// specified creation side (local or remote).
func makeFolderCreate(view *PathView, side FolderCreateSide) Action {
	a := MakeAction(ActionFolderCreate, view)
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
			preserved[i] = makeFolderCreate(action.View, CreateLocal)
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
	return isDelete && resolveItemType(action.View) == ItemTypeFolder
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

func actionRequiresParentFolder(actionType ActionType) bool {
	return actionType != ActionLocalDelete &&
		actionType != ActionCleanup &&
		actionType != ActionRemoteDelete
}

// buildDependencies computes dependency edges for a flat action list.
// Returns deps where deps[i] contains the indices that action i depends on.
// Rules: (1) folder create before any action in that subtree,
// (2) child delete/cleanup before parent folder delete,
// (3) move target parent must exist first.
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
		deps[i] = addRemoteMoveDeps(deps[i], i, &actions[i], actions)
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
	if a.Type != ActionDownload {
		return deps
	}

	if copyIdx, ok := conflictCopyIdx[a.Path]; ok && copyIdx != idx {
		deps = append(deps, copyIdx)
	}

	return deps
}

// addChildDeleteDeps makes folder deletes depend on child deletes at deeper paths.
func addChildDeleteDeps(deps []int, idx int, a *Action, deleteIdx map[string]int) []int {
	if a.Type != ActionLocalDelete && a.Type != ActionRemoteDelete {
		return deps
	}

	if resolveItemType(a.View) != ItemTypeFolder {
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

func addRemoteMoveDeps(deps []int, idx int, a *Action, actions []Action) []int {
	if a.Type == ActionRemoteMove || a.Type == ActionLocalDelete || a.Type == ActionRemoteDelete || a.Type == ActionCleanup {
		return deps
	}

	for moveIdx := range actions {
		move := &actions[moveIdx]
		if moveIdx == idx || move.Type != ActionRemoteMove {
			continue
		}
		if move.Path == a.Path {
			deps = append(deps, moveIdx)
			continue
		}
		if resolveItemType(move.View) == ItemTypeFolder && strings.HasPrefix(a.Path, move.Path+"/") {
			deps = append(deps, moveIdx)
		}
	}

	return deps
}

// CountByType counts actions grouped by ActionType. Exported for use by the
// sync engine when building pass reports from plan counts.
func CountByType(actions []Action) map[ActionType]int {
	counts := make(map[ActionType]int)
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
				return ErrDependencyCycle
			}
		}
	}

	return nil
}
