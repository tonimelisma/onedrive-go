package trustedpath

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_ReadsExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o600))

	file, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	data := make([]byte, 5)
	n, err := file.Read(data)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, "hello", string(data))
}

func TestReadFile_ReadsExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o600))

	data, err := ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestOpenFile_CreatesAndWritesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "created.txt")

	file, err := OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, writeErr := file.WriteString("created via root")
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())

	data, err := ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "created via root", string(data))
}

func TestOpen_MissingFileReturnsError(t *testing.T) {
	t.Parallel()

	_, err := Open(filepath.Join(t.TempDir(), "missing.txt"))
	require.Error(t, err)
}

func TestReadFile_EmptyPathReturnsError(t *testing.T) {
	t.Parallel()

	_, err := ReadFile("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}

func TestOpen_DirectoryPathReturnsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := Open(dir + string(filepath.Separator))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not name a file")
}

func TestOpen_SymlinkEscapeReturnsError(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsidePath, []byte("secret"), 0o600))

	linkPath := filepath.Join(rootDir, "escape.txt")
	require.NoError(t, os.Symlink(outsidePath, linkPath))

	_, err := Open(linkPath)
	require.Error(t, err)
}

func TestOpen_AbsolutePathSucceeds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "absolute.txt")
	require.NoError(t, os.WriteFile(path, []byte("absolute"), 0o600))

	file, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
}
