# Sync Store

GOVERNS: internal/cli/status.go, internal/cli/status_snapshot.go, internal/sync/baseline_orphans.go, internal/sync/store_remote_twins.go, internal/sync/store_pending_creates.go, internal/sync/store_held_deletes.go, internal/sync/block_scope_rows.go, internal/sync/blocked_retry_projection.go, internal/sync/condition_projection.go, internal/sync/observation_reconcile_policy.go, internal/sync/schema.go, internal/sync/scope_lifecycle_policy.go, internal/sync/sqlite_compare.go, internal/sync/store.go, internal/sync/store_compatibility.go, internal/sync/store_inspect.go, internal/sync/store_local_state.go, internal/sync/store_observation_issues.go, internal/sync/store_observation_state.go, internal/sync/store_planner_visible.go, internal/sync/store_read_remote_state.go, internal/sync/store_reset.go, internal/sync/store_retry_work.go, internal/sync/store_scope_admin.go, internal/sync/store_scratch.go, internal/sync/store_sql_encoding.go, internal/sync/store_types.go, internal/sync/store_write_baseline.go, internal/sync/store_write_block_scopes.go, internal/sync/store_write_observation.go, internal/sync/truth_status.go, internal/sync/tx.go, internal/syncverify/report.go, internal/syncverify/verify.go

LAYER: 1

Implements: R-2.5 [designed], R-2.5.7 [verified], R-2.5.8 [verified], R-2.7 [verified], R-2.8.8 [verified], R-2.8.17 [verified], R-2.12.7 [verified], R-2.10.33 [designed], R-2.10.48 [verified], R-2.10.49 [verified], R-2.15.1 [designed], R-6.5.1 [verified], R-6.5.2 [verified]

## Verification Counts Only What It Compared

Baseline verification reports files it could not check separately from files it
checked and found intact. A baseline entry can carry no local hash --
SharePoint-enriched files populate only `remote_hash`, and the scanner records
an empty hash when hashing fails -- and those used to be counted as verified.

That answers "is my data intact" with evidence that does not exist, and it is
the same mistake as treating equal sizes as proof of equal content: absence of
a comparison is not a passing comparison. The count a user reads now means what
it says, and the unchecked files are named so the gap is visible rather than
absorbed.

A file that could not be stat'ed or read is reported as `unreadable`, with the
I/O error in `reason`, not as `missing` or `hash_mismatch`: an unreadable file
is not evidence of loss or corruption, and an error string is never put in the
`actual` hash field. The report is the output; verification logs per-file
problems at DEBUG only, so a tree of unreadable files does not also print a
warning per file. Evidence: `TestVerifyBaseline_ClassifiesStatErrorsAsUnreadable`,
`TestVerifyBaseline_ClassifiesRootedPathErrorsAsUnreadable`,
`TestVerifyBaseline_ClassifiesHashComputationErrorsAsUnreadable`.

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
- observation resume/cadence and local-truth-confidence persistence
- state-DB diagnosis and explicit reset support
- read-only raw row access used by `status`

It does not own planning policy, execution policy, or a competing history/status
model.

`store_planner_visible.go` (previously `planner_visibility.go`) is store-owned
for the same reason every other table writer is: it issues the `INSERT` and
`DELETE` statements that materialize the transaction-local
`planner_visible_local_state` and `planner_visible_remote_state` tables, and it
is the only file outside this family that consumed the store's SQL encoding
helpers (`boolInt`, `nullString`, `nullKnownInt64`, `nullOptionalInt64`,
`sqlTxRunner`). Deciding *what* is visible remains planning policy through the
compiled `contentFilter`; writing the rows is store work.

`shortcut_alias_mutation.go` is no longer claimed here. It declares `*Engine`
methods that perform Graph mutations and persist the resulting shortcut-root
state, which is engine orchestration, not store truth. It was the only reason
this family appeared to depend on `Engine` at all. Shortcut status display metadata is sync-owned but derived from
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
If child artifacts are purged but the live-parent acknowledgement fails, the
parent cleanup-pending row remains durable retry work. The multisync runtime may
surface a transient cleanup diagnostic for the current owner, but the next
source of truth is still the parent `shortcut_roots` row and any remaining child
artifacts, not a multisync cache.

### Receiver Naming

