# onedrive-go — Project Hub

## Project Summary

**onedrive-go** — a fast, safe, and well-tested OneDrive CLI and sync client in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking. Targets Linux and macOS. MIT licensed.

Currently: Working CLI OneDrive client with auth + file ops. "Pragmatic Flat" architecture (5 packages). Next: Phase 2.3 (E2E edge cases) or Phase 3 (config).

## Current Phase

**Phase 1 complete. Phase 2 partially complete.** All 8 Phase 1 increments done (Graph API layer + CLI auth + file ops). Phase 2.1 (CI scaffold) and 2.2 (E2E round-trip tests) done. Users can `login`, `logout`, `whoami`, `ls`, `get`, `put`, `rm`, `mkdir`, `stat`. CI runs unit tests, integration tests against real Graph API, and E2E tests. See [docs/roadmap.md](docs/roadmap.md).

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                      cmd/onedrive-go/ (Cobra CLI)                   │
│  ls, get, put, rm, mkdir, sync, status, conflicts, resolve, login  │
│  logout, config, migrate, verify                                   │
└──────────┬───────────────────────────────────────┬──────────────────┘
           │ file ops (direct API)                 │ sync operations
           │                                       │
           ▼                                       ▼
┌──────────────────────┐             ┌──────────────────────────────┐
│   internal/graph/    │◄────────────│       internal/sync/         │
│   Graph API client   │             │  engine, db, scanner, delta, │
│   + quirk handling   │             │  reconciler, executor,       │
│   + auth             │             │  conflict, filter, transfer  │
└──────────────────────┘             └──────────────┬───────────────┘
                                                    │
                                     ┌──────────────┴───────────────┐
                                     │       internal/config/       │
                                     │       TOML + profiles        │
                                     └──────────────────────────────┘

           ┌────────────────┐
           │ pkg/           │
           │ quickxorhash/  │
           │ (vendored)     │
           └────────────────┘
