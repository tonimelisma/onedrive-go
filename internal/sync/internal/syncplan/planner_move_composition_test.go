package syncplan

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
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func composeNode(key string, itemType syncdomain.ItemType, baseline, local, remote string) moveNode {
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
		composeNode("a", syncdomain.ItemTypeFile, "a.txt", "a.txt", "b.txt"),
		composeNode("b", syncdomain.ItemTypeFile, "b.txt", "b.txt", "c.txt"),
		composeNode("c", syncdomain.ItemTypeFile, "c.txt", "c.txt", "a.txt"),
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
		composeNode("p", syncdomain.ItemTypeFile, "d/50%.txt", "d/50%.Txt", "d/a+b.txt"),
		composeNode("q", syncdomain.ItemTypeFile, "d/a+b.txt", "d/a+b.txt", "d/50%.txt"),
	)

	held := map[namekey.PathKey]string{}
	for _, n := range []moveNode{
		composeNode("p", syncdomain.ItemTypeFile, "", "d/50%.Txt", ""),
		composeNode("q", syncdomain.ItemTypeFile, "", "d/a+b.txt", ""),
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
		composeNode("a", syncdomain.ItemTypeFile, a, a, b),
		composeNode("b", syncdomain.ItemTypeFile, b, b, a),
	)

	require.NotEmpty(t, c.moves)
	for _, move := range c.moves {
		assert.LessOrEqual(t, len(move.newPath), 255, move.newPath)
		assert.True(t, utf8.ValidString(move.newPath), move.newPath)
	}
	assert.Contains(t, c.moves[0].newPath, MoveSwapInfix)
}

// Validates: R-2.17.6
func TestComposeMoves_OneSidedRenamesMoveTheOtherSide(t *testing.T) {
	t.Parallel()

	c := composeForTest(
		composeNode("a", syncdomain.ItemTypeFile, "a.txt", "b.txt", "a.txt"),
		composeNode("c", syncdomain.ItemTypeFile, "c.txt", "c.txt", "d.txt"),
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
		composeNode("dir", syncdomain.ItemTypeFolder, "dir", "renamed", "renamed"),
		composeNode("child", syncdomain.ItemTypeFile, "dir/child.txt", "renamed/child.txt", "renamed/child.txt"),
	)

	assert.Empty(t, c.moves)
	require.Len(t, c.rekeys, 1, "the child follows its folder; only the folder's own row is re-keyed")
	assert.Equal(t, composedRekey{key: "dir", oldPath: "dir", newPath: "renamed"}, c.rekeys[0])
}

// Validates: R-2.17.6
func TestComposeMoves_DivergentRenameKeepsTheOneDriveName(t *testing.T) {
	t.Parallel()

	c := composeForTest(composeNode("a", syncdomain.ItemTypeFile, "a", "b", "c"))

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
		composeNode("A", syncdomain.ItemTypeFolder, "A", "B/A", "A"),
		composeNode("B", syncdomain.ItemTypeFolder, "B", "B", "A/B"),
		composeNode("a-file", syncdomain.ItemTypeFile, "A/f.txt", "B/A/f.txt", "A/f.txt"),
		composeNode("b-file", syncdomain.ItemTypeFile, "B/g.txt", "B/g.txt", "A/B/g.txt"),
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
		composeNode("p", syncdomain.ItemTypeFolder, "Projects", "Archive", "Projects"),
		composeNode("kept", syncdomain.ItemTypeFile, "Projects/kept.txt", "Archive/kept.txt", "Projects/kept.txt"),
		composeNode("old", syncdomain.ItemTypeFile, "Projects/old.txt", "Archive/new.txt", "Projects/old.txt"),
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
		composeNode("A", syncdomain.ItemTypeFolder, "A", "A", "B/A"),
		composeNode("B", syncdomain.ItemTypeFolder, "A/B", "A/B", "B"),
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
		composeNode("y", syncdomain.ItemTypeFolder, "Y", "Y", "Y2"),
		composeNode("f", syncdomain.ItemTypeFile, "f.txt", "Y/f.txt", "f.txt"),
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

	nodes := []moveNode{composeNode("a", syncdomain.ItemTypeFile, "a", "a", "b")}
	occupancy := composeOccupancy(nodes)
	occupancy.local["b"] = true

	c := composeMoves(nodes, occupancy, nil)

	assert.Empty(t, c.moves)
	assert.True(t, c.deferred["a"])
}

// Validates: R-2.17.9
//
// How the entry at the local path of a synced file gives way to a move onto
// it, one row per kind of entry. Only a file OneDrive no longer has gives
// way. Its own copy is replaced when it is provably the synced version and
// kept as a conflict copy when it was edited; a new file that took the path
// (the synced copy left it: a replacing move took it into the trash and
// another program saved a file there before the rename) is kept as a
// conflict copy too, so the move still lands, and so is a new file at the
// path of a synced copy moved elsewhere here, whatever OneDrive holds: it is
// not the item, and waiting for it left two moves waiting on each other's
// destinations for good (chaos seeds 162 and 385). Nothing else gives way.
func TestComposedDisplacement_EntryAtTheDestination(t *testing.T) {
	t.Parallel()

	const syncedHash = "synced-hash"
	baseline := &syncdomain.BaselineEntry{ItemID: "b", ItemType: syncdomain.ItemTypeFile, Path: "b.txt", LocalHash: syncedHash}
	file := func(hash string) *syncdomain.LocalState {
		return &syncdomain.LocalState{Name: "b.txt", ItemType: syncdomain.ItemTypeFile, Hash: hash}
	}
	stillOnline := map[string]*syncdomain.RemoteStateRow{"b": {ItemID: "b", Path: "elsewhere.txt"}}

	tests := []struct {
		name string
		// localPosition is where composition holds the synced item here: its
		// baseline path, another path it moved to, or "" when the entry at
		// the baseline path is not the item.
		localPosition string
		local         *syncdomain.LocalState
		remoteByID    map[string]*syncdomain.RemoteStateRow
		want          moveDisplacement
	}{
		{name: "the synced copy unchanged is replaced", localPosition: "b.txt", local: file(syncedHash), want: moveDisplaceReplace},
		{name: "the synced copy edited is kept as a conflict copy", localPosition: "b.txt", local: file("edited-hash"), want: moveDisplaceConflictCopy},
		{name: "a new file that took the path is kept as a conflict copy", local: file("new-hash"), want: moveDisplaceConflictCopy},
		{name: "a new file of unknown content that took the path is kept as a conflict copy", local: file(""), want: moveDisplaceConflictCopy},
		{
			name:  "a folder that took the path makes the move wait",
			local: &syncdomain.LocalState{Name: "b.txt", ItemType: syncdomain.ItemTypeFolder}, want: moveDisplaceNever,
		},
		{name: "an empty path needs no displacement", want: moveDisplaceNever},
		{name: "the synced copy of unknown content makes the move wait", localPosition: "b.txt", local: file(""), want: moveDisplaceNever},
		{name: "a new file at the path of a synced copy moved elsewhere here is kept as a conflict copy", localPosition: "c.txt", local: file("new-hash"), want: moveDisplaceConflictCopy},
		{
			name:          "a new file at the path of a synced copy moved elsewhere here is kept as a conflict copy while the item is online",
			localPosition: "c.txt", local: file("new-hash"), remoteByID: stillOnline, want: moveDisplaceConflictCopy,
		},
		{
			name: "a folder at the path of a synced copy moved elsewhere here makes the move wait", localPosition: "c.txt",
			local: &syncdomain.LocalState{Name: "b.txt", ItemType: syncdomain.ItemTypeFolder}, want: moveDisplaceNever,
		},
		{name: "a new file at the path of an item still online makes the move wait", local: file("new-hash"), remoteByID: stillOnline, want: moveDisplaceNever},
		{name: "the synced copy of an item still online makes the move wait", localPosition: "b.txt", local: file(syncedHash), remoteByID: stillOnline, want: moveDisplaceNever},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			node := composeNode("b", syncdomain.ItemTypeFile, "b.txt", tt.localPosition, "")
			view := &syncdomain.PathView{Path: "b.txt", Local: tt.local, Baseline: baseline}

			assert.Equal(t, tt.want, composedDisplacement(&node, baseline, view, tt.remoteByID))
		})
	}
}

// Validates: R-2.17.9
//
// OneDrive moved a.txt onto b.txt, replacing b, and the file at b.txt here
// is a new one saved after b's copy left: no composed item holds the path.
// The move of a.txt still lands on it, with that file renamed to a conflict
// copy first, instead of waiting while the moved item is downloaded beside
// its old copy. An entry the deleted item does not answer for (no baseline
// row at the path, or a displacement that never gives way) keeps the move
// waiting.
func TestComposeMoves_MoveOntoANewFileAtTheFreedNameKeepsItAsAConflictCopy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		displaced   moveDisplacement
		withDeleted bool
		wantMoves   []string
	}{
		{name: "a new file at the freed name", displaced: moveDisplaceConflictCopy, withDeleted: true, wantMoves: []string{"a.txt->b.txt"}},
		{name: "an entry that never gives way", displaced: moveDisplaceNever, withDeleted: true},
		{name: "an entry at a path no synced item had", withDeleted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			nodes := []moveNode{composeNode("a", syncdomain.ItemTypeFile, "a.txt", "a.txt", "b.txt")}
			if tt.withDeleted {
				deleted := composeNode("b", syncdomain.ItemTypeFile, "b.txt", "", "")
				deleted.displace = tt.displaced
				nodes = append(nodes, deleted)
			}
			occupancy := composeOccupancy(nodes)
			occupancy.local["b.txt"] = true

			c := composeMoves(nodes, occupancy, nil)

			assert.Equal(t, tt.wantMoves, composedMovesBySide(&c, moveCoordLocal))
			if len(tt.wantMoves) == 0 {
				assert.True(t, c.deferred["a"], "the move waits")
				return
			}
			require.Len(t, c.moves, 1)
			assert.Equal(t, moveDisplaceConflictCopy, c.moves[0].displaced, "the new file is kept as a conflict copy")
			assert.False(t, c.deferred["a"])
		})
	}
}

