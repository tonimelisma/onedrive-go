package syncobserve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// Hash failure modifies with empty hash (B-102) — Watch variant
// ---------------------------------------------------------------------------

// Validates: R-2.4.12, R-6.2.14
// TestWatch_HashFailureModifyStillEmitsEvent verifies that a Write event for a
// file whose hash cannot be computed (e.g., write-only permissions) still
// writes the file's row with an empty hash instead of being silently dropped
// (B-102).
func TestWatch_HashFailureModifyStillEmitsEvent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	filePath := syncfixture.WriteTestFile(t, dir, "watchable.txt", "original")
	existingHash := hashContent(t, "original")

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "watchable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: syncdomain.ItemTypeFile, LocalHash: existingHash,
	})

	obs := NewLocalObserver(baseline, synctest.TestLogger(t), 0)
	batches := localBatches(obs, 10)
	cancel, done := startLocalWatch(t, obs, dir)

	// Make file write-only (stat succeeds, hash computation fails).
	require.NoError(t, os.Chmod(filePath, 0o200))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(filePath, 0o600)) })

	// Write new content — triggers Write event. os.WriteFile opens O_WRONLY
	// which succeeds with 0o200 permissions.
	require.NoError(t, os.WriteFile(filePath, []byte("modified"), 0o200))

	row := waitForLocalRow(t, batches, "watchable.txt")
	syncfixture.StopWatch(t, cancel, done)

	require.Empty(t, row.Hash, "hash should be empty when computation fails")
	require.Equal(t, int64(len("modified")), row.Size)
}

// ---------------------------------------------------------------------------
// Write coalescing tests (B-107)
// ---------------------------------------------------------------------------

// Rapid writes coalescing into one hash one cooldown after the last write:
// TestHandleWrite_VirtualTime_RapidWritesCoalesceIntoOneHash.

// Validates: R-2.8.16
// TestHandleWrite_DifferentPathsNotCoalesced verifies that Write events for
// different paths are independent — each gets its own timer and emits separately.
func TestHandleWrite_DifferentPathsNotCoalesced(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	syncfixture.WriteTestFile(t, dir, "fileA.txt", "origA")
	syncfixture.WriteTestFile(t, dir, "fileB.txt", "origB")

	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "fileA.txt", DriveID: driveid.New("d"), ItemID: "a1",
			ItemType: syncdomain.ItemTypeFile, LocalHash: hashContent(t, "origA"),
		},
		&syncdomain.BaselineEntry{
			Path: "fileB.txt", DriveID: driveid.New("d"), ItemID: "b1",
			ItemType: syncdomain.ItemTypeFile, LocalHash: hashContent(t, "origB"),
		},
	)

	mockWatcher := newMockFsWatcher()
	clock := syncfixture.NewManualClock(time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC))
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		Baseline:              baseline,
		AfterFunc:             clock.AfterFunc,
		WriteCoalesceCooldown: 50 * time.Millisecond,
	})

	batches := localBatches(obs, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir)

	// Write new content to both files.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fileA.txt"), []byte("newA"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fileB.txt"), []byte("newB"), 0o600))

	// Send Write events for both paths within the same cooldown window.
	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filepath.Join(dir, "fileA.txt"), Op: fsnotify.Write,
	}
	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filepath.Join(dir, "fileB.txt"), Op: fsnotify.Write,
	}

	require.Eventually(t, func() bool {
		return clock.ActiveTimerCount() == 2
	}, syncfixture.EventWait, time.Millisecond)

	clock.Advance(50 * time.Millisecond)

	// Collect both rows.
	collected := make(map[string]syncdomain.LocalStateRow)
	timeout := time.After(syncfixture.EventWait)

	for len(collected) < 2 {
		select {
		case batch := <-batches:
			for _, row := range batch.Rows {
				if batch.Kind == LocalPatch {
					collected[row.Path] = row
				}
			}
		case <-timeout:
			require.Fail(t, "timeout", "got %d rows, want 2", len(collected))
		}
	}

	require.Contains(t, collected, "fileA.txt")
	require.Contains(t, collected, "fileB.txt")
	require.Equal(t, hashContent(t, "newA"), collected["fileA.txt"].Hash)
	require.Equal(t, hashContent(t, "newB"), collected["fileB.txt"].Hash)

	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.8.16
