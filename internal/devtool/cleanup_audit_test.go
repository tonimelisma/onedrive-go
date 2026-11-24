package devtool

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCleanupRepoRoot = "/repo"

// Validates: R-6.10.11
func TestRunCleanupAuditClassifiesRepoState(t *testing.T) {
	t.Parallel()

	repoRoot := testCleanupRepoRoot
	worktreePath := filepath.Join(repoRoot, "wt-clean")
	dirtyWorktreePath := filepath.Join(repoRoot, "wt-dirty")

	runner := &fakeRunner{
		outputs: map[string][]byte{
			"git worktree list --porcelain": []byte(
				"worktree " + testCleanupRepoRoot + "\nHEAD 1111111\nbranch refs/heads/main\n\n" +
					"worktree " + worktreePath + "\nHEAD 2222222\nbranch refs/heads/refactor/merged\n\n" +
					"worktree " + dirtyWorktreePath + "\nHEAD 3333333\nbranch refs/heads/refactor/dirty\n",
			),
			"git status --porcelain": []byte(""),
			"git for-each-ref --format=%(refname:short)|%(refname) refs/heads": []byte(stringsJoinLines(
				"main|refs/heads/main",
				"refactor/merged|refs/heads/refactor/merged",
				"refactor/dirty|refs/heads/refactor/dirty",
				"refactor/unmerged|refs/heads/refactor/unmerged",
			)),
			"git for-each-ref --format=%(refname:short)|%(refname) refs/remotes/origin": []byte(stringsJoinLines(
				"origin/HEAD|refs/remotes/origin/HEAD",
				"origin/main|refs/remotes/origin/main",
				"origin/merged|refs/remotes/origin/merged",
				"origin/topic|refs/remotes/origin/topic",
			)),
			"git rev-list --count refs/heads/main --not refs/remotes/origin/main":              []byte("0\n"),
			"git rev-list --count refs/heads/refactor/merged --not refs/remotes/origin/main":   []byte("0\n"),
			"git rev-list --count refs/heads/refactor/dirty --not refs/remotes/origin/main":    []byte("1\n"),
			"git rev-list --count refs/heads/refactor/unmerged --not refs/remotes/origin/main": []byte("2\n"),
			"git rev-list --count refs/remotes/origin/main --not refs/remotes/origin/main":     []byte("0\n"),
			"git rev-list --count refs/remotes/origin/merged --not refs/remotes/origin/main":   []byte("0\n"),
			"git rev-list --count refs/remotes/origin/topic --not refs/remotes/origin/main":    []byte("3\n"),
		},
		outputsByCWD: map[string]map[string][]byte{
			dirtyWorktreePath: {
				"git status --porcelain": []byte(" M README.md\n"),
			},
			worktreePath: {
				"git status --porcelain": []byte(""),
			},
			repoRoot: {
				"git status --porcelain": []byte(""),
			},
		},
	}

	stdout := &bytes.Buffer{}
	err := runCleanupAudit(context.Background(), runner, cleanupAuditOptions{
		RepoRoot: repoRoot,
		Stdout:   stdout,
		Stderr:   &bytes.Buffer{},
	})
	require.NoError(t, err)

	require.NotEmpty(t, runner.runCommands)
	assert.Equal(t, "git", runner.runCommands[0].name)
	assert.Equal(t, []string{"fetch", "--prune", "origin"}, runner.runCommands[0].args)

	output := stdout.String()
	assert.Contains(t, output, "cleanup audit: worktrees")
	assert.Contains(t, output, "keep_main: "+testCleanupRepoRoot+" (branch: main; root main worktree)")
	assert.Contains(t, output, "safe_remove: "+worktreePath+" (branch: refactor/merged; worktree head is reachable from main and clean)")
	assert.Contains(t, output, "keep_dirty: "+dirtyWorktreePath+" (branch: refactor/dirty; attached worktree has local modifications)")
	assert.Contains(t, output, "keep_attached: refactor/merged (branch is attached to a worktree)")
	assert.Contains(t, output, "keep_unmerged: refactor/unmerged (branch has commits not reachable from main)")
	assert.Contains(t, output, "safe_remove: origin/merged (remote branch tip is reachable from main)")
	assert.Contains(t, output, "keep_unmerged: origin/topic (remote branch has commits not reachable from main)")
}

