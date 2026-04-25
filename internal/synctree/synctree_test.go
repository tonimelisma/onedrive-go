package synctree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

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
func TestRoot_PathStateNoFollow_ReportsSymlinkAsUnsafeDirectory(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.Mkdir(filepath.Join(dir, "target"), 0o700))
	if symlinkErr := os.Symlink("target", filepath.Join(dir, "link")); symlinkErr != nil {
		t.Skipf("symlink not available on this filesystem: %v", symlinkErr)
	}

	state, err := root.PathStateNoFollow("link")
	require.NoError(t, err)
	assert.True(t, state.Exists)
	assert.True(t, state.IsSymlink)
	assert.False(t, state.IsDir)
}

// Validates: R-2.10, R-6.2
func TestRoot_MkdirAllNoFollow_CreatesComponents(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, root.MkdirAllNoFollow(filepath.Join("shortcuts", "docs"), 0o700))

	assert.DirExists(t, filepath.Join(dir, "shortcuts", "docs"))
}

// Validates: R-2.10, R-6.2
func TestRoot_MkdirAllNoFollow_RejectsSymlinkedAncestor(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	outside := t.TempDir()
	if symlinkErr := os.Symlink(outside, filepath.Join(dir, "linked")); symlinkErr != nil {
		t.Skipf("symlink not available on this filesystem: %v", symlinkErr)
	}

	err = root.MkdirAllNoFollow(filepath.Join("linked", "docs"), 0o700)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsafePath)
	assert.NoDirExists(t, filepath.Join(outside, "docs"))
}

// Validates: R-2.10, R-6.2
func TestRoot_RenameWithTemporarySibling_RenamesThroughTemp(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.Mkdir(filepath.Join(dir, "Docs"), 0o700))

	require.NoError(t, root.RenameWithTemporarySibling("Docs", "docs", ".tmp-docs", 3))

	assert.NoDirExists(t, filepath.Join(dir, ".tmp-docs"))
	assert.DirExists(t, filepath.Join(dir, "docs"))
}

// Validates: R-2.10, R-6.2
func TestRoot_DirEmptyNoFollow(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty"), 0o700))
	empty, err := root.DirEmptyNoFollow("empty")
	require.NoError(t, err)
	assert.True(t, empty)

	require.NoError(t, os.Mkdir(filepath.Join(dir, "nonempty"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nonempty", "file.txt"), []byte("data"), 0o600))
	empty, err = root.DirEmptyNoFollow("nonempty")
	require.NoError(t, err)
	assert.False(t, empty)
}

// Validates: R-2.10, R-6.2
func TestRoot_TreesEqualNoFollow_MatchesStructureAndFileContent(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	for _, base := range []string{"left", "right"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, base, "nested", "empty"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, base, "nested", "file.txt"), []byte("same"), 0o600))
	}

	equal, err := root.TreesEqualNoFollow("left", "right")
	require.NoError(t, err)
	assert.True(t, equal)
}

// Validates: R-2.10, R-6.2
func TestRoot_TreesEqualNoFollow_DetectsMismatches(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "content differs",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "left"), 0o700))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "right"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "left", "file.txt"), []byte("left"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "right", "file.txt"), []byte("right"), 0o600))
			},
		},
		{
			name: "missing file",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "left"), 0o700))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "right"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "left", "file.txt"), []byte("left"), 0o600))
			},
		},
		{
			name: "extra empty directory",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "left", "empty"), 0o700))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "right"), 0o700))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			root, err := Open(dir)
			require.NoError(t, err)
			tt.setup(t, dir)

			equal, err := root.TreesEqualNoFollow("left", "right")
			require.NoError(t, err)
			assert.False(t, equal)
		})
	}
}

// Validates: R-2.10, R-6.2
func TestRoot_TreesEqualNoFollow_RejectsSymlinkEntries(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "left"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "right"), 0o700))
	if symlinkErr := os.Symlink("../right", filepath.Join(dir, "left", "link")); symlinkErr != nil {
		t.Skipf("symlink not available on this filesystem: %v", symlinkErr)
	}

	equal, err := root.TreesEqualNoFollow("left", "right")
	require.Error(t, err)
	assert.False(t, equal)
	require.ErrorIs(t, err, ErrUnsupportedTreeEntry)
}

