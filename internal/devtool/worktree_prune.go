package devtool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// builderWorktreesDir is where Claude Code puts the worktrees of its builder
// subagents, relative to the main checkout. prune-merged looks nowhere else:
// a worktree a person made by hand elsewhere is theirs to remove.
const builderWorktreesDir = ".claude/worktrees"

// procRoot is where Linux exposes its processes.
const procRoot = "/proc"

// agentLockPrefix starts the lock reason Claude Code writes while a builder
// agent works in a worktree: "claude agent <name> (pid <pid> start <ticks>)".
const agentLockPrefix = "claude agent "

// procStatStartField is the 1-based field of /proc/<pid>/stat that holds the
// process's start time in clock ticks since boot, counted from the field
// after the parenthesised command name (which may hold spaces).
const procStatStartField = 22

// procStatFieldsBeforeState is how many fields of /proc/<pid>/stat come
// before the one that follows the command name: pid and comm.
const procStatFieldsBeforeState = 2

// mergeTreeConflictExit is the exit status of `git merge-tree --write-tree`
// when the merge conflicts.
const mergeTreeConflictExit = 1

// squashSearchDepth bounds how many commits of origin/main's first-parent
// line, newest first, are tried as the one that held a branch's work: far
// more than a builder's lifetime of merges, so the bound only stops a
// branch cut long ago from costing a merge per commit of main's history.
const squashSearchDepth = 500

// squashPathspecLimit bounds how many changed paths are passed to git log to
// narrow the commits tried; a branch past it keeps the argument list short and
// is tried against every commit instead.
const squashPathspecLimit = 1000

// mergeBaseNoneExit is the exit status of `git merge-base` when the two
// commits share no history.
const mergeBaseNoneExit = 1

// WorktreePruneOptions configures `devtool worktree prune-merged`.
type WorktreePruneOptions struct {
	// RepoRoot is the checkout the command runs in. Any worktree of the
	// repository works: the builder worktrees are found under the main one.
	RepoRoot string
	// DryRun lists what would be removed and removes nothing.
	DryRun bool
	Stdout io.Writer
	// ProcessRunning reports whether the process an agent lock names still
	// runs. Nil reads /proc, and where /proc cannot tell, every agent
	// counts as running.
	ProcessRunning func(pid int, startTicks string) bool
}

// worktreePruneDecision is the verdict on one builder worktree.
type worktreePruneDecision struct {
	path   string
	branch string
	remove bool
	// unlock is set when the worktree carries the lock of an agent that no
	// longer runs, which has to go before git removes the worktree.
	unlock bool
	reason string
}

