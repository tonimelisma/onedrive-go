# Sync Execution

GOVERNS: internal/sync/action_freshness.go, internal/sync/active_scopes.go, internal/sync/dep_graph.go, internal/sync/executor.go, internal/sync/executor_conflict.go, internal/sync/executor_delete.go, internal/sync/executor_preconditions.go, internal/sync/executor_transfer.go, internal/sync/scope.go, internal/sync/worker.go

LAYER: 3

Implements: R-2.2.5 [verified], R-2.3.1 [verified], R-2.13.2 [verified], R-2.4.6 [verified], R-2.8.6 [verified], R-2.8.7 [verified], R-2.8.9 [verified], R-2.8.10 [verified], R-2.14.2 [verified], R-6.2.3 [verified], R-6.2.4 [verified], R-6.4.4 [verified], R-6.6.17 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified], R-6.4.9 [verified]

## Overview

Execution takes a runtime plan, dispatches concrete side-effecting
work through a dependency graph, runs workers, and reports one
`actionCompletion` per finished action. That runtime-plan handoff is
assembled on the engine side by the shared pipeline in
`engine_current_plan.go`, then admitted through `engine_runtime_start.go`
before execution begins. Publication-only planner actions are not executor
work: the engine
reduces them directly through the store before workers see any concrete
frontier.

The executor does **not** own retry policy, durable failure classification, or
scope lifecycle. It performs one action and reports the concrete outcome.

## Ownership Contract

- Owns: dispatch, dependency satisfaction, worker execution, conflict-copy creation, and success outcomes
- Does Not Own: planning, retry scheduling, scope activation policy, or store schema
- Source of Truth: planner-produced `actionPlan` plus the rooted capabilities injected into the executor
- Allowed Side Effects: sync-root filesystem mutation, Graph transfer calls, and store success commits through the engine
- Mutable Runtime Owner: workers own only action execution. The engine owns
  runtime quiescence, held-work timing, admission, and dependency completion
  decisions above the worker pool.
