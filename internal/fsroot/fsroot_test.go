package fsroot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
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

	baseErr := errors.New("write failed")
	removeErr := errors.New("remove failed")

	failing := &fakeRootHandle{remove: func(string) error { return removeErr }}
	err := cleanupTempOnError(failing, ".managed-a.tmp", baseErr)
	require.ErrorIs(t, err, baseErr)
	require.ErrorIs(t, err, removeErr)
	assert.Contains(t, err.Error(), "removing temp file .managed-a.tmp")

	// A temp file already renamed into place is not a second failure.
	gone := &fakeRootHandle{remove: func(string) error { return os.ErrNotExist }}
	require.EqualError(t, cleanupTempOnError(gone, ".managed-b.tmp", baseErr), baseErr.Error())
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
//
// Every failure after the temp file exists removes it, through the same
// rooted handle the write used, and reports the step that failed.
func TestRoot_AtomicWrite_FailureInjection(t *testing.T) {
	t.Parallel()

	errDiskFull := errors.New("disk full")
	errRenameFailed := errors.New("rename failed")
	errRemoveFailed := errors.New("remove failed")

	testCases := []atomicWriteFailureCase{
		{
			name: "create temp failure",
			configure: func(h *fakeRootHandle) {
				h.openFile = func(string, int, os.FileMode) (*os.File, error) { return nil, errDiskFull }
			},
			wantErr:      errDiskFull,
			wantContains: "creating temp file",
		},
		{
			name: "permission failure cleans temp",
			configure: func(h *fakeRootHandle) {
				h.openFile = h.createThenReturnClosed
			},
			wantErr:        os.ErrClosed,
			wantContains:   "setting temp file permissions",
			wantTempRemove: true,
		},
		{
			name: "write failure cleans temp",
			configure: func(h *fakeRootHandle) {
				h.openFile = h.createThenReturnReadOnly
			},
			wantErr:        syscall.EBADF,
			wantContains:   "writing temp file",
			wantTempRemove: true,
		},
		{
			name: "rename failure joins cleanup failure",
			configure: func(h *fakeRootHandle) {
				h.rename = func(string, string) error { return errRenameFailed }
				h.remove = func(string) error { return errRemoveFailed }
			},
			wantErr:        errRenameFailed,
			wantContains:   "renaming",
			wantAlsoErr:    errRemoveFailed,
			wantTempRemove: true,
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
	name           string
	configure      func(h *fakeRootHandle)
	wantErr        error
	wantContains   string
	wantAlsoErr    error
	wantTempRemove bool
}

func runAtomicWriteFailureCase(t *testing.T, testCase atomicWriteFailureCase) {
	t.Helper()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	fake := interposeFakeHandle(root)
	testCase.configure(fake)

	err = root.AtomicWrite("config.toml", []byte("payload"), 0o600, 0o700, ".tmp-*.tmp")
	require.ErrorIs(t, err, testCase.wantErr)
	assert.Contains(t, err.Error(), testCase.wantContains)
	if testCase.wantAlsoErr != nil {
		require.ErrorIs(t, err, testCase.wantAlsoErr)
	}

	if testCase.wantTempRemove {
		require.Len(t, fake.created, 1)
		assert.Equal(t, fake.created, fake.removed, "the temp file the write created must be the one removed")
	}

	if testCase.wantAlsoErr == nil {
		matches, globErr := filepath.Glob(filepath.Join(dir, ".tmp-*.tmp"))
		require.NoError(t, globErr)
		assert.Empty(t, matches, "no temp file may survive a failed write")
	}
	assert.NoFileExists(t, filepath.Join(dir, "config.toml"))
}

// fakeRootHandle wraps the real rooted handle and lets a test replace single
// operations, so failure injection runs through the seam production uses.
// It records the names of temp files created and removed through it.
type fakeRootHandle struct {
	rootHandle // the real handle, reached by every operation not replaced

	openFile func(name string, flag int, perm os.FileMode) (*os.File, error)
	open     func(name string) (*os.File, error)
	rename   func(oldname, newname string) error
	remove   func(name string) error

	created []string
	removed []string
}

// interposeFakeHandle makes every handle root opens a fake wrapping the real
// one, and returns the fake for the test to configure.
func interposeFakeHandle(root *Root) *fakeRootHandle {
	fake := &fakeRootHandle{}
	openReal := root.ops.openRoot
	root.ops.openRoot = func(dir string) (rootHandle, error) {
		realHandle, err := openReal(dir)
		if err != nil {
			return nil, err
		}
		fake.rootHandle = realHandle

		return fake, nil
	}

	return fake
}

func (h *fakeRootHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if h.openFile != nil {
		return h.openFile(name, flag, perm)
	}

	file, err := h.rootHandle.OpenFile(name, flag, perm)
	if err == nil && flag&os.O_EXCL != 0 {
		h.created = append(h.created, name)
	}

	//nolint:wrapcheck // the fake passes the real handle's error through unchanged, as osRootHandle does.
	return file, err
}

func (h *fakeRootHandle) Open(name string) (*os.File, error) {
	if h.open != nil {
		return h.open(name)
	}

	//nolint:wrapcheck // the fake passes the real handle's error through unchanged, as osRootHandle does.
	return h.rootHandle.Open(name)
}

func (h *fakeRootHandle) Rename(oldname, newname string) error {
	if h.rename != nil {
		return h.rename(oldname, newname)
	}

	//nolint:wrapcheck // the fake passes the real handle's error through unchanged, as osRootHandle does.
	return h.rootHandle.Rename(oldname, newname)
}

func (h *fakeRootHandle) Remove(name string) error {
	h.removed = append(h.removed, name)
	if h.remove != nil {
		return h.remove(name)
	}

	//nolint:wrapcheck // the fake passes the real handle's error through unchanged, as osRootHandle does.
	return h.rootHandle.Remove(name)
}

// createThenReturnClosed creates the temp file for real and hands back a
// closed handle to it, so the first step of the write fails.
func (h *fakeRootHandle) createThenReturnClosed(name string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := h.rootHandle.OpenFile(name, flag, perm)
	if err != nil {
		return nil, fmt.Errorf("creating fake temp file: %w", err)
	}
	h.created = append(h.created, name)
	if closeErr := file.Close(); closeErr != nil {
		return nil, fmt.Errorf("closing fake temp file: %w", closeErr)
	}

	return file, nil
}

// createThenReturnReadOnly creates the temp file for real and hands back a
// read-only handle to it, so writing the contents fails.
func (h *fakeRootHandle) createThenReturnReadOnly(name string, flag int, perm os.FileMode) (*os.File, error) {
	if _, err := h.createThenReturnClosed(name, flag, perm); err != nil {
		return nil, err
	}

	readOnly, err := h.rootHandle.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("reopening fake temp file read-only: %w", err)
	}

	return readOnly, nil
}

// Each step of writing the temp file reports its own failure, closing the
// file on the way out.
func TestWriteTempFile_ReportsEachStepFailure(t *testing.T) {
	t.Parallel()

	errStep := errors.New("injected")

	testCases := []struct {
		name         string
		temp         *fakeTempFile
		wantContains string
	}{
		{name: "chmod", temp: &fakeTempFile{chmodErr: errStep}, wantContains: "setting temp file permissions"},
		{name: "write", temp: &fakeTempFile{writeErr: errStep}, wantContains: "writing temp file"},
		{name: "sync", temp: &fakeTempFile{syncErr: errStep}, wantContains: "syncing temp file"},
		{name: "close", temp: &fakeTempFile{closeErr: errStep}, wantContains: "closing temp file"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := writeTempFile(testCase.temp, []byte("payload"), 0o600)
			require.ErrorIs(t, err, errStep)
			assert.Contains(t, err.Error(), testCase.wantContains)
			assert.Equal(t, 1, testCase.temp.closeCalls, "the temp file is closed exactly once")
		})
	}
}

