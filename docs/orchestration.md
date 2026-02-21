# Orchestration Guide — Planning, Agents, and Wrap-Up

This document defines the complete workflow for planning increments, instructing subagents, reviewing their work, and closing increments. The orchestrator (main Claude session) MUST follow this process for every increment.

**Core principle**: The human must have full visibility into what's happening, understand all quality trade-offs, and make all non-trivial decisions. Information flows upward — from agents through the orchestrator to the human. Nothing is hidden, nothing is silent.

---

## Phase 1: Planning

### 1.1 Intent Confirmation

Before investing in a plan, restate what you understand the goal to be:

- What the increment will accomplish
- What is explicitly out of scope (non-goals)
- What success looks like

Get the human's confirmation before proceeding. Don't assume.

### 1.2 Research

Before proposing a plan:

- Read the relevant design docs (architecture.md, sync-algorithm.md, etc.)
- Check BACKLOG.md for related issues that should be addressed
- Review LEARNINGS.md for patterns and gotchas
- Read existing code that will be modified or extended
- Check roadmap.md for current phase status and dependencies

**Show your research**: Present the human with a brief summary of what you found — relevant BACKLOG items, applicable LEARNINGS, existing code patterns you'll build on. The human sees your inputs, not just your conclusions.

### 1.3 Key Decisions

For every significant decision in the plan, present:

- At least two alternatives with trade-offs
- Your recommendation and why
- Risks of each approach

**Escalation rule**: Escalate anything non-trivial to the human. Only purely mechanical choices (variable names, import ordering) are autonomous. Anything involving trade-offs, architectural implications, or deviation from established patterns requires the human's input.

Also present:
- Test strategy (mocking approach, fixtures, test structure)
- Any BACKLOG items that should be addressed in this increment

### 1.4 CI Impact Analysis

If the increment changes anything that affects CI (token paths, secret naming, environment variables, workflow YAML, data directory locations, config file format):

1. **List every CI artifact affected**: Key Vault secret names, GitHub variables, workflow steps, token file paths
2. **Include infrastructure changes in the plan**: Don't defer Key Vault/GitHub variable updates to "after merge" — they are part of the increment. The orchestrator manages these directly via `az` CLI and `gh` CLI.
3. **Ask the minimal-environment question**: "What happens when this code runs with no config file, no home directory customization, and only environment variables for input?" CI environments are stripped-down — never assume config files exist.
4. **Include local validation step**: Plan must specify running `scripts/validate-ci-locally.sh` (or equivalent manual steps) before pushing CI-impacting changes.
5. **Check test-strategy.md accuracy**: If the increment changes CI behavior, verify that test-strategy.md §6.1 and §10.x still match the actual workflow YAML. Update if stale.

### 1.5 Parallelization Analysis (MANDATORY)

Every plan MUST include a parallelization strategy. This is not optional.

**Steps:**

1. **Identify work units**: Break the increment into tasks that could be separate PRs
2. **File conflict matrix**: For each pair of work units, list files both would touch. Zero shared files = safe to parallelize
3. **Dependency graph**: Which work units depend on others being merged first?
4. **Wave structure**: Group into waves. Within a wave, all agents run in parallel. Between waves, wait for completion
5. **Worktree plan**: Each agent gets its own worktree + branch. Include exact setup/cleanup commands

If work truly cannot be parallelized (single coherent change touching the same files throughout), state this explicitly with reasoning. But this should be rare.

### 1.6 Plan Structure

Every plan document must include:

| Section | Contents |
|---------|----------|
| Context | Why this work is needed, what increment |
| Non-Goals | What this increment deliberately does NOT include and why |
| Key Decisions | Trade-offs made, alternatives considered, human's choices |
| Implementation Steps | File-level detail per agent |
| Files Summary | Table: file, action (create/modify/delete) |
| Parallelization Strategy | Waves, agents, worktrees, file conflict matrix |
| Risk Register | Known risks, mitigations, accepted risks (with human acknowledgment) |
| Verification | Commands to validate the complete increment |

### 1.7 Definition of Ready

A plan is ready for execution when ALL of:

- [ ] Intent confirmed with human
- [ ] Research summary presented
- [ ] All non-trivial decisions escalated and resolved
- [ ] Alternatives documented for significant choices
- [ ] Non-goals section written
- [ ] Test strategy defined
- [ ] Parallelization analysis complete (waves, file conflicts, worktrees)
- [ ] CI impact analysis complete (if applicable — Key Vault, GitHub vars, workflow YAML, local validation)
- [ ] Risk register written, risks acknowledged
- [ ] Human has approved the plan

### 1.8 Decision Log

