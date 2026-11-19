package sync

import (
	"log/slog"
	"path"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
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

// Validates: R-2.17.1
//
// A rotation of three names online (a->b, b->c, c->a) parks one item beside
// its source so the others can move into the freed paths in order; the parked
// item reaches its target on the next pass.
func TestComposeMoves_RotationParksOneItemAndMovesTheRest(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("a", ItemTypeFile, "a.txt", "a.txt", "b.txt"),
		composeNode("b", ItemTypeFile, "b.txt", "b.txt", "c.txt"),
		composeNode("c", ItemTypeFile, "c.txt", "c.txt", "a.txt"),
	)

	assert.Equal(t, []string{"c.txt->c.swap-1.txt", "b.txt->c.txt", "a.txt->b.txt"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
	assert.Equal(t, map[string]bool{"c": true}, c.deferred, "the parked item finishes next pass")
}

// Validates: R-2.17.1
//
// The user renamed d/50%.txt to d/50%.Txt (case only) while OneDrive swapped
// it with d/a+b.txt. The move of a+b.txt onto d/50%.txt was emitted while
// 50%.Txt still sat there: two names one folder may not hold side by side
// under OneDrive's comparison. Observation then skipped 50%.Txt as a
// collision and the item downloaded a second time (model test seed 27). A
// destination is free only when no other entry holds it under OneDrive's
// name folding.
func TestComposeMoves_DestinationFreeUnderNameFolding(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("p", ItemTypeFile, "d/50%.txt", "d/50%.Txt", "d/a+b.txt"),
		composeNode("q", ItemTypeFile, "d/a+b.txt", "d/a+b.txt", "d/50%.txt"),
	)

	held := map[namekey.PathKey]string{}
	for _, n := range []moveNode{
		composeNode("p", ItemTypeFile, "", "d/50%.Txt", ""),
		composeNode("q", ItemTypeFile, "", "d/a+b.txt", ""),
	} {
		held[namekey.OfPath(n.paths[moveCoordLocal])] = n.key
	}
	for _, move := range c.moves {
		if move.side != moveCoordLocal {
			continue
		}
		occupant, taken := held[namekey.OfPath(move.newPath)]
		assert.False(t, taken && occupant != move.key, "%s -> %s lands on %s's name", move.oldPath, move.newPath, occupant)
		delete(held, namekey.OfPath(move.oldPath))
		held[namekey.OfPath(move.newPath)] = move.key
	}
	assert.NotEmpty(t, c.moves)
}

// Validates: R-2.17.1
//
// A swap of two names at the 255-byte limit parks one at a name that still
// fits: the stem is cut, on a character boundary, to make room for the
// .swap-N infix.
func TestComposeMoves_ParkingNameFitsTheNameLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 125) // 250 bytes
	a, b := long+"a.txt", long+"b.txt"
	require.Len(t, a, 255)
	c := composeForTest(
		composeNode("a", ItemTypeFile, a, a, b),
		composeNode("b", ItemTypeFile, b, b, a),
	)

	require.NotEmpty(t, c.moves)
	for _, move := range c.moves {
		assert.LessOrEqual(t, len(move.newPath), 255, move.newPath)
		assert.True(t, utf8.ValidString(move.newPath), move.newPath)
	}
	assert.Contains(t, c.moves[0].newPath, moveSwapInfix)
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

// recreatedFolderNodes are the composition inputs of the pass that sees all
// three steps of TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges
// at once: docs moved to photos/photos, a new docs made at its path, and
// docs/name moved from the moved folder to entry, on side; the other side
// still holds the synced tree. A folder entry holds inner.txt.
func recreatedFolderNodes(side moveCoord, entryType ItemType, name, entry string) ([]moveNode, moveOccupancy) {
	at := func(key string, itemType ItemType, synced, moved string) moveNode {
		if side == moveCoordLocal {
			return composeNode(key, itemType, synced, moved, synced)
		}
		return composeNode(key, itemType, synced, synced, moved)
	}
	nodes := []moveNode{
		at("docs", ItemTypeFolder, "docs", "photos/photos"),
		at("entry", entryType, "docs/"+name, entry),
		at("kept", ItemTypeFile, "docs/kept.txt", "photos/photos/kept.txt"),
		at("photos", ItemTypeFolder, "photos", "photos"),
		at("photo", ItemTypeFile, "photos/photo.jpg", "photos/photo.jpg"),
	}
	if entryType == ItemTypeFolder {
		nodes = append(nodes, at("inner", ItemTypeFile, "docs/"+name+"/inner.txt", entry+"/inner.txt"))
	}
	occupancy := composeOccupancy(nodes)
	if side == moveCoordLocal {
		occupancy.local["docs"] = true
	} else {
		occupancy.remote["docs"] = true
	}

	return nodes, occupancy
}