type fakeTempFile struct {
	chmodErr   error
	writeErr   error
	syncErr    error
	closeErr   error
	closeCalls int
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
	f.closeCalls++

	return f.closeErr
}

// escapeFixture builds a managed root containing "sub", a symlink to a
// directory OUTSIDE the root. A name under "sub/" is lexically inside the root
// but physically outside it, which is the escape the containment tests pin
// closed.
type escapeFixture struct {
	root    *Root
	rootDir string
	outside string
}

func newEscapeFixture(t *testing.T) escapeFixture {
	t.Helper()

	base := t.TempDir()
	rootDir := filepath.Join(base, "state")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(rootDir, 0o700))
	require.NoError(t, os.MkdirAll(outside, 0o700))

	if err := os.Symlink(outside, filepath.Join(rootDir, "sub")); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	root, err := Open(rootDir)
	require.NoError(t, err)

	return escapeFixture{root: root, rootDir: rootDir, outside: outside}
}

func (f escapeFixture) writeOutside(t *testing.T, name, content string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(f.outside, name), []byte(content), 0o600))
}

func (f escapeFixture) requireOutsideContent(t *testing.T, name, want string) {
	t.Helper()

	//nolint:gosec // test fixture reads a path it just created under t.TempDir.
	got, err := os.ReadFile(filepath.Join(f.outside, name))
	require.NoError(t, err, "the file outside the managed root must survive")
	assert.Equal(t, want, string(got), "content outside the managed root must be unmodified")
}