```

**Dependency direction**: `cmd/` -> `internal/*` -> `pkg/*`. No cycles. `cmd/` uses `graph/` directly for CLI file operations and `sync/` for sync operations. `sync/` depends on `graph/` for API access and `config/` for settings. `pkg/quickxorhash/` is a leaf utility used by `sync/` and `graph/`. `internal/graph/` handles all API quirks internally -- callers never see raw API data.

## Package Layout

### Active packages
- **`pkg/quickxorhash/`** — QuickXorHash algorithm (hash.Hash interface) — complete
- **`internal/config/`** — TOML configuration with profiles, validation, XDG paths — existing, will be updated in Phase 3
- **`internal/graph/`** — Graph API client: HTTP transport, auth, retry, rate limiting, items CRUD, delta+normalization, download/upload transfers, drives/user, path-based item resolution — feature-complete (92.3% coverage)
- **Root package** — Cobra CLI: login, logout, whoami, ls, get, put, rm, mkdir, stat (root.go, auth.go, files.go, format.go)
- **`e2e/`** — E2E test suite (`//go:build e2e`): builds binary, exercises full round-trip against live OneDrive

### Future phases
- **`internal/sync/`** — Sync engine: state DB, delta processing, local scanner, reconciler, executor, conflict handler, filter, transfer pipeline (Phase 4)

## Documentation Index

| Document | Description |
|----------|-------------|
| [docs/roadmap.md](docs/roadmap.md) | Implementation phases and increments |
| [docs/design/prd.md](docs/design/prd.md) | Product requirements and scope |
| [docs/design/architecture.md](docs/design/architecture.md) | System architecture |
| [docs/design/data-model.md](docs/design/data-model.md) | Database schema and state model |
| [docs/design/sync-algorithm.md](docs/design/sync-algorithm.md) | Sync algorithm specification |
| [docs/design/configuration.md](docs/design/configuration.md) | Config options spec |
| [docs/design/test-strategy.md](docs/design/test-strategy.md) | Testing approach |
| [docs/design/sharepoint-enrichment.md](docs/design/sharepoint-enrichment.md) | SharePoint enrichment design (per-side hash baselines) |
| [docs/design/decisions.md](docs/design/decisions.md) | Architectural and design decisions |
| [BACKLOG.md](BACKLOG.md) | Issue/task tracker (bugs, improvements, research) |
| [LEARNINGS.md](LEARNINGS.md) | Institutional knowledge base (patterns, gotchas) |
| [docs/archive/](docs/archive/) | Historical learnings and backlog from earlier phases |
| [docs/tier1-research/](docs/tier1-research/) | 16 Tier 1 research docs (API bugs, edge cases, reference impl analysis) |
| [docs/parallel-agents.md](docs/parallel-agents.md) | Parallel agent worktree workflow guide |

## Subagent Protocol

When work is delegated to subagents (via the Task tool), each subagent **must** document the following in `LEARNINGS.md` before wrapping up:

1. **Pivots**: Any deviation from the plan — what changed and why
2. **Issues found**: Bugs, code smells, architectural concerns, or surprising behavior
3. **Weird observations**: Unexpected API behavior, linter gotchas, edge cases discovered
4. **Suggested improvements**: Top-up work, coverage gaps, or follow-up items
5. **Cross-package concerns**: Anything that affects other packages or future work

This ensures institutional knowledge is captured even when agent contexts are discarded after completion. The main orchestrator will review and consolidate entries.

## Increment Wrap-Up Protocol

After subagents complete their work, before closing an increment:

1. **Review subagent output**: Read their code like a code reviewer. Check for consistency, missed edge cases, naming, test quality.
2. **Top-up work**: Fix anything that doesn't meet standards. Don't leave it for later.
3. **Propose improvements**: Present a concrete list of fixes/improvements to the user before wrapping up.
4. **Propose new safeguards**: Based on any mistakes or near-misses, propose new lint rules, tests, or process changes for continuous improvement.
5. **Update all documents**: CLAUDE.md, LEARNINGS.md, BACKLOG.md, roadmap.md — remove stale content, add new patterns. If something is clearly stale, update it. If you think something should be added or removed, propose it to the user.
6. **Retrospective in chat**: Always present the retrospective **in the chat** for the user to read and discuss. Cover: what went well, what went wrong, what to change. Suggest concrete actions. Then capture actionable items in LEARNINGS.md. Never skip this — the user wants to review and discuss it interactively.

## Planning Protocol

When entering plan mode for an increment:

1. **Ask questions first.** Present the user with key decisions, trade-offs, and alternatives. The user wants to make informed decisions — don't assume.
2. **Research before proposing.** Read the relevant design docs, check BACKLOG.md for related issues, review LEARNINGS.md for patterns.
3. **Define test strategy upfront.** How will this code be tested? What mocking approach? What fixtures? Decide before writing code.

## Context Preservation

Agent contexts expire. Mitigate knowledge loss:

- Commit frequently with descriptive messages so `git log` serves as a secondary knowledge trail
- Document decisions in commit messages (the "why", not just the "what")
- Subagents must write to LEARNINGS.md before finishing (see Subagent Protocol)
- After each increment, consolidate subagent learnings into LEARNINGS.md and CLAUDE.md

## Ownership and Standards

**You own this repo.** A broken test is not "unrelated" — it's your responsibility. This codebase is always in tip-top shape, always ready to deploy, and you are the one in charge of that.

- Never leave the repo in a broken state (build fails, tests fail, lint errors)
- Never introduce a regression and call it "pre-existing"
- If you touch a file, leave it better than you found it
- If something is broken, fix it — don't work around it

## Definition of Done (DOD)

Every change must pass ALL gates before committing:

1. **Build**: `go build ./...` — zero errors
2. **Unit tests**: `go test ./...` — all pass
3. **Lint**: `golangci-lint run` — zero issues
4. **Format**: `gofumpt` and `goimports -local github.com/tonimelisma/onedrive-go` applied
5. **Coverage**: New/modified code must have tests. Never decrease coverage.
6. **Logging review**: Review new/modified code for sufficient logging. Every public function entry, every state transition, every error path must have a log line. See Logging Standard below.
7. **Comment review**: Review new/modified code for sufficient comments explaining *why*. See Comment Convention below.
8. **Docs**: CLAUDE.md documentation index is current. All doc links are valid.
9. **Git**: Working tree is clean after commit. No uncommitted changes left behind.
10. **Retrospective**: After each increment, conduct a brief retro covering: what went well, what could be improved, and what to change going forward. Capture actionable improvements in `LEARNINGS.md`. This applies to the increment as a whole, not to every individual commit.
11. **Re-envisioning check**: After each increment, step back and consider the project from a blank slate. Ask: "If I were starting this today, knowing everything I know now, would I build the same thing?" Evaluate architecture, package boundaries, API design, roadmap ordering, and testing strategy. If something feels stale or constrained by earlier decisions, flag it. Don't just follow the roadmap — challenge it. Propose concrete changes if warranted, or explicitly confirm the current direction is still correct. This check prevents path dependency from accumulating across increments.

### DOD Quick Check
```bash
go build ./... && go test -race -coverprofile=/tmp/cover.out ./... && golangci-lint run && go tool cover -func=/tmp/cover.out | grep total && echo "ALL GATES PASS"
```

## Coding Conventions

- Go 1.23+, module path `github.com/tonimelisma/onedrive-go`
- Format with `gofumpt` and `goimports -local github.com/tonimelisma/onedrive-go`
- Follow `.golangci.yml` linter rules (140 char lines, complexity limits, etc.)
- US English spelling throughout (`canceled`, `marshaling`)
- Error returns must be checked (except where `// nolint:errcheck` is justified)
- Three-group imports: stdlib, third-party, then local (`github.com/tonimelisma/...`)

## Comment Convention

Comments explain **why**, not **what**. Good: intent, constraints, architectural boundaries, gotcha warnings, external references. Bad: restating code, temporary project state, obvious descriptions.

## Logging Standard

All code uses `log/slog` with structured key-value fields. Logging is a first-class concern — not an afterthought. Every function that does I/O, state changes, or non-trivial processing must log enough to debug a CI failure or user bug report without adding instrumentation later.

**Log levels**:
- **Debug**: Every HTTP request/response, token acquisition, file read/write. Off by default for users.
- **Info**: Lifecycle events — login/logout, token load/refresh/save, sync start/complete, config load.
- **Warn**: Degraded but recoverable — retries, expired tokens, failed persistence with fallback.
- **Error**: Terminal failures — request failed after all retries, unrecoverable auth failure.

**Minimum logging per code path**: public function entry with key parameters, every state transition, every error path, every external call (method, URL, status, request-id), every security event (token acquire/refresh/save/delete). Never log token values or secrets (architecture.md §9.2).

**Testing**: Integration tests use a Debug-level `testLogger(t)` writing to `t.Log`, so all activity appears in CI output.

## Linter Patterns

Common golangci-lint rules that require specific patterns:

- **mnd**: Every number needs a named constant; tests are exempt
- **funlen**: Max 100 lines / 50 statements — decompose into small helpers
- **depguard**: Update `.golangci.yml` when adding new external dependencies
- **gochecknoinits**: No `init()` functions allowed. Use constructor functions instead (e.g. `newRootCmd()`)
- **gocritic:rangeValCopy**: Use `for i := range items` with `items[i]` instead of `for _, item := range items` when struct > ~128 bytes
- **depguard**: Check transitive deps too (e.g. Cobra pulls in `mousetrap`)
- **go.mod pseudo-versions**: Never use placeholder timestamps. Always run `go mod download <module>@<commit>` first to discover the correct timestamp, then construct `v0.0.0-YYYYMMDDHHMMSS-<12-char-hash>`.

## Test Patterns

- Never pass nil context — runtime panics, not caught by compiler/linter
- Scope test verification to own package: `go test ./internal/graph/...` not `go test ./...`

## Tracking Protocol

- **`BACKLOG.md`**: Check for open issues before starting work. Update when discovering or fixing issues.
- **`docs/roadmap.md`**: Check for current phase status. Update status markers on increment completion.
- **`LEARNINGS.md`**: Read for patterns and gotchas. Add new institutional knowledge when discovered.

## Key Decisions

See [docs/design/decisions.md](docs/design/decisions.md) for the full list. Highlights: 5-package "Pragmatic Flat" architecture, CLI-first development order, TOML config, SQLite sync DB, MIT license, Linux + macOS.

## Code Quality Standards

Idiomatic Go: functions do one thing, accept interfaces / return structs (consumer-defined), sentinel errors with `%w` wrapping, no package-level mutable state, table-driven tests, mandatory regression tests for every bug fix. See [docs/design/decisions.md](docs/design/decisions.md) for rationale.

## CI Protocol

CI must never be broken. Work is not done until CI passes.

- **Code changes require PRs.** Create a branch, push, open a PR, let CI run.
- **Doc-only changes push directly to main.** If the change only touches `.md` files, CLAUDE.md, LEARNINGS.md, BACKLOG.md, or roadmap — push to main directly. No PR needed. This keeps doc updates snappy.
- **Workflow**: `.github/workflows/ci.yml` runs build + test (with race detector) + lint on every push and PR
- **Integration tests**: `.github/workflows/integration.yml` runs `go test -tags=integration` against real Graph API on push to main + nightly. Uses Azure OIDC + Key Vault for token management. Local: `go test -tags=integration -race -v -timeout=5m ./internal/graph/...` (requires token via `go run . login`)
- **E2E tests**: Same workflow runs `go test -tags=e2e` after integration tests. Builds binary, exercises full CLI round-trip (whoami, ls, mkdir, put, get, stat, rm). Local: `ONEDRIVE_TEST_PROFILE=personal go test -tags=e2e -race -v -timeout=5m ./e2e/...`
- **Merge**: `./scripts/poll-and-merge.sh <pr_number>` — polls checks, merges when green, verifies post-merge workflow
- If CI fails, fix it immediately — it's your top priority. Never leave CI broken.
- **Pre-commit hook**: `.githooks/pre-commit` runs `golangci-lint run` before every commit. Configured via `git config core.hooksPath .githooks`. If lint fails, the commit is rejected — fix lint first, then commit.

## Worktree Workflow

Source code changes require a worktree + PR. Doc-only changes (`.md`, CLAUDE.md, LEARNINGS.md, BACKLOG.md) push directly to main.

| Type | Format | Example |
|------|--------|---------|
| **Branch** | `<type>/<task-name>` | `feat/cli-auth`, `fix/delta-pagination` |
| **Worktree** | `onedrive-go-<type>-<task>` | `onedrive-go-feat-cli-auth` |

**Types:** `feat`, `fix`, `refactor`, `test`, `docs`, `chore`

**Setup:** `git worktree add ../onedrive-go-<type>-<task> -b <type>/<task> main` — then work entirely inside the worktree directory.

**Cleanup:** After merge, `git worktree remove` + `git branch -D`. Never `--force` — inspect first.

See [docs/parallel-agents.md](docs/parallel-agents.md) for the full guide.

## Self-Maintenance Rule

**This CLAUDE.md is the single source of truth for all AI agents.** After major changes (new packages, CLI commands, docs, dependencies, or architectural shifts), update this file. Keep it concise — link to detailed docs rather than duplicating content. Every linked doc must exist; remove stale links. When CLAUDE.md exceeds 200 lines, move reference content to linked docs.
