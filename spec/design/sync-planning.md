# Sync Planning

GOVERNS: internal/sync/internal/syncplan/planner.go, internal/sync/internal/syncplan/planner_dependencies.go, internal/sync/internal/syncplan/planner_actions.go, internal/sync/internal/syncplan/planner_kinds.go, internal/sync/internal/syncplan/planner_move_composition.go, internal/sync/internal/syncplan/planner_content_holds.go, internal/sync/internal/syncplan/planner_set_aside.go, internal/sync/internal/syncplan/planner_delete_safety.go, internal/sync/internal/syncplan/planner_truth_overlay.go, internal/sync/internal/syncplan/planner_test_seams.go, internal/sync/internal/syncplan/doc.go

Implements: R-2.1.3 [verified], R-2.17.11 [verified], R-2.17.12 [verified], R-2.17.13 [verified], R-2.1.7 [verified], R-6.2.11 [verified], R-2.1.4 [verified], R-2.2 [verified], R-2.3.1 [verified], R-2.14.2 [verified], R-6.2.1 [verified], R-6.2.5 [verified], R-6.2.16 [verified], R-6.4.2 [verified]

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
- Error Boundary: planner errors stop at invalid or unsupported planning states (a row outside the decision table, a folder planned as a content transfer, a built action outside the action enums), and each fails the whole plan rather than dropping its path. Transport, filesystem, store, and execution failures stay outside the planner boundary.

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
| A local move into a moving folder's destination lands inside the folder first when an item leaving the folder needs its source; a new folder here holding a folder's own source at the folder's destination is kept as a conflict copy and the folder moves out of it the next pass (R-2.17.6). | `TestComposeMoves_AFileWhoseTargetIsInAMovingFolderLandsInsideItFirst`, `TestRunOnce_AParkedFileLeavesItsFolderBeforeTheFolderMovesOnOneDrive`, `TestComposeMoves_AFolderInsideTheNewFolderAtItsPathMovesItAsideFirst`, `TestRunOnce_AFolderMovedBackOutOfANewFolderAtItsPathKeepsTheNewFolderAsAConflictCopy` |
| A folder whose OneDrive move lands in a folder made here at another folder's path, the other folder's target below it, converges: the holder moves aside on OneDrive first, the new folder is made there after it and the move lands in it, and nothing is moved into or made inside an old folder (R-2.17.6). | `TestComposeMoves_AFolderWhoseTargetLiesBelowAMoveIntoAFolderAtItsPathMovesAsideFirst`, `TestPlannerPairs_AFolderMovedIntoANewFolderAtThePathOfAFolderMovedBelowItConverges`, `TestRunOnce_ThreeFoldersMovedIntoEachOthersNewFoldersConverge` |
| Moves are composed per item (R-2.17.6): the same rename on both sides only re-keys the baseline and keeps either side's edit pending; divergent renames keep OneDrive's name; cross moves that would nest folders keep the OneDrive move; collisions undo the local claim; moves order against each other and their surroundings, a local move never fabricates its parent, and a pass that cannot order a move defers it, except that a cycle of moves each waiting for the next one's source (a swap or rotation of names) parks one item at a free `<name>.swap-N` sibling so the rest can run, and the parked item follows next pass. Work under a moved file's old path belongs to the folder that takes the path and waits for the move. A delete or cleanup never waits for its parent's folder create. A destination is free only when no other entry holds it under OneDrive's name folding, and an item's remote position may be a path another synced item is moving away from, or a path whose own synced item is gone here (the item ID proves it), so a rename here composes with OneDrive's move of the item onto a path deleted here instead of copying the item. | `TestComposeMoves_DestinationFreeUnderNameFolding`, `TestRunOnce_LocalRenameWhileOneDriveSwapsItConverges`, `TestRunOnce_LocalRenameAndDeleteWhileOneDriveSwapsThemConverges`, `TestPlannerPairs_AFileRenamedHereAndMovedOnOneDriveOntoAPathDeletedHereMoves`, `TestComposeMoves_RotationParksOneItemAndMovesTheRest`, `TestBuildDependencies_WorkUnderAFileMovesOldPathWaitsForTheMove`, `TestBuildDependencies_DeleteUnderARecreatedFolderDoesNotWaitForTheCreate`, `TestRunOnce_SwapOfTwoNamesConverges`, `TestComposeMoves_OneSidedRenamesMoveTheOtherSide`, `TestComposeMoves_SameRenameOnBothSidesOnlyRekeysTheBaseline`, `TestComposeMoves_DivergentRenameKeepsTheOneDriveName`, `TestComposeMoves_CrossMoveCycleKeepsTheRemoteMove`, `TestComposeMoves_DescendantRenameInsideRenamedFolder`, `TestComposeMoves_RemoteHierarchyInversionOrdersLocalMoves`, `TestComposeMoves_LocalMoveIntoRemotelyRenamedFolder`, `TestComposeMoves_OccupiedDestinationDefers`, `TestComposeMoves_FrozenItemsAndTheirSubtreesDoNotMove`, `TestBuildDependencies_NestedMoveEdges`, `TestBuildDependencies_MatchesScanningOracle`, `TestRunOnce_BothSidesRenamedIdenticallyConverges`, `TestRunOnce_BothSidesRenamedIdenticallyKeepsTheRemoteEditPending`, `TestRunOnce_DivergentRenameConvergesInOneSync`, `TestRunOnce_CrossMoveCycleKeepsBothTreesWithoutNesting`, `TestRunOnce_RemoteHierarchyInversionConverges`, `TestRunOnce_RemoteFolderRenameWithChildRenameAndMoveInConverges`, `TestRunOnce_LocalFolderRenameWithChildRenameConvergesInOnePass`, `TestRunOnce_RemoteFolderRenameWithNewSubfolderConverges`, `TestExecutor_LocalMove_MissingDestinationParentIsStale`, `TestRunOnce_RandomizedBidirectionalEditsConverge`, `TestComposeMoves_AnEntryMovedOnOneDriveIntoAFolderThatTakesItsPathMovesIntoItHereFirst`, `TestRunOnce_ARotationThroughNestingMadeOnOneDriveConverges` |
| A remote move of a locally edited file moves the local copy with its edit and uploads the edit afterwards (R-2.17.7); a local rename racing a remote edit downloads the edit (R-2.17.3). A move in either direction composes with OneDrive's edit of the same file in one pass when the copy here holds the synced content: the move, then the download of the edit at the moved path (Move Composition, A move composed with OneDrive's edit). | `TestRunOnce_RemoteMoveWithLocalEditMovesThenUploads`, `TestExecutor_LocalMove_LocallyEditedSourceStillMovesAndKeepsTheEditPending`, `TestRunOnce_LocalRenameWhileRemoteEditsDownloadsTheEdit`, `TestPlannerPlanCurrentState_LocalMoveComposesWithTheRemoteEdit`, `TestRunOnce_LocalRenameAndRemoteEditComposeInOnePass`, `TestRunOnce_RemoteMoveAndEditComposeInOnePass`, `TestExecutor_Download_MovedIntoPlaceTargetIsProvenByItsHash`, `TestActionFreshness_MovedIntoPlaceDownloadReadsTheMoveSource`, `TestActionFreshness_PostRemoteMoveDownloadAllowsOnlyTheMoveProducedETagChange` |
| A remote move over a file OneDrive deleted moves the local copy onto it, replacing a verified unchanged file or conflict-copying an edited one or a new file that took the name (one saved at the name a replacing move freed, between its two steps), and never re-uploads the moved item or downloads it beside its old copy; a move over a folder OneDrive deleted that holds only its synced content waits for that folder's delete here and then runs, and over one written into here renames it to a conflict copy first (R-2.17.9). | `TestPlannerPairs_AFileMovedOnOneDriveOntoAFolderItDeletedEndsOnce`, `TestPlannerPairs_AFolderOneDriveDeletedGivesWayToAnItemItMovedOntoItsPath`, `TestRunOnce_AFolderMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere`, `TestRunOnce_AFolderWrittenIntoHereThatOneDriveReplacedByAMovedFileIsKeptAsAConflictCopy`, `TestBuildDependencies_ALocalMoveOntoAFolderDeletedHereWaitsForItsDelete`, `TestRunOnce_AFileMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere`, `TestComposedDisplacement_EntryAtTheDestination`, `TestComposeMoves_MoveOntoANewFileAtTheFreedNameKeepsItAsAConflictCopy`, `TestRunOnce_ReplacingMoveFindingANewFileAtTheFreedNameStillMoves`, `TestRunOnce_RemoteMoveOverExistingFileReplacesWithoutResurrecting`, `TestRunOnce_RemoteMoveOverLocallyEditedFileKeepsTheEdit`, `TestExecutor_LocalMove_ReplacesOnlyTheVerifiedDestination`, `TestExecutor_LocalMove_StaleSourceLeavesTheReplacedDestinationInPlace`, `TestRunOnce_RemoteMoveOverIdenticalFileConvergesInOnePass`, `TestRunOnce_AFileMadeHereUnderTheNameOneDriveGaveAMovedFileIsKeptAsAConflictCopy` |
| Move pairing requires the item to have left its baseline path: hard links and copies are new files, rename-and-recreate is a move plus a create (R-2.17.8). A folder made here at the path a moved folder left, where OneDrive moved the folder away too and made a folder of its own, adopts OneDrive's folder instead of being superseded on every pass (R-2.17.6). A synced file moved here onto a path another synced item had pairs by its identity and synced content, OneDrive's move of it by item ID (R-2.17.8). | `TestQueryReconciliationState_MovePairingRequiresTheSourceToBeVacated`, `TestMovedOntoAnotherItemsPath_NeedsTheIdentityAndTheSyncedContent`, `TestPlannerPairs_AFileMovedHereOntoAPathAnotherItemHadAndElsewhereOnOneDriveEndsOnce`, `TestRunOnce_AFileMovedHereOntoAFolderDeletedHereAndRenamedOnOneDriveEndsOnce`, `TestPlannerPairs_AnItemMovedOnBothSidesOntoAnEntryOneDriveDeletedEndsOnce`, `TestRunOnce_AFileOfAFolderRenamedHereMovedOnOneDriveOntoAFolderItDeletedEndsOnce`, `TestRunOnce_HardLinkUploadsAsANewFile`, `TestRunOnce_RenameAndRecreateMovesThenCreates`, `TestPlannerPairs_AFolderMovedIntoDifferentNewFoldersAtItsOwnPathOnEachSideConverges`, `TestRunOnce_AFolderMovedIntoDifferentNewFoldersAtItsOwnPathOnEachSideConverges` |
| Remote moves applied locally carry the source baseline and do not re-upload unchanged content. | `TestPlannerPlanCurrentState_RemoteMoveCarriesSourceBaseline`, `TestRunOnce_RemoteRenameAppliedLocallyDoesNotReupload` |
| Type changes with content on both sides keep both (`conflict_keep_both`, and type-mismatched create/create): a type-aware conflict copy of the local entry, then OneDrive's item at the path; a folder's change includes its subtree; nothing below the path is deleted on either side (R-2.17.10). Both sides replacing the item with the same new type are two creates at one path: equal content (or two folders) only updates the baseline, different files keep both as a create/create conflict.  A new filesystem identity alone is never a local change (D-057): a folder rebuilt, restored from a copy or on a renumbered disk, with nothing changed below it, accepts OneDrive's delete or replacement, and the executor's folder delete checks the identity the plan saw rather than the baseline's (R-2.8.10). | `TestRunOnce_RemoteFolderDeleteAcceptsAContentIdenticalLocalRestore`, `TestRunOnce_RemoteFolderDeleteAfterDeviceRenumberingIsApplied`, `TestRunOnce_RemoteDeleteOfALocallyRecreatedEmptyFolderIsApplied`, `TestRunOnce_RemoteReplaceOfALocallyRecreatedFolderConverges`, `TestRunOnce_FolderReplacedByFilesOnBothSidesKeepsBoth`, `TestRunOnce_FileReplacedByFoldersOnBothSidesMergesThem`, `TestRunOnce_RemoteFileReplacedByNonEmptyFolderWithLocalEditKeepsBoth`, `TestRunOnce_RemoteFolderReplacedByFileWithLocalChildEditsKeepsBoth`, `TestRunOnce_LocalFolderReplacedByFileWithRemoteChildEditKeepsBoth`, `TestRunOnce_CreateConflictLocalDirRemoteFileKeepsBoth`, `TestRunOnce_CreateConflictLocalFileRemoteDirKeepsBoth`, `TestExecutor_ConflictCopy_DirectorySourceIsRenamed`, `TestExecutor_ConflictCopy_DirectoryNeverReplacesAnEmptyDirectoryAtItsName`, `TestExecutor_DirectoryConflictCopy_ParentSwappedForSymlinkStaysInside`, `TestExecutor_ConflictCopy_UncheckableSourceFailsWithItsReason` |
| Type changes on either side are replacements ordered delete-then-create, never a silent stall. | `TestPlannerPlanCurrentState_LocalFileReplacedByFolderReplacesRemoteItem`, `TestPlannerPlanCurrentState_LocalFolderReplacedByFileReplacesRemoteItem`, `TestPlannerPlanCurrentState_RemoteFileReplacedByFolderReplacesLocalItem`, `TestPlannerPlanCurrentState_RemoteFolderReplacedByFileReplacesLocalItem`, `TestRunOnce_LocalFileReplacedByFolderConverges`, `TestRunOnce_LocalFolderWithChildrenReplacedByFileConverges` |
| Deletes toward one side are held for confirmation when the other side is empty while the baseline holds files, or when they exceed max(50, 30% of baseline files); moves never count, non-delete work continues, and `sync --confirm-deletes` releases them. | `TestPlanner_MassRemoteDeleteIsHeldForConfirmation`, `TestPlanner_MassLocalDeleteAfterAnomalousFullRefreshIsHeld`, `TestPlanner_EmptiedLocalSideHoldsEvenFewDeletes`, `TestDeleteSafetyHold_RecentUnconfirmedDeletesCountTowardTheThreshold`, `TestDeleteSafetyHold_ConfirmedDeletesAreMarkedForTheLedger`, `TestRunOnce_SlowDeletionAcrossPassesIsHeldWithinTheWindow`, `TestRunOnce_DeletesOlderThanTheWindowStopCounting`, `TestPlanner_DeleteGuardBelowThresholdProceeds`, `TestPlanner_DeleteGuardIgnoresMoves`, `TestDeleteSafetyHold_FolderDeletesCountTowardThreshold`, `TestDeleteSafetyHold_LostAndFoundDoesNotMakeASideNonEmpty`, `TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo`, `TestDeleteSafetyHold_ConfirmReleasesExactlyTheHeldSet`, `TestDeleteSafetyHold_HeldRowWithoutItemIDDoesNotApproveAKnownItem`, `TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked` |
| A dependency cycle drops only its members and their dependents; the rest of the plan runs (R-2.17.6), and a one-shot run that ends on such a plan counts the dropped actions as work left for the next sync, so it exits 1 (R-6.6.28). A folder delete that closes a cycle through the move of a folder above it waits for the next pass instead, so the rest of that cycle runs and the next pass deletes the folder at its new path (R-2.17.2). | `TestIsolateDependencyCycles_KeepsUnrelatedWork`, `TestPassWithoutActions_ActionsDroppedAsACycleAreLeftForTheNextSync`, `TestWithholdDeletesInsideFolderMoveCycles_HoldsTheDeleteThatClosesTheCycle`, `TestRunOnce_FileMovedOutOfASubfolderIntoTheRemadeFolderWithTheSubfolderRemovedConverges` |
| A delete waits for the create on its side that writes the deleted file's content at another path, so a failed create, or a failure of what it waits for, skips the delete and the content stays on that side; an edge that would close a cycle, given the edges kept before it, is left out (R-2.17.2). | `TestBuildDependencies_ADeleteWaitsForTheCreateThatWritesItsContent`, `TestBuildDependencies_ADeleteDoesNotWaitForACreateThatWaitsForIt`, `TestBuildDependencies_OfTwoContentEdgesThatCloseACycleOnlyTogetherTheFirstIsKept`, `TestBuildDependencies_AReusedInodeWithOtherContentCarriesNothing`, `TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveWhileACreateBeforeItsUploadFails` |
| A removal of synced content (a delete of the file, or a download of another item over it here) waits while the other side holds the file at another path and no create of the plan writes its content on the removal's side, and a download over a synced file waits for the download of that file's item where OneDrive moved it, so a pass never leaves a side without content the user did not remove (R-2.17.11). | `TestPlanBuiltActions_ARemovalOfContentTheOtherSideMovedWaitsForItsCreate`, `TestBuildDependencies_ADownloadOverASyncedFileWaitsForTheDownloadOfItsItem`, `TestPlannerPairs_AFileRenamedIntoAFolderInAHeldFoldersPlaceKeepsItsOneDriveCopy`, `TestPlannerPairs_AFileOverwrittenHereWaitsForTheDownloadOfItsMovedContent` |
| A file moved and edited here, at the path OneDrive moved the item to, is the item: its local move to OneDrive's place carries the edit and the next pass uploads it as the same item, never a download beside it nor a new file (R-2.17.7). | `TestPlannerPairs_AFileEditedHereThatBothSidesMovedStaysOneItem`, `TestRunOnce_AFileEditedHereThatBothSidesMovedStaysOneItem`, `TestRunOnce_AFileMovedAndEditedHereThatOneDriveMovedAlikeStaysOneItem` |
| A synced folder whose place on OneDrive (where OneDrive moved it, or where it stays when this computer's move of it is undone) is held here by a new folder, its own copy here empty, is one folder with the new one by path: the new folder takes the item and the empty copy is deleted here, and nothing moves (R-2.17.13). | `TestComposeMoves_AFolderLandingOnANewFolderHereMergesWithItOnlyWhenItsCopyIsEmpty`, `TestPlannerPairs_ANewFolderHereMergesWithTheFolderOneDriveMovedOntoItsPath`, `TestPlannerPairs_AFolderWhoseMoveHereIsUndoneMergesWithANewFolderAtItsPlace`, `TestRunOnce_ANewFolderHereMergesWithTheFolderOneDriveMovedOntoItsPath`, `TestRunOnce_AFolderWhoseMoveHereIsUndoneMergesWithANewFolderAtItsPlace` |
| A rotation through nesting split across the two sides with a third move converges: an entry holding the path the undone folder takes back moves first into its folder where that folder is now, or waits a pass where a folder above it leaves; a synced file moved here onto that path pairs by identity and goes back; an empty folder made there gives way, its delete recording nothing; a folder moved on both sides onto an entry OneDrive deleted is moved here, not made (R-2.17.6, R-2.17.13). | `TestPlannerPairs_ARotationThroughNestingSplitAcrossTheSidesConverges`, `TestBuildDependencies_AMoveLandingInNothingADestinationHoldsRunsFirst`, `TestRunOnce_ARotationThroughNestingSplitAcrossTheSidesConverges`, `TestRunOnce_AnEmptyNewFolderGivesWayToTheFolderWhoseMoveHereIsUndone`, `TestCommitMutation_DeleteOfAnUnsyncedEntryRecordsNothing`, `TestPlannerPairs_AFolderMovedOnBothSidesOntoAnEntryOneDriveDeletedIsMovedHereNotMade`, `TestRunOnce_AFolderMovedOnBothSidesOntoAnEntryOneDriveDeletedEndsOnce` |
| A folder that must leave the path of a new folder made here, which a folder's OneDrive move lands in, while its own target lies below that move, moves first into the moving folder where that folder is on OneDrive, or aside when that name is taken; a folder OneDrive put at the path a synced item left by this computer's move is made here before the item moves into it (R-2.17.6). | `TestPlannerPairs_AHeldFolderMovedIntoAFolderThatMovesIntoTheNewFolderAtItsPathConverges`, `TestRunOnce_AHeldFolderMovedIntoAFolderThatMovesIntoTheNewFolderAtItsPathConverges`, `TestPlannerPairs_AFolderOneDriveMovedOntoThePathAFileLeftHereIsMadeHere`, `TestRunOnce_AFolderOneDriveMovedOntoThePathAFileLeftHereIsMadeHere` |
| A delete on OneDrive of a synced file whose content the upload of the file here writes at another path, where that upload needs the path the delete frees, is a move of the item to a free temporary name above every folder the plan deletes; the next pass deletes it once the upload holds the content, or moves it back where the file is here (R-2.17.12). | `TestPlannerPairs_AFileUploadedOntoThePathItsDeleteFreesIsSetAsideOnOneDrive`, `TestAsidePath_ParksAboveTheDeletedFoldersUnderAFreeName`, `TestRunOnce_AFileMovedUpOntoItsDeletedFoldersPathStaysOnOneDriveUntilItsUploadRuns` |
| Mode-specific deferral and dependency ordering stay planner-owned rather than executor- or CLI-owned. | `TestSyncModeFromFlags`, `internal/sync/internal/syncplan/planner_actions_test.go`, `internal/sync/internal/syncplan/planner_move_composition_test.go` (`TestBuildDependencies_NestedMoveEdges`, `TestBuildDependencies_WorkUnderAFileMovesOldPathWaitsForTheMove`), `internal/sync/internal/syncplan/planner_delete_safety_test.go` (`TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo`) |
| Every single-path combination of baseline, local and remote state (absent, file A/B/C, folder), and the move combinations of one synced file, has its plan checked in as `testdata/reconcile/matrix.golden` (refresh with `-update`); with `ONEDRIVE_GO_RECONCILE_CONVERGE=1` each cell is also run to convergence on a pure model and must end at the reference planner's content set. A planner-level model test applies random edits to both sides of a pure model, plans it through the real comparison SQL and planner, applies the plan without an executor (items addressed by ID, a create onto a taken name failing as a 409), and must converge with every written content kept (D-093). | `TestReconcileMatrix_Golden`, `TestPlannerModel_RandomTriplesConverge` |
| The decision table is Go and its kinds are typed enums; a side type the enum lacks fails the row scan naming the path. Every valid state a side holds is classified, except a drive root only one side holds (all 8,388,608 normalized states); a row outside the table, or a folder planned as a content transfer, fails the plan before the unavailable-truth suppression can plan it as nothing, naming the path; so does a built action whose type or folder create side is outside its enum, before any rule reads it, and every decision on the action type panics on such a value instead of answering it (R-6.2.16). Content proven equal on both sides never transfers, whichever side changed: a local edit equal to OneDrive's unchanged content is a baseline update that records the new local facts beside the remote ones, and the next plan is empty (R-2.2.3). | `TestPlannerKinds_ParseRoundTripsEveryMemberAndRefusesOthers`, `TestQueryPlannerRows_UnknownSideTypeFailsTheScanNamingThePath`, `TestQueryPlannerRows_GoDecidesEveryRowOfOneQuery`, `TestPlanCurrentState_UnclassifiedRowFailsEvenWhereTruthIsUnavailable`, `TestPlanCurrentState_UnclassifiedComparisonRowFailsThePlan`, `TestPlanCurrentState_FolderTransferFailsThePlan`, `TestPlanCurrentState_UnlistedActionTypeFailsThePlan`, `TestActionTypeDecisions_PanicOnATypeOutsideTheEnum`, `TestQueryReconciliationState_LocalEditEqualToUnchangedRemoteIsABaselineUpdate`, `TestRunOnce_LocalEditEqualToUnchangedRemoteUploadsNothing`, `TestRunOnce_UnclassifiedRowFailsThePassNamingThePath`, `TestDecide_EveryValidStateHasTheTableProperties`, `TestDecide_ARootOnlyOneSideHoldsIsUnclassified`, `TestDecisionState_ValidNamesTheBrokenRule` |
| Planner decisions stay row-driven and action-shaped across conflict and folder-parent preservation cases. | `TestPlannerPlanCurrentState_EditDeleteRecreateUploadClearsItemID`, `TestPlannerPlanCurrentState_RemoteParentDeleteRecreatesParentForEditedLocalChild`, `TestPlannerPlanCurrentState_DownloadOnlyKeepsParentDeleteWhenEditedChildUploadDeferred`, `internal/sync/syncstore/store_planner_visible_test.go` |

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
mutated. Those views feed the comparison facts (`comparison_flags`) the Go
decision table reads, including the invariant that a baseline row absent from both planner-visible
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
    R-2.17.6, D-059c): a cycle never stops the rest of the plan.

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

