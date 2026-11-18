// Package localpath provides explicit arbitrary-local-path filesystem
// operations. Unlike fsroot, it does not model a pre-established managed root;
// each call treats the supplied local path itself as the trust boundary.
package localpath

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// cleanPath refuses an empty path, which filepath.Clean would turn into ".",
// the working directory, and cleans every other one.
func cleanPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}

	return filepath.Clean(path), nil
}

// absolutePath cleans path and resolves a relative one against the working
// directory. An empty path is refused.
func absolutePath(path string) (string, error) {
	clean, err := cleanPath(path)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", failure("resolving path", clean, err)
	}

	return abs, nil
}

// Open opens path for reading. Like every function in this package it
// refuses an empty path, resolves a relative one against the working
// directory and names the path once in its error, as it was given
// (callError).
func Open(path string) (*os.File, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	file, err := os.Open(abs)
	if err != nil {
		return nil, failure("opening", path, err, abs)
	}

	return file, nil
}

// ErrNotRegularFile reports that a path names a FIFO, socket, device or other
// non-regular file, which sync never reads.
var ErrNotRegularFile = errors.New("not a regular file")

// OpenRegular opens path read-only and returns ErrNotRegularFile unless it is
// a regular file. The open never blocks: a plain open of a FIFO waits for a
// writer forever, which would hang the engine and its shutdown on any FIFO in
// the sync tree. O_NONBLOCK has no effect on regular files.
func OpenRegular(path string) (*os.File, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	file, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, failure("opening", path, err, abs)
	}

	info, err := file.Stat()
	if err != nil {
		return nil, closeAfterFailure(file, failure("stat", path, err, abs))
	}
	if !info.Mode().IsRegular() {
		return nil, closeAfterFailure(file, failure("opening", path, ErrNotRegularFile))
	}

	return file, nil
}

// OpenFile opens path with flag. A file the call creates asks for perm; the
// umask applies.
func OpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	file, err := os.OpenFile(abs, flag, perm)
	if err != nil {
		return nil, failure("opening", path, err, abs)
	}

	return file, nil
}

// ReadFile returns the whole content of the file at path.
func ReadFile(path string) ([]byte, error) {
	file, err := Open(path)
	if err != nil {
		return nil, err
	}

	return readAndClose(file, path)
}

// readAndClose reads all of file, opened from path, and closes it.
func readAndClose(file *os.File, path string) ([]byte, error) {
	data, readErr := io.ReadAll(file)
	if readErr != nil {
		return nil, closeAfterFailure(file, failure("reading", path, readErr, file.Name()))
	}

	if closeErr := file.Close(); closeErr != nil {
		return nil, failure("closing", path, closeErr, file.Name())
	}

	return data, nil
}

// closeAfterFailure closes file once cause, a failure that named its path,
// has ended its use, and returns cause joined with the close failure, if any,
// which does not name the path again.
func closeAfterFailure(file *os.File, cause error) error {
	if closeErr := file.Close(); closeErr != nil {
		return JoinOnOneLine(cause, failure("closing", "", closeErr, file.Name()))
	}

	return cause
}

// Stat describes what path names, following a symlink.
func Stat(path string) (os.FileInfo, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	// #nosec G703 -- localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	info, err := os.Stat(abs)
	if err != nil {
		return nil, failure("stating", path, err, abs)
	}

	return info, nil
}

// Lstat describes path itself: a symlink is described, not followed.
func Lstat(path string) (os.FileInfo, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	// #nosec G703 -- localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, failure("lstating", path, err, abs)
	}

	return info, nil
}

// IsMissingName reports whether err carries the errno of a name that is not
// there: ENOENT, or ENOTDIR for a folder on the way to it that is a file,
// below which nothing can exist. Only the filesystem's own answer counts, so
// a failure without an errno (a path escaping a rooted handle, the bare
// fs.ErrNotExist sentinel) is never evidence of absence. It is one of the two
// answers the rooted capabilities need before they report a failure as
// os.ErrNotExist; the other is a rooted lstat that finds the name missing too.
func IsMissingName(err error) bool {
	errno, ok := errors.AsType[syscall.Errno](err)

	return ok && (errno == syscall.ENOENT || errno == syscall.ENOTDIR)
}

// UserTreeDirPerm is the mode the client asks for when it creates a folder in
// a user's tree: below the sync folder (a synced folder, a shared folder's
// shortcut folder and the parents a moved one needs; the sync tree applies
// it to every folder it creates and takes no mode from its callers), a
// folder `get` downloads into, a missing parent of a `get` target, and a
// missing parent of a file restored from the trash. The kernel applies the
// umask, as for any other program, so these folders get what the files in
// them get (0666 minus the umask). The sync folder the client creates and
// the client's own state are private and keep their own modes; an existing
// folder is never changed.
const UserTreeDirPerm os.FileMode = 0o777

