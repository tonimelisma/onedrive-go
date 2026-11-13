package fsroot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

// An atomic write creates the root, the folders above it and those on the way
// to the target with dirPerm, so a caller has no root to create first.
func TestRoot_AtomicWrite_CreatesMissingFoldersWithDirPerm(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	rootDir := filepath.Join(base, "state", "accounts")
	root, err := Open(rootDir)
	require.NoError(t, err)

	require.NoError(t, root.AtomicWrite("nested/token.json", []byte("{}"), 0o600, 0o700, ".token-*.tmp"))

	for _, dir := range []string{filepath.Join(base, "state"), rootDir, filepath.Join(rootDir, "nested")} {
		info, statErr := os.Stat(dir)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), dir)
	}
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

	tempName, err := createTempThroughHandle(root, "nested", ".managed-*.tmp")
	require.NoError(t, err)
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

// Validates: R-6.4.10
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

// Validates: R-6.4.10
//
// A create always targets a missing name, so the name is still absent after
// any failed create. Only a failure that itself says the name is missing may
// read as absence: a denied folder, a full disk or a read-only mount reported
// as "does not exist" is invisible to the handling each needs.
func TestRoot_OpenFile_CreateFailureKeepsItsErrno(t *testing.T) {
	t.Parallel()

	errInjected := errors.New("injected failure without an errno")

	testCases := []struct {
		name    string
		failure error
	}{
		{name: "permission denied", failure: syscall.EACCES},
		{name: "disk full", failure: syscall.ENOSPC},
		{name: "read-only filesystem", failure: syscall.EROFS},
		{name: "failure without an errno", failure: errInjected},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			root, err := Open(t.TempDir())
			require.NoError(t, err)

			fake := interposeFakeHandle(root)
			fake.openFile = func(name string, _ int, _ os.FileMode) (*os.File, error) {
				return nil, &os.PathError{Op: "openat", Path: name, Err: testCase.failure}
			}

			file, err := root.OpenFile("absent.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			require.ErrorIs(t, err, testCase.failure)
			require.NotErrorIs(t, err, os.ErrNotExist, "the name is absent, but that is not why the create failed")
			assert.Nil(t, file)
		})
	}
}

// Validates: R-6.4.10
//
// Creating a log file in a folder the user cannot write says "permission
// denied", not "does not exist".
func TestRoot_OpenFile_PermissionDeniedIsNotNotExist(t *testing.T) {
	t.Parallel()

	root := readOnlyRoot(t)

	file, err := root.OpenFile("onedrive-go.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.ErrorIs(t, err, os.ErrPermission)
	require.NotErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, file)
}

// Validates: R-6.4.10
//
// Writing config or a token into a folder the user cannot write says
// "permission denied", not "does not exist".
func TestRoot_AtomicWrite_PermissionDeniedIsNotNotExist(t *testing.T) {
	t.Parallel()

	root := readOnlyRoot(t)

	err := root.AtomicWrite("config.toml", []byte("payload"), 0o600, 0o700, ".config-*.tmp")
	require.ErrorIs(t, err, os.ErrPermission)
	require.NotErrorIs(t, err, os.ErrNotExist)
}

// readOnlyRoot returns a managed root on a folder the test cannot write. The
// superuser writes into any folder, so a test that needs one skips as root.
func readOnlyRoot(t *testing.T) *Root {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("the superuser can write into a read-only folder")
	}

	dir := t.TempDir()
	// Searchable but not writable: a lookup inside still answers "absent",
	// which is the condition under which a refused create used to read as one.
	//nolint:gosec // a test folder is made read-only on purpose; it keeps its search bit.
	require.NoError(t, os.Chmod(dir, 0o500))
	// Cleanups run last in, first out, so the folder is writable again before
	// t.TempDir removes it.
	t.Cleanup(func() {
		//nolint:gosec // a test folder gets its owner's write access back so t.TempDir can remove it.
		assert.NoError(t, os.Chmod(dir, 0o700))
	})

	root, err := Open(dir)
	require.NoError(t, err)

	return root
}

// Validates: R-6.4.10
//
// No entry can exist below a file, which ENOTDIR says for a parent that is
// not a directory: that is absence. An existing file that an operation needed
// to be a directory is there, so its ENOTDIR is kept.
func TestRoot_NameBelowAFileIsNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain"), []byte("file"), 0o600))
	root, err := Open(dir)
	require.NoError(t, err)

	_, err = root.Stat("plain/state.db")
	require.ErrorIs(t, err, os.ErrNotExist)

	file, err := root.Open("plain/state.db")
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, file)

	require.ErrorIs(t, root.Remove("plain/state.db"), os.ErrNotExist)

	entries, err := root.ReadDir("plain")
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist, "an existing file listed as a directory is not absent")
	assert.Nil(t, entries)
}

