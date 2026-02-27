package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// Mock watcher for unit-testing watchLoop
// ---------------------------------------------------------------------------

// mockFsWatcher implements FsWatcher with injectable channels for testing.
type mockFsWatcher struct {
	events chan fsnotify.Event
	errs   chan error
}

func newMockFsWatcher() *mockFsWatcher {
	return &mockFsWatcher{
		events: make(chan fsnotify.Event, 10),
		errs:   make(chan error, 10),
	}
}

func (m *mockFsWatcher) Add(string) error              { return nil }
func (m *mockFsWatcher) Remove(string) error           { return nil }
func (m *mockFsWatcher) Close() error                  { close(m.events); close(m.errs); return nil }
func (m *mockFsWatcher) Events() <-chan fsnotify.Event { return m.events }
func (m *mockFsWatcher) Errors() <-chan error          { return m.errs }

// sleepRecorder captures durations passed to sleepFunc.
type sleepRecorder struct {
	mu       stdsync.Mutex
	calls    []time.Duration
	notifyCh chan struct{} // closed after each call to wake waiters
}

func newSleepRecorder() *sleepRecorder {
	return &sleepRecorder{notifyCh: make(chan struct{})}
}

func (s *sleepRecorder) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.calls = append(s.calls, d)
	ch := s.notifyCh
	s.notifyCh = make(chan struct{})
	s.mu.Unlock()

	close(ch) // notify waiters

	return nil
}

// waitForCalls blocks until at least n sleep calls have been recorded.
func (s *sleepRecorder) waitForCalls(t *testing.T, n int) {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		s.mu.Lock()
		count := len(s.calls)
		ch := s.notifyCh
		s.mu.Unlock()

		if count >= n {
			return
		}

		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timeout waiting for %d sleep calls (got %d)", n, count)
		}
	}
}

func (s *sleepRecorder) getCalls() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]time.Duration, len(s.calls))
	copy(result, s.calls)

	return result
}

// ---------------------------------------------------------------------------
// Watch tests
// ---------------------------------------------------------------------------

func TestWatch_DetectsFileCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t))

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Let the watcher settle, then create a file.
	time.Sleep(100 * time.Millisecond)
	writeTestFile(t, dir, "new-file.txt", "hello watch")

	var ev ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for create event")
	}

	cancel()
	<-done

	if ev.Type != ChangeCreate {
		t.Errorf("Type = %v, want ChangeCreate", ev.Type)
	}

	if ev.Path != "new-file.txt" {
		t.Errorf("Path = %q, want %q", ev.Path, "new-file.txt")
	}

	if ev.Source != SourceLocal {
		t.Errorf("Source = %v, want SourceLocal", ev.Source)
	}

	if ev.Hash == "" {
		t.Error("Hash should be non-empty for a file create")
	}
}

func TestWatch_DetectsFileModify(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "existing.txt", "original")
	existingHash := hashContent(t, "original")

	baseline := baselineWith(&BaselineEntry{
		Path: "existing.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile, LocalHash: existingHash,
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Modify the file.
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var ev ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for modify event")
	}

	cancel()
	<-done

	if ev.Type != ChangeModify {
		t.Errorf("Type = %v, want ChangeModify", ev.Type)
	}

	if ev.Path != "existing.txt" {
		t.Errorf("Path = %q, want %q", ev.Path, "existing.txt")
	}

	if ev.Hash != hashContent(t, "modified") {
		t.Errorf("Hash = %q, want %q", ev.Hash, hashContent(t, "modified"))
	}
}

