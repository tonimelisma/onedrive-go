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

// The index-based buildDependencies must find every pair of actions a rule
// accepts. The oracle here scans for them: every rule compares each action
// with every other action, which is unambiguous and quadratic. For moves both
// apply the same pair predicates (movePrecedes, underMoveSource, ...), so
// there the comparison proves the indexes complete, not what a predicate
// accepts; the rows of each rule hold that.

// Validates: R-2.17.6, R-2.12.1, R-2.17.1
//
// Random action sets of the shapes the planner emits (moves of folders and
// files with work under their sources and destinations, moves onto vacated
// paths, folder deletes with child deletes and moves out of them, creates at
// vacated paths, case-, Unicode- and emoji-variant names, type-change
// replacements, conflict copies, deletes and creates of the same content)
// build the same dependency lists under the indexed builder as under the
// scanning oracle, duplicates included.
func TestBuildDependencies_MatchesScanningOracle(t *testing.T) {
	t.Parallel()

	// The generator must reach every rule family, or the comparison proves
	// nothing about it; the counters say which edges the seeds produced.
	var edges, edgesOnMoves, edgesOnDeletes, edgesOnCaseVariants, edgesBelowFolderDeletes, edgesOnContent int

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
				case isDeleteAction(&actions[i]):
					// Only the content rule makes a delete wait for a create.
					edgesOnContent++
				}
				if dep.Path != actions[i].Path && namekey.EqualPath(dep.Path, actions[i].Path) {
					edgesOnCaseVariants++
				}
			}
		}
	}

	t.Logf("edges %d: on moves %d, below folder deletes %d, on other deletes %d, on case variants %d, on content %d",
		edges, edgesOnMoves, edgesBelowFolderDeletes, edgesOnDeletes, edgesOnCaseVariants, edgesOnContent)
	assert.Positive(t, edgesOnMoves)
	assert.Positive(t, edgesBelowFolderDeletes)
	assert.Positive(t, edgesOnDeletes)
	assert.Positive(t, edgesOnCaseVariants)
	assert.Positive(t, edgesOnContent)
}

// Validates: R-2.17.2, R-2.17.6
//
// A delete waits for the create on its side that writes the deleted file's
// content at another path: OneDrive's delete of z for the upload of the file
// here that has z's local identity (z renamed onto P/f, a rename SQLite could
// not pair), and the delete here of y for the download of y's item at q/y.
// So a create that fails, or fails to get what it waits for, keeps the delete
// from running and the content on its side. A delete waits for no create of
// other content, nor for one on the other side, nor for the create at its own
// path, which the same-path rule orders after it: that create is no content
// edge at all, not even one left out as closing a cycle.
func TestBuildDependencies_ADeleteWaitsForTheCreateThatWritesItsContent(t *testing.T) {
	t.Parallel()

	downloadOf := func(p, itemID string) Action {
		return Action{Type: ActionDownload, Path: p, ItemID: itemID, View: &pathView{}}
	}
	actions := []Action{
		{Type: ActionRemoteDelete, Path: "z", View: syncedFileView(7, "item-z")},
		uploadOfIdentity("P/f", 7),
		{Type: ActionLocalDelete, Path: "y", View: syncedFileView(8, "item-y")},
		downloadOf("q/y", "item-y"),
		{Type: ActionRemoteDelete, Path: "w", View: syncedFileView(9, "item-w")},
		uploadOfIdentity("v", 10),
		downloadOf("w2", "item-w"),
		uploadOfIdentity("y2", 8),
		{Type: ActionRemoteDelete, Path: "e", View: syncedFileView(11, "item-e")},
		uploadOfIdentity("e", 11),
	}

	deps, leftOut := buildDependencyGraph(actions)

	assert.Equal(t, []int{1}, deps[0], "OneDrive's delete of z waits for the upload at P/f of z's file")
	assert.Equal(t, []int{3}, deps[2],
		"the delete of y here waits for the download of y's item, not for the upload of y's file at y2")
	assert.Empty(t, deps[4], "the upload of v carries other content, the download at w2 is on the other side")
	assert.Empty(t, deps[8])
	assert.Equal(t, []int{8}, deps[9], "the same-path rule orders the upload at e after e's delete")
	assert.Empty(t, leftOut, "no content edge closes a cycle")
	require.NoError(t, detectDependencyCycle(deps))
}