// Validates: R-2.17.6
func TestComposeMoves_FrozenItemsAndTheirSubtreesDoNotMove(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("dir", syncdomain.ItemTypeFolder, "dir", "renamed", "dir"),
		composeNode("child", syncdomain.ItemTypeFile, "dir/c.txt", "renamed/c2.txt", "dir/c.txt"),
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
func recreatedFolderNodes(side moveCoord, entryType syncdomain.ItemType, name, entry string) ([]moveNode, moveOccupancy) {
	at := func(key string, itemType syncdomain.ItemType, synced, moved string) moveNode {
		if side == moveCoordLocal {
			return composeNode(key, itemType, synced, moved, synced)
		}
		return composeNode(key, itemType, synced, synced, moved)
	}
	nodes := []moveNode{
		at("docs", syncdomain.ItemTypeFolder, "docs", "photos/photos"),
		at("entry", entryType, "docs/"+name, entry),
		at("kept", syncdomain.ItemTypeFile, "docs/kept.txt", "photos/photos/kept.txt"),
		at("photos", syncdomain.ItemTypeFolder, "photos", "photos"),
		at("photo", syncdomain.ItemTypeFile, "photos/photo.jpg", "photos/photo.jpg"),
	}
	if entryType == syncdomain.ItemTypeFolder {
		nodes = append(nodes, at("inner", syncdomain.ItemTypeFile, "docs/"+name+"/inner.txt", entry+"/inner.txt"))
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
		entryType    syncdomain.ItemType
		entry        string
		wantRemote   []string
		wantLocal    []string
		wantDeferred map[string]bool
	}{
		{
			name: "a file put back, moved here", side: moveCoordLocal, entryType: syncdomain.ItemTypeFile, entry: "docs/50%.txt",
			wantRemote: []string{"docs->photos/photos", "photos/photos/50%.txt->docs/50%.txt"}, wantDeferred: map[string]bool{},
		},
		{
			name: "a file renamed, moved here", side: moveCoordLocal, entryType: syncdomain.ItemTypeFile, entry: "docs/renamed-50%.txt",
			wantRemote:   []string{"docs->photos/photos", "photos/photos/50%.txt->docs/renamed-50%.txt"},
			wantDeferred: map[string]bool{},
		},
		{
			name: "a folder put back, moved here", side: moveCoordLocal, entryType: syncdomain.ItemTypeFolder, entry: "docs/sub",
			wantRemote: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
		{
			name: "a file put back, moved on OneDrive", side: moveCoordRemote, entryType: syncdomain.ItemTypeFile, entry: "docs/50%.txt",
			wantLocal: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
		{
			name: "a file renamed, moved on OneDrive", side: moveCoordRemote, entryType: syncdomain.ItemTypeFile, entry: "docs/renamed-50%.txt",
			wantLocal: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
		{
			name: "a folder put back, moved on OneDrive", side: moveCoordRemote, entryType: syncdomain.ItemTypeFolder, entry: "docs/sub",
			wantLocal: []string{"docs->photos/photos"}, wantDeferred: map[string]bool{"entry": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synced := "50%.txt"
			if tc.entryType == syncdomain.ItemTypeFolder {
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
		composeNode("docs", syncdomain.ItemTypeFolder, "docs", "docs", "archive"),
		composeNode("kept", syncdomain.ItemTypeFile, "docs/kept.txt", "docs/kept.txt", "archive/kept.txt"),
		composeNode("sub", syncdomain.ItemTypeFolder, "docs/sub", "docs/sub", "docs"),
		composeNode("inner", syncdomain.ItemTypeFile, "docs/sub/inner.txt", "docs/sub/inner.txt", "docs/inner.txt"),
	)

	assert.Equal(t, []string{"docs->archive"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
	assert.Equal(t, map[string]bool{"sub": true}, c.deferred)
}

// composedMoveActions turns c's moves into a plan's move actions, in c's
// order, each with a baseline of its item's type.
func composedMoveActions(c *moveComposition, nodes []moveNode) []syncdomain.Action {
	itemTypes := make(map[string]syncdomain.ItemType, len(nodes))
	for i := range nodes {
		itemTypes[nodes[i].key] = nodes[i].itemType
	}
	actions := make([]syncdomain.Action, 0, len(c.moves))
	for i := range c.moves {
		moveType := syncdomain.ActionRemoteMove
		if c.moves[i].side == moveCoordLocal {
			moveType = syncdomain.ActionLocalMove
		}
		actions = append(actions, syncdomain.Action{
			Type: moveType, OldPath: c.moves[i].oldPath, Path: c.moves[i].newPath, MovedRemotePath: c.moves[i].remotePath,
			View: &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: itemTypes[c.moves[i].key]}},
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
// cycle, which the delete breaks by waiting a pass
// (TestWithholdDeletesInsideFolderMoveCycles_HoldsTheDeleteThatClosesTheCycle).
// When OneDrive added a file
// to docs/sub, SQLite makes archive/sub here again instead, and the plan has
// no delete: every action of it has an order
// (TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges).
func TestComposeMoves_EntryBelowAFolderOnOneSideOnlyKeepsItsSource(t *testing.T) {
	t.Parallel()

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	for _, tc := range []struct {
		name string
		// subWork is the plan's work on sub after the two moves and the docs
		// made online.
		subWork  []syncdomain.Action
		wantKept int
	}{
		{name: "deleted online", subWork: []syncdomain.Action{{Type: syncdomain.ActionRemoteDelete, Path: "docs/sub", View: folder}}},
		{name: "made here again", subWork: []syncdomain.Action{
			{Type: syncdomain.ActionFolderCreate, Path: "archive/sub", CreateSide: syncdomain.CreateLocal},
			{Type: syncdomain.ActionDownload, Path: "archive/sub/new.txt", View: &syncdomain.PathView{Path: "archive/sub/new.txt"}},
		}, wantKept: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			nodes := []moveNode{
				composeNode("docs", syncdomain.ItemTypeFolder, "docs", "archive", "docs"),
				composeNode("kept", syncdomain.ItemTypeFile, "docs/kept.txt", "archive/kept.txt", "docs/kept.txt"),
				composeNode("sub", syncdomain.ItemTypeFolder, "docs/sub", "", "docs/sub"),
				composeNode("inner", syncdomain.ItemTypeFile, "docs/sub/inner.txt", "docs/inner.txt", "docs/sub/inner.txt"),
			}
			occupancy := composeOccupancy(nodes)
			occupancy.local["docs"] = true
			c := composeMoves(nodes, occupancy, nil)

			assert.Equal(t, []string{"docs->archive", "docs/sub/inner.txt->docs/inner.txt"},
				composedMovesBySide(&c, moveCoordRemote))
			assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))

			actions := append(composedMoveActions(&c, nodes), syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateRemote})
			actions = append(actions, tc.subWork...)
			if tc.subWork[0].Type == syncdomain.ActionRemoteDelete {
				assert.Contains(t, buildDependencies(actions)[3], 1, "the delete of docs/sub waits for inner.txt to leave it")
			}
			kept, _, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, tc.wantKept)
		})
	}
}

// Validates: R-2.17.6, R-2.17.1
//
// A folder moved into its own former child: docs moved to tmp, a new docs
// made, tmp/sub moved into it and tmp moved to docs/sub/docs2, here or on
// OneDrive. sub's move lands below the path docs's move vacates, so it is
// decided after docs, while docs's target hangs off sub: both stayed pending
// and were deferred on every pass. docs moves aside to a free name beside its
// path on the other side, carrying sub, and both wait for the next pass,
// which makes the new docs at the path docs left and moves sub into it and
// docs below sub. Every action of the plan has an order.
func TestComposeMoves_FolderMovedIntoItsOwnFormerChildMovesAsideFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                  string
		onOneDrive            bool
		wantLocal, wantRemote []string
	}{
		{name: "here", wantRemote: []string{"docs->docs.swap-1"}},
		{name: "on OneDrive", onOneDrive: true, wantLocal: []string{"docs->docs.swap-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			moved := func(baseline, to string) [moveCoordCount]string {
				if tc.onOneDrive {
					return [moveCoordCount]string{baseline, baseline, to}
				}
				return [moveCoordCount]string{baseline, to, baseline}
			}
			node := func(key string, itemType syncdomain.ItemType, paths [moveCoordCount]string) moveNode {
				return composeNode(key, itemType, paths[moveCoordBaseline], paths[moveCoordLocal], paths[moveCoordRemote])
			}
			nodes := []moveNode{
				node("docs", syncdomain.ItemTypeFolder, moved("docs", "docs/sub/docs2")),
				node("kept", syncdomain.ItemTypeFile, moved("docs/kept.txt", "docs/sub/docs2/kept.txt")),
				node("sub", syncdomain.ItemTypeFolder, moved("docs/sub", "docs/sub")),
				node("inner", syncdomain.ItemTypeFile, moved("docs/sub/inner.txt", "docs/sub/inner.txt")),
			}
			occupancy := composeOccupancy(nodes)
			if tc.onOneDrive {
				occupancy.remote["docs"] = true
			} else {
				occupancy.local["docs"] = true
			}
			c := composeMoves(nodes, occupancy, nil)

			assert.Equal(t, tc.wantLocal, composedMovesBySide(&c, moveCoordLocal))
			assert.Equal(t, tc.wantRemote, composedMovesBySide(&c, moveCoordRemote))
			assert.Equal(t, map[string]bool{"docs": true, "sub": true}, c.deferred)

			actions := composedMoveActions(&c, nodes)
			kept, _, _ := isolateDependencyCycles(actions, nil)
			assert.Len(t, kept, len(actions), "every action of the plan has an order")
		})
	}
}

// Validates: R-2.17.2, R-2.17.6
//
// The plan of the row "deleted online" above, a cycle of docs's move, the
// delete of docs/sub, inner.txt's move and the new docs, holds the delete for
// the next pass, and with it nothing else: docs's move carries sub with
// inner.txt along, and every action left has an order. A plan whose delete
// closes no such cycle (inner.txt moved out of a sub that no moving folder
// holds) keeps it.
func TestWithholdDeletesInsideFolderMoveCycles_HoldsTheDeleteThatClosesTheCycle(t *testing.T) {
	t.Parallel()

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	nodes := []moveNode{
		composeNode("docs", syncdomain.ItemTypeFolder, "docs", "archive", "docs"),
		composeNode("kept", syncdomain.ItemTypeFile, "docs/kept.txt", "archive/kept.txt", "docs/kept.txt"),
		composeNode("sub", syncdomain.ItemTypeFolder, "docs/sub", "", "docs/sub"),
		composeNode("inner", syncdomain.ItemTypeFile, "docs/sub/inner.txt", "docs/inner.txt", "docs/sub/inner.txt"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.local["docs"] = true
	c := composeMoves(nodes, occupancy, nil)
	actions := append(composedMoveActions(&c, nodes),
		syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateRemote},
		syncdomain.Action{Type: syncdomain.ActionRemoteDelete, Path: "docs/sub", View: folder})

	kept, withheld := withholdDeletesInsideFolderMoveCycles(nil, actions, syncdomain.SyncBidirectional)

	require.Len(t, withheld, 1)
	assert.Equal(t, syncdomain.ActionRemoteDelete, withheld[0].Type)
	assert.Equal(t, "docs/sub", withheld[0].Path)
	assert.Len(t, kept, len(actions)-1, "docs's move, inner.txt's move and the new docs stay")
	_, _, cycled := isolateDependencyCycles(kept, nil)
	assert.Empty(t, cycled, "every action left has an order")

	unmoved := []syncdomain.Action{
		{
			Type: syncdomain.ActionRemoteMove, Path: "inner.txt", OldPath: "sub/inner.txt", ItemID: "inner",
			View: &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}},
		},
		{Type: syncdomain.ActionRemoteDelete, Path: "sub", View: folder},
	}
	kept, withheld = withholdDeletesInsideFolderMoveCycles(nil, unmoved, syncdomain.SyncBidirectional)
	assert.Empty(t, withheld)
	assert.Len(t, kept, len(unmoved))
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
		composeNode("docs", syncdomain.ItemTypeFolder, "docs", "photos/photos", "docs"),
		composeNode("sub", syncdomain.ItemTypeFolder, "docs/sub", "photos/photos/sub2", "docs/sub"),
		composeNode("inner", syncdomain.ItemTypeFile, "docs/sub/inner.txt", "docs/inner.txt", "docs/sub/inner.txt"),
		composeNode("kept", syncdomain.ItemTypeFile, "docs/kept.txt", "photos/photos/kept.txt", "docs/kept.txt"),
		composeNode("photos", syncdomain.ItemTypeFolder, "photos", "photos", "photos"),
		composeNode("photo", syncdomain.ItemTypeFile, "photos/photo.jpg", "photos/photo.jpg", "photos/photo.jpg"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.local["docs"] = true
	c := composeMoves(nodes, occupancy, nil)

	assert.Equal(t, []string{
		"docs/sub/inner.txt->docs/inner.txt", "docs->photos/photos", "photos/photos/sub->photos/photos/sub2",
	}, composedMovesBySide(&c, moveCoordRemote))
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, c.deferred)

	actions := append(composedMoveActions(&c, nodes), syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateRemote})
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
				composeNode("docs", syncdomain.ItemTypeFolder, "docs", "docs", tc.movedTo),
				composeNode("kept", syncdomain.ItemTypeFile, "docs/kept.txt", "docs/kept.txt", path.Join(tc.movedTo, "kept.txt")),
				composeNode("photos", syncdomain.ItemTypeFolder, "photos", "photos", "photos"),
				composeNode("entry", syncdomain.ItemTypeFile, "docs/a.txt", "docs/new/a.txt", tc.target),
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
				composeNode("x", syncdomain.ItemTypeFolder, "x", "x2", "x"),
				composeNode("keep", syncdomain.ItemTypeFile, "x/keep.txt", "x2/keep.txt", "x/keep.txt"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "x/f", "f"),
				composeNode("b", syncdomain.ItemTypeFile, "f/b.txt", tc.back, "f/b.txt"),
				composeNode("c", syncdomain.ItemTypeFile, "f/c.txt", "x/f/c.txt", "f/c.txt"),
			}
			newFolders := []syncdomain.Action{{Type: syncdomain.ActionFolderCreate, Path: "f", CreateSide: syncdomain.CreateRemote}}
			if tc.onto {
				nodes = append(nodes,
					composeNode("g", syncdomain.ItemTypeFolder, "g", "x", "g"),
					composeNode("g-file", syncdomain.ItemTypeFile, "g/g.txt", "x/g.txt", "g/g.txt"))
			} else {
				newFolders = append(newFolders, syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "x", CreateSide: syncdomain.CreateRemote})
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
		composeNode("zz", syncdomain.ItemTypeFolder, "zz", "zz", "zz"),
		composeNode("c", syncdomain.ItemTypeFolder, "c", "zz/c2", "c"),
		composeNode("keep", syncdomain.ItemTypeFile, "c/keep.txt", "zz/c2/keep.txt", "c/keep.txt"),
		composeNode("p", syncdomain.ItemTypeFolder, "p", "c/p", "p"),
		composeNode("b", syncdomain.ItemTypeFile, "p/b.txt", "p/d.txt", "p/b.txt"),
		composeNode("e", syncdomain.ItemTypeFile, "p/e.txt", "c/p/e.txt", "p/e.txt"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.local["c"] = true
	occupancy.local["p"] = true
	c := composeMoves(nodes, occupancy, nil)

	assert.ElementsMatch(t, []string{"c->zz/c2", "p->c/p"}, composedMovesBySide(&c, moveCoordRemote))
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
	assert.Equal(t, map[string]bool{"b": true}, c.deferred)

	actions := append(composedMoveActions(&c, nodes),
		syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "c", CreateSide: syncdomain.CreateRemote},
		syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "p", CreateSide: syncdomain.CreateRemote})
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
			composeNode("x", syncdomain.ItemTypeFolder, "x", "x", x2),
			composeNode("keep", syncdomain.ItemTypeFile, "x/keep.txt", "x/keep.txt", x2+"/keep.txt"),
			composeNode("f", syncdomain.ItemTypeFolder, "f", "f", into+"/f"),
			composeNode("c", syncdomain.ItemTypeFile, "f/c.txt", "f/c.txt", into+"/f/c.txt"),
			composeNode("b", syncdomain.ItemTypeFile, "f/b.txt", "f/b.txt", back),
		}
	}
	deeper := append(moved("x", "zz/a/x2", "f/d.txt"),
		composeNode("zz", syncdomain.ItemTypeFolder, "zz", "zz", "zz"),
		composeNode("a", syncdomain.ItemTypeFolder, "zz/a", "zz/a", "zz/a"))
	fileX := append([]moveNode{composeNode("x", syncdomain.ItemTypeFile, "x", "x", "zz/a/x2")}, deeper[2:]...)
	nested := []moveNode{
		composeNode("P", syncdomain.ItemTypeFolder, "P", "P", "P2"),
		composeNode("A", syncdomain.ItemTypeFolder, "P/A", "P/A", "P2/A"),
		composeNode("new", syncdomain.ItemTypeFolder, "P/A/new", "P/A/new", "P2/A/new2"),
		composeNode("c", syncdomain.ItemTypeFile, "P/A/c", "P/A/c", "P2/A/new/c"),
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
func landingNodes(x2, into string, xType syncdomain.ItemType) []moveNode {
	return []moveNode{
		composeNode("x", xType, "x", x2, "x"),
		composeNode("f", syncdomain.ItemTypeFolder, "f", into+"/f", "f"),
		composeNode("c", syncdomain.ItemTypeFile, "f/c.txt", into+"/f/c.txt", "f/c.txt"),
		composeNode("zz", syncdomain.ItemTypeFolder, "zz", "zz", "zz"),
		composeNode("a", syncdomain.ItemTypeFolder, "zz/a", "zz/a", "zz/a"),
	}
}

// keptOrMovedLandingCases are the rows where x stays where OneDrive has it,
// moves away there, or is a file or frozen.
func keptOrMovedLandingCases() []heldRemoteLandingCase {
	folder, chain := syncdomain.ItemTypeFolder, []string{"x", "x/sub"}
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
			name: "a file x moved deeper", nodes: landingNodes("zz/a/x2", "x", syncdomain.ItemTypeFile), made: []string{"x"},
			wantRemote: []string{"x->zz/a/x2"}, wantDeferred: map[string]bool{"f": true}, creates: []string{"x"},
		},
		{
			name: "a frozen x moved deeper", nodes: landingNodes("zz/a/x2", "x", folder), made: []string{"x"},
			frozen: map[string]bool{"x": true}, wantDeferred: bothWait,
		},
		{
			name: "a new X, x kept where OneDrive has it", made: []string{"X"}, onOneDrive: "X2",
			nodes: []moveNode{
				composeNode("x", syncdomain.ItemTypeFolder, "x", "x2", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "X/f", "f"),
			},
			wantDeferred: bothWait,
		},
		{
			name: "a new X, x moved away", made: []string{"X"},
			nodes: []moveNode{
				composeNode("x", syncdomain.ItemTypeFolder, "x", "x2", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "X/f", "f"),
			},
			wantRemote: []string{"x->x2", "f->X/f"}, creates: []string{"X"},
		},
		{
			name: "f's destination held by an item that swaps with f, x kept where OneDrive has it",
			made: []string{"x"}, onOneDrive: "X2",
			nodes: []moveNode{
				composeNode("x", syncdomain.ItemTypeFolder, "x", "x2", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "x/f", "f"),
				composeNode("p", syncdomain.ItemTypeFolder, "x/f", "f", "x/f"),
			},
			wantDeferred: map[string]bool{"x": true, "f": true, "p": true},
		},
	}
}