- Error Boundary: Workers and executor helpers return concrete action outcomes and execution errors; the engine classifies those outcomes into retries, failures, scope transitions, and durable state changes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| A download replaces an existing local file only when its content hashes to the planned version, or to the baseline's local hash when the plan has no hash, so an unsynced edit that kept size and mtime survives (R-2.2.5). | `TestExecutor_Download_LocalContentDiffersFromBaselineIsSuperseded` |
| A remote move whose item already sits at the destination succeeds without a Graph call. | `TestExecutor_RemoteMove_ItemAlreadyAtDestinationSucceedsWithoutMoving` |
| Folder eTag churn from descendant changes neither supersedes a folder move nor conditions it, while a file's eTag mismatch still supersedes. | `TestExecutor_RemoteMove_FolderETagChurnFromChildrenDoesNotSupersede`, `TestExecutor_RemoteMove_FileETagMismatchStillSupersedes`, `TestRunOnce_LocalFolderMoveSurvivesFolderETagChurn` |
| Move outcomes publish observed or proven local facts, keep a pending remote edit visible, and folder creates record local filesystem identity. | `TestExecutor_LocalMove_PublishesSourceBaselineLocalHalf`, `TestExecutor_LocalMove_RemoteEditKeepsBaselineRemoteContent`, `TestExecutor_RemoteMove_PublishesObservedDestinationLocalTruth`, `TestExecutor_CreateLocalFolder_RecordsFilesystemIdentity`, `TestExecutor_CreateRemoteFolder_RecordsObservedLocalIdentity` |
| Edit/edit and create/create conflicts are handled immediately by preserving both versions with a local conflict copy and downloading the canonical remote version. | `TestExecutor_Conflict_EditEdit_KeepBoth`, `TestExecutor_Conflict_EditEdit_KeepBoth_ConflictCopyCollisionGetsSuffix`, `TestExecutor_ConflictDownloadFails_LeavesConflictCopy`, `TestConflictCopyPath_Normal` |
| Planner-generated edit/delete uploads remain concrete execution work, while stale local deletes return a superseded precondition outcome so the engine replans instead of inventing new sync intent inside the executor. | `TestExecutor_Conflict_EditDelete_RecreatesRemoteFromLocal`, `TestExecutor_ConflictCopy_ReservationGivesEachCopyItsOwnDestination`, `TestExecutor_ConflictCopy_ReleasedReservationLeavesNoPlaceholder`, `TestExecutor_ConflictCopy_LeavesExactlyOneCopyHoldingTheOriginal`, `TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition`, `TestExecutor_LocalDelete_NoBaselineHashAndChangedFile_PreservesLocalContent`, `TestExecutor_LocalDelete_NoBaselineHashAndUnchangedFile_Deletes`, `TestExecutor_LocalDelete_NoBaselineEvidence_PreservesLocalContent`, `TestEngineFlow_ProcessNormalDecision_SupersededRetiresSubtreeWithoutRetryOrSuccess` |
| Worker-start validation rejects already-submitted stale actions before executor side effects, while suspect local truth disables local-state-based rejection. Dependent uploads after planned remote moves tolerate move-produced eTag churn but still reject proven remote content drift, and executable actions without planner truth fail closed. | `TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution`, `TestWorkerStartFreshness_SuspectLocalTruthDoesNotSupersedeFromLocalState`, `TestActionFreshness_PostRemoteMoveUploadAllowsMoveProducedETagChange`, `TestActionFreshness_PostRemoteMoveUploadRejectsRemoteContentChange`, `TestActionFreshness_MissingPlannerViewFailsClosedForExecutableAction` |
| Local mutation is refused beneath a symlinked ancestor, at every mutating action, with the target content outside the sync root left byte-identical. | `TestExecutor_Download_SymlinkedAncestorIsBlocked`, `TestExecutor_LocalMove_SymlinkedAncestorSourceIsBlocked`, `TestExecutor_LocalMove_SymlinkedAncestorDestinationIsBlocked`, `TestExecutor_ConflictCopy_SymlinkedAncestorIsBlocked`, `TestExecutor_CreateLocalFolder_SymlinkedAncestorIsBlocked`, `TestExecutor_LocalDelete_SymlinkedAncestorReturnsStalePrecondition`, `TestExecutor_LocalDelete_AliasItselfStillRemovesOnlyTheSymlink`, `TestExecutor_LocalMove_WithoutSymlinkBoundarySucceeds` |
| Uploads read through a followed alias: a file beneath a directory symlink whose target is outside the sync root uploads with the target content, its followed facts are still checked against the plan, the same path still refuses a download, and a path that lexically leaves the root is refused. | `TestRunOnce_FollowSymlinks_UploadsFileInsideDirectoryAliasOutsideRoot`, `TestExecutor_Upload_UnderFollowedDirectoryAliasSucceeds`, `TestExecutor_Upload_UnderFollowedDirectoryAliasDetectsChangedTarget`, `TestExecutor_UnderFollowedDirectoryAlias_UploadReadsButDownloadStaysRefused`, `TestExecutor_Upload_PathLeavingSyncRootIsRefused`, `TestExecutor_Upload_SymlinkAliasSourceUsesFollowedTargetPrecondition` |
| Local deletes and download overwrites act only on the inode they verified, keep a file replaced mid-check, and put what they remove or replace in the drive's local trash. | `TestDeleteLocalFile_MovesTheFileToTheTrash`, `TestDeleteLocalFile_FileReplacedMidHashSurvives`, `TestDetachRegularFile_FileReplacedDuringVerifyIsKept`, `TestRunOnce_RemoteDeleteMovesLocalFileToTrash`, `TestRunOnce_DownloadOverwriteKeepsPreviousVersionInTrash`, `TestDetachRegularFile_InPlaceWriteAfterVerifyIsKept`, `TestCopyVerified_WriteDuringCopyDiscardsTheCopy`, `TestTrash_CrashBetweenMoveAndCommitIsRecoverable`, `TestTrash_FailedCommitLeavesARecoverablePendingEntry`, `TestTrash_OrphanItemIsListedAndKept`, `TestTrash_RestoreAcrossFilesystemsCopies`, `TestTrash_RestoreRefusesSymlinkedAncestor`, `TestFullScan_LeftoverSetAsideFileIsReportedNotDeleted`, `TestTrash_CrossFilesystemRestoreKeepsModeAndTime`, `TestTrash_UncheckableItemIsNeverPurged`, `TestLinkOrCopyNoReplace_FilesystemWithoutHardLinksCopies`, `TestLinkOrCopyNoReplace_SourceThatCannotBeRemovedIsReportedAfterSuccess` |
| A cross-filesystem trash placement or restore is durable before the source goes: the destination folder is fsynced after the rename, a sync failure discards the copy, and an entry left with only an interrupted `.copy-*` file is listed, never purged, and not restored as the file. | `TestCopyVerified_SyncsTheDestinationFolder`, `TestCopyVerified_DirectorySyncFailureLeavesNothingAtTheDestination`, `TestCopyVerified_ToleratesUnsupportedDirectorySync`, `TestTrash_CrossFilesystemRestoreSyncsTheTargetFolder`, `TestTrash_IncompleteCrossFilesystemCopyIsNeverPurged` |
| A recursive remote folder delete refuses to take descendants sync never saw or that changed online; a remote delete re-checks local absence and the sync folder identity; a local folder delete over only ignored children converges and the folder stays deleted online until it holds something sync sees. | `TestExecuteRemoteDelete_FolderWithUnobservedChildIsStale`, `TestExecuteRemoteDelete_FolderWithOnlySyncedUnchangedChildrenRuns`, `TestExecuteRemoteDelete_FolderChildChangedOnlyInETagOrSizeIsStale`, `TestExecuteRemoteDelete_FolderOverListingCapIsDurableIssueNotStale`, `TestRunOnce_LocalFolderDeleteKeepsRemoteChildAddedAfterObservation`, `TestExecuteRemoteDelete_LocalPathReappearedIsSuperseded`, `TestDeleteLocalFolder_OnlyIgnoredChildrenConverges`, `TestRunOnce_RemoteFolderDeleteWithIgnoredLocalFileConverges`, `TestRunOnce_FolderRecreatedWhereAKeptFolderWasIsCreatedOnline` |
| Through the engine, a folder whose live listing exceeds the cap is left on OneDrive, its delete recorded as a backed-off durable issue that the next pass neither relists nor retries; a child edited online after observation stops the folder delete and the edit comes back down; a folder back locally before its delete runs stays on both sides; and local absence still holds below a regular file and across a type change, so those remote deletes run. | `TestRunOnce_RemoteFolderDeleteOverListingCapIsADurableIssueNotRelistedEveryPass`, `TestRunOnce_RemoteFolderDeleteRefusesAChildEditedOnlineAfterObservation`, `TestRunOnce_RemoteFolderDeleteRefusedWhenTheFolderIsBackLocally`, `TestExecuteRemoteDelete_LocalAbsenceHoldsBelowAFileAndAcrossATypeChange` |
| A crash right after a local delete moved the file into the trash (its entry still pending) or right after OneDrive deleted an item, before either outcome commits, recovers: the trash entry is listed once and restores, nothing is deleted twice, re-uploaded or downloaded again (crash points `delete.local_trashed` and `delete.remote_done`, D-091). | `TestCrashRecovery_DeleteBoundariesDeleteOnceAndKeepTheTrashEntry`, `TestCrashRecovery_EveryCrashPointConverges` |
| Executor live preconditions reject stale work at the side-effect boundary without mutating local or remote state. | `TestExecuteRemoteDelete_NotFoundPreflightReturnsStalePreconditionAndDoesNotDelete`, `TestExecuteRemoteDelete_ETagMismatchPreflightReturnsStalePreconditionAndDoesNotDelete`, `TestExecuteRemoteDelete_TransientPreflightFailureIsOrdinaryFailure`, `TestExecutor_RemoteDelete_UsesConditionalETagFromPreflight`, `TestExecutor_RemoteDelete_ConditionalMismatchReturnsStalePrecondition`, `TestExecutor_RemoteDelete_WrongDrivePreflightReturnsStalePrecondition`, `TestExecutor_RemoteDelete_StalePathPreflightReturnsStalePrecondition`, `TestExecutor_RemoteMove_StaleSourcePreflightReturnsStalePrecondition`, `TestExecutor_RemoteMove_UsesConditionalETagFromPreflight`, `TestExecutor_RemoteMove_ConditionalMismatchReturnsStalePrecondition`, `TestExecutor_CreateRemoteFolder_MissingParentPreflightReturnsStalePrecondition`, `TestExecutor_Upload_SourceHashChangedBeforeTransferReturnsStalePrecondition`, `TestExecutor_Download_TargetAppearsBeforeRenameReturnsStalePrecondition`, `TestExecutor_ConflictDownload_TargetReappearsAfterConflictCopyReturnsStalePrecondition`, `TestExecutor_Download_MountRootAllowsGraphDriveRootPath`, `TestExecutor_LocalMove_MissingDestinationParentIsStale`, `TestExecutor_LocalMove_LocallyEditedSourceStillMovesAndKeepsTheEditPending`, `TestExecutor_LocalMove_FolderIdentityChangedReturnsStalePrecondition`, `TestExecutor_LocalDelete_FolderIdentityChangedReturnsStalePrecondition`, `TestExecutor_LocalDelete_SymlinkedAncestorReturnsStalePrecondition` |
| On a byte-exact filesystem every local operation addresses an existing entry by its spelling on disk: an NFD-named file uploads in one pass, a remote edit replaces it in place, a remote delete, rename or move removes or moves it, a conflict copy preserves it, an NFD-named folder receives remote additions inside itself and is deleted, a remote delete is superseded when the NFD-named file is back, and the permission probe reports the stored boundary (R-2.13.2). | `TestRunOnce_LocalNFDNamedFileUploadsInOnePass`, `TestRunOnce_RemoteEditReplacesTheNFDNamedLocalFile`, `TestRunOnce_RemoteDeleteRemovesTheNFDNamedLocalFile`, `TestRunOnce_RemoteMoveMovesTheNFDNamedLocalFile`, `TestRunOnce_ConflictingEditsOfAnNFDNamedFileKeepBoth`, `TestRunOnce_NFDNamedLocalFolderReceivesAdditionsAndIsDeleted`, `TestExecuteRemoteDelete_NFDNamedLocalFileBackIsSuperseded`, `TestPermHandler_HandleLocalPermission_ProbesTheNFDNamedFolder` |
| Workers preserve executor failure capability on completions and record worker-start/live-precondition superseded counters by local-vs-remote source. | `TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution`, `TestWorkerStartFreshness_RemoteDownloadMismatchRecordsRemoteTruthCounter`, `TestWorkerPool_SendResultCountsLivePreconditionSupersededByCapability`, `TestWorkerPool_SendResultDoesNotGuessLivePreconditionSourceWithoutCapability` |
| A worker commits a successful action's outcome on a context detached from the pool's cancellation and bounded by `outcomeCommitTimeout`, so a shutdown landing between the transfer and its commit still records the finished work. | `TestWorkerPool_CommitsCompletedOutcomeAfterPoolCancellation` |
| Publication-only planner actions commit baseline mutations without worker dispatch and release dependents through the engine-owned publication-drain stage. | `TestPublicationMutation_SyncedUpdate`, `TestPublicationMutation_SyncedUpdate_BaselineFallback`, `TestPublicationMutation_Cleanup`, `TestPublicationMutation_Cleanup_FolderType`, `TestRunPublicationDrainStage_DoesNotReleaseUnrelatedHeldWork` |
| Watch-mode replan keeps old-runtime work out of dispatch once it is no longer current and preserves dirty intent across recoverable local-observation failure. | `TestWatchRuntime_RunNonDrainingWatchStepPrioritizesReadyReplanOverDispatch`, `TestWatchRuntime_QueuePendingReplanRetiresOldOutbox`, `TestWatchRuntime_PendingReplanRetiresDependentsReleasedByRunningAction`, `TestWatchRuntime_PendingReplanLocalObservationFailureReschedulesDirtySignal`, `TestWatchRuntime_IdleReplanLocalObservationFailureReschedulesDirtySignal` |

