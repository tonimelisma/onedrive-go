# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/engine_debug_events.go, internal/sync/engine_scope_invariants.go, internal/sync/engine_shortcuts.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_decisions.go, sync_helpers.go

Implements: R-2.1 [verified], R-2.10.1 [verified], R-2.10.2 [verified], R-2.10.3 [verified], R-2.10.4 [verified], R-2.10.5 [verified], R-2.10.6 [verified], R-2.10.7 [verified], R-2.10.8 [verified], R-2.10.9 [verified], R-2.10.10 [verified], R-2.10.12 [verified], R-2.10.13 [verified], R-2.10.14 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [verified], R-2.10.24 [verified], R-2.10.25 [verified], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified], R-2.10.30 [verified], R-2.10.31 [verified], R-2.10.36 [verified], R-2.10.37 [verified], R-2.10.38 [verified], R-2.10.43 [verified], R-2.10.45 [verified], R-2.10.46 [verified], R-2.14.1 [verified], R-2.14.2 [verified], R-2.14.3 [verified], R-2.14.4 [verified], R-2.14.5 [verified], R-6.3.4 [verified], R-6.4.1 [verified], R-6.4.2 [verified], R-6.4.3 [verified], R-6.6.7 [verified], R-6.6.8 [verified], R-6.6.9 [planned], R-6.6.10 [verified], R-6.6.12 [verified], R-6.7.27 [verified], R-6.8.15 [verified], R-6.8.16 [verified], R-6.10.6 [verified]

## Ownership Contract

- Owns: Single-drive runtime orchestration, watch-mode control state, retry/trial scheduling, permission-scope lifecycle, and result classification.
- Does Not Own: Multi-drive lifecycle, Graph wire normalization, config file policy, or SQLite schema ownership.
- Source of Truth: Durable sync state lives in `SyncStore`; watch-mode runtime state is the single-owner in-memory projection rebuilt from store data plus live observations.
- Allowed Side Effects: Coordinating observers, planner, executor, and store writes; reading local/remote state only through injected collaborators and rooted filesystem capabilities.
- Mutable Runtime Owner: `oneShotRunner` owns one-shot mutable state; `watchRuntime` owns watch-mode mutable state. `Engine` itself remains an immutable dependency container.
- Error Boundary: The engine turns observer and worker outcomes into `ResultDecision`, scope actions, retry scheduling, and success cleanup according to [error-model.md](error-model.md).

## Engine (`engine.go`)

Wires the sync pipeline: observers → buffer → planner → executor → SyncStore. Two entry points:

- `RunOnce()`: one-shot sync. Observes all changes, plans, executes, returns `SyncReport`.
- `RunWatch()`: continuous sync. Flow: `initWatchInfra → bootstrapSync → startObservers → runWatchLoop`.

Watch mode uses a single owner for runtime control state: the watch loop owns
active scopes, result processing, retry timing, trial timing, and dependent
admission. Filesystem events are debounced by the change buffer, remote changes
are polled at `poll_interval` (default 5 minutes), and periodic full
reconciliation runs every 24 hours to detect missed delta deletions.

### Watch Bootstrap

`RunWatch` no longer calls `RunOnce` for its initial sync. Instead:

1. **`initWatchInfra`** creates `watchRuntime`, DepGraph, WorkerPool, Buffer, and tickers, repairs persisted scopes according to the startup policy matrix, and loads watch-owned active scope state from surviving `scope_blocks` plus derived `perm:remote` held rows — but does NOT load baseline or start observers.
2. **`bootstrapSync`** loads baseline, observes initial changes, and dispatches them through the same single-owner watch loop machinery that steady-state mode uses. It runs until the graph is empty and no more bootstrap work is pending.
3. **`startObservers`** launches remote and local observers AFTER bootstrap — they see the post-bootstrap baseline.
4. **`runWatchLoop`** runs the steady-state select loop.

Cancellation is normalized across all four phases: if the watch context is
canceled during proof, startup repair, bootstrap observation, or bootstrap
quiescence, `RunWatch` returns `nil` just like a steady-state watch shutdown.

**Why not RunOnce?** The old approach created throwaway infrastructure for the
initial sync, then created a second watch pipeline. Unified bootstrap creates
the watch pipeline once and reuses it for both the initial sync and steady
state.

