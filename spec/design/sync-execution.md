# Sync Execution

GOVERNS: internal/syncexec/executor.go, internal/syncexec/executor_conflict.go, internal/syncexec/executor_delete.go, internal/syncexec/executor_transfer.go, internal/syncexec/worker.go, internal/syncdispatch/dep_graph.go, internal/syncdispatch/active_scopes.go, internal/syncdispatch/scope.go, internal/syncdispatch/delete_counter.go, internal/localtrash/trash.go, status.go

Implements: R-2.3 [verified], R-5.1 [verified], R-6.4 [implemented], R-6.4.4 [verified], R-6.4.5 [verified], R-6.4.6 [verified], R-6.5.3 [verified], R-6.7.25 [planned], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified], R-2.10.5 [verified], R-2.10.11 [verified], R-2.10.15 [verified], R-2.10.16 [verified], R-2.10.41 [verified], R-2.10.42 [verified], R-2.10.43 [verified], R-2.10.44 [verified], R-2.14.2 [verified], R-6.3.4 [verified], R-6.10.6 [verified]

## Executor (`executor.go`)

Implements: R-6.8.9 [verified]

Thin action dispatcher. Takes an `ActionPlan` and dispatches actions to workers
via `DepGraph` plus the engine's active-scope admission logic. Actions are
dispatched based on dependency satisfaction, not fixed phase ordering. Workers
produce individual `Outcome` values committed per-action by the SyncStore.

## Ownership Contract

- Owns: Action dispatch, dependency tracking, worker execution, local trash interaction, and per-action success commits.
- Does Not Own: Planning, retry scheduling, scope activation policy, or durable failure classification.
- Source of Truth: The `ActionPlan`, engine-owned ready/done channels, and baseline/outcome state supplied by the store boundary.
- Allowed Side Effects: Filesystem mutation under rooted capabilities, Graph transfer calls, local trash operations, and store success commits.
- Mutable Runtime Owner: The engine owns dispatch state (`DepGraph`, ready channel, done signal). `WorkerPool` owns worker goroutines and the results channel it closes in `Stop()`.
- Error Boundary: Execution returns raw `WorkerResult` values and driveops/graph errors upward. The engine owns mapping those results into [error-model.md](error-model.md) classes.

Action methods call the graph client directly and return `Outcome` — no retry loop, no error classification, no sleep. The following were removed as part of the retry-to-transport refactoring: `withRetry()`, `classifyError()`, `classifyStatusCode()`, `calcExecBackoff()`, `errClass` type + constants, `sleepFunc` field. Hash mismatch retry (`downloadWithHashRetry`) is unchanged — it's a data integrity mechanism orthogonal to the retry redesign.

## DepGraph (`dep_graph.go`)

Pure dependency graph with no channels, no callbacks, and no scope awareness. Tracks actions by sequential ID and resolves dependency edges. Methods return data — callers decide what to do with ready actions.

- **`Add(action, id, depIDs) *TrackedAction`**: Insert an action. Returns the action if immediately ready (all deps satisfied or unknown), nil if waiting on dependencies.
- **`Complete(id) ([]*TrackedAction, bool)`**: Mark done, delete from both `actions` and `byPath` maps (D-10 fix), return newly-ready dependents. Returns `(nil, false)` for unknown IDs.
- **`HasInFlight(path) bool`**: Check if a path has an in-flight action.
- **`CancelByPath(path)`**: Cancel and clean up byPath entry for a path.
- **`InFlightCount() int`**: Returns `len(actions)` — accurate because `Complete` deletes from the map.

The D-10 fix ensures completed actions are removed from the `actions` map. Without this deletion, a completed action would linger, and a subsequent `Add` could wire a dependency edge to it, causing the dependent to wait forever.

`TrackedAction` struct is defined here: pairs an `Action` with an ID, cancel function, trial metadata (`IsTrial`, `TrialScopeKey`), and dependency tracking (`depsLeft`, `dependents`).

## Active Scope Helpers (`active_scopes.go`)

`active_scopes.go` no longer owns a runtime subsystem. It provides pure helper
functions over an engine-owned `[]ScopeBlock` working set. There is no mutex,
no write-through cache, and no persistence layer in `syncdispatch`.

- **`FindBlockingScope(blocks, ta) ScopeKey`**: Check whether an action matches
  any active scope block. Returns the blocking key or zero. Priority-ordered:
  global scopes (throttle, service) first, then narrow scopes (disk, quota),
  then dynamic-key scopes (`quota:shortcut`, `perm:dir`, `perm:remote`).