// PruneMergedWorktrees removes every builder worktree under
// .claude/worktrees whose work is already in origin/main, and its branch.
//
// A builder's branch is squash-merged, so its commits never reach main by
// ancestry: its work counts as in origin/main when `git cherry` finds every
// commit's patch there, or when merging the branch into origin/main changes
// nothing (`git merge-tree --write-tree` yields origin/main's own tree). Only
// a clean worktree goes, and only through `git worktree remove` without
// --force, so git itself refuses anything with local changes; ignored files
// (build output) go with the directory, as git
// removes them. A worktree locked by a running agent, or locked by hand, is
// kept: the lock is someone saying "in use". Every other worktree is reported
// with the reason it stays. A removal that fails is reported and the rest go
// on; the command then fails naming every failure.
func PruneMergedWorktrees(ctx context.Context, runner commandRunner, opts WorktreePruneOptions) error {
	if opts.RepoRoot == "" {
		return errors.New("worktree prune-merged: repo root is required")
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	processRunning := opts.ProcessRunning
	if processRunning == nil {
		processRunning = procProcessRunning
	}

	if err := runner.Run(ctx, opts.RepoRoot, os.Environ(), io.Discard, os.Stderr,
		"git", "fetch", "origin", mainBranchName); err != nil {
		return fmt.Errorf("worktree prune-merged: fetch origin %s: %w", mainBranchName, err)
	}

	listing, err := runner.Output(ctx, opts.RepoRoot, os.Environ(), "git", "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("worktree prune-merged: list worktrees: %w", err)
	}
	worktrees := parseGitWorktreePorcelain(string(listing), opts.RepoRoot)
	if len(worktrees) == 0 {
		return errors.New("worktree prune-merged: git listed no worktrees")
	}
	// git lists the main worktree first.
	mainRoot := worktrees[0].Path
	buildersRoot := canonicalCleanupPath(filepath.Join(mainRoot, builderWorktreesDir))
	here := canonicalCleanupPath(opts.RepoRoot)

	mainTree, err := runner.Output(ctx, mainRoot, os.Environ(), "git", "rev-parse", originMainRef+"^{tree}")
	if err != nil {
		return fmt.Errorf("worktree prune-merged: read %s tree: %w", originMainRef, err)
	}
	judge := worktreeJudge{
		runner:         runner,
		mainRoot:       mainRoot,
		mainTree:       strings.TrimSpace(string(mainTree)),
		here:           here,
		processRunning: processRunning,
	}

	var (
		decisions     []worktreePruneDecision
		judgeFailures []error
	)
	for _, worktree := range worktrees[1:] {
		if !pathWithin(canonicalCleanupPath(worktree.Path), buildersRoot) {
			continue
		}
		// A worktree git cannot judge is kept and reported, and the rest are
		// still judged; the command then fails naming it.
		decision, judgeErr := judge.decide(ctx, worktree)
		if judgeErr != nil {
			judgeErr = fmt.Errorf("judge %s: %w", worktree.Path, judgeErr)
			judgeFailures = append(judgeFailures, judgeErr)
			decision = worktreePruneDecision{path: worktree.Path, branch: decision.branch, reason: judgeErr.Error()}
		}
		decisions = append(decisions, decision)
	}

	return applyWorktreePrune(ctx, runner, mainRoot, decisions, judgeFailures, opts.DryRun, stdout)
}

func applyWorktreePrune(
	ctx context.Context,
	runner commandRunner,
	mainRoot string,
	decisions []worktreePruneDecision,
	judgeFailures []error,
	dryRun bool,
	stdout io.Writer,
) error {
	failures := judgeFailures
	var (
		removed int
		kept    int
	)
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	// The verdicts print as they land, so a removal that hangs shows how far
	// the command got; the first write that fails is returned at the end.
	var writeErr error
	printf := func(format string, args ...any) {
		if writeErr == nil {
			_, writeErr = fmt.Fprintf(stdout, format, args...)
		}
	}

	for _, decision := range decisions {
		if !decision.remove {
			kept++
			printf("kept %s (branch %s): %s\n", decision.path, decision.branch, decision.reason)
			continue
		}
		if !dryRun {
			if err := removeBuilderWorktree(ctx, runner, mainRoot, decision); err != nil {
				kept++
				failures = append(failures, err)
				printf("kept %s (branch %s): %v\n", decision.path, decision.branch, err)
				continue
			}
		}
		removed++
		printf("%s %s (branch %s): %s\n", verb, decision.path, decision.branch, decision.reason)
	}
	printf("%d %s, %d kept\n", removed, verb, kept)
	if writeErr != nil {
		failures = append(failures, fmt.Errorf("write report: %w", writeErr))
	}

	if len(failures) > 0 {
		return fmt.Errorf("worktree prune-merged: %w", errors.Join(failures...))
	}

	return nil
}

func removeBuilderWorktree(ctx context.Context, runner commandRunner, mainRoot string, decision worktreePruneDecision) error {
	if decision.unlock {
		if err := runner.Run(ctx, mainRoot, os.Environ(), io.Discard, os.Stderr,
			"git", "worktree", "unlock", decision.path); err != nil {
			return fmt.Errorf("unlock worktree %s: %w", decision.path, err)
		}
	}
	if err := runner.Run(ctx, mainRoot, os.Environ(), io.Discard, os.Stderr,
		"git", "worktree", "remove", decision.path); err != nil {
		return fmt.Errorf("remove worktree %s: %w", decision.path, err)
	}
	if err := runner.Run(ctx, mainRoot, os.Environ(), io.Discard, os.Stderr,
		"git", "branch", "-D", decision.branch); err != nil {
		return fmt.Errorf("delete branch %s of removed worktree %s: %w", decision.branch, decision.path, err)
	}

	return nil
}

// worktreeJudge holds what every verdict reads: the main checkout git runs
// in and origin/main's tree.
type worktreeJudge struct {
	runner         commandRunner
	mainRoot       string
	mainTree       string
	here           string
	processRunning func(pid int, startTicks string) bool
}

func (j worktreeJudge) decide(ctx context.Context, worktree gitWorktreeState) (worktreePruneDecision, error) {
	decision := worktreePruneDecision{path: worktree.Path, branch: worktree.Branch}
	if decision.branch == "" {
		decision.branch = "(detached)"
	}

	keep := func(reason string) (worktreePruneDecision, error) {
		decision.reason = reason
		return decision, nil
	}

	switch {
	case canonicalCleanupPath(worktree.Path) == j.here:
		return keep("this command runs in it")
	case worktree.Prunable:
		return keep("its directory is gone; `git worktree prune` clears the entry")
	case worktree.Branch == "":
		return keep("detached HEAD: no branch to judge")
	case worktree.Locked:
		running, reason := j.lockHeld(worktree.LockReason)
		if running {
			return keep(reason)
		}
		decision.unlock = true
	}

	status, err := j.runner.Output(ctx, worktree.Path, os.Environ(), "git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return decision, fmt.Errorf("read status: %w", err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return keep("has uncommitted or untracked changes")
	}

	merged, reason, err := j.workInOriginMain(ctx, worktree.Branch)
	if err != nil {
		return decision, err
	}
	decision.reason = reason
	decision.remove = merged

	return decision, nil
}

// lockHeld reports whether a lock still stands, with the reason to give for
// keeping the worktree. Only the lock of an agent that no longer runs is
// stale; a lock without that form was set by a person and stands.
func (j worktreeJudge) lockHeld(lockReason string) (bool, string) {
	pid, startTicks, ok := parseAgentLock(lockReason)
	if !ok {
		return true, fmt.Sprintf("locked (%q): unlock it yourself if it is no longer in use", lockReason)
	}
	if j.processRunning(pid, startTicks) {
		return true, fmt.Sprintf("locked by a running agent (pid %d)", pid)
	}

	return false, ""
}

// parseAgentLock reads the pid and start ticks out of an agent lock reason,
// "claude agent <name> (pid <pid> start <ticks>)".
func parseAgentLock(lockReason string) (int, string, bool) {
	rest, ok := strings.CutPrefix(lockReason, agentLockPrefix)
	if !ok {
		return 0, "", false
	}
	open := strings.LastIndex(rest, "(")
	if open < 0 || !strings.HasSuffix(rest, ")") {
		return 0, "", false
	}
	fields := strings.Fields(rest[open+1 : len(rest)-1])
	const agentLockFieldCount = 4
	if len(fields) != agentLockFieldCount || fields[0] != "pid" || fields[2] != "start" {
		return 0, "", false
	}
	pid, err := strconv.Atoi(fields[1])
	if err != nil || pid <= 0 {
		return 0, "", false
	}

	return pid, fields[3], true
}

// procProcessRunning reports whether pid runs and started at startTicks, so
// a pid the kernel has since given to another process does not hold a dead
// agent's lock. Without /proc (macOS) it cannot tell and says it runs.
func procProcessRunning(pid int, startTicks string) bool {
	if err := statPath(filepath.Join(procRoot, "self", "stat")); err != nil {
		return true
	}
	data, err := readFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}

	return procStatStartTicks(string(data)) == startTicks
}

