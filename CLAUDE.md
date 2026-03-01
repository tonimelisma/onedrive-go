# onedrive-go — Project Hub

## Project Summary

**onedrive-go** — a fast, safe, and well-tested OneDrive CLI and sync client in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking. Targets Linux and macOS. MIT licensed. "Pragmatic Flat" architecture (7 active packages). See [docs/design/event-driven-rationale.md](docs/design/event-driven-rationale.md).

## Current Phase

**Phases 1-5.6 complete. Next: Phase 6 (multi-drive orchestration — sub-increments 6.0a-6.0d + shared content sync 6.3-6.4b, see MULTIDRIVE.md §11), Phase 7 (CLI completeness), Phase 8 (WebSocket), Phase 9 (ops hardening), Phase 10 (filtering), Phase 11 (packaging), Phase 12 (post-release).** See [docs/roadmap.md](docs/roadmap.md).

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                      cmd/onedrive-go/ (Cobra CLI)                   │
│  ls, get, put, rm, mkdir, sync, pause, resume, status, conflicts,  │
│  resolve, login, logout, whoami, verify                            │
└──────────┬───────────────────────────────────────────────────────────┘
           │ file ops (direct API)                 │ sync operations
           │                                       │
           ▼                                       ▼
┌──────────────────────┐             ┌──────────────────────────────┐
│   internal/graph/    │◄────────────│       internal/sync/         │
│   Graph API client   │             │  engine, observers, buffer,  │
│   + quirk handling   │             │  planner, executor, baseline │
│   + auth             │             │  tracker, workers, sessions  │
└─────────┬────────────┘             └──────────────┬───────────────┘
          │                                         │
          ├─────────┐                               │
          │         ▼                               │
          │  ┌──────────────────────┐               │
          │  │ internal/tokenfile/  │               │
          │  │ token I/O (leaf pkg) │               │
          │  └──────────────────────┘               │
          │         ▲                               │
          │          ┌──────────────────────┐       │
          ├─────────►│  internal/driveid/   │◄──────┤
          │          │  ID, CanonicalID,    │       │
          │          │  ItemKey (pure ID)   │       │
          │          └──────────┬───────────┘       │
          │                     ▲                   │
          │          ┌──────────┴───────────┐       │
          │          │  internal/config/    │◄──────┘
          │          │  TOML + drives +     │
          │          │  TokenCanonicalID()  │
          │          └─────────────────────-┘
          │
          │          ┌────────────────┐
          └─────────►│ pkg/           │
                     │ quickxorhash/  │
                     │ (vendored)     │
                     └────────────────┘
