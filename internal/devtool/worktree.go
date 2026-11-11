package devtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type includeEntry struct {
	path    string
	symlink bool
}

const (
	worktreeDirPerm = 0o750

	worktreeIncludeFile = ".worktreeinclude"
)

type worktreeFS struct {
	readFile  func(string) ([]byte, error)
	stat      func(string) (os.FileInfo, error)
	lstat     func(string) (os.FileInfo, error)
	mkdirAll  func(string, os.FileMode) error
	remove    func(string) error
	removeAll func(string) error
	symlink   func(string, string) error
	writeFile func(string, []byte, os.FileMode) error
}

func defaultWorktreeFS() worktreeFS {
	return worktreeFS{
		readFile:  readFile,
		stat:      stat,
		lstat:     lstat,
		mkdirAll:  mkdirAll,
		remove:    remove,
		removeAll: removeAll,
		symlink:   symlink,
		writeFile: func(path string, data []byte, perm os.FileMode) error {
			return atomicWrite(path, data, perm, worktreeDirPerm, ".worktree-include-*.tmp")
		},
	}
}

// AddWorktree creates a worktree for a new branch cut from origin/main and
// applies .worktreeinclude to it.
//
// It fetches origin's main first: origin/main is only as new as the last
// fetch, and a worktree cut from a stale one pays for it with a rebase before
// its first push. A fetch that fails stops the command, because creating the
// worktree anyway would hide that its base may be stale.
func AddWorktree(ctx context.Context, runner commandRunner, repoRoot, path, branch string) error {
	if branch == "" {
		return fmt.Errorf("worktree add: branch is required")
	}
	if path == "" {
		return fmt.Errorf("worktree add: path is required")
	}

	if err := runner.Run(
		ctx,
		repoRoot,
		os.Environ(),
		os.Stdout,
		os.Stderr,
		"git",
		"fetch",
		"origin",
		mainBranchName,
	); err != nil {
		return fmt.Errorf("worktree add: fetch origin %s: %w", mainBranchName, err)
	}

	if err := runner.Run(
		ctx,
		repoRoot,
		os.Environ(),
		os.Stdout,
		os.Stderr,
		"git",
		"worktree",
		"add",
		"-b",
		branch,
		path,
		"origin/main",
	); err != nil {
		return fmt.Errorf("worktree add: %w", err)
	}

	if err := BootstrapWorktree(repoRoot, path); err != nil {
		cleanupErr := cleanupFailedWorktree(ctx, runner, repoRoot, path, branch)
		if cleanupErr != nil {
			return fmt.Errorf("worktree bootstrap: %w (cleanup: %s)", err, cleanupErr.Error())
		}

		return fmt.Errorf("worktree bootstrap: %w", err)
	}

	return nil
}

func cleanupFailedWorktree(ctx context.Context, runner commandRunner, repoRoot, path, branch string) error {
	if err := runner.Run(
		ctx,
		repoRoot,
		os.Environ(),
		os.Stdout,
		os.Stderr,
		"git",
		"worktree",
		"remove",
		"--force",
		path,
	); err != nil {
		return fmt.Errorf("remove failed worktree: %w", err)
	}

	if err := runner.Run(
		ctx,
		repoRoot,
		os.Environ(),
		os.Stdout,
		os.Stderr,
		"git",
		"branch",
		"-D",
		branch,
	); err != nil {
		return fmt.Errorf("delete failed worktree branch: %w", err)
	}

	return nil
}

// BootstrapWorktree applies .worktreeinclude to targetPath and tells the
// developer on stderr about any entry the root checkout does not have.
func BootstrapWorktree(repoRoot, targetPath string) error {
	skipped, err := bootstrapWorktree(defaultWorktreeFS(), repoRoot, targetPath)
	for _, path := range skipped {
		fmt.Fprintf(os.Stderr, "Skipped %s: not present in %s. Only live E2E and integration tests need it.\n", path, repoRoot)
	}

	return err
}

// bootstrapWorktree returns the include entries it skipped because the root
// checkout lacks them. Every entry is a local adjunct that only live tests
// use (credentials, .env), so a contributor without test credentials still
// gets a working worktree; the live suites then fail their own preflight.
func bootstrapWorktree(fs worktreeFS, repoRoot, targetPath string) ([]string, error) {
	entries, err := readIncludeEntries(fs, filepath.Join(repoRoot, worktreeIncludeFile))
	if err != nil {
		return nil, err
	}

	var skipped []string
	for _, entry := range entries {
		applied, err := applyIncludeEntry(fs, repoRoot, targetPath, entry)
		if err != nil {
			return skipped, err
		}
		if !applied {
			skipped = append(skipped, entry.path)
		}
	}

	return skipped, nil
}

