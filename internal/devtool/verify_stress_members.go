package devtool

// The stress lane's sync step runs the tests declared here, by exact name and
// per package, not the tests a name pattern happens to match: which tests of
// the sync tree run fifty times under the race detector is a decision, and
// TestStressRepeatedStepsDeclareEveryTestTheirRuleNames holds every
// declaration to the test sources in the quick lane. A new test the step's
// placement rule names goes into one of the member lists or, with a reason,
// into the non-members; a member need not be one the rule names. The
// multisync step declares nothing: it runs every test of its package, so a new
// multisync test is repeated without a declaration.

// The reasons of the sync step's non-members: each names the test's subject,
// which a word of its name made look like concurrency.
const (
	stressNonMemberRootState    = "a shortcut root's final-drain state, without goroutines"
	stressNonMemberReleaseRule  = "a release rule of a removed folder's local tree"
	stressNonMemberWatchSummary = "a deterministic watch log summary"
	stressNonMemberWatchLookup  = "a path recheck or a lookup structure, without goroutines"
	stressNonMemberWatchHelpers = "the failure messages of the tests' wait helpers, in a child test binary"
	stressNonMemberPhaseTable   = "a pure lookup of the watch lifecycle table"
)

// stressSyncConcurrencySelection is the sync step's selection: the tests of
// the watch, one-shot, worker-pool, cancellation and shutdown code, and the
// rule that says which tests of the tree must be declared.
func stressSyncConcurrencySelection() *stressSelection {
	return &stressSelection{
		packages:  stressSyncConcurrencyPackages(),
		placement: stressSyncConcurrencyPlacement(),
	}
}

// stressSyncConcurrencyPackages are the sync step's packages and their
// declared tests, the engine's first.
func stressSyncConcurrencyPackages() []stressRepeatedPackage {
	return []stressRepeatedPackage{
		{
			dir: "internal/sync",
			memberLists: []func() []string{
				stressSyncRunWatchMembers,
				stressSyncWatchRuntimeMembers,
				stressSyncWatchMembers,
				stressSyncOneShotAndWorkerPoolMembers,
				stressSyncShutdownAndCancellationMembers,
			},
			nonMemberList: stressSyncConcurrencyNonMembers,
		},
		{
			dir: "internal/sync/internal/syncobserve",
			memberLists: []func() []string{
				stressSyncObserveWatchMembers,
				stressSyncObserveShutdownAndCancellationMembers,
			},
			nonMemberList: stressSyncObserveConcurrencyNonMembers,
		},
		{
			dir: "internal/sync/internal/syncexec",
			memberLists: []func() []string{
				stressSyncExecOneShotAndWorkerPoolMembers,
				stressSyncExecShutdownAndCancellationMembers,
			},
		},
		{
			dir: "internal/sync/syncstore",
			memberLists: []func() []string{
				stressSyncStoreShutdownAndCancellationMembers,
			},
			nonMemberList: stressSyncStoreConcurrencyNonMembers,
		},
		{
			dir:           "internal/sync/syncshortcut",
			nonMemberList: stressSyncShortcutConcurrencyNonMembers,
		},
	}
}

// stressSyncConcurrencyPlacement names the sync tree's tests that must be
// declared: by prefix the families of the watch's entry, loop and observers,
// of the write coalescing timers and the real clock, of one-shot and of the
// worker pool, and the words of shutdown, draining, concurrency and
// cancellation anywhere in a name.
func stressSyncConcurrencyPlacement() stressPlacementRule {
	return stressPlacementRule{
		prefixes: []string{
			"TestRunWatch", "TestPhase0_RunWatch", "TestWatch", "TestRemoteObserverWatch",
			"TestHandleWrite", "TestRealClock", "TestOneShot", "TestWorkerPool",
		},
		phrases: []string{"Shutdown", "Drain", "Concurren", "Cancel"},
	}
}

