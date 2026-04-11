# onedrive-go

Fast, safe OneDrive CLI and sync client in Go. Unix-style file ops (`ls`, `get`, `put`) plus bidirectional sync with conflict tracking. Linux and macOS. MIT licensed.

## Routing Table

| When modifying... | Read first | Also consult |
|-------------------|-----------|--------------|
| `internal/graph/` | `spec/design/graph-client.md` | `spec/reference/graph-api-quirks.md` |
| `internal/tokenfile/` | `spec/design/graph-client.md` | |
| `internal/config/` | `spec/design/config.md` | |
| `internal/driveid/`, `internal/cli/drive.go` | `spec/design/drive-identity.md` | |
| `internal/driveops/`, `internal/cli/get.go`, `internal/cli/put.go` | `spec/design/drive-transfers.md` | |
| `pkg/quickxorhash/` | `spec/design/drive-transfers.md` | |
| `internal/retry/` | `spec/design/retry.md` | |
| `internal/synctree/` | `spec/design/system.md` | `spec/design/sync-engine.md`, `spec/design/sync-observation.md`, `spec/design/drive-transfers.md` |
| `internal/logfile/` | `spec/design/cli.md` | `spec/requirements/configuration.md` |
| `internal/syncobserve/` | `spec/design/sync-observation.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/syncplan/`, `internal/synctypes/` | `spec/design/sync-planning.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/syncexec/`, `internal/syncdispatch/`, `internal/localtrash/` | `spec/design/sync-execution.md` | |
| `internal/sync/engine*.go`, `internal/sync/permissions.go`, `internal/sync/permission_*.go`, `internal/cli/sync_helpers.go` | `spec/design/sync-engine.md` | |
| `internal/multisync/`, `internal/cli/sync.go` | `spec/design/sync-control-plane.md` | `spec/design/sync-engine.md`, `spec/design/config.md` |
| `internal/syncstore/`, `internal/syncverify/`, `internal/syncrecovery/`, `internal/cli/recover.go` | `spec/design/sync-store.md` | `spec/design/data-model.md`, `spec/design/sync-execution.md`, `spec/design/sync-engine.md` |
| `internal/cli/root.go`, `internal/cli/httpclient.go`, `internal/cli/format.go`, `internal/cli/signal.go`, `internal/cli/pidfile.go`, `internal/cli/auth.go`, `internal/cli/ls.go`, `internal/cli/rm.go`, `internal/cli/mkdir.go`, `internal/cli/mv.go`, `internal/cli/cp.go`, `internal/cli/stat.go`, `internal/cli/pause.go`, `internal/cli/resume.go`, `internal/cli/recycle_bin.go`, `internal/cli/status.go`, `internal/cli/resolve.go`, `main.go` | `spec/design/cli.md` | |

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

- Prefer durable solutions for authority boundaries, state ownership, and side-effect boundaries over expedient patches.
- Never treat current implementation as a reason to avoid change.
- The architecture should be extremely robust and full of defensive coding practices.
- Change modules and packages deliberately when it reduces duplicated authority, hidden coupling, or unclear ownership. No code is sacred, but churn without architectural payoff is not a virtue.
- App has zero users and hasn't launched yet. There is zero backwards-compatibility requirement anywhere: config format, token/cache files, state DB/schema, CLI flags, command output, and internal APIs can all change. Prefer the best design now and remove compatibility shims rather than carrying old architecture forward.

### Architecture Priorities

Optimize first for a codebase that is easy to reason about, easy to test, and explicit about ownership.