// Validates: R-2.10, R-6.2
func TestRoot_ValidateTreeNoFollow_RejectsSymlinkEntries(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tree"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "target.txt"), []byte("target"), 0o600))
	if symlinkErr := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "tree", "link")); symlinkErr != nil {
		t.Skipf("symlink not available on this filesystem: %v", symlinkErr)
	}

	err = root.ValidateTreeNoFollow("tree")

	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsupportedTreeEntry)
}

type openRejectingRootHandle struct {
	root      rootHandle
	openCalls *int
}

func (h openRejectingRootHandle) Open(name string) (*os.File, error) {
	(*h.openCalls)++
	return nil, errors.New("open should not be called")
}

func (h openRejectingRootHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	//nolint:wrapcheck // test helper delegates all non-Open behavior.
	return h.root.OpenFile(name, flag, perm)
}

func (h openRejectingRootHandle) Stat(name string) (os.FileInfo, error) {
	//nolint:wrapcheck // test helper delegates all non-Open behavior.
	return h.root.Stat(name)
}

func (h openRejectingRootHandle) Lstat(name string) (os.FileInfo, error) {
	//nolint:wrapcheck // test helper delegates all non-Open behavior.
	return h.root.Lstat(name)
}

func (h openRejectingRootHandle) Mkdir(name string, perm os.FileMode) error {
	//nolint:wrapcheck // test helper delegates all non-Open behavior.
	return h.root.Mkdir(name, perm)
}

func (h openRejectingRootHandle) FS() fs.FS {
	return h.root.FS()
}

func (h openRejectingRootHandle) Close() error {
	//nolint:wrapcheck // test helper delegates all non-Open behavior.
	return h.root.Close()
}

// Validates: R-2.10, R-6.2
func TestRoot_ValidateTreeNoFollow_DoesNotReadFileContent(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tree"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tree", "file.txt"), []byte("payload"), 0o600))

	openRoot := root.ops.openRoot
	openCalls := 0
	root.ops.openRoot = func(dir string) (rootHandle, error) {
		handle, err := openRoot(dir)
		if err != nil {
			return nil, err
		}
		return openRejectingRootHandle{root: handle, openCalls: &openCalls}, nil
	}

	require.NoError(t, root.ValidateTreeNoFollow("tree"))
	assert.Zero(t, openCalls)
}

// Validates: R-2.10, R-6.2
func TestRoot_RemoveTreeNoFollow_RemovesRegularTree(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tree", "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tree", "nested", "file.txt"), []byte("data"), 0o600))

	require.NoError(t, root.RemoveTreeNoFollow("tree"))

	assert.NoDirExists(t, filepath.Join(dir, "tree"))
}

// Validates: R-2.10, R-6.2
func TestRoot_RemoveTreeNoFollow_DoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "kept.txt"), []byte("outside"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tree"), 0o700))
	if symlinkErr := os.Symlink(outside, filepath.Join(dir, "tree", "link")); symlinkErr != nil {
		t.Skipf("symlink not available on this filesystem: %v", symlinkErr)
	}

	err = root.RemoveTreeNoFollow("tree")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsupportedTreeEntry)
	assert.FileExists(t, filepath.Join(outside, "kept.txt"))
}

// Validates: R-2.10, R-6.2
func TestRoot_RemoveAllAndChtimes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, root.MkdirAll(filepath.Join("nested", "child"), 0o700))

	file, err := root.OpenFile(filepath.Join("nested", "child", "file.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString("payload")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	targetTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	require.NoError(t, root.Chtimes(filepath.Join("nested", "child", "file.txt"), targetTime, targetTime))

	info, err := root.Stat(filepath.Join("nested", "child", "file.txt"))
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(targetTime), "mtime should be updated")

	require.NoError(t, root.RemoveAll("nested"))
	_, err = root.Stat("nested")
	require.ErrorIs(t, err, os.ErrNotExist)
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
	lstatErr    error
	mkdirErr    error
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

func (h *fakeRootHandle) Lstat(name string) (os.FileInfo, error) {
	if h.lstatErr != nil {
		return nil, h.lstatErr
	}

	return nil, errUnexpectedFakeRootCall
}

func (h *fakeRootHandle) Mkdir(name string, perm os.FileMode) error {
	if h.mkdirErr != nil {
		return h.mkdirErr
	}

	return errUnexpectedFakeRootCall
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
