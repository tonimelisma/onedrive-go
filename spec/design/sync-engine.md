# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/engine_config.go, internal/sync/debug_event_sink.go, internal/sync/engine_debug_events.go, internal/sync/engine_scope_invariants.go, internal/sync/engine_shortcuts.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_decisions.go, sync_helpers.go

Implements: R-2.1 [verified], R-2.3.11 [verified], R-2.4.4 [verified], R-2.4.5 [verified], R-2.8.3 [verified], R-2.8.5 [verified], R-2.10.1 [verified], R-2.10.2 [verified], R-2.10.3 [verified], R-2.10.4 [verified], R-2.10.5 [verified], R-2.10.6 [verified], R-2.10.7 [verified], R-2.10.8 [verified], R-2.10.9 [verified], R-2.10.10 [verified], R-2.10.12 [verified], R-2.10.13 [verified], R-2.10.14 [verified], R-2.10.17 [verified], R-2.10.18 [verified], R-2.10.19 [verified], R-2.10.20 [verified], R-2.10.23 [verified], R-2.10.24 [verified], R-2.10.25 [verified], R-2.10.26 [verified], R-2.10.28 [verified], R-2.10.29 [verified], R-2.10.30 [verified], R-2.10.31 [verified], R-2.10.36 [verified], R-2.10.37 [verified], R-2.10.38 [verified], R-2.10.43 [verified], R-2.10.45 [verified], R-2.10.46 [verified], R-2.14.1 [verified], R-2.14.2 [verified], R-2.14.3 [verified], R-2.14.4 [verified], R-2.14.5 [verified], R-2.16.2 [verified], R-2.16.3 [verified], R-6.3.4 [verified], R-6.3.5 [verified], R-6.4.1 [verified], R-6.4.2 [verified], R-6.4.3 [verified], R-6.6.7 [verified], R-6.6.8 [verified], R-6.6.9 [verified], R-6.6.10 [verified], R-6.6.12 [verified], R-6.6.13 [verified], R-6.7.27 [verified], R-6.8.15 [verified], R-6.8.16 [verified], R-6.10.6 [verified], R-6.10.10 [verified], R-6.10.13 [verified]

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

`RunOnce()` intentionally keeps the incremental one-shot contract simple:
observe once, plan once, execute once. It does not add hidden settle loops just
because a different Graph surface already shows fresher state. OneDrive delta
visibility is eventually consistent; a direct REST item read elsewhere is not
proof that the current incremental delta pass is wrong. The explicit one-shot
freshness escape hatch remains `sync --full`, which enumerates remote truth
instead of trusting a single incremental delta pass.

Watch mode uses a single owner for runtime control state: the watch loop owns
active scopes, result processing, retry timing, trial timing, and dependent
admission. Filesystem events are debounced by the change buffer, remote changes
arrive through remote delta observation that is wake-driven by OneDrive
Socket.IO when `websocket = true` on eligible full-drive sessions, with
`poll_interval` retained as the fallback polling cadence and periodic full
reconciliation every 24 hours to detect missed delta deletions.

`BuildEngineConfig(session, resolved, verifyDrive, logger)` is the single
authority for translating a resolved drive plus authenticated session into
`synctypes.EngineConfig`. CLI single-drive setup and multisync orchestration
both call that one builder so watch-only dependencies such as
`SocketIOFetcher`, local observation filters/rules, and drive verification
cannot silently diverge between entrypoints.

Scoped-root watch sessions and `sync_paths`-scoped primary-drive watch
sessions do not use Socket.IO in v1. They continue on the existing polling
path because the repository only has verified Microsoft support for drive-root
`subscriptions/socketIo`.

### Bidirectional Sync Scope

`sync_paths` and `ignore_marker` are engine-owned sync-scope policy, not
planner policy and not raw remote-observation filtering. At the start of each
run or watch bootstrap, the engine asks local observation to build one
effective scope snapshot from normalized configured paths plus locally
discovered marker directories. The engine then:

