# Sync Planning

GOVERNS: internal/sync/planner.go, internal/sync/planner_actions.go, internal/sync/planner_move_composition.go, internal/sync/planner_delete_safety.go, internal/sync/planner_truth_overlay.go

LAYER: 2

Implements: R-2.1.3 [verified], R-2.1.7 [verified], R-6.2.11 [verified], R-2.1.4 [verified], R-2.2 [verified], R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.1 [verified], R-6.2.5 [verified], R-6.4.2 [verified]

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
| The comparison facts are defined once (one shared CTE) for the comparison and reconciliation queries. Filesystem identity never signals a content change: a folder or a same-hash file with a new identity is a baseline update, and a folder is never an upload or download (R-2.2.6). A missing remote hash is unknown, not changed, and two empty files are equal (R-2.2.7). A locally deleted folder that OneDrive replaced with a new item is re-created locally, not deleted (R-2.10.49). | `TestQueryReconciliationState_IdentityChangeAtSamePathIsNotAnEdit`, `TestRunOnce_RenumberedDeviceTransfersNothing`, `TestRunOnce_FolderDeletedAndRestoredLocallyTransfersNothing`, `TestBuildActionsForReconciliation_RefusesFolderTransfers`, `TestQueryReconciliationState_MissingRemoteHashIsUnknownNotChanged`, `TestPlanCurrentState_HashlessZeroByteBothSidesIsEqual`, `TestPlanCurrentState_HashlessNonEmptyCreateIsStillAConflict`, `TestQueryReconciliationState_LocallyDeletedFolderReplacedOnlineIsRecreatedLocally` |
| Conflict reconciliation rows expand into concrete actions for edit/edit, create/create, and edit/delete cases. | `TestPlannerPlanCurrentState_ExpandsEditEditConflictIntoConcreteActions`, `TestPlannerPlanCurrentState_ExpandsCreateCreateConflictIntoConcreteActions`, `TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID` |
| Folder-delete descendants are reconciled by SQLite after planner-visible pruning, with parent availability preserved only when descendant work requires it. A file deleted locally that OneDrive only carried along with a renamed ancestor is neither a new remote file nor gone from both sides: both paths wait (`followed_ancestor_deletes`) until the ancestor's local move re-paths the baseline, and the delete then reaches OneDrive. The mirror case waits the same way (`followed_local_ancestor_deletes`): an item OneDrive deleted whose local copy only followed a folder the user renamed is not uploaded again, and what OneDrive created at its place waits too. | `TestRunOnce_LocalDeleteInsideFolderRenamedOnlineIsNotUndone`, `TestRunOnce_RemoteDeleteInsideFolderRenamedLocallyIsNotUndone`, `TestRunOnce_RemoteReplaceInsideFolderRenamedLocallyConverges`, `TestReplacePlannerVisibleStateTx_PrunesRemoteDescendantsWhenBaselineFolderMissingRemotely`, `TestReplacePlannerVisibleStateTx_PrunesLocalDescendantsWhenBaselineFolderMissingLocally`, `TestPlannerPlanCurrentState_RemoteParentDeletePlansDescendantLocalDeleteThroughSQLite`, `TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild`, `TestPlannerPlanCurrentState_LocalParentDeleteCreatesParentForChangedRemoteChild`, `TestPlannerPlanCurrentState_BothParentSidesDeletedCleansUpDescendantsThroughSQLite` |
| Filesystem identity includes the entry's birth time where the filesystem records one: a deleted synced folder or file and an unrelated new entry holding its reused inode number (another birth time) are a delete plus a create, with the old item in the recycle bin, while a rename keeps its birth time and moves; a side without a birth time pairs on (device, inode); worker-start freshness rejects a move destination born again since planning (R-2.17.1). | `TestRunOnce_DeletedFolderAndNewFolderWithReusedInodeAreNotAMove`, `TestRunOnce_DeletedFileAndNewFileWithReusedInodeAreNotAMove`, `TestRunOnce_RenameWithMatchingBirthTimeIsStillAMove`, `TestRunOnce_WithoutBirthTimeIdentityIsDeviceAndInode`, `TestQueryReconciliationState_LocalMovePairingComparesBirthTimes`, `TestQueryReconciliationState_BirthTimeAtTheSamePath`, `TestActionFreshness_LocalOriginMoveRejectsADestinationBornAgain`, `TestSameIdentity_BirthTimeDecidesWhenBothSidesKnowIt`, `TestSQLSameLocalEntry_EveryAliasFollowsThePattern` |
| A file move needs content corroboration beyond inode identity; identity without it is delete + create, and unhashed moves carry the destination's observed truth. | `TestQueryReconciliationState_LocalFileMoveRequiresContentCorroboration`, `TestQueryReconciliationState_LocalFileMoveIdentityWithoutContentIsNotAMove`, `TestPlannerPlanCurrentState_IdentityMatchedEditedFileIsDeleteAndCreate`, `TestPlannerPlanCurrentState_UnhashedMoveCarriesDestinationTruthAndReuploads`, `TestActionFreshness_LocalOriginMoveRejectsChangedDestination` |
| Locally deleted or unpaired moved folders are deleted on OneDrive after their descendants, never re-created locally; identity-paired folder moves are one remote move. | `TestQueryReconciliationState_FolderDecisionMatrix`, `TestPlannerPlanCurrentState_LocalFolderDeletePropagatesAfterChildDeletes`, `TestPlannerPlanCurrentState_UnpairedLocalFolderMoveDeletesSourceAfterChildMoves`, `TestRunOnce_LocalFolderDeleteDeletesRemoteFolder`, `TestRunOnce_LocalFolderMoveBecomesOneRemoteMoveWithoutResurrectingSource` |
| A plan counts the work it leaves for the next pass (R-2.1.7): a converged plan leaves none, a folder renamed online over a local edit counts its move, a swap counts the parked item as deferred; conflict copies count as renames (the keep-both scenarios finish in one sync). | `TestPlanFollowUp_CountsWorkLeftForTheNextPass`, `TestRunOnce_OneShotUploadsLocalEditInsideFolderRenamedOnline`, `TestRunOnce_SwapOfTwoNamesConverges`, `TestRunOnce_CreateConflictLocalFileRemoteDirKeepsBoth` |
| Moves are composed per item (R-2.17.6): the same rename on both sides only re-keys the baseline and keeps either side's edit pending; divergent renames keep OneDrive's name; cross moves that would nest folders keep the OneDrive move; collisions undo the local claim; moves order against each other and their surroundings, a local move never fabricates its parent, and a pass that cannot order a move defers it, except that a cycle of moves each waiting for the next one's source (a swap or rotation of names) parks one item at a free `<name>.swap-N` sibling so the rest can run, and the parked item follows next pass. Work under a moved file's old path belongs to the folder that takes the path and waits for the move. A delete or cleanup never waits for its parent's folder create. A destination is free only when no other entry holds it under OneDrive's name folding, and an item's remote position may be a path another synced item is moving away from (the item ID proves it). | `TestComposeMoves_DestinationFreeUnderNameFolding`, `TestRunOnce_LocalRenameWhileOneDriveSwapsItConverges`, `TestComposeMoves_RotationParksOneItemAndMovesTheRest`, `TestBuildDependencies_WorkUnderAFileMovesOldPathWaitsForTheMove`, `TestBuildDependencies_DeleteUnderARecreatedFolderDoesNotWaitForTheCreate`, `TestRunOnce_SwapOfTwoNamesConverges`, `TestComposeMoves_OneSidedRenamesMoveTheOtherSide`, `TestComposeMoves_SameRenameOnBothSidesOnlyRekeysTheBaseline`, `TestComposeMoves_DivergentRenameKeepsTheOneDriveName`, `TestComposeMoves_CrossMoveCycleKeepsTheRemoteMove`, `TestComposeMoves_DescendantRenameInsideRenamedFolder`, `TestComposeMoves_RemoteHierarchyInversionOrdersLocalMoves`, `TestComposeMoves_LocalMoveIntoRemotelyRenamedFolder`, `TestComposeMoves_OccupiedDestinationDefers`, `TestComposeMoves_FrozenItemsAndTheirSubtreesDoNotMove`, `TestBuildDependencies_NestedMoveEdges`, `TestRunOnce_BothSidesRenamedIdenticallyConverges`, `TestRunOnce_BothSidesRenamedIdenticallyKeepsTheRemoteEditPending`, `TestRunOnce_DivergentRenameConvergesInOneSync`, `TestRunOnce_CrossMoveCycleKeepsBothTreesWithoutNesting`, `TestRunOnce_RemoteHierarchyInversionConverges`, `TestRunOnce_RemoteFolderRenameWithChildRenameAndMoveInConverges`, `TestRunOnce_LocalFolderRenameWithChildRenameConvergesInOnePass`, `TestRunOnce_RemoteFolderRenameWithNewSubfolderConverges`, `TestExecutor_LocalMove_MissingDestinationParentIsStale`, `TestRunOnce_RandomizedBidirectionalEditsConverge` |
| A remote move of a locally edited file moves the local copy with its edit and uploads the edit afterwards (R-2.17.7); a local rename racing a remote edit downloads the edit (R-2.17.3). | `TestRunOnce_RemoteMoveWithLocalEditMovesThenUploads`, `TestExecutor_LocalMove_LocallyEditedSourceStillMovesAndKeepsTheEditPending`, `TestRunOnce_LocalRenameWhileRemoteEditsDownloadsTheEdit` |
| A remote move over a file OneDrive deleted moves the local copy onto it, replacing a verified unchanged file or conflict-copying an edited one, and never re-uploads the moved item (R-2.17.9). | `TestRunOnce_RemoteMoveOverExistingFileReplacesWithoutResurrecting`, `TestRunOnce_RemoteMoveOverLocallyEditedFileKeepsTheEdit`, `TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination`, `TestExecutor_LocalMove_StaleSourceLeavesTheReplacedDestinationInPlace`, `TestRunOnce_RemoteMoveOverIdenticalFileConvergesInOnePass` |
| Move pairing requires the item to have left its baseline path: hard links and copies are new files, rename-and-recreate is a move plus a create (R-2.17.8). | `TestQueryReconciliationState_MovePairingRequiresTheSourceToBeVacated`, `TestRunOnce_HardLinkUploadsAsANewFile`, `TestRunOnce_RenameAndRecreateMovesThenCreates` |
| Remote moves applied locally carry the source baseline and do not re-upload unchanged content. | `TestPlannerPlanCurrentState_RemoteMoveCarriesSourceBaseline`, `TestRunOnce_RemoteRenameAppliedLocallyDoesNotReupload` |
| Type changes with content on both sides keep both (`conflict_keep_both`, and type-mismatched create/create): a type-aware conflict copy of the local entry, then OneDrive's item at the path; a folder's change includes its subtree; nothing below the path is deleted on either side (R-2.17.10). Both sides replacing the item with the same new type are two creates at one path: equal content (or two folders) only updates the baseline, different files keep both as a create/create conflict.  A new filesystem identity alone is never a local change (D-057): a folder rebuilt, restored from a copy or on a renumbered disk, with nothing changed below it, accepts OneDrive's delete or replacement, and the executor's folder delete checks the identity the plan saw rather than the baseline's (R-2.8.10). | `TestRunOnce_RemoteFolderDeleteAcceptsAContentIdenticalLocalRestore`, `TestRunOnce_RemoteFolderDeleteAfterDeviceRenumberingIsApplied`, `TestRunOnce_RemoteDeleteOfALocallyRecreatedEmptyFolderIsApplied`, `TestRunOnce_RemoteReplaceOfALocallyRecreatedFolderConverges`, `TestRunOnce_FolderReplacedByFilesOnBothSidesKeepsBoth`, `TestRunOnce_FileReplacedByFoldersOnBothSidesMergesThem`, `TestRunOnce_RemoteFileReplacedByNonEmptyFolderWithLocalEditKeepsBoth`, `TestRunOnce_RemoteFolderReplacedByFileWithLocalChildEditsKeepsBoth`, `TestRunOnce_LocalFolderReplacedByFileWithRemoteChildEditKeepsBoth`, `TestRunOnce_CreateConflictLocalDirRemoteFileKeepsBoth`, `TestRunOnce_CreateConflictLocalFileRemoteDirKeepsBoth`, `TestExecutor_ConflictCopy_DirectorySourceIsRenamed`, `TestExecutor_ConflictCopy_DirectoryNeverReplacesAnEmptyDirectoryAtItsName` |
| Type changes on either side are replacements ordered delete-then-create, never a silent stall. | `TestPlannerPlanCurrentState_LocalFileReplacedByFolderReplacesRemoteItem`, `TestPlannerPlanCurrentState_LocalFolderReplacedByFileReplacesRemoteItem`, `TestPlannerPlanCurrentState_RemoteFileReplacedByFolderReplacesLocalItem`, `TestPlannerPlanCurrentState_RemoteFolderReplacedByFileReplacesLocalItem`, `TestRunOnce_LocalFileReplacedByFolderConverges`, `TestRunOnce_LocalFolderWithChildrenReplacedByFileConverges` |
| Deletes toward one side are held for confirmation when the other side is empty while the baseline holds files, or when they exceed max(50, 30% of baseline files); moves never count, non-delete work continues, and `sync --confirm-deletes` releases them. | `TestPlanner_MassRemoteDeleteIsHeldForConfirmation`, `TestPlanner_MassLocalDeleteAfterAnomalousFullRefreshIsHeld`, `TestPlanner_EmptiedLocalSideHoldsEvenFewDeletes`, `TestPlanner_DeleteGuardBelowThresholdProceeds`, `TestPlanner_DeleteGuardIgnoresMoves`, `TestDeleteSafetyHold_FolderDeletesCountTowardThreshold`, `TestDeleteSafetyHold_LostAndFoundDoesNotMakeASideNonEmpty`, `TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo`, `TestDeleteSafetyHold_ConfirmReleasesExactlyTheHeldSet`, `TestDeleteSafetyHold_HeldRowWithoutItemIDDoesNotApproveAKnownItem`, `TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked` |
| A dependency cycle drops only its members and their dependents; the rest of the plan runs (R-2.8.11). | `TestIsolateDependencyCycles_KeepsUnrelatedWork` |
| Mode-specific deferral and dependency ordering stay planner-owned rather than executor- or CLI-owned. | `TestSyncModeFromFlags`, `internal/sync/planner_actions_test.go`, `internal/sync/planner_move_composition_test.go` (`TestBuildDependencies_NestedMoveEdges`, `TestBuildDependencies_WorkUnderAFileMovesOldPathWaitsForTheMove`), `internal/sync/planner_delete_safety_test.go` (`TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo`) |
| Every single-path combination of baseline, local and remote state (absent, file A/B/C, folder), and the move combinations of one synced file, has its plan checked in as `testdata/reconcile/matrix.golden` (refresh with `-update`); with `ONEDRIVE_GO_RECONCILE_CONVERGE=1` each cell is also run to convergence on a pure model and must end at the reference planner's content set. A planner-level model test applies random edits to both sides of a pure model, plans it through the real comparison SQL and planner, applies the plan without an executor (items addressed by ID, a create onto a taken name failing as a 409), and must converge with every written content kept (D-093). | `TestReconcileMatrix_Golden`, `TestPlannerModel_RandomTriplesConverge` |
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
8. Compose moves per item (`planner_move_composition.go`) and normalize the
   action set: preserve a deleted parent folder only when runnable descendant
   actions in the current sync mode need that parent to exist, then rebase
   remote-origin local writes to where folders sit locally and new local
   content under a folder this plan renames locally to the folder's new path.
