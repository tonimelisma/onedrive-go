# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/watch_summary.go, internal/sync/condition_summary.go, internal/sync/engine_config.go, internal/sync/debug_event_sink.go, internal/sync/engine_debug_events.go, internal/sync/engine_scope_invariants.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_decisions.go, internal/cli/sync_flow.go, internal/cli/sync_runtime.go

Implements: R-2.1 [verified], R-2.8.3 [verified], R-2.8.5 [verified], R-2.10 [designed], R-2.14 [designed], R-2.16.2 [verified], R-2.16.3 [verified], R-6.3.4 [verified], R-6.3.5 [verified]

## Overview

The engine is the single-drive runtime owner. It coordinates:

- observation
- planning
- publication-only action commits
- execution
- durable outcome writes
- retry and trial scheduling
- scope lifecycle
- watch-mode reconcile and maintenance work

The target engine persists durable status through three authorities:

- `observation_issues`
- `retry_work`
- `block_scopes`

It does not use a mixed failure table as durable control state.

## Ownership Contract

- Owns: single-drive runtime orchestration, watch-mode mutable state,
  worker-result classification, retry/trial scheduling, and scope lifecycle.
- Does Not Own: SQLite schema, Graph normalization, config parsing, or
  multi-drive daemon lifecycle.
- Source of Truth: durable sync state in `SyncStore`, plus engine-owned
  in-memory runtime state for the currently running session.
- Allowed Side Effects: coordinating observers, planner, executor, and store
  writes through injected boundaries.
- Mutable Runtime Owner: `Engine` owns all single-drive mutable runtime state.
  In watch mode that includes the event loop, outbox, in-flight actions,
  retry/trial timers, scope state, and admission flags.
- Error Boundary: the engine translates observer, planner, executor,
  permission, and store outcomes into engine-owned reports, retries, scope
  transitions, and durable authority writes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| One-shot sync remains a bounded observe-plan-execute pass without a live user-intent mailbox. | `TestBootstrapSync_NoChanges`, `TestBootstrapSync_WithChanges`, `TestOneShotEngineLoop_ClosedResultsStillProcessBufferedRetryWork`, `TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady` |
| Watch mode keeps single-owner runtime admission, periodic maintenance, and external-change reconciliation inside the engine boundary. | `TestEngine_CascadeRecordAndComplete_RecordsBlockedRetryWork`, `TestEngine_ExternalDBChanged`, `TestWatchRuntime_ArmRetryTimer_KicksImmediatelyWhenRetryIsDue`, `TestRunWatch_ShutdownStopsRetryAndTrialTimers` |

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
4. build the current actionable set in Go from structural reconciliation plus
   explicit truth-availability overlays
5. reconcile durable retry/blocker state to that actionable set
6. commit any ready publication-only actions directly through the store
7. execute remaining concrete work once
8. persist outcomes and return a report

There is no mid-pass mailbox for user intent. New external DB writes during a
one-shot run are durable state for a later run.

`materializeCurrentActionPlan()` is the durable reconciliation boundary. It
should prune stale `retry_work`, prune empty `block_scopes`, and align blocked
work with active scopes. It does not persist a durable executable plan table.
Permission scopes remain probe-owned facts and are not discarded just because
their blocked retry rows happened to clear.

## Watch Mode

`RunWatch()` is the long-lived runtime. It owns:

- observer startup and shutdown
- dirty-signal intake and debounce
- snapshot refresh and SQLite reconciliation
- action admission and dispatch
- action completion drain
- retry and trial timer scheduling
- periodic recheck and full reconciliation
- graceful drain on shutdown

The watch loop is the single owner of mutable runtime state. Other packages may
signal it, but they do not mutate its runtime data structures directly.

Local watcher events, remote delta batches, websocket wakes, and full
reconciliation results are scheduler hints only. After debounce or wake, watch
mode refreshes current truth, runs SQL comparison/reconciliation, rebuilds the
current actionable set in Go, reconciles durable retry/blocker state, and then
admits runnable actions.

### Recheck And External DB Changes

Watch mode periodically checks SQLite `PRAGMA data_version`. If another
connection committed a write, `handleExternalChanges()` runs.

That hook is intentionally narrow. It rechecks externally cleared scope state
and aligns runtime admission with the persisted `block_scopes` and blocked
`retry_work` rows. It is not a generic user-intent ingestion path.

## Shared-Root Drives

Shared folders are separate configured drives. The engine therefore supports
two drive shapes:

- ordinary drive-root sessions
- shared-root sessions rooted below the remote drive root

Embedded shared-folder links discovered inside another synced drive are ignored
by observation and never become nested engine-owned sub-sessions.

## Conflict Handling

Conflicts remain engine-owned and immediate:

- edit/edit and create/create preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path
- edit/delete is planner-expanded into a local-wins upload
- executor-time local-delete hash mismatch reports a stale precondition so the
  next replan can emit the correct upload from current truth

There is no durable conflict-request workflow and no CLI `resolve` command.

## Outcome And Scope Lifecycle

The engine classifies results into:

- success cleanup
- retryable transient work
- actionable current-truth/content problems
- shared blocker activation / re-arm / release-or-discard decisions

### Durable persistence rules

- observation-time invalid or unsyncable truth -> `observation_issues`
- exhausted transient exact work -> `retry_work`
- shared blockers -> `block_scopes` plus blocked `retry_work`
- execution-discovered conditions that should be observation-owned do not write
  `observation_issues`; execution logs the invariant violation, persists only
  execution-owned state, and waits for the next observation pass to record the
  durable issue

### Runtime admission model

`DepGraph` remains dependency-only. The engine still performs final scope
admission after dependency readiness:

- build the graph from current actions
- ask the graph for ready work
- gate that ready work against active scopes
- dispatch allowed work
- keep blocked work represented durably in `retry_work`

The benchmark target is earlier durable reconciliation, not removal of the
post-graph admission gate.

### Scope taxonomy

The target scope families are:

- `service`
- `throttle:target:drive:<driveID>`
- `quota:own`
- `disk:local`
- `perm:dir:read:<path>`
- `perm:dir:write:<path>`
- `perm:remote:read:<boundary>`
- `perm:remote:write:<boundary>`
- `account:throttled`

Remote permission blockers are first-class persisted `block_scopes`, not
derived-only runtime state.

### Permission recovery

Permission scopes are revalidated automatically; there is no manual retry or
manual recheck CLI for them. Observation/probe may create or clear permission
scopes directly when current truth already proves the shared blocker or its
recovery. A raw `403` or `os.ErrPermission` is only a trigger to probe; the
probe result is what may activate or release the scope.

### Retry and trial reconstruction

Retry and trial reconstruction is retry-owned. The engine revalidates due or
blocked work directly from `retry_work`, exact semantic work identity, and the
current snapshot/baseline view. Scope lifecycle is owned only by
`block_scopes` plus blocked/unblocked `retry_work`. Timed transient scopes are
discarded when no blocked `retry_work` remains for their `scope_key`. Permission
scopes are instead retained until affirmative revalidation proves recovery.

## What The Engine Does Not Own

The engine does not own:

- multi-drive orchestration
- manual resolution workflows
- a durable action queue
- a mixed failure-reporting/control table

Those concepts do not belong in the target architecture.