`RunOnce` remains a standalone one-shot entry point, but its mutable execution
state now lives on `oneShotRunner`, not on `Engine`.

### Runtime Invariants

The engine relies on a few non-negotiable behavioral invariants:

1. **Bootstrap ordering**: `RunWatch` must finish bootstrap dispatch and
   `waitForQuiescence` before `startObservers` creates either observer.
   Bootstrap actions run through the live watch-mode `DepGraph`, worker pool,
   and single-owner watch loop; observer startup is intentionally delayed
   until that graph reaches zero in-flight actions.
2. **One-shot completion barrier**: `executePlan` must not return its report
   until the worker pool has stopped, the results channel has been closed, and
   the unified engine loop has applied all result side effects. The barrier is
   “workers finished + results drained + side effects applied”, not merely
   “all workers exited”.
3. **Trial policy isolation**: trial results enter the same shared result
   router as normal results, but they branch immediately into explicit
   trial-only policy. Matching-scope evidence may extend the active interval,
   inconclusive outcomes preserve the current interval, and trial failures must
   never feed normal scope detection or overwrite the original scope with a new
   unrelated block.
4. **Trial success release**: a successful trial must clear the persistent
   scope block and make held transient failures retryable immediately via the
   scope-release path. Scope release also triggers an immediate retrier wakeup,
   so re-entry happens through the normal retrier/planner path without
   any new external observation.
5. **Permission recheck release**: both local permission recovery paths
   (`recheckLocalPermissions` and `clearScannerResolvedPermissions`) and the
   remote Graph-backed `recheckPermissions` path clear the active scope and
   release held transient failures through the same scope-release operation.
   Remote permission rechecks fail open: inconclusive API results or stale
   shortcut boundaries clear the stored denial instead of continuing to
   suppress writes on stale evidence.
6. **Scope-block ordering**: when a worker result creates or reinforces a
   scope block, the engine must record the failing action, cascade-record
   blocked dependents, and apply the block before any dependent action is
   re-admitted. The refactor target is single-owner state, but this ordering
   constraint is already required by the current behavior.
7. **Reconciliation handoff**: `runFullReconciliationAsync` may observe and
   commit from its own goroutine, but it must hand work back through the
   engine-owned watch buffer. It must never dispatch directly to `readyCh`
   from the reconciliation goroutine.

### Runtime Ownership

`Engine` is the immutable dependency container plus public entrypoints. It owns
shared collaborators such as config, planner, store, logger, and executor
factories. It does not own mode-specific mutable control state.

Filesystem boundaries are explicit:

- managed repo-state files use `internal/fsroot`
- sync-runtime local filesystem work under one configured sync root uses `internal/synctree`
- arbitrary one-off local paths outside those rooted domains use `internal/localpath`

The rooted filesystem capabilities (`fsroot.Root` and `synctree.Root`) are
deliberately failure-injectable through unexported ops stored on the root
value. This keeps the engine's managed-state writes, conflict-resolution file
operations, and retry/trial local rebuild paths covered by deterministic I/O
fault tests without reintroducing package-level test hooks.

Run-scoped state lives in two dedicated owners:

- `oneShotRunner`: one-shot mutable state (`engineFlow`, depGraph, readyCh,
  shortcut snapshot, result counters)
- `watchRuntime`: watch-mode mutable state (`engineFlow`, active scopes,
  scope detection state, buffer, delete counter, observer references,
  retry/trial timers, reconciliation state, next action ID)

`watchRuntime` keeps its `activeScopes` slice and retry/trial timer pointers
behind unexported runtime accessors. The watch loop still owns the semantics,
but those accessors snapshot or replace the tiny working sets under
per-runtime locks so tests, startup repair, and timer re-arming cannot race on
raw slice or timer-pointer fields.

The shared `engineFlow` object carries the mutable execution state common to
both coordinators: dependency graph, ready channel, shortcut snapshot,
aggregated success/error counters, shared observation helpers, skipped-item
failure maintenance, and coordinator-level result routing. `watchRuntime`
embeds `engineFlow` and adds watch-only state; `oneShotRunner` embeds
`engineFlow` without watch-specific fields.

Policy-heavy behavior lives behind dedicated collaborators owned by the flow:

- `scopeController`: persisted-scope repair, scope activation/release/discard,
  scope-detection application, cascade blocked-failure recording, and
  permission decision application