9. Partition the normalized action set into admitted work and mode-deferred
   counts.
10. Bind ordinary actions to the engine's mounted drive/root context and build
    dependency edges. A dependency cycle drops only the actions on it and the
    actions waiting on them, logged as an error (`isolateDependencyCycles`,
    R-2.8.11, D-059c); it used to abort the whole plan, so one bad pair of
    actions stopped every other change from syncing.

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

## Comparison Facts

`sqlComparisonStateCTEs` in `sqlite_compare.go` is the one definition of the
per-path facts (`local_changed`, `remote_changed`, `current_equal`,
`metadata_drift`, ...) and of the comparison kind. The comparison and
reconciliation queries both select from it, on the live tables and on the
planner-visible scratch tables, so a fact cannot mean two things in two queries
(the comparison query once classified `unchanged` by size and mtime where the
reconciliation query used hashes).

Remote content is compared by hash only when both the baseline and the current
item carry one (R-2.2.7). OneDrive omits hashes for zero-byte files on Business
and for some Office-for-web saves; a hash known on one side only falls back to
size and eTag, so a lost hash is unknown rather than an edit (which used to
download the file again, or conflict with a local edit). Two files whose known
sizes are both zero are equal, so an empty file present on both sides without a
baseline is a baseline update, not a create/create conflict copy.

