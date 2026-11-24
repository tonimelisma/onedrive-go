package syncplan

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// buildComposedMoveActions turns composed moves and re-keys into actions. It
// also returns the creates that wait with a move held for a later pass
// (recreatedSourceCreates).
func buildComposedMoveActions(
	composition *moveComposition,
	inputs *moveInputs,
	views map[string]*syncdomain.PathView,
	baseline syncdomain.BaselineView,
) (actions, waiting []syncdomain.Action) {
	nodes := inputs.nodes
	nodeByKey := make(map[string]*moveNode, len(nodes))
	for i := range nodes {
		nodeByKey[nodes[i].key] = &nodes[i]
	}
	viewAt := func(p string) *syncdomain.PathView {
		if v := views[p]; v != nil {
			return v
		}
		return &syncdomain.PathView{Path: p}
	}

	for i := range composition.moves {
		move := &composition.moves[i]
		node := nodeByKey[move.key]
		entry, ok := baseline.GetByID(move.key)
		if node == nil || !ok {
			continue
		}
		remote := viewAt(node.paths[moveCoordRemote]).Remote
		if move.side == moveCoordLocal {
			actions = append(actions, composedLocalMoveActions(move, node, &entry, remote, viewAt)...)
			continue
		}
		actions = append(actions, composedRemoteMoveActions(move, &entry, remote, viewAt)...)
	}

	creates, waiting := recreatedSourceCreates(composition, inputs, nodeByKey, viewAt)
	actions = append(actions, creates...)
	for _, aside := range composition.asideFolders {
		copyView := *viewAt(aside)
		actions = append(actions, makeConflictCopyAction(&copyView))
	}

	for i := range composition.merges {
		actions = append(actions, composedMergeActions(&composition.merges[i], nodeByKey, baseline, viewAt)...)
	}

	for i := range composition.rekeys {
		rekey := &composition.rekeys[i]
		node := nodeByKey[rekey.key]
		entry, ok := baseline.GetByID(rekey.key)
		if node == nil || !ok {
			continue
		}
		view := &syncdomain.PathView{
			Path:        rekey.newPath,
			Remote:      viewAt(node.paths[moveCoordRemote]).Remote,
			Local:       viewAt(node.paths[moveCoordLocal]).Local,
			Baseline:    &entry,
			TruthStatus: viewAt(rekey.newPath).TruthStatus,
		}
		action := makeAction(syncdomain.ActionBaselineUpdate, view)
		action.OldPath = rekey.oldPath
		actions = append(actions, action)
	}

	return actions, waiting
}

// composedMergeActions builds a merge: the baseline update that records the
// item at the new folder's path with that folder's local identity, and the
// delete here of the item's own empty copy. Either commit order ends right:
// the delete removes the row at the copy's path alone, and the update keys
// the item's row by item ID.
func composedMergeActions(
	merge *composedMerge, nodeByKey map[string]*moveNode, baseline syncdomain.BaselineView,
	viewAt func(string) *syncdomain.PathView,
) []syncdomain.Action {
	node := nodeByKey[merge.key]
	entry, ok := baseline.GetByID(merge.key)
	if node == nil || !ok {
		return nil
	}
	bind := makeAction(syncdomain.ActionBaselineUpdate, &syncdomain.PathView{
		Path:        merge.newPath,
		Remote:      viewAt(node.paths[moveCoordRemote]).Remote,
		Local:       viewAt(merge.newPath).Local,
		Baseline:    &entry,
		TruthStatus: viewAt(merge.newPath).TruthStatus,
	})
	bind.OldPath = node.paths[moveCoordBaseline]
	remove := makeAction(syncdomain.ActionLocalDelete, &syncdomain.PathView{
		Path:        merge.localPath,
		Local:       viewAt(merge.localPath).Local,
		Baseline:    &entry,
		TruthStatus: viewAt(merge.localPath).TruthStatus,
	})

	return []syncdomain.Action{bind, remove}
}

