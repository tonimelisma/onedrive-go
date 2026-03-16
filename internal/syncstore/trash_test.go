package syncstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.4.6
func TestDefaultTrashFunc_NonDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("test only applicable on non-darwin")
	}

	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))

	err := DefaultTrashFunc(path)
	require.Error(t, err, "expected error on non-darwin platform")
}

// Validates: R-6.4.5
func TestMoveToMacOSTrash(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only test")
	}

	t.Parallel()

	// Create a temp file.
	dir := t.TempDir()
	path := filepath.Join(dir, "trash-test-file.txt")

	require.NoError(t, os.WriteFile(path, []byte("trash me"), 0o600))

	// Move to trash.
	require.NoError(t, moveToMacOSTrash(path))

	// Original should be gone.
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should have been moved to trash")

	// Clean up from trash.
	home, _ := os.UserHomeDir()
	trashPath := filepath.Join(home, ".Trash", "trash-test-file.txt")
	os.Remove(trashPath) // best-effort cleanup
}

func TestMoveToMacOSTrash_NameCollision(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only test")
	}

	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	trashDir := filepath.Join(home, ".Trash")

	// Create a file in trash with the same name to force collision handling.
	collisionPath := filepath.Join(trashDir, "trash-collision-test.txt")
	require.NoError(t, os.WriteFile(collisionPath, []byte("existing"), 0o600))

	defer os.Remove(collisionPath)

	// Now try to trash a file with the same name.
	dir := t.TempDir()
	path := filepath.Join(dir, "trash-collision-test.txt")

	require.NoError(t, os.WriteFile(path, []byte("new"), 0o600))
	require.NoError(t, moveToMacOSTrash(path))

	// Original should be gone.
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should have been moved to trash")

	// Clean up: the collision file should be "trash-collision-test 2.txt".
	suffix2 := filepath.Join(trashDir, "trash-collision-test 2.txt")
	os.Remove(suffix2) // best-effort cleanup
}
