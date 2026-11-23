package devtool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pruneTestRepo is a main checkout with a local bare origin, so the tests
// run git for real and reach no network.
type pruneTestRepo struct {
	t        *testing.T
	mainRoot string
}

func newPruneTestRepo(t *testing.T) *pruneTestRepo {
	t.Helper()

	origin := filepath.Join(t.TempDir(), "origin.git")
	pruneGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	mainRoot := filepath.Join(t.TempDir(), "repo")
	pruneGit(t, "", "clone", origin, mainRoot)
	pruneGit(t, mainRoot, "checkout", "-B", "main")
	pruneGit(t, mainRoot, "config", "user.name", "Test User")
	pruneGit(t, mainRoot, "config", "user.email", "test@example.com")
	repo := &pruneTestRepo{t: t, mainRoot: mainRoot}
	repo.commitFile(mainRoot, "README.md", "line one\nline two\n", "initial")
	pruneGit(t, mainRoot, "push", "-u", "origin", "main")

	return repo
}

// addBuilder adds a builder worktree on a new branch cut from origin/main.
func (r *pruneTestRepo) addBuilder(name string) string {
	r.t.Helper()

	path := filepath.Join(r.mainRoot, ".claude", "worktrees", name)
	pruneGit(r.t, r.mainRoot, "worktree", "add", "-b", "worktree-"+name, path, "origin/main")

	return path
}