// Validates: R-6.10.11
//
// The audit's classification against a real repository: a merged and an
// unmerged branch, pushed, and a dirty attached worktree.
func TestRunCleanupAuditClassifiesARealRepository(t *testing.T) {
	t.Parallel()

	remoteRoot := filepath.Join(t.TempDir(), "origin.git")
	cleanupTestGit(t, "", "init", "--bare", remoteRoot)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	cleanupTestGit(t, "", "clone", remoteRoot, repoRoot)
	cleanupTestGit(t, repoRoot, "checkout", "-b", "main")
	cleanupTestGit(t, repoRoot, "config", "user.name", "Test User")
	cleanupTestGit(t, repoRoot, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("main\n"), 0o600))
	cleanupTestGit(t, repoRoot, "add", "README.md")
	cleanupTestGit(t, repoRoot, "commit", "-m", "initial")
	cleanupTestGit(t, repoRoot, "push", "-u", "origin", "main")

	cleanupTestGit(t, repoRoot, "checkout", "-b", "refactor/merged")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "merged.txt"), []byte("merged\n"), 0o600))
	cleanupTestGit(t, repoRoot, "add", "merged.txt")
	cleanupTestGit(t, repoRoot, "commit", "-m", "merged branch")
	cleanupTestGit(t, repoRoot, "checkout", "main")
	cleanupTestGit(t, repoRoot, "merge", "--ff-only", "refactor/merged")
	cleanupTestGit(t, repoRoot, "push", "origin", "main")
	cleanupTestGit(t, repoRoot, "push", "-u", "origin", "refactor/merged")

	cleanupTestGit(t, repoRoot, "checkout", "-b", "refactor/topic")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "topic.txt"), []byte("topic\n"), 0o600))
	cleanupTestGit(t, repoRoot, "add", "topic.txt")
	cleanupTestGit(t, repoRoot, "commit", "-m", "topic branch")
	cleanupTestGit(t, repoRoot, "push", "-u", "origin", "refactor/topic")
	cleanupTestGit(t, repoRoot, "checkout", "main")

	worktreePath := filepath.Join(t.TempDir(), "wt")
	cleanupTestGit(t, repoRoot, "worktree", "add", "-b", "refactor/attached", worktreePath, "main")
	require.NoError(t, os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("dirty\n"), 0o600))
	canonicalRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	require.NoError(t, err)
	canonicalWorktreePath, err := filepath.EvalSymlinks(worktreePath)
	require.NoError(t, err)

	var out, errOut bytes.Buffer
	require.NoError(t, runCleanupAudit(t.Context(), ExecRunner{}, cleanupAuditOptions{
		RepoRoot: repoRoot,
		Stdout:   &out,
		Stderr:   &errOut,
	}), errOut.String())
	stdout := out.String()
	assert.Contains(t, stdout, "cleanup audit: worktrees")
	assert.Contains(t, stdout, "keep_main: "+canonicalRepoRoot+" (branch: main; root main worktree)")
	assert.Contains(t, stdout, "keep_dirty: "+canonicalWorktreePath+" (branch: refactor/attached; attached worktree has local modifications)")
	assert.Contains(t, stdout, "keep_dirty: refactor/attached (attached worktree has local modifications)")
	assert.Contains(t, stdout, "safe_remove: refactor/merged (branch tip is reachable from main)")
	assert.Contains(t, stdout, "keep_unmerged: refactor/topic (branch has commits not reachable from main)")
	assert.Contains(t, stdout, "safe_remove: origin/refactor/merged (remote branch tip is reachable from main)")
	assert.Contains(t, stdout, "keep_unmerged: origin/refactor/topic (remote branch has commits not reachable from main)")
}

// cleanupTestGit runs git in cwd for a test's throwaway repository.
func cleanupTestGit(t *testing.T, cwd string, args ...string) {
	t.Helper()

	//nolint:gosec // test drives git against temporary repositories with explicit args.
	cmd := exec.CommandContext(t.Context(), "git", args...)
	if cwd != "" {
		cmd.Dir = cwd
	}

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func stringsJoinLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}