`SyncStore` methods use `s`. They used `m` -- residue from a previous name --
across all eighty methods, which is the "ghost architecture" the repo's own
guardrails ban: a reader looking for the store's methods does not grep for `m`,
and every other significant type in the tree already matched its own name
(`c *Client`, `s *Session`, `o *Orchestrator`, `tm *TransferManager`).

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
| Baseline, local observation truth, remote mirror truth, observation cursor/confidence, observation issues, retry work, and block scopes survive close/reopen as one restart authority. | `TestSyncStore_ReopenPreservesDurableRecoveryTruth` |
| Dry-run current-plan scratch stores are temporary SQLite stores seeded from committed truth and removed after preview planning. | `TestCreateScratchPlanningStore_SeedsCommittedStateAndCleansUp` |
| Read-only status and derived-truth queries continue to depend on store-owned raw-authority helpers rather than ad hoc writable opens; shortcut status leaves the store through sync-owned status views instead of raw shortcut-root rows. | `TestReadDriveStatusSnapshot`, `TestReadPathTruthStatus_DerivesUnavailableTruthFromDurableAuthorities`, `TestQuerySyncState_UsesReadOnlyStatusSnapshotHelper`, `TestStatusCommand_UnreadableStateStoreFallsBackToEmptySyncState`, `TestBuildChildStatusMount_RendersLifecycleState` |
| The baseline cache stays the one object callers hold across full rebuilds, and planning uses the baseline of its own comparison snapshot. | `TestCommitMutation_FolderMoveKeepsHeldBaselinePointerCurrent`, `TestPlanning_UsesBaselineFromTheComparisonSnapshot` |
| A folder re-created online (new item ID, same path) rebinds the baseline and its children's parent, and uploads land in the live folder; a full rescan where every item has a new ID plans no transfers (R-2.10.49). | `TestQueryReconciliationState_FolderWithNewItemIDAtSamePathIsBaselineUpdate`, `TestCommitMutation_FolderReboundAtSamePathRebindsItsChildren`, `TestCommitMutation_FolderRebindUpdatesCachedChildren`, `TestRunOnce_FolderRecreatedOnlineRebindsAndUploadsIntoTheNewItem`, `TestRunOnce_FullRescanWithEveryFolderReboundPlansNoTransfers` |
| One observation window converges to the same remote_state whatever its order: swaps, renames onto a freed name, create-before-move-away, delete and re-create across pages, a folder moved out of a deleted folder, and a full rescan's late orphan delete raise no remote name twin (R-2.12.7). | `TestCommitObservation_PathSwapInOneWindow`, `TestCommitObservation_CreateAtPathBeforeOccupantRenamedAway`, `TestCommitObservation_DeleteAndRecreateSamePathAcrossPages`, `TestCommitObservation_FolderMovedOutOfADeletedFolderKeepsItsSubtree`, `TestCommitObservation_RepeatedItemInOneWindowUsesItsLastObservation`, `TestCommitObservation_SubtreeRepathOntoAHeldPathKeepsBothApart`, `TestCommitObservation_WindowResultIsOrderIndependent`, `TestCommitObservation_UnplacedChildOfAParkedFolderFollowsTheFolder`, `TestRequireNoParkedRemoteRows_FailsTheWindow`, `TestRunOnce_FullRescanWithItemReplacedAtSamePath` |
| Engine startup refuses a state DB that fails `PRAGMA quick_check` with the `corrupt` reason and reset guidance, instead of planning from a damaged `remote_state`; a check that fails for any other reason is an ordinary failure (R-2.5.8). | `TestNewEngine_RequiresResetForCorruptStateDB`, `TestIntegrityCheckFailure_OnlyDamageIsCorrupt`, `TestCheckStoreIntegrity_CanceledCheckIsNotDamage` |
| A successful upload, download, folder create or move without a remote item ID is refused, because `item_id` is the baseline's primary key (R-2.5.7). | `TestCommitMutation_EmptyItemIDFails` |
| A folder move re-paths its subtree's baseline rows in the move transaction and in the cache. | `TestCommitMutation_FolderMoveRepathsSubtree`, `TestRunOnce_LocalFolderMoveSurvivesFolderETagChurn` |
| Local filesystem identity is persisted as generic truth for files and directories in `local_state` and `baseline`, allowing local move detection without shortcut-specific sibling scans; a full scan backfills identity onto baseline rows that predate it when the item is provably unchanged. | `TestReplaceLocalState_PersistsFilesystemIdentity`, `TestReplaceLocalState_BackfillsBaselineIdentityForUnchangedItems`, `TestRunOnce_LegacyFolderBaselineGainsIdentityAndMovesAsOne`, `TestCommitMutation_PersistsLocalFilesystemIdentity`, `TestQueryReconciliationState_LocalFolderMoveUsesFilesystemIdentity` |
| A full local snapshot is applied as a diff: only changed, added and removed `local_state` rows are written, the call reports whether anything changed, and an already-complete local-truth marker is not rewritten, so an idle watch safety scan writes no rows; restoring complete truth after a suspect marker counts as a change. | `TestReplaceLocalState_UnchangedSnapshotWritesNoRows`, `TestReplaceLocalState_WritesOnlyTheDifference`, `TestReplaceLocalState_RestoringCompleteTruthReportsChange` |
| Local watch observation can update durable local truth with scoped upsert/delete/prefix-delete patches, full snapshot replacement clears suspect local truth, and prefix deletion treats SQL wildcards as literal path bytes while preserving path case. | `TestScopedLocalStateMutation_UpsertReadAndDeleteExactPath`, `TestDeleteLocalStatePrefix_DeletesDirectoryAndDescendantsOnly`, `TestDeleteLocalStatePrefix_EscapesSQLWildcards`, `TestReplaceLocalState_MarksLocalTruthComplete` |
| Parent shortcut-root lifecycle state is stored in the parent sync store, rebuilt into parent-owned observation protection, and applied before the parent publishes child work and cleanup work to multisync. Empty complete remote shortcut observation batches are persisted and retire old roots; child final-drain acknowledgement first persists `removed_release_pending`; release cleanup later moves old roots to `removed_child_cleanup_pending` or promotes waiting replacements; cleanup-blocked release failures are durable state, never pass errors: the transition into `removed_cleanup_blocked` (or a change of its reason) is persisted and reported once as a plain warning, an unchanged blocked cleanup is retried every pass at Debug, and neither returns an error (that would fail every one-shot run and end the watch remote observer); child artifact cleanup acknowledgement deletes cleanup-pending rows; duplicate automatic shortcut targets are parent-owned blocked roots. Cleanup requests are derived from these rows with explicit child mount ID and local-root scope, not reconstructed by multisync. | `TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots`, `TestSyncStore_EmptyCompleteShortcutTopologyMarksRemovedFinalDrain`, `TestSyncStore_markShortcutChildFinalDrainReleasePendingIsDurable`, `TestSyncStore_acknowledgeShortcutChildArtifactsPurgedRemovesCleanupPendingRoot`, `TestSyncStore_SamePathReplacementWaitsBehindRetiringRoot`, `TestSyncStore_DuplicateAutomaticShortcutTargetIsParentBlocked`, `TestEngine_ReconcileShortcutRootLocalStateRetriesRemovedReleasePending`, `TestEngine_ReconcileShortcutRootLocalStatePersistsCleanupBlockedWithoutError`, `TestEngine_ReconcileShortcutRootLocalStateRepeatedCleanupBlockedIsQuiet`, `TestEngine_ReconcileShortcutRootLocalStatePromotesWaitingReplacementAfterReleasePending`, `TestNewMountEngine_LoadsPersistedShortcutProtectedRoots`, `TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler` |
| Shortcut alias mutation is a parent-engine-internal operation by binding item ID and updates parent shortcut-root state. | `TestEngine_ShortcutAliasRenameMutatesThroughParentAndUpdatesRootState`, `TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain` |
| Debug invariant checks reject malformed retry/block durable state before runtime handoff. | `TestEngineFlow_AssertPersistedInvariants_RejectsRetryWorkWithoutPath`, `TestEngineFlow_AssertPersistedInvariants_RejectsRetryWorkWithoutTiming`, `TestEngineFlow_AssertPersistedInvariants_RejectsRetryWorkWithoutAttempts`, `TestEngineFlow_AssertPersistedInvariants_AllowsDelayedRetryWorkWithTiming`, `TestEngineFlow_AssertPersistedInvariants_AllowsBlockedRetryWorkBackedBlockScope` |