func (r *pruneTestRepo) commitFile(dir, name, content, message string) {
	r.t.Helper()

	require.NoError(r.t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	pruneGit(r.t, dir, "add", name)
	pruneGit(r.t, dir, "commit", "-m", message)
}

// squashIntoMain lands the builder's change on origin/main as one new
// commit, the way a squash merge does, so no builder commit reaches main.
func (r *pruneTestRepo) squashIntoMain(name, content string) {
	r.t.Helper()

	pruneGit(r.t, r.mainRoot, "pull", "--ff-only", "origin", "main")
	r.commitFile(r.mainRoot, name, content, "squashed builder work")
	pruneGit(r.t, r.mainRoot, "push", "origin", "main")
}

func (r *pruneTestRepo) prune(dryRun bool, processRunning func(int, string) bool) (string, error) {
	r.t.Helper()

	var stdout bytes.Buffer
	err := PruneMergedWorktrees(r.t.Context(), ExecRunner{}, WorktreePruneOptions{
		RepoRoot:       r.mainRoot,
		DryRun:         dryRun,
		Stdout:         &stdout,
		ProcessRunning: processRunning,
	})

	return stdout.String(), err
}

func (r *pruneTestRepo) branchExists(branch string) bool {
	r.t.Helper()

	//nolint:gosec // test drives git against a temporary repository.
	cmd := exec.CommandContext(r.t.Context(), "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = r.mainRoot

	return cmd.Run() == nil
}

func pruneGit(t *testing.T, cwd string, args ...string) {
	t.Helper()

	//nolint:gosec // test drives git against temporary repositories with explicit args.
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = cwd
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func neverRunning(int, string) bool { return false }

// Validates: R-6.10.37
func TestPruneMergedWorktrees_RemovesSquashMergedAndAncestorBuilders(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)

	squashed := repo.addBuilder("agent-squashed")
	repo.commitFile(squashed, "feature.txt", "one\n", "first step")
	repo.commitFile(squashed, "feature.txt", "one\ntwo\n", "second step")
	repo.squashIntoMain("feature.txt", "one\ntwo\n")

	idle := repo.addBuilder("agent-idle")

	// Squash-merged, then main rewrote the same line: the branch's work was
	// all in main at the squash commit, and what main did with it since is
	// main's own history.
	rewritten := repo.addBuilder("agent-rewritten")
	repo.commitFile(rewritten, "README.md", "line one\nbuilder draft\n", "builder draft")
	repo.commitFile(rewritten, "README.md", "line one\nbuilder line\n", "builder edit")
	repo.squashIntoMain("README.md", "line one\nbuilder line\n")
	repo.squashIntoMain("README.md", "line one\nmain rewrote it\n")

	output, err := repo.prune(false, neverRunning)
	require.NoError(t, err, output)

	assert.NoDirExists(t, squashed)
	assert.NoDirExists(t, idle)
	assert.NoDirExists(t, rewritten)
	assert.False(t, repo.branchExists("worktree-agent-squashed"))
	assert.False(t, repo.branchExists("worktree-agent-idle"))
	assert.False(t, repo.branchExists("worktree-agent-rewritten"))
	assert.Contains(t, output, "removed "+rewritten+" (branch worktree-agent-rewritten): its changes were all in origin/main at ")
	assert.Contains(t, output, "removed "+squashed+" (branch worktree-agent-squashed): its changes are already in origin/main")
	assert.Contains(t, output, "removed "+idle+" (branch worktree-agent-idle): every commit is in origin/main")
	assert.Contains(t, output, "3 removed, 0 kept")
}

// Validates: R-6.10.37
func TestPruneMergedWorktrees_KeepsUnmergedDirtyAndDetachedBuilders(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)

	unmerged := repo.addBuilder("agent-unmerged")
	repo.commitFile(unmerged, "feature.txt", "work\n", "unmerged work")

	conflicting := repo.addBuilder("agent-conflicting")
	repo.commitFile(conflicting, "README.md", "line one\nbuilder line\n", "builder edit")
	repo.squashIntoMain("README.md", "line one\nmain line\n")

	dirty := repo.addBuilder("agent-dirty")
	require.NoError(t, os.WriteFile(filepath.Join(dirty, "notes.txt"), []byte("unsaved\n"), 0o600))

	detached := filepath.Join(repo.mainRoot, ".claude", "worktrees", "agent-detached")
	pruneGit(t, repo.mainRoot, "worktree", "add", "--detach", detached, "origin/main")

	outside := filepath.Join(t.TempDir(), "by-hand")
	pruneGit(t, repo.mainRoot, "worktree", "add", "-b", "by-hand", outside, "origin/main")

	output, err := repo.prune(false, neverRunning)
	require.NoError(t, err, output)

	for _, path := range []string{unmerged, conflicting, dirty, detached, outside} {
		assert.DirExists(t, path)
	}
	assert.True(t, repo.branchExists("worktree-agent-unmerged"))
	assert.True(t, repo.branchExists("by-hand"))
	assert.Contains(t, output, "kept "+unmerged+" (branch worktree-agent-unmerged): has changes origin/main does not contain")
	assert.Contains(t, output, "kept "+conflicting+" (branch worktree-agent-conflicting): has changes origin/main does not contain",
		"a branch whose change main never held stays, even though main changed the same line")
	assert.Contains(t, output, "kept "+dirty+" (branch worktree-agent-dirty): has uncommitted or untracked changes")
	assert.Contains(t, output, "kept "+detached+" (branch (detached)): detached HEAD")
	assert.NotContains(t, output, outside, "a worktree outside .claude/worktrees is not the command's to judge")
	assert.Contains(t, output, "0 removed, 4 kept")
}

// Validates: R-6.10.37
func TestPruneMergedWorktrees_DryRunRemovesNothing(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)
	idle := repo.addBuilder("agent-idle")

	output, err := repo.prune(true, neverRunning)
	require.NoError(t, err, output)

	assert.DirExists(t, idle)
	assert.True(t, repo.branchExists("worktree-agent-idle"))
	assert.Contains(t, output, "would remove "+idle+" (branch worktree-agent-idle)")
	assert.Contains(t, output, "1 would remove, 0 kept")
}

