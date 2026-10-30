// Package sync owns the single mounted content-root runtime, including local observation
// collision handling.
//
// Contents:
//   - HasCaseCollisionCached: cached per-directory collision check
//   - dirNameCache maintenance (update/remove)
//   - buildPeerRelPath: collision peer path construction
//
// Related files:
//   - observer_local_handlers.go: event handlers that call these methods
//   - scanner.go:                 FullScan's DetectCaseCollisions (authoritative)
package sync

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
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
func (o *localObserver) buildPeerRelPath(dbRelPath, collidingName string) string {
	dir := filepath.Dir(dbRelPath)
	if dir == "." {
		return collidingName
	}

	return dir + "/" + collidingName
}

// HasCaseCollisionCached checks if name collides with an existing sibling
// using a per-directory name cache (filesystem) and the baseline (synced
// files). A cache miss lists the directory through the sync tree, so it sees
// the filesystem every other sync-tree operation sees (D-094); without a tree
// (a caller outside the engine) it lists the path directly. The dbDir
// parameter is the db-relative directory path used for baseline cross-check.
// Single-goroutine (watchLoop) access — no mutex needed.
func (o *localObserver) HasCaseCollisionCached(tree *synctree.Root, dirPath, name, dbDir string) (string, bool) {
	if o.DirNameCache == nil {
		o.DirNameCache = make(map[string]map[string][]string)
	}

	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		entries, err := readDirForCollisionCheck(tree, dirPath)
		if err != nil {
			return "", false
		}

		cache = make(map[string][]string, len(entries))

		for _, e := range entries {
			key := namekey.Key(e.Name())
			cache[key] = append(cache[key], e.Name())
		}

		o.DirNameCache[dirPath] = cache
	}

	// Cached names are on-disk spellings, which may be NFD; name is NFC. A
	// differently spelled sibling under the same namespace key is a
	// collision, and so is a second on-disk spelling of name itself (an NFC
	// and an NFD twin, one name on OneDrive).
	sameSpellings := 0
	for _, existing := range cache[namekey.Key(name)] {
		if nfcNormalize(existing) != name {
			return existing, true
		}
		sameSpellings++
		if sameSpellings > 1 {
			return existing, true
		}
	}

	// Cross-check baseline for files that exist remotely but not locally.
	// A file synced to OneDrive but unchanged on disk produces no event
	// and may not appear in the filesystem cache (e.g., remote-only items).
	variants := o.baseline.GetCaseVariants(dbDir, name)
	for _, v := range variants {
		baseName := filepath.Base(v.Path)
		if baseName == name {
			continue // same casing — not a collision
		}

		// Skip variants that were recently deleted locally. The baseline
		// hasn't been updated yet (async worker), but the deletion event
		// is already in the pipeline. Without this guard, case-only
		// renames (File.txt → file.txt) would false-positive.
		if _, recentlyDeleted := o.RecentLocalDeletes[v.Path]; recentlyDeleted {
			continue
		}

		return baseName, true
	}

	return "", false
}

// updateDirNameCache adds a name to the cache for the given directory.
// Called after successfully processing a Create event.
func (o *localObserver) updateDirNameCache(dirPath, name string) {
	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		return // not cached yet — will be populated lazily on next check
	}

	key := namekey.Key(name)
	if slices.ContainsFunc(cache[key], func(existing string) bool { return nfcNormalize(existing) == nfcNormalize(name) }) {
		return // already present, possibly in its on-disk spelling
	}

	cache[key] = append(cache[key], name)
}

// removeDirNameCache removes a name from the cache for the given directory.
// Called after processing a Delete event.
func (o *localObserver) removeDirNameCache(dirPath, name string) {
	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		return
	}

	key := namekey.Key(name)
	names := cache[key]

	for i, n := range names {
		if nfcNormalize(n) == nfcNormalize(name) {
			cache[key] = append(names[:i], names[i+1:]...)

			if len(cache[key]) == 0 {
				delete(cache, key)
			}

			break
		}
	}
}

// populateDirNameCache pre-populates the directory name cache from an already-
// read set of directory entries. Called by scanNewDirectory after os.ReadDir
// to avoid a redundant filesystem read in HasCaseCollisionCached.
func (o *localObserver) populateDirNameCache(dirPath string, entries []os.DirEntry) {
	if o.DirNameCache == nil {
		o.DirNameCache = make(map[string]map[string][]string)
	}

	cache := make(map[string][]string, len(entries))

	for _, e := range entries {
		key := namekey.Key(e.Name())
		cache[key] = append(cache[key], e.Name())
	}

	o.DirNameCache[dirPath] = cache
}

// AddCollisionPeer records a bidirectional collision relationship between
// two paths. Creates inner sets lazily. Idempotent — safe to call multiple
// times for the same pair. Single-goroutine (watchLoop) access.
func (o *localObserver) AddCollisionPeer(a, b string) {
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
func (o *localObserver) removeCollisionPeersFor(dbRelPath string) map[string]struct{} {
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
