package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// Hash failure modifies with empty hash (B-102) — Watch variant
// ---------------------------------------------------------------------------

// TestWatch_HashFailureModifyStillEmitsEvent verifies that a Write event for a
// file whose hash cannot be computed (e.g., write-only permissions) still
// generates a ChangeModify event with an empty hash instead of being silently
// dropped (B-102).
func TestWatch_HashFailureModifyStillEmitsEvent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	filePath := writeTestFile(t, dir, "watchable.txt", "original")
	existingHash := hashContent(t, "original")

	baseline := baselineWith(&BaselineEntry{
		Path: "watchable.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: existingHash,
	})

	obs := NewLocalObserver(baseline, testLogger(t), 0)
	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Let the watcher settle.
	time.Sleep(100 * time.Millisecond)

	// Make file write-only (stat succeeds, hash computation fails).
	require.NoError(t, os.Chmod(filePath, 0o200))
	t.Cleanup(func() { _ = os.Chmod(filePath, 0o644) })

	// Write new content — triggers Write event. os.WriteFile opens O_WRONLY
	// which succeeds with 0o200 permissions.
	require.NoError(t, os.WriteFile(filePath, []byte("modified"), 0o200))

	var ev ChangeEvent

	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for modify event")
	}

	cancel()
	<-done

	require.Equal(t, ChangeModify, ev.Type)
	require.Equal(t, "watchable.txt", ev.Path)
	require.Equal(t, SourceLocal, ev.Source)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
}

// ---------------------------------------------------------------------------
// Write coalescing tests (B-107)
// ---------------------------------------------------------------------------

// TestHandleWrite_CoalescesRapidWrites verifies that two Write events for the
// same path within the cooldown window produce only one hash + one ChangeEvent.
func TestHandleWrite_CoalescesRapidWrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := writeTestFile(t, dir, "rapid.txt", "v1")
	existingHash := hashContent(t, "v1")

	baseline := baselineWith(&BaselineEntry{
		Path: "rapid.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: existingHash,
	})

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 100 * time.Millisecond,
		sleepFunc:             func(_ context.Context, _ time.Duration) error { return nil },
		safetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
		pendingTimers: make(map[string]*time.Timer),
		hashRequests:  make(chan hashRequest, hashRequestBufSize),
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Write new content so the hash differs from baseline.
	require.NoError(t, os.WriteFile(filePath, []byte("v2"), 0o644))

	// Send two rapid Write events (within 100ms cooldown).
	mockWatcher.events <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Write,
	}

	time.Sleep(20 * time.Millisecond)

	mockWatcher.events <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Write,
	}

	// Wait for the single coalesced event.
	select {
	case ev := <-events:
		require.Equal(t, ChangeModify, ev.Type)
		require.Equal(t, "rapid.txt", ev.Path)
		require.Equal(t, hashContent(t, "v2"), ev.Hash)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for coalesced modify event")
	}

	// Verify no second event arrives within a reasonable window.
	select {
	case ev := <-events:
		require.Fail(t, "unexpected second event", "%+v", ev)
	case <-time.After(300 * time.Millisecond):
		// Good — only one event.
	}

	cancel()
	<-done
}

// TestHandleWrite_EmitsAfterCooldownExpires verifies that a single Write event
// produces a ChangeModify event after the cooldown expires.
func TestHandleWrite_EmitsAfterCooldownExpires(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "single.txt", "original")
	existingHash := hashContent(t, "original")

	baseline := baselineWith(&BaselineEntry{
		Path: "single.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: existingHash,
	})

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 50 * time.Millisecond,
		sleepFunc:             func(_ context.Context, _ time.Duration) error { return nil },
		safetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
		pendingTimers: make(map[string]*time.Timer),
		hashRequests:  make(chan hashRequest, hashRequestBufSize),
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Write new content.
	filePath := filepath.Join(dir, "single.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("modified"), 0o644))

	mockWatcher.events <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Write,
	}

	select {
	case ev := <-events:
		require.Equal(t, ChangeModify, ev.Type)
		require.Equal(t, "single.txt", ev.Path)
		require.Equal(t, hashContent(t, "modified"), ev.Hash)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for modify event after cooldown")
	}

	cancel()
	<-done
}

