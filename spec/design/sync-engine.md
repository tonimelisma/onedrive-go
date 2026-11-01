# Sync Engine

GOVERNS: internal/cli/sync_flow.go, internal/cli/sync_runtime.go, internal/sync/api_types.go, internal/sync/debug_event_sink.go, internal/sync/doc.go, internal/sync/engine*.go, internal/sync/engine_config.go, internal/sync/engine_debug_events.go, internal/sync/engine_runtime*.go, internal/sync/engine_watch*.go, internal/sync/observation_findings.go, internal/sync/permission_evidence.go, internal/sync/permission_handler.go, internal/sync/permission_probe_local.go, internal/sync/permission_probe_remote.go, internal/sync/permissions.go, internal/sync/permissions_rdonly_bsd.go, internal/sync/permissions_rdonly_linux.go, internal/sync/shortcut_alias_mutation.go, internal/sync/shortcut_root_lifecycle.go, internal/sync/shortcut_root_publication.go, internal/sync/watch_condition_log.go, internal/sync/watch_summary.go

LAYER: 4

Implements: R-2.1 [verified], R-2.1.7 [verified], R-2.8.21 [verified], R-2.15.3 [verified], R-2.8.3 [verified], R-2.8.5 [verified], R-2.8.6 [verified], R-2.8.7 [verified], R-2.8.8 [verified], R-2.8.9 [verified], R-2.8.10 [verified], R-2.8.11 [verified], R-2.10 [designed], R-2.14 [designed], R-2.16.2 [verified], R-2.16.3 [verified], R-6.3.4 [verified], R-6.3.5 [verified], R-6.6.17 [verified], R-6.6.30 [verified], R-6.8.24 [verified], R-6.8.25 [verified]

## Overview

The engine is the single mounted content-root runtime owner. It coordinates:

- observation
- planning
- publication-only action commits
- execution
- durable outcome writes
- retry and trial scheduling
- scope lifecycle
- watch-mode refresh and maintenance work

The target engine persists durable content-sync status through three authorities:

- `observation_issues`
- `retry_work`
- `block_scopes`

It does not use a mixed failure table as durable control state.
Parent namespace engines also persist parent-owned shortcut alias lifecycle in
`shortcut_roots`. Those rows protect child-root paths from ordinary parent
content planning, record parent-scoped blockers and retries, and declare child
runner/cleanup publications for the control plane. They are not child content retry state: child
engines still own `observation_issues`, `retry_work`, and `block_scopes` for the
shared-folder target content inside the child projection.

`retry_work` and `block_scopes` are engine-owned control state, not
best-effort diagnostics. If the runtime cannot durably record or transition
required retry/scope state after an exact action result or admission decision,
it fails closed and terminates the current runtime. Product-facing last-sync
history is not a durable engine authority.

`observation_findings.go` is the engine-owned constructor boundary for
observation batches. Engine orchestration chooses when to reconcile those
batches durably or into a scratch planning store, but callers should not
assemble overlapping observation-managed batch shapes ad hoc.

## Ownership Contract

- Owns: single-mount runtime orchestration, watch-mode mutable state,
  worker-result classification, retry/trial scheduling, and scope lifecycle.
- Does Not Own: SQLite schema, Graph normalization, config parsing, or
  multi-mount daemon lifecycle.
- Source of Truth: durable sync state in `SyncStore`, plus engine-owned
  in-memory runtime state for the currently running session.
- Allowed Side Effects: coordinating observers, planner, executor, and store
  writes through injected boundaries.
- Mutable Runtime Owner: `engineFlow` is the single per-run mutable owner.
  One-shot and watch both execute through that same runtime state; watch keeps
  it alive across timer ticks and observation batches. The `Engine` itself is
  fixed after construction apart from the fields it guards itself: the
  protected roots live under `protectedRootsMu`, read through
  `currentProtectedRoots` (a copy) and written through `setProtectedRoots`,
  because `refreshProtectedRootsFromStore` runs on the loop goroutine before
  every observation and off it from the full-refresh goroutine and the remote
  observer's expired-delta recovery (R-6.3.4).
- Error Boundary: the engine translates observer, planner, executor,
  permission, and store outcomes into engine-owned reports, retries, scope
  transitions, and durable authority writes.

## Verified By

