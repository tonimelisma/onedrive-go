package sync

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// mapHashCache is the in-memory cache the scanner tests feed and consult.
type mapHashCache struct {
	entries map[string]localHashCacheEntry
	records int
}

func newMapHashCache() *mapHashCache {
	return &mapHashCache{entries: map[string]localHashCacheEntry{}}
}

func (c *mapHashCache) lookup(path string) (localHashCacheEntry, bool) {
	e, ok := c.entries[path]
	return e, ok
}

func (c *mapHashCache) record(_ context.Context, entries []localHashCacheEntry) error {
	c.records++
	for _, e := range entries {
		c.entries[e.Path] = e
	}
	return nil
}

// countingHasher hashes for real and counts the files it read.
func countingHasher(reads *atomic.Int32) func(context.Context, string) (string, error) {
	return func(ctx context.Context, path string) (string, error) {
		reads.Add(1)
		return driveops.ComputeQuickXorHashContext(ctx, path)
	}
}

func writeAgedFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	// Outside the racily-clean window: the fast path trusts nothing written
	// within a second of the scan.
	old := time.Now().Add(-5 * time.Second)
	require.NoError(t, os.Chtimes(path, old, old))
}

func scanWithCache(t *testing.T, syncRoot string, cache localHashCache, reads *atomic.Int32) scanResult {
	t.Helper()
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.hashFunc = countingHasher(reads)
	obs.SetLocalHashCache(cache)
	// ctime cannot be aged, so the racily-clean guard on it is satisfied
	// by a scan clock a few seconds ahead of the writes.
	obs.clock = newManualClock(time.Now().Add(5 * time.Second))
	result, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)
	return result
}

func hashesByPath(result scanResult) map[string]string {
	out := map[string]string{}
	for i := range result.Rows {
		if result.Rows[i].ItemType == ItemTypeFile {
			out[result.Rows[i].Path] = result.Rows[i].Hash
		}
	}
	return out
}

// Validates: R-2.2.8
//
// A first scan of 1.2 TB hashes for two hours and keeps nothing until its
// pass commits, so a kill or a restart during it started over. The hashes a
// scan computes for files the baseline does not know are now recorded as it
// goes, and the next scan reads them back instead of the files, under the
// fast path's own conditions.
func TestFullScan_RecordedHashesSpareTheNextScanTheRead(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "Docs"), 0o750))
	for _, name := range []string{"a.txt", "b.txt", "Docs/c.txt"} {
		writeAgedFile(t, filepath.Join(syncRoot, name), "content of "+name)
	}
	cache := newMapHashCache()

	var reads atomic.Int32
	first := scanWithCache(t, syncRoot, cache, &reads)
	assert.Equal(t, int32(3), reads.Load(), "the first scan reads every file")
	assert.Len(t, cache.entries, 3, "and records every hash")
	assert.Equal(t, 1, cache.records, "in one batch below the batch size")

	reads.Store(0)
	second := scanWithCache(t, syncRoot, cache, &reads)
	assert.Zero(t, reads.Load(), "the second scan reads nothing")
	assert.Equal(t, hashesByPath(first), hashesByPath(second), "and reports the same hashes")
	assert.Len(t, second.Events, 4, "three creates and a folder create, as before")

	// An edit that moves the mtime, and a same-size rewrite that keeps it
	// but not the ctime, are both read again; the untouched files are not.
	writeAgedFile(t, filepath.Join(syncRoot, "a.txt"), "content of a.txt, edited")
	sameSize := filepath.Join(syncRoot, "b.txt")
	info, err := os.Stat(sameSize)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sameSize, []byte("CONTENT OF b.txt"), 0o600))
	require.NoError(t, os.Chtimes(sameSize, info.ModTime(), info.ModTime()))
	reads.Store(0)
	third := scanWithCache(t, syncRoot, cache, &reads)
	assert.Equal(t, int32(2), reads.Load(), "only the two changed files are read")
	assert.NotEqual(t, hashesByPath(second)["a.txt"], hashesByPath(third)["a.txt"])
	assert.NotEqual(t, hashesByPath(second)["b.txt"], hashesByPath(third)["b.txt"], "a rewrite with a restored mtime is not trusted")
	assert.Equal(t, hashesByPath(second)["Docs/c.txt"], hashesByPath(third)["Docs/c.txt"])
}