// composedLocalMoveActions builds a remote-origin local move, preceded by the
// conflict copy of an edited file it displaces (buildDependencies orders the
// move after the copy at its destination), and followed by the download of
// OneDrive's edit of the item when the two compose (composedRemoteEditDownload).
func composedLocalMoveActions(
	move *composedMove, node *moveNode, entry *syncdomain.BaselineEntry, remote *syncdomain.RemoteState,
	viewAt func(string) *syncdomain.PathView,
) []syncdomain.Action {
	view := &syncdomain.PathView{
		Path:        move.newPath,
		Remote:      remote,
		Local:       viewAt(move.newPath).Local,
		Baseline:    entry,
		TruthStatus: viewAt(move.newPath).TruthStatus,
	}
	action := makeAction(syncdomain.ActionLocalMove, view)
	action.OldPath = move.oldPath
	action.MovedRemotePath = move.remotePath

	switch move.displaced {
	case moveDisplaceDeleteFirst:
		// The move waits for the delete at its destination
		// (moveOrderDependency) and lands where nothing is any more; its
		// view keeps the folder there as planned, which the worker's
		// freshness check reads in the snapshot.
	case moveDisplaceReplace:
		action.ReplaceLocalTarget = true
	case moveDisplaceConflictCopy:
		copyView := *viewAt(move.newPath)
		return []syncdomain.Action{makeConflictCopyAction(&copyView), action}
	case moveDisplaceEmptyFolder:
		// The delete names no synced item, so its commit records nothing;
		// the move, which waits for it (moveOrderDependency), records the
		// item where it lands. Its view keeps what OneDrive holds at the
		// path, which the worker's freshness check reads.
		at := viewAt(move.newPath)
		emptied := makeAction(syncdomain.ActionLocalDelete, &syncdomain.PathView{
			Path: move.newPath, Local: at.Local, Remote: at.Remote, TruthStatus: at.TruthStatus,
		})
		emptied.ItemID = ""
		emptied.DeletesUnsyncedEntry = true
		return []syncdomain.Action{emptied, action}
	case moveDisplaceNever:
		if download, ok := composedRemoteEditDownload(move, node, entry, remote, viewAt); ok {
			return []syncdomain.Action{action, download}
		}
	}

	return []syncdomain.Action{action}
}

// composedRemoteEditDownload is the download of the edit OneDrive made to an
// item it also moved, composed with the local move that follows it here: the
// move puts the copy here at OneDrive's place, and the download, which waits
// for it (work at a move's destination does), replaces it there with the
// edit. It is planned only when the copy here is proven to hold the synced
// content (its hash is the baseline's), so the download may replace nothing
// else, and only for a move from the item's own place in the local snapshot
// to the place OneDrive holds it, nothing displaced there: local_state does
// not follow the plan's moves, so freshness reads the copy at the move's
// source (MovedLocalPath). A copy edited here is moved alone, and the next
// pass meets the two edits as a content conflict at the moved path.
func composedRemoteEditDownload(
	move *composedMove, node *moveNode, entry *syncdomain.BaselineEntry, remote *syncdomain.RemoteState,
	viewAt func(string) *syncdomain.PathView,
) (syncdomain.Action, bool) {
	if node == nil || move.remotePath != "" || node.paths[moveCoordLocal] != move.oldPath {
		return syncdomain.Action{}, false
	}
	local := viewAt(move.oldPath).Local
	if local == nil || local.ItemType != syncdomain.ItemTypeFile || local.Hash == "" ||
		local.Hash != entry.LocalHash || !syncdomain.RemoteContentDiffersFromBaseline(remote, entry) {
		return syncdomain.Action{}, false
	}
	download := makeAction(syncdomain.ActionDownload, &syncdomain.PathView{
		Path:        move.newPath,
		Local:       local,
		Remote:      remote,
		Baseline:    entry,
		TruthStatus: viewAt(move.newPath).TruthStatus,
	})
	download.MovedLocalPath = move.oldPath

	return download, true
}

