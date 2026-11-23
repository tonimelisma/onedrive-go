package syncdomain

// BaselineView is the read-only face of the baseline: the confirmed synced
// state of every path. It is all that code outside the store holds. Lookups
// return entries by value, so no holder can change what another one reads;
// the store alone writes the baseline, in SQLite and in its cache together.
//
// Two indexes answer it: by path (primary) and by item ID (move detection).
// Case collisions are judged against the names on disk, never the baseline
// (syncobserve.DirNames).
type BaselineView interface {
	// GetByPath returns the entry at the given stored path.
	GetByPath(path string) (BaselineEntry, bool)
	// GetByID returns the entry of the given item ID.
	GetByID(itemID string) (BaselineEntry, bool)
	// Len returns the number of entries.
	Len() int
	// ForEachPath calls fn for every entry, in no particular order. fn must
	// not call back into the view: a live view holds its read lock for the
	// whole iteration.
	ForEachPath(fn func(path string, entry BaselineEntry))
}

// BaselineSnapshot is an immutable baseline: the entries one read saw. A plan
// reads its baseline as a snapshot taken in the same transaction as its
// comparison rows, so the two can never disagree; tests build one from
// literal entries. Safe for concurrent use without a lock, since nothing
// writes it after NewBaselineSnapshot.
type BaselineSnapshot struct {
	byPath map[string]*BaselineEntry
	byID   map[string]*BaselineEntry
}

// NewBaselineSnapshot indexes copies of entries; a later change to an entry
// the caller passed does not reach the snapshot.
func NewBaselineSnapshot(entries []*BaselineEntry) *BaselineSnapshot {
	snapshot := &BaselineSnapshot{
		byPath: make(map[string]*BaselineEntry, len(entries)),
		byID:   make(map[string]*BaselineEntry, len(entries)),
	}
	for _, entry := range entries {
		owned := *entry
		snapshot.byPath[owned.Path] = &owned
		snapshot.byID[owned.ItemID] = &owned
	}

	return snapshot
}

// GetByPath implements BaselineView.
func (b *BaselineSnapshot) GetByPath(path string) (BaselineEntry, bool) {
	entry, ok := b.byPath[path]
	if !ok {
		return BaselineEntry{}, false
	}

	return *entry, true
}

// GetByID implements BaselineView.
func (b *BaselineSnapshot) GetByID(itemID string) (BaselineEntry, bool) {
	entry, ok := b.byID[itemID]
	if !ok {
		return BaselineEntry{}, false
	}

	return *entry, true
}

// Len implements BaselineView.
func (b *BaselineSnapshot) Len() int {
	return len(b.byPath)
}

// ForEachPath implements BaselineView.
func (b *BaselineSnapshot) ForEachPath(fn func(path string, entry BaselineEntry)) {
	for path, entry := range b.byPath {
		fn(path, *entry)
	}
}