// Validates: R-2.2.8
//
// A file written within the racily-clean window of the scan is read even
// when the cache has it, as the baseline fast path would.
func TestFullScan_RecordedHashIsNotTrustedInsideTheRacilyCleanWindow(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()
	path := filepath.Join(syncRoot, "fresh.txt")
	require.NoError(t, os.WriteFile(path, []byte("fresh"), 0o600))
	cache := newMapHashCache()

	var reads atomic.Int32
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.hashFunc = countingHasher(&reads)
	obs.SetLocalHashCache(cache)
	_, err := obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)
	require.Equal(t, int32(1), reads.Load())
	require.Len(t, cache.entries, 1)

	reads.Store(0)
	obs = newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.hashFunc = countingHasher(&reads)
	obs.SetLocalHashCache(cache)
	_, err = obs.FullScan(t.Context(), mustOpenSyncTree(t, syncRoot))
	require.NoError(t, err)
	assert.Equal(t, int32(1), reads.Load(), "a file this young is read again, cache or not")
}

// Validates: R-2.2.8
//
// The store's cache round-trips what a scan recorded, and a committed local
// snapshot prunes the rows the baseline now owns and those of files that are
// gone.
func TestLocalHashCache_StoreRoundTripAndPrune(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	cache, err := store.newLocalHashCache(ctx)
	require.NoError(t, err)
	_, ok := cache.lookup("a.txt")
	assert.False(t, ok, "empty to begin with")

	entries := []localHashCacheEntry{
		{Path: "a.txt", Hash: "hash-a", Size: 1, Mtime: 10, Ctime: 11, Device: 7, Inode: 70, BirthTime: 1},
		{Path: "b.txt", Hash: "hash-b", Size: 2, Mtime: 20, Ctime: 21, Device: 7, Inode: 71},
		{Path: "gone.txt", Hash: "hash-gone", Size: 3, Mtime: 30, Ctime: 31, Device: 7, Inode: 72},
	}
	require.NoError(t, cache.record(ctx, entries))

	reloaded, err := store.newLocalHashCache(ctx)
	require.NoError(t, err)
	got, ok := reloaded.lookup("a.txt")
	require.True(t, ok)
	assert.Equal(t, entries[0], got, "every field survives the round trip")

	// a.txt gets a baseline row; the snapshot still lists a.txt and b.txt,
	// gone.txt is no longer on disk.
	require.NoError(t, store.CommitMutation(ctx, faultSeedOutcomeForCache("a.txt", "item-a")))
	_, err = store.ReplaceLocalStateBindingRoot(ctx, []localStateRow{
		{Path: "a.txt", ItemType: ItemTypeFile, Hash: "hash-a", Size: 1, Mtime: 10},
		{Path: "b.txt", ItemType: ItemTypeFile, Hash: "hash-b", Size: 2, Mtime: 20},
	}, nil, nil)
	require.NoError(t, err)

	pruned, err := store.newLocalHashCache(ctx)
	require.NoError(t, err)
	_, ok = pruned.lookup("a.txt")
	assert.False(t, ok, "the baseline owns a.txt now")
	_, ok = pruned.lookup("gone.txt")
	assert.False(t, ok, "gone.txt is not in the snapshot")
	_, ok = pruned.lookup("b.txt")
	assert.True(t, ok, "b.txt is still waiting for its pass")
}

func faultSeedOutcomeForCache(path, itemID string) *BaselineMutation {
	return mutationFromActionOutcome(&actionOutcome{
		Action: ActionUpload, Success: true, Path: path,
		DriveID: driveid.New(engineTestDriveID), ItemID: itemID, ParentID: "root", ItemType: ItemTypeFile,
		LocalHash: "hash-" + path, RemoteHash: "hash-" + path,
		LocalSize: 1, LocalSizeKnown: true, RemoteSize: 1, RemoteSizeKnown: true,
	})
}