// MkdirAll creates path and its missing parents, each asking for perm; the
// umask applies. A folder that exists keeps its mode.
func MkdirAll(path string, perm os.FileMode) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(abs, perm); err != nil {
		return failure("creating directory", path, err, abs)
	}

	return nil
}

// Chmod sets the mode of path to perm.
func Chmod(path string, perm os.FileMode) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.Chmod(abs, perm); err != nil {
		return failure("setting mode on", path, err, abs)
	}

	return nil
}

// Remove removes the file or the empty directory at path.
func Remove(path string) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.Remove(abs); err != nil {
		return failure("removing", path, err, abs)
	}

	return nil
}

// Rename moves src to dst and replaces a file already at dst. A move inside a
// sync folder that must refuse to replace one is synctree.Root.RenameNoReplace.
func Rename(src, dst string) error {
	srcPath, err := absolutePath(src)
	if err != nil {
		return err
	}
	dstPath, err := absolutePath(dst)
	if err != nil {
		return err
	}

	if err := os.Rename(srcPath, dstPath); err != nil {
		return failure("renaming", src+" to "+dst, err, srcPath, dstPath)
	}

	return nil
}

// ReadDir lists the entries of the directory at path, sorted by name.
func ReadDir(path string) ([]os.DirEntry, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, failure("reading directory", path, err, abs)
	}

	return entries, nil
}

// DirReader reads the entries of one open directory a batch at a time, so a
// walk holds a directory of any size a batch at a time instead of as one
// slice. It is not safe for concurrent use; Close releases it.
type DirReader struct {
	file *os.File
	path string
}

// OpenDir opens the directory at path for reading with Next, with
// openDirFlags: on a unix system a path that is not a directory, a FIFO
// among them, is refused at the open with ENOTDIR and the open never blocks.
func OpenDir(path string) (*DirReader, error) {
	file, err := OpenFile(path, openDirFlags, 0)
	if err != nil {
		return nil, err
	}

	return &DirReader{file: file, path: path}, nil
}

// Next returns up to n further entries of the directory, in the order the
// filesystem keeps them, not sorted. At the end of the directory it returns
// no entries and io.EOF; any other failure names the path. n must be at
// least one: os.File.ReadDir would read the whole directory for less, which
// is what a batch reader exists to avoid, so a smaller n is refused.
func (d *DirReader) Next(n int) ([]os.DirEntry, error) {
	if n < 1 {
		return nil, fmt.Errorf("reading directory %s: a batch of %d entries is below one", d.path, n)
	}

	entries, err := d.file.ReadDir(n)
	switch {
	case err == nil:
		return entries, nil
	case errors.Is(err, io.EOF):
		return entries, io.EOF
	default:
		return entries, failure("reading directory", d.path, err, d.file.Name())
	}
}

// Close releases the directory.
func (d *DirReader) Close() error {
	if err := d.file.Close(); err != nil {
		return failure("closing", d.path, err, d.file.Name())
	}

	return nil
}

// EvalSymlinks returns path as an absolute path with every symlink in it
// resolved.
func EvalSymlinks(path string) (string, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", failure("resolving symlinks for", path, err, abs)
	}

	return resolved, nil
}

// Chtimes sets the access and modification times of path.
func Chtimes(path string, atime, mtime time.Time) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.Chtimes(abs, atime, mtime); err != nil {
		return failure("setting times on", path, err, abs)
	}

	return nil
}

// SyncDir flushes a directory's entries to stable storage, so a rename into
// it survives a power cut. Without it the rename can be lost while state
// recorded elsewhere says the file is in place.
func SyncDir(path string) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	dir, err := os.Open(abs)
	if err != nil {
		return failure("opening directory", path, err, abs)
	}

	syncErr := dir.Sync()
	closeErr := dir.Close()

	if syncErr != nil {
		return failure("syncing directory", path, syncErr, abs)
	}

	if closeErr != nil {
		return failure("closing directory", path, closeErr, abs)
	}

	return nil
}

// CreateTemp creates a new file in dir, named from pattern as os.CreateTemp
// names it, and returns it open for reading and writing.
func CreateTemp(dir, pattern string) (*os.File, error) {
	abs, err := absolutePath(dir)
	if err != nil {
		return nil, err
	}

	file, err := os.CreateTemp(abs, pattern)
	if err != nil {
		return nil, failure("creating temp file in", dir, err, osErrorPath(err))
	}

	return file, nil
}

// MkdirTemp creates a new directory in dir, named from pattern as
// os.MkdirTemp names it, and returns its path.
func MkdirTemp(dir, pattern string) (string, error) {
	abs, err := absolutePath(dir)
	if err != nil {
		return "", err
	}

	tempDir, err := os.MkdirTemp(abs, pattern)
	if err != nil {
		return "", failure("creating temp directory in", dir, err, osErrorPath(err))
	}

	return tempDir, nil
}

