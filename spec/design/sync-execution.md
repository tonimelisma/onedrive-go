# Sync Execution

GOVERNS: internal/sync/internal/syncexec/action_freshness.go, internal/sync/internal/syncexec/executor.go, internal/sync/internal/syncexec/executor_conflict.go, internal/sync/internal/syncexec/executor_delete.go, internal/sync/internal/syncexec/executor_parent_checks.go, internal/sync/internal/syncexec/executor_preconditions.go, internal/sync/internal/syncexec/executor_transfer.go, internal/sync/internal/syncexec/worker.go, internal/sync/internal/syncexec/executor_test_seams.go, internal/sync/internal/syncexec/doc.go, internal/sync/internal/syncsched/*.go

Implements: R-2.2.5 [verified], R-2.3.1 [verified], R-2.13.2 [verified], R-5.10.2 [verified], R-2.4.6 [verified], R-2.8.6 [verified], R-2.8.7 [verified], R-2.8.9 [verified], R-2.8.10 [verified], R-2.14.2 [verified], R-6.2.3 [verified], R-6.2.4 [verified], R-6.4.4 [verified], R-6.6.17 [verified], R-6.8.7 [verified], R-6.8.8 [verified], R-6.8.9 [verified], R-6.4.9 [verified], R-6.4.12 [verified], R-6.4.13 [verified], R-6.10.30 [verified]

## Overview

Execution takes a runtime plan, dispatches concrete side-effecting
work through a dependency graph, runs workers, and reports one
`ActionCompletion` per finished action. That runtime-plan handoff is
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
- Source of Truth: planner-produced `ActionPlan` plus the rooted capabilities injected into the executor
- Allowed Side Effects: sync-root filesystem mutation, Graph transfer calls, and store success commits through the engine
- Store Access: the worker pool and the freshness check see the store only as `ExecutorStore` (`action_freshness.go`): the reads that check an action is still fresh and the commit of its outcome. The engine hands `*SyncStore` over as one
- Mutable Runtime Owner: workers own only action execution. The engine owns
  runtime quiescence, held-work timing, admission, and dependency completion
  decisions above the worker pool.
- Error Boundary: Workers and executor helpers return concrete action outcomes and execution errors; the engine classifies those outcomes into retries, failures, scope transitions, and durable state changes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| The executor's and its worker pools' test seams are one `Hooks` value (`executor_test_seams.go`), passed to `NewExecutorConfig`; its zero value hashes with QuickXorHash, keeps the full detached store-access and outcome-commit bounds and makes every crash point a no-op (R-6.10.29). | `TestHooks_ZeroValueIsProduction` |
| The executor configuration is built whole from `ExecutorInputs` or refused, naming the first missing required input (R-6.10.30). | `TestNewExecutorConfig_RejectsEachMissingRequiredField` |
| A download replaces an existing local file only when its content hashes to the planned version, or to the baseline's local hash when the plan has no hash, so an unsynced edit that kept size and mtime survives (R-2.2.5). | `TestExecutor_Download_LocalContentDiffersFromBaselineIsSuperseded` |
| A remote move whose item already sits at the destination succeeds without a Graph call. | `TestExecutor_RemoteMove_ItemAlreadyAtDestinationSucceedsWithoutMoving` |
| Folder eTag churn from descendant changes neither supersedes a folder move nor conditions it, while a file's eTag mismatch still supersedes. | `TestExecutor_RemoteMove_FolderETagChurnFromChildrenDoesNotSupersede`, `TestExecutor_RemoteMove_FileETagMismatchStillSupersedes`, `TestRunOnce_LocalFolderMoveSurvivesFolderETagChurn` |
| Move outcomes publish observed or proven local facts, keep a pending remote edit visible, and folder creates record local filesystem identity. | `TestExecutor_LocalMove_PublishesSourceBaselineLocalHalf`, `TestExecutor_LocalMove_RemoteEditKeepsBaselineRemoteContent`, `TestExecutor_RemoteMove_PublishesObservedDestinationLocalTruth`, `TestExecutor_CreateLocalFolder_RecordsFilesystemIdentity`, `TestExecutor_CreateRemoteFolder_RecordsObservedLocalIdentity` |
| Edit/edit and create/create conflicts are handled immediately by preserving both versions with a local conflict copy and downloading the canonical remote version. | `TestExecutor_Conflict_EditEdit_KeepBoth`, `TestExecutor_Conflict_EditEdit_KeepBoth_ConflictCopyCollisionGetsSuffix`, `TestExecutor_ConflictDownloadFails_LeavesConflictCopy`, `TestConflictCopyPath_Normal`, `TestExecutor_DirectoryConflictCopy_VanishedSourceIsTheVanishedFileOutcome` |
| Planner-generated edit/delete uploads remain concrete execution work, while stale local deletes return a superseded precondition outcome so the engine replans instead of inventing new sync intent inside the executor. | `TestExecutor_Conflict_EditDelete_RecreatesRemoteFromLocal`, `TestExecutor_ConflictCopy_SameSecondCopiesTakeTheirOwnNames`, `TestExecutor_ConflictCopy_FailedRenameLeavesNoConflictCopyName`, `TestExecutor_ConflictCopy_LeavesExactlyOneCopyHoldingTheOriginal`, `TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition`, `TestExecutor_LocalDelete_NoBaselineHashAndChangedFile_PreservesLocalContent`, `TestExecutor_LocalDelete_NoBaselineHashAndUnchangedFile_Deletes`, `TestExecutor_LocalDelete_NoBaselineEvidence_PreservesLocalContent`, `TestEngineFlow_ProcessNormalDecision_SupersededRetiresSubtreeWithoutRetryOrSuccess` |
| Worker-start validation rejects already-submitted stale actions before executor side effects, while suspect local truth disables local-state-based rejection. Dependent uploads after planned remote moves tolerate move-produced eTag churn but still reject proven remote content drift, and executable actions without planner truth fail closed. | `TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution`, `TestWorkerStartFreshness_SuspectLocalTruthDoesNotSupersedeFromLocalState`, `TestActionFreshness_PostRemoteMoveUploadAllowsMoveProducedETagChange`, `TestActionFreshness_PostRemoteMoveUploadRejectsRemoteContentChange`, `TestActionFreshness_MissingPlannerViewFailsClosedForExecutableAction` |
| Local mutation is refused beneath a symlinked ancestor, at every mutating action, with the target content outside the sync root left byte-identical. | `TestExecutor_Download_SymlinkedAncestorIsBlocked`, `TestExecutor_LocalMove_SymlinkedAncestorSourceIsBlocked`, `TestExecutor_LocalMove_SymlinkedAncestorDestinationIsBlocked`, `TestExecutor_ConflictCopy_SymlinkedAncestorIsBlocked`, `TestExecutor_CreateLocalFolder_SymlinkedAncestorIsBlocked`, `TestExecutor_LocalDelete_SymlinkedAncestorReturnsStalePrecondition`, `TestExecutor_LocalDelete_AliasItselfStillRemovesOnlyTheSymlink`, `TestExecutor_LocalMove_WithoutSymlinkBoundarySucceeds` |
| Uploads read through a followed alias: a file beneath a directory symlink whose target is outside the sync root uploads with the target content, its followed facts are still checked against the plan, the same path still refuses a download, and a path that lexically leaves the root is refused. | `TestRunOnce_FollowSymlinks_UploadsFileInsideDirectoryAliasOutsideRoot`, `TestExecutor_Upload_UnderFollowedDirectoryAliasSucceeds`, `TestExecutor_Upload_UnderFollowedDirectoryAliasDetectsChangedTarget`, `TestExecutor_UnderFollowedDirectoryAlias_UploadReadsButDownloadStaysRefused`, `TestExecutor_Upload_PathLeavingSyncRootIsRefused`, `TestExecutor_Upload_SymlinkAliasSourceUsesFollowedTargetPrecondition` |
| Local deletes and download overwrites act only on the inode they verified, keep a file replaced mid-check, and put what they remove or replace in the drive's local trash. | `TestDeleteLocalFile_MovesTheFileToTheTrash`, `TestDeleteLocalFile_FileReplacedMidHashSurvives`, `TestDetachRegularFile_FileReplacedDuringVerifyIsKept`, `TestRunOnce_RemoteDeleteMovesLocalFileToTrash`, `TestRunOnce_DownloadOverwriteKeepsPreviousVersionInTrash`, `TestDetachRegularFile_InPlaceWriteAfterVerifyIsKept`, `TestCopyVerified_WriteDuringCopyDiscardsTheCopy`, `TestTrash_CrashBetweenMoveAndCommitIsRecoverable`, `TestTrash_FailedCommitLeavesARecoverablePendingEntry`, `TestTrash_OrphanItemIsListedAndKept`, `TestTrash_RestoreAcrossFilesystemsCopies`, `TestTrash_RestoreRefusesSymlinkedAncestor`, `TestRefuseSymlinkedAncestors_RefusesATargetItCannotPlaceInTheSyncFolder`, `TestTrash_RestoreRefusesAMissingSyncFolder`, `TestTrash_RestoreCreatesMissingFoldersThatFollowTheUmask`, `TestFullScan_LeftoverSetAsideFileIsReportedNotDeleted`, `TestTrash_CrossFilesystemRestoreKeepsModeAndTime`, `TestTrash_UncheckableItemIsNeverPurged`, `TestTrash_RestoreThroughASwappedFolderStaysInside`, `TestTrash_CrossFilesystemRestoreThroughASwappedFolderStaysInside`, `TestTrash_PutBackMakesNoFolderThroughASwappedFolder`, `TestRoot_MoveInNoReplace_CopyKeepsModeAndTime`, `TestRoot_RenameNoReplace_SourceThatCannotBeRemovedIsReportedAfterSuccess`, `TestTrash_RestoreThatLeavesItsTemporaryCopyFinishesAndNamesIt`, `TestTrash_RestoreThatLeavesItsTemporaryCopyAndThenFailsNamesBoth`, `TestTrash_RestoreThatFailsAfterPlacingTheFileReturnsWhereItIs` |
| A cross-filesystem trash placement or restore is durable before the source goes: the destination folder is fsynced after the rename, a sync failure discards the copy, and an entry left with only an interrupted `.copy-*` file is listed, never purged, and not restored as the file. | `TestCopyVerified_SyncsTheDestinationFolder`, `TestCopyVerified_DirectorySyncFailureLeavesNothingAtTheDestination`, `TestCopyVerified_ToleratesUnsupportedDirectorySync`, `TestTrash_CrossFilesystemRestoreSyncsTheTargetFolder`, `TestTrash_IncompleteCrossFilesystemCopyIsNeverPurged` |
| A recursive remote folder delete refuses to take descendants sync never saw or that changed online; a remote delete re-checks local absence and the sync folder identity; a local folder delete over only ignored children converges and the folder stays deleted online until it holds something sync sees. | `TestExecuteRemoteDelete_FolderWithUnobservedChildIsStale`, `TestExecuteRemoteDelete_FolderWithOnlySyncedUnchangedChildrenRuns`, `TestExecuteRemoteDelete_FolderChildChangedOnlyInETagOrSizeIsStale`, `TestExecuteRemoteDelete_FolderOverListingCapIsDurableIssueNotStale`, `TestRunOnce_LocalFolderDeleteKeepsRemoteChildAddedAfterObservation`, `TestExecuteRemoteDelete_LocalPathReappearedIsSuperseded`, `TestDeleteLocalFolder_OnlyIgnoredChildrenConverges`, `TestRunOnce_RemoteFolderDeleteWithIgnoredLocalFileConverges`, `TestRunOnce_FolderRecreatedWhereAKeptFolderWasIsCreatedOnline` |
| Through the engine, a folder whose live listing exceeds the cap is left on OneDrive, its delete recorded as a backed-off durable issue that the next pass neither relists nor retries; a child edited online after observation stops the folder delete and the edit comes back down; a folder back locally before its delete runs stays on both sides; and local absence still holds below a regular file and across a type change, so those remote deletes run. | `TestRunOnce_RemoteFolderDeleteOverListingCapIsADurableIssueNotRelistedEveryPass`, `TestRunOnce_RemoteFolderDeleteRefusesAChildEditedOnlineAfterObservation`, `TestRunOnce_RemoteFolderDeleteRefusedWhenTheFolderIsBackLocally`, `TestExecuteRemoteDelete_LocalAbsenceHoldsBelowAFileAndAcrossATypeChange` |
| A crash right after a local delete moved the file into the trash (its entry still pending) or right after OneDrive deleted an item, before either outcome commits, recovers: the trash entry is listed once and restores, nothing is deleted twice, re-uploaded or downloaded again (crash points `delete.local_trashed` and `delete.remote_done`, D-091). | `TestCrashRecovery_DeleteBoundariesDeleteOnceAndKeepTheTrashEntry`, `TestCrashRecovery_EveryCrashPointConverges` |
| A crash between the two steps of a local move that replaces its destination, after the destination's file went to the trash and before the rename, recovers: the next plan finishes the move with no delete on OneDrive, no conflict copy and no upload, and the displaced file stays in the trash once (crash point `exec.local_move_destination_trashed`, R-2.17.9). | `TestCrashRecovery_ReplacingMoveBetweenItsTwoStepsFinishesTheMove` |
| Executor live preconditions reject stale work at the side-effect boundary without mutating local or remote state. | `TestExecuteRemoteDelete_NotFoundPreflightReturnsStalePreconditionAndDoesNotDelete`, `TestExecuteRemoteDelete_ETagMismatchPreflightReturnsStalePreconditionAndDoesNotDelete`, `TestExecuteRemoteDelete_TransientPreflightFailureIsOrdinaryFailure`, `TestExecutor_RemoteDelete_UsesConditionalETagFromPreflight`, `TestExecutor_RemoteDelete_ConditionalMismatchReturnsStalePrecondition`, `TestExecutor_RemoteDelete_WrongDrivePreflightReturnsStalePrecondition`, `TestExecutor_RemoteDelete_StalePathPreflightReturnsStalePrecondition`, `TestExecutor_RemoteMove_StaleSourcePreflightReturnsStalePrecondition`, `TestExecutor_RemoteMove_UsesConditionalETagFromPreflight`, `TestExecutor_RemoteMove_ConditionalMismatchReturnsStalePrecondition`, `TestExecutor_CreateRemoteFolder_MissingParentPreflightReturnsStalePrecondition`, `TestExecutor_Upload_SourceHashChangedBeforeTransferReturnsStalePrecondition`, `TestExecutor_Download_TargetAppearsBeforeRenameReturnsStalePrecondition`, `TestExecutor_ConflictDownload_TargetReappearsAfterConflictCopyReturnsStalePrecondition`, `TestExecutor_Download_MountRootAllowsGraphDriveRootPath`, `TestExecutor_LocalMove_MissingDestinationParentIsStale`, `TestExecutor_LocalMove_LocallyEditedSourceStillMovesAndKeepsTheEditPending`, `TestExecutor_LocalMove_FolderIdentityChangedReturnsStalePrecondition`, `TestExecutor_LocalDelete_FolderIdentityChangedReturnsStalePrecondition`, `TestExecutor_LocalDelete_SymlinkedAncestorReturnsStalePrecondition` |
| On a byte-exact filesystem every local operation addresses an existing entry by its spelling on disk: an NFD-named file uploads in one pass, a remote edit replaces it in place, a remote delete, rename or move removes or moves it, a conflict copy preserves it, an NFD-named folder receives remote additions inside itself and is deleted, a remote delete is superseded when the NFD-named file is back, and the permission probe reports the stored boundary (R-2.13.2). | `TestRunOnce_LocalNFDNamedFileUploadsInOnePass`, `TestRunOnce_RemoteEditReplacesTheNFDNamedLocalFile`, `TestRunOnce_RemoteDeleteRemovesTheNFDNamedLocalFile`, `TestRunOnce_RemoteMoveMovesTheNFDNamedLocalFile`, `TestRunOnce_ConflictingEditsOfAnNFDNamedFileKeepBoth`, `TestRunOnce_NFDNamedLocalFolderReceivesAdditionsAndIsDeleted`, `TestExecuteRemoteDelete_NFDNamedLocalFileBackIsSuperseded`, `TestPermHandler_HandleLocalPermission_ProbesTheNFDNamedFolder` |
| Workers preserve executor failure capability on completions and record worker-start/live-precondition superseded counters by local-vs-remote source. | `TestWorkerStartFreshness_LocalUploadMismatchIsSupersededBeforeExecution`, `TestWorkerStartFreshness_RemoteDownloadMismatchRecordsRemoteTruthCounter`, `TestWorkerPool_SendResultCountsLivePreconditionSupersededByCapability`, `TestWorkerPool_SendResultDoesNotGuessLivePreconditionSourceWithoutCapability` |
| A worker commits a successful action's outcome on a context detached from the pool's cancellation and bounded by `OutcomeCommitTimeout`, so a shutdown landing between the transfer and its commit still records the finished work, and after the cancel it still hands the engine every completion its channel has room for, so the stopping run's drain records that result too; one it has no room for is dropped and said at Debug. | `TestWorkerPool_CommitsCompletedOutcomeAfterPoolCancellation`, `TestWorkerPool_AStoppedPoolDeliversEveryCompletionItHasRoomFor`, `TestWorkerPool_ACompletionDroppedAfterTheStopIsLogged` |
| Publication-only planner actions commit baseline mutations without worker dispatch and release dependents through the engine-owned publication-drain stage. | `TestPublicationMutation_SyncedUpdate`, `TestPublicationMutation_SyncedUpdate_BaselineFallback`, `TestPublicationMutation_Cleanup`, `TestPublicationMutation_Cleanup_FolderType`, `TestRunPublicationDrainStage_DoesNotReleaseUnrelatedHeldWork` |
| Watch-mode replan keeps old-runtime work out of dispatch once it is no longer current and preserves dirty intent across recoverable local-observation failure. | `TestWatchRuntime_RunNonDrainingWatchStepPrioritizesReadyReplanOverDispatch`, `TestWatchRuntime_QueuePendingReplanRetiresOldOutbox`, `TestWatchRuntime_PendingReplanRetiresDependentsReleasedByRunningAction`, `TestWatchRuntime_PendingReplanLocalObservationFailureReschedulesDirtySignal`, `TestWatchRuntime_IdleReplanLocalObservationFailureReschedulesDirtySignal` |

## Worker And Dependency Model

The executor configuration and each worker pool are built in one call:
`NewExecutorConfig(ExecutorInputs)` and `NewWorkerPool(WorkerPoolInputs)`.
The configuration's required inputs (item client, downloader, uploader,
transfer manager, sync tree, drive, logger, clock) are refused when zero,
naming the field (R-6.10.30, `TestNewExecutorConfig_RejectsEachMissingRequiredField`);
nothing changes either after it is built, so no worker can see a
configuration half set up. The engine builds the configuration once in
`newEngine`, and a pool per pass (one-shot) or per watch session, passing its
perf collector and its serialize test seam into the pool's inputs.

`DepGraph` is the execution-time dependency graph. It tracks:

- which actions are in flight
- which dependencies remain unsatisfied
- which dependents become ready after completion

The graph has no lock: only the goroutine running the engine's `RunOnce` or
`RunWatch` calls it. Workers report outcomes on the completion channel and
never call the graph (`TestWorker_NeverCallsComplete`); a test-installed owner
check on every method fails a call from another goroutine (R-6.3.4).

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
`ErrActionPreconditionChanged`, so the engine classifies it as superseded
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
the main `PathView`, so a changed local source, a remote move source the moved
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
their failure capability through `ActionCompletion`, so the worker boundary can
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

A create upload records its intent at the path before the request, and the
new item as a pending create through `UploadOpts.OnCreated` before any
content is sent (R-5.6.15). The hook also runs for an item a failure names
(a large file's session request that failed after its create,
`TestSessionUpload_FailedSessionRecordsTheItemTheCreateMade`), and it writes
on a context detached from the action's, bounded by `OutcomeCommitTimeout`,
so a shutdown right after the create cannot drop the record
(`TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled`). An
attempt known to have created nothing clears the intent, and so does one
that succeeded without the hook (a zero-byte file is the created item), on a
context detached from the action's and bounded by `OutcomeCommitTimeout`, so
a shutdown landing as it succeeded does not leave the intent of a synced file
(`TestUploadNewFile_ZeroByteCreateLeavesNoPendingRow`). When the cleanup
after a failure reports the created item removed
(`graph.CreatedItemError.Removed`), the attempt drops the item's record the
same way: the item can never receive the content
(`TestUploadNewFile_RemovedPlaceholderLeavesNoRecord`). Two kinds of attempt
are not known to have created nothing: one that was canceled before it
succeeded, whose create may still land after the client stopped waiting
(`TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly`), and one whose
create ended without an answer that says whether it landed
(`graph.ErrCreateOutcomeUnknown`: no response, a 5xx, a 408, a success whose
item could not be read). That is the window the intent covers, so it is kept
until observation or a retry claims it, or the step after planning drops it:
once it is past `pendingCreateIntentTTL`, and, after a pass that observed
OneDrive before the local tree (a one-shot sync, a watch bootstrap), once a
complete local scan finds no file at its path, since no create there will
be retried. A watch replan observes the local tree only and keeps a fresh
intent, so the change batch that shows a landed create still claims it; the
watch loop drops it once a remote poll that began after the attempt ended has
been applied and the file is gone
([sync-store.md](sync-store.md), Pending creates;
`TestRunOnce_CreateIntentOfAFileRenamedAwayIsDropped`,
`TestWatchRuntime_ReplanKeepsAFreshCreateIntentForTheBatchThatShowsItsFile`,
`TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll`). A
failed lookup of an upload's created item keeps the create mark, so its 404
is never read as the create's parent being gone
(`TestExecuteUpload_LookupFailureAfterACreateIsNotAGoneParent`).
Kept for good, an intent whose file was renamed away could claim an empty
file a stranger made at that path, and the next pass then deleted it.
Before, the unanswered create cleared its intent, and when it had in fact
landed the next pass read the empty file as someone else's: the user's file
was renamed aside as a conflict copy and the empty one downloaded (live,
2026-11-11, during a network outage;
`TestRunOnce_CreateWhoseAnswerWasLostFinishesIntoItsOwnEmptyFile`).
`uploadNewFile` checks the unknown outcome before the conflict, and reads the
path's row before it records the attempt's own intent, so it knows whether an
earlier attempt left one (`priorCreateIntent`: no item ID, within
`pendingCreateIntentTTL`).

A 409 on the create is resolved, not retried as is (`resolveCreateConflict`).
One function reads the folder's listing and says what holds the name
(`classifyCreateConflict`, by `namekey.Find`; a child without an item ID is
no candidate and a repeated child counts once): the item the create wanted
(the same OneDrive name and the same stored form), a rival (the same OneDrive
name in another stored form), or nothing it can name. A sibling that only
shares the stored form is another name for OneDrive and is never the wanted
item (R-2.12.3, `TestClassifyCreateConflict_NeverAdoptsATwin`). Two rivals are
a rival, the first listed named in the message; two children of the wanted
tier, which OneDrive never lists, are nothing. By that answer the 409 is
resolved:

- into our own placeholder from an interrupted attempt: upload by ID with
  If-Match on the eTag just read while it is still an empty live file (a
  folder, a deleted item or a package of size 0 is none) at the create's own
  path: the name in NFC, under the create's parent (`placeholderIsAtCreate`;
  the drive root's alias `root` names no item, so an item read back for a
  create there is placed by its parent path, and only a location known to be
  elsewhere refuses it). An item renamed or moved online before observation
  took its record along would otherwise receive the user's file under
  another name or in another folder
  (`TestFinishIntoPlaceholder_RefusesAnItemThatIsNotAnEmptyLiveFile`). The
  creation eTag does not work, since a canceled upload session moves it
  (`TestRunOnce_UploadOnlyCreateFinishesIntoItsPlaceholderAfterItsETagMoved`);
- into the file of a create whose answer was lost: only with a prior intent,
  and only when the folder lists a file of exactly this spelling, live and
  empty. Its ID and eTag are recorded as the pending create
  (`claimLostCreate`), then it is finished the same way, an empty local file
  included (`TestUploadNewFile_CreateWhoseAnswerWasLostIsFinishedByTheRetry`).
  Content written between the listing and the upload is never overwritten:
  the read by ID sees it, or the If-Match refuses it
  (`TestUploadNewFile_ClaimNeverOverwritesContentWrittenMeanwhile`);
- as a durable name collision when OneDrive holds a differently spelled
  sibling of the same name (`RemoteNameTakenError`, classified as
  `case_collision` with backoff, naming both names;
  `TestRunOnce_UploadOnlyCreateOntoADifferentlySpelledRemoteNameIsADurableCollision`);
- as a stale precondition otherwise. A 409 without a prior intent, or on a
  file with content, a folder or another spelling, claims nothing and writes
  nothing (`TestUploadNewFile_ConflictNeverClaimsAFileThatIsNotItsOwnEmptyCreate`).
  When nothing of that name is listed yet, a prior intent is kept for the
  next retry (`TestUploadNewFile_ConflictOnAFileNotListedYetKeepsThePriorIntent`).
  The stale precondition keeps the 409 in its error chain, and the engine
  supersedes it all the same and replans in the same run, which finds a name
  the listing did not show yet
  (`TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt`).

A read that fails while resolving the 409 (the pending-create row, the read
of the placeholder, the folder listing, the record of a claim) is that read's
failure, classified by its own answer: a throttled or failing listing proves
nothing about the plan. Joined to the 409, it read as a stale precondition
under the 409's status
(`TestExecutor_Upload_ListingFailureAfterACreateConflictIsClassifiedByItsOwnAnswer`,
`TestFinishIntoPlaceholder_AReadThatFailsIsThatReadsFailure`; a placeholder
that is gone is no placeholder, and the listing decides).
The same holds for the lookup of an upload whose final fragment may have
committed: driveops reports its failure as an unknown outcome carrying the
lookup's error only, so a final fragment's 409 or 412 before it does not make
`uploadTransferError` read it as stale
(`TestUploadTransferError_AFailedOutcomeLookupIsNotAStalePrecondition`).

A remote folder create resolves its 409 the same way (`adoptExistingRemoteFolder`): a
folder of the wanted name is adopted, a twin never is (beside a rival the name
is taken, alone it is a stale precondition;
`TestExecutor_CreateRemoteFolder_ConflictBesideATwinAdoptsNothing`), a rival
is the same durable collision (as a stale precondition it was
replanned and refused on every pass;
`TestRunOnce_UploadOnlyFolderCreateOntoADifferentlySpelledRemoteNameIsADurableCollision`,
`TestExecutor_CreateRemoteFolder_ConflictWithAnotherSpellingIsADurableCollision`),
and a listing that fails is the listing's failure, not recorded under the 409
(`TestExecutor_CreateRemoteFolder_ListingFailureAfterAConflictIsClassifiedByItsOwnAnswer`).

A spelling-only local move (the old and new names are one OneDrive name:
case or Unicode form) accepts a destination that is the source's own inode,
which is what a case- or normalization-insensitive volume reports. Like every
local move it first renames without replacing; only when that rename finds the
destination taken and the destination is the source itself, as on a volume
that folds case, does it rename plainly. A different entry made at the new
spelling after the check is kept and the move superseded: the two names'
entries are compared, and a different file is not the source
(`TestExecutor_LocalMove_DestinationTakenAfterTheCheckIsKept`,
`TestExecutor_RenameLocal_SpellingRenameOntoAnotherFileKeepsBoth`). On a volume
that tells the spellings apart, two hard links of one file named in both
spellings pass that check too, and the plain rename leaves both names. When
the directory still holds the old spelling after the rename (the entry
spelled as the move asks, else the only entry of that OneDrive name, by
`namekey.Find`; two such entries and none spelled so fail the move with an
error naming the folder, which the user sees, instead of renaming one of them
by listing order: `TestExecutor_StoredNameSpelling_TwoKeyEqualEntriesAreAnError`), the move goes
through a temporary sibling with two renames that never replace; the sibling
is named `.onedrive-go.spelling.<name>` (then `<name>-1` up to `<name>-9`
while those are taken, `syncdomain.SpellingRenameArtifactAttempts`), which
local observation always excludes and cleanup never deletes, so a crash
between the two renames leaves nothing that syncs (R-2.12.4,
`TestContentFilter_SpellingRenameArtifactIsAlwaysExcluded`,
`TestExecutor_LocalMove_SpellingOnlyRenameOntoSameInodeIsAllowed`). What it
leaves is the user's entry, so the engine's next live start puts it back
under its plain name before the first plan, through the no-replace rename,
and keeps it, warning with both paths, where that name is taken or the
temporary name does not say which it was (R-2.12.8, sync-engine.md,
`TestRunOnce_SpellingRenameLeftoverIsPutBackBeforeThePlan`); the rename is
then planned again from truth. Accepting it
is what lets a remote case-only rename apply on macOS's case-insensitive volume.
A failure of either step through the temporary sibling, the second finding the
new spelling taken included, is the rename's own failure, not a superseded
move (`TestExecutor_LocalMove_SpellingRenameWhoseSecondStepFindsTheNameTakenFails`).
A name left behind once the entry has its new spelling is a rename that
happened, logged with that name
(`TestExecutor_RenameLocal_SpellingRenameThatLeavesItsTemporaryNameIsDone`).

A local move checks that its destination is absent and then renames with the
sync tree's no-replace rename (`synctree.Root.RenameNoReplace`), which never
replaces an entry at the destination. The check turns an occupied destination
into a stale precondition early; an entry that takes the destination after
the check is kept, the rename refuses, and the move ends the same way,
superseded and replanned from what is there
(`TestExecutor_LocalMove_DestinationTakenAfterTheCheckIsKept`). A file moves
without a window wherever the system has the flag (`RENAME_NOREPLACE` on
Linux, `RENAME_EXCL` on macOS) or hard links: by the flag, or by a link and
an unlink where the flag is refused. A folder has no hard link, so where the
flag is refused (FreeBSD, which the build supports, or a filesystem that
refuses it) the rename checks the destination on its folder's descriptor and
then renames, and so does a file on a filesystem that has neither the flag
nor hard links. That check and rename is the one window left, which only
something outside this program can fill: the planner chose the destination,
the executor found it absent, and the dependency graph serializes it against
other actions on that path. A move whose link and unlink placed the entry but
could not remove its old name (`synctree.ErrSourceNotRemoved`) happened, and
the name left is logged; that name is a second name of the same file, which a
later pass sees as a new local file
(`TestExecutor_LocalMove_OldNameThatCannotBeRemovedIsAMoveThatHappened`).

A move that replaces its destination (D-058: OneDrive moved an item over a
file it deleted, whose local copy is the synced version) makes way first by
the local delete rule: the file at the destination leaves the sync folder
only while it is the version the plan saw, verified through the descriptor
of the inode that leaves, into the trash when the drive has one and removed
otherwise. The rename then finds the name free and replaces nothing, and the
displaced file is moved once, or copied through its verified descriptor
when the trash is on another filesystem
(`TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination`). A file that
takes the freed name between the two steps is kept: the rename refuses, the
move is superseded, and the displaced file stays in the trash
(`TestExecutor_LocalMove_ReplacingMoveKeepsAFileThatTakesTheFreedName`); the
next plan renames that file to a conflict copy and finishes the move
(`TestRunOnce_ReplacingMoveFindingANewFileAtTheFreedNameStillMoves`). A
trash that fails fails the move, with both files where they were, and never
reads as a destination that left on its own
(`TestExecutor_LocalMove_ATrashThatFailsFailsTheReplacingMove`). A
crash between the two steps (crash point `exec.local_move_destination_trashed`)
leaves the destination absent, its old content in the trash under a pending
entry and the source at its old path, with nothing recorded; the next plan
finishes the move as an ordinary one, with no delete on OneDrive and no
conflict copy (`TestCrashRecovery_ReplacingMoveBetweenItsTwoStepsFinishesTheMove`).

A conflict-copy collision is reachable by design: the destination name carries
a one-second timestamp, so two copies resolved in the same second are handed
the same name by construction, and no amount of care in the planner prevents
it.

A conflict copy takes its destination with that rename instead of finding a
free name first. The readable part of the name is the timestamp, but
uniqueness cannot come from statting candidates and then renaming onto the
winner: rename silently replaces whatever occupies the destination, so
anything that appeared between the check and the act would be destroyed by
the operation whose entire purpose is to preserve a file. Two conflict copies
resolved within the same second are the reachable case, and mounts sharing a
tree or the user creating the name are others.

The executor therefore renames the file or folder onto each candidate with
`RenameNoReplace` and treats `ErrTargetExists` as a taken name, walking the
ordinal suffixes (at most 100, and a copy that finds all of them taken says
so with the count and the second they share,
`TestExecutor_ConflictCopy_EveryNameTakenSaysHowManyAndWhichSecond`) until a
rename succeeds; a file's candidate
keeps its extension last, a folder's keeps a dot in its name in the stem.
Nothing is created before the rename, so a copy that fails leaves no name
behind, and the entry itself moves, never a copy of it, so a program writing
to the file keeps writing to the conflict copy. A rename that placed the entry
but could not remove the source (`ErrSourceNotRemoved`) is the success it is,
logged with the name left behind
(`TestExecutor_ConflictCopy_OriginalNameThatCannotBeRemovedIsASavedCopy`). A
rename that fails as missing is a vanished
source only when a lstat through the sync tree finds the source absent; the
missing name can be the copy's folder, and a lstat that fails another way is
joined to the failure (`TestExecutor_ConflictCopy_TakenNameMovesOnToTheNextOrdinal`,
`TestExecutor_ConflictCopy_WithoutTheFlagOrLinksRenamesTheEntryItself`,
`TestExecutor_ConflictCopy_MissingDestinationFolderIsNotAVanishedSource`,
`TestExecutor_ConflictCopy_UncheckableSourceAfterAMissingRenameKeepsBothReasons`).
Every candidate, ordinal and extension included, is built to fit the 255-byte
name limit (R-2.11.11, `TestExecutor_SecondSameSecondConflictCopyOfALongNameIsWritable`),
so the second same-second copy of a long name is as writable as the first.

The dependency graph is dependency-only: it does not define runtime
quiescence. Held retry/scope work intentionally keeps exact nodes unresolved,
so the engine decides when the current runtime is quiescent based on outbox,
running work, and due held entries. `DepGraph` therefore does not expose a
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
(`SymlinkBoundaryForPath`, a rooted no-follow walk), the source is stated by
following links, as observation did; otherwise it is stated through the rooted
no-follow `synctree.Lstat`. The rooted capability is deliberately not asked
about a followed source, because it refuses every name that resolves outside
the sync root, which is right for a write and wrong for this read: an alias to
another disk would otherwise never upload. The lexical containment check
(`synctree.Abs`) still runs first. The transfer manager then reuses the executor-supplied
callback before upload reads so a large file can abort if it changes
mid-session. Expected source-hash mismatches are returned as
`ErrActionPreconditionChanged`.

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
an unexpectedly occupied destination before their rename, which itself never
replaces one (local move, above).

For remote deletes, moves, downloads, overwrite uploads, and remote folder/file
creates, the executor performs a live `GetItem` preflight for the planned
source item or parent. Parent-based remote creates first wait for the
already-planned parent path to become visible through the convergence boundary,
then run the stale parent preflight so transient post-create Graph visibility
lag is absorbed before stale proof is evaluated. If the visibility wait times
out because the parent is actually gone, the stale parent preflight result takes
precedence over the visibility timeout. Both parent reads are made once per
parent per plan (`remoteParentChecks`, cleared when the engine registers a
plan): the first create into a folder waits for its path and reads the item,
its siblings skip both, and a create into a parent gone since then is answered
404 by Graph, which `remoteCreateParentGone` reports as the same parent-gone
fact, so the engine forgets the item and replans as it would after the live
read. A folder's own post-create visibility probe primes the memo for its
children. A file upload does not probe its own path afterwards: the upload's
response carries the item, and sync reads no file by path. Each of those three
reads cost one request per new file, half of the traffic of a 6,000-file
upload in the soak of 2026-11-02
(`TestExecutor_Upload_SiblingCreatesReadTheirParentOncePerPlan`,
`TestExecutor_Upload_CreateIntoAParentGoneMidPlanIsAParentGoneFact`,
`TestExecutor_CreateRemoteFolder_IntoAParentGoneMidPlanIsAParentGoneFact`,
`TestExecutor_Upload_DoesNotProbeItsOwnPathAfterTheUpload`). A live `itemNotFound` result, changed item identity,
drive, item type, eTag, hash, size, or same-coordinate path where Graph supplies
that fact is a stale precondition and maps to `ErrActionPreconditionChanged`.
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
work for that exact old action. An overwrite upload is conditional on the
preflight eTag the same way (R-5.6.2): its 412, a create's 409 resolved as
stale and an invalidated upload session are stale preconditions with
remote-write capability (`uploadTransferError`; a changed upload source stays
local-read). Their error keeps the Graph answer for the log, and the engine
classifies a stale precondition before any status it carries, so the action is
superseded and replanned in the same run
(`TestRunOnce_OverwriteRefusedWithA412IsReplannedInTheSameRun`).

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
`refuseLiveDatabaseTarget` returns `ErrDatabaseInUse` when the target has a
non-empty `-wal` or `-journal` sibling, whether or not the target itself exists yet, both before the transfer and again in
`ValidateTargetBeforeRename`. It is the retryable per-file condition
`local_database_open`, not a superseded action, because superseding would
replan the same download at once. Only a sidecar that does not exist is "no
sidecar": one that cannot be checked (permission denied, I/O error) refuses the
replacement too, as an ordinary precondition failure naming the sidecar rather
than `ErrDatabaseInUse`, because the download cannot prove the database closed
(R-2.4.15, `TestExecutor_Download_RefusesToReplaceDatabaseWithLiveWAL`,
`TestExecutor_Download_DatabaseOpenedDuringDownloadIsNotReplaced`,
`TestExecutor_Download_RefusesNewDatabaseBesideALiveWAL`,
`TestExecutor_Download_UncheckableSidecarRefusesReplacement`).

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

### Local folder creates

A local folder create (`createLocalFolder`) makes the folder, and any parent
it is missing, through the rooted sync tree, which takes no mode from it:
`synctree.Root.MkdirAll` asks for `localpath.UserTreeDirPerm`, 0777, and the
umask applies, as for every folder the client creates in a user's tree and
for the files downloaded into it (R-5.10.2). It was 0755 whatever the umask,
so under a umask of 002 the downloaded files were group-writable and their
folders were not. The sync folder itself is not created here, and an
existing folder is never chmodded
(`TestExecutor_CreatedFolderFollowsTheUmask`).

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
  destination is what a replacing move takes out of the way), a conflict
  copy's source, and a remote delete's local-absence check;
- a new entry (`SpellingOfParent`): a download of a new file, a local folder
  create and a conflict copy's name are written under the stored name inside
  their parents' spellings on disk, and the new entry's own folder is never
  listed for it, so a large initial download does not read its folder once
  per file.

The parent engine's shortcut-alias lifecycle (`engine_shortcut_root_lifecycle.go`)
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
- a content hash that no longer matches the baseline is a proven change
  (`LocalContentChangedError`), which its stat facts may not show (a damaged
  block keeps size, times and identity): beside superseding the delete, the
  engine clears that baseline row's stored change time
  (`SyncStore.ForgetBaselineLocalCtime`, in `forgetLocalCtimeProvenChanged`
  next to the remote-item-gone record), so the next scan reads the file
  instead of trusting the row (R-2.2.4) and plans the local edit it is, a
  re-upload beside a remote delete, rather than the same delete until it is
  parked (`TestRunOnce_ALocalDeleteRefusedForAnUnseenChangePlansTheLocalEditNext`).
  A refusal for a row with no hash, verified by size and mtime only, proves
  nothing about content and records nothing

Verification and removal are bound to one inode (D-010). `DeleteLocalFile`
hands `synctree.DetachRegularFile` a verify function that hashes through the
descriptor the tree opened without following links. The tree stamps the file
(size, mtime, ctime) before the check, compares the stamp again just before
setting the file aside under a hidden `.onedrive-go.<random>.detach` sibling,
proves the sibling is the verified inode with the same size and mtime (the
rename itself changes ctime), and only then moves it out. A file replaced or
written in place while it was being checked is put back with a no-replace link
and the delete is superseded, so the new content survives; on a filesystem
that cannot make the link (`localpath.IsLinkUnsupported`) the put-back is a
rename that replaces nothing, with the system's no-replace flag or a check that
the name is free and a plain rename, instead of leaving the file under its
hidden name (`TestDetachRegularFile_PutsTheFileBackWithoutHardLinks`).

The trash side of a take-out is reached the way the sync folder is: through
one descriptor. Before the file is opened, the folder of the trash entry's
`item` is opened once, as a folder and without following a symbolic link in
its own name (`openDestination`), and whether it lies outside the sync folder
is decided on that descriptor, by the device and inode of the folder and of
each folder above it reached by `..` (`refuseFolderWithinRoot`, R-6.4.8),
never by a path's text. The move out, a preserve's link, a copy's create and
rename, and the removal of a stale or torn copy are all made on that
descriptor and the base name `item`, and none replaces an entry there: the
move and the copy's rename take the system's no-replace flag, or where the
filesystem refuses it a check that the name is free and a plain rename, and a
link fails on a taken name. A folder swapped for a link into the sync folder
after the check is therefore never followed: the step is made in the folder
that was opened, which is gone, the take-out fails as a destination that
failed, and the file is at its name with its content
(`TestDetachRegularFile_ADestinationFolderSwappedForALinkIsNotFollowed`,
`TestDetachRegularFile_ADestinationFolderThatIsALinkIsNotFollowed`,
`TestDetachRegularFile_ANameTakenAtTheDestinationIsNotReplaced`). A
cross-filesystem move copies through the verified descriptor and re-checks
the stamp after the copy, discarding a copy a concurrent write may have torn
(`TestDetachRegularFile_CopiesAcrossFilesystems`). That copy is made durable
before the set-aside original is unlinked: the temporary `.copy-*` sibling is
created exclusively, fsynced, renamed to `item`, and the entry directory is
fsynced too (`copyThroughDescriptor`, through the `rootOps.syncDirFile` seam),
because the rename is a directory update that is not durable until the
directory is; unlinking the original first left a power cut with a trash entry
holding only a `.copy-*` file and no original anywhere. A directory sync the
filesystem cannot do (`localpath.IsDirSyncUnsupported`) is tolerated; any
other failure discards the copy and is reported, and the original is linked
back. A trash that fails, its entry's folder gone (the trash emptied
meanwhile) or a link, or the move or copy into it refused or finding `item`
taken, is `synctree.ErrDestinationFailed`, its cause named as text, and never
reads as a file already gone: the delete fails and the file stays, instead of
the delete being superseded as one whose file is already absent
(`TestDeleteLocalFile_ATrashThatFailsIsAFailureNotAFileAlreadyGone`,
`TestDetachRegularFile_ADestinationThatFailsIsNotAFileThatIsGone`). A trash
entry that cannot be reserved is named as text the same way, so a reservation
whose folder went missing never reads as a file already gone in a delete, or
as a destination that left on its own in a replacing local move
(`TestDeleteLocalFile_AReservationThatFailsIsAFailureNotAFileAlreadyGone`,
`TestExecutor_LocalMove_AReservationThatFailsFailsTheReplacingMove`). The
in-place check
after the set-aside rests on size and mtime alone (the rename changes ctime):
a writer that keeps both, such as `rsync --inplace -t` or a write through an
mmap that does not update mtime, can still slip a change past it.

The file does not vanish: it moves into the drive's local trash
(`internal/localtrash`, P-4), a directory under the data directory named after
the drive's state DB (`DirForStatePath`). Each entry is recorded as pending before anything is
moved and as committed after, so a crash in between leaves a pending entry
that `trash list` shows and `trash restore` puts back; an entry holding a file
but no readable record is listed as an orphan with the file's location. An
entry directory holds a file when it has `item` or, failing that, any regular
file other than `entry.json` (`heldFileIn`), and an entry directory that is
gone holds nothing, so abandoning a reservation whose folder was removed
succeeds (`TestPlacement_AbandonOfAnEntryWhoseFolderIsGoneSucceeds`). A
`.copy-*` left by a
cross-filesystem placement an earlier build or a crash interrupted is listed
at its own path, is never purged or expired, blocks `Placement.Abandon`, and
is refused by `trash restore`, because it may be torn and the original was
left set aside in the sync folder. Purge expires committed entries after 30
days and only removes reservations whose directory holds no file at all; it
never deletes an uncommitted file. The crash point `delete.local_trashed`
sits exactly between the move and the commit, and `delete.remote_done` right
after a Graph DELETE succeeds (D-091, `spec/design/sync-engine.md`). Restore
changes the sync folder only through its rooted capability (`putBack`,
R-6.4.8): the trash lives under the data directory, outside every sync
folder, and is reached by path, while the target's missing folders, the move
and any copy go through `synctree`. It never overwrites (the sync tree's
no-replace rename, `spec/design/system.md`: renameat2 `RENAME_NOREPLACE` on
Linux, `renameatx_np RENAME_EXCL` on macOS, link and unlink elsewhere, and on
filesystems without hard links an `O_EXCL` create plus copy, which is not
atomic but never replaces, all on folder descriptors: the target's folder
opened through the root, the trash entry's folder, outside every sync folder,
by path; a copy's final rename where the sync folder offers neither the flag
nor hard links is R-6.4.8's one exception), refuses a symbolic link between
the sync folder and the target up
front with a message that names it (a link swapped in after that check fails
the rooted move instead of redirecting it), refuses a sync folder that is
gone (making it again would give the drive a new folder its sync refuses as
not the one its state belongs to), creates the file's missing folders without
following a symbolic link, even one that stays inside the sync folder
(`MkdirAllNoFollow`), as folders in the user's tree
(`localpath.UserTreeDirPerm`, 0777 minus the umask, R-5.10.2), and copies,
keeping mode and mtime, through a `.onedrive-go-restore-*` sibling created
exclusively through the root when the trash is on another filesystem, with
the times set through the root on the copy's name (`Chtimes`, to the
nanosecond) and the final name taken by the no-replace rename; observation
never sees such a sibling. Once the file is at its target, `Restore` returns
the target with whatever fails after, and ends the entry in one order, each
failure stopping it there: a copy's folder is fsynced after the rename that
named the copy (`finishCopiedRestore`, through the trash's `syncDir` seam),
then the trashed file is removed, then the entry's record and directory. A
folder that cannot be synced keeps the entry whole (`ErrRestoreNotDurable`),
and a trashed file that cannot be removed keeps its record, so the entry
stays listed; restoring either again finds the file in the way
(`ErrRestoreTargetExists`). A sibling that cannot be removed is named
(`ErrRestoreLeftover`). `trash restore` prints a restore that put the file
back and then failed as restored, with what failed on stderr and in the
`warning` field of its `--json` document, and exits 1
(`reportTrashRestore`). A file that
cannot be checked in the trash is never purged as empty. A
`.detach` sibling left by an interrupted run is never cleaned up: every full
scan reports it as `local_interrupted_removal`, and the first full scan that
no longer finds it clears the report.

The previous version of a file a download replaces is kept the same way: just
before the finished download is renamed over its target,
`validateDownloadTargetBeforeReplace` verifies the target through the
descriptor it hard-links (or copies) into the trash, made on the entry
folder's one descriptor as a take-out is. The link is by name, so the
preserve then proves it names the verified inode, and removes only its own
link: an entry another program put at `item` meanwhile is left as it is, and
the preserve reports the race
(`TestPreserveRegularFile_LeavesAnEntryThatReplacedItsLink`). One-shot
housekeeping and
the watch maintenance tick (at most hourly) purge, and `onedrive-go trash
list/restore/empty` exposes the trash. Without a trash (executor-only tests)
the verified inode is removed with `RemoveVerifiedRegularFile` instead.

An entry says why its file left the sync folder (`Reason`): `deleted` for a
local delete of an item deleted on OneDrive, `replaced` for the version a
download replaced, and `released` for a synced file the release of a removed
shared folder took out (`Action.ReleasesSharedFolder`, set by
`engine_shortcut_release.go`'s `releaseAction`; `localDeleteTrashReason`
picks the reason): that file was not deleted on OneDrive, the sharer's copy
still exists
(`TestDeleteLocalFile_ReleaseOfASharedFolderTrashesTheFileAsReleased`,
`TestRunOnce_FinalDrainReleasesASyncedTreeToTheTrash`).

A shortcut child mount's trash belongs to its parent drive (R-6.4.12). Its
engine writes to the parent's child trash (`ChildDir`:
`<data>/trash/children/<parent trash name>/<child state name>`, outside the
parent's own entries; `EngineMountConfig.TrashDir`, which multisync's
`shortcutChildTrashDirs` derives from the child mount ID, whose parent is the
configured drive it names), so the parent's `Trash.Children` finds every child
trash whether or not the child still runs: the parent's `trash list`,
`restore` and `empty` cover them (each entry names its own sync folder), and
the parent engine's housekeeping purges them with its own
(`purgeTrashAndChildren`,
`TestPurgeExpiredTrash_ExpiresTheEntriesOfTheDrivesShortcutChildren`). When
the child's state is removed, its artifact purge first hands its trash over
(`Trash.HandOverTo`): each entry directory, committed, pending, orphan or a
reservation without a file, moves into the parent's trash by one rename inside
the data directory, keeping its ID, record and state, so it is in exactly one
trash at every moment; both directories are flushed, then the child's trash
directory is removed. An entry it cannot move, or whose ID the parent already
holds, stays where it was, and so does the child's directory; the purge then
removes nothing of the child and fails as a remaining state artifact, which the
parent publishes again (`TestHandOverTo_MovesEveryEntryAsItIs`,
`TestHandOverTo_StoppedPartWayAndRunAgainHoldsEachEntryOnce`,
`TestHandOverTo_KeepsAnEntryWhoseIDIsTaken`,
`TestShortcutChildArtifactCleanupExecutor_HandsTheChildsTrashToItsDrive`,
`TestShortcutChildArtifactCleanupExecutor_KeepsTheChildWhenItsTrashCannotBeHandedOver`).
A restore of an entry whose sync folder is gone, such as a released shared
folder's, does not make the folder again and names where the file is in the
trash (`TestTrashRestore_IntoASyncFolderThatIsGoneNamesWhereTheFileIs`).

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

The local delete is the only way a removed shared folder's release removes
anything (`engine_shortcut_release.go`, sync-engine.md): it drives
`ExecuteLocalDelete` for one synced entry at a time from the entry's baseline
row, a file whose remote item still exists included, and keeps what the
delete refuses. Content it keeps that sync does not sync is named in the
folder-not-empty sentence a folder delete gives (`FolderNotEmptyReason`).

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
`ErrActionPreconditionChanged` instead of overwriting the newly appeared file.

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

- `ExecutorConfig.driveID`
- `ExecutorConfig.remoteRootItemID`
- `Action.DriveID`

Path convergence checks after successful remote mutation are resolved relative
to that mounted subtree, not via per-action target-root overrides. Scope
classification likewise uses the action completion's authoritative `DriveID`.

Direct shared-item CLI flows still keep explicit target-scoped behavior above
sync through `driveops.SharedTargetClients(...)` and root-aware
`driveops.MountSession` path operations. That is a transfer/CLI boundary, not
ordinary sync execution.

## Scope Helpers

The scope helpers (`syncsched/active_scopes.go`) are pure functions over the
scheduler's slice of active block scopes; the scheduler (`syncsched`) is
their one caller.

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
