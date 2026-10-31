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

func cleanPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}

	clean := filepath.Clean(path)
	if clean == "" {
		return "", fmt.Errorf("path is empty")
	}

	return clean, nil
}

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
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("stat %s: %w", path, err), closeErr)
	}
	if !info.Mode().IsRegular() {
		closeErr := file.Close()
		return nil, errors.Join(fmt.Errorf("opening %s: %w", path, ErrNotRegularFile), closeErr)
	}

	return file, nil
}

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

func ReadFile(path string) ([]byte, error) {
	file, err := Open(path)
	if err != nil {
		return nil, err
	}

	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}

		return nil, fmt.Errorf("reading %s: %w", path, readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing %s: %w", path, closeErr)
	}

	return data, nil
}

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

func Mkdir(path string, perm os.FileMode) error {
	abs, err := absolutePath(path)
	if err != nil {
		return err
	}

	if err := os.Mkdir(abs, perm); err != nil {
		return fmt.Errorf("creating directory %s: %w", path, err)
	}

	return nil
}

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

func closeWithContext(temp *os.File, action string, cause error) error {
	closeErr := temp.Close()
	baseErr := fmt.Errorf("%s: %w", action, cause)
	if closeErr != nil {
		return errors.Join(baseErr, fmt.Errorf("closing temp file: %w", closeErr))
	}

	return baseErr
}