- applies that snapshot to remote observations before they reach the planner
- builds one pure `ScopeSession` / `ObservationSessionPlan` pair that both one-shot
  and watch execution paths consume
- persists the snapshot plus observation metadata in the dedicated
  `scope_state` row, then asks the store to apply row-level
  `remote_state.sync_status = filtered` transitions atomically
- predicts the next effective scope generation from the diff between
  persisted and live scope truth instead of letting each runtime path derive
  that independently
- forces full reconciliation only when scope expansion enters the drive root;
  narrower expansions use targeted re-entry reconciliation
- routes both primary path scopes and shortcut scopes through the same
  target-level delta/enumerate execution helpers so scoped observation logic
  no longer forks by scope type

When `sync_paths` is non-empty, primary remote observation no longer starts
from the whole drive by default. The engine resolves the minimal current set
of observable remote folder scopes that can cover the selected paths, then:

- uses folder-scoped delta on personal drives
- falls back to recursive subtree enumeration on business/sharepoint scopes
- falls back to recursive subtree enumeration for a personal scoped path too
  when folder-scoped delta is temporarily unavailable for that folder, instead
  of failing the whole sync pass during initial path bootstrap
- keeps exact-file selection as an engine-side policy by observing the nearest
  covering folder scope and then reapplying the effective scope snapshot
- runs targeted re-entry reconciliation for newly-entered paths before normal
  planning, instead of forcing an unrelated whole-drive resync

Boundary-crossing remote moves are reclassified at the engine boundary: move
into scope becomes create-on-entry, move out of scope becomes delete-on-exit,
and move entirely outside scope is dropped from planning while the
corresponding `remote_state` row stays `filtered`.

`ScopeSession` is the engine's pure scope-planning boundary. It owns the
current effective snapshot, the persisted snapshot from `scope_state`, the
entered/exited diff between them, and the predicted current generation.

`ObservationSessionPlan` is the pure observation-side result of that session.
It owns:

- `PrimaryPhase`: always populated for session-backed plans, including
  full-drive root-delta sessions. The phase owns explicit driver
  (`root_delta`, `scoped_root`, `scoped_targets`), dispatch
  (`single_batch`, `sequential_targets`), batch error policy, delta fallback,
  and token-commit policy.
- `ShortcutPhase`: resolved shortcut targets plus explicit dispatch, isolated
  per-target error policy, and shortcut token-commit policy
- `Reentry`: the pending primary-scope re-entry decision
- `Hash`: the persisted primary-scope-only plan fingerprint written to
  `scope_state`

The primary and shortcut phases share one coordinator shape, but they do not
share the same failure policy. Primary observation still fails as a batch and
may fall back from delta to recursive enumeration when a recursive lister is
available. Shortcut observation still isolates per-target failures, never
silently switches from delta to enumerate, and commits successful shortcut
delta tokens only after the shortcut phase finishes. Shortcut-only callers
invoke the same planner without a `ScopeSession`, which means primary scope
resolution and primary re-entry planning are skipped entirely; shortcut
observation must not depend on current `sync_paths` lookup success.

`engine_scope_session.go` owns only pure planning. Execution policy lives in
phase executors: one-shot/reconcile paths call the generic observation-phase
executor, while watch startup consumes `PrimaryPhase.Driver` through one
watch-side executor instead of branching on ad hoc top-level watch strategy
fields in orchestration code.

Root-delta watch is also part of the same coordinator flow now. The engine
still commits primary-drive observations atomically inside
`RemoteObserver.Watch`, but once that commit succeeds it runs one engine-owned
post-primary batch handler that consumes `ChangeShortcut`, updates durable
shortcut truth, builds a shortcut follow-up phase, observes shortcut content,
reapplies remote scope to that content, and only then emits the final batch to
the watch buffer. Scoped-root watch, scoped-target watch, one-shot sync, and
periodic reconciliation use that same shortcut-batch helper; steady-state
watch no longer waits for periodic reconciliation to discover shortcut
content.

That post-primary coordinator is intentionally split into two ownership
boundaries inside `engine_shortcuts.go`:

- `applyShortcutBatchMutations(...)` owns only durable shortcut truth changes
  (register/remove shortcuts and delete per-shortcut delta tokens)
- `loadShortcutSnapshot(...)` owns only loading the current durable shortcut
  snapshot from the store
- `observeShortcutFollowUp(...)` owns only shortcut-content observation
  (collision detection, suppressed-target filtering, shortcut-only
  `ObservationSessionPlan` construction, and shortcut-phase execution)

No engine path is allowed to both mutate shortcut truth and observe shortcut
content in the same helper anymore. `processCommittedPrimaryBatch(...)` is the
single engine-owned composition point that filters visible primary events,
applies durable shortcut mutations, reloads and publishes the runtime shortcut
snapshot when needed, then runs shortcut follow-up observation after primary
observations are durably committed.

Dry-run one-shot passes never persist deferred delta tokens. `observeRemoteChanges`
clears pending tokens before returning to `RunOnce`, so planner-only previews
cannot advance drive-root, scoped-root, scoped-target, or targeted re-entry
observation cursors.

One-shot startup, watch startup, watch-time scope changes, shortcut content
observation, and shortcut reconciliation all consume this same plan shape so
scope policy is no longer split across separate primary-vs-shortcut builders.

Observation planning and execution now fail fast on structural misuse in all
builds, not only under debug/test invariants. `BuildObservationSessionPlan`,
`executeObservationPhase`, `startPrimaryWatchPhase`, and `scopeStateRecord`
validate that session-backed plans always carry an explicit primary phase,
driver/dispatch combinations stay legal, shortcut phases cannot enable
delta-to-enumerate fallback, and persisted observation metadata is derived
only from the primary phase.

Upload-only runs and upload-only watch sessions still rebuild and persist local
scope truth, but they intentionally defer remote re-entry reconciliation on
scope expansion until a later mode that is allowed to observe/download remote
state. This preserves the “do not invent remote truth in upload-only mode”
authority boundary.

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
   engine-owned watch buffer. It must never dispatch directly to `dispatchCh`
   from the reconciliation goroutine.
8. **Shutdown admission seal**: when watch-mode cancellation is observed, the
   runtime must transition from `running` to `draining` exactly once, stop
   retry/trial wakeups, stop recheck/reconcile admission, complete any
   not-yet-dispatched outbox actions as shutdown, and never admit new work
   afterward. Already-dispatched actions may still finish if workers produce
   results before they exit.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Watch bootstrap reaches quiescence before local or remote observers start. | `TestPhase0_RunWatch_BootstrapCompletesBeforeLocalObserverStarts`, `TestPhase0_RunWatch_BootstrapCompletesBeforeRemoteObserverStarts` |
| Bootstrap subroutines stay directly testable for quiescence, no-change startup, change-driven startup, and crash-recovery cleanup without helper-loop shims. | `TestWaitForQuiescence_EmptyGraph`, `TestWaitForQuiescence_ContextCancel`, `TestBootstrapSync_NoChanges`, `TestBootstrapSync_WithChanges`, `TestBootstrapSync_CrashRecovery_MixedDeletingCandidates` |
| Watch shutdown seals admission, stops retry/trial wake handling, and drops reconcile handoff after drain begins. | `TestRunWatch_ShutdownStopsRetryAndTrialTimers`, `TestRunWatch_ShutdownDropsReconcileResult`, `TestRunFullReconciliationAsync_ShutdownAfterCommit` |
| Cancellation wins over fatal observer-exit shutdown races, and fallback waits honor cancellation without wall-clock sleeps. | `TestRunWatch_ContextCancel`, `TestRunWatch_CancellationWinsOverFinalObserverExit`, `TestRunWatch_FallbackSleepHonorsCancellation` |

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

- `oneShotRunner`: one-shot mutable state (`engineFlow`, depGraph, dispatchCh,
  shortcut snapshot, result counters)
- `watchRuntime`: watch-mode mutable state (`engineFlow`, active scopes,
  scope detection state, buffer, delete counter, observer references,
  retry/trial timers, reconciliation state, next action ID, runtime phase)

