# onedrive-go

Fast, safe OneDrive CLI and sync client in Go. Unix-style file ops (`ls`, `get`, `put`) plus bidirectional sync. Linux and macOS. MIT licensed.

## Routing Table

| When modifying... | Read first | Also consult |
|-------------------|-----------|--------------|
| `internal/graph/` | `spec/design/graph-client.md` | `spec/reference/graph-api-quirks.md` |
| `internal/graphtransport/` | `spec/design/graph-client.md` | |
| `internal/tokenfile/` | `spec/design/graph-client.md` | |
| `internal/config/` | `spec/design/config.md` | |
| `internal/cli/drive_reset_sync_state.go` | `spec/design/cli.md` | `spec/design/sync-store.md`, `spec/design/sync-engine.md` |
| `internal/driveid/`, `internal/cli/drive*.go` | `spec/design/drive-identity.md` | |
| `internal/driveops/`, `internal/cli/get.go`, `internal/cli/put.go` | `spec/design/drive-transfers.md` | |
| `pkg/quickxorhash/` | `spec/design/drive-transfers.md` | |
| `internal/retry/` | `spec/design/retry.md` | |
| `internal/errclass/` | `spec/design/error-model.md` | |
| `internal/perf/` | `spec/design/system.md` | `spec/design/cli.md`, `spec/design/sync-control-plane.md` |
| `internal/devtool/` | `spec/design/system.md` | |
| `internal/synctree/` | `spec/design/system.md` | `spec/design/sync-engine.md`, `spec/design/sync-observation.md`, `spec/design/drive-transfers.md` |
| `internal/logfile/` | `spec/design/cli.md` | `spec/requirements/configuration.md` |
| `internal/sync/observer*.go`, `internal/sync/scanner*.go`, `internal/sync/socketio*.go`, `internal/sync/item_converter*.go`, `internal/sync/local_hash_reuse.go` | `spec/design/sync-observation.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/planner*.go`, `internal/sync/single_path*.go`, `internal/sync/truth_status.go` | `spec/design/sync-planning.md` | `spec/reference/onedrive-sync-behavior.md` |
| `internal/sync/executor*.go`, `internal/sync/worker*.go`, `internal/sync/action_freshness.go`, `internal/sync/dep_graph*.go`, `internal/sync/active_scopes*.go` | `spec/design/sync-execution.md` | |
| `internal/sync/engine.go`, `internal/sync/engine_config.go`, `internal/sync/engine_run_once*.go`, `internal/sync/engine_current_*.go`, `internal/sync/engine_runtime_*.go`, `internal/sync/engine_watch_*.go`, `internal/sync/shortcut_topology.go`, `internal/sync/shortcut_root_lifecycle.go`, `internal/sync/shortcut_root_planner*.go`, `internal/sync/shortcut_root_transition.go`, `internal/sync/shortcut_root_publication.go`, `internal/sync/shortcut_root_status*.go`, `internal/sync/protected_roots.go`, `internal/sync/shortcut_alias_mutation.go`, `internal/sync/permissions.go`, `internal/sync/permission_*.go`, `internal/cli/sync_flow.go`, `internal/cli/sync_runtime.go`, `internal/cli/sync_render.go` | `spec/design/sync-engine.md` | |
| `internal/multisync/`, `internal/cli/sync.go` | `spec/design/sync-control-plane.md` | `spec/design/sync-engine.md`, `spec/design/config.md` |
| `internal/sync/store*.go`, `internal/sync/shortcut_root_state.go`, `internal/sync/shortcut_root_store.go`, `internal/sync/condition_keys.go`, `internal/sync/condition_projection.go`, `internal/sync/blocked_retry_projection.go`, `internal/sync/observation_reconcile_policy.go`, `internal/sync/scope_key.go`, `internal/sync/scope_semantics.go`, `internal/sync/scope_block.go`, `internal/syncverify/` | `spec/design/sync-store.md` | `spec/design/data-model.md`, `spec/design/sync-execution.md`, `spec/design/sync-engine.md` |
| `internal/cli/root.go`, `internal/cli/format.go`, `internal/cli/signal.go`, `internal/cli/auth*.go`, `internal/cli/account_view*.go`, `internal/cli/email_reconcile.go`, `internal/cli/shared*.go`, `internal/cli/get*.go`, `internal/cli/put*.go`, `internal/cli/ls.go`, `internal/cli/rm.go`, `internal/cli/mkdir.go`, `internal/cli/mv.go`, `internal/cli/cp.go`, `internal/cli/stat.go`, `internal/cli/purge.go`, `internal/cli/pause.go`, `internal/cli/resume.go`, `internal/cli/sync_pause_resume.go`, `internal/cli/recycle_bin.go`, `internal/cli/recycle_bin_flow.go`, `internal/cli/status*.go`, `internal/cli/perf.go`, `internal/cli/cleanup.go`, `internal/cli/failure_class.go`, `internal/cli/degraded_discovery.go`, `main.go` | `spec/design/cli.md` | |

