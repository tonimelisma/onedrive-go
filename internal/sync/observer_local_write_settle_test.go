package sync

import (
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
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// newSettleTestObserver returns an observer driven entirely by a manual clock
// and a counting hash function, for handler-level tests that call the watch
// handlers directly instead of running the watch loop.
func newSettleTestObserver(t *testing.T, baseline *Baseline, clock *manualClock, hashCalls *atomic.Int64) *localObserver {
	t.Helper()

	obs := newLocalObserver(baseline, synctest.TestLogger(t), 0)
	obs.clock = clock
	// Each read returns a new hash: every write in these tests changes the
	// content.
	obs.hashFunc = func(string) (string, error) {
		return fmt.Sprintf("hash-from-read-%d", hashCalls.Add(1)), nil
	}

	return obs
}

func syncedFileAt(t *testing.T, path string, info os.FileInfo, size int64, hash string) *BaselineEntry {
	t.Helper()

	identity, ok := synctree.IdentityFromFileInfo(info)
	require.True(t, ok, "test needs filesystem identity")

	return &BaselineEntry{
		Path: path, DriveID: driveid.New("d"), ItemID: "item-" + path, ItemType: ItemTypeFile,
		LocalHash: hash, LocalSize: size, LocalSizeKnown: true, LocalMtime: info.ModTime().UnixNano(),
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
	fsPath := writeTestFile(t, dir, "new.txt", "edited after the last sync")
	info, err := os.Stat(fsPath)
	require.NoError(t, err)

	// The synced item is the same inode under its old name, but its size
	// differs, so its synced hash cannot be reused.
	baseline := baselineWith(syncedFileAt(t, "old.txt", info, info.Size()-1, "synced-hash"))
	clock := newManualClock(time.Now().Add(time.Hour))
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, baseline, clock, &hashCalls)
	tree := mustOpenSyncTree(t, dir)
	events := make(chan changeEvent, 10)

	obs.handleCreate(t.Context(), tree, fsPath, "new.txt", "new.txt", newMockFsWatcher(), events)

	require.Len(t, events, 1)
	ev := <-events
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
	obs.HashAndEmit(t.Context(), tree, <-obs.HashRequests, events)

	require.Len(t, events, 1)
	followUp := <-events
	assert.Equal(t, "new.txt", followUp.Path)
	assert.Equal(t, "hash-from-read-1", followUp.Hash)
}

// Validates: R-2.8.15
func TestHandleCreate_RenamedUnchangedFileReusesSyncedHash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := writeTestFile(t, dir, "renamed.bin", "large content that must not be re-read")
	info, err := os.Stat(fsPath)
	require.NoError(t, err)

	baseline := baselineWith(syncedFileAt(t, "original.bin", info, info.Size(), "synced-hash"))
	// Well past the racily-clean window of the file's mtime.
	clock := newManualClock(info.ModTime().Add(time.Hour))
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, baseline, clock, &hashCalls)
	events := make(chan changeEvent, 10)

	obs.handleCreate(t.Context(), mustOpenSyncTree(t, dir), fsPath, "renamed.bin", "renamed.bin", newMockFsWatcher(), events)

	require.Len(t, events, 1)
	ev := <-events
	assert.Equal(t, "synced-hash", ev.Hash, "same inode, size and mtime: the synced hash is reused")
	assert.Zero(t, hashCalls.Load(), "a renamed file is not re-read")
	assert.Empty(t, obs.PendingTimers, "nothing left to hash")
}

// Validates: R-2.8.15
func TestHandleCreate_DirectoryRowPrecedesItsContents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "moved/inner.txt", "content")

	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	events := make(chan changeEvent, 10)

	obs.handleCreate(t.Context(), mustOpenSyncTree(t, dir), filepath.Join(dir, "moved"), "moved", "moved", newMockFsWatcher(), events)

	require.Len(t, events, 2)
	first := <-events
	assert.Equal(t, "moved", first.Path, "the folder's row, which pairs a folder move, is not held back by its contents")
	assert.Equal(t, ItemTypeFolder, first.ItemType)
	assert.Equal(t, "moved/inner.txt", (<-events).Path)
}

