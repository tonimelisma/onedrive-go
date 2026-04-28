# Sync Store

GOVERNS: internal/sync/store.go, internal/sync/store_types.go, internal/sync/store_inspect.go, internal/sync/store_read_remote_state.go, internal/sync/store_local_state.go, internal/sync/store_observation_state.go, internal/sync/store_observation_issues.go, internal/sync/observation_reconcile_policy.go, internal/sync/store_retry_work.go, internal/sync/store_scratch.go, internal/sync/schema.go, internal/sync/tx.go, internal/sync/store_write_baseline.go, internal/sync/store_write_observation.go, internal/sync/store_write_block_scopes.go, internal/sync/block_scope_rows.go, internal/sync/store_scope_admin.go, internal/sync/store_compatibility.go, internal/sync/store_reset.go, internal/sync/shortcut_root_state.go, internal/sync/shortcut_root_store.go, internal/sync/shortcut_alias_mutation.go, internal/sync/condition_projection.go, internal/sync/blocked_retry_projection.go, internal/sync/scope_key.go, internal/sync/scope_semantics.go, internal/sync/scope_block.go, internal/syncverify/verify.go, internal/cli/status.go, internal/cli/status_snapshot.go

Implements: R-2.5 [designed], R-2.7 [verified], R-2.10.33 [designed], R-2.15.1 [designed], R-6.5.1 [verified], R-6.5.2 [verified]

## Overview

`SyncStore` is the sole durable owner of per-mount sync state. In the target
architecture it owns:

- canonical schema application and validation
- baseline persistence
- local snapshot persistence
- remote mirror persistence
- observation issue persistence
- retry-work persistence
- block-scope persistence
- observation resume/cadence persistence
- state-DB diagnosis and explicit reset support
- read-only raw row access used by `status`

It does not own planning policy, execution policy, or a competing history/status
model. Shortcut status display metadata is sync-owned but derived from
`ShortcutRootState`; the store persists the state rows, not presentation copy.
Read-only shortcut status helpers return `ShortcutRootStatusView`, a sync-owned
projection that gives the CLI display-ready fields without exposing raw
`shortcut_roots` policy columns such as protected-path bookkeeping, blocker
detail, or waiting replacement internals.

For parent namespace engines, it also owns parent-local shortcut-root state in
`shortcut_roots`. Those rows are not child content state: they describe the
shortcut placeholder observed in the parent drive, the protected parent-local
alias path(s), the known target identity, and parent-owned lifecycle blockers
such as final drain or same-path replacement waiting. Child content retry,
observation issues, and target sync state remain in the child engine's own
state DB.

`shortcut_roots.local_root_identity` is the parent-owned lease on the concrete
local alias directory. It lets the parent detect same-folder renames and lets
managed child engines reject a deleted/recreated projection before observation
turns it into false local truth. Removed/final-drain roots keep their protected
paths until either clean child drain is acknowledged or the user deletes the
local projection directory as manual discard. Manual discard removes the parent
root row but does not mutate the child target; multisync owns purging the child
DB and other child artifacts after it sees the release.

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
| Read-only status and derived-truth queries continue to depend on store-owned raw-authority helpers rather than ad hoc writable opens; shortcut status leaves the store through sync-owned status views instead of raw shortcut-root rows. | `TestReadDriveStatusSnapshot`, `TestReadPathTruthStatus_DerivesUnavailableTruthFromDurableAuthorities`, `TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper`, `TestStatusCommand_UnreadableStateStoreFallsBackToEmptySyncState`, `TestBuildChildStatusMount_RendersLifecycleState` |
| Local filesystem identity is persisted as generic truth for files and directories in `local_state` and `baseline`, allowing local move detection without shortcut-specific sibling scans. | `TestReplaceLocalState_PersistsFilesystemIdentity`, `TestCommitMutation_PersistsLocalFilesystemIdentity`, `TestQueryReconciliationState_LocalFolderMoveUsesFilesystemIdentity` |
| Parent shortcut-root lifecycle state is stored in the parent sync store, rebuilt into parent-owned observation protection, and applied before the parent publishes child process and cleanup work to multisync. Empty complete remote shortcut observation batches are persisted and retire old roots; child final-drain acknowledgement first persists `removed_release_pending`; release cleanup later moves old roots to `removed_child_cleanup_pending` or promotes waiting replacements; cleanup-blocked release failures are persisted before returning errors; child artifact cleanup acknowledgement deletes cleanup-pending rows; duplicate automatic shortcut targets are parent-owned blocked roots. Cleanup requests are derived from these rows with explicit child mount ID and local-root scope, not reconstructed by multisync. | `TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots`, `TestSyncStore_EmptyCompleteShortcutTopologyMarksRemovedFinalDrain`, `TestSyncStore_markShortcutChildFinalDrainReleasePendingIsDurable`, `TestSyncStore_acknowledgeShortcutChildArtifactsPurgedRemovesCleanupPendingRoot`, `TestSyncStore_SamePathReplacementWaitsBehindRetiringRoot`, `TestSyncStore_DuplicateAutomaticShortcutTargetIsParentBlocked`, `TestEngine_ReconcileShortcutRootLocalStateRetriesRemovedReleasePending`, `TestEngine_ReconcileShortcutRootLocalStatePersistsCleanupBlockedBeforeReturningError`, `TestEngine_ReconcileShortcutRootLocalStatePromotesWaitingReplacementAfterReleasePending`, `TestNewMountEngine_LoadsPersistedShortcutProtectedRoots`, `TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler` |
| Shortcut alias mutation is a parent-engine-internal operation by binding item ID and updates parent shortcut-root state. | `TestEngine_ShortcutAliasRenameMutatesThroughParentAndUpdatesRootState`, `TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain` |