`watchRuntime` keeps its `activeScopes` slice and retry/trial timer pointers
behind unexported runtime accessors. The watch loop still owns the semantics,
but those accessors snapshot or replace the tiny working sets under
per-runtime locks so tests, startup repair, and timer re-arming cannot race on
raw slice or timer-pointer fields.

Time boundaries are also engine-owned. `Engine.afterFunc`,
`Engine.newTicker`, `Engine.sleepFn`, and `Engine.jitterFn` are the only
production timer, ticker, sleep, and jitter constructors used by the watch
runtime, bootstrap quiescence loop, retry/trial scheduling, and periodic
full-scan fallback. Production uses wall-clock implementations; same-package
tests inject deterministic time without rewriting the runtime logic.

The shared `engineFlow` object carries the mutable execution state common to
both coordinators: dependency graph, dispatch channel, shortcut snapshot,
aggregated success/error counters, shared observation helpers, skipped-item
failure maintenance, and coordinator-level result routing. `watchRuntime`
embeds `engineFlow` and adds watch-only state; `oneShotRunner` embeds
`engineFlow` without watch-specific fields.

Watch-mode websocket diagnostics remain internal-only. `watchRuntime`
translates `syncobserve.SocketIOLifecycleEvent` values into engine debug
events with drive identity attached, and the CLI may opt into a hidden
newline-delimited JSON debug-event sink (`ONEDRIVE_TEST_DEBUG_EVENTS_PATH`)
for E2E/runtime proof. This sink is test infrastructure, not a user-facing
status surface or durable state authority.

Policy-heavy behavior lives behind dedicated collaborators owned by the flow:

- `scopeController`: persisted-scope repair, scope activation/release/discard,
  scope-detection application, cascade blocked-failure recording, and
  permission decision application
- `shortcutCoordinator`: shortcut discovery, registration, removal handling,
  shortcut-target planning, durable shortcut mutation, shortcut follow-up
  observation, and shortcut-scope reconciliation
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

Classification uses `ScopeKeyForResult(httpStatus, targetDriveID, shortcutKey)`
as the single source of truth for HTTP status -> scope key mapping: 401 ->
fatal, 403 -> skip with permission flow, 429 -> scope block
`SKThrottleDrive(targetDriveID)` or `SKThrottleShared(shortcutKey)`, 507 ->
scope block `SKQuotaOwn` or `SKQuotaShortcut(key)`, 5xx -> requeue,
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

Result handling is intentionally split into three layers inside the engine:

- `classifyResult()` is the pure classification boundary.
- Class-based ready routing (`routeReadyForClass`) decides how dependents are
  admitted or completed from the classified result only.
- Post-routing effect application (`applySuccessEffects`,
  `applyOrdinaryFailureEffects`, and explicit trial/fatal handlers) performs
  persistence, scope actions, timer arming, and logging without re-inspecting
  raw transport facts.

That split keeps raw worker evidence at the classification boundary and makes
the remaining flow consume an explicit policy object instead of re-deriving
behavior ad hoc.

Structured sync outcome logs use one schema across classified paths:
`summary_key`, `failure_class`, `log_owner`, `scope_key`, `drive_id`,
`action_type`, `path`, `run_id`, and `action_id`, with `shortcut_key`,
`http_status`, and `trial_scope_key` included when they exist. `run_id` is
stable for one engine run, and `action_id` follows the tracked action through
the runtime flow.

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
actions are collected into an outbox slice before being sent to `dispatchCh`.
This prevents deadlock that would occur if result handling tried to
synchronously send to a full `dispatchCh` while workers tried to synchronously
send to a full results channel. One-shot mode keeps a separate coordinator,
`oneShotRunner.runResultsLoop`, with the same shared result-routing semantics
but without watch-only mutable state.

The watch loop has two explicit phases:

- `running`: normal admission is active. Buffer batches, retry/trial wakes,
  recheck ticks, reconcile ticks, worker results, and observer exits all feed
  the shared single-owner loop.
