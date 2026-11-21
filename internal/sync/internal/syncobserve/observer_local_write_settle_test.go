package syncobserve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// newSettleTestObserver returns an observer driven entirely by a manual clock
// and a counting hash function, for handler-level tests that call the watch
// handlers directly instead of running the watch loop.
func newSettleTestObserver(t *testing.T, baseline *syncdomain.Baseline, clock *syncfixture.ManualClock, hashCalls *atomic.Int64) *LocalObserver {
	t.Helper()

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.clock = clock
	// Each read returns a new hash: every write in these tests changes the
	// content.
	obs.hooks.HashFile = func(context.Context, string) (string, error) {
		return fmt.Sprintf("hash-from-read-%d", hashCalls.Add(1)), nil
	}

	return obs
}

func syncedFileAt(t *testing.T, path string, info os.FileInfo, size int64, hash string) *syncdomain.BaselineEntry {
	t.Helper()

	identity, ok := synctree.IdentityFromFileInfo(info)
	require.True(t, ok, "test needs filesystem identity")
	ctime, ok := synctree.ChangeTimeFromFileInfo(info)
	require.True(t, ok, "test needs ctime")

	return &syncdomain.BaselineEntry{
		Path: path, DriveID: driveid.New("d"), ItemID: "item-" + path, ItemType: syncdomain.ItemTypeFile,
		LocalHash: hash, LocalSize: size, LocalSizeKnown: true, LocalMtime: info.ModTime().UnixNano(),
		LocalCtime:  ctime,
		LocalDevice: identity.Device, LocalInode: identity.Inode, LocalHasIdentity: true,
	}
}

// ---------------------------------------------------------------------------
// D-080: the destination half of a rename is not held back by hashing
// ---------------------------------------------------------------------------

// Validates: R-2.8.15
func TestHandleCreate_MovedChangedFileRowIsNotHeldBackByHashing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := syncfixture.WriteTestFile(t, dir, "new.txt", "edited after the last sync")
	info, err := os.Stat(fsPath)
	require.NoError(t, err)

	// The synced item is the same inode under its old name, but its size
	// differs, so its synced hash cannot be reused.
	baseline := syncfixture.BaselineWith(syncedFileAt(t, "old.txt", info, info.Size()-1, "synced-hash"))
	clock := syncfixture.NewManualClock(time.Now().Add(time.Hour))
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, baseline, clock, &hashCalls)
	tree := syncfixture.MustOpenSyncTree(t, dir)
	batches := localBatches(obs, 10)

	obs.handleCreate(t.Context(), tree, fsPath, "new.txt", "new.txt", newMockFsWatcher())

	rows := drainPatchRows(batches)
	require.Len(t, rows, 1)
	ev := rows[0]
	assert.Equal(t, "new.txt", ev.Path)
	assert.Empty(t, ev.Hash, "the moved file's row goes out before its content is read")
	assert.True(t, ev.LocalHasIdentity, "identity lets the planner pair the move")
	assert.Equal(t, info.ModTime().UnixNano(), ev.Mtime)
	assert.Zero(t, hashCalls.Load(), "no synchronous read on the create path")
	require.Contains(t, obs.writeChurn, "new.txt")
	assert.False(t, obs.writeChurn["new.txt"].firstPendingAt.IsZero(),
		"the deferred hash opens a coalescing window, so later writes cannot postpone it past the max wait")

	// The hash follows as a deferred request.
	clock.Advance(defaultWriteCoalesceCooldown)
	require.Len(t, obs.HashRequests, 1)
	obs.HashAndEmit(t.Context(), tree, <-obs.HashRequests)

	rows = drainPatchRows(batches)
	require.Len(t, rows, 1)
	followUp := rows[0]
	assert.Equal(t, "new.txt", followUp.Path)
	assert.Equal(t, "hash-from-read-1", followUp.Hash)
}

// Validates: R-2.8.15
func TestHandleCreate_RenamedUnchangedFileReusesSyncedHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := syncfixture.WriteTestFile(t, dir, "renamed.bin", "large content that must not be re-read")
	info, err := os.Stat(fsPath)
	require.NoError(t, err)

	baseline := syncfixture.BaselineWith(syncedFileAt(t, "original.bin", info, info.Size(), "synced-hash"))
	// Well past the racily-clean window of the file's mtime.
	clock := syncfixture.NewManualClock(info.ModTime().Add(time.Hour))
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, baseline, clock, &hashCalls)
	batches := localBatches(obs, 10)

	obs.handleCreate(t.Context(), syncfixture.MustOpenSyncTree(t, dir), fsPath, "renamed.bin", "renamed.bin", newMockFsWatcher())

	rows := drainPatchRows(batches)
	require.Len(t, rows, 1)
	ev := rows[0]
	assert.Equal(t, "synced-hash", ev.Hash, "same inode, size and mtime: the synced hash is reused")
	assert.Zero(t, hashCalls.Load(), "a renamed file is not re-read")
	assert.Empty(t, obs.PendingTimers, "nothing left to hash")
}