func TestWatch_DetectsFileDelete(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, "doomed.txt", "goodbye")

	baseline := baselineWith(&BaselineEntry{
		Path: "doomed.txt", DriveID: driveid.New("d"), ItemID: "i1",
		ItemType: ItemTypeFile,
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	if err := os.Remove(filepath.Join(dir, "doomed.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var ev ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for delete event")
	}

	cancel()
	<-done

	if ev.Type != ChangeDelete {
		t.Errorf("Type = %v, want ChangeDelete", ev.Type)
	}

	if ev.Path != "doomed.txt" {
		t.Errorf("Path = %q, want %q", ev.Path, "doomed.txt")
	}

	if !ev.IsDeleted {
		t.Error("IsDeleted = false, want true")
	}
}

// TestWatch_DeleteDirectoryRemovesWatch verifies that deleting a watched
// directory emits a ChangeDelete event and the watch continues to function
// normally for other events (B-112).
func TestWatch_DeleteDirectoryRemovesWatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))

	baseline := baselineWith(&BaselineEntry{
		Path: "subdir", DriveID: driveid.New("d"), ItemID: "d1",
		ItemType: ItemTypeFolder,
	})

	obs := NewLocalObserver(baseline, testLogger(t))
	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Delete the subdirectory.
	require.NoError(t, os.Remove(subDir))

	var ev ChangeEvent

	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for delete event")
	}

	cancel()
	<-done

	require.Equal(t, ChangeDelete, ev.Type)
	require.Equal(t, "subdir", ev.Path)
	require.Equal(t, ItemTypeFolder, ev.ItemType)
	require.True(t, ev.IsDeleted)
}

func TestWatch_IgnoresExcludedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t))

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create an excluded file — should not produce an event.
	writeTestFile(t, dir, "temp.tmp", "temporary")

	// Then create a valid file — should produce an event.
	time.Sleep(50 * time.Millisecond)
	writeTestFile(t, dir, "valid.txt", "keep")

	var ev ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for any event")
	}

	cancel()
	<-done

	if ev.Path != "valid.txt" {
		t.Errorf("Path = %q, want %q (excluded file should be ignored)", ev.Path, "valid.txt")
	}
}

func TestWatch_NosyncGuard(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, dir, ".nosync", "")

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events := make(chan ChangeEvent, 10)

	err := obs.Watch(context.Background(), dir, events)
	if !errors.Is(err, ErrNosyncGuard) {
		t.Errorf("err = %v, want ErrNosyncGuard", err)
	}
}

func TestWatch_NewDirectoryWatched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t))

	events := make(chan ChangeEvent, 20)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a subdirectory and a file inside it.
	subDir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// Give the watcher time to add the new directory watch.
	time.Sleep(200 * time.Millisecond)

	writeTestFile(t, dir, "subdir/inner.txt", "nested")

	// Collect events until we find the file inside the subdirectory.
	var foundInnerFile bool

	timeout := time.After(5 * time.Second)

	for !foundInnerFile {
		select {
		case ev := <-events:
			if ev.Path == "subdir/inner.txt" {
				foundInnerFile = true
			}
		case <-timeout:
			cancel()
			<-done
			t.Fatal("timeout waiting for inner file event")
		}
	}

	cancel()
	<-done

	if !foundInnerFile {
		t.Error("inner file event not received")
	}
}

// TestWatch_NewDirectoryPreExistingFiles verifies that files already present
// in a newly-created directory are detected immediately (not deferred to the
// next safety scan). Regression test for B-100.
func TestWatch_NewDirectoryPreExistingFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t))

	events := make(chan ChangeEvent, 30)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a directory with a file already inside it using os.MkdirAll +
	// os.WriteFile atomically (from the watcher's perspective, the directory
	// create event fires, and the file is already present when handleCreate
	// runs scanNewDirectory).
	subDir := filepath.Join(dir, "pre-populated")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	preExistingFile := filepath.Join(subDir, "already-here.txt")
	if err := os.WriteFile(preExistingFile, []byte("pre-existing"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Collect events. The file should appear without a separate fsnotify event
	// because scanNewDirectory picks it up during directory creation handling.
	var foundPreExisting bool

	timeout := time.After(5 * time.Second)

	for !foundPreExisting {
		select {
		case ev := <-events:
			if ev.Path == "pre-populated/already-here.txt" && ev.Type == ChangeCreate {
				foundPreExisting = true
			}
		case <-timeout:
			cancel()
			<-done
			t.Fatal("timeout waiting for pre-existing file event (B-100)")
		}
	}

	cancel()
	<-done

	if !foundPreExisting {
		t.Error("pre-existing file in new directory was not detected")
	}
}

func TestLocalWatch_ContextCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t))

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Let the watcher start, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// trySend tests
// ---------------------------------------------------------------------------