// ---------------------------------------------------------------------------
// Write coalescing: no starvation, and backoff for files that never settle
// ---------------------------------------------------------------------------

// Validates: R-2.8.16
func TestScheduleWriteHash_ContinuousWritesHashWithinMaxWait(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fsPath := writeTestFile(t, dir, "busy.log", "line")
	clock := newManualClock(time.Now())
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, emptyBaseline(), clock, &hashCalls)

	// A write every 400 ms, faster than the 500 ms quiet period. Before the
	// fix every write restarted the timer and the file was never hashed.
	const writeInterval = 400 * time.Millisecond
	var elapsed time.Duration
	for elapsed <= 2*maxWriteCoalesceWait && len(obs.HashRequests) == 0 {
		obs.handleWrite(nil, fsPath, "busy.log", "busy.log")
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
	fsPath := writeTestFile(t, dir, "app.db", "v0")
	clock := newManualClock(time.Now())
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, emptyBaseline(), clock, &hashCalls)
	tree := mustOpenSyncTree(t, dir)
	events := make(chan changeEvent, 16)

	// writeAndHashAfter writes once and expects the hash exactly delay later,
	// not one quiet period earlier.
	writeAndHashAfter := func(delay time.Duration) {
		t.Helper()
		obs.handleWrite(tree, fsPath, "app.db", "app.db")
		if delay > defaultWriteCoalesceCooldown {
			clock.Advance(delay - defaultWriteCoalesceCooldown)
			require.Empty(t, obs.HashRequests, "hashed before the %s backoff passed", delay)
			obs.handleWrite(tree, fsPath, "app.db", "app.db") // writes during backoff do not restart it
			clock.Advance(defaultWriteCoalesceCooldown)
		} else {
			clock.Advance(delay)
		}
		require.Len(t, obs.HashRequests, 1, "the change is picked up once the %s backoff passes", delay)
		obs.HashAndEmit(t.Context(), tree, <-obs.HashRequests, events)
		require.Len(t, events, 1)
		<-events
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
	fsPath := writeTestFile(t, dir, "growing.log", "x")
	clock := newManualClock(time.Now())
	obs := newLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)
	obs.clock = clock
	// The file grows during every read, as a log being appended to does.
	grow := 0
	obs.hashFunc = func(path string) (string, error) {
		grow++
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", grow+1)), 0o600))
		return "partial", nil
	}
	tree := mustOpenSyncTree(t, dir)
	events := make(chan changeEvent, 8)

	obs.handleWrite(tree, fsPath, "growing.log", "growing.log")
	for range maxCoalesceRetries + 1 {
		clock.Advance(defaultWriteCoalesceCooldown)
		require.Len(t, obs.HashRequests, 1)
		obs.HashAndEmit(t.Context(), tree, <-obs.HashRequests, events)
	}

	assert.Empty(t, events, "a file still being written is not emitted with an empty hash")
	require.Contains(t, obs.PendingTimers, "growing.log", "the path stays pending")
	assert.Equal(t, unstableWriteInitialBackoff, obs.writeChurn["growing.log"].backoff)

	clock.Advance(unstableWriteInitialBackoff)
	assert.Len(t, obs.HashRequests, 1, "it is checked again after the backoff")
}

// Validates: R-2.8.16
func TestPruneWriteChurn_DropsSettledPaths(t *testing.T) {
	t.Parallel()

	clock := newManualClock(time.Now())
	var hashCalls atomic.Int64
	obs := newSettleTestObserver(t, emptyBaseline(), clock, &hashCalls)
	obs.recordWriteEmit("settled.txt", "h1")
	obs.writeChurnFor("pending.txt", clock.Now()).firstPendingAt = clock.Now()

	clock.Advance(2 * unstableWriteWindow)
	obs.pruneWriteChurn(clock.Now())

	assert.NotContains(t, obs.writeChurn, "settled.txt")
	assert.Contains(t, obs.writeChurn, "pending.txt", "a path with a pending hash is kept")
}