// stressSyncRunWatchMembers are the members of the watch's entry and loop.
func stressSyncRunWatchMembers() []string {
	return []string{
		"TestPhase0_RunWatch_BootstrapCompletesBeforeLocalObserverStarts",
		"TestPhase0_RunWatch_BootstrapCompletesBeforeRemoteObserverStarts",
		"TestPhase0_RunWatch_BootstrapQuiescesWithFutureHeldRetryWork",
		"TestRunWatchLoop_SteadyStateCompletionWithoutDrainingIsAnError",
		"TestRunWatchUntilQuiescent_BootstrapCompletionStaysClean",
		"TestRunWatch_ATimerPanicEndsTheRunAndTheActionInFlightIsAShutdown",
		"TestRunWatch_BusyRuntimeQueuesPendingReplanInsteadOfOverlappingPrepare",
		"TestRunWatch_CancelDuringBootstrapStopsWithoutStartingObservers",
		"TestRunWatch_CancellationWinsOverFinalObserverExit",
		"TestRunWatch_ContextCancel",
		"TestRunWatch_CountsOnTheEngineCollectorWithoutAnOwnerWrap",
		"TestRunWatch_DownloadOnly_StartsLocalObserver",
		"TestRunWatch_FallbackSleepHonorsCancellation",
		"TestRunWatch_FinishesTheRemoteEditBehindARekey",
		"TestRunWatch_FinishesWorkTheBootstrapMovesLeaveForTheNextPass",
		"TestRunWatch_HousekeepingRunsBeforeTheFirstRefreshTimerIsArmed",
		"TestRunWatch_LocalObserverPanicFailsMountInsteadOfCrashing",
		"TestRunWatch_MountRootKeepsPollingOnly",
		"TestRunWatch_ObserverThatIgnoresItsContextIsNamedAfterTheBound",
		"TestRunWatch_ProcessBatch_EmptyPlan",
		"TestRunWatch_RecordsTheCancelOfItsRun",
		"TestRunWatch_ReturnsOnlyAfterObserversHaveExited",
		"TestRunWatch_ShutdownAfterCommittedRefreshDoesNotDropAppliedBatch",
		"TestRunWatch_ShutdownStopsRetryAndTrialTimers",
		"TestRunWatch_SocketIOWakeSourcePanicKeepsWatchRunning",
		"TestRunWatch_TimedOutRequestIsRetriedAndItsDependentRuns",
		"TestRunWatch_UploadOnly_StartsRemoteObserver",
		"TestRunWatch_UsesRunStateOnlyFromItsGoroutine",
		"TestRunWatch_WatchFallbackRetriesTheWatcher",
		"TestRunWatch_WatchFallbackScansAtOnceThenAtSafetyInterval",
		"TestRunWatch_WatchLimitExhausted_FallsBackToPolling",
		"TestRunWatch_WatcherCreateEMFILEFallsBackToPeriodicScan",
		"TestRunWatch_WebsocketDisabledKeepsPollingOnly",
		"TestRunWatch_WebsocketEnabledStartsWakeSource",
	}
}