`remote_item_replaced` marks a baseline path whose remote item now has another
ID. A folder deleted locally while OneDrive replaced it is re-created locally
(`folder_create_local`) rather than deleted online: the new item is not the
folder the user deleted, and a delete would remove it with its content.

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
(`local_device`, `local_inode`, `local_birth_time`, `local_has_identity`)
between `baseline` and `local_state` for both files and directories. File-hash
matching is a fallback only for files without identity.

Identity is (device, inode, birth time where the filesystem records one), and
two identities match only when device and inode agree and, if both sides know
a birth time, the birth times agree too (R-2.17.1). The birth time is what
makes identity name one entry over time: ext4, tmpfs and most Linux
filesystems hand a just-freed inode number to the next entry created, so a
deleted synced folder and an unrelated folder the user creates elsewhere could
share (device, inode). Paired, the old OneDrive folder was renamed into the new
one, and the unrelated folder inherited its sharing links, permissions and
version history; content corroboration cannot catch it for a folder (no
content) or for a file that happens to hold the same bytes. A rename keeps the
birth time and a new entry gets its own, so a mismatch is a delete plus a
create. Where the filesystem records none (ext2/3, FAT, NFS, SMB, most FUSE
filesystems, a kernel without `statx`) identity falls back to (device, inode)
and inode reuse stays indistinguishable from a rename. The rule is written
once per language: `synctree.SameIdentity` in Go and one SQL expression
pattern in the comparison queries (the `sqlSameLocalEntry*` constants, one per
local-row alias, held to the pattern by
`TestSQLSameLocalEntry_EveryAliasFollowsThePattern`), and every identity
comparison goes through one of them.
Identity stays a pairing hint, never a change signal (D-057): a new birth time
at the same path changes nothing by itself, and the baseline learns it through
`metadata_drift`.

