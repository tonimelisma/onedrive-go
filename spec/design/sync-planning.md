# Sync Planning

GOVERNS: internal/sync/planner.go, internal/sync/planner_actions.go, internal/sync/planner_delete_safety.go, internal/sync/planner_truth_overlay.go

LAYER: 2

Implements: R-2.1.3 [verified], R-6.2.11 [verified], R-2.1.4 [verified], R-2.2 [verified], R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.1 [verified], R-6.2.5 [verified], R-6.4.2 [verified]

## Overview

Planning is split between SQLite reconciliation and a Go action builder.

- SQLite computes deterministic comparison and reconciliation rows from raw
  `baseline` plus planner-visible `local_state` and `remote_state` temp tables.
  Those temp tables are filter-scoped and then pruned for same-side descendants
  under a baseline folder that is missing on that side.
- Go turns reconciliation rows into the current executable action set, then
  normalizes action-level execution semantics, partitions work by sync mode,
  and builds dependencies.

The executable plan is runtime-owned and is not a durable SQLite authority.

### The Vocabulary Moved Out

`Action`, `ItemType`, `Baseline`, the sentinel errors, and the rest of the sync
vocabulary used to live in this family. Because every other family needs those
names, every other family appeared to depend on planning: 28 symbols from the
store, 32 from observation, 24 from execution.

That inverted the real relationship. Planning is a deterministic transform
sitting *above* the store, not a foundation beneath everything. The vocabulary
now lives in [sync-domain.md](sync-domain.md), and planning's inbound coupling
is a single symbol.

### File Names Follow Ownership, Not History

`planner_actions.go` was called `planner_sqlite.go` and contains no SQL at all:
its functions build, normalize, and partition the action set from
reconciliation rows already loaded into Go. The SQL half of planning lives in
`sqlite_compare.go` and in the store-owned planner-visible projection.

That mattered beyond tidiness. `spec/design/system.md` measures cross-family
coupling to decide whether `internal/sync` can be split, and it slices by
filename prefix. A planning file named for SQL is scored as store coupling, and
a store file named for planning is scored as planning coupling, so the measured
tangle was partly an artifact of the names. Filing this file by its name rather
than its content produced a phantom store-to-planning edge of 16 symbols; the
real number is 1.
Shortcut-root lifecycle planning follows the same functional-core rule inside
`internal/sync`: remote shortcut topology observations and local alias identity
facts enter deterministic shortcut-root planner helpers, while Graph,
filesystem, SQLite, logging, clocks, and goroutines stay in the engine shell.
The output is parent-owned shortcut-root state plus child work snapshot
intent; multisync receives only that child work intent.

## Ownership Contract

- Owns: preparing planner-visible current truth for SQLite reconciliation and
  turning reconciliation rows into the current executable action set, including
  conflict expansion, action normalization, filtering, deferral, and dependency
  ordering
- Does Not Own: observation, execution, retry timing, scope persistence, or remote/local I/O
- Source of Truth: raw `baseline`, raw current-state tables, and the engine's
  compiled `contentFilter`. Planning sees filtered current-state views through
  SQLite reconciliation rows, plus engine-owned sync mode and runtime policy.
