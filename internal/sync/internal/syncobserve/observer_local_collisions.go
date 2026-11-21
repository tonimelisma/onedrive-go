// The local observer's case-collision handling.
//
// Contents:
//   - readDirForCollisionCheck: the directory listing a collision check reads
//   - HasCaseCollisionCached: cached per-directory collision check
//   - DirNameCache maintenance (updateDirNameCache, removeDirNameCache, populateDirNameCache)
//   - buildPeerRelPath: collision peer path construction
//   - onDiskSiblingName: a stored name's spelling on disk, for re-emission
//   - reportCaseCollisionSkip: the debug event of a skipped colliding entry
//   - collision peers (AddCollisionPeer, removeCollisionPeersFor)
//
// Related files:
//   - observer_local_handlers.go: event handlers that call these methods
//   - scanner.go:                 FullScan's DetectCaseCollisions (authoritative)
//   - dir_names.go:               DirNames, the one collision rule both use

package syncobserve

import (
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// readDirForCollisionCheck lists dirPath through tree when there is one. A
// directory the tree refuses, such as one reached through a followed symlink
// (R-2.4.6) whose target lies outside the sync root, is listed through
// localpath, as observation reads followed paths: refusing it left its new
// files unchecked for collisions in watch mode. The listing is read-only, so
// the fallback cannot write outside the root.
//
// A listing that fails both ways makes the caller report no collision (fail
// open), as it always has: the event's own handling meets the same failure,
// and the full scan's DetectCaseCollisions, which is authoritative, judges
// the directory again.
func readDirForCollisionCheck(tree *synctree.Root, dirPath string) ([]os.DirEntry, error) {
	if tree != nil {
		if entries, err := tree.ReadDirAbs(dirPath); err == nil {
			return entries, nil
		}
	}

	return localpath.ReadDir(dirPath) //nolint:wrapcheck // localpath names the path
}

// buildPeerRelPath constructs the db-relative path for a collision peer
// given the current file's dbRelPath and the colliding file's name.
func (o *LocalObserver) buildPeerRelPath(dbRelPath, collidingName string) string {
	dir := filepath.Dir(dbRelPath)
	if dir == "." {
		return collidingName
	}

	return dir + "/" + collidingName
}

// onDiskSiblingName returns the name under which the entry stored as name
// (NFC) sits in dirPath on disk, which differs on a byte-exact filesystem
// holding an NFD name. It reads the directory name cache, listing the
// directory on a miss, and returns name itself when no single entry matches:
// the caller's stat then finds nothing, or finds the entry spelled as given.
func (o *LocalObserver) onDiskSiblingName(tree *synctree.Root, dirPath, name string) string {
	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		entries, err := readDirForCollisionCheck(tree, dirPath)
		if err != nil {
			return name
		}
		o.populateDirNameCache(dirPath, entries)
		cache = o.DirNameCache[dirPath]
	}

	spellings := cache.spellings(name)
	match := namekey.FindStored(spellings, name, func(spelling *string) string { return *spelling })
	if !match.Found() {
		return name
	}

	return spellings[match.Index]
}

// HasCaseCollisionCached checks whether name (its stored form) collides with
// a sibling on disk, using the per-directory name cache. A cache miss lists
// the directory through the sync tree, so it sees the filesystem every other
// sync-tree operation sees (D-094); without a tree (a caller outside the
// engine) it lists the path directly. The rule is the full scan's
// (DetectCaseCollisions, DirNames.rivals): only on-disk rivals count, so a
// synced name that is gone from disk (the old name of a case-only rename)
// is not one. Single-goroutine (watchLoop) access — no mutex needed.
func (o *LocalObserver) HasCaseCollisionCached(tree *synctree.Root, dirPath, name string) (string, bool) {
	if o.DirNameCache == nil {
		o.DirNameCache = make(map[string]DirNames)
	}

	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		entries, err := readDirForCollisionCheck(tree, dirPath)
		if err != nil {
			return "", false
		}

		cache = dirNamesOf(entries)
		o.DirNameCache[dirPath] = cache
	}

	others, sameForm := cache.rivals(name)
	if len(others) > 0 {
		return others[0], true
	}
	if sameForm {
		return name, true
	}

	return "", false
}

// reportCaseCollisionSkip tells the debug event sink, when one is installed,
// that the entry at dbRelPath was skipped because its name collides with
// collidingName's (R-2.12.2). It is how a test learns that the observer
// handled the entry, instead of waiting out a window with no event.
func (o *LocalObserver) reportCaseCollisionSkip(dbRelPath, collidingName string) {
	if o.debugEvents == nil {
		return
	}

	o.debugEvents(syncdomain.EngineDebugEvent{
		Type:     syncdomain.EngineDebugEventLocalCaseCollisionSkipped,
		Observer: syncdomain.EngineDebugObserverLocal,
		Path:     dbRelPath,
		Note:     collidingName,
	})
}

// updateDirNameCache adds a name to the cache for the given directory.
// Called after successfully processing a Create event.
func (o *LocalObserver) updateDirNameCache(dirPath, name string) {
	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		return // not cached yet — will be populated lazily on next check
	}

	cache.addUnlessStored(name)
}

// removeDirNameCache removes a name from the cache for the given directory.
// Called after processing a Delete event.
func (o *LocalObserver) removeDirNameCache(dirPath, name string) {
	if cache, ok := o.DirNameCache[dirPath]; ok {
		cache.remove(name)
	}
}

// populateDirNameCache pre-populates the directory name cache from an already-
// read set of directory entries. Called by scanNewDirectory after os.ReadDir
// to avoid a redundant filesystem read in HasCaseCollisionCached.
func (o *LocalObserver) populateDirNameCache(dirPath string, entries []os.DirEntry) {
	if o.DirNameCache == nil {
		o.DirNameCache = make(map[string]DirNames)
	}

	o.DirNameCache[dirPath] = dirNamesOf(entries)
}

// AddCollisionPeer records a bidirectional collision relationship between
// two paths. Creates inner sets lazily. Idempotent — safe to call multiple
// times for the same pair. Single-goroutine (watchLoop) access.
func (o *LocalObserver) AddCollisionPeer(a, b string) {
	if o.CollisionPeers == nil {
		return
	}

	if o.CollisionPeers[a] == nil {
		o.CollisionPeers[a] = make(map[string]struct{})
	}

	o.CollisionPeers[a][b] = struct{}{}

	if o.CollisionPeers[b] == nil {
		o.CollisionPeers[b] = make(map[string]struct{})
	}

	o.CollisionPeers[b][a] = struct{}{}
}

// removeCollisionPeersFor removes dbRelPath from the peer map and from all
// peers' sets. Returns the set of former peers (for re-emission via
// handleCreate). Returns nil if no peers existed.
func (o *LocalObserver) removeCollisionPeersFor(dbRelPath string) map[string]struct{} {
	if o.CollisionPeers == nil {
		return nil
	}

	peers, ok := o.CollisionPeers[dbRelPath]
	if !ok {
		return nil
	}

	delete(o.CollisionPeers, dbRelPath)

	for peerPath := range peers {
		if peerSet, peerOk := o.CollisionPeers[peerPath]; peerOk {
			delete(peerSet, dbRelPath)

			if len(peerSet) == 0 {
				delete(o.CollisionPeers, peerPath)
			}
		}
	}

	return peers
}