## Worker And Dependency Model

`depGraph` is the execution-time dependency graph. It tracks:

- which actions are in flight
- which dependencies remain unsatisfied
- which dependents become ready after completion

Workers run independent actions concurrently up to the configured worker limit.
The engine owns the worker pool lifecycle and completion drain. In watch mode,
the worker dispatch channel is intentionally unbuffered so not-yet-started
actions remain in the engine-owned outbox until a worker is ready to receive
one. The worker completion channel remains separately buffered so workers can
report completed actions without reintroducing a hidden dispatch backlog.
When a replan signal is already ready, the watch loop consumes it before
enabling the dispatch send for the current step, so the old outbox is retired
instead of racing an idle worker receive.

Before a worker begins executor work, it validates the exact action against
latest committed truth using the shared action-freshness predicate. Admission
uses the same predicate only for actions that are about to enter the worker
outbox; retry-held or scope-held actions are checked when they later become
dispatch candidates. A stale worker-start action returns
`errActionPreconditionChanged`, so the engine classifies it as superseded
instead of ordinary retry. This gate runs before hashing, upload-session
creation, download writes, delete/move mutation, or other executor side
effects. Canceled worker/admission freshness checks fail closed before any
store read or executor side effect; the shutdown-completion path is the only
caller allowed to bypass canceled-context freshness, and it immediately
collapses the returned graph frontier instead of dispatching it. The predicate
is intentionally not a second planner: it only rejects when current
truth disproves the exact planned assumptions. Local absence or presence can
reject only while `local_truth_complete` is true;
suspect local truth leaves local-state-based rejection to the next full local
refresh. Remote-state changes can reject when committed remote truth proves a
path, item identity, hash, or eTag assumption false. A dependent upload after a
planned remote move is the narrow exception: its planned remote snapshot is the
pre-move item, so the freshness predicate ignores eTag churn caused by the
planned move itself while still requiring the current target row to match item
identity and content facts. Executable actions must carry planner view truth;
missing planner view data is an internal validation error once committed truth
is authoritative, not a reason to skip stale work checks. Move actions
additionally check the source/destination peer path that is not represented by
the main `pathView`, so a changed local source, a remote move source the moved
item itself is back at, missing local move destination, a local move destination
that no longer matches the planner's `Action.MovedLocal`, or occupied remote move
destination supersedes the old move before side effects. Another item at a local
move's remote source path is a chain or swap that took the vacated path, not the
move undone; superseding on it wedged the chain on every pass
(`TestRunOnce_RemoteMoveChainIntoVacatedPathConverges`). A local move source with a new
inode that still holds the synced content (size and hash) is the synced item
(D-057), in worker-start freshness and in the executor alike: a folder rebuilt
with the same files gave each a new inode, and every move of them superseded
forever (`TestRunOnce_RemoteRenameInsideLocallyRecreatedFolderConverges`). SQLite's remote-move pairing and
move composition apply the same test, so a file replaced at the path by a
different one (new inode, other content) is a new local entry, never the
source of a planned move (`TestRunOnce_RemoteMoveOfAFileTheUserReplacedLocallyKeepsBoth`).

