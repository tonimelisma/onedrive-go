package sync

import (
	"cmp"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Dispatch-order control for tests (D-090). The engine's ready frontier
// normally leaves in dependency-graph order and runs on a pool of workers, so
// a bug that depends on which ready action runs first could pass by timing.
// A test can pin the order -- forward, reversed or a seeded shuffle -- and
// serialize the workers so the order is also the execution order.

// dispatchOrderForward sorts a frontier by action ID, which is plan order:
// the canonical order the other two permute.
func dispatchOrderForward(frontier []*syncdomain.TrackedAction) []*syncdomain.TrackedAction {
	slices.SortStableFunc(frontier, func(a, b *syncdomain.TrackedAction) int {
		return compareInt64(a.ID, b.ID)
	})

	return frontier
}

// dispatchOrderCanonical sorts a frontier by what its actions do (type,
// path, old path), then by action ID. A seeded shuffle permutes this order,
// not plan order: the planner's order of actions that do not depend on each
// other is not part of its contract, and a shuffle of it replayed one seed
// differently run to run (chaos seed 50).
func dispatchOrderCanonical(frontier []*syncdomain.TrackedAction) []*syncdomain.TrackedAction {
	slices.SortStableFunc(frontier, func(a, b *syncdomain.TrackedAction) int {
		return cmp.Or(
			cmp.Compare(a.Action.Type.String(), b.Action.Type.String()),
			cmp.Compare(a.Action.Path, b.Action.Path),
			cmp.Compare(a.Action.OldPath, b.Action.OldPath),
			compareInt64(a.ID, b.ID),
		)
	})

	return frontier
}

func dispatchOrderReverse(frontier []*syncdomain.TrackedAction) []*syncdomain.TrackedAction {
	dispatchOrderForward(frontier)
	slices.Reverse(frontier)

	return frontier
}

// dispatchOrderShuffle permutes each frontier with one seeded generator, so
// the same seed reproduces the same order run after run.
func dispatchOrderShuffle(seed uint64) func([]*syncdomain.TrackedAction) []*syncdomain.TrackedAction {
	var mu stdsync.Mutex
	rng := rand.New(rand.NewPCG(seed, seed^0x5851f42d4c957f2d)) //nolint:gosec // deterministic test RNG

	return func(frontier []*syncdomain.TrackedAction) []*syncdomain.TrackedAction {
		dispatchOrderCanonical(frontier)
		mu.Lock()
		defer mu.Unlock()
		rng.Shuffle(len(frontier), func(i, j int) { frontier[i], frontier[j] = frontier[j], frontier[i] })

		return frontier
	}
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// parseDispatchOrder turns an order name into the engine hook.
func parseDispatchOrder(t *testing.T, name string) func([]*syncdomain.TrackedAction) []*syncdomain.TrackedAction {
	t.Helper()

	switch {
	case name == "forward":
		return dispatchOrderForward
	case name == "reverse":
		return dispatchOrderReverse
	case strings.HasPrefix(name, "shuffle:"):
		seed, err := strconv.ParseUint(strings.TrimPrefix(name, "shuffle:"), 10, 64)
		require.NoError(t, err, syncfixture.DispatchOrderEnv)
		return dispatchOrderShuffle(seed)
	default:
		require.Failf(t, "unknown dispatch order", "%s=%q: want forward, reverse or shuffle:<seed>", syncfixture.DispatchOrderEnv, name)
		return nil
	}
}

// registeredDispatchOrders maps a test name to the order engines built under
// it (or its subtests) use.
//
//nolint:gochecknoglobals // a registry test engines consult by test name; newTestEngine has no other way to learn its test's order
var registeredDispatchOrders stdsync.Map

// runUnderDispatchOrder runs fn as t with every engine it builds dispatching
// in the named order on one worker.
func runUnderDispatchOrder(t *testing.T, order string, fn func(*testing.T)) {
	t.Helper()

	registeredDispatchOrders.Store(t.Name(), order)
	t.Cleanup(func() { registeredDispatchOrders.Delete(t.Name()) })
	fn(t)
}

// applyRegisteredDispatchOrder gives a test engine the order registered for
// its test or an ancestor, else the one dispatchOrderEnv names.
func applyRegisteredDispatchOrder(t *testing.T, eng *Engine) {
	t.Helper()

	order := os.Getenv(syncfixture.DispatchOrderEnv)
	for name := t.Name(); name != ""; name = parentTestName(name) {
		if registered, ok := registeredDispatchOrders.Load(name); ok {
			if name, isString := registered.(string); isString {
				order = name
			}
			break
		}
	}
	if order == "" {
		return
	}
	eng.hooks.dispatchOrder = parseDispatchOrder(t, order)
	eng.hooks.serializeWorkers = true
}

func parentTestName(name string) string {
	i := strings.LastIndex(name, "/")
	if i < 0 {
		return ""
	}

	return name[:i]
}

// runOnceScenarios is every TestRunOnce_ scenario except the model test,
// which has its own determinism check. TestReverseDispatch_RunOnceScenariosPass
// runs each once with the frontier reversed on one worker, and
// TestReverseDispatchTable_ListsEveryRunOnceScenario keeps the list complete.
//
//nolint:funlen // one scenario per line
func runOnceScenarios() []func(*testing.T) {
	return []func(*testing.T){
		TestRunOnce_AMountRootPositionThatIsNotALinkIsRecoveredByAFullEnumeration,
		TestRunOnce_ASavedPositionThatIsNotALinkIsRecoveredByAFullEnumeration,
		TestRunOnce_DryRunLeavesSpellingRenameLeftover,
		TestRunOnce_FileMovedOutOfASubfolderIntoTheRemadeFolderWithTheSubfolderRemovedConverges,
		TestRunOnce_FolderMovedIntoItsOwnFormerChildConverges,
		TestRunOnce_ReplacingMoveFindingANewFileAtTheFreedNameStillMoves,
		TestRunOnce_SpellingRenameLeftoverIsPutBackBeforeThePlan,
		TestRunOnce_SpellingRenameLeftoverWhosePlainNameIsTakenIsKeptAndReported,
		TestRunOnce_UsesRunStateOnlyFromItsGoroutine,
		TestRunOnce_CountsOnTheEngineCollectorWithoutAnOwnerWrap,
		TestRunOnce_ABlockedRowWhoseScopeIsGoneIsReleasedAtStartup,
		TestRunOnce_AQuotaRefusalBelowTheWindowIsTimedRetryWorkNotABlock,
		TestRunOnce_AcceptedServerTransformIsNotDownloadedAgain,
		TestRunOnce_LocalEditEqualToUnchangedRemoteUploadsNothing,
		TestRunOnce_ActivityHookNamesEachCompletedFile,
		TestRunOnce_BaselineUpdatedAfterRun,
		TestRunOnce_Bidirectional_FullRun,
		TestRunOnce_BothSidesRenamedIdenticallyConverges,
		TestRunOnce_AFolderRenamedTheSameOnBothSidesWithANewFolderAtItsOldPathConverges,
		TestRunOnce_BothSidesRenamedIdenticallyKeepsTheRemoteEditPending,
		TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked,
		TestRunOnce_ReportCountsTheWorkAThrottleLeftForTheNextSync,
		TestRunOnce_TimedOutRequestIsKeptForRetryAndLeftWork,
		TestRunOnce_ACancelWhileTheRunGoesOnIsRetriedAndSaid,
		TestRunOnce_AFatalFailureEndsTheTransferItInterruptsAsAShutdown,
		TestRunOnce_ReportCountsUnresolvedWork,
		TestRunOnce_DryRunLeavesUnresolvedZero,
		TestRunOnce_RetryRowsThePlanDoesNotActOnAreGone,
		TestRunOnce_ALastStoreReadThatFailsReturnsTheReportAndTheError,
		TestRunOnce_FollowUpWorkTheRunEndsOnIsLeftForTheNextSync,
		TestRunOnce_UploadOnlyLeavesNoFollowUpForMovesTheModeHoldsBack,
		TestRunOnce_APauseThatRefusesItsTrialAgainLeavesEachFileOnce,
		TestRunOnce_APauseWaitedOutLeavesNothing,
		TestRunOnce_AFailureTheNextPassNoLongerNeedsIsNotLeft,
		TestRunOnce_AFailureALaterPassFinishesIsNotLeft,
		TestRunOnce_AFailedCleanupCountsOnlyWhenTheLastPassFailedIt,
		TestRunOnce_AConflictCopyAtThePassBoundIsLeftWork,
		TestRunOnce_ReportsTheFilesAnInterruptedRemovalSetAside,
		TestRunOnce_FinalDrainReleasesASyncedTreeToTheTrash,
		TestRunOnce_FinalDrainKeepsTheWholeTreeForWhatTheDeleteRuleDoesNotRemove,
		TestRunOnce_FinalDrainRemovesBundledJunkUnderTheConfiguredRule,
		TestRunOnce_FinalDrainRemovesALeftoverDownloadPartialFirst,
		TestRunOnce_UnreadableSharedFolderReleasesWhatMatchesAndTheReAddPlansNoDelete,
		TestRunOnce_FinalDrainOfAnUnreadableSharedFolderThatKeepsNothingFinishes,
		TestRunOnce_FinalDrainOfAnUnreadableSharedFolderIsHeldOnlyByWhatItKept,
		TestRunOnce_ACrashDuringTheReleaseResumesWithNoRemoteDelete,
		TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain,
		TestRunOnce_ALocalDeleteRefusedForAnUnseenChangePlansTheLocalEditNext,
		TestRunOnce_FinalDrainCreatesNothingNewLocally,
		TestRunOnce_AnOrdinaryRunStillDownloadsANewRemoteFile,
		TestRunOnce_FinalDrainMovesASyncedFileIntoANewFolderAndReleasesBoth,
		TestRunOnce_ARefusalThatPersistsNeverDownloadsWhatTheReleaseTook,
		TestRunOnce_FinalDrainKeepsATreeWithAFolderThatCannotBeWrittenTo,
		TestRunOnce_EmptyItemACrashLeftUnrecordedIsFinishedNotConflicted,
		TestRunOnce_EmptyItemOfAKilledCreateWhoseFileWasDeletedIsRemoved,
		TestRunOnce_SlowDeletionAcrossPassesIsHeldWithinTheWindow,
		TestRunOnce_DeletesOlderThanTheWindowStopCounting,
		TestRunOnce_ContextCancellation,
		TestRunOnce_ATimerPanicEndsTheRunAndTheActionInFlightIsAShutdown,
		TestRunOnce_AHelperPanicOfTheCommandEndsTheActionInFlightAsAShutdown,
		TestRunOnce_TheSignInRequirementIsTheSyncedAccountsAloneOfTwoAtOneAddress,
		TestRunOnce_ConflictOverAnInterruptedLargeUploadDropsItsSession,
		TestRunOnce_ConflictOverAnInterruptedLargeCreateDropsItsSession,
		TestRunOnce_LocalMoveOfAnInterruptedLargeUploadDropsItsSession,
		TestRunOnce_LocalDeleteOfAnInterruptedLargeUploadDropsItsSession,
		TestRunOnce_CreateConflictLocalDirRemoteFileKeepsBoth,
		TestRunOnce_CreateConflictLocalFileRemoteDirKeepsBoth,
		TestRunOnce_CreateConflictInsideADivergentlyRenamedFolderKeepsBoth,
		TestRunOnce_ANewFileAtAMovedFilesOldPathGivesWayToASwapOnOneDrive,
		TestRunOnce_CreateIntentOfAFileRenamedAwayIsDropped,
		TestRunOnce_CreateWhoseAnswerWasLostFinishesIntoItsOwnEmptyFile,
		TestRunOnce_CrossMoveCycleKeepsBothTreesWithoutNesting,
		TestRunOnce_DeltaExpired_AutoRetry,
		TestRunOnce_DeltaExpiredRecoverySynthesizesMissedDeletes,
		TestRunOnce_DeltaExpiredUploadDifferencesKeepsAndReuploadsLocal,
		TestRunOnce_DeltaTokenCommittedWithObservations,
		TestRunOnce_DeltaTokenPersisted,
		TestRunOnce_DivergentRenameConvergesInOneSync,
		TestRunOnce_DownloadOnly_DefersEditDeleteRecreateUpload,
		TestRunOnce_DownloadOnly_DoesNotOverrideLocalDeleteWhenRemoteAlsoChanged,
		TestRunOnce_DownloadOnly_ObservesLocalScanButSuppressesUploads,
		TestRunOnce_DownloadOverwriteKeepsPreviousVersionInTrash,
		TestRunOnce_DryRunActivityListsPlannedActions,
		TestRunOnce_DryRun_MountRootDoesNotSaveScopedDeltaToken,
		TestRunOnce_DryRun_NoExecution,
		TestRunOnce_EmojiOnlyVariantIsACollisionNotAnEndlessConflict,
		TestRunOnce_EmptiedMountPointPlansNoRemoteDeletes,
		TestRunOnce_EmptyPlan_NoPanic,
		TestRunOnce_ExecutorPartialFailure,
		TestRunOnce_FailedActionsRemainInReportErrorsAfterSummaryLogging,
		TestRunOnce_FolderDeletedAndRestoredLocallyTransfersNothing,
		TestRunOnce_FolderReplacedByFilesOnBothSidesKeepsBoth,
		TestRunOnce_FileReplacedByFoldersOnBothSidesMergesThem,
		TestRunOnce_FolderOneDriveRefusesAsTooLongIsAConditionNotAnonymousRetryWork,
		TestRunOnce_ARefusalOnlyTheUserCanClearIsLeftAsItsCondition,
		TestRunOnce_ARefusalAfterItsRetryWorkWasDroppedStartsTheConditionAgain,
		TestRunOnce_FolderRecreatedOnlineRebindsAndUploadsIntoTheNewItem,
		TestRunOnce_FollowSymlinks_UploadsFileInsideDirectoryAliasOutsideRoot,
		TestRunOnce_FullCaseFoldingVariantNeverOverwritesTheSyncedFile,
		TestRunOnce_FullRescanDropsUnsyncedRemoteStateOrphans,
		TestRunOnce_FullRescanWithEveryFolderReboundPlansNoTransfers,
		TestRunOnce_FullRescanWithItemReplacedAtSamePath,
		TestRunOnce_HardLinkUploadsAsANewFile,
		TestRunOnce_InPlaceEditWithRestoredMtimeConflictsWithRemoteEdit,
		TestRunOnce_InPlaceEditWithRestoredMtimeUploads,
		TestRunOnce_InterruptedCreateFinishesIntoItsOwnPlaceholder,
		TestRunOnce_InterruptedRemovalReportClearsOnceTheLeftoverIsGone,
		TestRunOnce_UploadOnlyCreateFinishesIntoItsPlaceholderAfterItsETagMoved,
		TestRunOnce_ItemMovedIntoRemotelyDeletedFolderIsDeletedLocally,
		TestRunOnce_LocalDeleteInsideFolderRenamedOnlineIsNotUndone,
		TestRunOnce_LeavesWorkBehindAPauseLongerThanTenMinutes,
		TestRunOnce_LegacyFolderBaselineGainsIdentityAndMovesAsOne,
		TestRunOnce_LocalFileReplacedByFolderConverges,
		TestRunOnce_LocalFolderDeleteDeletesRemoteFolder,
		TestRunOnce_LocalFolderDeleteKeepsRemoteChildAddedAfterObservation,
		TestRunOnce_LocalFolderMoveBecomesOneRemoteMoveWithoutResurrectingSource,
		TestRunOnce_LocalFolderMoveSurvivesFolderETagChurn,
		TestRunOnce_ThrottledBacklogRampsInsteadOfFlooding,
		TestRunOnce_LocalFolderRenameWithChildRenameConvergesInOnePass,
		TestRunOnce_LocalFolderReplacedByFileWithRemoteChildEditKeepsBoth,
		TestRunOnce_LocalFolderWithChildrenReplacedByFileConverges,
		TestRunOnce_LocalRenameWhileOneDriveSwapsItConverges,
		TestRunOnce_LocalRenameAndDeleteWhileOneDriveSwapsThemConverges,
		TestRunOnce_LocalRenameKeepsTheRemoteModificationTime,
		TestRunOnce_LocalRenameWhileRemoteEditsDownloadsTheEdit,
		TestRunOnce_MountRootDeltaIncapableSkipsFolderDelta,
		TestRunOnce_MountRootEnumerateStillPersistsFullReconcileCadenceWithoutToken,
		TestRunOnce_MountRootUsesScopedDeltaAndToken,
		TestRunOnce_NoChanges,
		TestRunOnce_OneRefusedConnectionOpensNoOutageScope,
		TestRunOnce_OneShotDryRunPlansOnePass,
		TestRunOnce_OneShotUploadsLocalEditInsideFolderRenamedOnline,
		TestRunOnce_PersistsLocalSnapshotAndConvergedSQLiteReconciliation,
		TestRunOnce_PlaceholderOfACreateWhoseFileWasDeletedIsRemoved,
		TestRunOnce_ReconcilesRemoteDeleteDriftWithoutFreshDelta,
		TestRunOnce_ReconcilesRemoteMirrorDownloadDriftWithoutFreshDelta,
		TestRunOnce_RefusedConnectionsOpenOneOutageScopeAndNoFileIssue,
		TestRunOnce_RemoteDeleteAfterRebindingInTheSameEngineSucceeds,
		TestRunOnce_RemoteDeleteOfALocallyRecreatedEmptyFolderIsApplied,
		TestRunOnce_RemoteDeleteMovesLocalFileToTrash,
		TestRunOnce_RemoteDeleteInsideFolderRenamedLocallyIsNotUndone,
		TestRunOnce_RemoteReplaceInsideFolderRenamedLocallyConverges,
		TestRunOnce_RemoteFileReplacedByNonEmptyFolderWithLocalEditKeepsBoth,
		TestRunOnce_RemoteFolderDeleteAcceptsAContentIdenticalLocalRestore,
		TestRunOnce_RemoteFolderDeleteAfterDeviceRenumberingIsApplied,
		TestRunOnce_RemoteFolderDeleteOverListingCapIsADurableIssueNotRelistedEveryPass,
		TestRunOnce_RemoteFolderDeleteRefusesAChildEditedOnlineAfterObservation,
		TestRunOnce_RemoteFolderDeleteRefusedWhenTheFolderIsBackLocally,
		TestRunOnce_DeletedFolderAndNewFolderWithReusedInodeAreNotAMove,
		TestRunOnce_DeletedFileAndNewFileWithReusedInodeAreNotAMove,
		TestRunOnce_RenameWithMatchingBirthTimeIsStillAMove,
		TestRunOnce_ThreeQuotaRefusalsActivateTheScopeAndBlockTheRows,
		TestRunOnce_WaitsOutAShortPauseBeforeTheNextPass,
		TestRunOnce_ReplansASupersededMoveInTheSameRun,
		TestRunOnce_OverwriteRefusedWithA412IsReplannedInTheSameRun,
		TestRunOnce_WithoutBirthTimeIdentityIsDeviceAndInode,
		TestRunOnce_RemoteFolderDeleteRemovesLocalTreeAndLeavesNoDrift,
		TestRunOnce_RemoteFolderDeleteWithIgnoredLocalFileConverges,
		TestRunOnce_RemoteFolderRenameMovesExistingChildrenOnce,
		TestRunOnce_RemoteFolderRenameWithChildRenameAndMoveInConverges,
		TestRunOnce_RemoteFolderRenameWithNewSubfolderConverges,
		TestRunOnce_RemoteFolderReplacedByFileWithLocalChildEditsKeepsBoth,
		TestRunOnce_RemoteHierarchyInversionConverges,
		TestRunOnce_RemoteMoveChainIntoVacatedPathConverges,
		TestRunOnce_RemoteMoveOfAFileTheUserReplacedLocallyKeepsBoth,
		TestRunOnce_RemoteMoveOverExistingFileReplacesWithoutResurrecting,
		TestRunOnce_RemoteMoveOverIdenticalFileConvergesInOnePass,
		TestRunOnce_RemoteMoveOverLocallyEditedFileKeepsTheEdit,
		TestRunOnce_RemoteMoveWithLocalEditMovesThenUploads,
		TestRunOnce_RemoteRenameInsideLocallyRecreatedFolderConverges,
		TestRunOnce_RemoteReplaceOfALocallyRecreatedFolderConverges,
		TestRunOnce_RemoteRenameAppliedLocallyDoesNotReupload,
		TestRunOnce_RemoteTwinOfASyncedFileKeepsTheLocalFile,
		TestRunOnce_RemoteTwinsAreHeldAndTheDriveKeepsSyncing,
		TestRunOnce_RenameAndRecreateMovesThenCreates,
		TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges,
		TestRunOnce_RenumberedDeviceTransfersNothing,
		TestRunOnce_SameRenameOnBothSidesStillAppliesTheRemoteEdit,
		TestRunOnce_MountRootUndecidedOrphanPullsTheNextRescanForward,
		TestRunOnce_LostMoveAnswerAfterARemoteEditStillAppliesTheEdit,
		TestRunOnce_SavedTokenOneDriveKeepsRefusingIsReenumerated,
		TestRunOnce_SharePointRootFormsRecordsActionableFailure,
		TestRunOnce_SyncedItemsMadeUnrepresentableAreNotDeletedOnline,
		TestRunOnce_ReplacedSyncedFileSyncsAgainOnceRestored,
		TestRunOnce_MountBoundaryClearsWhenTheFolderIsPlainAgain,
		TestRunOnce_AVanishedCrossedMountIsMissingNotABoundary,
		TestRunOnce_MountMissingClearsWhenTheScanNoLongerFindsIt,
		TestRunOnce_SwapOfTwoNamesConverges,
		TestRunOnce_SyncRootReplacedWhileEngineRunsIsRefused,
		TestRunOnce_TimestampOnlyLocalChangeRefreshesBaselineWithoutTransfer,
		TestRunOnce_TransfersRecordLocalCtimeAndIdentity,
		TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted,
		TestRunOnce_ARebindKeepsEveryDiskThatIsNotMounted,
		TestRunOnce_ADiskGoneForGoodIsForgottenOnceItsFolderIsRemoved,
		TestRunOnce_ARebindOntoAFolderWithoutAMountPointHoldsItsDeletes,
		TestRunOnce_ADiskMountedAgainLosesNothing,
		TestRunOnce_AnEditOnOneDriveWhileItsDiskIsNotMountedIsDownloadedOnceItIsBack,
		TestRunOnce_UploadOnlyCreateOntoADifferentlySpelledRemoteNameIsADurableCollision,
		TestRunOnce_UploadOnlyFolderCreateOntoADifferentlySpelledRemoteNameIsADurableCollision,
		TestRunOnce_UploadOnly_ReportsDeferredRemoteMirrorDriftWithoutFreshDelta,
		TestRunOnce_UploadOnly_StillObservesRemoteDelta,
		TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes,
		TestRunOnce_LocalNFDNamedFileUploadsInOnePass,
		TestRunOnce_UnicodeFormTwinOfSyncedFileUploadsNothing,
		TestRunOnce_UnicodeFormFolderTwinOfSyncedFileFailsNothing,
		TestRunOnce_RemoteEditReplacesTheNFDNamedLocalFile,
		TestRunOnce_RemoteDeleteRemovesTheNFDNamedLocalFile,
		TestRunOnce_RemoteMoveMovesTheNFDNamedLocalFile,
		TestRunOnce_NFDNamedLocalFolderReceivesAdditionsAndIsDeleted,
		TestRunOnce_ConflictingEditsOfAnNFDNamedFileKeepBoth,
		TestRunOnce_AppliedMoveLeavesTheBaselineCurrent,
		TestRunOnce_FolderRecreatedWhereAKeptFolderWasIsCreatedOnline,
		TestRunOnce_StoreTransactionsCountOnTheRunContextCollector,
		TestRunOnce_UnclassifiedRowFailsThePassNamingThePath,
		TestRunOnce_RefusedCreateWaitsOnTheDurableCurveAndClearsItsIntent,
		TestRunOnce_UploadOnlyCollisionHiddenByListingLagIsFoundOnTheNextAttempt,
		TestRunOnce_LargeFileWhoseSessionRequestFailedFinishesIntoItsPlaceholder,
		TestRunOnce_ARemoteFileDated2300DownloadsWithTheLastStorableTime,
		TestRunOnce_ARemoteFileDated1601DownloadsWithTheFirstStorableTime,
		TestRunOnce_ALocalFolderForARemoteFolderDated2300GetsTheLastStorableTime,
		TestRunOnce_ALocalFileDated2300UploadsAndConverges,
		TestRunOnce_AFileRedatedToTheEpochConverges,
		TestRunOnce_AFolderBothSidesReplacedWithTheSameFileConverges,
		TestRunOnce_ARemoteFileDatedTheEpochKeepsItsTimeLocally,
		TestRunOnce_ClaimedZeroByteFileThenRemoteMtimeChangeIsARemoteChange,
		TestRunOnce_AdoptedPlaceholderWithAnEmptyLocalFileLosesItsRecord,
		TestRunOnce_LookupFailureAfterACreateKeepsItsParentInRemoteTruth,
		TestRunOnce_PlaceholderAtAPathTheFilterHidesLosesItsRecord,
		TestRunOnce_AdoptedHashlessPlaceholderRenamedOnlineIsFinishedWithoutAConflict,
		TestRunOnce_TimeoutsAgainstOneSilentServiceOpenOneOutageScope,
		TestRunOnce_OneTimedOutRequestOpensNoOutageScope,
		TestRunOnce_AnOutageOfTimeoutsIsOneConditionNotOnePerFile,
		TestRunOnce_SubfolderMovedOnOneDriveToThePathItsMovedParentLeftConverges,
		TestRunOnce_ARotationThroughNestingMadeOnOneDriveConverges,
		TestRunOnce_ARotationThroughNestingSplitAcrossTheSidesKeepsOneDrivesNesting,
		TestRunOnce_AFolderMovedIntoDifferentNewFoldersAtItsOwnPathOnEachSideConverges,
		TestRunOnce_ThreeFoldersMovedIntoEachOthersNewFoldersConverge,
		TestRunOnce_AFileMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere,
		TestRunOnce_AFolderMovedOnOneDriveOntoAFolderItDeletedReplacesTheFolderHere,
		TestRunOnce_AFolderWrittenIntoHereThatOneDriveReplacedByAMovedFileIsKeptAsAConflictCopy,
		TestRunOnce_AFileOfAFolderRenamedHereMovedOnOneDriveOntoAFolderItDeletedEndsOnce,
		TestRunOnce_AFileMovedHereOntoAFolderDeletedHereAndRenamedOnOneDriveEndsOnce,
		TestRunOnce_AParkedFileLeavesItsFolderBeforeTheFolderMovesOnOneDrive,
		TestRunOnce_AFolderMovedBackOutOfANewFolderAtItsPathKeepsTheNewFolderAsAConflictCopy,
		TestRunOnce_AFileMadeHereUnderTheNameOneDriveGaveAMovedFileIsKeptAsAConflictCopy,
		TestRunOnce_FileMovedOnOneDriveOutOfAFolderMadeHereConverges,
		TestRunOnce_FileMovedOutOfARenamedSubfolderIntoTheRecreatedFolderConverges,
		TestRunOnce_FileMovedOutOfASubfolderRemovedHereButAddedToOnOneDriveConverges,
		TestRunOnce_FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPathConverges,
		TestRunOnce_FolderMovedOnOneDriveIntoAFolderMadeWhereAFileLeftConverges,
		TestRunOnce_FolderMovedOnOneDriveIntoAChainOfNewFoldersConverges,
		TestRunOnce_FolderMovedOnOneDriveIntoNewFoldersWhereAFolderMovedDeeperLeftConverges,
		TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileOneDriveKeepsTheOldFolderThere,
		TestRunOnce_MoveIntoAFolderMadeHereWhileOneDriveMakesTheRenamedPathConverges,
		TestRunOnce_MoveIntoAFolderMadeHereAfterARenameOntoADeletedFileConverges,
		TestRunOnce_MoveIntoAFolderMadeHereWaitsWhileTheOldFolderWaitsInANewFolder,
		TestRunOnce_MoveIntoAFolderMadeHereWhileTheOldFolderWaitsForADeleteConverges,
		TestRunOnce_TwoFoldersMovedHereIntoNewFoldersAtEachOthersPathsConverge,
		TestRunOnce_FileAndFolderMovedHereIntoNewFoldersAtEachOthersPathsConverge,
		TestRunOnce_ThreeFoldersMovedHereInARingOfNewFoldersConverge,
		TestRunOnce_TwoFilesMovedHereIntoNewFoldersAtEachOthersPathsWait,
		TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaits,
		TestRunOnce_AFolderDeletedOnOneDriveStaysHereWhileAMoveOutOfItWaits,
		TestRunOnce_AFolderRenamedHereIsRenamedOnOneDriveWhileAMoveOutOfAFolderDeletedInItWaits,
		TestRunOnce_AFolderRenamedOnOneDriveIsRenamedHereWhileAMoveOutOfAFolderDeletedInItWaits,
		TestRunOnce_AFolderDeletedHereStaysOnOneDriveWhileAMoveOutOfItWaitsBelowAKeptConflict,
		TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveUntilItsUploadRuns,
		TestRunOnce_AFileRenamedHereOntoAPathAWaitingMoveLeavesStaysOnOneDriveWhileACreateBeforeItsUploadFails,
		TestRunOnce_AFileRenamedHereOntoADeletedFileInAFolderMovedOnOneDriveStaysThereUntilItsUploadRuns,
		TestRunOnce_AnItemMovedIntoANewFolderAtItsOwnPathMovesAsideFirst,
		TestRunOnce_AFileMovedUpOntoItsRenamedFoldersPathStaysOnOneDriveUntilItsUploadRuns,
		TestRunOnce_AFileMovedUpOntoItsDeletedFoldersPathStaysOnOneDriveUntilItsUploadRuns,
		TestRunOnce_AFolderMovedIntoTheFolderThatTakesItsPathConvergesWithAnEditInIt,
		TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath,
		TestRunOnce_AFileEditedInASubfolderMovedWithItsRenamedFolderIsUploadedOnce,
		TestRunOnce_NamesTradedHereInAnotherCaseAreMovedOnOneDrive,
		TestRunOnce_AFileMadeOnOneDriveInAFolderWhoseMoveWaitsIsDownloadedOnceItMoves,
		TestRunOnce_AFileMovedOnOneDriveIntoAFolderWhoseMoveWaitsIsMovedHereOnce,
	}
}

// reverseDispatchSampleStride is the share of the scenarios a run outside
// the full tier re-runs reversed (syncfixture.FullTier): every one is a
// second run of a scenario the suite already ran.
const reverseDispatchSampleStride = 8

// Validates: R-2.17.1, R-6.10.31
func TestReverseDispatch_RunOnceScenariosPass(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	stride := 1
	if !syncfixture.FullTier() {
		stride = reverseDispatchSampleStride
	}
	for i, scenario := range runOnceScenarios() {
		if i%stride != 0 {
			continue
		}
		name := testFuncName(scenario)
		t.Run(name, func(t *testing.T) {
			runUnderDispatchOrder(t, "reverse", scenario)
		})
	}
}

// Validates: R-6.10.31
func TestReverseDispatchTable_ListsEveryRunOnceScenario(t *testing.T) {
	t.Parallel()

	listed := map[string]bool{}
	for _, scenario := range runOnceScenarios() {
		listed[testFuncName(scenario)] = true
	}

	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var missing []string
	for _, file := range files {
		parsed, parseErr := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr, file)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "TestRunOnce_") {
				continue
			}
			if fn.Name.Name == "TestRunOnce_RandomizedBidirectionalEditsConverge" || listed[fn.Name.Name] {
				continue
			}
			missing = append(missing, fn.Name.Name)
		}
	}
	assert.Empty(t, missing, "add these to runOnceScenarios so they also run under reverse dispatch")
}

func testFuncName(fn func(*testing.T)) string {
	full := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()

	return full[strings.LastIndex(full, ".")+1:]
}