// procStatStartTicks returns the start-time field of a /proc/<pid>/stat line.
func procStatStartTicks(stat string) string {
	closeParen := strings.LastIndex(stat, ")")
	if closeParen < 0 {
		return ""
	}
	fields := strings.Fields(stat[closeParen+1:])
	index := procStatStartField - procStatFieldsBeforeState - 1
	if index >= len(fields) {
		return ""
	}

	return fields[index]
}

// workInOriginMain reports whether branch's work is already in origin/main,
// with the reason for the verdict.
//
// A squash merge lands the branch's changes as one new commit, and main may
// change the same lines afterwards, so the branch's work counts as in
// origin/main when merging the branch into some commit of origin/main's
// first-parent line since the merge base changes nothing: the newest first,
// origin/main itself before every older one. That commit held all of the
// branch's work; what main did with it later is main's own history.
func (j worktreeJudge) workInOriginMain(ctx context.Context, branch string) (bool, string, error) {
	// Every check below compares from the merge base. Without one, which is
	// what a shallow clone shows for a branch cut before its oldest fetched
	// commit, no answer means anything.
	base, err := j.runner.Output(ctx, j.mainRoot, os.Environ(), "git", "merge-base", originMainRef, branch)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == mergeBaseNoneExit {
		return false, "shares no history with " + originMainRef + " here; in a shallow clone, " +
			"`git fetch --unshallow origin` and run again", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("find merge base of %s and %s: %w", branch, originMainRef, err)
	}

	cherry, err := j.runner.Output(ctx, j.mainRoot, os.Environ(), "git", "cherry", originMainRef, branch)
	if err != nil {
		return false, "", fmt.Errorf("compare %s with %s: %w", branch, originMainRef, err)
	}
	if everyCherryLineUpstream(string(cherry)) {
		return true, "every commit is in " + originMainRef, nil
	}

	mergeBase := strings.TrimSpace(string(base))
	paths, err := j.changedPaths(ctx, mergeBase, branch)
	if err != nil {
		return false, "", err
	}
	candidates, err := j.squashCandidates(ctx, mergeBase, branch, paths)
	if err != nil {
		return false, "", err
	}
	for _, candidate := range candidates {
		changedEvery, err := j.changedEveryPath(ctx, mergeBase, candidate.commit, paths)
		if err != nil {
			return false, "", err
		}
		if !changedEvery {
			continue
		}
		contained, err := j.mergeChangesNothing(ctx, candidate.commit, candidate.tree, branch)
		if err != nil {
			return false, "", err
		}
		if !contained {
			continue
		}
		if candidate.tree == j.mainTree {
			return true, "its changes are already in " + originMainRef + " (squash-merged)", nil
		}
		const shortCommitLength = 12
		return true, fmt.Sprintf("its changes were all in %s at %s (squash-merged; main changed them since)",
			originMainRef, candidate.commit[:min(len(candidate.commit), shortCommitLength)]), nil
	}

	return false, "has changes " + originMainRef + " does not contain", nil
}

