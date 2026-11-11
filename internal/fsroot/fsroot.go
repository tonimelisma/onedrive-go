// Package fsroot provides root-bound capabilities for managed state files.
// Callers establish a trusted directory once, then operate on relative names
// within that root so atomic writes and path validation stay explicit. Every
// read and every mutation beneath the root resolves its name through an open
// os.Root descriptor, so a name whose components lead outside the root,
// through ".." or a symlink that points elsewhere, fails instead of following
// the escape.
package fsroot

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// tempNameAttempts bounds how many fresh names a temp-file creation tries.
// The suffix is random, so a taken name means another writer, or a temp file
// a crash left behind, holds exactly that name; a few retries cover it, and
// the bound stops a misbehaving suffix source from looping forever.
const tempNameAttempts = 100

// tempFilePerm is the mode a temp file is created with. It stays private to
// the owner until AtomicWrite applies the caller's mode just before writing.
const tempFilePerm os.FileMode = 0o600

// rootHandle is the contained filesystem surface every operation beneath the
// root runs through. The mutating methods are part of it because they are
// the ones that redirect or destroy data when containment is skipped: a
// lexically checked name joined onto the root and handed to the os package
// follows a symlinked directory out of the root. Tests substitute a fake
// handle to inject failures.
type rootHandle interface {
	Open(name string) (*os.File, error)
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Stat(name string) (os.FileInfo, error)
	Lstat(name string) (os.FileInfo, error)
	MkdirAll(name string, perm os.FileMode) error
	Remove(name string) error
	Rename(oldname, newname string) error
	FS() fs.FS
	Close() error
}

type osRootHandle struct {
	root *os.Root
}

func (h *osRootHandle) Open(name string) (*os.File, error) {
	//nolint:wrapcheck // Root methods add the managed-root boundary context.
	return h.root.Open(name)
}

func (h *osRootHandle) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	//nolint:wrapcheck // Root methods add the managed-root boundary context.
	return h.root.OpenFile(name, flag, perm)
}

func (h *osRootHandle) Stat(name string) (os.FileInfo, error) {
	//nolint:wrapcheck // Root methods add the managed-root boundary context.
	return h.root.Stat(name)
}

func (h *osRootHandle) Lstat(name string) (os.FileInfo, error) {
	//nolint:wrapcheck // Root methods add the managed-root boundary context.
	return h.root.Lstat(name)
}

func (h *osRootHandle) MkdirAll(name string, perm os.FileMode) error {
	//nolint:wrapcheck // Root methods add the managed-root boundary context.
	return h.root.MkdirAll(name, perm)
}

func (h *osRootHandle) Remove(name string) error {
	//nolint:wrapcheck // Root methods add the managed-root boundary context.
	return h.root.Remove(name)
}

func (h *osRootHandle) Rename(oldname, newname string) error {
	//nolint:wrapcheck // Root methods add the managed-root boundary context.
	return h.root.Rename(oldname, newname)
}

func (h *osRootHandle) FS() fs.FS {
	return h.root.FS()
}

func (h *osRootHandle) Close() error {
	//nolint:wrapcheck // caller owns the close-site context.
	return h.root.Close()
}

// tempFile is the part of a temp file AtomicWrite uses, so each step's
// failure can be tested without a filesystem that fails on demand.
type tempFile interface {
	Chmod(perm os.FileMode) error
	Write(p []byte) (n int, err error)
	Sync() error
	Close() error
}

// rootOps is the injection seam for failure testing. Everything beneath the
// root goes through the handle openRoot returns, so a test substitutes a fake
// handle rather than a function that takes an absolute path: an absolute path
// carries no containment. tempSuffix supplies the random part of a temp-file
// name, a seam so a test can make names collide.
type rootOps struct {
	openRoot   func(dir string) (rootHandle, error)
	tempSuffix func() string
}

func defaultRootOps() rootOps {
	return rootOps{
		openRoot: func(dir string) (rootHandle, error) {
			root, err := os.OpenRoot(dir)
			if err != nil {
				//nolint:wrapcheck // callers add the concrete managed-root path context.
				return nil, err
			}

			return &osRootHandle{root: root}, nil
		},
		tempSuffix: rand.Text,
	}
}

// Root is a capability for managed files rooted under one trusted directory.
// Callers establish trust once by constructing the root, then operate on
// relative names within it. This keeps managed-state I/O explicit and avoids
// repeating ad hoc path handling at every call site.
type Root struct {
	dir string
	ops rootOps
}