Identity is necessary but not sufficient for a file (R-2.17.1). After a delete
and a create in one batch, ext4 and other filesystems may give the new file the
freed inode, and pairing on inode alone once moved `vim.swp`'s OneDrive content
onto an unrelated unreadable `secret.txt`. A rename preserves size, mtime and
content; inode reuse preserves none of them (and, where the filesystem records
one, not the birth time either, see above). So an identity pair is a file move
only when sizes are equal and either both hashes are known and equal, or a hash
is unknown and the mtimes are equal. Everything else (different size, different
known hash, unknown hash with a different mtime) is delete + create. The cost is
that a rename followed by an edit inside one observation window also becomes
delete + create: the content is uploaded fresh and the old item goes to the
recycle bin, which is safe. Directories pair on identity alone: a directory has
no content of its own and its descendants pair independently. A wrong pairing
after rmdir + mkdir reuse converges to the same tree but hands the new folder
the old OneDrive item, which the birth time prevents wherever it is recorded. The pairing rule
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
Identity mismatch at the same path is never a move, and never by itself a
change (R-2.2.6): identity is a pairing hint. A folder with a new identity, or a
file with a new identity whose hash and size equal the baseline's, is a
`baseline_update` that records the new identity; a file whose content differs or
cannot be proven equal (no hash on one side) is an ordinary edit. Before, every
identity change set `local_changed`: a folder removed and restored planned an
upload of a directory that failed every pass, and a device renumbered after a
reboot re-uploaded every file. `buildActionsForReconciliation` refuses a folder
upload or download as a comparison bug (logged, row dropped) so one bad row
cannot wedge the plan.

