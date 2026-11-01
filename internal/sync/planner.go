package sync

import (
	"log/slog"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

// countByType counts actions grouped by ActionType. Exported for use by the
// sync engine when building pass reports from plan counts.
func countByType(actions []Action) map[actionType]int {
	counts := make(map[actionType]int)
	for i := range actions {
		counts[actions[i].Type]++
	}

	return counts
}

// isolateDependencyCycles builds the dependency graph and removes any actions
// that sit on a dependency cycle, together with everything that waits on them
// (D-059c). A cycle is a planner bug, but it used to abort the whole plan, so
// one bad pair of actions stopped every other change from syncing. The rest
// of the plan runs; the next pass replans the removed work from current truth.
func isolateDependencyCycles(actions []Action, logger *slog.Logger) ([]Action, [][]int) {
	for {
		deps := buildDependencies(actions)
		onCycle := dependencyCycleMembers(deps)
		if len(onCycle) == 0 {
			return actions, deps
		}

		dropped := withDependents(onCycle, deps)
		kept := make([]Action, 0, len(actions)-len(dropped))
		for i := range actions {
			if !dropped[i] {
				kept = append(kept, actions[i])
				continue
			}
			if logger != nil && onCycle[i] {
				logger.Error("planner dropped an action caught in a dependency cycle; it is replanned next pass",
					slog.String("action", actions[i].Type.String()),
					slog.String("path", actions[i].Path),
					slog.String("old_path", actions[i].OldPath),
				)
			}
		}
		actions = kept
	}
}

// dependencyCycleMembers returns the actions on a dependency cycle: members of
// a strongly connected component with more than one action, or depending on
// themselves (Tarjan's algorithm).
func dependencyCycleMembers(deps [][]int) map[int]bool {
	index := make([]int, len(deps))
	low := make([]int, len(deps))
	onStack := make([]bool, len(deps))
	for i := range index {
		index[i] = -1
	}
	var stack []int
	next := 0
	members := map[int]bool{}

	var visit func(v int)
	visit = func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range deps[v] {
			switch {
			case w == v:
				members[v] = true
			case index[w] < 0:
				visit(w)
				low[v] = min(low[v], low[w])
			case onStack[w]:
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		var component []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			component = append(component, w)
			if w == v {
				break
			}
		}
		if len(component) > 1 {
			for _, w := range component {
				members[w] = true
			}
		}
	}
	for v := range deps {
		if index[v] < 0 {
			visit(v)
		}
	}

	return members
}

// withDependents extends roots with every action that depends on one of them,
// directly or transitively.
func withDependents(roots map[int]bool, deps [][]int) map[int]bool {
	dropped := make(map[int]bool, len(roots))
	for v := range roots {
		dropped[v] = true
	}
	for changed := true; changed; {
		changed = false
		for v := range deps {
			if dropped[v] {
				continue
			}
			for _, w := range deps[v] {
				if dropped[w] {
					dropped[v] = true
					changed = true
					break
				}
			}
		}
	}

	return dropped
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
