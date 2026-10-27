package synctree

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDetachTree(t *testing.T) (*Root, string) {
	t.Helper()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "a.txt"), []byte("synced\n"), 0o600))
	return root, dir
}

// Validates: R-6.4.9
func TestDetachRegularFile_MovesTheVerifiedFileOut(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	dest := filepath.Join(t.TempDir(), "item")

	require.NoError(t, root.DetachRegularFile("docs/a.txt", nil, dest))

	assert.NoFileExists(t, filepath.Join(dir, "docs", "a.txt"))
	data, err := os.ReadFile(dest) //nolint:gosec // the test reads a file it created
	require.NoError(t, err)
	assert.Equal(t, "synced\n", string(data))
}

// Validates: R-6.2.4
func TestDetachRegularFile_VerifyRefusalLeavesTheFile(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	refused := errors.New("changed")

	err := root.DetachRegularFile("docs/a.txt", func(*os.File, os.FileInfo) error { return refused }, filepath.Join(t.TempDir(), "item"))

	require.ErrorIs(t, err, refused)
	assert.FileExists(t, filepath.Join(dir, "docs", "a.txt"))
}

// An editor's atomic save lands while the old file is being verified: the
// path now names a new inode. Only the verified inode may leave the tree, so
// nothing moves and the new content stays (D-010).
//
// Validates: R-6.2.4
func TestDetachRegularFile_FileReplacedDuringVerifyIsKept(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"detach", "remove", "preserve"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			target := filepath.Join(dir, "docs", "a.txt")
			swap := func(*os.File, os.FileInfo) error {
				replacement := filepath.Join(dir, "docs", "a.txt.new")
				require.NoError(t, os.WriteFile(replacement, []byte("user's new edit\n"), 0o600))
				require.NoError(t, os.Rename(replacement, target))
				return nil
			}
			dest := filepath.Join(t.TempDir(), "item")

			var err error
			switch operation {
			case "detach":
				err = root.DetachRegularFile("docs/a.txt", swap, dest)
			case "remove":
				err = root.RemoveVerifiedRegularFile("docs/a.txt", swap)
			default:
				err = root.PreserveRegularFile("docs/a.txt", swap, dest)
			}

			require.ErrorIs(t, err, ErrDetachRaced)
			data, readErr := os.ReadFile(target) //nolint:gosec // the test reads a file it created
			require.NoError(t, readErr)
			assert.Equal(t, "user's new edit\n", string(data))
			assert.NoFileExists(t, dest)
			entries, readDirErr := os.ReadDir(filepath.Join(dir, "docs"))
			require.NoError(t, readDirErr)
			assert.Len(t, entries, 1, "no set-aside file is left behind")
		})
	}
}

// Validates: R-6.4.9
func TestPreserveRegularFile_KeepsTheFileInPlace(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	dest := filepath.Join(t.TempDir(), "item")

	require.NoError(t, root.PreserveRegularFile("docs/a.txt", nil, dest))

	assert.FileExists(t, filepath.Join(dir, "docs", "a.txt"))
	data, err := os.ReadFile(dest) //nolint:gosec // the test reads a file it created
	require.NoError(t, err)
	assert.Equal(t, "synced\n", string(data))
}

// Validates: R-6.4.8
func TestDetachRegularFile_RefusesSymlinks(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	require.NoError(t, os.Symlink(filepath.Join(dir, "docs", "a.txt"), filepath.Join(dir, "link.txt")))

	err := root.DetachRegularFile("link.txt", nil, filepath.Join(t.TempDir(), "item"))
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(dir, "docs", "a.txt"))
}
