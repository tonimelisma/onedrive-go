package sync

import (
	"context"
)

// LocalHashCacheEntry is one recorded hash and the observation it was made
// under.
type LocalHashCacheEntry struct {
	Path      string
	Hash      string
	Size      int64
	Mtime     int64
	Ctime     int64
	Device    uint64
	Inode     uint64
	BirthTime int64
}

// LocalHashCache is what a scan consults and feeds. The store implements it;
// tests substitute a map.
type LocalHashCache interface {
	Lookup(path string) (LocalHashCacheEntry, bool)
	Record(ctx context.Context, entries []LocalHashCacheEntry) error
}