## Write Responsibilities

### Retry and block state invariants

`retry_work` rows are exact retry obligations. Every row must carry a non-empty
path, a valid `actionType`, and a positive attempt count. Delayed retry rows
must have `next_retry_at`; blocked rows must not have retry timing and must
reference an existing `block_scopes` row. Every persisted block scope must in
turn have at least one blocked retry row. These invariants are asserted by the
engine debug invariant pass so malformed durable state is caught at the store
ownership boundary instead of becoming silent runtime debt.

### Observation writes

`CommitObservation()` is the remote-observation boundary. It atomically:

- upserts or deletes `remote_state` rows derived from observation
- advances `observation_state.cursor`

The store persists the remote observation it is given. It does not own product
content filtering and does not silently drop configured-junk or ignored-path
rows. Sync visibility is applied by local observation for `local_state`, by
planner-visible temp views for `remote_state`, and by remote watch wake
optimization after raw observation progress has been committed.

Each `remote_state` row persists the true owning remote `drive_id` seen during
observation. `observation_state.content_drive_id` identifies the remote drive
for the mounted content root that owns the DB and cursor; it is not the
durable owner of every remote row. If a later observation corrects a row's
owning `drive_id` without changing path/hash/mtime metadata,
`CommitObservation()` still updates the stored row owner so downstream planning
and execution read the repaired durable truth.