A remote move whose live preflight finds the item already at the planned
destination, with the same identity and content, is done rather than stale
(an earlier attempt or a parent folder move got there): the executor records
the move outcome without calling Graph. The source preflight runs before parent
resolution so an already-applied move needs nothing else.

Move outcomes publish only what was observed or proven (R-2.17.3).
`ActionRemoteMove` (a local-origin move) takes its local half from
`Action.MovedLocal` and keeps an unknown destination hash unknown instead of
inheriting the source baseline's. `ActionLocalMove` (a remote move applied
locally) takes its local half from the source baseline its view carries: the
precondition proved the source still matched it and a rename preserves content,
size, mtime and identity. The one fact a rename changes is a file's inode
change time (Linux filesystems update it), so the executor re-reads it at the
destination when that entry still has the moved file's identity
(`observeMovedLocalChangeTime`); kept at the source's, the next scan saw
metadata drift and every applied remote file move was followed by a second,
metadata-only baseline update (`TestRunOnce_AppliedMoveLeavesTheBaselineCurrent`).
If the remote item's content changed as well as its
path, the outcome keeps the baseline's remote content facts so the next pass
downloads the edit instead of recording an agreement that does not exist.

Worker-start rejections record aggregate perf counters by truth authority
(`local_state` or `remote_state`). Executor live-precondition rejections carry
their failure capability through `actionCompletion`, so the worker boundary can
record local-vs-remote live-precondition superseded counters without parsing
logs or paths. The worker metric boundary does not infer live-precondition
source from action type; ambiguous outcomes must be fixed at the executor
boundary instead of guessed during result reporting. Delete-side stale
preconditions follow that same rule: local delete missing/hash-mismatch races
carry local-write capability, and remote delete not-found-after-preflight races
carry remote-write capability.

Local delete verifies before it destroys, and the verification has to hold
when the baseline carries no hash. The scanner records an empty hash when
hashing a file fails, so an unhashed baseline row is a normal state rather
than a corrupt one. Deleting on that row without a check would remove local
content without ever asking whether the user changed it since -- the exact
loss S4 exists to prevent, reached through the gap in S4's own precondition.

The executor therefore verifies in three tiers: by hash when the baseline
records one; otherwise by the baseline's recorded size and mtime; and when the
baseline records neither, it refuses. Size and mtime are too weak to prove two
files equal, which is why planning never uses them that way, but they are
sound in this direction: any difference is proof the file changed. The refusal
on no evidence is the same rule stated for the empty case -- absence of
evidence is not evidence of sameness, and a destructive action may not run on
it. Each refusal is a superseded precondition, so the path is replanned rather
than stranded, and a row that later hashes cleanly deletes normally.

A create upload records the new item as a pending create through
`UploadOpts.OnCreated` before any content is sent (R-5.6.15). A 409 on the
create is resolved, not retried as is: into our own placeholder from an
interrupted attempt (upload by ID with If-Match on the eTag just read while it
is still empty; the creation eTag does not work, since a canceled upload
session moves it, `TestRunOnce_UploadOnlyCreateFinishesIntoItsPlaceholderAfterItsETagMoved`), as a
durable name collision when OneDrive holds a differently spelled sibling of
the same name (`remoteNameTakenError`, classified as `case_collision` with
backoff, naming both names;
`TestRunOnce_UploadOnlyCreateOntoADifferentlySpelledRemoteNameIsADurableCollision`),
or as a stale precondition when the same spelling exists or nothing is
visible yet. A remote folder create resolves its 409 the same way: an
existing folder of the same spelling is adopted, a differently spelled
sibling is the same durable collision (as a stale precondition it was
replanned and refused on every pass;
`TestRunOnce_UploadOnlyFolderCreateOntoADifferentlySpelledRemoteNameIsADurableCollision`,
`TestExecutor_CreateRemoteFolder_ConflictWithAnotherSpellingIsADurableCollision`).

A spelling-only local move (the old and new names are one OneDrive name:
case or Unicode form) accepts a destination that is the source's own inode,
which is what a case- or normalization-insensitive volume reports. It renames
directly, and only when the directory still holds the old spelling goes
through a temporary sibling named `.onedrive-go.spelling.<name>`, which local
observation always excludes and cleanup never deletes, so a crash between the
two renames leaves nothing that syncs (R-2.12.4,
`TestContentFilter_SpellingRenameArtifactIsAlwaysExcluded`,
`TestExecutor_LocalMove_SpellingOnlyRenameOntoSameInodeIsAllowed`). Refusing it
left a remote case-only rename stale on every pass on macOS.