// Validates: R-6.4.10
//
// Nothing can exist beneath a file. A root that is a file, or that lies below
// one, therefore has every name beneath it absent, whatever the operation. A
// listing of a root that is a file finds it there but not a directory, as a
// listing of any other file does; a root below a file is not there at all.
func TestRoot_RootThatIsAFileHasNothingBeneathIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(plain, []byte("file"), 0o600))
	belowFile := filepath.Join(plain, "below")

	for _, rootDir := range []string{plain, belowFile} {
		root, err := Open(rootDir)
		require.NoError(t, err)

		_, err = root.Stat("state.db")
		require.ErrorIs(t, err, os.ErrNotExist, rootDir)

		data, err := root.ReadFile("state.db")
		require.ErrorIs(t, err, os.ErrNotExist, rootDir)
		assert.Nil(t, data)

		require.ErrorIs(t, root.Remove("state.db"), os.ErrNotExist, rootDir)
		require.ErrorIs(t, root.Rename("state.db", "moved.db"), os.ErrNotExist, rootDir)
	}

	fileRoot, err := Open(plain)
	require.NoError(t, err)
	entries, err := fileRoot.ReadDir("")
	require.ErrorIs(t, err, syscall.ENOTDIR, "a root that is a file is there, and it is not a directory")
	require.NotErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, entries)

	missingRoot, err := Open(belowFile)
	require.NoError(t, err)
	entries, err = missingRoot.ReadDir("")
	require.ErrorIs(t, err, os.ErrNotExist, "a root below a file is not there")
	assert.Nil(t, entries)
}

// fifoAnswerTimeout bounds how long an operation on a root that is a FIFO may
// take. It answers from a stat, so it is immediate; opening the FIFO would
// block until a writer appeared, which is forever. The bound is long because
// only a failing run waits for it, and a loaded host must not fail a good one.
const fifoAnswerTimeout = 10 * time.Second

// Validates: R-6.4.10
//
// A root that is not a directory is recognized by a stat before anything
// opens it: os.OpenRoot opens the path for reading, which blocks forever on a
// FIFO. A name beneath such a root is absent, and a listing of it reports
// ENOTDIR, as for a root that is a regular file.
func TestRoot_RootThatIsAFIFOAnswersWithoutOpeningIt(t *testing.T) {
	t.Parallel()

	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("FIFOs not available on this filesystem: %v", err)
	}
	root, err := Open(fifo)
	require.NoError(t, err)

	type answers struct {
		statErr error
		listErr error
	}
	answered := make(chan answers, 1)
	go func() {
		_, statErr := root.Stat("state.db")
		_, listErr := root.ReadDir("")
		answered <- answers{statErr: statErr, listErr: listErr}
	}()

	select {
	case got := <-answered:
		require.ErrorIs(t, got.statErr, os.ErrNotExist)
		require.ErrorIs(t, got.listErr, syscall.ENOTDIR)
		require.NotErrorIs(t, got.listErr, os.ErrNotExist)
	case <-time.After(fifoAnswerTimeout):
		// Hold the FIFO open for writing: the blocked open, and every one
		// after it, then returns, so the goroutine ends before the test fails.
		//nolint:gosec // the test opens the FIFO it created under t.TempDir.
		writer, openErr := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if openErr == nil {
			<-answered
			require.NoError(t, writer.Close())
		}
		require.FailNow(t, "an operation on a root that is a FIFO blocked instead of answering")
	}
}

// When the read and the close of a managed file both fail, each failure keeps
// the step and the name it belongs to.
func TestRoot_ReadFile_ReportsReadAndCloseFailures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600))
	root, err := Open(dir)
	require.NoError(t, err)

	fake := interposeFakeHandle(root)
	fake.open = fake.openThenReturnClosed

	data, err := root.ReadFile("state.json")
	require.ErrorIs(t, err, os.ErrClosed)
	assert.Contains(t, err.Error(), "reading state.json")
	assert.Contains(t, err.Error(), "closing state.json")
	assert.Nil(t, data)
}

// An open whose root handle then fails to close hands back no file, so it
// closes the file itself and reports both failures with what each was doing.
func TestRoot_Open_ReportsRootAndFileCloseFailures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600))
	root, err := Open(dir)
	require.NoError(t, err)

	errRootClose := errors.New("root close failed")
	fake := interposeFakeHandle(root)
	fake.open = fake.openThenReturnClosed
	fake.closeErr = errRootClose

	file, err := root.Open("state.json")
	require.ErrorIs(t, err, errRootClose)
	require.ErrorIs(t, err, os.ErrClosed)
	assert.Contains(t, err.Error(), "closing root")
	assert.Contains(t, err.Error(), "closing state.json")
	assert.Nil(t, file)
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
	closeErr error // returned by Close once the real handle is closed

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

