# Sync Store

GOVERNS: internal/sync/store.go, internal/sync/store_inspect.go, internal/sync/store_read_snapshots.go, internal/sync/store_read_remote_state.go, internal/sync/store_read_failures.go, internal/sync/schema.go, internal/sync/migrations/*.sql, internal/sync/tx.go, internal/sync/store_write_baseline.go, internal/sync/store_write_observation.go, internal/sync/store_write_failures.go, internal/sync/store_write_scope_blocks.go, internal/sync/store_repair.go, internal/syncverify/verify.go, internal/cli/status.go, internal/cli/status_snapshot.go, internal/cli/recover.go, internal/cli/recover_flow.go

Implements: R-2.5 [verified], R-2.7 [verified], R-2.10.33 [verified], R-2.15.1 [verified], R-6.5.1 [verified], R-6.5.2 [verified]

## Overview

`SyncStore` is the sole durable owner of per-drive sync state. It owns:

- schema application and migration validation
- baseline persistence
- remote mirror persistence
- retry/actionable failure persistence
- scope-block persistence
- sync metadata persistence
- read-only inspectors used by CLI status and recovery

It does **not** own planning policy, conflict-resolution policy, delete-safety
policy, or live watch-mode coordination. Those belong to the engine.

## Ownership Contract

- Owns: SQLite truth, transactions, restart-safe rows, and read-only snapshot helpers.
- Does Not Own: Graph calls, local filesystem observation, planner decisions, or watch-loop runtime state.
- Source of Truth: Embedded goose migrations plus the rows they define.
- Allowed Side Effects: SQLite reads, writes, migrations, checkpoints, and read-only inspection.
- Mutable Runtime Owner: `SyncStore` owns its DB handle and rebuildable in-memory baseline cache. No background goroutines.
- Error Boundary: Store methods add SQLite/store context, but they do not invent new sync policy.

## Verified By

| Behavior | Evidence |
| --- | --- |
| The store remains the sole durable authority for baseline, remote mirror, failure, scope-block, and sync-metadata rows. | `TestNewSyncStore_CreatesDB`, `TestNewSyncStore_AppliesSchema`, `TestWriteSyncMetadata_RoundTrip`, `TestSyncStore_FailureAdminMutations` |
| Read-only snapshot helpers back status/recovery without reopening deleted conflict/delete-approval workflows. | `TestReadDriveStatusSnapshotAndScopeBlockHelpers`, `TestSyncStore_ListVisibleIssueGroups`, `TestQuerySyncState_UsesReadOnlyProjectionHelper` |
| Store repair and migration behavior stay store-owned and transactional. | `TestRepairStateDB_RepairsReadableStoreInPlace`, `TestSyncStore_MigrationProviderFreshDBUpgradesToCurrent`, `TestNewSyncStore_RejectsUnversionedExistingStateDB` |

## Write Responsibilities

### Observation writes

`CommitObservation()` is the remote-observation boundary. It atomically:

- upserts or deletes `remote_state` rows derived from observation
- advances the matching `delta_tokens` cursor

Observation never writes planner state or runtime intent.

### Outcome writes

`CommitMutation()` is the successful-execution boundary. It updates `baseline`
and, when needed, keeps `remote_state` aligned with the remote truth implied by
the successful action.

`RefreshLocalBaseline()` is the narrower reconciliation path for cases where
local disk has become authoritative without a new executor-produced transfer
result, such as conflict-copy preservation and other local layout convergence.

### Failure writes

`RecordFailure()` is the one durable write path for retryable and actionable
path failures. The engine/classifier supplies the already-decided issue type,
category, scope key, and retry delay; the store persists them transactionally.

Supporting failure mutations include:

- `ClearSyncFailure*` helpers
- `TakeSyncFailure()`
- `UpsertActionableFailures()`
- scope-owned release/discard helpers that move or delete blocked rows

### Scope writes

`UpsertScopeBlock()`, `DeleteScopeBlock()`, and `ListScopeBlocks()` are the
scope-block boundary. The engine remains the sole owner of when scopes are set,
preserved, retried, released, or discarded; the store just persists that
runtime-owned decision.

### Repair/admin writes

`store_repair.go` owns deterministic store repair and recovery helpers such as:

- reset/remove retry rows
- release/discard scope state
- sync metadata writes
- integrity-oriented repair primitives used by `recover`

## Read Responsibilities

Read-only store helpers are intentionally separate from writable paths.

- `store_read_remote_state.go` reads current remote mirror truth
- `store_read_failures.go` reads retry/actionable failures
- `store_read_snapshots.go` and `store_inspect.go` build status/recovery views

CLI `status` and status-like flows should prefer these read-only helpers rather
than opening a writable store.

## Baseline Cache

`SyncStore` maintains an in-memory baseline cache as a rebuildable optimization.
The cache is loaded from SQLite on demand and updated after committed baseline
mutations. If the store detects impossible cache state, it drops and reloads
from SQLite instead of creating a second authority.

## Schema And Open Semantics

`NewSyncStore()`:

1. prepares the managed DB path
2. opens SQLite in WAL mode
3. applies embedded goose migrations
4. returns a ready store

The current schema version is `1`. Old tables for durable conflict requests,
held deletes, embedded shared-folder registries, and sync-scope snapshots were removed in
the current architecture. New DBs therefore bootstrap directly into the
simplified schema.

Stores with missing or malformed goose history are rejected loudly. Recovery is
the supported path when the DB cannot be trusted.

## What The Store No Longer Owns

The store no longer persists:

- conflict history/request workflows
- delete-safety approvals or held-delete ledgers
- sync-scope snapshots or filtered remote-state projections
- embedded shared-folder registries inside another drive

Those concepts were deleted from the current architecture. Conflicts are now
resolved immediately inside engine/executor flows, delete safety is ordinary
per-item safety only, and shared folders are separate configured drives.
