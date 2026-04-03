package devtool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type includeEntry struct {
	path    string
	symlink bool
}

const worktreeDirPerm = 0o750

type worktreeFS struct {
	readFile  func(string) ([]byte, error)
	stat      func(string) (os.FileInfo, error)
	mkdirAll  func(string, os.FileMode) error
	removeAll func(string) error
	symlink   func(string, string) error
	writeFile func(string, []byte, os.FileMode) error
}

func defaultWorktreeFS() worktreeFS {
	return worktreeFS{
		readFile:  localpath.ReadFile,
		stat:      localpath.Stat,
		mkdirAll:  localpath.MkdirAll,
		removeAll: localpath.RemoveAll,
		symlink:   localpath.Symlink,
		writeFile: localpath.WriteFile,
	}
}

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

func BootstrapWorktree(repoRoot, targetPath string) error {
	return bootstrapWorktree(defaultWorktreeFS(), repoRoot, targetPath)
}

func bootstrapWorktree(fs worktreeFS, repoRoot, targetPath string) error {
	entries, err := readIncludeEntries(fs, filepath.Join(repoRoot, ".worktreeinclude"))
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if err := applyIncludeEntry(fs, repoRoot, targetPath, entry); err != nil {
			return err
		}
	}

	return nil
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

func applyIncludeEntry(fs worktreeFS, repoRoot, targetPath string, entry includeEntry) error {
	sourcePath := filepath.Join(repoRoot, entry.path)
	targetEntryPath := filepath.Join(targetPath, entry.path)

	info, err := fs.stat(sourcePath)
	if err != nil {
		return fmt.Errorf("stat include source %s: %w", sourcePath, err)
	}

	if mkdirErr := fs.mkdirAll(filepath.Dir(targetEntryPath), worktreeDirPerm); mkdirErr != nil {
		return fmt.Errorf("mkdir include target %s: %w", filepath.Dir(targetEntryPath), mkdirErr)
	}

	if removeErr := fs.removeAll(targetEntryPath); removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("remove existing include target %s: %w", targetEntryPath, removeErr)
	}

	if entry.symlink {
		if symlinkErr := fs.symlink(sourcePath, targetEntryPath); symlinkErr != nil {
			return fmt.Errorf("symlink include %s -> %s: %w", sourcePath, targetEntryPath, symlinkErr)
		}

		return nil
	}

	if info.IsDir() {
		return fmt.Errorf("copy include %s: directories must use @ symlink entries", sourcePath)
	}

	data, err := fs.readFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read include source %s: %w", sourcePath, err)
	}

	if err := fs.writeFile(targetEntryPath, data, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write include target %s: %w", targetEntryPath, err)
	}

	return nil
}