// RemoveAll removes path and everything below it. A path that does not
// exist is not an error.
func RemoveAll(path string) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(abs); err != nil {
		return failure("removing tree", path, err, abs)
	}

	return nil
}

// WriteDisposableFile writes data directly to a disposable artifact path. It is
// intentionally non-atomic; durable config, token, state, or user-content writes
// must use an owner-specific atomic writer instead.
func WriteDisposableFile(path string, data []byte, perm os.FileMode) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.WriteFile(abs, data, perm); err != nil {
		return failure("writing", path, err, abs)
	}

	return nil
}

// IsDirSyncUnsupported reports the errnos a filesystem that cannot fsync a
// directory answers with (FUSE, SMB and some network mounts). A caller that
// has already renamed a file into place treats these as best effort: the
// bytes are on disk and the rename is visible, only the durability of the
// directory entry could not be established. Any other failure is reported.
func IsDirSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

// IsLinkUnsupported reports the errnos of a hard link the filesystem cannot
// make: no hard links at all (EPERM, ENOTSUP, EOPNOTSUPP, and ENOSYS, which a
// FUSE filesystem without a link operation can answer), a folder (EPERM), or
// a file with too many links already (EMLINK). It is the one answer to
// whether an entry can be linked, for the no-replace rename that falls back
// from a link and for the tests that need one.
func IsLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EMLINK)
}

// JoinOnOneLine returns first and second as one error, either alone when the
// other is nil, on one line where errors.Join would print two: the console
// prints a failure as one line. errors.Is finds each.
func JoinOnOneLine(first, second error) error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return fmt.Errorf("%w; %w", first, second)
	}
}

// AtomicWrite writes data to path via a temp file created in the target
// directory, then fsyncs, closes, renames it into place and fsyncs the
// directory (R-6.5.4): the rename is a directory update, and without the
// second sync a crash can lose it after every byte reached the disk. A
// directory sync the filesystem does not support is tolerated; any other
// failure is reported, because success would claim a durability the
// filesystem refused.
func AtomicWrite(path string, data []byte, filePerm, dirPerm os.FileMode, pattern string) error {
	return atomicWrite(path, data, filePerm, dirPerm, pattern, SyncDir)
}

// atomicWrite is AtomicWrite with the directory sync injectable: an fsync has
// no observable effect, so tests prove it happened and that its failure is
// reported through the seam.
func atomicWrite(
	path string, data []byte, filePerm, dirPerm os.FileMode, pattern string, syncDir func(dir string) error,
) (err error) {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	targetDir := filepath.Dir(abs)
	if mkErr := os.MkdirAll(targetDir, dirPerm); mkErr != nil {
		return failure("creating target directory", targetDir, mkErr, targetDir)
	}

	temp, err := os.CreateTemp(targetDir, pattern)
	if err != nil {
		return failure("creating temp file in", targetDir, err, osErrorPath(err))
	}

	tempPath := temp.Name()
	defer func() {
		if err == nil {
			return
		}
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = JoinOnOneLine(err, failure("removing temp file", tempPath, removeErr, tempPath))
		}
	}()

	if chmodErr := temp.Chmod(filePerm); chmodErr != nil {
		return closeWithContext(temp, "setting temp file permissions", chmodErr)
	}

	if _, writeErr := temp.Write(data); writeErr != nil {
		return closeWithContext(temp, "writing temp file", writeErr)
	}

	if syncErr := temp.Sync(); syncErr != nil {
		return closeWithContext(temp, "syncing temp file", syncErr)
	}

	if closeErr := temp.Close(); closeErr != nil {
		return fmt.Errorf("closing temp file: %w", closeErr)
	}

	// #nosec G703 -- tempPath is created in targetDir and abs is normalized by absolutePath.
	if renameErr := os.Rename(tempPath, abs); renameErr != nil {
		return failure("renaming temp file into", path, renameErr, tempPath, abs)
	}

	// The rename has happened, so the new contents are visible; this makes
	// that visibility survive a crash. The temp file is gone, so the deferred
	// cleanup has nothing to remove, and rewriting the same bytes is safe to
	// retry.
	if syncErr := syncDir(targetDir); syncErr != nil && !IsDirSyncUnsupported(syncErr) {
		return fmt.Errorf("making %s durable: %w", path, syncErr)
	}

	return nil
}

// closeWithContext closes temp after action failed with cause and returns
// that failure, joined with the close failure, if any.
func closeWithContext(temp *os.File, action string, cause error) error {
	closeErr := temp.Close()
	baseErr := fmt.Errorf("%s: %w", action, cause)
	if closeErr != nil {
		return JoinOnOneLine(baseErr, fmt.Errorf("closing temp file: %w", closeErr))
	}

	return baseErr
}
