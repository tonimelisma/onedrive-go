package driveops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanStalePartials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create an owned partial file (should be deleted).
	partialPath := filepath.Join(dir, downloadPartialPrefix+"download.txt"+downloadPartialSuffix)
	require.NoError(t, os.WriteFile(partialPath, []byte("partial"), 0o600))

	// Create a regular file (should be preserved).
	regularPath := filepath.Join(dir, "document.txt")
	require.NoError(t, os.WriteFile(regularPath, []byte("content"), 0o600))
	userPartialPath := filepath.Join(dir, "user.partial")
	require.NoError(t, os.WriteFile(userPartialPath, []byte("user"), 0o600))

	n, err := CleanStalePartials(mustOpenSyncTree(t, dir), testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// .partial file should be gone.
	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "partial file should have been deleted")

	// Regular file should still exist.
	_, statErr = os.Stat(regularPath)
	require.NoError(t, statErr, "regular file should be preserved")
	assert.FileExists(t, userPartialPath, "arbitrary user .partial file should be preserved by default")
}

func TestCleanStalePartials_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	n, err := CleanStalePartials(mustOpenSyncTree(t, dir), testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestCleanStalePartials_NestedDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create nested directories with .partial files.
	subDir := filepath.Join(dir, "a", "b")
	require.NoError(t, os.MkdirAll(subDir, 0o700))

	partialPath := filepath.Join(subDir, downloadPartialPrefix+"deep.bin"+downloadPartialSuffix)
	require.NoError(t, os.WriteFile(partialPath, []byte("nested"), 0o600))

	n, err := CleanStalePartials(mustOpenSyncTree(t, dir), testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "nested partial should have been deleted")
}

// Validates: R-2.4.10
func TestCleanStalePartialsWithOptions_SkipsConfiguredDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	childDir := filepath.Join(dir, "Shortcuts", "Docs")
	require.NoError(t, os.MkdirAll(childDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, downloadPartialPrefix+"child.txt"+downloadPartialSuffix), []byte("child"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, downloadPartialPrefix+"root.txt"+downloadPartialSuffix), []byte("root"), 0o600))

	n, err := CleanStalePartialsWithOptions(
		mustOpenSyncTree(t, dir),
		testLogger(t),
		StalePartialCleanupOptions{
			SkipDirs: []string{"Shortcuts/Docs"},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	assert.NoFileExists(t, filepath.Join(dir, downloadPartialPrefix+"root.txt"+downloadPartialSuffix))
	assert.FileExists(t, filepath.Join(childDir, downloadPartialPrefix+"child.txt"+downloadPartialSuffix))
}

func TestCleanStalePartials_MultipleFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create 3 .partial files.
	for _, name := range []string{
		downloadPartialPrefix + "a.txt" + downloadPartialSuffix,
		downloadPartialPrefix + "b.txt" + downloadPartialSuffix,
		downloadPartialPrefix + "c.txt" + downloadPartialSuffix,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}

	// Create a non-partial file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o600))

	n, err := CleanStalePartials(mustOpenSyncTree(t, dir), testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// All partials gone.
	for _, name := range []string{
		downloadPartialPrefix + "a.txt" + downloadPartialSuffix,
		downloadPartialPrefix + "b.txt" + downloadPartialSuffix,
		downloadPartialPrefix + "c.txt" + downloadPartialSuffix,
	} {
		_, statErr := os.Stat(filepath.Join(dir, name))
		assert.True(t, os.IsNotExist(statErr), "%s should have been deleted", name)
	}

	// Non-partial preserved.
	_, statErr := os.Stat(filepath.Join(dir, "keep.txt"))
	assert.NoError(t, statErr)
}

func TestCleanStalePartialsWithOptions_DeletesJunkPartialsWhenEnabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	userPartialPath := filepath.Join(dir, "user.partial")
	require.NoError(t, os.WriteFile(userPartialPath, []byte("user"), 0o600))

	n, err := CleanStalePartialsWithOptions(
		mustOpenSyncTree(t, dir),
		testLogger(t),
		StalePartialCleanupOptions{IncludeJunkPartials: true},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NoFileExists(t, userPartialPath)
}

func TestCleanStalePartials_PermissionError(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}

	dir := t.TempDir()

	// Create a restricted subdirectory containing a .partial file.
	restricted := filepath.Join(dir, "restricted")
	require.NoError(t, os.MkdirAll(restricted, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(restricted, downloadPartialPrefix+"hidden.txt"+downloadPartialSuffix), []byte("x"), 0o600))

	// Create an accessible .partial at the top level.
	topPartial := filepath.Join(dir, downloadPartialPrefix+"top.txt"+downloadPartialSuffix)
	require.NoError(t, os.WriteFile(topPartial, []byte("y"), 0o600))

	// Remove read+execute permission on the subdirectory.
	setTestDirPermissions(t, restricted, 0o000)
	t.Cleanup(func() { setTestDirPermissions(t, restricted, 0o700) })

	// Should still delete the accessible .partial and not panic.
	n, err := CleanStalePartials(mustOpenSyncTree(t, dir), testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Top-level partial deleted.
	_, statErr := os.Stat(topPartial)
	assert.True(t, os.IsNotExist(statErr), "accessible partial should have been deleted")
}

func TestCleanStalePartials_NonexistentDir(t *testing.T) {
	t.Parallel()

	_, err := CleanStalePartials(mustOpenSyncTree(t, "/nonexistent/path/that/does/not/exist"), testLogger(t))
	assert.Error(t, err)
}
