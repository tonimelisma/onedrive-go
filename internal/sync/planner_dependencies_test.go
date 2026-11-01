package sync

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// The index-based buildDependencies must produce exactly the edges the
// scanning implementation it replaced produced. That implementation is kept
// here as the oracle: every rule compared each action with every other
// action, which is unambiguous and quadratic.

// Validates: R-2.17.6, R-2.12.1, R-2.17.1
//
// Random action sets of the shapes the planner emits (moves of folders and
// files with work under their sources and destinations, moves onto vacated
// paths, folder deletes with child deletes and moves out of them, creates at
// vacated paths, case-, Unicode- and emoji-variant names, type-change
// replacements, conflict copies) build the same dependency lists under the
// indexed builder as under the scanning oracle, duplicates included.
func TestBuildDependencies_MatchesScanningOracle(t *testing.T) {
	t.Parallel()

	// The generator must reach every rule family, or the comparison proves
	// nothing about it; the counters say which edges the seeds produced.
	var edges, edgesOnMoves, edgesOnDeletes, edgesOnCaseVariants, edgesBelowFolderDeletes int

	const seeds = 400
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 0x6465_7073)) //nolint:gosec // deterministic test RNG
		actionCount := 1 + rng.IntN(80)
		if seed%100 == 0 {
			actionCount = 300 + rng.IntN(200)
		}
		actions := randomDependencyActions(rng, actionCount)

		want := oracleBuildDependencies(actions)
		got := buildDependencies(actions)

		if !assert.Equal(t, want, got, "seed %d", seed) {
			t.Logf("actions:\n%s", describeDependencyActions(actions))
			t.FailNow()
		}

		for i := range got {
			for _, depIdx := range got[i] {
				edges++
				dep := &actions[depIdx]
				switch {
				case isMoveAction(dep):
					edgesOnMoves++
				case isDeleteAction(dep) && isSubtreeDelete(&actions[i]):
					edgesBelowFolderDeletes++
				case isDeleteAction(dep):
					edgesOnDeletes++
				}
				if dep.Path != actions[i].Path && namekey.EqualPath(dep.Path, actions[i].Path) {
					edgesOnCaseVariants++
				}
			}
		}
	}

	t.Logf("edges %d: on moves %d, below folder deletes %d, on other deletes %d, on case variants %d",
		edges, edgesOnMoves, edgesBelowFolderDeletes, edgesOnDeletes, edgesOnCaseVariants)
	assert.Positive(t, edgesOnMoves)
	assert.Positive(t, edgesBelowFolderDeletes)
	assert.Positive(t, edgesOnDeletes)
	assert.Positive(t, edgesOnCaseVariants)
}

// Validates: R-2.17.6
func TestPathAncestors_InvertsHasPrefix(t *testing.T) {
	t.Parallel()

	paths := []string{"", "a", "a/b", "a/b/c.txt", "/lead", "trail/", "a//b", "a/b/", "trail", "a/", "b"}
	for _, path := range paths {
		var ancestors []string
		for parent := range pathAncestors(path) {
			ancestors = append(ancestors, parent)
			assert.True(t, strings.HasPrefix(path, parent+"/"), "%q is not above %q", parent, path)
		}
		for _, candidate := range paths {
			assert.Equal(t, strings.HasPrefix(path, candidate+"/"), slices.Contains(ancestors, candidate),
				"path %q, candidate %q", path, candidate)
		}
	}
}

// Validates: R-2.12.1
func TestPathNameKey_EqualPathsShareAKey(t *testing.T) {
	t.Parallel()

	variants := []string{"a/edit.txt", "A/EDIT.txt", "a/é.txt", "a/é.txt", "a/É.txt", "a/x😀.txt", "a/x.txt", "a/b/c", "ab/c", "a/b"}
	for _, x := range variants {
		for _, y := range variants {
			if namekey.EqualPath(x, y) {
				assert.Equal(t, pathNameKey(x), pathNameKey(y), "%q and %q are one OneDrive path", x, y)
			}
		}
	}
	assert.NotEqual(t, pathNameKey("a/b/c"), pathNameKey("ab/c"))
	assert.NotEqual(t, pathNameKey("a/b"), pathNameKey("a/b/c"))
}

