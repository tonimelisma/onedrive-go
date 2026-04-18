# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/engine_config.go, internal/sync/debug_event_sink.go, internal/sync/engine_debug_events.go, internal/sync/engine_scope_invariants.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_decisions.go, internal/cli/sync_flow.go, internal/cli/sync_runtime.go

Implements: R-2.1 [verified], R-2.8.3 [verified], R-2.8.5 [verified], R-2.10 [verified], R-2.14 [verified], R-2.16.2 [verified], R-2.16.3 [verified], R-6.3.4 [verified], R-6.3.5 [verified]

## Overview

The engine is the single-drive runtime owner. It coordinates:

- observation
- planning
- execution
- publication-only action commits
- durable store commits
- retry and trial timers
- scope lifecycle
- watch-mode reconcile and maintenance work

Watch mode persists its full-refresh cadence in `observation_state`:

- primary remote full refresh every 24 hours while delta is healthy
- primary remote full refresh every 1 hour when delta is degraded
- local full refresh every 5 minutes while watcher-based observation is healthy
- local full refresh every 1 hour when watcher-based observation has degraded or fallen back

One-shot still performs a full local scan at startup for every run.

Retry and trial admission now read from `retry_state`:

- ready per-item retry work comes from unblocked `retry_state` rows whose `next_retry_at` is due
- scope trials sample one blocked `retry_state` row at random for each due scope
- retry/trial revalidation rebuilds candidates directly from `RetryStateRow`
  and `RetryWorkKey` plus current snapshots and baseline
- `sync_failures` remains available for issue reporting, but it is no longer part of retry scheduling, retry candidate reconstruction, scope admission, or scope lifecycle

The engine does **not** own multi-drive orchestration or control-socket
lifecycle. Those belong to `internal/multisync`.

## Ownership Contract

- Owns: single-drive runtime orchestration, watch-mode mutable state, worker-result classification, retry/trial scheduling, and permission-scope lifecycle
- Does Not Own: SQLite schema, Graph normalization, config parsing, or multi-drive daemon lifecycle
- Source of Truth: durable sync state in `SyncStore`, plus engine-owned in-memory runtime state for the currently running pass/session
- Allowed Side Effects: coordinating observers, planner, executor, store writes, and rooted filesystem/Graph collaborators through injected boundaries
- Mutable Runtime Owner: `Engine` owns all single-drive mutable runtime state for one active run. In watch mode that includes the event loop, outbox, in-flight actions, retry/trial timers, scope state, and admission flags.
- Error Boundary: the engine translates observer, planner, executor, permission, and store outcomes into engine-owned reports, retries, scope transitions, and durable failure rows. Lower layers keep their own transport and I/O semantics.

## Verified By

| Behavior | Evidence |
| --- | --- |
| One-shot sync remains a bounded observe-plan-execute pass without a live user-intent mailbox. | `TestBootstrapSync_NoChanges`, `TestBootstrapSync_WithChanges`, `TestOneShotEngineLoop_Success_ClearsSyncFailure` |
| Watch mode keeps single-owner runtime admission, periodic maintenance, and external-change reconciliation inside the engine boundary. | `TestEngine_ReleaseScope_SignalsImmediateRetrySweep`, `TestEngine_AdmitReady_ScopeBlocked`, `TestEngine_ExternalDBChanged`, `TestEngine_HandleExternalChanges_RemotePermissionClearance`, `TestRunWatch_ShutdownStopsRetryAndTrialTimers` |
| Runtime-visible issue and scope state are rebuilt from durable store facts instead of separate manual conflict/delete workflows. | `TestSyncStore_ListVisibleIssueGroups`, `TestReadDriveStatusSnapshotAndScopeBlockHelpers`, `TestQuerySyncState_CountsAuthAndRemoteBlockedScopesAsIssues` |

## Construction

`newEngine()` wires one resolved drive into a runtime:

- rooted sync tree
- store
- planner
- executor configuration
- transfer manager
- permission handler
- optional websocket wake source

For separately configured shared-root drives, the engine also carries the
configured `rootItemID`. That root item defines the remote boundary for scoped
observation and execution metadata.

## One-Shot Sync

`RunOnce()` keeps one-shot behavior intentionally simple:

1. bootstrap durable state and startup checks
2. refresh current remote and local snapshots once
3. compute SQL structural diff and reconciliation once
4. build the current actionable set in Go
5. commit any ready publication-only actions directly through the store
6. execute remaining concrete work once
7. commit outcomes and return a report

There is no mid-pass mailbox for user intent. New external DB writes during a
one-shot run are simply durable state for a later run.