| Working on capability... | Requirements | Design docs |
|--------------------------|-------------|-------------|
| R-1 File Operations | `spec/requirements/file-operations.md` | `spec/design/cli.md`, `spec/design/drive-transfers.md` |
| R-2 Sync | `spec/requirements/sync.md` | `spec/design/sync-*.md`, `spec/design/data-model.md` |
| R-3 Drive Management | `spec/requirements/drive-management.md` | `spec/design/drive-identity.md`, `spec/design/config.md` |
| R-4 Configuration | `spec/requirements/configuration.md` | `spec/design/config.md` |
| R-5 Transfers | `spec/requirements/transfers.md` | `spec/design/drive-transfers.md` |
| R-6 Non-Functional | `spec/requirements/non-functional.md` | `spec/design/system.md`, `spec/design/retry.md` |

Planned work: search `spec/` for `[planned]`. Reference docs: `spec/reference/`.
New contributors should start with `spec/reference/developer-onboarding.md`.

## Eng Philosophy

- Prefer durable solutions for authority boundaries, state ownership, and side-effect boundaries over expedient patches.
- Never treat current implementation as a reason to avoid change.
- Robustness comes from explicit ownership, simple data flow, and strong invariants, not from adding layers, wrappers, or framework-shaped local architecture.
- Do not add services, managers, adapters, builders, or DTO-style pass-through types unless they own a real boundary or eliminate duplicated authority.
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

### Anti-Fizzbuzz Guardrails

- **No abstraction without authority.** If a new type, package, or layer does not own I/O, policy, lifecycle, or a durable truth boundary, it probably should not exist.
- **Prefer direct concrete call flow.** Inside one package, a short concrete helper is usually better than a same-process handler/service/manager chain.
- **Package moves must reduce cognitive load.** If a boundary is collapsed or renamed, remove the old vocabulary everywhere in the same increment.
- **Avoid ghost architecture.** Do not keep comments, docs, verifier rules, error prefixes, or test names referring to deleted packages, files, or concepts.
- **Say what the code really does.** Do not call code "pure" if it logs, mutates state, or performs I/O. Use "deterministic" when that is what you mean.
- **Avoid god packages.** If one package starts owning unrelated reasons to change, split it by authority boundary, not by generic technical phase names.
- **No fake intermediate boundaries.** If code now lives in `internal/sync`, either document the current file-family ownership clearly or split it again for a real reason; do not pretend deleted packages still exist conceptually.

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
- Comments and error prefixes must name the current package or concept, not the package or boundary that used to own the code before a refactor.
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
- **Use explicit synchronization, not wall-clock sleeps.** `time.Sleep` is banned in test code. Use readiness channels, injected clocks/timers, or observable state transitions instead. The only acceptable exception is a narrowly justified live E2E helper that waits on external process or provider propagation and carries a specific `nolint`.
- **Test concurrent code with the race detector and stress.** `-race` is in the DoD checklist, but also: write tests that exercise concurrent paths with `sync.WaitGroup` barriers. Use `-count=100` on flaky-candidate tests.
- **Golden files for complex output.** Sync plans, conflict reports, any structured output — compare against checked-in golden files, not inline string assertions. Update with `-update` flag.
- **Mocks implement the full interface contract.** A mock that returns `nil, nil` is a lie. Mocks should simulate realistic behavior: latency, partial results, error conditions. Use `testify/mock` with explicit expectations.