func TestTrySend_ChannelAvailable_SendsEvent(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events := make(chan ChangeEvent, 1)
	ctx := context.Background()

	ev := ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate, Path: "test.txt",
		ItemType: ItemTypeFile,
	}

	obs.trySend(ctx, events, &ev)

	select {
	case got := <-events:
		if got.Path != "test.txt" {
			t.Errorf("got path %q, want %q", got.Path, "test.txt")
		}
	default:
		t.Fatal("expected event on channel")
	}

	if obs.DroppedEvents() != 0 {
		t.Errorf("DroppedEvents() = %d, want 0", obs.DroppedEvents())
	}
}

func TestTrySend_ChannelFull_DropsEvent(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events := make(chan ChangeEvent, 1)
	ctx := context.Background()

	// Fill the channel.
	first := ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate, Path: "first.txt",
		ItemType: ItemTypeFile,
	}
	events <- first

	// This should be dropped (channel full).
	second := ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate, Path: "second.txt",
		ItemType: ItemTypeFile,
	}

	obs.trySend(ctx, events, &second)

	if obs.DroppedEvents() != 1 {
		t.Errorf("DroppedEvents() = %d, want 1", obs.DroppedEvents())
	}

	// ResetDroppedEvents returns the count and resets to 0 (B-190).
	reset := obs.ResetDroppedEvents()
	if reset != 1 {
		t.Errorf("ResetDroppedEvents() = %d, want 1", reset)
	}

	if obs.DroppedEvents() != 0 {
		t.Errorf("DroppedEvents() after reset = %d, want 0", obs.DroppedEvents())
	}

	// Original event still in channel.
	got := <-events
	if got.Path != "first.txt" {
		t.Errorf("channel event path = %q, want %q", got.Path, "first.txt")
	}
}

func TestTrySend_ContextCanceled_NoDrop(t *testing.T) {
	t.Parallel()

	obs := NewLocalObserver(emptyBaseline(), testLogger(t))
	events := make(chan ChangeEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Fill the channel so default branch would fire, but ctx is canceled.
	events <- ChangeEvent{Path: "fill.txt"}

	ev := ChangeEvent{
		Source: SourceLocal, Type: ChangeCreate, Path: "test.txt",
		ItemType: ItemTypeFile,
	}

	obs.trySend(ctx, events, &ev)

	// Context cancel takes priority over default branch in select, but
	// Go's select is non-deterministic. The drop counter may or may not
	// increment. The key invariant is: trySend must not block.
	// Just verify it returned (no deadlock).
}

// ---------------------------------------------------------------------------
// Recursive scanNewDirectory tests
// ---------------------------------------------------------------------------

func TestWatch_NewDirectoryNestedRecursion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t))

	events := make(chan ChangeEvent, 50)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a 3-level nested directory structure with a file at the bottom.
	deepDir := filepath.Join(dir, "level1", "level2", "level3")
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	deepFile := filepath.Join(deepDir, "deep-file.txt")
	if err := os.WriteFile(deepFile, []byte("deep content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Collect events — look for the deep file and all intermediate directories.
	foundDirs := make(map[string]bool)
	foundFile := false

	wantDirs := map[string]bool{
		"level1":               true,
		"level1/level2":        true,
		"level1/level2/level3": true,
	}

	timeout := time.After(5 * time.Second)

	for !foundFile || len(foundDirs) < len(wantDirs) {
		select {
		case ev := <-events:
			if ev.Type == ChangeCreate {
				if ev.ItemType == ItemTypeFolder && wantDirs[ev.Path] {
					foundDirs[ev.Path] = true
				}

				if ev.Path == "level1/level2/level3/deep-file.txt" && ev.ItemType == ItemTypeFile {
					foundFile = true
				}
			}
		case <-timeout:
			cancel()
			<-done
			t.Fatalf("timeout: foundFile=%v, foundDirs=%v (want %v)",
				foundFile, foundDirs, wantDirs)
		}
	}

	cancel()
	<-done

	if !foundFile {
		t.Error("deep file not detected")
	}

	for d := range wantDirs {
		if !foundDirs[d] {
			t.Errorf("directory %q not detected", d)
		}
	}
}

// ---------------------------------------------------------------------------
// Hash failure creates with empty hash (B-102) — Watch variant
// ---------------------------------------------------------------------------

// TestWatch_HashFailureStillEmitsCreate verifies that a file whose hash cannot
// be computed (e.g., unreadable) still generates a ChangeCreate event with an
// empty hash instead of being silently dropped (B-102).
func TestWatch_HashFailureStillEmitsCreate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t))

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Let the watcher settle, then create an unreadable file.
	time.Sleep(100 * time.Millisecond)
	path := writeTestFile(t, dir, "unreadable.txt", "secret")
	require.NoError(t, os.Chmod(path, 0o000))

	t.Cleanup(func() {
		// Restore permissions so TempDir cleanup works.
		_ = os.Chmod(path, 0o644)
	})

	var ev ChangeEvent

	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timeout waiting for create event")
	}

	cancel()
	<-done

	require.Equal(t, ChangeCreate, ev.Type)
	require.Equal(t, "unreadable.txt", ev.Path)
	require.Equal(t, SourceLocal, ev.Source)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
}

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

	obs := NewLocalObserver(baseline, testLogger(t))
	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

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
		t.Fatal("timeout waiting for modify event")
	}

	cancel()
	<-done

	require.Equal(t, ChangeModify, ev.Type)
	require.Equal(t, "watchable.txt", ev.Path)
	require.Equal(t, SourceLocal, ev.Source)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
}