Local observation writes belong to `local_state`. Full scans replace the entire
`local_state` snapshot in one transaction, including filesystem identity for
files and directories when the platform provides a device/inode pair. The same
transaction backfills that identity onto baseline rows that lack one (rows
written before baselines recorded identity): a folder needs the same path and
type, a file must also still match the baseline's hash, size and mtime. Without
it a rename of such an item could only propagate as a delete plus a re-upload
(R-2.17.1). The in-memory baseline cache is reloaded when any row changed.
Watch-mode
scoped local observations use store-owned exact helpers instead of an event
journal: exact path reads, upsert rows, exact path deletes, prefix deletes, and
atomic mixed patches for one engine-applied batch. Prefix deletes match the
directory path itself plus descendants below `path/`; SQL wildcard characters in
path bytes are escaped and never broaden the delete, and SQLite prefix matching
uses TEXT character length so non-ASCII directory names delete the same rows the
sync path model names.

`observation_state.local_truth_complete` records whether `local_state` is a
complete local mirror. Full local snapshot replacement marks it complete and
clears `local_truth_recovery_reason`. Dropped local observation batches, dropped
hash requests, watcher errors, and failed safety scans mark it incomplete with a
reason so later stale-work checks know that local absence or presence is not
proof until a full refresh succeeds. Worker-start and admission freshness checks
therefore read this store-owned confidence bit before using `local_state` to
supersede old actions.

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
- exact-path batches manage only the observed path set they proved. The store
  honors this mode and is tested for it directly, but no current caller sets
  `ManagedPaths`: its only consumer was single-path observation, removed when
  the linear runtime replaced per-path retry reconstruction with replanning
  from committed truth. The mode is kept as a store primitive rather than
  removed, because dropping a durable reconciliation semantic is a decision
  about persisted state, not cleanup of an unused helper

### Mutation writes

`CommitMutation()` is the successful-execution boundary. It updates `baseline`
and, when needed, keeps `remote_state` aligned with the remote truth implied by
the successful action.