// stressSyncWatchRuntimeMembers are the members of the watch runtime.
func stressSyncWatchRuntimeMembers() []string {
	return []string{
		"TestWatchRuntime_ADrainRecordsTheCompletionsItReceives",
		"TestWatchRuntime_ASnapshotCommitsRowsFindingsAndMountsTogether",
		"TestWatchRuntime_ASnapshotRefreshesTheObserversMountFlags",
		"TestWatchRuntime_ASnapshotTakenBeforeShutdownIsStillCommitted",
		"TestWatchRuntime_ASnapshotWhoseLastFindingClearedMarksDirty",
		"TestWatchRuntime_AStopsBoundStartsAtItsCancel",
		"TestWatchRuntime_AStoreThatDoesNotAnswerEndsTheDrainAtItsBound",
		"TestWatchRuntime_AnObserverWhoseRequestTimedOutIsWarnedAndReturned",
		"TestWatchRuntime_ArmRetryTimer_FiresAfterDelay",
		"TestWatchRuntime_ArmRetryTimer_KicksImmediatelyWhenRetryIsDue",
		"TestWatchRuntime_BeginObserverBackedRunningRejectsDrainingOrDuplicateObservers",
		"TestWatchRuntime_BeginObserverBackedRunningStartsFromASettledBootstrap",
		"TestWatchRuntime_BeginWatchDrain_ObserverChannelsStayRuntimeOwned",
		"TestWatchRuntime_BootstrapWaitLogsOnlyWhenCountsChange",
		"TestWatchRuntime_DispatchIsGatedByTheColdStartAllowance",
		"TestWatchRuntime_DroppedObservationRescanFailureWarnsOnce",
		"TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound",
		"TestWatchRuntime_FallbackScanRecordsAndClearsFindings",
		"TestWatchRuntime_HandleMaintenanceTick_EmitsDebugEventWithoutSQLiteProbe",
		"TestWatchRuntime_HandleProtectedRootEventOwnsLocalAliasRename",
		"TestWatchRuntime_HandleWatchActionCompletion_DrainsPublicationOnlyDependents",
		"TestWatchRuntime_HandleWatchActionCompletion_DrainsPublicationOnlyDependentsDuringBootstrap",
		"TestWatchRuntime_HandleWatchHeldRelease_CompletesReleasedConcreteActionsOnReductionFailure",
		"TestWatchRuntime_HandleWatchHeldRelease_RetryTickReducesReleasedPublicationRetryOnEngineSide",
		"TestWatchRuntime_IdleReplanLocalObservationFailureReschedulesDirtySignal",
		"TestWatchRuntime_InitWatchInfraUsesUnbufferedDispatch",
		"TestWatchRuntime_IsItsOwnSignalSinkAndInspector",
		"TestWatchRuntime_LocalObservationBatchDeletesDirectoryPrefix",
		"TestWatchRuntime_LocalObservationBatchDeletesExactPath",
		"TestWatchRuntime_LocalObservationBatchFullSnapshotReplacesLocalTruth",
		"TestWatchRuntime_LocalObservationBatchMarksLocalTruthSuspect",
		"TestWatchRuntime_LocalObservationBatchUpsertsScopedRows",
		"TestWatchRuntime_MaintenanceMarksLocalTruthSuspectAfterDroppedObservation",
		"TestWatchRuntime_MarkReplanNeededWithoutDirtyBufferIsSafe",
		"TestWatchRuntime_ObserverExit_ReturnedOnceWithCause",
		"TestWatchRuntime_ParkedBatchMembersWaitForTheirAdmitInstant",
		"TestWatchRuntime_PendingReplanLocalObservationFailureReschedulesDirtySignal",
		"TestWatchRuntime_PendingReplanRetiresDependentsReleasedByRunningAction",
		"TestWatchRuntime_QueuePendingReplanRetiresOldOutbox",
		"TestWatchRuntime_ReplanKeepsAFreshCreateIntentForTheBatchThatShowsItsFile",
		"TestWatchRuntime_RunNonDrainingWatchStepPrioritizesReadyReplanOverDispatch",
		"TestWatchRuntime_RunNonDrainingWatchStep_BootstrapDispatchUsesSharedHandler",
		"TestWatchRuntime_RunNonDrainingWatchStep_BootstrapRetryTickReducesReleasedPublicationRetryOnEngineSide",
		"TestWatchRuntime_RunNonDrainingWatchStep_CanceledClosedReplanStartsDrain",
		"TestWatchRuntime_RunNonDrainingWatchStep_ClosedReplanWithoutCancelErrors",
		"TestWatchRuntime_RunNonDrainingWatchStep_ConsumesReplanReady",
		"TestWatchRuntime_RunNonDrainingWatchStep_ContextCancelStartsDrain",
		"TestWatchRuntime_RunWatchLoop_BootstrapPhaseQuiescesToReady",
		"TestWatchRuntime_RunWatchLoop_CanceledClosedReplanDrainsBeforeReturn",
		"TestWatchRuntime_RunWatchLoop_CanceledClosedReplanDrainsFromBootstrapPhase",
		"TestWatchRuntime_SafetyScanWithoutNewTruthKeepsAdmittedPlan",
		"TestWatchRuntime_ScopeActivationRetiresAdmittedWorkItBlocks",
		"TestWatchRuntime_SkippedFindingsReplanOnlyWhenTheyChange",
		"TestWatchRuntime_TimerCallbacksTouchOnlyChannels",
	}
}