// TestHandleWrite_DeleteClearsTimer verifies that a Delete event cancels any
// pending write coalesce timer for the deleted path.
func TestHandleWrite_DeleteClearsTimer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := syncfixture.WriteTestFile(t, dir, "doomed.txt", "original")
	existingHash := hashContent(t, "original")

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "doomed.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: syncdomain.ItemTypeFile, LocalHash: existingHash,
	})

	mockWatcher := newMockFsWatcher()
	clock := syncfixture.NewManualClock(time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC))
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		Baseline:              baseline,
		AfterFunc:             clock.AfterFunc,
		WriteCoalesceCooldown: 200 * time.Millisecond,
	})

	batches := localBatches(obs, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir)

	// Modify and send Write event (starts 200ms timer).
	require.NoError(t, os.WriteFile(filePath, []byte("modified"), 0o600))

	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Write,
	}

	require.Eventually(t, func() bool {
		return clock.ActiveTimerCount() == 1
	}, syncfixture.EventWait, time.Millisecond)

	// Delete the file before timer fires.
	require.NoError(t, os.Remove(filePath))

	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Remove,
	}

	require.Eventually(t, func() bool {
		return clock.ActiveTimerCount() == 0
	}, syncfixture.EventWait, time.Millisecond)

	// Should get only the removal, not a row for the write.
	patch := waitForLocalPatch(t, batches, func(*LocalObservation) bool { return true }, "the delete's patch")
	require.Equal(t, []string{"doomed.txt"}, patch.DeletedPrefixes, "the first patch is the removal")
	require.Empty(t, patch.Rows)

	clock.Advance(400 * time.Millisecond)

	requireNoLocalPatch(t, batches, "the canceled write timer")

	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.8.3
// TestCancelPendingTimers verifies that all pending timers are cleaned up when
// the watchLoop exits (context cancellation).
func TestCancelPendingTimers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	syncfixture.WriteTestFile(t, dir, "pending1.txt", "data1")
	syncfixture.WriteTestFile(t, dir, "pending2.txt", "data2")

	baseline := syncfixture.BaselineWith(
		&syncdomain.BaselineEntry{
			Path: "pending1.txt", DriveID: driveid.New("d"), ItemID: "p1",
			ItemType: syncdomain.ItemTypeFile, LocalHash: hashContent(t, "old1"),
		},
		&syncdomain.BaselineEntry{
			Path: "pending2.txt", DriveID: driveid.New("d"), ItemID: "p2",
			ItemType: syncdomain.ItemTypeFile, LocalHash: hashContent(t, "old2"),
		},
	)

	mockWatcher := newMockFsWatcher()
	clock := syncfixture.NewManualClock(time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC))
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		Baseline:              baseline,
		AfterFunc:             clock.AfterFunc,
		WriteCoalesceCooldown: 5 * time.Second, // very long — timers should NOT fire
	})

	batches := localBatches(obs, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir)

	// Send Write events for two paths (creates timers with 5s cooldown).
	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filepath.Join(dir, "pending1.txt"), Op: fsnotify.Write,
	}
	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filepath.Join(dir, "pending2.txt"), Op: fsnotify.Write,
	}

	require.Eventually(t, func() bool {
		return clock.ActiveTimerCount() == 2
	}, syncfixture.EventWait, time.Millisecond)

	// Cancel context — should trigger cancelPendingTimers via defer.
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(syncfixture.EventWait):
		require.Fail(t, "Watch did not return after context cancellation")
	}

	assert.Zero(t, clock.ActiveTimerCount(), "all coalesce timers should be stopped on watch exit")

	// There should be no patch (timers were canceled).
	requireNoLocalPatch(t, batches, "the watch's exit")
}