// Open constructs a managed root for dir. The directory does not need to
// exist yet; callers may create it later via MkdirAll or AtomicWrite.
func Open(dir string) (*Root, error) {
	if dir == "" {
		return nil, fmt.Errorf("root directory is empty")
	}

	return &Root{
		dir: filepath.Clean(dir),
		ops: defaultRootOps(),
	}, nil
}

// OpenPath splits a managed file path into its parent root capability plus the
// relative name within that root.
func OpenPath(path string) (*Root, string, error) {
	if path == "" {
		return nil, "", fmt.Errorf("path is empty")
	}
	if path != string(filepath.Separator) && os.IsPathSeparator(path[len(path)-1]) {
		return nil, "", fmt.Errorf("path %q does not name a file", path)
	}

	root, err := Open(filepath.Dir(path))
	if err != nil {
		return nil, "", err
	}

	name, err := cleanRelative(filepath.Base(path))
	if err != nil {
		return nil, "", err
	}

	return root, name, nil
}

func cleanRelative(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("relative path is empty")
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("relative path %q must not be absolute", name)
	}

	clean := filepath.Clean(name)
	if clean == "." {
		return "", fmt.Errorf("relative path %q does not name a file", name)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("relative path %q escapes root", name)
	}

	return clean, nil
}

// cleanDirName is cleanRelative for a directory name, where "" and "." name
// the root itself.
func cleanDirName(dirName string) (string, error) {
	if dirName == "" || filepath.Clean(dirName) == "." {
		return ".", nil
	}

	return cleanRelative(dirName)
}

// withRoot opens a handle on the managed root, runs op against it, and closes
// it. Every operation beneath the root goes through a handle opened here, so
// none joins a name onto the root and hands the result to the os package.
func (r *Root) withRoot(op func(root rootHandle) error) error {
	root, err := r.ops.openRoot(r.dir)
	if err != nil {
		return fmt.Errorf("opening root %s: %w", r.dir, err)
	}

	opErr := op(root)
	closeErr := root.Close()
	if opErr != nil {
		if closeErr != nil {
			return errors.Join(opErr, fmt.Errorf("closing root %s: %w", r.dir, closeErr))
		}

		return opErr
	}

	if closeErr != nil {
		return fmt.Errorf("closing root %s: %w", r.dir, closeErr)
	}

	return nil
}

func (r *Root) openWithRoot(
	name string,
	opener func(root rootHandle, clean string) (*os.File, error),
) (*os.File, error) {
	clean, err := cleanRelative(name)
	if err != nil {
		return nil, err
	}

	var file *os.File
	err = r.withRoot(func(root rootHandle) error {
		var openErr error
		file, openErr = opener(root, clean)
		if openErr != nil {
			return fmt.Errorf("opening %s: %w", clean, normalizeNotExist(root, clean, openErr))
		}

		return nil
	})
	if err != nil {
		if file != nil {
			return nil, errors.Join(err, file.Close())
		}

		return nil, err
	}

	return file, nil
}

func (r *Root) Open(name string) (*os.File, error) {
	return r.openWithRoot(name, func(root rootHandle, clean string) (*os.File, error) {
		return root.Open(clean)
	})
}

func (r *Root) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return r.openWithRoot(name, func(root rootHandle, clean string) (*os.File, error) {
		return root.OpenFile(clean, flag, perm)
	})
}

func (r *Root) ReadFile(name string) ([]byte, error) {
	file, err := r.Open(name)
	if err != nil {
		return nil, err
	}

	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}

		return nil, fmt.Errorf("reading %s: %w", name, readErr)
	}

	if closeErr != nil {
		return nil, fmt.Errorf("closing %s: %w", name, closeErr)
	}

	return data, nil
}

