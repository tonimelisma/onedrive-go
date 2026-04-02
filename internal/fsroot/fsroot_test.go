package fsroot

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenPath_SplitsParentRootAndLeaf(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	path := filepath.Join(base, "token.json")

	root, name, err := OpenPath(path)
	require.NoError(t, err)
	require.NotNil(t, root)
	assert.Equal(t, "token.json", name)
}

func TestOpen_RejectsEmptyDirectory(t *testing.T) {
	t.Parallel()

	root, err := Open("")
	require.Error(t, err)
	assert.Nil(t, root)
	assert.Contains(t, err.Error(), "root directory is empty")
}

func TestOpenPath_RejectsEmptyAndDirectoryPaths(t *testing.T) {
	t.Parallel()

	root, name, err := OpenPath("")
	require.Error(t, err)
	assert.Nil(t, root)
	assert.Empty(t, name)
	assert.Contains(t, err.Error(), "path is empty")

	root, name, err = OpenPath(t.TempDir() + string(filepath.Separator))
	require.Error(t, err)
	assert.Nil(t, root)
	assert.Empty(t, name)
	assert.Contains(t, err.Error(), "does not name a file")
}

func TestRoot_AtomicWrite_WritesFileAtomically(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root, err := Open(base)
	require.NoError(t, err)

	err = root.AtomicWrite("config.toml", []byte("hello = true\n"), 0o600, 0o700, ".config-*.tmp")
	require.NoError(t, err)

	data, err := root.ReadFile("config.toml")
	require.NoError(t, err)
	assert.Equal(t, "hello = true\n", string(data))

	matches, err := filepath.Glob(filepath.Join(base, ".config-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, matches, "temporary files should be cleaned up after a successful rename")
}

func TestRoot_AtomicWrite_RejectsRootEscape(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root, err := Open(base)
	require.NoError(t, err)

	err = root.AtomicWrite("../escape.txt", []byte("nope"), 0o600, 0o700, ".tmp-*.tmp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes root")

	_, statErr := os.Stat(filepath.Join(base, "..", "escape.txt"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRoot_FileLifecycleOperations(t *testing.T) {
	t.Parallel()

	base := filepath.Join(t.TempDir(), "state")
	root, err := Open(base)
	require.NoError(t, err)
	require.NoError(t, root.MkdirAll(0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "nested"), 0o700))

	file, err := root.OpenFile("nested/item.txt", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString("hello")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	opened, err := root.Open("nested/item.txt")
	require.NoError(t, err)
	data, err := io.ReadAll(opened)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
	require.NoError(t, opened.Close())

	readData, err := root.ReadFile("nested/item.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(readData))

	temp, tempName, err := root.CreateTemp("nested", ".managed-*.tmp")
	require.NoError(t, err)
	require.NoError(t, temp.Close())
	assert.Equal(t, "nested", filepath.Dir(tempName))

	require.NoError(t, root.Rename("nested/item.txt", "renamed.txt"))
	renamedData, err := root.ReadFile("renamed.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(renamedData))

	require.NoError(t, root.Remove("renamed.txt"))
	_, err = os.Stat(filepath.Join(base, "renamed.txt"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRoot_StatAndReadDir(t *testing.T) {
	t.Parallel()

	base := filepath.Join(t.TempDir(), "state")
	root, err := Open(base)
	require.NoError(t, err)
	require.NoError(t, root.MkdirAll(0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "nested", "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(base, "nested", "b.txt"), []byte("bb"), 0o600))

	info, err := root.Stat("nested/a.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(1), info.Size())

	entries, err := root.ReadDir("nested")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt"}, []string{entries[0].Name(), entries[1].Name()})
}

func TestRoot_ReadDirRoot(t *testing.T) {
	t.Parallel()

	base := filepath.Join(t.TempDir(), "state")
	root, err := Open(base)
	require.NoError(t, err)
	require.NoError(t, root.MkdirAll(0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "root.txt"), []byte("root"), 0o600))

	entries, err := root.ReadDir("")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "root.txt", entries[0].Name())
}

func TestRoot_MissingPathsNormalizeToNotExist(t *testing.T) {
	t.Parallel()

	base := filepath.Join(t.TempDir(), "missing-root")
	root, err := Open(base)
	require.NoError(t, err)

	info, err := root.Stat("state.db")
	require.Error(t, err)
	assert.Nil(t, info)
	require.ErrorIs(t, err, os.ErrNotExist)

	file, err := root.Open("state.db")
	require.Error(t, err)
	assert.Nil(t, file)
	require.ErrorIs(t, err, os.ErrNotExist)

	entries, err := root.ReadDir("")
	require.Error(t, err)
	assert.Nil(t, entries)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRoot_OpenAndOpenFile_RejectRootEscape(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	file, err := root.Open("../escape.txt")
	require.Error(t, err)
	assert.Nil(t, file)
	assert.Contains(t, err.Error(), "escapes root")

	file, err = root.OpenFile("../escape.txt", os.O_RDONLY, 0)
	require.Error(t, err)
	assert.Nil(t, file)
	assert.Contains(t, err.Error(), "escapes root")
}

func TestRoot_AtomicWrite_CleansTempFileWhenRenameFails(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root, err := Open(base)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "existing-dir"), 0o700))

	err = root.AtomicWrite("existing-dir", []byte("hello"), 0o600, 0o700, ".managed-*.tmp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renaming")

	matches, globErr := filepath.Glob(filepath.Join(base, ".managed-*.tmp"))
	require.NoError(t, globErr)
	assert.Empty(t, matches, "temporary files should be cleaned up after rename errors")
}

func TestCleanupTempOnError_JoinsRemovalError(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	baseErr := errors.New("write failed")
	err = root.cleanupTempOnError("../escape.txt", baseErr)
	require.Error(t, err)
	require.ErrorIs(t, err, baseErr)
	assert.Contains(t, err.Error(), "escapes root")
}

func TestWriteTempFile_ClosedFileReturnsCloseContext(t *testing.T) {
	t.Parallel()

	temp, err := os.CreateTemp(t.TempDir(), "managed-*.tmp")
	require.NoError(t, err)
	require.NoError(t, temp.Close())

	err = writeTempFile(temp, []byte("hello"), 0o600)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "setting temp file permissions")
	assert.Contains(t, err.Error(), "closing temp file")
}

// Validates: R-6.2.3
func TestRoot_AtomicWrite_FailureInjection(t *testing.T) {
	t.Parallel()

	errDiskFull := errors.New("disk full")
	errWriteFailed := errors.New("write failed")
	errSyncFailed := errors.New("sync failed")
	errCloseFailed := errors.New("close failed")
	errRenameFailed := errors.New("rename failed")
	errRemoveFailed := errors.New("remove failed")

	testCases := []atomicWriteFailureCase{
		{
			name:          "create temp failure",
			createTempErr: errDiskFull,
			wantErr:       errDiskFull,
			wantContains:  "creating temp file",
		},
		{
			name: "write failure cleans temp",
			tempFile: &fakeTempFile{
				basename: "managed-write.tmp",
				writeErr: errWriteFailed,
			},
			wantErr:         errWriteFailed,
			wantContains:    "writing temp file",
			wantRemovedBase: "managed-write.tmp",
		},
		{
			name: "sync failure cleans temp",
			tempFile: &fakeTempFile{
				basename: "managed-sync.tmp",
				syncErr:  errSyncFailed,
			},
			wantErr:         errSyncFailed,
			wantContains:    "syncing temp file",
			wantRemovedBase: "managed-sync.tmp",
		},
		{
			name: "close failure cleans temp",
			tempFile: &fakeTempFile{
				basename: "managed-close.tmp",
				closeErr: errCloseFailed,
			},
			wantErr:         errCloseFailed,
			wantContains:    "closing temp file",
			wantRemovedBase: "managed-close.tmp",
		},
		{
			name: "rename failure joins cleanup failure",
			tempFile: &fakeTempFile{
				basename: "managed-rename.tmp",
			},
			renameErr:       errRenameFailed,
			removeErr:       errRemoveFailed,
			wantErr:         errRenameFailed,
			wantContains:    "renaming",
			wantRemovedBase: "managed-rename.tmp",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runAtomicWriteFailureCase(t, testCase)
		})
	}
}

type atomicWriteFailureCase struct {
	name            string
	createTempErr   error
	tempFile        *fakeTempFile
	renameErr       error
	removeErr       error
	wantErr         error
	wantContains    string
	wantRemovedBase string
}

func runAtomicWriteFailureCase(t *testing.T, testCase atomicWriteFailureCase) {
	t.Helper()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	var removedPath string
	root.ops.createTemp = func(dir, pattern string) (tempFile, error) {
		if testCase.createTempErr != nil {
			return nil, testCase.createTempErr
		}

		require.NotNil(t, testCase.tempFile)
		testCase.tempFile.name = filepath.Join(dir, testCase.tempFile.basename)

		return testCase.tempFile, nil
	}
	root.ops.rename = func(oldpath, newpath string) error {
		if testCase.renameErr != nil {
			return testCase.renameErr
		}

		return nil
	}
	root.ops.remove = func(path string) error {
		removedPath = path
		return testCase.removeErr
	}

	err = root.AtomicWrite("config.toml", []byte("payload"), 0o600, 0o700, ".tmp-*.tmp")
	require.Error(t, err)
	require.ErrorIs(t, err, testCase.wantErr)
	assert.Contains(t, err.Error(), testCase.wantContains)

	if testCase.wantRemovedBase != "" {
		assert.Equal(t, filepath.Join(root.dir, testCase.wantRemovedBase), removedPath)
	}

	if testCase.removeErr != nil {
		assert.ErrorIs(t, err, testCase.removeErr)
	}
}

type fakeTempFile struct {
	name     string
	basename string
	chmodErr error
	writeErr error
	syncErr  error
	closeErr error
}

func (f *fakeTempFile) Name() string {
	return f.name
}

func (f *fakeTempFile) Chmod(os.FileMode) error {
	return f.chmodErr
}

func (f *fakeTempFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}

	return len(p), nil
}

func (f *fakeTempFile) Sync() error {
	return f.syncErr
}

func (f *fakeTempFile) Close() error {
	return f.closeErr
}
