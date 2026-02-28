# onedrive-go — Project Hub

## Multi-Agent Environment

**Multiple AI agents work in this repo concurrently** Be aware:

- Always check git status before committing — another agent may have pushed changes
- Do not assume you are the only one modifying files; expect merge conflicts
- Coordinate through BACKLOG.md — check for in-progress items before claiming work
- If you encounter unexpected changes, investigate before overwriting

## Engineering Philosophy

**Always do the right thing. Engineering effort is free.**

- Prefer large, long-term solutions over quick fixes. Do big re-architectures early, not late.
- Always suggest the most ambitious correct approach — never settle for "good enough for now."
- Never treat current implementation as a reason to avoid change. We are never stuck in a local minimum.
- The fact that an architectural issue won't bite until later is not a reason to defer fixing it. Fix it now.
- Even tiny, minor issues deserve attention. The architecture should be extremely robust and full of defensive coding practices.
- Modules and packages can be rethought at a whim if a better design appears. No code is sacred.
- When you see a better way to structure something, propose it — regardless of how much code it touches.

## Ownership and Standards

**You own this repo.** A broken test is not "unrelated" — it's your responsibility.

- Never leave the repo in a broken state (build fails, tests fail, lint errors)
- Never introduce a regression and call it "pre-existing"
- If you touch a file, leave it better than you found it
- If something is broken, fix it — don't work around it

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

**Test patterns**:
- Never pass nil context — runtime panics not caught by compiler
- Table-driven tests where appropriate, with specific assertions (check values, not just "no error")
- Mandatory regression tests for every bug fix
- Scope verification to own package: `go test ./internal/graph/...` not `go test ./...`

**Code quality**: Functions do one thing, accept interfaces / return structs, sentinel errors with `%w` wrapping, no package-level mutable state.

## Code Review Checklist

Self-review every change against this checklist before considering work done.

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
- Tests are table-driven with specific assertions
- No speculative code (unused helpers, premature abstractions)
- Regression test written for every bug fix or issue noticed

## Branching Workflow

Code changes require a worktree (`claude --worktree <name>`), branch + PR targeting `main`. Doc-only changes push directly to `main` without a worktree.

Branch format: `<type>/<task-name>` (e.g., `feat/cli-auth`). Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.

Pre-commit hook: `.githooks/pre-commit` runs `golangci-lint run` before every commit.

## Definition of Done

Work is done in increments. After each increment, run through this entire checklist. Do not ask permission, do not skip any step. If something fails, fix and re-run from the top. **When complete, present this checklist to the human with pass/fail status for each item.**

1. [ ] **Format**: `gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w .`
2. [ ] **Build**: `go build ./...`
3. [ ] **Unit tests**: `go test -race -coverprofile=/tmp/cover.out ./...`
4. [ ] **Lint**: `golangci-lint run`
5. [ ] **Coverage**: `go tool cover -func=/tmp/cover.out | grep total` — never decrease
6. [ ] **Fast E2E**: `ONEDRIVE_TEST_DRIVE="personal:testitesti18@outlook.com" go test -tags=e2e -race -v -timeout=10m ./e2e/...`
7. [ ] **Self-review**: Review every changed file against the Code Review Checklist above
8. [ ] **Top-up loop**: Review the entire increment. Identify anything that could be improved — even minor issues (naming, logging, edge cases, defensive checks, test gaps, comments). Present the full list to the human. Then fix all of them automatically, re-run gates 1-6, and review again. Repeat until a full review pass finds zero issues of any size
9. [ ] **Docs updated**:
    - `CLAUDE.md` — update if structural changes (new packages, commands, deps)
    - `BACKLOG.md` — check before starting work, update when discovering or fixing issues
    - `LEARNINGS.md` — read for patterns and gotchas, add new institutional knowledge
    - `docs/roadmap.md` — check current phase status, update on completion
    - `docs/design/` — update relevant design docs if design changed
10. [ ] **Push and CI green**: Push branch, open PR, both `ci.yml` and `integration.yml` green. Merge with `./scripts/poll-and-merge.sh <pr_number>`
11. [ ] **Cleanup**: Clean `git status`. Remove the current worktree after merge. **NEVER delete other worktrees or branches — even if they appear stale.** Instead, report all other worktrees and branches to the human, including their last commit date (use `git log -1 --format='%ci' <branch>` for each). Let the human decide what to clean up
12. [ ] **Increment report**: Present to the human:
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