func (r *Root) Stat(name string) (os.FileInfo, error) {
	clean, err := cleanRelative(name)
	if err != nil {
		return nil, err
	}

	var info os.FileInfo
	err = r.withRoot(func(root rootHandle) error {
		var statErr error
		info, statErr = root.Stat(clean)
		if statErr != nil {
			return fmt.Errorf("stating %s: %w", clean, normalizeNotExist(root, clean, statErr))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return info, nil
}

func (r *Root) ReadDir(dirName string) ([]os.DirEntry, error) {
	cleanDir, err := cleanDirName(dirName)
	if err != nil {
		return nil, err
	}

	var entries []os.DirEntry
	err = r.withRoot(func(root rootHandle) error {
		var readErr error
		entries, readErr = fs.ReadDir(root.FS(), cleanDir)
		if readErr != nil {
			return fmt.Errorf("reading directory %s: %w", cleanDir, normalizeNotExist(root, cleanDir, readErr))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return entries, nil
}

// MkdirAll creates the root directory itself. The root is the anchor every
// handle is opened on, so it is the one directory created by path; every
// directory beneath it is created through a handle.
func (r *Root) MkdirAll(perm os.FileMode) error {
	if err := os.MkdirAll(r.dir, perm); err != nil {
		return fmt.Errorf("creating root directory %s: %w", r.dir, err)
	}

	return nil
}

func (r *Root) Remove(name string) error {
	clean, err := cleanRelative(name)
	if err != nil {
		return err
	}

	return r.withRoot(func(root rootHandle) error {
		if removeErr := root.Remove(clean); removeErr != nil {
			return fmt.Errorf("removing %s: %w", name, removeErr)
		}

		return nil
	})
}

func (r *Root) Rename(src, dst string) error {
	cleanSrc, err := cleanRelative(src)
	if err != nil {
		return err
	}
	cleanDst, err := cleanRelative(dst)
	if err != nil {
		return err
	}

	return r.withRoot(func(root rootHandle) error {
		if renameErr := root.Rename(cleanSrc, cleanDst); renameErr != nil {
			return fmt.Errorf("renaming %s to %s: %w", src, dst, renameErr)
		}

		return nil
	})
}

// CreateTemp creates a new file in dirName beneath the root, named after
// pattern with its last "*" replaced by a random string (as os.CreateTemp
// names it), opened for reading and writing with mode 0600. It returns the
// file and its name relative to the root; the caller closes and removes it.
func (r *Root) CreateTemp(dirName, pattern string) (*os.File, string, error) {
	cleanDir, err := cleanDirName(dirName)
	if err != nil {
		return nil, "", err
	}

	var (
		temp     *os.File
		tempName string
	)
	err = r.withRoot(func(root rootHandle) error {
		var createErr error
		temp, tempName, createErr = r.createTempIn(root, cleanDir, pattern)

		return createErr
	})
	if err != nil {
		if temp != nil {
			return nil, "", errors.Join(err, temp.Close(), r.Remove(tempName))
		}

		return nil, "", err
	}

	return temp, tempName, nil
}

// createTempIn creates a temp file in cleanDir through root. The name is
// claimed with O_EXCL, so an existing entry of that name, a symlink included,
// is never opened or replaced; a taken name is retried with a fresh suffix.
func (r *Root) createTempIn(root rootHandle, cleanDir, pattern string) (*os.File, string, error) {
	tempDir := filepath.Join(r.dir, cleanDir)

	prefix, suffix, err := splitTempPattern(pattern)
	if err != nil {
		return nil, "", fmt.Errorf("creating temp file in %s: %w", tempDir, err)
	}

	var lastErr error
	for range tempNameAttempts {
		name := prefix + r.ops.tempSuffix() + suffix
		if cleanDir != "." {
			name = filepath.Join(cleanDir, name)
		}

		temp, openErr := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, tempFilePerm)
		if openErr == nil {
			return temp, name, nil
		}
		if !errors.Is(openErr, os.ErrExist) {
			return nil, "", fmt.Errorf("creating temp file in %s: %w", tempDir, openErr)
		}

		lastErr = openErr
	}

	return nil, "", fmt.Errorf("creating temp file in %s: %d names taken: %w", tempDir, tempNameAttempts, lastErr)
}

// splitTempPattern splits pattern at its last "*" into the parts before and
// after the random suffix; a pattern without one gets the suffix at its end.
// A separator is refused because the pattern names a file, not a path.
func splitTempPattern(pattern string) (string, string, error) {
	if strings.ContainsRune(pattern, filepath.Separator) {
		return "", "", fmt.Errorf("temp file pattern %q contains a path separator", pattern)
	}

	if star := strings.LastIndexByte(pattern, '*'); star >= 0 {
		return pattern[:star], pattern[star+1:], nil
	}

	return pattern, "", nil
}

// AtomicWrite replaces name beneath the root with data: it writes a temp file
// in the target's directory, syncs it, renames it over the target and syncs
// the directory. The root directory and any missing directories on the way
// to the target are created with dirPerm.
func (r *Root) AtomicWrite(
	name string,
	data []byte,
	filePerm os.FileMode,
	dirPerm os.FileMode,
	pattern string,
) error {
	cleanName, err := cleanRelative(name)
	if err != nil {
		return err
	}

	if mkErr := r.MkdirAll(dirPerm); mkErr != nil {
		return mkErr
	}

	return r.withRoot(func(root rootHandle) error {
		return r.atomicWriteIn(root, cleanName, data, filePerm, dirPerm, pattern)
	})
}

func (r *Root) atomicWriteIn(
	root rootHandle,
	cleanName string,
	data []byte,
	filePerm os.FileMode,
	dirPerm os.FileMode,
	pattern string,
) (err error) {
	targetDir := filepath.Dir(cleanName)
	if targetDir != "." {
		if mkErr := root.MkdirAll(targetDir, dirPerm); mkErr != nil {
			return fmt.Errorf("creating target directory %s: %w", filepath.Join(r.dir, targetDir), mkErr)
		}
	}

	temp, tempName, err := r.createTempIn(root, targetDir, pattern)
	if err != nil {
		return err
	}

	defer func() { err = cleanupTempOnError(root, tempName, err) }()

	if writeErr := writeTempFile(temp, data, filePerm); writeErr != nil {
		return writeErr
	}

	if renameErr := root.Rename(tempName, cleanName); renameErr != nil {
		return fmt.Errorf("renaming %s to %s: %w", tempName, cleanName, renameErr)
	}

	// The rename has already happened, so the new contents are visible. This
	// only makes that visibility survive a crash, and a failure here means the
	// write is not yet durable, which the caller has to be told. Rewriting the
	// same bytes is harmless, so reporting it is safe to retry.
	return r.syncDirectory(root, targetDir)
}

// syncDirectory flushes a directory's own entries to stable storage.
//
// Syncing the temp file makes its contents durable, but the rename that gives
// those contents their final name is a directory update, and that update is
// not durable until the directory itself is synced. Without this, a crash
// right after AtomicWrite can leave the target missing or still holding its
// previous contents even though the new bytes reached the disk -- the one
// outcome an atomic write exists to rule out. Every failure is reported,
// including a filesystem that cannot sync a directory at all.
func (r *Root) syncDirectory(root rootHandle, cleanDir string) error {
	dirPath := filepath.Join(r.dir, cleanDir)

	dir, err := root.Open(cleanDir)
	if err != nil {
		return fmt.Errorf("opening directory %s to sync: %w", dirPath, err)
	}

	syncErr := dir.Sync()
	closeErr := dir.Close()

	if syncErr != nil {
		return fmt.Errorf("syncing directory %s: %w", dirPath, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing directory %s after sync: %w", dirPath, closeErr)
	}

	return nil
}

// cleanupTempOnError removes the temp file once a write has failed. A temp
// file already gone (renamed into place before a later step failed) is not a
// second failure.
func cleanupTempOnError(root rootHandle, tempName string, currentErr error) error {
	if currentErr == nil {
		return nil
	}

	removeErr := root.Remove(tempName)
	if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
		return currentErr
	}

	return errors.Join(currentErr, fmt.Errorf("removing temp file %s: %w", tempName, removeErr))
}

func writeTempFile(temp tempFile, data []byte, filePerm os.FileMode) error {
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

	return nil
}

// normalizeNotExist reports a failed operation on name as os.ErrNotExist when
// name is absent. The check is a rooted lstat on the same handle, so for a
// name beneath a symlink that leaves the root it fails with the escape rather
// than answering about the location outside: a containment failure is never
// turned into "nothing is here", which callers read as permission to create.
func normalizeNotExist(root rootHandle, name string, original error) error {
	if _, statErr := root.Lstat(name); errors.Is(statErr, os.ErrNotExist) {
		return os.ErrNotExist
	}

	return original
}

func closeWithContext(temp tempFile, action string, cause error) error {
	closeErr := temp.Close()
	baseErr := fmt.Errorf("%s: %w", action, cause)
	if closeErr != nil {
		return errors.Join(baseErr, fmt.Errorf("closing temp file: %w", closeErr))
	}

	return baseErr
}