- `shortcutCoordinator`: shortcut discovery, registration, removal handling,
  delta/full observation, and shortcut-scope reconciliation
- `PermissionHandler`: permission evidence interpreter only; it returns
  decisions that the engine applies through `scopeController`

The controllers are constructed once per `engineFlow` / `watchRuntime` and
reused for the full run. Accessors return the flow-owned collaborator instead
of synthesizing new wrappers, so policy state stays attached to the live
runtime owner even when same-package tests copy `engineFlow` values.

Tests use same-package helpers that construct `watchRuntime` / `engineFlow`
and the flow-backed collaborators directly when they need internal
characterization. Production code does not publish a runtime-registration hook
just so tests can discover live watch state, and test helpers do not synthesize
an `engineFlow` behind the caller's back. The sync test suite is organized by
runtime concern (`RunOnce`, watch, reconcile, conflicts, result/scope flow)
rather than a single mixed `engine_test.go` file.

### Result Classification (`classifyResult()`)

Implements: R-6.8.9 [verified], R-6.8.15 [verified], R-6.7.27 [verified]

Pure function `classifyResult(*WorkerResult) -> ResultDecision`. The classifier
is the single policy entry point for worker results. It returns a decision
object, not a partial tuple, so downstream code does not re-derive policy from
raw HTTP/local error facts.

`ResultDecision` carries:

- `Class`
- `SummaryKey`
- `ScopeKey`
- `ScopeEvidence`
- `Persistence`
- `PermissionFlow`
- `RunScopeDetection`
- `RecordSuccess`
- `TrialHint`
- `IssueType`
- `LogOwner`
- `LogLevel`

The engine-level result classes are direct aliases of the shared
`internal/failures` classes documented in [error-model.md](error-model.md):
success, shutdown, retryable transient, scope-blocking transient, actionable,
and fatal.

`SummaryKey` is the shared sync-domain rendering key exported by
`internal/synctypes`. Classification assigns it once so result routing, store
inspection, and CLI issue/status rendering can all group the same failure
family without re-inspecting raw HTTP status or filesystem errors.

Classification uses `ScopeKeyForStatus(httpStatus, shortcutKey)` as the single
source of truth for HTTP status -> scope key mapping: 401 -> fatal, 403 -> skip
with permission flow, 429 -> scope block `SKThrottleAccount`, 507 -> scope
block `SKQuotaOwn` or `SKQuotaShortcut(key)`, 5xx -> requeue,
408/412/404/423 -> requeue, context cancellation -> shutdown,
`os.ErrPermission` -> skip with local-permission flow, `ErrDiskFull` ->
direct `disk:local` scope activation. HTTP 400 stays on the ordinary
non-retryable path unless a separately evidenced quirk is documented and
implemented.

All downstream result flow consumes the `ResultDecision` only. Trial handling,
failure persistence shape, retry/trial timing, and log ownership are not
re-derived from raw `HTTPStatus`, wrapped errors, or `RetryAfter` headers after
classification. Structured sync logs emit `summary_key` from the same
`ResultDecision`, which gives tests and operators one normalized failure family
across runtime logs, `issues`, and `status`.
Permission-flow and fatal-auth side effects follow the same rule: their
durable writes and scope-activation logs emit the normalized `summary_key`
instead of inventing a second presentation taxonomy for shared-folder blocked,
local-permission, or auth-required paths.

### Scope Detection and Management

Implements: R-2.10.3 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [verified], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified]

`processWorkerResult()` classifies each result and routes it — all cases call `depGraph.Complete()`:

- **success** → `Complete` + `RecordSuccess` (scope window reset) + counter + `clearFailureOnSuccess` (engine owns failure lifecycle exclusively — D-6)
- **requeue** (transient) → `recordFailure` with `retry.ReconcilePolicy().Delay` + `Complete` + `feedScopeDetection` + arm retry timer
- **scopeBlock** (429, 507) → `recordFailure` with `retry.ReconcilePolicy().Delay` + `feedScopeDetection` + `Complete` + `armTrialTimer()` (belt-and-suspenders)
- **skip** (non-retryable) → `handle403` side effect. Confirmed remote read-only
  403s record the triggering blocked write as one held transient row with
  `scope_key='perm:remote:{boundary}'` and skip duplicate file-level failure
  recording; other skips use `recordFailure` with nil delayFn (no
  `next_retry_at`) + `Complete`