// takenBackLandingCases are the rows where f's move is planned and then
// taken back, as x ends the composition without an OneDrive move.
func takenBackLandingCases() []heldRemoteLandingCase {
	folder, bothWait := syncdomain.ItemTypeFolder, map[string]bool{"x": true, "f": true}

	return []heldRemoteLandingCase{
		{
			name: "x held in a new b, decided first", made: []string{"b", "x"}, onOneDrive: "B2",
			nodes:        append(landingNodes("b/x", "x", folder), composeNode("b", syncdomain.ItemTypeFolder, "b", "b2", "b")),
			wantDeferred: map[string]bool{"b": true, "x": true, "f": true},
		},
		{
			name: "x held in a new y, decided after f", made: []string{"y", "x"}, onOneDrive: "Y2",
			nodes:        append(landingNodes("y/x", "x", folder), composeNode("y", syncdomain.ItemTypeFolder, "y", "y2", "y")),
			wantDeferred: map[string]bool{"y": true, "x": true, "f": true},
		},
		{
			name: "x waiting for a file deleted in a renamed a, deferred after f", made: []string{"x"},
			nodes: []moveNode{
				composeNode("a", syncdomain.ItemTypeFolder, "a", "b", "a"),
				composeNode("w", syncdomain.ItemTypeFile, "a/x", "", "a/x"),
				composeNode("x", syncdomain.ItemTypeFolder, "x", "b/x", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "x/f", "f"),
			},
			wantRemote: []string{"a->b"}, wantDeferred: bothWait,
		},
		{
			name: "m into a new b, b into a new c, c waiting for a file deleted in a renamed p", made: []string{"c", "b"},
			nodes: []moveNode{
				composeNode("p", syncdomain.ItemTypeFolder, "p", "q", "p"),
				composeNode("w", syncdomain.ItemTypeFile, "p/c", "", "p/c"),
				composeNode("c", syncdomain.ItemTypeFolder, "c", "q/c", "c"),
				composeNode("b", syncdomain.ItemTypeFolder, "b", "c/b", "b"),
				composeNode("m", syncdomain.ItemTypeFolder, "m", "b/m", "m"),
			},
			wantRemote: []string{"p->q"}, wantDeferred: map[string]bool{"c": true, "b": true, "m": true},
		},
		{
			name: "f's destination held by an item that swaps with f, x held in a new b",
			made: []string{"b", "x"}, onOneDrive: "B2",
			nodes: []moveNode{
				composeNode("b", syncdomain.ItemTypeFolder, "b", "b2", "b"),
				composeNode("x", syncdomain.ItemTypeFolder, "x", "b/x", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "x/f", "f"),
				composeNode("p", syncdomain.ItemTypeFolder, "x/f", "f", "x/f"),
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
				composeNode("x", syncdomain.ItemTypeFolder, "x", "f/x", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "x/f", "f"),
			},
			wantRemote: []string{"x->f/x"}, wantDeferred: map[string]bool{"f": true}, creates: []string{"x"},
		},
		{
			name: "each into a new folder at the other's path, and g into the new f", made: []string{"x", "f"},
			nodes: []moveNode{
				composeNode("x", syncdomain.ItemTypeFolder, "x", "f/x", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "x/f", "f"),
				composeNode("g", syncdomain.ItemTypeFolder, "g", "f/g", "g"),
			},
			wantRemote: []string{"x->f/x"}, wantDeferred: map[string]bool{"f": true, "g": true}, creates: []string{"x"},
		},
		{
			name: "a file a into a new f, f into a new a, f decided first", made: []string{"a", "f"},
			nodes: []moveNode{
				composeNode("a", syncdomain.ItemTypeFile, "a", "f/a", "a"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "a/f", "f"),
			},
			wantRemote: []string{"a->f/a"}, wantDeferred: map[string]bool{"f": true}, creates: []string{"a"},
		},
		{
			name: "x into a new f/sub, f into a new x", made: []string{"x", "f", "f/sub"},
			nodes: []moveNode{
				composeNode("x", syncdomain.ItemTypeFolder, "x", "f/sub/x", "x"),
				composeNode("f", syncdomain.ItemTypeFolder, "f", "x/f", "f"),
			},
			wantRemote: []string{"f->x/f"}, wantDeferred: map[string]bool{"x": true}, creates: []string{"f", "f/sub"},
		},
		{
			name: "a ring of three, and g into the new b", made: []string{"a", "b", "c"},
			nodes: []moveNode{
				composeNode("a", syncdomain.ItemTypeFolder, "a", "b/a", "a"),
				composeNode("b", syncdomain.ItemTypeFolder, "b", "c/b", "b"),
				composeNode("c", syncdomain.ItemTypeFolder, "c", "a/c", "c"),
				composeNode("g", syncdomain.ItemTypeFolder, "g", "b/g", "g"),
			},
			wantRemote: []string{"c->a/c", "a->b/a"}, wantDeferred: map[string]bool{"b": true, "g": true},
			creates: []string{"a", "c"},
		},
		{
			name: "a ring of four", made: []string{"a", "b", "c", "d"},
			nodes: []moveNode{
				composeNode("a", syncdomain.ItemTypeFolder, "a", "b/a", "a"),
				composeNode("b", syncdomain.ItemTypeFolder, "b", "c/b", "b"),
				composeNode("c", syncdomain.ItemTypeFolder, "c", "d/c", "c"),
				composeNode("d", syncdomain.ItemTypeFolder, "d", "a/d", "d"),
			},
			wantRemote: []string{"d->a/d", "a->b/a", "b->c/b"}, wantDeferred: map[string]bool{"c": true},
			creates: []string{"a", "b", "d"},
		},
		{
			name: "two files each into a new folder at the other's path", made: []string{"a", "b"},
			nodes: []moveNode{
				composeNode("a", syncdomain.ItemTypeFile, "a", "b/a", "a"),
				composeNode("b", syncdomain.ItemTypeFile, "b", "a/b", "b"),
			},
			wantDeferred: map[string]bool{"a": true, "b": true},
		},
		{
			name: "a ring through a file: b into a new a, a into a new g, g into a new b", made: []string{"a", "g", "b"},
			nodes: []moveNode{
				composeNode("b", syncdomain.ItemTypeFile, "b", "a/b", "b"),
				composeNode("a", syncdomain.ItemTypeFile, "a", "g/a", "a"),
				composeNode("g", syncdomain.ItemTypeFolder, "g", "b/g", "g"),
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
				actions = append(actions, syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: made, CreateSide: syncdomain.CreateRemote})
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
// names: nothing is made there this pass. A folder moving above the item on
// its side is looked through: renamed Y to Z on OneDrive, with Y/f moved
// there into a new Z/f/a/f, f waits for the pass after Y's move, and the
// path Y/f, where the creates of Z/f and Z/f/a are addressed here until Y
// has moved, is held.
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
			nodes:    []moveNode{composeNode("f", syncdomain.ItemTypeFile, "f", "f/f", "f")},
			madeHere: []string{"f"}, wantRemote: []string{"f->f.swap-1"}, wantDeferred: map[string]bool{"f": true},
			landing: "f",
		},
		{
			name: "a folder moved here into a new folder at its own path",
			nodes: []moveNode{
				composeNode("k", syncdomain.ItemTypeFolder, "k", "k/k", "k"),
				composeNode("c", syncdomain.ItemTypeFile, "k/c.txt", "k/k/c.txt", "k/c.txt"),
			},
			madeHere: []string{"k"}, wantRemote: []string{"k->k.swap-1"}, wantDeferred: map[string]bool{"k": true},
			landing: "k",
		},
		{
			name:           "a file moved on OneDrive into a new folder at its own path",
			nodes:          []moveNode{composeNode("f", syncdomain.ItemTypeFile, "f", "f", "f/f")},
			madeOnOneDrive: []string{"f"}, wantLocal: []string{"f->f.swap-1"}, wantDeferred: map[string]bool{"f": true},
			landing: "f",
		},
		{
			name: "a folder moved on OneDrive into a new folder at its own path",
			nodes: []moveNode{
				composeNode("k", syncdomain.ItemTypeFolder, "k", "k", "k/k"),
				composeNode("c", syncdomain.ItemTypeFile, "k/c.txt", "k/c.txt", "k/k/c.txt"),
			},
			madeOnOneDrive: []string{"k"}, wantLocal: []string{"k->k.swap-1"}, wantDeferred: map[string]bool{"k": true},
			landing: "k",
		},
		{
			name: "the folder above it renamed on OneDrive too",
			nodes: []moveNode{
				composeNode("P", syncdomain.ItemTypeFolder, "P", "Q", "P"),
				composeNode("f", syncdomain.ItemTypeFile, "P/f", "Q/f/f", "P/f"),
			},
			madeHere: []string{"Q/f"}, wantRemote: []string{"P->Q"}, wantDeferred: map[string]bool{"f": true},
			landing: "Q/f",
		},
		{
			name: "a folder above its unmoved parent renamed on OneDrive too",
			nodes: []moveNode{
				composeNode("Y", syncdomain.ItemTypeFolder, "Y", "Z", "Y"),
				composeNode("P", syncdomain.ItemTypeFolder, "Y/P", "Z/P", "Y/P"),
				composeNode("f", syncdomain.ItemTypeFile, "Y/P/f", "Z/P/f/f", "Y/P/f"),
			},
			madeHere: []string{"Z/P/f"}, wantRemote: []string{"Y->Z"}, wantDeferred: map[string]bool{"f": true},
			landing: "Z/P/f",
		},
		{
			name: "the folder above it renamed on OneDrive, the item moved there into a new folder deeper",
			nodes: []moveNode{
				composeNode("Y", syncdomain.ItemTypeFolder, "Y", "Y", "Z"),
				composeNode("f", syncdomain.ItemTypeFolder, "Y/f", "Y/f", "Z/f/a/f"),
				composeNode("c", syncdomain.ItemTypeFile, "Y/f/c.txt", "Y/f/c.txt", "Z/f/a/f/c.txt"),
			},
			madeOnOneDrive: []string{"Z/f", "Z/f/a"}, wantLocal: []string{"Y->Z"}, wantDeferred: map[string]bool{"f": true},
			landing: "Y/f",
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
// An earlier pass parked d/c.md at d/c.swap-1.md for a swap on OneDrive, and
// OneDrive has since moved d into e and the file at c.md into d. Here the
// parked file moves to c.md, out of d, before d moves; the file at c.md has
// its target inside d's destination, so it moves into d at d's path here,
// where d's move carries it. Ordered after d's move it waited for it, while
// d waited for the parked file to leave and the parked file for c.md: a cycle
// dropped on every pass (chaos seeds 189 and 232).
func TestComposeMoves_AFileWhoseTargetIsInAMovingFolderLandsInsideItFirst(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("e", syncdomain.ItemTypeFolder, "e", "e", "e"),
		composeNode("d", syncdomain.ItemTypeFolder, "d", "d", "e/d"),
		composeNode("c", syncdomain.ItemTypeFile, "d/c.swap-1.md", "d/c.swap-1.md", "c.md"),
		composeNode("r", syncdomain.ItemTypeFile, "c.md", "c.md", "e/d/c.md"),
	}
	c := composeForTest(nodes...)

	assert.ElementsMatch(t, []string{"d->e/d", "c.md->d/c.md", "d/c.swap-1.md->c.md"}, composedMovesBySide(&c, moveCoordLocal))
	assert.Empty(t, c.deferred)
	actions := composedMoveActions(&c, nodes)
	kept, _, _ := isolateDependencyCycles(actions, nil)
	assert.Len(t, kept, len(actions), "every action of the plan has an order")
}

// Validates: R-2.17.6, R-2.17.2
//
// Here x moved into Q as Q/renamed-x, a new x was made and Q moved into it,
// while OneDrive moved Q into the old x. OneDrive's move wins, so the old x
// must move back onto x, which the new folder holds while it holds the old
// x's source too: no move can land. The new folder is renamed to a conflict
// copy this pass, kept with everything in it, and the old x waits for the
// next pass, which moves it out of the copy onto its path.
func TestComposeMoves_AFolderInsideTheNewFolderAtItsPathMovesItAsideFirst(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("x", syncdomain.ItemTypeFolder, "x", "x/Q/renamed-x", "x"),
		composeNode("Q", syncdomain.ItemTypeFolder, "Q", "x/Q", "x/Q"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.local["x"] = true
	c := composeMoves(nodes, occupancy, nil)

	assert.Equal(t, []string{"x"}, c.asideFolders)
	assert.True(t, c.deferred["x"], "the old x waits for the next pass")
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal), "nothing moves into the folder renamed aside")
}

// Validates: R-2.17.6, R-2.17.1
//
// Here H moved aside, Y/f moved into a new Y/f/a/f, Y moved into a new folder
// H as H/Y, and the old H into H/Y/f/a/H, while OneDrive kept H, Y and Y/f.
// Y's OneDrive move lands in the folder made at H's path, which OneDrive
// gives the old H, and H's own target lies below Y's: taken back, Y's move
// held H, and H's waited for Y, so all three waited for good. H moves aside
// on OneDrive first, which frees its path for the new H, and Y moves into
// it in the same plan; f, whose new folder is at its own path, and H wait
// for a later pass, and nothing is made at f's path (ownLandings). Every
// move of the plan has an order.
func TestComposeMoves_AFolderWhoseTargetLiesBelowAMoveIntoAFolderAtItsPathMovesAsideFirst(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("H", syncdomain.ItemTypeFolder, "H", "H/Y/f/a/H", "H"),
		composeNode("h", syncdomain.ItemTypeFile, "H/h.txt", "H/Y/f/a/H/h.txt", "H/h.txt"),
		composeNode("Y", syncdomain.ItemTypeFolder, "Y", "H/Y", "Y"),
		composeNode("f", syncdomain.ItemTypeFolder, "Y/f", "H/Y/f/a/f", "Y/f"),
		composeNode("c", syncdomain.ItemTypeFile, "Y/f/c.txt", "H/Y/f/a/f/c.txt", "Y/f/c.txt"),
	}
	occupancy := composeOccupancy(nodes)
	for _, made := range []string{"H", "H/Y/f", "H/Y/f/a"} {
		occupancy.local[made] = true
	}
	c := composeMoves(nodes, occupancy, nil)

	assert.Equal(t, []string{"Y->H/Y", "H->H.swap-1"}, composedMovesBySide(&c, moveCoordRemote))
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))
	assert.Equal(t, map[string]bool{"H": true, "f": true}, c.deferred)
	assert.True(t, atOrBelowOwnLanding("H/Y/f/a", c.ownLandings[moveCoordRemote]), "nothing is made at f's path")
	assert.False(t, atOrBelowOwnLanding("H", c.ownLandings[moveCoordRemote]), "the new H is made on OneDrive")

	actions := composedMoveActions(&c, nodes)
	kept, _, _ := isolateDependencyCycles(actions, nil)
	assert.Len(t, kept, len(actions), "every action of the plan has an order")
}