// Validates: R-2.17.2, R-2.17.6
//
// Where the create that writes a deleted file's content waits for the delete
// through the other rules, the delete does not wait for it: the edge would
// close a cycle, and both would be dropped on every pass. The upload of
// a1.txt, edited in a folder A renamed B here, waits for A's move, which
// waits for the delete of A/a1.txt below its source; the upload of x, moved
// here up onto the path of its folder P, which is deleted, waits for P's
// delete, which waits for the delete of P/x. In the same plan the delete of
// z, renamed here into B, still waits for its upload.
func TestBuildDependencies_ADeleteDoesNotWaitForACreateThatWaitsForIt(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}, Remote: &remoteState{ItemType: ItemTypeFolder}}
	actions := []Action{
		{Type: ActionRemoteMove, OldPath: "A", Path: "B", View: folder},
		{Type: ActionRemoteDelete, Path: "A/a1.txt", View: syncedFileView(7, "item-a1")},
		uploadOfIdentity("B/a1.txt", 7),
		{Type: ActionRemoteDelete, Path: "P", View: folder},
		{Type: ActionRemoteDelete, Path: "P/x", View: syncedFileView(8, "item-x")},
		uploadOfIdentity("P", 8),
		{Type: ActionRemoteDelete, Path: "z", View: syncedFileView(9, "item-z")},
		uploadOfIdentity("B/z", 9),
	}

	deps := buildDependencies(actions)

	assert.Equal(t, []int{1}, deps[0], "A's move waits for the delete below its source")
	assert.Empty(t, deps[1], "a1.txt's old copy does not wait for the upload after A's move")
	assert.Equal(t, []int{0}, deps[2])
	assert.Equal(t, []int{4}, deps[3])
	assert.Empty(t, deps[4], "P/x does not wait for the upload after P's delete")
	assert.Equal(t, []int{3}, deps[5])
	assert.Equal(t, []int{7}, deps[6], "z's delete waits for its upload")
	assert.Equal(t, []int{0}, deps[7])
	require.NoError(t, detectDependencyCycle(deps))
}

// Validates: R-2.17.2, R-2.17.6
//
// Two content edges can close a cycle only together: OneDrive deletes a.txt
// and b.txt while each one's content is uploaded at the other's name in
// another case (B.txt, A.txt), and each upload waits for the delete of the
// name it takes. The edges are decided one at a time, so the first, which
// closes no cycle alone, is kept: a.txt's delete waits for the upload of its
// content, and only b.txt's edge, whose upload then waits for b.txt's delete
// through it, is left out. Left out together, neither upload reached its
// delete, so both deletes waited for the next pass, and with them the
// uploads: the plan withheld all four actions on every pass. Now the plan
// keeps all four.
func TestBuildDependencies_OfTwoContentEdgesThatCloseACycleOnlyTogetherTheFirstIsKept(t *testing.T) {
	t.Parallel()

	actions := []Action{
		{Type: ActionRemoteDelete, Path: "a.txt", View: syncedFileView(1, "item-a")},
		uploadOfIdentity("B.txt", 1),
		{Type: ActionRemoteDelete, Path: "b.txt", View: syncedFileView(2, "item-b")},
		uploadOfIdentity("A.txt", 2),
	}

	deps, leftOut := buildDependencyGraph(actions)

	assert.Equal(t, []int{1}, deps[0], "a.txt's delete waits for the upload of its content")
	assert.Equal(t, []int{2}, deps[1], "B.txt's upload waits for b.txt's delete")
	assert.Empty(t, deps[2], "b.txt's delete waits for nothing")
	assert.Equal(t, []int{0}, deps[3], "A.txt's upload waits for a.txt's delete")
	assert.Equal(t, []contentEdge{{deleteIdx: 2, createIdx: 3}}, leftOut)
	require.NoError(t, detectDependencyCycle(deps))

	kept, withheld, _ := withholdDeletesOfHeldWork(nil, slices.Clone(actions), nil, &moveInputs{}, &moveComposition{}, SyncBidirectional)
	assert.Len(t, kept, len(actions), "the plan keeps every action")
	assert.Zero(t, withheld)
}