### E2E & Integration Tests

Run against live OneDrive accounts. Test account names are never committed — use `.env` (gitignored) or environment variables. Both suites require `ONEDRIVE_ALLOWED_TEST_ACCOUNTS` and `ONEDRIVE_TEST_DRIVE` to be set (crashes without them). Copy `.env.example` to `.env` and fill in your test accounts. E2E tests are tiered: `e2e` tag (minimal PR smoke lane) vs `e2e_full` tag (slow, nightly/manual, 60-min bucket timeout). The fast `e2e` profile is intentionally limited to verifier-owned auth preflight, fast fixture preflight, a basic direct CLI CRUD smoke test, `TestE2E_Sync_UploadOnly`, `TestE2E_Sync_DownloadOnly`, `TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount`, and `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression`; the verifier gates the dedicated preflight tests with `ONEDRIVE_E2E_RUN_AUTH_PREFLIGHT=1` / `ONEDRIVE_E2E_RUN_FAST_FIXTURE_PREFLIGHT=1` and lets only the first fast invocation own the suite scrub while later fast invocations reuse it via `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1`. The full suite builds on top of the fast suite, so the canonical invocation is `go run ./cmd/devtool verify e2e-full` rather than `go test -tags=e2e_full` alone. In that profile, the verifier runs the full-suite preflight once, then executes three explicit buckets (`full-parallel-misc`, `full-serial-sync`, `full-serial-watch-shared`) so only the already-vetted misc tests regain `t.Parallel()` concurrency, and the demoted richer live coverage now lives there. Fast `e2e` no longer uses `-race`; race-heavy live coverage remains in `e2e-full`, `public`, and `stress`. Scheduled/manual full runs can also pass `--summary-json <path>`; the verifier then writes a machine-readable per-phase summary while the shared E2E harness writes `timing-summary.json` under the E2E log dir.

**Test credential pipeline** (one-time setup, then CI is self-sustaining):

1. **Bootstrap** — run once per test account (interactive, requires browser):
   ./scripts/bootstrap-test-credentials.sh   # opens browser for OAuth login
   Creates `.testdata/` with token files, `catalog.json`, and `config.toml`. Run multiple times to add accounts (config accumulates drive sections and the catalog accumulates inventory).

2. **Migrate to CI** — upload `.testdata/` to Azure Key Vault:
   az login                                   # if not already logged in
   ./scripts/migrate-test-data-to-ci.sh       # uploads tokens + catalog + config to Key Vault

## Dev Process

Work is done in increments. Do not ask permission, do not skip any step.

### Step 1: Claim work

1. Search `spec/` for `[planned]` items.
2. If the work involves a live CI / E2E / integration failure, search `spec/reference/live-incidents.md` first for an existing matching incident before starting a new investigation trail.
3. Read the governing design doc and requirements file (see Routing Table above).
4. Run `go run ./cmd/devtool verify --dod --stage start` before heavy implementation. This writes/updates the local ignored `.dod-pr-comments.json` manifest from the latest recent merged-PR review threads. If it fails because unresolved threads need manual classification, classify each unresolved thread in the manifest as `fixed`, `already_fixed`, or `non_actionable`; include `what_changed`, `how_fixed`, and evidence for fixed/already-fixed threads, or `reason` for non-actionable threads. Fold every actionable stale comment into the current increment before running expensive verification.
5. Evaluate the codebase to determine if any foundational improvements are needed before starting.
6. If docs and code disagree about package names, file ownership, or boundaries, fix that drift in the same increment before building more work on top of the stale model.

### Step 2: Definition of Ready