// dependencyPathPool is a small, deeply related set of paths: children of
// earlier paths and name-folded twins of them are frequent, so the move,
// delete and case-variant rules all fire.
type dependencyPathPool struct {
	rng   *rand.Rand
	paths []string
}

// dependencyNameTwins pairs names OneDrive folds together: case, Unicode
// form (precomposed and decomposed é) and an emoji-only difference.
func dependencyNameTwins() [][2]string {
	return [][2]string{
		{"a", "A"}, {"b", "B"}, {"dir", "DIR"}, {"x.txt", "X.TXT"}, {"é.txt", "e\u0301.txt"}, {"f😀.txt", "f.txt"},
	}
}

func newDependencyPathPool(rng *rand.Rand, size int) *dependencyPathPool {
	var segments []string
	twins := make(map[string]string)
	for _, pair := range dependencyNameTwins() {
		segments = append(segments, pair[0], pair[1])
		twins[pair[0]], twins[pair[1]] = pair[1], pair[0]
	}
	segments = append(segments, "ab")

	pool := &dependencyPathPool{rng: rng, paths: []string{segments[rng.IntN(len(segments))]}}
	for len(pool.paths) < size {
		base := pool.pick()
		switch rng.IntN(4) {
		case 0:
			pool.paths = append(pool.paths, segments[rng.IntN(len(segments))])
		case 1:
			parts := strings.Split(base, "/")
			if twin, ok := twins[parts[len(parts)-1]]; ok {
				parts[len(parts)-1] = twin
				pool.paths = append(pool.paths, strings.Join(parts, "/"))
			}
		default:
			pool.paths = append(pool.paths, base+"/"+segments[rng.IntN(len(segments))])
		}
	}

	return pool
}

func (pool *dependencyPathPool) pick() string {
	return pool.paths[pool.rng.IntN(len(pool.paths))]
}

func (pool *dependencyPathPool) pickItemType() ItemType {
	if pool.rng.IntN(2) == 0 {
		return ItemTypeFolder
	}

	return ItemTypeFile
}

// pickView draws a view with any combination of sides present, each of
// either type, so deletedItemType and movedItemIsFolder see every case.
func (pool *dependencyPathPool) pickView(path string) *pathView {
	if pool.rng.IntN(4) == 0 {
		return nil
	}
	view := &pathView{Path: path}
	if pool.rng.IntN(3) > 0 {
		view.Baseline = &BaselineEntry{Path: path, ItemType: pool.pickItemType()}
	}
	if pool.rng.IntN(3) > 0 {
		view.Local = &localState{ItemType: pool.pickItemType()}
	}
	if pool.rng.IntN(3) > 0 {
		view.Remote = &remoteState{ItemType: pool.pickItemType()}
	}

	return view
}

// pickOldPath draws a move source: usually another pool path, sometimes the
// destination itself or empty.
func (pool *dependencyPathPool) pickOldPath(path string) string {
	switch pool.rng.IntN(12) {
	case 0:
		return ""
	case 1:
		return path
	default:
		return pool.pick()
	}
}

// randomDependencyActions draws actionCount actions of every type over a
// dependencyPathPool.
func randomDependencyActions(rng *rand.Rand, actionCount int) []Action {
	pool := newDependencyPathPool(rng, max(2, actionCount/2))
	types := []actionType{
		ActionUpload, ActionDownload, ActionFolderCreate, ActionLocalDelete, ActionRemoteDelete, ActionCleanup,
		ActionLocalMove, ActionRemoteMove, ActionLocalMove, ActionRemoteMove, ActionConflictCopy, ActionBaselineUpdate,
	}
	sides := []folderCreateSide{CreateRemote, createLocal}

	actions := make([]Action, 0, actionCount)
	for range actionCount {
		path := pool.pick()
		action := Action{Type: types[rng.IntN(len(types))], Path: path, View: pool.pickView(path)}
		switch action.Type {
		case ActionFolderCreate:
			action.CreateSide = sides[rng.IntN(len(sides))]
		case ActionLocalMove, ActionRemoteMove:
			action.OldPath = pool.pickOldPath(path)
		case ActionUpload, ActionDownload, ActionLocalDelete, ActionRemoteDelete,
			ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		}
		actions = append(actions, action)
	}

	return actions
}