| Behavior | Evidence |
| --- | --- |
| A one-shot sync runs further passes while the previous pass's plan left work for the next (moves, re-keys, conflict copies, deferred or dropped work), up to four, never in dry-run and never after a pass that made no progress or failed an action; its report adds up every pass. Watch replans by itself once such a plan settles (R-2.1.7). | `TestRunOnce_OneShotUploadsLocalEditInsideFolderRenamedOnline`, `TestRunOnce_OneShotDryRunPlansOnePass`, `TestRunOnce_RemoteMoveWithLocalEditMovesThenUploads`, `TestRunOnce_SwapOfTwoNamesConverges`, `TestRunOnce_BothSidesRenamedIdenticallyKeepsTheRemoteEditPending`, `TestRunWatch_FinishesWorkTheBootstrapMovesLeaveForTheNextPass`, `TestRunWatch_FinishesTheRemoteEditBehindARekey`, `TestReport_WithPassAddsWorkAndKeepsTheLastHeldState`, `TestOneShotFollowUp_StopsWithoutProgressOnFailureAndAtTheBound` |
| One-shot sync remains a bounded observe-plan-execute run without a live user-intent mailbox. | `TestBootstrapSync_NoChanges`, `TestBootstrapSync_WithChanges`, `TestOneShotEngineLoop_ClosedResultsStillProcessBufferedRetryWork`, `TestOneShotEngineLoop_UnauthorizedTerminatesAndDrainsQueuedReady` |
| Dry-run one-shot sync builds a preview without executor, durable cursor commits, durable observation-findings writes, or shortcut child publication, while normal engine startup/store/close housekeeping still runs. | `TestRunOnce_DryRun_NoExecution`, `TestLoadDryRunCurrentInputs_ObservationFindingsStayScratchOnly`, `TestLoadDryRunCurrentInputs_LocalObservationFindingsStayScratchOnly`, `TestRunOnce_DryRun_MountRootDoesNotSaveScopedDeltaToken` |
| One-shot and watch share the same admission/runtime contract, while watch alone keeps the runtime alive for future timer release. | `TestWatchRuntime_ArmRetryTimer_KicksImmediatelyWhenRetryIsDue`, `TestReleaseDueHeldRetriesNow_ReleasesHeldRetryEntriesOnly`, `TestReleaseDueHeldTrialsNow_ReleasesFirstHeldScopeCandidateAsTrial`, `TestPhase0_OneShotEngineLoop_TrialSuccessMakesFailuresRetryableAndReinjectableWithoutExternalObservation` |
| After a throttle, re-admission ramps from a bounded batch released in held order, a further 429 re-blocks and restarts the ramp, admitted work a scope activation blocks is taken back from the watch and one-shot outboxes, the scope row is deleted with its last blocked rows, and the 429s an episode costs stay bounded by the transfers in flight (R-6.8.24). | `TestRunOnce_ThrottledBacklogRampsInsteadOfFlooding`, `TestNextReleaseAllowance_RampsFromTheWorkerCountAndDoubles`, `TestEngineFlow_TrialSuccess_ReleasesOneBatchAndKeepsTheScopeOnProbation`, `TestEngineFlow_ThrottledMemberDuringProbation_ReblocksRetiresTheOutboxAndResetsTheRamp`, `TestWatchRuntime_ScopeActivationRetiresAdmittedWorkItBlocks`, `TestOneShotRunner_ScopeActivationRetiresPendingOutbox`, `TestSyncStore_ReleaseScopeWork_RemovesTheScopeWithItsLastBlockedRows`, `TestSyncStore_ReleaseScopeWork_EmptyBatchRemovesAScopeWithoutBlockedWork`, `TestSlowStartThresholdAfterThrottle_RemembersHalfTheBatchThatDrewThe429`, `TestSyncStore_BlockScope_Roundtrip`, `TestEngineFlow_ReleaseScopeBatch_ReportsThroughRuntimeSignals` |
| A released ramp batch's members are admitted at distinct instants spread over a one-second window on the engine clock, the trial at the Retry-After deadline is not jittered, the watch loop wakes for a parked member by its admit timer and a one-shot pass waits for its parked members in-loop and still completes (R-6.8.24). | `TestReleaseJitter_OneShotBatchIsAdmittedOverAWindow`, `TestReleaseJitter_OneShotBatchUnderReverseDispatch`, `TestWatchRuntime_ParkedBatchMembersWaitForTheirAdmitInstant` |
| A fresh plan opens into its backlog with two of a drive's actions in flight, doubling per round of successful completions up to the worker pool size; a 429 during the slow start hands the drive to the throttle scope, a plan that starts under a persisted scope is the scope's from the start, and the next fresh plan starts over (R-6.8.25). | `TestColdStart_OneShotRampsInFlightFromTwo`, `TestColdStart_OneShotHandsOverToTheThrottleScopeAndResetsPerPlan`, `TestWatchRuntime_DispatchIsGatedByTheColdStartAllowance`, `TestColdStart_AllowanceDoublesPerRoundToThePoolSize`, `TestColdStart_HandOverOpensTheTargetToTheScope` |
| Superseded action completions retire exact stale work without success, ordinary retry, blocker mutation, or old-plan dependent admission. | `TestClassifyResult_LocalPersistenceAndScopeRouting`, `TestEngineFlow_ProcessNormalDecision_SupersededRetiresSubtreeWithoutRetryOrSuccess`, `TestEngineFlow_ProcessTrialDecision_SupersededClearsExactRetryAndDiscardsEmptyScope`, `TestOneShotEngineLoop_SupersededCompletionRetiresDependentsWithoutSuccessOrRetry` |
| Admission validates ready actions against committed current truth before worker dispatch. Remote mismatches retire old-plan work as superseded, do not persist ordinary retry work, do not release dependents, and dirty watch mode for replacement planning. | `TestEngineAdmissionFreshness_RemoteMismatchRetiresWithoutDispatchOrDependents` |
| Canceled worker/admission freshness checks fail closed without store reads or side effects, while shutdown completion may bypass canceled-context freshness only to collapse newly-ready graph frontier without dispatch. | `TestActionFreshness_CanceledContextFailsClosedWithoutStoreRead`, `TestOneShotRunner_HandleOneShotCompletion_AfterCancelCompletesReleasedReadyAsShutdown` |
| Post-sync housekeeping skips the transfer-artifact cleanup for a pass whose protected-root refresh failed, warns once per failure streak and resumes when the store reads again; a failing trash purge is one Warn naming the trash directory per streak, Debug on repeats, Info on recovery (R-6.6.30). | `TestPostSyncHousekeeping_SkipsTransferCleanupWhenProtectedRootsCannotBeRead`, `TestPurgeExpiredTrash_WarnsOncePerFailureStreak`, `TestPostSyncHousekeeping_SkipsProtectedShortcutRoots`, `TestPostSyncHousekeeping_SkipsConfiguredChildRoots`, `TestPostSyncHousekeeping_RespectsIncludedDirs` |
| A store failure while clearing `retry_work` after a successful action is returned from the completion path, and a store failure reading the persisted remote block scopes is returned from the 403 probe instead of answering "no blocked boundaries". | `TestClearRetryWorkOnSuccess_StoreFailureIsReturned`, `TestClearRetryWorkOnSuccess_RemovesResolvedRetryRow`, `TestPermHandler_Handle403_BlockScopeReadFailureIsReturned` |
| Repeated supersedes of one action are visible: Debug on each, one Warn per streak, reset on success. From the streak threshold the action is parked in retry_work with exponential backoff (30 s doubling to 1 h), so a wedge is a slow retry visible in status instead of a hot replan loop in watch mode (R-2.8.11, D-059d). Only consecutive supersedes count: any other completion of the action, or a plan without it, resets the streak. | `TestRecordSupersede_WarnsOnceWhenTheSameActionKeepsGoingStale`, `TestPruneSupersedeStreaks_ForgetsActionsTheNewPlanLacks`, `TestApplySupersededCompletion_ParksTheSameActionAfterAStreak`, `TestSupersedeParkDelay_DoublesToACap`, `TestRunOnce_RandomizedBidirectionalEditsConverge` |
| Every ready frontier leaves through one engine-owned dispatch order (`dispatchOrder`, nil in production), and a test can serialize the worker pool to one worker (`serializeWorkers`), so a test pins the execution order forward, reversed or seeded-shuffled (D-090). Every TestRunOnce_ scenario also passes with the frontier reversed, and one model-test seed replays the same actions and trees. `ONEDRIVE_GO_DISPATCH_ORDER=forward|reverse|shuffle:<seed>` runs the package's test engines under one order. The reversed re-run and the replay are exhaustive tests: `go test -short` (the per-PR `verify quick` lane) skips them (`skipInShort`), and the full profiles and nightly CI run them. | `TestReverseDispatch_RunOnceScenariosPass`, `TestReverseDispatchTable_ListsEveryRunOnceScenario`, `TestModel_SameSeedReplaysIdentically` |
| Timing-dependent watch behavior is tested on a virtual clock (`inVirtualTime`, a `testing/synctest` bubble): the real timers run and the tests state exact durations -- one quiet period after the last signal, the max wait under sustained churn, one write-coalescing hash one cooldown after the last write, the safety scan at its interval -- without real waiting (D-095). | `TestDirtyBuffer_VirtualTime_FlushesOneQuietPeriodAfterTheLastSignal`, `TestDirtyBuffer_VirtualTime_ContinuousSignalsFlushAtExactlyTheMaxWait`, `TestHandleWrite_VirtualTime_RapidWritesCoalesceIntoOneHash`, `TestWatch_VirtualTime_SafetyScanFindsAMissedFileAtItsInterval` |
| One invariant assertion (`assertSyncInvariants`) checks a converged sync for the model test and key scenarios: a further pass changes nothing; the trees agree; every content either side held is still on a side, in the fake OneDrive's recycle bin or versions, or in the local trash; integrity and foreign-key checks pass; no `:park:` rows; baseline agrees with remote_state and local_state; retry_work, held deletes and pending creates are empty; no partials, upload sessions or empty conflict reservations remain (D-095). | `TestRunOnce_RandomizedBidirectionalEditsConverge`, `TestRunOnce_ItemMovedIntoRemotelyDeletedFolderIsDeletedLocally`, `TestRunOnce_RemoteMoveChainIntoVacatedPathConverges`, `TestRunOnce_RemoteFileReplacedByNonEmptyFolderWithLocalEditKeepsBoth` |
| The model test's operations cover the shapes that found most sync bugs (D-096): beyond create, edit, delete, move, mkdir and type change, a remote swap, a remote delete and re-create at the same path, the same rename on both sides, divergent renames, a local move with a remote edit and the reverse, a hard link, a move then re-create, a folder rebuilt with new inodes, case-only renames (on a case-sensitive filesystem), cross-side case-variant creates, percent and plus names, and wiping a side, whose first pass must hold the deletes (D-002). The model plays the user who renames a name collision the engine reports, and advances the engine clock past retry backoff between passes. The model test, its replay, chaos and crash variants, the planner model and the reconcile matrix are the exhaustive tier: skipped under `go test -short` (the per-PR lane), run by `verify default`, `verify public` and nightly CI; the named TestRunOnce_ scenarios they generalize stay in the short lane. | `TestRunOnce_RandomizedBidirectionalEditsConverge` |
| A process killed at a commit boundary recovers (D-091): crash points after OneDrive moved an item, after a local rename, after an upload reached OneDrive, after a download was renamed into place, after a local delete moved the file into the trash under a still-pending entry, after OneDrive deleted an item, and before an observation window or an action outcome commits (`crash_points.go`; `crashAt` hooks on the executor configuration and the store, nil in production). A fresh engine on a snapshot taken at each point (committed database, local tree with its file identities, OneDrive as it is) converges with nothing lost, and so does one whose database lost every commit since the last checkpoint. At the two delete boundaries the file deleted on OneDrive is in the local trash exactly once and restores, the file deleted locally is deleted on OneDrive exactly once and neither re-uploaded nor downloaded again, and both baseline rows are gone. The model test also crashes every batch of a few seeds at a seeded point and continues on the recovered engine. The per-crash-point test runs in every lane; the seeded model variant is exhaustive and skipped under `-short`. | `TestCrashRecovery_EveryCrashPointConverges`, `TestCrashRecovery_DeleteBoundariesDeleteOnceAndKeepTheTrashEntry`, `TestModel_CrashAtSeededPointsRecovers` |
| Executor live preconditions use the same superseded completion path as worker-start and admission stale-work checks. | `TestExecuteRemoteDelete_NotFoundPreflightReturnsStalePreconditionAndDoesNotDelete`, `TestExecutor_RemoteDelete_ConditionalMismatchReturnsStalePrecondition`, `TestExecutor_RemoteMove_ConditionalMismatchReturnsStalePrecondition`, `TestExecutor_LocalDelete_FolderIdentityChangedReturnsStalePrecondition`, `TestExecutor_Upload_SourceHashChangedBeforeTransferReturnsStalePrecondition`, `TestExecutor_Download_TargetAppearsBeforeRenameReturnsStalePrecondition` |
| Watch runtime and admission record aggregate stale-work perf counters without exposing paths or IDs. | `TestEngineAdmissionFreshness_RemoteMismatchRetiresWithoutDispatchOrDependents`, `TestWatchRuntime_QueuePendingReplanRetiresOldOutbox`, `TestSnapshotAttrs_IncludesStaleWorkAndObservationCounters` |
| Watch shutdown enters drain before accepting cancellation-closed intake channels as terminal, and non-canceled scheduler closure is fatal instead of a clean stop, so retry and trial timers stop at the observable shutdown boundary. | `TestWatchRuntime_RunNonDrainingWatchStep_CanceledClosedReplanStartsDrain`, `TestWatchRuntime_RunWatchLoop_CanceledClosedReplanDrainsBeforeReturn`, `TestWatchRuntime_RunWatchLoop_CanceledClosedReplanDrainsFromBootstrapPhase`, `TestWatchRuntime_RunNonDrainingWatchStep_ClosedReplanWithoutCancelErrors`, `TestRunWatch_ShutdownStopsRetryAndTrialTimers` |
| The observer-backed watch loop starts through an explicit phase boundary after bootstrap quiescence, so a stale bootstrap phase cannot make steady-state watch exit cleanly without observer intake. | `TestWatchRuntime_BeginObserverBackedRunningNormalizesBootstrapPhase`, `TestWatchRuntime_BeginObserverBackedRunningRejectsDrainingOrDuplicateObservers`, `TestWatchRuntime_RunWatchLoop_BootstrapPhaseQuiescesAndReturnsToRunning`, `TestRunWatchLoop_SteadyStateCompletionWithoutDrainingIsAnError`, `TestRunWatchUntilQuiescent_BootstrapCompletionStaysClean`, `TestStartObservers_RefusalDuringDrainClassifiesAsShutdown` |
| A panic inside a watch observer or the full-refresh goroutine fails its mount instead of terminating the process, a panic in the socket.io wake source leaves watch polling on its interval, and each is logged at Error with its stack so the failure stays diagnosable after the goroutine is gone. | `TestRecoverObserverPanic_DeliversPanicAsObserverError`, `TestRecoverObserverPanic_CleanExitReportsNothing`, `TestRecoverObserverPanic_FullChannelDoesNotBlock`, `TestRunWatch_LocalObserverPanicFailsMountInsteadOfCrashing`, `TestRunFullRemoteRefreshAsync_PanicFailsMountInsteadOfCrashing`, `TestRunWatch_SocketIOWakeSourcePanicKeepsWatchRunning` |
| The protected roots are the one `Engine` field rewritten while a run is live; the lock keeps the loop's observer construction and the off-loop refreshes race-free (R-6.3.4). | `TestRefreshProtectedRootsFromStore_ConcurrentReadersAreRaceFree` (run under `-race`), `TestNewMountEngine_LoadsPersistedShortcutProtectedRoots` |
| Parent engines persist shortcut-root state, merge that state into protected-root observation filters on startup, route protected-root lifecycle signals through the parent engine, and suppress/report protected roots without turning them into parent content. | `TestNewMountEngine_LoadsPersistedShortcutProtectedRoots`, `TestNewMountEngine_DoesNotProtectCleanupPendingShortcutRoot`, `TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots`, `TestApplyShortcutObservationBatch_PersistsParentStateBeforeHandler`, `TestFullScan_ProtectedRootIdentityMatchSuppressesRenamedRoot`, `TestFullScan_ExpectedSyncRootIdentityMismatchReturnsMountRootUnavailable`, `TestEngine_ReconcileRemovedFinalDrainMissingLocalAliasReleasesWithoutRemoteDelete` |
| Parent shortcut-root transitions are table-validated and watch-mode alias lifecycle stays engine-internal before only child work snapshots reach multisync. Ack handles are live-parent capabilities and zero handles fail loudly. | `TestShortcutRootTransitionTableCoversStates`, `TestShortcutRootTransitionMatrixEnumeratesEveryStateAndEvent`, `TestValidateShortcutRootTransitionAllowsKnownLifecycleEdges`, `TestValidateShortcutRootTransitionRejectsIllegalLifecycleEdges`, `TestWatchRuntime_HandleProtectedRootEventOwnsLocalAliasRename`, `TestShortcutChildAckHandleZeroReturnsExplicitErrors` |
| Console reporting states each fact once: sign-in and observer-exit failures are returned, not logged; persistent conditions (skipped files, block scopes, poll outages, socket.io retries, condition summaries, blocked shortcut cleanup) are reported when they start; shutdown bookkeeping stays below WARN. | `TestEngineFlow_FatalAuth_ReturnedOnceNotLogged`, `TestEngineFlow_SuppressedShutdownCompletionError_QuietForCancellationAndRepeatFatal`, `TestPollOutageLog_WarnsOncePerOutage`, `TestEngineFlow_SkippedFindings_WarnOnlyWhenNew`, `TestEngineFlow_BlockScope_ReportedOnceWhenItStarts`, `TestSocketIOWakeSource_RetriesNeverWarn`, `TestWatchRuntime_ObserverExit_ReturnedOnceWithCause`, `TestWatchConditionSummary_WarnsOnlyForNewActionableKinds`, `TestEngineFlow_LogFailureSummary_AggregatesRetryWorkBelowWarn`, `TestEngine_ReconcileShortcutRootLocalStatePersistsCleanupBlockedWithoutError`, `TestEngine_ReconcileShortcutRootLocalStateRepeatedCleanupBlockedIsQuiet`, `TestWatchRuntime_DroppedObservationRescanFailureWarnsOnce`, `TestWatchRuntime_BootstrapWaitLogsOnlyWhenCountsChange`, `TestWorkerPool_CommitsCompletedOutcomeAfterPoolCancellation`, `TestFullScan_UnreadableFileIsSkippedItemNotWarning` |
| A rescan that changes no committed truth schedules no replan and leaves the admitted plan dispatchable. | `TestWatchRuntime_SafetyScanWithoutNewTruthKeepsAdmittedPlan`, `TestWatchRuntime_SkippedFindingsReplanOnlyWhenTheyChange` |
| Pending watch replans retire old-runtime work that has not started yet, including dependents released by already-running old actions, and leave replacement work to a fresh plan from current truth. | `TestWatchRuntime_RunNonDrainingWatchStepPrioritizesReadyReplanOverDispatch`, `TestWatchRuntime_QueuePendingReplanRetiresOldOutbox`, `TestWatchRuntime_PendingReplanRetiresDependentsReleasedByRunningAction`, `TestWatchRuntime_PendingReplanLocalObservationFailureReschedulesDirtySignal` |