The one-shot pass now persists `local_state` from the full local scan, derives
reconciliation rows from snapshots, and builds the current actionable set in
Go before execution begins.

After the actionable set is built, runtime action-state materialization only
prunes `retry_state` and `scope_blocks` to match the current action set. It
does not persist a durable executable plan table.

Dry-run now uses that same snapshot and SQLite reconciliation path against an
isolated scratch `SyncStore`. The runtime seeds that scratch store from the
current committed `baseline`, `remote_state`, and `observation_state`, commits
the freshly observed dry-run `remote_state` and `local_state` snapshots there,
and then builds the exact current actionable set from those committed scratch
rows. The durable store keeps its original cursor, snapshots, and baseline.

`sync --full` remains the explicit stronger-freshness path when incremental
delta visibility is not sufficient.

## Watch Mode

`RunWatch()` is the long-lived runtime. It owns:

- observer startup and shutdown
- dirty-signal intake and debounce
- snapshot refresh and SQLite reconciliation after debounce
- action admission and dispatch
- action completion drain
- retry and trial timer scheduling
- periodic recheck and full reconciliation
- graceful drain on shutdown

The watch loop is the single owner of mutable runtime state. Other packages may
signal it, but they do not mutate its runtime data structures directly.

Local watcher events, remote delta batches, websocket wakes, and full
reconciliation results are scheduler hints only. After 5 seconds without a new
local or remote observation, watch mode refreshes current truth, runs SQL
comparison/reconciliation, builds the current actionable set in Go, and then
admits runnable actions. Any ready `ActionUpdateSynced` / `ActionCleanup`
actions are committed immediately through the store before the remaining
side-effecting actions go to workers.

### Recheck And External DB Changes

Watch mode periodically checks SQLite `PRAGMA data_version`. If another
connection committed a write, `handleExternalChanges()` runs.

That reconciliation hook is intentionally narrow. It currently rechecks
externally cleared permission-scope state and releases any runtime permission
scope whose backing `scope_blocks` or blocked `retry_state` rows disappeared.

It is **not** a generic user-intent ingestion path.

## Shared-Root Drives

Shared folders are separate configured drives now. The engine therefore
supports two drive shapes:

- ordinary drive-root sessions
- shared-root sessions rooted below the remote drive root

Embedded shared-folder links discovered inside another synced drive are ignored
by observation and never become nested engine-owned sub-sessions.

## Conflict Handling

Conflicts are engine-owned and immediate:

- edit/edit and create/create preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path
- edit/delete is planner-expanded into a local-wins upload
- executor-time local-delete hash mismatch now reports a stale precondition so
  the next replan can emit that upload from current truth

There is no durable conflict-request workflow and no CLI `resolve` command.

## Scope And Failure Lifecycle

The engine classifies action completions into:

- success cleanup
- retryable failure
- actionable failure
- scope activation / preserve / release decisions

Runtime scope state is an in-memory working set rebuilt from persisted
`scope_blocks` plus blocked `retry_state` rows at startup. Current persisted
scope families in `scope_blocks` are:

- `quota:own`
- `throttle:target:drive:*`
- `service`
- `perm:dir:*`
- `disk:local`

Remote permission scopes are derived from blocked `retry_state` rows with
`perm:remote:*` scope keys rather than persisted scope-block rows.

Account-auth rejection is no longer a persisted sync scope. Durable
account-auth state lives in the managed catalog, and sync consults that catalog
before startup proof and after fatal unauthorized results.

Permission scopes are revalidated automatically; there is no manual retry or
manual recheck CLI for them.

Scanner-proved recovery for file-level local permission issues now clears only
the actionable `sync_failures` row for that path. It does not route through
permission recheck decisions and it does not delete `retry_state`.

Retry/trial reconstruction is retry-owned now. The engine revalidates due or
blocked retry work directly from `RetryStateRow`, exact `RetryWorkKey`
identity, and the current snapshot/baseline view. It does not synthesize
`SyncFailureRow` values to decide whether retry work is resolved, still
blocked, or newly actionable.

`scope_blocks` remains timer-only metadata. Releasing or discarding a scope
updates the blocked retry ledger transactionally so no orphaned blocked retry
rows survive a scope transition.

## What The Engine No Longer Owns

The engine no longer contains:

- sync-scope snapshots built from removed path-narrowing or marker-file features
- embedded shared-folder registries and nested follow-up runtime
- durable conflict request replay
- held-delete approval workflows or delete counters

Those concepts were deleted from the current architecture.