// composedRemoteMoveActions builds a local-origin remote move. A move whose
// content could not be proven unchanged (no hash), or of a file edited here
// that its birth time proves is the item (R-2.17.1), also uploads what now
// sits at the destination, but only when the item is already there locally
// (the upload reads local truth at its path) and OneDrive did not edit it
// too.
func composedRemoteMoveActions(
	move *composedMove, entry *syncdomain.BaselineEntry, remote *syncdomain.RemoteState, viewAt func(string) *syncdomain.PathView,
) []syncdomain.Action {
	movedLocal := viewAt(move.localPath).Local
	view := &syncdomain.PathView{
		Path:        move.oldPath,
		Remote:      remote,
		Local:       viewAt(move.oldPath).Local,
		Baseline:    entry,
		TruthStatus: viewAt(move.oldPath).TruthStatus,
	}
	action := makeAction(syncdomain.ActionRemoteMove, view)
	action.OldPath = move.oldPath
	action.Path = move.newPath
	action.MovedLocal = movedLocal
	if move.localPath != move.newPath {
		action.MovedLocalPath = move.localPath
	}
	actions := []syncdomain.Action{action}
	if move.localPath != move.newPath {
		return actions
	}

	transferView := &syncdomain.PathView{
		Path:        move.newPath,
		Local:       movedLocal,
		Remote:      remote,
		Baseline:    entry,
		TruthStatus: viewAt(move.newPath).TruthStatus,
	}
	localEdited := syncdomain.LocalFileDiffersFromBaseline(movedLocal, entry)
	remoteEdited := syncdomain.RemoteContentDiffersFromBaseline(remote, entry)
	switch {
	case localEdited && remoteEdited:
		// Both sides edited the item this computer moved (a file whose
		// birth time proves it is the item, R-2.17.1): the move goes up
		// alone, and the next pass meets the two edits at the moved path
		// as a content conflict that keeps both. An upload here would
		// replace OneDrive's edit.
	case localEdited:
		actions = append(actions, makeAction(syncdomain.ActionUpload, transferView))
	case remoteEdited:
		// OneDrive edited the item this computer renamed, and the copy
		// here still holds the synced content: the move and the edit
		// compose, and the edit comes down at the new name after the move
		// (the download replaces only the verified synced content). With
		// both sides edited the next pass meets them as a content conflict
		// at the moved path.
		actions = append(actions, makeAction(syncdomain.ActionDownload, transferView))
	}

	return actions
}

// recreatedSourceCreates plans the new local entry at a path the synced item
// was renamed away from (D-056). It is created once OneDrive no longer holds
// the moved item there: already moved elsewhere online, or moved by a remote
// move of this plan, which the create waits for (buildDependencies). While
// the move waits for a later pass, so does the create: uploading at the old
// path first would write a new version of the moved item instead. Such a
// create is returned as waiting, work held for a later pass
// (withholdDeletesOfHeldWork).
func recreatedSourceCreates(
	composition *moveComposition,
	inputs *moveInputs,
	nodeByKey map[string]*moveNode,
	viewAt func(string) *syncdomain.PathView,
) (creates, waiting []syncdomain.Action) {
	keys := make([]string, 0, len(inputs.recreated))
	for key := range inputs.recreated {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		source := inputs.recreated[key]
		node := nodeByKey[key]
		sourceView := viewAt(source)
		if node == nil || sourceView.Local == nil || plannerSuppressesUnavailableTruth(&sourceView.TruthStatus) {
			continue
		}
		if _, synced := inputs.identified[source]; synced {
			// The entry that took the path is a synced file moved there
			// (movedOntoAnotherItemsPath), whose own moves carry it.
			continue
		}
		createView := &syncdomain.PathView{Path: source, Local: sourceView.Local, TruthStatus: sourceView.TruthStatus}
		create := makeCreateUploadAction(createView)
		if sourceView.Local.ItemType == syncdomain.ItemTypeFolder {
			createView.Remote = remoteFolderOfAnotherItem(sourceView.Remote, key)
			create = makeFolderCreate(createView, syncdomain.CreateRemote)
		}
		vacated := recreatedSourceVacated(composition, key, node, source)
		if inputs.frozen[key] || (composition.deferred[key] && !movedAwayOnOneDrive(composition, key, source)) || !vacated {
			waiting = append(waiting, create)
			continue
		}
		creates = append(creates, create)
	}

	return creates, waiting
}