## The Engine Clock Is One Capability

Every time-dependent decision in the runtime -- `Now`, delayed callbacks,
tickers, sleeps, and retry jitter -- goes through a single `syncClock` value on
the engine. The local and remote observers hold the same capability for their
safety-scan ticks, retry sleeps, and write-coalesce timers.

These were five independently injectable function fields (`nowFn`, `afterFunc`,
`newTicker`, `sleepFn`, `jitterFn`) plus three more on the local observer. Each
was individually reasonable and the set was not: a test could replace `nowFn`
while leaving a real ticker, and that pair describes a state production can
never reach -- a clock that has jumped an hour while the tickers pacing the
runtime have not moved. Assertions made under that mix prove nothing about
production behavior.

One interface makes the incoherent combination unrepresentable rather than
merely discouraged, which is the same "make illegal states unrepresentable"
rule the rest of the codebase applies to domain types, applied to the test
surface. Tests that need to observe rather than replace behavior wrap the clock
(`observingClock`, `observerTestClock`) so the underlying clock still supplies
the semantics.

## Construction

`newEngine()` wires one mounted content root into a runtime:

- rooted sync tree
- store
- planner
- executor configuration
- transfer manager
- permission handler
- optional websocket wake source

Production entrypoints now call `NewMountEngine()` with:

- the authenticated session capabilities for the target mount
- `EngineMountConfig`, the sync-owned constructor input carrying the non-client
  runtime facts for that mount
- logger, perf collector, and drive-verification flag

`NewMountEngine()` is the only exported engine constructor. Config-shaped
inputs such as `ResolvedDrive` are compiled into `EngineMountConfig` above the
engine boundary; there is no extra exported builder layer above
`EngineMountConfig`, and `engineInputs` remains an internal seam for focused
engine tests rather than a parallel production construction model.

For mount-root runtimes, the engine also carries the configured
`remoteRootItemID`. That root item defines the remote boundary for scoped
observation, planning, and execution. Planner and executor consume that
engine-owned root context directly; ordinary actions do not re-thread
per-action target-root overrides. Mount-root delta capability is
resolved in config and passed into the engine as construction input; the
engine does not reopen catalog state just to decide whether a mount root
should try folder delta. Separately configured shared folders and managed
shortcut child mounts both use this mount-root engine path when their remote
root is below the backing drive root.

Permission handling is intentionally split three ways:

- probe/evidence (`permission_probe_*.go`, `permission_evidence.go`) gathers
  filesystem or Graph facts only
- runtime classification (`engine_result_classify.go`) owns the condition
  family and ordinary retry/scope class for the finished action
- direct engine runtime permission handlers (`engine_runtime_permissions.go`
  and `engine_runtime_lifecycle.go`) gather probe evidence when needed,
  persist exact retry work or blocked scope rows, activate or release timed
  write scopes, and emit engine-owned logs without an intermediate policy DTO

Normal completion handling and trial reclassification both reuse the same
engine-owned permission-evidence handlers. The probe still returns facts only;
the engine decides directly whether to persist delayed retry work, persist
blocked retry work, activate a timed scope, or fall back to generic result
handling.

The remote permission probe walks boundaries directly from the engine-owned
`driveID` and `remoteRootItemID`. There is no separate remote-root carrier object;
the root item ID is the only special case, and all ancestor walking uses the
same boundary-path helpers the probe already owns.

Permission timing follows the engine-owned runtime decision, not the probe:

- file-scoped permission failures persist delayed `retry_work` and arm the
  ordinary retry timer in watch mode
- boundary-scoped permission failures persist blocked `retry_work` and any
  timed write scope they own, but they do not arm the ordinary retry timer
- known-active-boundary outcomes are a true no-op for durable state and timer
  arming because the boundary is already active
- unmatched permission evidence falls back to the generic classified result
  path; trial reclassification reuses that same fallback instead of treating
  inconclusive permission probes as fatal runtime errors

Every persisted block scope has blocked `retry_work` under it, and the
transitions keep that true at each step (found by the chaos Graph layer,
D-092):

- scope detection runs before a failure is persisted, so the failure that
  activates a scope (a 429, a 503 with Retry-After, or the failure that
  crosses the service window) is recorded once, blocked under that scope, as
  its first trial candidate
  (`TestEngineFlow_ProcessNormalDecision_ServiceRetryAfterBlocksTheTriggeringWork`,
  `TestEngineFlow_ProcessNormalDecision_ServiceWindowBlocksTheCrossingWork`)
- a failed trial whose failure names a scope of its own moves its work there
  and transitions the trial's scope in one step, discarding it if that was
  its only blocked work
  (`TestEngineFlow_ProcessTrialDecision_ServiceRetryAfterMovesTheTrialsOnlyWork`)
- admission that re-homes a scope's trial under a higher-priority scope
  discards the scope it left when nothing is blocked under it any more
  (`TestEngineFlow_AdmitReady_RehomedWorkDoesNotStrandItsOldScope`)

## Run State Is Split By Authority

`engineFlow`'s mutable state lives in four owners, each answering one question
about a run:

- `actionScheduler` -- which actions exist, which are queued, which are running
- `retryLedger` -- the durable retry rows for this run, what is held, and in
  what order it releases
- `scopeLedger` -- the active shared blockers
- `runResults` -- what the run will report when it finishes

This does not change the concurrency model. One goroutine still owns all four,
and `scopeLedger.mu` still guards only the slice that admission checks and
status snapshots read concurrently. What changes is that the compiler now shows
which owner a method touches, so "this decision is about scheduling" or "this
decision is about retry timing" is visible in the code rather than inferred
from field names on a struct that carried several dozen of them.

Methods that touch exactly one owner moved onto it -- `markRunning` is
scheduler state, `snapshotActiveScopes` is scope state. Methods that span
owners stayed on `engineFlow`, because spanning owners is what they are for:
holding an action moves it out of the scheduler and into the retry ledger, and
releasing due trials reads the scope ledger to decide which held keys are
eligible.

`engineFlow` is still large. Splitting the state was the part that could be
done without changing behavior; splitting the coordination is a separate
question about what the runtime does, not about where its fields live.

## Watch State Is Grouped And Qualified

`watchRuntime` holds its mutable state in three named fields: `loop` (phase,
outbox, pending replan), `resources` (observer streams, refresh channels,
websocket lifecycle), and `timers` (the retry and trial timers plus the mutex
guarding them).

Two of the three were embedded. Embedding grouped the declarations but promoted
every field back to the top level, so any of the type's methods could still
reach anything, and the grouping was documentation rather than structure.
Qualified access makes each method's reach visible at its call sites, which is
the input any further decomposition of this type needs.

## Run Capabilities Are Passed, Not Reached For

Every per-run flow needs the same four things regardless of what it is
deciding: somewhere to log, the durable store, the clock, and the debug event
stream. Those arrive as `runtimeDeps`, constructed once when the run starts.

They were reached through a back-pointer to the whole `Engine`. That pointer
carried 320 references, and four targets accounted for 191 of them. Those four
are not collaborators the flow coordinates with; they are ambient capabilities
that happened to be in scope. Passing them in narrows what a flow can reach
for, and leaves the remaining engine references as what they actually are: the
specific configuration and collaborators a particular decision consults --
`driveID`, `contentFilter`, `permHandler`, `syncTree`.

The snapshot is deliberate. A run's capabilities do not change underneath it,
so `runtimeDeps` copies them at construction rather than re-reading the engine
on each access. The consequence is visible in tests: swapping the engine's
clock after a runtime exists has no effect on that runtime, and the test helper
that does so refreshes the deps explicitly rather than relying on a read-through.

## Asking For Future Work

Completion, admission, and scope decisions do not reach for the watch runtime.
They report through `runtimeSignals`: arm the held timers, arm the retry timer,
arm the trial timer, release due held work now, or mark that a replan is
needed.

One-shot and watch differ in exactly one way those decisions can observe:
whether there is a future. A one-shot pass returns when its work settles, so
nothing armed during it could fire and no replan mark has a reader. (A one-shot
run's follow-up passes are decided by the plan's `FollowUp`, not by replan
marks: a superseded action means truth changed under the run, which the next
sync observes.) Watch keeps
the runtime alive across timer ticks and observation batches, so all five mean
something.

