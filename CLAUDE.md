# onedrive-go

Fast, safe OneDrive CLI and sync client in Go. Unix-style file ops (`ls`, `get`, `put`) plus bidirectional sync with conflict tracking. Linux and macOS. MIT licensed.

## Routing Table

| When modifying... | Read first | Also consult |
|-------------------|-----------|--------------|
| `internal/graph/` | `spec/design/graph-client.md` | `spec/reference/graph-api-quirks.md` |
| `internal/tokenfile/` | `spec/design/graph-client.md` | |
| `internal/config/` | `spec/design/config.md` | |
| `internal/driveid/`, `drive.go` | `spec/design/drive-identity.md` | |
| `internal/driveops/`, `get.go`, `put.go` | `spec/design/drive-transfers.md` | |
| `pkg/quickxorhash/` | `spec/design/drive-transfers.md` | |
| `internal/retry/` | `spec/design/retry.md` | |
| `internal/sync/observer_*.go`, `scanner.go`, `buffer.go` | `spec/design/sync-observation.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/planner.go`, `types.go` | `spec/design/sync-planning.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/executor*.go`, `worker.go`, `tracker.go`, `reconciler.go` | `spec/design/sync-execution.md` | |
| `internal/sync/engine*.go`, `orchestrator.go`, `drive_runner.go`, `sync.go` | `spec/design/sync-engine.md` | |
| `internal/sync/baseline.go`, `store_interfaces.go`, `migrations.go` | `spec/design/sync-store.md` | `spec/design/data-model.md` |
| Root package CLI files | `spec/design/cli.md` | |

| Working on capability... | Requirements | Design docs |
|--------------------------|-------------|-------------|
| R-1 File Operations | `spec/requirements/file-operations.md` | `spec/design/cli.md`, `spec/design/drive-transfers.md` |
| R-2 Sync | `spec/requirements/sync.md` | `spec/design/sync-*.md`, `spec/design/data-model.md` |
| R-3 Drive Management | `spec/requirements/drive-management.md` | `spec/design/drive-identity.md`, `spec/design/config.md` |
| R-4 Configuration | `spec/requirements/configuration.md` | `spec/design/config.md` |
| R-5 Transfers | `spec/requirements/transfers.md` | `spec/design/drive-transfers.md` |
| R-6 Non-Functional | `spec/requirements/non-functional.md` | `spec/design/system.md`, `spec/design/retry.md` |

Planned work: search `spec/` for `[planned]`. Reference docs: `spec/reference/`.

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

**Multiple AI agents work in this repo concurrently.** Be aware:

- Always check git status before committing — another agent may have pushed changes
- Do not assume you are the only one modifying files; expect merge conflicts
- If you encounter unexpected changes, investigate before overwriting

## Coding Conventions

- Go 1.23+, module path `github.com/tonimelisma/onedrive-go`
- Format: `gofumpt` + `goimports -local github.com/tonimelisma/onedrive-go`
- Follow `.golangci.yml` (140 char lines, complexity limits)
- US English (`canceled`, `marshaling`), three-group imports (stdlib / third-party / local)
- Comments explain **why**, not **what**
- OAuth app ID: `d50ca740-7a5b-4916-88b6-e8152e0e5071` — same ID everywhere (code, scripts, CI)

**Logging** (`log/slog` with structured fields):
- **Debug**: HTTP request/response, token acquisition, file read/write
- **Info**: Lifecycle events — login/logout, sync start/complete, config load
- **Warn**: Degraded but recoverable — retries, expired tokens, fallbacks
- **Error**: Terminal failures — request failed after all retries, unrecoverable auth
- Minimum per code path: function entry with key params, state transitions, error paths, external calls. Never log secrets.

**Test style**:
- **All assertions use testify** (`github.com/stretchr/testify/assert` and `require`). Never use stdlib `t.Fatal`, `t.Fatalf`, `t.Error`, `t.Errorf` for assertions. Use `require` when the test cannot continue without the assertion passing (nil checks, error checks before using result). Use `assert` for non-fatal value comparisons.
- Never pass nil context — runtime panics not caught by compiler
- Table-driven tests where appropriate, with specific assertions (check values, not just "no error")
- Scope verification to own package: `go test ./internal/graph/...` not `go test ./...`

**E2E & integration tests** run against live OneDrive accounts. Test account names are never committed — use `.env` (gitignored) or environment variables. Both suites require `ONEDRIVE_ALLOWED_TEST_ACCOUNTS` and `ONEDRIVE_TEST_DRIVE` to be set (crashes without them). Copy `.env.example` to `.env` and fill in your test accounts. E2E tests are tiered: `e2e` tag (fast, every CI push) vs `e2e_full` tag (slow, nightly/manual, 30-min timeout).

**Test credential pipeline** (one-time setup, then CI is self-sustaining):

1. **Bootstrap** — run once per test account (interactive, requires browser):
   ```bash
   ./scripts/bootstrap-test-credentials.sh   # opens browser for OAuth login
   ```
   Creates `.testdata/` with token files and `config.toml`. Run multiple times to add accounts (config accumulates drive sections).

2. **Migrate to CI** — upload `.testdata/` to Azure Key Vault:
   ```bash
   az login                                   # if not already logged in
   ./scripts/migrate-test-data-to-ci.sh       # uploads tokens + config to Key Vault
   ```

