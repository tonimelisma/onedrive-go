package synctree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Error text names paths inside the sync folder relative to it, the way the
// rest of the output does, instead of mixing relative names (Open) with
// absolute ones (Stat, ReadDir, Remove, Rename). The sync folder itself is
// still named by its full path.
//
// Validates: R-6.6.26
func TestRoot_ErrorsNamePathsRelativeToTheSyncFolder(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	missing := filepath.Join("docs", "missing.txt")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "docs"), 0o700))

	_, openErr := root.Open(missing)
	_, statErr := root.Stat(missing)
	_, lstatErr := root.Lstat(missing)
	_, readDirErr := root.ReadDir(missing)
	removeErr := root.Remove(missing)
	renameErr := root.Rename(missing, filepath.Join("docs", "other.txt"))

	for name, err := range map[string]error{
		"Open": openErr, "Stat": statErr, "Lstat": lstatErr,
		"ReadDir": readDirErr, "Remove": removeErr, "Rename": renameErr,
	} {
		require.Errorf(t, err, "%s of a missing path must fail", name)
		require.ErrorIsf(t, err, os.ErrNotExist, "%s: %v", name, err)
		assert.Containsf(t, err.Error(), missing, "%s names the path relative to the sync folder", name)
		assert.NotContainsf(t, err.Error(), dir, "%s must not name the absolute path", name)
	}

	_, rootStatErr := root.Stat(".")
	require.NoError(t, rootStatErr)
	assert.Equal(t, dir, root.nameForMessage("."), "the sync folder itself is named by its full path")
}