// Validates: R-2.17.6, R-2.17.1
//
// On OneDrive an entry is moved into a folder and the folder then takes the
// entry's old path: p into x and x to p, x into p and p to x, p/f into x and
// x onto p/f, or the file z into p and p onto z. Here the entry still holds
// the path the folder's move lands on, and its own target hangs off the
// folder, so both waited for each other and were deferred on every pass. The
// entry moves first into the folder at the folder's path here, and the
// folder's move, which waits for it to leave that path, then carries it to
// its target: both run in one plan, with no cycle between them.
func TestComposeMoves_AnEntryMovedOnOneDriveIntoAFolderThatTakesItsPathMovesIntoItHereFirst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		nodes []moveNode
		want  []string
	}{
		{
			name: "p into x, x to p",
			nodes: []moveNode{
				composeNode("p", syncdomain.ItemTypeFolder, "p", "p", "p/p"),
				composeNode("f", syncdomain.ItemTypeFile, "p/f", "p/f", "p/p/f"),
				composeNode("x", syncdomain.ItemTypeFolder, "x", "x", "p"),
			},
			want: []string{"p->x/p", "x->p"},
		},
		{
			name: "p/f into x, x onto p/f",
			nodes: []moveNode{
				composeNode("p", syncdomain.ItemTypeFolder, "p", "p", "p"),
				composeNode("f", syncdomain.ItemTypeFile, "p/f", "p/f", "p/f/f"),
				composeNode("x", syncdomain.ItemTypeFolder, "x", "x", "p/f"),
			},
			want: []string{"p/f->x/f", "x->p/f"},
		},
		{
			name: "the file z into p, p onto z",
			nodes: []moveNode{
				composeNode("p", syncdomain.ItemTypeFolder, "p", "p", "z"),
				composeNode("f", syncdomain.ItemTypeFile, "p/f", "p/f", "z/f"),
				composeNode("z", syncdomain.ItemTypeFile, "z", "z", "z/z"),
			},
			want: []string{"z->p/z", "p->z"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := composeMoves(tc.nodes, composeOccupancy(tc.nodes), nil)

			require.Equal(t, tc.want, composedMovesBySide(&c, moveCoordLocal))
			assert.Empty(t, composedMovesBySide(&c, moveCoordRemote))
			assert.Empty(t, c.deferred)

			actions := composedMoveActions(&c, tc.nodes)
			kept, deps, _ := isolateDependencyCycles(actions, nil)
			require.Len(t, kept, len(actions), "no move is dropped as a dependency cycle")
			assert.Equal(t, []int{0}, deps[1], "the folder's move waits for the entry to leave its destination")
			assert.Empty(t, deps[0], "the entry's move into the folder waits for nothing")
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
		composeNode("Q", syncdomain.ItemTypeFolder, "Q", "Q", "R"),
		composeNode("z", syncdomain.ItemTypeFolder, "Q/z", "Q/z", "R/h"),
		composeNode("h", syncdomain.ItemTypeFolder, "Q/h", "Q/h", "R/z"),
		composeNode("m", syncdomain.ItemTypeFile, "m", "m", "Q/z/m"),
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
		composeNode("f", syncdomain.ItemTypeFolder, "f", "f2", "f"),
		composeNode("b", syncdomain.ItemTypeFile, "f/b.txt", "f/d.txt", "f/b.txt"),
		composeNode("c", syncdomain.ItemTypeFile, "f/c.txt", "f2/c.txt", "f/c.txt"),
		composeNode("g", syncdomain.ItemTypeFolder, "g", "f/b.txt", "g"),
		composeNode("g-file", syncdomain.ItemTypeFile, "g/g.txt", "f/b.txt/g.txt", "g/g.txt"),
	}
	occupancy := composeOccupancy(first)
	occupancy.local["f"] = true
	one := composeMoves(first, occupancy, nil)

	assert.Equal(t, []string{"f->f2"}, composedMovesBySide(&one, moveCoordRemote), "first pass")
	assert.Equal(t, map[string]bool{"b": true, "g": true}, one.deferred, "first pass")

	second := []moveNode{
		composeNode("f", syncdomain.ItemTypeFolder, "f2", "f2", "f2"),
		composeNode("new-f", syncdomain.ItemTypeFolder, "f", "f", "f"),
		composeNode("b", syncdomain.ItemTypeFile, "f2/b.txt", "f/d.txt", "f2/b.txt"),
		composeNode("c", syncdomain.ItemTypeFile, "f2/c.txt", "f2/c.txt", "f2/c.txt"),
		composeNode("g", syncdomain.ItemTypeFolder, "g", "f/b.txt", "g"),
		composeNode("g-file", syncdomain.ItemTypeFile, "g/g.txt", "f/b.txt/g.txt", "g/g.txt"),
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
		deferredMoves: map[string][]syncdomain.ActionType{"item-b": {syncdomain.ActionRemoteMove}},
	}
	update := syncdomain.Action{Type: syncdomain.ActionBaselineUpdate, Path: "f/b.txt", ItemID: "item-b"}
	other := syncdomain.Action{Type: syncdomain.ActionBaselineUpdate, Path: "f/c.txt", ItemID: "item-c"}

	assert.Equal(t, []syncdomain.Action{other}, dropReconciliationOfComposedMoves([]syncdomain.Action{update, other}, held, nil, syncdomain.SyncBidirectional))
	assert.Equal(t, []syncdomain.Action{update, other},
		dropReconciliationOfComposedMoves([]syncdomain.Action{update, other}, held, nil, syncdomain.SyncDownloadOnly),
		"download-only never moves the item on OneDrive")
}

// heldSourceDeleteCase is one row of
// TestPlanBuiltActions_ADeleteAboveAMoveHeldForTheNextPassWaitsWithIt.
type heldSourceDeleteCase struct {
	name string
	node moveNode
	// held is the moves composition holds for the next pass, nil for none;
	// moved is the move it composes, built as composed.
	held     []syncdomain.ActionType
	moved    []composedMove
	composed []syncdomain.Action
	// waiting is the creates that wait with a held move, built aside.
	waiting []syncdomain.Action
	actions []syncdomain.Action
	mode    syncdomain.SyncMode
	// want lists the actions the plan keeps, as "type path".
	want                []string
	dropped, unfinished int
	deferredByMode      syncdomain.DeferredCounts
}

func heldSourceDeleteCases() []heldSourceDeleteCase {
	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	remoteDelete := func(p string, view *syncdomain.PathView) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionRemoteDelete, Path: p, View: view}
	}
	localDelete := func(p string, view *syncdomain.PathView) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionLocalDelete, Path: p, View: view}
	}
	remoteMove := func(from, to string, view *syncdomain.PathView) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionRemoteMove, ItemID: "item-" + from, OldPath: from, Path: to, View: view}
	}
	madeHere := func(p string) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionUpload, Path: p, View: &syncdomain.PathView{Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile}}}
	}
	onOneDrive, here := []syncdomain.ActionType{syncdomain.ActionRemoteMove}, []syncdomain.ActionType{syncdomain.ActionLocalMove}
	heldInP := composeNode("f", syncdomain.ItemTypeFolder, "P/f", "x/f", "P/f")

	return append([]heldSourceDeleteCase{
		{
			name: "P deleted here holds f, whose OneDrive move waits", node: heldInP, held: onOneDrive,
			actions: []syncdomain.Action{
				remoteDelete("P", folder), remoteDelete("P/old.txt", file), remoteDelete("Pa", folder),
				remoteMove("P/g", "y/g", file),
			},
			want:    []string{"remote_delete P/old.txt", "remote_delete Pa", "remote_move y/g"},
			dropped: 1, unfinished: 2,
		},
		{
			name: "P and P/Q deleted here hold f", held: onOneDrive,
			node:    composeNode("f", syncdomain.ItemTypeFolder, "P/Q/f", "x/f", "P/Q/f"),
			actions: []syncdomain.Action{remoteDelete("P/Q", folder), remoteDelete("P", folder), remoteDelete("R", folder)},
			want:    []string{"remote_delete R"}, dropped: 2, unfinished: 3,
		},
		{
			name: "P deleted on OneDrive holds f here, whose local move waits", held: here,
			node:    composeNode("f", syncdomain.ItemTypeFolder, "P/f", "P/f", "x/f"),
			actions: []syncdomain.Action{localDelete("P", folder), localDelete("P/gone.txt", file)},
			want:    []string{"local_delete P/gone.txt"}, dropped: 1, unfinished: 2,
		},
		{
			name: "a file made here at P's path waits with P's delete", node: heldInP, held: onOneDrive,
			actions: []syncdomain.Action{remoteDelete("P", folder), madeHere("P")},
			dropped: 2, unfinished: 3,
		},
		{
			name: "a file made here at p, the same name as P on OneDrive, waits with P's delete", node: heldInP, held: onOneDrive,
			actions: []syncdomain.Action{remoteDelete("P", folder), madeHere("p")},
			dropped: 2, unfinished: 3,
		},
		{
			name: "download-only holds P's delete back itself", node: heldInP, held: onOneDrive, mode: syncdomain.SyncDownloadOnly,
			actions:        []syncdomain.Action{remoteDelete("P", folder)},
			deferredByMode: syncdomain.DeferredCounts{RemoteDeletes: 1},
		},
		{
			name:  "f's move into a folder made here where OneDrive edited the file K, trimmed below the keep-both K",
			node:  composeNode("f", syncdomain.ItemTypeFolder, "P/f", "K/f", "P/f"),
			moved: []composedMove{{key: "f", side: moveCoordRemote, oldPath: "P/f", newPath: "K/f", localPath: "K/f"}},
			composed: []syncdomain.Action{{
				Type: syncdomain.ActionRemoteMove, ItemID: "f", OldPath: "P/f", Path: "K/f", View: folder,
			}},
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionConflictCopy, Path: "K", View: &syncdomain.PathView{
					Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFolder}, Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFile},
				}},
				remoteDelete("P", folder),
			},
			want: []string{"conflict_copy K"}, dropped: 2, unfinished: 2,
		},
		{
			name: "f's move planned: P's delete runs after it", node: heldInP,
			moved:    []composedMove{{key: "f", side: moveCoordRemote, oldPath: "P/f", newPath: "x/f", localPath: "x/f"}},
			composed: []syncdomain.Action{{Type: syncdomain.ActionRemoteMove, ItemID: "f", OldPath: "P/f", Path: "x/f", View: folder}},
			actions:  []syncdomain.Action{remoteDelete("P", folder), madeHere("P")},
			want:     []string{"remote_delete P", "upload P", "remote_move x/f"},
		},
	}, slices.Concat(
		carriedHeldDeleteCases(folder, file), heldWriteDeleteCases(folder), cycledWriteDeleteCases(folder),
		aheadOfContentDeleteCases())...)
}