- **Explicit ownership of truth, mutation, and side effects.** Every persisted fact, derived view, runtime control path, and external effect must have a clear owner. If two modules can both "really" decide or mutate the same thing, the design is wrong.
- **Functional core, imperative shell.** Planning, classification, validation, resolution, and safety policy should be deterministic and side-effect free. Filesystem, network, database, clock, process, and environment effects stay at the edges behind explicit boundaries.
- **Single-owner mutable runtime state.** For coordinators such as watch loops, worker pools, retries, and lifecycle control, prefer one owner over shared mutable state. In concurrent code, single-writer discipline beats defensive mutation spread across goroutines.
- **High cohesion, low coupling.** Packages should own one axis of change and one slice of authority. Avoid pass-through layers, convenience imports, and sideways dependencies that blur responsibility.
- **Least-privilege capabilities.** Filesystem roots, arbitrary local paths, HTTP dispatch, token files, and state stores should be accessed through narrow capability objects or interfaces, not ambient access to `os`, `http`, or global process state.
- **DRY applies to knowledge, not syntax.** Do not duplicate facts, policy, invariants, or lifecycle rules. Small code duplication is acceptable when it preserves clearer ownership and avoids the wrong abstraction.
- **Domain types may own behavior.** Keep invariant-preserving behavior with the data it constrains when that improves clarity. Avoid both god objects and anemic "data only" types that force policy into distant helpers.
- **Composition and dependency inversion are tools, not rituals.** Compose small collaborators with explicit dependencies. Introduce interfaces at consuming boundaries for I/O, policy, or coordination seams; do not manufacture interfaces around every concrete type.
- **Prefer immutability in decision logic, ownership in runtime logic.** Pure planning inputs and outputs should be treated as values. For long-lived coordinators, optimize for explicit ownership and controlled mutation rather than blanket copying.

### Performance and Simplicity

These rules are the project-specific version of Pike/Hoare/KISS/Brooks. The short version: do not speculate, do not get clever early, and do not add coordination state without proof.

- **Measure before optimizing.** Do not add caches, batching, special-case fast paths, or algorithmic complexity because they seem faster. In this codebase, bottlenecks are often Graph latency, filesystem I/O, or transactional boundaries — not the place intuition points first.
- **Optimization needs evidence.** Performance work starts with profiles, benchmarks, or traces that show one part of the system dominating the rest. If the cost is not measured, it is not a justified optimization target.
- **Prefer simple algorithms while `n` is small.** Most sync decisions are per-path, per-folder, per-scope, or per-batch, and those sets are usually small. A direct linear scan over a small set is often the right choice until measured data proves otherwise.
- **Prefer simple data structures over clever machinery.** Fancy algorithms and layered caches bring invalidation bugs, ownership confusion, and duplicate state. If a simpler structure is fast enough, it is the better design.
- **Data model first, algorithm second.** Get the ownership boundaries, persisted state, and in-memory projections right first. When the data structures are correct, the right algorithm is usually obvious. If an optimization requires a second source of truth, it is probably the wrong optimization.

Project-specific consequences:

- **One source of truth beats duplicated state.** Durable state belongs in the authoritative store. In-memory state is allowed for ephemeral working sets or derived indexes, but it must be rebuildable and must never become a competing authority.
- **Hot-path complexity must earn its keep.** Watch-mode admission, planner checks, and retry scheduling should stay simple unless profiling shows a real bottleneck under realistic load.
- **Use brute force when it keeps the design honest.** A straightforward scan, recomputation, or rebuild is often preferable to a fragile cache with subtle invalidation rules.

### Ownership - you own this repo

- Never leave the repo in a broken state (build fails, tests fail, lint errors)
- Never call issues "pre-existing" - you find it, you fix it
- If you touch a file, leave it better than you found it
- If something is broken, fix it — don't work around it
- If you intentionally deviate from a rule, document the exception, why it was necessary, and any follow-up in the increment report

## Coding Conventions

### General

- Write comments for **why**, invariants, ownership rules, concurrency contracts, and non-obvious tradeoffs. Do not narrate the code line by line.
- Functions do one thing, with side effects explicit in their name, signature, or owning type
- Return concrete types by default. Define small interfaces in the consuming package at I/O, policy, or coordination seams
- No package-level mutable state
- No magic numbers — use named constants near their usage
- Always use named fields in struct literals — positional initialization breaks silently when fields are added
- Unexported by default. Export only what other packages need. The exported API is a contract.

### Naming

- **Names carry the semantics the type can't.** `count int` is useless. `pendingUploadCount int` is self-documenting. A name should let you understand usage without reading the definition.
- **Boolean names state the true condition.** `isReady`, `hasConflict`, `canRetry` — never negated names like `notDone` (double negatives in `if !notDone` are unreadable).
- **Package names are single lowercase words.** No underscores, no `util`, no `common`, no `helpers`. If you can't name the package, the abstraction is wrong.