Before implementation, produce and present a Definition of Ready report to the
human. Do not start code edits until every item is `PASS`, or until a failed
item is converted into a documented assumption with a concrete validation step.
Once the report is presented and every item is `PASS` or `ASSUMPTION`, continue
the increment without waiting for separate approval unless the human explicitly
pauses or redirects the work. If later discovery changes the behavior, scope,
authority owner, regression plan, verification plan, or safety profile, refresh
this report before continuing.

**DoR report format:** List all 12 checklist items in order. Prefix every item
with `PASS`, `FAIL`, or `ASSUMPTION`. Include the evidence, file path, command,
design-doc section, or explicit assumption that supports the status.

1. [ ] **Task intent**: state the increment as one product-visible behavior, repo-maintenance outcome, or investigation result.
2. [ ] **Non-goals**: state what is intentionally out of scope for this increment.
3. [ ] **Governing docs**: identify the requirements, design docs, and reference docs read for the touched area.
4. [ ] **Authority owner**: name the owner of truth, mutation, side effects, durable state, runtime lifecycle, and user-facing output for the change.
5. [ ] **Code scope**: name the expected packages/files and any areas that must not be touched.
6. [ ] **Regression plan**: name the failing test, new test, or existing verification surface that will prove the behavior.
7. [ ] **Verification plan**: name the focused test commands, race/stress/E2E needs, and verifier profile required before PR.
8. [ ] **Data safety**: state how the increment avoids unintended local deletes, remote deletes, overwrites, state DB corruption, and stale-truth mutation.
9. [ ] **Secret and credential safety**: state whether token files, `.env`, CI secrets, live account names, or credential-bearing logs are involved; if so, state the redaction/avoidance plan.
10. [ ] **Live provider and destructive operations**: state whether the increment can mutate live OneDrive/SharePoint state, reset local state, alter release artifacts, rewrite git history, or upgrade dependencies; if so, name the exact operation, why it is necessary, and the rollback or recovery plan.
11. [ ] **Review carryover**: summarize the `go run ./cmd/devtool verify --dod --stage start` result and how any actionable stale review threads are folded into this increment.
12. [ ] **Unknowns and assumptions**: list each unknown in this format: `Question:`, `Current assumption:`, `Validation step:`, and `Owner document/code path affected if assumption is wrong:`.

### Step 3: Set up worktree

1. `git fetch origin` before creating the worktree so new work starts from the current `origin/main`, not a stale local `main`
2. Create the worktree from `origin/main` with `go run ./cmd/devtool worktree add --path <path> --branch <branch>`
3. Create a branch with the naming convention: `<type>/<task-name>` (e.g., `feat/cli-auth`). Types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.
4. All changes go through PRs

`.worktreeinclude` lists local adjuncts to apply into new worktrees. Plain entries are copied as files; entries prefixed with `@` are symlinked instead of copied. Use `go run ./cmd/devtool worktree bootstrap --path <path>` to repair an existing worktree if needed.

### Step 4: Develop with TDD

All development follows strict red/green/refactor TDD
Mandatory regression tests for every bug fix.

### Step 5: Update docs

Mandatory, not optional:
- **Design doc**: update the module doc(s) you touched. New behavior → new spec section. Changed behavior → updated spec. New constraint discovered → constraints section.
- **Requirements**: if you completed a feature, update status (`implemented` → `verified` once tests pass). Mirror status in the design doc `Implements:` line.
- **Reference**: if you discovered a new API quirk, update the relevant reference doc upstream.
- **Live incidents ledger**: if the increment investigates or fixes a live CI / E2E / integration failure, add or update the matching entry in `spec/reference/live-incidents.md` in the same increment. Reuse the existing entry when the incident is clearly recurring instead of creating duplicates.
- **README/public status**: if the increment changes product capability status, phase/status language, supported platforms, setup commands, verification entrypoints, or contributor workflow, update `README.md` in the same increment. `README.md` status must summarize `spec/requirements/index.md`; it must not introduce a separate phase or roadmap model.
- **Refactor hygiene**: package/file renames, deletions, merges, and boundary moves must update in the same increment: `AGENTS.md`, `CLAUDE.md`, routing tables, `GOVERNS:` lists, verifier rules, package comments, error-message prefixes, test names, and design-doc references. Do not leave basic naming cleanup for a follow-up increment.
- **Deleted-name sweep**: after boundary refactors, grep for deleted package, file, and concept names and remove or justify every remaining reference. Historical references must be explicitly labeled as historical.