// heldWriteDeleteCases are the rows of heldSourceDeleteCases in which a held
// create writes the content of a synced item another delete removes.
func heldWriteDeleteCases(folder *syncdomain.PathView) []heldSourceDeleteCase {
	synced := func(inode uint64, itemID string) *syncdomain.PathView {
		return &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{
			ItemType: syncdomain.ItemTypeFile, ItemID: itemID, LocalHash: contentOfInode(inode), LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
		}}
	}
	uploadOf := func(p string, inode uint64) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionUpload, Path: p, View: &syncdomain.PathView{Local: &syncdomain.LocalState{
			ItemType: syncdomain.ItemTypeFile, Hash: contentOfInode(inode), LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
		}}}
	}
	heldInP := composeNode("f", syncdomain.ItemTypeFolder, "P/f", "x/f", "P/f")

	return []heldSourceDeleteCase{
		{
			name: "z renamed here onto P/f, which f's waiting move leaves: OneDrive's delete of z waits with z's upload",
			node: heldInP, held: []syncdomain.ActionType{syncdomain.ActionRemoteMove},
			waiting: []syncdomain.Action{uploadOf("P/f", 7)},
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
				{Type: syncdomain.ActionRemoteDelete, Path: "y", View: synced(8, "item-y")},
			},
			want: []string{"remote_delete y"}, dropped: 1, unfinished: 2,
		},
		{
			name: "z renamed here onto P, deleted here: z's delete waits with the upload at P, which waits with P's delete",
			node: heldInP, held: []syncdomain.ActionType{syncdomain.ActionRemoteMove},
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionRemoteDelete, Path: "P", View: folder},
				uploadOf("P", 7),
				{Type: syncdomain.ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
			},
			dropped: 3, unfinished: 4,
		},
		{
			name: "y moved on OneDrive onto a path here that f's waiting move leaves: the delete of y here waits with its download",
			node: composeNode("f", syncdomain.ItemTypeFolder, "P/f", "P/f", "x/f"), held: []syncdomain.ActionType{syncdomain.ActionLocalMove},
			waiting: []syncdomain.Action{{Type: syncdomain.ActionDownload, Path: "P/f", ItemID: "item-y", View: &syncdomain.PathView{}}},
			actions: []syncdomain.Action{{Type: syncdomain.ActionLocalDelete, Path: "y", View: synced(8, "item-y")}},
			dropped: 1, unfinished: 2,
		},
		{
			name: "g made on OneDrive in P, whose move into x here waits: g's download, rebased below x, waits with it",
			node: composeNode("P", syncdomain.ItemTypeFolder, "P", "x/P", "P"), held: []syncdomain.ActionType{syncdomain.ActionRemoteMove},
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionDownload, Path: "P/g", ItemID: "item-g", View: &syncdomain.PathView{
					Path: "P/g", Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFile},
				}},
				{Type: syncdomain.ActionDownload, Path: "y/h", ItemID: "item-h", View: &syncdomain.PathView{
					Path: "y/h", Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFile},
				}},
			},
			want: []string{"download y/h"}, dropped: 1, unfinished: 2,
		},
		{
			name: "a waiting upload of another file holds no delete",
			node: heldInP, held: []syncdomain.ActionType{syncdomain.ActionRemoteMove},
			waiting: []syncdomain.Action{uploadOf("P/f", 9)},
			actions: []syncdomain.Action{{Type: syncdomain.ActionRemoteDelete, Path: "z", View: synced(7, "item-z")}},
			want:    []string{"remote_delete z"}, unfinished: 1,
		},
		{
			name: "z renamed here onto P/f, which f still holds on OneDrive this pass: the upload there waits, and so does z's delete",
			node: heldInP, held: []syncdomain.ActionType{syncdomain.ActionRemoteMove},
			actions: []syncdomain.Action{
				uploadOf("P/f", 7),
				{Type: syncdomain.ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
			},
			dropped: 2, unfinished: 3,
		},
		{
			name: "z renamed here into the folder made at K, where OneDrive edited the file K: the upload trimmed below the keep-both K holds z's delete",
			node: composeNode("y", syncdomain.ItemTypeFile, "y", "y", "y"),
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionConflictCopy, Path: "K", View: &syncdomain.PathView{
					Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFolder}, Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFile},
				}},
				uploadOf("K/z", 7),
				{Type: syncdomain.ActionRemoteDelete, Path: "z", View: synced(7, "item-z")},
			},
			want: []string{"conflict_copy K"}, dropped: 2, unfinished: 2,
		},
	}
}

