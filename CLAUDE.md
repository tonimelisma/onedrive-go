# onedrive-go — Project Hub

## Project Summary

**onedrive-go** — a fast, safe, and well-tested OneDrive CLI and sync client in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking. Targets Linux and macOS. MIT licensed. "Pragmatic Flat" architecture (6 active packages). See [docs/design/event-driven-rationale.md](docs/design/event-driven-rationale.md).

## Current Phase

**Phases 1-3.5 complete. Phase 4v2 Increments 0-6 complete. Next: 4v2.7-4v2.8.** All development on `main` branch. See [docs/roadmap.md](docs/roadmap.md).

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
│   + auth             │             │  filter, transfer, conflict  │
└─────────┬────────────┘             └──────────────┬───────────────┘
          │                                         │
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

**Dependency direction**: `cmd/` -> `internal/*` -> `pkg/*`. No cycles. `internal/driveid/` is a leaf package (stdlib only). `internal/graph/` does NOT import `internal/config/` — callers pass token paths directly. See [docs/design/architecture.md](docs/design/architecture.md).

## Package Layout

- **`pkg/quickxorhash/`** — QuickXorHash algorithm (hash.Hash interface)
- **`internal/driveid/`** — Type-safe drive identity: ID, CanonicalID, ItemKey — 98.3% coverage
- **`internal/config/`** — TOML config, drive sections, XDG paths, four-layer override chain — 94.6% coverage
- **`internal/graph/`** — Graph API client: auth, retry, items CRUD, delta, transfers — 93.0% coverage
- **`internal/sync/`** — Event-driven sync: types, baseline, observers, buffer, planner, executor — 90.3% coverage
- **Root package** — Cobra CLI: login, logout, whoami, status, drive, ls, get, put, rm, mkdir, stat, sync
- **`e2e/`** — E2E test suite against live OneDrive

## Documentation Index

| Document | Description |
|----------|-------------|
| [docs/roadmap.md](docs/roadmap.md) | Implementation phases and increments |
| [docs/workflow.md](docs/workflow.md) | Increment workflow: planning, review, close |
| [docs/conventions.md](docs/conventions.md) | Coding conventions: logging, linting, testing |
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
| [BACKLOG.md](BACKLOG.md) | Issue/task tracker |
| [LEARNINGS.md](LEARNINGS.md) | Institutional knowledge base |
| [docs/archive/](docs/archive/) | Historical docs |
| [docs/tier1-research/](docs/tier1-research/) | 16 Tier 1 research docs |

## Ownership and Standards

**You own this repo.** A broken test is not "unrelated" — it's your responsibility. This codebase is always in tip-top shape, always ready to deploy, and you are the one in charge of that.

- Never leave the repo in a broken state (build fails, tests fail, lint errors)
- Never introduce a regression and call it "pre-existing"
- If you touch a file, leave it better than you found it
- If something is broken, fix it — don't work around it

## Quality Gates (AUTOMATIC)

MANDATORY: Run ALL gates automatically after every code change, before every commit. Do not ask permission. Do not pause between gates. Do not skip any gate. If a gate fails, fix the issue and re-run. Never commit with a failing gate.

### Gate sequence

1. **Format**: `gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w .`
2. **Build**: `go build ./...`
3. **Unit tests**: `go test -race -coverprofile=/tmp/cover.out ./...`
4. **E2E tests**: `ONEDRIVE_TEST_DRIVE="personal:testitesti18@outlook.com" go test -tags=e2e -race -v -timeout=15m ./e2e/...`
5. **Lint**: `golangci-lint run`
6. **Coverage**: `go tool cover -func=/tmp/cover.out | grep total` — never decrease
7. **Review changes silently**: sufficient logging? Comments explain why? Fix issues, do not ask.

### Quick command (gates 1-6)

```bash
gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w . && go build ./... && go test -race -coverprofile=/tmp/cover.out ./... && ONEDRIVE_TEST_DRIVE="personal:testitesti18@outlook.com" go test -tags=e2e -race -v -timeout=15m ./e2e/... && golangci-lint run && go tool cover -func=/tmp/cover.out | grep total && echo "ALL GATES PASS"
```

### Cleanup check (after increment)