This was previously expressed by passing `watch *watchRuntime` into `engineFlow`
methods and nil-checking it at nineteen sites -- a parent taking its own child
as a nullable parameter, since `watchRuntime` embeds `*engineFlow`. The
consequence was that `engineFlow` could not be reasoned about without knowing
which callers passed nil, and the type relationship was circular. The no-op
implementation states the same rule as a fact about one-shot rather than as an
absence every call site must remember to check.

`watchInspector` is the parallel seam for the debug-only invariant pass, which
production leaves disabled through `engine.assertInvariants`. It is separate
from `runtimeSignals` because the two have different lifetimes, and keeping them
apart stops a debug-only read from re-introducing the production coupling.

## One-Shot Sync

`RunOnce()` keeps one-shot behavior intentionally simple. Each pass:

1. shared startup prep and durable startup checks (first pass only)
2. observe and commit current remote/local truth once
3. load planner inputs once from that committed truth
4. compute SQL structural diff and reconciliation once
5. build the current actionable set in Go from structural reconciliation plus
   explicit truth-availability overlays
6. reconcile durable retry/blocker state to that actionable set
7. commit any ready publication-only actions directly through the store and
   drain publication-only dependents before worker dispatch
8. execute remaining concrete work once using the same blocker/trial admission
   model watch mode uses
9. persist outcomes

There is no mid-pass mailbox for user intent. New external DB writes during a
one-shot run are durable state for a later run.

### A One-Shot Sync Finishes Its Own Follow-Up Work

Planning moves is conservative by design (sync-planning.md, Follow-Up Work):
a plan that moves, re-keys or conflict-copies items, or defers or drops work
until those have happened, leaves ready work for the pass after it, and says
so in `actionPlan.FollowUp`. Watch mode always has a next pass; a one-shot
sync used to stop after the first and report success with that work left
(a local edit inside a folder renamed online stayed unuploaded until the next
`sync`). `RunOnce` therefore runs passes until one leaves nothing for the
next (R-2.1.7):

- after a live pass whose plan has `FollowUp.pending()`, another full pass
  runs: a fresh runner, the baseline loaded again, remote and local truth
  observed again, planned and executed exactly like the first. The planner
  says when; the engine never guesses from counts;
- it stops at the first pass whose plan leaves nothing, that made no progress
  (`Succeeded == 0`), that failed an action (failures wait in retry backoff and
  the summary already says they will be retried), when the context is done,
  or after `oneShotMaxPasses` (4) passes, after which the next sync finishes;
- a dry run is always one pass: it executes nothing, so a second pass would
  plan from the same truth;
- a follow-up pass decides the full refresh again without the caller's
  request (the first pass did it) and never carries `ConfirmDeletes`: the
  first pass released the confirmed deletes, and any held since are judged
  by the hold again;
- each pass commits its own delta cursor and observation progress, publishes
  its own shortcut child snapshot and logs its own `sync pass starting` /
  `sync pass complete` lines; a Debug line names why a follow-up runs.
  Post-sync housekeeping runs once, after the last pass, when any pass
  executed (see Post-Sync Housekeeping below);
- the returned `Report` covers the run (`Report.withPass`): action counts,
  `Succeeded`, `Failed` and `Errors` add up across passes, `Duration` runs from
  the first pass's start, and what is still held back (`DeferredByMode`, held
  deletes) is the last pass's, because a deferral every pass sees is a state,
  not work done again. The CLI's `Done in ...` line therefore covers every
  pass.

This is not a settle loop for delta lag (R-2.16.3): a follow-up pass runs only
when the run's own plan left work, and it observes OneDrive exactly like any
pass.

The current-plan pipeline is the handoff boundary between planning and runtime
startup. Observation remains entrypoint-specific, but once an entrypoint has
produced observed current truth the engine runs the same named stage sequence:
observe current truth, load current inputs, build the current plan, then
reconcile runtime state by pruning/loading surviving `retry_work` /
`block_scopes`. In code, `engine_current_plan.go` owns that shared
observe/load/build/reconcile pipeline. Stale `retry_work` and empty
`block_scopes` are pruned there, not from timer held-release paths.

Scope startup cleanup follows the same policy with a deliberate
decision-then-apply split: the engine first derives which persisted scopes are
still justified by blocked `retry_work`, then applies only the required delete
mutations. The same timed-scope liveness rule also drives runtime
rearm-or-discard handling and store-side prune helpers so empty timed scopes
do not survive by accident in one path but not another.

Within that one-shot flow, the engine now treats current-plan construction as
an explicit stage sequence: observe current truth, load current inputs, build
the current plan from that observed state, then either reconcile runtime state
or keep the dry-run build in memory without touching durable held-work state.
Dry-run engine construction uses the same state-store open, startup
normalization, and close housekeeping path as a live one-shot run. Dry-run
current observation writes the fresh remote snapshot, remote observation
findings, local snapshot, and local skipped-item findings into a scratch
planning store only; the live `observation_issues`, local snapshot, and remote
cursor remain unchanged. Dry-run also suppresses all executor-owned local
sync-tree mutators and remote Graph content mutators; the allowed side effects
are operational setup/housekeeping and scratch planning artifacts, not local or
remote content mutation. Live, dry-run, watch bootstrap, and steady-state watch
replans all use that same current-plan pipeline; they differ only in how they
collected the observed state, whether the executor runs, and whether a
deferred cursor commit is present.
The top-level coordinators should stay at that stage level rather than
inlining planner input loads, durable prune/load logic, or runtime-start
bookkeeping. The same rule applies to the explicit runtime-start,
publication-drain, and post-sync housekeeping stages: keep them next to their
stage so a reader can see the flow at a glance. Current-plan construction
should read from `engine_current_plan.go`; runtime-start/admission should read
from `engine_runtime_start.go`; and completion plus publication drain should
read from `engine_runtime_completion.go` plus the trial-specific
`engine_runtime_completion_trial.go`.

Parent child-admission readiness is part of the normal parent run path.
Multisync attaches a child work snapshot sink before it starts a selected
parent engine, then consumes live parent publications from the normal
current-plan pipeline.
That pipeline performs the same remote observation cadence decision, full local
observation, current-plan build, retry/block reconciliation, and shortcut-root
lifecycle publication the parent needs for ordinary work. One-shot parents
publish after current parent state and protected-root decisions reach an
accepted snapshot point; multisync stores that exact published snapshot for
the active runtime and starts that parent's child work only after that parent
reaches its one-shot safe point. Parent A's children do not wait for unrelated
parent B, but no one-shot child starts from a parent snapshot that is merely
cached from an earlier run. Watch initial startup starts parent runners only;
watch bootstrap and steady-state changes admit children from live parent
publications and reconcile only that parent child runner set.
Child runner admission is therefore derived from fresh parent local and remote
truth rather than a long-lived control-plane cache, and multisync never
constructs a temporary startup parent engine for shortcut admission.

Once one-shot shutdown has started, late worker completions no longer re-enter
the normal outbox path. The engine runs them through the same shutdown
completion boundary watch drain uses, immediately collapsing any newly-ready
frontier back into shutdown completion instead of handing it to dispatch.
Shutdown collapse carries an explicit freshness-bypass marker only for that
graph bookkeeping path; ordinary canceled worker/admission freshness checks
fail closed and do not authorize executor side effects.
Likewise, one-shot idle waiting must release any already-due held retry or
trial work before blocking on worker completions; held-work timing remains part
of the shared engine runtime, not a watch-only side path. If idle held-release
reduction fails after shutdown-completing a returned exact frontier, the
one-shot runner consumes that shutdown work immediately instead of surfacing the
same outbox to a second shutdown-completion pass.

Full-remote-refresh cadence is restart-safe even when a full remote refresh
returns no delta cursor. The engine still advances the persisted cadence in
`observation_state` so enumerate-only and mount-root sessions do not fall
into back-to-back expensive full refreshes.

That cadence is capability-driven, not websocket-driven:

- delta-based observation schedules the next full remote refresh 24 hours out
- enumerate-only observation schedules it 1 hour out

Websocket wakeups are additive only. They wake delta polling sooner, but they
do not replace delta polling and they do not change the full-refresh cadence.

### Post-Sync Housekeeping

`postSyncHousekeeping` (`engine_housekeeping.go`) runs after the last pass:
the transfer-artifact cleanup (`driveops.CleanTransferArtifactsWithOptions`,
owned partials and expired upload sessions) and the local-trash purge. Both
are best effort, and both report a persistent failure once per streak
(R-6.6.30):

- the artifact cleanup removes every owned partial outside the protected
  roots with no age check, so it runs only when
  `refreshProtectedRootsFromStore` succeeded for this pass. A refresh failure
  skips the cleanup for the pass instead of walking with a stale list, which
  could delete a child mount's in-flight partials; it is one Warn when the
  streak starts, Debug on repeats, Info once when the list reads again
  (`Engine.protectedRootRefreshFailing`)
- `purgeExpiredTrash` is one Warn naming the trash directory when a purge
  starts failing, Debug while it keeps failing, Info once when it works again
  (`Engine.trashPurgeFailing`); the trash would otherwise grow without bound
  with nothing saying so. Watch maintenance purges at most once per
  `trashPurgeInterval`

### Absence Must Be Confirmed

A full remote refresh deletes what a complete enumeration did not return, so
"did not return" has to mean exactly that:

- the drive-root refresh builds its seen set from the raw ID of every live
  item the enumeration returned (`FullEnumerationWithShortcutTopology`), not
  from converted events; an item the converter skipped (a package facet, an
  orphan whose parent it could not fetch) was still enumerated (D-008); every
  orphan it still synthesizes is then confirmed by `GetItem`, and only a 404
  or a deleted facet becomes a delete; an unconfirmed orphan keeps its local
  copy and is judged again by the next full refresh;
- a mount-root enumeration (`/children` walk or folder delta) is rejected as
  incomplete when an item hangs off a folder the same listing did not return,
  and every synthesized orphan is confirmed by `GetItem`: only a 404, a
  deleted facet, or ancestors that provably lead outside the mount (the parent
  walk reaches a drive root without passing the mount) is absence; a live item
  still inside, one with an empty or unresolvable parent, or a lookup that
  failed is left for the next listing (D-009);
- a complete observation that omits an active shortcut placeholder no longer
  drains it on that evidence alone: `confirmShortcutCompleteOmissions` looks
  the binding up and re-adds it when it is still a live shortcut to the same
  target, marks it target-unavailable (local data kept) when the lookup fails,
  and lets it drain only on a 404, a deleted facet, or a changed target
  (D-007).

### An Expired Token Is A Full Refresh