// stressSyncWatchMembers are the members of the watch's loop, observers and
// scenarios.
func stressSyncWatchMembers() []string {
	return []string{
		"TestAckHandle_ConcurrentWithFullRefresh_IsRaceFree",
		"TestRemoteObserverWatch_AFailedBatchEndsTheWatchUnlessItsRunEnded",
		"TestWatchDirtyScheduling_PathFanoutStillProducesOneCoarseDirtySignal",
		"TestWatchLoop_RefreshTick_RunsPeriodicFullRemoteRefreshThroughResultHandoff",
		"TestWatchLoop_ServicesOnlyThePhasesEnabledInputs",
		"TestWatchObserver_SeesBindingMadeAfterItWasBuilt",
		"TestWatchPipelineCleanup_KeepsRefreshResultChannelUsable",
		"TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll",
		"TestWatch_MountRootPollDropsTheFreshIntentOfAGoneFile",
		"TestWatch_MountRootPollThatTimedOutBacksOffAndPollsAgain",
		"TestWatch_PollKeepsTheIntentOfAFileThatIsStillThere",
		"TestWatch_PollThatMayHaveMissedTheCreateKeepsTheIntent",
		"TestWatch_ReplanWaitsWhileAnUploadRunsBeforeItPrunesPendingCreates",
	}
}

// stressSyncObserveWatchMembers are the members of the watch's loop, observers and
// scenarios.
// These tests are in internal/sync/internal/syncobserve.
func stressSyncObserveWatchMembers() []string {
	return []string{
		"TestHandleWrite_DeleteClearsTimer",
		"TestHandleWrite_DifferentPathsNotCoalesced",
		"TestHandleWrite_VirtualTime_RapidWritesCoalesceIntoOneHash",
		"TestRemoteObserverWatch_BacksOffAfterATimedOutPollInsteadOfStopping",
		"TestWatchLoop_BackoffEscalatesWithoutReset",
		"TestWatchLoop_BackoffResetsOnSafetyScan",
		"TestWatchLoop_ChmodCreateCombinedEvent",
		"TestWatchLoop_MoveOutOfOrderRenameCreate",
		"TestWatchLoop_TransientFileCreateDelete",
		"TestWatch_BackoffOnError",
		"TestWatch_BackoffWithAWakeChannelWaitsOnTheRunClock",
		"TestWatch_BatchHandlerError_ReturnsError",
		"TestWatch_CommitsObservations",
		"TestWatch_ContextCancellation",
		"TestWatch_ContinuouslyWrittenFileBacksOff",
		"TestWatch_CurrentDeltaToken",
		"TestWatch_DeleteDirectoryRemovesWatch",
		"TestWatch_DeltaExpiredRecoversThroughTheFullRefresh",
		"TestWatch_DeltaExpiredResets",
		"TestWatch_DetectsFileCreate",
		"TestWatch_DetectsFileDelete",
		"TestWatch_DetectsFileModify",
		"TestWatch_DirectoryCreatedDuringOverflowIsWatchedAfterRecoveryScan",
		"TestWatch_ENOSPC_ReturnsWatchLimitExhausted",
		"TestWatch_EmptyCompleteTopologyApplyFailureDoesNotAdvanceCursor",
		"TestWatch_EventOverflowRunsRecoveryScan",
		"TestWatch_EveryPollHandsOverTheTimeItBegan",
		"TestWatch_HashFailureModifyStillEmitsEvent",
		"TestWatch_HashFailureStillEmitsCreate",
		"TestWatch_IgnoresExcludedFiles",
		"TestWatch_IntervalClamping",
		"TestWatch_NetworkRootShortensSafetyScanInterval",
		"TestWatch_NewDirectoryEMFILEStopsWatchingForFallback",
		"TestWatch_NewDirectoryNestedRecursion",
		"TestWatch_NewDirectoryPreExistingFiles",
		"TestWatch_NewDirectoryWatched",
		"TestWatch_OnlyUnrecoverableDeletes_AdvancesToken",
		"TestWatch_OrphanUnderFetchedPackageStaysExcluded",
		"TestWatch_OrphanUnderFetchedVaultStaysExcluded",
		"TestWatch_OrphanWithReadableParent_AdvancesToken",
		"TestWatch_OrphanWithUnreachableParent_AdvancesToken",
		"TestWatch_PollsAtInterval",
		"TestWatch_RecoveryRequestsCoalesceIntoOneArmedScan",
		"TestWatch_RecoveryScanDefersWhileRuntimeHasNotTakenObservations",
		"TestWatch_RenamedDirectoryDescendantEventsUseNewPath",
		"TestWatch_RepeatedlyFailingRecoveryBacksOff",
		"TestWatch_SafetyTickDefersWhileRuntimeHasNotTakenObservations",
		"TestWatch_StartupSafetyScanCatchesChangeDuringWatchSetup",
		"TestWatch_SustainedServerErrorsOnTheCursorResetIt",
		"TestWatch_TopologyApplyFailureDoesNotAdvanceCursor",
		"TestWatch_TransientErrorRetriesAndKeepsToken",
		"TestWatch_VirtualTime_SafetyScanFindsAMissedFileAtItsInterval",
		"TestWatch_WakeSignalTriggersImmediatePoll",
		"TestWatch_WakeSignalsCoalesce",
		"TestWatch_ZeroEvents_NoTokenAdvance",
		"TestWatch_ZeroEvents_NoTokenAdvanceAfterWake",
	}
}