func (f escapeFixture) outsideEntryNames(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(f.outside)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// Validates: R-6.4.10
func TestRoot_Remove_RefusesSymlinkedParent(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "victim.txt", "PRECIOUS")

	err := f.root.Remove("sub/victim.txt")
	require.Error(t, err, "remove through a symlinked parent must fail closed")
	f.requireOutsideContent(t, "victim.txt", "PRECIOUS")
}

// Validates: R-6.4.10
func TestRoot_Rename_RefusesSymlinkedParent(t *testing.T) {
	t.Parallel()

	t.Run("source", func(t *testing.T) {
		t.Parallel()

		f := newEscapeFixture(t)
		f.writeOutside(t, "source.txt", "SOURCE")

		err := f.root.Rename("sub/source.txt", "moved.txt")
		require.Error(t, err, "a rename must not pull a file in through a symlinked parent")
		f.requireOutsideContent(t, "source.txt", "SOURCE")
		assert.NoFileExists(t, filepath.Join(f.rootDir, "moved.txt"))
	})

	t.Run("destination", func(t *testing.T) {
		t.Parallel()

		f := newEscapeFixture(t)
		f.writeOutside(t, "victim.txt", "PRECIOUS")
		require.NoError(t, os.WriteFile(filepath.Join(f.rootDir, "inside.txt"), []byte("INSIDE"), 0o600))

		err := f.root.Rename("inside.txt", "sub/victim.txt")
		require.Error(t, err, "a rename must not replace a file through a symlinked parent")
		f.requireOutsideContent(t, "victim.txt", "PRECIOUS")
		assert.FileExists(t, filepath.Join(f.rootDir, "inside.txt"))
	})
}

// Validates: R-6.4.10
func TestRoot_AtomicWrite_RefusesSymlinkedParent(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "state.json", "PRECIOUS")

	err := f.root.AtomicWrite("sub/state.json", []byte("REPLACED"), 0o600, 0o700, ".state-*.tmp")
	require.Error(t, err, "an atomic write through a symlinked parent must fail closed")
	f.requireOutsideContent(t, "state.json", "PRECIOUS")
	assert.Equal(t, []string{"state.json"}, f.outsideEntryNames(t),
		"no temp file may be left outside the managed root")
}

// Validates: R-6.4.10
func TestRoot_AtomicWrite_RefusesDirectoryCreationBelowSymlinkedParent(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)

	err := f.root.AtomicWrite("sub/created/state.json", []byte("NEW"), 0o600, 0o700, ".state-*.tmp")
	require.Error(t, err, "creating the target directory through a symlinked parent must fail closed")
	assert.Empty(t, f.outsideEntryNames(t), "nothing may be created outside the managed root")
}

// Validates: R-6.4.10
func TestRoot_CreateTemp_RefusesSymlinkedDir(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)

	temp, _, err := f.root.CreateTemp("sub", ".managed-*.tmp")
	if err == nil {
		require.NoError(t, temp.Close())
	}
	require.Error(t, err, "a temp file must not be created through a symlinked directory")
	assert.Empty(t, f.outsideEntryNames(t), "nothing may be created outside the managed root")
}

// Validates: R-6.4.10
//
// Containment constrains escapes, not symlinks as such: a managed root whose
// own path is a symlink (a config folder linked into a dotfiles checkout) and
// a link that stays inside the root keep working.
func TestRoot_AtomicWrite_FollowsSymlinksThatStayInside(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(realDir, "nested"), 0o700))
	linkedRoot := filepath.Join(base, "linked")
	if err := os.Symlink(realDir, linkedRoot); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}
	require.NoError(t, os.Symlink("nested", filepath.Join(realDir, "inner")))

	root, err := Open(linkedRoot)
	require.NoError(t, err)

	require.NoError(t, root.AtomicWrite("inner/state.json", []byte("{}"), 0o600, 0o700, ".state-*.tmp"))

	//nolint:gosec // test reads a path it just created under t.TempDir.
	data, err := os.ReadFile(filepath.Join(realDir, "nested", "state.json"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(data))
}

