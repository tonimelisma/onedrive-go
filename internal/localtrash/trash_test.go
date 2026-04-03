package localtrash

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.4.6
func TestDefault_NonDarwin(t *testing.T) {
	if runtime.GOOS == platformDarwin {
		t.Skip("test only applicable on non-darwin")
	}

	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")

	require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))

	err := Default(path)
	require.Error(t, err, "expected error on non-darwin platform")
}

// Validates: R-6.4.5
func TestMoveToMacOSTrash(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "trash-test-file.txt")

	require.NoError(t, os.WriteFile(path, []byte("trash me"), 0o600))
	require.NoError(t, moveToMacOSTrash(path))

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should have been moved to trash")

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	trashPath := filepath.Join(home, ".Trash", "trash-test-file.txt")
	assert.NoError(t, os.Remove(trashPath))
}

func TestMoveToMacOSTrash_NameCollision(t *testing.T) {
	if runtime.GOOS != platformDarwin {
		t.Skip("macOS-only test")
	}

	t.Parallel()

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	trashDir := filepath.Join(home, ".Trash")
	collisionPath := filepath.Join(trashDir, "trash-collision-test.txt")
	require.NoError(t, os.WriteFile(collisionPath, []byte("existing"), 0o600))

	defer func() {
		assert.NoError(t, os.Remove(collisionPath))
	}()

	dir := t.TempDir()
	path := filepath.Join(dir, "trash-collision-test.txt")

	require.NoError(t, os.WriteFile(path, []byte("new"), 0o600))
	require.NoError(t, moveToMacOSTrash(path))

	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should have been moved to trash")

	suffix2 := filepath.Join(trashDir, "trash-collision-test 2.txt")
	assert.NoError(t, os.Remove(suffix2))
}