```bash
echo "=== Branches ===" && git branch && echo "=== Remote ===" && git branch -r && echo "=== Worktrees ===" && git worktree list && echo "=== Stashes ===" && git stash list && echo "=== Open PRs ===" && gh pr list --state open && echo "=== Status ===" && git status
```

Expected: `main` local branch only. Only `origin/main` remote. One worktree. No stashes, no open PRs, clean status.

## Per-Increment Lifecycle

### Top-Up Review (iterative)

After all planned commits for the increment:

1. Read every new/modified file line by line against the code review checklist in [docs/workflow.md](docs/workflow.md)
2. For each issue found: fix it now, commit (with quality gates), return to step 1
3. If an issue cannot be fixed because it depends on future functionality, add it to BACKLOG.md with an ID
4. For every bug fix, gotcha, or subtle issue discovered during top-up: write a regression test that would catch it if it recurred
5. Repeat until a complete review pass finds zero actionable issues
6. Only then proceed to the lifecycle gates below

"Good enough" is not a stopping criterion. The stopping criterion is: a complete review pass with no actionable findings.

### Lifecycle gates (after top-up is complete)

1. Docs: CLAUDE.md, BACKLOG.md, roadmap.md, LEARNINGS.md updated
2. Git cleanup: run cleanup check (command above)
3. CI verification: both `ci.yml` and `integration.yml` green
4. CI infrastructure: if CI-affecting changes, validate Key Vault + GitHub vars
5. Retrospective + re-envisioning (see [docs/workflow.md](docs/workflow.md))

## Coding Conventions

- Go 1.23+, module path `github.com/tonimelisma/onedrive-go`
- Format: `gofumpt` + `goimports -local github.com/tonimelisma/onedrive-go`
- Follow `.golangci.yml` (140 char lines, complexity limits)
- US English (`canceled`, `marshaling`), three-group imports
- Comments explain **why**, not **what**. Logging via `log/slog` with structured fields
- See [docs/conventions.md](docs/conventions.md) for full logging standard, linter patterns, test patterns

## Tracking Protocol

- **`BACKLOG.md`**: Check for open issues before starting work. Update when discovering or fixing issues.
- **`docs/roadmap.md`**: Check for current phase status. Update status markers on increment completion.
- **`LEARNINGS.md`**: Read for patterns and gotchas. Add new institutional knowledge when discovered.

## Key Decisions

See [docs/design/decisions.md](docs/design/decisions.md). Highlights: 6-package "Pragmatic Flat" architecture, event-driven sync (Option E), delete-first strategy, CLI-first development order, TOML config, SQLite sync DB, MIT license, Linux + macOS.

## Code Quality Standards

Idiomatic Go: functions do one thing, accept interfaces / return structs (consumer-defined), sentinel errors with `%w` wrapping, no package-level mutable state, table-driven tests, mandatory regression tests for every bug fix.

## CI Protocol

CI must never be broken. Work is not done until CI passes.

- **Code changes require PRs.** Create a branch, push, open a PR, let CI run.
- **Doc-only changes push directly to main.** No PR needed.
- **Workflow**: `ci.yml` runs build + test (race detector) + lint on every push/PR
- **Integration tests**: `integration.yml` runs against real Graph API on push to main + nightly
- **E2E tests**: Same workflow, builds binary, exercises full CLI round-trip
- **Merge**: `./scripts/poll-and-merge.sh <pr_number>` — polls checks, merges when green
- **Pre-commit hook**: `.githooks/pre-commit` runs `golangci-lint run` before every commit
- If CI fails, fix it immediately. See [docs/workflow.md §5](docs/workflow.md) for Key Vault and local CI validation.

## Worktree Workflow

Source code changes require a worktree + PR targeting `main`. Doc-only changes push directly to `main`.

Branch format: `<type>/<task-name>` (e.g., `feat/cli-auth`). Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.

## Self-Maintenance Rule

**This CLAUDE.md is the single source of truth for working on this codebase.** After major changes (new packages, CLI commands, docs, dependencies, or architectural shifts), update this file. Keep it concise — link to detailed docs rather than duplicating content. Every linked doc must exist; remove stale links. When CLAUDE.md exceeds 200 lines, move reference content to linked docs.