// ---------------------------------------------------------------------------
// watchLoop backoff reset tests (B-189)
// ---------------------------------------------------------------------------

// TestWatchLoop_BackoffResetsOnSafetyScan verifies that after a safety scan
// fires, the error backoff resets to the initial value. Without this reset,
// a previous error escalation would persist, causing unnecessarily long
// waits after unrelated safety scans.
func TestWatchLoop_BackoffResetsOnSafetyScan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	recorder := newSleepRecorder()
	mockWatcher := newMockFsWatcher()

	obs := &LocalObserver{
		baseline:           emptyBaseline(),
		logger:             testLogger(t),
		safetyScanOverride: 200 * time.Millisecond,
		sleepFunc:          recorder.sleep,
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
	}

	events := make(chan ChangeEvent, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Step 1: Inject a watcher error → watchLoop sleeps with initial backoff (1s).
	mockWatcher.errs <- errors.New("test watcher error 1")
	recorder.waitForCalls(t, 1)

	calls := recorder.getCalls()
	require.Equal(t, watchErrInitBackoff, calls[0],
		"first error should use initial backoff")

	// Step 2: Wait for the safety scan to fire (200ms override).
	// The safety scan resets errBackoff to watchErrInitBackoff.
	time.Sleep(300 * time.Millisecond)

	// Step 3: Inject another watcher error → should use initial backoff (1s),
	// NOT the escalated value (2s), because the safety scan reset it.
	mockWatcher.errs <- errors.New("test watcher error 2")
	recorder.waitForCalls(t, 2)

	calls = recorder.getCalls()
	require.Equal(t, watchErrInitBackoff, calls[1],
		"second error after safety scan should use initial backoff (reset)")

	cancel()
	<-done
}

// TestWatchLoop_BackoffEscalatesWithoutReset verifies that without a safety
// scan, consecutive errors escalate the backoff exponentially.
func TestWatchLoop_BackoffEscalatesWithoutReset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	recorder := newSleepRecorder()
	mockWatcher := newMockFsWatcher()

	obs := &LocalObserver{
		baseline:           emptyBaseline(),
		logger:             testLogger(t),
		safetyScanOverride: 10 * time.Minute, // long enough to never fire
		sleepFunc:          recorder.sleep,
		watcherFactory: func() (FsWatcher, error) {
			return mockWatcher, nil
		},
	}

	events := make(chan ChangeEvent, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Inject two consecutive errors.
	mockWatcher.errs <- errors.New("error 1")
	recorder.waitForCalls(t, 1)

	mockWatcher.errs <- errors.New("error 2")
	recorder.waitForCalls(t, 2)

	calls := recorder.getCalls()
	require.Equal(t, watchErrInitBackoff, calls[0], "first error: initial backoff")
	require.Equal(t, watchErrInitBackoff*watchErrBackoffMult, calls[1],
		"second error: escalated backoff (no safety scan to reset)")

	cancel()
	<-done
}