Local move checks that its destination is absent and then renames, which is
the same check-then-act shape the conflict copy no longer uses. The asymmetry
is deliberate, and worth stating so it does not read as an oversight.

A conflict-copy collision is reachable by design: the destination name carries
a one-second timestamp, so two copies resolved in the same second are handed
the same name by construction, and no amount of care in the planner prevents
it. A local move destination is chosen by the planner, verified absent, and
serialized against other actions on that path by the dependency graph. What
remains is the window between the check and the rename, which only something
outside this program can fill.

Closing that window atomically needs `RENAME_NOREPLACE` on Linux and
`RENAME_EXCL` on macOS, with no equivalent on FreeBSD, which the build
supports. The exclusive-create reservation used for conflict copies does not
substitute here, because a local move can move a directory and a rename cannot
replace a placeholder file with one. Rather than carry a three-way platform
split and a type-dependent code path for a race the planner already narrows,
the check stands and the residual risk is recorded here.

A conflict copy claims its destination instead of finding a free one. The
readable part of the name is the timestamp, but uniqueness cannot come from
statting candidates and then renaming onto the winner: rename silently
replaces whatever occupies the destination, so anything that appeared between
the check and the act was destroyed by the operation whose entire purpose is
to preserve a file. Two conflict copies resolved within the same second are
the reachable case, and mounts sharing a tree or the user creating the name
are others.

The executor therefore creates each candidate exclusively and treats an
existing name as taken, walking the ordinal suffixes until a create succeeds.
That makes the claim atomic against every other creator of the name. The
placeholder is empty, so a claim that ends up unused is released rather than
left behind, where it would read as a truncated conflict copy. Every
candidate, ordinal and extension included, is built to fit the 255-byte name
limit (R-2.11.11, `TestExecutor_SecondSameSecondConflictCopyOfALongNameIsWritable`);
the ordinal used to be appended after the stem was already trimmed to the
limit, so the second same-second copy of a long name was unwritable.

The dependency graph is dependency-only. It no longer defines runtime
quiescence. Held retry/scope work intentionally keeps exact nodes unresolved,
so the engine decides when the current runtime is quiescent based on outbox,
running work, and due held entries. `depGraph` therefore does not expose a
runtime-completion channel; callers use dependency release plus engine-owned
settle checks instead. When shutdown has already started, the engine still
processes late worker completions for bookkeeping, but it immediately converts
any newly-ready frontier back into shutdown completion instead of reopening the
dispatch path.

When watch mode has a pending replan, newly-ready concrete frontier from the old
runtime is likewise not reopened into dispatch. The engine retires
not-yet-dispatched old outbox work and any dependents released by already-running
old actions without completing those dependency nodes as success. The next
runtime plan is rebuilt from current truth and durable retry/block state. If
local observation fails before replacement runtime installation, retired work
remains retired and the dirty/full-refresh intent is rescheduled for a later
steady-state replan. Idle replans that fail during local observation preserve
the same dirty/full-refresh intent through that scheduler instead of dropping
the trigger.

## Executor Live Preconditions

Worker-start and admission freshness checks keep stale work from reaching the
executor when committed truth already proves the action obsolete. The executor
still owns the final side-effect boundary, because local files and Graph items
can change after dispatch and before the mutation or transfer actually begins.
`executor_preconditions.go` is that boundary owner.

For uploads, the executor validates that the local source path is still inside
the sync root, resolves to a regular file, and still matches planned identity,
size/mtime, and hash facts when those facts are available. Symlink aliases
admitted by local observation are validated against the followed target facts
that observation persisted, not rejected merely because the alias directory
entry is a symlink. That holds for a file beneath a symlinked directory as
well as for a file symlink: when the source or any ancestor is a link
(`symlinkBoundaryForPath`, a rooted no-follow walk), the source is stated by
following links, as observation did; otherwise it is stated through the rooted
no-follow `synctree.Lstat`. The rooted capability is deliberately not asked
about a followed source, because it refuses every name that resolves outside
the sync root, which is right for a write and wrong for this read: an alias to
another disk would otherwise never upload. The lexical containment check
(`synctree.Abs`) still runs first. The transfer manager then reuses the executor-supplied
callback before upload reads so a large file can abort if it changes
mid-session. Expected source-hash mismatches are returned as
`errActionPreconditionChanged`.

For downloads, the executor validates the planned destination before transfer
starts and supplies the same callback for the transfer manager to run
immediately before the partial file is atomically renamed into place. If the
plan expected absence and a local file appears, or the plan expected an
overwrite and the existing file no longer matches, the download is superseded
and the partial remains available for ordinary transfer recovery.

For local deletes and moves, the executor rejects missing or changed planned
sources instead of treating them as successful mutation. File sources validate
planned content facts when available; folder sources validate stable local
identity when the sync tree supplied device/inode facts (and the birth time,
where both the plan and the live entry know one: a folder born again under the
planned inode number is not the planned source, R-2.17.1). Local moves also reject
an unexpectedly occupied destination before `Rename`.

For remote deletes, moves, downloads, overwrite uploads, and remote folder/file
creates, the executor performs a live `GetItem` preflight for the planned
source item or parent. Parent-based remote creates first wait for the
already-planned parent path to become visible through the convergence boundary,
then run the stale parent preflight so transient post-create Graph visibility
lag is absorbed before stale proof is evaluated. If the visibility wait times
out because the parent is actually gone, the stale parent preflight result takes
precedence over the visibility timeout. A live `itemNotFound` result, changed item identity,
drive, item type, eTag, hash, size, or same-coordinate path where Graph supplies
that fact is a stale precondition and maps to `errActionPreconditionChanged`.
Folder eTags are the exception: OneDrive changes a folder's eTag whenever
anything beneath it changes, so a folder's recorded eTag (often captured when
it was created, before its children were uploaded) is not evidence the folder
changed, and comparing it once made every local folder move supersede forever.
Folders are checked on identity, drive, type and path, both in this preflight
and in worker-start freshness. Remote delete and move also pass the live
preflight eTag to Graph as an `If-Match` condition when one is available,
except for a folder move, where a concurrent child change does not make the
move unsafe; a folder delete keeps the condition, so a child appearing after
the preflight still stops the delete. If Graph returns 412 after the
explicit preflight, the executor treats that as a post-preflight stale
precondition with remote-write capability and does not report ordinary retry
work for that exact old action.