- Allowed Side Effects: none
- Mutable Runtime Owner: None. Planning is deterministic value transformation over one input set and owns no long-lived mutable runtime state.
- Error Boundary: planner errors stop at invalid or unsupported planning states. Transport, filesystem, store, and execution failures stay outside the planner boundary.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Local/remote equality is never inferred from size alone: a missing hash on either side forces reconciliation instead of a baseline update recording unproven agreement. | `TestPlanCurrentState_HashlessSameSizeIsNotTreatedAsEqual`, `TestPlanCurrentState_HashOnOneSideOnlyIsNotTreatedAsEqual`, `TestPlanCurrentState_MatchingHashesStillCompareEqual` |
| Conflict reconciliation rows expand into concrete actions for edit/edit, create/create, and edit/delete cases. | `TestPlannerPlanCurrentState_ExpandsEditEditConflictIntoConcreteActions`, `TestPlannerPlanCurrentState_ExpandsCreateCreateConflictIntoConcreteActions`, `TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID` |
| Folder-delete descendants are reconciled by SQLite after planner-visible pruning, with parent availability preserved only when descendant work requires it. | `TestReplacePlannerVisibleStateTx_PrunesRemoteDescendantsWhenBaselineFolderMissingRemotely`, `TestReplacePlannerVisibleStateTx_PrunesLocalDescendantsWhenBaselineFolderMissingLocally`, `TestPlannerPlanCurrentState_RemoteParentDeletePlansDescendantLocalDeleteThroughSQLite`, `TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild`, `TestPlannerPlanCurrentState_LocalParentDeleteCreatesParentForChangedRemoteChild`, `TestPlannerPlanCurrentState_BothParentSidesDeletedCleansUpDescendantsThroughSQLite` |
| A file move needs content corroboration beyond inode identity; identity without it is delete + create, and unhashed moves carry the destination's observed truth. | `TestQueryReconciliationState_LocalFileMoveRequiresContentCorroboration`, `TestQueryReconciliationState_LocalFileMoveIdentityWithoutContentIsNotAMove`, `TestPlannerPlanCurrentState_IdentityMatchedEditedFileIsDeleteAndCreate`, `TestPlannerPlanCurrentState_UnhashedMoveCarriesDestinationTruthAndReuploads`, `TestActionFreshness_LocalOriginMoveRejectsChangedDestination` |
| Locally deleted or unpaired moved folders are deleted on OneDrive after their descendants, never re-created locally; identity-paired folder moves are one remote move. | `TestQueryReconciliationState_FolderDecisionMatrix`, `TestPlannerPlanCurrentState_LocalFolderDeletePropagatesAfterChildDeletes`, `TestPlannerPlanCurrentState_UnpairedLocalFolderMoveDeletesSourceAfterChildMoves`, `TestRunOnce_LocalFolderDeleteDeletesRemoteFolder`, `TestRunOnce_LocalFolderMoveBecomesOneRemoteMoveWithoutResurrectingSource` |
| Remote moves applied locally carry the source baseline and do not re-upload unchanged content. | `TestPlannerPlanCurrentState_RemoteMoveCarriesSourceBaseline`, `TestRunOnce_RemoteRenameAppliedLocallyDoesNotReupload` |
| Type changes on either side are replacements ordered delete-then-create, never a silent stall. | `TestPlannerPlanCurrentState_LocalFileReplacedByFolderReplacesRemoteItem`, `TestPlannerPlanCurrentState_LocalFolderReplacedByFileReplacesRemoteItem`, `TestPlannerPlanCurrentState_RemoteFileReplacedByFolderReplacesLocalItem`, `TestPlannerPlanCurrentState_RemoteFolderReplacedByFileReplacesLocalItem`, `TestRunOnce_LocalFileReplacedByFolderConverges` |
| Deletes toward one side are held for confirmation when the other side is empty while the baseline holds files, or when they exceed max(50, 30% of baseline files); moves never count, non-delete work continues, and `sync --confirm-deletes` releases them. | `TestPlanner_MassRemoteDeleteIsHeldForConfirmation`, `TestPlanner_MassLocalDeleteAfterAnomalousFullRefreshIsHeld`, `TestPlanner_EmptiedLocalSideHoldsEvenFewDeletes`, `TestPlanner_DeleteGuardBelowThresholdProceeds`, `TestPlanner_DeleteGuardIgnoresMoves`, `TestDeleteSafetyHold_FolderDeletesCountTowardThreshold`, `TestDeleteSafetyHold_LostAndFoundDoesNotMakeASideNonEmpty`, `TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo`, `TestDeleteSafetyHold_ConfirmReleasesExactlyTheHeldSet`, `TestDeleteSafetyHold_HeldRowWithoutItemIDDoesNotApproveAKnownItem`, `TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked` |
| Mode-specific deferral and dependency ordering stay planner-owned rather than executor- or CLI-owned. | `TestSyncModeFromFlags`, `internal/sync/planner_actions_test.go`, `internal/sync/planner_dependency_test.go` |
| Planner decisions stay row-driven and action-shaped across conflict and folder-parent preservation cases. | `TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID`, `TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild`, `TestPlannerPlanCurrentState_DownloadOnlyKeepsParentDeleteWhenEditedChildUploadDeferred`, `internal/sync/store_planner_visible_test.go` |

## Inputs

- `baseline`: last known synced common ancestor
- `local_state`: latest admissible local snapshot, filtered again when planning
  so stale rows from a changed filter cannot leak into comparison
- `remote_state`: latest raw manageable remote snapshot, filtered when planning
  so disabling a filter can re-present already-observed remote truth
- `observation_issues`: durable blocked-truth facts for unreadable or
  unsyncable current state
- `Mode`: bidirectional, download-only, or upload-only
- runtime safety/policy inputs owned by the engine

