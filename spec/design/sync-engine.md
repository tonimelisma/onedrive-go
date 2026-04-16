# Sync Engine

GOVERNS: internal/sync/engine*.go, internal/sync/engine_config.go, internal/sync/debug_event_sink.go, internal/sync/engine_debug_events.go, internal/sync/engine_scope_invariants.go, internal/sync/permissions.go, internal/sync/permission_handler.go, internal/sync/permission_decisions.go, internal/cli/sync_flow.go, internal/cli/sync_runtime.go

Implements: R-2.1 [verified], R-2.8.3 [verified], R-2.8.5 [verified], R-2.10 [verified], R-2.14 [verified], R-2.16.2 [verified], R-2.16.3 [verified], R-6.3.4 [verified], R-6.3.5 [verified]

## Overview

The engine is the single-drive runtime owner. It coordinates:

- observation
- planning
- execution
- durable store commits
- retry and trial timers
- scope lifecycle
- watch-mode reconcile and maintenance work

Watch mode uses two fixed internal cadences:

- a restart-safe 24-hour full primary remote reconcile driven by `observation_state.last_full_remote_reconcile_at`
- a fixed 5-minute recurring local full scan for watch safety and no-watcher fallback

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
2. observe once
3. plan once
4. execute once
5. commit outcomes and return a report

There is no mid-pass mailbox for user intent. New external DB writes during a
one-shot run are simply durable state for a later run.

`sync --full` remains the explicit stronger-freshness path when incremental
delta visibility is not sufficient.

## Watch Mode

`RunWatch()` is the long-lived runtime. It owns:

- observer startup and shutdown
- buffered change intake
- action admission and dispatch
- worker result drain
- retry and trial timer scheduling
- periodic recheck and full reconciliation
- graceful drain on shutdown

The watch loop is the single owner of mutable runtime state. Other packages may
signal it, but they do not mutate its runtime data structures directly.

### Recheck And External DB Changes

Watch mode periodically checks SQLite `PRAGMA data_version`. If another
connection committed a write, `handleExternalChanges()` runs.

That reconciliation hook is intentionally narrow. It currently rechecks
externally cleared permission-scope state and releases any runtime permission
scope whose backing failure rows disappeared.

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
- edit/delete is auto-resolved with local-wins upload
- executor-time local-delete hash mismatch also routes through that same
  edit/delete recovery path

There is no durable conflict-request workflow and no CLI `resolve` command.

## Scope And Failure Lifecycle

The engine classifies worker results into:

- success cleanup
- retryable failure
- actionable failure
- scope activation / preserve / release decisions

Runtime scope state is an in-memory working set rebuilt from `scope_blocks` at
startup. Current persisted scope families are:

- `quota:own`
- `throttle:target:drive:*`
- `service`
- `perm:dir:*`
- `perm:remote:*`
- `disk:local`

Account-auth rejection is no longer a persisted sync scope. Durable
account-auth state lives in the managed catalog, and sync consults that catalog
before startup proof and after fatal unauthorized results.

Permission scopes are revalidated automatically; there is no manual retry or
manual recheck CLI for them.

## What The Engine No Longer Owns

The engine no longer contains:

- sync-scope snapshots built from removed path-narrowing or marker-file features
- embedded shared-folder registries and nested follow-up runtime
- durable conflict request replay
- held-delete approval workflows or delete counters

Those concepts were deleted from the current architecture.