A folder delete on OneDrive is recursive, so after the preflight it lists the
folder's live descendants (`validateRemoteFolderDeleteChildren`). The plan's
child deletes have already run, so every remaining descendant must be a
baseline item whose remote hash, eTag and size are unchanged (the baseline
keeps no cTag); a descendant sync never saw (a child created online after
observation, or hidden by delta lag) or one changed online supersedes the
delete, and the next pass observes it and plans from the truth (D-006). A
folder holding more than 10,000 items is not listed further: the delete fails
with the durable `remote_folder_delete_unverifiable` issue instead of a stale
verdict that would relist it on every pass. The folder eTag the preflight read is the `If-Match`, so a child
added after the listing still turns the DELETE into a 412.

A remote delete is planned from local absence, so before acting it re-checks
that absence (`validateRemoteDeleteLocalAbsence`, D-011): the sync folder must
still match the identity the engine validated, and an entry of the deleted
item's kind back at the exact name supersedes the delete. An entry of the other
kind is the new half of a type-change replacement, and a differently-cased
name on a case-insensitive volume is the rename the delete belongs to; neither
blocks.

A local folder delete whose only remaining children are hidden by the content
filter (an ignored directory, a dotfile under `ignore_dotfiles`, junk under
`ignore_junk_files`) used to fail on every pass. It now succeeds without
removing anything: the folder's baseline row goes, and the folder and its
ignored content stay (D-012). The outcome is marked `KeptLocally` with the
folder's identity, and the store records it in `kept_local_folders` in the
same transaction, so the planner does not read the folder as new local content
and re-create it, empty, online: the deletion on OneDrive stands (R-2.17.2).
Once the folder holds something sync sees it is ordinary local content again,
and it is created online with that content.
Mount-root engines do not use raw `GetItem` parent-reference paths as
staleness proof because Graph reports those paths relative to the drive root
while the executor's actions are relative to the mounted item. A transient `GetItem`
failure is not superseded; it remains an ordinary read failure so retry policy
can handle it normally. A dependent upload after a planned remote move keeps
the same eTag exception used by worker-start freshness: move-produced eTag churn
alone does not make the upload stale when item identity and content facts still
match.

When the dependency graph releases `ActionBaselineUpdate` or `ActionCleanup`,
the engine does not spend worker capacity on them. It commits the matching
baseline mutation synchronously, marks that graph node successful, drains any
further publication-only dependents through the engine-owned publication
drain stage, and only then releases concrete dependents for worker
dispatch.

A download never replaces a SQLite database an application has open:
`refuseLiveDatabaseTarget` returns `errDatabaseInUse` when the target has a
non-empty `-wal` or `-journal` sibling, whether or not the target itself exists yet, both before the transfer and again in
`ValidateTargetBeforeRename`. It is the retryable per-file condition
`local_database_open`, not a superseded action, because superseding would
replan the same download at once (R-2.4.15,
`TestExecutor_Download_RefusesToReplaceDatabaseWithLiveWAL`,
`TestExecutor_Download_DatabaseOpenedDuringDownloadIsNotReplaced`,
`TestExecutor_Download_RefusesNewDatabaseBesideALiveWAL`).

## Publication-Only Actions

`ActionBaselineUpdate` and `ActionCleanup` remain planner-visible action types so
they still participate in dependency ordering, accounting, and reporting.

Execution does not own them because they perform no external side effects:

- no filesystem mutation
- no Graph call
- no transfer manager work

Their only durable effect is baseline publication, so the engine/store
boundary commits them directly via `CommitMutation()`.

Publication success stays entirely on the engine side: the engine marks the
graph node successful and releases any dependents without synthesizing a worker
completion. Publication failure still uses the shared result classifier so the
exact publication action can persist `retry_work` and remain held in the
current runtime instead of terminating the loop on a transient store error.
When held publication work becomes due again, the engine routes it back through
the publication-drain stage before any worker dispatch. Workers reject
publication-only action types only as an invariant guard; normal runtime flow
must never send those actions to the worker pool.

Publication drain itself is not an outbox helper. It is an effectful
engine/store stage, and `reduceReadyFrontierStage` is the owning runtime stage
that sequences publication drain plus due-held release. Callers hand the engine
exact ready actions, the engine durably applies publication mutations, routes
publication failures through normal completion handling, releases any already-
due held work, and returns only the surviving concrete worker frontier to the
caller’s dispatch queue. Watch bootstrap, steady-state completions, and held-
release ticks all re-enter that same stage before anything reaches the worker
outbox.

## File And Folder Mutation

### Uploads and downloads

Execution delegates transfer mechanics to `driveops.TransferManager`.

- downloads use atomic local writes and integrity verification, and write
  through `syncDownloadTarget`, a `driveops.TargetFS` rooted in the sync
  tree: the partial, its fsync and mode, the rename and the directory sync
  never leave the root, and a parent is never created. A missing or
  non-folder parent is a stale precondition and the engine replans
  (`TestExecutor_Download_MissingParentIsStaleAndNotCreated`,
  `TestExecutor_Download_ParentSwappedForSymlinkAfterCheckStaysInRoot`).
  Rule: no executor side effect creates or replaces namespace state the plan
  did not observe. Downloads used to `MkdirAll` the parent through absolute
  paths, so a local rename between planning and dispatch re-created the old
  folder holding the download (D-043)
- uploads prefer overwrite-by-item-ID when the planner already knows the
  authoritative remote item
- true creates still use parent-path upload because no remote item identity
  exists yet

Successful outcomes still preserve remote ancestry for later baseline writes,
but execution no longer reloads that parent ID from `remote_state`. Outcome
parent recovery is:

- use the live Graph item returned by the mutation when it already carries
  `ParentID`