- **shutdown** → `Complete` (no failure recorded)
- **fatal** (401) → activate scope block `auth:account` with `timing_source='none'` + `Complete` + terminate one-shot/watch execution without creating a per-path `sync_failures` row

Scope-blocked actions are not held in memory. Instead, `processWorkerResult`
records the failure in `sync_failures` and calls `depGraph.Complete()`. When
the scope clears, `releaseScope` marks held descendants retryable immediately
and kicks the retry sweep so they re-enter via the engine-owned retry work
path and the normal planner/DepGraph flow. `perm:remote` is special: it has no
persisted `scope_blocks` row, so the watch runtime rebuilds that scope from
held blocked-write rows only.

Trial result routing uses the shared `processResult()` entry with explicit
trial policy. The `TrialScopeKey ScopeKey` from `WorkerResult` identifies the
currently blocked scope. Trial outcomes are:

- success -> `releaseScope(scopeKey)`
- matching-scope persistence evidence -> `extendScopeTrial(scopeKey, ...)`
- inconclusive candidate failure -> `preserveScopeTrial(scopeKey)` plus
  candidate-specific handling (for example item failure replacement, permission
  scope activation, or disk-scope re-home)
- fatal unauthorized -> activate `auth:account` and terminate the current
  one-shot pass or watch session without mutating the original trial scope

Scope detection is intentionally NOT called for trial failures — the scope is
already blocked, and re-detecting would overwrite or duplicate the original
state.

**Trial dispatch**: `runTrialDispatch()` is called from the watch loop when the
trial timer fires. It snapshots due scope keys from the watch-owned
`activeScopes` working set through the runtime accessor, then iterates each
exactly once. For each scope it uses
`PickTrialCandidate` from `sync_failures` to find an actual blocked item,
rebuilds planner input from durable state plus current local truth, and sends
that through the normal planner/admission path as an explicit internal trial
work request. No bespoke `reobserve` API path remains. If current local
observation now rejects the candidate (for example, oversized file), the held
row is converted into an actionable failure and trial selection continues. If
the rebuilt candidate resolves silently, the engine clears the stale failure
row using the same normalized drive identity it would use to re-record it, so
legacy zero-drive rows cannot survive by missing the `(path, drive_id)` delete.
If no usable trial candidate exists, the engine preserves the scope at the current
interval instead of auto-releasing it. On successful dispatch, the trial
interval is NOT extended until the worker result arrives. Trial actions are
marked `IsTrial=true` with `TrialScopeKey` set.

**Retry sweep**: Retry is integrated directly into the watch loop via
`runRetrierSweep()` — no separate goroutine. The watch loop's select includes
a retry timer that triggers sweeps of `sync_failures` for items whose
`next_retry_at` has expired. Each sweep is batch-limited with zero-delay re-arm
when the batch is full. Items are checked via `isFailureResolved()` before
redispatch (D-11 fix: prevents re-dispatching items whose underlying condition
has resolved). Redispatch rebuilds planner input from durable state via the
retry/trial rebuild path (full `remote_state` for downloads, `ObserveSinglePath`
for upload-side local truth) and sends it through the same engine-owned planner
work path that normal observation uses after buffering. Current local
validation failures are converted into actionable failures instead of being
silently dropped.

`feedScopeDetection()` feeds results into `ScopeState.UpdateScope()`. When a
threshold is crossed, it creates or refreshes a persisted scope block via
`activateScope()` and then re-arms trial timing. Direct scopes such as
`disk:local` bypass the sliding window and activate immediately from the
classifier's `ResultDecision`.

The engine owns all completion decisions — workers are pure executors. In
watch mode the single watch loop on `watchRuntime` uses an actor-with-outbox
pattern: results, buffer flushes, trial ticks, retry ticks, and dependent
admission are processed single-threaded within one select loop, and ready
actions are collected into an outbox slice before being sent to `readyCh`.
This prevents deadlock that would occur if result handling tried to
synchronously send to a full `readyCh` while workers tried to synchronously
send to a full results channel. One-shot mode keeps a separate coordinator,
`oneShotRunner.runResultsLoop`, with the same shared result-routing semantics
but without watch-only mutable state.