// Validates: R-6.10.37
//
// Claude Code locks a builder's worktree while the agent runs. The lock of a
// running agent, and a lock set by hand, mean "in use"; only the lock of an
// agent that is gone is lifted before the removal.
func TestPruneMergedWorktrees_HonorsLiveLocksAndLiftsDeadAgentLocks(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)
	live := repo.addBuilder("agent-live")
	pruneGit(t, repo.mainRoot, "worktree", "lock", "--reason", "claude agent agent-live (pid 41 start 900)", live)
	dead := repo.addBuilder("agent-dead")
	pruneGit(t, repo.mainRoot, "worktree", "lock", "--reason", "claude agent agent-dead (pid 42 start 901)", dead)
	byHand := repo.addBuilder("agent-by-hand")
	pruneGit(t, repo.mainRoot, "worktree", "lock", "--reason", "keep for bisecting", byHand)

	running := func(pid int, startTicks string) bool { return pid == 41 && startTicks == "900" }
	output, err := repo.prune(false, running)
	require.NoError(t, err, output)

	assert.DirExists(t, live)
	assert.DirExists(t, byHand)
	assert.NoDirExists(t, dead)
	assert.False(t, repo.branchExists("worktree-agent-dead"))
	assert.Contains(t, output, "kept "+live+" (branch worktree-agent-live): locked by a running agent (pid 41)")
	assert.Contains(t, output, `kept `+byHand+` (branch worktree-agent-by-hand): locked ("keep for bisecting")`)
}

// Validates: R-6.10.37
func TestPruneMergedWorktrees_KeepsTheWorktreeItRunsIn(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)
	builder := repo.addBuilder("agent-self")

	var stdout bytes.Buffer
	err := PruneMergedWorktrees(t.Context(), ExecRunner{}, WorktreePruneOptions{
		RepoRoot:       builder,
		Stdout:         &stdout,
		ProcessRunning: neverRunning,
	})
	require.NoError(t, err, stdout.String())

	assert.DirExists(t, builder)
	assert.Contains(t, stdout.String(), "kept "+builder+" (branch worktree-agent-self): this command runs in it")
}

// failingRemoveRunner runs git for real but fails the removal of one path.
type failingRemoveRunner struct {
	ExecRunner
	failPath string
}

var errInjectedRemove = errors.New("injected remove failure")

func (r failingRemoveRunner) Run(
	ctx context.Context, cwd string, env []string, stdout, stderr io.Writer, name string, args ...string,
) error {
	if name == nightlyTestGit && len(args) == 3 && args[0] == "worktree" && args[1] == "remove" && args[2] == r.failPath {
		return errInjectedRemove
	}

	return r.ExecRunner.Run(ctx, cwd, env, stdout, stderr, name, args...)
}

// Validates: R-6.10.37
func TestPruneMergedWorktrees_ReportsAFailedRemovalAndGoesOn(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)
	stuck := repo.addBuilder("agent-a-stuck")
	other := repo.addBuilder("agent-b-other")

	var stdout bytes.Buffer
	err := PruneMergedWorktrees(t.Context(), failingRemoveRunner{failPath: stuck}, WorktreePruneOptions{
		RepoRoot:       repo.mainRoot,
		Stdout:         &stdout,
		ProcessRunning: neverRunning,
	})
	require.ErrorIs(t, err, errInjectedRemove)

	assert.DirExists(t, stuck)
	assert.True(t, repo.branchExists("worktree-agent-a-stuck"), "a branch whose worktree stayed is not deleted")
	assert.NoDirExists(t, other)
	assert.Contains(t, stdout.String(), "kept "+stuck)
	assert.Contains(t, stdout.String(), "1 removed, 1 kept")
}

// Validates: R-6.10.37
//
// A shallow clone shows a builder branch cut before its oldest fetched commit
// as sharing no history with origin/main; with no merge base neither check
// means anything, so the worktree stays, with a reason that says what to do.
func TestPruneMergedWorktrees_KeepsABranchWithNoCommonHistory(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)
	orphan := repo.addBuilder("agent-orphan")
	pruneGit(t, orphan, "checkout", "--orphan", "worktree-agent-orphan-history")
	pruneGit(t, orphan, "commit", "-m", "unrelated root")

	output, err := repo.prune(false, neverRunning)
	require.NoError(t, err, output)

	assert.DirExists(t, orphan)
	assert.Contains(t, output, "kept "+orphan+" (branch worktree-agent-orphan-history): shares no history with origin/main")
	assert.Contains(t, output, "git fetch --unshallow origin")
}

