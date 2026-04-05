package devtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type CleanupAuditOptions struct {
	RepoRoot string
	JSON     bool
	Stdout   io.Writer
	Stderr   io.Writer
}

type CleanupClassification string

const (
	CleanupSafeRemove   CleanupClassification = "safe_remove"
	CleanupKeepAttached CleanupClassification = "keep_attached"
	CleanupKeepDirty    CleanupClassification = "keep_dirty"
	CleanupKeepUnmerged CleanupClassification = "keep_unmerged"
	CleanupKeepMain     CleanupClassification = "keep_main"
)

const (
	cleanupMainBranchName         = "main"
	cleanupOriginMainBranchName   = "origin/main"
	cleanupMainBranchRef          = "refs/heads/main"
	cleanupDirtyWorktreeDetail    = "attached worktree has local modifications"
	cleanupRootMainWorktreeDetail = "root main worktree"
	cleanupMainBranchDetail       = "main branch"
	cleanupOriginMainBranchDetail = "origin main branch"
)

type CleanupSubject string

const (
	CleanupSubjectWorktree     CleanupSubject = "worktree"
	CleanupSubjectLocalBranch  CleanupSubject = "local_branch"
	CleanupSubjectRemoteBranch CleanupSubject = "remote_branch"
)

type CleanupAuditEntry struct {
	Subject        CleanupSubject        `json:"subject"`
	Name           string                `json:"name,omitempty"`
	Path           string                `json:"path,omitempty"`
	Branch         string                `json:"branch,omitempty"`
	Classification CleanupClassification `json:"classification"`
	Detail         string                `json:"detail"`
}

type CleanupAuditReport struct {
	Worktrees      []CleanupAuditEntry `json:"worktrees"`
	LocalBranches  []CleanupAuditEntry `json:"local_branches"`
	RemoteBranches []CleanupAuditEntry `json:"remote_branches"`
}

type gitWorktreeState struct {
	Path    string
	Head    string
	Branch  string
	Dirty   bool
	MainRef bool
}

type gitBranchState struct {
	Name     string
	Ref      string
	Attached bool
	Dirty    bool
}

func RunCleanupAudit(ctx context.Context, runner commandRunner, opts CleanupAuditOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	if opts.RepoRoot == "" {
		return fmt.Errorf("cleanup audit: missing repo root")
	}

	if err := runner.Run(ctx, opts.RepoRoot, os.Environ(), io.Discard, stderr, "git", "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("cleanup audit: fetch --prune origin: %w", err)
	}

	worktrees, err := loadWorktreeStates(ctx, runner, opts.RepoRoot)
	if err != nil {
		return fmt.Errorf("cleanup audit: load worktrees: %w", err)
	}
	localBranches, err := loadBranchStates(ctx, runner, opts.RepoRoot, "refs/heads", worktrees)
	if err != nil {
		return fmt.Errorf("cleanup audit: load local branches: %w", err)
	}
	remoteBranches, err := loadBranchStates(ctx, runner, opts.RepoRoot, "refs/remotes/origin", nil)
	if err != nil {
		return fmt.Errorf("cleanup audit: load remote branches: %w", err)
	}

	report, err := classifyCleanupAudit(ctx, runner, opts.RepoRoot, worktrees, localBranches, remoteBranches)
	if err != nil {
		return fmt.Errorf("cleanup audit: classify repo state: %w", err)
	}

	if opts.JSON {
		return writeCleanupAuditJSON(stdout, report)
	}

	return writeCleanupAuditText(stdout, report)
}

func loadWorktreeStates(ctx context.Context, runner commandRunner, repoRoot string) ([]gitWorktreeState, error) {
	output, err := runner.Output(ctx, repoRoot, os.Environ(), "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git worktree list --porcelain: %w", err)
	}

	states := parseGitWorktreePorcelain(string(output), repoRoot)
	for i := range states {
		dirty, dirtyErr := worktreeDirty(ctx, runner, states[i].Path)
		if dirtyErr != nil {
			return nil, fmt.Errorf("git status --porcelain %s: %w", states[i].Path, dirtyErr)
		}
		states[i].Dirty = dirty
	}

	return states, nil
}