- **`UpsertScope(blocks, block) []ScopeBlock`**: Return a copy with the scope
  inserted or replaced by key.
- **`RemoveScope(blocks, key) []ScopeBlock`**: Return a copy with the scope
  removed.
- **`LookupScope(blocks, key) (ScopeBlock, bool)`**: Return a value copy of the
  active block.
- **`ExtendScopeTrial(blocks, key, nextAt, interval)`**: Return a copy with the
  scope's trial metadata updated and `TrialCount` incremented.
- **`DueTrials(blocks, now)` / `EarliestTrialAt(blocks)` / `ScopeKeys(blocks)`**: Pure helpers for
  watch-loop timer scheduling and scope iteration.

The watch loop owns runtime scope state. `scope_blocks` remains the persisted
restart/recovery record; store transactions update it, then the watch loop
updates its own in-memory working set.

### Persisted Scope Blocks (`scope_blocks` table)

Scope blocks survive crashes. The `scope_blocks` table is the persisted
restart/recovery record. In watch mode the engine loads those rows into its own
single-owner runtime working set; there is no separate write-through cache
subsystem. Typically 0-5 rows.

```sql
CREATE TABLE scope_blocks (
    scope_key      TEXT PRIMARY KEY,
    issue_type     TEXT NOT NULL,
    blocked_at     INTEGER NOT NULL,     -- unix nanos
    trial_interval INTEGER NOT NULL,     -- nanoseconds
    next_trial_at  INTEGER NOT NULL,     -- unix nanos
    trial_count    INTEGER NOT NULL DEFAULT 0
);
```

No FK between `scope_blocks` and `sync_failures` — intentional. Scope release
and discard are transactional lifecycle operations in the store layer, not
schema-level cascades.

### ScopeBlockStore Interface

`ScopeBlockStore` is the persistence interface for scope-block rows.
Implemented by `SyncStore` in `store_scope_blocks.go`:
- `UpsertScopeBlock(ctx, block) error` — INSERT OR REPLACE
- `DeleteScopeBlock(ctx, key) error` — DELETE WHERE
- `ListScopeBlocks(ctx) ([]*ScopeBlock, error)` — SELECT all rows

Fixes D-2 (no `onHeld` callback, no cross-lock paths), D-8 (scope blocks
persisted, survive crash).

## Scope Detection (`scope.go`)

Implements: R-2.10.3 [verified], R-2.10.26 [verified], R-2.10.42 [verified]

### ScopeKey Type System

All scope keys are typed `ScopeKey{Kind ScopeKeyKind, Param string}` — a comparable value type usable as map key. Seven kinds: `ScopeThrottleAccount`, `ScopeService`, `ScopeQuotaOwn`, `ScopeQuotaShortcut` (Param = "remoteDrive:remoteItem"), `ScopePermDir` (Param = relative dir path), `ScopePermRemote` (Param = relative boundary path), `ScopeDiskLocal`. Pre-built singletons for non-parameterized scopes (`SKThrottleAccount`, `SKService`, `SKQuotaOwn`, `SKDiskLocal`); constructor functions for parameterized scopes (`SKQuotaShortcut(key)`, `SKPermDir(path)`, `SKPermRemote(path)`).

Methods on `ScopeKey` centralize logic that was previously scattered across 9+ files:
- **`BlocksAction(path, shortcutKey, actionType, targetsOwnDrive)`** — scope-specific action blocking (used by `blockedScope()`)
- ~~`MaxTrialInterval()`~~ — removed; interval computation is centralized in the engine's scope-aware trial timing helper
- **`Humanize(shortcuts)`** — user-friendly description for display
- **`IssueType()`** — maps scope kind to `sync_failures.issue_type` constant
- **`IsGlobal()`** — true for scopes that block ALL actions (throttle, service)
- **`IsPermDir()` / `DirPath()`** — type-safe access for local directory permission scopes
- **`IsPermRemote()` / `RemotePath()`** — type-safe access for remote shared-folder permission scopes
- **`IsZero()`** — detects the zero-value (invalid) key
- **`String()` / `ParseScopeKey(s)`** — wire format serialization for SQLite `scope_key` columns. The wire format is `"throttle:account"`, `"service"`, `"quota:own"`, `"quota:shortcut:X"`, `"perm:dir:X"`, `"perm:remote:X"`, `"disk:local"`.
- **`ScopeKeyForStatus(httpStatus, shortcutKey)`** — single source of truth for HTTP status → scope key classification, replacing scattered switch/if chains in `classifyResult` and `deriveScopeKey`.