## Write Responsibilities

### Observation writes

`CommitObservation()` is the remote-observation boundary. It atomically:

- upserts or deletes `remote_state` rows derived from observation
- advances `observation_state.cursor`

Each `remote_state` row persists the true owning remote `drive_id` seen during
observation. `observation_state.content_drive_id` identifies the remote drive
for the mounted content root that owns the DB and cursor; it is not the
durable owner of every remote row. If a later observation corrects a row's
owning `drive_id` without changing path/hash/mtime metadata,
`CommitObservation()` still updates the stored row owner so downstream planning
and execution read the repaired durable truth.

Local observation writes belong to `local_state`. Full scans replace the entire
`local_state` snapshot in one transaction, including filesystem identity for
files and directories when the platform provides a device/inode pair.

Observation-owned durable problems belong to `observation_issues`, not retry
lanes or `block_scopes`. Read-denied subtree boundaries are represented as
observation issue rows tagged with the corresponding `ScopeKey`; later truth
reads derive blocked descendants from those tagged boundary issues instead of a
second durable scope table.

Only observation-owned reconciliation mutates `observation_issues`.
Worker-result handling, held retry release, and held trial release may read
observation facts, but they must not create, clear, or rewrite
`observation_issues`.

Observation-owned reconciliation supports two scopes of authority:

- whole-observation batches replace the managed observation issue types they
  own
- single-path observation batches manage only the exact observed path set they
  proved

### Mutation writes

`CommitMutation()` is the successful-execution boundary. It updates `baseline`
and, when needed, keeps `remote_state` aligned with the remote truth implied by
the successful action.

Baseline writes also publish the local filesystem identity observed for the
successful mutation. Local move detection compares that last-synced identity to
the next `local_state` snapshot; when identity is unavailable, file moves may
fall back to unique content-hash candidates, but directory moves do not.

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

- observation-findings reconciliation that replaces the current
  observation-owned issue set in one transaction, either as a full managed
  issue-type set or as an exact-path reconciliation for single-path observation
- exact retry-work upsert/delete helpers
- retry-work rearm helpers that reschedule exact held work without inventing
  new planning or observation authority
- scope release/discard/extend helpers that mutate `block_scopes` and linked
  blocked `retry_work` in one transaction
- retry-resolution helpers that delete exact `retry_work` and return the
  resolved `retry_work` row for engine-owned cleanup decisions

`retry_work` is the single durable lane for exact pending roots regardless of
whether their next execution boundary is worker-side I/O or the engine-owned
publication-drain stage. The store does not split publication retries into a
second durable table. Admission, completion, held-release, and
direct permission handlers all act on that same durable lane; the store owns
the rows, while the engine owns the policy around them. Delayed exact retries
and blocked scope retries now use separate store helpers, but they still land
in the same `retry_work` table shape. Blocked rows are canonicalized
engine-side: all runtime paths persist `blocked=true`, the exact `scope_key`,
and the next due retry/trial time. Condition family, HTTP status, and error
text remain runtime/log concerns instead of durable retry-row columns.
Current-truth loading, planner-input loading, and dry-run scratch preparation
for that policy live in `engine_current_plan.go`, while startup/runtime
reconcile and admission live in `engine_startup.go` and
`engine_runtime_start.go`.

The store does not own a mixed failure table, failure-role transitions,
timer-time stale-row cleanup, or a store-owned grouped condition projection.

