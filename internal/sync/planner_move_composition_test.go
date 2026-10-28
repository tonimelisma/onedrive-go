package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func composeNode(key string, itemType ItemType, baseline, local, remote string) moveNode {
	n := moveNode{key: key, itemType: itemType}
	n.paths[moveCoordBaseline] = baseline
	n.paths[moveCoordLocal] = local
	n.paths[moveCoordRemote] = remote

	return n
}

func composeOccupancy(nodes []moveNode) moveOccupancy {
	occupancy := moveOccupancy{local: map[string]bool{}, remote: map[string]bool{}}
	for i := range nodes {
		if p := nodes[i].paths[moveCoordLocal]; p != "" {
			occupancy.local[p] = true
		}
		if p := nodes[i].paths[moveCoordRemote]; p != "" {
			occupancy.remote[p] = true
		}
	}

	return occupancy
}

func composeForTest(nodes ...moveNode) moveComposition {
	return composeMoves(nodes, composeOccupancy(nodes), nil)
}

func composedMovesBySide(c *moveComposition, side moveCoord) []string {
	var out []string
	for i := range c.moves {
		if c.moves[i].side == side {
			out = append(out, c.moves[i].oldPath+"->"+c.moves[i].newPath)
		}
	}

	return out
}

// Validates: R-2.17.6
func TestComposeMoves_OneSidedRenamesMoveTheOtherSide(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("a", ItemTypeFile, "a.txt", "b.txt", "a.txt"),
		composeNode("c", ItemTypeFile, "c.txt", "c.txt", "d.txt"),
	)

	assert.Equal(t, []string{"a.txt->b.txt"}, composedMovesBySide(&c, moveCoordRemote))
	assert.Equal(t, []string{"c.txt->d.txt"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, c.rekeys)
	assert.Empty(t, c.deferred)
}

// Validates: R-2.17.6
func TestComposeMoves_SameRenameOnBothSidesOnlyRekeysTheBaseline(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("dir", ItemTypeFolder, "dir", "renamed", "renamed"),
		composeNode("child", ItemTypeFile, "dir/child.txt", "renamed/child.txt", "renamed/child.txt"),
	)

	assert.Empty(t, c.moves)
	require.Len(t, c.rekeys, 1, "the child follows its folder; only the folder's own row is re-keyed")
	assert.Equal(t, composedRekey{key: "dir", oldPath: "dir", newPath: "renamed"}, c.rekeys[0])
}

// Validates: R-2.17.6
func TestComposeMoves_DivergentRenameKeepsTheOneDriveName(t *testing.T) {
	t.Parallel()

	c := composeForTest(composeNode("a", ItemTypeFile, "a", "b", "c"))

	assert.Equal(t, []string{"b->c"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
	require.NotEmpty(t, c.notices)
	assert.Equal(t, moveNoticeDivergentRename, c.notices[0].kind)
}

// Validates: R-2.17.6
//
// Local A→B/A while OneDrive moved B→A/B. Applied as two independent moves,
// each folder ended up inside the other (D-055). The remote move is kept and
// the local one undone, so local follows OneDrive: A at the top, B inside it.
func TestComposeMoves_CrossMoveCycleKeepsTheRemoteMove(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("A", ItemTypeFolder, "A", "B/A", "A"),
		composeNode("B", ItemTypeFolder, "B", "B", "A/B"),
		composeNode("a-file", ItemTypeFile, "A/f.txt", "B/A/f.txt", "A/f.txt"),
		composeNode("b-file", ItemTypeFile, "B/g.txt", "B/g.txt", "A/B/g.txt"),
	)

	assert.Equal(t, []string{"B/A->A"}, composedMovesBySide(&c, moveCoordLocal)[:1],
		"A leaves B first, so B can then move into A")
	assert.Equal(t, []string{"B/A->A", "B->A/B"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
	assert.Empty(t, c.deferred)

	var cycleNotices int
	for i := range c.notices {
		if c.notices[i].kind == moveNoticeCycle {
			cycleNotices++
		}
	}
	assert.Equal(t, 1, cycleNotices)
}

// Validates: R-2.17.6
//
// A folder renamed locally whose child was renamed inside it: one folder
// move, then the child's rename addressed through the folder's new path.
func TestComposeMoves_DescendantRenameInsideRenamedFolder(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("p", ItemTypeFolder, "Projects", "Archive", "Projects"),
		composeNode("kept", ItemTypeFile, "Projects/kept.txt", "Archive/kept.txt", "Projects/kept.txt"),
		composeNode("old", ItemTypeFile, "Projects/old.txt", "Archive/new.txt", "Projects/old.txt"),
	)

	assert.Equal(t, []string{"Projects->Archive", "Archive/old.txt->Archive/new.txt"},
		composedMovesBySide(&c, moveCoordRemote),
		"the folder move carries kept.txt; the renamed child follows it")
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
}

// Validates: R-2.17.6
//
// OneDrive inverted a hierarchy (A/B → B, A → B/A). Local B leaves A first,
// then A moves into B; neither move is fabricated through MkdirAll.
func TestComposeMoves_RemoteHierarchyInversionOrdersLocalMoves(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("A", ItemTypeFolder, "A", "A", "B/A"),
		composeNode("B", ItemTypeFolder, "A/B", "A/B", "B"),
	)

	assert.Equal(t, []string{"A/B->B", "A->B/A"}, composedMovesBySide(&c, moveCoordLocal))
}

// Validates: R-2.17.6
//
// A file moved locally into a folder that OneDrive renamed: the move targets
// the folder's new name, after the local rename of the folder.
func TestComposeMoves_LocalMoveIntoRemotelyRenamedFolder(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("y", ItemTypeFolder, "Y", "Y", "Y2"),
		composeNode("f", ItemTypeFile, "f.txt", "Y/f.txt", "f.txt"),
	)

	assert.Equal(t, []string{"Y->Y2"}, composedMovesBySide(&c, moveCoordLocal))
	require.Equal(t, []string{"f.txt->Y2/f.txt"}, composedMovesBySide(&c, moveCoordRemote))
	for i := range c.moves {
		if c.moves[i].key == "f" {
			assert.Equal(t, "Y/f.txt", c.moves[i].localPath, "freshness reads the item where the snapshot holds it")
		}
	}
}