- `draining`: entered once after `ctx.Done()`. New admission is sealed by
  stopping retry/trial timers, disabling batch/recheck/reconcile sources, and
  completing the local outbox as shutdown. The loop keeps consuming worker
  results, observer exits, and reconcile-result bookkeeping until the runtime
  settles. Once draining starts, terminal observer exit is no longer fatal and
  reconcile results are dropped instead of feeding new work back into the
  observation buffer.

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
- `throttle:target:*` and `service` survive restart only when `timing_source='server_retry_after'`; expired deadlines trial immediately rather than auto-releasing
- legacy persisted `throttle:account` rows are released during startup repair because they do not encode the throttled remote boundary
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
Unlike target-scoped throttles and `SKService`, `SKDiskLocal` blocks downloads only —
`ScopeKey.BlocksAction()` returns true only for `ActionDownload`. Uploads,
deletes, and moves continue because they either free space or don't consume it.
Admission priority still places `SKDiskLocal` between `SKService` and
`SKQuotaOwn`. `disk:local` uses its own trial curve: 5-minute initial
interval, 2x backoff, 1-hour max. Startup repair revalidates current free
space instead of trusting stale persisted timing.

### Scanner ScanResult Contract

Implements: R-2.11.5 [verified], R-2.10.2 [verified]

Scanner returns `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` instead of `[]ChangeEvent`. Engine processes skipped items via two methods. The engine also threads platform-derived local observation rules into the observer, so drive-type-specific naming constraints such as SharePoint root-level `forms` are enforced in full scans, watch mode, and retry/trial single-path reconstruction:

- **`recordSkippedItems(skipped []SkippedItem)`** — Groups skipped items by reason, batch-upserts to `sync_failures` as actionable failures. Uses aggregated logging: when >10 items share the same reason, logs 1 WARN summary with count and sample paths, while still logging every skipped path at DEBUG for diagnosis. When <=10 items, logs each as an individual WARN.
- **`clearResolvedSkippedItems(skipped []SkippedItem)`** — Deletes `sync_failures` entries for scanner-detectable file-scoped actionable issues that are no longer skipped (e.g., user renamed a previously invalid file or a one-off hash panic no longer reproduces). Compares current skipped paths against recorded failures and removes stale entries.

### Aggregated Logging

Implements: R-6.6.7 [verified], R-6.6.8 [verified], R-6.6.9 [verified], R-6.6.10 [verified], R-6.6.12 [verified]

When >10 items share the same warning category, log 1 WARN summary with count and sample paths + individual paths at DEBUG. When <=10 items, log each as an individual WARN. This pattern is implemented in `recordSkippedItems()` for scanner-time validation failures. Transient retries log at DEBUG, transient item failures that later clear log one INFO with the persisted `failure_count`, and exhausted transients log at WARN. Extends to execution-time transient failures: when >10 transient failures of the same `issue_type` exhaust their retry budget in a single sync pass, aggregate into 1 WARN summary with count, individual paths at DEBUG (R-6.6.12).

Transient-resolution INFO is emitted by the engine's shared clear path after it calls the store-owned take-and-delete failure API. That API returns the authoritative `sync_failures` row, so `attempt_count` comes from durable state at clear time instead of from an in-memory retry counter. The INFO log is intentionally limited to transient `failure_role='item'` rows. Held and boundary scope rows still use scope lifecycle logging; `releaseScope()` remains the sole INFO owner for scope-clear events.

Watch-mode visible issue logging uses the store-owned visible-issue projection
instead of ad hoc counters. Shared-folder blocked-write summaries therefore log
one activation message per boundary, log again only when the blocked-child set
materially changes, and log one resolution message when the boundary clears.

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
- Otherwise it resolves the relevant shortcut, calls `ListItemPermissions` on the target folder, and runs the caller-aware graph permission classifier to confirm whether the 403 is a real write denial or a transient/inconclusive failure.
- On confirmed denial it walks upward, still using `ListItemPermissions`, to find the highest denied ancestor but never above the shortcut root.
- On confirmed denial it records the triggering blocked write as one held
  transient row with `scope_key='perm:remote:{boundary}'` and the shortcut's
  remote drive ID. That held row is the durable authority for the derived
  scope.