After plan approval, write a brief summary:

```
Decisions made:
- Chose X over Y because [human's reasoning]
- Accepted risk Z because [rationale]
- Deferred W to [future increment] because [reason]
```

This goes in the plan file or commit message. It's a permanent record.

---

## Phase 2: Agent Execution

### 2.1 Agent Task Prompt Template

Every task prompt given to a subagent MUST follow this structure. Copy this template and fill it in — do not freestyle prompts.

```
## Task: [Short description]

### Context
[Why this work is needed, what increment it's part of, how it fits
into the larger system. Include relevant LEARNINGS.md entries that
the agent should know about to avoid repeating known mistakes.]

### Parallel Agents
[What other agents are running in parallel, what files they're
touching. This agent must NOT touch those files. Flag immediately
if you discover a dependency on another agent's work.]

### Working Directory
- Worktree: [path, e.g. /Users/.../onedrive-go-feat-xxx]
- Branch: [e.g. feat/xxx]
- Base: main

### Implementation Steps
[Detailed steps with file paths, function signatures.
Be specific enough that the agent can work autonomously.]

### Files You Will Create/Modify
[Explicit list — the agent must NOT touch files outside this list
without good reason and documentation of why.]

### Existing Test Helpers (DO NOT REDEFINE)
[List all types, functions, and variables already defined in
test files within the same package. The agent MUST NOT redeclare
these — use the existing versions. Example:
- `mockStore` (delta_test.go) — use this, do NOT create your own
- `testLogger(t)` (delta_test.go) — returns *slog.Logger for tests
- `newMockStore()` (delta_test.go) — returns initialized mock
If you need a mock with different behavior, embed the existing
mock and override specific methods, or use a uniquely-prefixed
name like `<component>MockStore`.]

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
section heading. Include ALL of the following categories. If a
category has nothing to report, write "None" — do not skip it.

- **Pivots**: Any deviation from this plan — what changed and why
- **Issues found**: Bugs, code smells, architectural concerns
- **Linter surprises**: Unexpected linter behavior or workarounds
- **Suggested improvements**: Things you noticed but didn't fix
  (out of scope for this task)
- **Cross-package concerns**: Anything affecting other packages or
  future work
- **Code smells noticed**: In your own code AND in existing code
  you read. Even if you can't fix them. List each one.

Commit the LEARNINGS.md update as part of your PR.

#### B. Decision Log
List every decision you made that was NOT specified in the plan.
Even small ones. "I named this function X because Y." "I chose to
handle this error with Z because W." The orchestrator needs this
to understand your reasoning after your context expires.

#### C. Final Summary Report
Your final message back to the orchestrator MUST include ALL of:

1. **What was built**: Files created/modified, test counts,
   new dependencies added
2. **Quality metrics**: Coverage % (before and after), lint status,
   build status
3. **Confidence ratings**: Rate your confidence 1-5 for each major
   area of your work (e.g., "happy path: 5/5, error handling: 3/5,
   edge cases: 2/5"). Be honest — this helps prioritize review.
4. **Risk flags**: Anything you're uncertain about, anything that
   felt wrong, anything you'd want a second pair of eyes on.
   Use red/yellow/green per area.
5. **Architecture observations**: Does the current architecture
   feel right for this component? Any friction or awkwardness?
   Any coupling that concerns you?
6. **Code quality concerns**: Anything that feels like tech debt?
   Code you're not proud of? Patterns that feel forced by the
   linter rather than genuinely good?
7. **Test gaps**: What edge cases are NOT covered? What would break
   if someone changed the code carelessly? What would you test
   "if you had more time"?
8. **Process observations**: What was hard about this task? What
   info was missing from the plan? What would make it easier?
9. **Re-envisioning**: If you were designing this component from
   scratch knowing what you know now, would you build it the same
   way? What would you change about the architecture, API surface,
   package structure, or test approach?

DO NOT just report "done, all tests pass." The orchestrator needs
your observations to make informed decisions. Your perspective from
inside the implementation is invaluable and cannot be reconstructed
after your context expires.
```

### 2.2 Test Symbol Collision Prevention (MANDATORY)

When multiple agents create test files in the same Go package, symbol redeclarations cause build failures after merge. The orchestrator MUST prevent this:

1. **List existing test symbols**: Before launching agents, grep all `*_test.go` files in the target package for exported/unexported types, functions, and variables. Include this list in every agent's prompt under "Existing Test Helpers."