// stressSyncOneShotAndWorkerPoolMembers are the members of the one-shot
// result loop, whole one-shot passes and the worker pool.
func stressSyncOneShotAndWorkerPoolMembers() []string {
	return []string{
		"TestOneShotEngineLoop_ACanceledPassRecordsTheCompletionsItDrains",
		"TestOneShotEngineLoop_ACanceledPassSealsItsOutbox",
		"TestOneShotEngineLoop_ClosedResultsStillProcessBufferedRetryWork",
		"TestOneShotEngineLoop_SupersededCompletionRetiresDependentsWithoutSuccessOrRetry",
		"TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady",
		"TestOneShotFollowUp_StopsWithoutProgressOnFailureAndAtTheBound",
		"TestOneShotRunner_HandleOneShotCompletion_AfterCancelCompletesReleasedReadyAsShutdown",
		"TestOneShotRunner_HandleOneShotCompletion_AfterFatalCompletesReleasedReadyAsShutdown",
		"TestOneShotRunner_ReleaseIdleDueHeldWork_ClearsShutdownCompletedOutboxOnReductionFailure",
		"TestOneShotRunner_RunResultsLoopIdle_ReleasesDueHeldWorkBeforeBlocking",
		"TestOneShotRunner_ScopeActivationRetiresPendingOutbox",
		"TestReleaseLocalTree_ANewOrChangedFileIsLeftForTheNextDrain",
		"TestReleaseLocalTree_ARowWithoutAModificationTimeWaitsForTheNextDrain",
		"TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain",
		"TestRunOnce_FinalDrainCreatesNothingNewLocally",
		"TestRunOnce_FinalDrainKeepsATreeWithAFolderThatCannotBeWrittenTo",
		"TestRunOnce_FinalDrainKeepsTheWholeTreeForWhatTheDeleteRuleDoesNotRemove",
		"TestRunOnce_FinalDrainMovesASyncedFileIntoANewFolderAndReleasesBoth",
		"TestRunOnce_FinalDrainOfAnUnreadableSharedFolderIsHeldOnlyByWhatItKept",
		"TestRunOnce_FinalDrainOfAnUnreadableSharedFolderThatKeepsNothingFinishes",
		"TestRunOnce_FinalDrainReleasesASyncedTreeToTheTrash",
		"TestRunOnce_FinalDrainRemovesALeftoverDownloadPartialFirst",
		"TestRunOnce_FinalDrainRemovesBundledJunkUnderTheConfiguredRule",
		"TestWorkerPool_ACommitThatOutlastsItsBoundIsAStoreFailure",
	}
}