3. **CI pipeline** — fully automatic after migration:
   - Downloads credentials from Key Vault to `.testdata/` via OIDC
   - Runs tests with XDG isolation (`.testdata/` → temp dirs)
   - Saves rotated tokens back to Key Vault (keeps refresh tokens alive)
   - Nightly cron (10 AM UTC) prevents 90-day token expiry

**Re-bootstrapping** is needed if tokens expire (90 days idle) or Azure Key Vault secrets are purged. Run bootstrap + migrate again.

**Code quality**: Functions do one thing, accept interfaces / return structs, sentinel errors with `%w` wrapping, no package-level mutable state.

## Development Process

Work is done in increments. Follow this process from start to finish. Do not ask permission, do not skip any step.

### Step 1: Claim work

1. Search `spec/` for `[planned]` items.
2. Read the governing design doc and requirements file (see Routing Table above).
3. Evaluate the codebase to determine if any foundational improvements are needed before starting.

### Step 2: Set up worktree

1. Create a worktree: `claude --worktree <name>`.
2. Create a branch with the naming convention: `<type>/<task-name>` (e.g., `feat/cli-auth`). Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.
3. All changes go through PRs — including doc-only changes. Branch protection requires CI to pass before merge.

Pre-commit hook: `.githooks/pre-commit` runs `golangci-lint run` before every commit.

`.worktreeinclude` lists files to copy into new worktrees. Entries prefixed with `@` are symlinked instead of copied (preserves changes like rotated tokens back to the main worktree).

### Step 3: Develop with TDD

All development follows strict red/green/refactor TDD. For every behavior change — new feature, bug fix, refactoring, edge case:

1. **Red**: Write a failing test that specifies the desired behavior. Run it. Confirm it fails for the right reason.
2. **Green**: Write the minimum production code to make the test pass. Nothing more.
3. **Refactor**: Clean up the production code and test code while keeping all tests green.

Mandatory regression tests for every bug fix. Every new exported function, every behavior change, and every bug fix must have a test that was written first and seen to fail before the implementation made it pass.

### Step 4: Update docs

Mandatory, not optional:
- **Design doc**: update the module doc(s) you touched. New behavior → new spec section. Changed behavior → updated spec. New constraint discovered → constraints section.
- **Requirements**: if you completed a feature, update status (`implemented` → `verified` once tests pass). Mirror status in the design doc `Implements:` line.
- **Reference**: if you discovered a new API quirk, update the relevant reference doc upstream.

### Step 5: Self-verify

Re-read the governing design doc. Produce a compliance report listing each spec item, whether it was implemented in full, partially, or not at all, and how it was implemented.

### Step 6: Code review checklist

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

### Step 7: Definition of Done

After each increment, run through this entire checklist. If something fails, fix and re-run from the top. **When complete, present this checklist to the human with pass/fail status for each item.**

1. [ ] **Format**: `gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w .`
2. [ ] **Build**: `go build ./...`
3. [ ] **Unit tests**: `go test -race -coverprofile=/tmp/cover.out ./...`
4. [ ] **Lint**: `golangci-lint run`
5. [ ] **Coverage**: `go tool cover -func=/tmp/cover.out | grep total` — never decrease
6. [ ] **Fast E2E**: `go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...` (reads `.env` for test accounts)
7. [ ] **Docs updated**: CLAUDE.md, spec/design/, spec/requirements/ as needed
8. [ ] **Push and CI green**: Push branch, open PR with `gh pr create`, then enable auto-merge with `gh pr merge --auto --squash --delete-branch`. Branch protection requires all 4 CI jobs (lint, test, integration, e2e) to pass before merge (`enforce_admins=true`, no direct pushes). Monitor with `gh pr checks <pr_number> --watch`
9. [ ] **Cleanup**: Clean `git status`. Remove the current worktree after merge. Then, **from the root repo** (not the worktree), prune stale remote-tracking branches and pull main forward:
    ```bash
    cd /Users/tonimelisma/Development/onedrive-go
    git fetch --prune origin
    git checkout main && git pull --ff-only origin main
    ```
    **NEVER delete other worktrees or branches — even if they appear stale.** Instead, report all other worktrees and branches to the human, including their last commit date (use `git log -1 --format='%ci' <branch>` for each). Let the human decide what to clean up
10. [ ] **Increment report**: Present to the human:
    - **Plan deviations**: For every deviation from the approved plan — what changed, why it changed, what was done instead, and whether the new approach is the long-term solution or a temporary measure that needs follow-up
    - **Process changes**: What you would do differently next time in how the work was planned or executed
    - **Top-up recommendations**: Any remaining codebase improvements you'd make. Don't be coy. Engineering effort is free, and this is mission-critical software. Ensure even small issues are brought up, and don't be coy to suggest more ambitious refactoring.
    - **Architecture re-envisioning**: If you were starting from a blank slate, would you build it the same way? Propose any dramatic architectural changes if a better design is apparent
    - **Unfixed items**: Anything you were unable to address in this increment

Quick command (gates 1-6):
```bash
gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w . && go build ./... && go test -race -coverprofile=/tmp/cover.out ./... && golangci-lint run && go tool cover -func=/tmp/cover.out | grep total && go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/... && echo "ALL GATES PASS"
```

Cleanup check:
```bash
echo "=== Branches ===" && git branch && echo "=== Remote ===" && git branch -r && echo "=== Stashes ===" && git stash list && echo "=== Worktrees ===" && git worktree list && echo "=== Open PRs ===" && gh pr list --state open && echo "=== Status ===" && git status
```