A comparison row (`SQLiteComparisonRow`) carries each side's type
(`BaselineType`, `LocalType`, `RemoteType`, `SideAbsent` where the side holds
nothing) and no presence flags or item type of its own: the planner reads a
side's presence as `SideType.Present` and the path's item type as the first
present side's (`SQLiteComparisonRow.ItemType`), which is what the query's
`COALESCE` over the three tables gave. The two agree on every row the query
can produce because every table it reads, the live ones and the
planner-visible copies, refuses an `item_type` outside the enum: a joined
side always has a type, and only an absent one reads as the empty string
(`TestQueryComparisonState_SideTypesCarryPresenceAndItemType`).

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

## Decision Table Kinds

The decision table is Go (`domain_decide.go`) and has two stages:
`classify` gives every path a comparison kind (which of baseline, local and
remote hold it, and how the sides relate to the baseline), and `Reconcile`
turns that kind and the path's flags into a reconciliation kind, the decision
`buildActionsForReconciliation` maps to actions. `Decide(DecisionState)` is
the two in a row, pure and total. `DecisionState` is what each side holds at
the path (`SideType`: absent, file, folder, root) and the 17 per-path facts
the comparison query derives (content changes, replacements, move pairing,
the ancestor-move waits); the path's item type and the two type-changed flags
are derived from the side types. Both vocabularies are Go enums in
`domain_compare_kinds.go` (`ComparisonKind`, `ReconciliationKind`);
`planner_kinds.go` holds the planner's check of them and its error. Every
`switch` over a kind lists every member (the `exhaustive` linter), so Go
cannot forget a new kind. The reconciliation enum owns its names; some equal
action-type names, which are a persisted wire format
(`retry_work.action_type`), by choice rather than by definition.

SQL keeps what is set-valued and per-path facts: the path universe, local and
remote move pairing, the ancestor-move sets, the descendant probes and the
scalar flags (`sqlComparisonStateCTEs`). One query per plan
(`sqlQueryPlannerState`) returns every input of the table for every path, and
`QueryPlannerRows` decides each row in Go. Until 2026-11-20 two SQL `CASE`
expressions decided, and the comparison chain ran twice per plan, once for
each; the Go table was first proven equal to them stage by stage over every
SQL input (131,072 comparison inputs, 442,368 reconciliation inputs) and run
beside them on every plan, then they were deleted. The reconciliation matrix
golden did not change (`TestReconcileMatrix_Golden`).

Every genuine "nothing to do" is an explicit `noop`, and a state no rule
covers is `unclassified`, the zero value of both enums. The planner tests,
the reconciliation matrix and the planner model check `DecisionState.Valid`
on every row they plan.

`Valid` states the flag definitions as rules (a flag about an absent side is
false, a type change is a content change, a folder has no content to change,
two folders are equal, and so on). Over all 8,388,608 normalized states
(23,077 of them valid), `TestDecide_EveryValidStateHasTheTableProperties`
proves that a valid state is unclassified only when no side holds the path or
it would create a drive root, that no valid state uploads a local folder or
downloads onto a remote folder, and that every reconciliation kind is reached.
A root-typed entry only one side holds (the `(absent, root, absent)` cell and
its remote mirror) was an upload or a download; no observer records one (the
remote observer drops the drive root, the local scanner records only files
and folders), and a drive root is no file, so it is unclassified and planning
refuses it (`TestDecide_ARootOnlyOneSideHoldsIsUnclassified`).