// Validates: R-2.17.2, R-2.17.6
//
// carriedPath follows the plan's moves on the delete's side from the deleted
// entry's path: the move of the nearest folder above it, then the nearest
// move above where that one puts it that runs after it. On OneDrive a
// subfolder's move is rebased below its folder's new path and runs after the
// folder's; here a subfolder moves first, from below the folder's old path. A
// subfolder leaving its folder first is the nearest move; a folder whose path
// another move takes moved first and carries the entry nowhere; a move on the
// other side carries nothing.
func TestCarriedPath_FollowsTheMovesThatCarryTheDeletedEntry(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}, Remote: &remoteState{ItemType: ItemTypeFolder}}
	remoteMove := func(from, to string) Action {
		return Action{Type: ActionRemoteMove, OldPath: from, Path: to, View: folder}
	}
	localMove := func(from, to string) Action {
		return Action{Type: ActionLocalMove, OldPath: from, Path: to, View: folder}
	}
	for _, tc := range []struct {
		name    string
		moves   []Action
		deleted Action
		want    string
	}{
		{
			name:    "no move above it",
			moves:   []Action{remoteMove("x", "y")},
			deleted: Action{Type: ActionRemoteDelete, Path: "P/f"},
			want:    "P/f",
		},
		{
			name:    "its folder renamed on OneDrive",
			moves:   []Action{remoteMove("P", "Q")},
			deleted: Action{Type: ActionRemoteDelete, Path: "P/f"},
			want:    "Q/f",
		},
		{
			name:    "a renamed subfolder of a renamed folder on OneDrive",
			moves:   []Action{remoteMove("P", "Q"), remoteMove("Q/S", "Q/T")},
			deleted: Action{Type: ActionRemoteDelete, Path: "P/S/f"},
			want:    "Q/T/f",
		},
		{
			name:    "a renamed subfolder of a renamed folder here",
			moves:   []Action{localMove("P", "Q"), localMove("P/S", "P/T")},
			deleted: Action{Type: ActionLocalDelete, Path: "P/S/f"},
			want:    "Q/T/f",
		},
		{
			name:    "a subfolder leaving its renamed folder first",
			moves:   []Action{remoteMove("P", "Q"), remoteMove("P/S", "R/S")},
			deleted: Action{Type: ActionRemoteDelete, Path: "P/S/f"},
			want:    "R/S/f",
		},
		{
			name:    "a folder moved to the path a renamed folder leaves",
			moves:   []Action{remoteMove("P", "Q"), remoteMove("X", "P")},
			deleted: Action{Type: ActionRemoteDelete, Path: "X/f"},
			want:    "P/f",
		},
		{
			name:    "a subfolder moved on the other side",
			moves:   []Action{remoteMove("P", "Q"), localMove("P/S", "P/U")},
			deleted: Action{Type: ActionRemoteDelete, Path: "P/S/f"},
			want:    "Q/S/f",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			actions := append(slices.Clone(tc.moves), tc.deleted)
			deps := buildDependencies(actions)
			got := carriedPath(actions, deps, movesBySource(actions), &actions[len(actions)-1])
			assert.Equal(t, tc.want, got)
		})
	}
}