// TestHandleWrite_DifferentPathsNotCoalesced verifies that Write events for
// different paths are independent — each gets its own timer and emits separately.
func TestHandleWrite_DifferentPathsNotCoalesced(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "fileA.txt", "origA")
	writeTestFile(t, dir, "fileB.txt", "origB")

	baseline := baselineWith(
		&BaselineEntry{
			Path: "fileA.txt", DriveID: driveid.New("d"), ItemID: "a1",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "origA"),
		},
		&BaselineEntry{
			Path: "fileB.txt", DriveID: driveid.New("d"), ItemID: "b1",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "origB"),
		},
	)

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 50 * time.Millisecond,
		sleepFunc:             func(_ context.Context, _ time.Duration) error { return nil },
		safetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
		pendingTimers: make(map[string]*time.Timer),
		hashRequests:  make(chan hashRequest, hashRequestBufSize),
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Write new content to both files.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fileA.txt"), []byte("newA"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fileB.txt"), []byte("newB"), 0o644))

	// Send Write events for both paths within the same cooldown window.
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "fileA.txt"), Op: fsnotify.Write,
	}
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "fileB.txt"), Op: fsnotify.Write,
	}

	// Collect both events.
	collected := make(map[string]ChangeEvent)
	timeout := time.After(5 * time.Second)

	for len(collected) < 2 {
		select {
		case ev := <-events:
			if ev.Type == ChangeModify {
				collected[ev.Path] = ev
			}
		case <-timeout:
			require.Fail(t, "timeout", "got %d events, want 2", len(collected))
		}
	}

	require.Contains(t, collected, "fileA.txt")
	require.Contains(t, collected, "fileB.txt")
	require.Equal(t, hashContent(t, "newA"), collected["fileA.txt"].Hash)
	require.Equal(t, hashContent(t, "newB"), collected["fileB.txt"].Hash)

	cancel()
	<-done
}

// TestHandleWrite_DeleteClearsTimer verifies that a Delete event cancels any
// pending write coalesce timer for the deleted path.
func TestHandleWrite_DeleteClearsTimer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := writeTestFile(t, dir, "doomed.txt", "original")
	existingHash := hashContent(t, "original")

	baseline := baselineWith(&BaselineEntry{
		Path: "doomed.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: existingHash,
	})

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 200 * time.Millisecond,
		sleepFunc:             func(_ context.Context, _ time.Duration) error { return nil },
		safetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
		pendingTimers: make(map[string]*time.Timer),
		hashRequests:  make(chan hashRequest, hashRequestBufSize),
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Modify and send Write event (starts 200ms timer).
	require.NoError(t, os.WriteFile(filePath, []byte("modified"), 0o644))

	mockWatcher.events <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Write,
	}

	// Give watchLoop time to process the Write event.
	time.Sleep(20 * time.Millisecond)

	// Delete the file before timer fires.
	require.NoError(t, os.Remove(filePath))

	mockWatcher.events <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Remove,
	}

	// Should get only the Delete event, not a Modify.
	select {
	case ev := <-events:
		require.Equal(t, ChangeDelete, ev.Type, "first event should be Delete")
		require.Equal(t, "doomed.txt", ev.Path)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timeout waiting for delete event")
	}

	// Wait past the original timer window — no Modify should appear.
	select {
	case ev := <-events:
		require.Fail(t, "unexpected event after delete", "%+v", ev)
	case <-time.After(400 * time.Millisecond):
		// Good — timer was canceled.
	}

	cancel()
	<-done
}

// TestCancelPendingTimers verifies that all pending timers are cleaned up when
// the watchLoop exits (context cancellation).
func TestCancelPendingTimers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "pending1.txt", "data1")
	writeTestFile(t, dir, "pending2.txt", "data2")

	baseline := baselineWith(
		&BaselineEntry{
			Path: "pending1.txt", DriveID: driveid.New("d"), ItemID: "p1",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "old1"),
		},
		&BaselineEntry{
			Path: "pending2.txt", DriveID: driveid.New("d"), ItemID: "p2",
			ItemType: ItemTypeFile, LocalHash: hashContent(t, "old2"),
		},
	)

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 5 * time.Second, // very long — timers should NOT fire
		sleepFunc:             func(_ context.Context, _ time.Duration) error { return nil },
		safetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
		pendingTimers: make(map[string]*time.Timer),
		hashRequests:  make(chan hashRequest, hashRequestBufSize),
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Send Write events for two paths (creates timers with 5s cooldown).
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "pending1.txt"), Op: fsnotify.Write,
	}
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "pending2.txt"), Op: fsnotify.Write,
	}

	// Give watchLoop time to process both events and create timers.
	time.Sleep(50 * time.Millisecond)

	// Cancel context — should trigger cancelPendingTimers via defer.
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "Watch did not return after context cancellation")
	}

	// Drain any events — there should be none (timers were canceled).
	select {
	case ev := <-events:
		require.Fail(t, "unexpected event after cancellation", "%+v", ev)
	default:
		// Good — no events leaked.
	}
}

// ---------------------------------------------------------------------------
// Hash retry cap tests
// ---------------------------------------------------------------------------

// TestHashAndEmit_RetriesExhausted_EmitsEvent verifies that hashAndEmit emits
// an event (no re-schedule) when the request's retry count has reached the cap.
// This tests the maxCoalesceRetries guard added to prevent infinite retry loops
// when a file is being written to continuously.
func TestHashAndEmit_RetriesExhausted_EmitsEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := writeTestFile(t, dir, "exhausted.txt", "content")

	// Baseline with a DIFFERENT hash so the event is not suppressed.
	baseline := baselineWith(&BaselineEntry{
		Path: "exhausted.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: "old-hash",
	})

	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 100 * time.Millisecond,
		pendingTimers:         make(map[string]*time.Timer),
		hashRequests:          make(chan hashRequest, 10),
	}

	events := make(chan ChangeEvent, 5)
	ctx := t.Context()

	// Call hashAndEmit with retries at the cap. Even though the file is
	// stable (no errFileChangedDuringHash), this verifies the code path
	// works correctly at the retry boundary.
	obs.hashAndEmit(ctx, hashRequest{
		fsPath:    filePath,
		dbRelPath: "exhausted.txt",
		name:      "exhausted.txt",
		retries:   maxCoalesceRetries,
	}, events)

	select {
	case ev := <-events:
		require.Equal(t, ChangeModify, ev.Type)
		require.Equal(t, "exhausted.txt", ev.Path)
		require.NotEmpty(t, ev.Hash, "hash should be computed for stable file")
	case <-time.After(time.Second):
		require.Fail(t, "expected event from hashAndEmit, got none")
	}

	// No timer should be pending — the request should not be re-scheduled.
	require.Empty(t, obs.pendingTimers, "no timer should be pending after exhausted retries")
}