A type change (file <-> folder) at one path is a replacement (R-2.17.4):
`replace_remote` emits `ActionRemoteDelete` of the old item and then a remote
folder create or a new-item upload; `replace_local` emits `ActionLocalDelete`
and then a local folder create or a download that requires a missing target.
The create depends on the same-path delete. When the two sides now hold
different types and both changed (R-2.17.10, D-059) the result is
`conflict_keep_both`: the local entry, file or folder, becomes a conflict copy,
and OneDrive's item takes the path (a folder is created locally, a file
downloaded into the vacated target). A folder counts as changed when anything
below it is new or differs from its baseline (`local_subtree_changed`,
`remote_subtree_changed`, computed only for a folder the other side replaced
with a file). Before, local won: a local edit inside a folder OneDrive replaced
with a file was deleted with the folder, a remote edit inside a folder the user
replaced with a file went with the remote delete, and a remote folder's children
wedged against a local file still at the path. `trimKeepBothDescendants` keeps
below the path only OneDrive content arriving into the folder that takes it
(turning the remote deletes of its unchanged children into local restores) and
baseline cleanup; work addressed at the renamed local entry is replanned next
pass. A create/create conflict between a folder and a file expands the same way
(D-059a), and a directory's conflict copy is a plain rename to a free
`name.conflict-<timestamp>` with a no-replace rename (renameat2
`RENAME_NOREPLACE` / `renamex_np` `RENAME_EXCL`; a plain rename would replace an
empty directory at the name, and a placeholder cannot be renamed onto without
being replaced). When only one side changed, local wins as before
(`replace_remote`), because a replaced OneDrive item survives in the recycle
bin. A remote delete under a folder the user replaced with a file treats the
path as absent (nothing can exist below a file); its local-absence check used to
fail on the non-folder parent and held the folder's delete forever. Delete
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

