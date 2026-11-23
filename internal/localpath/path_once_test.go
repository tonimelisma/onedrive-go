package localpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pathOnceCase is one failing call of this package and the paths its error
// must name once each.
type pathOnceCase struct {
	name   string
	call   func(t *testing.T, dir string) error
	paths  func(dir string) []string
	reason string
}

// onMissing is call on the name "missing" in dir, which is not there.
func onMissing(call func(path string) error) func(*testing.T, string) error {
	return func(_ *testing.T, dir string) error {
		return call(filepath.Join(dir, "missing"))
	}
}

func missingPath(dir string) []string { return []string{filepath.Join(dir, "missing")} }

const noSuchFile = "no such file or directory"

// pathOnceCases fail one call of each function of this package that names
// a path. dir holds the regular file "file" and nothing else.
func pathOnceCases() []pathOnceCase {
	errOf := func(_ any, err error) error { return err }
	filePath := func(dir string) []string { return []string{filepath.Join(dir, "file")} }

	return []pathOnceCase{
		{name: "Open", call: onMissing(func(p string) error { return errOf(Open(p)) }), paths: missingPath, reason: noSuchFile},
		{name: "OpenRegular", call: onMissing(func(p string) error { return errOf(OpenRegular(p)) }), paths: missingPath, reason: noSuchFile},
		{
			name:   "OpenRegular of a folder",
			call:   func(_ *testing.T, dir string) error { return errOf(OpenRegular(dir)) },
			paths:  func(dir string) []string { return []string{dir} },
			reason: ErrNotRegularFile.Error(),
		},
		{
			name: "OpenFile", paths: missingPath, reason: noSuchFile,
			call: onMissing(func(p string) error { return errOf(OpenFile(p, os.O_RDONLY, 0)) }),
		},
		{name: "ReadFile", call: onMissing(func(p string) error { return errOf(ReadFile(p)) }), paths: missingPath, reason: noSuchFile},
		{name: "Stat", call: onMissing(func(p string) error { return errOf(Stat(p)) }), paths: missingPath, reason: noSuchFile},
		{name: "Lstat", call: onMissing(func(p string) error { return errOf(Lstat(p)) }), paths: missingPath, reason: noSuchFile},
		{
			name:   "MkdirAll onto a file",
			call:   func(_ *testing.T, dir string) error { return MkdirAll(filepath.Join(dir, "file"), 0o700) },
			paths:  filePath,
			reason: "not a directory",
		},
		{name: "Chmod", call: onMissing(func(p string) error { return Chmod(p, 0o600) }), paths: missingPath, reason: noSuchFile},
		{name: "Remove", call: onMissing(Remove), paths: missingPath, reason: noSuchFile},
		{
			name: "Rename",
			call: func(_ *testing.T, dir string) error {
				return Rename(filepath.Join(dir, "missing"), filepath.Join(dir, "other"))
			},
			paths:  func(dir string) []string { return []string{filepath.Join(dir, "missing"), filepath.Join(dir, "other")} },
			reason: noSuchFile,
		},
		{name: "ReadDir", call: onMissing(func(p string) error { return errOf(ReadDir(p)) }), paths: missingPath, reason: noSuchFile},
		{name: "OpenDir", call: onMissing(func(p string) error { return errOf(OpenDir(p)) }), paths: missingPath, reason: noSuchFile},
		{
			name: "DirReader.Next after Close", paths: func(dir string) []string { return []string{dir} },
			reason: "use of closed file",
			call: func(t *testing.T, dir string) error {
				t.Helper()
				reader, err := OpenDir(dir)
				require.NoError(t, err)
				require.NoError(t, reader.Close())

				return errOf(reader.Next(1))
			},
		},
		{
			name: "DirReader.Close twice", paths: func(dir string) []string { return []string{dir} },
			reason: os.ErrClosed.Error(),
			call: func(t *testing.T, dir string) error {
				t.Helper()
				reader, err := OpenDir(dir)
				require.NoError(t, err)
				require.NoError(t, reader.Close())

				return reader.Close()
			},
		},
		{
			name: "EvalSymlinks", paths: missingPath, reason: noSuchFile,
			call: onMissing(func(p string) error { return errOf(EvalSymlinks(p)) }),
		},
		{
			name: "Chtimes", paths: missingPath, reason: noSuchFile,
			call: onMissing(func(p string) error { return Chtimes(p, time.Now(), time.Now()) }),
		},
		{name: "SyncDir", call: onMissing(SyncDir), paths: missingPath, reason: noSuchFile},
		{
			name: "CreateTemp", paths: missingPath, reason: noSuchFile,
			call: onMissing(func(p string) error { return errOf(CreateTemp(p, "temp-*")) }),
		},
		{
			name: "MkdirTemp", paths: missingPath, reason: noSuchFile,
			call: onMissing(func(p string) error { return errOf(MkdirTemp(p, "temp-*")) }),
		},
		{
			name: "WriteDisposableFile",
			call: func(_ *testing.T, dir string) error {
				return WriteDisposableFile(filepath.Join(dir, "missing", "x"), nil, 0o600)
			},
			paths:  func(dir string) []string { return []string{filepath.Join(dir, "missing", "x")} },
			reason: noSuchFile,
		},
		{
			name: "reading and closing a closed file", paths: filePath, reason: os.ErrClosed.Error(),
			call: func(t *testing.T, dir string) error {
				t.Helper()
				path := filepath.Join(dir, "file")
				file, err := Open(path)
				require.NoError(t, err)
				require.NoError(t, file.Close())

				return errOf(readAndClose(file, path))
			},
		},
	}
}

