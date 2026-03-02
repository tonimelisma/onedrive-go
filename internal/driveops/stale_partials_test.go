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

	// Create a .partial file (should be deleted).
	partialPath := filepath.Join(dir, "download.partial")
	require.NoError(t, os.WriteFile(partialPath, []byte("partial"), 0o644))

	// Create a regular file (should be preserved).
	regularPath := filepath.Join(dir, "document.txt")
	require.NoError(t, os.WriteFile(regularPath, []byte("content"), 0o644))

	n, err := CleanStalePartials(dir, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// .partial file should be gone.
	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "partial file should have been deleted")

	// Regular file should still exist.
	_, statErr = os.Stat(regularPath)
	assert.NoError(t, statErr, "regular file should be preserved")
}

func TestCleanStalePartials_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	n, err := CleanStalePartials(dir, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestCleanStalePartials_NestedDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create nested directories with .partial files.
	subDir := filepath.Join(dir, "a", "b")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	partialPath := filepath.Join(subDir, "deep.partial")
	require.NoError(t, os.WriteFile(partialPath, []byte("nested"), 0o644))

	n, err := CleanStalePartials(dir, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, statErr := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(statErr), "nested partial should have been deleted")
}

func TestCleanStalePartials_MultipleFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create 3 .partial files.
	for _, name := range []string{"a.partial", "b.partial", "c.partial"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}

	// Create a non-partial file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644))

	n, err := CleanStalePartials(dir, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// All partials gone.
	for _, name := range []string{"a.partial", "b.partial", "c.partial"} {
		_, statErr := os.Stat(filepath.Join(dir, name))
		assert.True(t, os.IsNotExist(statErr), "%s should have been deleted", name)
	}

	// Non-partial preserved.
	_, statErr := os.Stat(filepath.Join(dir, "keep.txt"))
	assert.NoError(t, statErr)
}

func TestCleanStalePartials_PermissionError(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("test requires non-root user")
	}

	dir := t.TempDir()

	// Create a restricted subdirectory containing a .partial file.
	restricted := filepath.Join(dir, "restricted")
	require.NoError(t, os.MkdirAll(restricted, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(restricted, "hidden.partial"), []byte("x"), 0o644))

	// Create an accessible .partial at the top level.
	topPartial := filepath.Join(dir, "top.partial")
	require.NoError(t, os.WriteFile(topPartial, []byte("y"), 0o644))

	// Remove read+execute permission on the subdirectory.
	require.NoError(t, os.Chmod(restricted, 0o000))
	t.Cleanup(func() { _ = os.Chmod(restricted, 0o755) })

	// Should still delete the accessible .partial and not panic.
	n, err := CleanStalePartials(dir, testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Top-level partial deleted.
	_, statErr := os.Stat(topPartial)
	assert.True(t, os.IsNotExist(statErr), "accessible partial should have been deleted")
}

func TestCleanStalePartials_NonexistentDir(t *testing.T) {
	t.Parallel()

	_, err := CleanStalePartials("/nonexistent/path/that/does/not/exist", testLogger(t))
	assert.Error(t, err)
}