// ---------------------------------------------------------------------------
// Hash retry cap tests
// ---------------------------------------------------------------------------

// Validates: R-2.8.16
// TestHashAndEmit_RetriesExhausted_EmitsEvent verifies that hashAndEmit emits
// an event (no re-schedule) when the request's retry count has reached the cap.
// This tests the MaxCoalesceRetries guard added to prevent infinite retry loops
// when a file is being written to continuously.
func TestHashAndEmit_RetriesExhausted_EmitsEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := syncfixture.WriteTestFile(t, dir, "exhausted.txt", "content")

	// Baseline with a DIFFERENT hash so the event is not suppressed.
	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "exhausted.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: syncdomain.ItemTypeFile, LocalHash: "old-hash",
	})

	obs := &LocalObserver{
		baseline:              baseline,
		logger:                synctest.TestLogger(t),
		writeCoalesceCooldown: 100 * time.Millisecond,
		localWatchState: localWatchState{
			PendingTimers: make(map[string]syncdomain.SyncTimer),
			HashRequests:  make(chan HashRequest, 10),
		},
		clock: &observerTestClock{SyncClock: syncdomain.RealClock{}, afterFunc: syncdomain.RealClock{}.AfterFunc},
	}

	batches := localBatches(obs, 5)
	ctx := t.Context()

	// Call hashAndEmit with retries at the cap. Even though the file is
	// stable (no ErrFileChangedDuringHash), this verifies the code path
	// works correctly at the retry boundary.
	obs.HashAndEmit(ctx, syncfixture.MustOpenSyncTree(t, dir), HashRequest{
		FsPath:    filePath,
		DbRelPath: "exhausted.txt",
		Retries:   maxCoalesceRetries,
	})

	row := waitForLocalRow(t, batches, "exhausted.txt")
	require.Equal(t, hashContent(t, "content"), row.Hash, "hash should be computed for stable file")

	// No timer should be pending — the request should not be re-scheduled.
	require.Empty(t, obs.PendingTimers, "no timer should be pending after exhausted retries")
}

// Validates: R-2.2.1
// TestHashAndEmit_BaselineMatch_NoEvent verifies that hashAndEmit does NOT emit
// an event when the file hash matches the baseline (no-op write detection).
func TestHashAndEmit_BaselineMatch_NoEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := syncfixture.WriteTestFile(t, dir, "noop.txt", "unchanged")
	hash := hashContent(t, "unchanged")

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "noop.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: syncdomain.ItemTypeFile, LocalHash: hash,
	})

	obs := &LocalObserver{
		baseline:              baseline,
		logger:                synctest.TestLogger(t),
		writeCoalesceCooldown: 100 * time.Millisecond,
		localWatchState: localWatchState{
			PendingTimers: make(map[string]syncdomain.SyncTimer),
			HashRequests:  make(chan HashRequest, 10),
		},
		clock: &observerTestClock{SyncClock: syncdomain.RealClock{}, afterFunc: syncdomain.RealClock{}.AfterFunc},
	}

	batches := localBatches(obs, 5)
	ctx := t.Context()

	obs.HashAndEmit(ctx, syncfixture.MustOpenSyncTree(t, dir), HashRequest{
		FsPath:    filePath,
		DbRelPath: "noop.txt",
	})

	// No patch should be sent since hash matches baseline.
	requireNoLocalPatch(t, batches, "the no-op write's hash")
}