### Error Handling

- **Wrap with `fmt.Errorf("verb noun: %w", err)`** — the message reads as a chain: `"sync file: upload chunk: HTTP POST: connection refused"`. Verb-noun, not "failed to" or "error while".
- **Error boundaries are explicit.** Translate Graph, filesystem, SQLite, and library errors into domain semantics at the boundary that understands them. Callers should branch on domain errors or decisions, not transport details leaking upward.
- **Errors cross exactly one boundary before being wrapped.** Don't double-wrap: if `graph.Client` wraps the HTTP error, `driveops` adds its own context but doesn't re-wrap the inner error. Each layer adds *its* perspective: what it was trying to do.
- **Sentinel errors are for callers that branch on them.** If no caller checks `errors.Is(err, ErrFoo)`, it doesn't need to be a sentinel — a formatted string is fine.
- **Never swallow errors.** If you handle an error (retry, fallback, skip), convert it into an explicit domain decision, durable record, or returned error. Log at the boundary that owns the decision, and avoid duplicate logging of the same failure as it crosses layers. The only silent discard allowed is when the doc comment on the function explicitly says so and why.
- **Panics are bugs, not flow control.** Panic only for programmer errors (invariant violations, impossible states). Never panic on external input — network responses, file system results, user config. Recover only at goroutine boundaries to prevent crashes, and log the panic as Error.
- **Partial failure is a first-class concept.** Operations that process multiple items return both results and errors. Never stop-on-first-error for bulk operations — collect, report, continue.

### Concurrency

- **Goroutine ownership:** Every goroutine must have a clear owner responsible for its lifecycle. The owner must ensure the goroutine terminates. No fire-and-forget goroutines — ever.
- **Context is the cancellation backbone.** Every function that does I/O or could block takes `context.Context` as its first parameter. Respect cancellation: check `ctx.Err()` before expensive operations, and select on `ctx.Done()` in loops.
- **Channel direction:** Always declare channel direction in function signatures (`chan<-`, `<-chan`). The sender closes, never the receiver.
- **Mutex scope:** A mutex protects data, not code. Comment what fields it guards. Keep critical sections small — no I/O under a lock, no channel operations under a lock. Prefer `sync.Mutex` over `sync.RWMutex` unless profiling shows read contention.
- **No goroutine in `init()` or package-level scope.** Goroutines start from explicit method calls, never implicitly.
- **Worker pools have bounded concurrency.** Use semaphores (`chan struct{}`) or `errgroup.SetLimit()`. Never let fan-out scale with input size.

### Resource Lifecycle

- **`defer` for cleanup, but verify the close.** `defer f.Close()` silently drops the error. For writes: check the error from `Close()` — it flushes buffers. Pattern: `defer func() { closeErr := f.Close(); if err == nil { err = closeErr } }()`.
- **Streams over buffers.** Never read an entire file or HTTP body into memory unless the size is bounded and small (< 1 MB). Use `io.Reader`/`io.Writer` pipelines. For large files, chunk.
- **HTTP response bodies are always closed.** Every `http.Response` gets `defer resp.Body.Close()` immediately, even on error status codes. Failing to do so leaks connections.
- **Temporary files use the target directory.** `os.CreateTemp(targetDir, pattern)` — so the subsequent `os.Rename` is atomic (same filesystem). Never create temps in `/tmp` and rename across mount points.

### File System Safety

- **Atomic writes only.** Never write directly to the target path. Write to a temp file in the same directory, `fsync`, then `os.Rename`. This is non-negotiable for any file that matters.
- **Paths use `filepath.Join`, never `+` concatenation.** No exceptions.
- **Validate all paths from external sources.** API responses can contain `..`, absolute paths, or names with special characters. Sanitize before any filesystem operation. Reject path traversal.
- **File comparison is content-based, not timestamp-based.** Timestamps lie (timezone bugs, FAT32 precision, NTP skew). Use checksums as the source of truth; timestamps as a fast-path hint only.
- **Preserve what you don't understand.** If the remote has metadata you don't model, don't destroy it by round-tripping through your structs. Only write back fields you explicitly manage.