```

**Dependency direction**: `cmd/` -> `internal/*` -> `pkg/*`. No cycles. `internal/driveid/` and `internal/tokenfile/` are leaf packages. `driveid` is pure identity (no business logic); `config` imports `driveid` and provides `TokenCanonicalID()` for token resolution. Both `graph/` and `config/` import `tokenfile/` for token file I/O. `internal/graph/` does NOT import `internal/config/` — callers pass token paths directly. See [docs/design/architecture.md](docs/design/architecture.md).

**Packages:**
- **`pkg/quickxorhash/`** — QuickXorHash algorithm (hash.Hash interface)
- **`internal/driveid/`** — Type-safe drive identity: ID, CanonicalID, ItemKey. Four drive types (personal, business, sharepoint, shared). Pure identity — no business logic.
- **`internal/tokenfile/`** — Token file format + I/O (leaf package: stdlib + oauth2 only)
- **`internal/config/`** — TOML config, drive sections, XDG paths, four-layer override chain, token resolution (`TokenCanonicalID()`)
- **`internal/graph/`** — Graph API client: auth, retry, items CRUD, delta, transfers
- **`internal/sync/`** — Event-driven sync: types, baseline, observers, buffer, planner, executor, transfer_manager, tracker, workers, session_store, engine, verify
- **Root package** — Cobra CLI: login, logout, whoami, status, drive (list/add/remove/search), ls, get, put, rm, mkdir, stat, sync, pause, resume, conflicts, resolve, verify
- **`e2e/`** — E2E test suite against live OneDrive

## Engineering Philosophy

**Always do the right thing. Engineering effort is free.**

- Prefer large, long-term solutions over quick fixes. Do big re-architectures early, not late.
- Always suggest the most ambitious correct approach — never settle for "good enough for now."
- Never treat current implementation as a reason to avoid change. We are never stuck in a local minimum.
- The fact that an architectural issue won't bite until later is not a reason to defer fixing it. Fix it now.
- Even tiny, minor issues deserve attention. The architecture should be extremely robust and full of defensive coding practices.
- Modules and packages can be rethought at a whim if a better design appears. No code is sacred.
- When you see a better way to structure something, propose it — regardless of how much code it touches.
- Every behavior change is developed test-first. Write the test, watch it fail, write the minimum code to pass, then refactor. No exceptions.

**You own this repo.** A broken test is not "unrelated" — it's your responsibility.

- Never leave the repo in a broken state (build fails, tests fail, lint errors)
- Never introduce a regression and call it "pre-existing"
- If you touch a file, leave it better than you found it
- If something is broken, fix it — don't work around it

## Multi-Agent Environment

**Multiple AI agents work in this repo concurrently** Be aware:

- Always check git status before committing — another agent may have pushed changes
- Do not assume you are the only one modifying files; expect merge conflicts
- Coordinate through BACKLOG.md — check for in-progress items before claiming work
- If you encounter unexpected changes, investigate before overwriting

## Coding Conventions

- Go 1.23+, module path `github.com/tonimelisma/onedrive-go`
- Format: `gofumpt` + `goimports -local github.com/tonimelisma/onedrive-go`
- Follow `.golangci.yml` (140 char lines, complexity limits)
- US English (`canceled`, `marshaling`), three-group imports (stdlib / third-party / local)
- Comments explain **why**, not **what**

**Logging** (`log/slog` with structured fields):
- **Debug**: HTTP request/response, token acquisition, file read/write
- **Info**: Lifecycle events — login/logout, sync start/complete, config load
- **Warn**: Degraded but recoverable — retries, expired tokens, fallbacks
- **Error**: Terminal failures — request failed after all retries, unrecoverable auth
- Minimum per code path: function entry with key params, state transitions, error paths, external calls. Never log secrets.

**Test style**:
- Never pass nil context — runtime panics not caught by compiler
- Table-driven tests where appropriate, with specific assertions (check values, not just "no error")
- Scope verification to own package: `go test ./internal/graph/...` not `go test ./...`

**E2E tests** run against a live OneDrive account (`testitesti18@outlook.com`). A valid OAuth token already exists on the local dev machine. The token auto-refreshes on use. For full E2E details (credentials, CI setup, bootstrapping, tiers), see [docs/design/test-strategy.md §6](docs/design/test-strategy.md#6-e2e-test-strategy).

**Code quality**: Functions do one thing, accept interfaces / return structs, sentinel errors with `%w` wrapping, no package-level mutable state.

## Development Process

Work is done in increments. Follow this process from start to finish. Do not ask permission, do not skip any step.

### Step 1: Claim work

1. Check `docs/roadmap.md` for the next increment.
2. Evaluate the codebase to determine if any foundational improvements are needed before starting the increment.
3. Check `BACKLOG.md` for items that should be addressed before or alongside the increment.
4. Mark the work as in-progress in BACKLOG.md.

### Step 2: Read context

1. Read all design docs relevant to the increment (see Documentation Index below).
2. Read `LEARNINGS.md` for patterns and gotchas.
3. Read the existing code you will be modifying or extending.

### Step 3: Set up worktree

1. Create a worktree: `claude --worktree <name>`.
2. Create a branch with the naming convention: `<type>/<task-name>` (e.g., `feat/cli-auth`). Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.
3. Doc-only changes push directly to `main` without a worktree.

Pre-commit hook: `.githooks/pre-commit` runs `golangci-lint run` before every commit.

### Step 4: Develop with TDD

All development follows strict red/green/refactor TDD. For every behavior change — new feature, bug fix, refactoring, edge case:

1. **Red**: Write a failing test that specifies the desired behavior. Run it. Confirm it fails for the right reason.
2. **Green**: Write the minimum production code to make the test pass. Nothing more.
3. **Refactor**: Clean up the production code and test code while keeping all tests green.

Mandatory regression tests for every bug fix. Every new exported function, every behavior change, and every bug fix must have a test that was written first and seen to fail before the implementation made it pass.

### Step 5: Code review checklist

Self-review every change against this checklist before proceeding to the Definition of Done.

**Correctness:**
- Error paths handled correctly (wrapped with `fmt.Errorf("context: %w", err)`)
- Edge cases covered (nil, empty, zero values, boundary conditions)
- Resource cleanup (defer Close, temp file removal)

**Consistency:**
- Naming follows codebase conventions (camelCase, verb-first functions)
- Import grouping (stdlib / third-party / local, separated by blank lines)
- Error message style matches codebase (lowercase, no punctuation)
- Function signatures follow Go conventions (context first, error last)

**Quality:**
- Functions focused (<100 lines), no unnecessary duplication
- Comments explain "why" not "how", logging at entry/transitions/errors/external calls
- Tests are table-driven with specific assertions, developed test-first
- No speculative code (unused helpers, premature abstractions)

### Step 6: Definition of Done

After each increment, run through this entire checklist. If something fails, fix and re-run from the top. **When complete, present this checklist to the human with pass/fail status for each item.**

1. [ ] **Format**: `gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w .`
2. [ ] **Build**: `go build ./...`
3. [ ] **Unit tests**: `go test -race -coverprofile=/tmp/cover.out ./...`
4. [ ] **Lint**: `golangci-lint run`
5. [ ] **Coverage**: `go tool cover -func=/tmp/cover.out | grep total` — never decrease
6. [ ] **Fast E2E**: `ONEDRIVE_TEST_DRIVE="personal:testitesti18@outlook.com" go test -tags=e2e -race -v -timeout=10m ./e2e/...`
7. [ ] **Top-up loop**: Review the entire increment. Identify anything that could be improved — even minor issues (naming, logging, edge cases, defensive checks, test gaps, comments). Present the full list to the human. Then fix all of them automatically, re-run gates 1-6, and review again. Repeat until a full review pass finds zero issues of any size
8. [ ] **Docs updated**:
    - `CLAUDE.md` — update if structural changes (new packages, commands, deps)
    - `BACKLOG.md` — check before starting work, update when discovering or fixing issues
    - `LEARNINGS.md` — read for patterns and gotchas, add new institutional knowledge
    - `docs/roadmap.md` — check current phase status, update on completion
    - `docs/design/` — update relevant design docs if design changed
9. [ ] **Push and CI green**: Push branch, open PR, both `ci.yml` and `integration.yml` green. Merge with `./scripts/poll-and-merge.sh <pr_number>`
10. [ ] **Cleanup**: Clean `git status`. Remove the current worktree after merge. **NEVER delete other worktrees or branches — even if they appear stale.** Instead, report all other worktrees and branches to the human, including their last commit date (use `git log -1 --format='%ci' <branch>` for each). Let the human decide what to clean up
11. [ ] **Increment report**: Present to the human:
    - **Process changes**: What you would do differently next time in how the work was planned or executed
    - **Top-up recommendations**: Any remaining codebase improvements you'd make
    - **Architecture re-envisioning**: If you were starting from a blank slate, would you build it the same way? Propose any dramatic architectural changes if a better design is apparent
    - **Unfixed items**: Anything you were unable to address in this increment (with BACKLOG IDs for deferred items)

Quick command (gates 1-6):
```bash
gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w . && go build ./... && go test -race -coverprofile=/tmp/cover.out ./... && golangci-lint run && go tool cover -func=/tmp/cover.out | grep total && ONEDRIVE_TEST_DRIVE="personal:testitesti18@outlook.com" go test -tags=e2e -race -v -timeout=10m ./e2e/... && echo "ALL GATES PASS"
```

Cleanup check:
```bash
echo "=== Branches ===" && git branch && echo "=== Remote ===" && git branch -r && echo "=== Stashes ===" && git stash list && echo "=== Worktrees ===" && git worktree list && echo "=== Open PRs ===" && gh pr list --state open && echo "=== Status ===" && git status
```

## Documentation Index

| Document | Description |
|----------|-------------|
| [docs/roadmap.md](docs/roadmap.md) | Implementation phases and increments |
| [BACKLOG.md](BACKLOG.md) | Issue/task tracker |
| [LEARNINGS.md](LEARNINGS.md) | Institutional knowledge base |
| [docs/design/prd.md](docs/design/prd.md) | Product requirements and scope |
| [docs/design/architecture.md](docs/design/architecture.md) | System architecture |
| [docs/design/data-model.md](docs/design/data-model.md) | Database schema and state model |
| [docs/design/sync-algorithm.md](docs/design/sync-algorithm.md) | Sync algorithm specification |
| [docs/design/configuration.md](docs/design/configuration.md) | Config options spec |
| [docs/design/test-strategy.md](docs/design/test-strategy.md) | Testing approach |
| [docs/design/sharepoint-enrichment.md](docs/design/sharepoint-enrichment.md) | SharePoint enrichment design |
| [docs/design/decisions.md](docs/design/decisions.md) | Architectural and design decisions |
| [docs/design/accounts.md](docs/design/accounts.md) | Account and drive system design |
| [docs/design/MULTIDRIVE.md](docs/design/MULTIDRIVE.md) | Multi-drive architecture (shared drives, display_name, vault exclusion) |
| [docs/design/event-driven-rationale.md](docs/design/event-driven-rationale.md) | Option E architectural decision record |
| [docs/design/concurrent-execution.md](docs/design/concurrent-execution.md) | Execution architecture |
| [docs/design/legacy-sequential-architecture.md](docs/design/legacy-sequential-architecture.md) | Old 9-phase architecture reference (migration guide) |
| [docs/archive/](docs/archive/) | Historical docs |
| [docs/tier1-research/](docs/tier1-research/) | 16 Tier 1 research docs |

## Self-Maintenance Rule

**This CLAUDE.md is the single source of truth for working on this codebase.** After major changes (new packages, CLI commands, docs, dependencies, or architectural shifts), update this file. Keep it concise — link to detailed docs rather than duplicating content. Every linked doc must exist; remove stale links.