// remoteFolderOfAnotherItem is the folder OneDrive holds at a recreated
// source when it is not the moved item key: both sides moved the item away
// and each made a new folder at its path, which are one folder by path. The
// folder create made here carries it as its OneDrive truth, so the worker's
// freshness check reads that folder by item ID instead of superseding the
// create on every pass because the path is no longer free, and the executor
// adopts it by name (adoptExistingRemoteFolder). Whichever of the create and
// the moved item's move commits first, the baseline ends right: run first,
// the create's row replaces the moved item's at the path and the move
// records the item where it went; run second, the move carries the rows
// below the path with it, and the next pass re-keys the new folder's back.
// A file there, or the moved item itself, is no such folder.
func remoteFolderOfAnotherItem(remote *syncdomain.RemoteState, key string) *syncdomain.RemoteState {
	if remote == nil || remote.IsDeleted || remote.ItemType != syncdomain.ItemTypeFolder || remote.ItemID == key {
		return nil
	}

	return remote
}

// movedAwayOnOneDrive reports whether the plan moves key away from source on
// OneDrive: a deferred item parked there frees the path in this pass
// (parkedFirst), so the folder made here at it is made on OneDrive after the
// move, and the move that lands in it after that.
func movedAwayOnOneDrive(composition *moveComposition, key, source string) bool {
	for i := range composition.moves {
		move := &composition.moves[i]
		if move.key == key && move.side == moveCoordRemote && move.oldPath == source {
			return true
		}
	}

	return false
}

// displaceFoldersDeletedHere marks a synced folder OneDrive deleted as giving
// way to a move onto its path when the plan deletes it here (localDeletes,
// the paths SQLite reconciles as a local delete): the folder is unmoved
// here and gone online. Before, such a folder made the move wait, while the
// moved item was downloaded at the folder's path and its copy here uploaded
// again: OneDrive replacing a deleted folder by a moved file (`rm x, mv z
// x`) left the file twice on both sides.
func displaceFoldersDeletedHere(inputs *moveInputs, localDeletes map[string]bool) {
	for i := range inputs.nodes {
		n := &inputs.nodes[i]
		if n.itemType == syncdomain.ItemTypeFolder && n.displace == moveDisplaceNever && !n.present(moveCoordRemote) &&
			n.paths[moveCoordLocal] == n.paths[moveCoordBaseline] && localDeletes[n.paths[moveCoordBaseline]] {
			n.displace = moveDisplaceDeleteFirst
		}
	}
}

