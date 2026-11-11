package devtool

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.10.21
func TestBootstrapWorktreeCopiesFilesAndSymlinksEntries(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	targetRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("@.testdata\n.env\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".testdata"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".testdata", "config.toml"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".env"), []byte("TOKEN=1\n"), 0o600))

	require.NoError(t, BootstrapWorktree(repoRoot, targetRoot))

	//nolint:gosec // test reads a temp file created within the test root.
	envData, err := os.ReadFile(filepath.Join(targetRoot, ".env"))
	require.NoError(t, err)
	assert.Equal(t, "TOKEN=1\n", string(envData))

	linkTarget, err := os.Readlink(filepath.Join(targetRoot, ".testdata"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repoRoot, ".testdata"), linkTarget)
}

// Validates: R-6.10.21
func TestBootstrapWorktreeSkipsIncludeSourceMissingFromRoot(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	targetRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("@.testdata\n.env\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".env"), []byte("TOKEN=1\n"), 0o600))

	skipped, err := bootstrapWorktree(defaultWorktreeFS(), repoRoot, targetRoot)
	require.NoError(t, err)
	assert.Equal(t, []string{".testdata"}, skipped)

	_, statErr := os.Lstat(filepath.Join(targetRoot, ".testdata"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
	assert.FileExists(t, filepath.Join(targetRoot, ".env"))
}

// Validates: R-6.10.21
func TestBootstrapWorktreeFailsWhenIncludeSourceUnreadable(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	targetRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte(".env\n"), 0o600))

	fs := defaultWorktreeFS()
	fs.stat = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }

	_, err := bootstrapWorktree(fs, repoRoot, targetRoot)
	require.ErrorIs(t, err, os.ErrPermission)
	assert.Contains(t, err.Error(), "stat include source")
}

// Validates: R-6.10.21
func TestAddWorktreeCleansUpWhenBootstrapFails(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	repoRoot := t.TempDir()

	err := AddWorktree(context.Background(), runner, repoRoot, filepath.Join(repoRoot, "wt"), "refactor/test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree bootstrap")
	require.Len(t, runner.runCommands, 4)
	assert.Equal(t, []string{"fetch", "origin", "main"}, runner.runCommands[0].args)
	assert.Equal(t, []string{"worktree", "add", "-b", "refactor/test", filepath.Join(repoRoot, "wt"), "origin/main"}, runner.runCommands[1].args)
	assert.Equal(t, []string{"worktree", "remove", "--force", filepath.Join(repoRoot, "wt")}, runner.runCommands[2].args)
	assert.Equal(t, []string{"branch", "-D", "refactor/test"}, runner.runCommands[3].args)
}

// Validates: R-6.10.21
//
// A worktree is cut from origin/main, and origin/main is only as new as the
// last fetch. Without the fetch a new increment silently starts from a stale
// base and pays for it with a rebase before its first push.
func TestAddWorktreeFetchesOriginMainBeforeCreatingTheWorktree(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("# nothing to apply\n"), 0o600))
	worktreePath := filepath.Join(repoRoot, "wt")

	require.NoError(t, AddWorktree(context.Background(), runner, repoRoot, worktreePath, "refactor/test"))

	require.Len(t, runner.runCommands, 2)
	assert.Equal(t, "git", runner.runCommands[0].name)
	assert.Equal(t, []string{"fetch", "origin", "main"}, runner.runCommands[0].args)
	assert.Equal(t, repoRoot, runner.runCommands[0].cwd)
	assert.Equal(t, "git", runner.runCommands[1].name)
	assert.Equal(t, []string{"worktree", "add", "-b", "refactor/test", worktreePath, "origin/main"}, runner.runCommands[1].args)
}

// Validates: R-6.10.21
//
// A fetch that fails must stop the command: creating the worktree anyway
// would hide that its base may be stale.
func TestAddWorktreeStopsWhenTheFetchFails(t *testing.T) {
	t.Parallel()

	fetchErr := errors.New("could not resolve host")
	runner := &fakeRunner{runErrByKey: map[string]error{"git fetch origin main": fetchErr}}
	repoRoot := t.TempDir()

	err := AddWorktree(context.Background(), runner, repoRoot, filepath.Join(repoRoot, "wt"), "refactor/test")
	require.ErrorIs(t, err, fetchErr)
	assert.Contains(t, err.Error(), "fetch origin main")
	require.Len(t, runner.runCommands, 1, "no worktree is created from a base that could not be refreshed")
}