### Step 6: Self-verify

Re-read the governing design doc. Produce a compliance report listing each spec item, whether it was implemented in full, partially, or not at all, and how it was implemented.
If the increment moved, deleted, or collapsed a boundary, run an architecture-drift sweep and verify that docs, verifier rules, comments, package docs, and user-facing/internal error text use the current package and file names.
If you deleted a package or file, prove there are no stale references left outside intentionally labeled historical notes.

### Step 7: Code review checklist

Self-review every change against coding standards proceeding to the Definition of Done. After opening a PR, review all human and automation feedback. This repo currently runs Codex PR reviews; Codex does not provide a required approval, but unresolved Codex review threads still block merge because `main` requires conversation resolution.
When a PR carries review-thread follow-up from prior work, include a PR body
section named `Review-thread carryover` with the prior PR number, comment
summary, code/test evidence, and resolution status. If there is no carryover,
state `none`.
Every increment must also audit recent merged PRs for unfixed actionable review
comments or unresolved review threads via `go run ./cmd/devtool verify --dod
--stage start`. The tool never guesses actionability: agents classify unresolved
threads manually in `.dod-pr-comments.json`. Fix any stale actionable comments
in the current increment, include regression evidence, and let the DoD tool post
the original-thread reply with clear what/how/evidence text. Agents must comment
and resolve every handled thread, always. Non-actionable noise such as review
tool quota notices must be classified as `non_actionable` with a reason, then
commented and resolved by the tool rather than silently ignored.

### Step 8: Definition of Done

After each increment, run through the entire DoD checklist. If something fails,
fix and re-run from the top. If you rebase, resolve conflicts, or otherwise
rewrite branch history, re-run the checklist from item 1 on the rebased branch
before creating the PR. **When complete, present the numbered DoD checklist and
the numbered Final Increment Report to the human.**

**DoD checklist format:** List checklist items 1-11 in order. Prefix every item with a status emoji and text: `✅ PASS` when complete, `❌ FAIL` when failed, blocked, skipped, or not run. Do not collapse items together; include the command, PR, CI, cleanup, or review evidence that proves each status.