`equal_again` is every row whose three sides are present, that changed on
some side, and whose content is proven equal (`current_equal = 1`), whichever
side changed; `diverged` therefore always means the content differs, and the
reconciliation stage writes no rule for a diverged row with equal content.
`equal_again` is a `baseline_update`: nothing transfers, and the baseline
learns both sides' current facts, as for `create_equal` and for `unchanged`
with metadata drift. Until 2026-11-12 it required a remote change, so a local
edit that made a file equal to OneDrive's unchanged content (the baseline
recorded other local bytes, as an accepted server transform does, R-5.8.2)
was `diverged` and an upload of bytes OneDrive already held: a new version
online for nothing, and on a library that transforms uploads, two sides that
could differ again. The local file holds exactly the bytes OneDrive reports, so
a later remote change is a plain fast-forward. A local mtime newer than
OneDrive's is not sent on its own, as with metadata drift
(`TestQueryReconciliationState_LocalEditEqualToUnchangedRemoteIsABaselineUpdate`,
`TestRunOnce_LocalEditEqualToUnchangedRemoteUploadsNothing`). Each consumer of
the two kinds keeps its meaning: the `diverged` rules for a type mismatch or
two type changes hold different content by construction, the download and
edit/edit rules met equal content only after a remote change, which was
`equal_again` already, and the upload rule now plans only bytes OneDrive does
not hold.

An unclassified row fails the plan (R-6.2.16). `PlanCurrentState` checks every row
before it plans any (`validatePlannerKinds`), ahead of the unavailable-truth
suppression that would otherwise return early for the path, and returns
`unclassifiedPlanError` (`errUnclassifiedPlan`): the first unclassified path,
its two kinds, and how many other rows are unclassified. An unclassified row
carries its decision state (`DecisionState.Inputs`: each side's presence and
item type, then the 17 flags as 0 or 1), so the error says which inputs no
rule matched; a flag the query yields as NULL fails the row scan instead. A folder planned as a content transfer
(`errFolderTransferPlanned`) fails the plan the same way. Either error stops
that drive's sync pass before anything executes: one-shot `sync` returns it,
and in watch mode the replan ends with it, which ends the drive's runner; the
control plane reports it once and restarts the runner after its backoff
(R-2.8.11). The drive stays stopped until the state or the table changes.
Skipping the path and syncing the rest was the alternative, rejected: it hides
the bug behind a path that never syncs, while the error carries what a fix to
the table needs.

