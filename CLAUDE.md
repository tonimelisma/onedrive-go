# onedrive-go — Project Hub

## Project Summary

**onedrive-go** — a fast, safe, and well-tested OneDrive CLI and sync client in Go. Unix-style file operations (`ls`, `get`, `put`) plus robust bidirectional sync with conflict tracking. Targets Linux and macOS. MIT licensed.

Currently: Working CLI OneDrive client with discovery-based auth, account management, file ops, config integration. "Pragmatic Flat" architecture (4 active packages — `internal/sync/` deleted in Phase 4v2 Increment 0). Event-driven sync engine designed; implementation starts with 4v2.1. See [docs/design/event-driven-rationale.md](docs/design/event-driven-rationale.md).

## Current Phase

**Phases 1, 2, 3, 3.5 complete. Phase 4v2 Increment 0 (clean slate) complete. Next: 4v2.1-4v2.8.** Old batch-pipeline sync engine deleted (~16,655 lines). `sync` command returns "not yet implemented" stub. `tombstone_retention_days` removed from config. All development now happens on `clean-slate` branch (`main` is read-only safety net). Login is discovery-based: device code auth -> API discovery -> auto-create config. Users can `login`, `logout`, `whoami`, `status`, `drive add`, `drive remove`, `ls`, `get`, `put`, `rm`, `mkdir`, `stat`, `sync`. CLI loads config via `PersistentPreRunE` with four-layer override: defaults -> file -> env -> CLI flags. Auth and account management commands skip config loading. See [docs/roadmap.md](docs/roadmap.md).

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                      cmd/onedrive-go/ (Cobra CLI)                   │
│  ls, get, put, rm, mkdir, sync, status, conflicts, resolve, login  │
│  logout, whoami, verify                                            │
└──────────┬───────────────────────────────────────┬──────────────────┘
           │ file ops (direct API)                 │ sync operations
           │                                       │
           ▼                                       ▼
┌──────────────────────┐             ┌──────────────────────────────┐
│   internal/graph/    │◄────────────│       internal/sync/         │
│   Graph API client   │             │  engine, observers, buffer,  │
│   + quirk handling   │             │  planner, executor, baseline │
│   + auth             │             │  filter, transfer, conflict  │
└──────────────────────┘             └──────────────┬───────────────┘
                                                    │
                                     ┌──────────────┴───────────────┐
                                     │       internal/config/       │
                                     │       TOML + drives          │
                                     └──────────────────────────────┘

           ┌────────────────┐
           │ pkg/           │
           │ quickxorhash/  │
           │ (vendored)     │
           └────────────────┘