// Validates: R-6.5.4
//
// Syncing the temp file makes its contents durable, but the rename that gives
// those contents their final name is a directory update. Until the directory
// itself is synced, a crash can leave the target missing or still holding its
// previous contents even though the new bytes reached the disk -- the one
// outcome an atomic write exists to rule out.
func TestAtomicWriteSyncsTargetDirectory(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	// The directory sync is the write's only plain open through the handle.
	var synced []string
	fake := interposeFakeHandle(root)
	fake.open = func(name string) (*os.File, error) {
		synced = append(synced, name)

		return fake.rootHandle.Open(name)
	}

	require.NoError(t, root.AtomicWrite("nested/state.json", []byte("{}"), 0o600, 0o700, ".state-*.tmp"))

	assert.Equal(t, []string{"nested"}, synced,
		"the sync must target the directory the rename landed in, not the managed root")
}

// Validates: R-6.5.4
//
// A write whose durability cannot be established must say so. The rename has
// already happened, so the contents are visible, but reporting success would
// claim a guarantee the filesystem refused to give.
func TestAtomicWriteReportsDirectorySyncFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	// A closed handle on the real directory makes the sync itself fail.
	fake := interposeFakeHandle(root)
	fake.open = func(name string) (*os.File, error) {
		opened, openErr := fake.rootHandle.Open(name)
		if openErr != nil {
			return nil, fmt.Errorf("opening directory for the fake: %w", openErr)
		}
		if closeErr := opened.Close(); closeErr != nil {
			return nil, fmt.Errorf("closing directory for the fake: %w", closeErr)
		}

		return opened, nil
	}

	err = root.AtomicWrite("state.json", []byte("{}"), 0o600, 0o700, ".state-*.tmp")
	require.ErrorIs(t, err, os.ErrClosed)
	assert.Contains(t, err.Error(), "syncing directory")

	// The bytes are still in place: only the durability guarantee failed.
	unfaked, err := Open(dir)
	require.NoError(t, err)
	data, readErr := unfaked.ReadFile("state.json")
	require.NoError(t, readErr)
	assert.Equal(t, "{}", string(data))
}

// Validates: R-6.4.10
//
// A temp name is claimed with O_EXCL, so a name another writer holds, or a
// symlink planted at it, is never opened or replaced: the write retries with
// a fresh suffix.
func TestRoot_AtomicWrite_TempNameCollisionRetries(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "victim.txt", "PRECIOUS")
	require.NoError(t, os.WriteFile(filepath.Join(f.rootDir, ".state-taken.tmp"), []byte("OTHER WRITER"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(f.outside, "victim.txt"), filepath.Join(f.rootDir, ".state-planted.tmp")))

	suffixes := []string{"taken", "planted", "fresh"}
	f.root.ops.tempSuffix = func() string {
		next := suffixes[0]
		suffixes = suffixes[1:]

		return next
	}

	require.NoError(t, f.root.AtomicWrite("state.json", []byte("{}"), 0o600, 0o700, ".state-*.tmp"))
	assert.Empty(t, suffixes, "each taken name must be retried with the next suffix")

	data, err := f.root.ReadFile("state.json")
	require.NoError(t, err)
	assert.Equal(t, "{}", string(data))

	other, err := f.root.ReadFile(".state-taken.tmp")
	require.NoError(t, err)
	assert.Equal(t, "OTHER WRITER", string(other), "another writer's temp file must be left alone")
	f.requireOutsideContent(t, "victim.txt", "PRECIOUS")
	assert.NoFileExists(t, filepath.Join(f.rootDir, ".state-fresh.tmp"), "the claimed temp file is renamed into place")
}

// Validates: R-6.4.10
func TestRoot_CreateTemp_StopsAfterBoundedNameCollisions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".managed-taken.tmp"), nil, 0o600))

	attempts := 0
	root.ops.tempSuffix = func() string {
		attempts++

		return "taken"
	}

	temp, name, err := root.CreateTemp("", ".managed-*.tmp")
	require.ErrorIs(t, err, os.ErrExist)
	assert.Nil(t, temp)
	assert.Empty(t, name)
	assert.Equal(t, tempNameAttempts, attempts, "a suffix source that keeps colliding must not loop forever")
}

func TestRoot_CreateTemp_RejectsPatternWithSeparator(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	temp, _, err := root.CreateTemp("", "../.managed-*.tmp")
	if err == nil {
		require.NoError(t, temp.Close())
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains a path separator")
}
