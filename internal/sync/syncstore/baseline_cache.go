package syncstore

import (
	stdsync "sync"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// baselineIndexes are the three lookups of a baseline: by path (primary), by
// item ID (move detection) and by (directory, namespace key) for case
// collision detection. They always change together, so a lookup never sees a
// path present in one index and absent from another.
type baselineIndexes struct {
	byPath       map[string]*syncdomain.BaselineEntry
	byID         map[string]*syncdomain.BaselineEntry                  // keyed by item_id within this per-mount DB
	byDirNameKey map[syncdomain.DirNameKey][]*syncdomain.BaselineEntry // OneDrive-equivalent sibling index (namekey)
}

// indexBaselineEntries indexes entries. The entries become the cache's own:
// the caller must not keep or change them.
func indexBaselineEntries(entries []*syncdomain.BaselineEntry) baselineIndexes {
	idx := baselineIndexes{
		byPath:       make(map[string]*syncdomain.BaselineEntry, len(entries)),
		byID:         make(map[string]*syncdomain.BaselineEntry, len(entries)),
		byDirNameKey: make(map[syncdomain.DirNameKey][]*syncdomain.BaselineEntry, len(entries)),
	}
	for _, entry := range entries {
		idx.byPath[entry.Path] = entry
		idx.byID[entry.ItemID] = entry
		dnk := syncdomain.DirNameKeyFromPath(entry.Path)
		idx.byDirNameKey[dnk] = append(idx.byDirNameKey[dnk], entry)
	}

	return idx
}

// baselineCache is the store's in-memory copy of the baseline table, and the
// syncdomain.BaselineView every run holds. Only the store writes it, through
// the unexported put, delete and swap, right after the transaction whose
// rows they mirror commits. Holders keep the same *baselineCache for a whole
// run (watch keeps its startup one), which is why a reload swaps the indexes
// in place instead of replacing the object.
type baselineCache struct {
	mu  stdsync.RWMutex // guards idx: lookups read-lock it, put, delete and swap write-lock it
	idx baselineIndexes
}

func newBaselineCache(idx baselineIndexes) *baselineCache {
	return &baselineCache{idx: idx}
}

// GetByPath implements syncdomain.BaselineView.
func (b *baselineCache) GetByPath(path string) (syncdomain.BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.idx.byPath[path]
	if !ok {
		return syncdomain.BaselineEntry{}, false
	}

	return *entry, true
}

// GetByID implements syncdomain.BaselineView.
func (b *baselineCache) GetByID(itemID string) (syncdomain.BaselineEntry, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.idx.byID[itemID]
	if !ok {
		return syncdomain.BaselineEntry{}, false
	}

	return *entry, true
}

// GetCaseVariants implements syncdomain.BaselineView. The values are copied
// under the read lock: the watch observer reads them on its own goroutine
// while workers commit outcomes through put (R-6.3.4).
func (b *baselineCache) GetCaseVariants(dir, name string) []syncdomain.BaselineEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return syncdomain.CopyBaselineEntries(b.idx.byDirNameKey[syncdomain.DirNameKey{Dir: dir, NameKey: namekey.Of(name)}])
}

// Len implements syncdomain.BaselineView.
func (b *baselineCache) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.idx.byPath)
}

// ForEachPath implements syncdomain.BaselineView; the read lock is held for
// the whole iteration.
func (b *baselineCache) ForEachPath(fn func(path string, entry syncdomain.BaselineEntry)) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for path, entry := range b.idx.byPath {
		fn(path, *entry)
	}
}

// put records a copy of entry in all three indexes, the way the
// item_id-keyed row does: a path taken over by a new item ID drops the old
// ID, and an item at a new path leaves its old one.
func (b *baselineCache) put(entry *syncdomain.BaselineEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	copied := *entry
	owned := &copied
	if old, ok := b.idx.byPath[owned.Path]; ok && old.ItemID != owned.ItemID {
		delete(b.idx.byID, old.ItemID)
	}
	if old, ok := b.idx.byID[owned.ItemID]; ok && old.Path != owned.Path {
		b.deleteLocked(old.Path)
	}

	b.idx.byPath[owned.Path] = owned
	b.idx.byID[owned.ItemID] = owned

	dnk := syncdomain.DirNameKeyFromPath(owned.Path)
	for i, existing := range b.idx.byDirNameKey[dnk] {
		if existing.Path == owned.Path {
			b.idx.byDirNameKey[dnk][i] = owned
			return
		}
	}
	b.idx.byDirNameKey[dnk] = append(b.idx.byDirNameKey[dnk], owned)
}

// delete removes the entry at path from all three indexes.
func (b *baselineCache) delete(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.deleteLocked(path)
}

// deleteLocked is delete for a caller holding mu.
func (b *baselineCache) deleteLocked(path string) {
	if entry, ok := b.idx.byPath[path]; ok {
		delete(b.idx.byID, entry.ItemID)
	}
	delete(b.idx.byPath, path)

	dnk := syncdomain.DirNameKeyFromPath(path)
	entries := b.idx.byDirNameKey[dnk]
	for i, existing := range entries {
		if existing.Path != path {
			continue
		}
		b.idx.byDirNameKey[dnk] = append(entries[:i], entries[i+1:]...)
		if len(b.idx.byDirNameKey[dnk]) == 0 {
			delete(b.idx.byDirNameKey, dnk)
		}

		return
	}
}

// swap replaces every entry with idx, so every holder of b sees them.
func (b *baselineCache) swap(idx baselineIndexes) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.idx = idx
}
