package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.6.28
//
// Only a missing file (or a missing parent) is "absent". Any other failure to
// check is an error: a permission problem or a symlink loop used to read as
// "no login" and "nothing synced".
func TestManagedPathExists_DistinguishesAbsentFromUnreadable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	present := filepath.Join(dir, "present.db")
	require.NoError(t, os.WriteFile(present, []byte("x"), 0o600))

	exists, err := managedPathExists(present)
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = managedPathExists(filepath.Join(dir, "missing.db"))
	require.NoError(t, err)
	assert.False(t, exists, "a missing file is absent")

	exists, err = managedPathExists(filepath.Join(dir, "missing-dir", "missing.db"))
	require.NoError(t, err)
	assert.False(t, exists, "a missing parent directory is absent")

	exists, err = managedPathExists(filepath.Join(present, "child.db"))
	require.NoError(t, err)
	assert.False(t, exists, "a parent that is a file (ENOTDIR) is absent")

	loop := filepath.Join(dir, "loop.db")
	require.NoError(t, os.Symlink("loop.db", loop))
	exists, err = managedPathExists(loop)
	require.Error(t, err, "a path that cannot be checked is an error, not absent")
	assert.False(t, exists)
	assert.Contains(t, err.Error(), loop)
}