- otherwise resolve the parent from the action path against current baseline
  state (`ResolveParentID`)
- finally fall back to the baseline view already attached to the action when a
  durable parent is known there

That keeps `baseline.parent_id` as the durable ancestry authority without
requiring a second persisted parent field in `remote_state`.

Execution-time validation is always-on where it matters. Upload overwrite
preflight and similar validation-before-mutate checks are executor-owned and
apply in both one-shot and watch mode; they are not gated on a watch-only
policy flag.

### Local spelling on disk

Every stored path is NFC (R-2.13.1). On macOS the filesystem finds an entry
by either Unicode spelling, but a byte-exact filesystem (Linux ext4, the CI
runners) answers ENOENT for the NFC spelling of an entry whose name on disk
is NFD. Before R-2.13.2 an NFD-named local file was never uploaded: each pass
superseded the upload as "upload source ... is missing" and reported success.

The executor therefore turns the action's stored path into a rooted path
spelled as on disk once, at the start of each local operation, and uses that
path for every filesystem call of the action; outcomes, logs and messages
keep the stored path. The resolution is `synctree.Root.Spelling` (see
`system.md`):

- an existing entry (`Spelling`): the upload source, a download over an
  existing file (`downloadTargetSpelling`, chosen when the plan saw a local
  file), a local delete, both endpoints of a local move (an existing
  destination is what a replacing move replaces), a conflict copy's source,
  and a remote delete's local-absence check;
- a new entry (`SpellingOfParent`): a download of a new file, a local folder
  create and a conflict copy's name are written under the stored name inside
  their parents' spellings on disk, and the new entry's own folder is never
  listed for it, so a large initial download does not read its folder once
  per file.

The parent engine's shortcut-alias lifecycle (`shortcut_root_lifecycle.go`)
resolves the same way (`shortcutRootOnDisk`): observing the alias folder,
materializing it inside existing parents, listing its parent for rename
candidates (which are named by their stored NFC path, so the alias listed
under its own NFD name is not a rename of itself), moving it after a remote
rename, and removing it after the shortcut is removed. It used the stored
spelling, so an NFD-named alias read as missing: the lifecycle renamed the
shortcut online to the NFD name, or deleted it, and a removed shortcut's
folder stayed behind (`TestEngine_ShortcutAliasWithNFDNameOnDiskIsNotARename`,
`TestEngine_RemovedShortcutReleaseRemovesTheNFDNamedAliasFolder`).

Absolute paths for transfers and hashing (`driveops` opens upload sources and
hashes through `localpath` by absolute path) come from `Abs` of the resolved
path, so they bypass nothing. The local permission probe stats the resolved
path and reports the boundary it finds in the stored form.

### Local deletes

Local delete keeps the ordinary per-item safety rule:

- if the file still hashes to the baseline hash, delete it
- if the file changed after planning, do **not** delete newer local content;
  return a stale-precondition/superseded outcome so the engine replans from
  current truth without retrying that exact old delete

Verification and removal are bound to one inode (D-010). `DeleteLocalFile`
hands `synctree.DetachRegularFile` a verify function that hashes through the
descriptor the tree opened without following links. The tree stamps the file
(size, mtime, ctime) before the check, compares the stamp again just before
setting the file aside under a hidden `.onedrive-go.<random>.detach` sibling,
proves the sibling is the verified inode with the same size and mtime (the
rename itself changes ctime), and only then moves it out. A file replaced or
written in place while it was being checked is put back with a no-replace link
and the delete is superseded, so the new content survives. A cross-filesystem
move copies through the verified descriptor and re-checks the stamp after the
copy, discarding a copy a concurrent write may have torn. That copy is made
durable before the set-aside original is unlinked: the temporary sibling is
fsynced, renamed to `item`, and the entry directory is fsynced too
(`copyThroughDescriptor`, through the `rootOps.syncDir` seam), because the
rename is a directory update that is not durable until the directory is;
unlinking the original first left a power cut with a trash entry holding only
a `.copy-*` file and no original anywhere. A directory sync the filesystem
cannot do (`localpath.IsDirSyncUnsupported`) is tolerated; any other failure
discards the copy and is reported, and the original is linked back. The
in-place check
after the set-aside rests on size and mtime alone (the rename changes ctime):
a writer that keeps both, such as `rsync --inplace -t` or a write through an
mmap that does not update mtime, can still slip a change past it.