// squashCandidate is a commit of origin/main's first-parent line, with its
// tree, tried as the one that held a branch's work.
type squashCandidate struct {
	commit string
	tree   string
}

// squashCandidates lists the commits worth a merge to find the one that held
// branch's work: origin/main itself, then, newest first and at most
// squashSearchDepth, the commits of its first-parent line since base that
// changed a path the branch changed. Merging the branch into a commit leaves
// every other path as that commit has it, so between two such commits the
// verdict cannot change; a merge per commit of main's history is what made
// the command take minutes once a clone holds its full history. Each verdict
// still comes from a real merge, so a commit skipped here can only keep a
// worktree, never remove one wrongly. A branch touching more than
// squashPathspecLimit paths is tried against every commit instead.
func (j worktreeJudge) squashCandidates(
	ctx context.Context, base, branch string, paths []string,
) ([]squashCandidate, error) {
	tip, err := j.runner.Output(ctx, j.mainRoot, os.Environ(), "git", "rev-parse", originMainRef)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", originMainRef, err)
	}
	tipCommit := strings.TrimSpace(string(tip))
	candidates := []squashCandidate{{commit: tipCommit, tree: j.mainTree}}
	if len(paths) == 0 {
		return candidates, nil
	}

	args := []string{
		"log", "--first-parent", "--format=%H %T", "--max-count=" + strconv.Itoa(squashSearchDepth),
		base + ".." + originMainRef,
	}
	if len(paths) <= squashPathspecLimit {
		args = append(args, "--")
		for _, path := range paths {
			// A literal pathspec: a path with glob characters names only itself.
			args = append(args, ":(literal)"+path)
		}
	}
	listing, err := j.runner.Output(ctx, j.mainRoot, os.Environ(), "git", args...)
	if err != nil {
		return nil, fmt.Errorf("list %s since its merge base with %s: %w", originMainRef, branch, err)
	}
	for _, entry := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
		commit, tree, ok := strings.Cut(entry, " ")
		if !ok || commit == tipCommit {
			continue
		}
		candidates = append(candidates, squashCandidate{commit: commit, tree: tree})
	}

	return candidates, nil
}

