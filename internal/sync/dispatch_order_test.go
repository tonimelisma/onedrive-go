package sync

import (
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
)

// Dispatch-order control for tests (D-090). The engine's ready frontier
// normally leaves in dependency-graph order and runs on a pool of workers, so
// a bug that depends on which ready action runs first could pass by timing.
// A test can pin the order -- forward, reversed or a seeded shuffle -- and
// serialize the workers so the order is also the execution order.

// dispatchOrderEnv runs every engine the package's tests build under one
// order: "forward", "reverse" or "shuffle:<seed>". Unset, engines keep the
// graph order and the worker pool, as in production.
const dispatchOrderEnv = "ONEDRIVE_GO_DISPATCH_ORDER"

// dispatchOrderForward sorts a frontier by action ID, which is plan order:
// the canonical order the other two permute.
func dispatchOrderForward(frontier []*trackedAction) []*trackedAction {
	slices.SortStableFunc(frontier, func(a, b *trackedAction) int {
		return compareInt64(a.ID, b.ID)
	})

	return frontier
}

func dispatchOrderReverse(frontier []*trackedAction) []*trackedAction {
	dispatchOrderForward(frontier)
	slices.Reverse(frontier)

	return frontier
}

// dispatchOrderShuffle permutes each frontier with one seeded generator, so
// the same seed reproduces the same order run after run.
func dispatchOrderShuffle(seed uint64) func([]*trackedAction) []*trackedAction {
	var mu stdsync.Mutex
	rng := rand.New(rand.NewPCG(seed, seed^0x5851f42d4c957f2d)) //nolint:gosec // deterministic test RNG

	return func(frontier []*trackedAction) []*trackedAction {
		dispatchOrderForward(frontier)
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
func parseDispatchOrder(t *testing.T, name string) func([]*trackedAction) []*trackedAction {
	t.Helper()

	switch {
	case name == "forward":
		return dispatchOrderForward
	case name == "reverse":
		return dispatchOrderReverse
	case strings.HasPrefix(name, "shuffle:"):
		seed, err := strconv.ParseUint(strings.TrimPrefix(name, "shuffle:"), 10, 64)
		require.NoError(t, err, dispatchOrderEnv)
		return dispatchOrderShuffle(seed)
	default:
		require.Failf(t, "unknown dispatch order", "%s=%q: want forward, reverse or shuffle:<seed>", dispatchOrderEnv, name)
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

	order := os.Getenv(dispatchOrderEnv)
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
	eng.dispatchOrder = parseDispatchOrder(t, order)
	eng.serializeWorkers = true
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
		TestRunOnce_AcceptedServerTransformIsNotDownloadedAgain,
		TestRunOnce_ActivityHookNamesEachCompletedFile,
		TestRunOnce_BaselineUpdatedAfterRun,
		TestRunOnce_Bidirectional_FullRun,
		TestRunOnce_BothSidesRenamedIdenticallyConverges,
		TestRunOnce_BothSidesRenamedIdenticallyKeepsTheRemoteEditPending,
		TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked,
		TestRunOnce_ContextCancellation,
		TestRunOnce_CreateConflictLocalDirRemoteFileKeepsBoth,
		TestRunOnce_CreateConflictLocalFileRemoteDirKeepsBoth,
		TestRunOnce_CrossMoveCycleKeepsBothTreesWithoutNesting,
		TestRunOnce_DeltaExpired_AutoRetry,
		TestRunOnce_DeltaExpiredRecoverySynthesizesMissedDeletes,
		TestRunOnce_DeltaExpiredUploadDifferencesKeepsAndReuploadsLocal,
		TestRunOnce_DeltaTokenCommittedWithObservations,
		TestRunOnce_DeltaTokenPersisted,
		TestRunOnce_DivergentRenameConvergesInTwoPasses,
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
		TestRunOnce_ItemMovedIntoRemotelyDeletedFolderIsDeletedLocally,
		TestRunOnce_LocalDeleteInsideFolderRenamedOnlineIsNotUndone,
		TestRunOnce_LegacyFolderBaselineGainsIdentityAndMovesAsOne,
		TestRunOnce_LocalFileReplacedByFolderConverges,
		TestRunOnce_LocalFolderDeleteDeletesRemoteFolder,
		TestRunOnce_LocalFolderDeleteKeepsRemoteChildAddedAfterObservation,
		TestRunOnce_LocalFolderMoveBecomesOneRemoteMoveWithoutResurrectingSource,
		TestRunOnce_LocalFolderMoveSurvivesFolderETagChurn,
		TestRunOnce_LocalFolderRenameWithChildRenameConvergesInOnePass,
		TestRunOnce_LocalFolderReplacedByFileWithRemoteChildEditKeepsBoth,
		TestRunOnce_LocalFolderWithChildrenReplacedByFileConverges,
		TestRunOnce_LocalRenameWhileOneDriveSwapsItConverges,
		TestRunOnce_LocalRenameKeepsTheRemoteModificationTime,
		TestRunOnce_LocalRenameWhileRemoteEditsDownloadsTheEdit,
		TestRunOnce_MountRootDeltaIncapableSkipsFolderDelta,
		TestRunOnce_MountRootEnumerateStillPersistsFullReconcileCadenceWithoutToken,
		TestRunOnce_MountRootUsesScopedDeltaAndToken,
		TestRunOnce_NoChanges,
		TestRunOnce_PersistsLocalSnapshotAndConvergedSQLiteReconciliation,
		TestRunOnce_PlaceholderOfACreateWhoseFileWasDeletedIsRemoved,
		TestRunOnce_ReconcilesRemoteDeleteDriftWithoutFreshDelta,
		TestRunOnce_ReconcilesRemoteMirrorDownloadDriftWithoutFreshDelta,
		TestRunOnce_RemoteDeleteAfterRebindingInTheSameEngineSucceeds,
		TestRunOnce_RemoteDeleteOfALocallyRecreatedEmptyFolderIsApplied,
		TestRunOnce_RemoteDeleteMovesLocalFileToTrash,
		TestRunOnce_RemoteDeleteInsideFolderRenamedLocallyIsNotUndone,
		TestRunOnce_RemoteReplaceInsideFolderRenamedLocallyConverges,
		TestRunOnce_RemoteFileReplacedByNonEmptyFolderWithLocalEditKeepsBoth,
		TestRunOnce_RemoteFolderDeleteAcceptsAContentIdenticalLocalRestore,
		TestRunOnce_RemoteFolderDeleteAfterDeviceRenumberingIsApplied,
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
		TestRunOnce_RenumberedDeviceTransfersNothing,
		TestRunOnce_SameRenameOnBothSidesStillAppliesTheRemoteEdit,
		TestRunOnce_MountRootUndecidedOrphanPullsTheNextRescanForward,
		TestRunOnce_LostMoveAnswerAfterARemoteEditStillAppliesTheEdit,
		TestRunOnce_SavedTokenOneDriveKeepsRefusingIsReenumerated,
		TestRunOnce_SharePointRootFormsRecordsActionableFailure,
		TestRunOnce_SyncedItemsMadeUnrepresentableAreNotDeletedOnline,
		TestRunOnce_SwapOfTwoNamesConverges,
		TestRunOnce_SyncRootReplacedWhileEngineRunsIsRefused,
		TestRunOnce_TimestampOnlyLocalChangeRefreshesBaselineWithoutTransfer,
		TestRunOnce_TransfersRecordLocalCtimeAndIdentity,
		TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted,
		TestRunOnce_UploadOnlyCreateOntoADifferentlySpelledRemoteNameIsADurableCollision,
		TestRunOnce_UploadOnlyFolderCreateOntoADifferentlySpelledRemoteNameIsADurableCollision,
		TestRunOnce_UploadOnly_ReportsDeferredRemoteMirrorDriftWithoutFreshDelta,
		TestRunOnce_UploadOnly_StillObservesRemoteDelta,
		TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes,
		TestRunOnce_LocalNFDNamedFileUploadsInOnePass,
		TestRunOnce_RemoteEditReplacesTheNFDNamedLocalFile,
		TestRunOnce_RemoteDeleteRemovesTheNFDNamedLocalFile,
		TestRunOnce_RemoteMoveMovesTheNFDNamedLocalFile,
		TestRunOnce_NFDNamedLocalFolderReceivesAdditionsAndIsDeleted,
		TestRunOnce_ConflictingEditsOfAnNFDNamedFileKeepBoth,
	}
}

// Validates: R-2.17.1, R-6.8.9
func TestReverseDispatch_RunOnceScenariosPass(t *testing.T) {
	t.Parallel()

	for _, scenario := range runOnceScenarios() {
		name := testFuncName(scenario)
		t.Run(name, func(t *testing.T) {
			runUnderDispatchOrder(t, "reverse", scenario)
		})
	}
}

// Validates: R-6.8.9
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