`ScopePermRemote` is the recursive download-only shared-folder scope. `BlocksAction` returns true for uploads, folder creates, remote moves, and remote deletes at the boundary path and every descendant, while allowing downloads to continue.

### Persisted Failure And Scope Shapes

The execution layer relies on two explicit persisted models:

- `sync_failures.failure_role` = `item`, `held`, `boundary`
- `scope_blocks.timing_source` = `none`, `backoff`, `server_retry_after`

Those columns are important to execution correctness:

- trial dispatch reads only `held` rows
- permission and startup repair reason about `boundary` rows explicitly
- restart logic preserves only server-timed throttle/service scopes
- watch-mode `activeScopes` is rebuilt from persisted scope rows and never becomes a peer source of truth

### Scope Escalation

`ScopeState` maintains sliding windows for scope escalation detection and
records successes that reset windows. In watch mode it is owned by the
single-owner watch loop. Windows are keyed by `ScopeKey` (not string).

- **Immediate blocks** (server signals): 429 → `SKThrottleAccount` (single response triggers). 503 with Retry-After → `SKService` (single response triggers).
- **Sliding window detection**: 507 → 3 unique paths in 10s → `SKQuotaOwn` or `SKQuotaShortcut(key)`. 5xx → 5 unique paths in 30s → `SKService`.
- **Success resets**: `RecordSuccess()` clears sliding windows for the relevant scope — a successful request proves the service is up.

## Worker Pool (`worker.go`)

Implements: R-2.10.16 [verified], R-6.8.12 [verified]

Flat pool of `transfer_workers` goroutines. Decoupled from dispatch infrastructure: accepts `readyCh <-chan *TrackedAction` (actions to execute) and `doneCh <-chan struct{}` (shutdown signal) as constructor parameters instead of holding a reference to the dispatch infrastructure. Workers are pure executors — they execute actions, persist success outcomes, and send `WorkerResult` to the engine. Workers never call `DepGraph.Complete()` — the engine owns all completion decisions.

`WorkerResult` carries target drive identity (`TargetDriveID`, `ShortcutKey`) from the action, `RetryAfter` from `GraphError`, the full `error` for classification, `ActionID` for DepGraph routing, `IsTrial` and `TrialScopeKey ScopeKey` for scope trial routing. The engine classifies and routes each result.

### Mutable Runtime Ownership

- `DepGraph` is engine-owned mutable state. Execution code never mutates it behind the engine's back.
- `WorkerPool` owns worker goroutine lifecycle and closes `results` exactly once after all workers exit.
- Workers own only per-action local state plus the cancellation function stored on their current `TrackedAction`.
- There are no long-lived mutexes in `syncexec`; coordination flows through explicit channel ownership instead of shared mutable maps.

### Channel And Timer Ownership

The execution path relies on a small set of long-lived channels with strict
ownership rules:

- **`readyCh`**: owned by the engine. Created by `executePlan` in one-shot
  mode and by `initWatchInfra` in watch mode. Written by one-shot admission
  code or by the single watch loop's outbox flush. Read by worker goroutines
  only. It is intentionally not closed; workers exit via `doneCh`/context
  cancellation instead of ranging on channel close.
- **Worker `results` channel**: owned by `WorkerPool`. Created in
  `NewWorkerPool`, written only by workers through `sendResult`, read by
  the engine-owned result loop in both one-shot and watch mode. Closed exactly
  once by `WorkerPool.Stop()` after all worker goroutines exit.
- **Trial timer delivery (`trialCh`)**: owned by the engine and created once
  in `NewEngine`. Written only by `time.AfterFunc` callbacks scheduled by
  `armTrialTimer`. Read by the watch loop in watch mode. The channel is never
  closed; shutdown stops the current timer via `stopTrialTimer()`.
- **Retry timer delivery (`retryTimerCh`)**: owned by watch-mode engine state
  and created in `initWatchInfra`. Written by `armRetryTimer`'s
  `time.AfterFunc` callback and by inline engine wakeups that need an
  immediate retrier pass (for example, due items or batch spillover). Read
  only by the watch loop. Like `trialCh`, it is persistent and never closed;
  the timer object is replaced or stopped as needed.
- **Bootstrap completion barrier**: there is no separate "bootstrap finished"
  signal channel between bootstrap and observer startup. The barrier is the
  unified engine loop itself: `bootstrapSync` keeps consuming ready batches,
  worker results, retry ticks, and trial ticks until the graph is empty and no
  bootstrap work remains. One-shot mode uses the same internal loop with a
  one-shot configuration and returns only after the results channel closes and
  all result side effects have been applied.
