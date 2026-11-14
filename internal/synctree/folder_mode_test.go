package synctree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Every folder the sync tree creates is a folder in the user's tree: its
// callers pass no mode, each new folder asks for 0777 and gets the umask,
// and a folder that already exists keeps its own.
//
// Validates: R-5.10.2
func TestRoot_CreatedFoldersFollowTheUmask(t *testing.T) {
	t.Parallel()
	if !synctest.RunUnderUmask(t, synctest.GroupWritableUmask) {
		return
	}

	dir := t.TempDir()
	root, err := synctree.Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "existing"), 0o700))

	require.NoError(t, root.MkdirAll(filepath.Join("a", "b")))
	require.NoError(t, root.MkdirAllNoFollow(filepath.Join("c", "d")))
	require.NoError(t, root.MkdirAll(filepath.Join("existing", "new")))

	modes := map[string]os.FileMode{
		"a":            0o775,
		"a/b":          0o775,
		"c":            0o775,
		"c/d":          0o775,
		"existing":     0o700,
		"existing/new": 0o775,
	}
	for rel, want := range modes {
		info, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		require.NoError(t, statErr)
		assert.Equal(t, want, info.Mode().Perm(), rel)
	}
}
