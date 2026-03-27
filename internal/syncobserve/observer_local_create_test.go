package syncobserve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Watch tests — file/directory creation
// ---------------------------------------------------------------------------

// Validates: R-2.1.2
func TestWatch_DetectsFileCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)

	events := make(chan synctypes.ChangeEvent, 10)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	// Let the watcher settle, then create a file.
	time.Sleep(100 * time.Millisecond)
	writeTestFile(t, dir, "new-file.txt", "hello watch")

	var ev synctypes.ChangeEvent
	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for create event")
	}

	cancel()
	<-done

	assert.Equal(t, synctypes.ChangeCreate, ev.Type)
	assert.Equal(t, "new-file.txt", ev.Path)
	assert.Equal(t, synctypes.SourceLocal, ev.Source)
	assert.NotEmpty(t, ev.Hash, "Hash should be non-empty for a file create")
}

func TestWatch_NewDirectoryWatched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)

	events := make(chan synctypes.ChangeEvent, 20)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a subdirectory and a file inside it.
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o700))

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
	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)

	events := make(chan synctypes.ChangeEvent, 30)
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
	require.NoError(t, os.MkdirAll(subDir, 0o700))

	preExistingFile := filepath.Join(subDir, "already-here.txt")
	require.NoError(t, os.WriteFile(preExistingFile, []byte("pre-existing"), 0o600))

	// Collect events. The file should appear without a separate fsnotify event
	// because scanNewDirectory picks it up during directory creation handling.
	var foundPreExisting bool

	timeout := time.After(5 * time.Second)

	for !foundPreExisting {
		select {
		case ev := <-events:
			if ev.Path == "pre-populated/already-here.txt" && ev.Type == synctypes.ChangeCreate {
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
	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)

	events := make(chan synctypes.ChangeEvent, 50)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- obs.Watch(ctx, dir, events)
	}()

	time.Sleep(100 * time.Millisecond)

	// Create a 3-level nested directory structure with a file at the bottom.
	deepDir := filepath.Join(dir, "level1", "level2", "level3")
	require.NoError(t, os.MkdirAll(deepDir, 0o700))

	deepFile := filepath.Join(deepDir, "deep-file.txt")
	require.NoError(t, os.WriteFile(deepFile, []byte("deep content"), 0o600))

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
			if ev.Type == synctypes.ChangeCreate {
				if ev.ItemType == synctypes.ItemTypeFolder && wantDirs[ev.Path] {
					foundDirs[ev.Path] = true
				}

				if ev.Path == "level1/level2/level3/deep-file.txt" && ev.ItemType == synctypes.ItemTypeFile {
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
	obs := NewLocalObserver(synctest.EmptyBaseline(), synctest.TestLogger(t), 0)

	events := make(chan synctypes.ChangeEvent, 10)
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
		assert.NoError(t, os.Chmod(path, 0o600))
	})

	var ev synctypes.ChangeEvent

	select {
	case ev = <-events:
	case <-time.After(5 * time.Second):
		cancel()
		require.Fail(t, "timeout waiting for create event")
	}

	cancel()
	<-done

	require.Equal(t, synctypes.ChangeCreate, ev.Type)
	require.Equal(t, "unreadable.txt", ev.Path)
	require.Equal(t, synctypes.SourceLocal, ev.Source)
	require.Empty(t, ev.Hash, "hash should be empty when computation fails")
}

// Case collision tests moved to observer_local_collisions_test.go.