func parseGitWorktreePorcelain(output string, repoRoot string) []gitWorktreeState {
	blocks := strings.Split(strings.TrimSpace(output), "\n\n")
	states := make([]gitWorktreeState, 0, len(blocks))
	canonicalRepoRoot := canonicalCleanupPath(repoRoot)

	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}

		state := gitWorktreeState{}
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				state.Path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			case strings.HasPrefix(line, "HEAD "):
				state.Head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
			case strings.HasPrefix(line, "branch "):
				state.Branch = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "branch ")), "refs/heads/")
			}
		}

		state.MainRef = canonicalCleanupPath(state.Path) == canonicalRepoRoot && state.Branch == cleanupMainBranchName
		states = append(states, state)
	}

	return states
}

func worktreeDirty(ctx context.Context, runner commandRunner, worktreePath string) (bool, error) {
	output, err := runner.Output(ctx, worktreePath, os.Environ(), "git", "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status --porcelain: %w", err)
	}

	return strings.TrimSpace(string(output)) != "", nil
}

func loadBranchStates(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	refRoot string,
	worktrees []gitWorktreeState,
) ([]gitBranchState, error) {
	output, err := runner.Output(
		ctx,
		repoRoot,
		os.Environ(),
		"git",
		"for-each-ref",
		"--format=%(refname:short)|%(refname)",
		refRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref %s: %w", refRoot, err)
	}

	attachedBranches := make(map[string]gitWorktreeState, len(worktrees))
	for _, worktree := range worktrees {
		if worktree.Branch != "" {
			attachedBranches[worktree.Branch] = worktree
		}
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	branches := make([]gitBranchState, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("parse branch listing %q", line)
		}

		shortName := strings.TrimSpace(parts[0])
		fullRef := strings.TrimSpace(parts[1])
		if shortName == "origin/HEAD" || shortName == "origin" {
			continue
		}

		branch := gitBranchState{
			Name: shortName,
			Ref:  fullRef,
		}
		if refRoot == "refs/heads" {
			if attached, ok := attachedBranches[shortName]; ok {
				branch.Attached = true
				branch.Dirty = attached.Dirty
			}
		}

		branches = append(branches, branch)
	}

	return branches, nil
}

func canonicalCleanupPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}

	return filepath.Clean(path)
}

func classifyCleanupAudit(
	ctx context.Context,
	runner commandRunner,
	repoRoot string,
	worktrees []gitWorktreeState,
	localBranches []gitBranchState,
	remoteBranches []gitBranchState,
) (CleanupAuditReport, error) {
	report := CleanupAuditReport{
		Worktrees:      make([]CleanupAuditEntry, 0, len(worktrees)),
		LocalBranches:  make([]CleanupAuditEntry, 0, len(localBranches)),
		RemoteBranches: make([]CleanupAuditEntry, 0, len(remoteBranches)),
	}

	for _, worktree := range worktrees {
		reachable, err := refReachableFromMain(ctx, runner, repoRoot, worktreeRef(worktree))
		if err != nil {
			return CleanupAuditReport{}, err
		}
		report.Worktrees = append(report.Worktrees, classifyWorktreeEntry(worktree, reachable))
	}

	for _, branch := range localBranches {
		reachable, err := refReachableFromMain(ctx, runner, repoRoot, branch.Ref)
		if err != nil {
			return CleanupAuditReport{}, err
		}
		report.LocalBranches = append(report.LocalBranches, classifyLocalBranchEntry(branch, reachable))
	}

	for _, branch := range remoteBranches {
		reachable, err := refReachableFromMain(ctx, runner, repoRoot, branch.Ref)
		if err != nil {
			return CleanupAuditReport{}, err
		}
		report.RemoteBranches = append(report.RemoteBranches, classifyRemoteBranchEntry(branch, reachable))
	}

	return report, nil
}

func worktreeRef(worktree gitWorktreeState) string {
	if worktree.Branch != "" {
		return "refs/heads/" + worktree.Branch
	}

	return worktree.Head
}

func refReachableFromMain(ctx context.Context, runner commandRunner, repoRoot, ref string) (bool, error) {
	output, err := runner.Output(
		ctx,
		repoRoot,
		os.Environ(),
		"git",
		"rev-list",
		"--count",
		ref,
		"--not",
		cleanupMainBranchRef,
	)
	if err != nil {
		return false, fmt.Errorf("git rev-list --count %s --not %s: %w", ref, cleanupMainBranchRef, err)
	}

	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return false, fmt.Errorf("parse rev-list count for %s: %w", ref, err)
	}

	return count == 0, nil
}