```

**Dependency direction**: `cmd/` -> `internal/*` -> `pkg/*`. No cycles. `cmd/` uses `graph/` directly for CLI file operations and `sync/` for sync operations. `sync/` depends on `graph/` for API access and `config/` for settings. `pkg/quickxorhash/` is a leaf utility used by `sync/` and `graph/`. `internal/graph/` handles all API quirks internally -- callers never see raw API data. `internal/graph/` does NOT import `internal/config/` — callers pass token paths directly.

## Package Layout

### Active packages
- **`pkg/quickxorhash/`** — QuickXorHash algorithm (hash.Hash interface) — complete
- **`internal/config/`** — TOML configuration with flat global settings and per-drive sections, validation, XDG paths, four-layer override chain (`ResolveDrive()`), cross-field validation (`ValidateResolved()`), drive matching (exact/alias/partial), token/state path derivation (`DriveTokenPath()`, `DriveStatePath()`), token-based drive discovery (`DiscoverTokens()`) — 95.4% coverage
- **`internal/graph/`** — Graph API client: HTTP transport, auth (token path-based, no config import; device code + browser-based PKCE login), retry, rate limiting, items CRUD, delta+normalization (incl. URL-decode + Prefer header), download/upload transfers (incl. session resume + fileSystemInfo), drives/user, path-based item resolution — feature-complete (94.2% coverage)
- **Root package** — Cobra CLI: login (discovery-based, browser+device code), logout, whoami, status, drive add/remove, ls, get, put, rm, mkdir, stat, sync (root.go, auth.go, files.go, sync.go, format.go, status.go, drive.go). Global flags: `--account`, `--drive`, `--config`, `--json`, `--verbose`, `--debug`, `--quiet`
- **`e2e/`** — E2E test suite (`//go:build e2e`): builds binary, exercises full round-trip against live OneDrive

### Next (Phase 4 v2 — Event-Driven Sync Engine)
- **`internal/sync/`** — Deleted in Increment 0. Will be rebuilt from scratch with event-driven components: Remote Observer (delta -> ChangeEvent[]), Local Observer (FS -> ChangeEvent[]), Change Buffer (debounce, dedup, batch), Planner (pure function: events + baseline -> ActionPlan, EF1-EF14 file matrix, ED1-ED8 folder matrix), Executor (actions -> Outcomes), Baseline Manager (sole DB writer, atomic commit). See [docs/design/architecture.md](docs/design/architecture.md)

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
| [docs/design/accounts.md](docs/design/accounts.md) | Account and drive system design |
| [docs/design/event-driven-rationale.md](docs/design/event-driven-rationale.md) | Option E architectural decision record (full rationale) |
| [BACKLOG.md](BACKLOG.md) | Issue/task tracker (bugs, improvements, research) |
| [LEARNINGS.md](LEARNINGS.md) | Institutional knowledge base (patterns, gotchas) |
| [docs/archive/](docs/archive/) | Historical docs (learnings-v1, backlog-v1, migration-v1) |
| [docs/tier1-research/](docs/tier1-research/) | 16 Tier 1 research docs (API bugs, edge cases, reference impl analysis) |
| [docs/parallel-agents.md](docs/parallel-agents.md) | Parallel agent worktree workflow guide |
| [docs/orchestration.md](docs/orchestration.md) | Full orchestration workflow: planning, agent prompts, review, wrap-up |

## Orchestration — Planning, Agents, Wrap-Up

**Full process**: [docs/orchestration.md](docs/orchestration.md). Read it before every increment.

**Core principle**: The human has full visibility. Information flows upward — nothing is hidden, nothing is silent. Escalate anything non-trivial.

**Planning**:
1. Confirm intent with the human before investing in a plan
2. Show research findings (not just conclusions)
3. Present alternatives with trade-offs for every significant decision
4. Every plan MUST include parallelization strategy (file conflict matrix, waves, worktrees)
5. Write non-goals, risk register, decision log
6. Check Definition of Ready before launching agents

**Execution**:
1. Agent task prompts MUST use the template from orchestration.md (includes four focused wrap-up questions)
2. Show pre-launch briefing to human (agent summary, files, conflicts)
3. Provide milestone updates during execution (PR created, CI passed, merged, etc.)
4. Escalate non-trivial issues to human — do NOT decide autonomously

**Post-Agent Review** (MANDATORY — not optional):
1. Read all agent reports (four wrap-up questions, LEARNINGS.md entries)
2. Line-by-line code review of every new/modified file
3. Top-up work: fix issues BEFORE presenting retrospective
4. Consolidate learnings, update BACKLOG, roadmap, CLAUDE.md

**Increment Report** (presented to human in chat):
1. Executive summary
2. Agent reports + orchestrator assessment (raw quotes + narrative judgment)
3. Top-up work report (file, change, why)
4. Code changes summary
5. Retrospective (well/wrong/change) + orchestrator self-assessment + metrics
6. Re-envisioning check drawing on agent perspectives
7. Action items with BACKLOG IDs

**Context preservation**: Commit frequently with descriptive messages. Document decisions in commit messages. After each increment, consolidate learnings into LEARNINGS.md and CLAUDE.md.

## Ownership and Standards

**You own this repo.** A broken test is not "unrelated" — it's your responsibility. This codebase is always in tip-top shape, always ready to deploy, and you are the one in charge of that.

- Never leave the repo in a broken state (build fails, tests fail, lint errors)
- Never introduce a regression and call it "pre-existing"
- If you touch a file, leave it better than you found it
- If something is broken, fix it — don't work around it

## Definition of Done (DOD)

Every change must pass ALL gates before committing:

1. **Build**: `go build ./...` — zero errors
2. **Unit tests**: `go test -race ./...` — all pass
3. **E2E tests**: `ONEDRIVE_TEST_DRIVE="personal:testitesti18@outlook.com" go test -tags=e2e -race -v -timeout=15m ./e2e/...` — all pass. E2E tests run the real binary against live OneDrive and catch integration-seam bugs that unit tests miss. Never push before running these locally.
4. **Lint**: `golangci-lint run` — zero issues
5. **Format**: `gofumpt` and `goimports -local github.com/tonimelisma/onedrive-go` applied
6. **Coverage**: New/modified code must have tests. Never decrease coverage.
7. **Logging review**: Review new/modified code for sufficient logging. Every public function entry, every state transition, every error path must have a log line. See Logging Standard below.
8. **Comment review**: Review new/modified code for sufficient comments explaining *why*. See Comment Convention below.
9. **Docs**: CLAUDE.md documentation index is current. All doc links are valid.
10. **Git clean**: Working tree is clean after commit. No uncommitted changes left behind.
11. **Git cleanup**: After merging PRs, delete merged branches (local and remote), remove worktrees (`git worktree remove`), prune remote refs (`git fetch --prune`), verify no stashes (`git stash list`), remove coordination files (e.g., PLAN_LEFT.md). Verify: only `main` and `clean-slate` branches exist locally, only `origin/main` and `origin/clean-slate` remotely, no open PRs, no orphaned worktree directories on disk. This is not optional — the repo must be fully clean before declaring an increment done.
12. **CI verification**: After merging PRs, wait for ALL CI workflows (both `ci.yml` and `integration.yml`) to pass before declaring the increment done. Integration tests catch regressions that unit tests miss (e.g., config changes breaking `--drive` in CI). Never skip this step.
13. **CI infrastructure**: If the increment changed anything affecting CI (token paths, secret naming, env vars, workflow YAML), verify that Key Vault secrets and GitHub variables are updated, and run `scripts/validate-ci-locally.sh` before pushing. See [docs/design/test-strategy.md §6.1](docs/design/test-strategy.md) and [docs/orchestration.md §1.4](docs/orchestration.md).
14. **Retrospective**: After each increment, conduct a brief retro covering: what went well, what could be improved, and what to change going forward. Capture actionable improvements in `LEARNINGS.md`. This applies to the increment as a whole, not to every individual commit.
15. **Re-envisioning check**: After each increment, step back and consider the project from a blank slate. Ask: "If I were starting this today, knowing everything I know now, would I build the same thing?" Evaluate architecture, package boundaries, API design, roadmap ordering, and testing strategy. If something feels stale or constrained by earlier decisions, flag it. Don't just follow the roadmap — challenge it. Propose concrete changes if warranted, or explicitly confirm the current direction is still correct. This check prevents path dependency from accumulating across increments.

### DOD Quick Check
```bash
go build ./... && go test -race -coverprofile=/tmp/cover.out ./... && ONEDRIVE_TEST_DRIVE="personal:testitesti18@outlook.com" go test -tags=e2e -race -v -timeout=15m ./e2e/... && golangci-lint run && go tool cover -func=/tmp/cover.out | grep total && echo "ALL GATES PASS"
```

### DOD Cleanup Check (after increment)
```bash
echo "=== Branches ===" && git branch && echo "=== Remote ===" && git branch -r && echo "=== Worktrees ===" && git worktree list && echo "=== Stashes ===" && git stash list && echo "=== Open PRs ===" && gh pr list --state open && echo "=== Status ===" && git status
```
Expected: `main` (safety net, read-only) and `clean-slate` (active) local branches. Only `origin/main` and `origin/clean-slate` remote. One worktree (clean-slate). No stashes, no open PRs, clean status.

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
- **depguard**: Update `.golangci.yml` when adding new external dependencies. Check transitive deps too (e.g. Cobra pulls in `mousetrap`)
- **gochecknoinits**: No `init()` functions allowed. Use constructor functions instead (e.g. `newRootCmd()`)
- **gocritic:rangeValCopy**: Use `for i := range items` with `items[i]` instead of `for _, item := range items` when struct > ~128 bytes
- **go.mod pseudo-versions**: Never use placeholder timestamps. Always run `go mod download <module>@<commit>` first to discover the correct timestamp, then construct `v0.0.0-YYYYMMDDHHMMSS-<12-char-hash>`.

## Test Patterns

- Never pass nil context — runtime panics, not caught by compiler/linter
- Scope test verification to own package: `go test ./internal/graph/...` not `go test ./...`

## Tracking Protocol

- **`BACKLOG.md`**: Check for open issues before starting work. Update when discovering or fixing issues.
- **`docs/roadmap.md`**: Check for current phase status. Update status markers on increment completion.
- **`LEARNINGS.md`**: Read for patterns and gotchas. Add new institutional knowledge when discovered.

## Key Decisions

See [docs/design/decisions.md](docs/design/decisions.md) for the full list. Highlights: 5-package "Pragmatic Flat" architecture, event-driven sync (Option E), delete-first strategy (old sync code deleted before new code is written — zero path dependency), CLI-first development order, TOML config, SQLite sync DB, MIT license, Linux + macOS.

## Code Quality Standards

Idiomatic Go: functions do one thing, accept interfaces / return structs (consumer-defined), sentinel errors with `%w` wrapping, no package-level mutable state, table-driven tests, mandatory regression tests for every bug fix. See [docs/design/decisions.md](docs/design/decisions.md) for rationale.

## CI Protocol

CI must never be broken. Work is not done until CI passes.

- **Code changes require PRs.** Create a branch, push, open a PR, let CI run.
- **Doc-only changes push directly to clean-slate.** If the change only touches `.md` files, CLAUDE.md, LEARNINGS.md, BACKLOG.md, or roadmap — push to clean-slate directly. No PR needed. This keeps doc updates snappy.
- **Workflow**: `.github/workflows/ci.yml` runs build + test (with race detector) + lint on every push and PR
- **Integration tests**: `.github/workflows/integration.yml` runs `go test -tags=integration` against real Graph API on push to main/clean-slate + nightly. Uses Azure OIDC + Key Vault for token management. Local: `go test -tags=integration -race -v -timeout=5m ./internal/graph/...` (requires token via `onedrive-go login --drive <canonical-id>`)
- **E2E tests**: Same workflow runs `go test -tags=e2e` after integration tests. Builds binary, exercises full CLI round-trip (whoami, ls, mkdir, put, get, stat, rm). Local: `ONEDRIVE_TEST_DRIVE=personal:testitesti18@outlook.com go test -tags=e2e -race -v -timeout=5m ./e2e/...`
- **Merge**: `./scripts/poll-and-merge.sh <pr_number>` — polls checks, merges when green, verifies post-merge workflow
- If CI fails, fix it immediately — it's your top priority. Never leave CI broken.
- **Pre-commit hook**: `.githooks/pre-commit` runs `golangci-lint run` before every commit. Configured via `git config core.hooksPath .githooks`. If lint fails, the commit is rejected — fix lint first, then commit.
- **Azure Key Vault management**: The orchestrator (Claude) has `az` CLI access and **should** manage Key Vault secrets directly when CI changes affect token paths, secret naming, or environment variables. Use `az keyvault secret set/download/list` for token management and `gh variable set` for GitHub repository variables. See [docs/design/test-strategy.md §6.1](docs/design/test-strategy.md) for naming conventions and local validation steps. The human only handles one-time Azure infrastructure setup and interactive browser-based `login` flows.
- **Local CI validation**: Before pushing changes that affect integration.yml (token paths, secret names, env vars), validate locally by mirroring the workflow's token loading logic. See [docs/design/test-strategy.md §6.1](docs/design/test-strategy.md) for the full local validation script. This avoids push-and-pray cycles.

## Worktree Workflow

Source code changes require a worktree + PR targeting `clean-slate`. Doc-only changes push directly to `clean-slate`. `main` is read-only (safety net).

Branch format: `<type>/<task-name>` (e.g., `feat/cli-auth`). Worktree: `onedrive-go-<type>-<task>`. Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`. See [docs/parallel-agents.md](docs/parallel-agents.md) for the full guide.

## Self-Maintenance Rule

**This CLAUDE.md is the single source of truth for all AI agents.** After major changes (new packages, CLI commands, docs, dependencies, or architectural shifts), update this file. Keep it concise — link to detailed docs rather than duplicating content. Every linked doc must exist; remove stale links. When CLAUDE.md exceeds 200 lines, move reference content to linked docs.