func readIncludeEntries(fs worktreeFS, includePath string) ([]includeEntry, error) {
	data, err := fs.readFile(includePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", includePath, err)
	}

	lines := strings.Split(string(data), "\n")
	entries := make([]includeEntry, 0, len(lines))

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		entry := includeEntry{path: line}
		if strings.HasPrefix(line, "@") {
			entry.symlink = true
			entry.path = strings.TrimPrefix(line, "@")
		}

		if entry.path == "" {
			return nil, fmt.Errorf("parse %s:%d: empty entry", includePath, i+1)
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// applyIncludeEntry reports false, with no error, when the source does not
// exist in the root checkout.
func applyIncludeEntry(fs worktreeFS, repoRoot, targetPath string, entry includeEntry) (bool, error) {
	sourcePath := filepath.Join(repoRoot, entry.path)
	targetEntryPath := filepath.Join(targetPath, entry.path)

	info, err := fs.stat(sourcePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat include source %s: %w", sourcePath, err)
	}

	if mkdirErr := fs.mkdirAll(filepath.Dir(targetEntryPath), worktreeDirPerm); mkdirErr != nil {
		return false, fmt.Errorf("mkdir include target %s: %w", filepath.Dir(targetEntryPath), mkdirErr)
	}

	if removeErr := fs.removeAll(targetEntryPath); removeErr != nil && !os.IsNotExist(removeErr) {
		return false, fmt.Errorf("remove existing include target %s: %w", targetEntryPath, removeErr)
	}

	if entry.symlink {
		if symlinkErr := fs.symlink(sourcePath, targetEntryPath); symlinkErr != nil {
			return false, fmt.Errorf("symlink include %s -> %s: %w", sourcePath, targetEntryPath, symlinkErr)
		}

		return true, nil
	}

	if info.IsDir() {
		return false, fmt.Errorf("copy include %s: directories must use @ symlink entries", sourcePath)
	}

	data, err := fs.readFile(sourcePath)
	if err != nil {
		return false, fmt.Errorf("read include source %s: %w", sourcePath, err)
	}

	if err := fs.writeFile(targetEntryPath, data, info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("write include target %s: %w", targetEntryPath, err)
	}

	return true, nil
}

// removeBootstrapEntries undoes bootstrapWorktree in targetPath: it removes the
// symlink of every `@` entry and the copied file of every plain entry that
// repoRoot's .worktreeinclude names, and nothing else.
//
// git refuses to remove a worktree that holds an untracked file, and a
// bootstrap symlink is one, so cleanup removes these first. It removes only
// what bootstrap creates: a real directory where a symlink belongs, or
// anything but a regular file where a copy belongs, is somebody's data, and
// the call fails naming the path instead of deleting it. An entry that is not
// there was never applied and is skipped. Parent directories stay: they may
// hold other files, and an empty directory does not stop git.
//
// It refuses to run on the root checkout, where the entries are the originals
// every worktree links to or copies from.
func removeBootstrapEntries(fs worktreeFS, repoRoot, targetPath string) error {
	sameCheckout, err := sameDirectory(fs, repoRoot, targetPath)
	if err != nil {
		return err
	}
	if sameCheckout {
		return fmt.Errorf("remove bootstrap entries: %s is the root checkout, which owns the originals", targetPath)
	}

	entries, err := readIncludeEntries(fs, filepath.Join(repoRoot, worktreeIncludeFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if err := removeBootstrapEntry(fs, filepath.Join(targetPath, entry.path), entry.symlink); err != nil {
			return err
		}
	}

	return nil
}

func removeBootstrapEntry(fs worktreeFS, entryPath string, wantSymlink bool) error {
	info, err := fs.lstat(entryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect bootstrap entry: %w", err)
	}

	isSymlink := info.Mode()&os.ModeSymlink != 0
	switch {
	case wantSymlink && !isSymlink:
		return fmt.Errorf("remove bootstrap entry %s: not a symlink, so bootstrap did not create it", entryPath)
	case !wantSymlink && !info.Mode().IsRegular():
		return fmt.Errorf("remove bootstrap entry %s: not a regular file, so bootstrap did not create it", entryPath)
	}

	if err := fs.remove(entryPath); err != nil {
		return fmt.Errorf("remove bootstrap entry: %w", err)
	}

	return nil
}

// sameDirectory reports whether two paths name one directory, by any spelling:
// a trailing separator and a symlink alias included. A path that does not
// exist is no directory at all.
func sameDirectory(fs worktreeFS, left, right string) (bool, error) {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true, nil
	}

	leftInfo, err := fs.stat(left)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare checkouts: %w", err)
	}

	rightInfo, err := fs.stat(right)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare checkouts: %w", err)
	}

	return os.SameFile(leftInfo, rightInfo), nil
}