## Project Summary

**onedrive-go** — a fast, safe, and well-tested OneDrive CLI and sync client in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking. Targets Linux and macOS. MIT licensed. "Pragmatic Flat" architecture (7 active packages). See [docs/design/event-driven-rationale.md](docs/design/event-driven-rationale.md).

## Current Phase

**Phases 1-4 complete. Phase 5.0 (DAG-based concurrent execution), Phase 5.1 (watch mode observers + debounced buffer), Phase 5.2 (Engine.RunWatch() + continuous pipeline), Phase 5.3 (graceful shutdown + crash recovery + P2 hardening), and Phase 5.4 (drop ledger + universal transfer resume) complete. Ledger removed (~1,500 net lines deleted); crash recovery now relies on idempotent delta re-observation. File-based upload session store + `.partial` download resume shared between CLI and sync engine. Next: Phase 5.5 (pause/resume + SIGHUP config reload + final cleanup).** See [docs/roadmap.md](docs/roadmap.md).

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                      cmd/onedrive-go/ (Cobra CLI)                   │
│  ls, get, put, rm, mkdir, sync, status, conflicts, resolve, login  │
│  logout, whoami, verify                                            │
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
          │          │  ItemKey (leaf pkg)  │       │
          │          └──────────┬───────────┘       │
          │                     ▲                   │
          │          ┌──────────┴───────────┐       │
          │          │  internal/config/    │◄──────┘
          │          │  TOML + drives       │
          │          └─────────────────────-┘
          │
          │          ┌────────────────┐
          └─────────►│ pkg/           │
                     │ quickxorhash/  │
                     │ (vendored)     │
                     └────────────────┘
```

**Dependency direction**: `cmd/` -> `internal/*` -> `pkg/*`. No cycles. `internal/driveid/` and `internal/tokenfile/` are leaf packages. Both `graph/` and `config/` import `tokenfile/` for token file I/O. `internal/graph/` does NOT import `internal/config/` — callers pass token paths directly. See [docs/design/architecture.md](docs/design/architecture.md).

## Package Layout

- **`pkg/quickxorhash/`** — QuickXorHash algorithm (hash.Hash interface)
- **`internal/driveid/`** — Type-safe drive identity: ID, CanonicalID, ItemKey
- **`internal/tokenfile/`** — Token file format + I/O (leaf package: stdlib + oauth2 only)
- **`internal/config/`** — TOML config, drive sections, XDG paths, four-layer override chain
- **`internal/graph/`** — Graph API client: auth, retry, items CRUD, delta, transfers
- **`internal/sync/`** — Event-driven sync: types, baseline, observers, buffer, planner, executor, tracker, workers, session_store, engine, verify
- **Root package** — Cobra CLI: login, logout, whoami, status, drive (list/add/remove/search), ls, get, put, rm, mkdir, stat, sync, conflicts, resolve, verify
- **`e2e/`** — E2E test suite against live OneDrive

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
| [docs/design/event-driven-rationale.md](docs/design/event-driven-rationale.md) | Option E architectural decision record |
| [docs/design/concurrent-execution.md](docs/design/concurrent-execution.md) | Execution architecture |
| [docs/design/legacy-sequential-architecture.md](docs/design/legacy-sequential-architecture.md) | Old 9-phase architecture reference (migration guide) |
| [docs/archive/](docs/archive/) | Historical docs |
| [docs/tier1-research/](docs/tier1-research/) | 16 Tier 1 research docs |

## Self-Maintenance Rule

**This CLAUDE.md is the single source of truth for working on this codebase.** After major changes (new packages, CLI commands, docs, dependencies, or architectural shifts), update this file. Keep it concise — link to detailed docs rather than duplicating content. Every linked doc must exist; remove stale links.

## E2E Testing

E2E tests run against a live OneDrive account (`testitesti18@outlook.com`). A valid OAuth token already exists on the local dev machine. The token auto-refreshes on use. For full E2E details (credentials, CI setup, bootstrapping, tiers), see [docs/design/test-strategy.md §6](docs/design/test-strategy.md#6-e2e-test-strategy).