`recordFailure()` writes explicit durable semantics into `sync_failures`:

- ordinary failures use `failure_role='item'`
- scope-blocked descendants use `failure_role='held'`
- scope-defining actionable rows use `failure_role='boundary'`

`SyncStore.RecordFailure()` persists the row transactionally and computes
`next_retry_at` only when the engine provides a retry delay function.

**Scope lifecycle terminology**:
- `activateScope()` means "this blocking condition is now active" — persist the scope row, refresh watch-mode active scope state, and arm trial timing if the scope is trial-driven.
- `extendScopeTrial()` means "the same scope is still blocked" — update `next_trial_at`, `trial_interval`, and `trial_count` for an existing scope.
- `preserveScopeTrial()` means "the original scope is still plausible, but this candidate did not prove it" — update `next_trial_at`, set `preserve_until`, and keep `trial_count` unchanged.
- `activateAuthScope()` means "account authorization is invalid" — persist `auth:account` with `timing_source='none'` and zero trial metadata. Auth is a blocking condition, not a trial-driven scope.
- `releaseScope()` means "the blocking condition resolved" — delete the persisted scope row when one exists, delete any legacy boundary row for that scope, and make held descendants retryable immediately.
- `discardScope()` means "the blocked subtree/work is gone" — delete the persisted scope row when one exists and delete all scoped failure rows without retrying them.

### Startup Repair

Before watch admission begins, and before one-shot observation starts, the
engine runs `repairPersistedScopes()` against durable store state.

Policy matrix:

- `perm:dir` survives only while a matching `boundary` row exists
- `perm:remote` survives only while at least one held blocked-write row still exists; any legacy persisted `perm:remote` scope row or boundary row is normalized away at startup
- `quota:own` and `quota:shortcut:*` survive only while at least one scoped failure row exists
- scoped-failure-backed scopes survive restart when they still have scoped failures or when `preserve_until` is still in the future
- `throttle:account` and `service` survive restart only when `timing_source='server_retry_after'`; expired deadlines trial immediately rather than auto-releasing
- `auth:account` is revalidated exactly once at startup via `DriveVerifier.Drive(ctx, driveID)`; success releases it, unauthorized keeps it and aborts startup, and other probe failures leave it untouched and abort startup
- `disk:local` is revalidated from current local free-space truth and refreshed or released accordingly

The repair pass runs before `activeScopes` is loaded, so the watch loop starts
from repaired durable state rather than trusting stale persisted scope rows.

### ScopeState

Implements: R-2.10.35 [verified], R-2.10.36 [verified], R-2.10.37 [verified]

In-memory data structure in `scope.go`: sliding windows (`ScopeKey` →
`slidingWindow`) for scope escalation detection. All keys are typed `ScopeKey`
structs (see sync-execution.md § ScopeKey Type System). Engine-internal — no
cross-engine coordination (each engine discovers independently). Active scope
runtime state is owned by the watch loop, while persisted scope blocks live in
the `scope_blocks` table for restart/recovery. `perm:remote` is the exception:
its watch-time scope is rebuilt from held blocked-write rows, not from
`scope_blocks`.

### `disk:local` Scope Block

Implements: R-2.10.43 [verified]

Scope key `SKDiskLocal` is created by `classifyResult()` when a download fails
with `ErrDiskFull` (deterministic signal — immediate, no sliding window).
Unlike `SKThrottleAccount` and `SKService` which block ALL actions (via
`ScopeKey.IsGlobal()`), `SKDiskLocal` blocks downloads only —
`ScopeKey.BlocksAction()` returns true only for `ActionDownload`. Uploads,
deletes, and moves continue because they either free space or don't consume it.
Admission priority still places `SKDiskLocal` between `SKService` and
`SKQuotaOwn`. `disk:local` uses its own trial curve: 5-minute initial
interval, 2x backoff, 1-hour max. Startup repair revalidates current free
space instead of trusting stale persisted timing.

### Scanner ScanResult Contract

Implements: R-2.11.5 [verified], R-2.10.2 [verified]

Scanner returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. Engine processes skipped items via two methods. The engine also threads platform-derived local observation rules into the observer, so drive-type-specific naming constraints such as SharePoint root-level `forms` are enforced in full scans, watch mode, and retry/trial single-path reconstruction:

- **`recordSkippedItems(skipped []SkippedItem)`** — Groups skipped items by reason, batch-upserts to `sync_failures` as actionable failures. Uses aggregated logging: when >10 items share the same reason, logs 1 WARN summary with count and sample paths, individual paths at DEBUG. When <=10 items, logs each as an individual WARN.
- **`clearResolvedSkippedItems(skipped []SkippedItem)`** — Deletes `sync_failures` entries for scanner-detectable file-scoped actionable issues that are no longer skipped (e.g., user renamed a previously invalid file or a one-off hash panic no longer reproduces). Compares current skipped paths against recorded failures and removes stale entries.

### Aggregated Logging

Implements: R-6.6.7 [verified], R-6.6.8 [verified], R-6.6.9 [planned], R-6.6.10 [verified], R-6.6.12 [verified]

When >10 items share the same warning category, log 1 WARN summary with count and sample paths + individual paths at DEBUG. When <=10 items, log each as an individual WARN. This pattern is implemented in `recordSkippedItems()` for scanner-time validation failures. Transient retries at DEBUG, resolved at INFO, exhausted at WARN. Extends to execution-time transient failures: when >10 transient failures of the same `issue_type` exhaust their retry budget in a single sync pass, aggregate into 1 WARN summary with count, individual paths at DEBUG (R-6.6.12).

### Local Permission Handling

Implements: R-2.10.12 [verified], R-2.10.13 [verified], R-2.10.10 [verified]

`PermissionHandler` is a policy layer. It does not mutate engine state
directly. Local permission detection returns explicit decisions that the engine
applies through `scopeController`, which owns the actual lifecycle mutation.

`os.ErrPermission` -> check parent directory accessibility via
`handleLocalPermission()`. Inaccessible directory: return an
`activate_boundary_scope` decision for `SKPermDir(path)`. Accessible
directory: return a `record_file_failure` decision. Directory-level issues are
rechecked at the start of each sync pass via `recheckLocalPermissions()`.

**Scanner-driven auto-clear** (R-2.10.10): `clearScannerResolvedPermissions()`
returns `PermissionRecheckDecision` values when the scanner proves previously
blocked paths are accessible again. File-level failures are cleared directly.
Directory-level permission scopes are released through `scopeController.releaseScope()`.

### Remote Shared Permission Handling

Implements: R-2.10.9 [verified], R-2.10.17 [verified], R-2.10.23 [verified], R-2.10.24 [verified], R-2.10.25 [verified], R-2.10.38 [verified], R-2.14.1 [verified], R-2.14.2 [verified], R-2.14.3 [verified], R-2.14.4 [verified]

`handle403()` is the single remote-permission entry point for write failures on
shared content. Like the local permission path, it returns a decision for the
engine to apply through `scopeController`:

- First it checks whether the failing path is already under an active derived `perm:remote:{localPath}` boundary. If so, it short-circuits without another Graph permission walk.
- Otherwise it resolves the relevant shortcut, calls `ListItemPermissions` on the target folder, and confirms whether the 403 is a real write denial or a transient/inconclusive failure.
- On confirmed denial it walks upward, still using `ListItemPermissions`, to find the highest denied ancestor but never above the shortcut root.
- On confirmed denial it records the triggering blocked write as one held
  transient row with `scope_key='perm:remote:{boundary}'`. That held row is
  the durable authority for the derived scope.

`perm:remote` is recursive and download-only while blocked write intent exists:

- uploads, folder creates, remote moves, and remote deletes are blocked for the boundary path and every descendant
- downloads continue so the subtree remains readable and delta/reconciliation can keep it current
- if the last blocked write disappears, the derived scope is forgotten immediately instead of leaving behind a remembered read-only boundary

`recheckPermissions()` revisits visible derived `perm:remote` scopes at the start
of each sync pass and returns explicit release/keep decisions:

- writable again -> `releaseScope`
- Graph/API failure or stale shortcut boundary -> fail open via `releaseScope`
- still denied -> keep the boundary active

`issues retry <blocked-path>` requests a manual trial for that exact held row.
The retry path is candidate-specific: the engine dispatches that row as a
trial, success releases the scope, and repeated same-scope 403 keeps it held.