The file does not vanish: it moves into the drive's local trash
(`internal/localtrash`, P-4), a per-mount directory under the data directory
named after the state DB. Each entry is recorded as pending before anything is
moved and as committed after, so a crash in between leaves a pending entry
that `trash list` shows and `trash restore` puts back; an entry holding a file
but no readable record is listed as an orphan with the file's location. An
entry directory holds a file when it has `item` or, failing that, any regular
file other than `entry.json` (`heldFileIn`): a `.copy-*` left by a
cross-filesystem placement an earlier build or a crash interrupted is listed
at its own path, is never purged or expired, blocks `Placement.Abandon`, and
is refused by `trash restore`, because it may be torn and the original was
left set aside in the sync folder. Purge expires committed entries after 30
days and only removes reservations whose directory holds no file at all; it
never deletes an uncommitted file. Restore never
that `trash list` shows and `trash restore` puts back (the crash point
`delete.local_trashed` sits exactly there, and `delete.remote_done` right after
a Graph DELETE succeeds; D-091, `spec/design/sync-engine.md`); an entry
holding a file but no readable record is listed as an orphan with the file's
location. Purge
expires committed entries after 30 days and only removes reservations whose
file never arrived; it never deletes an uncommitted file. Restore never
overwrites (renameat2 `RENAME_NOREPLACE` on Linux, `renamex_np RENAME_EXCL` on
macOS, link and unlink elsewhere, and on filesystems without hard links an
`O_EXCL` create plus copy, which is not atomic but never replaces), refuses a
symbolic link between the sync folder and the target, and copies, keeping mode
and mtime, through a `.onedrive-go-restore-*` sibling when the trash is on
another filesystem, and fsyncs the target's folder after the final rename
before the trashed original is removed (`finishCopiedRestore`, through the
trash's `syncDir` seam); observation never sees such a sibling. A file that
cannot be checked in the trash is never purged as empty. A
`.detach` sibling left by an interrupted run is never cleaned up: every full
scan reports it as `local_interrupted_removal`.

The previous version of a file a download replaces is kept the same way: just
before the finished download is renamed over its target,
`validateDownloadTargetBeforeReplace` verifies the target through the
descriptor it hard-links (or copies) into the trash. One-shot housekeeping and
the watch maintenance tick (at most hourly) purge, and `onedrive-go trash
list/restore/empty` exposes the trash. Without a trash (executor-only tests)
the verified inode is removed with `RemoveVerifiedRegularFile` instead.

Both download-target checks hash the existing file and compare it with
`expectedDownloadTargetHash`: the hash of the version the plan observed, or,
when the plan carries none, the baseline's local hash (R-2.2.5). Size, mtime
and identity alone never prove the file is the synced version, because an
in-place edit can keep all three; ctime is checked too
(`plannedCtimeChanged`), and the upload source precondition applies the same
ctime check, so a file rewritten after it was observed is re-observed instead
of transferred.

Directory delete also preserves non-disposable local content by refusing to
remove directories blocked by non-disposable children. Folder removal uses the
rooted `synctree.RemoveEmptyDirNoFollow` helper for the final delete: it
rechecks the directory after disposable cleanup, then uses an `rmdir` syscall
instead of generic file removal. A concurrent child creation or same-path
replacement therefore fails closed and leaves the new content in place for the
next observation pass instead of deleting content from a stale plan.

## Conflict Execution

The snapshot-based runtime does not execute abstract conflict rows. Conflict
handling is expanded before execution into concrete work: `ActionConflictCopy`
plus a dependent `ActionDownload` for keep-both cases, or `ActionUpload` for
edit/delete.

### Edit/edit and create/create

The executor preserves both versions:

1. execute `ActionConflictCopy` to rename the local canonical file to `<stem>.conflict-<timestamp><ext>`
2. execute the dependent `ActionDownload` back to the canonical path

The dependent download carries `RequireMissingLocalTarget`, so executor-side
local preconditions treat the post-copy missing canonical path as the expected
state. If the canonical path reappears before the dependent download runs or
before its final atomic rename, that is a stale local-write precondition and the executor returns
`errActionPreconditionChanged` instead of overwriting the newly appeared file.

If the download fails, the preserved local conflict copy remains on disk and
the canonical path stays pending for retry/replan. Execution does not recreate
an abstract conflict action to roll the pair back.

### Edit/delete

The planner expands local-edit vs remote-delete into a concrete `ActionUpload`
with no stale item ID. The executor then performs that upload as ordinary
create-by-parent work:

1. keep the local file in place
2. upload it to recreate the remote item

No conflict copy is needed for this case, and the executor does not invent the
upload from a stale local delete.

## Mount-Local Execution

For engines rooted below the remote drive root, execution uses the engine's own
mount context:

- `executorConfig.driveID`
- `executorConfig.remoteRootItemID`
- `Action.DriveID`

Path convergence checks after successful remote mutation are resolved relative
to that mounted subtree, not via per-action target-root overrides. Scope
classification likewise uses the action completion's authoritative `DriveID`.

Direct shared-item CLI flows still keep explicit target-scoped behavior above
sync through `driveops.SharedTargetClients(...)` and root-aware
`driveops.MountSession` path operations. That is a transfer/CLI boundary, not
ordinary sync execution.

## Scope Helpers

`active_scopes.go` is no longer a separate runtime subsystem. It provides pure
helper functions over an engine-owned slice of active block scopes.

Execution itself does not decide whether an action is blocked. The engine asks
those helpers before dispatch and after action completions.

Scope admission evaluates both the action's current path and, for moves, the
source `OldPath`. A move whose source subtree is blocked stays blocked even if
its destination path is outside the blocked subtree.

## Execution Boundary

Execution owns concrete action side effects and reports their outcomes. Planning
owns reconciliation decisions, baseline-only cleanup, conflict expansion, and
shared-folder runtime topology before work reaches the executor.

## Symlink Boundary Policy

`follow_symlinks=true` lets observation walk through a symlinked directory and
record content at the alias path. The executor treats that alias as a
**read boundary, not a write boundary** (R-2.4.6).

Every mutating local action asserts `validateNoSymlinkBoundary` before acting.
The check walks the target path one component at a time and refuses the action
if any ancestor is a symbolic link, reporting
`won't <verb> <path>: <ancestor> is a symbolic link, and sync never writes
through one` (local delete says "deletes") and classifying
the failure as `PermissionCapabilityLocalWrite` so it lands in the durable
blocked-scope projection and is explained by `status` rather than retried as a
transient error.

Covered actions: download, local move (both endpoints), conflict copy, local
folder create, and local delete. Local delete reaches the policy through
`ExecuteLocalDelete`, which special-cases the alias itself — deleting the alias
removes only the symlink and never target content.

Two deliberate exclusions:

- **Upload is not blocked.** Uploading reads through the alias, and observing
  plus uploading target content is the entire purpose of the setting. The
  upload source precondition follows the alias for its stat (see the upload
  paragraph above) instead of going through the rooted `synctree` capability,
  which would refuse an alias whose target is outside the sync root. Allowing
  that read opens no write path: every local mutation beneath the alias still
  hits `validateNoSymlinkBoundary` and, below it, `synctree` containment.
- **Reads are not blocked** for the same reason.

This policy is redundant with `synctree`, which already fails such calls closed
at the rooted descriptor, and that redundancy is intentional in exactly one
direction. Download is the case that needs it: `ExecuteDownload` resolves an
absolute path and hands it to `driveops`, which writes through the deliberately
uncontained `localpath` helpers, so the executor is the only layer that can
refuse it. For the remaining actions the executor check converts a
rooted-descriptor error into a stated policy with an actionable message and a
durable classification.