// Validates: R-2.17.6
//
// A destination occupied by an entry composition does not own is never a
// move target: the item waits instead of being renamed onto it.
func TestComposeMoves_OccupiedDestinationDefers(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{composeNode("a", ItemTypeFile, "a", "a", "b")}
	occupancy := composeOccupancy(nodes)
	occupancy.local["b"] = true

	c := composeMoves(nodes, occupancy, nil)

	assert.Empty(t, c.moves)
	assert.True(t, c.deferred["a"])
}

// Validates: R-2.17.6
func TestComposeMoves_FrozenItemsAndTheirSubtreesDoNotMove(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("dir", ItemTypeFolder, "dir", "renamed", "dir"),
		composeNode("child", ItemTypeFile, "dir/c.txt", "renamed/c2.txt", "dir/c.txt"),
	}
	c := composeMoves(nodes, composeOccupancy(nodes), map[string]bool{"dir": true})

	assert.Empty(t, c.moves)
	assert.True(t, c.deferred["dir"])
	assert.True(t, c.deferred["child"])
}

// Validates: R-2.17.6
//
// Dependency edges for nested moves (D-054): work under a move's destination
// waits for it, a child leaving a moving folder goes first, a move onto a
// vacated path waits for the vacating move, and a create at a remotely moved
// item's old path waits for that move.
func TestBuildDependencies_NestedMoveEdges(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	actions := []Action{
		{Type: ActionLocalMove, OldPath: "A", Path: "Z", View: folder},             // 0
		{Type: ActionLocalMove, OldPath: "A/out.txt", Path: "out.txt", View: file}, // 1: leaves A first
		{Type: ActionDownload, Path: "Z/new.txt", View: file},                      // 2: under 0's destination
		{Type: ActionLocalMove, OldPath: "b.txt", Path: "c.txt", View: file},       // 3
		{Type: ActionLocalMove, OldPath: "a.txt", Path: "b.txt", View: file},       // 4: onto 3's vacated path
		{Type: ActionRemoteMove, OldPath: "p.txt", Path: "q.txt", View: file},      // 5
		{Type: ActionUpload, Path: "p.txt", View: &pathView{Path: "p.txt"}},        // 6: new entry at 5's old path
		{Type: ActionDownload, Path: "A/in.txt", View: file},                       // 7: lands before A moves
	}

	deps := buildDependencies(actions)

	assert.ElementsMatch(t, []int{1, 7}, deps[0], "the folder move waits for work under its source")
	assert.Equal(t, []int{0}, deps[2])
	assert.Equal(t, []int{3}, deps[4])
	assert.Equal(t, []int{5}, deps[6])
	assert.Empty(t, deps[1])
	require.NoError(t, detectDependencyCycle(deps))
}