// stressSyncExecOneShotAndWorkerPoolMembers are the members of the one-shot
// result loop, whole one-shot passes and the worker pool.
// These tests are in internal/sync/internal/syncexec.
func stressSyncExecOneShotAndWorkerPoolMembers() []string {
	return []string{
		"TestWorkerPool_ACompletionDroppedAfterTheStopIsLogged",
		"TestWorkerPool_AStoppedPoolDeliversEveryCompletionItHasRoomFor",
		"TestWorkerPool_ClosedDispatchChannelClosesCompletions",
		"TestWorkerPool_CommitsCompletedOutcomeAfterPoolCancellation",
		"TestWorkerPool_ContextCancellationClosesCompletions",
		"TestWorkerPool_DependencyChain",
		"TestWorkerPool_FailedOutcome",
		"TestWorkerPool_FolderCreate",
		"TestWorkerPool_FolderCreateThenUpload_ParentResolvedFromBaseline",
		"TestWorkerPool_PanicRecovery",
		"TestWorkerPool_ResultChannel",
		"TestWorkerPool_SendResultCountsLivePreconditionSupersededByCapability",
		"TestWorkerPool_SendResultDoesNotGuessLivePreconditionSourceWithoutCapability",
		"TestWorkerPool_SendResultDropsWhenContextCancellationWins",
		"TestWorkerPool_StopCancelsWork",
		"TestWorkerPool_StopIsIdempotentAfterCompletionsClose",
		"TestWorkerPool_StopReturnsWhileWorkerHashesBeforeUpload",
	}
}

// stressSyncShutdownAndCancellationMembers are the members of concurrent
// access, cancellation, timers and the shutdown and drain of running work.
func stressSyncShutdownAndCancellationMembers() []string {
	return []string{
		"TestCancelCauses_AnswerToCanceledAndNameTheirCancel",
		"TestClassifyResult_StalledTransferIsRetryableNotShutdown",
		"TestClassifyResult_SuccessAndShutdown",
		"TestCommitLocalSnapshot_ConcurrentBindsLeaveOneMarker",
		"TestDrainAfterCanceledStepError_AlreadyDrainingReportsTheError",
		"TestDrainAfterCanceledStepError_CanceledStepFailureStartsTheDrain",
		"TestDrainAfterCanceledStepError_LiveContextReportsTheError",
		"TestDrainAfterCanceledStepError_NoErrorDoesNothing",
		"TestEngineFlow_CompleteOutboxAsShutdown_CompletesTrackedActions",
		"TestEngineFlow_CompleteQueuedDispatchAsShutdown_CompletesQueuedSubtree",
		"TestEngineFlow_ProcessNormalDecision_ShutdownReturnsWithoutPersistence",
		"TestEngineFlow_ProcessTrialDecision_ShutdownCompletesWithoutTerminating",
		"TestEngineFlow_SuppressedShutdownCompletionError_QuietForCancellationAndRepeatFatal",
		"TestFault_ContextCancel_WorkerPool",
		"TestHandleObserverExit_DrainingOutsideShutdownViolatesInvariant",
		"TestHandleObserverExit_LastObserverDuringShutdownIsNotAnError",
		"TestHandleObserverExit_LastObserverOutsideShutdownReturnsError",
		"TestHandleRemoteObservationBatch_PrimaryWatchCanceledContextReturnsCommitError",
		"TestIsWatchShutdownError_OnlyTheEndedRunsOwnCancellation",
		"TestPhase0_RunWatch_DueTrialDrainsDuringBootstrapBeforeObserversStart",
		"TestPostSyncHousekeeping_CanceledContextIsQuiet",
		"TestRealClock_ATimerPanicWithNoRunBoundIsOnlyLogged",
		"TestRefreshProtectedRootsFromStore_ConcurrentReadersAreRaceFree",
		"TestRunFullRemoteRefreshAsync_ShutdownAfterCommit",
		"TestRunOnce_ACancelWhileTheRunGoesOnIsRetriedAndSaid",
		"TestRunOnce_AFatalFailureEndsTheTransferItInterruptsAsAShutdown",
		"TestRunOnce_AHelperPanicOfTheCommandEndsTheActionInFlightAsAShutdown",
		"TestRunOnce_ATimerPanicEndsTheRunAndTheActionInFlightIsAShutdown",
		"TestRunOnce_ContextCancellation",
		"TestRunPublicationDrainStage_DoesNotReleaseUnrelatedHeldWork",
		"TestRunPublicationDrainStage_PersistsRetryWorkOnPublicationCommitFailure",
		"TestRunPublicationDrainStage_PublicationSuccessClearsRetryWorkAndAdmitsDependents",
		"TestRunPublicationDrainStage_PublicationSuccessDoesNotResetScopeFailureWindows",
		"TestRunPublicationDrainStage_TerminatesWhenPublicationRetryPersistenceFails",
		"TestRuntimeArchitecture_ShutdownDrainDoesNotRoutePublicationWorkToWorkers",
		"TestRuntimeArchitecture_ShutdownDrainSealsAdmissionOnCompletionError",
		"TestStartObservers_RefusalDuringDrainClassifiesAsShutdown",
		"TestWaitForQuiescence_ContextCancel",
	}
}