// syncedFileView is the view of a synced file with a local identity on
// device 1.
func syncedFileView(inode uint64, itemID string) *pathView {
	return &pathView{Baseline: &BaselineEntry{
		ItemType: ItemTypeFile, ItemID: itemID, LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
	}}
}

// uploadOfIdentity uploads the file here at p that has inode on device 1.
func uploadOfIdentity(p string, inode uint64) Action {
	return Action{Type: ActionUpload, Path: p, View: &pathView{Local: &localState{
		ItemType: ItemTypeFile, LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
	}}}
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

// dependencyContentPool is how many local identities and item IDs the
// generator shares out, so deletes and creates of the same content are
// frequent.
const dependencyContentPool = 4

// pickView draws a view with any combination of sides present, each of
// either type, so deletedItemType and movedItemIsFolder see every case. The
// baseline and the local side may carry an identity, and the baseline an
// item ID, drawn from a small pool (addContentCarrierDeps).
func (pool *dependencyPathPool) pickView(path string) *pathView {
	if pool.rng.IntN(4) == 0 {
		return nil
	}
	view := &pathView{Path: path}
	if pool.rng.IntN(3) > 0 {
		inode, hasIdentity := pool.pickIdentity()
		view.Baseline = &BaselineEntry{
			Path: path, ItemType: pool.pickItemType(), ItemID: pool.pickItemID(),
			LocalInode: inode, LocalHasIdentity: hasIdentity,
		}
	}
	if pool.rng.IntN(3) > 0 {
		inode, hasIdentity := pool.pickIdentity()
		view.Local = &localState{ItemType: pool.pickItemType(), LocalInode: inode, LocalHasIdentity: hasIdentity}
	}
	if pool.rng.IntN(3) > 0 {
		view.Remote = &remoteState{ItemType: pool.pickItemType()}
	}

	return view
}

// pickIdentity draws a local inode from the pool, or none.
func (pool *dependencyPathPool) pickIdentity() (uint64, bool) {
	if pool.rng.IntN(2) == 0 {
		return 0, false
	}

	return 1 + pool.rng.Uint64N(dependencyContentPool), true
}

// pickItemID draws an item ID from the pool, or none.
func (pool *dependencyPathPool) pickItemID() string {
	if pool.rng.IntN(2) == 0 {
		return ""
	}

	return fmt.Sprintf("item-%d", pool.rng.IntN(dependencyContentPool))
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
		case ActionDownload:
			action.ItemID = pool.pickItemID()
		case ActionUpload, ActionLocalDelete, ActionRemoteDelete,
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

	var contentEdges []contentEdge
	for deleteIdx := range actions {
		for createIdx := range actions {
			if carriesContentOf(&actions[createIdx], &actions[deleteIdx]) {
				contentEdges = append(contentEdges, contentEdge{deleteIdx: deleteIdx, createIdx: createIdx})
			}
		}
	}
	addContentEdges(deps, contentEdges)

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
	sourceRecreated := oracleRecreatesMoveSource(a, actions)
	for otherIdx := range actions {
		if otherIdx == idx {
			continue
		}
		other := &actions[otherIdx]
		if isMoveAction(other) && movePrecedes(other, a) {
			deps = append(deps, otherIdx)
			continue
		}
		if isMoveAction(a) && underMoveSource(other, a) && !underMoveDestination(other, a) &&
			!movesIntoRecreatedSource(other, a, sourceRecreated) {
			deps = append(deps, otherIdx)
			continue
		}
		if isMoveAction(a) && (isDeleteAction(other) || other.Type == ActionConflictCopy) && other.Path == a.Path {
			deps = append(deps, otherIdx)
		}
	}

	return deps
}

// oracleRecreatesMoveSource scans the whole plan for a folder create at the
// path move vacates (recreatesMoveSource).
func oracleRecreatesMoveSource(move *Action, actions []Action) bool {
	if !isMoveAction(move) {
		return false
	}
	for i := range actions {
		if recreatesMoveSource(&actions[i], move) {
			return true
		}
	}

	return false
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
