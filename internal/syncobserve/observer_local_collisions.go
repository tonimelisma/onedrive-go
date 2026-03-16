// observer_local_collisions.go — case collision detection helpers for watch mode.
//
// Contents:
//   - HasCaseCollisionCached: cached per-directory collision check
//   - dirNameCache maintenance (update/remove)
//   - buildPeerRelPath: collision peer path construction
//
// Related files:
//   - observer_local_handlers.go: event handlers that call these methods
//   - scanner.go:                 FullScan's DetectCaseCollisions (authoritative)
package syncobserve

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// buildPeerRelPath constructs the db-relative path for a collision peer
// given the current file's dbRelPath and the colliding file's name.
func (o *LocalObserver) buildPeerRelPath(dbRelPath, collidingName string) string {
	dir := filepath.Dir(dbRelPath)
	if dir == "." {
		return collidingName
	}

	return dir + "/" + collidingName
}

// HasCaseCollisionCached checks if name collides with an existing sibling
// using a per-directory name cache (filesystem) and the baseline (synced
// files). Falls back to os.ReadDir on cache miss. The dbDir parameter is
// the db-relative directory path used for baseline cross-check.
// Single-goroutine (watchLoop) access — no mutex needed.
func (o *LocalObserver) HasCaseCollisionCached(dirPath, name, dbDir string) (string, bool) {
	if o.DirNameCache == nil {
		o.DirNameCache = make(map[string]map[string][]string)
	}

	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			return "", false
		}

		cache = make(map[string][]string, len(entries))

		for _, e := range entries {
			low := strings.ToLower(e.Name())
			cache[low] = append(cache[low], e.Name())
		}

		o.DirNameCache[dirPath] = cache
	}

	low := strings.ToLower(name)

	for _, existing := range cache[low] {
		if existing != name {
			return existing, true
		}
	}

	// Cross-check baseline for files that exist remotely but not locally.
	// A file synced to OneDrive but unchanged on disk produces no event
	// and may not appear in the filesystem cache (e.g., remote-only items).
	variants := o.Baseline.GetCaseVariants(dbDir, name)
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
func (o *LocalObserver) updateDirNameCache(dirPath, name string) {
	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		return // not cached yet — will be populated lazily on next check
	}

	low := strings.ToLower(name)

	if slices.Contains(cache[low], name) {
		return // already present
	}

	cache[low] = append(cache[low], name)
}

// removeDirNameCache removes a name from the cache for the given directory.
// Called after processing a Delete event.
func (o *LocalObserver) removeDirNameCache(dirPath, name string) {
	cache, ok := o.DirNameCache[dirPath]
	if !ok {
		return
	}

	low := strings.ToLower(name)
	names := cache[low]

	for i, n := range names {
		if n == name {
			cache[low] = append(names[:i], names[i+1:]...)

			if len(cache[low]) == 0 {
				delete(cache, low)
			}

			break
		}
	}
}

// populateDirNameCache pre-populates the directory name cache from an already-
// read set of directory entries. Called by scanNewDirectory after os.ReadDir
// to avoid a redundant filesystem read in HasCaseCollisionCached.
func (o *LocalObserver) populateDirNameCache(dirPath string, entries []os.DirEntry) {
	if o.DirNameCache == nil {
		o.DirNameCache = make(map[string]map[string][]string)
	}

	cache := make(map[string][]string, len(entries))

	for _, e := range entries {
		low := strings.ToLower(e.Name())
		cache[low] = append(cache[low], e.Name())
	}

	o.DirNameCache[dirPath] = cache
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