Shortcut removal also returns explicit discard decisions for any
`perm:remote:*` boundary under the removed shortcut plus the matching
`quota:shortcut:*` scope. Removed shortcuts discard blocked work instead of
releasing it back into dispatch, and watch mode rebuilds active scopes from the
post-removal durable state immediately.

### Planned: Observation Suppression

Implements: R-2.10.30 [verified], R-2.10.31 [verified]

During `SKThrottleAccount` or `SKService` scope block (detected via `ScopeKey.IsGlobal()`), suppress shortcut observation polling (wastes API calls). During `quota:shortcut:*` block, observation continues (read-only).

Observation suppression (`scopeController.isObservationSuppressed()`) suppresses the entire `shortcutCoordinator.processShortcuts()` call, which includes both shortcut discovery and delta polling. Also suppresses `recheckPermissions()` API calls since those are equally wasteful during an outage. Suppressing discovery is acceptable — new shortcuts during an outage would fail immediately anyway. Discovery resumes when the scope clears. Local permission rechecks (`recheckLocalPermissions`) proceed regardless since they are filesystem-only.

**Trial dispatch correctness**: `runTrialDispatch()` uses due-scope snapshot iteration — each due scope is visited exactly once per tick, making infinite iteration structurally impossible. On successful dispatch, the trial interval is NOT mutated (awaiting worker result). When no usable candidate exists, the engine preserves the scope at the same interval instead of auto-releasing it. The timer re-arms after each trial decision.

**Trial path separation**: `processWorkerResult()` checks `IsTrial` and routes through explicit trial policy inside the shared result router. This eliminates the prior fragile pattern where all trial failures collapsed into one branch. Success releases the scope, matching evidence extends it, inconclusive outcomes preserve it, and scope detection is never called for trial results because the original scope is already blocked.

**External perm:dir clearance**: `handleExternalChanges()` checks whether
`local_permission_denied` failures were cleared via CLI (`issues clear`).
Iterates the watch loop's `activeScopes`, filters via `ScopeKey.IsPermDir()`,
and releases cleared blocks via `releaseScope()`.

**Watch mode summary**: `logWatchSummary()` logs a periodic one-liner at the recheck interval (10s) showing actionable issue counts by type. Only logs when the count changes to avoid noisy output.

### Failure Logging (R-6.6.8, R-6.6.10, R-6.6.12)

Implements: R-6.6.8 [verified], R-6.6.10 [verified], R-6.6.12 [verified]

Sync failure logging follows a tiered approach matching CLAUDE.md policy — individual items at DEBUG, aggregated summaries at WARN:

- **Per-failure DEBUG**: `recordFailure()` logs each failure with path, action, HTTP status, error, and scope_key. This is the per-item detail (matching CLAUDE.md Debug = "file read/write").
- **Scope block WARN**: `applyScopeBlock()` logs when a scope block activates with scope_key, issue_type, and trial_interval. This is a degraded-but-recoverable event (matching CLAUDE.md Warn).
- **Scope release INFO**: `releaseScope()` logs when a scope block clears. This is a lifecycle state transition (matching CLAUDE.md Info).
- **Trial preserve/extend DEBUG**: trial routing logs whether a trial extended or preserved a scope, including scope_key and interval. This is retry detail.
- **End-of-pass summary**: `logFailureSummary()` aggregates syncErrors by error message prefix. Groups with >10 items get one WARN with count + 3 samples. Groups with ≤10 items get per-item WARN. Mirrors the scanner aggregation in `recordSkippedItems()` (R-6.6.7). Called at end of `executePlan()`.
- **IssueType population**: `recordFailure()` derives issue_type from HTTP status via `issueTypeForHTTPStatus()` and stores it in sync_failures for display grouping.

### Shortcut Integration (`engine_shortcuts.go`)

`shortcutCoordinator` detects shortcuts to shared folders in the delta stream,
creates additional delta scopes for shared-folder observation, and owns
shortcut removal side effects. Shortcut removal clears any persisted
`perm:remote` scope under the removed shortcut and discards its held failures,
preventing stale recursive write suppression after the share disappears.

## CLI / Engine Boundary (`sync_helpers.go`)

`sync_helpers.go` is the root-package bridge into the single-drive engine. It
constructs `sync.Engine` instances for engine-facing flows such as conflict
resolution and verification, while the multi-drive `sync` command itself is
governed by `sync-control-plane.md`.

## Watch Mode Behavior

