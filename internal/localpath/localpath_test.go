package localpath

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestCreateTempAndChtimes(t *testing.T) {
	t.Parallel()

	base := t.TempDir()

	temp, err := CreateTemp(base, "localpath-*.tmp")
	require.NoError(t, err)
	tempPath := temp.Name()
	require.NoError(t, temp.Close())

	infoBefore, err := Stat(tempPath)
	require.NoError(t, err)

	targetTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	require.NoError(t, Chtimes(tempPath, targetTime, targetTime))

	infoAfter, err := Stat(tempPath)
	require.NoError(t, err)
	assert.True(t, infoAfter.ModTime().Equal(targetTime), "mtime should be updated")
	assert.Equal(t, infoBefore.Name(), infoAfter.Name())
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

func TestSymlinkTargetsBehaveLikeOrdinaryPaths(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	targetDir := filepath.Join(base, "target-dir")
	targetFile := filepath.Join(targetDir, "file.txt")
	require.NoError(t, os.MkdirAll(targetDir, 0o700))
	require.NoError(t, os.WriteFile(targetFile, []byte("through-link"), 0o600))

	dirLink := filepath.Join(base, "dir-link")
	fileLink := filepath.Join(base, "file-link.txt")
	require.NoError(t, Symlink(targetDir, dirLink))
	require.NoError(t, Symlink(targetFile, fileLink))

	info, err := Stat(dirLink)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	linkInfo, err := Lstat(fileLink)
	require.NoError(t, err)
	assert.NotZero(t, linkInfo.Mode()&os.ModeSymlink)

	resolvedDir, err := EvalSymlinks(dirLink)
	require.NoError(t, err)
	expectedResolvedDir, err := filepath.EvalSymlinks(targetDir)
	require.NoError(t, err)
	assert.Equal(t, expectedResolvedDir, resolvedDir)

	entries, err := ReadDir(dirLink)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "file.txt", entries[0].Name())

	file, err := Open(fileLink)
	require.NoError(t, err)
	data, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	assert.Equal(t, "through-link", string(data))
}

func TestWriteFileAndRemoveAll(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	targetDir := filepath.Join(base, "nested")
	targetFile := filepath.Join(targetDir, "file.txt")

	require.NoError(t, MkdirAll(targetDir, 0o700))
	require.NoError(t, WriteFile(targetFile, []byte("written"), 0o600))

	data, err := ReadFile(targetFile)
	require.NoError(t, err)
	assert.Equal(t, "written", string(data))

	require.NoError(t, RemoveAll(targetDir))

	_, err = Stat(targetDir)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestAtomicWrite(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	target := filepath.Join(base, "nested", "config.toml")

	require.NoError(t, AtomicWrite(target, []byte("hello = true\n"), 0o600, 0o700, ".localpath-*.tmp"))

	data, err := ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "hello = true\n", string(data))
}

func TestAtomicWrite_CleansTempOnRenameFailure(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	targetDir := filepath.Join(base, "nested")
	target := filepath.Join(targetDir, "config.toml")

	require.NoError(t, MkdirAll(target, 0o700))

	err := AtomicWrite(target, []byte("hello = true\n"), 0o600, 0o700, ".localpath-*.tmp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renaming temp file")

	entries, readErr := os.ReadDir(targetDir)
	require.NoError(t, readErr)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	assert.Equal(t, []string{"config.toml"}, names, "rename failure should not leave temp files behind")
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