// stressSyncObserveShutdownAndCancellationMembers are the members of concurrent
// access, cancellation, timers and the shutdown and drain of running work.
// These tests are in internal/sync/internal/syncobserve.
func stressSyncObserveShutdownAndCancellationMembers() []string {
	return []string{
		"TestCancelPendingTimers",
		"TestCancelRecoveryScan_StaleTimerFiringIsIgnored",
		"TestFullDelta_ContextCanceled",
		"TestFullScan_ContextCanceled",
		"TestHandleDelete_CancelsCoalesceTimer",
		"TestLocalWatch_ContextCancellation",
		"TestTrySend_ContextCanceled_NoDrop",
	}
}

// stressSyncExecShutdownAndCancellationMembers are the members of concurrent
// access, cancellation, timers and the shutdown and drain of running work.
// These tests are in internal/sync/internal/syncexec.
func stressSyncExecShutdownAndCancellationMembers() []string {
	return []string{
		"TestActionFreshness_CanceledContextFailsClosedWithoutStoreRead",
		"TestUploadNewFile_CanceledCreateKeepsItsIntentQuietly",
		"TestUploadNewFile_CreatedItemIsRecordedWhenTheActionIsCanceled",
	}
}

// stressSyncStoreShutdownAndCancellationMembers are the members of concurrent
// access, cancellation, timers and the shutdown and drain of running work.
// These tests are in internal/sync/syncstore.
func stressSyncStoreShutdownAndCancellationMembers() []string {
	return []string{
		"TestBaselineCache_GetByPathCopyIsSafeAgainstConcurrentPut",
		"TestBaseline_ConcurrentAccess",
		"TestCheckCacheConsistency_IsSafeAgainstConcurrentMutation",
		"TestCheckStoreIntegrity_CanceledCheckIsNotDamage",
		"TestSyncStore_CloseWithCanceledContextStillCheckpoints",
	}
}