### API Interaction Discipline

- **Timeout budgets propagate inward.** A sync pass has a deadline. Subdivide it across phases. Don't let one stuck request consume the entire budget. Use `context.WithTimeout` at each layer.
- **Treat every API response as untrusted input.** Nil-check nested fields. Validate enums against known values. Don't trust `Content-Length`. Don't assume array order.
- **Pagination is mandatory, not optional.** If an API can return `@odata.nextLink`, code must follow it. Never assume a single page is the full result. Test with collections larger than one page.
- **Idempotency awareness.** Know which operations are idempotent (GET, PUT, DELETE by ID) and which aren't (POST to create). Only retry idempotent operations automatically. For non-idempotent operations, implement idempotency keys or check-before-act.

### State Machine Discipline

- **State transitions are explicit, enumerated, and validated.** If an item can be `clean → modified → uploading → clean`, enforce the valid transitions. Reject illegal transitions loudly (log + error, not silent ignore).
- **State persists atomically.** Never write half a state update. Use transactions or write-replace for the state store. A crash between two writes must not leave inconsistent state.
- **State is recoverable from truth.** If local state is lost or corrupt, the system must be able to rebuild from the remote (or local FS + remote) without user intervention. Design state as a cache of decisions, not the source of truth.

### Defensive Coding

- **Validate at system boundaries, trust internally.** Validate: CLI args, config files, API responses, file system reads. Don't validate: function-to-function calls within a package (that's what types are for).
- **Make illegal states unrepresentable.** Use types to prevent misuse. A `DriveID` type prevents passing a random string where a drive ID is expected. Enums over raw strings. Required fields are struct members, optional fields use pointer-or-option patterns.
- **Timeouts on everything external.** Every HTTP call, every file operation that could hang (NFS, FUSE), every channel send that could block. No operation waits forever.
- **Bound all collections from external input.** If the API returns items, cap how many you process per batch. If a directory has 500k files, don't load them all into a slice. Stream or paginate.
- **Invariant assertions in debug/test.** For critical invariants (e.g., "every item has a parent"), add assertions that panic in test builds. Use build tags or a flag to enable them.

### Shutdown and Lifecycle

- **Graceful shutdown is cooperative.** On SIGINT/SIGTERM: stop accepting new work, drain in-flight operations, flush state, and exit when the runtime settles. Second signal = immediate exit.
- **In-flight operations are interruptible.** Every long operation checks context cancellation. An upload that ignores cancellation holds the process hostage.
- **Cleanup runs even on error paths.** Temp files, partial uploads, lock files — all cleaned up on any exit path. `defer` is the mechanism; never rely on "we'll clean up next run."

### Dependencies

- Evaluate every new dependency for maintenance health, transitive deps, and whether the functionality justifies the coupling. Fewer deps = smaller attack surface.
- Prefer stdlib over third-party when the stdlib solution is reasonable. Don't add a library for something `net/http` or `os` already does.

### Logging

`log/slog` with structured fields:
- **Debug**: HTTP request/response, token acquisition, file read/write
- **Info**: Lifecycle events — login/logout, sync start/complete, config load
- **Warn**: Degraded but recoverable — retries, expired tokens, fallbacks.
  For bulk sync operations, scope-level events and end-of-pass failure
  summaries replace per-item warnings. Individual items logged at Debug.
- **Error**: Terminal failures — request failed after all retries, unrecoverable auth
- Minimum per meaningful boundary: lifecycle events, state transitions, retries, external calls, and terminal error paths. Do not log routine function entry by default. Never log secrets.

### Test Style

