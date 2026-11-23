package syncplan

import (
	"log/slog"
	"path"
	"strconv"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// setAsideRemovedContent keeps on OneDrive the content of a synced file whose
// delete the upload of that content waits for. The upload of the file here
// that has the synced file's local identity and hash needs a path the delete
// frees, directly (the file moved up onto the path of its folder, which
// OneDrive deletes) or through what waits for it (a folder made at the
// deleted file's path, which a move out of the upload's path waits for), so
// addContentCarrierDeps leaves the delete's content edge out, and the delete
// ran first: with the upload, or what it waits for, refused, OneDrive was
// without the content until a later pass uploaded it again. Such a delete is
// now a remote move of the item to a free temporary name (asidePath) in the
// nearest folder above it that no delete of the plan removes: it frees the
// path as the delete did, and what waited for the delete waits for the move
// (a folder delete for the moves out of it, a create at a path for the move
// that vacates it). The next pass deletes the parked item once the upload
// holds its content, or moves it back to where the file is here when the
// upload failed, since its baseline row, which the move re-paths, keeps the
// file's local identity.
//
// The move's local truth is the file here the upload reads (MovedLocal at
// MovedLocalPath): the worker checks that file is still the one planned, as
// for any move of an item whose local copy sits elsewhere in the snapshot.
// Only remote deletes are set aside, and only where the mode runs both the
// delete and the upload. It logs each at Debug.
func setAsideRemovedContent(
	logger *slog.Logger, actions []syncdomain.Action, inputs *moveInputs, mode syncdomain.SyncMode,
) []syncdomain.Action {
	if !deletesContentACreateWrites(actions) {
		return actions
	}
	_, leftOut := buildDependencyGraph(actions)
	if len(leftOut) == 0 {
		return actions
	}

	taken := asideTakenPaths(actions, inputs)
	removed := remoteFolderDeletePaths(actions)
	setAside := map[int]bool{}
	for _, edge := range leftOut {
		deleted, create := &actions[edge.deleteIdx], &actions[edge.createIdx]
		if setAside[edge.deleteIdx] || !setsAsideForUpload(deleted, create, mode) {
			continue
		}
		aside := asidePath(deleted.Path, removed, taken)
		if aside == "" {
			continue
		}
		if logger != nil {
			logger.Debug("synced file moved aside on OneDrive until the upload of its content runs",
				slog.String("path", deleted.Path),
				slog.String("aside_path", aside),
				slog.String("upload_path", create.Path))
		}
		taken[namekey.OfPath(aside)] = true
		setAside[edge.deleteIdx] = true
		actions[edge.deleteIdx] = asideMove(deleted, create, aside)
	}

	return actions
}

// setsAsideForUpload reports whether the remote delete deleted is set aside
// for create, the upload of its content here: a delete of a OneDrive item the
// plan knows by ID, with both actions run by the mode.
func setsAsideForUpload(deleted, create *syncdomain.Action, mode syncdomain.SyncMode) bool {
	return deleted.Type == syncdomain.ActionRemoteDelete && create.Type == syncdomain.ActionUpload &&
		deleted.ItemID != "" && deleted.View != nil && deleted.View.Remote != nil &&
		create.View != nil && create.View.Local != nil &&
		actionAllowedInMode(deleted, mode) && actionAllowedInMode(create, mode)
}

// asideMove is the remote move that sets the item deleted removes aside at
// aside, its local truth the file here create uploads.
func asideMove(deleted, create *syncdomain.Action, aside string) syncdomain.Action {
	move := *deleted
	move.Type = syncdomain.ActionRemoteMove
	move.OldPath = deleted.Path
	move.Path = aside
	local := *create.View.Local
	move.MovedLocal = &local
	move.MovedLocalPath = syncdomain.ActionViewPath(create)

	return move
}

// remoteFolderDeletePaths lists the folders the plan deletes on OneDrive.
func remoteFolderDeletePaths(actions []syncdomain.Action) []string {
	var paths []string
	for i := range actions {
		if actions[i].Type == syncdomain.ActionRemoteDelete && isSubtreeDelete(&actions[i]) {
			paths = append(paths, actions[i].Path)
		}
	}

	return paths
}

// asideTakenPaths are the paths, as OneDrive compares names, that an aside
// name may not take: every path either side holds and every path an action
// of the plan names.
func asideTakenPaths(actions []syncdomain.Action, inputs *moveInputs) map[namekey.PathKey]bool {
	taken := make(map[namekey.PathKey]bool, len(inputs.occupancy.local)+len(inputs.occupancy.remote)+len(actions))
	for _, occupied := range []map[string]bool{inputs.occupancy.local, inputs.occupancy.remote} {
		for p := range occupied {
			taken[namekey.OfPath(p)] = true
		}
	}
	for i := range actions {
		taken[namekey.OfPath(actions[i].Path)] = true
		if actions[i].OldPath != "" {
			taken[namekey.OfPath(actions[i].OldPath)] = true
		}
	}

	return taken
}

// asidePath is a free temporary name for the entry at p, named as a parked
// move names one (MoveSwapInfix), in the nearest folder above p that no
// folder delete of removed takes away with it: parked inside such a folder,
// the item would go with the folder. It is "" when no free name is found.
func asidePath(p string, removed []string, taken map[namekey.PathKey]bool) string {
	dir := path.Dir(p)
	for dir != "." && removedWith(dir, removed) {
		dir = path.Dir(dir)
	}
	if dir == "." {
		dir = ""
	}
	name := path.Base(p)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= len(taken)+1; i++ {
		suffix := MoveSwapInfix + strconv.Itoa(i) + ext
		candidate := joinMovePath(dir, truncateNameBytes(stem, moveNameMaxBytes-len(suffix))+suffix)
		if !taken[namekey.OfPath(candidate)] {
			return candidate
		}
	}

	return ""
}

// removedWith reports whether dir is at or below one of the removed folders.
func removedWith(dir string, removed []string) bool {
	for _, folder := range removed {
		if pathAtOrBelow(dir, folder) {
			return true
		}
	}

	return false
}