### Move Composition

SQLite proves where each synced item sits now (identity pairing locally, item
ID remotely); `composeMoves` decides where it ends up (R-2.17.6). It works on
locations, an item's parent and name on each side, because a path is the
composition of every ancestor's move: composing path by path planned a remote
move whose source the local side had already moved (D-051), superseded the same
rename made on both sides forever (D-050), and moved two folders into each
other (D-055). An item whose location changed on a side was relocated there;
one that only follows a moved parent needs no move of its own, which is why no
descendant move is ever emitted for a folder move.

The target is the relocating side's location. When both sides relocated an
item: to the same place, nothing moves and a publication-only
`ActionBaselineUpdate` with `OldPath` re-keys the baseline row and its subtree,
keeping the content facts of a side that also edited the item so the edit is
still transferred; to different places, OneDrive's location wins (policy P-6,
chosen because neither choice loses data and remote-wins needs no remote
mutation) and the local copy is moved there, with an Info notice. A cycle in the
target tree (local A→B/A while OneDrive moved B→A/B) undoes the local
relocations on it, so local follows OneDrive and no folder nests in itself. Two
items claiming one target path (namekey-folded), or a local claim landing where
OneDrive created a new item, undo the local claim the same way. Undoing only
ever restores OneDrive's locations, so the loop ends with the remote tree.

Emission is conservative and in pass-local coordinates. A move is emitted only
when its target parent is in place or is placed by a move earlier in the same
plan, its destination is free in the snapshot or vacated by an earlier move,
and its source is not displaced by a move that runs first. OneDrive moves are
rebased through an ancestor's committed move (remote_state and the baseline
follow it). Local moves are not, because `local_state` is observation-owned:
a local child move runs inside its folder before the folder moves, or waits a
pass. Anything that cannot be ordered is deferred, with a Debug notice, and the
next pass, which observes the moves this one made, plans it simply; the
shallowest pending item is always ready, so every pass progresses. Items whose
paths touch unavailable truth are frozen with their subtree. Creates that would
land on a path a composed item still holds this pass are dropped until the item
has moved. A remote move whose local copy sits elsewhere in the snapshot carries
`MovedLocalPath`, and a local move ordered before its folder's move carries
`MovedRemotePath`, so freshness reads the snapshot where the item is.