// TestHashAndEmit_BaselineMatch_NoEvent verifies that hashAndEmit does NOT emit
// an event when the file hash matches the baseline (no-op write detection).
func TestHashAndEmit_BaselineMatch_NoEvent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	filePath := writeTestFile(t, dir, "noop.txt", "unchanged")
	hash := hashContent(t, "unchanged")

	baseline := baselineWith(&BaselineEntry{
		Path: "noop.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hash,
	})

	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 100 * time.Millisecond,
		pendingTimers:         make(map[string]*time.Timer),
		hashRequests:          make(chan hashRequest, 10),
	}

	events := make(chan ChangeEvent, 5)
	ctx := t.Context()

	obs.hashAndEmit(ctx, hashRequest{
		fsPath:    filePath,
		dbRelPath: "noop.txt",
		name:      "noop.txt",
	}, events)

	// No event should be emitted since hash matches baseline.
	select {
	case ev := <-events:
		require.Fail(t, "unexpected event for unchanged file", "%+v", ev)
	default:
		// Good — no event for no-op write.
	}
}

// ---------------------------------------------------------------------------
// hashAndEmit case collision suppression (R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
func TestHashAndEmit_CaseCollision_Suppressed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create two files differing only in case.
	writeTestFile(t, dir, "Existing.txt", "content1")
	filePath := writeTestFile(t, dir, "existing.txt", "content2")

	// On case-insensitive FS, only one file exists — skip test.
	info1, err1 := os.Lstat(filepath.Join(dir, "Existing.txt"))
	info2, err2 := os.Lstat(filepath.Join(dir, "existing.txt"))
	if err1 != nil || err2 != nil || os.SameFile(info1, info2) {
		t.Skip("case-insensitive filesystem — cannot create case-colliding files")
	}

	baseline := baselineWith(&BaselineEntry{
		Path: "existing.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: hashContent(t, "old"),
	})

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline:              baseline,
		logger:                testLogger(t),
		writeCoalesceCooldown: 50 * time.Millisecond,
		sleepFunc:             func(_ context.Context, _ time.Duration) error { return nil },
		safetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
		pendingTimers: make(map[string]*time.Timer),
		hashRequests:  make(chan hashRequest, hashRequestBufSize),
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Send a Write event for the lowercase file.
	mockWatcher.events <- fsnotify.Event{
		Name: filePath, Op: fsnotify.Write,
	}

	// No event should be emitted — the case collision suppresses it.
	select {
	case ev := <-events:
		t.Fatalf("expected no event due to case collision, got %+v", ev)
	case <-time.After(500 * time.Millisecond):
		// Good — no event emitted.
	}

	cancel()
	<-done
}

// Validates: R-2.12.2 — platform-independent hashAndEmit collision test.
// Uses pre-populated dirNameCache instead of relying on filesystem case sensitivity.
func TestHashAndEmit_CaseCollision_CachedLookup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create a single file — the collision is injected via the cache.
	writeTestFile(t, dir, "existing.txt", "content")

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline:       emptyBaseline(),
		logger:         testLogger(t),
		collisionPeers: make(map[string]map[string]struct{}),
		// Pre-populate dirNameCache with a different-cased entry.
		// This simulates a collision without requiring a case-sensitive FS.
		dirNameCache: map[string]map[string][]string{
			dir: {
				"existing.txt": {"existing.txt", "Existing.txt"},
			},
		},
		writeCoalesceCooldown: 50 * time.Millisecond,
		sleepFunc: func(_ context.Context, _ time.Duration) error {
			return nil
		},
		safetyTickFunc: func(time.Duration) (<-chan time.Time, func()) {
			return make(chan time.Time), func() {}
		},
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
		pendingTimers: make(map[string]*time.Timer),
		hashRequests:  make(chan hashRequest, hashRequestBufSize),
	}

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Wait for watcher setup.
	time.Sleep(100 * time.Millisecond)

	// Send a Write event for "existing.txt" — hashAndEmit should detect
	// the collision via the pre-populated cache and suppress the event.
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "existing.txt"),
		Op:   fsnotify.Write,
	}

	// Wait for write coalesce cooldown + processing.
	select {
	case ev := <-events:
		t.Fatalf("expected no event (collision via cached lookup), got %+v", ev)
	case <-time.After(500 * time.Millisecond):
		// Good — no event emitted.
	}

	cancel()
	<-done
}
