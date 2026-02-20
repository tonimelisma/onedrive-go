# Orchestration Guide — Planning, Agents, and Wrap-Up

This document defines the complete workflow for planning increments, instructing subagents, reviewing their work, and closing increments. The orchestrator (main Claude session) MUST follow this process for every increment that uses subagents.

---

## Phase 1: Planning

### 1.1 Research

Before proposing a plan:

- Read the relevant design docs (architecture.md, sync-algorithm.md, etc.)
- Check BACKLOG.md for related issues
- Review LEARNINGS.md for patterns and gotchas
- Read existing code that will be modified or extended
- Check roadmap.md for current phase status and dependencies

### 1.2 Key Decisions

Present the user with:

- Trade-offs and alternatives (don't assume — ask)
- Test strategy (mocking approach, fixtures, test structure)
- Any BACKLOG items that should be addressed in this increment

### 1.3 Parallelization Analysis (MANDATORY)

Every plan MUST include a parallelization strategy. This is not optional — the user expects work to be parallelized wherever possible.

**Steps:**

1. **Identify work units**: Break the increment into tasks that could be separate PRs
2. **File conflict matrix**: For each pair of work units, list files both would touch. Zero shared files = safe to parallelize
3. **Dependency graph**: Which work units depend on others being merged first?
4. **Wave structure**: Group into waves. Within a wave, all agents run in parallel. Between waves, wait for completion
5. **Worktree plan**: Each agent gets its own worktree + branch. Include exact setup/cleanup commands

If work truly cannot be parallelized (single coherent change touching the same files throughout), state this explicitly with reasoning. But this should be rare — most increments can be split.

### 1.4 Plan Structure

Every plan document must include:

| Section | Contents |
|---------|----------|
| Context | Why this work is needed, what increment |
| Key Decisions | Trade-offs made, alternatives considered |
| Implementation Steps | File-level detail per agent |
| Files Summary | Table: file, action, estimated LOC |
| Parallelization Strategy | Waves, agents, worktrees, file conflict matrix |
| Verification | Commands to validate the complete increment |

---

## Phase 2: Agent Execution

### 2.1 Agent Task Prompt Template

Every task prompt given to a subagent MUST follow this structure. Copy this template and fill it in — do not freestyle prompts.

```
## Task: [Short description]

### Context
[Why this work is needed, what increment it's part of, how it fits
into the larger system]

### Working Directory
- Worktree: [path, e.g. /Users/.../onedrive-go-feat-xxx]
- Branch: [e.g. feat/xxx]
- Base: main

### Implementation Steps
[Detailed steps with file paths, function signatures, expected LOC.
Be specific enough that the agent can work autonomously.]

### Files You Will Create/Modify
[Explicit list — the agent must NOT touch files outside this list
without good reason]

### Quality Gates
Before committing, you MUST verify ALL of:
1. `go build ./...` — zero errors
2. `go test -race ./[package]/...` — all pass (scoped to YOUR package)
3. `golangci-lint run` — zero issues
4. `gofumpt -w .` and `goimports -local github.com/tonimelisma/onedrive-go -w .`
5. Coverage: `go test -coverprofile=/tmp/cover.out ./[package]/... &&
   go tool cover -func=/tmp/cover.out | grep total`

### PR and Merge
[Create PR with gh pr create, then run poll-and-merge.sh]

### Wrap-Up Requirements (MANDATORY — DO NOT SKIP)

Before reporting completion, you MUST do ALL of the following.
Failure to complete these steps means your task is NOT done.

#### A. Update LEARNINGS.md
Append to LEARNINGS.md with a new subsection under the appropriate
section heading. Include ALL of the following categories. If a category
has nothing to report, write "None" — do not skip it.

- **Pivots**: Any deviation from this plan — what changed and why
- **Issues found**: Bugs, code smells, architectural concerns
- **Linter surprises**: Unexpected linter behavior or workarounds
- **Suggested improvements**: Things you noticed but didn't fix
  (out of scope for this task)
- **Cross-package concerns**: Anything affecting other packages or
  future work

Commit the LEARNINGS.md update as part of your PR.

#### B. Final Summary Report
Your final message back to the orchestrator MUST include ALL of:

1. **What was built**: Files created/modified, LOC counts, test counts
2. **Quality metrics**: Coverage %, lint status, build status
3. **Architecture observations**: Does the current architecture feel
   right for this component? Any friction or awkwardness?
4. **Code quality concerns**: Anything that feels like tech debt?
   Code you're not proud of? Patterns that feel forced?
5. **Test gaps**: What edge cases are NOT covered? What would break
   if someone changed the code carelessly?
6. **Process observations**: What was hard about this task? What info
   was missing from the plan? What would make this easier next time?
7. **Re-envisioning**: If you were designing this component from
   scratch knowing what you know now, would you build it the same
   way? What would you change?

DO NOT just report "done, all tests pass." The orchestrator needs
your observations to make informed decisions. Your perspective from
inside the implementation is invaluable and cannot be reconstructed
after your context expires.
```

### 2.2 Launching Agents

1. Set up worktrees for all Wave 1 agents (from main repo)
2. Launch all Wave 1 agents in parallel using the Task tool
3. Monitor progress (check output files periodically)
4. When all Wave 1 agents complete, clean up worktrees
5. Sync main: `git fetch origin && git merge --ff-only origin/main`
6. Set up worktrees for Wave 2, launch Wave 2 agents
7. Repeat until all waves complete

### 2.3 Monitoring

- Check agent output files periodically for progress
- If an agent is stuck, read its output to understand why
- If an agent hits an unexpected issue, consider resuming it with guidance

---

## Phase 3: Post-Agent Review

This phase is MANDATORY. It is NOT optional. The orchestrator must complete ALL steps before presenting the retrospective to the user.

### 3.1 Read Agent Reports

For each agent:
- Read their final summary (the 7-point report from the template)
- Read their LEARNINGS.md entries
- Note concerns, suggested improvements, architectural observations

### 3.2 Code Review (Line-by-Line)

For EVERY file created or significantly modified by agents, the orchestrator MUST read the actual code and check:

**Correctness:**
- [ ] Error paths handled correctly (wrapped with `fmt.Errorf("context: %w", err)`)
- [ ] Edge cases covered (nil, empty, zero values, boundary conditions)
- [ ] No security vulnerabilities (injection, secrets in logs, path traversal)
- [ ] Resource cleanup (defer Close, temp file removal)

**Consistency:**
- [ ] Naming follows codebase conventions (camelCase, verb-first functions)
- [ ] Import grouping (stdlib, third-party, local — separated by blank lines)
- [ ] Error message style matches rest of codebase (lowercase, no punctuation)
- [ ] Function signatures follow Go conventions (context first, error last)
- [ ] Matches patterns established in existing code (e.g., constructor pattern)

**Quality:**
- [ ] Functions are focused (single responsibility, <100 lines)
- [ ] No unnecessary duplication (check with existing code, not just within new code)
- [ ] Comments explain "why" not "what"
- [ ] Logging present at: function entry, state transitions, error paths
- [ ] Tests are table-driven where appropriate
- [ ] Test assertions are specific (not just "no error" — check actual values)

**Gaps:**
- [ ] Missing test cases for edge cases identified by agent or reviewer
- [ ] Missing error handling for unlikely-but-possible failures
- [ ] Missing logging at important code paths
- [ ] Missing comments on non-obvious logic

### 3.3 Top-Up Work

Based on the code review, create a concrete list of issues. For each:

| Issue Type | Action |
|-----------|--------|
| Trivial (typo, missing comment, formatting) | Fix directly in a top-up commit |
| Moderate (missing test, weak error handling) | Fix directly, note in LEARNINGS.md |
| Structural (refactor, design concern) | Present to user with recommendation before acting |

Top-up work is done in a worktree+PR if it touches code, or directly on main if doc-only. The top-up work MUST be completed before presenting the retrospective — don't leave it as a "TODO for next time."

### 3.4 Consolidate Learnings

- Merge all agent LEARNINGS.md entries (deduplicate, clarify, correct)
- Add orchestrator's own observations from the code review
- Update BACKLOG.md: close completed items, add new discoveries
- Update roadmap.md: mark increments as done with actuals (LOC, coverage)

### 3.5 Update CLAUDE.md

- Current phase description
- Package layout (new packages, coverage numbers)
- Any new conventions, linter patterns, or test patterns discovered
- Documentation index (add/remove links as needed)

---

## Phase 4: Retrospective

Present to the user in chat (not just in docs). This is interactive — the user wants to discuss.

### 4.1 Retrospective Content

1. **What went well** — specific examples
2. **What went wrong** — specific examples, not vague
3. **What to change** — concrete actions (not "we should try to...")
4. **Agent observations synthesis** — combine all agents' architecture/quality/process observations into a coherent picture
5. **Top-up work performed** — what the orchestrator fixed after agent review
6. **Re-envisioning check**:
   - Architecture: Are package boundaries still right?
   - Roadmap: Is the increment ordering still optimal?
   - Process: Should we change how we plan/execute/review?
   - Test strategy: Are we testing the right things at the right level?

### 4.2 Capture Actionable Items

After discussing with user:
- Add concrete improvements to LEARNINGS.md
- Add new BACKLOG items for deferred work
- Update CLAUDE.md if process changes are agreed

---

## Quick Reference Checklist

Use this checklist for every increment. Check off each item.

### Planning
- [ ] Research: design docs, BACKLOG, LEARNINGS read
- [ ] Key decisions presented to user
- [ ] Test strategy defined
- [ ] Parallelization analysis complete (waves, file conflicts, worktrees)
- [ ] Plan document written and approved by user

### Execution
- [ ] Worktrees created for Wave 1
- [ ] Agent task prompts include the FULL template (especially Wrap-Up Requirements)
- [ ] Wave 1 agents launched in parallel
- [ ] Worktrees cleaned up after each wave
- [ ] All PRs merged, main CI green

### Post-Agent Review
- [ ] Agent final summaries read (all 7 points)
- [ ] Agent LEARNINGS.md entries read
- [ ] Code review performed (every new/modified file, line by line)
- [ ] Top-up issues identified and fixed
- [ ] LEARNINGS.md consolidated
- [ ] BACKLOG.md updated
- [ ] roadmap.md updated
- [ ] CLAUDE.md updated

### Retrospective
- [ ] Presented to user in chat
- [ ] What went well / wrong / change discussed
- [ ] Agent observations synthesized
- [ ] Top-up work reported
- [ ] Re-envisioning check performed
- [ ] Actionable items captured in docs