Within that observation-findings boundary, pure reconciliation policy stays
separate from SQLite mutation. The store computes the exact observation issue
upserts and deletes to reconcile in a deterministic helper, then applies that
plan inside one transaction. SQLite helpers do not own the policy for what a
batch manages, and they do not re-infer managed issue types from
`ObservationFindingsBatch` during apply. That policy should read as explicit
set reconciliation: current managed observation issues, desired managed
observation issues, exact deletes, exact upserts. The key used for those exact
deletes is the same managed issue identity used on both the current and
desired sides; the store does not need a second delete-only shape to express
it.

Derived truth inspection stays read-only and authority-based. Observation-owned
boundary issues tagged with read-scope keys suppress descendant truth through
`ReadPathTruthStatus`; timed `block_scopes` for write blockers do not change
truth availability on their own.

### Admin writes

Administrative write helpers are split by authority:

- store compatibility helpers diagnose incompatible DBs
- `store_reset.go` owns explicit delete-and-recreate reset

## Read Responsibilities

Read-only store helpers are intentionally narrow:

- raw/narrow reads for `remote_state`, `local_state`, `baseline`,
  `observation_state`
- raw/narrow reads for `observation_issues`
- raw/narrow reads for `retry_work`
- raw/narrow reads for `block_scopes`
- derived read helpers that compose only those durable authorities into
  query-scoped debug views such as per-path truth availability

`remote_state` reads return the durable per-row `drive_id` from the table
itself. Fallback content drive IDs exist only for empty stores or absent per-row
durable state and must not overwrite a stored row owner on read.

`remote_state` intentionally does not persist remote parent ancestry. Sparse
path recovery during observation still uses baseline parent context, and
successful execution reconstructs outcome parent IDs from live Graph results or
baseline path resolution before baseline publication.

`status` composes directly from those authorities plus counts derived from
`baseline` and `remote_state`. The store must not own grouping or rendering
policy for `status` or watch summaries, and it no longer persists a separate
last-sync history table.
Shared condition-family grouping and ordering belong to
`internal/sync/condition_keys.go`, while raw grouped projection over durable
authorities belongs to `internal/sync/condition_projection.go`; neither
responsibility belongs to store query helpers.
Store maintenance must also keep `block_scopes` honest:

- timed scopes may exist only while blocked `retry_work` still exists for the
  same `scope_key`
- no persisted scope row may be orphaned from blocked `retry_work`; if blocked
  work no longer references the `scope_key`, the row is durable garbage and
  invariant checks must fail loudly

That liveness rule is shared with engine startup normalization and runtime
scope release/rearm handling. The store does not invent a separate notion of
when an empty timed scope may survive; it applies the same "blocked work or
discard" policy the engine uses during normal current-plan build/runtime-state
reconcile.

Pruning an empty scope removes only the scope row. Ready `retry_work` that no
longer depends on that scope must survive pruning; only explicit discard of a
still-blocked scope may delete the blocked work under it.

`block_scopes` persists only:

- `scope_key`
- `trial_interval`
- `next_trial_at`

`scope_key` remains the durable identity used by `retry_work`, while in-memory
scope semantics are reconstructed from `DescribeScopeKey` during read/write
validation. The shared raw block-scope read path therefore validates the
durable key once and returns a canonical `BlockScope` shape without storing a
second copy of parsed metadata in SQLite.

`ScopeKey.PersistsInBlockScopes()` is the single sync-domain rule for whether a
scope belongs in `block_scopes`. Timed blocked-work scopes persist there;
observation-owned read boundaries do not.

`observation_state` persists only the restart-safe next full-remote refresh
deadline, not the runtime mode that produced it. Runtime decides whether the
current observation path is delta-based or enumerate-only and then stores the
next due time accordingly. When watch mode shortens that persisted deadline
after startup, the same watch step must rearm the live timer immediately so
the process does not keep sleeping on the superseded later deadline.

`retry_work` stores only exact held roots. Dependents blocked behind those
roots remain dependency state in the current runtime; they are not persisted as
cascade retry rows.

That exact-root rule includes publication-only actions such as
`ActionUpdateSynced` and `ActionCleanup`. They may persist in `retry_work`, but
their retry still re-enters the engine-owned publication-drain stage rather
than worker dispatch.

Read-denied subtree boundaries do not persist as `block_scopes`. They remain
tagged on `observation_issues` via `ScopeKey`, and truth-status reads derive
blocked descendants from those boundary facts.

`store_inspect.go` now owns two read-side shapes:

- `DriveStatusSnapshot` for raw durable authorities used by `status` and watch
- `ReadPathTruthStatus` for derived truth availability over requested paths,
  built from `observation_issues` plus boundary-tagged `ScopeKey` facts without
  materializing fake durable descendant rows

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