The engine first materializes transaction-local planner-visible current-state
tables from `local_state` and `remote_state` using the compiled `contentFilter`.
It then removes same-side descendants below baseline folders that are absent
from that side's visible current table. Raw `baseline` is not filtered or
mutated. Those views feed `comparison_state` and `reconciliation_state`,
including the invariant that a baseline row absent from both planner-visible
current snapshots becomes `baseline_remove`.

Before Go emits actions, a shared derivation step computes one per-path
truth-status value from `observation_issues`, including any read-boundary
`ScopeKey` tags. Planner then applies its own suppression policy so unreadable
or unobservable paths stay unavailable instead of being misread as deletions.

`truthAvailabilityIndex` is the one raw derived read model for that question.
Planner uses it directly, and read-only inspection of specific paths uses the
same derivation rather than a second planner-shaped helper.

## Pipeline

1. Build filtered transaction-local current-state views from `local_state` and
   `remote_state`; keep `baseline` raw.
2. Prune planner-visible same-side descendants under any baseline folder that is
   missing from that side's visible current-state table. Descendant matching uses
   exact path-prefix checks, not pattern matching.
3. Run SQL structural diff and reconciliation over planner-visible current views
   plus baseline.
4. Load reconciliation rows into Go.
5. Derive per-path local/remote truth status from filtered `observation_issues`
   through `truthAvailabilityIndex`.
6. Apply planner-owned suppression and sync-mode safety rules on top of that
   derived status.
7. Emit the current runtime action set from reconciliation rows, including
   conflict expansion into concrete actions.
8. Normalize the action set: omit descendant remote-move actions already covered
   by a folder move sent to Graph, and preserve a deleted parent folder only
   when runnable descendant actions in the current sync mode need that parent to
   exist.
9. Partition the normalized action set into admitted work and mode-deferred
   counts.
10. Bind ordinary actions to the engine's mounted drive/root context, build
    dependency edges, and reject dependency cycles.

## File Decisions

### Baseline exists

- local deleted + remote unchanged -> `ActionRemoteDelete` (files and folders
  alike, see below)
- local deleted + remote changed -> `ActionDownload` (remote wins canonical path)
- local deleted + remote deleted -> `ActionCleanup` (publication-only)
- local changed + remote unchanged -> `ActionUpload`
- local unchanged + remote changed -> `ActionDownload`
- local changed + remote changed with equal hashes -> `ActionBaselineUpdate` (publication-only)
- local changed + remote changed with different hashes -> `ActionConflictCopy` + dependent `ActionDownload`
- local unchanged + remote deleted -> `ActionLocalDelete`
- local changed + remote deleted -> `ActionUpload` with no stale item ID, so execution recreates the remote file
- content unchanged on both sides but file metadata moved (a new local mtime
  from `touch`, a new remote mtime or eTag over the same hash) ->
  `ActionBaselineUpdate` (publication-only, R-2.2.3). Nothing transfers, but the
  scanner's size+mtime fast path compares against the baseline, so a stale
  mtime would re-hash the file on every scan

### No baseline

- local only -> `ActionUpload`
- remote only -> `ActionDownload`
- local and remote with equal hashes -> `ActionBaselineUpdate` (publication-only)
- local and remote with different hashes -> `ActionConflictCopy` + dependent `ActionDownload`

Local absence alone never plans `ActionRemoteDelete`: without a baseline row
proving the item was synced, a remote-only item is downloaded (R-6.2.1,
`TestPlannerPlanCurrentState_LocalAbsenceDeletesRemoteOnlyWithBaseline`).

## Publication-Only Planner Actions

`ActionBaselineUpdate` and `ActionCleanup` remain real planner action types.
They stay in the action set so dependency ordering, counts, and reporting can
see them.

They are publication-only outcomes, not executor side effects:

- the planner still emits them from reconciliation rows
- the dependency graph still orders them against dependent actions
- the engine commits their baseline mutation directly through the store
- the executor does not own handlers for them

## Blocked Truth Overlay

Observation-owned unreadable or unsyncable truth must not be interpreted as
structural absence:

- local read-denied, invalid, or otherwise unsyncable paths are unavailable
  local truth, not local deletes
- a local unreadable subtree boundary suppresses all descendant structural
  actions; descendants are unavailable current truth, not "missing local"
- remote read-denied subtrees are unavailable remote truth, not remote deletes
- a remote unreadable subtree boundary suppresses all descendant structural
  actions; descendants are unavailable current truth, not "missing remote"
- subtree unavailability also suppresses publication-only cleanup for those
  descendants; baseline rows under an unreadable subtree must not be cleaned
  up just because current observation could not safely prove their absence
- tagged read-boundary issues suppress destructive and mutating work under the
  blocked subtree until later observation restores trustworthy truth

