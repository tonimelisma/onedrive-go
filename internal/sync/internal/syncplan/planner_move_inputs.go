package syncplan

import (
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// moveCompositionInputs adapts the SQLite comparison rows to composition
// nodes. Positions come only from SQLite's pairing, so composition never
// moves an item SQLite did not prove is the same item:
//   - local: the unique identity-paired destination of a local move, or the
//     baseline path while it still holds an entry of the same type;
//   - remote: the item's remote path when it is the baseline path, the paired
//     remote-move destination, or a destination shadowed by (or equal to) the
//     local move.
//
// A node whose paths touch unavailable truth is frozen: it keeps its baseline
// position and nothing below it moves this pass.
type moveInputs struct {
	nodes     []moveNode
	occupancy moveOccupancy
	frozen    map[string]bool
	// recreated maps an item's key to the baseline path it moved away from
	// while a new local entry took that path (rename and recreate, D-056).
	recreated map[string]string
	// identified maps a local path to the key of the synced file found there
	// by its identity (movedOntoAnotherItemsPath): the move here SQLite did
	// not pair, as the path's baseline names another item.
	identified map[string]string
	// filesByContent locates each file of the snapshots by the key a delete
	// of its synced content carries (contentKey): a file here by its local
	// identity, an item on OneDrive by its ID (withholdDeletesOfUncarriedContent).
	filesByContent map[contentKey]string
}

func moveCompositionInputs(
	comparisons []DecidedComparison,
	views map[string]*syncdomain.PathView,
	remoteRows []syncdomain.RemoteStateRow,
	localRows []syncdomain.LocalStateRow,
) moveInputs {
	cmpByPath := make(map[string]*DecidedComparison, len(comparisons))
	for i := range comparisons {
		cmpByPath[comparisons[i].Path] = &comparisons[i]
	}
	remoteByID, occupancy := indexMoveSnapshot(remoteRows, localRows)

	var nodes []moveNode
	frozen := map[string]bool{}
	recreated := map[string]string{}
	identified := map[string]string{}
	for i := range comparisons {
		cmp := &comparisons[i]
		view := views[cmp.Path]
		if !cmp.BaselineType.Present() || view == nil || view.Baseline == nil || view.Baseline.ItemID == "" {
			continue
		}
		entry := view.Baseline
		node := moveNode{key: entry.ItemID, itemType: entry.ItemType}
		node.paths[moveCoordBaseline] = cmp.Path
		node.paths[moveCoordLocal] = composedLocalPosition(cmp, view)
		if node.paths[moveCoordLocal] == "" {
			if at, ok := movedOntoAnotherItemsPath(cmp, entry, localRows, cmpByPath); ok {
				node.paths[moveCoordLocal] = at
				identified[at] = node.key
			} else if at, ok := folderMovedOntoAPathItsItemLeft(cmp, entry, localRows, cmpByPath); ok {
				node.paths[moveCoordLocal] = at
				identified[at] = node.key
			} else if at, ok := editedWhereOneDriveMovedIt(cmp, entry, localRows, remoteByID); ok {
				node.paths[moveCoordLocal] = at
				identified[at] = node.key
			}
		}
		node.paths[moveCoordRemote] = composedRemotePosition(cmp, entry, remoteByID, cmpByPath, node.paths[moveCoordLocal])
		node.displace = composedDisplacement(&node, entry, view, remoteByID)
		if moveNodeTouchesUnavailableTruth(&node, views) {
			frozen[node.key] = true
		}
		if cmp.ComparisonKind == ComparisonLocalMoveSource && cmp.LocalType.Present() && cmp.LocalMoveTarget != "" {
			recreated[node.key] = cmp.Path
		}
		nodes = append(nodes, node)
	}

	return moveInputs{
		nodes: nodes, occupancy: occupancy, frozen: frozen, recreated: recreated, identified: identified,
		filesByContent: filesByContent(remoteRows, localRows),
	}
}

// movedOntoAnotherItemsPath finds where this computer moved a synced file
// that SQLite left unpaired: gone from its baseline path on both sides (cmp,
// the row of that path), and here at a path whose baseline names another
// item, freed by that item's move or delete (`mv p q, mv z p`,
// `rm p, mv z p`). SQLite pairs a local move only onto a path no synced item
// had, so it reconciled the path as that item replaced and the file as new
// content there, while OneDrive's own move of the file elsewhere was a new
// file too: the file was downloaded at OneDrive's path and uploaded at this
// computer's. The file's filesystem identity and its synced content prove
// it is the item, so its moves compose. Only a file whose identity both
// sides know and whose hash is the synced one: an edited file, or a new one
// on a reused inode, stays new content.
func movedOntoAnotherItemsPath(
	cmp *DecidedComparison, entry *syncdomain.BaselineEntry,
	localRows []syncdomain.LocalStateRow, cmpByPath map[string]*DecidedComparison,
) (string, bool) {
	if entry.ItemType != syncdomain.ItemTypeFile || cmp.LocalType.Present() {
		return "", false
	}
	// OneDrive still holding the file where it was synced, the file pairs
	// only onto the path of a synced item this computer moved away
	// (pathOfAnItemMovedAwayHere).
	keptOnOneDrive := cmp.RemoteType.Present()
	if keptOnOneDrive && cmp.RemoteItemID != entry.ItemID {
		return "", false
	}
	want, known := entry.FileIdentity()
	if !known {
		return "", false
	}
	for i := range localRows {
		row := &localRows[i]
		if row.ItemType != syncdomain.ItemTypeFile {
			continue
		}
		have, ok := row.FileIdentity()
		if !ok || !synctree.SameIdentity(have, want) {
			continue
		}
		if row.Hash == "" || row.Hash != entry.LocalHash {
			// A reused inode, or the file edited too: not proven the item,
			// so it stays new content and nothing of OneDrive's is
			// overwritten by it.
			return "", false
		}
		return row.Path, takesAnotherItemsPath(cmpByPath[row.Path], entry, keptOnOneDrive)
	}

	return "", false
}

// takesAnotherItemsPath reports whether at, the comparison row of the path
// where this computer holds the synced file of entry, is another synced
// item's path the file may pair onto: any while OneDrive no longer holds the
// file where it was synced, otherwise only one whose item this computer
// moved away (pathOfAnItemMovedAwayHere).
func takesAnotherItemsPath(at *DecidedComparison, entry *syncdomain.BaselineEntry, keptOnOneDrive bool) bool {
	return at != nil && at.BaselineType.Present() && at.BaselineItemID != entry.ItemID &&
		(!keptOnOneDrive || pathOfAnItemMovedAwayHere(at))
}

// pathOfAnItemMovedAwayHere reports whether at, the comparison row of the
// path a synced file was moved onto here, is that of another synced item
// this computer moved away, which OneDrive keeps there: here p moved into x
// and z onto p, while OneDrive moved x into p. The file pairs as a move onto
// p, so its claim on p meets p's, whose move here a cycle undoes, and the
// collision undoes the file's move; read as new content at p, the file was
// uploaded beside OneDrive's z while p waited for good to come back to its
// path.
func pathOfAnItemMovedAwayHere(at *DecidedComparison) bool {
	return at.ComparisonKind == ComparisonLocalMoveSource && at.LocalItemReplaced &&
		at.RemoteItemID == at.BaselineItemID
}

// editedWhereOneDriveMovedIt finds a synced file this computer moved and
// edited, gone from its baseline path here (cmp, the row of that path), at
// the path where OneDrive moved the item too: the local file there has the
// item's filesystem identity and OneDrive's item is at that very path. An
// edit leaves only the identity to prove a move here (R-2.17.1), which alone
// never pairs one; OneDrive's own move of the item to the same path
// corroborates it. Unpaired, the path read as two files made on both sides:
// OneDrive's version was downloaded beside the edited file, which went up
// as a new file, so the item was split in two (from the held start, f moved
// here into the new x and edited, while OneDrive moved it into the old x).
// Paired, the moves compose and the edit is carried as a remote move of an
// edited file is (R-2.17.7): the next pass uploads it as the same item, and
// OneDrive keeps the version it replaces.
func editedWhereOneDriveMovedIt(
	cmp *DecidedComparison, entry *syncdomain.BaselineEntry,
	localRows []syncdomain.LocalStateRow, remoteByID map[string]*syncdomain.RemoteStateRow,
) (string, bool) {
	if entry.ItemType != syncdomain.ItemTypeFile || cmp.LocalType.Present() {
		return "", false
	}
	moved, online := remoteByID[entry.ItemID]
	if !online || moved.ItemType != syncdomain.ItemTypeFile || moved.Path == cmp.Path {
		return "", false
	}
	want, known := entry.FileIdentity()
	if !known {
		return "", false
	}
	for i := range localRows {
		row := &localRows[i]
		if row.Path != moved.Path || row.ItemType != syncdomain.ItemTypeFile {
			continue
		}
		have, ok := row.FileIdentity()

		return row.Path, ok && synctree.SameIdentity(have, want)
	}

	return "", false
}

// folderMovedOntoAPathItsItemLeft finds where this computer moved a synced
// folder that SQLite left unpaired: gone from its baseline path here (cmp,
// the row of that path), and here at a path whose own synced item this
// computer moved away (`mv x p/x, mv p x`). SQLite pairs a local move only
// onto a path no synced item had, so it read the folder as gone, deleted it
// on OneDrive, and the entries the user kept in it went up again as new
// content beside OneDrive's copies. Folders pair on identity alone
// (R-2.17.1), so the identity proves it is the item and its moves compose.
func folderMovedOntoAPathItsItemLeft(
	cmp *DecidedComparison, entry *syncdomain.BaselineEntry,
	localRows []syncdomain.LocalStateRow, cmpByPath map[string]*DecidedComparison,
) (string, bool) {
	if entry.ItemType != syncdomain.ItemTypeFolder || cmp.LocalType.Present() {
		return "", false
	}
	want, known := entry.FileIdentity()
	if !known {
		return "", false
	}
	for i := range localRows {
		row := &localRows[i]
		if row.ItemType != syncdomain.ItemTypeFolder {
			continue
		}
		have, ok := row.FileIdentity()
		if !ok || !synctree.SameIdentity(have, want) {
			continue
		}
		if displacedIntoIt(cmpByPath[row.Path], entry, row.Path) &&
			!losesAnEntryHere(cmp.Path, cmpByPath) && !losesAnEntryHere(row.Path, cmpByPath) {
			return row.Path, true
		}
		return "", false
	}

	return "", false
}

// displacedIntoIt reports whether at, the row of the path p where this
// computer holds the folder of entry, is another synced folder's that this
// computer moved below p, into the folder that took its path.
func displacedIntoIt(at *DecidedComparison, entry *syncdomain.BaselineEntry, p string) bool {
	return at != nil && at.BaselineType == syncdomain.SideFolder && at.BaselineItemID != entry.ItemID && at.LocalItemReplaced &&
		at.ComparisonKind == ComparisonLocalMoveSource && strings.HasPrefix(at.LocalMoveTarget, p+"/")
}

// losesAnEntryHere reports whether a synced entry below folder (the moved
// folder's baseline path, or that of the folder it displaced) is gone here
// without a move SQLite paired (an entry edited as it moved with the folder,
// or deleted): composed as the folder's move, the delete SQLite plans for it
// on OneDrive, addressed below the folder's old path, would wait for the
// folder's move while the move waited for it. Such a folder is left as
// SQLite reads it, and the rotation is then taken apart by parking.
func losesAnEntryHere(folder string, cmpByPath map[string]*DecidedComparison) bool {
	for p, row := range cmpByPath {
		if strings.HasPrefix(p, folder+"/") && row.BaselineType.Present() && !row.LocalType.Present() &&
			row.ComparisonKind != ComparisonLocalMoveSource {
			return true
		}
	}

	return false
}

func indexMoveSnapshot(
	remoteRows []syncdomain.RemoteStateRow, localRows []syncdomain.LocalStateRow,
) (map[string]*syncdomain.RemoteStateRow, moveOccupancy) {
	remoteByID := make(map[string]*syncdomain.RemoteStateRow, len(remoteRows))
	occupancy := moveOccupancy{
		local:        make(map[string]bool, len(localRows)),
		remote:       make(map[string]bool, len(remoteRows)),
		remoteFiles:  make(map[string]bool, len(remoteRows)),
		localFiles:   make(map[string]bool, len(localRows)),
		localFolders: make(map[string]bool, len(localRows)),
	}
	for i := range remoteRows {
		remoteByID[remoteRows[i].ItemID] = &remoteRows[i]
		occupancy.remote[remoteRows[i].Path] = true
		if remoteRows[i].ItemType != syncdomain.ItemTypeFolder {
			occupancy.remoteFiles[remoteRows[i].Path] = true
		}
	}
	for i := range localRows {
		occupancy.local[localRows[i].Path] = true
		if localRows[i].ItemType == syncdomain.ItemTypeFile {
			occupancy.localFiles[localRows[i].Path] = true
		}
		if localRows[i].ItemType == syncdomain.ItemTypeFolder {
			occupancy.localFolders[localRows[i].Path] = true
		}
	}

	return remoteByID, occupancy
}

func moveNodeTouchesUnavailableTruth(node *moveNode, views map[string]*syncdomain.PathView) bool {
	for coord := range moveCoordCount {
		if p := node.paths[coord]; p != "" && views[p] != nil && plannerSuppressesUnavailableTruth(&views[p].TruthStatus) {
			return true
		}
	}

	return false
}

// composedDisplacement decides whether the entry at the baseline path of a
// synced file may give way to another item moved onto that path. The file's
// own local copy is replaced when it is provably the synced version and
// renamed to a conflict copy when it was edited, both only once OneDrive no
// longer has the item. A new file that took the path (the entry there is not
// the baseline item: the node has no local position, or the synced copy moved
// elsewhere here) is renamed to a conflict copy too, whatever OneDrive holds:
// it is kept and uploaded later as a new file, and the move still lands, so
// the moved item is neither downloaded beside its old copy nor uploaded
// again. Never otherwise: a folder at either end, unknown content, or the
// item's own copy while it still exists online somewhere. When the synced
// copy moved elsewhere here, the displacement is the new entry's only: the
// node's own copy, at its new path, never gives way (localDisplacementAt).
func composedDisplacement(
	node *moveNode, entry *syncdomain.BaselineEntry, view *syncdomain.PathView, remoteByID map[string]*syncdomain.RemoteStateRow,
) moveDisplacement {
	if entry.ItemType != syncdomain.ItemTypeFile || view.Local == nil {
		return moveDisplaceNever
	}
	if movedAwayHere(node) {
		// The synced copy left the path here and a new file took it: that
		// file is not the item, whatever OneDrive holds, so it is kept as a
		// conflict copy. Waiting for it to leave left two moves waiting on
		// each other's destinations for good (chaos seeds 162 and 385).
		if view.Local.ItemType == syncdomain.ItemTypeFile {
			return moveDisplaceConflictCopy
		}
		return moveDisplaceNever
	}
	if _, stillOnline := remoteByID[entry.ItemID]; stillOnline {
		return moveDisplaceNever
	}
	if !node.present(moveCoordLocal) {
		if view.Local.ItemType == syncdomain.ItemTypeFile {
			return moveDisplaceConflictCopy
		}
		return moveDisplaceNever
	}
	if view.Local.Hash == "" || entry.LocalHash == "" {
		return moveDisplaceNever
	}
	if view.Local.Hash != entry.LocalHash {
		return moveDisplaceConflictCopy
	}

	return moveDisplaceReplace
}

func composedLocalPosition(cmp *DecidedComparison, view *syncdomain.PathView) string {
	if cmp.ComparisonKind == ComparisonLocalMoveSource && cmp.LocalMoveTarget != "" {
		return cmp.LocalMoveTarget
	}
	if view.Local != nil && localEntryIsBaselineItem(view.Local, view.Baseline) {
		return cmp.Path
	}

	return ""
}

// localEntryIsBaselineItem applies the source check a local move's freshness
// and executor make (LocalRowIsBaselineItem) to a planner view, so
// composition never plans a move of an entry they would refuse: a file
// replaced at the path by a different one (new identity, other content) is a
// new local entry, and its move was superseded on every pass.
func localEntryIsBaselineItem(local *syncdomain.LocalState, baseline *syncdomain.BaselineEntry) bool {
	return syncdomain.LocalRowIsBaselineItem(&syncdomain.LocalStateRow{
		ItemType:         local.ItemType,
		Hash:             local.Hash,
		Size:             local.Size,
		LocalDevice:      local.LocalDevice,
		LocalInode:       local.LocalInode,
		LocalBirthTime:   local.LocalBirthTime,
		LocalHasIdentity: local.LocalHasIdentity,
	}, baseline)
}

func composedRemotePosition(
	cmp *DecidedComparison,
	entry *syncdomain.BaselineEntry,
	remoteByID map[string]*syncdomain.RemoteStateRow,
	cmpByPath map[string]*DecidedComparison,
	localPosition string,
) string {
	row, ok := remoteByID[entry.ItemID]
	if !ok || row.ItemType != entry.ItemType {
		return ""
	}
	if row.Path == cmp.Path || row.Path == cmp.RemoteMoveTarget {
		return row.Path
	}
	at := cmpByPath[row.Path]
	if at == nil {
		return ""
	}
	switch {
	case at.ComparisonKind == ComparisonRemoteMoveShadowed:
		return row.Path
	// The move OneDrive made is shadowed by this item's local move, and a
	// new local file took its destination here (z moved into p here and to
	// g on OneDrive, a new g made here): the path is a create conflict, but
	// the item ID proves the item sits there, so the moves compose and the
	// new file is kept as a conflict copy (localDisplacementAt). Without it
	// the item was downloaded at the path beside its moved copy here. Only
	// a file meeting a new file: two folders at one path merge as a create
	// conflict of folders, and a folder composed so would undo that merge.
	// Only a shadowed move: one OneDrive made alone, SQLite reconciles as a
	// delete here at the item's path and a create conflict at the new one,
	// which no composed move takes back.
	case at.RemoteMoveShadowed && !at.BaselineType.Present() &&
		entry.ItemType == syncdomain.ItemTypeFile && at.LocalType == syncdomain.SideFile:
		return row.Path
	case sitsAtAnotherSyncedItemsPath(at, entry):
		return row.Path
	case row.Path == localPosition && heldHereWhereOneDriveHasIt(at, entry):
		return row.Path
	case unpairedRemoteMoveOf(cmp, at, entry, remoteByID):
		return row.Path
	default:
		return ""
	}
}

// heldHereWhereOneDriveHasIt reports whether at, the comparison row of the
// path where OneDrive holds the item of entry and this computer holds it
// too, places the item there: SQLite paired the local move onto the path,
// or the local position was found there by the item's identity
// (editedWhereOneDriveMovedIt).
func heldHereWhereOneDriveHasIt(at *DecidedComparison, entry *syncdomain.BaselineEntry) bool {
	return at.ComparisonKind == ComparisonLocalMoveDest ||
		(at.RemoteItemID == entry.ItemID && !at.BaselineType.Present())
}

// sitsAtAnotherSyncedItemsPath reports whether OneDrive holds the item of
// entry at the path of the comparison row at, a path that is another synced
// item's: the item ID proves this item now sits there. It does when
// OneDrive moved the path's own item away (a swap or a chain), which is
// composed too; without that a local rename of an item OneDrive swapped had
// no remote position and was copied, not moved. It does when the path's own
// item is gone here (deleted here, or moved away from a swap with this one)
// and no entry here holds the path; without that the item was downloaded at
// the path and its renamed copy here uploaded as a new file, one content at
// two paths, or, where OneDrive still held the path's own item elsewhere,
// the download waited as a removal of that item's content and sync never
// converged (chaos seed 55).
func sitsAtAnotherSyncedItemsPath(at *DecidedComparison, entry *syncdomain.BaselineEntry) bool {
	if at.RemoteItemID != entry.ItemID {
		return false
	}

	return at.ComparisonKind == ComparisonRemoteMoveSource ||
		(at.BaselineType.Present() && !at.LocalType.Present())
}

// movedOntoAPathItsItemLeftOnOneDrive reports whether OneDrive holds the
// item of entry at the path of the comparison row at, a path another synced
// item had that OneDrive moved elsewhere, which SQLite did not pair: x moved
// to q and p/f onto x on OneDrive, while here p was renamed q, so the two q
// folders merged as one created on both sides and x's move was never read
// as one. The item ID proves the item sits there, so its moves compose.
func movedOntoAPathItsItemLeftOnOneDrive(
	at *DecidedComparison, entry *syncdomain.BaselineEntry, remoteByID map[string]*syncdomain.RemoteStateRow,
) bool {
	if at.RemoteItemID != entry.ItemID || !at.BaselineType.Present() || at.BaselineItemID == entry.ItemID {
		return false
	}
	moved, online := remoteByID[at.BaselineItemID]

	return online && moved.Path != at.Path
}

// unpairedRemoteMoveOf reports OneDrive's move of entry onto the path whose
// comparison row is at, which SQLite did not pair with the entry's own row
// (cmp), while the item ID proves the item sits there:
//   - the path is another synced item's, which OneDrive deleted (a
//     replacing move, D-058), while this item also moved here, so its own
//     row is a local move and names no remote target: p renamed q here,
//     while OneDrive deleted x and moved p/f onto it, and x still stands
//     here (sitsAtAnotherSyncedItemsPath answers the path freed here);
//   - the item left its path on both sides and SQLite paired neither move
//     (movedOntoAnotherItemsPath): OneDrive's destination reads as a new
//     file;
//   - the path is another synced item's, which OneDrive moved elsewhere
//     without SQLite pairing that move (movedOntoAPathItsItemLeftOnOneDrive).
//
// Without it the item had no remote position and was downloaded at
// OneDrive's path while its copy here went back up as a new file.
func unpairedRemoteMoveOf(
	cmp, at *DecidedComparison, entry *syncdomain.BaselineEntry, remoteByID map[string]*syncdomain.RemoteStateRow,
) bool {
	if at.RemoteItemID != entry.ItemID {
		return false
	}
	if at.BaselineType.Present() && remoteByID[at.BaselineItemID] == nil {
		return true
	}
	if movedOntoAPathItsItemLeftOnOneDrive(at, entry, remoteByID) {
		return true
	}

	return at.ComparisonKind == ComparisonRemoteOnlyCreate && !cmp.LocalType.Present() && !cmp.RemoteType.Present()
}

// remoteToLocalFolderPrefixes maps each composed folder's remote path to where
// the same folder sits locally now, deepest first. Remote-origin local writes
// carry remote paths; rebased through this map they address the local tree as
// it is, and moves wait for writes under their source (buildDependencies).
func remoteToLocalFolderPrefixes(nodes []moveNode) []graphFolderMoveBoundary {
	var prefixes []graphFolderMoveBoundary
	for i := range nodes {
		n := &nodes[i]
		if n.itemType != syncdomain.ItemTypeFolder || !n.present(moveCoordLocal) || !n.present(moveCoordRemote) {
			continue
		}
		if n.paths[moveCoordLocal] == n.paths[moveCoordRemote] {
			continue
		}
		prefixes = append(prefixes, graphFolderMoveBoundary{
			oldPrefix: n.paths[moveCoordRemote] + "/",
			newPrefix: n.paths[moveCoordLocal] + "/",
		})
	}
	sort.Slice(prefixes, func(i, j int) bool {
		return len(prefixes[i].oldPrefix) > len(prefixes[j].oldPrefix)
	})

	return prefixes
}
