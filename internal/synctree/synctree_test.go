package synctree

import (
	"errors"
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

// Validates: R-2.10, R-6.2
func TestRoot_Open_FailureInjection(t *testing.T) {
	t.Parallel()

	runRootHandleErrorCase(t,
		func(root *Root, targetErr error) {
			root.ops.openRoot = func(dir string) (rootHandle, error) {
				return &fakeRootHandle{openErr: targetErr}, nil
			}
		},
		func(root *Root) error {
			file, err := root.Open("file.txt")
			assert.Nil(t, file)

			return err
		},
		errors.New("open failed"),
		"open failed",
	)
}

// Validates: R-2.10, R-6.2
func TestRoot_OpenRootFailureInjection(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	openRootErr := errors.New("open root failed")
	root.ops.openRoot = func(dir string) (rootHandle, error) {
		return nil, openRootErr
	}

	file, err := root.Open("file.txt")
	require.Error(t, err)
	assert.Nil(t, file)
	require.ErrorIs(t, err, openRootErr)
	assert.Contains(t, err.Error(), "opening sync root")
}

// Validates: R-2.10, R-6.2
func TestRoot_Stat_FailureInjection(t *testing.T) {
	t.Parallel()

	runRootHandleErrorCase(t,
		func(root *Root, targetErr error) {
			root.ops.openRoot = func(dir string) (rootHandle, error) {
				return &fakeRootHandle{statErr: targetErr}, nil
			}
		},
		func(root *Root) error {
			info, err := root.Stat("file.txt")
			assert.Nil(t, info)

			return err
		},
		errors.New("stat failed"),
		"stating",
	)
}

// Validates: R-2.10, R-6.2
func TestRoot_ReadDir_FailureInjection(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	readDirErr := errors.New("readdir failed")
	root.ops.lstat = nonNormalizingLstat
	root.ops.openRoot = func(dir string) (rootHandle, error) {
		return &fakeRootHandle{fsys: errorFS{err: readDirErr}}, nil
	}

	entries, err := root.ReadDir("nested")
	require.Error(t, err)
	assert.Nil(t, entries)
	require.ErrorIs(t, err, readDirErr)
	assert.Contains(t, err.Error(), "reading directory")
}

// Validates: R-2.10, R-6.2
func TestRoot_WalkDir_FailureInjection(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	walkErr := errors.New("walk failed")
	root.ops.openRoot = func(dir string) (rootHandle, error) {
		return &fakeRootHandle{fsys: errorFS{err: walkErr}}, nil
	}

	err = root.WalkDir(func(path string, d fs.DirEntry, walkErr error) error {
		return walkErr
	})
	require.Error(t, err)
	require.ErrorIs(t, err, walkErr)
	assert.Contains(t, err.Error(), "walking sync tree")
}

// Validates: R-2.10, R-6.2
func TestRoot_Remove_FailureInjection(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	removeErr := errors.New("remove failed")
	root.ops.remove = func(path string) error {
		return removeErr
	}

	err = root.Remove("file.txt")
	require.Error(t, err)
	require.ErrorIs(t, err, removeErr)
	assert.Contains(t, err.Error(), "removing")
}

// Validates: R-2.10, R-6.2
func TestRoot_Rename_FailureInjection(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	renameErr := errors.New("rename failed")
	root.ops.rename = func(oldpath, newpath string) error {
		return renameErr
	}

	err = root.Rename("old.txt", "new.txt")
	require.Error(t, err)
	require.ErrorIs(t, err, renameErr)
	assert.Contains(t, err.Error(), "renaming")
}

type fakeRootHandle struct {
	openErr     error
	openFileErr error
	statErr     error
	closeErr    error
	fsys        fs.FS
}

func (h *fakeRootHandle) Open(name string) (*os.File, error) {
	if h.openErr != nil {
		return nil, h.openErr
	}

	return nil, errUnexpectedFakeRootCall
}

func (h *fakeRootHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if h.openFileErr != nil {
		return nil, h.openFileErr
	}

	return nil, errUnexpectedFakeRootCall
}

func (h *fakeRootHandle) Stat(name string) (os.FileInfo, error) {
	if h.statErr != nil {
		return nil, h.statErr
	}

	return nil, errUnexpectedFakeRootCall
}

func (h *fakeRootHandle) FS() fs.FS {
	return h.fsys
}

func (h *fakeRootHandle) Close() error {
	return h.closeErr
}

type errorFS struct {
	err error
}

var errUnexpectedFakeRootCall = errors.New("unexpected fake root success path")

func nonNormalizingLstat(path string) (os.FileInfo, error) {
	return nil, errors.New("non-normalizing stat")
}

func runRootHandleErrorCase(
	t *testing.T,
	configure func(root *Root, targetErr error),
	run func(root *Root) error,
	targetErr error,
	wantContains string,
) {
	t.Helper()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	root.ops.lstat = nonNormalizingLstat
	configure(root, targetErr)

	err = run(root)
	require.Error(t, err)
	require.ErrorIs(t, err, targetErr)
	assert.Contains(t, err.Error(), wantContains)
}

func (f errorFS) Open(name string) (fs.File, error) {
	return nil, f.err
}
