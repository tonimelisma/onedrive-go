package synctree

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10, R-6.2
func TestRoot_AbsRelRoundTrip(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	absPath, err := root.Abs(filepath.Join("nested", "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "nested", "file.txt"), absPath)

	relPath, err := root.Rel(absPath)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("nested", "file.txt"), relPath)
}

// Validates: R-2.10, R-6.2
func TestRoot_RejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	_, err = root.Abs("../escape.txt")
	require.Error(t, err)

	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	_, err = root.Rel(outside)
	require.Error(t, err)
}

// Validates: R-2.10, R-6.2
func TestRoot_OpenAbsAndStatAbs(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	absPath := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(absPath, []byte("payload"), 0o600))

	file, err := root.OpenAbs(absPath)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	info, err := root.StatAbs(absPath)
	require.NoError(t, err)
	assert.Equal(t, int64(len("payload")), info.Size())
}

// Validates: R-2.10, R-6.2
func TestRoot_WalkDirUsesAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "file.txt"), []byte("payload"), 0o600))

	var walked []string
	err = root.WalkDir(func(path string, d fs.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		walked = append(walked, path)
		return nil
	})
	require.NoError(t, err)
	assert.Contains(t, walked, dir)
	assert.Contains(t, walked, filepath.Join(dir, "nested"))
	assert.Contains(t, walked, filepath.Join(dir, "nested", "file.txt"))
}

// Validates: R-2.10, R-6.2
func TestRoot_GlobReturnsRelativeMatches(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.conflict-1.txt"), []byte("one"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.conflict-2.txt"), []byte("two"), 0o600))

	matches, err := root.Glob("keep.conflict-*.txt")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"keep.conflict-1.txt", "keep.conflict-2.txt"}, matches)
}

// Validates: R-2.10, R-6.2
func TestRoot_FileLifecycleOperations(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, root.Path())

	require.NoError(t, root.MkdirAll("nested", 0o700))

	file, err := root.OpenFile(filepath.Join("nested", "file.txt"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString("payload")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	entries, err := root.ReadDir("nested")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "file.txt", entries[0].Name())

	entries, err = root.ReadDirAbs(filepath.Join(dir, "nested"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "file.txt", entries[0].Name())

	require.NoError(t, root.Rename(filepath.Join("nested", "file.txt"), filepath.Join("nested", "renamed.txt")))

	info, err := root.Stat(filepath.Join("nested", "renamed.txt"))
	require.NoError(t, err)
	assert.Equal(t, int64(len("payload")), info.Size())

	require.NoError(t, root.Remove(filepath.Join("nested", "renamed.txt")))

	absExtra := filepath.Join(dir, "nested", "abs-remove.txt")
	require.NoError(t, os.WriteFile(absExtra, []byte("extra"), 0o600))
	require.NoError(t, root.RemoveAbs(absExtra))
}

// Validates: R-2.10, R-6.2
func TestRoot_NotExistErrors(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	_, err = root.Open("missing.txt")
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = root.Stat("missing.txt")
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = root.ReadDir("missing")
	require.ErrorIs(t, err, os.ErrNotExist)
}