// bootstrappedWorktreeFixture builds a root checkout with one symlinked and
// one copied include entry, applies them to a fresh worktree directory and
// returns both paths.
func bootstrappedWorktreeFixture(t *testing.T) (repoRoot, worktreePath string) {
	t.Helper()

	repoRoot = t.TempDir()
	worktreePath = t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("@.testdata\n.env\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".testdata"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".testdata", "config.toml"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".env"), []byte("KEY=1\n"), 0o600))

	skipped, err := bootstrapWorktree(defaultWorktreeFS(), repoRoot, worktreePath)
	require.NoError(t, err)
	require.Empty(t, skipped)

	return repoRoot, worktreePath
}

// Validates: R-6.10.21
//
// Removing a worktree's bootstrap entries is the exact inverse of applying
// them: the symlink and the copied file go, the developer's own files and
// everything in the root checkout stay.
func TestRemoveBootstrapEntriesRemovesOnlyWhatBootstrapCreated(t *testing.T) {
	t.Parallel()

	repoRoot, worktreePath := bootstrappedWorktreeFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "notes.txt"), []byte("mine\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(worktreePath, "scratch"), 0o750))

	require.NoError(t, removeBootstrapEntries(defaultWorktreeFS(), repoRoot, worktreePath))

	_, linkErr := os.Lstat(filepath.Join(worktreePath, ".testdata"))
	require.ErrorIs(t, linkErr, os.ErrNotExist)
	_, envErr := os.Lstat(filepath.Join(worktreePath, ".env"))
	require.ErrorIs(t, envErr, os.ErrNotExist)

	assert.FileExists(t, filepath.Join(worktreePath, "notes.txt"))
	assert.DirExists(t, filepath.Join(worktreePath, "scratch"))
	assert.FileExists(t, filepath.Join(repoRoot, ".testdata", "config.toml"), "the symlink goes, never what it points at")
	assert.FileExists(t, filepath.Join(repoRoot, ".env"))
	assert.FileExists(t, filepath.Join(repoRoot, ".worktreeinclude"))
}

// Validates: R-6.10.21
//
// An entry the root checkout never had was skipped by bootstrap, so there is
// nothing to remove and no error to report.
func TestRemoveBootstrapEntriesIgnoresEntriesThatWereNeverApplied(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	worktreePath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("@.testdata\n.env\n"), 0o600))

	require.NoError(t, removeBootstrapEntries(defaultWorktreeFS(), repoRoot, worktreePath))
	require.NoError(t, removeBootstrapEntries(defaultWorktreeFS(), t.TempDir(), worktreePath), "no include file, nothing applied")
}

// Validates: R-6.10.21
//
// A real directory where bootstrap would have put a symlink is somebody's
// data (a copied credential bundle, for one). Cleanup refuses it instead of
// deleting it, and names the path.
func TestRemoveBootstrapEntriesRefusesARealDirectoryAtASymlinkEntry(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	worktreePath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("@.testdata\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(worktreePath, ".testdata"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, ".testdata", "config.toml"), []byte("x"), 0o600))

	err := removeBootstrapEntries(defaultWorktreeFS(), repoRoot, worktreePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(worktreePath, ".testdata"))
	assert.Contains(t, err.Error(), "not a symlink")
	assert.FileExists(t, filepath.Join(worktreePath, ".testdata", "config.toml"))
}

// Validates: R-6.10.21
func TestRemoveBootstrapEntriesRefusesADirectoryAtACopiedEntry(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	worktreePath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte(".env\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(worktreePath, ".env"), 0o750))

	err := removeBootstrapEntries(defaultWorktreeFS(), repoRoot, worktreePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
	assert.DirExists(t, filepath.Join(worktreePath, ".env"))
}

// Validates: R-6.10.21
//
// In the root checkout the include entries are the originals every worktree
// links to or copies from. Pointing cleanup at it must remove nothing.
func TestRemoveBootstrapEntriesRefusesTheRootCheckout(t *testing.T) {
	t.Parallel()

	repoRoot, _ := bootstrappedWorktreeFixture(t)
	alias := filepath.Join(t.TempDir(), "root-by-another-name")
	require.NoError(t, os.Symlink(repoRoot, alias))

	for _, target := range []string{repoRoot, repoRoot + string(filepath.Separator), alias} {
		err := removeBootstrapEntries(defaultWorktreeFS(), repoRoot, target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "root checkout")
	}
	assert.FileExists(t, filepath.Join(repoRoot, ".env"))
	assert.FileExists(t, filepath.Join(repoRoot, ".testdata", "config.toml"))
}

// Validates: R-6.10.16
//
// git refuses to remove a worktree that holds an untracked file, and the
// bootstrap symlink is one. Post-merge cleanup therefore removes the bootstrap
// entries first, and nothing else, before it asks git to remove the worktree.
func TestDODCleanupRemovesBootstrapEntriesBeforeTheWorktree(t *testing.T) {
	t.Parallel()

	repoRoot, worktreePath := bootstrappedWorktreeFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "notes.txt"), []byte("mine\n"), 0o600))
	runner := &fakeRunner{}

	require.NoError(t, removeDODWorktree(context.Background(), runner, repoRoot, worktreePath, io.Discard))

	_, linkErr := os.Lstat(filepath.Join(worktreePath, ".testdata"))
	require.ErrorIs(t, linkErr, os.ErrNotExist)
	_, envErr := os.Lstat(filepath.Join(worktreePath, ".env"))
	require.ErrorIs(t, envErr, os.ErrNotExist)
	assert.FileExists(t, filepath.Join(worktreePath, "notes.txt"), "only git decides about the developer's files")
	assert.FileExists(t, filepath.Join(repoRoot, ".testdata", "config.toml"))

	require.Len(t, runner.runCommands, 1)
	assert.Equal(t, "git", runner.runCommands[0].name)
	assert.Equal(t, []string{"worktree", "remove", worktreePath}, runner.runCommands[0].args)
	assert.Equal(t, repoRoot, runner.runCommands[0].cwd)
}

// Validates: R-6.10.16
func TestDODCleanupLeavesTheWorktreeWhenABootstrapEntryIsNotWhatBootstrapCreated(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	worktreePath := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte("@.testdata\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(worktreePath, ".testdata"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, ".testdata", "config.toml"), []byte("x"), 0o600))
	runner := &fakeRunner{}

	err := removeDODWorktree(context.Background(), runner, repoRoot, worktreePath, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap entries")
	assert.Empty(t, runner.runCommands, "git is not asked to remove a worktree that still holds data")
	assert.FileExists(t, filepath.Join(worktreePath, ".testdata", "config.toml"))
}