- **All assertions use testify** (`github.com/stretchr/testify/assert` and `require`). Never use stdlib `t.Fatal`, `t.Fatalf`, `t.Error`, `t.Errorf` for assertions. Use `require` when the test cannot continue without the assertion passing (nil checks, error checks before using result). Use `assert` for non-fatal value comparisons.
- **Requirement traceability**: Every test that validates a spec requirement MUST have a `// Validates: R-X.Y.Z` comment on the line immediately before the `func Test...` declaration. Multiple requirements use comma separation: `// Validates: R-1.1, R-1.2`. For table-driven subtests, place the comment on the subtest case struct. This enables `grep -r "Validates:"` to produce a full traceability matrix.
- Never pass nil context — runtime panics not caught by compiler
- Table-driven tests where appropriate, with specific assertions (check values, not just "no error")

### Test Strategy

- **Test the contract, not the implementation.** Tests should break when behavior changes, not when you refactor internals. Test through exported APIs. Only test unexported functions when the logic is complex and the exported surface doesn't exercise it.
- **Failure injection is mandatory for I/O paths.** Use interfaces to inject: network failures, partial reads, slow responses, disk-full errors. Every I/O path must have at least one failure test.
- **Test concurrent code with the race detector and stress.** `-race` is in the DoD checklist, but also: write tests that exercise concurrent paths with `sync.WaitGroup` barriers. Use `-count=100` on flaky-candidate tests.
- **Golden files for complex output.** Sync plans, conflict reports, any structured output — compare against checked-in golden files, not inline string assertions. Update with `-update` flag.
- **Mocks implement the full interface contract.** A mock that returns `nil, nil` is a lie. Mocks should simulate realistic behavior: latency, partial results, error conditions. Use `testify/mock` with explicit expectations.

### E2E & Integration Tests

Run against live OneDrive accounts. Test account names are never committed — use `.env` (gitignored) or environment variables. Both suites require `ONEDRIVE_ALLOWED_TEST_ACCOUNTS` and `ONEDRIVE_TEST_DRIVE` to be set (crashes without them). Copy `.env.example` to `.env` and fill in your test accounts. E2E tests are tiered: `e2e` tag (fast, every CI push) vs `e2e_full` tag (slow, nightly/manual, 60-min bucket timeout). The full suite builds on top of the fast suite, so the canonical invocation is `go run ./cmd/devtool verify e2e-full` rather than `go test -tags=e2e_full` alone. In that profile, the verifier runs the full-suite preflight once, then executes three explicit buckets (`full-parallel-misc`, `full-serial-sync`, `full-serial-watch-shared`) so only the already-vetted misc tests regain `t.Parallel()` concurrency. Scheduled/manual full runs can also pass `--summary-json <path>`; the verifier then writes a machine-readable per-phase summary while the shared E2E harness writes `timing-summary.json` under the E2E log dir.

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
2. If the work involves a live CI / E2E / integration failure, search `spec/reference/live-incidents.md` first for an existing matching incident before starting a new investigation trail.
3. Read the governing design doc and requirements file (see Routing Table above).
4. Evaluate the codebase to determine if any foundational improvements are needed before starting.

### Step 2: Set up worktree

1. `git fetch origin` before creating the worktree so new work starts from the current `origin/main`, not a stale local `main`
2. Create the worktree from `origin/main` with `go run ./cmd/devtool worktree add --path <path> --branch <branch>`
3. Create a branch with the naming convention: `<type>/<task-name>` (e.g., `feat/cli-auth`). Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.
4. All changes go through PRs

`.worktreeinclude` lists local adjuncts to apply into new worktrees. Plain entries are copied as files; entries prefixed with `@` are symlinked instead of copied. Use `go run ./cmd/devtool worktree bootstrap --path <path>` to repair an existing worktree if needed.

### Step 3: Develop with TDD

All development follows strict red/green/refactor TDD
Mandatory regression tests for every bug fix.

### Step 4: Update docs

Mandatory, not optional:
- **Design doc**: update the module doc(s) you touched. New behavior → new spec section. Changed behavior → updated spec. New constraint discovered → constraints section.
- **Requirements**: if you completed a feature, update status (`implemented` → `verified` once tests pass). Mirror status in the design doc `Implements:` line.
- **Reference**: if you discovered a new API quirk, update the relevant reference doc upstream.
- **Live incidents ledger**: if the increment investigates or fixes a live CI / E2E / integration failure, add or update the matching entry in `spec/reference/live-incidents.md` in the same increment. Reuse the existing entry when the incident is clearly recurring instead of creating duplicates.

