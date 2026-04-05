package devtool

import (
	"bytes"
	"context"
	"encoding/json"
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
			"git rev-list --count refs/heads/main --not refs/heads/main":              []byte("0\n"),
			"git rev-list --count refs/heads/refactor/merged --not refs/heads/main":   []byte("0\n"),
			"git rev-list --count refs/heads/refactor/dirty --not refs/heads/main":    []byte("1\n"),
			"git rev-list --count refs/heads/refactor/unmerged --not refs/heads/main": []byte("2\n"),
			"git rev-list --count refs/remotes/origin/main --not refs/heads/main":     []byte("0\n"),
			"git rev-list --count refs/remotes/origin/merged --not refs/heads/main":   []byte("0\n"),
			"git rev-list --count refs/remotes/origin/topic --not refs/heads/main":    []byte("3\n"),
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
	err := RunCleanupAudit(context.Background(), runner, CleanupAuditOptions{
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
func TestRunCleanupAuditJSONOutput(t *testing.T) {
	t.Parallel()

	repoRoot := testCleanupRepoRoot
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"git worktree list --porcelain":                                    []byte("worktree " + testCleanupRepoRoot + "\nHEAD 1111111\nbranch refs/heads/main\n"),
			"git status --porcelain":                                           []byte(""),
			"git for-each-ref --format=%(refname:short)|%(refname) refs/heads": []byte("main|refs/heads/main\n"),
			"git for-each-ref --format=%(refname:short)|%(refname) refs/remotes/origin": []byte(
				"origin/main|refs/remotes/origin/main\n",
			),
			"git rev-list --count refs/heads/main --not refs/heads/main":          []byte("0\n"),
			"git rev-list --count refs/remotes/origin/main --not refs/heads/main": []byte("0\n"),
		},
	}

	stdout := &bytes.Buffer{}
	err := RunCleanupAudit(context.Background(), runner, CleanupAuditOptions{
		RepoRoot: repoRoot,
		JSON:     true,
		Stdout:   stdout,
	})
	require.NoError(t, err)

	var report CleanupAuditReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Len(t, report.Worktrees, 1)
	assert.Equal(t, CleanupKeepMain, report.Worktrees[0].Classification)
	require.Len(t, report.LocalBranches, 1)
	assert.Equal(t, CleanupKeepMain, report.LocalBranches[0].Classification)
	require.Len(t, report.RemoteBranches, 1)
	assert.Equal(t, CleanupKeepMain, report.RemoteBranches[0].Classification)
}

func stringsJoinLines(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}