A 410 means a stretch of changes will never be reported, deletes included. So
every recovery is the full refresh above, not a bare token-less enumeration
(which re-listed the drive but synthesized no orphans, so a delete made while
the token aged out was missed until the next scheduled refresh, up to a day
later): one-shot drive-root observation falls through to
`executeDriveRootFullObservation`, watch mode's remote observer calls the same
function through `SetDeltaExpiredRecovery` and continues from its token, and
mount-root delta re-lists with orphan detection. The recovery is marked as a
full refresh, so the cadence restarts from it (R-2.15.3). In watch mode a recovery
that fails backs off like any failed poll and keeps the expired token, and
recoveries are at least `expiredDeltaRecoveryMinInterval` (15 min) apart, so
one that fails the same way every time cannot re-enumerate the drive in a hot
loop (`TestWatch_RepeatedlyFailingRecoveryBacksOff`). One-shot runs never
reset a token on sustained 5xx; only watch mode measures that.

The 410's inner code (`newDeltaExpiredError`) decides what a confirmed orphan
means:

- `resyncChangesApplyDifferences`: OneDrive's view is current, an orphan was
  deleted, and the local delete goes through the delete-safety hold (D-002)
  like any other;
- `resyncChangesUploadDifferences`, or no recognized code: OneDrive asks for
  the items it did not return to be uploaded again. The orphan's remote row
  goes and so do its baseline rows (`ReuploadLocal`), so a surviving local copy
  plans as an upload. It is never a local delete.

A full refresh this client starts on its own schedule is apply-differences.

| Behavior | Evidence |
| --- | --- |
| An expired delta token (410) recovers through the full refresh with orphan detection in one-shot and watch runs, and the resync flavor decides what an orphan means: apply-differences deletes it, upload-differences or an unknown flavor forgets its synced agreement so the local copy is uploaded again (R-2.15.3). | `TestRunOnce_DeltaExpiredRecoverySynthesizesMissedDeletes`, `TestRunOnce_DeltaExpiredUploadDifferencesKeepsAndReuploadsLocal`, `TestWatch_DeltaExpiredRecoversThroughTheFullRefresh`, `TestMountRootEvents_ResyncFlavorDecidesWhatAnOrphanMeans`, `TestNewDeltaExpiredError_ParsesTheResyncFlavor` |
| A full refresh never treats an enumerated item as gone, shared-folder orphans need a live 404/deleted/moved-out answer, a listing with an unlisted parent is incomplete, and an omitted shortcut drains only on a confirmed removal. | `TestFullRefresh_ItemTheConverterSkipsIsNotSynthesizedAsDelete`, `TestConfirmMountRootOrphans_ListingThatSkippedALiveItemDeletesNothing`, `TestValidateMountRootListingParents_ChildUnderUnlistedFolderAbortsListing`, `TestApplyShortcutRootCompleteOmission_ConfirmedLiveBindingStaysActive`, `TestFullRefresh_OrphanStillLiveOnOneDriveIsNotDeleted` |

If watch mode shortens `observation_state.next_full_remote_refresh_at` after
startup, it must rearm the in-memory timer in that same control path.
Mount-root enumerate fallback therefore clamps the persisted deadline and
immediately rebuilds the active timer instead of waiting for a later full
refresh commit.

In this increment, "degraded" means exactly "running without delta." The main
drive-root watch path remains delta-based. Mount-root watch chooses its
mode from the config-resolved capability surface:

- business/sharepoint mount roots skip folder delta and use recursive
  enumeration
- personal mount roots try folder delta first and retry it on later passes
  after any temporary enumerate fallback

## Watch Mode

`RunWatch()` is the long-lived runtime. It owns:

- observer startup and shutdown
- dirty-signal intake and debounce
- snapshot refresh and SQLite reconciliation
- action admission and dispatch
- action completion drain
- retry and trial timer scheduling
- periodic maintenance ticks and full remote refresh
- graceful drain on shutdown

`RunWatch` starts with the same shared startup boundary one-shot uses.
That startup boundary owns persisted-scope normalization, account-auth
normalization, and the single startup baseline load. `bootstrapSync`
consumes that startup baseline; it is not allowed to lazily reload or
recreate startup state.

The watch loop is the single owner of mutable scheduling/runtime state:
bootstrap/running/drain phase, outbox, dispatch admission, held-work timing,
refresh coordination, and drain behavior. Remote observation commits now
follow the same single-owner rule:
watch observers and full-refresh goroutines emit one loop-applied
`remoteObservationBatch` value, and the loop itself owns projected remote
observation commits, cursor commits, observation-finding reconciliation, dirty
marking, and refresh-timer re-arm.
Observer lifecycle follows that same runtime-owned model. `startObservers`
populates the watch runtime's observer error stream, local-event stream,
remote-batch stream, skipped-item stream, refresh channels, and active-observer
count directly. `watchPipeline` keeps only loop inputs that are not already
runtime-owned: baseline, replan-ready debounce output, worker completions,
maintenance ticks, worker pool, and cleanup.
That owner boundary stays concrete in code too: `runWatchLoop` is the shared
outer owner for bootstrap, steady-state, and drain; `runNonDrainingWatchStep`
is the one gated non-draining `select`; and `runDrainStep` remains the only
distinct shutdown shell instead of routing bootstrap, idle, and outbox cases
through overlapping wrappers.
After bootstrap succeeds, `RunWatch` enters observer-backed running through a
named runtime phase boundary before starting observers. Bootstrap may use loop
termination to mean "bootstrap quiesced"; the observer-backed loop may only
terminate through drain completion or a fatal runtime error. Starting observers
while draining or while another observer set is still active is an engine
invariant violation and fails before new goroutines are admitted.
Cancellation wins over non-draining intake, including a dirty-buffer
`replanReady` channel that closes because its context was canceled. The watch
loop must enter drain and emit `shutdown_started` before it accepts any
closed-input shortcut as terminal, otherwise retry/trial timers can outlive the
observable shutdown boundary. If the dirty scheduler closes without
cancellation, the watch loop reports it as an unexpected scheduler stop rather
than treating the closed channel as a clean watch-mode exit.
Engine debug events expose those lifecycle points for tests and diagnostics
only; they do not own or redirect control flow.
Watch replan debug events include timestamped pending-replan, old-outbox
retirement, running-drain, local-observation, planning, new-plan, and
first-post-replan-dispatch stages. Each event carries outbox, running, and idle
worker counts so production/debug traces can distinguish workers waiting on
legitimate replanning from coordinator stalls.
Refresh timer callbacks and full-refresh goroutines signal through stable
runtime-owned channels for the lifetime of the watch session. The watch loop
disables select cases by phase instead of mutating those channel pointers
mid-shutdown, so asynchronous senders never race the single-owner runtime over
which signal channel is authoritative.

Local watcher events are scheduler hints plus scoped local-truth updates. The
local observer re-observes the affected file, directory, or safety-scan
snapshot and emits a `localObservationBatch`; the watch loop applies that batch
to `local_state`/`observation_state` before marking the dirty scheduler. Remote
delta batches and full remote refresh results similarly arrive as loop-applied
durable observation batches. Websocket wakes remain scheduler hints only. After
debounce or wake, watch mode refreshes local truth as a recovery/full-snapshot
step, loads already-committed remote/current state, runs SQL
comparison/reconciliation, rebuilds the current actionable set in Go,
reconciles durable retry/blocker state, and then admits runnable actions. There
is one steady-state replan entry for that work: refresh local truth, load the
already-committed remote/current state, build the current plan, reconcile
runtime state, then append the resulting concrete worker frontier through the
watch-owned frontier helpers. DirtyBuffer emits only a coarse
dirty/full-refresh scheduler signal, and that signal feeds only this
steady-state replan path; it does not define a second planning model.

Bootstrap uses that same outer owner after the initial
observe/build/reconcile/start-runtime handoff. The only bootstrap-specific semantics
that remain are the bootstrap quiescence rule, bootstrap wait logging, and the
fact that observers do not start until that bootstrap-owned quiescent boundary
has been reached.

Watch runtime replacement is linear: one current runtime graph at a time.
Dirty observation while work is still queued or running sets a pending replan
flag instead of appending a second graph into the current runtime. Setting that
flag retires not-yet-dispatched old outbox actions as superseded runtime work:
the engine removes them from queued state, does not mark them successful, does
not admit their dependents, and does not create ordinary retry rows for them. A
replan signal that is already ready is consumed before the watch loop enables
old-outbox dispatch for that step, so ready replacement pressure wins over
handing more old work to idle workers. That pressure exists only when committed
truth changed: a safety or recovery scan whose snapshot changes no
`local_state` row and whose findings change nothing persisted marks nothing
dirty, so the admitted plan keeps dispatching (R-2.8.21). The scan's events are
counted against the baseline, so during a backlog it re-reports every change
the plan already covers; treating that as dirty retired every plan at every
safety scan, and once a replan took longer than the safety interval (ten
minutes on 11,000 entries, 2026-11-01) nothing was ever dispatched while
`status` read up to date.
Already-running or already-submitted worker actions are not automatically
invalidated by the old plan becoming replaceable; they continue through normal
completion and executor-side precondition policy. If such a completion releases
new dependents while a pending replan exists, that newly-ready frontier is also
retired instead of appended to the old outbox. Once running work settles, the
loop rebuilds from current committed truth plus durable `retry_work` /
`block_scopes`. If local observation fails before that replacement runtime is
installed, the retired work stays retired and the same dirty/full-refresh intent
is rescheduled through `dirtyBuffer` instead of being dropped. The same
reschedule rule applies when an idle runtime consumes a ready dirty signal
directly and local observation fails before a replacement runtime is installed.
The idle watch-step owner still receives debounced coarse dirty hints directly;
an empty outbox does not mean steady-state replans can be deferred until some
other watch event arrives.

Watch replans on observed changes, but the work a plan leaves for the next
pass (`actionPlan.FollowUp`) often follows from the engine's own commits, which
nothing observes: a baseline re-key whose OneDrive edit is downloaded next
pass, a move made on OneDrive, a bootstrap rename made before the observers
started. The loop therefore records the installed plan's follow-up
(`watchLoopState.followUp`, set before the runtime starts so publication-only
successes count) and, once that plan's actions have settled (empty outbox,
nothing running: checked after each completion, after a replan installs, and
when bootstrap quiesces), marks the dirty buffer itself when the plan made
progress and failed nothing (R-2.1.7). Consecutive self-scheduled replans are
bounded by the same `oneShotMaxPasses`; a replan from an observed change
starts a new streak.

