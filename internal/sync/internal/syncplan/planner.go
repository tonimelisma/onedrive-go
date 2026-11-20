package syncplan

import (
	"log/slog"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// planner is a deterministic decision engine that transforms SQLite-owned
// comparison and reconciliation rows plus baseline state into an ordered
// ActionPlan. It performs no I/O apart from logging.
type Planner struct {
	logger *slog.Logger
}

// PlannerMountContext is what the plan knows of the mount it is for.
type PlannerMountContext struct {
	DriveID driveid.ID
	// ReleasesLocalTree is set for a removed shared folder's final drain
	// (RunOptions.ReleaseLocalTree), whose run ends by releasing the local
	// tree: its plan creates nothing new locally (leaveOutNewLocalItems).
	ReleasesLocalTree bool
}

// NewPlanner creates a planner with the given logger.
func NewPlanner(logger *slog.Logger) *Planner {
	return &Planner{logger: logger}
}

// actionAllowedInMode lists every action a mode admits. A value outside the
// action type or folder create side enum cannot reach it (the plan refuses
// one first, validatePlannedActions); admitting or deferring one would decide
// work no mode rule names, so its defaults panic.
func actionAllowedInMode(action *syncdomain.Action, mode syncdomain.SyncMode) bool {
	switch action.Type {
	case syncdomain.ActionDownload:
		return mode != syncdomain.SyncUploadOnly
	case syncdomain.ActionUpload:
		return mode != syncdomain.SyncDownloadOnly
	case syncdomain.ActionLocalDelete:
		return mode != syncdomain.SyncUploadOnly
	case syncdomain.ActionRemoteDelete:
		return mode != syncdomain.SyncDownloadOnly
	case syncdomain.ActionLocalMove:
		return mode != syncdomain.SyncUploadOnly
	case syncdomain.ActionRemoteMove:
		return mode != syncdomain.SyncDownloadOnly
	case syncdomain.ActionFolderCreate:
		switch action.CreateSide {
		case syncdomain.CreateLocal:
			return mode != syncdomain.SyncUploadOnly
		case syncdomain.CreateRemote:
			return mode != syncdomain.SyncDownloadOnly
		default:
			panic(syncdomain.OutsideTheEnum(action.CreateSide))
		}
	case syncdomain.ActionConflictCopy:
		// Conflict-copy is only safe when the paired remote-winner download
		// can also run. In upload-only mode that download is deferred, so keeping
		// the rename would mutate local truth into a fake delete/create sequence.
		return mode != syncdomain.SyncUploadOnly
	case syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return true
	default:
		panic(syncdomain.OutsideTheEnum(action.Type))
	}
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
func makeAction(actionType syncdomain.ActionType, view *syncdomain.PathView) syncdomain.Action {
	a := syncdomain.Action{
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

func bindMountContext(actions []syncdomain.Action, mount PlannerMountContext) {
	for i := range actions {
		if actions[i].DriveID.IsZero() {
			actions[i].DriveID = mount.DriveID
		}
	}
}

func makeConflictCopyAction(view *syncdomain.PathView) syncdomain.Action {
	return makeAction(syncdomain.ActionConflictCopy, view)
}

func makeDownloadAfterConflictCopyAction(view *syncdomain.PathView) syncdomain.Action {
	action := makeAction(syncdomain.ActionDownload, view)
	action.RequireMissingLocalTarget = true
	return action
}

func makeCreateUploadAction(view *syncdomain.PathView) syncdomain.Action {
	action := makeAction(syncdomain.ActionUpload, view)
	action.ItemID = ""
	return action
}

// makeFolderCreate constructs an ActionFolderCreate action with the
// specified creation side (local or remote).
func makeFolderCreate(view *syncdomain.PathView, side syncdomain.FolderCreateSide) syncdomain.Action {
	a := makeAction(syncdomain.ActionFolderCreate, view)
	a.CreateSide = side

	return a
}

func preserveParentFoldersForRunnableDescendants(actions []syncdomain.Action, mode syncdomain.SyncMode) []syncdomain.Action {
	preserved := append([]syncdomain.Action(nil), actions...)
	for i := range preserved {
		action := &preserved[i]
		if !isFolderDelete(action) || !runnableSubtreeActionRequiresParent(preserved, action.Path, mode) {
			continue
		}

		switch action.Type {
		case syncdomain.ActionLocalDelete:
			preserved[i] = makeFolderCreate(action.View, syncdomain.CreateRemote)
		case syncdomain.ActionRemoteDelete:
			preserved[i] = makeFolderCreate(action.View, syncdomain.CreateLocal)
		case syncdomain.ActionCleanup:
		case syncdomain.ActionDownload,
			syncdomain.ActionUpload,
			syncdomain.ActionLocalMove,
			syncdomain.ActionRemoteMove,
			syncdomain.ActionFolderCreate,
			syncdomain.ActionConflictCopy,
			syncdomain.ActionBaselineUpdate:
		default:
			panic(syncdomain.OutsideTheEnum(action.Type))
		}
	}

	return preserved
}

func isFolderDelete(action *syncdomain.Action) bool {
	if action == nil {
		return false
	}

	return isDeleteAction(action) && syncdomain.DeletedItemType(action) == syncdomain.ItemTypeFolder
}

// runnableSubtreeActionRequiresParent reports whether an admitted action below
// parentPath needs the folder to exist: anything but a delete or cleanup,
// which removes an entry and never needs its parent (addParentFolderDep).
func runnableSubtreeActionRequiresParent(actions []syncdomain.Action, parentPath string, mode syncdomain.SyncMode) bool {
	prefix := parentPath + "/"

	for i := range actions {
		action := &actions[i]
		if !strings.HasPrefix(action.Path, prefix) || !actionAllowedInMode(action, mode) {
			continue
		}
		if !isDeleteAction(action) {
			return true
		}
	}

	return false
}

// CountByType counts actions grouped by action type, for the plan summary log
// and the engine's pass report.
func CountByType(actions []syncdomain.Action) map[syncdomain.ActionType]int {
	counts := make(map[syncdomain.ActionType]int)
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
// It also returns the removed actions.
func isolateDependencyCycles(actions []syncdomain.Action, logger *slog.Logger) (kept []syncdomain.Action, deps [][]int, removed []syncdomain.Action) {
	for {
		deps = buildDependencies(actions)
		onCycle := dependencyCycleMembers(deps)
		if len(onCycle) == 0 {
			return actions, deps, removed
		}

		dropped := withDependents(onCycle, deps)
		kept = make([]syncdomain.Action, 0, len(actions)-len(dropped))
		for i := range actions {
			if !dropped[i] {
				kept = append(kept, actions[i])
				continue
			}
			removed = append(removed, actions[i])
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
// themselves.
func dependencyCycleMembers(deps [][]int) map[int]bool {
	_, members := dependencyComponents(deps)

	return members
}

// dependencyComponents numbers the strongly connected components of deps
// (Tarjan's algorithm): two actions share a number exactly when each waits
// for the other, directly or through others, so an edge lies on a cycle
// exactly when its ends share one. It also returns the actions on a cycle, as
// dependencyCycleMembers does.
func dependencyComponents(deps [][]int) (component []int, members map[int]bool) {
	index := make([]int, len(deps))
	low := make([]int, len(deps))
	onStack := make([]bool, len(deps))
	component = make([]int, len(deps))
	for i := range index {
		index[i] = -1
	}
	var stack []int
	next, components := 0, 0
	members = map[int]bool{}

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
		var scc []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			component[w] = components
			scc = append(scc, w)
			if w == v {
				break
			}
		}
		components++
		if len(scc) > 1 {
			for _, w := range scc {
				members[w] = true
			}
		}
	}
	for v := range deps {
		if index[v] < 0 {
			visit(v)
		}
	}

	return component, members
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
				return syncdomain.ErrDependencyCycle
			}
		}
	}

	return nil
}