// Validates: R-2.8.15, R-2.2.4
func TestHandleCreate_RenameThatMovedCtimeDefersTheHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	oldPath := syncfixture.WriteTestFile(t, dir, "original.bin", "content the baseline hashed")
	before, err := os.Stat(oldPath)
	require.NoError(t, err)
	synced := syncedFileAt(t, "original.bin", before, before.Size(), "hash-before-rename")
	baseline := syncfixture.BaselineWith(synced)

	// The kernel moves ctime on rename, and ctime cannot tell a rename from
	// an in-place edit that restored the mtime, so the synced hash is not
	// trusted. The row still goes out at once for move pairing; only the
	// read is deferred.
	fsPath := filepath.Join(dir, "renamed.bin")
	require.NoError(t, os.Rename(oldPath, fsPath))
	after, err := os.Stat(fsPath)
	require.NoError(t, err)
	ctimeAfter, ok := synctree.ChangeTimeFromFileInfo(after)
	require.True(t, ok)
	require.NotEqual(t, synced.LocalCtime, ctimeAfter, "Linux and macOS move ctime on rename")

	clock := syncfixture.NewManualClock(after.ModTime().Add(time.Hour))
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, baseline, clock, &hashCalls)
	batches := localBatches(obs, 10)

	obs.handleCreate(t.Context(), syncfixture.MustOpenSyncTree(t, dir), fsPath, "renamed.bin", "renamed.bin", newMockFsWatcher())

	rows := drainPatchRows(batches)
	require.Len(t, rows, 1)
	ev := rows[0]
	assert.Empty(t, ev.Hash, "the synced hash is not reused across a ctime change")
	assert.True(t, ev.LocalHasIdentity, "identity still pairs the move")
	assert.Zero(t, hashCalls.Load(), "no synchronous read on the create path")
	assert.Contains(t, obs.PendingTimers, "renamed.bin", "the read follows as a deferred hash")
}

// Validates: R-2.8.15
func TestHandleCreate_ReusedInodeWhileSyncedPathExistsIsHashed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := syncfixture.WriteTestFile(t, dir, "new.txt", "unrelated content")
	syncfixture.WriteTestFile(t, dir, "old.txt", "the synced file, rewritten in place")
	info, err := os.Stat(fsPath)
	require.NoError(t, err)

	// The synced file's recorded inode was freed (the file was replaced) and
	// reused by an unrelated file of the same size with a preserved mtime.
	// The synced path still exists, so this is not a move.
	baseline := syncfixture.BaselineWith(syncedFileAt(t, "old.txt", info, info.Size(), "synced-hash"))
	clock := syncfixture.NewManualClock(info.ModTime().Add(time.Hour))
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, baseline, clock, &hashCalls)
	batches := localBatches(obs, 10)

	obs.handleCreate(t.Context(), syncfixture.MustOpenSyncTree(t, dir), fsPath, "new.txt", "new.txt", newMockFsWatcher())

	rows := drainPatchRows(batches)
	require.Len(t, rows, 1)
	ev := rows[0]
	assert.Equal(t, "hash-from-read-1", ev.Hash, "the synced hash is not attached to unrelated content")
	assert.Equal(t, int64(1), hashCalls.Load())
	assert.Empty(t, obs.PendingTimers)
}

// Validates: R-2.8.15
func TestHandleCreate_DirectoryRowPrecedesItsContents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	syncfixture.WriteTestFile(t, dir, "moved/inner.txt", "content")

	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	batches := localBatches(obs, 10)

	obs.handleCreate(t.Context(), syncfixture.MustOpenSyncTree(t, dir), filepath.Join(dir, "moved"), "moved", "moved", newMockFsWatcher())

	rows := drainPatchRows(batches)
	require.Len(t, rows, 2)
	first := rows[0]
	assert.Equal(t, "moved", first.Path, "the folder's row, which pairs a folder move, is not held back by its contents")
	assert.Equal(t, syncdomain.ItemTypeFolder, first.ItemType)
	assert.Equal(t, "moved/inner.txt", rows[1].Path)
}

// ---------------------------------------------------------------------------
// Write coalescing: no starvation, and backoff for files that never settle
// ---------------------------------------------------------------------------