The planner therefore keeps structural reconciliation raw, then attaches an
explicit local/remote truth-status value to each path view before action
emission. `truthAvailabilityIndex` is the reusable derived view over durable
authorities; planner suppression is the separate policy layer that blocks
action emission when observation issues prove the path is currently
unavailable.

That same truth-status derivation may also power debug/read-only inspection for
specific paths. Those debug reads must stay derived-only: descendants under a
read-denied subtree are reported as unavailable by scope derivation, not by
writing additional descendant `observation_issues` rows.

## Conflict Planning

SQLite emits conflict reconciliation rows, and the action builder expands those
rows directly into concrete runtime actions as part of the current action set.
It does not create or preserve separate conflict metadata.

Execution applies the already-expanded conflict policy immediately:

- edit/edit and create/create -> preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path; the dependent
  download explicitly requires the canonical local target to be missing after
  the conflict copy
- edit/delete -> keep local content in place and upload it as a new remote item

If executor-time precondition revalidation later proves a planned local delete
went stale, execution reports that stale precondition and returns control to
the engine; the next plan rebuild is still the only place that may emit the
concrete edit/delete upload.

## Folder And Move Semantics

Folder decisions are symmetric with file decisions. When Graph delta reports a
deleted parent folder without descendant delete rows, planner-visible pruning
removes the stale same-side descendant rows before SQLite reconciliation. SQLite
then emits ordinary descendant reconciliation rows: unchanged descendants under
the deleted parent delete locally, edited descendants recreate remote content,
and descendants missing on both sides clean up baseline.

Action normalization rewrites only the deleted parent folder action when
descendant work that can run in the current sync mode needs the folder to exist:
local parent deletes become remote folder creates, remote parent deletes become
local folder creates, and cleanup stays cleanup. Deferred descendant work must
not preserve a parent that the current mode would otherwise delete.

A folder that is missing locally while its baseline and remote item remain is a
local delete and becomes `ActionRemoteDelete`, ordered after the deletes and
same-side moves of everything under it (R-2.17.2). Only action normalization may
turn it back into a local folder create, and only for runnable descendant work.
History: the event-driven planner's ED4 ("local deleted, remote present ->
recreate locally, remote wins") meant "remote reported a change"; when #513
moved planning to snapshots, remote presence became unconditional and ED4
swallowed ED8 ("propagate delete"), so every locally deleted or unpaired moved
folder was resurrected. Remote wins only when the remote side actually
changed, which the descendant rows and parent preservation already express.

Folder reconciliation is existence/type-based, not metadata-based. Folder
size, mtime, and ETag churn caused by child mutations must not be treated as
content drift or expanded into conflict/download actions.

Local or remote moves that stay within one Graph drive become move actions.
Local move detection first matches unique filesystem identity
(`local_device`, `local_inode`, `local_has_identity`) between `baseline` and
`local_state` for both files and directories. File-hash matching is a fallback
only for files without identity.

Identity is necessary but not sufficient for a file (R-2.17.1). After a delete
and a create in one batch, ext4 and other filesystems may give the new file the
freed inode, and pairing on inode alone once moved `vim.swp`'s OneDrive content
onto an unrelated unreadable `secret.txt`. A rename preserves size, mtime and
content; inode reuse preserves none of them. So an identity pair is a file move
only when sizes are equal and either both hashes are known and equal, or a hash
is unknown and the mtimes are equal. Everything else (different size, different
known hash, unknown hash with a different mtime) is delete + create. The cost is
that a rename followed by an edit inside one observation window also becomes
delete + create: the content is uploaded fresh and the old item goes to the
recycle bin, which is safe. Directories pair on identity alone: a directory has
no content of its own, its descendants pair independently, and a wrong pairing
after rmdir + mkdir reuse still converges to the same tree. The pairing rule
lives once, in `sqlLocalAndRemoteMoveCTEs`, shared by the comparison and
reconciliation queries.

A local folder move emits one `ActionRemoteMove` for the folder and makes
descendant work depend on that move instead of re-uploading or
deleting/recreating the subtree. A moved file whose hash is unknown (size and
mtime corroborated) emits the remote move before an upload at the moved path.
A local-origin move carries the destination's planned local truth as
`Action.MovedLocal`: freshness rejects a destination that changed since
planning, and the move outcome publishes exactly that truth. A remote move
applied locally carries the source's baseline in its view, so the precondition
validates the source and the outcome keeps its local facts (R-2.17.3).
Identity mismatch at the same path is treated as replacement or delete/create
truth, not as a move.

