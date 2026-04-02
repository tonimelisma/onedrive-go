package localpath

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadWriteLifecycle(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	target := filepath.Join(base, "nested", "file.txt")

	require.NoError(t, MkdirAll(filepath.Dir(target), 0o700))

	file, err := OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString("hello")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	data, err := ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	info, err := Stat(target)
	require.NoError(t, err)
	assert.Equal(t, int64(5), info.Size())

	renamed := filepath.Join(base, "nested", "renamed.txt")
	require.NoError(t, Rename(target, renamed))

	data, err = ReadFile(renamed)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	require.NoError(t, Remove(renamed))
	_, err = os.Stat(renamed)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadDir(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(base, "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(base, "b.txt"), []byte("b"), 0o600))

	entries, err := ReadDir(base)
	require.NoError(t, err)
	require.Len(t, entries, 2)
}

func TestOpenSuccess(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	target := filepath.Join(base, "opened.txt")
	require.NoError(t, os.WriteFile(target, []byte("opened"), 0o600))

	file, err := Open(target)
	require.NoError(t, err)

	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	assert.Equal(t, "opened", string(data))
}

func TestOpenAndReadDirMissingPath(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	missingFile := filepath.Join(base, "missing.txt")
	missingDir := filepath.Join(base, "missing-dir")

	file, err := Open(missingFile)
	require.Error(t, err)
	assert.Nil(t, file)
	require.ErrorIs(t, err, os.ErrNotExist)

	info, err := Stat(missingFile)
	require.Error(t, err)
	assert.Nil(t, info)
	require.ErrorIs(t, err, os.ErrNotExist)

	entries, err := ReadDir(missingDir)
	require.Error(t, err)
	assert.Nil(t, entries)
	require.ErrorIs(t, err, os.ErrNotExist)

	err = Remove(missingFile)
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRenameRejectsEmptySource(t *testing.T) {
	t.Parallel()

	err := Rename("", filepath.Join(t.TempDir(), "dst.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}

func TestRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	_, err := ReadFile("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")

	_, err = Open("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")

	_, err = Stat("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")

	err = MkdirAll("", 0o700)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")

	_, err = ReadDir("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is empty")
}