// Validates: R-2.8.16
func TestScheduleWriteHash_ContinuousWritesHashWithinMaxWait(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := syncfixture.WriteTestFile(t, dir, "busy.log", "line")
	clock := syncfixture.NewManualClock(time.Now())
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, syncfixture.EmptyBaseline(), clock, &hashCalls)

	// A write every 400 ms, faster than the 500 ms quiet period. Before the
	// fix every write restarted the timer and the file was never hashed.
	const writeInterval = 400 * time.Millisecond
	var elapsed time.Duration
	for elapsed <= 2*maxWriteCoalesceWait && len(obs.HashRequests) == 0 {
		obs.handleWrite(nil, fsPath, "busy.log")
		clock.Advance(writeInterval)
		elapsed += writeInterval
	}

	require.Len(t, obs.HashRequests, 1, "continuous writes must not postpone the hash forever")
	assert.LessOrEqual(t, elapsed, maxWriteCoalesceWait+defaultWriteCoalesceCooldown+writeInterval)
}

// Validates: R-2.8.16
func TestWatch_ContinuouslyWrittenFileBacksOff(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := syncfixture.WriteTestFile(t, dir, "app.db", "v0")
	clock := syncfixture.NewManualClock(time.Now())
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, syncfixture.EmptyBaseline(), clock, &hashCalls)
	tree := syncfixture.MustOpenSyncTree(t, dir)
	batches := localBatches(obs, 16)

	// writeAndHashAfter writes once and expects the hash exactly delay later,
	// not one quiet period earlier.
	writeAndHashAfter := func(delay time.Duration) {
		t.Helper()
		obs.handleWrite(tree, fsPath, "app.db")
		if delay > defaultWriteCoalesceCooldown {
			clock.Advance(delay - defaultWriteCoalesceCooldown)
			require.Empty(t, obs.HashRequests, "hashed before the %s backoff passed", delay)
			obs.handleWrite(tree, fsPath, "app.db") // writes during backoff do not restart it
			clock.Advance(defaultWriteCoalesceCooldown)
		} else {
			clock.Advance(delay)
		}
		require.Len(t, obs.HashRequests, 1, "the change is picked up once the %s backoff passes", delay)
		obs.HashAndEmit(t.Context(), tree, <-obs.HashRequests)
		require.Len(t, drainPatchRows(batches), 1)
	}

	for range unstableWriteEmits {
		writeAndHashAfter(defaultWriteCoalesceCooldown)
		clock.Advance(10 * time.Second)
	}

	// Unstable now. Every further change doubles the backoff up to the cap.
	// Before the fix the backoff stayed pinned at the initial minute, because
	// three emits never fit in the two-minute window once each waited a
	// minute.
	for _, want := range []time.Duration{
		unstableWriteInitialBackoff,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		unstableWriteMaxBackoff,
		unstableWriteMaxBackoff,
	} {
		writeAndHashAfter(want)
	}

	// Quiet for twice the backoff: the path is stable again.
	clock.Advance(2 * unstableWriteMaxBackoff)
	writeAndHashAfter(defaultWriteCoalesceCooldown)
}

// Validates: R-2.8.16
func TestHashAndEmit_StillChangingAtRetryCapStaysPendingWithoutEmptyHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := syncfixture.WriteTestFile(t, dir, "growing.log", "x")
	clock := syncfixture.NewManualClock(time.Now())
	obs := NewLocalObserver(syncfixture.EmptyBaseline(), synctest.TestLogger(t), 0)
	obs.clock = clock
	// The file grows during every read, as a log being appended to does.
	grow := 0
	obs.hooks.HashFile = func(_ context.Context, path string) (string, error) {
		grow++
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", grow+1)), 0o600))
		return "partial", nil
	}
	tree := syncfixture.MustOpenSyncTree(t, dir)
	batches := localBatches(obs, 8)

	obs.handleWrite(tree, fsPath, "growing.log")
	for range maxCoalesceRetries + 1 {
		clock.Advance(defaultWriteCoalesceCooldown)
		require.Len(t, obs.HashRequests, 1)
		obs.HashAndEmit(t.Context(), tree, <-obs.HashRequests)
	}

	assert.Empty(t, drainPatchRows(batches), "a file still being written is not emitted with an empty hash")
	require.Contains(t, obs.PendingTimers, "growing.log", "the path stays pending")
	assert.Equal(t, unstableWriteInitialBackoff, obs.writeChurn["growing.log"].backoff)

	clock.Advance(unstableWriteInitialBackoff)
	assert.Len(t, obs.HashRequests, 1, "it is checked again after the backoff")
}

// Validates: R-2.8.16
func TestPruneWriteChurn_DropsSettledPaths(t *testing.T) {
	t.Parallel()

	clock := syncfixture.NewManualClock(time.Now())
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, syncfixture.EmptyBaseline(), clock, &hashCalls)
	obs.recordWriteEmit("settled.txt", "h1")
	obs.writeChurnFor("pending.txt", clock.Now()).firstPendingAt = clock.Now()

	clock.Advance(2 * unstableWriteWindow)
	obs.pruneWriteChurn(clock.Now())

	assert.NotContains(t, obs.writeChurn, "settled.txt")
	assert.Contains(t, obs.writeChurn, "pending.txt", "a path with a pending hash is kept")
}