func describeDependencyActions(actions []Action) string {
	var out strings.Builder
	for i := range actions {
		a := &actions[i]
		fmt.Fprintf(&out, "%3d %-14s %q", i, a.Type.String(), a.Path)
		if isMoveAction(a) {
			fmt.Fprintf(&out, " from %q", a.OldPath)
		}
		if a.Type == ActionFolderCreate {
			fmt.Fprintf(&out, " side=%v", a.CreateSide)
		}
		if a.View != nil {
			fmt.Fprintf(&out, " view{baseline=%v local=%v remote=%v}",
				viewItemType(a.View.Baseline != nil, func() ItemType { return a.View.Baseline.ItemType }),
				viewItemType(a.View.Local != nil, func() ItemType { return a.View.Local.ItemType }),
				viewItemType(a.View.Remote != nil, func() ItemType { return a.View.Remote.ItemType }))
		}
		out.WriteString("\n")
	}

	return out.String()
}

func viewItemType(present bool, itemType func() ItemType) string {
	if !present {
		return "-"
	}

	return itemType().String()
}

// oracleBuildDependencies is buildDependencies as it was before the index:
// every rule that relates two actions scans the whole list for each action.
func oracleBuildDependencies(actions []Action) [][]int {
	deps := make([][]int, len(actions))

	folderCreateIdx := make(map[string]int)
	conflictCopyIdx := make(map[string]int)
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
		deps[i] = oracleAddChildDeleteDeps(deps[i], i, &actions[i], deleteIdx)
		deps[i] = addSamePathReplacementDep(deps[i], i, &actions[i], deleteIdx)
		deps[i] = oracleAddMoveDeps(deps[i], i, &actions[i], actions)
		deps[i] = oracleAddMoveOutOfDeletedFolderDeps(deps[i], i, &actions[i], actions)
		deps[i] = oracleAddRemoteCaseVariantDeps(deps[i], i, &actions[i], actions)
		sort.Ints(deps[i])
	}

	return deps
}

func oracleAddChildDeleteDeps(deps []int, idx int, a *Action, deleteIdx map[string]int) []int {
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

func oracleAddMoveOutOfDeletedFolderDeps(deps []int, idx int, a *Action, actions []Action) []int {
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

func oracleAddRemoteCaseVariantDeps(deps []int, idx int, a *Action, actions []Action) []int {
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
			vacated = other.OldPath
		default:
			continue
		}
		if vacated == a.Path || vacated == a.OldPath || !namekey.EqualPath(vacated, a.Path) {
			continue
		}
		if a.Type == ActionRemoteMove && strings.HasPrefix(a.OldPath, vacated+"/") {
			continue
		}
		deps = append(deps, otherIdx)
	}

	return deps
}

func oracleAddMoveDeps(deps []int, idx int, a *Action, actions []Action) []int {
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
			deps = append(deps, otherIdx)
		}
	}

	return deps
}

// Validates: R-2.17.6
//
// The oracle itself must still be the quadratic scan the benchmark's
// "before" numbers were taken from, so a deliberate difference in a rule
// (a folder delete not waiting for a child delete) is visible to the
// equivalence test rather than silently shared by both implementations.
func TestBuildDependencies_OracleDetectsAMissingEdge(t *testing.T) {
	t.Parallel()

	folder := &pathView{Local: &localState{ItemType: ItemTypeFolder}}
	actions := []Action{
		{Type: ActionLocalDelete, Path: "d", View: folder},
		{Type: ActionLocalDelete, Path: "d/f.txt"},
	}

	want := oracleBuildDependencies(actions)
	require.Equal(t, [][]int{{1}, nil}, want)
	assert.Equal(t, want, buildDependencies(actions))
}