// stressSyncConcurrencyNonMembers are the sync tests the placement rule names
// and the step leaves out: a word of the name means a state of a shortcut root,
// a release rule, a log summary, a lookup structure or the tests' own wait
// helpers, not concurrency. When in doubt a test stays a member: a one-shot
// pass that runs the worker pool is one, whatever its subject.
func stressSyncConcurrencyNonMembers() []stressNonMember {
	return []stressNonMember{
		{test: "TestEngine_AcknowledgeChildFinalDrainBlocksWhenAliasProjectionCannotBeRemoved", reason: stressNonMemberRootState},
		{test: "TestEngine_AcknowledgeChildFinalDrainReleasesParentShortcutRoot", reason: stressNonMemberRootState},
		{test: "TestEngine_AcknowledgeChildFinalDrainSendsANonEmptyRootBackToTheFinalDrain", reason: stressNonMemberRootState},
		{test: "TestEngine_ReconcileRemovedFinalDrainMissingLocalAliasReleasesWithoutRemoteDelete", reason: stressNonMemberRootState},
		{test: "TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain", reason: stressNonMemberRootState},
		{test: "TestReleaseLocalTree_RunsOnlyWhenTheDrainLetsIt", reason: stressNonMemberReleaseRule},
		{test: "TestWatchConditionCountTotal_SumsRawCounts", reason: stressNonMemberWatchSummary},
		{test: "TestWatchConditionCounts_SortsAndAggregatesProjectedGroups", reason: stressNonMemberWatchSummary},
		{test: "TestWatchConditionSummaryFingerprint_IsTheSetOfConditionKinds", reason: stressNonMemberWatchSummary},
		{test: "TestWatchConditionSummary_AFailedReadIsLoggedOnce", reason: stressNonMemberWatchSummary},
		{test: "TestWatchConditionSummary_DebugLineOnlyWhenTheBreakdownChanges", reason: stressNonMemberWatchSummary},
		{test: "TestWatchConditionSummary_StatesBreakdownAndHintOnce", reason: stressNonMemberWatchSummary},
		{test: "TestWatchConditionSummary_WarnsOnlyForNewActionableKinds", reason: stressNonMemberWatchSummary},
		{test: "TestWatchHelpers_FailNamingWhatTheyWaitedFor", reason: stressNonMemberWatchHelpers},
		{test: "TestWatchPhase_NothingLeavesDrainingExceptDrained", reason: stressNonMemberPhaseTable},
		{test: "TestWatchPhase_TransitionTableIsTotal", reason: stressNonMemberPhaseTable},
	}
}

// stressSyncObserveConcurrencyNonMembers are the sync tests the placement rule names
// and the step leaves out: a word of the name means a state of a shortcut root,
// a release rule, a log summary, a lookup structure or the tests' own wait
// helpers, not concurrency. When in doubt a test stays a member: a one-shot
// pass that runs the worker pool is one, whatever its subject.
// These tests are in internal/sync/internal/syncobserve.
func stressSyncObserveConcurrencyNonMembers() []stressNonMember {
	return []stressNonMember{
		{test: "TestWatchRechecks_FindTheNFDNamedLocalFile", reason: stressNonMemberWatchLookup},
		{test: "TestWatchRegistry_SubtreeLookupFollowsTheIndexAndUnlinksEmptyBranches", reason: stressNonMemberWatchLookup},
	}
}

// stressSyncStoreConcurrencyNonMembers are the sync tests the placement rule names
// and the step leaves out: a word of the name means a state of a shortcut root,
// a release rule, a log summary, a lookup structure or the tests' own wait
// helpers, not concurrency. When in doubt a test stays a member: a one-shot
// pass that runs the worker pool is one, whatever its subject.
// These tests are in internal/sync/syncstore.
func stressSyncStoreConcurrencyNonMembers() []stressNonMember {
	return []stressNonMember{
		{test: "TestSyncStore_EmptyCompleteShortcutTopologyMarksRemovedFinalDrain", reason: stressNonMemberRootState},
		{test: "TestSyncStore_markShortcutChildFinalDrainReleasePendingIsDurable", reason: stressNonMemberRootState},
	}
}

// stressSyncShortcutConcurrencyNonMembers are the sync tests the placement rule names
// and the step leaves out: a word of the name means a state of a shortcut root,
// a release rule, a log summary, a lookup structure or the tests' own wait
// helpers, not concurrency. When in doubt a test stays a member: a one-shot
// pass that runs the worker pool is one, whatever its subject.
// These tests are in internal/sync/syncshortcut.
func stressSyncShortcutConcurrencyNonMembers() []stressNonMember {
	return []stressNonMember{
		{test: "TestPlanShortcutRootReleaseCleanupSendsANonEmptyRootBackToTheFinalDrain", reason: stressNonMemberRootState},
	}
}