- If a broader-scope trial (for example `service`) discovers a more specific
  confirmed remote permission boundary, the engine rehomes the candidate into
  the new `perm:remote` held row and removes the older broader-scope held
  candidate instead of duplicating blocked work across scopes.

The graph-side classifier is intentionally stricter than raw `roles`
inspection. Live Personal-account testing showed `permissions` responses on
read-only shared folders including both the recipient's read-only link grant
and an unrelated owner row for the sharer. The engine therefore treats
`ListItemPermissions` as evidence that still needs caller applicability
evaluation; unrelated owner/write rows do not defeat a confirmed read-only
boundary.

`perm:remote` is recursive and download-only while blocked write intent exists:

- uploads, folder creates, remote moves, and remote deletes are blocked for the boundary path and every descendant
- downloads continue so the subtree remains readable and delta/reconciliation can keep it current
- if the last blocked write disappears, the derived scope is forgotten immediately instead of leaving behind a remembered read-only boundary

`recheckPermissions()` revisits visible derived `perm:remote` scopes at the start
of each sync pass and returns explicit release/keep decisions:

- writable again -> `releaseScope`
- Graph/API failure or stale shortcut boundary -> fail open via `releaseScope`
- still denied -> keep the boundary active

Shortcut removal also returns explicit discard decisions for any
`perm:remote:*` boundary under the removed shortcut plus the matching
`quota:shortcut:*` scope. Removed shortcuts discard blocked work instead of
releasing it back into dispatch, and watch mode rebuilds active scopes from the
post-removal durable state immediately.

### Observation Suppression

Implements: R-2.10.30 [verified], R-2.10.31 [verified]

During `SKService`, suppress shortcut observation polling globally because the
Graph backend itself is degraded. During `SKThrottleShared(...)`, suppress
observation only for that exact shared target because continuing to poll that
same target just re-spends the known throttled budget. `SKThrottleDrive(...)`
does not suppress unrelated shortcut observation, and `quota:shortcut:*`
observation continues (read-only).

Observation suppression is intentionally split:

- `scopeController.isObservationSuppressed()` handles only global cases such as
  `service`, `auth:account`, and legacy startup-cleanup `throttle:account`
- `scopeController.suppressedShortcutTargets()` provides the exact set of
  shared targets whose observation should be skipped for target-scoped 429s

That split prevents one drive-scoped or shared-target-scoped throttle from
silencing unrelated shortcuts.

**Trial dispatch correctness**: `runTrialDispatch()` uses due-scope snapshot iteration — each due scope is visited exactly once per tick, making infinite iteration structurally impossible. On successful dispatch, the trial interval is NOT mutated (awaiting worker result). When no usable candidate exists, the engine preserves the scope at the same interval instead of auto-releasing it. The timer re-arms after each trial decision.

**Trial path separation**: `processWorkerResult()` checks `IsTrial` and routes through explicit trial policy inside the shared result router. This eliminates the prior fragile pattern where all trial failures collapsed into one branch. Success releases the scope, matching evidence extends it, inconclusive outcomes preserve it, and scope detection is never called for trial results because the original scope is already blocked.

**External perm:dir clearance**: `handleExternalChanges()` checks whether
`local_permission_denied` failures were cleared via CLI-side store mutation.
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
- **End-of-pass summary**: `logFailureSummary()` aggregates exhausted transient failures by `issue_type`, not by raw error text. Groups with >10 items get one WARN summary with count and sample paths, while per-item detail remains available at DEBUG; groups with ≤10 items get per-item WARN lines. The summary state is tracked separately from `SyncReport.Errors`, so one-shot report errors remain intact after logging. Called at end of `executePlan()`.
- **IssueType population**: `recordFailure()` derives issue_type from HTTP status via `issueTypeForHTTPStatus()` and stores it in sync_failures for display grouping.

### Shortcut Integration (`engine_shortcuts.go`)

