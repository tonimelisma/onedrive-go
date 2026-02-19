# onedrive-go — Project Hub

## Project Summary

**onedrive-go** — a fast, safe, and well-tested OneDrive CLI and sync client in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking. Targets Linux and macOS. MIT licensed.

Currently: CLI-first OneDrive client, pivoted to "Pragmatic Flat" architecture (5 packages). Next: Phase 1 — Graph client + auth + CLI basics.

## Current Phase

**Phase 1 in progress (Graph Client + Auth + CLI Basics).** Increments 1.1 (HTTP transport) and 1.2 (auth) complete. Foundation: config package (94.8% coverage), QuickXorHash, design docs, research corpus. See [docs/roadmap.md](docs/roadmap.md) for the phase plan.

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
- **`internal/graph/`** — Graph API client: HTTP transport (1.1 done), device code auth with token persistence (1.2 done), retry, rate limiting, delta, upload, download, all quirk handling

### Building next (Phase 1)
- **`internal/graph/`** — Remaining: auth (1.2), items (1.3), delta (1.4), transfers (1.5), drives (1.6)
- **`cmd/onedrive-go/`** — CLI commands (Cobra): login, logout, ls, get, put, rm, mkdir

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
6. **Docs**: CLAUDE.md documentation index is current. All doc links are valid.
7. **Git**: Working tree is clean after commit. No uncommitted changes left behind.
8. **Retrospective**: After each increment, conduct a brief retro covering: what went well, what could be improved, and what to change going forward. Capture actionable improvements in `LEARNINGS.md`. This applies to the increment as a whole, not to every individual commit.

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

Comments explain **why**, not **what**. Both humans and AI read the code — inline comments are the most reliable context since they're right there when the code is read. Doc files can be missed.

**Good comments** (keep):
- **Why / intent**: `// Jitter prevents thundering herd when multiple workers hit rate limits`
- **Non-obvious constraints**: `// Must be 320 KiB-aligned or the Graph API returns 400`
- **Architectural boundaries**: `// Defined at consumer per "accept interfaces, return structs" — do not move to provider`
- **External references**: `// Per architecture.md §7.2` or `// See tier1-research/issues-api-bugs.md §3`
- **Gotcha warnings**: `// Graph API returns driveId in inconsistent casing across endpoints`
- **Contract/caller obligations**: `// Caller is responsible for closing the response body on success`

**Bad comments** (remove or rewrite):
- Restating the code: `// Check if status is 429` next to `if status == 429`
- Temporary project state: `// Increment 1.2 will implement this` (goes stale)
- Obvious type/function descriptions: `// staticToken returns a static token` on a type named staticToken

## Linter Patterns

Common golangci-lint rules that require specific patterns:

- **mnd**: Every number needs a named constant; tests are exempt
- **funlen**: Max 100 lines / 50 statements — decompose into small helpers
- **depguard**: Update `.golangci.yml` when adding new external dependencies
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

Idiomatic Go and clean code principles — enforced by review, not just linters:

- **Functions do one thing.** If a function name contains "and", split it. If it needs a comment explaining *what* it does (not *why*), rename it.
- **Accept interfaces, return structs.** Define interfaces at the consumer, not the provider. Keep interfaces small (1-3 methods).
- **Errors are values, not strings.** Use sentinel errors (`var ErrNotFound = errors.New(...)`) or custom types. Wrap with `%w` for context. Never `log.Fatal` in library code.
- **No package-level state.** No `init()`, no global `var` for mutable state. Pass dependencies explicitly.
- **Table-driven tests.** Preferred for any function with >2 interesting inputs. Name subtests descriptively.
- **Regression tests are mandatory.** Every bug fix must include a test that reproduces the bug. The test must fail without the fix and pass with it. No exceptions.

## CI Protocol

CI must never be broken. Work is not done until CI passes.

- **Code changes require PRs.** Create a branch, push, open a PR, let CI run.
- **Doc-only changes push directly to main.** If the change only touches `.md` files, CLAUDE.md, LEARNINGS.md, BACKLOG.md, or roadmap — push to main directly. No PR needed. This keeps doc updates snappy.
- **Workflow**: `.github/workflows/ci.yml` runs build + test (with race detector) + lint on every push and PR
- **Merge**: `./scripts/poll-and-merge.sh <pr_number>` — polls checks, merges when green, verifies post-merge workflow
- If CI fails, fix it immediately — it's your top priority. Never leave CI broken.
- **Pre-commit hook**: `scripts/pre-commit` runs `golangci-lint run` before every commit. Configured via `git config core.hooksPath scripts`. If lint fails, the commit is rejected — fix lint first, then commit.

## Self-Maintenance Rule

**This CLAUDE.md is the single source of truth for all AI agents.** After major changes (new packages, CLI commands, docs, dependencies, or architectural shifts), update this file. Keep it concise — link to detailed docs rather than duplicating content. Every linked doc must exist; remove stale links. When CLAUDE.md exceeds 200 lines, move reference content to linked docs.