Built actions are held to their enums the same way. `PlanCurrentState` builds
every action from a constant, then plans them through `planBuiltActions`,
which first refuses an action whose type or folder create side is outside its
enum (`validatePlannedActions`, `errUnlistedActionPlanned`, naming the path).
Only a programming error makes one, and every step after the build decides by
action type: mode admission refused such an action and the deferral counts
had no bucket for it, so it vanished from the plan. Every switch on the action
type, in the planner and in the rest of `internal/sync`, names every member,
which the `exhaustive` linter holds (no `//nolint:exhaustive` remains on one);
the planner's former tagless switches and its `isDeleteAction` and
`isMoveAction` tests are switches over the type for that reason, and every
"creates an entry on this side" test goes through `createSide`. Each
default, which no value can reach, panics with `OutsideTheEnum` where the
decision returns a plain value, and returns an error where it already returns
one (the store's row value, mutation classification, publication mutation,
delete ledger and remote mirror; worker dispatch), so no default answers with
a verdict.

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
same-side moves of everything under it (R-2.17.2), and held for a later pass
while a move out of it waits for one (Move Composition). Only action
normalization may turn it back into a local folder create, and only for
runnable descendant work.
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
upload or download as a comparison bug (`errFolderTransferPlanned`), which
fails the plan like a row outside the decision table (see Decision Table
Kinds); it used to be logged and the row dropped, a path that silently stopped
syncing.

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
(D-059a), and a directory's conflict copy moves it to a free
`name.conflict-<timestamp>` in the one loop the conflict copy of a file or a
folder takes, the sync tree's no-replace rename with its fallbacks
(`synctree.Root.RenameNoReplace`; `spec/design/sync-execution.md`, the
conflict copy). When only one side changed, local wins as before
(`replace_remote`), because a replaced OneDrive item survives in the recycle
bin. A remote delete under a folder the user replaced with a file treats the
path as absent (nothing can exist below a file); its local-absence check used to
fail on the non-folder parent and held the folder's delete forever. Delete
ordering and parent preservation use the deleted side's type
(`DeletedItemType`), since the two sides disagree across a type change. Moves that cross drive ownership are
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
still transferred; to different places, OneDrive's location wins (R-2.17.6,
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
and its source is not displaced by a move that runs first. On this computer a
folder OneDrive made at a path that a synced item it moved away still holds
here is not in place: SQLite plans no create at a path the baseline still
gives that item, so the pass after the item's move makes the folder. A local
move into such a folder and the item's own move are never planned together
(`x` moved to `x2` and made again on OneDrive, and `f` moved into the new
`x`: `x` moves, then `f`). Planned together, `f`'s move would land in the old
`x` and be carried to `x2/f` if it ran first, and would find no `x` if it ran
second. The item is found at the path the move is planned to, compared as
OneDrive compares names: a move that runs inside a folder before the folder
moves lands where that folder is now. Whichever composition decides first is
planned, and the other waits for the next pass, except that the move into the
folder waits while the item is a file or a frozen folder, neither of which it
can land in. Deciding the first rather than waiting for the item lets two
folders that each move into a new folder at the other's path converge
(`TestComposeMoves_FolderMovedIntoAFolderMadeWhereAnotherFolderLeftWaits`,
`TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges`,
`TestRunOnce_FolderMovedOnOneDriveIntoAFolderMadeWhereAFileLeftConverges`).
A move decided first lands in the folder that holds the path; while that
folder's own move waits, this computer shows the moved item inside the old
folder, and no remote move is planned for it, until a later pass moves the
folder and then the item. Such a folder is not the item parked to break a
swap or rotation on this computer
(`TestComposeMoves_FolderAMoveLandsInIsNotParkedToBreakASwap`). On OneDrive
the twin, a folder made here at the path of an item moved here, is created in
the same pass once the item's own move has left the path, and the move into
it waits for both. While OneDrive keeps a synced item at that path this pass,
the move waits for the next pass instead: run, it would land in that item and
stay in it for as long as the item does (`x` renamed `x2` here and made again,
`f` moved into the new `x`, and `X2` made on OneDrive: the rename collides,
`x` stays where OneDrive has it, and `f` waits with it). The move waits while
the item is at its target there, or is a file or a frozen folder still to be
decided, even a file that then moves in the same pass. An
`x2` made on OneDrive with exactly that name is the path the rename made here,
and the sync makes the two one folder: what `x` held moves into it, and `x`
stays at its path on OneDrive, holding `f`. A rename here onto the path of a
synced file deleted here before the same sync ends the same way, with the file
deleted on OneDrive. A folder still to be decided when the move is, and an
item decided without an OneDrive move (held for the next pass, or parked),
hold the move through a take-back: the move is planned, and when composition
ends with that item not moved on OneDrive, the move is taken back and
composition runs again with its item waiting, so whatever was decided because
it moved is decided again, until a run takes nothing back (`y` renamed `y2`
here and made again while `Y2` is made on OneDrive, `x` moved into the new `y`
and made again, and `f` moved into the new `x`: `f`, decided before `x`, waits
as long as the collision lasts; or the synced file `a/x` deleted here, `x`
moved into `a` in its place, `a` renamed `b`, and `f` moved into a new `x`:
`x`'s move waits for the delete, `f` waits with it, and the next pass moves
both). A move that waits for the item at its folder's path is deferred at
once, so no item is parked to free its destination. A folder the plan moves
away holds the move only when that folder's own move lands, directly or
through the moves it waits for, in a folder made at the moving item's path: of
two folders each moved here into a new folder at the other's path, which
planned together wait for each other, a cycle dropped on every pass, the one
decided first moves into the old folder at that path and the other waits, then
carries it along a pass later. Of a file and a folder moved so, the folder
waits even when it is decided first, and three folders moved here in a ring
converge the same way. Only those moves land in the folder that waits: any
other move into it is taken back and waits with it. Only a folder carries
them: of two files moved so, or a ring through a file, every move waits on
every pass, since the other's move would land in the file OneDrive keeps at
that path and be refused
(`TestComposeMoves_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsAnItemAtItsPath`,
`TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsTheOldFolderThere`,
`TestRunOnce_MoveIntoAFolderMadeHereWhileOneDriveMakesTheRenamedPathConverges`,
`TestRunOnce_MoveIntoAFolderMadeHereAfterARenameOntoADeletedFileConverges`,
`TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileTheOldFolderWaitsInANewFolder`,
`TestRunOnce_MoveIntoAFolderMadeHereWhileTheOldFolderWaitsForADeleteConverges`,
`TestRunOnce_TwoFoldersMovedHereIntoNewFoldersAtEachOthersPathsConverge`,
`TestRunOnce_FileAndFolderMovedHereIntoNewFoldersAtEachOthersPathsConverge`,
`TestRunOnce_ThreeFoldersMovedHereInARingOfNewFoldersConverge`,
`TestRunOnce_TwoFilesMovedHereIntoNewFoldersAtEachOthersPathsWait`). An item
moved into a new folder made at its own path on the other side holds that
path itself, so it moves aside first: to a free `<name>.swap-N` beside its
source, as a swap's parked item does, deferred; the next pass makes the
folder at the path it left and moves the item into it. Waiting for itself, a
file stayed on every pass, and a folder's move and the create of the folder
it lands in waited for each other, a cycle dropped on every pass. When a
folder above the item moves on that side in the same plan, the item waits a
pass instead. While such an item is deferred, parked or not, nothing is made
at or below the new folder's path on its side, compared as OneDrive compares
names (`ownLandings`), wherever that side's snapshot has the item at the
folder's path as the item's target names it, looked up back through the
plan's earlier moves on that side; on this computer the path held is the one
in the snapshot, where writes here are addressed until the folders above
them have moved. Nothing orders such a create after the move aside, so it
went into the item while the item still had the path, and when that move
failed, a folder made so between the new folder and the item's target held
the item's own landing, which it cannot move into, and the syncs stalled;
below a folder whose OneDrive move carries the item there, the create of the
new folder collided with it; and when OneDrive renamed `Y` to `Z` and moved
`Z/f` into a new `Z/f/a/f`, the create of `Z/f/a`, addressed here at `Y/f/a`
until `Y` has moved, went into the old `f`, which could then never move into
its own subfolder, and every later sync waited with only Debug lines. Now the
item waits for the pass after `Y`'s move, which moves it aside there
(`TestComposeMoves_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst`,
`TestRunOnce_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst`,
`TestRunOnce_AFolderMovedIntoTheFolderThatTakesItsPathConvergesWithAnEditInIt`,
`TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath`).
On both sides a move into a chain of new folders (`x` and `x/sub`) is held by the
item at the first path above its new folders that a synced item holds: looked
for at `x/sub` alone, `f`'s move into it would be planned after `x`'s and
find no `x/sub` on this computer
(`TestRunOnce_FolderMovedOnOneDriveIntoAChainOfNewFoldersConverges`,
`TestRunOnce_FolderMovedOnOneDriveIntoNewFoldersWhereAFolderMovedDeeperLeftConverges`).
OneDrive moves are rebased through an ancestor's committed move (remote_state
and the baseline follow it). Local moves are not, because `local_state` is
observation-owned:
a local child move runs inside its folder before the folder moves, or waits a
pass. An item whose target lies at or below the path a folder above it vacates
on that side (a folder made at that path, or the path itself) is decided after
that folder's move and moves after it: an item's path equal to its baseline
path does not make it unmoved, since its parent there may be another folder
now. On OneDrive a file's move is rebased through the folder's move and runs in
the same pass; a local move, or a folder's move, whose subtree can hold work
addressed at the old paths, waits a pass
(`TestComposeMoves_EntryMovedIntoTheFolderRecreatedAtAMovedFoldersPath`,
`TestComposeMoves_SubfolderMovedToThePathItsMovedParentLeftWaitsForTheParent`,
`TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges`,
`TestRunOnce_SubfolderMovedOnOneDriveToThePathItsMovedParentLeftConverges`).
So does the file when the folder moves into or onto a path that another
folder's OneDrive move of the plan vacates (`x` moved to `x2` and made again,
`f` moved into the new `x`, a new `f` made and `x/f/b.txt` moved into it): its
rebased source `x/f/b.txt` would lie below `x`, the dependency rules would
make `x`'s move wait for the file to leave first, and the file waits for
`f`'s move, which waits for the path `x` left to be filled, a cycle. Such a
file is decided after every folder OneDrive relocates, so composition sees
each of their sources whichever of them it decides first, and moves in the
next pass
(`TestComposeMoves_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathWaits`,
`TestComposeMoves_FileMovedBackWaitsForAFolderDecidedAfterIt`,
`TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges`).
A directory between the item and the moving folder that is not a synced item
(a folder made on one side) moves with that folder and changes none of this
(`TestComposeMoves_FileMovedOnOneDriveOutOfAFolderMadeHereWaitsForItsFolder`,
`TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges`). The search
for the moving folder (`vacatingFolder`) stops at the nearest synced folder
above the item that is relocated on that side or present on that side only
(one SQLite deletes or re-creates). A nearer relocated folder the item does
not land below does not end the walk: when a farther relocated folder above
it vacates the path the item's target lies at or below, the item's move
waits for the next pass (`landsPastARelocatedFolder`), which finds it where
both folders' moves carried it. Planned from its source below both moving
folders, such a move closed a cycle wherever nothing else ordered it: with
`docs/sub` renamed `docs/sub2` and `docs` moved to `archive`, a move of
`archive/sub2/inner.txt` onto `docs` itself, or into `photos` moved onto
`docs`, waited for `docs`'s move, directly or through `photos`'s, while
`docs`'s move waited for it to leave first; and with `docs` moved to
`archive` and made again and `archive/sub` moved out of it to `photos/sub`,
a move of `photos/sub/inner.txt` into the new `docs` waited for the new
folder, the new folder for `docs`'s move, `docs`'s for `sub` to leave, and
`sub`'s for the file to leave it. Each made on either side converges in one
sync, as does the renamed subfolder with its `inner.txt` moved into a `docs`
made again, which `buildDependencies` once ordered by letting the move run
from the source the folder move took away, superseded once
(`TestComposeMoves_EntryBelowARelocatedSubfolderWaitsForTheNextPass`,
`TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges`,
`TestRunOnce_FileMovedOutOfARenamedSubfolderOntoTheMovedFoldersPathConverges`,
`TestRunOnce_FileMovedOutOfARenamedSubfolderIntoAFolderMovedOntoTheMovedFoldersPathConverges`,
`TestRunOnce_FileMovedOutOfASubfolderThatLeftTheMovedFolderIntoTheRemadeFolderConverges`).
In the `photos` shape made on OneDrive, the local rename of `docs/sub` to
`docs/sub2` runs inside `docs` before `docs` moves, so it does not wait for
`photos`'s local move onto `docs` though its path lies below that move's
destination: a local move from below another local move's destination moves
inside what holds that destination now, wherever it lands there
(`TestBuildDependencies_ALocalMoveFromBelowAnotherLocalMovesDestinationRunsFirst`).
When the search stops at the one-sided folder, composition neither rebases
nor holds the move, which keeps its source below the folder move's. Where
the plan creates a folder at the vacated path on that side, and SQLite made
the one-sided folder again on the side it lost it, `buildDependencies` lets
such a move run after the folder move, from the source that move took away:
it is superseded once, and the next pass of the same sync moves it
(`TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges`).
Where SQLite deletes the one-sided folder instead, the delete waits for the
move to leave it (named from where the folder move puts it, the move would
not hold the delete back, and the folder would be deleted online with the
file still inside), the folder move waits for the delete, and the move waits
for the folder made at the vacated path, which waits for the folder move: a
cycle. Dropped, nothing ran on any pass and every sync reported the work
unfinished. A folder delete that closes a cycle through the move of a folder
above it on its side waits for the next pass instead
(`withholdDeletesInsideFolderMoveCycles`, before the cycles are isolated): the
folder move carries the one-sided folder with the item in it, the folder is
made at the vacated path, the item's move, whose source the folder move took
away, is superseded, and the next pass moves the item and then deletes the
folder at its new path, so one sync converges and nothing is deleted before
the item has left (`mv docs archive; mkdir docs; mv archive/sub/inner.txt
docs/inner.txt; rmdir archive/sub`, here or on OneDrive)
(`TestComposeMoves_EntryBelowAFolderOnOneSideOnlyKeepsItsSource`,
`TestWithholdDeletesInsideFolderMoveCycles_HoldsTheDeleteThatClosesTheCycle`,
`TestRunOnce_FileMovedOutOfASubfolderIntoTheRemadeFolderWithTheSubfolderRemovedConverges`).
A cycle of another kind closes where the new folder can
only be made after the moving folder has moved (`y` moved to `x/y` and made
again, `x/a.txt` moved into the new `y`, and `x` moved to `x2`): `x`'s move
waits for `a.txt` to leave, `a.txt` for the new `y`, and the new `y` for
`y`'s move, which, composed to `x2/y`, waits for `x`'s. On OneDrive that last
cycle has an order: a file directly in a folder OneDrive moves leaves after
the folder's move, from its source rebased below the folder's new path
(`leavesAfterItsFolder`), as OneDrive moves an item into its parent by ID
and lands it in either order, so `x`'s move no longer waits for `a.txt`. A
file further down still leaves first: the folders between it and the moving
folder may be deleted or moved on their own, and their deletes wait for it
at their paths as the plan has them
(`TestComposeMoves_AFileLeavingAFolderMovedOnOneDriveMovesAfterIt`,
`TestPlannerPairs_AHeldFolderMovedIntoARenamedFolderRunsOnceItsNameIsFree`).
Anything that cannot be ordered is
deferred, with a Debug notice, and the next pass, which observes the moves
this one made, plans it simply. An item is decided after the item its target
hangs off, which is shallower, after a folder whose move vacates the path it
lands at or below, an ancestor of its source, and, as a file OneDrive moves
there, after every folder OneDrive relocates; each of these waits ends with
the decision of the item waited for. An item also waits while another item
holds its destination, whatever that item does: until a move of the plan
vacates the destination, until one item of a swap or rotation is parked
beside its source, or until the end of composition defers it. So the pending
items are decided in turn unless their waits loop. The loop of a folder
whose target hangs off an entry that waits for it, as one that lands below
the path the folder's move vacates (a folder moved into its own former child:
`mv docs tmp; mkdir docs; mv tmp/sub docs/sub; mv tmp docs/sub/docs2`, here or
on OneDrive), has no order of plain moves: the new folder takes the path only
once the folder has left it, and the folder lands below the entry. The folder
moves aside first, to a free `<name>.swap-N` beside its source on that side
as a swap's parked item does, carrying the entry, and both wait; the next
pass makes the new folder, moves the entry into it and the folder below the
entry (`parkOneVacatingLoop`,
`TestComposeMoves_FolderMovedIntoItsOwnFormerChildMovesAsideFirst`,
`TestRunOnce_FolderMovedIntoItsOwnFormerChildConverges`). Before, both were
deferred on every pass and a one-shot sync ended with them unfinished. The
loop of an entry that OneDrive moved into a folder which then took the
entry's old path (`mv p x/p, mv x p`, `mv p/f x/f, mv x p/f`, `mv z p/z,
mv p z`) holds on this computer only: the folder's move waits for the entry
to leave its destination, and the entry's target hangs off the folder. The
entry moves first into the folder at the path the folder has here (`p` to
`x/p`), which is already its place under its target parent, and the
folder's move, waiting for the path the entry left, carries it to its
target in the same plan (`moveOneIntoItsWaitingFolder`). The entry's move
carries `MovedRemotePath`, OneDrive's path for it, and lands in the folder
the folder's move carries, not in a new folder at the path that move
vacates, so it does not wait for that move (`landsInTheFolderMoveCarries`).
The same rotation made here is replayed on OneDrive the same way, the
entry's remote move carrying `MovedLocalPath`, where this computer holds it,
since OneDrive too refuses a move onto a name a sibling holds
(`TestComposeMoves_AnEntryMovedOnOneDriveIntoAFolderThatTakesItsPathMovesIntoItHereFirst`,
`TestRunOnce_ARotationThroughNestingMadeOnOneDriveConverges`,
`TestRunOnce_AFolderRotatedThroughNestingHereIsRotatedOnOneDrive`). The loop of a
folder whose OneDrive move lands in a folder made here at the path OneDrive
still gives another folder, whose own target lies below the first one's
(here `H` moved aside, `Y` into a new `H` as `H/Y`, and the old `H` into
`H/Y/f/a/H`), has no order either: the first move is taken back while the
holder stays at the path (`takeBackHeldLandings`), and the holder's move
waits for the first. The holder moves aside on OneDrive first
(`parkedFirst`), which frees the path; the folder made here is then made on
OneDrive after that move, though the holder waits for a later pass
(`movedAwayOnOneDrive`), the first move lands in it, and later passes move
the holder below it. A holder that cannot be parked has the move taken back
as before (`TestComposeMoves_AFolderWhoseTargetLiesBelowAMoveIntoAFolderAtItsPathMovesAsideFirst`,
`TestPlannerPairs_AFolderMovedIntoANewFolderAtThePathOfAFolderMovedBelowItConverges`,
`TestRunOnce_ThreeFoldersMovedIntoEachOthersNewFoldersConverge`). Before,
every sync left all three moves for the next one. A local move whose target
lies inside the destination of a local folder move of the plan, from a
source outside the folder, lands inside the folder at its path here first,
and the folder's move carries it, when an item leaving the folder needs the
path it vacates (`landsInsideBeforeItMoves`; the folder's move waits for it,
`landsInTheFolderMoveCarries`): a file parked inside a folder for a swap on
OneDrive, the file at its target moving into the folder's destination, and
the folder moving waited on each other in a cycle dropped on every pass
(`TestComposeMoves_AFileWhoseTargetIsInAMovingFolderLandsInsideItFirst`,
`TestRunOnce_AParkedFileLeavesItsFolderBeforeTheFolderMovesOnOneDrive`).
And a folder whose local move lands on the path of a new folder made here
that holds the folder's own source (here `x` moved into `Q` as
`Q/renamed-x`, a new `x` made and `Q` moved into it, while OneDrive moved
`Q` into the old `x`) can never land: the new folder is renamed to a
conflict copy, kept with all it holds, and the next pass moves the folder
out of it onto its path; the creates below it wait with it
(`asideFolders`,
`TestComposeMoves_AFolderInsideTheNewFolderAtItsPathMovesItAsideFirst`,
`TestRunOnce_AFolderMovedBackOutOfANewFolderAtItsPathKeepsTheNewFolderAsAConflictCopy`).
Before, the folder waited for good and the creates below it failed on every
pass, their parent unknown to the baseline. The conflict copy is left empty
on both sides once the items leave it: data-safe, and the user removes it. The loop of a
folder whose destination is held by a file that waits for it, as one of the
folders OneDrive relocates, lasts until the move that vacates the file's
target commits: the pass defers both, and a pass in which the file lands
where no move vacates plans both
(`TestComposeMoves_FolderMovedOntoAWaitingFilesPathWaitsOnePass`). Items whose
paths touch unavailable truth are frozen with their subtree. Creates that would
land on a path a composed item still holds this pass are dropped until the item
has moved, and so are creates on OneDrive below such a path whose source
here does not lie in that item (`belowAnotherItemsFolder`): two folders at one
path, told apart by the item that holds it. Here `x` renamed `x2` while
OneDrive keeps it at `x` (the held start), and a new `x` with a folder `sub`
in it made here: the create of `sub` on OneDrive, sent, was made inside the
old `x`, carried to `x2/sub` once `x` moved, and moved back by the next pass,
and the upload of a file made here in a folder OneDrive deleted and moved
another folder onto (`rm p, mv x p` on OneDrive, a file made in `p` here)
went into the moved folder. Such a create waits, with a Debug line naming the folder, as the move
into the new folder does, and is held work
(`TestDropCreatesOntoComposedItems_ACreateBelowAnotherItemsFolderWaits`,
`TestPlannerPairs_AFolderMadeInTheHeldFoldersPlaceWaitsForItToLeave`). The
twin here stays as it is: composition plans a move into the folder made here
inside the item that still holds the path, the item's move carries both, and
the next pass moves them where they belong, in one sync
(`TestRunOnce_FolderMovedOnOneDriveIntoNewFoldersWhereAFolderMovedDeeperLeftConverges`);
held, the create left that move without its folder. And so is a write here rebased from below a folder's OneDrive path
to where the folder sits here while the folder's OneDrive move waits for a
later pass. The rebase writes into the folder as if OneDrive had moved it: a
download that applies a move OneDrive made into the folder wrote a second copy
of the item while it stayed at its old path here, uploaded again as a new
file, and a file OneDrive made in the folder under a name it takes for that of
a file made here in it was downloaded beside that file, so the sides never
met. A file OneDrive made there under a free name waits too, and arrives in
the pass that moves the folder; a write SQLite reconciles at the folder's path
here is not rebased and not held. Each held write is logged at Debug with the
folder's path here and on OneDrive
(`TestRunOnce_AFileMovedOnOneDriveIntoAFolderWhoseMoveWaitsIsMovedHereOnce`,
`TestRunOnce_AFileMadeOnOneDriveInAFolderWhoseMoveWaitsIsDownloadedOnceItMoves`,
`TestPlanBuiltActions_AHeldRebasedCreateLogsTheFolderItWaitsFor`).
Deletes then follow one rule on each side
(`withholdDeletesOfHeldWork`): a delete waits while work held for a later pass
has its source at or below the deleted path there. Held work is an item's move
that waits for a later pass, for whatever reason, or whose composed move the
keep-both trim removed; what another step leaves for the next pass: a create
that waits with such a move (the new entry at a path its item was renamed away
from), work below a keep-both path, a create onto a path a composed item
holds, an action caught in a dependency cycle; and whatever the rule itself
holds. The source of a move is the
item's path on its side; the source of a create is the synced item whose
content it writes at another path on that side: the file here that an upload
carries, when SQLite could not pair its rename because OneDrive keeps another
item at the new path, or the item OneDrive keeps that a download fetches.
Whatever waits for a held delete waits with it, such as the upload of a file
made at a folder's path or at a name OneDrive takes for it, and is held work in
turn. The delete is released by the pass that plans the held work: nothing
holds it then, and it waits for that work through the ordinary dependencies, a
folder delete for the moves out of it and a delete for the create that writes
its content (Dependency Build), so a failure of that work, or of what it waits
for, skips the delete too. Where such a create waits for the delete itself, no
dependency orders the two, and a create caught in a dependency cycle holds
the delete as work held for the next pass. So does a create of the plan that
waits for the delete only through the move of a folder above the deleted file,
which children leaving first orders after the delete, when it lands
elsewhere than where the plan's moves carry the file
(`deletesAheadOfTheirContent`): held, the delete lets the move carry the file
along, the create runs after it, and the next pass deletes the file at its new
path; a create at that path needs the file gone first. (`P` renamed `Q` here
and `Q/f` moved up to `P` was such a case; `f`, found by its identity and
synced content on the path of a folder this computer moved away, now pairs
as a move, `pathOfAnItemMovedAwayHere`, so `P`'s move carries it and it moves
up on OneDrive as the same item, nothing deleted.) The moves are followed from the file's path: the
move of the nearest folder above it, then the nearest move above where that
one puts it that waits for it (on OneDrive a subfolder's move, rebased below
its folder's new path, runs after the folder's; here a subfolder moves
first), each found by its source, so the check costs the depth of the path
(`carriedPath`, `TestCarriedPath_FollowsTheMovesThatCarryTheDeletedEntry`,
`TestRunOnce_AFileEditedInASubfolderMovedWithItsRenamedFolderIsUploadedOnce`).
Run, such a delete removed what the held work still needs: a folder delete on
OneDrive took a held item into the recycle bin and the next pass made it again
as new items, one here failed, as the folder was not empty (`P/f` moved here
into a new `x` and `P` deleted while `f`'s move waits for `x`, or the same made
on OneDrive), and the delete of a renamed file's old name on OneDrive left no
copy of it there while its upload waited (`z.txt` renamed onto `P`, or onto
`P/f`, which `f`'s waiting move leaves). A move of a folder above such a
delete, on its side, does not wait with it although it waits for the deletes
below its source: it carries the folder and the held work along, and the next
pass plans the delete at the new path. Every delete held so is logged at Debug
with the path of the work it waits for
(`TestPlanBuiltActions_ADeleteAboveAMoveHeldForTheNextPassWaitsWithIt`,
`TestPlanBuiltActions_AHeldDeleteLogsTheItemItWaitsFor`,
`TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaits`,
`TestRunOnce_AFolderDeletedOnOneDriveStaysHereWhileAMoveOutOfItWaits`,
`TestRunOnce_AFolderRenamedHereIsRenamedOnOneDriveWhileAMoveOutOfAFolderDeletedInItWaits`,
`TestRunOnce_AFolderRenamedOnOneDriveIsRenamedHereWhileAMoveOutOfAFolderDeletedInItWaits`,
`TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaitsBelowAKeptConflict`,
`TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveUntilItsUploadRuns`,
`TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveWhileACreateBeforeItsUploadFails`,
`TestRunOnce_AFileMovedUpOntoItsRenamedFoldersPathStaysOnOneDriveUntilItsUploadRuns`,
`TestRunOnce_AFileRenamedHereOntoADeletedFileInAFolderMovedOnOneDriveStaysThereUntilItsUploadRuns`).
A removal of synced content also waits, on either side, while the other side
holds the file at another path and no create of the plan writes its content
on the removal's side (R-2.17.11, `deletesOfUncarriedContent`,
`planner_content_holds.go`). A removal is a delete of the synced file, or a
download of another item over it here (OneDrive moved the file away and put
another item at its path), which therefore takes part in the content edges
of Dependency Build like a delete: it waits for the download that writes the
moved file's content where the item went and is skipped when that download
fails. Where the file is comes from the snapshots, by the key the removal
carries (a local identity and hash for a delete on OneDrive, an item ID here); a file
the other side already holds at the same path with the same content is not
counted, so a removal after that content is synced elsewhere runs. The shape
this catches: the other side moved the file and the move did not pair, and
the comparison set the file's new place beside an entry of another item, so
nothing carries the content this pass: from a held folder's place, `f`
renamed here into the new folder `x` while OneDrive made a file at the same
path in the old `x`, which sits at `x2` here, and the delete of `f`'s old
copy on OneDrive removed its only copy there until a later pass uploaded it.
Held, it is planned with the upload of a later pass and waits for it. Each
such removal is logged at Debug with the path where the content is
(`TestPlanBuiltActions_ARemovalOfContentTheOtherSideMovedWaitsForItsCreate`,
`TestBuildDependencies_ADownloadOverASyncedFileWaitsForTheDownloadOfItsItem`,
`TestPlannerPairs_AFileRenamedIntoAFolderInAHeldFoldersPlaceKeepsItsOneDriveCopy`,
`TestPlannerPairs_AFileOverwrittenHereWaitsForTheDownloadOfItsMovedContent`).
Where the create that writes the content needs the path the removal frees,
no order keeps the content: the upload of a file moved up onto its deleted
folder's path waits for the folder's delete, which waits for the delete of
the file's synced copy below it, and the upload of a file moved onto another
file's path while a folder made here takes that file's name waits for the
folder's create, which waits for the delete at its path. The content edge
would close a cycle and is left out (addContentCarrierDeps), and with the
upload, or what it waits for, refused, OneDrive was without the content until
a later pass uploaded it again. Such a delete on OneDrive is planned as a move
of the item to a free temporary name, named as a parked move's
(`MoveSwapInfix`), in the nearest folder above it that no folder delete of
the plan removes (R-2.17.12, `setAsideRemovedContent`,
`planner_set_aside.go`): the move frees the path as the delete did, and what
waited for the delete waits for the move (a folder delete for the moves out
of it, a create at a path for the move that vacates it). Its local truth is
the file the upload reads (`MovedLocal` at `MovedLocalPath`), which the
worker's freshness check reads where the file is, as for any move of an item
whose local copy sits elsewhere in the snapshot, so the executor needs no
change. The move counts as a rename, so the one-shot sync runs the next pass,
which deletes the parked item once the upload holds the content, or, with
the upload refused, pairs the file here with the parked item, whose
baseline row the move re-paths with the file's local identity, and moves it
back: still the item it was
(`TestPlannerPairs_AFileUploadedOntoThePathItsDeleteFreesIsSetAsideOnOneDrive`,
`TestAsidePath_ParksAboveTheDeletedFoldersUnderAFreeName`,
`TestRunOnce_AFileMovedUpOntoItsDeletedFoldersPathStaysOnOneDriveUntilItsUploadRuns`).
Only deletes on OneDrive are set aside; the same shape here (a download over
a path a local delete frees) has no run in the exhaustive check.
The rule is checked over every pair of short histories on a small tree
(`TestPlannerExhaustive_ShortHistoriesOnBothSidesKeepContentAndConverge`), and
again, for every pair whose passes delete something, with each create of each
pass refused once: a pass removes from a side only content the user deleted
or overwrote, or that the pass keeps as a conflict copy or a version, the
sides converge, the last plan holds nothing, and a delete nobody else touched
happens (a synced item in a folder the user deleted with it, which the other
side wrote into, stays with that folder). A run that breaks one is listed, with what it breaks,
in `internal/sync/internal/syncplan/testdata/exhaustive/pairs.golden`, which lists none now. The check plans with the
real planner and store but executes through a model, which compares content
between passes, not between the actions of one, and fails only creates. A
remote move whose local copy sits elsewhere in the snapshot carries
`MovedLocalPath`, and a local move ordered before its folder's move carries
`MovedRemotePath`, so freshness reads the snapshot where the item is.

#### What the exhaustive check's broken runs show

`pairs.golden` lists no run: every pair keeps every invariant. The runs it
listed (a run names its start, the two histories, the refused create if any,
and the invariants it breaks) fell into the twelve shapes below, each
recorded with what broke it and how it was fixed, or why it was right as it
is; a run that breaks an invariant again belongs to one of them or opens a
new one, and a fix removes its lines from the file. The refused-create variants of a run break what
the run itself breaks, so they belong to its shape. 58 runs of shapes 4, 7
and 8 were the check's own: its model of OneDrive gave a remote folder
create the item the action's baseline row names, wherever that item now
was, so a folder made here again at a path OneDrive had moved the synced
folder away from was bound to the moved folder and nothing could be
uploaded into it. The executor creates the folder by its parent and name
and adopts only a folder OneDrive already has there (`createRemoteFolder`),
and so does the model now; those runs converge with the planner as it is.
The model also kept a downloaded or uploaded file's old baseline row beside
the new one, while the store keys the baseline by item ID and moves the
row; it now moves it too. And a move in the model re-pathed the baseline
from the action's `OldPath`, the moving side's path, where the store's
commit re-paths it from the row the item ID names (`commitMove`), so after
a move here to OneDrive's place of an item the two sides had put at
different paths, the model kept the item's row at its old path and every
later pass re-keyed it back and forth: that made all of shape 8, all of
shape 9 and two refusal variants of shape 6, which converge now too. A
baseline update in the model wrote its row at its path beside the row the
item had elsewhere, where the store's upsert keys the row by item ID and
moves it (`commitUpsert`), so the plan repeated the update on every pass:
that made all of shapes 4 and 7. And a local delete in the model checked
the file against the row at its path, which a download of OneDrive's move
of the same item earlier in the pass had moved away, where the executor
checks it against the baseline the plan carries (`verifyLocalDeleteContent`):
the model refused deletes the executor runs. Both now follow the store
and the executor. A folder delete in the model also removed the baseline
rows below the folder, where the store's commit removes the row at the
path alone (`commitDelete`): a synced item that had left the folder lost
its row, and the next pass read its two copies as two new files. 10 runs
of shape 12 were that (from the held start, `rm p` on either side while
OneDrive moved `p/f` into `X2`); they converge now. The check also counts a duplicate now (shape 12). And a remote folder
create, here or on OneDrive, wrote its row at its path beside the row its
item had elsewhere, where the store's upsert keys the row by item ID and
moves it; the plan made the same folder again on the next pass.

1. A rotation through nesting made on OneDrive (no runs left).
   One entry moves into a folder, which then takes the entry's old path
   (`mv p x/p, mv x p`, `mv z p/z, mv p z`, `mv p/f x/f, mv x p/f`),
   whatever this side did elsewhere. Here the entry still holds the path
   the folder's move lands on, and the entry's target hangs off the folder,
   so the two moves waited on each other and stayed held for good. Fixed:
   the entry moves first into the folder at the path the folder has here,
   and the folder's move then carries it (Move Composition,
   `moveOneIntoItsWaitingFolder`), and 51 of its runs converge (48 from
   the synced tree, 3 from the held one). Fixed too, from the held start,
   where the folder made in the held one's place takes part: OneDrive
   moves `p/f` into its new `X2` and `X2` onto `p/f` (through `x` it
   converges since the item ID places `f`, `sitsAtAnotherSyncedItemsPath`)
   while here `f` already sits in the new `x`, so there is no move into the
   folder to make first, and OneDrive's place for `f` wins: SQLite reads
   `p/f` here only as the source of `f`'s move, so nothing made OneDrive's
   folder there, and `f`'s move into it failed for want of its parent on
   every pass; the folder is made here now
   (`createFoldersOneDriveMadeAtAMovedSource`, above). And
   `mv x p/x, mv p x`, where `p` lands on the new `x` made here, empty since
   `f` left it: the two folders at `x` merge by path (R-2.17.13, above).
2. A rotation through nesting split across the two sides (no runs left): `p` moved into `x` here while OneDrive moved `x` into
   `p`, with a third move onto a path they vacate or a folder made again at
   the old path. Composition keeps OneDrive's nesting and undoes this
   computer's (R-2.17.6, D-055), which is the move conflict resolved with
   both entries kept. Fixed where OneDrive's third move lands on the path
   one of the two left (`mv x p/x, mv z x` there, `mv p x/p` here): this
   computer's move of `p` out of `x` waited for the move onto `x` by the
   rule that work below a move's destination runs after it, which a local
   move's source, never rebased, does not need; the three moves were a
   cycle dropped on every pass, and run now in one plan (Dependency rules
   below). Fixed too, each with its own device:
   - a third move that carries an entry of the folder that leaves
     (`mv p/f x` there) was ordered before that folder's move, as children
     leave first, while it can only land once the folder has gone: a local
     move onto the path a folder above it vacates now waits a pass
     (`landsWhereAFolderAboveItLeaves`), which finds it where the folder's
     move carried it, and the folder no longer lands inside itself, at the
     path a folder it holds has now (`landsInsideBeforeItMoves`);
   - an entry that left the folder here for the path the folder above it
     must take back (`mv p/f x`, `mv z x` here; `mv z p/z, mv x z`,
     `mv z x/z, mv p z` there) held that path while its own target hung off
     the folder: it moves first into its folder where that folder is now
     (`moveOneIntoItsWaitingFolder` through
     `holdsTheLandingOfAFolderAbove`), and a local move planned so, at the
     paths folders have here now, is never read as landing in a new folder
     at a vacated path (`runsInTheLocalSnapshot`);
   - a synced file moved here onto the path of the folder whose move is
     undone (`mv z p` here) read as new content there while OneDrive still
     held it; it pairs as a move by its identity and synced hash
     (`movedOntoAnotherItemsPath`, `pathOfAnItemMovedAwayHere`), and its
     claim on the path is undone with the folder's, so it goes back; a
     OneDrive move from below the path a file's move lands on no longer
     waits for it, as a file holds nothing (`movesAFile`);
   - an empty folder made here at the path the folder must take
     (`mkdir p`) gives way: it is deleted here and the folder lands in its
     place (`emptyNewFolderGivesWay`, `moveDisplaceEmptyFolder`), the delete
     naming no synced item, so its commit records nothing
     (`Action.DeletesUnsyncedEntry`)
   (`TestPlannerPairs_ARotationThroughNestingSplitAcrossTheSidesConverges`,
   `TestBuildDependencies_AMoveLandingInNothingADestinationHoldsRunsFirst`,
   `TestRunOnce_ARotationThroughNestingSplitAcrossTheSidesConverges`,
   `TestRunOnce_AnEmptyNewFolderGivesWayToTheFolderWhoseMoveHereIsUndone`).
   A folder whose local relocation is undone by a collision with another
   item's claim (from the held start, `x2` against OneDrive's `X2`) is
   never merged with, nor given way by, a folder made here: undoing the
   user's rename and deleting the folder it left would undo what this
   computer did twice, so it waits as before. Fixed where the folder made again here is at the path of the
   folder whose move here is undone (`mv x p/x, mkdir x` here,
   `mv p x/p` there), and from the held start, where the held folder moved
   into `p` meets the new `x` (`mv x2 p/x2` here, `mv p x/p` there): the
   undone folder, empty here, merges with the new folder at its place
   (R-2.17.13, above), and `p` moves into it. Fixed, of the same family: a folder moved into different new
   folders at its own path on each side (`f` into a new `f/a/f` here, into
   a new `f/b/f` on OneDrive) left work on every sync for a reason that was
   not the move order. Both sides made a new `f`, and composition plans the
   one made here as the folder made at the path the moved folder left
   (`recreatedSourceCreates`); it was planned without OneDrive truth, since
   the moved folder no longer holds the path there, but OneDrive's own new
   `f` does, so the worker's freshness check superseded the create on every
   pass ("has current remote truth but was absent when planned"), and the
   folder's move, waiting for `f/b` here, never came. The two new folders
   are one folder by path: the create now carries OneDrive's folder as its
   OneDrive truth (`remoteFolderOfAnotherItem`) and adopts it by name, and
   one sync converges
   (`TestPlannerPairs_AFolderMovedIntoDifferentNewFoldersAtItsOwnPathOnEachSideConverges`,
   `TestRunOnce_AFolderMovedIntoDifferentNewFoldersAtItsOwnPathOnEachSideConverges`).
   The check's model now supersedes a remote folder create planned without
   OneDrive truth at a path OneDrive holds, as the freshness check does,
   which shows this shape in its tree too (`mv p q, mkdir p, mkdir p/a,
   mv q p/a/p` against the same through `p/b`), and also a run of the held
   start, `mv x X2/x, mv X2 x` on OneDrive, that the same fix converges.
3. A folder moved onto the path of a folder deleted on the same side (no
   runs left): `rm p, mv x p` or `rm x, mv p x` on OneDrive. SQLite
   reconciles a folder replaced by a folder as unchanged at its path, so
   nothing deleted the old folder here and the move onto its path waited
   for good, and the local rebase sent the deleted folder's child deletes
   into the moved folder (`local_delete x/f` for `p/f`), so with the other
   side writing into the moved folder the deleted entry came back. Fixed
   (`giveWayFoldersReplacedOnOneDrive`, `deletesWhatIsHere`): the old
   folder is deleted here when it holds only what was synced, or renamed to
   a conflict copy with all it holds when it holds more, and the move then
   lands, and the child deletes stay where the children are; 26 of its
   runs converge (24 synced, 2 held). Fixed too: from the held start,
   `rm x, mv p x`, where `x` here is the new folder made in the held one's
   place, not the deleted folder (that is `x2` here and goes), and `p`,
   which `f` left, is empty here: OneDrive's folder and the new one are one
   folder by path, the new `x` takes `p`'s item and the empty `p` is
   deleted here (`mergesWithANewFolderHere`), and the run and its two
   refusals converge.
4. No longer listed: a replacing move that meets the other side's move of
   its entry, on OneDrive `x` moved onto the path of a deleted entry
   (`rm p, mv x p`, `rm p/f, mv x p/f`) while here `x` moved elsewhere
   (`mv x q`, `mv x p/x`). Its runs repeated the model's baseline update
   (above) and converge. They are composed: OneDrive's place wins and the
   copy here moves onto it, the synced entry OneDrive deleted displaced
   (`unpairedRemoteMoveOf`, `moveDisplacement`), so `x` ends once and
   nothing is uploaded. What the type change SQLite reads at the path still
   planned, OneDrive's folder made here beside the move that brings it, is
   left out (`makesHereAFolderTheMoveBrings`)
   (`TestPlannerPairs_AFolderMovedOnBothSidesOntoAnEntryOneDriveDeletedIsMovedHereNotMade`,
   `TestRunOnce_AFolderMovedOnBothSidesOntoAnEntryOneDriveDeletedEndsOnce`).
   Where the moving entry is a file, the content ended twice, which shape 12
   lists.
5. No longer listed, and right as it is: a folder deleted on one side, and
   replaced there by a file moved onto its path, written into on the other
   side (`rm p, mv z p` on one side, a file made in `p` or a folder moved
   into it on the other). The written-into folder is kept whole, as a
   conflict copy here or made again here as OneDrive keeps it, so what the
   other side wrote survives, and the synced file inside it that nobody
   touched is kept with it. The check's delete rule now leaves out a synced
   item in a folder the user deleted with it that the other side wrote into
   (`inAWrittenIntoDeletedFolder`), which takes the delete-undone verdict
   off those runs and off runs of shape 3 that keep such a folder.
6. No longer listed: a move made the same on both sides, its vacated path
   reused on one (`mv p q` on both, and here `mv x p` or `mkdir p`), kept
   being planned. The check's model ran the plan's actions in plan order,
   so a folder create at the old path could record its row there before
   the re-key moved the renamed folder's row away, and the re-key then
   moved the new folder's row; the engine commits a ready publication-only
   action (a baseline update, a cleanup) through the store before it
   dispatches the frontier, so the re-key is always first. The model now
   reduces its frontier as the engine does
   (`TestPlannerPairs_AFolderRenamedTheSameOnBothSidesWithANewFolderAtItsOldPathConverges`,
   `TestRunOnce_AFolderRenamedTheSameOnBothSidesWithANewFolderAtItsOldPathConverges`).
7. Two different entries given the same new name, one on each side (no
   runs left): `mv p q, mv x q/x` here and `mv x q` on OneDrive, or, from
   the held start, the folder made in the held one's place moved where
   OneDrive moved the held one. Its runs repeated the model's baseline update
   (above); with the row keyed by item ID the two folders merge, keeping
   both folders' entries, which converges. Of a folder and a folder made
   here, see shape 2's merges (R-2.17.13). Fixed for a file: a file moved
   here and renamed on OneDrive onto the name of a file made here
   (`mv z p/z, new g` here, `mv z g` there) was downloaded at that name
   beside its moved copy, pass after pass. The new file is now kept as a
   conflict copy and the moved file moved onto its name (Move Composition,
   `localDisplacementAt`).
8. One entry moved to two places, or moved on one side and deleted on the
   other (no runs left). Every run of it was the check's own (above): with
   OneDrive's folder creates modelled by name and the baseline re-pathed by
   item ID, here `mv p x/p, mv x p` while OneDrive renamed `p`, with the
   remote folder create refused once, and the held folder moved or deleted
   here while OneDrive moved it, converge: OneDrive's move wins over this
   computer's (R-2.17.6) and a move over a delete, and both copies stay.
9. A refused folder create of the held start (no runs left): OneDrive moves
   the held folder away, freeing its name for the folder made here in its
   place, and that folder's create on OneDrive is refused once. The plan
   seemed never to empty, but only because the check's model kept the held
   folder's baseline row at its old path after this computer followed
   OneDrive's move (above); with the row re-pathed by item ID, as the store
   does, the next pass creates the folder and the work that waited for it,
   and the plan empties
   (`TestPlannerPairs_AFolderCreateRefusedAfterTheHeldFolderMovesAwayConverges`).
10. A held folder moved into a folder that moves into the new folder made
   in the held one's place (no runs left, nothing done on OneDrive):
   `mv p x/p, mv x2 x/p/x2` or `mv x2 p/f, mv p x/p`. The held folder must
   go into `p` before `p` moves into the new `x`, which can only be made
   once the held folder has left `x`, a rotation through nesting. Fixed:
   the held folder's move on OneDrive lands in `p` where `p` is on OneDrive
   now (`landsInTheFolderItMakesWayFor`), and its local position, where
   `p`'s move carries that path, orders `p`'s move after it
   (`landsInTheFolderMoveCarries`, which keeps it out of
   `movesIntoVacatedPath`), so no move names its parent by item: the
   executor resolves `p` by its path, which `p` still has when the held
   folder moves. Where that name in `p` is still an entry's that leaves for
   the new `x` (`p/f`), the held folder moves aside on OneDrive first
   (`landsBelowTheMoveItHolds`, `takeBackHeldLandings`), and the next pass
   moves it to its target. The engine converged on both through parking
   before, as its passes differ from the model's, which dropped the cycle
   on every pass; the first now takes one move fewer
   (`TestPlannerPairs_AHeldFolderMovedIntoAFolderThatMovesIntoTheNewFolderAtItsPathConverges`,
   `TestRunOnce_AHeldFolderMovedIntoAFolderThatMovesIntoTheNewFolderAtItsPathConverges`).
   The two runs where `p` moves elsewhere (`mv p q, mv x2 q/x2`,
   `mv x2 p/f, mv p q`) are fixed: `f`, leaving `p` for the new `x`, now
   moves after `p`'s move, which no longer waits for it.
11. Content removed from a side in a pass (no runs left), the rule that
   matters most: a removal whose content's create needs the path the
   removal frees, with that create, or what it waits for, refused once. A
   file moved up onto the path of its folder, deleted or renamed here
   (`rm p, mv x/f p` from the held start; `mv p q, mv q/f p` here while
   OneDrive renamed `x` to `q`), or a file moved onto another file's path
   while a folder takes that file's name (`mv z p/f, mv x z` from the held
   start). The upload waited for the delete of what holds its path on
   OneDrive, so the delete could not wait for the upload, and with the
   upload refused OneDrive was without the content until the next pass
   uploaded it. Fixed: the delete is a move of the item to a temporary name
   above the folders the plan deletes, and the next pass deletes it or, the
   upload refused, moves it back (`setAsideRemovedContent`, above). The
   removals a pass ran although the other side held the moved file
   elsewhere and nothing carried it (R-2.17.11) are fixed too.
12. One synced or written content at two plain paths of a side at the end
   (`duplicated`, no pairs left; a conflict copy keeps
   both on purpose, under a name that tells the user, and is not counted).
   Nothing is lost, so the content rules hold, but the user finds a copy
   nobody asked for, and edits to one never reach the other. Two families,
   both fixed. OneDrive moved a file onto the path of an entry it deleted
   (`rm x, mv z x`, `rm p, mv z p`, `rm z, mv p/f z`, 26 pairs), with this
   computer moving or writing beside it, where the move waited while the
   entry stood at its destination here and the file was downloaded there
   while its copy here was uploaded again: the deleted folder is deleted
   here when as synced, renamed to a conflict copy when written into
   (shape 3's fix); a file this computer also moved has its OneDrive place
   read by item ID at the deleted entry's path, where its row, a local
   move, names no OneDrive target (`unpairedRemoteMoveOf`); and a new
   folder made here at the path, from the held start, is kept as a
   conflict copy (`newFolderMeetsAFile`). And a move here onto a path
   another entry vacated or a delete freed (`mv p q, mv z p`,
   `rm p, mv z p`, `mv x q, mv p/f x`) against OneDrive's move of the same
   file elsewhere (90 pairs): SQLite does not pair a local move onto a path
   the baseline gives another item, so it read the file here as new content
   and OneDrive's copy as a new file. The file is now found by its identity
   and synced hash (`movedOntoAnotherItemsPath`), OneDrive's copy by item ID,
   the moves compose, and the upload, download, cleanup and recreated-path
   create SQLite planned for it are dropped (`replacedByAComposedMove`,
   `recreatedSourceCreates`). Fixed too: from the held start, OneDrive moving
   `p/f` into `X2` or `z` onto `x` while here `p/f` is in the new `x` or
   `z` took `p/f` (5 pairs), where the held folder's move is involved:
   two were the model's (above), and three were the planner's. An item
   whose move waits for a later pass keeps its synced row, which the next
   pass needs to know it: a move of another item onto that row's path
   waits with it, since its commit records the other item's row at the
   path and the store removes a row of another item there
   (`baselineRowsOfHeldItems`; OneDrive moved `p/f` into `X2` and `z` onto
   `p/f` while `f`'s move here waited), and a download of such an item at
   a path it never had waits too (`downloadsOntoItsOwnRow`; `z` moved onto
   `p/f` here while OneDrive moved it into `x`, which sits at `x2` here, and
   it was downloaded into `x2` beside its copy)
   (`TestPlannerPairs_AnItemWhoseMoveWaitsKeepsItsRowAndIsNeverCopied`,
   `TestRunOnce_AFileWhoseMoveWaitsKeepsItsSyncedRowAndIsNeverCopied`).
   Fixed, the two synced pairs where two moves meet at one name. Here `p`
   renamed `q` while OneDrive renamed `x` to `q` and moved `p/f` onto `x`:
   the two `q` folders merge as one made on both sides, so SQLite never read
   `x`'s move, and `f` on OneDrive, at a path another synced item had, read
   as a new file; the item ID now places it there, as the path's own item is
   elsewhere on OneDrive (`movedOntoAPathItsItemLeftOnOneDrive`), and the
   moves compose. And here `x` moved into `p` and `p` renamed `x`, a rotation
   through nesting, while OneDrive moved `p/f` into `x`: SQLite read `p` as
   gone, since it pairs a local move only onto a path no synced item had,
   deleted it on OneDrive and uploaded `f` again. A folder found here by its
   identity at the path of a folder this computer moved into it
   (`folderMovedOntoAPathItsItemLeft`) now has that position, the delete
   SQLite planned for it is dropped, and the rotation is replayed on
   OneDrive as one made there is replayed here
   (`moveOneIntoItsWaitingFolder`, either side): `x` moves into `p` at the
   path `p` has on OneDrive, which a remote move into the folder a folder's
   move carries, already held there here (`MovedLocalPath`), runs before
   that move (`landsInTheFolderMoveCarries`), and `p`'s move then carries
   it (`TestPlannerPairs_AFileMovedOnOneDriveBesideAFolderRenamedOrRotatedHereEndsOnce`,
   `TestBuildDependencies_ARemoteMoveIntoTheFolderARotationCarriesRunsFirst`,
   `TestRunOnce_AFileOneDriveMovedOntoAPathItsFolderLeftWhileTheFolderMergedHereEndsOnce`,
   `TestRunOnce_AFolderRotatedThroughNestingHereIsRotatedOnOneDrive`). The
   identity is used only where the displaced folder went inside the one
   that took its path, and every synced entry below both folders is still
   here or paired as a move (`losesAnEntryHere`): an entry edited as it
   moved leaves a delete on OneDrive below its old path that the replayed
   rotation would wait on in a cycle, and that rotation is still taken
   apart by parking
   (`TestRunOnce_AFolderMovedIntoTheFolderThatTakesItsPathConvergesWithAnEditInIt`).
   The other local rotations of a folder onto a vacated path (shape 2) stay
   as SQLite reads them.

A move composed with OneDrive's edit (R-2.17.3). A move and an edit are two
attributes of one item, so when OneDrive edited a file this computer renamed,
or moved and edited a file this computer kept or renamed differently, the
plan moves the item and downloads the edit at the moved path in the same
pass, instead of leaving the edit to the pass after the move commits. The
download waits for the move (work at a move's destination does) and is
planned only when the copy here is proven by its hash to hold the synced
content, so it replaces nothing else; with both sides edited the move runs
alone and the next pass meets the two edits as a content conflict at the
moved path. After a local-origin remote move (`composedRemoteMoveActions`)
the copy is already at the path in the snapshot, and the download, like the
upload of an unhashed move, tolerates the move's new eTag but not a content
change (`contentTransferHasMovedBaseline` in syncexec). After a remote-origin
local move (`composedRemoteEditDownload`, only for a move from the item's own
snapshot path to where OneDrive holds it, nothing displaced) local_state
still records the copy at the move's source, so the download carries that
path as `MovedLocalPath`: freshness reads the copy there, and the overwrite
check proves the target by the planned hash alone, since the rename moved
its inode change time. The reconciliation matrix records the three cells
(`TestPlannerPlanCurrentState_LocalMoveComposesWithTheRemoteEdit`,
`TestRunOnce_LocalRenameAndRemoteEditComposeInOnePass`,
`TestRunOnce_RemoteMoveAndEditComposeInOnePass`). A file moved and edited
here is not composed so: identity alone never pairs a file (R-2.17.1), so it
stays a delete and a create unless OneDrive's move corroborates it (below).

A remote move of a locally edited file is still applied (R-2.17.7): the local
move source must be the same item (type and identity) but its content may
differ; the rename carries the bytes, the outcome keeps the baseline's content
facts, and the next pass uploads the edit as the same item. A move outcome
re-paths the baseline subtree from the row's own previous path (looked up by
item ID), not from `OldPath`, which for a local copy moved to OneDrive's name is
a path the baseline never held.

A file this computer moved and edited is no move here by itself: identity
alone never pairs a file (R-2.17.1). Where OneDrive moved the item to the
very path where the edited file now sits, that move corroborates the
identity, and the file is the item (`editedWhereOneDriveMovedIt`): from the
held start, `f` moved here into the new `x` and edited while OneDrive moved
it into the old `x`, both at `x/f` in different folders. Read as two files
made at one path, OneDrive's version was downloaded beside the edit, which
went up as a new file, and the item was split in two. Paired, the moves
compose, OneDrive's place wins, the local move carries the edit, and the next
pass uploads it as the same item, OneDrive keeping the version it replaces
Where both sides then hold it at one place (renamed alike on both, edited
here), the baseline is re-keyed there with the synced content, and the
conflict copy of the edit and the download of OneDrive's version SQLite
planned for the path as two files made there are left out
(`dropReconciliationOfComposedMoves`), so the next pass uploads the edit as
the same item; the reconciliation matrix records the case
(`TestPlannerPairs_AFileEditedHereThatBothSidesMovedStaysOneItem`,
`TestRunOnce_AFileEditedHereThatBothSidesMovedStaysOneItem`,
`TestRunOnce_AFileMovedAndEditedHereThatOneDriveMovedAlikeStaysOneItem`).

Move pairing requires the item to have left its baseline path (R-2.17.8): an
identity pair is refused while the baseline path still holds the baseline
identity (a hard link), a hash pair while the baseline path still exists (a copy
without identity). When the baseline path holds a new entry (rename and
recreate), SQLite still reports `local_move_source` and composition also plans
the new entry as an upload or remote folder create that waits for the remote
move to vacate the path. Where OneDrive moved the item away too and made a
new folder of its own at the path, the folder made here is the same folder
by path: its create carries OneDrive's folder as its OneDrive truth, so the
freshness check does not supersede it, and the executor adopts it by name
(`remoteFolderOfAnotherItem`). Swaps and rotations are deliberately not paired: the
paths keep their items and exchange content, which is the same end state
without a temporary name.

A remote move onto the path of a synced file OneDrive deleted (a replacing
move, D-058) used to be planned as a download at the destination, which left
the source behind to be uploaded again as a duplicate. The destination's
baseline item now gives way (`moveDisplacement`) when it no longer exists
online: a file whose local copy is provably the synced version is replaced by
the local move (`Action.ReplaceLocalTarget`; the executor re-verifies it through
the descriptor it keeps in the trash, R-6.4.9), an edited one is renamed to a
conflict copy first and uploaded later as a new file, a folder OneDrive
deleted that the plan deletes here too gives way to that delete, which the
move waits for (`moveDisplaceDeleteFirst`, `displaceFoldersDeletedHere`;
the delete's own checks keep a folder that holds anything not as synced,
and the move is then skipped with it), and anything else (a folder kept
here, unknown content) makes the move wait
(`TestPlannerPairs_AFileMovedOnOneDriveOntoAFolderItDeletedEndsOnce`,
`TestRunOnce_AFileMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere`).
A synced folder OneDrive deleted while it moved another synced item onto its
path, which this computer left where it was, gives way whatever moves there
(`giveWayFoldersReplacedOnOneDrive`): holding only what was synced, or less
(`holdsOnlySyncedHere`: every entry below it a synced item at its baseline
path, a file with its synced hash), it is deleted here and the move waits
for that delete; SQLite reconciles a folder replaced by a folder as
unchanged at its path, with a baseline update that would bind the path to
the moved folder, so the planner plans the delete in its place. Holding
anything else written here, it is renamed to a conflict copy with all it
holds (`moveDisplaceConflictCopy`; a folder's conflict copy trims the work
below it like one between types, `keepBothPaths`) and the move lands, and
the next pass uploads the copy as a new folder. Such a delete is never
turned into a folder create to keep it for work below its path
(`deletesAFolderOneDriveGaveAway`): that work, on OneDrive, belongs to the
item that took the path. And a local delete of what this computer holds at
its path is never rebased through a folder's remote move
(`deletesWhatIsHere`): where OneDrive deleted p and moved x onto it, the
delete of p/f removes the local p/f, not x/f
(`TestPlannerPairs_AFolderOneDriveDeletedGivesWayToAnItemItMovedOntoItsPath`,
`TestRunOnce_AFolderMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere`,
`TestRunOnce_AFolderWrittenIntoHereThatOneDriveReplacedByAMovedFileIsKeptAsAConflictCopy`).
Nor is a conflict copy rebased so: it keeps the entry this computer holds at
its path, and where the comparison set that entry beside OneDrive's item in
a folder that sits elsewhere here (a file made in the new folder that took a
held folder's name, and one OneDrive made in the held folder, which is at
`x2` here), the download is rebased to where that folder is, which holds
nothing to keep, and the copy is not planned. Rebased with it, the copy
renamed nothing the first pass and the downloaded file on every pass after
(`TestPlannerPairs_AFileOneDriveMadeInAHeldFolderIsDownloadedWithoutAConflictCopy`,
`TestRunOnce_AFileOneDriveMadeInAFolderWhoseMoveWaitsIsDownloadedWithoutAConflictCopy`).
Where this computer moved the item OneDrive moved onto a deleted entry's
path, the item's own row is a local move and names no OneDrive target; the
item ID places it at that path (`unpairedRemoteMoveOf`), so the moves compose
and OneDrive's place wins
(`TestPlannerPairs_AnItemMovedOnBothSidesOntoAnEntryOneDriveDeletedEndsOnce`,
`TestRunOnce_AFileOfAFolderRenamedHereMovedOnOneDriveOntoAFolderItDeletedEndsOnce`).
A file moving onto a folder here that is no synced item at its place (one
made here) is composed with that folder renamed to a conflict copy, which
the path's reconciliation keeps anyway (`newFolderMeetsAFile`).
A synced folder whose local move lands where OneDrive holds it (OneDrive
moved it, or this computer's own move of it is undone, as in a cycle), on a
folder here that is no synced item (one made here after the synced folder at
the path left or went), while the folder's own copy here holds nothing, is
merged with that folder by path, as two folders made at one path on both
sides are (R-2.17.13, `mergesWithANewFolderHere`): composition records a
merge instead of the move, which binds the item's baseline row to the path
with the new folder's identity and deletes the empty copy here
(`composedMergeActions`), the baseline update SQLite planned for the item
left to it, and the executor's delete refusing the copy if it gained an
entry since the scan. Before, the move here waited for good for the new
folder to leave its destination. No local move of the plan may carry the
copy or the destination, the parent of the path must be the item's target
parent here, and a copy holding anything is left to the move, which waits
(`TestComposeMoves_AFolderLandingOnANewFolderHereMergesWithItOnlyWhenItsCopyIsEmpty`,
`TestPlannerPairs_ANewFolderHereMergesWithTheFolderOneDriveMovedOntoItsPath`,
`TestPlannerPairs_AFolderWhoseMoveHereIsUndoneMergesWithANewFolderAtItsPlace`,
`TestRunOnce_ANewFolderHereMergesWithTheFolderOneDriveMovedOntoItsPath`,
`TestRunOnce_AFolderWhoseMoveHereIsUndoneMergesWithANewFolderAtItsPlace`).
A folder OneDrive put at the path a synced item left by this computer's
move, the path empty here, is made here
(`createFoldersOneDriveMadeAtAMovedSource`): SQLite reconciles the path as
the move's source alone, which plans nothing, and a local move into the
folder, OneDrive's place winning, failed for want of its parent on every
pass. Its create replaces at the path the baseline row of the item that
left; the item's move, which waits for the create, records the item by item
ID where it lands
(`TestPlannerPairs_AFolderOneDriveMovedOntoThePathAFileLeftHereIsMadeHere`,
`TestRunOnce_AFolderOneDriveMovedOntoThePathAFileLeftHereIsMadeHere`).

SQLite pairs a local move only onto a path no synced item had. A synced
file gone from its path on both sides that this computer moved onto a path
another synced item had, vacated by its move or freed by its delete
(`mv p q, mv z p`, `rm p, mv z p`), was read as new content there, and
OneDrive's move of it elsewhere as a new file: downloaded at OneDrive's path
and uploaded at this computer's. Composition now finds it by its filesystem
identity and synced hash (`movedOntoAnotherItemsPath`; an edited file, or a
new file on a reused inode, stays new content) and OneDrive's copy by item ID,
so its moves compose; the upload at its path, the download of it, the
cleanup of its baseline row and the create planned for the entry at a
renamed item's old path are dropped (`replacedByAComposedMove`,
`recreatedSourceCreates`)
(`TestMovedOntoAnotherItemsPath_NeedsTheIdentityAndTheSyncedContent`,
`TestPlannerPairs_AFileMovedHereOntoAPathAnotherItemHadAndElsewhereOnOneDriveEndsOnce`,
`TestRunOnce_AFileMovedHereOntoAFolderDeletedHereAndRenamedOnOneDriveEndsOnce`).
A download of an item the plan moves on OneDrive is dropped too: the move
puts OneDrive's copy where this one is. A file at the destination that
is not the baseline item at all (the node has no local position: another
program saved a new file at the name after a replacing move took the old one
into the trash and before its rename, so the rename refused and the move was
superseded) is the user's new content: it is renamed to a conflict copy first
in the same way and the move is still composed (`localDisplacementAt` reads
the displacement from the deleted item whose baseline path the file holds),
so the moved item is neither downloaded beside its old copy nor its old copy
uploaded again as a new item
(`TestRunOnce_ReplacingMoveFindingANewFileAtTheFreedNameStillMoves`). So is
a new file at the path of a synced file whose copy moved elsewhere here,
whatever OneDrive holds (`composedDisplacement`, `movedAwayHere`): it is not
the item either, and waiting for it to leave held two moves on each other's
destinations for good, the item moved here and swapped on OneDrive with a
file whose copy had to move onto the new file's path
(`TestRunOnce_ANewFileAtAMovedFilesOldPathGivesWayToASwapOnOneDrive`, chaos
seeds 162 and 385). The item's own copy at its new path never gives way. A
folder at that path still makes the move wait. A new file at a path no synced item had
gives way the same way: a file moved here and renamed on OneDrive onto the
name of a file made here (`mv z p/z, new g` here, `mv z g` there) is a create
conflict at that name, which SQLite pairs as no move, though the item ID
proves the item sits there and OneDrive's move is shadowed by the one here.
Composition takes that path as the file's position on OneDrive, the file
moves onto it here, and the new file is renamed to a conflict copy first
and uploaded as a new file. Before, the moved file was downloaded at the
name beside its copy here, pass after pass. Only a file meeting a new file
composes so: two folders at one path merge, and an OneDrive move the other
side did not also make is reconciled as a delete at the old path, which no
composed move takes back
(`TestRunOnce_AFileMadeHereUnderTheNameOneDriveGaveAMovedFileIsKeptAsAConflictCopy`). Reconciliation downloads and
conflict copies of an item composition moves locally are dropped, and so is a
metadata baseline update of a moved item (identical content at the destination,
new eTag), which recorded the replaced file's local identity for the moved item.
A baseline update of an item whose move, one the mode runs, waits for the next
pass is dropped too, so the item keeps the baseline row the plan's moves leave
it: a folder move of the plan can carry the item, while the update, addressed
at the path the item had and waiting for the folder made there, would record
it at that path again, and the next pass would read it as moved on the other
side and move it back into the folder
(`TestDropReconciliationOfComposedMoves_HeldMoveKeepsItsBaselineRow`).

`buildDependencies` (`planner_dependencies.go`) orders moves by the paths they
carry (`addMoveDeps`): work at or below a move's destination runs after it,
except a local move from below another local move's destination, which runs
before it: a local move is never rebased, so its source is in what holds that
destination now, which leaves it first
(`TestBuildDependencies_ALocalMoveFromBelowAnotherLocalMovesDestinationRunsFirst`,
`TestRunOnce_ARotationThroughNestingSplitAcrossTheSidesKeepsOneDrivesNesting`),
and so does a local delete or a cleanup below a local move's destination: it
removes what holds the destination now, a folder deleted here that the move
waits for
(`TestBuildDependencies_ALocalMoveOntoAFolderDeletedHereWaitsForItsDelete`); a
move onto a path another move of the same side vacates waits for it, and so
does a move into a folder below that path from a source outside it, whether
or not the plan creates that folder: what it lands in takes the path only
after the move. Both paths are compared as OneDrive compares names, so a move
into a new `X` waits for `x`'s move away, a second guard beside composition's
(`TestBuildDependencies_MoveIntoThePathAnotherMoveVacatesWaitsForIt`), except a local
move composition planned into the moving folder itself, which runs first
(`landsInTheFolderMoveCarries`); a move
waits for same-side moves out of its source, for local writes under a local
move's source and for remote deletes under a remote move's source; a create at
the path a move vacates waits for the move; the delete of a destination's old
occupant runs first. A move from a folder's source into or below a folder the
plan creates at the path the folder move vacates (`docs` moved to
`photos/photos` and re-created, `docs/a.txt` moved to `docs/b.txt`) would wait
for the create, the create for the folder move and the folder move for the
child leaving first; one onto the vacated path itself would wait for the
folder move directly. Either is a cycle. Composition names such a move's
source where the folder move puts it (`photos/photos/a.txt`), so it waits for
the folder move and the create and holds neither, or holds it for the next
pass. It holds it also where that source would lie below the source of
another folder move of the plan, which the rule for children leaving first
would then make wait for it while the path that folder leaves is filled only
after it moves: a cycle
(`TestBuildDependencies_FileRebasedBelowAnotherFoldersVacatedPath`). Where
composition neither rebases nor holds the move (Move Composition), the one
exception to children leaving first orders the first kind: a same-side move
from the folder's source into or below a folder created at the vacated path
on that side does not hold the folder move back, so it runs after the folder
move from its old source, is superseded, and the next pass moves it from
where the folder move put it. The exception needs that create, so it does
not reach a move onto the vacated path itself, which stays a cycle
(`TestBuildDependencies_MovesThroughAFolderRecreatedAtAVacatedPathHaveAnOrder`,
`TestBuildDependencies_ChildrenLeaveFirstUnlessTheyMoveIntoTheRecreatedSource`,
`TestBuildDependencies_FileMovedOntoAVacatedPathFromAFolderMadeInsideIt`).
`ExecuteLocalMove` requires the destination's parent to exist and reports a
missing one as stale instead of creating it (D-054). A local folder create's
outcome never moves the item in `remote_state` (it may run at a local path
that differs from OneDrive's until a parent's local move commits).

A delete waits for each create on its side that writes the deleted file's
content at another path (`addContentCarrierDeps`): on OneDrive the upload of
the file here that has the file's synced local identity and content hash,
which SQLite plans when it cannot pair a rename because OneDrive keeps
another item at the new path, and here the download of the file's item. The
kernel gives a deleted file's inode to the next file made, so the identity
alone does not name content: a new file with a reused inode and other
content, or with no hash known, carries nothing
(`TestBuildDependencies_AReusedInodeWithOtherContentCarriesNothing`). So the content stays on that
side until it is written again, and a failed create, or a failure of what it
waits for, skips the delete. An edge whose create already waits for the
delete through the other rules would close a cycle and is left out, as for
the upload of a file left unpaired in a folder renamed here, which waits for the
folder's move, which waits for the delete of the file's old copy
(`TestBuildDependencies_ADeleteWaitsForTheCreateThatWritesItsContent`,
`TestBuildDependencies_ADeleteDoesNotWaitForACreateThatWaitsForIt`).

A baseline update below the path a baseline rekey moves a subtree to (a folder
both sides moved alike) waits for the rekey (`addRekeyDestinationDep`). The
rebind of a folder OneDrive deleted and made again below the moved folder
(R-2.10.49) ran first, took the path a row of the subtree moves to, and the
rekey failed on the store's unique path every pass
(`TestBuildDependencies_AnUpdateBelowARekeyedFolderWaitsForTheRekey`, chaos
seed 324).

### Dependency Build

Every dependency rule relates an action to actions at its own path, at the
folders above its path or move source, at the OneDrive name key of its path,
or with the content it writes or removes. `buildDependencies` therefore builds
a `dependencyIndex` once per plan (actions by path, moves by source and by
destination, every move by the name key of its source, remote deletes and
moves by the name key of the path they free, uploads and downloads by the
content they write, and, per folder delete and per move, the work below it,
found by walking the folders above each path) and each rule applies its pair
predicate to the candidates the index yields, so the build costs the actions
times their path depth, not the actions squared. The content rule then finds
the edges that can close a cycle in one pass over the graph's strongly
connected components, only in a plan that has such an edge, and decides
those one at a time, each against the edges kept before it, so of two edges
that close a cycle only together the first is kept
(`TestBuildDependencies_OfTwoContentEdgesThatCloseACycleOnlyTogetherTheFirstIsKept`,
`TestRunOnce_NamesTradedHereInAnotherCaseAreMovedOnOneDrive`). Each such
decision walks the plan from the create, asking each action for the delete
before walking what it waits for, so through a folder's move it costs the
walk's depth. A scanning implementation that compares every action with every other is kept as the
oracle of `TestBuildDependencies_MatchesScanningOracle`, which holds the
indexed builder to its exact edges over random plans, and
`BenchmarkBuildDependencies` pins the curve
(spec/design/performance-benchmarking.md, Planner Micro-Benchmarks).

### Follow-Up Work

Planning moves leaves work for the next pass on purpose, and the plan says so
in `ActionPlan.FollowUp` (`PlanFollowUp`, R-2.1.7). The executing side uses it
to run that next pass now instead of reporting the run finished. The count is
set where the planner leaves ready work:

- `Renames`: every admitted move, baseline re-key and conflict copy. A move's
  outcome publishes the new path and keeps the baseline's content facts
  (R-2.17.3, R-2.17.7), and SQLite reconciles a moved folder's subtree against
  the paths the move changes, so a local edit of a moved item, an edit OneDrive
  made with a re-key or with a move it does not compose with (Move
  Composition, A move composed with OneDrive's edit), or a local edit inside a
  folder renamed online is planned only once the move is committed. A conflict copy is new local content
  this plan never saw, uploaded next pass.
- `Deferred`: relocations composition held back (`moveComposition.deferred`):
  moves that cannot be ordered this plan, and the item parked at
  `<name>.swap-N` to break a swap or rotation, or to make way for the folder
  made at its own path that it moves into.
- `Dropped`: actions removed until the next pass: creates onto a path a
  composed item still holds or at or below the path of a new folder a
  deferred item moves into at its own path (`dropCreatesOntoComposedItems`),
  a new item's create on OneDrive at a path onto which a folder move of the
  plan carries another OneDrive item (`carriedOntoByRemoteMove`: a folder
  renamed here with a file inside it replaced by a folder, while OneDrive
  edited or made anew that file, reconciles the new folder here and OneDrive's
  file at its old path apart; the rename lands the file at the new folder's
  path, where OneDrive refuses the folder create, and the next pass meets the
  two at one path as a keep-both conflict, R-2.17.10,
  `TestDropCreatesOntoComposedItems_RemoteCreateOntoAnItemAFolderMoveCarriesWaits`),
  creates here at a name an item that stays put here holds under OneDrive's
  name comparison rather than as spelled (`foldedHeldPaths`: on a
  case-insensitive volume, macOS's default, a folder OneDrive made as X2
  while a folder renamed here to x2 waited was made into x2, and OneDrive's
  later rename of X2 carried x2's files away and deleted the old folder;
  a byte-exact volume would hold two names OneDrive takes for one), with the
  creates below such a create as spelled (`dropCreatesBelowSpelled`: a
  download into the holder, spelled as the holder is, keeps its folder),
  and every create below a create it drops on the same side, which has no
  folder to land in (a new OneDrive folder in a folder renamed differently on
  both sides, at the name of a new local file: its child's download, rebased
  below the file, failed every pass and held the folder's move,
  `TestRunOnce_CreateConflictInsideADivergentlyRenamedFolderKeepsBoth`),
  deletes at or above the source of work held for the next pass or ahead of
  the create that writes their content, with what waits for them except a
  folder's move that carries them along (`withholdDeletesOfHeldWork`), work
  below a keep-both path (`trimKeepBothDescendants`), and actions caught in a
  dependency cycle (`isolateDependencyCycles`).

`Renames` is a deliberate superset: not every move leaves content work behind.
Deciding which do would repeat in Go the reconciliation SQLite performs on the
next pass, and a quiet follow-up pass costs one delta request and one local
scan only after a pass that moved something. Every other "next pass" in the
planner is one of these three; work held back by the sync mode or the
delete-safety hold is not follow-up, because another pass would hold it again.

`Deferred` and `Dropped` are counted before the mode partition, so they can
include work a one-way mode holds back. `Unfinished` is the part of them the
plan's mode runs (`moveComposition.deferredRelocationsInMode`,
`countActionsAllowedInMode`). When a one-shot run ends on a plan,
`PlanFollowUp.LeftWhenTheRunEnds` is the work it leaves for the next sync:
`Unfinished` always, since that work is in no action list, and `Renames` only
when the pass bound ended the run, since a rename is an action already
counted and the work behind it is planned by the pass the bound prevented
(sync-engine.md, A One-Shot Sync Reports What It Left, R-6.6.28).

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
(`ApplyDeleteSafetyHold`, run by the engine's planning stage after
`PlanCurrentState`). Per-item checks prove each delete matches what was
synced; they cannot tell an intended mass delete from a wrong tree that
passed every identity check (an fscrypt folder with its key unloaded, a
restored folder without its contents, a complete but anomalous remote
enumeration). So, per direction (R-6.2.5):

- **structural**: when the planner-visible snapshot of one side has no
  non-root entries while the baseline holds items, every delete toward the
  other side is held, however few; `lost+found` (an ext mount root keeps one
  even when the disk is otherwise gone) and the client's own artifacts do not
  count as entries;
- **proportional**: when more than `max(50, 30% of baseline items)` deletes,
  files and folders alike, go toward one side, all deletes toward that side
  are held. The unconfirmed deletes sync already ran toward that side within
  the last 24 hours (`DeleteSafetyWindow`) count with the plan's, against the
  baseline as it stood before them (`delete_ledger`, written and
  pruned by `CommitMutation` in the outcome's own transaction, read by
  `RecentUnconfirmedDeletes` at plan time). Each plan used to be judged alone,
  so a process removing files below the threshold per pass (a failing disk, a
  misbehaving app, a cron job) emptied the drive one small batch at a time and
  was never held;
- **mount point gone**: a delete toward OneDrive of a recorded mount point
  (`mount_boundaries`, R-6.2.15) whose folder is not in the local snapshot,
  or of anything below it, is held however few such deletes there are; the
  plan's other deletes are judged by the two rules above. While the folder is
  there, a disk that is not mounted is a read boundary and plans no deletes,
  and a mounted disk the user emptied is an ordinary delete; a folder that is
  missing is either the user letting go of a disk that is gone for good or a
  restored or re-pointed sync folder without it (after `drive rebind-root`),
  and only a confirmation tells the two apart
  (`TestDeleteSafetyHold_DeletesOfAMissingMountPointAreHeldWhateverTheirNumber`,
  `TestRunOnce_ARebindOntoAFolderWithoutAMountPointHoldsItsDeletes`).

Moves never count. A held delete is removed from the plan together with
every action that depends on it, directly or transitively (the create half of
a type-change replacement, an upload waiting on the delete of a case
variant); dependencies are then rebuilt. Everything else runs.

The hold is re-derived from current truth on every plan, so it stays while
the evidence is unchanged and lifts by itself when the user restores the
files. The engine records the held deletes in `held_deletes` for `status`
(condition `deletes_held`, scope `delete_safety:{local|remote}`) and in the
pass report. `sync --confirm-deletes` releases exactly the recorded held set
(a one-shot run), or the set a running watch's user confirmed by its count and
token (R-6.2.17, `sync-engine.md` "Confirming Held Deletes in a Running
Watch"), matched by path, side and item ID (a delete that knows its
item is approved only by a row naming the same item); the rules are then
applied to every other delete in that pass, so a batch that appeared after the
user looked is held again rather than waved through. The approved deletes
run as confirmed (`Action.DeleteConfirmed`, carried through the outcome to
the mutation as `ExemptFromDeleteHold`), so they never enter the ledger, and the confirmation clears
the ledger of the sides it approved: it reviewed what went before it.
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

## A Tree Being Released Creates Nothing New Locally

A removed shared folder's final drain (`RunOptions.ReleaseLocalTree`) plans for
a mount whose local tree is being released
(`PlannerMountContext.ReleasesLocalTree`). `planBuiltActions` admits its
actions through one rule beside the mode, `leaveOutNewLocalItems`, after
normalization: a download or a local folder create of an item with no
baseline row and no local entry is left out. The mode stays bidirectional,
every other action is admitted as before, and a plan without the flag is the
plan it always was (`TestRunOnce_AnOrdinaryRunStillDownloadsANewRemoteFile`).
A left-out folder stays when an admitted action below it needs it (anything
but a delete or cleanup, as for a preserved parent): a synced item OneDrive
moved into a folder new to the drain moves there, and the release then takes
both (`TestRunOnce_FinalDrainMovesASyncedFileIntoANewFolderAndReleasesBoth`).
What is left out is counted nowhere a run's left work is: not in
`DeferredByMode` and not in the plan's follow-up, so it causes no extra pass
and does not hold the tree; the plan logs its count once, at Debug
(`TestRunOnce_FinalDrainCreatesNothingNewLocally`). An entry the release took
is therefore never downloaded back by a later drain
(`TestRunOnce_ARefusalThatPersistsNeverDownloadsWhatTheReleaseTook`).

## Considered And Rejected: An Identity-Keyed Planner

A second planner that keyed the three trees by item identity (the baseline
row joined to OneDrive's by item ID and to the local row by device, inode and
birth time) and decided each item's position and content in one step ran in
shadow beside this one: compared on every planner-model round, every engine
model pass and every steady-state watch replan, never executed. It was
deleted in November 2026, on measured facts:

- As the executed planner it failed 59 of 2000 planner-model seeds, against
  this planner's 2 on the same seeds (502 and 793, both since fixed).
- Per replan it cost about 5.7 ms and 2.6 MB at 1k files, 64 ms and 25 MB at
  10k, and 0.94 s and 242 MB at 100k, for a debug event no user could
  receive.
- Its one proven win was composing a move with an edit in one pass. That is
  ported: a move in either direction composes with OneDrive's edit (Move
  Composition, A move composed with OneDrive's edit). The other cells it
  planned differently, a file moved and edited here, it paired on filesystem
  identity alone, which R-2.17.1 refuses: where no birth time is recorded a
  reused inode looks like a rename, and the pairing would give an unrelated
  file the item's sharing links and history.
