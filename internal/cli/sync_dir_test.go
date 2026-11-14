package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-3.3.1, R-3.3.5
func TestMaterializeDriveSyncDirRejectsRelativePathWithoutCreatingDirectory(t *testing.T) {
	cwd := t.TempDir()
	previousCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(cwd))
	t.Cleanup(func() {
		require.NoError(t, os.Chdir(previousCWD))
	})

	err = materializeDriveSyncDir("relative-sync")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")

	_, statErr := os.Stat(filepath.Join(cwd, "relative-sync"))
	assert.True(t, os.IsNotExist(statErr))
}