- **Observer error signaling (`errs`)**: owned by `startObservers`. Created by
  the engine, written once per observer goroutine on exit, read only by the
  watch loop. It is not explicitly closed because the watch loop tracks
  observer lifetimes by counting terminal sends rather than ranging until
  close.
- **Observer event bridge (`events`)**: owned by `startObservers`. Local and
  remote observers write `ChangeEvent` values into it; a dedicated bridge
  goroutine reads from it and adds them to the watch buffer. The engine closes
  `events` only after all observer goroutines finish, ensuring the bridge can
  drain remaining events cleanly.
- **Skipped-item forwarding (`skippedCh`)**: owned by `startObservers`.
  Created by the engine, written by the local observer's safety scan path, and
  read by the watch loop for issue recording and resolution cleanup. It is not
  closed; shutdown relies on context cancellation and watch-loop exit rather
  than channel completion.

## Action Execution

### Downloads (`executor_transfer.go`)
`.partial` file + hash verify + atomic rename. Uses `TransferManager` from `driveops`.

### Uploads (`executor_transfer.go`)
Simple PUT (≤4 MiB) or resumable session (>4 MiB). Post-upload validation detects SharePoint enrichment.

**Watch-Mode Upload Freshness Check**: In watch mode, `ExecuteUpload` performs a pre-flight eTag comparison before uploading. The debounce-based event batching can split simultaneous local+remote changes across passes: a local fsnotify event may trigger an upload before the remote observer has polled the collaborator's edit. The freshness check fetches the item's current remote eTag via `GetItem` and compares it against the baseline eTag. If they differ, the upload is aborted with a descriptive error (recorded in `sync_failures`). On the next pass, the remote observer will have polled, the planner will see both changes, and a proper conflict will be detected. Cost: one additional `GetItem` API call per upload in watch mode only — controlled by `ExecutorConfig.watchMode` (set by `initWatchInfra`).

### Deletes (`executor_delete.go`)
Implements: R-6.2.4 [verified]

