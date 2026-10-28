package synctest

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func shimmedTree(t *testing.T, wrap func(synctree.Handle) synctree.Handle) (*synctree.Root, string) {
	t.Helper()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	return tree.Interpose(wrap), dir
}

func storedNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

func writeTreeFile(t *testing.T, tree *synctree.Root, rel, content string) {
	t.Helper()

	f, err := tree.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// Validates: R-2.12.4
func TestCaseInsensitive_VariantNamesAreTheStoredEntry(t *testing.T) {
	t.Parallel()

	tree, dir := shimmedTree(t, CaseInsensitive)
	require.NoError(t, tree.MkdirAll("Docs", 0o700))
	writeTreeFile(t, tree, "Docs/Readme.txt", "first")

	upper, err := tree.Lstat("DOCS/README.TXT")
	require.NoError(t, err, "a case variant finds the entry")
	stored, err := tree.Lstat("Docs/Readme.txt")
	require.NoError(t, err)
	assert.True(t, os.SameFile(upper, stored), "a variant is the same inode, as a hard link to it would be")

	writeTreeFile(t, tree, "docs/README.txt", "second")
	assert.Equal(t, []string{"Docs"}, storedNames(t, dir))
	assert.Equal(t, []string{"Readme.txt"}, storedNames(t, filepath.Join(dir, "Docs")), "creating a variant opens the stored entry")
	data, err := os.ReadFile(filepath.Join(dir, "Docs", "Readme.txt"))
	require.NoError(t, err)
	assert.Equal(t, "second", string(data))

	entries, err := tree.ReadDir("DOCS")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "Readme.txt", entries[0].Name(), "listings keep the stored spelling")
}

// Validates: R-2.12.4
func TestCaseInsensitive_UnicodeFormVariantsAreOneName(t *testing.T) {
	t.Parallel()

	tree, dir := shimmedTree(t, CaseInsensitive)
	composed, decomposed := "café.txt", "café.txt"
	writeTreeFile(t, tree, composed, "x")

	_, err := tree.Stat(decomposed)
	require.NoError(t, err)
	assert.Equal(t, []string{composed}, storedNames(t, dir))
}

// Validates: R-2.12.4
func TestCaseInsensitive_RenameRespellsOrReplaces(t *testing.T) {
	t.Parallel()

	tree, dir := shimmedTree(t, CaseInsensitive)
	writeTreeFile(t, tree, "edit.txt", "edit")
	require.NoError(t, tree.Rename("edit.txt", "EDIT.txt"))
	assert.Equal(t, []string{"EDIT.txt"}, storedNames(t, dir), "a rename onto its own variant respells the entry")

	writeTreeFile(t, tree, "other.txt", "other")
	require.NoError(t, tree.Rename("other.txt", "edit.TXT"))
	assert.Equal(t, []string{"edit.TXT"}, storedNames(t, dir), "a rename onto another entry's variant replaces it")
	data, err := os.ReadFile(filepath.Join(dir, "edit.TXT"))
	require.NoError(t, err)
	assert.Equal(t, "other", string(data))
}

// Validates: R-2.10.60, R-2.10.61
func TestErrnoShim_FailsMatchingOperationsUntilHealed(t *testing.T) {
	t.Parallel()

	shim := NewErrnoShim(
		ErrnoRule{Op: FSOpWrite, Path: "full", Errno: syscall.ENOSPC},
		ErrnoRule{Op: FSOpRename, Path: "ro", Errno: syscall.EROFS},
		ErrnoRule{Op: FSOpOpen, Path: "bad", Errno: syscall.EIO},
	)
	tree, dir := shimmedTree(t, shim.Wrap)
	for _, sub := range []string{"full", "ro", "bad", "fine"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, sub), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fine", "f"), []byte("x"), 0o600))

	_, err := tree.OpenFile("full/new", os.O_CREATE|os.O_WRONLY, 0o600)
	require.ErrorIs(t, err, syscall.ENOSPC)
	dirFile, err := tree.Open("full")
	require.NoError(t, err, "a read under the full path is not a write")
	require.NoError(t, dirFile.Close())

	err = tree.Rename("fine/f", "ro/f")
	require.ErrorIs(t, err, syscall.EROFS, "a rename matches by either name")

	_, err = tree.ReadDir("bad")
	require.ErrorIs(t, err, syscall.EIO, "directory reads go through the shim too")
	walkErr := tree.WalkDir(func(_ string, _ fs.DirEntry, err error) error { return err })
	require.ErrorIs(t, walkErr, syscall.EIO)

	assert.Equal(t, 4, shim.Hits())
	shim.SetRules()
	created, err := tree.OpenFile("full/new", os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err, "no rules heals the filesystem")
	require.NoError(t, created.Close())
}
