package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Validates: R-2.4.6, R-2.12.1
//
// A directory reached through a followed symlink is outside what the sync
// tree will list, but its new files still need the collision check: it is
// listed through localpath, as observation reads followed paths.
func TestReadDirForCollisionCheck_ListsAFollowedDirectory(t *testing.T) {
	t.Parallel()

	root, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "Name.txt"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "linked")))
	tree, err := synctree.Open(root)
	require.NoError(t, err)

	entries, err := readDirForCollisionCheck(tree, filepath.Join(root, "linked"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "Name.txt", entries[0].Name())
}