// aheadOfContentDeleteCases are the rows of heldSourceDeleteCases in which a
// create writes a deleted file's content but waits for that delete, so the
// delete cannot wait for it in the plan.
func aheadOfContentDeleteCases() []heldSourceDeleteCase {
	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}, Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFolder}}
	elsewhere := composeNode("y", syncdomain.ItemTypeFile, "y", "y", "y")
	renamed := func(from, to string) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionRemoteMove, ItemID: "item-" + from, OldPath: from, Path: to, View: folder}
	}
	movedUpOntoP := []syncdomain.Action{
		renamed("P", "Q"),
		{Type: syncdomain.ActionRemoteDelete, Path: "P/f", View: syncedFileView(7, "item-f")},
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
			node: elsewhere, actions: slices.Clone(movedUpOntoP), mode: syncdomain.SyncDownloadOnly,
			deferredByMode: syncdomain.DeferredCounts{Moves: 1, Uploads: 1, RemoteDeletes: 1},
		},
		{
			name: "a1.txt edited in A renamed B: the upload where A's move carries a1.txt needs its old copy deleted first",
			node: elsewhere,
			actions: []syncdomain.Action{
				renamed("A", "B"),
				{Type: syncdomain.ActionRemoteDelete, Path: "A/a1.txt", View: syncedFileView(7, "item-a1")},
				uploadOfIdentity("B/a1.txt", 7),
			},
			want: []string{"remote_move B", "remote_delete A/a1.txt", "upload B/a1.txt"},
		},
		{
			name: "x moved up onto its folder P's path, P deleted: the upload waits for P's delete, which waits for P/x's",
			node: elsewhere,
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionRemoteDelete, Path: "P", View: folder},
				{Type: syncdomain.ActionRemoteDelete, Path: "P/x", View: syncedFileView(8, "item-x")},
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
func cycledWriteDeleteCases(folder *syncdomain.PathView) []heldSourceDeleteCase {
	synced := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{
		ItemType: syncdomain.ItemTypeFile, ItemID: "item-z", LocalHash: contentOfInode(7), LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
	}}
	uploadOfZ := func(p string) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionUpload, Path: p, View: &syncdomain.PathView{Local: &syncdomain.LocalState{
			ItemType: syncdomain.ItemTypeFile, Hash: contentOfInode(7), LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
		}}}
	}
	cycle := []syncdomain.Action{
		{Type: syncdomain.ActionRemoteMove, ItemID: "item-x", OldPath: "x", Path: "x/x2", View: folder},
		{Type: syncdomain.ActionFolderCreate, Path: "x", CreateSide: syncdomain.CreateRemote},
	}
	elsewhere := composeNode("y", syncdomain.ItemTypeFile, "y", "y", "y")

	return []heldSourceDeleteCase{
		{
			name: "z renamed here into x/x2: its upload waits for x's move, and z's delete for the upload",
			node: elsewhere,
			actions: slices.Concat(cycle, []syncdomain.Action{
				uploadOfZ("x/x2/z"), {Type: syncdomain.ActionRemoteDelete, Path: "z", View: synced},
			}),
			dropped: 4, unfinished: 4,
		},
		{
			name: "x/z renamed here onto x/x2/w: z's delete, which x's move waits for, waits with the upload after that move",
			node: elsewhere,
			actions: slices.Concat(cycle, []syncdomain.Action{
				{Type: syncdomain.ActionRemoteDelete, Path: "x/z", View: synced}, uploadOfZ("x/x2/w"),
			}),
			dropped: 4, unfinished: 4,
		},
		{
			name: "z moved up onto its folder P's path, P deleted, P/y's move out of P in a cycle: the upload after P's delete holds z's delete",
			node: elsewhere,
			actions: []syncdomain.Action{
				// P/y into a new k, whose create waits for K, which OneDrive
				// takes for k, to move away, into P/y's destination.
				{Type: syncdomain.ActionRemoteMove, ItemID: "item-y", OldPath: "P/y", Path: "k/y", View: folder},
				{Type: syncdomain.ActionFolderCreate, Path: "k", CreateSide: syncdomain.CreateRemote},
				{Type: syncdomain.ActionRemoteMove, ItemID: "item-K", OldPath: "K", Path: "k/y/K", View: folder},
				{Type: syncdomain.ActionRemoteDelete, Path: "P", View: &syncdomain.PathView{
					Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}, Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFolder},
				}},
				{Type: syncdomain.ActionRemoteDelete, Path: "P/z", View: synced},
				uploadOfZ("P"),
			},
			dropped: 6, unfinished: 6,
		},
	}
}