// failingMergeBaseRunner runs git for real but fails the merge-base lookup
// of one branch the way a broken repository would, not with "no base".
type failingMergeBaseRunner struct {
	ExecRunner
	failBranch string
}

var errInjectedMergeBase = errors.New("injected merge-base failure")

func (r failingMergeBaseRunner) Output(ctx context.Context, cwd string, env []string, name string, args ...string) ([]byte, error) {
	if name == nightlyTestGit && len(args) == 3 && args[0] == "merge-base" && args[2] == r.failBranch {
		return nil, errInjectedMergeBase
	}

	return r.ExecRunner.Output(ctx, cwd, env, name, args...)
}

// Validates: R-6.10.37
func TestPruneMergedWorktrees_KeepsAWorktreeItCannotJudgeAndGoesOn(t *testing.T) {
	t.Parallel()

	repo := newPruneTestRepo(t)
	broken := repo.addBuilder("agent-a-broken")
	other := repo.addBuilder("agent-b-other")

	var stdout bytes.Buffer
	err := PruneMergedWorktrees(t.Context(), failingMergeBaseRunner{failBranch: "worktree-agent-a-broken"}, WorktreePruneOptions{
		RepoRoot:       repo.mainRoot,
		Stdout:         &stdout,
		ProcessRunning: neverRunning,
	})
	require.ErrorIs(t, err, errInjectedMergeBase)

	assert.DirExists(t, broken)
	assert.NoDirExists(t, other)
	assert.Contains(t, stdout.String(), "kept "+broken+" (branch worktree-agent-a-broken): judge "+broken)
	assert.Contains(t, stdout.String(), "1 removed, 1 kept")
}

// Validates: R-6.10.37
func TestPruneMergedWorktrees_StopsWhenTheFetchFails(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{runErr: errors.New("network down")}
	err := PruneMergedWorktrees(t.Context(), runner, WorktreePruneOptions{RepoRoot: t.TempDir(), Stdout: io.Discard})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch origin main")
	assert.Empty(t, runner.outputCommands, "nothing is judged against a stale origin/main")
}

func TestParseAgentLock(t *testing.T) {
	t.Parallel()

	pid, start, ok := parseAgentLock("claude agent agent-x (pid 102 start 362)")
	require.True(t, ok)
	assert.Equal(t, 102, pid)
	assert.Equal(t, "362", start)

	for _, reason := range []string{"", "keep", "claude agent x", "claude agent x (pid abc start 1)", "claude agent x (pid 1)"} {
		_, _, ok := parseAgentLock(reason)
		assert.False(t, ok, reason)
	}
}

func TestProcProcessRunningMatchesPidAndStart(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skip("no /proc on this platform: every agent lock counts as held")
	}
	start := procStatStartTicks(string(data))
	require.NotEmpty(t, start)

	assert.True(t, procProcessRunning(os.Getpid(), start))
	assert.False(t, procProcessRunning(os.Getpid(), start+"0"), "a reused pid does not hold a dead agent's lock")
}

func TestProcStatStartTicks(t *testing.T) {
	t.Parallel()

	fields := make([]string, 0, procStatStartField)
	for i := 3; i <= procStatStartField; i++ {
		fields = append(fields, "f"+strings.Repeat("x", i%3))
	}
	fields[len(fields)-1] = "4242"
	stat := "77 (a (weird) name) " + strings.Join(fields, " ") + " 0 0"

	assert.Equal(t, "4242", procStatStartTicks(stat))
	assert.Empty(t, procStatStartTicks("77 (short) S 1"))
}