Baseline writes also publish the local filesystem identity observed for the
successful mutation, for directories as well as files: remote folder creates
record the observed local folder's identity, and local folder creates and
downloads stat the entry they just materialized (a failed lookup records "no
identity", never a stale one). Local move detection compares that last-synced
identity to the next `local_state` snapshot; when identity is unavailable, file
moves may fall back to unique content-hash candidates, but directory moves do
not. Move outcomes publish only observed or proven local facts (R-2.17.3).
A folder move commits its whole subtree: `commitMove` re-paths every baseline
row below the old folder path in the same transaction (and, for a
local-origin move that just moved the remote subtree, the `remote_state` rows
too), then rebuilds the baseline cache. Graph and `rename(2)` both move the
subtree, and the planner omits the descendant moves they cover, so descendant
rows left at old paths replanned as moves whose source had vanished and were
superseded on every pass.

The baseline cache is one object for the store's lifetime: watch holds the
pointer `Load` returned at startup, so a full rebuild (`reloadBaselineCache`,
used after a folder move or an impossible cache state) refills that object in
place. Swapping in a new object once orphaned watch's pointer, so later commits
never reached it and planning failed with "missing baseline entry for
comparison path", restarting the drive runner. Planning additionally reads its
baseline in the same read transaction as the comparison rows
(`currentInputs.baseline`), so a commit landing between the cache and the
snapshot cannot make the two disagree.

That same store boundary also owns publication-only planner actions:

- `ActionBaselineUpdate` publishes an upsert for converged current truth
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
  issue-type set or as an exact-path reconciliation (see the note above on
  `ManagedPaths` having no current caller)
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
`engine_runtime_start.go`. Dry-run scratch preparation uses a temporary
`SyncStore` seeded from committed truth and removes its temporary directory
after planning. Dry-run engine startup uses the normal durable sync-store open
path, so schema creation, observation-state materialization, and close-time WAL
checkpointing remain normal operational side effects. Generic checkpointing is
WAL-only; semantic cleanup for observation issues, retry work, or block scopes
belongs to the reconciliation helpers that own those rows.

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
`readPathTruthStatus`; timed `block_scopes` for write blockers do not change
truth availability on their own.

### Admin writes

Administrative write helpers are split by authority:

- store compatibility helpers diagnose incompatible DBs
- `store_reset.go` owns explicit delete-and-recreate reset

### Pending creates

A new file is created as an empty item first and filled by ID second; until
the content commits, that empty item is a real OneDrive file (and a
path-routed upload session makes one too, L-34). Seen by the next pass without
memory of who made it, it was a create_conflict: the user's file was renamed
to a conflict copy and the empty file downloaded. `pending_creates`
(generation 18, `store_pending_creates.go`) is that memory: the executor
records item ID, path and creation eTag before the first content byte is
sent. The observation commit then classifies an observed item with a record:
still empty with its creation eTag and not synced, it is adopted as a baseline
row whose local side is the empty content, so the planner uploads by ID with
If-Match, or deletes the placeholder with If-Match when the local file is
gone; still empty with its creation eTag after the upload committed (a delta
page captured mid-upload), it is stale and not recorded as a remote change;
anything else drops the record (R-5.6.15,
`TestCommitObservation_OwnPlaceholderIsAdoptedAsEmptySyncedContent`,
`TestCommitObservation_PlaceholderStateSeenAfterTheCommitIsNotARemoteChange`,
`TestRunOnce_InterruptedCreateFinishesIntoItsOwnPlaceholder`). Adopted rows
are put into the in-memory baseline after the commit.

### Remote twins

`remote_state.path` is unique, and stored paths are NFC, but OneDrive keeps
some canonically equivalent siblings as distinct items (live probe L-33:
U+FB2A next to U+05E9 U+05C1, U+1F71 next to U+03AC). The second such item
used to fail the observation commit with "UNIQUE constraint failed:
remote_state.path", and every retry of the drive with it. Now the first item
to hold a path keeps it and every other twin is stored at a shadow path below
it, `P/:twin:<itemID>` (':' cannot occur in a stored name), with its subtree
placed under the shadow through the observed parent ID
(`store_remote_twins.go`). Each commit recomputes a `remote_name_twin`
observation issue for every such P; the issue blocks remote truth for P and
its subtree, so neither twin syncs and a baselined holder keeps its baseline
(no local delete is derived). When the holder is deleted or moves away, the
lowest-ID twin is promoted to P with its subtree; a twin renamed to a distinct
name simply moves there (R-2.12.6,
`TestCommitObservation_RemoteTwinsInOneBatchDoNotFailTheCommit`,
`TestCommitObservation_RemoteTwinRecoversWhenOneIsDeletedOrRenamed`,
`TestRunOnce_RemoteTwinsAreHeldAndTheDriveKeepsSyncing`). Insert and update
failures name the item ID and path.

### One window, one set of facts

Twins must be real: two items that still claim one stored path once the
whole window is applied. Applied item by item, a window made ordinary moves
look like twins whenever the newcomer arrived before its path's occupant left
(a swap, save-as-new-then-rename, delete and re-create across pages, or a full
rescan's orphan delete synthesized after the replacement), and every such
window warned the user about "two items". A tombstone applied before a move
out of the deleted folder also took the moved folder's unreported children
with the folder's subtree delete, and those files then read as deleted on
OneDrive. `applyObservationWindowTx` (R-2.12.7) therefore:

1. keeps only the last observation of each item (each is a full snapshot);
2. parks every live item whose row holds a real path other than the one it
   is reported at at `:park:<itemID>`, subtree included (shadowed twins hold
   no real path and are not parked);
3. applies tombstones and orphan deletes, then live items in window order;
4. returns any parked row the window did not place to its old path,
   re-rooted under the current path of its nearest parked ancestor (old paths
   are read before anything is parked, so a child carried along by its
   parked folder records its real path), offers every path parking freed to
   a twin shadowed at it, and fails the transaction if any `:park:` path is
   left (`requireNoParkedRemoteRowsTx`).

A newcomer whose path is still held after step 3's predecessors is a real
twin and takes the twin path above. The spec's alternative, deleting a row
outside the window that holds a newcomer's path as stale, was not taken: a
genuine canonically equivalent twin is exactly such a row, and deleting it
would read as a remote delete and plan a local one.

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
scope semantics are reconstructed from `describeScopeKey` during read/write
validation. The shared raw block-scope read path therefore validates the
durable key once and returns a canonical `BlockScope` shape without storing a
second copy of parsed metadata in SQLite.

`ScopeKey.PersistsInBlockScopes()` is the single sync-domain rule for whether a
scope belongs in `block_scopes`. Timed blocked-work scopes persist there;
observation-owned read boundaries do not.

`observation_state` persists the restart-safe next full-remote refresh deadline
and local-truth confidence, not the runtime mode that produced the remote
deadline. Runtime decides whether the current observation path is delta-based or
enumerate-only and then stores the next due time accordingly. When watch mode
shortens that persisted deadline after startup, the same watch step must rearm
the live timer immediately so the process does not keep sleeping on the
superseded later deadline.

`retry_work` stores only exact held roots. Dependents blocked behind those
roots remain dependency state in the current runtime; they are not persisted as
cascade retry rows.

That exact-root rule includes publication-only actions such as
`ActionBaselineUpdate` and `ActionCleanup`. They may persist in `retry_work`, but
their retry still re-enters the engine-owned publication-drain stage rather
than worker dispatch.

Read-denied subtree boundaries do not persist as `block_scopes`. They remain
tagged on `observation_issues` via `ScopeKey`, and truth-status reads derive
blocked descendants from those boundary facts.

`store_inspect.go` now owns two read-side shapes:

- `DriveStatusSnapshot` for raw durable authorities used by `status` and watch
- `readPathTruthStatus` for derived truth availability over requested paths,
  built from `observation_issues` plus boundary-tagged `ScopeKey` facts without
  materializing fake durable descendant rows

`DriveStatusSnapshot.RemoteDriftItems` (status "Remote changes") uses the same
content test as the planner's `remote_changed` in `sqlite_compare.go`: folders
differ only by path or type, and a file's hash decides whenever either side has
one. Baseline stores no folder mtime, so comparing folder times counted every
remote folder as drift on a converged drive (R-2.10.48).

A folder's item ID is the one folder fact that is not content but must still
reach the baseline: `metadata_drift` is 1 for a folder present on all three
sides whose remote item ID differs from the baseline's, so an `unchanged`
folder becomes a publication-only `baseline_update` that rebinds it, and
`commitUpsert` moves the children's `parent_id` from the dead item to the new
one (R-2.10.49); the baseline cache is rebuilt after such a rebind so cached
children carry the new parent too. Without it, uploads into a folder deleted and re-created
online targeted the dead ID forever. A folder missing locally whose remote ID
changed is still classified by `remote_changed`, which ignores folder IDs;
that case belongs to the comparison-facts work, not this rule.

## State-DB Diagnosis And Reset

Two store-owned helpers isolate DB lifecycle policy:

- `store_compatibility.go` opens an existing DB non-mutating, classifies
  unreadable or unsupported stores, and returns typed incompatible-store errors.
  After a successful open it runs `PRAGMA quick_check`; a finding other than
  `ok`, or `SQLITE_CORRUPT` while the schema is read, is the `corrupt` reason
  with the same reset guidance (R-2.5.8). A damaged `remote_state` b-tree can
  hide rows, and a hidden row reads as a remote delete, so startup refuses the
  DB instead of planning from it. `quick_check` skips `integrity_check`'s
  index cross-check, which keeps it linear and cheap enough for every open.
  Only damage is `corrupt`: a check that fails for another reason (a
  canceled context, `SQLITE_BUSY`, an I/O or scan error) is an ordinary
  startup failure, never a reason to reset (`integrityCheckFailure`)
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

Those concerns belong to the engine or CLI.
