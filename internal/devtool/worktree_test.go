package devtool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.1
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

// Validates: R-6.2.1
func TestBootstrapWorktreeFailsWhenIncludeSourceMissing(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	targetRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, ".worktreeinclude"), []byte(".env\n"), 0o600))

	err := BootstrapWorktree(repoRoot, targetRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat include source")
}

// Validates: R-6.2.1
func TestAddWorktreeCleansUpWhenBootstrapFails(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	repoRoot := t.TempDir()

	err := AddWorktree(context.Background(), runner, repoRoot, filepath.Join(repoRoot, "wt"), "refactor/test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree bootstrap")
	require.Len(t, runner.runCommands, 3)
	assert.Equal(t, []string{"worktree", "add", "-b", "refactor/test", filepath.Join(repoRoot, "wt"), "origin/main"}, runner.runCommands[0].args)
	assert.Equal(t, []string{"worktree", "remove", "--force", filepath.Join(repoRoot, "wt")}, runner.runCommands[1].args)
	assert.Equal(t, []string{"branch", "-D", "refactor/test"}, runner.runCommands[2].args)
}