2. **Assign unique prefixes**: Each agent's mock types and helpers MUST use a component-specific prefix:
   - Agent working on `reconciler.go` → `reconcilerMockStore`, `newReconcilerMockStore`
   - Agent working on `safety.go` → `safetyMockStore`, `newSafetyMockStore`
   - **Exception**: If an existing mock (e.g., `mockStore` from `delta_test.go`) provides all needed methods, the agent should reuse it directly instead of creating a new one.

3. **Share common helpers**: `testLogger(t)` and similar utilities should be defined exactly ONCE per package. All agents reuse the existing definition.

4. **Plan merge order**: When possible, merge agents that define shared test infrastructure first. Agents that only consume test helpers merge after.

5. **Assign unique LEARNINGS.md section numbers**: Each agent prompt MUST specify the section number for LEARNINGS.md (e.g., "Agent A → section 27, Agent B → section 28"). Without this, agents pick the same number and cause merge conflicts.

6. **Plan for gocritic/hugeParam on new struct types**: When an agent creates functions accepting structs > 80 bytes by value, the `gocritic:hugeParam` linter will force pointer returns/parameters. Include these type changes in the file conflict matrix, since they affect shared types in `types.go`.

### 2.3 Pre-Launch Briefing

Before launching agents, present the human with a brief summary:

```
Launching [N] agents in Wave [X]:

Agent A ([branch]): [one-line description]
  Files: [list]

Agent B ([branch]): [one-line description]
  Files: [list]

File conflicts: None (safe to parallelize)
```

This gives the human a last chance to catch scope issues before agents start.

### 2.4 Launching Agents

1. Set up worktrees for all Wave N agents (from main repo)
2. Launch all Wave N agents in parallel using the Task tool
3. Provide milestone updates to the human (see 2.4)
4. When all Wave N agents complete, clean up worktrees
5. Sync main: `git fetch origin && git merge --ff-only origin/main`
6. Repeat for next wave

### 2.5 Milestone Updates

During execution, report to the human at these milestones:

- Agent started working
- Agent created PR (include PR number and link)
- CI passed/failed for PR
- PR merged to main
- **Integration tests passed/failed on main** (wait for `integration.yml` — catches regressions unit tests miss)
- **Key Vault / CI infrastructure updated** (when increment changes token paths, secret naming, or env vars — orchestrator manages this directly via `az` CLI)
- Agent finished (brief one-line: success or issue encountered)
- Wave complete (all agents in wave done)

Format: concise, one line per event. Don't interrupt the human's flow with walls of text.

### 2.6 Escalation During Execution

If an agent encounters an issue that requires a non-trivial decision:

1. Read the agent's output to understand the issue
2. Present the issue to the human with options
3. Resume the agent with the human's decision

Do NOT make the decision autonomously. Do NOT let the agent make it.

**Operational tasks the orchestrator handles directly** (no escalation needed):
- Azure Key Vault: `az keyvault secret set/download/list/show` — creating, updating, verifying secrets
- GitHub variables: `gh variable set/get` — setting repository variables
- CI workflow dispatch: `gh workflow run` — re-triggering workflows
- Token validation: downloading tokens, checking JSON structure, verifying `whoami` works
- Local CI validation: running `scripts/validate-ci-locally.sh`

**Tasks that require the human**:
- One-time Azure infrastructure: service principal creation, RBAC assignment, federated credentials
- Interactive browser-based flows: OAuth `login` that opens a browser
- Trade-offs in architecture, API design, or scope
- Any deviation from the approved plan

---

## Phase 3: Post-Agent Review

This phase is MANDATORY. The orchestrator must complete ALL steps before presenting the retrospective.

### 3.1 Read Agent Reports

For each agent:

- Read their final summary (all 9 points from the template)
- Read their LEARNINGS.md entries
- Read their decision log
- Note: confidence ratings, risk flags, architecture observations, code quality concerns
- Note: any agent-to-agent contradictions or overlapping concerns

### 3.2 Code Review (Line-by-Line)

For EVERY file created or significantly modified by agents, read the actual code and check:

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
- [ ] No unnecessary duplication (check against existing code, not just within new code)
- [ ] Comments explain "why" not "what"
- [ ] Logging present at: function entry, state transitions, error paths
- [ ] Tests are table-driven where appropriate
- [ ] Test assertions are specific (not just "no error" — check actual values)
- [ ] No speculative code (unused helpers, premature abstractions)

**Gaps:**
- [ ] Missing test cases for edge cases
- [ ] Missing error handling for unlikely-but-possible failures
- [ ] Missing logging at important code paths
- [ ] Missing comments on non-obvious logic

Cross-reference with agent confidence ratings — focus extra attention on areas agents rated low confidence.

### 3.3 Top-Up Work

Based on the code review, create a concrete issues list. For each:

| Issue Type | Action |
|-----------|--------|
| Trivial (typo, missing comment, formatting) | Fix directly in a top-up commit |
| Moderate (missing test, weak error handling) | Fix directly, note in LEARNINGS.md |
| Structural (refactor, design concern) | Present to human with recommendation before acting |

Top-up work is done in a worktree+PR if it touches code, or directly on main if doc-only. All top-up work MUST be completed before presenting the retrospective.

Record every top-up fix — the human needs to see what was changed and why.

### 3.4 Consolidate Learnings

- Merge all agent LEARNINGS.md entries (deduplicate, clarify, correct)
- Add orchestrator's own observations from the code review
- Update BACKLOG.md: close completed items, add new discoveries (with IDs)
- Update roadmap.md: mark increments as done with actuals (coverage)

### 3.5 Update Metrics

Update `docs/metrics.md` with data from this increment (see Metrics section below).

### 3.6 Update CLAUDE.md

- Current phase description
- Package layout (new packages, coverage numbers)
- Any new conventions, linter patterns, or test patterns discovered
- Documentation index (add/remove links as needed)

### 3.7 Git Cleanup (MANDATORY)

After all PRs are merged, the repo must be fully clean. This is not optional.

1. **Delete merged local branches**: `git branch -D <branch>` for every branch except `main`
2. **Delete merged remote branches**: `git push origin --delete <branch>` for every merged branch
3. **Remove worktrees**: `git worktree remove <path>` for every worktree except main
4. **Prune remote refs**: `git fetch --prune`
5. **Verify no stashes**: `git stash list` (must be empty)
6. **Remove coordination files**: Delete PLAN_LEFT.md or any other temporary coordination files
7. **Verify no open PRs**: `gh pr list --state open` (must be empty)
8. **Verify no orphaned directories**: Check for `onedrive-go-*` directories on disk

Run the DOD Cleanup Check (see CLAUDE.md) and confirm the output matches expectations before declaring the increment done.

---

## Phase 4: Increment Report & Retrospective

Present to the human in chat. This is the primary deliverable of the wrap-up process. It must be comprehensive — the human should understand everything that happened, everything that was decided, and everything that needs attention.

### 4.1 Executive Summary

Brief overview (3-5 sentences): what was built, how many agents, how many PRs, overall quality assessment.

### 4.2 Agent Scorecards

For EACH agent, present:

| Criterion | Rating (1-5) | Notes |
|-----------|:---:|-------|
| **Correctness** | | Error handling, edge cases, security |
| **Test quality** | | Coverage, assertions, edge case coverage |
| **Code style** | | Naming, structure, idiomatic Go |
| **Logging** | | Sufficient structured logging at all paths |
| **Documentation** | | Comments explain why, LEARNINGS.md updated |
| **Plan adherence** | | Followed plan vs. deviated |
| **Communication** | | Quality of final summary, decision log |

Include the agent's own confidence ratings and risk flags alongside your assessment.

### 4.3 Agent Observations (Raw + Synthesized)

Present the agents' actual observations:

> **Agent A said**: "[direct quote of their architecture/quality/process observations]"
> **Agent B said**: "[direct quote]"

Then synthesize: "Taken together, agents observed X. This suggests Y. I recommend Z."

### 4.4 Top-Up Work Report

For every fix the orchestrator made after agent review:

| File | What Changed | Why |
|------|-------------|-----|
| `files.go:42` | Added error wrapping | Agent returned bare error, inconsistent with codebase |
| `items_test.go` | Added edge case test | Agent's tests didn't cover empty input |

### 4.5 Orchestrator Self-Assessment

The orchestrator rates their own work on this increment:

| Criterion | Rating (1-5) | Notes |
|-----------|:---:|-------|
| **Plan quality** | | Were agents well-directed? Did they have what they needed? |
| **Parallelization** | | Was work split optimally? |
| **Agent prompts** | | Did agents understand expectations? |
| **Review thoroughness** | | Did I catch everything? |
| **Top-up quality** | | Were my fixes correct and complete? |
| **Communication** | | Did I keep the human informed? |

### 4.6 Code Changes Summary

For each file created or significantly modified in this increment:

- What the file does (one line)
- Key functions/types it exposes
- How it connects to the rest of the system

### 4.7 Retrospective

1. **What went well** — specific examples
2. **What went wrong** — specific examples with root causes
3. **What to change** — concrete actions with owners (not "we should try to...")
4. **Metrics comparison** — coverage change, deviation count

### 4.8 Re-Envisioning Check

Drawing on BOTH agent observations and orchestrator review:

- **Architecture**: Are package boundaries still right? Any coupling creep?
- **Roadmap**: Is the increment ordering still optimal? Should we re-prioritize?
- **Process**: Should we change how we plan/execute/review?
- **Test strategy**: Are we testing the right things at the right level?
- **Technical debt**: What debt did we accumulate? Is it acceptable?

### 4.9 Action Items

Every action item gets a BACKLOG ID:

| ID | Action | Priority | Owner |
|----|--------|----------|-------|
| B-0XX | [specific action] | P1/P2/P3 | Next increment / specific phase |

### 4.10 Changelog Entry

Human-readable summary suitable for release notes:

```
## [Increment X.Y] — [Date]
- Added: [feature]
- Changed: [modification]
- Fixed: [bug fix]
- Removed: [deprecated item]
```

---

## Metrics Tracking

Maintained in `docs/metrics.md`. Updated after every increment.

### Per-Increment Metrics

| Metric | Description |
|--------|-------------|
| **Agent count** | Number of subagents used |
| **Wave count** | Number of sequential waves |
| **PR count** | Number of PRs created and merged |
| **Coverage before** | Total test coverage % before increment |
| **Coverage after** | Total test coverage % after increment |
| **Top-up fix count** | Number of issues fixed in orchestrator review |
| **Agent deviation count** | Number of plan deviations across all agents |
| **CI failures** | Number of CI failures during the increment |
| **Wall-clock time** | Approximate total time from plan approval to retrospective |

### Per-Agent Metrics (within each increment)

| Metric | Description |
|--------|-------------|
| **Test count** | Number of test cases written |
| **Coverage** | Package-scoped coverage % |
| **Scorecard avg** | Average of 7 scorecard ratings (1-5) |
| **Confidence avg** | Average of agent's self-reported confidence ratings |
| **Deviation count** | Number of plan deviations |
| **LEARNINGS entries** | Number of learnings documented |

---

## Quick Reference Checklist

Use this checklist for every increment. Check off each item.

### Planning
- [ ] Intent confirmed with human
- [ ] Research summary presented (BACKLOG, LEARNINGS, existing code)
- [ ] Alternatives presented for significant decisions
- [ ] Non-goals section written
- [ ] Test strategy defined
- [ ] Parallelization analysis complete (waves, file conflicts, worktrees)
- [ ] CI impact analysis complete (if applicable — secret names, GitHub vars, minimal-environment check)
- [ ] Risk register written
- [ ] Definition of Ready met
- [ ] Plan approved by human
- [ ] Decision log written

### Execution
- [ ] Pre-launch briefing shown to human (agent summary, files, conflicts)
- [ ] Agent task prompts use FULL template (especially Wrap-Up Requirements)
- [ ] Agents launched with milestone update commitment
- [ ] Milestone updates provided during execution
- [ ] Non-trivial issues escalated to human during execution
- [ ] Worktrees cleaned up after each wave
- [ ] All PRs merged, main CI green
- [ ] Integration tests (`integration.yml`) pass on main after merge — WAIT for this before proceeding

### Post-Agent Review
- [ ] All agent final summaries read (all 9 points)
- [ ] All agent LEARNINGS.md entries read
- [ ] All agent decision logs reviewed
- [ ] Line-by-line code review performed on every new/modified file
- [ ] Top-up issues identified and FIXED (not deferred)
- [ ] LEARNINGS.md consolidated
- [ ] BACKLOG.md updated (closed items + new discoveries with IDs)
- [ ] roadmap.md updated with actuals
- [ ] Metrics updated in docs/metrics.md
- [ ] CLAUDE.md updated
- [ ] If CI-impacting: Key Vault secrets and GitHub variables updated
- [ ] If CI-impacting: `scripts/validate-ci-locally.sh` run successfully (or equivalent manual validation)
- [ ] If CI-impacting: test-strategy.md §6.1 and §10.x still match actual workflow YAML

### Increment Report & Retrospective
- [ ] Executive summary written
- [ ] Agent scorecards completed (all 7 criteria rated)
- [ ] Agent observations presented (raw quotes + synthesis)
- [ ] Top-up work report presented (file, change, why)
- [ ] Orchestrator self-assessment completed
- [ ] Code changes summarized
- [ ] Retrospective: well / wrong / change with specifics
- [ ] Metrics comparison: coverage change, deviation count
- [ ] Re-envisioning check performed (architecture, roadmap, process, tests, debt)
- [ ] Action items captured in BACKLOG with IDs
- [ ] Changelog entry written
- [ ] **Git cleanup**: merged branches deleted (local+remote), worktrees removed, refs pruned, no stashes, no open PRs, no orphaned dirs