The watch runtime records perf counters at the same single-owner boundaries:
admission counts actions superseded before worker dispatch, pending replan
retirement counts old outbox/frontier actions retired without success
semantics, and steady-state replan records worker-idle duration for waiting on
old running actions, local refresh, planning, and runtime installation. These
counters are aggregates only and do not include paths, item IDs, drive IDs, or
account details.

Retry/trial is not an alternate planner. Timer-driven follow-up only
re-releases exact held actions that are already part of the current runtime.
The engine holds dependency-ready exact work in memory, keyed by exact
`RetryWorkKey` and grouped by `ScopeKey` for blocked scopes. Timer ticks do
not rebuild subset plans, do not compute dependency closure, and do not
revalidate stale rows. Stale-row cleanup belongs only to normal
current-plan build/runtime-state reconcile. Dependency tracking stays inside `depGraph`, but runtime
completion does not: the engine owns quiescence and no longer waits on a
graph-owned completion signal.

Released held work always re-enters the engine-owned publication-drain stage
before any worker dispatch. Timer-released `ActionBaselineUpdate` and
`ActionCleanup` actions stay engine-side, commit through the store, and unlock
dependents without ever crossing into the worker pool.

Action completion drain stays inside the engine boundary. When a completion
unlocks publication-only dependents, watch mode commits those mutations
synchronously and keeps draining them on the engine/store side until concrete
worker actions are the only dispatchable work left. That publication drain is
an effectful engine/store stage, not a pure transform: it durably applies
publication mutations, routes publication failures through normal completion
classification, and returns only the remaining concrete worker frontier.
Successful worker actions and successful publication-only actions still reuse
the same exact-action success bookkeeping: mark finished, increment success
counts, clear exact retry state, optionally feed scope-success detection, then
admit newly-ready dependents.
One-shot and watch coordinators still own their outbox state explicitly.

Runtime completion handling follows the same boundary shape everywhere:
classify the finished exact action, apply the resulting durable/runtime
mutation, then reduce the ready frontier through publication drain plus due-
held release. It does not mix that decision step with worker-queue ownership.
In code, the top-level effectful boundary is `applyRuntimeCompletionStage`,
while `reduceReadyFrontierStage` owns the frontier reduction and
`runPublicationDrainStage` remains the publication-only substage. If an action
completion is classified as `superseded`, that boundary retires the exact action
and its old dependent subtree without incrementing success or failure counts,
clears any exact `retry_work` row for the stale action, avoids blocker mutation,
and marks watch mode dirty so replacement work is produced only by a later plan
from current truth. Because a superseded action leaves no retry row and no
user-facing line, the flow counts consecutive supersedes per action (type, old
path, path): each is logged at Debug with its reason, the third in a row warns
once, and a success resets the streak. A precondition that can never hold is
therefore visible instead of a silent stall.

Admission uses the same action-freshness predicate as worker-start validation
before a ready action enters the worker outbox. Retry-held or scope-held work is
validated when it later becomes a dispatch candidate, not while it is already
known to be held. If committed current truth proves the planned action stale,
admission applies the superseded completion path directly: exact retry state is
cleared, the action and its old dependents are retired without success
semantics, and watch mode is dirtied for a fresh plan.
Local truth only participates when `observation_state.local_truth_complete` is
true; suspect local truth cannot reject a planned action from `local_state`.
Remote truth participates once the state store has an established remote truth
authority, and can reject path or item identity changes that prove the planned
remote assumption false. The shared predicate fails closed if an executable
runtime action is missing planner view truth once committed truth is
authoritative. A dependent upload after a planned remote move tolerates eTag
churn caused by that planned move, but still rejects when current remote truth
proves the target item identity or content changed.
Move actions validate both endpoints: the main planned view path and the
opposite source/destination peer needed for the mutation.

Watch replan failure policy is also explicit. Pre-authority local observation
failure is recoverable and reports that no replacement runtime was applied; the
pending-replan and direct-idle callers reschedule the same dirty/full-refresh
intent through `dirtyBuffer` rather than restoring retired work or dropping the
trigger. Once the engine starts depending on authoritative current-truth writes
or runtime state, failure is fatal to the current watch session: remote
observation apply, observation findings reconciliation, local snapshot commit,
current-plan build from committed truth, and runtime startup/admission all fail
closed. Shutdown cancellation is the
one exception: if context cancellation lands during that steady-state replan
handoff, the loop clears the best-effort sync-status batch and exits cleanly
into shutdown instead of surfacing a fatal watch error.

Once shutdown drain has sealed new admission, late action-completion
classification or persistence errors are treated as shutdown bookkeeping only.
The loop logs them, keeps draining the already-owned completion sources, and
returns clean shutdown rather than converting cancellation timing into a fatal
watch error. That same shutdown-completion boundary now lives in
`engine_runtime_shutdown.go` and is shared by watch drain plus one-shot
late-completion handling after fatal shutdown or cancellation.

### Maintenance And Refresh

Watch mode still owns periodic maintenance ticks for summary logging and full
remote refresh cadence, but it no longer polls SQLite for mysterious external
runtime-state changes. Live scope/runtime state changes must arrive through the
engine's own control paths; there is no generic external DB reconciliation
loop.

Watch summary grouping is engine-owned. `watch_summary.go` builds only raw
watch-condition aggregates: condition-key counts, total condition count,
retrying count, and raw remote-write-block groups keyed by `ScopeKey`. Those
remote-write groups come only from active remote-write
`block_scopes`, not from every projected condition group that happens to share
the same condition key. The watch runtime owns log phrasing and churn
suppression separately. Signature/fingerprint helpers belong in that watch-log
boundary too, but the fingerprint itself must stay raw-only and must not
depend on the current human-readable breakdown wording. The store does not own
grouped watch-condition projections or watch-specific presentation.

## Drive-Root And Mount-Root Runtime Shapes

The engine supports two runtime shapes:

- drive-root sessions rooted at the remote drive root
- mount-root sessions rooted below the remote drive root

The constructor chooses the runtime shape from `EngineMountConfig.RemoteRootItemID`.
A blank `RemoteRootItemID` means drive-root observation and execution. A non-blank
`RemoteRootItemID` means the engine must stay scoped below that remote boundary for
observation, planning, and execution.

Separately configured shared folders and managed shortcut child mounts use the
mount-root path when their content root is below the backing drive root, but
those product surfaces should not be confused with the engine's internal
runtime model.

Embedded shared-folder links discovered inside another synced drive are still
suppressed by ordinary drive-root content observation and never become nested
engine-owned sub-sessions. For parent namespace mounts, the same observer emits
those placeholders as shortcut snapshot facts before remote cursor commit. The
parent engine also persists parent-owned `shortcut_roots` state in its sync
store: binding item ID, alias path, target identity, protected parent-local
paths, lifecycle state, and same-path replacement waiting state. The
multi-mount control plane consumes the parent-declared child work commands only to
start, final-drain, stop, or purge child runners. The child command shape owns
validation, equality, and lowering into `EngineMountConfig`, so multisync can
pass execution data through without inspecting shortcut lifecycle, protected
paths, or status facts.

Managed shortcut child work snapshots also carry the parent-observed local root
identity when the parent has materialized the alias directory. Child engines
verify that identity at construction and before full local scans. If the local
root disappeared, moved away, or was deleted and recreated at the same path, the
engine reports `ErrMountRootUnavailable` instead of producing an empty local
snapshot that could plan remote deletes.

Shortcut placeholder rename/delete mutations are parent-engine operations by
binding item ID. The parent engine observes the need for local alias
rename/delete from its protected-root scan/watch path, applies the Graph
mutation itself, persists the retry/block state in `shortcut_roots`, and then
publishes the resulting child work commands. Multisync must not rediscover parent
remote state, call parent-drive alias mutation APIs, or decide parent alias
lifecycle.

Shortcut-root lifecycle decisions are expressed as deterministic planner
helpers before the engine shell performs I/O. The planner chooses remote
shortcut-observation transitions, alias delete/rename, historical projection move,
ambiguous-rename, cleanup-blocked, waiting-replacement, duplicate-target, and
local-root-unavailable state transitions from stored shortcut-root records plus
observed value outcomes. Local shortcut lifecycle planning uses explicit
observation values and local actions (`noop`, `keepRecord`, `dropRecord`,
`materializeRoot`, `mutateAlias`, and `moveProjection`) so the shell performs
Graph, filesystem, and SQLite effects only, then feeds those outcomes back
through the same transition table.

When a child final drain completes, multisync acknowledges that completion to
the already-running parent engine through the concrete sync-owned
`shortcutChildAckHandle` obtained from that live engine. A zero handle is an
error, not a no-op, so multisync cannot synthesize an acknowledgement path
outside a live parent runner. The parent engine first persists
`removed_release_pending`, then releases its own protected alias projection or
promotes a waiting same-path replacement to `active`. After parent release, the
old binding remains in `removed_child_cleanup_pending` without protected paths
and publishes a child artifact cleanup request carrying an opaque
acknowledgement reference, child mount ID, child local root, and cleanup
reason. Multisync purges the
child-owned artifacts through its injected cleanup executor and acknowledges
cleanup; it does not derive cleanup scope from the parent namespace. Only then
does the parent delete the cleanup-pending row. If cleanup is interrupted or
blocked, startup and later parent shortcut refresh retry the release from
`removed_release_pending` or `removed_cleanup_blocked`; a later complete remote
observation batch is not required to release that parent protected-root.

If the parent later observes that a retiring or cleanup-blocked shortcut alias
directory is gone, it treats that as user-directed manual discard of the local
projection. The parent removes the shortcut root, or promotes a same-path
waiting replacement, without calling the shortcut delete/rename Graph mutation
path and without interpreting the missing directory as child content deletion.
Multisync receives the release through a parent cleanup request publication and
purges child-owned state.

If a mounted sync root disappears, the engine treats that as mount lifecycle
(`ErrMountRootUnavailable`) rather than as content deletion below the root.
Startup and watch root-loss checks stop or fail the mount before planner input
can be built from a missing root.

## Conflict Handling

Conflicts remain engine-owned and immediate:

- edit/edit and create/create preserve both versions by renaming local to a
  conflict copy and downloading remote to the canonical path
- edit/delete is planner-expanded into a create-by-parent upload that recreates
  the remote item from the edited local file
- executor-time local-delete hash mismatch reports a superseded stale
  precondition so the next replan can emit the correct upload from current truth

The engine stores only the baseline and current-truth facts needed for the next
plan; conflict handling is visible as those concrete actions and executor
outcomes.

### Observer Panics Fail One Mount, Not The Process

Watch observers run for the lifetime of a mount, in goroutines the runtime
owns. A panic in one of them used to terminate the whole process, so a bug
reachable from one account's data took down every other mount with it --
including mounts that shared nothing with the failing one.