func (h *fakeRootHandle) Close() error {
	if err := h.rootHandle.Close(); err != nil {
		return fmt.Errorf("closing the real handle: %w", err)
	}

	return h.closeErr
}

// openThenReturnClosed opens name for real and hands back the handle already
// closed, so the first read, sync or close through it fails.
func (h *fakeRootHandle) openThenReturnClosed(name string) (*os.File, error) {
	opened, err := h.rootHandle.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening %s for the fake: %w", name, err)
	}
	if closeErr := opened.Close(); closeErr != nil {
		return nil, fmt.Errorf("closing %s for the fake: %w", name, closeErr)
	}

	return opened, nil
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

	_, err := createTempThroughHandle(f.root, "sub", ".managed-*.tmp")
	require.Error(t, err, "a temp file must not be created through a symlinked directory")
	assert.Empty(t, f.outsideEntryNames(t), "nothing may be created outside the managed root")
}

// Validates: R-6.4.10
//
// A name beneath a symlink that leaves the root is refused for reads too, and
// the refusal never reads as absence, which callers take as permission to
// create.
func TestRoot_Stat_RefusesSymlinkedParent(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "present.txt", "OUTSIDE")

	_, err := f.root.Stat("sub/present.txt")
	require.Error(t, err, "a stat must not answer about a file outside the managed root")

	data, err := f.root.ReadFile("sub/present.txt")
	require.Error(t, err, "a read must not return a file outside the managed root")
	assert.Nil(t, data)

	_, err = f.root.Stat("sub/missing.txt")
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist, "an escape must not read as an absent name")
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
func TestRoot_AtomicWrite_SyncsTargetDirectory(t *testing.T) {
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
func TestRoot_AtomicWrite_ReportsDirectorySyncFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	// A closed handle on the real directory makes the sync itself fail.
	fake := interposeFakeHandle(root)
	fake.open = fake.openThenReturnClosed

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

// Validates: R-6.5.4
//
// A filesystem that cannot fsync a directory (FUSE, SMB) answers EINVAL or
// ENOTSUP. The rename has happened and the file's own bytes were synced, so
// the write succeeds, as localpath.AtomicWrite does; any other failure of the
// directory sync is still reported.
func TestRoot_AtomicWrite_ToleratesUnsupportedDirectorySync(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		errno syscall.Errno
	}{
		{name: "EINVAL", errno: syscall.EINVAL},
		{name: "ENOTSUP", errno: syscall.ENOTSUP},
		{name: "EOPNOTSUPP", errno: syscall.EOPNOTSUPP},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			root, err := Open(dir)
			require.NoError(t, err)

			var synced []string
			root.ops.syncDir = func(opened *os.File) error {
				synced = append(synced, opened.Name())

				return &os.PathError{Op: "sync", Path: opened.Name(), Err: testCase.errno}
			}

			require.NoError(t, root.AtomicWrite("nested/state.json", []byte("{}"), 0o600, 0o700, ".state-*.tmp"))
			assert.Equal(t, []string{filepath.Join(dir, "nested")}, synced, "the directory the rename landed in is synced once")

			data, err := root.ReadFile("nested/state.json")
			require.NoError(t, err)
			assert.Equal(t, "{}", string(data))
		})
	}
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

	name, err := createTempThroughHandle(root, ".", ".managed-*.tmp")
	require.ErrorIs(t, err, os.ErrExist)
	assert.Empty(t, name)
	assert.Equal(t, tempNameAttempts, attempts, "a suffix source that keeps colliding must not loop forever")
}

func TestRoot_CreateTemp_RejectsPatternWithSeparator(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	_, err = createTempThroughHandle(root, ".", "../.managed-*.tmp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains a path separator")
}

// createTempThroughHandle claims a temp file in cleanDir the way AtomicWrite
// does, on one rooted handle, closes it and returns its name relative to the
// root.
func createTempThroughHandle(root *Root, cleanDir, pattern string) (string, error) {
	var tempName string
	err := root.withRoot(func(handle rootHandle) error {
		temp, name, createErr := root.createTemp(handle, cleanDir, pattern)
		if createErr != nil {
			return createErr
		}
		tempName = name
		if closeErr := temp.Close(); closeErr != nil {
			return fmt.Errorf("closing temp file: %w", closeErr)
		}

		return nil
	})

	return tempName, err
}