A remote move of a locally edited file is still applied (R-2.17.7): the local
move source must be the same item (type and identity) but its content may
differ; the rename carries the bytes, the outcome keeps the baseline's content
facts, and the next pass uploads the edit as the same item. A move outcome
re-paths the baseline subtree from the row's own previous path (looked up by
item ID), not from `OldPath`, which for a local copy moved to OneDrive's name is
a path the baseline never held.

Move pairing requires the item to have left its baseline path (R-2.17.8): an
identity pair is refused while the baseline path still holds the baseline
identity (a hard link), a hash pair while the baseline path still exists (a copy
without identity). When the baseline path holds a new entry (rename and
recreate), SQLite still reports `local_move_source` and composition also plans
the new entry as an upload or remote folder create that waits for the remote
move to vacate the path. Swaps and rotations are deliberately not paired: the
paths keep their items and exchange content, which is the same end state
without a temporary name.

A remote move onto the path of a synced file OneDrive deleted (a replacing
move, D-058) used to be planned as a download at the destination, which left
the source behind to be uploaded again as a duplicate. The destination's
baseline item now gives way (`moveDisplacement`) when it no longer exists
online: a file whose local copy is provably the synced version is replaced by
the local move (`Action.ReplaceLocalTarget`; the executor re-verifies it through
the descriptor it keeps in the trash, P-4), an edited one is renamed to a
conflict copy first and uploaded later as a new file, and anything else (a
folder, unknown content) makes the move wait. Reconciliation downloads and
conflict copies of an item composition moves locally are dropped, and so is a
metadata baseline update of a moved item (identical content at the destination,
new eTag), which recorded the replaced file's local identity for the moved item.

`buildDependencies` orders moves by the paths they carry (`addMoveDeps`): work
at or below a move's destination runs after it; a move onto a path another move
of the same side vacates waits for it; a move waits for same-side moves out of
its source, for local writes under a local move's source and for remote deletes
under a remote move's source; a create at the path a move vacates waits for the
move; the delete of a destination's old occupant runs first. `ExecuteLocalMove`
requires the destination's parent to exist and reports a missing one as stale
instead of creating it (D-054). A local folder create's outcome never moves the
item in `remote_state` (it may run at a local path that differs from OneDrive's
until a parent's local move commits).

### Follow-Up Work

Planning moves leaves work for the next pass on purpose, and the plan says so
in `actionPlan.FollowUp` (`planFollowUp`, R-2.1.7). The executing side uses it
to run that next pass now instead of reporting the run finished. The count is
set where the planner leaves ready work:

- `Renames`: every admitted move, baseline re-key and conflict copy. A move's
  outcome publishes the new path and keeps the baseline's content facts
  (R-2.17.3, R-2.17.7), and SQLite reconciles a moved folder's subtree against
  the paths the move changes, so a local edit of a moved item, an edit OneDrive
  made with a move or re-key, or a local edit inside a folder renamed online is
  planned only once the move is committed. A conflict copy is new local content
  this plan never saw, uploaded next pass.
- `Deferred`: relocations composition held back (`moveComposition.deferred`):
  moves that cannot be ordered this plan, and the item parked at
  `<name>.swap-N` to break a swap or rotation.
- `Dropped`: actions removed until the next pass: creates onto a path a
  composed item still holds (`dropCreatesOntoComposedItems`), work below a
  keep-both path (`trimKeepBothDescendants`), and actions caught in a
  dependency cycle (`isolateDependencyCycles`).

`Renames` is a deliberate superset: not every move leaves content work behind.
Deciding which do would repeat in Go the reconciliation SQLite performs on the
next pass, and a quiet follow-up pass costs one delta request and one local
scan only after a pass that moved something. Every other "next pass" in the
planner is one of these three; work held back by the sync mode or the
delete-safety hold is not follow-up, because another pass would hold it again.

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
