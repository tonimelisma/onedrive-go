# Sync Store

GOVERNS: internal/sync/store.go, internal/sync/store_types.go, internal/sync/store_inspect.go, internal/sync/store_read_snapshots.go, internal/sync/store_read_remote_state.go, internal/sync/store_read_failures.go, internal/sync/store_local_state.go, internal/sync/store_observation_state.go, internal/sync/store_retry_state.go, internal/sync/store_scratch.go, internal/sync/store_recreate.go, internal/sync/schema.go, internal/sync/tx.go, internal/sync/store_write_baseline.go, internal/sync/store_write_observation.go, internal/sync/store_write_failures.go, internal/sync/store_write_scope_blocks.go, internal/sync/store_admin.go, internal/syncverify/verify.go, internal/cli/status.go, internal/cli/status_snapshot.go

Implements: R-2.5 [verified], R-2.7 [verified], R-2.10.33 [verified], R-2.15.1 [verified], R-6.5.1 [verified], R-6.5.2 [verified]

## Overview

`SyncStore` is the sole durable owner of per-drive sync state. It owns:

- canonical schema application and validation
- baseline persistence
- local snapshot persistence
- remote mirror persistence
- retry-state persistence
- retry/actionable failure persistence
- scope-block persistence
- one-shot run-status persistence
- read-only inspectors used by CLI status

It does **not** own planning policy, conflict-resolution policy, delete-safety
policy, or live watch-mode coordination. Those belong to the engine.

## Ownership Contract

- Owns: SQLite truth, transactions, restart-safe rows, and read-only snapshot helpers.
- Does Not Own: Graph calls, local filesystem observation, planner decisions, or watch-loop runtime state.
- Source of Truth: The current canonical SQLite schema plus the rows it defines.
- Allowed Side Effects: SQLite reads, writes, schema bootstrap/validation, checkpoints, and read-only inspection.
- Mutable Runtime Owner: `SyncStore` owns its DB handle and rebuildable in-memory baseline cache. No background goroutines.
- Error Boundary: Store methods add SQLite/store context, but they do not invent new sync policy.

## Verified By

| Behavior | Evidence |
| --- | --- |
| The store remains the sole durable authority for baseline, local/remote snapshots, retry state, scope-block, observation-state, and run-status rows. | `TestNewSyncStore_CreatesDB`, `TestNewSyncStore_AppliesSchema`, `TestWriteSyncRunStatus_RoundTrip`, `TestSyncStore_FailureAdminMutations` |
| Read-only snapshot helpers back status without reopening deleted conflict/delete-approval workflows. | `TestReadDriveStatusSnapshotAndScopeBlockHelpers`, `TestSyncStore_ListVisibleIssueGroups`, `TestQuerySyncState_UsesReadOnlyProjectionHelper` |
| Schema validation stays store-owned, while engine startup owns destructive recreate for unusable or unsupported DBs. | `TestNewSyncStore_CreatesCanonicalSchema`, `TestNewSyncStore_RejectsNonCanonicalSchema`, `TestNewEngine_RecreatesNonSQLiteStateDB`, `TestNewEngine_RecreatesIncompatibleSchemaStateDB`, `TestNewEngine_RecreatesUnsupportedLegacyPersistedState` |

## Write Responsibilities

### Observation writes

`CommitObservation()` is the remote-observation boundary. It atomically:

- upserts or deletes `remote_state` rows derived from observation
- advances `observation_state.cursor`

Observation never writes planner state or runtime intent.

Local observation writes belong to `local_state`; the canonical store now owns
durable current-truth tables for both sides even though planner/executor
cutovers may still arrive in later increments.

One-shot full scans replace the entire `local_state` snapshot in one
transaction. The stored rows represent the latest admissible local truth for
that pass, not a journal of local events.

Dry-run planning does not stage those snapshot writes inside the durable store.
Instead, the store can seed an isolated scratch `SyncStore` with the current
committed `baseline`, `remote_state`, and `observation_state`; dry-run then
commits preview-only `remote_state` and `local_state` rows there before
querying SQLite comparison and reconciliation.