- SIGHUP → reload `config.toml`, apply drive changes immediately
- PID file with flock for single-instance enforcement
- Two-signal shutdown (drain, then force)
- Periodic full reconciliation (default 24h, async — see below)

### Async Full Reconciliation

`runFullReconciliationAsync` spawns a goroutine for full delta enumeration + orphan detection. Non-blocking — the watch loop continues processing events while reconciliation runs.

**Event flow**: The reconciliation goroutine never dispatches directly. It
observes and commits, packages the resulting events plus refreshed shortcut
snapshot into a `reconcileResult`, and sends that result back over the
watch-owned `reconcileResults` channel. The watch loop then applies the result
on its own goroutine by updating `shortcuts`, clearing `reconcileActive`, and
feeding any resulting events into `buf.Add()`.

**Concurrency guard**: `watchRuntime.reconcileActive` is owned by the watch loop.
`runFullReconciliationAsync` sets it before launching the goroutine and skips a
new launch while it remains true. The goroutine never clears the flag directly;
only the watch loop clears it when it applies the returned `reconcileResult`.
This preserves single-owner engine state while still allowing asynchronous
observation/commit work.

**Shutdown awareness**: After `CommitObservation` succeeds, a `ctx.Err()` check detects shutdown — if the context is canceled, the function returns immediately without feeding events to the buffer (the watch loop is also shutting down and won't process them). Next startup re-observes idempotently. Error logging during delta observation is also suppressed when `ctx.Err() != nil` — context cancellation during shutdown is not a terminal failure.

**Duration logging**: Both completion paths (events found / no changes) include `slog.Duration("duration", ...)` in the completion log. Operators can grep for duration to assess reconciliation performance on large drives.

### Watch-Mode Big-Delete Protection (`delete_counter.go`)

Implements: R-6.4.2 [verified], R-6.4.3 [verified]

In watch mode, the planner-level big-delete check is disabled (`threshold=MaxInt32`) because 2-second debounced batches would fragment a mass delete across many small batches, each below threshold. Instead, a rolling-window `deleteCounter` accumulates planned deletes across batches.

**Counter**: `deleteCounter` tracks timestamps of planned delete actions within a configurable rolling window (5 minutes). When the cumulative count exceeds `big_delete_threshold`, the counter latches `held=true`. Expired entries (older than the window) are pruned on each `Add()` call.

**Flow in `processBatch()`**: After `planner.Plan()` returns, the engine counts `ActionLocalDelete` + `ActionRemoteDelete` actions and calls `counter.Add(count)`. If `counter.IsHeld()`:
1. Delete actions are filtered out of the plan (via `applyDeleteCounter()`)
2. Non-delete actions continue to DepGraph and execute normally
3. Held deletes are recorded as `sync_failures` rows with `issue_type=big_delete_held` via `UpsertActionableFailures()`

**CLI notification**: `issues list` shows held deletes in a dedicated "HELD DELETES" section. User approves via `issues clear --all` (or `issues clear <path>` for individual files).

**External change detection**: A 10-second `recheckTicker` in the `RunWatch()` select loop runs `PRAGMA data_version` to detect CLI writes. When the data version changes, `handleExternalChanges()` queries `ListSyncFailuresByIssueType(IssueBigDeleteHeld)`. If zero rows remain (user cleared them all), calls `counter.Release()`. On the next observation cycle, deletions are re-observed and dispatched normally.

**Startup cleanup**: `RunWatch()` clears stale `big_delete_held` entries from prior daemon sessions, since the in-memory counter resets on restart.

**Force mode**: `--force` skips counter creation (`deleteCounter` stays nil), so no watch-mode big-delete protection applies.

### Rationale

- **Crash recovery requires explicit bridging**: On restart after crash, [`internal/syncrecovery/recovery.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncrecovery/recovery.go) resets `remote_state` items stuck mid-execution to pending or deleted, AND creates `sync_failures` entries so the engine retry sweep can rediscover them. This is necessary because the delta token was already advanced before execution — items that crashed mid-execution won't appear in the next delta response. The planner is idempotent for items that DO appear in observations, but crash recovery items need the `sync_failures` → retrier → planner path.
- **Keep control plane separate from the engine**: multi-drive coordination now lives in `internal/multisync`, leaving `internal/sync` focused on the single-drive runtime and conflict APIs.