1. [ ] **Format**: reported by `go run ./cmd/devtool verify default`
2. [ ] **Lint**: reported by `go run ./cmd/devtool verify default`
3. [ ] **Build**: reported by `go run ./cmd/devtool verify default`
4. [ ] **Unit tests**: reported by `go run ./cmd/devtool verify default`
5. [ ] **Coverage**: reported by `go run ./cmd/devtool verify default`
6. [ ] **Fast E2E**: reported by `go run ./cmd/devtool verify default`
7. [ ] **Docs updated**: `README.md`, `AGENTS.md`, `CLAUDE.md`, the routing table, governed design docs, and verifier rules all match the current code layout and capability status
8. [ ] **Architecture/docs drift sweep**: no stale references to deleted packages, files, or boundaries remain in repo guidance, design docs, package comments, or error strings unless explicitly marked historical
9. [ ] **Rebase onto latest main**: Immediately before any `gh pr create`, run `git fetch origin` and `git rebase origin/main` in the worktree branch, then run `go run ./cmd/devtool verify --dod --stage pre-pr`. Never create a PR from a stale merge base. If the rebase changes the branch or you resolve conflicts, restart this checklist at item 1 and only continue once the rebased branch is green.
10. [ ] **Push, review, CI green, PR comments audited, and PR merged to main**: After item 9 passes, push branch, using `git push --force-with-lease` if the rebase rewrote history, open PR with `gh pr create`, and include the review-thread carryover section from `.dod-pr-comments.json`. When CI and review are ready, run `go run ./cmd/devtool verify --dod --stage pre-merge --pr <pr_number>`. This waits for required PR CI, applies classified review-thread replies, resolves handled threads, and verifies no unresolved current or recent merged-PR review threads remain. Do not wait indefinitely for automatic Codex comments that may never arrive; if no automatic review/comments exist when CI is terminal, continue after the final sweep. If a late actionable thread appears after merge, fix it in a follow-up PR and resolve that thread there. Auto-merge may be requested, but it is never DoD evidence by itself. This item is `✅ PASS` only after `go run ./cmd/devtool verify --dod --stage post-merge --pr <pr_number> --worktree <worktree-path> --branch <branch-name>` reports the PR merged to `main`, the root checkout fast-forwarded, configured post-merge CI interpreted cleanly, and cleanup audit run.
11. [ ] **Cleanup**: `go run ./cmd/devtool verify --dod --stage post-merge --pr <pr_number> --worktree <worktree-path> --branch <branch-name>` owns current-increment cleanup: it handles the known `gh pr merge` multi-worktree `main` checkout quirk by verifying server-side merge state, fetches/prunes, fast-forwards root `main`, removes the increment worktree, deletes the local branch, tolerates an already-deleted remote branch, interprets configured squash-merge push CI skips, and runs cleanup audit.
    Include **Emoji cleanup evidence** inside this item. The evidence must cover root checkout status, the current increment worktree/branch/remote branch, other worktrees, other local branches, other remote branches, stashes, open PRs, and anything dirty or untracked. Use one line per finding with path/ref/stash/PR, last activity evidence, unique-work evidence, and action taken:
    - `✅ CLEAN`: no item exists or the checked item is exactly clean/current, such as local `main` matching `origin/main`.
    - `🟢 ACTIVE`: other work has clear evidence of active use right now, such as last activity within 15 minutes, a running command/check, a PR updated within 15 minutes, or an explicit human/agent owner. Do not modify or delete it.
    - `🟡 RECENT/UNKNOWN`: other work was last touched more than 15 minutes ago and no more than 1 hour ago, or ownership/activity is ambiguous. Do not modify or delete it.
    - `🔴 STALE`: other work was last touched more than 1 hour ago, is merged/closed/superseded, or has no unique tree/diff content compared with `origin/main`. Report it as a human cleanup candidate unless it is the current increment's own merged work being removed by this checklist.
    Determine "last activity" from the best available evidence: dirty path mtimes for dirty worktrees, branch or remote-ref committer dates for clean refs, stash dates for stashes, and PR `updatedAt`/check activity for PRs. If the evidence conflicts, choose the more conservative color and explain why.
    **NEVER delete other worktrees, branches, stashes, or dirty files — even if they appear stale.** Instead, include them in the emoji cleanup evidence, including their last activity evidence and whether they contain unique work not in `origin/main`. Let the human decide what to clean up.

### Final Increment Report

After the DoD checklist passes, present one numbered final increment report to
the human. This report is not a checklist item. It is the narrative handoff for
the completed increment.

1. **What you changed**: What files did you change, why and how
2. **Definition of Ready**: The original DoR result, assumptions made, and any DoR refreshes caused by changed scope, authority owner, regression plan, verification plan, or safety profile
3. **PR comment audit**: Current PR review-thread status, old merged PR comment/thread sweep scope, stale actionable comments fixed, original PR comments posted with evidence, and non-actionable comments left alone
4. **Plan deviations**: For every deviation from the approved plan — what changed, why it changed, what was done instead, and whether the new approach is the long-term solution or a temporary measure that needs follow-up
5. **Live incidents**: Which `spec/reference/live-incidents.md` entries were added or updated in this increment, or explicitly say `none`
6. **Top-up recommendations**: Any remaining codebase improvements you'd make. Don't be coy. Engineering effort is free, and this is mission-critical software. Ensure even small issues are brought up, and don't be coy to suggest more ambitious refactoring.
7. **Unfixed items**: Anything you were unable to address in this increment