// carriedHeldDeleteCases are the rows of heldSourceDeleteCases in which a
// folder's move has a held action below its source.
func carriedHeldDeleteCases(folder, file *syncdomain.PathView) []heldSourceDeleteCase {
	heldInAP := composeNode("f", syncdomain.ItemTypeFolder, "A/P/f", "x/f", "A/P/f")
	renameA := composedMove{key: "A", side: moveCoordRemote, oldPath: "A", newPath: "B", localPath: "B"}
	madeHere := func(p string) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionUpload, Path: p, View: &syncdomain.PathView{Local: &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFile}}}
	}

	return []heldSourceDeleteCase{
		{
			name: "A renamed B here carries P, which holds f, and B/a1.txt edited is uploaded after it",
			node: heldInAP, held: []syncdomain.ActionType{syncdomain.ActionRemoteMove},
			moved:    []composedMove{renameA},
			composed: []syncdomain.Action{{Type: syncdomain.ActionRemoteMove, ItemID: "A", OldPath: "A", Path: "B", View: folder}},
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionRemoteDelete, Path: "A/P", View: folder},
				{Type: syncdomain.ActionRemoteDelete, Path: "A/a1.txt", View: file},
				madeHere("B/a1.txt"), madeHere("B/new.txt"),
			},
			want:    []string{"remote_delete A/a1.txt", "upload B/a1.txt", "upload B/new.txt", "remote_move B"},
			dropped: 1, unfinished: 2,
		},
		{
			name: "A renamed B on OneDrive carries P here, which holds f",
			node: composeNode("f", syncdomain.ItemTypeFolder, "A/P/f", "A/P/f", "x/f"), held: []syncdomain.ActionType{syncdomain.ActionLocalMove},
			moved: []composedMove{{key: "A", side: moveCoordLocal, oldPath: "A", newPath: "B", localPath: "A"}},
			composed: []syncdomain.Action{{
				Type: syncdomain.ActionLocalMove, ItemID: "A", OldPath: "A", Path: "B", View: folder,
			}},
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionLocalDelete, Path: "A/P", View: folder},
				{Type: syncdomain.ActionDownload, Path: "B/new.txt", View: &syncdomain.PathView{Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFile}}},
			},
			want:    []string{"download B/new.txt", "local_move B"},
			dropped: 1, unfinished: 2,
		},
		{
			name: "A moved into p/A, where c leaves A for p, the same name as P on OneDrive, waits with c's move",
			node: composeNode("f", syncdomain.ItemTypeFolder, "P/f", "x/f", "P/f"), held: []syncdomain.ActionType{syncdomain.ActionRemoteMove},
			actions: []syncdomain.Action{
				{Type: syncdomain.ActionRemoteDelete, Path: "P", View: folder},
				{Type: syncdomain.ActionRemoteMove, ItemID: "c", OldPath: "A/c", Path: "p", View: folder},
				{Type: syncdomain.ActionRemoteMove, ItemID: "A", OldPath: "A", Path: "p/A", View: folder},
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

			composition := &moveComposition{moves: tc.moved, deferred: map[string]bool{}, deferredMoves: map[string][]syncdomain.ActionType{}}
			if tc.held != nil {
				composition.deferred[tc.node.key] = true
				composition.deferredMoves[tc.node.key] = tc.held
			}
			inputs := &moveInputs{nodes: []moveNode{tc.node}}
			plan, err := NewPlanner(nil).planBuiltActions(
				tc.actions, tc.composed, tc.waiting, composition, inputs, PlannerMountContext{}, tc.mode)
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	composition := &moveComposition{
		deferred:      map[string]bool{"f": true},
		deferredMoves: map[string][]syncdomain.ActionType{"f": {syncdomain.ActionRemoteMove}},
	}
	inputs := &moveInputs{nodes: []moveNode{composeNode("f", syncdomain.ItemTypeFolder, "P/Q/f", "x/f", "P/Q/f")}}
	actions := []syncdomain.Action{
		{Type: syncdomain.ActionRemoteDelete, Path: "P/Q", View: folder},
		{Type: syncdomain.ActionRemoteDelete, Path: "P", View: folder},
		{Type: syncdomain.ActionRemoteDelete, Path: "R", View: folder},
		{
			Type: syncdomain.ActionRemoteMove, ItemID: "item-S", OldPath: "S", Path: "T",
			View: &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}, Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFolder}},
		},
		{Type: syncdomain.ActionRemoteDelete, Path: "S/g", View: syncedFileView(7, "item-g")},
		uploadOfIdentity("S", 7),
	}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, err := NewPlanner(logger).planBuiltActions(actions, nil, nil, composition, inputs, PlannerMountContext{}, syncdomain.SyncBidirectional)
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
		deferredMoves: map[string][]syncdomain.ActionType{"P": {syncdomain.ActionRemoteMove}},
	}
	inputs := &moveInputs{nodes: []moveNode{composeNode("P", syncdomain.ItemTypeFolder, "P", "x/P", "P")}}
	madeOnOneDrive := func(p, itemID string) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionDownload, Path: p, ItemID: itemID, View: &syncdomain.PathView{
			Path: p, Remote: &syncdomain.RemoteState{ItemType: syncdomain.ItemTypeFile},
		}}
	}
	actions := []syncdomain.Action{madeOnOneDrive("P/g", "item-g"), madeOnOneDrive("y/h", "item-h")}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	plan, err := NewPlanner(logger).planBuiltActions(actions, nil, nil, composition, inputs, PlannerMountContext{}, syncdomain.SyncBidirectional)
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	actions := []syncdomain.Action{
		{Type: syncdomain.ActionLocalMove, OldPath: "A", Path: "Z", View: folder},                 // 0
		{Type: syncdomain.ActionLocalMove, OldPath: "A/out.txt", Path: "out.txt", View: file},     // 1: leaves A first
		{Type: syncdomain.ActionDownload, Path: "Z/new.txt", View: file},                          // 2: under 0's destination
		{Type: syncdomain.ActionLocalMove, OldPath: "b.txt", Path: "c.txt", View: file},           // 3
		{Type: syncdomain.ActionLocalMove, OldPath: "a.txt", Path: "b.txt", View: file},           // 4: onto 3's vacated path
		{Type: syncdomain.ActionRemoteMove, OldPath: "p.txt", Path: "q.txt", View: file},          // 5
		{Type: syncdomain.ActionUpload, Path: "p.txt", View: &syncdomain.PathView{Path: "p.txt"}}, // 6: new entry at 5's old path
		{Type: syncdomain.ActionDownload, Path: "A/in.txt", View: file},                           // 7: lands before A moves
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

	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	actions := []syncdomain.Action{
		{Type: syncdomain.ActionLocalMove, OldPath: "r.txt", Path: "b.txt", View: file}, // 0
		{Type: syncdomain.ActionDownload, Path: "r.txt/inside.txt", View: file},         // 1
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	actions := []syncdomain.Action{
		{Type: syncdomain.ActionRemoteMove, OldPath: "q", Path: "docs", View: folder},         // 0
		{Type: syncdomain.ActionFolderCreate, Path: "q", CreateSide: syncdomain.CreateRemote}, // 1: re-created at 0's old path
		{Type: syncdomain.ActionCleanup, Path: "q/x", View: folder},                           // 2: gone online, moved locally
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	folderMove := syncdomain.Action{Type: syncdomain.ActionRemoteMove, OldPath: "docs", Path: "photos/photos", View: folder}
	recreate := syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateRemote}
	fileMoveTo := func(p string) syncdomain.Action {
		return syncdomain.Action{Type: syncdomain.ActionRemoteMove, OldPath: "photos/photos/50%.txt", Path: p, View: file}
	}

	for _, tc := range []struct {
		name     string
		actions  []syncdomain.Action
		wantDeps [][]int
	}{
		{name: "first of two runs", actions: []syncdomain.Action{folderMove, recreate}, wantDeps: [][]int{nil, {0}}},
		{name: "second of two runs", actions: []syncdomain.Action{fileMoveTo("docs/renamed-50%.txt")}, wantDeps: [][]int{nil}},
		{
			name:     "one run",
			actions:  []syncdomain.Action{folderMove, fileMoveTo("docs/renamed-50%.txt"), recreate},
			wantDeps: [][]int{nil, {0, 2}, {0}},
		},
		{
			name:     "one run, the file under its own name",
			actions:  []syncdomain.Action{folderMove, fileMoveTo("docs/50%.txt"), recreate},
			wantDeps: [][]int{nil, {0, 2}, {0}},
		},
		{name: "a folder the plan creates on this computer at the path a local move leaves", actions: []syncdomain.Action{
			{Type: syncdomain.ActionLocalMove, OldPath: "docs", Path: "photos/photos", View: folder},
			{Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateLocal},
		}, wantDeps: [][]int{nil, {0}}},
		{name: "one run, into a folder made inside the new one", actions: []syncdomain.Action{
			folderMove,
			fileMoveTo("docs/new/50%.txt"),
			recreate,
			{Type: syncdomain.ActionFolderCreate, Path: "docs/new", CreateSide: syncdomain.CreateRemote},
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	move := func(moveType syncdomain.ActionType, from, to string, view *syncdomain.PathView) syncdomain.Action {
		return syncdomain.Action{Type: moveType, OldPath: from, Path: to, View: view}
	}

	for _, tc := range []struct {
		name     string
		actions  []syncdomain.Action
		wantDeps [][]int
	}{
		{name: "on this computer", actions: []syncdomain.Action{
			move(syncdomain.ActionLocalMove, "x", "x2", folder),
			move(syncdomain.ActionLocalMove, "f", "x/f", folder),
		}, wantDeps: [][]int{nil, {0}}},
		{name: "on OneDrive, with the new x", actions: []syncdomain.Action{
			move(syncdomain.ActionRemoteMove, "x", "x2", folder),
			move(syncdomain.ActionRemoteMove, "f", "x/f", folder),
			{Type: syncdomain.ActionFolderCreate, Path: "x", CreateSide: syncdomain.CreateRemote},
		}, wantDeps: [][]int{nil, {0, 2}, {0}}},
		{name: "into a new X", actions: []syncdomain.Action{
			move(syncdomain.ActionLocalMove, "x", "x2", folder),
			move(syncdomain.ActionLocalMove, "f", "X/f", folder),
		}, wantDeps: [][]int{nil, {0}}},
		{name: "on OneDrive, with the new X", actions: []syncdomain.Action{
			move(syncdomain.ActionRemoteMove, "x", "x2", folder),
			move(syncdomain.ActionRemoteMove, "f", "X/f", folder),
			{Type: syncdomain.ActionFolderCreate, Path: "X", CreateSide: syncdomain.CreateRemote},
		}, wantDeps: [][]int{nil, {0, 2}, {0}}},
		{name: "a rename inside the vacated path", actions: []syncdomain.Action{
			move(syncdomain.ActionLocalMove, "x", "x2", folder),
			move(syncdomain.ActionLocalMove, "x/a.txt", "x/b.txt", file),
		}, wantDeps: [][]int{{1}, nil}},
		{name: "a rename inside the vacated path, named X", actions: []syncdomain.Action{
			move(syncdomain.ActionLocalMove, "x", "x2", folder),
			move(syncdomain.ActionLocalMove, "x/a.txt", "X/b.txt", file),
		}, wantDeps: [][]int{{1}, nil}},
		{name: "a move of the other side", actions: []syncdomain.Action{
			move(syncdomain.ActionLocalMove, "x", "x2", folder),
			move(syncdomain.ActionRemoteMove, "f", "x/f", folder),
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

// Validates: R-2.17.9, R-2.17.2
//
// OneDrive deleted archive and moved notes.txt onto its path, and the plan
// deletes archive here: the local move of notes.txt onto archive waits for
// that delete, which waits for the deletes and the cleanup below it, and
// those do not wait for the move. Below a local move's destination a local
// delete, in snapshot coordinates, and a cleanup, in the baseline's, remove
// what holds the destination now; ordered after the move they closed a cycle
// through the folder's delete, dropped on every pass.
func TestBuildDependencies_ALocalMoveOntoAFolderDeletedHereWaitsForItsDelete(t *testing.T) {
	t.Parallel()

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	actions := []syncdomain.Action{
		{Type: syncdomain.ActionLocalDelete, Path: "archive/old.txt", View: file},
		{Type: syncdomain.ActionCleanup, Path: "archive/gone.txt", View: file},
		{Type: syncdomain.ActionLocalDelete, Path: "archive", View: folder},
		{Type: syncdomain.ActionLocalMove, OldPath: "notes.txt", Path: "archive", View: file},
	}
	kept, deps, _ := isolateDependencyCycles(actions, nil)
	require.Len(t, kept, len(actions), "no action is dropped as a cycle")
	assert.Equal(t, [][]int{nil, nil, {0, 1}, {2}}, deps)

	remote := []syncdomain.Action{
		{Type: syncdomain.ActionRemoteDelete, Path: "archive/old.txt", View: file},
		{Type: syncdomain.ActionRemoteMove, OldPath: "notes.txt", Path: "archive", View: file},
	}
	assert.Equal(t, [][]int{{1}, nil}, buildDependencies(remote), "a delete on OneDrive below a move's destination still waits for it")
}

// Validates: R-2.17.6
//
// Local moves are in the snapshot's coordinates, so a local move whose source
// lies below another local move's destination moves an entry out of what
// holds that destination now: here p moved into x while OneDrive moved x into
// p and z onto x, and this computer follows OneDrive. p leaves x first, x
// moves into p, and z lands where x was. On OneDrive a move's source is
// rebased through the moves before it, so a remote move from below another
// remote move's destination still waits for it.
func TestBuildDependencies_ALocalMoveFromBelowAnotherLocalMovesDestinationRunsFirst(t *testing.T) {
	t.Parallel()

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	local := []syncdomain.Action{
		{Type: syncdomain.ActionLocalMove, OldPath: "x/p", Path: "p", View: folder},
		{Type: syncdomain.ActionLocalMove, OldPath: "x", Path: "p/x", View: folder},
		{Type: syncdomain.ActionLocalMove, OldPath: "z", Path: "x", View: file},
	}
	kept, deps, _ := isolateDependencyCycles(local, nil)
	require.Len(t, kept, len(local), "no move is dropped as a cycle")
	assert.Equal(t, [][]int{nil, {0}, {1}}, deps, "p leaves x, x moves into p, z lands where x was")

	remote := []syncdomain.Action{
		{Type: syncdomain.ActionRemoteMove, OldPath: "z", Path: "x", View: file},
		{Type: syncdomain.ActionRemoteMove, OldPath: "x/a.txt", Path: "b.txt", View: file},
	}
	assert.Equal(t, [][]int{nil, {0}}, buildDependencies(remote), "a rebased OneDrive source below x waits for the move onto x")
}

// Validates: R-2.17.6
//
// A folder deleted on OneDrive while a folder is made here at its path, x
// again, X or x/sub below a deleted x/sub, and f moved into the new folder:
// the create waits for the delete and f's move for the create, so the move
// never lands in the folder the delete removes.
func TestBuildDependencies_MoveIntoAFolderMadeAtADeletedPathWaitsForTheDeleteAndTheCreate(t *testing.T) {
	t.Parallel()

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	for _, tc := range []struct{ deleted, made string }{
		{deleted: "x", made: "x"},
		{deleted: "x", made: "X"},
		{deleted: "x/sub", made: "x/sub"},
	} {
		t.Run(tc.deleted+" deleted, "+tc.made+" made", func(t *testing.T) {
			t.Parallel()

			actions := []syncdomain.Action{
				{Type: syncdomain.ActionRemoteDelete, Path: tc.deleted, View: folder},
				{Type: syncdomain.ActionFolderCreate, Path: tc.made, CreateSide: syncdomain.CreateRemote},
				{Type: syncdomain.ActionRemoteMove, OldPath: "f", Path: tc.made + "/f", View: folder},
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	folderMove := syncdomain.Action{Type: syncdomain.ActionRemoteMove, OldPath: "docs", Path: "photos/photos", View: folder}
	fileMove := syncdomain.Action{Type: syncdomain.ActionRemoteMove, OldPath: "docs/a.txt", Path: "docs/b.txt", View: file}
	recreate := syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateRemote}
	innerMove := syncdomain.Action{Type: syncdomain.ActionRemoteMove, OldPath: "docs/sub/inner.txt", Path: "docs/inner.txt", View: file}

	for _, tc := range []struct {
		name     string
		actions  []syncdomain.Action
		wantDeps [][]int
	}{
		{name: "no create at the vacated path", actions: []syncdomain.Action{fileMove, folderMove}, wantDeps: [][]int{nil, {0}}},
		{name: "a create on the other side", actions: []syncdomain.Action{
			fileMove, folderMove, {Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateLocal},
		}, wantDeps: [][]int{{2}, {0}, nil}},
		{name: "a move into the folder made there", actions: []syncdomain.Action{fileMove, folderMove, recreate}, wantDeps: [][]int{{2}, nil, {1}}},
		{name: "the same on this computer", actions: []syncdomain.Action{
			{Type: syncdomain.ActionLocalMove, OldPath: "docs/a.txt", Path: "docs/b.txt", View: file},
			{Type: syncdomain.ActionLocalMove, OldPath: "docs", Path: "photos/photos", View: folder},
			{Type: syncdomain.ActionFolderCreate, Path: "docs", CreateSide: syncdomain.CreateLocal},
		}, wantDeps: [][]int{{2}, nil, {1}}},
		{name: "a move into a folder made inside the new one", actions: []syncdomain.Action{
			{Type: syncdomain.ActionRemoteMove, OldPath: "docs/a.txt", Path: "docs/new/a.txt", View: file},
			folderMove,
			recreate,
			{Type: syncdomain.ActionFolderCreate, Path: "docs/new", CreateSide: syncdomain.CreateRemote},
		}, wantDeps: [][]int{{3}, nil, {1}, {2}}},
		{name: "a sibling that moves elsewhere", actions: []syncdomain.Action{
			fileMove,
			folderMove,
			{Type: syncdomain.ActionRemoteMove, OldPath: "docs/c.txt", Path: "c.txt", View: file},
			recreate,
		}, wantDeps: [][]int{{3}, {2}, nil, {1}}},
		{name: "a delete under the source", actions: []syncdomain.Action{
			{Type: syncdomain.ActionRemoteDelete, Path: "docs/old.txt", View: file},
			folderMove,
			recreate,
		}, wantDeps: [][]int{nil, {0}, {1}}},
		{name: "a file moved out of a renamed subfolder", actions: []syncdomain.Action{
			innerMove,
			folderMove,
			{Type: syncdomain.ActionRemoteMove, OldPath: "photos/photos/sub", Path: "photos/photos/sub2", View: folder},
			recreate,
		}, wantDeps: [][]int{{3}, nil, {1}, {1}}},
		{name: "a file moved out of a subfolder made here again", actions: []syncdomain.Action{
			{Type: syncdomain.ActionFolderCreate, Path: "archive/sub", CreateSide: syncdomain.CreateLocal},
			{Type: syncdomain.ActionDownload, Path: "archive/sub/new.txt", View: &syncdomain.PathView{Path: "archive/sub/new.txt"}},
			{Type: syncdomain.ActionRemoteMove, OldPath: "docs", Path: "archive", View: folder},
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	folderMove := syncdomain.Action{Type: syncdomain.ActionLocalMove, OldPath: "docs", Path: "archive", View: folder}

	assertDependencyRows(t, []dependencyRow{
		{name: "a.txt moved in the pass that moves docs", actions: []syncdomain.Action{
			{Type: syncdomain.ActionLocalMove, OldPath: "docs/new/a.txt", Path: "docs", View: file},
			folderMove,
		}, wantDeps: [][]int{{1}, {0}}},
		{name: "first pass", actions: []syncdomain.Action{
			folderMove,
			{Type: syncdomain.ActionFolderCreate, Path: "archive/new", CreateSide: syncdomain.CreateRemote},
		}, wantDeps: [][]int{nil, {0}}, wantKept: 2},
		{name: "second pass", actions: []syncdomain.Action{
			{Type: syncdomain.ActionLocalMove, OldPath: "archive/new/a.txt", Path: "docs", View: file},
		}, wantDeps: [][]int{nil}, wantKept: 1},
	})
}

// dependencyRow is a plan, the dependency lists buildDependencies gives it,
// and how many of its actions isolateDependencyCycles keeps.
type dependencyRow struct {
	name     string
	actions  []syncdomain.Action
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

	folder := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFolder}}
	moveX := syncdomain.Action{Type: syncdomain.ActionRemoteMove, OldPath: "x", Path: "x2", View: folder}
	moveF := syncdomain.Action{Type: syncdomain.ActionRemoteMove, OldPath: "f", Path: "x/f", View: folder}
	createX := syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "x", CreateSide: syncdomain.CreateRemote}
	createF := syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "f", CreateSide: syncdomain.CreateRemote}
	moveB := syncdomain.Action{
		Type: syncdomain.ActionRemoteMove, OldPath: "x/f/b.txt", Path: "f/d.txt",
		View: &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}},
	}

	assertDependencyRows(t, []dependencyRow{
		{
			name:     "b.txt moved in the pass that moves x and f",
			actions:  []syncdomain.Action{moveX, moveF, moveB, createX, createF},
			wantDeps: [][]int{{2}, {0, 3}, {1, 4}, {0}, {1}},
		},
		{
			name:     "first pass",
			actions:  []syncdomain.Action{moveX, moveF, createX, createF},
			wantDeps: [][]int{nil, {0, 2}, {0}, {1}}, wantKept: 4,
		},
		{name: "second pass", actions: []syncdomain.Action{moveB}, wantDeps: [][]int{nil}, wantKept: 1},
	})
}

// Validates: R-2.17.6
//
// A dependency cycle drops only the actions on it and their dependents
// (D-059c): a cycle never stops the rest of the plan.
func TestIsolateDependencyCycles_KeepsUnrelatedWork(t *testing.T) {
	t.Parallel()

	file := &syncdomain.PathView{Baseline: &syncdomain.BaselineEntry{ItemType: syncdomain.ItemTypeFile}}
	actions := []syncdomain.Action{
		// Two local moves that vacate each other's destination: a cycle.
		{Type: syncdomain.ActionLocalMove, OldPath: "a.txt", Path: "b.txt", View: file},
		{Type: syncdomain.ActionLocalMove, OldPath: "b.txt", Path: "a.txt", View: file},
		{Type: syncdomain.ActionUpload, Path: "unrelated.txt", View: &syncdomain.PathView{Path: "unrelated.txt"}},
	}
	require.Error(t, detectDependencyCycle(buildDependencies(actions)), "the fixture must contain a cycle")

	kept, deps, _ := isolateDependencyCycles(actions, nil)

	require.Len(t, kept, 1)
	assert.Equal(t, "unrelated.txt", kept[0].Path)
	require.NoError(t, detectDependencyCycle(deps))
}

// Validates: R-2.17.6
//
// Here p was renamed q, the folder x moved into it as q/x2 and a new x made,
// and f moved out of p into the new x; OneDrive changed nothing. x's move
// lands below p's new path, so it runs after p's; the new x waits for x to
// leave its path, and f's move waits for the new x. Leaving p first, f's
// move held p's move back and closed a cycle with all of them; a file
// directly in a folder OneDrive moves leaves after the folder's move, from
// below its new path, so every action has an order. A file further down
// still leaves first (TestComposeMoves_EntryBelowAFolderOnOneSideOnlyKeepsItsSource).
func TestComposeMoves_AFileLeavingAFolderMovedOnOneDriveMovesAfterIt(t *testing.T) {
	t.Parallel()

	nodes := []moveNode{
		composeNode("p", syncdomain.ItemTypeFolder, "p", "q", "p"),
		composeNode("x", syncdomain.ItemTypeFolder, "x", "q/x2", "x"),
		composeNode("f", syncdomain.ItemTypeFile, "p/f", "x/f", "p/f"),
	}
	occupancy := composeOccupancy(nodes)
	occupancy.local["x"] = true
	c := composeMoves(nodes, occupancy, nil)

	assert.ElementsMatch(t, []string{"p->q", "x->q/x2", "q/f->x/f"}, composedMovesBySide(&c, moveCoordRemote))
	assert.Empty(t, composedMovesBySide(&c, moveCoordLocal))

	actions := append(composedMoveActions(&c, nodes), syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "x", CreateSide: syncdomain.CreateRemote})
	kept, _, cycled := isolateDependencyCycles(actions, nil)
	assert.Len(t, kept, len(actions))
	assert.Empty(t, cycled)
}

// Validates: R-2.17.6
//
// Here x was renamed x2 while OneDrive keeps it at x (its move waits), and a
// new x with a folder sub in it was made here. The create of sub on OneDrive
// lies below x, which the old x still holds there, and its source here is not
// in the old x: it waits a pass and logs the folder, rather than being made
// inside the old x, carried to x2/sub and moved back. An upload of a file made
// in the old x here, at x2, runs, and so does work below a folder no other
// item holds. Here such a create is kept: a move of the plan into the folder
// it makes relies on it.
func TestDropCreatesOntoComposedItems_ACreateBelowAnotherItemsFolderWaits(t *testing.T) {
	t.Parallel()

	inputs := &moveInputs{nodes: []moveNode{
		composeNode("x", syncdomain.ItemTypeFolder, "x", "x2", "x"),
		composeNode("y", syncdomain.ItemTypeFolder, "y", "y", "y"),
	}}
	composition := &moveComposition{
		deferred:      map[string]bool{"x": true},
		deferredMoves: map[string][]syncdomain.ActionType{"x": {syncdomain.ActionRemoteMove}},
	}
	folderHere := &syncdomain.LocalState{ItemType: syncdomain.ItemTypeFolder}
	actions := []syncdomain.Action{
		{
			Type: syncdomain.ActionFolderCreate, CreateSide: syncdomain.CreateRemote, Path: "x/sub",
			View: &syncdomain.PathView{Path: "x/sub", Local: folderHere},
		},
		uploadOfIdentity("x2/g", 7),
		{
			Type: syncdomain.ActionFolderCreate, CreateSide: syncdomain.CreateRemote, Path: "y/sub",
			View: &syncdomain.PathView{Path: "y/sub", Local: folderHere},
		},
		{Type: syncdomain.ActionFolderCreate, CreateSide: syncdomain.CreateLocal, Path: "x2/sub", View: &syncdomain.PathView{Path: "x2/sub"}},
	}

	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	kept, dropped := dropCreatesOntoComposedItems(logger, actions, inputs, composition)

	var keptPaths, droppedPaths []string
	for i := range kept {
		keptPaths = append(keptPaths, kept[i].Path)
	}
	for i := range dropped {
		droppedPaths = append(droppedPaths, dropped[i].Path)
	}
	assert.Equal(t, []string{"x2/g", "y/sub", "x2/sub"}, keptPaths)
	assert.Equal(t, []string{"x/sub"}, droppedPaths)
	assert.Contains(t, logs.String(),
		`msg="create waits for another item to leave the folder path above it" action=folder_create path=x/sub folder=x`)
}

// Validates: R-2.17.8
//
// A synced file gone from its path on both sides is found by its identity at
// a path whose baseline names another item, only while it also holds its
// synced content: an edited file, or a new file on a reused inode, stays new
// content, so a move never carries it over OneDrive's copy of the item.
func TestMovedOntoAnotherItemsPath_NeedsTheIdentityAndTheSyncedContent(t *testing.T) {
	t.Parallel()

	const inode = 42
	entry := &syncdomain.BaselineEntry{
		ItemID: "item-z", ItemType: syncdomain.ItemTypeFile, LocalHash: "hash-z",
		LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
	}
	gone := &syncdomain.SQLiteComparisonRow{Path: "z", BaselineType: syncdomain.SideFile}
	cmpByPath := map[string]*syncdomain.SQLiteComparisonRow{
		"z": gone,
		"p": {Path: "p", BaselineItemID: "item-p", BaselineType: syncdomain.SideFolder, LocalType: syncdomain.SideFile},
		"n": {Path: "n", LocalType: syncdomain.SideFile},
	}
	local := func(p, hash string) []syncdomain.LocalStateRow {
		return []syncdomain.LocalStateRow{{
			Path: p, ItemType: syncdomain.ItemTypeFile, Hash: hash, LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
		}}
	}

	at, ok := movedOntoAnotherItemsPath(gone, entry, local("p", "hash-z"), cmpByPath)
	assert.True(t, ok)
	assert.Equal(t, "p", at, "the file sits at p, the path of the folder p")

	_, ok = movedOntoAnotherItemsPath(gone, entry, local("p", "hash-edited"), cmpByPath)
	assert.False(t, ok, "edited, the file is not proven the item")

	_, ok = movedOntoAnotherItemsPath(gone, entry, local("n", "hash-z"), cmpByPath)
	assert.False(t, ok, "at a path no synced item had, SQLite pairs the move itself")

	stillHere := &syncdomain.SQLiteComparisonRow{Path: "z", BaselineType: syncdomain.SideFile, RemoteType: syncdomain.SideFile}
	_, ok = movedOntoAnotherItemsPath(stillHere, entry, local("p", "hash-z"), cmpByPath)
	assert.False(t, ok, "OneDrive still holds the item at its path")
}

// Validates: R-2.17.13
//
// OneDrive moved the synced folder p onto x, where this computer holds a new
// folder of its own, and p is left here at its path. With p empty here, the
// new x takes p's item (p's position here is x, so composition re-keys) and
// p is deleted here; the baseline update SQLite planned at x is left to the
// re-key. With anything in p here, or with the folder at x a synced item's,
// nothing is merged and nothing deleted.
func TestMergeNewFoldersIntoFoldersOneDriveMovedOntoThem_OnlyAnEmptyFolderGivesWay(t *testing.T) {
	t.Parallel()

	inputsFor := func() *moveInputs {
		node := moveNode{key: "item-p", itemType: syncdomain.ItemTypeFolder}
		node.paths = [moveCoordCount]string{"p", "p", "x"}
		return &moveInputs{nodes: []moveNode{node}, frozen: map[string]bool{}, identified: map[string]string{}}
	}
	comparisons := []syncdomain.SQLiteComparisonRow{
		{Path: "p", BaselineItemID: "item-p", BaselineType: syncdomain.SideFolder, LocalType: syncdomain.SideFolder},
		{
			Path: "x", BaselineItemID: "item-x", RemoteItemID: "item-p", BaselineType: syncdomain.SideFolder,
			LocalType: syncdomain.SideFolder, RemoteType: syncdomain.SideFolder, LocalItemReplaced: true,
		},
	}
	views := map[string]*syncdomain.PathView{
		"p": {Path: "p", Baseline: &syncdomain.BaselineEntry{ItemID: "item-p", ItemType: syncdomain.ItemTypeFolder}},
		"x": {Path: "x", Remote: &syncdomain.RemoteState{ItemID: "item-p", ItemType: syncdomain.ItemTypeFolder}},
	}
	reconciled := []syncdomain.Action{makeAction(syncdomain.ActionBaselineUpdate, views["x"])}
	folders := []syncdomain.LocalStateRow{
		{Path: "p", ItemType: syncdomain.ItemTypeFolder},
		{Path: "x", ItemType: syncdomain.ItemTypeFolder},
	}

	inputs := inputsFor()
	planned := mergeNewFoldersIntoFoldersOneDriveMovedOntoThem(inputs, reconciled, comparisons, views, folders)
	require.Len(t, planned, 1)
	assert.Equal(t, syncdomain.ActionLocalDelete, planned[0].Type)
	assert.Equal(t, "p", planned[0].Path, "the empty p is deleted here")
	assert.Equal(t, "x", inputs.nodes[0].paths[moveCoordLocal], "the new x holds p's item here")

	inputs = inputsFor()
	written := append(slices.Clone(folders), syncdomain.LocalStateRow{Path: "p/new", ItemType: syncdomain.ItemTypeFile})
	planned = mergeNewFoldersIntoFoldersOneDriveMovedOntoThem(inputs, reconciled, comparisons, views, written)
	assert.Equal(t, reconciled, planned, "p holds a file here: nothing is deleted")
	assert.Equal(t, "p", inputs.nodes[0].paths[moveCoordLocal])

	inputs = inputsFor()
	other := moveNode{key: "item-y", itemType: syncdomain.ItemTypeFolder}
	other.paths = [moveCoordCount]string{"y", "x", "y"}
	inputs.nodes = append(inputs.nodes, other)
	planned = mergeNewFoldersIntoFoldersOneDriveMovedOntoThem(inputs, reconciled, comparisons, views, folders)
	assert.Equal(t, reconciled, planned, "the folder at x is the synced y moved there: no merge")
	assert.Equal(t, "p", inputs.nodes[0].paths[moveCoordLocal])
}