// giveWayFoldersReplacedOnOneDrive decides how a synced folder OneDrive
// deleted gives way to another synced item OneDrive moved onto its path
// (`rm p, mv x p`, `rm p, mv z p`), while this computer left the folder
// where it was. A folder that holds only what was synced, or less, is
// deleted here, and the move waits for that delete
// (displaceFoldersDeletedHere); a folder OneDrive replaced by a folder is
// reconciled as unchanged at its path, so the delete is planned here in
// place of the baseline update SQLite gives it, which would bind the path
// to the moved folder while the deleted one still stood there. A folder
// that holds anything else written here is renamed to a conflict copy
// first, with everything in it, and uploaded later as a new folder, so the
// move lands and nothing written here is lost. Before, the folder made the
// move wait for good, and a file moved onto its path was downloaded there
// while its copy here went back to OneDrive as a new file.
func giveWayFoldersReplacedOnOneDrive(
	inputs *moveInputs,
	reconciled []syncdomain.Action,
	comparisons []DecidedComparison,
	views map[string]*syncdomain.PathView,
	localRows []syncdomain.LocalStateRow,
) []syncdomain.Action {
	cmpByPath := make(map[string]*DecidedComparison, len(comparisons))
	for i := range comparisons {
		cmpByPath[comparisons[i].Path] = &comparisons[i]
	}
	synced := make(map[string]bool, len(inputs.nodes))
	for i := range inputs.nodes {
		synced[inputs.nodes[i].key] = true
	}
	deleted := localDeletePaths(reconciled)
	replace := map[string]bool{}
	for i := range inputs.nodes {
		n := &inputs.nodes[i]
		p := n.paths[moveCoordBaseline]
		cmp := cmpByPath[p]
		if !replacedOnOneDriveAndLeftHere(n, cmp, synced) {
			continue
		}
		switch {
		case !holdsOnlySyncedHere(p, localRows, views):
			n.displace = moveDisplaceConflictCopy
		case !deleted[p] && cmp.RemoteType == syncdomain.SideFolder:
			replace[p] = true
		}
	}
	if len(replace) == 0 {
		return reconciled
	}

	kept := reconciled[:0:0]
	for i := range reconciled {
		p := reconciled[i].Path
		if replace[p] && reconciled[i].Type == syncdomain.ActionBaselineUpdate {
			kept = append(kept, makeAction(syncdomain.ActionLocalDelete, views[p]))
			delete(replace, p)
			continue
		}
		kept = append(kept, reconciled[i])
	}
	for _, p := range slices.Sorted(maps.Keys(replace)) {
		kept = append(kept, makeAction(syncdomain.ActionLocalDelete, views[p]))
	}

	return kept
}

// replacedOnOneDriveAndLeftHere reports a synced folder OneDrive deleted,
// at its baseline path here, whose path OneDrive gives another synced item it
// moved there (cmp is the comparison row of that path, synced the keys of
// the synced items).
func replacedOnOneDriveAndLeftHere(n *moveNode, cmp *DecidedComparison, synced map[string]bool) bool {
	p := n.paths[moveCoordBaseline]
	return n.itemType == syncdomain.ItemTypeFolder && n.displace == moveDisplaceNever && !n.present(moveCoordRemote) &&
		n.paths[moveCoordLocal] == p && cmp != nil && cmp.RemoteType.Present() &&
		cmp.RemoteItemID != n.key && synced[cmp.RemoteItemID]
}

// holdsOnlySyncedHere reports whether every local entry below folder is a
// synced item at its baseline path, a file with its synced content: what a
// delete of the folder here would remove is all on record, and OneDrive's
// delete of the folder covers it. Content of unknown hash is not proven
// synced.
func holdsOnlySyncedHere(folder string, localRows []syncdomain.LocalStateRow, views map[string]*syncdomain.PathView) bool {
	for i := range localRows {
		row := &localRows[i]
		if !strings.HasPrefix(row.Path, folder+"/") {
			continue
		}
		view := views[row.Path]
		if view == nil || view.Baseline == nil || !syncdomain.LocalRowIsBaselineItem(row, view.Baseline) {
			return false
		}
		if row.ItemType == syncdomain.ItemTypeFile && (row.Hash == "" || row.Hash != view.Baseline.LocalHash) {
			return false
		}
	}

	return true
}

// recreatedSourceVacated reports whether OneDrive no longer holds key at
// source once the plan's moves have run.
func recreatedSourceVacated(composition *moveComposition, key string, node *moveNode, source string) bool {
	return node.paths[moveCoordRemote] != source || movedAwayOnOneDrive(composition, key, source)
}