Each observer goroutine now recovers at its boundary and delivers the panic as
an observer error. That is deliberately classified as a *fatal* observer error
rather than an ordinary exit. The runtime tolerates a single observer stopping
because the conditions it was built for -- watch-limit exhaustion, transient
remote failure -- leave the mount degraded but coherent, and the local observer
has a documented periodic-full-scan fallback. A panic leaves undefined state
instead, with no fallback: continuing would run the mount half-blind while its
status still read healthy. Failing the mount is recoverable, and multisync's
`MountRunner` already isolates a mount failure. Silently observing nothing is
not recoverable, because the component that would report the gap is the one
that stopped.

The recovery is registered as the goroutine's last defer so it runs first while
unwinding, delivering the error before the observer's channels close rather
than racing a closed-channel exit. Both this path and `MountRunner`'s existing
recovery log the panic stack: the recovered value names what failed but not
where, and the goroutine that could answer that is gone by the time anyone
reads the report.

The two other goroutines the watch runtime starts have the same boundary, and
each delivers the panic through the error path it already had:

- The full-refresh goroutine (`runFullRemoteRefreshAsync`) recovers in
  `recoverFullRemoteRefreshPanic` and sends a refresh result carrying
  `fatalErr` over `refreshResults`, the channel a finished refresh uses, so
  the loop learns of it in order. `applyRemoteRefreshResult` marks the refresh
  finished and returns that error, which fails the mount: the same reasoning
  as the observer case, since the ordinary retry-when-idle path would repeat
  the bug at every refresh interval while the mount's status read healthy.
- The socket.io wake-source goroutine recovers in `runSocketIOWakeSource` and
  turns the panic into the error its Info "notifications stopped; still
  checking on the poll interval" path already handles. The wake source is an
  accelerator, so losing it changes nothing but latency; the panic itself is
  still logged at Error with its stack.

The one remaining boundary is outside this package: the per-mount runner
goroutine multisync starts around `RunWatch` recovers too, so a panic that
escapes the engine fails that mount and restarts it with the usual backoff
(see sync-control-plane.md, `RunWatch`).

### Loop Completion Is Phase-Checked, Not Assumed

Bootstrap and steady state share `runWatchLoop`, but they may not end for the
same reasons. Bootstrap ends at quiescence; steady state may only end by
draining. The phase boundary in `beginObserverBackedRunning` is what keeps a
stale bootstrap phase from ending steady-state watch early, and the loop now
also checks the rule it depends on: a steady-state loop that reports
completion in any phase other than draining returns an error naming the phase
and the context state.

The distinction matters because the two outcomes are indistinguishable to the
caller. Returning `nil` says "the shutdown you asked for finished"; but a loop
that stops while its context is still live has stopped observing a mount the
caller still believes is synced. That is silent staleness, and it is the one
failure mode watch mode cannot report through its own health output, because
the runtime that would report it is the one that stopped.

Observer startup is classified the same way. Cancellation can land between
bootstrap draining and observer startup, and the runtime then refuses to start
observers into a drain. That refusal is not a context error, so it is matched
by sentinel through `isWatchObserverStartupStopped` rather than by string, and
only a refusal that coincides with a done context reads as a clean stop.

## Outcome And Scope Lifecycle

The engine classifies results into:

- success cleanup
- retryable transient work
- actionable current-truth/content problems
- shared blocker activation / re-arm / release-or-discard decisions

### Durable persistence rules

- observation-time invalid or unsyncable truth -> `observation_issues`
- exhausted transient exact work -> `retry_work`
- shared blockers -> `block_scopes` plus blocked `retry_work`
- normal worker-result handling and retry/trial never write
  `observation_issues`; only observation-owned reconciliation does
- execution may still persist `retry_work` for a failed exact action even when
  a later observation pass may record the corresponding durable current-truth
  issue; planning suppression plus retry pruning collapses that overlap on the
  next normal current-plan cycle
- durable `retry_work` / `block_scopes` mutations are fail-closed runtime
  control writes; if the engine cannot persist or transition them, it stops
  the current runtime instead of logging and continuing

### Runtime admission model

`depGraph` remains dependency-only. The engine still performs final scope
admission after dependency readiness:

- build the graph from current actions
- ask the graph for ready work
- gate that ready work against active scopes
- pace the dispatch of admitted work: the slow start of a fresh plan and
  the admission instants of a released ramp batch (see "Cold start and
  batch jitter" below)
- dispatch allowed work
- keep exact failed/blocked roots represented durably in `retry_work`

Held work stays unresolved in the dependency graph. Dependents are not
persisted as cascade retry rows; they remain blocked naturally until the exact
parent action succeeds, is released for retry, or the whole runtime is
replaced by a fresh current-plan cycle.

The benchmark target is earlier durable reconciliation, not removal of the
post-graph admission gate.

### Scope taxonomy

The target scope families are:

- `service`
- `throttle:target:drive:<driveID>`
- `quota:own`
- `disk:local`
- `perm:dir:write:<path>`
- `local:read-only:<path>`
- `perm:remote:write:<boundary>`
- `account:throttled`

Write-side shared permission blockers are first-class persisted
`block_scopes`. Read-side subtree blockers are observation-owned boundary facts
carried on `observation_issues` via `ScopeKey`, not a second persisted scope
table.

### Sync root ownership

Before it opens the state store, `newEngine` takes an exclusive,
non-blocking `flock` on `<data dir>/locks/root-<st_dev>-<st_ino>.lock`
(`engine_root_lock.go`) and holds it until `Close` has closed the store; on a
startup failure the store's close also runs first. Keying the file by the
root's device and inode means a symlink, bind mount or second configuration
sharing the data dir contends for the same lock, and a refused engine never
touches its store. The lock is a regular file in the data dir rather than a
lock on the folder itself because `flock` on a directory descriptor fails on
Linux NFS (`EBADF`) and some FUSE filesystems (`ENOSYS`). Lock files are
never removed. Errnos that mean "cannot lock here" (`ENOTSUP`, `EOPNOTSUPP`,
`ENOLCK`, `EINVAL`, `EBADF`, `ENOSYS`) are a warning, not a failure. Startup
also warns when the root carries another sync client's marker or sits under
macOS `~/Library/CloudStorage` (R-2.9.5, R-2.9.6,
`TestEngine_SecondEngineOnSameRootRefused`,
`TestSyncRootLock_IsADataDirFileKeyedByDeviceAndInode`,
`TestSyncRootLock_UnsupportedFilesystemWarnsAndContinues`,
`TestSyncRootLock_ReleasedOnlyAfterTheStoreCloses`,
`TestSyncRootLock_ReleasedWhenStartupFailsAfterTakingIt`,
`TestWarnAboutForeignSyncClients`).

### Permission recovery

Permission blockers are revalidated automatically; there is no manual retry or
manual maintenance CLI for them. Observation may create or clear read-boundary
facts directly when current truth proves the blocker or its recovery. Probe and
execution may create or clear persisted write scopes when write access is
affirmatively denied or restored. A raw `403` or `os.ErrPermission` is only a
trigger to probe; the probe returns evidence only, and only the engine-owned
permission handlers may activate or release a write-side scope. The probe's
Graph failures are evidence (none) and Debug; a failure to read the persisted
block scopes (`ActiveRemoteBlockedBoundaries`) is a store error that
`handle403` returns, never "no blocked boundaries", which would re-admit
writes under a read-only subtree.

Ownership splits by access kind:

- read boundaries (`perm:dir:read`, `perm:remote:read`) are observation-owned
  boundary tags on `observation_issues`
- write scopes (`perm:dir:write`, `perm:remote:write`) are
  probe/execution-owned persisted `block_scopes`
- file-level local permission failures remain exact `retry_work` rows with the
  normal reconcile backoff; they are not promoted into `block_scopes` unless a
  later probe proves a subtree-wide boundary

Permission handling may still tag blocked `retry_work` with a read-boundary
`ScopeKey` for derived truth and reporting, but only
`ScopeKey.PersistsInBlockScopes()` outcomes may materialize `block_scopes`
rows.

Read boundaries clear only when a later observation batch no longer proves the
corresponding issue-boundary fact.
Remote write block scopes clear through normal timed trials, successful write work, or
cleanup that leaves no blocked work. They do not have a separate global
maintenance cadence.

A local `EROFS` is handled by the same local probe as `os.ErrPermission`
(`shouldHandleLocalPermission`). The probe walks up from the failing path's
parent while each directory is still on a read-only mount (statfs mount
flags `ST_RDONLY`/`MNT_RDONLY`, which cover `bind,ro`; `faccessat(W_OK)`
answering `EROFS` when statfs cannot) and stops at the sync root, so the boundary
is the inner read-only mount point or the root itself. It records a
`local:read-only:<path>` write scope with condition `local_read_only`: the
same family, blocking and trial behavior as `perm:dir:write`, but its own
condition, because no permission change fixes it. A writable parent keeps the
failure per file (`TestPermHandler_HandleLocalPermission_ReadOnlyMountIsOneBoundary`,
`TestPermHandler_HandleLocalPermission_ReadOnlySyncRootBlocksWholeTree`,
`TestPermHandler_HandleLocalPermission_ReadOnlyWithWritableParentStaysPerFile`).

Remote `403` handling is intentionally narrow:

- raw `403` never creates a permission block scope by itself
- only remote-write actions may invoke remote write-denial probing
- probe-confirmed write denial may activate `perm:remote:write`
- observation-owned `remote_read_denied` findings come only from remote
  observation/probe at the observation orchestration seam

### Retry and trial release

Retry and trial release is retry-owned, but it is no longer timer-time
replanning. Current-plan preparation prunes stale `retry_work` against the
latest actionable set, loads the surviving `retry_work` / `block_scopes`, and
initializes the current runtime's held-work indexes. Admission then decides,
for each dependency-ready exact action, whether it dispatches now, stays held
behind `next_retry_at`, or stays held behind an active `scope_key`.

Held retry release and held trial release operate only on that current
runtime state:

- held retry release emits exact held actions whose `next_retry_at` is due
- held trial release emits one deterministic held blocked candidate for each
  due scope
- neither held-release path rebuilds an `actionPlan`, refreshes current
  truth, or walks a second dependency closure

Scope lifecycle is owned only by `block_scopes` plus blocked/unblocked
`retry_work`. Timed transient scopes are discarded when no blocked
`retry_work` remains for their `scope_key`. Remote write block scopes follow
that same rule; recovery happens through normal timed trials or successful
writes, not through a separate maintenance loop. A scope row may still share a
`scope_key` with observation or ready retry rows for reporting/history, but
the engine keeps a blocker active only while blocked `retry_work` still
exists. Scope release updates both authorities in one logical step: store-side
blocked rows become ready immediately, and the current runtime flips the
matching held entries into due retry-held work so one-shot and watch can
continue without waiting for a replan. Trial-driven reclassification from one
blocked scope to another follows that same ownership rule: the newly
reclassified exact work moves to the new scope immediately, but the old scope is
discarded only after its prior `scope_key` no longer owns any blocked
`retry_work`. If blocked work remains under the old scope, the engine rearms that
retained scope's next trial interval in the same transition so it does not stay
immediately due after the just-finished trial.

### Release ramps and activation retires admitted work

Measured on a live account on 2026-11-01 (R-6.8.24): with 10,000 small
uploads planned against a rate-limited drive, a successful trial released the
whole backlog at once, the workers drained it through one 429 after another,
each failure re-held its action and re-activated the scope, and the next trial
repeated the flood. The client made 22-28 requests a second, of which 3,167
and then 5,416 per five-minute window were 429s, while about ten files were
uploaded, and `throttle:target:drive:<id>` was "released" 191 times in 15
minutes against one announced activation. Two rules fix both halves:

- **Release ramps.** A successful trial puts the scope on probation instead of
  removing it: `releaseScopeBatch` releases the first `nextReleaseAllowance`
  held entries of the scope in held order (the transfer worker count, at
  least `releaseRampMinimumBatch`), the store marks exactly those rows ready
  (`ReleaseScopeWork`), and the scope stays in the ledger with that allowance
  (`activeScope.ReleaseAllowance`) and a trial re-armed one interval out, so
  a probation that stalls (every member superseded, a replan rebuilt the
  runtime) recovers by an ordinary trial. The batch's members are dispatched
  as trials of the scope, which is what lets admission pass them while the
  scope still blocks everything else, and what routes their outcomes back:
  the batch's last member to finish (`BatchOutstanding` reaches zero)
  doubles the allowance and releases the next batch. The ramp advances per
  batch, not per success: releasing a doubled batch on every member success
  cascaded 4+8+16+32+64 admissions off five successes, so the whole backlog
  was "released" (and the scope removed) long before it had run, and the
  next 429 began a new announced episode on every pass. A batch whose last
  member failed releases nothing; the trial that is due one interval later
  resets the count and its success continues the ramp. The scope is
  removed, the `scope_released` event emitted and "Resumed syncing" said
  once, by the batch that releases the last blocked row (the store deletes
  the row in that transaction). A 429 by a member re-blocks:
  `extendScopeTrial` takes the new `Retry-After`, resets the allowance, and
  the next trial starts from the first batch again. The 429 also leaves the
  ramp a memory, `SlowStartThreshold` (`slowStartThresholdAfterThrottle`):
  half the batch that drew it, at least the first batch. The next ramp
  doubles only up to that threshold and then grows by one per batch
  (`nextReleaseAllowance`; TCP's slow start to ssthresh, then additive
  increase, RFC 5681 §3.1), because pure doubling overshoots the limit by up
  to 2x on every cycle and each overshoot costs a full `Retry-After` plus
  Microsoft's penalty for requests made while throttled. The threshold is
  persisted in `block_scopes.slow_start_threshold`: a one-shot pass builds a
  fresh runtime from the store, and a watch restart does too, so a memory
  kept only in the ledger would be lost exactly when it is needed. The
  allowance and the outstanding count stay runtime-only; a restart starts a
  fresh trial, and since persisted scopes are loaded before any admission, a
  run started inside the `Retry-After` holds everything the scope blocks and
  sends nothing to the drive before `next_trial_at`.
- **Activation retires admitted work.** Each block activation
  (`applyBlockScope`, `extendScopeTrial`,
  `transitionTrialScopeToPersistedBlock`) bumps the scope's `Generation` and
  then asks the runtime for the admitted, not yet dispatched work the scope
  blocks (`admittedWorkOwner.takeAdmittedWorkBlockedBy`: the watch loop's
  outbox, or the one-shot results loop's); the flow holds it under the scope
  and persists it blocked through the ordinary `admissionHoldScope` path.
  Work a worker already took finishes and is classified normally. A member's
  completion carries the generation it was released under
  (`TrialScopeGeneration`): a success from before a re-block is an ordinary
  success and releases nothing, and a member whose scope was removed while
  it ran is handled as an ordinary completion (its 429 starts a new episode).
  One-shot's dispatch channel is unbuffered like watch's, so admitted work
  stays engine-owned until a worker takes it; with a plan-sized buffer there
  was nothing to take back. Both loops apply finished outcomes before handing
  out the next action, so a 429 is seen before more work reaches the workers
  and an episode costs at most the transfers already in flight.

The announcement rules (R-6.6.30) are unchanged: one Info per throttling
episode, repeats and batch releases at Debug, "Resumed syncing" once when the
scope is actually removed.

### Cold start and batch jitter

Two follow-ups measured on the same live account after the ramp landed.

- **A fresh plan slow-starts** (R-6.8.25, `engine_runtime_cold_start.go`).
  The ramp above paces re-admission once a scope exists; a plan with no
  scope opened at full `transfer_workers` concurrency, and eight workers of
  small uploads on a personal drive made 19 requests a second and drew the
  first 429 within a minute. The scheduler, the single owner of dispatch
  state, keeps one `coldStartRamp` per throttle target
  (`Action.ThrottleTargetKey`, the drive Microsoft meters and a throttle
  scope blocks): an in-flight allowance that starts at
  `coldStartInitialAllowance` (2) and doubles after each round of that many
  successful worker-run completions, up to the worker pool size
  (`workerPoolSize`, the pool's floor included). `markRunning` and
  `markFinished` keep the per-target in-flight count, so an action whose
  dependents are waiting counts as one in flight and nothing more, and
  dependency order is never starved: the dispatch picker takes the first
  admissible outbox entry, not necessarily the head. A block activation
  (`applyBlockScope`, `extendScopeTrial`,
  `transitionTrialScopeToPersistedBlock`) hands the target over
  (`handOverColdStart`): a throttle scope takes its drive, a service scope
  every target, and from then on the scope's ramp paces admission and the
  allowance no longer gates. A plan that starts under a persisted throttle
  scope (a run inside the `Retry-After`, a restart on probation) is handed
  over in `initializeRuntimeState`, so the two ramps never stack. The
  allowance is runtime state of one plan: `initializeRuntimeState` resets it
  on every one-shot pass, the watch bootstrap and each replan, which is also
  when a changed worker count takes effect. Publication-only actions never
  reach a worker and do not count. Both loops and the reverse-dispatch test
  order go through the same picker.
- **A released batch is spread over a window** (R-6.8.24). `releaseScopeBatch`
  gives each member an admission instant, `trackedAction.AdmitAt`, uniformly
  within `releaseBatchAdmissionWindow` (1 s) of the release through the
  engine clock's `Jitter`, so a batch of 32 does not reach the drive as one
  spike and tests stay deterministic on the manual clock. The trial itself,
  released by `drainDueHeldWorkNow` at the server-given `Retry-After`, has no
  jitter: it is the one request that deadline allows. The watch loop's
  picker arms an admit timer for the earliest parked member and wakes on
  it; a one-shot pass has no runtime signals by design, so its results loop
  waits in-loop on the same clock, bounded by the window and interrupted by
  any completion, rather than ending the pass with members parked. A
  queued action held back with nothing in flight to make room is a broken
  invariant and ends the pass with an error rather than a hang.

The shared picker is `nextDispatchable`: the first outbox entry, in order,
that is due and whose target has room; -1 and the earliest admission instant
when none is. `markDispatched` is the one place an action leaves the outbox
for a worker, and it emits `action_dispatched` for diagnostics.

A successful completion clears the action's `retry_work` row
(`clearRetryWorkOnSuccess`). A store failure there is returned like any other
store write failure in the completion path, not logged and dropped: the row
left behind keeps `status` listing a file that just synced as retrying and
schedules a redundant retry.

Fatal authentication failures are not retry-owned. A `401` action completion
marks the account auth requirement and stops the runtime; it must not create
`retry_work`, activate `block_scopes`, or leave a timed trial path behind. The
catalog auth requirement is the durable restart fact for that class of failure.
The account is marked once per run, and the failure is returned, not logged:
the CLI prints it once as a sign-in message, and the same 401 on other
in-flight transfers adds nothing (`fatalCompletionError`).

## Console Reporting

A console WARN is shown to every user at default verbosity and ERROR even
with `--quiet`, so the engine keeps both for things a user must notice and
reports each fact once (R-6.6.21, R-6.6.22, R-6.6.30):

- Per-file failures belong to the sync console's activity line. The executor
  logs a failed action at Debug, and the end-of-pass retry summary is INFO
  (R-6.6.12).
- Errors the engine returns (fatal sign-in, all observers stopped, worker
  baseline/commit failures, scanner root checks) are not also logged at
  WARN/ERROR.
- A persistent condition is reported when it starts or changes: skipped
  files only when newly found (diffed against the persisted observation
  issues), block scopes once on activation (throttling and outages at Info),
  poll failures once per outage via `pollOutageLog` with an Info on recovery,
  socket.io retries once at Info, the watch condition summary only when a new
  kind of condition appears, and the inotify limit once with its fix. A
  blocked shared-folder cleanup is persisted and warned once when it first
  blocks (or its reason changes), retried quietly after that, and never
  returned as a pass error. A failing rescan after dropped watch events warns
  once per distinct error, and the initial pass's periodic "waiting for the
  remaining changes" line repeats only when its counts move.
- A worker commits a finished action's outcome on a context detached from
  shutdown (bounded by `outcomeCommitTimeout`), so Ctrl-C between the
  transfer and its commit cannot drop the record of work already done.
- Shutdown is quiet: cancellation and repeat-fatal errors in shutdown
  bookkeeping, a canceled safety scan or full-refresh commit, and the final
  dirty-buffer drain are Debug.

## Activity Hook

`Engine.SetActivityHook` reports each finished user-visible file action as an
`Activity` (kind, path, old path, size, success, will-retry, error), so the CLI
can name files instead of printing counts (R-6.6.18). It is emitted from
`applyRuntimeCompletionStage`, the one completion seam shared by one-shot and
watch mode, right after classification: successes and failures are reported,
shutdown and superseded completions are not (the work stopped on request or
was replaced by newer truth), and bookkeeping actions (baseline updates,
cleanups) are never activity. A dry run reports each planned action with
`Planned` set. `internal/multisync` tags activity with its mount
(`MountActivity`) so multi-drive output can say where it happened. The hook
runs on the completion path and must not block.

## What The Engine Does Not Own

The engine does not own:

- multi-drive orchestration
- a durable action queue
- a mixed failure-reporting/control table

Those concepts do not belong in the target architecture.