func classifyWorktreeEntry(worktree gitWorktreeState, reachableFromMain bool) CleanupAuditEntry {
	entry := CleanupAuditEntry{
		Subject: CleanupSubjectWorktree,
		Path:    worktree.Path,
		Branch:  worktree.Branch,
	}

	switch {
	case worktree.MainRef:
		entry.Classification = CleanupKeepMain
		entry.Detail = cleanupRootMainWorktreeDetail
	case worktree.Dirty:
		entry.Classification = CleanupKeepDirty
		entry.Detail = cleanupDirtyWorktreeDetail
	case reachableFromMain:
		entry.Classification = CleanupSafeRemove
		entry.Detail = "worktree head is reachable from main and clean"
	default:
		entry.Classification = CleanupKeepUnmerged
		entry.Detail = "worktree head has commits not reachable from main"
	}

	return entry
}

func classifyLocalBranchEntry(branch gitBranchState, reachableFromMain bool) CleanupAuditEntry {
	entry := CleanupAuditEntry{
		Subject: CleanupSubjectLocalBranch,
		Name:    branch.Name,
	}

	switch {
	case branch.Name == cleanupMainBranchName:
		entry.Classification = CleanupKeepMain
		entry.Detail = cleanupMainBranchDetail
	case branch.Dirty:
		entry.Classification = CleanupKeepDirty
		entry.Detail = cleanupDirtyWorktreeDetail
	case branch.Attached:
		entry.Classification = CleanupKeepAttached
		entry.Detail = "branch is attached to a worktree"
	case reachableFromMain:
		entry.Classification = CleanupSafeRemove
		entry.Detail = "branch tip is reachable from main"
	default:
		entry.Classification = CleanupKeepUnmerged
		entry.Detail = "branch has commits not reachable from main"
	}

	return entry
}

func classifyRemoteBranchEntry(branch gitBranchState, reachableFromMain bool) CleanupAuditEntry {
	entry := CleanupAuditEntry{
		Subject: CleanupSubjectRemoteBranch,
		Name:    branch.Name,
	}

	switch {
	case branch.Name == cleanupOriginMainBranchName:
		entry.Classification = CleanupKeepMain
		entry.Detail = cleanupOriginMainBranchDetail
	case reachableFromMain:
		entry.Classification = CleanupSafeRemove
		entry.Detail = "remote branch tip is reachable from main"
	default:
		entry.Classification = CleanupKeepUnmerged
		entry.Detail = "remote branch has commits not reachable from main"
	}

	return entry
}

func writeCleanupAuditJSON(w io.Writer, report CleanupAuditReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("cleanup audit: write json: %w", err)
	}

	return nil
}

func writeCleanupAuditText(w io.Writer, report CleanupAuditReport) error {
	sections := []struct {
		title   string
		entries []CleanupAuditEntry
	}{
		{title: "worktrees", entries: report.Worktrees},
		{title: "local branches", entries: report.LocalBranches},
		{title: "remote branches", entries: report.RemoteBranches},
	}

	for _, section := range sections {
		if _, err := fmt.Fprintf(w, "cleanup audit: %s\n", section.title); err != nil {
			return fmt.Errorf("cleanup audit: write section header: %w", err)
		}
		if len(section.entries) == 0 {
			if _, err := io.WriteString(w, "- none\n"); err != nil {
				return fmt.Errorf("cleanup audit: write empty section: %w", err)
			}
			continue
		}

		for _, entry := range section.entries {
			label := entry.Name
			if entry.Path != "" {
				label = entry.Path
			}
			if entry.Branch != "" {
				if _, err := fmt.Fprintf(w, "- %s: %s (branch: %s; %s)\n", entry.Classification, label, entry.Branch, entry.Detail); err != nil {
					return fmt.Errorf("cleanup audit: write entry: %w", err)
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "- %s: %s (%s)\n", entry.Classification, label, entry.Detail); err != nil {
				return fmt.Errorf("cleanup audit: write entry: %w", err)
			}
		}
	}

	return nil
}
