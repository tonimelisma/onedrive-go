# Parallel Agent Workflow Guide

This guide covers running multiple AI coding agents simultaneously on the same repository using Git worktrees.

## Overview

| Component | Choice | Why |
|-----------|--------|-----|
| Isolation | Git Worktrees | Disk-efficient, shared .git |
| Branching | Feature branches | Required by worktrees, enables PRs |
| Merging | Manual after CI | Wait for CI to pass, then merge |
| Alerts | Email notifications | Know immediately if CI fails |

## Naming Conventions

| Type | Format | Example |
|------|--------|---------|
| **Branch** | `<type>/<task-name>` | `feat/add-login`, `fix/api-timeout` |
| **Worktree** | `onedrive-go-<type>-<task>` | `onedrive-go-feat-add-login`, `onedrive-go-fix-api-timeout` |

**Types:** `feat`, `fix`, `refactor`, `test`, `docs`, `chore`

## GitHub Setup (One-Time)

**Notifications** (Personal Settings → Notifications):
- Actions: Send notifications for failed workflows only

> **Note:** The repo has "Automatically delete head branches" enabled. Remote branches are deleted by GitHub when PRs merge.

## Pre-Work Checklist

**Run these commands from the main repo BEFORE starting any task:**

```bash
cd ~/Development/onedrive-go

# 1. Sync main with GitHub
git fetch origin && git merge --ff-only origin/main

# 2. List existing worktrees
git worktree list

# 3. Clean up stale worktrees (whose branches have been merged)
#    For each finished worktree:
#    NEVER use --force! Inspect uncommitted work first with git status.
git worktree remove ../onedrive-go-<type>-<old-task>
git branch -D <type>/<old-task>

# 4. Prune orphaned worktree refs
git worktree prune

# 5. Create new worktree for your task
#    (config.json is copied automatically by the post-checkout hook
#     using .worktreeinclude — no manual cp needed)
git worktree add ../onedrive-go-<type>-<task> -b <type>/<task-name> main

# 6. Move to worktree
cd ../onedrive-go-<type>-<task>
```

> **CRITICAL: You are now in the worktree directory.**
>
> From this point forward, ALL work happens inside `~/Development/onedrive-go-<type>-<task>/`.
>
> - Run all commands from this directory (just `git status`, not `git -C /path status`)
> - Do NOT use path flags or absolute paths to target the worktree
> - Do NOT return to the main repo until cleanup after merge
>
> **If a command is rejected or blocked:** Check your working directory with `pwd`.
> If you're in `~/Development/onedrive-go/` instead of the worktree, that's the problem.
> Change to the worktree directory and retry.

## Working in a Worktree

### Making Changes

```bash
# IMPORTANT: You MUST be in ~/Development/onedrive-go-<type>-<task>/
# Verify with: pwd

# Go modules auto-download — no install step needed

# Run tests and lint
go test -race ./...
golangci-lint run

# Commit
git add specific-files.go
git commit -m "feat: add new capability"

# Push to remote
git push -u origin <type>/<task-name>
```

### Creating PR and Merging

```bash
# Create the PR
gh pr create \
  --title "feat: add new capability" \
  --body "Description of changes"

# Wait for CI to pass, then merge
./scripts/poll-and-merge.sh <pr_number>

# Or poll multiple PRs sequentially:
./scripts/poll-and-merge.sh 71 72
```

### When Another Agent's PR Merges

If your worktree is based on an older main, rebase:

```bash
git fetch origin main
git rebase origin/main

# If conflicts:
# 1. Resolve conflicts in files
# 2. git add resolved-files
# 3. git rebase --continue

# Push updated branch
git push --force-with-lease
```

## Definition of Done (DOD)

Run the DOD Quick Check from CLAUDE.md, then:

- [ ] Committed with descriptive message
- [ ] Pushed to feature branch
- [ ] PR created with `gh pr create`
- [ ] Run `./scripts/poll-and-merge.sh <pr_number>` (polls CI, merges, verifies post-merge)

## After PR: Wait for CI, Merge, and Cleanup

### MANDATORY: AI agents MUST follow all steps

```bash
# 1. Wait for CI checks to pass and merge
./scripts/poll-and-merge.sh <pr_number>

# 2. Return to main repo
cd ~/Development/onedrive-go

# 3. Remove worktree and branch
# NEVER use --force! If remove refuses, inspect uncommitted work first.
#    Go back to the worktree, run `git status`, review any uncommitted files.
#    --force DESTROYS uncommitted work with NO recovery.
git worktree remove ../onedrive-go-<type>-<task>
git branch -D <type>/<task-name>

# 4. Sync main
git fetch origin && git merge --ff-only origin/main
```

> **CRITICAL FOR AI AGENTS:** You MUST complete ALL steps including cleanup. Failure to clean up leaves stale worktrees that block other agents.
>
> **NEVER force-remove a worktree.** If `git worktree remove` refuses due to uncommitted/untracked files, go back to the worktree, run `git status`, and manually inspect what's there before deciding whether to discard it. `--force` is equivalent to `rm -rf` on uncommitted work.
>
> **Note:** Remote branches are auto-deleted by GitHub when PRs merge. Only local worktree and branch cleanup is needed.

## Task Assignment Guidelines

To minimize conflicts, assign agents to **non-overlapping areas**:

| Agent | Scope | Example Directories |
|-------|-------|---------------------|
| Agent 1 | Graph client | `internal/graph/` |
| Agent 2 | CLI commands | `cmd/onedrive-go/` |
| Agent 3 | Tests/Docs | `*_test.go`, `docs/` |

**High-conflict files** to avoid parallel edits:
- `go.mod` / `go.sum`
- `CLAUDE.md`, `LEARNINGS.md`
- `.golangci.yml`

### Test Symbol Collisions in Same-Package Agents

See orchestration.md §2.2 for the full prevention rules. Key pattern — embed for extension:

```go
type reconcilerMockStore struct {
    mockStore // embed the existing mock from delta_test.go
}
// Override only the methods you need different behavior for
func (s *reconcilerMockStore) ListAllActiveItems(...) { ... }
```

## Troubleshooting

### "Branch already checked out"
```bash
# Can't checkout same branch in two worktrees
# Solution: Use different branch names
git worktree add ../onedrive-go-feat-other -b feat/other-task main
```

### CI Fails, PR Won't Merge
```bash
# poll-and-merge.sh will report the failure and exit code.
# View failed workflow logs:
gh run view --log-failed
# Fix the issue, push, and re-run:
git push
./scripts/poll-and-merge.sh <pr_number>
```

### Merge Conflicts on Rebase
```bash
git status                    # See conflicting files
# ... resolve conflicts ...
git add resolved-file.go
git rebase --continue

# If too messy:
git rebase --abort
git merge origin/main
```

## Quick Reference

| Task | Command |
|------|---------|
| Sync main | `git fetch origin && git merge --ff-only origin/main` |
| Create worktree | `git worktree add ../onedrive-go-<type>-<task> -b <type>/<task> main` |
| List worktrees | `git worktree list` |
| Remove worktree | `git worktree remove ../onedrive-go-<type>-<task>` (NEVER `--force` — inspect first) |
| Delete branch | `git branch -D <type>/<task>` |
| Prune refs | `git worktree prune` |
| Create PR | `gh pr create` |
| Wait for CI + merge + verify | `./scripts/poll-and-merge.sh <pr_number>` |
| Rebase | `git fetch origin main && git rebase origin/main` |
| Force push | `git push --force-with-lease` |
