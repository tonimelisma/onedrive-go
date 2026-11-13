package driveops

import (
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/synctree"

	"github.com/stretchr/testify/require"
)

func writeTestResponsef(t *testing.T, w io.Writer, format string, args ...any) {
	t.Helper()

	_, err := fmt.Fprintf(w, format, args...)
	require.NoError(t, err)
}

func setTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}

// makeReadOnlyForTest takes the owner's write access from dir, so nothing in
// it can be removed, and gives it back when the test ends. The folder stays
// readable and searchable.
func makeReadOnlyForTest(t *testing.T, dir string) {
	t.Helper()

	setTestDirPermissions(t, dir, 0o500)
	t.Cleanup(func() { setTestDirPermissions(t, dir, 0o700) })
}

func mustOpenSyncTree(t *testing.T, path string) *synctree.Root {
	t.Helper()

	tree, err := synctree.Open(path)
	if err != nil {
		panic(fmt.Sprintf("open sync tree %s: %v", path, err))
	}

	return tree
}