`shortcutCoordinator` detects shortcuts to shared folders in the delta stream,
registers them, and plans shortcut-scoped observation targets. The actual
delta/enumerate execution path is shared with primary scoped observation, so
shortcut scopes and `sync_paths` scopes use the same target-level observation
rules. Shortcut removal still owns the side effects that clear any persisted
`perm:remote` scope under the removed shortcut and discard its held failures,
preventing stale recursive write suppression after the share disappears.

## CLI / Engine Boundary (`sync_helpers.go`)

`sync_helpers.go` is the root-package bridge into the single-drive engine. It
constructs `sync.Engine` instances for engine-facing flows such as conflict
resolution and verification, while the multi-drive `sync` command itself is
governed by `sync-control-plane.md`.

## Conflict Resolution

Manual conflict resolution is part of the engine boundary, not a CLI-only
string operation. `ResolveConflict()` owns the runtime flow for `keep_local`,
`keep_remote`, and `keep_both`.

`keep_both` is intentionally modeled as an explicit reconciliation path, not
as a synthetic executor transfer outcome:

- the executor-level file operations still produce the visible filesystem
  result: keep the renamed local conflict copy and restore/download the remote
  file at the original path
- once that local disk state exists, the engine hashes the current file and
  calls `SyncStore.RefreshLocalBaseline(...)`
- the store updates only the local-side baseline tuple for the original path,
  preserves the known remote-side tuple/`etag`, and marks a matching
  `remote_state` row `synced`
- conflict-copy placeholders use synthetic local-only item IDs so the
  baseline can remember the extra file without claiming it has a remote
  counterpart yet

This separation matters because `ActionUpdateSynced` still means true
planner/executor convergence: both sides are already equivalent without a
manual resolution-specific store transition. `keep_both` is different. It is a
manual reconciliation decision that needs explicit durable-state repair.

## Watch Mode Behavior

- SIGHUP → reload `config.toml`, apply drive changes immediately
- PID file with flock for single-instance enforcement
- Two-signal shutdown (drain, then force)
- Periodic full reconciliation (default 24h, async — see below)

### Graceful Watch Shutdown

The first shutdown signal cancels the shared watch context. The engine then
seals new work admission and enters `draining`:

- retry and trial timers are stopped
- debounced observation batches, skipped-item intake, recheck ticks, and
  reconcile-launch ticks are disabled
- not-yet-dispatched outbox actions are completed as shutdown
- already-dispatched worker results are still accepted if they arrive before
  worker exit
- reconcile results that arrive after shutdown starts clear only their own
  bookkeeping and must not enqueue new work

The second shutdown signal is still the force-exit path owned by the CLI.

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

**CLI notification**: `issues` shows held deletes in a dedicated "HELD DELETES" section. User approves them via `issues force-deletes`.

**External change detection**: A 10-second `recheckTicker` in the `RunWatch()` select loop runs `PRAGMA data_version` to detect CLI writes. When the data version changes, `handleExternalChanges()` queries `ListSyncFailuresByIssueType(IssueBigDeleteHeld)`. If zero rows remain (user cleared them all), calls `counter.Release()`. On the next observation cycle, deletions are re-observed and dispatched normally.

**Startup cleanup**: `RunWatch()` clears stale `big_delete_held` entries from prior daemon sessions, since the in-memory counter resets on restart.

**Force mode**: `--force` skips counter creation (`deleteCounter` stays nil), so no watch-mode big-delete protection applies.

### Rationale

- **Crash recovery requires explicit bridging**: On restart after crash, [`internal/syncrecovery/recovery.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncrecovery/recovery.go) resets `remote_state` items stuck mid-execution to pending or deleted, AND creates `sync_failures` entries so the engine retry sweep can rediscover them. This is necessary because the delta token was already advanced before execution — items that crashed mid-execution won't appear in the next delta response. The planner is idempotent for items that DO appear in observations, but crash recovery items need the `sync_failures` → retrier → planner path.
- **Keep control plane separate from the engine**: multi-drive coordination now lives in `internal/multisync`, leaving `internal/sync` focused on the single-drive runtime and conflict APIs.