// Every failure of this package names the path it was given once, after
// the verb of what was being done, and then the reason the filesystem gave:
// the os error inside it holds the path too and is not printed a second
// time. A failure that names two paths names each once.
//
// Validates: R-6.6.26
func TestFailures_NameEachPathOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range pathOnceCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The folder is named by its resolved path, as macOS keeps temp
			// folders below /var, a link to /private/var: a call that
			// resolves links then reaches the very path it was given.
			dir, resolveErr := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, resolveErr)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), nil, 0o600))

			err := tc.call(t, dir)

			require.Error(t, err)
			text := err.Error()
			for _, path := range tc.paths(dir) {
				assert.Equal(t, 1, strings.Count(text, path), "%q names %s once", text, path)
			}
			assert.True(t, strings.HasSuffix(text, ": "+tc.reason), "%q ends with the reason", text)
		})
	}
}

// A relative path is named as it was given, not as the absolute path the
// call reached; the os error inside the failure still holds the absolute
// one.
//
// Validates: R-6.6.26
func TestFailures_NameARelativePathAsItWasGiven(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, err := Open(filepath.Join("rel", "missing"))

	require.EqualError(t, err, "opening "+filepath.Join("rel", "missing")+": "+noSuchFile)
	pathErr, ok := errors.AsType[*fs.PathError](err)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(dir, "rel", "missing"), pathErr.Path)
}

// FailedPath names the path an os error names as the caller gave it when a
// call of this package failed on that very path, the os error's own path
// when the error names another one (a parent MkdirAll could not create) or
// came from outside this package, and nothing for an error on no path.
//
// Validates: R-6.6.26
func TestFailedPath_IsThePathAsTheCallerGaveIt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	require.NoError(t, os.WriteFile("file", nil, 0o600))

	_, openErr := Open(filepath.Join("rel", "missing"))
	mkdirErr := MkdirAll(filepath.Join("file", "sub"), 0o700)
	_, osErr := os.Open(filepath.Join(dir, "absent")) //nolint:gosec // the test opens a path under its own temp folder

	assert.Equal(t, filepath.Join("rel", "missing"), FailedPath(fmt.Errorf("reading: %w", openErr)))
	assert.Equal(t, filepath.Join(dir, "file"), FailedPath(mkdirErr), "the parent the os error names")
	assert.Equal(t, filepath.Join(dir, "absent"), FailedPath(osErr))
	assert.Empty(t, FailedPath(errors.New("no path here")))
}

