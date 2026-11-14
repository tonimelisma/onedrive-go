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
		return "", fmt.Errorf("resolving path %s: %w", clean, err)
	}

	return abs, nil
}

// Open opens path for reading. Like every function in this package it
// refuses an empty path, resolves a relative one against the working
// directory and names the path as it was given in its error.
func Open(path string) (*os.File, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	file, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	return file, nil
}

// ErrNotRegularFile reports that a path names a FIFO, socket, device or other
// non-regular file, which sync never reads.
var ErrNotRegularFile = errors.New("not a regular file")

// OpenRegular opens path read-only and returns ErrNotRegularFile unless it is
// a regular file. The open never blocks: a plain open of a FIFO waits for a
// writer forever, which used to hang the whole sync engine (and its shutdown)
// on any FIFO in the sync tree. O_NONBLOCK has no effect on regular files.
func OpenRegular(path string) (*os.File, error) {
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}

	//nolint:gosec // localpath is the explicit arbitrary-path boundary after clean+Abs validation.
	file, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	info, err := file.Stat()
	if err != nil {
		return nil, closeAfterFailure(file, path, fmt.Errorf("stat %s: %w", path, err))
	}
	if !info.Mode().IsRegular() {
		return nil, closeAfterFailure(file, path, fmt.Errorf("opening %s: %w", path, ErrNotRegularFile))
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
		return nil, fmt.Errorf("opening %s: %w", path, err)
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
		return nil, closeAfterFailure(file, path, fmt.Errorf("reading %s: %w", path, readErr))
	}

	if closeErr := file.Close(); closeErr != nil {
		return nil, fmt.Errorf("closing %s: %w", path, closeErr)
	}

	return data, nil
}

// closeAfterFailure closes file, opened from path, once cause has ended its
// use, and returns cause joined with the close failure, if any, which names
// path like every other step does.
func closeAfterFailure(file *os.File, path string, cause error) error {
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(cause, fmt.Errorf("closing %s: %w", path, closeErr))
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
		return nil, fmt.Errorf("stating %s: %w", path, err)
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
		return nil, fmt.Errorf("lstating %s: %w", path, err)
	}

	return info, nil
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
		return fmt.Errorf("creating directory %s: %w", path, err)
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
		return fmt.Errorf("setting mode on %s: %w", path, err)
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
		return fmt.Errorf("removing %s: %w", path, err)
	}

	return nil
}

// Rename moves src to dst and replaces a file already at dst.
// RenameNoReplace is the move that refuses to replace one.
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
		return fmt.Errorf("renaming %s to %s: %w", src, dst, err)
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
		return nil, fmt.Errorf("reading directory %s: %w", path, err)
	}

	return entries, nil
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
		return "", fmt.Errorf("resolving symlinks for %s: %w", path, err)
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
		return fmt.Errorf("setting times on %s: %w", path, err)
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
		return fmt.Errorf("opening directory %s for sync: %w", path, err)
	}

	syncErr := dir.Sync()
	closeErr := dir.Close()

	if syncErr != nil {
		return fmt.Errorf("syncing directory %s: %w", path, syncErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing directory %s: %w", path, closeErr)
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
		return nil, fmt.Errorf("creating temp file in %s: %w", dir, err)
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
		return "", fmt.Errorf("creating temp directory in %s: %w", dir, err)
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
		return fmt.Errorf("removing tree %s: %w", path, err)
	}

	return nil
}

// Symlink creates dst as a symlink to src. Both are made absolute first, so
// the link does not depend on the directory it lies in.
func Symlink(src, dst string) error {
	srcPath, err := absolutePath(src)
	if err != nil {
		return err
	}
	dstPath, err := absolutePath(dst)
	if err != nil {
		return err
	}

	if err := os.Symlink(srcPath, dstPath); err != nil {
		return fmt.Errorf("symlinking %s to %s: %w", src, dst, err)
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
		return fmt.Errorf("writing %s: %w", path, err)
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
		return fmt.Errorf("creating target directory %s: %w", targetDir, mkErr)
	}

	temp, err := os.CreateTemp(targetDir, pattern)
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", targetDir, err)
	}

	tempPath := temp.Name()
	defer func() {
		if err == nil {
			return
		}
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing temp file %s: %w", tempPath, removeErr))
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
		return fmt.Errorf("renaming temp file into %s: %w", path, renameErr)
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
		return errors.Join(baseErr, fmt.Errorf("closing temp file: %w", closeErr))
	}

	return baseErr
}
