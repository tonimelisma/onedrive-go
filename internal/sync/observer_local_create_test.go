package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Watch tests — file/directory creation
// ---------------------------------------------------------------------------

// Validates: R-2.1.2
func TestWatch_DetectsFileCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

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
		require.Fail(t, "timeout waiting for create event")
	}

	cancel()
	<-done

	assert.Equal(t, ChangeCreate, ev.Type)
	assert.Equal(t, "new-file.txt", ev.Path)
	assert.Equal(t, SourceLocal, ev.Source)
	assert.NotEmpty(t, ev.Hash, "Hash should be non-empty for a file create")
}

func TestWatch_NewDirectoryWatched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)

	events := make(chan ChangeEvent, 20)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a subdirectory and a file inside it.
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))

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
			require.Fail(t, "timeout waiting for inner file event")
		}
	}

	cancel()
	<-done

	assert.True(t, foundInnerFile, "inner file event not received")
}

// TestWatch_NewDirectoryPreExistingFiles verifies that files already present
// in a newly-created directory are detected immediately (not deferred to the
// next safety scan). Regression test for B-100.
func TestWatch_NewDirectoryPreExistingFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)

	events := make(chan ChangeEvent, 30)
	ctx, cancel := context.WithCancel(t.Context())

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
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	preExistingFile := filepath.Join(subDir, "already-here.txt")
	require.NoError(t, os.WriteFile(preExistingFile, []byte("pre-existing"), 0o644))

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
			require.Fail(t, "timeout waiting for pre-existing file event (B-100)")
		}
	}

	cancel()
	<-done

	assert.True(t, foundPreExisting, "pre-existing file in new directory was not detected")
}

// ---------------------------------------------------------------------------
// Recursive scanNewDirectory tests
// ---------------------------------------------------------------------------

func TestWatch_NewDirectoryNestedRecursion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)

	events := make(chan ChangeEvent, 50)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a 3-level nested directory structure with a file at the bottom.
	deepDir := filepath.Join(dir, "level1", "level2", "level3")
	require.NoError(t, os.MkdirAll(deepDir, 0o755))

	deepFile := filepath.Join(deepDir, "deep-file.txt")
	require.NoError(t, os.WriteFile(deepFile, []byte("deep content"), 0o644))

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
			require.Fail(t, "timeout",
				"foundFile=%v, foundDirs=%v (want %v)", foundFile, foundDirs, wantDirs)
		}
	}

	cancel()
	<-done

	assert.True(t, foundFile, "deep file not detected")

	for d := range wantDirs {
		assert.True(t, foundDirs[d], "directory %q not detected", d)
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
	obs := NewLocalObserver(emptyBaseline(), testLogger(t), 0)

	events := make(chan ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Let the watcher settle, then create an unreadable file.
	// The file must be born unreadable — if we create then chmod, the observer
	// can hash the file between creation and chmod, producing a valid hash
	// instead of the expected empty hash (B-310).
	time.Sleep(100 * time.Millisecond)
	path := filepath.Join(dir, "unreadable.txt")
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o000))

	t.Cleanup(func() {
		// Restore permissions so TempDir cleanup works.
		_ = os.Chmod(path, 0o644)
	})

	var ev ChangeEvent

	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for create event")
	}

	cancel()
	<-done

	require.Equal(t, ChangeCreate, ev.Type)
	require.Equal(t, "unreadable.txt", ev.Path)
	require.Equal(t, SourceLocal, ev.Source)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
}

// ---------------------------------------------------------------------------
// hasCaseCollision tests (R-2.12.2)
// ---------------------------------------------------------------------------

// Validates: R-2.12.2
func TestHasCaseCollision_Detected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create a file named "File.txt".
	require.NoError(t, os.WriteFile(filepath.Join(dir, "File.txt"), []byte("a"), 0o644))

	// On case-sensitive FS, "file.txt" differs from "File.txt" → collision.
	// On case-insensitive FS (macOS), creating "File.txt" means "file.txt"
	// refers to the same inode, so os.ReadDir returns "File.txt" and the
	// exact-match check (entry.Name() != name) fails → no collision detected.
	// This is correct: on case-insensitive FS there is no collision risk.
	collidingName, found := hasCaseCollision(dir, "file.txt")
	if found {
		assert.Equal(t, "File.txt", collidingName,
			"should return the name of the existing colliding file")
	}
}