### Outcome writes

`CommitMutation()` is the successful-execution boundary. It updates `baseline`
and, when needed, keeps `remote_state` aligned with the remote truth implied by
the successful action.

That same store boundary also owns publication-only planner actions:

- `ActionUpdateSynced` publishes an upsert for converged current truth
- `ActionCleanup` publishes a delete for baseline rows absent from both current
  snapshots

The engine may commit those two action types directly without worker/executor
dispatch, but `CommitMutation()` remains the only durable publication path.
`publicationMutationFromAction()` lives beside that boundary so planner intent
becomes a `BaselineMutation` at the same authority boundary that commits it.

`RefreshLocalBaseline()` is the narrower reconciliation path for cases where
local disk has become authoritative without a new executor-produced transfer
result, such as conflict-copy preservation and other local layout convergence.

### Failure writes

`RecordFailure()` is the one durable write path for retryable and actionable
path failures. The engine/classifier supplies the already-decided issue type,
category, scope key, and retry delay; the store persists them transactionally.

`retry_state` is the durable retry ledger. It persists retryable and blocked
work keyed to semantic work identity, while the executable action set remains
runtime-owned in Go. `sync_failures` remains the reporting surface, but it no
longer decides which retryable rows are due, which blocked row is trialed, or
which scope-backed rows keep runtime admission blocked.

Issue-only cleanup and exact retry cleanup are separate store boundaries now:

- actionable issue cleanup may delete `sync_failures` rows only
- retry-owned resolution may delete one exact `retry_state` work item and the
  matching transient reporting row in the same transaction
- scope-owned release/discard may mutate `scope_blocks`, scoped
  `sync_failures`, and blocked `retry_state` rows for that scope

Supporting failure mutations include:

- issue-only cleanup helpers for actionable rows
- `ResolveTransientRetryWork()` for exact retry-work resolution
- `UpsertActionableFailures()`
- scope-owned release/discard helpers that move or delete blocked rows

### Scope writes

`UpsertScopeBlock()`, `DeleteScopeBlock()`, and `ListScopeBlocks()` are the
scope-block boundary. The engine remains the sole owner of when scopes are set,
preserved, retried, released, or discarded; the store just persists that
runtime-owned decision. `scope_blocks` is timer/trial metadata authority, not a
second owner of blocked work. When a scope is released or discarded, the store
updates both `sync_failures` and `retry_state` in the same transaction so the
retry ledger cannot lag the scope transition.

Remote permission scopes are not persisted as `scope_blocks`. They are rebuilt
from blocked `retry_state` rows keyed by `perm:remote:*` scope keys.

### Admin writes

`store_admin.go` owns administrative store helpers such as:

- reset/remove retry rows
- release/discard scope state
- one-shot run-status writes

## Read Responsibilities

Read-only store helpers are intentionally separate from writable paths.

- `store_read_remote_state.go` reads current remote mirror truth
- `store_read_failures.go` reads raw persisted `sync_failures` rows for
  store-owned projections and tests
- `visible_issues.go` owns the higher-level visible issue projection used by
  status/watch surfaces
- `store_retry_state.go` owns retry/trial reads such as ready blocked work
- `store_read_snapshots.go` and `store_inspect.go` build status views

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
3. creates or validates the current canonical schema
4. returns a ready store

There is no migration history in the current architecture. New DBs bootstrap
directly into the simplified schema. Non-canonical stores are rejected loudly
at the store boundary. Engine startup may then discard an unusable or
unsupported DB file family and recreate a fresh canonical store once before it
gives up.

## What The Store No Longer Owns

The store no longer persists:

- conflict history/request workflows
- delete-safety approvals or held-delete ledgers
- sync-scope snapshots or filtered remote-state projections
- embedded shared-folder registries inside another drive

Those concepts were deleted from the current architecture. Conflicts are now
resolved immediately inside engine/executor flows, delete safety is ordinary
per-item safety only, and shared folders are separate configured drives.
