package sync

import (
	"context"
	"io/fs"
	"log/slog"
	stdsync "sync"
)

// hashCacheBatch is how many freshly computed hashes the hash phase writes
// to the cache at a time: a transaction every few hundred files keeps the
// scan's progress durable without a commit per file.
const hashCacheBatch = 256

// cachedHashForNewFile answers the recorded hash of a file the baseline does
// not know, when the record is still good for it: the same contract as the
// baseline fast path (R-2.2.4), size, full-precision mtime, ctime, identity
// and the racily-clean window. A record that fails any of them is ignored,
// and the hash phase overwrites it.
func (o *localObserver) cachedHashForNewFile(
	dbRelPath string, info fs.FileInfo, observed *localStateRow, scanStartNano int64,
) (string, bool) {
	if o.hashCache == nil || info.IsDir() {
		return "", false
	}
	entry, ok := o.hashCache.lookup(dbRelPath)
	if !ok || entry.Hash == "" {
		return "", false
	}
	mtime := info.ModTime().UnixNano()
	if info.Size() != entry.Size || mtime != entry.Mtime {
		return "", false
	}
	if observed.Ctime == 0 || observed.Ctime != entry.Ctime {
		return "", false
	}
	if !observed.LocalHasIdentity || observed.LocalDevice != entry.Device || observed.LocalInode != entry.Inode ||
		observed.LocalBirthTime != entry.BirthTime {
		return "", false
	}
	if scanStartNano-mtime < nanosPerSecond || scanStartNano-observed.Ctime < nanosPerSecond {
		return "", false
	}

	return entry.Hash, true
}

// hashCacheRecorder batches the hashes the hash phase computes for new files
// and writes them to the cache. A write that fails is logged once and the
// scan goes on: the cache only saves work, it never decides anything.
type hashCacheRecorder struct {
	cache  localHashCache
	logger *slog.Logger
	mu     stdsync.Mutex
	batch  []localHashCacheEntry
	failed bool
}

func newHashCacheRecorder(cache localHashCache, logger *slog.Logger) *hashCacheRecorder {
	return &hashCacheRecorder{cache: cache, logger: logger}
}

func (r *hashCacheRecorder) add(ctx context.Context, entry localHashCacheEntry) {
	if r.cache == nil {
		return
	}
	r.mu.Lock()
	r.batch = append(r.batch, entry)
	var full []localHashCacheEntry
	if len(r.batch) >= hashCacheBatch {
		full, r.batch = r.batch, nil
	}
	r.mu.Unlock()
	if full != nil {
		r.write(ctx, full)
	}
}

// flush writes what is left. It takes the scan's own context, not the hash
// group's, so the last batch of a scan that finished is kept even though
// the group's context is done by then.
func (r *hashCacheRecorder) flush(ctx context.Context) {
	if r.cache == nil {
		return
	}
	r.mu.Lock()
	rest := r.batch
	r.batch = nil
	r.mu.Unlock()
	if len(rest) > 0 && ctx.Err() == nil {
		r.write(ctx, rest)
	}
}

func (r *hashCacheRecorder) write(ctx context.Context, entries []localHashCacheEntry) {
	if err := r.cache.record(ctx, entries); err != nil {
		r.mu.Lock()
		first := !r.failed
		r.failed = true
		r.mu.Unlock()
		if first {
			r.logger.Warn("Could not record scan hashes; a restart during this scan will hash these files again",
				slog.Int("files", len(entries)), slog.Any("error", err))
		}
	}
}