// changedPaths lists the paths whose content differs between two commits, a
// rename as both of its paths. It compares trees only, never file contents
// line by line, so it costs milliseconds where a merge costs a second.
func (j worktreeJudge) changedPaths(ctx context.Context, from, to string) ([]string, error) {
	changed, err := j.runner.Output(ctx, j.mainRoot, os.Environ(), "git", "diff", "--name-only", "--no-renames", "-z",
		from, to)
	if err != nil {
		return nil, fmt.Errorf("list paths changed from %s to %s: %w", from, to, err)
	}
	var paths []string
	for _, path := range strings.Split(string(changed), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}

	return paths, nil
}

// changedEveryPath reports whether commit differs from base at every one of
// paths, the paths a branch changed. It is necessary for the branch's work to
// be in commit: where commit still has base's version of a path the branch
// changed, a three-way merge takes the branch's version, so the merge changes
// commit's tree. Checking it first spares the merge for nearly every commit.
func (j worktreeJudge) changedEveryPath(ctx context.Context, base, commit string, paths []string) (bool, error) {
	if len(paths) == 0 {
		return true, nil
	}
	changed, err := j.changedPaths(ctx, base, commit)
	if err != nil {
		return false, err
	}
	changedSet := make(map[string]struct{}, len(changed))
	for _, path := range changed {
		changedSet[path] = struct{}{}
	}
	for _, path := range paths {
		if _, ok := changedSet[path]; !ok {
			return false, nil
		}
	}

	return true, nil
}

// mergeChangesNothing reports whether merging branch into commit, whose tree
// is tree, leaves that tree as it is. A conflicting merge changes something.
func (j worktreeJudge) mergeChangesNothing(ctx context.Context, commit, tree, branch string) (bool, error) {
	merged, err := j.runner.Output(ctx, j.mainRoot, os.Environ(), "git", "merge-tree", "--write-tree", commit, branch)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == mergeTreeConflictExit {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("merge %s into %s: %w", branch, commit, err)
	}
	mergedTree, _, _ := strings.Cut(strings.TrimSpace(string(merged)), "\n")

	return mergedTree == tree, nil
}

// everyCherryLineUpstream reports whether every line of `git cherry` output
// starts with "-": each commit's patch is already upstream. No line at all
// means the branch has no commit of its own.
func everyCherryLineUpstream(output string) bool {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" && !strings.HasPrefix(line, "-") {
			return false
		}
	}

	return true
}

// pathWithin reports whether path lies inside dir (not dir itself).
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}

	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
