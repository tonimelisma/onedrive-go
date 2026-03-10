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

## Eng Philosophy

- Prefer large, long-term solutions over quick fixes. Do big re-architectures early, not late.
- Never settle for "good enough for now."
- Never treat current implementation as a reason to avoid change.
- The architecture should be extremely robust and full of defensive coding practices.
- Modules and packages can be rethought at a whim if a better design appears. No code is sacred.
- App hasn't been launched yet. No backwards compatibility. Ensure after refactoring the code doesn't show any signs of the old architecture.

### Ownership - you own this repo

- Never leave the repo in a broken state (build fails, tests fail, lint errors)
- Never call issues "pre-existing" - you find it, you fix it
- If you touch a file, leave it better than you found it
- If something is broken, fix it — don't work around it

## Coding Conventions

- Write lots of comments explaining **why**, not **what**
- Functions do one thing
- Accept interfaces / return structs
- Sentinel errors with `%w` wrapping
- No package-level mutable state

**Logging** (`log/slog` with structured fields):
- **Debug**: HTTP request/response, token acquisition, file read/write
- **Info**: Lifecycle events — login/logout, sync start/complete, config load
- **Warn**: Degraded but recoverable — retries, expired tokens, fallbacks
- **Error**: Terminal failures — request failed after all retries, unrecoverable auth
- Minimum per code path: function entry with key params, state transitions, error paths, external calls. Never log secrets.

**Test style**:
- **All assertions use testify** (`github.com/stretchr/testify/assert` and `require`). Never use stdlib `t.Fatal`, `t.Fatalf`, `t.Error`, `t.Errorf` for assertions. Use `require` when the test cannot continue without the assertion passing (nil checks, error checks before using result). Use `assert` for non-fatal value comparisons.
- **Requirement traceability**: Every test that validates a spec requirement MUST have a `// Validates: R-X.Y.Z` comment on the line immediately before the `func Test...` declaration. Multiple requirements use comma separation: `// Validates: R-1.1, R-1.2`. For table-driven subtests, place the comment on the subtest case struct. This enables `grep -r "Validates:"` to produce a full traceability matrix.
- Never pass nil context — runtime panics not caught by compiler
- Table-driven tests where appropriate, with specific assertions (check values, not just "no error")

**E2E & integration tests** run against live OneDrive accounts. Test account names are never committed — use `.env` (gitignored) or environment variables. Both suites require `ONEDRIVE_ALLOWED_TEST_ACCOUNTS` and `ONEDRIVE_TEST_DRIVE` to be set (crashes without them). Copy `.env.example` to `.env` and fill in your test accounts. E2E tests are tiered: `e2e` tag (fast, every CI push) vs `e2e_full` tag (slow, nightly/manual, 30-min timeout).

**Test credential pipeline** (one-time setup, then CI is self-sustaining):

1. **Bootstrap** — run once per test account (interactive, requires browser):
   ./scripts/bootstrap-test-credentials.sh   # opens browser for OAuth login
   Creates `.testdata/` with token files and `config.toml`. Run multiple times to add accounts (config accumulates drive sections).

2. **Migrate to CI** — upload `.testdata/` to Azure Key Vault:
   az login                                   # if not already logged in
   ./scripts/migrate-test-data-to-ci.sh       # uploads tokens + config to Key Vault

## Dev Process

Work is done in increments. Do not ask permission, do not skip any step.

### Step 1: Claim work

1. Search `spec/` for `[planned]` items.
2. Read the governing design doc and requirements file (see Routing Table above).
3. Evaluate the codebase to determine if any foundational improvements are needed before starting.

### Step 2: Set up worktree

1. Create a worktree using tool
2. Create a branch with the naming convention: `<type>/<task-name>` (e.g., `feat/cli-auth`). Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.
3. All changes go through PRs

`.worktreeinclude` lists files to copy into new worktrees. Entries prefixed with `@` are symlinked instead of copied

### Step 3: Develop with TDD

All development follows strict red/green/refactor TDD
Mandatory regression tests for every bug fix.

### Step 4: Update docs

Mandatory, not optional:
- **Design doc**: update the module doc(s) you touched. New behavior → new spec section. Changed behavior → updated spec. New constraint discovered → constraints section.
- **Requirements**: if you completed a feature, update status (`implemented` → `verified` once tests pass). Mirror status in the design doc `Implements:` line.
- **Reference**: if you discovered a new API quirk, update the relevant reference doc upstream.

### Step 5: Self-verify

Re-read the governing design doc. Produce a compliance report listing each spec item, whether it was implemented in full, partially, or not at all, and how it was implemented.

### Step 6: Code review checklist

Self-review every change against coding standards proceeding to the Definition of Done.

### Step 7: Definition of Done

After each increment, run through this entire checklist. If something fails, fix and re-run from the top. **When complete, present this checklist to the human with pass/fail status for each item.**

1. [ ] **Format**: `gofumpt -w . && goimports -local github.com/tonimelisma/onedrive-go -w .`
2. [ ] **Lint**: `golangci-lint run`
3. [ ] **Build**: `go build ./...`
4. [ ] **Unit tests**: `go test -race -coverprofile=/tmp/cover.out ./...`
5. [ ] **Coverage**: `go tool cover -func=/tmp/cover.out | grep total`
6. [ ] **Fast E2E**: `go test -tags=e2e -race -v -parallel 5 -timeout=10m ./e2e/...`
7. [ ] **Docs updated**: CLAUDE.md, spec/design/, spec/requirements/ as needed
8. [ ] **Push and CI green**: Push branch, open PR with `gh pr create`, then enable auto-merge with `gh pr merge --auto --squash --delete-branch`. Branch protection requires CI to pass before merge. Monitor with `gh pr checks <pr_number> --watch`
9. [ ] **Cleanup**: Clean `git status`. From the root repo (not worktree), remove the current worktree after merge. Then force-delete the local branch with `git branch -D` (squash merges create a new commit on main, so Git cannot detect the branch as merged — `git branch -d` will wrongly warn "not fully merged"). Prune stale remote-tracking branches and pull main forward:
    cd /Users/tonimelisma/Development/onedrive-go
    git worktree remove <worktree-path>
    git branch -D <branch-name>
    git fetch --prune origin
    git checkout main && git pull --ff-only origin main
    echo "=== Branches ===" && git branch && echo "=== Remote ===" && git branch -r && echo "=== Stashes ===" && git stash list && echo "=== Worktrees ===" && git worktree list && echo "=== Open PRs ===" && gh pr list --state open && echo "=== Status ===" && git status
    **NEVER delete other worktrees or branches — even if they appear stale.** Instead, report all other worktrees and branches to the human, including their last commit date. Let the human decide what to clean up
10. [ ] **Increment report**: Present to the human:
    - **What you changed**: What files did you change, why and how
    - **Plan deviations**: For every deviation from the approved plan — what changed, why it changed, what was done instead, and whether the new approach is the long-term solution or a temporary measure that needs follow-up
    - **Top-up recommendations**: Any remaining codebase improvements you'd make. Don't be coy. Engineering effort is free, and this is mission-critical software. Ensure even small issues are brought up, and don't be coy to suggest more ambitious refactoring.
    - **Unfixed items**: Anything you were unable to address in this increment
