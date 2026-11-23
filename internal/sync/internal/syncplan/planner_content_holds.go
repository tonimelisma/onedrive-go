package syncplan

import (
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// filesByContent locates the files of the snapshots by the key a removal of
// synced content carries (deletedContentKey): a file here by its local
// identity and hash, an item on OneDrive by its ID. A file the other side already
// holds at the same path with the same content is left out: its content is
// on both sides, so no removal elsewhere can leave a side without it.
func filesByContent(remoteRows []syncdomain.RemoteStateRow, localRows []syncdomain.LocalStateRow) map[contentKey]string {
	localHashes := make(map[string]string, len(localRows))
	for i := range localRows {
		if row := &localRows[i]; row.ItemType == syncdomain.ItemTypeFile {
			localHashes[row.Path] = row.Hash
		}
	}
	remoteHashes := make(map[string]string, len(remoteRows))
	located := make(map[contentKey]string, len(remoteRows)+len(localRows))
	for i := range remoteRows {
		row := &remoteRows[i]
		if row.ItemType != syncdomain.ItemTypeFile {
			continue
		}
		remoteHashes[row.Path] = row.Hash
		if hash, here := localHashes[row.Path]; row.ItemID == "" || (here && sameKnownHash(hash, row.Hash)) {
			continue
		}
		located[contentKey{itemID: row.ItemID}] = row.Path
	}
	for i := range localRows {
		row := &localRows[i]
		if row.ItemType != syncdomain.ItemTypeFile {
			continue
		}
		key, known := localContentKey(row.LocalHasIdentity, row.LocalDevice, row.LocalInode, row.Hash)
		if !known {
			continue
		}
		if hash, online := remoteHashes[row.Path]; online && sameKnownHash(hash, row.Hash) {
			continue
		}
		located[key] = row.Path
	}

	return located
}

// sameKnownHash reports whether two content hashes are known and equal.
func sameKnownHash(a, b string) bool {
	return a != "" && a == b
}

// deletesOfUncarriedContent lists the removals of synced content (a delete of
// a synced file, a download of another item over it) whose file the other
// side still holds at another path, while no create of the plan writes that
// content onto the removal's side (carriesContentOf). The other side moved
// the file, and the move was not paired: SQLite could not pair it, or the
// comparison put the moved file beside an entry of another item, as a file
// renamed here into a new folder that takes the name of a folder OneDrive
// keeps elsewhere here. Run, the removal left its side without the content
// until a later pass carried it; held, it waits as a delete waits for held
// work, and the pass that plans the create releases it, ordered after the
// create by its content edge (addContentCarrierDeps). The removals the
// mode does not run are left to the mode partition. It logs each at Debug.
func deletesOfUncarriedContent(
	logger *slog.Logger, actions []syncdomain.Action, located map[contentKey]string, mode syncdomain.SyncMode,
) map[int]bool {
	if len(located) == 0 {
		return nil
	}
	roots := map[int]bool{}
	for i := range actions {
		removal := &actions[i]
		key, removes := deletedContentKey(removal)
		if !removes || !actionAllowedInMode(removal, mode) {
			continue
		}
		elsewhere, held := located[key]
		if !held || elsewhere == syncdomain.ActionViewPath(removal) || contentCarried(actions, removal) {
			continue
		}
		roots[i] = true
		if logger != nil {
			logger.Debug("delete waits for the create that carries its content",
				slog.String("action", removal.Type.String()),
				slog.String("path", removal.Path),
				slog.String("content_path", elsewhere))
		}
	}

	return roots
}

// contentCarried reports whether a create of actions writes the content the
// removal removes at another path on the removal's side.
func contentCarried(actions []syncdomain.Action, removal *syncdomain.Action) bool {
	side := contentRemovalSide(removal)
	for i := range actions {
		if createsOnSide(&actions[i], side) && carriesContentOf(&actions[i], removal) {
			return true
		}
	}

	return false
}