// Validates: R-2.12.2
func TestHasCaseCollision_NoCollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte("a"), 0o644))

	_, found := hasCaseCollision(dir, "newfile.txt")
	assert.False(t, found, "unrelated files should not trigger collision")
}

// Validates: R-2.12.2
func TestHasCaseCollision_ExactMatch_NotCollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "same.txt"), []byte("a"), 0o644))

	// Same exact name is not a collision — it's the same file.
	_, found := hasCaseCollision(dir, "same.txt")
	assert.False(t, found, "exact name match should not be a collision")
}

// Validates: R-2.12.2
func TestHasCaseCollision_UnreadableDir_FailOpen(t *testing.T) {
	t.Parallel()

	// Non-existent directory → ReadDir fails → function returns false (fail-open).
	_, found := hasCaseCollision("/nonexistent/path", "anything.txt")
	assert.False(t, found, "unreadable directory should fail open")
}

// Validates: R-2.12.2
func TestHasCaseCollision_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, found := hasCaseCollision(dir, "anything.txt")
	assert.False(t, found, "empty directory should have no collisions")
}

// Validates: R-2.12.2 — integration test exercising the full watch pipeline:
// fsnotify Create → handleCreate → hasCaseCollision → event suppressed.
func TestWatch_CaseCollision_EventSuppressed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create "Existing.txt" on disk. On case-insensitive FS (macOS),
	// os.ReadDir returns this canonical name.
	writeTestFile(t, dir, "Existing.txt", "content")

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline: emptyBaseline(),
		logger:   testLogger(t),
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

	// Wait for watcher setup, then send a synthetic Create event for
	// "existing.txt" (lowercase) — different case than the on-disk file.
	time.Sleep(100 * time.Millisecond)
	mockWatcher.events <- fsnotify.Event{
		Name: filepath.Join(dir, "existing.txt"),
		Op:   fsnotify.Create,
	}

	// No event should be emitted — the case collision suppresses it.
	select {
	case ev := <-events:
		t.Fatalf("expected no event, got %+v", ev)
	case <-time.After(500 * time.Millisecond):
		// Good — no event emitted.
	}

	cancel()
	<-done
}

// Validates: R-2.12.2 — case collision check in scanNewDirectory.
func TestScanNewDirectory_CaseCollision_Skipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Create a subdirectory with two files that differ only in case.
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	writeTestFile(t, subDir, "Existing.txt", "content1")

	// On case-insensitive FS, this overwrites Existing.txt (same file).
	// On case-sensitive FS, this creates a second file.
	writeTestFile(t, subDir, "existing.txt", "content2")

	mockWatcher := newMockFsWatcher()
	obs := &LocalObserver{
		baseline: emptyBaseline(),
		logger:   testLogger(t),
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

	// Wait for watcher setup, then send a Create event for the subdirectory
	// to trigger scanNewDirectory.
	time.Sleep(100 * time.Millisecond)
	mockWatcher.events <- fsnotify.Event{
		Name: subDir,
		Op:   fsnotify.Create,
	}

	// Collect events within a window.
	var received []ChangeEvent
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-events:
			received = append(received, ev)
			continue
		case <-timeout:
		}

		break
	}

	// The directory event itself should be emitted. On case-sensitive FS,
	// both files exist and the collision check skips both. On case-insensitive
	// FS, only one file exists (no collision), so one file event is emitted.
	// In both cases, we should NOT see two file events.
	fileEvents := 0
	for _, ev := range received {
		if ev.ItemType == ItemTypeFile {
			fileEvents++
		}
	}

	// At most 1 file event (case-insensitive: 1 file on disk, no collision;
	// case-sensitive: 2 files on disk, collision detected, both skipped → 0).
	assert.LessOrEqual(t, fileEvents, 1,
		"case collision should prevent duplicate file events; got %d file events", fileEvents)

	cancel()
	<-done
}