Hash-before-delete guard for local deletions (verifies the file hasn't changed since planning). Remote deletes use `If-Match` with eTag. Local deletes go to OS trash if configured via [`internal/localtrash/trash.go`](/Users/tonimelisma/Development/onedrive-go/internal/localtrash/trash.go). When a local folder delete would fail due to non-empty directory containing only disposable files (OS junk like `.DS_Store`, editor temps like `.swp`, invalid OneDrive names), `deleteLocalFolder` auto-removes them before retrying the folder delete.

### Conflicts (`executor_conflict.go`)
Default: keep both versions. Remote version at original path, local version renamed to `<name>.conflict-<timestamp>.<ext>`. Conflict recorded in `conflicts` table.

## Issue Types (`issue_types.go`)

Issue type constants for failure classification (e.g., `IssueInvalidFilename`, `IssuePathTooLong`, `IssueFileTooLarge`, `IssueBigDeleteHeld`). Moved from the deleted `upload_validation.go`. The upload validation functions (`filterInvalidUploads`, `validateUploadActions`, `validateSingleUpload`, `ValidationFailure`, `removeActionsByIndex`) have been removed entirely — all validation now happens in the observation layer via `shouldObserve()` (Stage 1) and post-stat size checks (Stage 2). See `spec/design/sync-observation.md`.

`IssueBigDeleteHeld` is an actionable issue type used by the watch-mode big-delete protection (see sync-engine.md). Held delete actions are recorded with this type and displayed in a dedicated "HELD DELETES" section in `issues list`.

## Crash Recovery

Implements: R-2.10.41 [verified]

[`internal/syncrecovery/recovery.go`](/Users/tonimelisma/Development/onedrive-go/internal/syncrecovery/recovery.go) handles crash recovery: on startup, it resets items stuck in `downloading`/`deleting` state to `pending_download`, `pending_delete`, or `deleted`. The sync tree decides whether a local delete completed before the crash; the store applies only the durable state transitions. One-shot mode does this in `prepareRunOnceState`; watch mode does it during watch bootstrap before observation starts. Both modes therefore rediscover crash-recovery items without relying on `RunWatch` calling `RunOnce`.

Implements: R-2.5.4 [verified]

After resetting `remote_state`, crash recovery also creates
corresponding `sync_failures` entries (category=`transient`, direction matching
the action, `next_retry_at` computed via `delayFn`) for each item that
transitioned to a pending state. This bridges `remote_state` to the retry queue
(`sync_failures`). Without this bridge, items that crashed mid-execution would
become zombies: the delta token was already advanced (no new events), and the
retry sweep only queries `sync_failures`. `RecordFailure` uses UPSERT — if a
`sync_failures` entry already exists from a prior failure before the crash, the
existing `failure_count` is preserved and incremented, so backoff continues
from where it left off.

The retry sweep is the sole retry mechanism for sync actions. In watch mode it
is integrated directly into the single-owner watch loop; in one-shot mode there
is no long-lived retry loop. `runRetrierSweep()` periodically sweeps
`sync_failures` for items whose `next_retry_at` has expired and re-injects them
into the pipeline via buffer → planner → DepGraph. The engine calls
`DepGraph.Complete` on every worker result and records failures in
`sync_failures` with exponential backoff via `retry.ReconcilePolicy().Delay`.
`CommitOutcome` updates baseline and `remote_state`, but it does not clear
`sync_failures`; the engine owns success-side failure cleanup explicitly via
`clearFailureOnSuccess`. `runTrialDispatch()` handles scope trial
candidate selection via `PickTrialCandidate` and re-observation.

## Status Computation (`compute_status.go`)

Pure function `computeNewStatus()` determines the new `sync_status` for a `remote_state` row based on the action outcome. Used by `CommitObservation`.

## Disk Space Pre-Check

Implements: R-2.10.43 [verified], R-2.10.44 [verified], R-6.2.6 [verified], R-6.4.7 [verified]

Disk space pre-checks now live in `TransferManager.DownloadToFile` (see [drive-transfers.md](drive-transfers.md#disk-space-pre-check)), giving every download caller automatic protection. The sync engine wires the check via `driveops.WithDiskCheck(cfg.MinFreeSpace, driveops.DiskAvailable)` when constructing the TransferManager in `NewEngine`.

Config wiring: `min_free_space` string (default "1GB") is parsed via `config.ParseSize()` and threaded from `ResolvedDrive` through `EngineConfig.MinFreeSpace` to `TransferManager` via `WithDiskCheck`. Zero disables the check (R-6.4.7).

Scope block classification remains in the sync engine: `classifyResult` maps `driveops.ErrDiskFull` → `disk:local` scope block, `driveops.ErrFileTooLargeForSpace` → per-file skip.

## Design Constraints

- Dotfile conflict naming: `filepath.Ext(".bashrc")` returns `.bashrc`, not `""`. The `conflictStemExt` helper detects single-dot dotfiles and treats extension as empty.
- Delete ordering: depth-first (deepest first), and files before folders at the same depth. `resolveItemType` is a tiebreaker in the sort comparator.
- Ephemeral `Executor` struct per call via `NewExecution(cfg, bl)` — always initializes all mutable fields at construction. Prevents nil-map panics from temporal coupling.
- Edit-delete conflicts (local edit, remote delete) auto-resolve: local modified version wins and is uploaded. Conflict recorded as `resolved_by: "auto"`.
- Individual worker failures increment `report.Failed` and collect errors in `report.Errors`, but `RunOnce()` returns nil. Tests check `report.Failed >= 1`, not `err != nil`.
- Two concurrency knobs: `transfer_workers` (default 8, range 4-64) for file operations, `check_workers` (default 4, range 1-16) for QuickXorHash computation.
- All executor write operations use `containedPath()` with `filepath.IsLocal()` to confine local filesystem writes to the sync root directory. This is defense-in-depth against path reconstruction bugs, not input validation (the OneDrive API is the source of truth, not an attacker). Symlink escape is prevented by resolving symlinks on the parent directory via `filepath.EvalSymlinks`.
- Concurrent folder creates via Graph API `$batch` for sibling folders at same depth. Diminishing returns after first sync. [planned]
- Targeted `-race` stress tests for DepGraph, active-scope admission helpers, Buffer, WorkerPool. [planned]
- Sub-second uniqueness in `conflictCopyPath`: second-precision timestamps mean two conflicts in the same second collide. [planned]
- Explicit error for unknown `ActionType` in `applySingleOutcome`: default case currently returns nil, silently dropping outcomes. [planned]
- Graceful shutdown test under active worker pool: verify SIGTERM during active transfers drains correctly. [planned]

## CLI Status (`status.go`)

The `status` command reads config, token files, and state databases to display account and drive status. Concurrent-reader safe while sync is running.