A type change (file <-> folder) at one path is a replacement (R-2.17.4):
`replace_remote` emits `ActionRemoteDelete` of the old item and then a remote
folder create or a new-item upload; `replace_local` emits `ActionLocalDelete`
and then a local folder create or a download that requires a missing target.
The create depends on the same-path delete. When both sides changed and either
changed type, local wins (`replace_remote`), because a replaced OneDrive item
survives in the recycle bin while replaced local truth would be gone. Delete
ordering and parent preservation use the deleted side's type
(`deletedItemType`), since the two sides disagree across a type change. Moves that cross drive ownership are
decomposed into delete + upload because Graph move cannot cross drive
ownership.

OneDrive's namespace is case-insensitive, so a case-only rename needs the same
care (R-2.12.1). When it cannot be paired as a move because the content
changed too, it plans `ActionRemoteDelete` of `edit.txt` and `ActionUpload` of
`EDIT.txt`. Uploads replace on name conflict, so an upload that ran first
would overwrite the old item in place and the delete would then remove the new
content. A remote upload, remote folder create or remote move therefore
depends on any remote delete of a sibling whose path differs from it only in
case, and an upload or folder create also on a remote move away from such a
path. Move-to-move edges are skipped: two moves that trade case-variant names
would otherwise wait on each other, and a blocked rename fails on its name
conflict and is retried.

Local folder moves execute as `ActionRemoteMove` work. Graph applies a folder
move to unchanged descendants, so descendant `ActionRemoteMove` entries whose
old/new suffixes are exactly covered by the folder move are omitted from the
executable action set. Descendant renames or content work that are not covered
by the folder move remain explicit actions.

## Mount-Local Planning

Some engines are rooted below the remote drive root. The planner therefore
receives engine-owned mount context:

- `DriveID`
- `RemoteRootItemID`

Ordinary sync actions are bound directly to that mounted subtree. When
`makeAction(...)` leaves `Action.DriveID` empty for brand-new local work, the
planner fills it from the mount context before runtime admission. Ordinary
actions do not carry separate target-drive or target-root override fields.

Cross-drive move decomposition still happens in planning. By the time work
reaches execution, ordinary actions are mount-local concrete work.

## Delete-Safety Hold

`planner_delete_safety.go` is a post-pass over the admitted plan
(`applyDeleteSafetyHold`, run by the engine's planning stage after
`PlanCurrentState`). Per-item checks prove each delete matches what was
synced; they cannot tell an intended mass delete from a wrong tree that
passed every identity check (an fscrypt folder with its key unloaded, a
restored folder without its contents, a complete but anomalous remote
enumeration). So, per direction (product decision P-1):

- **structural**: when the planner-visible snapshot of one side has no
  non-root entries while the baseline holds items, every delete toward the
  other side is held, however few; `lost+found` (an ext mount root keeps one
  even when the disk is otherwise gone) and the client's own artifacts do not
  count as entries;
- **proportional**: when more than `max(50, 30% of baseline items)` deletes,
  files and folders alike, go toward one side, all deletes toward that side
  are held.

Moves never count. A held delete is removed from the plan together with
every action that depends on it, directly or transitively (the create half of
a type-change replacement, an upload waiting on the delete of a case
variant); dependencies are then rebuilt. Everything else runs.

The hold is re-derived from current truth on every plan, so it stays while
the evidence is unchanged and lifts by itself when the user restores the
files. The engine records the held deletes in `held_deletes` for `status`
(condition `deletes_held`, scope `delete_safety:{local|remote}`) and in the
pass report. `sync --confirm-deletes` (one-shot only) releases exactly the
recorded held set, matched by path, side and item ID (a delete that knows its
item is approved only by a row naming the same item); the rules are then
applied to every other delete in that pass, so a batch that appeared after the
user looked is held again rather than waved through.
The thresholds are fixed policy, not configuration (R-6.4.3).

## Directional Suppression

`download-only` and `upload-only` do not stop observation. They suppress only
the forbidden action classes and record those counts in `DeferredByMode`.
Conflict-copy is suppressed together with its paired remote-winner download in
`upload-only`; the planner must not rename local truth into a fake delete/create
sequence when the corresponding download is deferred by mode.

Permission scopes are different: planner is blocked-truth-aware for active read
scopes and observation-owned unreadable paths so unavailable truth is never
treated as a delete. Final runtime admission still happens later in the engine
against the ready frontier.

That invariant is subtree-wide: if a read-denied boundary blocks truth for a
folder, descendants and move endpoints below that boundary stay unavailable and
must not produce delete, move, cleanup, or repair actions until observation
restores trustworthy truth.