// Validates: R-2.17.6, R-2.17.1
//
// An entry that leaves a moving folder for the folder re-created at the path
// the move vacates, under its old name or a new one, moves only after the
// folder has. A file put back at its path is still a move: its parent there is
// the new folder. Deferred as unmoved, it would be carried away by the
// folder's move and moved back into it by the next pass. On OneDrive a file's
// move runs after the folder's, from the path the folder's move gives it, in
// the same pass. A local move would name a source local truth does not hold
// yet, and a folder may hold work addressed at its old paths, so those wait
// for the next pass, which sees the folder moved.
func TestComposeMoves_EntryMovedIntoTheFolderRecreatedAtAMovedFoldersPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		side         moveCoord
		entryType    ItemType
		entry        string
		wantRemote   []string
		wantLocal    []string
		wantDeferred map[string]bool
	}{
		{
			name: "a file put back, moved here", side: moveCoordLocal, entryType: ItemTypeFile, entry: "docs/50%.txt",
			wantRemote: []string{"docs->photos/photos", "photos/photos/50%.txt->docs/50%.txt"}, wantDeferred: map[string]bool{},
		},
		{
			name: "a file renamed, moved here", side: moveCoordLocal, entryType: ItemTypeFile, entry: "docs/renamed-50%.txt",
			wantRemote:   []string{"docs->photos/photos", "photos/photos/50%.txt->docs/renamed-50%.txt"},
			wantDeferred: map[string]bool{},
		},
		{
			name: "a folder put back, moved here", side: moveCoordLocal, entryType: ItemTypeFolder, entry: "docs/sub",
			wantRemote: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
		{
			name: "a file put back, moved on OneDrive", side: moveCoordRemote, entryType: ItemTypeFile, entry: "docs/50%.txt",
			wantLocal: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
		{
			name: "a file renamed, moved on OneDrive", side: moveCoordRemote, entryType: ItemTypeFile, entry: "docs/renamed-50%.txt",
			wantLocal: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
		{
			name: "a folder put back, moved on OneDrive", side: moveCoordRemote, entryType: ItemTypeFolder, entry: "docs/sub",
			wantLocal: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synced := "50%.txt"
			if tc.entryType == ItemTypeFolder {
				synced = "sub"
			}
			nodes, occupancy := recreatedFolderNodes(tc.side, tc.entryType, synced, tc.entry)
			c := composeMoves(nodes, occupancy, nil)

			assert.Equal(t, tc.wantRemote, composedMovesBySide(&c, moveCoordRemote))
			assert.Equal(t, tc.wantLocal, composedMovesBySide(&c, moveCoordLocal))
			assert.Equal(t, tc.wantDeferred, c.deferred)
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// The composition inputs of
// TestRunOnce_SubfolderMovedOnOneDriveToThePathItsMovedParentLeftConverges:
// docs moved to archive on OneDrive, then archive/sub to docs. sub's local move
// onto docs cannot leave first, since docs is the path the folder's move
// vacates. Planned from docs/sub in this pass, it would hold back the folder
// move it waits for, a cycle the planner would drop on every pass. The folder
// moves, and sub waits for the next pass.
func TestComposeMoves_SubfolderMovedToThePathItsMovedParentLeftWaitsForTheParent(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("docs", ItemTypeFolder, "docs", "docs", "archive"),
		composeNode("kept", ItemTypeFile, "docs/kept.txt", "docs/kept.txt", "archive/kept.txt"),
		composeNode("sub", ItemTypeFolder, "docs/sub", "docs/sub", "docs"),
		composeNode("inner", ItemTypeFile, "docs/sub/inner.txt", "docs/sub/inner.txt", "docs/inner.txt"),
	)

	assert.Equal(t, []string{"docs->archive"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
	assert.Equal(t, map[string]bool{"sub": true}, c.deferred)
}

// composedMoveActions turns c's moves into a plan's move actions, in c's
// order, each with a baseline of its item's type.
func composedMoveActions(c *moveComposition, nodes []moveNode) []Action {
	itemTypes := make(map[string]ItemType, len(nodes))
	for i := range nodes {
		itemTypes[nodes[i].key] = nodes[i].itemType
	}
	actions := make([]Action, 0, len(c.moves))
	for i := range c.moves {
		moveType := ActionRemoteMove
		if c.moves[i].side == moveCoordLocal {
			moveType = ActionLocalMove
		}
		actions = append(actions, Action{
			Type: moveType, OldPath: c.moves[i].oldPath, Path: c.moves[i].newPath,
			View: &pathView{Baseline: &BaselineEntry{ItemType: itemTypes[c.moves[i].key]}},
		})
	}

	return actions
}

// Validates: R-2.17.6, R-2.17.1
//
// On this computer docs was moved to archive, a new docs made and
// archive/sub/inner.txt moved into it, and archive/sub is gone, so SQLite
// holds sub on OneDrive only. inner.txt lands below the path docs's move
// vacates, but docs/sub lies between it and docs and is not a move
// participant, so its move keeps the source docs/sub/inner.txt in both rows.
// When OneDrive left docs/sub as it was, SQLite deletes it online, and the
// delete waits for inner.txt to leave it; named from archive/sub, where docs's
// move puts it, the move would not hold the delete back, and the delete would
// remove docs/sub online with inner.txt still inside. Kept so, the plan is a
// cycle the planner drops, so nothing is deleted. When OneDrive added a file
// to docs/sub, SQLite makes archive/sub here again instead, and the plan has
// no delete: every action of it has an order
// (TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges).
func TestComposeMoves_EntryBelowAFolderOnOneSideOnlyKeepsItsSource(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	for _, tc := range []struct {
		name string
		// subWork is the plan's work on sub after the two moves and the docs
		// made online.
		subWork  []Action
		wantKept int
	}{
		{name: "deleted online", subWork: []Action{{Type: ActionRemoteDelete, Path: "docs/sub", View: folder}}},
		{name: "made here again", subWork: []Action{
			{Type: ActionFolderCreate, Path: "archive/sub", CreateSide: createLocal},
			{Type: ActionDownload, Path: "archive/sub/new.txt", View: &pathView{Path: "archive/sub/new.txt"}},
		}, wantKept: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodes := []moveNode{
				composeNode("docs", ItemTypeFolder, "docs", "archive", "docs"),
				composeNode("kept", ItemTypeFile, "docs/kept.txt", "archive/kept.txt", "docs/kept.txt"),
				composeNode("sub", ItemTypeFolder, "docs/sub", "", "docs/sub"),
				composeNode("inner", ItemTypeFile, "docs/sub/inner.txt", "docs/inner.txt", "docs/sub/inner.txt"),
			}
			occupancy := composeOccupancy(nodes)
			occupancy.local["docs"] = true
			c := composeMoves(nodes, occupancy, nil)

			assert.Equal(t, []string{"docs->archive", "docs/sub/inner.txt->docs/inner.txt"},
				composedMovesBySide(&c, moveCoordRemote))
			assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))

			actions := append(composedMoveActions(&c, nodes), Action{Type: ActionFolderCreate, Path: "docs", CreateSide: CreateRemote})
			actions = append(actions, tc.subWork...)
			if tc.subWork[0].Type == ActionRemoteDelete {
				assert.Contains(t, buildDependencies(actions)[3], 1, "the delete of docs/sub waits for inner.txt to leave it")
			}
			kept, _, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, tc.wantKept)
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// The composition inputs of
// TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges,
// made here: docs/sub renamed docs/sub2, docs moved to photos/photos, a new
// docs made, and photos/photos/sub2/inner.txt moved into it. inner.txt lands
// below the path docs's move vacates, but the nearest folder above it that is
// relocated is sub, and its target does not land below sub's path
// (vacatingFolder), so its move is neither rebased nor held: it keeps the
// source docs/sub/inner.txt, while sub's rename follows docs's move. Every
// action of the plan they make with the docs made online has an order.
func TestComposeMoves_EntryBelowARenamedSubfolderKeepsItsSource(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("docs", ItemTypeFolder, "docs", "photos/photos", "docs"),
		composeNode("sub", ItemTypeFolder, "docs/sub", "photos/photos/sub2", "docs/sub"),
		composeNode("inner", ItemTypeFile, "docs/sub/inner.txt", "docs/inner.txt", "docs/sub/inner.txt"),
		composeNode("kept", ItemTypeFile, "docs/kept.txt", "photos/photos/kept.txt", "docs/kept.txt"),
		composeNode("photos", ItemTypeFolder, "photos", "photos", "photos"),
		composeNode("photo", ItemTypeFile, "photos/photo.jpg", "photos/photo.jpg", "photos/photo.jpg"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.local["docs"] = true
	c := composeMoves(nodes, occupancy, nil)

	assert.Equal(t, []string{
		"docs/sub/inner.txt->docs/inner.txt", "docs->photos/photos", "photos/photos/sub->photos/photos/sub2",
	}, composedMovesBySide(&c, moveCoordRemote))
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, c.deferred)

	actions := append(composedMoveActions(&c, nodes), Action{Type: ActionFolderCreate, Path: "docs", CreateSide: CreateRemote})
	kept, _, _ := isolateDependencyCycles(actions, nil)
	assert.Len(t, kept, len(actions))
}

// Validates: R-2.17.6, R-2.17.1
//
// The composition inputs of
// TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges: OneDrive moved
// docs away and docs/a.txt to the path docs left, into a new docs made there
// or onto that path itself, while a.txt was moved here into docs/new, a folder
// made here that is not a synced item. docs/new moves with docs on this
// computer, so a.txt lands where docs's move vacates: it is decided after
// docs and, as a local move, waits for the next pass. Moved first from
// docs/new, it would be carried away by docs's move and moved again, or, onto
// docs's own path, wait for docs to leave while docs waits for it to leave
// first: a cycle the planner drops on every pass.
func TestComposeMoves_FileMovedOnOneDriveOutOfAFolderMadeHereWaitsForItsFolder(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, movedTo, target string
	}{
		{name: "into the folder made at its folder's path", movedTo: "photos/photos", target: "docs/a.txt"},
		{name: "onto its folder's path", movedTo: "archive", target: "docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodes := []moveNode{
				composeNode("docs", ItemTypeFolder, "docs", "docs", tc.movedTo),
				composeNode("kept", ItemTypeFile, "docs/kept.txt", "docs/kept.txt", path.Join(tc.movedTo, "kept.txt")),
				composeNode("photos", ItemTypeFolder, "photos", "photos", "photos"),
				composeNode("entry", ItemTypeFile, "docs/a.txt", "docs/new/a.txt", tc.target),
			}
			occupancy := composeOccupancy(nodes)
			occupancy.local["docs/new"] = true
			occupancy.remote["docs"] = true
			c := composeMoves(nodes, occupancy, nil)

			assert.Equal(t, []string{"docs->" + tc.movedTo}, composedMovesBySide(&c, moveCoordLocal))
			assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
			assert.Equal(t, map[string]bool{"entry": true}, c.deferred)
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// The composition inputs of
// TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges,
// made here with a new x: x moved to x2, a new x made, f moved into it, a new
// f made, and x/f/b.txt moved into the new f; the last row moves a synced
// folder g onto x instead, paired as a move, which the scenario's pass does
// not make of it (there g reaches OneDrive as a new x and a delete): the row
// holds composition's answer for a plan that pairs it. b.txt lands where f's
// move vacates, so it is
// decided after f, but f lands in the path x's move vacates. Rebased through
// f's move, b.txt's OneDrive source x/f/b.txt would lie below x, and the
// dependency rules would make x's move wait for b.txt to leave first, while
// b.txt waits for f's move and f's move for x's path to be filled after x's
// move (rebasedSourceMisread). b.txt waits for the next pass; the plan of the
// folders' moves and the new folders has an order.
func TestComposeMoves_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathWaits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, back string
		onto       bool
		wantRemote []string
	}{
		{name: "renamed", back: "f/d.txt", wantRemote: []string{"x->x2", "f->x/f"}},
		{name: "same name", back: "f/b.txt", wantRemote: []string{"x->x2", "f->x/f"}},
		{name: "same name, a folder moved onto x", back: "f/b.txt", onto: true, wantRemote: []string{"x->x2", "g->x", "f->x/f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodes := []moveNode{
				composeNode("x", ItemTypeFolder, "x", "x2", "x"),
				composeNode("keep", ItemTypeFile, "x/keep.txt", "x2/keep.txt", "x/keep.txt"),
				composeNode("f", ItemTypeFolder, "f", "x/f", "f"),
				composeNode("b", ItemTypeFile, "f/b.txt", tc.back, "f/b.txt"),
				composeNode("c", ItemTypeFile, "f/c.txt", "x/f/c.txt", "f/c.txt"),
			}
			newFolders := []Action{{Type: ActionFolderCreate, Path: "f", CreateSide: CreateRemote}}
			if tc.onto {
				nodes = append(nodes,
					composeNode("g", ItemTypeFolder, "g", "x", "g"),
					composeNode("g-file", ItemTypeFile, "g/g.txt", "x/g.txt", "g/g.txt"))
			} else {
				newFolders = append(newFolders, Action{Type: ActionFolderCreate, Path: "x", CreateSide: CreateRemote})
			}
			occupancy := composeOccupancy(nodes)
			occupancy.local["x"] = true
			occupancy.local["f"] = true
			c := composeMoves(nodes, occupancy, nil)

			assert.Equal(t, tc.wantRemote, composedMovesBySide(&c, moveCoordRemote))
			assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
			assert.Equal(t, map[string]bool{"b": true}, c.deferred)

			actions := append(composedMoveActions(&c, nodes), newFolders...)
			kept, _, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, len(actions))
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// The wait of such a file for every folder OneDrive relocates, in the order
// that needs it: c moved to zz/c2 and made again, p moved into the new c, a
// new p made, and c/p/b.txt moved into it as d.txt, on this computer. b.txt
// lands where p's move vacates and p in the path c's move vacates, as above,
// but composition decides p (target c/p) before b.txt (target p/d.txt) and c
// (target zz/c2) after it. Decided knowing p's move alone, b.txt would move
// from c/p/b.txt, below c, and c's move would wait for it to leave: a cycle.
// b.txt waits until c is decided and then for the next pass; the plan of the
// folders' moves and the new folders has an order.
func TestComposeMoves_FileMovedBackWaitsForAFolderDecidedAfterIt(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("zz", ItemTypeFolder, "zz", "zz", "zz"),
		composeNode("c", ItemTypeFolder, "c", "zz/c2", "c"),
		composeNode("keep", ItemTypeFile, "c/keep.txt", "zz/c2/keep.txt", "c/keep.txt"),
		composeNode("p", ItemTypeFolder, "p", "c/p", "p"),
		composeNode("b", ItemTypeFile, "p/b.txt", "p/d.txt", "p/b.txt"),
		composeNode("e", ItemTypeFile, "p/e.txt", "c/p/e.txt", "p/e.txt"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.local["c"] = true
	occupancy.local["p"] = true
	c := composeMoves(nodes, occupancy, nil)

	assert.ElementsMatch(t, []string{"c->zz/c2", "p->c/p"}, composedMovesBySide(&c, moveCoordRemote))
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
	assert.Equal(t, map[string]bool{"b": true}, c.deferred)

	actions := append(composedMoveActions(&c, nodes),
		Action{Type: ActionFolderCreate, Path: "c", CreateSide: CreateRemote},
		Action{Type: ActionFolderCreate, Path: "p", CreateSide: CreateRemote})
	kept, _, _ := isolateDependencyCycles(actions, nil)
	assert.Len(t, kept, len(actions))
}

// Validates: R-2.17.6, R-2.17.1
//
// The composition inputs of the first pass of
// TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges,
// made on OneDrive: x moved to x2, a new x made, f moved into it, a new f
// made, and x/f/b.txt moved into the new f. On this computer x and f are still
// where they were, and the new folders are made only by a later pass: SQLite
// plans no create at a path the baseline still gives an item OneDrive moved
// away. So f's move into the new x and x's move away from its path are never
// planned together: run first, f's would land in the old x and be carried to
// x2/f; run second, it would find no x. Whichever composition decides first
// is planned, and the other waits for the next pass, and so does b.txt, whose
// new f is not there either. The rows hold x decided first, f decided first
// (x moved deeper), and two folders each moved into a new folder at the
// other's path, where waiting for the other's decision would never end. A
// file x cannot carry f, nor can a frozen x, below which nothing moves this
// pass, so f waits for it although f is decided first. A new folder X, whose
// name folds to x as OneDrive compares names, is held as x is, and so is a
// chain of new folders x/sub
// (TestRunOnce_FolderMovedOnOneDriveIntoAChainOfNewFoldersConverges), whose
// deepest folder no synced item holds: the item is found at the first path
// above it that one holds. One level down, inside a folder OneDrive also
// moved, c's move into the new P/A/new waits for new's move away from that
// path.
func TestComposeMoves_FolderMovedIntoAFolderMadeWhereAnotherFolderLeftWaits(t *testing.T) {
	t.Parallel()

	// moved holds x moved to x2 on OneDrive and f moved into the new folder
	// into; b.txt, last, moved to back.
	moved := func(into, x2, back string) []moveNode {
		return []moveNode{
			composeNode("x", ItemTypeFolder, "x", "x", x2),
			composeNode("keep", ItemTypeFile, "x/keep.txt", "x/keep.txt", x2+"/keep.txt"),
			composeNode("f", ItemTypeFolder, "f", "f", into+"/f"),
			composeNode("c", ItemTypeFile, "f/c.txt", "f/c.txt", into+"/f/c.txt"),
			composeNode("b", ItemTypeFile, "f/b.txt", "f/b.txt", back),
		}
	}
	deeper := append(moved("x", "zz/a/x2", "f/d.txt"),
		composeNode("zz", ItemTypeFolder, "zz", "zz", "zz"),
		composeNode("a", ItemTypeFolder, "zz/a", "zz/a", "zz/a"))
	fileX := append([]moveNode{composeNode("x", ItemTypeFile, "x", "x", "zz/a/x2")}, deeper[2:]...)
	nested := []moveNode{
		composeNode("P", ItemTypeFolder, "P", "P", "P2"),
		composeNode("A", ItemTypeFolder, "P/A", "P/A", "P2/A"),
		composeNode("new", ItemTypeFolder, "P/A/new", "P/A/new", "P2/A/new2"),
		composeNode("c", ItemTypeFile, "P/A/c", "P/A/c", "P2/A/new/c"),
	}
	madeXF := []string{"x", "f"}
	bothWait := map[string]bool{"f": true, "b": true}
	for _, tc := range []struct {
		name         string
		nodes        []moveNode
		made         []string // the folders OneDrive made
		frozen       map[string]bool
		wantLocal    []string
		wantDeferred map[string]bool
	}{
		{name: "renamed", nodes: moved("x", "x2", "f/d.txt"), made: madeXF, wantLocal: []string{"x->x2"}, wantDeferred: bothWait},
		{name: "same name", nodes: moved("x", "x2", "f/b.txt"), made: madeXF, wantLocal: []string{"x->x2"}, wantDeferred: bothWait},
		{
			name: "x moved deeper, decided after f", nodes: deeper, made: madeXF,
			wantLocal: []string{"f->x/f"}, wantDeferred: map[string]bool{"x": true, "b": true},
		},
		{
			name: "each into a new folder at the other's path", nodes: moved("x", "f/x", "")[:4], made: madeXF,
			wantLocal: []string{"x->f/x"}, wantDeferred: map[string]bool{"f": true},
		},
		{
			name: "a file x moved deeper, decided after f", nodes: fileX, made: madeXF,
			wantLocal: []string{"x->zz/a/x2"}, wantDeferred: bothWait,
		},
		{
			name: "a frozen x moved deeper, decided after f", nodes: deeper, made: madeXF, frozen: map[string]bool{"x": true},
			wantDeferred: map[string]bool{"x": true, "f": true, "b": true},
		},
		{
			name: "a new folder X", nodes: moved("X", "x2", "f/d.txt"), made: []string{"X", "f"},
			wantLocal: []string{"x->x2"}, wantDeferred: bothWait,
		},
		{
			name: "a chain of new folders", nodes: moved("x/sub", "x2", "f/d.txt"), made: []string{"x", "x/sub", "f"},
			wantLocal: []string{"x->x2"}, wantDeferred: bothWait,
		},
		{
			name: "inside a folder OneDrive moved", nodes: nested, made: []string{"P2/A/new"},
			wantLocal: []string{"P->P2", "P/A/new->P/A/new2"}, wantDeferred: map[string]bool{"c": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			occupancy := composeOccupancy(tc.nodes)
			for _, made := range tc.made {
				occupancy.remote[made] = true
			}
			c := composeMoves(tc.nodes, occupancy, tc.frozen)

			assert.Equal(t, tc.wantLocal, composedMovesBySide(&c, moveCoordLocal))
			assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
			assert.Equal(t, tc.wantDeferred, c.deferred)

			actions := composedMoveActions(&c, tc.nodes)
			kept, _, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, len(actions))
		})
	}
}

// heldRemoteLandingCase is one row of
// TestComposeMoves_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsAnItemAtItsPath.
type heldRemoteLandingCase struct {
	name  string
	nodes []moveNode
	// made lists the folders made here, onOneDrive an entry made on OneDrive.
	made       []string
	onOneDrive string
	frozen     map[string]bool
	wantRemote []string
	// wantDeferred is nil for none; creates are the OneDrive folder creates
	// the plan makes with the moves.
	wantDeferred map[string]bool
	creates      []string
}

func heldRemoteLandingCases() []heldRemoteLandingCase {
	return slices.Concat(keptOrMovedLandingCases(), takenBackLandingCases(), partnerLandingCases())
}

// landingNodes holds x moved to x2 here and f moved into the new folder into.
func landingNodes(x2, into string, xType ItemType) []moveNode {
	return []moveNode{
		composeNode("x", xType, "x", x2, "x"),
		composeNode("f", ItemTypeFolder, "f", into+"/f", "f"),
		composeNode("c", ItemTypeFile, "f/c.txt", into+"/f/c.txt", "f/c.txt"),
		composeNode("zz", ItemTypeFolder, "zz", "zz", "zz"),
		composeNode("a", ItemTypeFolder, "zz/a", "zz/a", "zz/a"),
	}
}

// keptOrMovedLandingCases are the rows where x stays where OneDrive has it,
// moves away there, or is a file or frozen.
func keptOrMovedLandingCases() []heldRemoteLandingCase {
	folder, chain := ItemTypeFolder, []string{"x", "x/sub"}
	bothWait := map[string]bool{"x": true, "f": true}

	return []heldRemoteLandingCase{
		{
			name: "x kept where OneDrive has it", nodes: landingNodes("x2", "x", folder), made: []string{"x"},
			onOneDrive: "X2", wantDeferred: bothWait,
		},
		{
			name: "a chain, x kept", nodes: landingNodes("x2", "x/sub", folder), made: chain,
			onOneDrive: "X2", wantDeferred: bothWait,
		},
		{
			name: "x moved away", nodes: landingNodes("x2", "x", folder), made: []string{"x"},
			wantRemote: []string{"x->x2", "f->x/f"}, creates: []string{"x"},
		},
		{
			name: "a chain, x moved away", nodes: landingNodes("x2", "x/sub", folder), made: chain,
			wantRemote: []string{"x->x2", "f->x/sub/f"}, creates: chain,
		},
		{
			name: "x moved deeper, decided after f", nodes: landingNodes("zz/a/x2", "x", folder), made: []string{"x"},
			wantRemote: []string{"f->x/f", "x->zz/a/x2"}, creates: []string{"x"},
		},
		{
			name: "a file x moved deeper", nodes: landingNodes("zz/a/x2", "x", ItemTypeFile), made: []string{"x"},
			wantRemote: []string{"x->zz/a/x2"}, wantDeferred: map[string]bool{"f": true}, creates: []string{"x"},
		},
		{
			name: "a frozen x moved deeper", nodes: landingNodes("zz/a/x2", "x", folder), made: []string{"x"},
			frozen: map[string]bool{"x": true}, wantDeferred: bothWait,
		},
		{
			name: "a new X, x kept where OneDrive has it", made: []string{"X"}, onOneDrive: "X2",
			nodes: []moveNode{
				composeNode("x", ItemTypeFolder, "x", "x2", "x"),
				composeNode("f", ItemTypeFolder, "f", "X/f", "f"),
			},
			wantDeferred: bothWait,
		},
		{
			name: "a new X, x moved away", made: []string{"X"},
			nodes: []moveNode{
				composeNode("x", ItemTypeFolder, "x", "x2", "x"),
				composeNode("f", ItemTypeFolder, "f", "X/f", "f"),
			},
			wantRemote: []string{"x->x2", "f->X/f"}, creates: []string{"X"},
		},
		{
			name: "f's destination held by an item that swaps with f, x kept where OneDrive has it",
			made: []string{"x"}, onOneDrive: "X2",
			nodes: []moveNode{
				composeNode("x", ItemTypeFolder, "x", "x2", "x"),
				composeNode("f", ItemTypeFolder, "f", "x/f", "f"),
				composeNode("p", ItemTypeFolder, "x/f", "f", "x/f"),
			},
			wantDeferred: map[string]bool{"x": true, "f": true, "p": true},
		},
	}
}

// takenBackLandingCases are the rows where f's move is planned and then
// taken back, as x ends the composition without an OneDrive move.
func takenBackLandingCases() []heldRemoteLandingCase {
	folder, bothWait := ItemTypeFolder, map[string]bool{"x": true, "f": true}

	return []heldRemoteLandingCase{
		{
			name: "x held in a new b, decided first", made: []string{"b", "x"}, onOneDrive: "B2",
			nodes:        append(landingNodes("b/x", "x", folder), composeNode("b", ItemTypeFolder, "b", "b2", "b")),
			wantDeferred: map[string]bool{"b": true, "x": true, "f": true},
		},
		{
			name: "x held in a new y, decided after f", made: []string{"y", "x"}, onOneDrive: "Y2",
			nodes:        append(landingNodes("y/x", "x", folder), composeNode("y", ItemTypeFolder, "y", "y2", "y")),
			wantDeferred: map[string]bool{"y": true, "x": true, "f": true},
		},
		{
			name: "x waiting for a file deleted in a renamed a, deferred after f", made: []string{"x"},
			nodes: []moveNode{
				composeNode("a", ItemTypeFolder, "a", "b", "a"),
				composeNode("w", ItemTypeFile, "a/x", "", "a/x"),
				composeNode("x", ItemTypeFolder, "x", "b/x", "x"),
				composeNode("f", ItemTypeFolder, "f", "x/f", "f"),
			},
			wantRemote: []string{"a->b"}, wantDeferred: bothWait,
		},
		{
			name: "m into a new b, b into a new c, c waiting for a file deleted in a renamed p", made: []string{"c", "b"},
			nodes: []moveNode{
				composeNode("p", ItemTypeFolder, "p", "q", "p"),
				composeNode("w", ItemTypeFile, "p/c", "", "p/c"),
				composeNode("c", ItemTypeFolder, "c", "q/c", "c"),
				composeNode("b", ItemTypeFolder, "b", "c/b", "b"),
				composeNode("m", ItemTypeFolder, "m", "b/m", "m"),
			},
			wantRemote: []string{"p->q"}, wantDeferred: map[string]bool{"c": true, "b": true, "m": true},
		},
		{
			name: "f's destination held by an item that swaps with f, x held in a new b",
			made: []string{"b", "x"}, onOneDrive: "B2",
			nodes: []moveNode{
				composeNode("b", ItemTypeFolder, "b", "b2", "b"),
				composeNode("x", ItemTypeFolder, "x", "b/x", "x"),
				composeNode("f", ItemTypeFolder, "f", "x/f", "f"),
				composeNode("p", ItemTypeFolder, "x/f", "f", "x/f"),
			},
			wantDeferred: map[string]bool{"b": true, "x": true, "f": true, "p": true},
		},
	}
}

// partnerLandingCases are the rows of items each moved into a new folder at
// the path of the next.
func partnerLandingCases() []heldRemoteLandingCase {
	return []heldRemoteLandingCase{
		{
			name: "each into a new folder at the other's path", made: []string{"x", "f"},
			nodes: []moveNode{
				composeNode("x", ItemTypeFolder, "x", "f/x", "x"),
				composeNode("f", ItemTypeFolder, "f", "x/f", "f"),
			},
			wantRemote: []string{"x->f/x"}, wantDeferred: map[string]bool{"f": true}, creates: []string{"x"},
		},
		{
			name: "each into a new folder at the other's path, and g into the new f", made: []string{"x", "f"},
			nodes: []moveNode{
				composeNode("x", ItemTypeFolder, "x", "f/x", "x"),
				composeNode("f", ItemTypeFolder, "f", "x/f", "f"),
				composeNode("g", ItemTypeFolder, "g", "f/g", "g"),
			},
			wantRemote: []string{"x->f/x"}, wantDeferred: map[string]bool{"f": true, "g": true}, creates: []string{"x"},
		},
		{
			name: "a file a into a new f, f into a new a, f decided first", made: []string{"a", "f"},
			nodes: []moveNode{
				composeNode("a", ItemTypeFile, "a", "f/a", "a"),
				composeNode("f", ItemTypeFolder, "f", "a/f", "f"),
			},
			wantRemote: []string{"a->f/a"}, wantDeferred: map[string]bool{"f": true}, creates: []string{"a"},
		},
		{
			name: "x into a new f/sub, f into a new x", made: []string{"x", "f", "f/sub"},
			nodes: []moveNode{
				composeNode("x", ItemTypeFolder, "x", "f/sub/x", "x"),
				composeNode("f", ItemTypeFolder, "f", "x/f", "f"),
			},
			wantRemote: []string{"f->x/f"}, wantDeferred: map[string]bool{"x": true}, creates: []string{"f", "f/sub"},
		},
		{
			name: "a ring of three, and g into the new b", made: []string{"a", "b", "c"},
			nodes: []moveNode{
				composeNode("a", ItemTypeFolder, "a", "b/a", "a"),
				composeNode("b", ItemTypeFolder, "b", "c/b", "b"),
				composeNode("c", ItemTypeFolder, "c", "a/c", "c"),
				composeNode("g", ItemTypeFolder, "g", "b/g", "g"),
			},
			wantRemote: []string{"c->a/c", "a->b/a"}, wantDeferred: map[string]bool{"b": true, "g": true},
			creates: []string{"a", "c"},
		},
		{
			name: "a ring of four", made: []string{"a", "b", "c", "d"},
			nodes: []moveNode{
				composeNode("a", ItemTypeFolder, "a", "b/a", "a"),
				composeNode("b", ItemTypeFolder, "b", "c/b", "b"),
				composeNode("c", ItemTypeFolder, "c", "d/c", "c"),
				composeNode("d", ItemTypeFolder, "d", "a/d", "d"),
			},
			wantRemote: []string{"d->a/d", "a->b/a", "b->c/b"}, wantDeferred: map[string]bool{"c": true},
			creates: []string{"a", "b", "d"},
		},
		{
			name: "two files each into a new folder at the other's path", made: []string{"a", "b"},
			nodes: []moveNode{
				composeNode("a", ItemTypeFile, "a", "b/a", "a"),
				composeNode("b", ItemTypeFile, "b", "a/b", "b"),
			},
			wantDeferred: map[string]bool{"a": true, "b": true},
		},
		{
			name: "a ring through a file: b into a new a, a into a new g, g into a new b", made: []string{"a", "g", "b"},
			nodes: []moveNode{
				composeNode("b", ItemTypeFile, "b", "a/b", "b"),
				composeNode("a", ItemTypeFile, "a", "g/a", "a"),
				composeNode("g", ItemTypeFolder, "g", "b/g", "g"),
			},
			wantDeferred: map[string]bool{"a": true, "b": true, "g": true},
		},
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// The OneDrive twin of the rows above, made here: x moved to x2, deeper to
// zz/a/x2 or into a new folder, a new x or X made, alone or with x/sub in it,
// and f moved into it. On OneDrive the folder made here is created only once
// x has left its path there, by x's own move of the plan, so f's move into it
// waits while OneDrive keeps x at that path this pass: when x's rename
// collides with X2, made on OneDrive, and x stays where OneDrive has it (the
// composition inputs of
// TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsTheOldFolderThere),
// whether f lands in x, X or x/sub; while x is a file still to be decided,
// even one that then moves, or frozen. When x waits in a new folder whose
// rename collides with B2 or Y2 (the composition inputs of
// TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileTheOldFolderWaitsInANewFolder),
// or for a file deleted here at its destination below a renamed a (those of
// TestRunOnce_MoveIntoAFolderMadeHereWhileTheOldFolderWaitsForADeleteConverges),
// f's move is planned and then taken back, whichever is decided first, and in
// a chain of such folders, m into a new b and b into a new c, each move is
// taken back in turn. A move that waits so is deferred at once: p, which
// swaps with f through x's rename, is not parked to free f's destination. Of
// two items each moved into a new folder at the other's path, the one decided
// second waits, as its holder's move lands at its own path, and so does a
// folder decided before the file it waits for: the move of the other lands in
// the old folder of the one that waits, which carries it along the next pass,
// and rings of three and four converge the same way. A folder g moved into
// the same new folder is not one of them and waits. Where the item that waits
// is a file, two files each into a new folder at the other's path or a ring
// through a file, nothing moves: the move of the other would land in the file
// OneDrive keeps at that path, which refuses it on every pass. Run anyway,
// f's move would land in the old x on OneDrive, or the moves would wait for
// each other, a cycle dropped on every pass. Where x moves away, f's move is
// planned with it, whichever composition decides first, and runs after it and
// the new folders.
func TestComposeMoves_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsAnItemAtItsPath(t *testing.T) {
	t.Parallel()

	for _, tc := range heldRemoteLandingCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			occupancy := composeOccupancy(tc.nodes)
			for _, made := range tc.made {
				occupancy.local[made] = true
			}
			if tc.onOneDrive != "" {
				occupancy.remote[tc.onOneDrive] = true
			}
			c := composeMoves(tc.nodes, occupancy, tc.frozen)

			assert.Equal(t, tc.wantRemote, composedMovesBySide(&c, moveCoordRemote))
			assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
			if tc.wantDeferred == nil {
				tc.wantDeferred = map[string]bool{}
			}
			assert.Equal(t, tc.wantDeferred, c.deferred)

			actions := composedMoveActions(&c, tc.nodes)
			for _, made := range tc.creates {
				actions = append(actions, Action{Type: ActionFolderCreate, Path: made, CreateSide: CreateRemote})
			}
			kept, _, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, len(actions), "every action of the plan has an order")
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// An item moved into a new folder made at its own path on the other side, a
// file or a folder with a file in it, holds that path itself: the folder can
// take the path only once the item has left it, and the item's move lands in
// the folder. The item moves aside to a free name beside its source, on
// OneDrive when the folder was made here and here when OneDrive made it,
// carrying what is in it, and waits for the next pass, which makes the folder
// at the path it left and moves the item into it. Waiting for itself, a file
// stayed where it was on every pass, and a folder's move and the create of
// the folder it lands in waited for each other, a cycle dropped on every
// pass. When a folder above the item moves on OneDrive in the same plan, the
// item waits for the next pass instead, which finds it at its new path: its
// parent's move, or that of a folder above an unmoved parent. Either way the
// item is deferred, and the folder's path, as the plan names it, is held
// (ownLandings) with everything below it, compared as OneDrive compares
// names: nothing is made there this pass. The rows with a folder moving above
// the item move it on OneDrive; on this computer such a folder hides the item
// from the lookup, and nothing is held (a known limit, holdOwnLandings).
func TestComposeMoves_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		nodes []moveNode
		// madeHere and madeOnOneDrive are the new folders.
		madeHere, madeOnOneDrive []string
		wantLocal, wantRemote    []string
		wantDeferred             map[string]bool
		// landing is the path of the folder at the item's own path on the
		// side the item moves on, held with what lies below it.
		landing string
	}{
		{
			name:     "a file moved here into a new folder at its own path",
			nodes:    []moveNode{composeNode("f", ItemTypeFile, "f", "f/f", "f")},
			madeHere: []string{"f"}, wantRemote: []string{"f->f.swap-1"}, wantDeferred: map[string]bool{"f": true},
			landing: "f",
		},
		{
			name: "a folder moved here into a new folder at its own path",
			nodes: []moveNode{
				composeNode("k", ItemTypeFolder, "k", "k/k", "k"),
				composeNode("c", ItemTypeFile, "k/c.txt", "k/k/c.txt", "k/c.txt"),
			},
			madeHere: []string{"k"}, wantRemote: []string{"k->k.swap-1"}, wantDeferred: map[string]bool{"k": true},
			landing: "k",
		},
		{
			name:           "a file moved on OneDrive into a new folder at its own path",
			nodes:          []moveNode{composeNode("f", ItemTypeFile, "f", "f", "f/f")},
			madeOnOneDrive: []string{"f"}, wantLocal: []string{"f->f.swap-1"}, wantDeferred: map[string]bool{"f": true},
			landing: "f",
		},
		{
			name: "the folder above it renamed on OneDrive too",
			nodes: []moveNode{
				composeNode("P", ItemTypeFolder, "P", "Q", "P"),
				composeNode("f", ItemTypeFile, "P/f", "Q/f/f", "P/f"),
			},
			madeHere: []string{"Q/f"}, wantRemote: []string{"P->Q"}, wantDeferred: map[string]bool{"f": true},
			landing: "Q/f",
		},
		{
			name: "a folder above its unmoved parent renamed on OneDrive too",
			nodes: []moveNode{
				composeNode("Y", ItemTypeFolder, "Y", "Z", "Y"),
				composeNode("P", ItemTypeFolder, "Y/P", "Z/P", "Y/P"),
				composeNode("f", ItemTypeFile, "Y/P/f", "Z/P/f/f", "Y/P/f"),
			},
			madeHere: []string{"Z/P/f"}, wantRemote: []string{"Y->Z"}, wantDeferred: map[string]bool{"f": true},
			landing: "Z/P/f",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			occupancy := composeOccupancy(tc.nodes)
			for _, made := range tc.madeHere {
				occupancy.local[made] = true
			}
			for _, made := range tc.madeOnOneDrive {
				occupancy.remote[made] = true
			}
			c := composeMoves(tc.nodes, occupancy, nil)

			assert.Equal(t, tc.wantLocal, composedMovesBySide(&c, moveCoordLocal))
			assert.Equal(t, tc.wantRemote, composedMovesBySide(&c, moveCoordRemote))
			assert.Equal(t, tc.wantDeferred, c.deferred)
			side, other := moveCoordRemote, moveCoordLocal
			if len(tc.madeOnOneDrive) > 0 {
				side, other = moveCoordLocal, moveCoordRemote
			}
			for _, held := range []string{tc.landing, tc.landing + "/a", tc.landing + "/a/f", strings.ToUpper(tc.landing) + "/a"} {
				assert.True(t, atOrBelowOwnLanding(held, c.ownLandings[side]), "nothing is made at %s", held)
			}
			assert.False(t, atOrBelowOwnLanding(tc.landing+".swap-1", c.ownLandings[side]), "the parking name is free")
			assert.Empty(t, c.ownLandings[other], "nothing is held on the other side")

			actions := composedMoveActions(&c, tc.nodes)
			kept, _, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, len(actions), "every action of the plan has an order")
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive Q is renamed R, the folders z and h inside it trade names, a new
// Q is made with a new folder z in it, and m is moved into that z. On this
// computer m's move lands in the folder at Q/z, which the old z holds until
// its own move, so the old z is not the item parked to break the swap: h is
// parked beside its path, and the old z waits for the next pass. Parked
// instead, the old z would leave Q/z to h, which m's move would then land in.
// m's move also waits for Q's, as it lands below the path Q vacates, so it
// finds no Q/z when it runs, and a later pass, which makes the new Q, moves it.
func TestComposeMoves_FolderAMoveLandsInIsNotParkedToBreakASwap(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("Q", ItemTypeFolder, "Q", "Q", "R"),
		composeNode("z", ItemTypeFolder, "Q/z", "Q/z", "R/h"),
		composeNode("h", ItemTypeFolder, "Q/h", "Q/h", "R/z"),
		composeNode("m", ItemTypeFile, "m", "m", "Q/z/m"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.remote["Q"] = true
	occupancy.remote["Q/z"] = true
	c := composeMoves(nodes, occupancy, nil)

	require.Equal(t, []string{"Q->R", "m->Q/z/m", "Q/h->Q/h.swap-1"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
	assert.Equal(t, map[string]bool{"z": true, "h": true}, c.deferred)

	actions := composedMoveActions(&c, nodes)
	kept, deps, _ := isolateDependencyCycles(actions, nil)
	assert.Len(t, kept, len(actions))
	assert.Contains(t, deps[1], 0, "m's move waits for Q's")
}

// Validates: R-2.17.6
//
// A loop through that wait lasts one pass. f moved to f2 and made again,
// f2/b.txt moved into the new f as d.txt, and a folder g moved to f/b.txt,
// the path b.txt still holds on OneDrive, on this computer. b.txt lands where
// f's move vacates, so it waits for every folder OneDrive relocates, g among
// them, while g waits for b.txt to leave its destination: the end of
// composition defers both, and the pass moves f alone. In the next pass f is
// moved, b.txt lands where no move vacates, and both moves are planned.
func TestComposeMoves_FolderMovedOntoAWaitingFilesPathWaitsOnePass(t *testing.T) {
	t.Parallel()

	first := []moveNode{
		composeNode("f", ItemTypeFolder, "f", "f2", "f"),
		composeNode("b", ItemTypeFile, "f/b.txt", "f/d.txt", "f/b.txt"),
		composeNode("c", ItemTypeFile, "f/c.txt", "f2/c.txt", "f/c.txt"),
		composeNode("g", ItemTypeFolder, "g", "f/b.txt", "g"),
		composeNode("g-file", ItemTypeFile, "g/g.txt", "f/b.txt/g.txt", "g/g.txt"),
	}
	occupancy := composeOccupancy(first)
	occupancy.local["f"] = true
	one := composeMoves(first, occupancy, nil)

	assert.Equal(t, []string{"f->f2"}, composedMovesBySide(&one, moveCoordRemote), "first pass")
	assert.Equal(t, map[string]bool{"b": true, "g": true}, one.deferred, "first pass")

	second := []moveNode{
		composeNode("f", ItemTypeFolder, "f2", "f2", "f2"),
		composeNode("new-f", ItemTypeFolder, "f", "f", "f"),
		composeNode("b", ItemTypeFile, "f2/b.txt", "f/d.txt", "f2/b.txt"),
		composeNode("c", ItemTypeFile, "f2/c.txt", "f2/c.txt", "f2/c.txt"),
		composeNode("g", ItemTypeFolder, "g", "f/b.txt", "g"),
		composeNode("g-file", ItemTypeFile, "g/g.txt", "f/b.txt/g.txt", "g/g.txt"),
	}
	two := composeMoves(second, composeOccupancy(second), nil)

	assert.ElementsMatch(t, []string{"f2/b.txt->f/d.txt", "g->f/b.txt"},
		composedMovesBySide(&two, moveCoordRemote), "second pass")
	assert.Empty(t, two.deferred, "second pass")
}

// Validates: R-2.17.6
//
// An item whose move composition holds for the next pass keeps its baseline
// row: the plan drops SQLite's baseline update of it, which a folder move of
// the plan would leave addressed at a path the item no longer has. An item
// whose held move the mode never runs keeps its update.
func TestDropReconciliationOfComposedMoves_HeldMoveKeepsItsBaselineRow(t *testing.T) {
	t.Parallel()

	held := &moveComposition{
		deferred:      map[string]bool{"item-b": true},
		deferredMoves: map[string][]actionType{"item-b": {ActionRemoteMove}},
	}
	update := Action{Type: ActionBaselineUpdate, Path: "f/b.txt", ItemID: "item-b"}
	other := Action{Type: ActionBaselineUpdate, Path: "f/c.txt", ItemID: "item-c"}

	assert.Equal(t, []Action{other}, dropReconciliationOfComposedMoves([]Action{update, other}, held, SyncBidirectional))
	assert.Equal(t, []Action{update, other},
		dropReconciliationOfComposedMoves([]Action{update, other}, held, SyncDownloadOnly),
		"download-only never moves the item on OneDrive")
}

// heldSourceDeleteCase is one row of
// TestPlanBuiltActions_ADeleteAboveAMoveHeldForTheNextPassWaitsWithIt.
type heldSourceDeleteCase struct {
	name string
	node moveNode
	// held is the moves composition holds for the next pass, nil for none;
	// moved is the move it composes, built as composed.
	held     []actionType
	moved    []composedMove
	composed []Action
	// waiting is the creates that wait with a held move, built aside.
	waiting []Action
	actions []Action
	mode    SyncMode
	// want lists the actions the plan keeps, as "type path".
	want                []string
	dropped, unfinished int
	deferredByMode      DeferredCounts
}

func heldSourceDeleteCases() []heldSourceDeleteCase {
	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	remoteDelete := func(p string, view *pathView) Action { return Action{Type: ActionRemoteDelete, Path: p, View: view} }
	localDelete := func(p string, view *pathView) Action { return Action{Type: ActionLocalDelete, Path: p, View: view} }
	remoteMove := func(from, to string, view *pathView) Action {
		return Action{Type: ActionRemoteMove, ItemID: "item-" + from, OldPath: from, Path: to, View: view}
	}
	madeHere := func(p string) Action {
		return Action{Type: ActionUpload, Path: p, View: &pathView{Local: &localState{ItemType: ItemTypeFile}}}
	}
	onOneDrive, here := []actionType{ActionRemoteMove}, []actionType{ActionLocalMove}
	heldInP := composeNode("f", ItemTypeFolder, "P/f", "x/f", "P/f")

	return append([]heldSourceDeleteCase{
		{
			name: "P deleted here holds f, whose OneDrive move waits", node: heldInP, held: onOneDrive,
			actions: []Action{
				remoteDelete("P", folder), remoteDelete("P/old.txt", file), remoteDelete("Pa", folder),
				remoteMove("P/g", "y/g", file),
			},
			want:    []string{"remote_delete P/old.txt", "remote_delete Pa", "remote_move y/g"},
			dropped: 1, unfinished: 2,
		},
		{
			name: "P and P/Q deleted here hold f", held: onOneDrive,
			node:    composeNode("f", ItemTypeFolder, "P/Q/f", "x/f", "P/Q/f"),
			actions: []Action{remoteDelete("P/Q", folder), remoteDelete("P", folder), remoteDelete("R", folder)},
			want:    []string{"remote_delete R"}, dropped: 2, unfinished: 3,
		},
		{
			name: "P deleted on OneDrive holds f here, whose local move waits", held: here,
			node:    composeNode("f", ItemTypeFolder, "P/f", "P/f", "x/f"),
			actions: []Action{localDelete("P", folder), localDelete("P/gone.txt", file)},
			want:    []string{"local_delete P/gone.txt"}, dropped: 1, unfinished: 2,
		},
		{
			name: "a file made here at P's path waits with P's delete", node: heldInP, held: onOneDrive,
			actions: []Action{remoteDelete("P", folder), madeHere("P")},
			dropped: 2, unfinished: 3,
		},
		{
			name: "a file made here at p, the same name as P on OneDrive, waits with P's delete", node: heldInP, held: onOneDrive,
			actions: []Action{remoteDelete("P", folder), madeHere("p")},
			dropped: 2, unfinished: 3,
		},
		{
			name: "download-only holds P's delete back itself", node: heldInP, held: onOneDrive, mode: SyncDownloadOnly,
			actions:        []Action{remoteDelete("P", folder)},
			deferredByMode: DeferredCounts{RemoteDeletes: 1},
		},
		{
			name:  "f's move into a folder made here where OneDrive edited the file K, trimmed below the keep-both K",
			node:  composeNode("f", ItemTypeFolder, "P/f", "K/f", "P/f"),
			moved: []composedMove{{key: "f", side: moveCoordRemote, oldPath: "P/f", newPath: "K/f", localPath: "K/f"}},
			composed: []Action{{
				Type: ActionRemoteMove, ItemID: "f", OldPath: "P/f", Path: "K/f", View: folder,
			}},
			actions: []Action{
				{Type: ActionConflictCopy, Path: "K", View: &pathView{
					Local: &localState{ItemType: ItemTypeFolder}, Remote: &remoteState{ItemType: ItemTypeFile},
				}},
				remoteDelete("P", folder),
			},
			want: []string{"conflict_copy K"}, dropped: 2, unfinished: 2,
		},
		{
			name: "f's move planned: P's delete runs after it", node: heldInP,
			moved:    []composedMove{{key: "f", side: moveCoordRemote, oldPath: "P/f", newPath: "x/f", localPath: "x/f"}},
			composed: []Action{{Type: ActionRemoteMove, ItemID: "f", OldPath: "P/f", Path: "x/f", View: folder}},
			actions:  []Action{remoteDelete("P", folder), madeHere("P")},
			want:     []string{"remote_delete P", "upload P", "remote_move x/f"},
		},
	}, slices.Concat(
		carriedHeldDeleteCases(folder, file), heldWriteDeleteCases(folder), cycledWriteDeleteCases(folder),
		aheadOfContentDeleteCases())...)
}

// heldWriteDeleteCases are the rows of heldSourceDeleteCases in which a held
// create writes the content of a synced item another delete removes.
func heldWriteDeleteCases(folder *pathView) []heldSourceDeleteCase {
	synced := func(inode uint64, itemID string) *pathView {
		return &pathView{Baseline: &BaselineEntry{
			ItemType: ItemTypeFile, ItemID: itemID, LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
		}}
	}
	uploadOf := func(p string, inode uint64) Action {
		return Action{Type: ActionUpload, Path: p, View: &pathView{Local: &localState{
			ItemType: ItemTypeFile, LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
		}}}
	}
	heldInP := composeNode("f", ItemTypeFolder, "P/f", "x/f", "P/f")

	return []heldSourceDeleteCase{
		{
			name: "z renamed here onto P/f, which f's waiting move leaves: OneDrive's delete of z waits with z's upload",
			node: heldInP, held: []actionType{ActionRemoteMove},
			waiting: []Action{uploadOf("P/f", 7)},
			actions: []Action{
				{Type: ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
				{Type: ActionRemoteDelete, Path: "y", View: synced(8, "item-y")},
			},
			want: []string{"remote_delete y"}, dropped: 1, unfinished: 2,
		},
		{
			name: "z renamed here onto P, deleted here: z's delete waits with the upload at P, which waits with P's delete",
			node: heldInP, held: []actionType{ActionRemoteMove},
			actions: []Action{
				{Type: ActionRemoteDelete, Path: "P", View: folder},
				uploadOf("P", 7),
				{Type: ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
			},
			dropped: 3, unfinished: 4,
		},
		{
			name: "y moved on OneDrive onto a path here that f's waiting move leaves: the delete of y here waits with its download",
			node: composeNode("f", ItemTypeFolder, "P/f", "P/f", "x/f"), held: []actionType{ActionLocalMove},
			waiting: []Action{{Type: ActionDownload, Path: "P/f", ItemID: "item-y", View: &pathView{}}},
			actions: []Action{{Type: ActionLocalDelete, Path: "y", View: synced(8, "item-y")}},
			dropped: 1, unfinished: 2,
		},
		{
			name: "g made on OneDrive in P, whose move into x here waits: g's download, rebased below x, waits with it",
			node: composeNode("P", ItemTypeFolder, "P", "x/P", "P"), held: []actionType{ActionRemoteMove},
			actions: []Action{
				{Type: ActionDownload, Path: "P/g", ItemID: "item-g", View: &pathView{
					Path: "P/g", Remote: &remoteState{ItemType: ItemTypeFile},
				}},
				{Type: ActionDownload, Path: "y/h", ItemID: "item-h", View: &pathView{
					Path: "y/h", Remote: &remoteState{ItemType: ItemTypeFile},
				}},
			},
			want: []string{"download y/h"}, dropped: 1, unfinished: 2,
		},
		{
			name: "a waiting upload of another file holds no delete",
			node: heldInP, held: []actionType{ActionRemoteMove},
			waiting: []Action{uploadOf("P/f", 9)},
			actions: []Action{{Type: ActionRemoteDelete, Path: "z", View: synced(7, "item-z")}},
			want:    []string{"remote_delete z"}, unfinished: 1,
		},
		{
			name: "z renamed here onto P/f, which f still holds on OneDrive this pass: the upload there waits, and so does z's delete",
			node: heldInP, held: []actionType{ActionRemoteMove},
			actions: []Action{
				uploadOf("P/f", 7),
				{Type: ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
			},
			dropped: 2, unfinished: 3,
		},
		{
			name: "z renamed here into the folder made at K, where OneDrive edited the file K: the upload trimmed below the keep-both K holds z's delete",
			node: composeNode("y", ItemTypeFile, "y", "y", "y"),
			actions: []Action{
				{Type: ActionConflictCopy, Path: "K", View: &pathView{
					Local: &localState{ItemType: ItemTypeFolder}, Remote: &remoteState{ItemType: ItemTypeFile},
				}},
				uploadOf("K/z", 7),
				{Type: ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
			},
			want: []string{"conflict_copy K"}, dropped: 2, unfinished: 2,
		},
	}
}

// aheadOfContentDeleteCases are the rows of heldSourceDeleteCases in which a
// create writes a deleted file's content but waits for that delete, so the
// delete cannot wait for it in the plan.
func aheadOfContentDeleteCases() []heldSourceDeleteCase {
	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}, Remote: &remoteState{ItemType: ItemTypeFolder}}
	elsewhere := composeNode("y", ItemTypeFile, "y", "y", "y")
	renamed := func(from, to string) Action {
		return Action{Type: ActionRemoteMove, ItemID: "item-" + from, OldPath: from, Path: to, View: folder}
	}
	movedUpOntoP := []Action{
		renamed("P", "Q"),
		{Type: ActionRemoteDelete, Path: "P/f", View: syncedFileView(7, "item-f")},
		uploadOfIdentity("P", 7),
	}

	return []heldSourceDeleteCase{
		{
			name: "f moved up to P, the path its folder left as it was renamed Q: P/f's delete waits a pass, carried along",
			node: elsewhere, actions: slices.Clone(movedUpOntoP),
			want: []string{"remote_move Q", "upload P"}, dropped: 1, unfinished: 1,
		},
		{
			name: "the same in download-only: P/f's delete is left to the mode, which holds back all three",
			node: elsewhere, actions: slices.Clone(movedUpOntoP), mode: SyncDownloadOnly,
			deferredByMode: DeferredCounts{Moves: 1, Uploads: 1, RemoteDeletes: 1},
		},
		{
			name: "a1.txt edited in A renamed B: the upload where A's move carries a1.txt needs its old copy deleted first",
			node: elsewhere,
			actions: []Action{
				renamed("A", "B"),
				{Type: ActionRemoteDelete, Path: "A/a1.txt", View: syncedFileView(7, "item-a1")},
				uploadOfIdentity("B/a1.txt", 7),
			},
			want: []string{"remote_move B", "remote_delete A/a1.txt", "upload B/a1.txt"},
		},
		{
			name: "x moved up onto its folder P's path, P deleted: the upload waits for P's delete, which waits for P/x's",
			node: elsewhere,
			actions: []Action{
				{Type: ActionRemoteDelete, Path: "P", View: folder},
				{Type: ActionRemoteDelete, Path: "P/x", View: syncedFileView(8, "item-x")},
				uploadOfIdentity("P", 8),
			},
			want: []string{"remote_delete P", "remote_delete P/x", "upload P"},
		},
	}
}

// cycledWriteDeleteCases are the rows of heldSourceDeleteCases in which the
// create that writes a deleted file's content is caught in a dependency
// cycle, planned as such: a folder's move into a new folder made at its own
// path, whose create waits for the move and the move for the create, or a
// move into a new folder whose create waits for an item at a name OneDrive
// takes for the same to move into that move's destination.
func cycledWriteDeleteCases(folder *pathView) []heldSourceDeleteCase {
	synced := &pathView{Baseline: &BaselineEntry{
		ItemType: ItemTypeFile, ItemID: "item-z", LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
	}}
	uploadOfZ := func(p string) Action {
		return Action{Type: ActionUpload, Path: p, View: &pathView{Local: &localState{
			ItemType: ItemTypeFile, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
		}}}
	}
	cycle := []Action{
		{Type: ActionRemoteMove, ItemID: "item-x", OldPath: "x", Path: "x/x2", View: folder},
		{Type: ActionFolderCreate, Path: "x", CreateSide: CreateRemote},
	}
	elsewhere := composeNode("y", ItemTypeFile, "y", "y", "y")

	return []heldSourceDeleteCase{
		{
			name: "z renamed here into x/x2: its upload waits for x's move, and z's delete for the upload",
			node: elsewhere,
			actions: slices.Concat(cycle, []Action{
				uploadOfZ("x/x2/z"), {Type: ActionRemoteDelete, Path: "z", View: synced},
			}),
			dropped: 4, unfinished: 4,
		},
		{
			name: "x/z renamed here onto x/x2/w: z's delete, which x's move waits for, waits with the upload after that move",
			node: elsewhere,
			actions: slices.Concat(cycle, []Action{
				{Type: ActionRemoteDelete, Path: "x/z", View: synced}, uploadOfZ("x/x2/w"),
			}),
			dropped: 4, unfinished: 4,
		},
		{
			name: "z moved up onto its folder P's path, P deleted, P/y's move out of P in a cycle: the upload after P's delete holds z's delete",
			node: elsewhere,
			actions: []Action{
				// P/y into a new k, whose create waits for K, which OneDrive
				// takes for k, to move away, into P/y's destination.
				{Type: ActionRemoteMove, ItemID: "item-y", OldPath: "P/y", Path: "k/y", View: folder},
				{Type: ActionFolderCreate, Path: "k", CreateSide: CreateRemote},
				{Type: ActionRemoteMove, ItemID: "item-K", OldPath: "K", Path: "k/y/K", View: folder},
				{Type: ActionRemoteDelete, Path: "P", View: &pathView{
					Baseline: &BaselineEntry{ItemType: ItemTypeFolder}, Remote: &remoteState{ItemType: ItemTypeFolder},
				}},
				{Type: ActionRemoteDelete, Path: "P/z", View: synced},
				uploadOfZ("P"),
			},
			dropped: 6, unfinished: 6,
		},
	}
}

// carriedHeldDeleteCases are the rows of heldSourceDeleteCases in which a
// folder's move has a held action below its source.
func carriedHeldDeleteCases(folder, file *pathView) []heldSourceDeleteCase {
	heldInAP := composeNode("f", ItemTypeFolder, "A/P/f", "x/f", "A/P/f")
	renameA := composedMove{key: "A", side: moveCoordRemote, oldPath: "A", newPath: "B", localPath: "B"}
	madeHere := func(p string) Action {
		return Action{Type: ActionUpload, Path: p, View: &pathView{Local: &localState{ItemType: ItemTypeFile}}}
	}

	return []heldSourceDeleteCase{
		{
			name: "A renamed B here carries P, which holds f, and B/a1.txt edited is uploaded after it",
			node: heldInAP, held: []actionType{ActionRemoteMove},
			moved:    []composedMove{renameA},
			composed: []Action{{Type: ActionRemoteMove, ItemID: "A", OldPath: "A", Path: "B", View: folder}},
			actions: []Action{
				{Type: ActionRemoteDelete, Path: "A/P", View: folder},
				{Type: ActionRemoteDelete, Path: "A/a1.txt", View: file},
				madeHere("B/a1.txt"), madeHere("B/new.txt"),
			},
			want:    []string{"remote_delete A/a1.txt", "upload B/a1.txt", "upload B/new.txt", "remote_move B"},
			dropped: 1, unfinished: 2,
		},
		{
			name: "A renamed B on OneDrive carries P here, which holds f",
			node: composeNode("f", ItemTypeFolder, "A/P/f", "A/P/f", "x/f"), held: []actionType{ActionLocalMove},
			moved: []composedMove{{key: "A", side: moveCoordLocal, oldPath: "A", newPath: "B", localPath: "A"}},
			composed: []Action{{
				Type: ActionLocalMove, ItemID: "A", OldPath: "A", Path: "B", View: folder,
			}},
			actions: []Action{
				{Type: ActionLocalDelete, Path: "A/P", View: folder},
				{Type: ActionDownload, Path: "B/new.txt", View: &pathView{Remote: &remoteState{ItemType: ItemTypeFile}}},
			},
			want:    []string{"download B/new.txt", "local_move B"},
			dropped: 1, unfinished: 2,
		},
		{
			name: "A moved into p/A, where c leaves A for p, the same name as P on OneDrive, waits with c's move",
			node: composeNode("f", ItemTypeFolder, "P/f", "x/f", "P/f"), held: []actionType{ActionRemoteMove},
			actions: []Action{
				{Type: ActionRemoteDelete, Path: "P", View: folder},
				{Type: ActionRemoteMove, ItemID: "c", OldPath: "A/c", Path: "p", View: folder},
				{Type: ActionRemoteMove, ItemID: "A", OldPath: "A", Path: "p/A", View: folder},
			},
			dropped: 3, unfinished: 4,
		},
	}
}

// Validates: R-2.17.2, R-2.17.6
//
// A folder holds an item whose move composition holds for the next pass: on
// the side the move waits on, the delete of that folder, and of every folder
// above the item, waits with it, and so does what waits for that delete, such
// as the upload of a file made at the folder's path or at a name OneDrive
// takes for it, and a move of c onto such a name with the move of A into c,
// which would land in the folder OneDrive keeps there. So does the delete of
// a folder that holds an item whose composed move the plan no longer holds:
// the move of f into a folder made here at K, where OneDrive edited the file
// K, is trimmed below the keep-both path K. Run, a delete on OneDrive took the
// item with it into the recycle bin, and a delete here failed on a folder
// that is not empty. Deletes of other paths, P/old.txt below the folder and
// Pa beside it, and the move of P/g out of it run, and the plan counts what it
// holds as work for the next pass. A move of a folder above the held delete,
// A renamed B on either side, runs and carries the folder and the item
// along, with what waits for it, such as the upload of a1.txt edited in it;
// held with the delete, it left the delete of a1.txt's old copy, which it
// waits for, to run alone. A create held for the next pass holds the delete of
// the synced item whose content it writes: z renamed here onto a path f's
// waiting move leaves, its upload waiting with that move, keeps z on OneDrive,
// whether the upload waits as the new entry at f's old path, behind P's
// delete, or as a create onto the path f still holds there, and so do the
// delete here of an item whose download waits and an upload of z's content
// trimmed below a keep-both path; a held upload of another file holds
// nothing. So does a create caught in a dependency cycle (a folder's move into
// a new folder at its own path, planned as such): one after the cycle takes
// the delete with it, as the delete waits for it; one that waits for the
// delete through x's move, which carries the file, leaves the delete waiting a
// pass anyway; and one that waits for it through P's delete, so the two have
// no edge, holds it as held work. A create that waits for the delete only
// through the move of a folder above the file, and lands elsewhere than where
// that move carries it, makes the delete wait a pass: P renamed Q here and
// Q/f moved up to P. One at the carried path, a1.txt edited in A renamed B,
// and one behind a folder's delete, x moved up onto P's path with P deleted,
// keep the delete first. A download
// rebased through a folder whose OneDrive move waits, g made on OneDrive in P
// while P waits to move into x here, waits with that move. A mode that holds
// the delete back itself, as download-only does on OneDrive, defers it as
// before, and once the item's move is in the plan the delete runs after it.
func TestPlanBuiltActions_ADeleteAboveAMoveHeldForTheNextPassWaitsWithIt(t *testing.T) {
	t.Parallel()

	for _, tc := range heldSourceDeleteCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			composition := &moveComposition{moves: tc.moved, deferred: map[string]bool{}, deferredMoves: map[string][]actionType{}}
			if tc.held != nil {
				composition.deferred[tc.node.key] = true
				composition.deferredMoves[tc.node.key] = tc.held
			}
			inputs := &moveInputs{nodes: []moveNode{tc.node}}
			plan, err := newPlanner(nil).planBuiltActions(
				tc.actions, tc.composed, tc.waiting, composition, inputs, plannerMountContext{}, tc.mode)
			require.NoError(t, err)

			var kept []string
			for i := range plan.Actions {
				kept = append(kept, plan.Actions[i].Type.String()+" "+plan.Actions[i].Path)
			}
			assert.Equal(t, tc.want, kept)
			assert.Equal(t, tc.dropped, plan.FollowUp.Dropped, "dropped")
			assert.Equal(t, tc.unfinished, plan.FollowUp.Unfinished, "unfinished")
			assert.Equal(t, tc.deferredByMode, plan.DeferredByMode)
		})
	}
}

// Validates: R-2.17.2
//
// Each delete the plan holds back for work held for the next pass logs, at
// Debug, the delete and the path of the work it waits for, so a pass that
// leaves work names the folder that waits: the deletes of P/Q and P name f,
// whose move out of them waits, the delete of S/g, moved here up to S as S
// was renamed T, names the upload at S that writes its content, and the
// delete of R, which runs, logs nothing.
func TestPlanBuiltActions_AHeldDeleteLogsTheItemItWaitsFor(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	composition := &moveComposition{
		deferred:      map[string]bool{"f": true},
		deferredMoves: map[string][]actionType{"f": {ActionRemoteMove}},
	}
	inputs := &moveInputs{nodes: []moveNode{composeNode("f", ItemTypeFolder, "P/Q/f", "x/f", "P/Q/f")}}
	actions := []Action{
		{Type: ActionRemoteDelete, Path: "P/Q", View: folder},
		{Type: ActionRemoteDelete, Path: "P", View: folder},
		{Type: ActionRemoteDelete, Path: "R", View: folder},
		{
			Type: ActionRemoteMove, ItemID: "item-S", OldPath: "S", Path: "T",
			View: &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}, Remote: &remoteState{ItemType: ItemTypeFolder}},
		},
		{Type: ActionRemoteDelete, Path: "S/g", View: syncedFileView(7, "item-g")},
		uploadOfIdentity("S", 7),
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, err := newPlanner(logger).planBuiltActions(actions, nil, nil, composition, inputs, plannerMountContext{}, SyncBidirectional)
	require.NoError(t, err)

	var held []string
	for line := range strings.Lines(logs.String()) {
		if _, fields, found := strings.Cut(line, `msg="delete waits for work held for the next pass" `); found {
			held = append(held, strings.TrimSpace(fields))
		}
	}
	assert.Equal(t, []string{
		"action=remote_delete path=P/Q held_path=P/Q/f",
		"action=remote_delete path=P held_path=P/Q/f",
		"action=remote_delete path=S/g held_path=S",
	}, held)
}

// Validates: R-2.17.6
//
// Each create here that waits for the move on OneDrive of the folder it was
// rebased through logs, at Debug, the create and the folder's path here and
// on OneDrive, so a file that does not arrive names the folder it waits
// for: g, made on OneDrive in P while P waits to move into x here, names x/P
// and P, and h, made in y, which no move holds, logs nothing.
func TestPlanBuiltActions_AHeldRebasedCreateLogsTheFolderItWaitsFor(t *testing.T) {
	t.Parallel()

	composition := &moveComposition{
		deferred:      map[string]bool{"P": true},
		deferredMoves: map[string][]actionType{"P": {ActionRemoteMove}},
	}
	inputs := &moveInputs{nodes: []moveNode{composeNode("P", ItemTypeFolder, "P", "x/P", "P")}}
	madeOnOneDrive := func(p, itemID string) Action {
		return Action{Type: ActionDownload, Path: p, ItemID: itemID, View: &pathView{
			Path: p, Remote: &remoteState{ItemType: ItemTypeFile},
		}}
	}
	actions := []Action{madeOnOneDrive("P/g", "item-g"), madeOnOneDrive("y/h", "item-h")}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	plan, err := newPlanner(logger).planBuiltActions(actions, nil, nil, composition, inputs, plannerMountContext{}, SyncBidirectional)
	require.NoError(t, err)
	require.Len(t, plan.Actions, 1)
	assert.Equal(t, "y/h", plan.Actions[0].Path)

	var held []string
	for line := range strings.Lines(logs.String()) {
		if _, fields, found := strings.Cut(line, `msg="create waits for its folder's move on OneDrive" `); found {
			held = append(held, strings.TrimSpace(fields))
		}
	}
	assert.Equal(t, []string{"action=download path=x/P/g folder=x/P folder_on_onedrive=P"}, held)
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

// Validates: R-2.17.6
//
// A file moved away from a path that a new folder takes (OneDrive moved the
// file and created a folder there): work under the old path belongs to the
// new folder, so it waits for the move instead of the move waiting for it.
// Ordered the other way, the download into the folder failed against the
// file still in place, and the move that would clear it never ran (model
// test: remote swap, then a remote file replaced by a folder).
func TestBuildDependencies_WorkUnderAFileMovesOldPathWaitsForTheMove(t *testing.T) {
	t.Parallel()

	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	actions := []Action{
		{Type: ActionLocalMove, OldPath: "r.txt", Path: "b.txt", View: file}, // 0
		{Type: ActionDownload, Path: "r.txt/inside.txt", View: file},         // 1
	}

	deps := buildDependencies(actions)

	assert.Empty(t, deps[0], "a file has nothing under it to wait for")
	assert.Equal(t, []int{0}, deps[1], "the new folder's content lands after the file leaves")
	require.NoError(t, detectDependencyCycle(deps))
}

// Validates: R-2.17.1, R-2.17.6
//
// The user moved folder q to docs and re-created q, while OneDrive deleted
// q/x. The re-created q waits for the move vacating q, and the move waits for
// the cleanup under its source, so a cleanup of q/x that waited for the
// re-created q would close a cycle and docs would never reach OneDrive.
// Removing an entry never needs its parent created.
func TestBuildDependencies_DeleteUnderARecreatedFolderDoesNotWaitForTheCreate(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	actions := []Action{
		{Type: ActionRemoteMove, OldPath: "q", Path: "docs", View: folder}, // 0
		{Type: ActionFolderCreate, Path: "q", CreateSide: CreateRemote},    // 1: re-created at 0's old path
		{Type: ActionCleanup, Path: "q/x", View: folder},                   // 2: gone online, moved locally
	}

	deps := buildDependencies(actions)

	require.NoError(t, detectDependencyCycle(deps))
	assert.Equal(t, []int{0}, deps[1], "the re-created folder waits for the move")
	assert.NotContains(t, deps[2], 1, "a cleanup does not wait for a folder create")
}

// Validates: R-2.17.6
//
// Plans of TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges,
// made on this computer: the user moved docs to photos/photos, made a new
// docs, and moved docs/50%.txt into the new docs, as renamed-50%.txt or under
// its own name, with a run after each step or one run for all three. Every
// plan has an order, and nothing is dropped. In the one run, the folder moves
// first, the new docs waits for it, and the file's move, from where the
// folder move puts it, waits for both. Two rows are not taken from that
// scenario. One holds the same rule on the other side: a folder the plan
// creates on this computer at a path waits for the local move that leaves
// the path. The other holds a file's move, rebased through the folder's move,
// into a folder made inside the new docs: it waits for the folder move and
// for that inner folder, which waits for the new docs.
func TestBuildDependencies_MovesThroughAFolderRecreatedAtAVacatedPathHaveAnOrder(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	folderMove := Action{Type: ActionRemoteMove, OldPath: "docs", Path: "photos/photos", View: folder}
	recreate := Action{Type: ActionFolderCreate, Path: "docs", CreateSide: CreateRemote}
	fileMoveTo := func(p string) Action {
		return Action{Type: ActionRemoteMove, OldPath: "photos/photos/50%.txt", Path: p, View: file}
	}

	for _, tc := range []struct {
		name     string
		actions  []Action
		wantDeps [][]int
	}{
		{name: "first of two runs", actions: []Action{folderMove, recreate}, wantDeps: [][]int{nil, {0}}},
		{name: "second of two runs", actions: []Action{fileMoveTo("docs/renamed-50%.txt")}, wantDeps: [][]int{nil}},
		{
			name:     "one run",
			actions:  []Action{folderMove, fileMoveTo("docs/renamed-50%.txt"), recreate},
			wantDeps: [][]int{nil, {0, 2}, {0}},
		},
		{
			name:     "one run, the file under its own name",
			actions:  []Action{folderMove, fileMoveTo("docs/50%.txt"), recreate},
			wantDeps: [][]int{nil, {0, 2}, {0}},
		},
		{name: "a folder the plan creates on this computer at the path a local move leaves", actions: []Action{
			{Type: ActionLocalMove, OldPath: "docs", Path: "photos/photos", View: folder},
			{Type: ActionFolderCreate, Path: "docs", CreateSide: createLocal},
		}, wantDeps: [][]int{nil, {0}}},
		{name: "one run, into a folder made inside the new one", actions: []Action{
			folderMove,
			fileMoveTo("docs/new/50%.txt"),
			recreate,
			{Type: ActionFolderCreate, Path: "docs/new", CreateSide: CreateRemote},
		}, wantDeps: [][]int{nil, {0, 3}, {0}, {2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kept, deps, _ := isolateDependencyCycles(tc.actions, nil)
			assert.Len(t, kept, len(tc.actions), "no action is dropped as a cycle")
			require.NoError(t, detectDependencyCycle(deps))
			assert.Equal(t, tc.wantDeps, deps)
		})
	}
}

// Validates: R-2.17.6
//
// A move into a folder at the path another move of the same side vacates, from
// a source outside that path, waits for that move, whether or not the plan
// creates the folder there: x moved to x2 and f moved into a new x. Unordered,
// f's move would land in the old x if it ran first, which x's move would
// then carry to x2/f, and would find no x if it ran second. On OneDrive the
// plan also creates the new x, which waits for x's move, and f's move waits
// for both. Paths are compared as OneDrive compares names, so a move into a
// new X waits for x's move, and on OneDrive for the create of X too. A move
// from below the vacated path (a rename inside x, named x or X) is not held:
// x's move waits for it to leave first. A move of the other side is not held
// either.
func TestBuildDependencies_MoveIntoThePathAnotherMoveVacatesWaitsForIt(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	move := func(moveType actionType, from, to string, view *pathView) Action {
		return Action{Type: moveType, OldPath: from, Path: to, View: view}
	}

	for _, tc := range []struct {
		name     string
		actions  []Action
		wantDeps [][]int
	}{
		{name: "on this computer", actions: []Action{
			move(ActionLocalMove, "x", "x2", folder),
			move(ActionLocalMove, "f", "x/f", folder),
		}, wantDeps: [][]int{nil, {0}}},
		{name: "on OneDrive, with the new x", actions: []Action{
			move(ActionRemoteMove, "x", "x2", folder),
			move(ActionRemoteMove, "f", "x/f", folder),
			{Type: ActionFolderCreate, Path: "x", CreateSide: CreateRemote},
		}, wantDeps: [][]int{nil, {0, 2}, {0}}},
		{name: "into a new X", actions: []Action{
			move(ActionLocalMove, "x", "x2", folder),
			move(ActionLocalMove, "f", "X/f", folder),
		}, wantDeps: [][]int{nil, {0}}},
		{name: "on OneDrive, with the new X", actions: []Action{
			move(ActionRemoteMove, "x", "x2", folder),
			move(ActionRemoteMove, "f", "X/f", folder),
			{Type: ActionFolderCreate, Path: "X", CreateSide: CreateRemote},
		}, wantDeps: [][]int{nil, {0, 2}, {0}}},
		{name: "a rename inside the vacated path", actions: []Action{
			move(ActionLocalMove, "x", "x2", folder),
			move(ActionLocalMove, "x/a.txt", "x/b.txt", file),
		}, wantDeps: [][]int{{1}, nil}},
		{name: "a rename inside the vacated path, named X", actions: []Action{
			move(ActionLocalMove, "x", "x2", folder),
			move(ActionLocalMove, "x/a.txt", "X/b.txt", file),
		}, wantDeps: [][]int{{1}, nil}},
		{name: "a move of the other side", actions: []Action{
			move(ActionLocalMove, "x", "x2", folder),
			move(ActionRemoteMove, "f", "x/f", folder),
		}, wantDeps: [][]int{nil, nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kept, deps, _ := isolateDependencyCycles(tc.actions, nil)
			assert.Len(t, kept, len(tc.actions), "no action is dropped as a cycle")
			assert.Equal(t, tc.wantDeps, deps)
		})
	}
}

// Validates: R-2.17.6
//
// A folder deleted on OneDrive while a folder is made here at its path, x
// again, X or x/sub below a deleted x/sub, and f moved into the new folder:
// the create waits for the delete and f's move for the create, so the move
// never lands in the folder the delete removes.
func TestBuildDependencies_MoveIntoAFolderMadeAtADeletedPathWaitsForTheDeleteAndTheCreate(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	for _, tc := range []struct{ deleted, made string }{
		{deleted: "x", made: "x"},
		{deleted: "x", made: "X"},
		{deleted: "x/sub", made: "x/sub"},
	} {
		t.Run(tc.deleted+" deleted, "+tc.made+" made", func(t *testing.T) {
			t.Parallel()

			actions := []Action{
				{Type: ActionRemoteDelete, Path: tc.deleted, View: folder},
				{Type: ActionFolderCreate, Path: tc.made, CreateSide: CreateRemote},
				{Type: ActionRemoteMove, OldPath: "f", Path: tc.made + "/f", View: folder},
			}
			kept, deps, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, len(actions), "no action is dropped as a cycle")
			assert.Equal(t, [][]int{nil, {0}, {1}}, deps)
		})
	}
}

// Validates: R-2.17.6
//
// Work under a folder move's source that does not follow it leaves first: a
// rename inside the folder, a sibling moving elsewhere, a delete, and any of
// them when the folder made at the vacated path is on the other side. The one
// exception is a same-side move from the source into or below a folder the
// plan creates, on that side, at the path the folder move vacates: it waits
// for that folder, which waits for the folder move, and the folder move does
// not wait for it, so it runs after the folder move from its old source.
// Move composition plans such a move only when it neither rebases nor holds
// it (vacatingFolder); the last two rows are the plans of
// TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges,
// made here, and of
// TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges.
func TestBuildDependencies_ChildrenLeaveFirstUnlessTheyMoveIntoTheRecreatedSource(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	folderMove := Action{Type: ActionRemoteMove, OldPath: "docs", Path: "photos/photos", View: folder}
	fileMove := Action{Type: ActionRemoteMove, OldPath: "docs/a.txt", Path: "docs/b.txt", View: file}
	recreate := Action{Type: ActionFolderCreate, Path: "docs", CreateSide: CreateRemote}
	innerMove := Action{Type: ActionRemoteMove, OldPath: "docs/sub/inner.txt", Path: "docs/inner.txt", View: file}

	for _, tc := range []struct {
		name     string
		actions  []Action
		wantDeps [][]int
	}{
		{name: "no create at the vacated path", actions: []Action{fileMove, folderMove}, wantDeps: [][]int{nil, {0}}},
		{name: "a create on the other side", actions: []Action{
			fileMove, folderMove, {Type: ActionFolderCreate, Path: "docs", CreateSide: createLocal},
		}, wantDeps: [][]int{{2}, {0}, nil}},
		{name: "a move into the folder made there", actions: []Action{fileMove, folderMove, recreate}, wantDeps: [][]int{{2}, nil, {1}}},
		{name: "the same on this computer", actions: []Action{
			{Type: ActionLocalMove, OldPath: "docs/a.txt", Path: "docs/b.txt", View: file},
			{Type: ActionLocalMove, OldPath: "docs", Path: "photos/photos", View: folder},
			{Type: ActionFolderCreate, Path: "docs", CreateSide: createLocal},
		}, wantDeps: [][]int{{2}, nil, {1}}},
		{name: "a move into a folder made inside the new one", actions: []Action{
			{Type: ActionRemoteMove, OldPath: "docs/a.txt", Path: "docs/new/a.txt", View: file},
			folderMove,
			recreate,
			{Type: ActionFolderCreate, Path: "docs/new", CreateSide: CreateRemote},
		}, wantDeps: [][]int{{3}, nil, {1}, {2}}},
		{name: "a sibling that moves elsewhere", actions: []Action{
			fileMove,
			folderMove,
			{Type: ActionRemoteMove, OldPath: "docs/c.txt", Path: "c.txt", View: file},
			recreate,
		}, wantDeps: [][]int{{3}, {2}, nil, {1}}},
		{name: "a delete under the source", actions: []Action{
			{Type: ActionRemoteDelete, Path: "docs/old.txt", View: file},
			folderMove,
			recreate,
		}, wantDeps: [][]int{nil, {0}, {1}}},
		{name: "a file moved out of a renamed subfolder", actions: []Action{
			innerMove,
			folderMove,
			{Type: ActionRemoteMove, OldPath: "photos/photos/sub", Path: "photos/photos/sub2", View: folder},
			recreate,
		}, wantDeps: [][]int{{3}, nil, {1}, {1}}},
		{name: "a file moved out of a subfolder made here again", actions: []Action{
			{Type: ActionFolderCreate, Path: "archive/sub", CreateSide: createLocal},
			{Type: ActionDownload, Path: "archive/sub/new.txt", View: &pathView{Path: "archive/sub/new.txt"}},
			{Type: ActionRemoteMove, OldPath: "docs", Path: "archive", View: folder},
			innerMove,
			recreate,
		}, wantDeps: [][]int{{2}, {0, 2}, nil, {4}, {2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantDeps, buildDependencies(tc.actions))
		})
	}
}

// Validates: R-2.17.6
//
// The plans of TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges,
// onto its folder's path: OneDrive moved docs to archive and docs/a.txt to
// docs, while a.txt was moved here into a new folder docs/new. Moved on this
// computer in the pass that moves docs, a.txt would wait for docs to leave
// its path, and docs for a.txt to leave it first: a cycle the planner drops.
// Composition holds a.txt for the next pass instead, so the first plan moves
// docs, which takes docs/new along, and then makes that folder on OneDrive at
// archive/new, and the second moves a.txt onto the path docs left.
func TestBuildDependencies_FileMovedOntoAVacatedPathFromAFolderMadeInsideIt(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	folderMove := Action{Type: ActionLocalMove, OldPath: "docs", Path: "archive", View: folder}

	assertDependencyRows(t, []dependencyRow{
		{name: "a.txt moved in the pass that moves docs", actions: []Action{
			{Type: ActionLocalMove, OldPath: "docs/new/a.txt", Path: "docs", View: file},
			folderMove,
		}, wantDeps: [][]int{{1}, {0}}},
		{name: "first pass", actions: []Action{
			folderMove,
			{Type: ActionFolderCreate, Path: "archive/new", CreateSide: CreateRemote},
		}, wantDeps: [][]int{nil, {0}}, wantKept: 2},
		{name: "second pass", actions: []Action{
			{Type: ActionLocalMove, OldPath: "archive/new/a.txt", Path: "docs", View: file},
		}, wantDeps: [][]int{nil}, wantKept: 1},
	})
}

// dependencyRow is a plan, the dependency lists buildDependencies gives it,
// and how many of its actions isolateDependencyCycles keeps.
type dependencyRow struct {
	name     string
	actions  []Action
	wantDeps [][]int
	wantKept int
}

func assertDependencyRows(t *testing.T, rows []dependencyRow) {
	t.Helper()

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.wantDeps, buildDependencies(tc.actions))
			kept, _, _ := isolateDependencyCycles(tc.actions, nil)
			assert.Len(t, kept, tc.wantKept)
		})
	}
}

// Validates: R-2.17.6
//
// The plans of
// TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges,
// renamed: x moved to x2, a new x made, f moved into it, a new f made, and
// x/f/b.txt moved into the new f as d.txt, on this computer. Moved on
// OneDrive in the pass that moves x and f, from x/f/b.txt, where f's move
// puts it, b.txt's move lies below x's source, so x's move waits for it
// (children leave first), while it waits for f's move, and f's move for x's
// move, directly (it lands below the path x's move vacates) and through the
// new x, which waits for x's move: a cycle the planner drops, with the new f,
// on every pass. Composition holds b.txt instead (rebasedSourceMisread): the
// first plan moves both folders and makes both new ones, the second moves
// b.txt.
func TestBuildDependencies_FileRebasedBelowAnotherFoldersVacatedPath(t *testing.T) {
	t.Parallel()

	folder := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFolder}}
	moveX := Action{Type: ActionRemoteMove, OldPath: "x", Path: "x2", View: folder}
	moveF := Action{Type: ActionRemoteMove, OldPath: "f", Path: "x/f", View: folder}
	createX := Action{Type: ActionFolderCreate, Path: "x", CreateSide: CreateRemote}
	createF := Action{Type: ActionFolderCreate, Path: "f", CreateSide: CreateRemote}
	moveB := Action{
		Type: ActionRemoteMove, OldPath: "x/f/b.txt", Path: "f/d.txt",
		View: &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}},
	}

	assertDependencyRows(t, []dependencyRow{
		{
			name:     "b.txt moved in the pass that moves x and f",
			actions:  []Action{moveX, moveF, moveB, createX, createF},
			wantDeps: [][]int{{2}, {0, 3}, {1, 4}, {0}, {1}},
		},
		{
			name:     "first pass",
			actions:  []Action{moveX, moveF, createX, createF},
			wantDeps: [][]int{nil, {0, 2}, {0}, {1}}, wantKept: 4,
		},
		{name: "second pass", actions: []Action{moveB}, wantDeps: [][]int{nil}, wantKept: 1},
	})
}

// Validates: R-2.17.6
//
// A dependency cycle drops only the actions on it and their dependents
// (D-059c): a cycle never stops the rest of the plan.
func TestIsolateDependencyCycles_KeepsUnrelatedWork(t *testing.T) {
	t.Parallel()

	file := &pathView{Baseline: &BaselineEntry{ItemType: ItemTypeFile}}
	actions := []Action{
		// Two local moves that vacate each other's destination: a cycle.
		{Type: ActionLocalMove, OldPath: "a.txt", Path: "b.txt", View: file},
		{Type: ActionLocalMove, OldPath: "b.txt", Path: "a.txt", View: file},
		{Type: ActionUpload, Path: "unrelated.txt", View: &pathView{Path: "unrelated.txt"}},
	}
	require.Error(t, detectDependencyCycle(buildDependencies(actions)), "the fixture must contain a cycle")

	kept, deps, _ := isolateDependencyCycles(actions, nil)

	require.Len(t, kept, 1)
	assert.Equal(t, "unrelated.txt", kept[0].Path)
	require.NoError(t, detectDependencyCycle(deps))
}
