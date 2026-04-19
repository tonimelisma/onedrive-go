# Sync Store

GOVERNS: internal/sync/store.go, internal/sync/store_types.go, internal/sync/store_inspect.go, internal/sync/store_read_remote_state.go, internal/sync/store_local_state.go, internal/sync/store_observation_state.go, internal/sync/store_observation_issues.go, internal/sync/store_retry_work.go, internal/sync/store_scratch.go, internal/sync/schema.go, internal/sync/tx.go, internal/sync/store_write_baseline.go, internal/sync/store_write_observation.go, internal/sync/store_write_block_scopes.go, internal/sync/store_run_status.go, internal/sync/store_scope_admin.go, internal/sync/store_compatibility.go, internal/sync/store_reset.go, internal/sync/visible_conditions.go, internal/sync/condition_summary.go, internal/sync/scope_key.go, internal/syncverify/verify.go, internal/cli/status.go, internal/cli/status_snapshot.go

Implements: R-2.5 [designed], R-2.7 [verified], R-2.10.33 [designed], R-2.15.1 [designed], R-6.5.1 [verified], R-6.5.2 [verified]

## Overview

`SyncStore` is the sole durable owner of per-drive sync state. In the target
architecture it owns:

- canonical schema application and validation
- baseline persistence
- local snapshot persistence
- remote mirror persistence
- observation issue persistence
- retry-work persistence
- block-scope persistence
- observation resume/cadence persistence
- one-shot run-status persistence
- state-DB diagnosis and explicit reset support
- read-only raw row access used by `status`

It does not own planning policy, execution policy, or a competing status model.

## Ownership Contract

- Owns: SQLite truth, transactions, restart-safe rows, and narrow read helpers.
- Does Not Own: Graph calls, local filesystem observation, planner decisions,
  worker scheduling, or status rendering policy.
- Source of Truth: the canonical SQLite schema plus the rows it defines.
- Allowed Side Effects: SQLite reads, writes, schema bootstrap/validation,
  checkpoints, and explicit reset/recreate.
- Mutable Runtime Owner: `SyncStore` owns its DB handle and rebuildable
  in-memory baseline cache. It has no background goroutines.
- Error Boundary: store methods add SQLite/store context, but they do not
  invent new sync policy.

## Verified By

| Behavior | Evidence |
| --- | --- |
| The store remains the sole durable owner of schema validation/open semantics and explicit reset flows. | `TestNewSyncStore_CreatesDB`, `TestNewSyncStore_AppliesSchema`, `TestNewSyncStore_CreatesCanonicalSchema`, `TestNewSyncStore_RejectsNonCanonicalSchema`, `TestRunDriveResetSyncStateWithInput_ResetsAndRecreatesStateDB` |
| Read-only status queries continue to depend on store-owned read helpers rather than ad hoc writable opens. | `TestReadDriveStatusSnapshot`, `TestQuerySyncState_UsesReadOnlyProjectionHelper`, `TestStatusCommand_UnreadableStateStoreFallsBackToEmptySyncState` |

## Write Responsibilities

### Observation writes

`CommitObservation()` is the remote-observation boundary. It atomically:

- upserts or deletes `remote_state` rows derived from observation
- advances `observation_state.cursor`

Local observation writes belong to `local_state`. Full scans replace the entire
`local_state` snapshot in one transaction.

Observation-owned durable problems belong to `observation_issues`, not retry
lanes. Observation may also create `block_scopes` directly when current truth
already proves a shared blocker.

### Mutation writes

`CommitMutation()` is the successful-execution boundary. It updates `baseline`
and, when needed, keeps `remote_state` aligned with the remote truth implied by
the successful action.

That same store boundary also owns publication-only planner actions:

- `ActionUpdateSynced` publishes an upsert for converged current truth
- `ActionCleanup` publishes a delete for baseline rows absent from both current
  snapshots

### Outcome writes

The target store API persists three durable control authorities:

- `observation_issues` for durable current-truth/content problems
- `retry_work` for exact delayed work
- `block_scopes` for shared blockers and trial timing

Supporting outcome mutations should stay separate by owner:

- observation-issue upsert/delete helpers
- exact retry-work upsert/delete helpers
- scope release/discard/extend helpers that mutate `block_scopes` and linked
  blocked `retry_work` in one transaction
- retry-resolution helpers that delete exact `retry_work` and return the
  resolved `retry_work` row for engine-owned cleanup decisions

The store does not own a mixed failure table, failure-role transitions, or a
store-owned competing status projection.

### Admin writes

Administrative write helpers are split by authority:

- `store_run_status.go` owns one-shot run-status writes
- store compatibility helpers diagnose incompatible DBs
- `store_reset.go` owns explicit delete-and-recreate reset

## Read Responsibilities

Read-only store helpers are intentionally narrow:

- raw/narrow reads for `remote_state`, `local_state`, `baseline`,
  `observation_state`, `run_status`
- raw/narrow reads for `observation_issues`
- raw/narrow reads for `retry_work`
- raw/narrow reads for `block_scopes`

`status` should compose its output directly from those authorities. The store
may offer grouping primitives, but it must not become a second owner of status
policy. Store maintenance must also keep `block_scopes` honest: a scope row may
exist only while blocked `retry_work` still exists for the same `scope_key`.

## State-DB Diagnosis And Reset

Two store-owned helpers isolate DB lifecycle policy:

- `store_compatibility.go` opens an existing DB non-mutating, classifies
  unreadable or unsupported stores, and returns typed incompatible-store errors
- `store_reset.go` deletes one drive's DB file family and recreates a fresh
  canonical DB in place

Engine startup may use the diagnosis helper, but it must not call the
destructive reset helper automatically. The explicit CLI reset command owns the
delete-and-recreate action.

## Baseline Cache

`SyncStore` maintains an in-memory baseline cache as a rebuildable
optimization. If the store detects impossible cache state, it drops and reloads
from SQLite instead of creating a second authority.

## Schema And Open Semantics

`NewSyncStore()`:

1. prepares the managed DB path
2. opens SQLite in WAL mode
3. creates or validates the current canonical schema
4. returns a ready store

The target architecture prefers a new store generation plus explicit reset over
compatibility bridges. New DBs bootstrap directly into the current schema.
Non-canonical stores are rejected loudly at the store boundary.

## What The Store Does Not Own

The store does not own:

- the actionable set
- dependency ordering
- retry scheduling policy
- worker dispatch
- status rendering policy
- manual resolution workflows

Those concerns belong to the engine or CLI.