// ---------------------------------------------------------------------------
// hashAndEmit case collision suppression (R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
func TestHashAndEmit_CaseCollision_Suppressed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create two files differing only in case.
	syncfixture.WriteTestFile(t, dir, "Existing.txt", "content1")
	filePath := syncfixture.WriteTestFile(t, dir, "existing.txt", "content2")

	// On case-insensitive FS, only one file exists — skip test.
	info1, err1 := os.Lstat(filepath.Join(dir, "Existing.txt"))
	info2, err2 := os.Lstat(filepath.Join(dir, "existing.txt"))
	if err1 != nil || err2 != nil || os.SameFile(info1, info2) {
		t.Skip("case-insensitive filesystem — cannot create case-colliding files")
	}

	baseline := syncfixture.BaselineWith(&syncdomain.BaselineEntry{
		Path: "existing.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: syncdomain.ItemTypeFile, LocalHash: hashContent(t, "old"),
	})

	mockWatcher := newMockFsWatcher()
	clock := syncfixture.NewManualClock(time.Date(2026, time.April, 13, 12, 0, 0, 0, time.UTC))
	recorder := syncfixture.NewDebugEventRecorder()
	obs := newWatchTestObserver(t, mockWatcher, watchObserverTestOptions{
		Baseline:              baseline,
		AfterFunc:             clock.AfterFunc,
		WriteCoalesceCooldown: 50 * time.Millisecond,
		DebugEvents:           recorder.Record,
	})

	batches := localBatches(obs, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir)

	// Send a Write event for the lowercase file.
	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Write,
	}

	require.Eventually(t, func() bool {
		return clock.ActiveTimerCount() == 1
	}, syncfixture.EventWait, time.Millisecond)

	clock.Advance(50 * time.Millisecond)

	// The case collision suppresses the hashed write: the observer reports
	// the skip and emits nothing.
	requireDroppedWithoutEvent(t, recorder, batches, collisionSkipOf("existing.txt"), "the collision skip of existing.txt's write")

	syncfixture.StopWatch(t, cancel, done)
}

// Validates: R-2.12.2
// Platform-independent hashAndEmit collision test.
// Uses pre-populated dirNameCache instead of relying on filesystem case sensitivity.
func TestHashAndEmit_CaseCollision_CachedLookup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create a single file — the collision is injected via the cache.
	syncfixture.WriteTestFile(t, dir, "existing.txt", "content")

	mockWatcher := newMockFsWatcher()
	recorder := syncfixture.NewDebugEventRecorder()
	obs := &LocalObserver{
		baseline:    syncfixture.EmptyBaseline(),
		logger:      synctest.TestLogger(t),
		debugEvents: recorder.Record,
		localWatchState: localWatchState{
			CollisionPeers: make(map[string]map[string]struct{}),
			// A differently cased sibling in the name cache simulates a
			// collision without requiring a case-sensitive FS.
			DirNameCache: map[string]map[namekey.Key][]string{
				dir: {
					"existing.txt": {"existing.txt", "Existing.txt"},
				},
			},
			PendingTimers: make(map[string]syncdomain.SyncTimer),
			HashRequests:  make(chan HashRequest, HashRequestBufSize),
		},
		writeCoalesceCooldown: 50 * time.Millisecond,
		clock: &observerTestClock{
			SyncClock: syncdomain.RealClock{},
			afterFunc: syncdomain.RealClock{}.AfterFunc,
			sleep: func(_ context.Context, _ time.Duration) error {
				return nil
			},
			tickCh: make(chan time.Time),
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
	}

	batches := localBatches(obs, 10)
	cancel, done := startMockWatch(t, obs, mockWatcher, dir)

	// Send a Write event for "existing.txt" — hashAndEmit should detect
	// the collision via the pre-populated cache and suppress the event.
	mockWatcher.EventsCh <- fsnotify.Event{
		Name: filepath.Join(dir, "existing.txt"),
		Op:   fsnotify.Write,
	}

	requireDroppedWithoutEvent(t, recorder, batches, collisionSkipOf("existing.txt"), "the collision skip found through the cache")

	syncfixture.StopWatch(t, cancel, done)
}
