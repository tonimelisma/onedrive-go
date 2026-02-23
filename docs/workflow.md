# Increment Workflow

Core principle: The human has full visibility. Escalate anything non-trivial.

---

## 1. Planning

### 1.1 Intent Confirmation

Before investing in a plan, restate:
- What the increment will accomplish
- What is explicitly out of scope (non-goals)
- What success looks like

Get the human's confirmation before proceeding.

### 1.2 Research

Before proposing a plan:
- Read the relevant design docs (architecture.md, sync-algorithm.md, etc.)
- Check BACKLOG.md for related issues
- Review LEARNINGS.md for patterns and gotchas
- Read existing code that will be modified or extended
- Check roadmap.md for current phase status

**Show your research**: Present a brief summary of what you found — relevant BACKLOG items, applicable LEARNINGS, existing code patterns. The human sees your inputs, not just your conclusions.

### 1.3 Key Decisions

For every significant decision, present:
- At least two alternatives with trade-offs
- Your recommendation and why
- Risks of each approach

**Escalation rule**: Escalate anything non-trivial to the human. Only purely mechanical choices (variable names, import ordering) are autonomous.

### 1.4 CI Impact Analysis

If the increment changes anything affecting CI (token paths, secret naming, env vars, workflow YAML):

1. List every CI artifact affected (Key Vault secrets, GitHub variables, workflow steps, token paths)
2. Include infrastructure changes in the plan — don't defer to "after merge"
3. Ask: "What happens with no config file, no home dir, only env vars?" CI environments are stripped-down
4. Plan to run `scripts/validate-ci-locally.sh` before pushing
5. Verify test-strategy.md §6.1 still matches actual workflow YAML

---

## 2. Implementation

- Worktree + PR for code changes, direct push for doc-only changes
- Quality gates are automatic — see CLAUDE.md §Quality Gates
- Commit frequently with descriptive messages
- Document decisions in commit messages

---

## 3. Top-Up Review (Iterative)

After all planned commits for the increment:

1. Read every new/modified file line by line against the checklist below
2. For each issue found: fix it now, commit (with quality gates), return to step 1
3. If an issue cannot be fixed because it depends on future functionality, add it to BACKLOG.md with an ID
4. For every bug fix, gotcha, or subtle issue discovered during top-up: write a regression test that would catch it if it recurred
5. Repeat until a complete review pass finds zero actionable issues
6. Only then proceed to §4 Increment Close

"Good enough" is not a stopping criterion. The stopping criterion is: a complete review pass with no actionable findings.

### 3.1 Code Review Checklist

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

---

## 4. Increment Close

### 4.1 Lifecycle Gates

After top-up review is complete:

1. **Docs**: CLAUDE.md, BACKLOG.md, roadmap.md, LEARNINGS.md updated
2. **Git cleanup**: Run DOD Cleanup Check (see CLAUDE.md)
3. **CI verification**: Both `ci.yml` and `integration.yml` green
4. **CI infrastructure**: If CI-affecting changes, validate Key Vault + GitHub vars

### 4.2 Increment Report

Present to the human:
1. Executive summary (3-5 sentences)
2. Code changes summary (file, what it does, key types/functions)
3. Top-up work done (file, change, why)
4. Retrospective: what went well, what went wrong, what to change
5. Re-envisioning: "If starting today, would I build the same thing?"
6. Action items with BACKLOG IDs

### 4.3 Re-Envisioning Check

Step back from micro details. Evaluate architecture, package boundaries, API design, roadmap ordering, testing strategy. If something feels stale or constrained by earlier decisions, flag it. Don't just follow the roadmap — challenge it.

---

## 5. CI Operations Reference

### Key Vault Management

Use `az` CLI for Key Vault operations (creating, updating, verifying secrets). Use `gh variable set` for GitHub repository variables. The human only handles one-time Azure infrastructure and interactive browser-based flows.

```bash
# Download token
az keyvault secret download --vault-name <vault> --name <secret> --file <path>
# Upload token
az keyvault secret set --vault-name <vault> --name <secret> --file <path>
# List secrets
az keyvault secret list --vault-name <vault>
```

### Local CI Validation

Before pushing changes that affect `integration.yml`, validate locally by mirroring the workflow's token loading logic. See [test-strategy.md §6.1](design/test-strategy.md) for the full validation script.