### Step 5: Self-verify

Re-read the governing design doc. Produce a compliance report listing each spec item, whether it was implemented in full, partially, or not at all, and how it was implemented.

### Step 6: Code review checklist

Self-review every change against coding standards proceeding to the Definition of Done. After opening a PR, review all human and automation feedback. This repo currently runs Codex PR reviews; Codex does not provide a required approval, but unresolved Codex review threads still block merge because `main` requires conversation resolution.

### Step 7: Definition of Done

After each increment, run through this entire checklist. If something fails, fix and re-run from the top. If you rebase, resolve conflicts, or otherwise rewrite branch history, re-run the checklist from item 1 on the rebased branch before creating the PR. **When complete, present this checklist to the human with pass/fail status for each item.**

1. [ ] **Format**: reported by `go run ./cmd/devtool verify default`
2. [ ] **Lint**: reported by `go run ./cmd/devtool verify default`
3. [ ] **Build**: reported by `go run ./cmd/devtool verify default`
4. [ ] **Unit tests**: reported by `go run ./cmd/devtool verify default`
5. [ ] **Coverage**: reported by `go run ./cmd/devtool verify default`
6. [ ] **Fast E2E**: reported by `go run ./cmd/devtool verify default`
7. [ ] **Docs updated**: AGENTS.md, CLAUDE.md, spec/design/, spec/requirements/ as needed
8. [ ] **Rebase onto latest main**: Immediately before any `gh pr create`, run `git fetch origin` and `git rebase origin/main` in the worktree branch, then verify with `git merge-base --is-ancestor origin/main HEAD`. Never create a PR from a stale merge base. If that verification fails, do not open the PR. If the rebase changes the branch or you resolve conflicts, restart this checklist at item 1 and only continue once the rebased branch is green.
9. [ ] **Push, review, and CI green**: After item 8 passes, push branch, using `git push --force-with-lease` if the rebase rewrote history, open PR with `gh pr create`, then enable auto-merge with `gh pr merge --auto --squash --delete-branch`. Branch protection requires the required CI checks to pass and all PR conversations to be resolved before merge. Monitor with `gh pr checks <pr_number> --watch`, wait for Codex PR review to finish, and address or explicitly resolve every actionable review thread before considering the increment complete.
10. [ ] **Cleanup**: Clean `git status`. From the root repo (not worktree), remove the current worktree after merge. Then force-delete the local branch with `git branch -D` (squash merges create a new commit on main, so Git cannot detect the branch as merged — `git branch -d` will wrongly warn "not fully merged"). Prune stale remote-tracking branches and pull main forward:
    cd /Users/tonimelisma/Development/onedrive-go
    rm -f <worktree-path>/.testdata   # remove stale symlink before worktree removal
    git worktree remove <worktree-path>
    git branch -D <branch-name>
    git fetch --prune origin
    git checkout main && git pull --ff-only origin main
    echo "=== Branches ===" && git branch && echo "=== Remote ===" && git branch -r && echo "=== Stashes ===" && git stash list && echo "=== Worktrees ===" && git worktree list && echo "=== Open PRs ===" && gh pr list --state open && echo "=== Status ===" && git status
    **NEVER delete other worktrees or branches — even if they appear stale.** Instead, report all other worktrees and branches to the human, including their last commit date. Let the human decide what to clean up
11. [ ] **Increment report**: Present to the human:
    - **What you changed**: What files did you change, why and how
    - **Plan deviations**: For every deviation from the approved plan — what changed, why it changed, what was done instead, and whether the new approach is the long-term solution or a temporary measure that needs follow-up
    - **Live incidents**: Which `spec/reference/live-incidents.md` entries were added or updated in this increment, or explicitly say `none`
    - **Top-up recommendations**: Any remaining codebase improvements you'd make. Don't be coy. Engineering effort is free, and this is mission-critical software. Ensure even small issues are brought up, and don't be coy to suggest more ambitious refactoring.
    - **Unfixed items**: Anything you were unable to address in this increment
