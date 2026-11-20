package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// ---------------------------------------------------------------------------
// Watch tests — file/directory creation
// ---------------------------------------------------------------------------

// Validates: R-2.1.2
func TestWatch_DetectsFileCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	batches := localBatches(obs, 10)
	cancel, done := startLocalWatch(t, obs, dir)
	writeTestFile(t, dir, "new-file.txt", "hello watch")

	row := waitForLocalRow(t, batches, "new-file.txt")
	stopWatch(t, cancel, done)

	assert.Equal(t, ItemTypeFile, row.ItemType)
}

// Validates: R-2.1.2
func TestWatch_NewDirectoryWatched(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	batches := localBatches(obs, 20)
	cancel, done := startLocalWatch(t, obs, dir)

	// Create a subdirectory and a file inside it.
	subDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o700))

	dirRow := waitForLocalRow(t, batches, "subdir")
	assert.Equal(t, ItemTypeFolder, dirRow.ItemType)

	writeTestFile(t, dir, "subdir/inner.txt", "nested")

	row := waitForLocalRow(t, batches, "subdir/inner.txt")
	stopWatch(t, cancel, done)

	assert.Equal(t, ItemTypeFile, row.ItemType)
}

// TestWatch_NewDirectoryPreExistingFiles verifies that files already present
// in a newly-created directory are detected immediately (not deferred to the
// next safety scan). Regression test for B-100.
// Validates: R-2.1.2
func TestWatch_NewDirectoryPreExistingFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	batches := localBatches(obs, 30)
	cancel, done := startLocalWatch(t, obs, dir)

	// Create a directory with a file already inside it using os.MkdirAll +
	// os.WriteFile atomically (from the watcher's perspective, the directory
	// create event fires, and the file is already present when handleCreate
	// runs scanNewDirectory).
	subDir := filepath.Join(dir, "pre-populated")
	require.NoError(t, os.MkdirAll(subDir, 0o700))

	preExistingFile := filepath.Join(subDir, "already-here.txt")
	require.NoError(t, os.WriteFile(preExistingFile, []byte("pre-existing"), 0o600))

	// The file's row arrives without a separate fsnotify event because
	// scanNewDirectory picks it up during directory creation handling.
	row := waitForLocalRow(t, batches, "pre-populated/already-here.txt")
	stopWatch(t, cancel, done)

	assert.Equal(t, ItemTypeFile, row.ItemType)
}

// ---------------------------------------------------------------------------
// Recursive scanNewDirectory tests
// ---------------------------------------------------------------------------

// Validates: R-2.1.2
func TestWatch_NewDirectoryNestedRecursion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	batches := localBatches(obs, 50)
	cancel, done := startLocalWatch(t, obs, dir)

	// Create a 3-level nested directory structure with a file at the bottom.
	deepDir := filepath.Join(dir, "level1", "level2", "level3")
	require.NoError(t, os.MkdirAll(deepDir, 0o700))

	deepFile := filepath.Join(deepDir, "deep-file.txt")
	require.NoError(t, os.WriteFile(deepFile, []byte("deep content"), 0o600))

	// Collect rows: the deep file and all intermediate directories.
	foundDirs := make(map[string]bool)
	foundFile := false

	wantDirs := map[string]bool{
		"level1":               true,
		"level1/level2":        true,
		"level1/level2/level3": true,
	}

	timeout := time.After(eventWait)

	for !foundFile || len(foundDirs) < len(wantDirs) {
		select {
		case batch := <-batches:
			for _, row := range batch.Rows {
				if row.ItemType == ItemTypeFolder && wantDirs[row.Path] {
					foundDirs[row.Path] = true
				}

				if row.Path == "level1/level2/level3/deep-file.txt" && row.ItemType == ItemTypeFile {
					foundFile = true
				}
			}
		case <-timeout:
			require.Fail(t, "timeout",
				"foundFile=%v, foundDirs=%v (want %v)", foundFile, foundDirs, wantDirs)
		}
	}

	stopWatch(t, cancel, done)

	assert.True(t, foundFile, "deep file not detected")

	for d := range wantDirs {
		assert.True(t, foundDirs[d], "directory %q not detected", d)
	}
}

// ---------------------------------------------------------------------------
// Hash failure creates with empty hash (B-102) — Watch variant
// ---------------------------------------------------------------------------

// Validates: R-2.4.12
// TestWatch_HashFailureStillEmitsCreate verifies that a file whose hash cannot
// be computed (e.g., unreadable) still gets a row with an empty hash instead
// of being silently dropped (B-102).
func TestWatch_HashFailureStillEmitsCreate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (root can read all files)")
	}

	t.Parallel()

	dir := t.TempDir()
	obs := NewLocalObserver(emptyBaseline(), synctest.TestLogger(t), 0)

	batches := localBatches(obs, 10)
	cancel, done := startLocalWatch(t, obs, dir)

	// Create an unreadable file after the watch loop is live.
	// The file must be born unreadable — if we create then chmod, the observer
	// can hash the file between creation and chmod, producing a valid hash
	// instead of the expected empty hash (B-310).
	path := filepath.Join(dir, "unreadable.txt")
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o000))

	t.Cleanup(func() {
		// Restore permissions so TempDir cleanup works.
		assert.NoError(t, os.Chmod(path, 0o600))
	})

	row := waitForLocalRow(t, batches, "unreadable.txt")
	stopWatch(t, cancel, done)

	require.Empty(t, row.Hash, "hash should be empty when computation fails")
}

// Case collision tests moved to observer_local_collisions_test.go.