// A failure answers errors.Is and errors.As as the os error it holds does:
// the errno and its fs sentinel, the *fs.PathError with its operation and
// absolute path, the *os.LinkError of a rename, this package's own
// sentinel, and IsMissingName.
//
// Validates: R-6.6.26
func TestFailures_AnswerAsTheOSErrorTheyHold(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")

	_, openErr := Open(missing)
	require.ErrorIs(t, openErr, fs.ErrNotExist)
	require.ErrorIs(t, openErr, syscall.ENOENT)
	assert.True(t, IsMissingName(openErr))
	pathErr, ok := errors.AsType[*fs.PathError](openErr)
	require.True(t, ok)
	assert.Equal(t, "open", pathErr.Op)
	assert.Equal(t, missing, pathErr.Path)

	other := filepath.Join(dir, "other")
	renameErr := Rename(missing, other)
	require.ErrorIs(t, renameErr, syscall.ENOENT)
	linkErr, ok := errors.AsType[*os.LinkError](renameErr)
	require.True(t, ok)
	assert.Equal(t, "rename", linkErr.Op)
	assert.Equal(t, missing, linkErr.Old)
	assert.Equal(t, other, linkErr.New)

	_, regularErr := OpenRegular(dir)
	require.ErrorIs(t, regularErr, ErrNotRegularFile)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), nil, 0o600))
	notDirErr := MkdirAll(filepath.Join(dir, "file", "sub"), 0o700)
	require.ErrorIs(t, notDirErr, syscall.ENOTDIR)
	assert.True(t, IsMissingName(notDirErr), "a file on the way is a missing name")
}

// An os error that names another path than the one the call was given, a
// parent MkdirAll could not create, is printed whole after the failure: only
// an os error on the very path the call reached gives up its path.
//
// Validates: R-6.6.26
func TestFailures_KeepAnOSErrorThatNamesAnotherPath(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	sub := filepath.Join(file, "sub")

	err := MkdirAll(sub, 0o700)

	require.EqualError(t, err, "creating directory "+sub+": mkdir "+file+": not a directory")
}

// A file the user cannot read fails as permission denied, and names the
// path once.
//
// Validates: R-6.6.26
func TestFailures_AnUnreadableFileIsPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores file modes")
	}
	t.Parallel()

	locked := filepath.Join(t.TempDir(), "locked.txt")
	require.NoError(t, os.WriteFile(locked, nil, 0o000))

	_, err := OpenRegular(locked)

	require.ErrorIs(t, err, fs.ErrPermission)
	require.EqualError(t, err, "opening "+locked+": permission denied")
}

// WithoutPath is an error's text with every local path its failures name
// left out, each keeping its verb and its reason: what a line that names the
// path itself prints after it. It leaves out the path of this package's
// failures, of an os error on its own and of a failure's os error that names
// another path, and leaves every other word of the chain as it was.
//
// Validates: R-6.6.26, R-1.3.1
func TestWithoutPath_LeavesOutEveryLocalPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, openErr := Open(filepath.Join(dir, "missing"))
	opened, err := Open(file)
	require.NoError(t, err)
	require.NoError(t, opened.Close())
	_, closedErr := readAndClose(opened, file)
	renameErr := Rename(filepath.Join(dir, "missing"), filepath.Join(dir, "other"))

	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "a failure under a wrap", err: fmt.Errorf("hashing: %w", openErr), want: "hashing: opening: " + noSuchFile},
		{name: "a rename", err: renameErr, want: "renaming: " + noSuchFile},
		{name: "two failures on one line", err: closedErr, want: "reading: file already closed; closing: file already closed"},
		{
			name: "two failures joined", err: errors.Join(openErr, renameErr),
			want: "opening: " + noSuchFile + "\nrenaming: " + noSuchFile,
		},
		{
			name: "an os error of another path", err: MkdirAll(filepath.Join(file, "sub"), 0o700),
			want: "creating directory: mkdir: not a directory",
		},
		{
			name: "an os error on its own",
			err:  fmt.Errorf("reading content to hash: %w", &fs.PathError{Op: "read", Path: file, Err: syscall.EIO}),
			want: "reading content to hash: read: input/output error",
		},
		{
			name: "a link error on its own",
			err:  &os.LinkError{Op: "link", Old: file, New: filepath.Join(dir, "copy"), Err: syscall.EEXIST},
			want: "link: file exists",
		},
		{name: "no path at all", err: fmt.Errorf("listing: %w", errors.New("boom")), want: "listing: boom"},
		{name: "no error", err: nil, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, WithoutPath(tc.err))
		})
	}
}
