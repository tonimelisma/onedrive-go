package localpath

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.4.9
func TestRenameNoReplace_NeverReplacesAnExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	require.NoError(t, os.WriteFile(src, []byte("from trash"), 0o600))
	require.NoError(t, os.WriteFile(dst, []byte("current"), 0o600))

	err := RenameNoReplace(src, dst)
	require.ErrorIs(t, err, ErrTargetExists)
	data, readErr := os.ReadFile(dst) //nolint:gosec // test reads its own temp tree
	require.NoError(t, readErr)
	assert.Equal(t, "current", string(data))
	assert.FileExists(t, src)

	require.NoError(t, os.Remove(dst))
	require.NoError(t, RenameNoReplace(src, dst))
	assert.NoFileExists(t, src)
	assert.FileExists(t, dst)
}
