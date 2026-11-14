# Sync Control Plane

GOVERNS: internal/cli/sync.go, internal/multisync/*.go, internal/synccontrol/*.go

Implements: R-2.4.8 [verified], R-2.4.9 [verified], R-2.4.10 [verified], R-2.8.1 [verified], R-2.8.2 [verified], R-2.8.3 [verified], R-2.8.11 [verified], R-2.9.1 [verified], R-2.9.2 [verified], R-2.9.3 [verified], R-2.9.7 [verified], R-3.4.2 [verified], R-6.3.3 [verified], R-6.3.4 [verified], R-6.6.15 [verified], R-6.6.16 [verified], R-6.6.17 [verified], R-6.6.32 [verified], R-6.10.6 [verified], R-6.10.13 [verified], R-6.3.8 [verified], R-6.3.9 [verified], R-6.3.10 [verified]

## Control Socket Exposure

The control socket can stop sync and make the daemon write profile output, so
reaching it is equivalent to controlling the runtime.

The socket file is created 0600, which is what stops another user connecting to
it. The attack that permission does not stop is replacement: anyone who can
create entries in the socket's directory can unlink it and bind their own, and
the CLI then talks to them. So the directory property that matters is who may
*write* there, not who may read.

That distinction is load-bearing. `MkdirAll` only applies its mode to
directories it creates, and the socket path falls back to a name under the
system temp directory when the data-directory path would exceed the Unix socket
length limit -- a name derived from a predictable hash, so a local user can
create it first and own it. Startup therefore checks the directory rather than
trusting the mode it asked for.

A directory is acceptable when it belongs to this user or to root, and is
either closed to other writers or sticky. The sticky bit is what makes a shared
temp directory safe: others may create their own entries but cannot remove
ours. An attacker-owned directory is refused even when sticky, because its
owner can always remove what it contains. Refusing on group or other *read*
access instead would reject both the system temp directory and a conventional
0755 data directory, neither of which lets anyone replace the socket.

Request bodies are bounded. The daemon is long-lived and decodes the perf
capture request whole, so an unbounded body is a way for anything that can
reach the socket to spend the daemon's memory.

## Overview

The control plane owns multi-mount sync lifecycle. It sits above the
single-mount engine in `internal/sync` and answers questions the engine should
not answer:

- which runtime mounts are active right now
- how those mounts are started and stopped
- how control-socket reload changes the active mount set
- how live control-socket requests are serialized through the running control loop
- how per-mount failures are isolated from one another

`sync.go` is the CLI entrypoint for this layer. `internal/multisync` is the
runtime package that implements it.

## Ownership Contract

- Owns: Multi-mount sync lifecycle, runtime work from exact parent-declared child work snapshots, child runner start/final-drain/stop/purge process work, execution-failure skips, explicit child-artifact cleanup from the CLI-supplied data directory, reload diffing, control-socket ownership, and per-mount panic/error isolation.
- Does Not Own: Parent-drive Graph discovery, shortcut placeholder mutation, parent sync-dir shortcut alias filesystem policy, single-mount content observation, planning, execution, retry/trial policy, or sync-store persistence semantics.
- Source of Truth: The current `config.Holder` snapshot, the CLI-compiled standalone mount configs, plus the runtime runner set, run-owned parent child-work snapshot cache, and `runners` map owned by the active one-shot/watch runtime.
- Allowed Side Effects: Session creation, engine construction/closure, child artifact cleanup scoped by parent snapshot, Unix control-socket bind/unlink, per-mount goroutine startup, live perf capture, and control-plane logging.
- Mutable Runtime Owner: One-shot and watch each own their current parent child-work snapshots. `RunWatch` owns the live `runners` map, the runner generation counter and each runner's stop deadline and stuck mark. Each `watchRunner` owns one cancel function and one completion channel for exactly one mount, and one generation that its events carry.
- Error Boundary: The control plane converts mount startup into structured per-mount startup outcomes, returns one-shot startup classification separately from completed `MountReport` values, and keeps watch-runner failures isolated to the affected mount or log path. Engine-internal errors remain inside the single-mount boundary.

## Verified By

| Behavior | Evidence |
| --- | --- |
| `RunWatch` reports ready, reload and stop transitions through `WatchLifecycle`, and a watch that never started reports none. | `TestRunWatch_ReportsLifecycleTransitions`, `TestRunWatch_StartupFailureIsNotReady` |
| `RunWatch` starts the runnable runtime mount set, skips incompatible-store mounts with immediate warnings, refuses a start in which no drive could start, and with every drive paused starts, reports ready with the paused drives and idles until a reload resumes one (R-3.4.2). | `TestOrchestrator_RunWatch_SingleMount`, `TestOrchestrator_RunWatch_MultiMount`, `TestOrchestrator_RunWatch_SkipsIncompatibleStoreMountWhenAnotherMountStarts`, `TestOrchestrator_RunWatch_ReturnsStartupFailureWhenNoMountStarts`, `TestOrchestrator_RunWatch_IdlesWhenAllMountsPausedUntilOneIsResumed` |
| The Unix control socket is the single owner lock for one-shot and watch sync, is acquired before parent engines start, reports owner mode/status, rejects unsupported one-shot control requests with typed `foreground_sync_running`, and keeps reload/stop serialized through the watch control loop. Dry-run one-shot sync uses the same owner lock as live one-shot sync. | `TestRunOnce_ControlSocketBlocksWatchOwner`, `TestRunOnce_BindsControlSocketBeforeEngineStartup`, `TestRunOnce_DryRunBindsControlSocketBeforeEngineStartup`, `TestRunWatch_BindsControlSocketBeforeEngineStartup`, `TestOrchestrator_OneShotControlSocket_StatusAndRejectsNonStatus`, `TestOrchestrator_ControlSocket_StatusAndStop`, `TestE2E_SyncWatch_OwnerSocketBlocksCompetingOwners` |
| The control socket also exposes live perf snapshots and explicit capture bundles for both one-shot and watch owners without creating a second network surface or durable metrics store. | `TestOrchestrator_OneShotControlSocket_PerfStatusAndCapture`, `TestOrchestrator_OneShotControlSocket_PerfCaptureRejectsInvalidDuration`, `internal/cli/perf_test.go` (`TestMainWithWriters_PerfCaptureJSON_ForOneShotOwner`, `TestMainWithWriters_PerfCaptureFailsWhenNoOwnerIsRunning`) |
| Each mount's perf collector is carried in that mount's run context, for a watch runner and for a one-shot pass, so what the work records through its context (HTTP requests and retries, store transactions, transfers) shows in that mount's live snapshot and is counted once each in the owner aggregate and the command collector (R-6.6.15). | `TestOrchestrator_MountPerfIncludesContextRecordedMetrics`, `TestOrchestrator_OneShotMountPerfIncludesContextRecordedMetrics`, `internal/sync/engine_perf_context_test.go` (`TestRunOnce_StoreTransactionsCountOnTheRunContextCollector`) |
| Socket files are permissioned private, ownership is a `flock` on `<socket>.lock` held for the owner's lifetime, stale sockets are replaced only under that lock, and empty hash-runtime socket directories are cleaned up on close. | `TestControlSocketServer_PermissionsStaleCleanupAndRuntimeDirRemoval` |
| Every control-socket wait has a deadline (R-2.9.7): headers, the whole request (a client that never finishes its body is cut off), idle connections and the header block are bounded server-wide; a command the watch loop does not take in time is answered `busy` (503) and changed nothing; a reload it took but had not finished when its answer was due is answered `reload_in_progress` (202); a handler that waits moves its own write deadline, so it still answers after the server-wide timeouts; a perf capture is at most 5 minutes. | `TestControlSocketServer_SetsReadIdleAndHeaderLimits`, `TestControlSocket_SlowBodyIsCutOff`, `TestHandleControlRequest_LoopNotAcceptingAnswersBusy`, `TestHandleControlRequest_SlowReloadAnswersInProgress`, `TestHandleControlRequest_LongWaitOutlivesTheServerTimeouts`, `TestPerfCaptureOptions_RejectsTooLongDuration` |
| A control server that stops on its own reports on `RunWatch`'s fatal channel, and the watch ends with that error (exit 1) instead of syncing on with no way to pause, reload or query it (R-2.9.7). | `TestStartControlSocketServer_UnexpectedServeEndIsFatal`, `TestRunWatch_ControlServerExitEndsTheWatch` |
| Watch mode announces each drive start failure once and again only when its reason changes or the drive has started in between, including the carried-over CLI config failures; repeats of the same shortcut-refresh or cleanup failure stay at DEBUG. | `TestReconcileWatchRunners_AnnouncesEachStartFailureOnceUntilItChanges`, `TestWarnOnChange_RepeatsOfTheSameFailureStayBelowWarn` |
| A removed shared folder whose final sync did not finish cleanly keeps its local copy, and multisync says so in one plain Info line naming the folder (not its mount ID), repeated only when the reason changes; unchanged repeats from watch passes go to Debug. The memory is mutex-guarded because one-shot parents finalize their children concurrently. | `TestReportKeptFinalDrains_NoticesOnceUntilReasonChanges` |
| A watch runner that exits with an error is restarted after exponential backoff from the RunWatch loop's own wake-up timer, warned about once per distinct error with the retry delay, and starts over after a healthy run; stale events from runners the orchestrator already stopped, and canceled final drains, produce no warning. | `TestHandleWatchRunnerEvent_BacksOffAndWarnsOncePerDistinctRunnerFailure`, `TestHandleWatchRunnerEvent_HealthyRunResetsBackoff`, `TestRunWatch_RestartsFailedRunnerWhenBackoffTimerFires`, `TestRunnerRestartDelay_DoublesUpToTheCap`, `TestHandleWatchRunnerEvent_IgnoresEventFromAlreadyStoppedRunner`, `TestStartWatchRunner_CanceledFinalDrainSendsNoEvent`, `TestHandleWatchRunnerEvent_ParentExitStopsChildrenAndForgetsCachedSnapshot` |
| Every runner event carries its runner's generation, and an event from a runner that was stopped or replaced under the same mount ID changes nothing: a replaced drive's queued failure, a normal shortcut child's failure after a final drain took its place, a replaced parent's snapshot. A canceled runner never blocks on the event channel. A shortcut republished with only its run mode changed to a final drain restarts as a final drain (R-6.3.9). | `TestHandleWatchRunnerEvent_EventFromReplacedRunnerIsIgnored`, `TestHandleWatchRunnerEvent_EventFromReplacedFinalDrainChildIsIgnored`, `TestHandleWatchRunnerEvent_IgnoresStaleParentSnapshotEvents`, `TestStartWatchRunner_CancelledRunnerDoesNotBlockOnAFullEventChannel`, `TestApplyWatchMountSetForParent_NormalToFinalDrainRestartsChild`, `TestRunWatch_PublishesParentChildWorkSnapshotBeforeStartingChildren` |
| Every stop of a watch runner goes through `retireWatchRunner`, whose waits are bounded: 45 s for the runner to return and 15 s for its engine to close. A runner that misses either fails closed (it stays in the runner set, its engine stays open, nothing starts under its ID, one Error line with every goroutine's stack) and the watch ends with `ErrSyncStuck`, on a reload, a removed or paused drive, a retired shortcut, a drive's exit and shutdown alike; drives stopped together share one bound. The owner lock is held until every runner has stopped (R-6.3.9, R-2.9.4). | `TestRetireWatchRunner_TimeoutFailsClosed`, `TestRetireWatchRunner_EngineCloseTimeoutFailsClosed`, `TestWatchRunnerStopPaths_ARunnerThatDoesNotStopFailsClosed`, `TestRetireAllWatchRunners_StuckDrivesShareOneStopBound`, `TestRunWatch_StuckRunnerOnReloadEndsTheWatchWithAnError`, `TestRunWatch_StuckRunnerOnShutdownEndsTheWatchWithAnError`, `TestRunWatch_OwnerLockHeldUntilRunnersDrain`, `internal/cli/failure_class_test.go` (`TestCommandExitCode`) |
| A one-shot run closes its engines with the watch's bounded close on a detached context; an engine whose close runs over fails the run with `ErrSyncStuck` (exit 1) while the pass's reports stand, and a shortcut whose engine did not close is neither acknowledged nor cleaned up in that run (R-6.3.9). | `TestRunOnce_AnEngineThatDoesNotCloseFailsTheRun`, `TestRunOnce_AShortcutEngineThatDoesNotCloseIsNotAcknowledged`, `internal/cli/sync_console_test.go` (`TestRunOnceResultError_AnEngineThatDidNotCloseFailsAPassThatSynced`) |
| Every context the engine or the control plane detaches from cancellation carries a deadline, so the stop and close bounds rest on bounded waits (R-6.3.9). | `internal/sync/detached_context_test.go` (`TestDetachedContexts_AlwaysCarryADeadline`, `TestUnboundedDetachedContexts_FlagsOnlyTheUnboundedForm`) |
| A `WatchReloaded` event states its outcome (`ReloadApplied`, `ReloadKeptPreviousSettings`), and the CLI's service status follows the outcome, not `Err` (R-6.6.32). | `TestRunWatch_ReportsLifecycleTransitions`, `internal/cli/sync_notify_test.go` (`TestSyncNotifier_ReloadStatusFollowsTheStatedOutcome`) |
| A panic in one mount's runner goroutine (watch loop or final drain) is recovered, logged once at Error with its stack, and becomes that runner's error, so the mount is backed off and restarted while its siblings keep running; one-shot `MountRunner` panics take the same path, a shortcut's panic is that shortcut's report, and the pass still waits for every shortcut; a panic in the work around a drive's run (starting its shortcuts, building their engines) is joined into that drive's one report or is a report against it, and every lock a boundary reports through is released with defer, so the pass never hangs on it (R-6.3.8, R-6.3.10). | `TestRunWatch_RunnerPanicFailsOnlyThatMountAndRestartsIt`, `TestMountRunner_Run_PanicIsLoggedAtErrorWithStack`, `TestMountRunner_Run_Panic`, `TestMountRunner_Run_PanicWithError`, `TestRunOnce_AShortcutPanicIsThatShortcutsErrorAndThePassFinishes`, `TestRunIndexedMountWork_APanicWhileStartingShortcutsIsThatDrivesReport`, `TestRunIndexedMountWork_APanicBeforeTheRunFillsTheDrivesReport`, `TestRunOnce_APanicAroundADrivesShortcutsIsThatDrivesReport` |
| A panic outside a drive's runtime ends the watch with exit 1 after its drain: on the loop itself or during its startup (`RunWatch` registers the drain and the socket close before anything starts and recovers last), on the control server's goroutine or the config file watcher's (reported on the `fatal` channel), and in an engine's close (which fails closed like an overrun, in watch and one-shot mode). Each is logged once with its stack; a one-shot owner whose control server panicked finishes its pass, and a control handler's panic reaches the owner's logger (R-6.3.10). | `TestRunWatch_LoopPanicIsLoggedWithStackAndReturnsAnError`, `TestRunWatch_APanicDuringStartupStopsTheDrivesThatStarted`, `TestRunWatch_ControlServerPanicEndsTheWatch`, `TestRunOnce_ControlServerPanicIsLoggedAndThePassFinishes`, `TestWatchConfigReloadTriggers_PanicIsReportedOnTheFatalChannel`, `TestRetireWatchRunner_APanickingEngineCloseFailsClosed`, `TestRunOnce_APanickingEngineCloseFailsTheRun`, `TestControlServer_HandlerPanicIsLoggedThroughTheOwnersLogger` |
| A reload the watch owner cannot apply is answered with `reload_failed` instead of `reloaded`, and the running drives keep their previous settings. Another sync owning the socket is one process-level startup failure, not one per drive, and its message names the way out rather than the socket path. | `TestOrchestrator_Reload_InvalidConfig`, `TestRunOnce_ControlSocketInUseIsReportedOnceForAllDrives` |
| Control-socket reload applies add/remove/pause/expired-pause/filter diffs to the live runner set without bouncing unaffected mounts. | `TestOrchestrator_Reload_AddDrive`, `TestOrchestrator_Reload_RemoveMount`, `TestOrchestrator_Reload_PausedMount`, `TestOrchestrator_Reload_TimedPauseExpiry`, `TestOrchestrator_Reload_ContentFilterChangeRestartsOnlyAffectedMount` |
| A reload hands the config's `log_level` to `OrchestratorConfig.ApplyLogLevel`; the CLI's setter moves the log file handler's `slog.LevelVar` in place, so a level changed in `config.toml` reaches the file without a restart, and an unknown value keeps the level with one warning per value (R-2.8.1). A watch that stops with an error logs it at Error before the terminal line (R-6.6.35). | `TestOrchestrator_Reload_AppliesTheConfigLogLevel`, `TestApplyFileLogLevel_MovesTheFileLevelAndWarnsOnceForAnUnknownValue` |
| A timed pause ends on its own (R-2.8.12): the `RunWatch` loop arms `watchPauseExpiryWake` for the earliest `paused_until` among the configured drives (`nextPauseExpiryDelay`, from `config.Drive.PauseExpiry`, one second of slack for the second-precision timestamp) and, when it fires, runs the ordinary reload, which clears the expired pause and starts the drive. Before this only a `resume` or an unrelated reload noticed the expiry; a one-minute pause was still holding three minutes later on 2026-11-02. The reload's expiry clock is the orchestrator's `now`, so the test drives it. | `TestRunWatch_TimedPauseExpiresWithoutAReload`, `TestNextPauseExpiryDelay_OnlyATimedPauseArmsTheWake`, `TestDrivePauseExpiry_OnlyATimedPauseHasAnEnd` |
| Parent engines own shortcut-root state, alias mutation, protected-root derivation, and durable cleanup retry state before multisync sees child work. | `TestSyncStore_applyShortcutTopologyPersistsParentShortcutRoots`, `TestSyncStore_EmptyCompleteShortcutTopologyMarksRemovedFinalDrain`, `TestSyncStore_markShortcutChildFinalDrainReleasePendingIsDurable`, `TestSyncStore_SamePathUpsertDoesNotDowngradeActiveProtectedOwner`, `TestSyncStore_DuplicateAutomaticShortcutTargetIsParentBlocked`, `TestEngine_AcknowledgeChildFinalDrainReleasesParentShortcutRoot`, `TestEngine_ReconcileShortcutRootLocalStateRetriesRemovedReleasePending`, `TestEngine_ReconcileShortcutRootLocalStatePersistsCleanupBlockedWithoutError`, `TestEngine_ShortcutAliasRenameMutatesThroughParentAndUpdatesRootState`, `TestEngine_ShortcutAliasDeleteMarksParentRootFinalDrain` |
| Multisync owns runtime-only shortcut child admission from exact parent snapshots: one-shot stores the exact publication and starts that parent's children only after that parent's safe point, while watch initial startup starts parent runners only and admits children from live parent publications that still match the live runner/cache state. | `TestReceiveParentChildWorkSnapshot_StoresSnapshotInMemory`, `TestReceiveParentChildWorkSnapshot_EmptySnapshotClearsCachedChildren`, `TestRunOnce_PublishesParentChildWorkSnapshotBeforeStartingChildren`, `TestRunOnce_StartsParentChildrenAfterPublishingParentSafePoint`, `TestRunOnce_StartsParentChildrenWithoutWaitingForOtherParents`, `TestRunOnce_UsesFinalParentSnapshotInsteadOfIntermediateSkip`, `TestRunWatch_PublishesParentChildWorkSnapshotBeforeStartingChildren`, `TestRunWatch_ReconcilesChildRunnersFromLiveParentSnapshot`, `TestApplyWatchMountSet_ParentRestartClearsSnapshotAndDoesNotRestartChild`, `TestHandleWatchRunnerEvent_ParentExitStopsChildrenAndForgetsCachedSnapshot`, `TestHandleWatchRunnerEvent_IgnoresStaleParentSnapshotEvents` |
| Multisync executes parent-declared final-drain and artifact-cleanup work without becoming the parent lifecycle owner; cleanup paths use explicit orchestrator `DataDir`, parent-scoped cleanup diagnostics remain transient, and runtime work compilation keeps shortcut child commands scoped to the declaring parent. Final-drain one-shot option rewriting may force bidirectional full reconcile, but it must preserve caller options such as dry-run. | `TestRunOnce_FinalDrainChildRunsBidirectionalFullReconcileAndReleasesAfterSuccess`, `TestBuildEngineWork_FinalDrainChildPreservesDryRunOption`, `TestRunOnce_FinalDrainChildFailureKeepsProjectionReserved`, `TestStartWatchRunner_FinalDrainRunsOnceBidirectionalFullReconcile`, `TestHandleFinalDrainWatchRunnerEvent_DoesNotAckParentWhenDrainErrs`, `TestRunOnce_ParentCleanupRequestPurgesShortcutChildStateArtifacts`, `TestOrchestratorCleanupWithEmptyDataDirFailsLoudly`, `TestOrchestratorPurgeShortcutChildArtifactsClearsDiagnosticsWhenNoCleanupWorkRemains`, `TestBuildRuntimeWorkFromParentChildWorkSnapshot_DoesNotClassifyDuplicateAutomaticChildren`, `TestBuildRuntimeWorkFromParentChildWorkSnapshot_StandaloneContentRootRunsBesideChild`, `TestBuildRuntimeWork_ParentBlockedSnapshotHasNoChildWork`, `TestClassifyShortcutChildDrainResultsOnlyCleanIsAckable`, `TestBuildChildStatusDrive_RendersLifecycleState` |

## Runtime Mount Specs

The control plane compiles runtime work from two inputs:
configured standalone parent mounts and parent-declared child work snapshots. The CLI
resolves configured drives and compiles them into
`multisync.StandaloneMountSelection`; valid selections become
`StandaloneMountConfig` values, while per-drive conversion failures become
`MountStartupResult` values before the orchestrator is constructed.

One-shot and watch startup bind the owner control socket before parent engines
start. Each run owns its parent child-work snapshot cache; those snapshots are
discarded with the one-shot/watch runtime instead of living on the
`Orchestrator`. On startup, reload, and parent snapshot events the control
plane:

1. consumes the CLI-compiled standalone mount selection
2. starts selected standalone parent engines with the child work snapshot sink
   attached before the engine begins its normal one-shot or watch bootstrap
3. caches each exact parent-declared child work snapshot in runtime memory when
   the live parent publishes from its normal current-plan path
4. reconciles child runners for that parent directly from normal and
   final-drain child work commands plus explicit cleanup commands

There is no separate shortcut-only startup path and no control-plane startup
publisher. The parent engine derives local protection and child publications
from the same current truth and current plan that it uses for ordinary work.
Watch initial startup starts selected standalone parent runners only. Children
are never admitted from a cached child work snapshot when a watch runtime
starts, or when a parent exits or fails before publishing; multisync stops any
existing children for that parent, forgets that runtime's snapshot for the
parent, and waits for a fresh live parent snapshot before admitting
replacements. Watch snapshot events are buffered process messages, so the watch
loop admits a parent-scoped event only when a live standalone parent runner
still exists and the event snapshot exactly matches the run-owned cache; stale
queued events from an exited/restarted parent are ignored. In one-shot,
multisync records the latest exact snapshot
published by each parent and starts that parent's child work only after that
same parent reaches its one-shot safe point. Parent A's children do not wait
for unrelated parent B, and child engines still run ordinary `RunOnce` passes.
Because one-shot child work begins after the parent safe point, final-drain and
artifact-cleanup acknowledgements can proceed through that same live parent
without a separate parent-done gate.

Managed child mounts are runtime projections declared by the parent engine, not
synthetic configured drives and not durable control-plane inventory.

Runtime mount inputs are discriminated parent/child values. Parent runtime
values carry configured namespace identity, token-owner/session facts, sync
tunables, and the child work sink. Child runtime values carry only the
parent-declared child engine input, child mount ID, final-drain mode, and
opaque acknowledgement reference. Parent values cannot carry child ack refs,
child run mode, expected shortcut identity, or child engine specs. Child values
cannot masquerade as configured standalone drives. The common runner boundary
receives the lowered runtime value only after the kind-specific shape has been
validated.

Watch lifecycle is parent-first and child-safe. Reload stops child runners
before parents and starts parents before children admitted from the run-owned
snapshot cache. If a parent runner exits,
panics, loses its root, is paused/removed by reload, or fails startup,
multisync immediately cancels all child runners whose `parentMountID` matches
that parent and forgets the runtime child work snapshot for that parent.
Replacement children can start only after the live parent has published fresh
child work commands through the normal engine path.

Runtime mount values do not carry parent-owned protected child paths or
protected roots; parent engines rebuild those from their own `shortcut_roots`.
`mountSpec` no longer carries `ResolvedDrive`, and `OrchestratorConfig` no
longer accepts resolved-drive values. Configured drives are compiled at the CLI
edge into `StandaloneMountConfig`, and sync session construction consumes those
facts through `driveops.MountSessionConfig`. `internal/multisync` derives
`sync.EngineMountConfig` from the runtime mount value. For shortcut children it
asks sync-owned child command helpers to validate and lower the parent command
into engine config, rather than inspecting shortcut lifecycle, protected-path,
or parent status facts.

`OrchestratorConfig.DataDir` is also explicit. The CLI/config composition
boundary may call `config.DefaultDataDir()`, but `internal/multisync` never
falls back to that ambient path. Child artifact cleanup fails as setup error
when the data directory dependency is missing, which keeps cleanup scope tied
to the orchestrator invocation and makes tests inject the directory they use.

Runtime reporting is mount-identified. Standalone mounts keep their configured
canonical drive ID inside `MountIdentity`, but managed child mounts report by
stable `MountID` and do not synthesize `shared:` canonical drive IDs. The
control socket keeps this mount-shaped internal contract. CLI `status`
translates it at the presentation boundary into account -> drive -> shared
folder output: public JSON nests child projections under `shared_folders` and
does not expose `mount_id`, `canonical_id`, or namespace IDs.

Managed child token owner and sync tunables are inherited from the selected
standalone namespace mount. There is no child pause, resume, reset, config, or
CLI control surface. A child projection is controlled by the OneDrive shortcut
itself and by the parent drive's pause state. Explicit standalone shared-folder
mounts may project the same remote content root as automatic shortcut children
when their configured local `sync_dir`s do not overlap. Duplicate automatic
shortcuts inside one parent remain parent-engine shortcut-root state.

Shortcut lifecycle state is producer-owned:

| Situation | Durable state | Runner behavior | Parent protection |
| --- | --- | --- | --- |
| Shortcut binding is healthy | parent root `active` | child runner may start unless parent is paused | current child path |
| Parent drive is paused | parent root `active` | child runner is paused with the parent | current child path |
| Binding target cannot be refreshed | parent root `target_unavailable` | no child work command; retry on parent refresh | current child path |
| Shortcut was authoritatively removed | parent root `removed_final_drain` | child runs a final bidirectional full sync, then stops after parent release | current and reserved paths until finalized |
| Clean final drain acknowledged, parent release not yet complete | parent root `removed_release_pending` | child is already drained; release cleanup retries in parent | current and reserved paths until finalized |
| Removed shortcut final drain cannot reach the shared-folder target | parent root `removed_final_drain` plus child retry/block state | child final-drain runner retries normally; status guides target-access recovery | current and reserved paths until finalized |
| Removed shortcut cleanup cannot release the alias root | parent root `removed_cleanup_blocked` | child is already drained; release cleanup retries in parent | current and reserved paths until finalized |
| Parent released the alias root and child artifacts still need purge | parent root `removed_child_cleanup_pending` | no child runner; multisync purges child-owned artifacts and acknowledges completion | none |
| Same-path replacement arrives before old child finalizes | parent root with `Waiting` replacement | old child drains first, new child starts after parent promotion | shared path remains reserved |
| Duplicate automatic shortcut to same target in one parent | parent root `duplicate_target` | duplicate root does not publish a child work command | conflicting child paths |
| Parent cannot safely move or inspect a renamed shortcut alias root | parent root `blocked_path` or `removed_cleanup_blocked` | no child work command while parent retries or waits for user action | current and reserved paths |
| Child local root is a file, final symlink, or unsafe path | parent root `blocked_path` | no child work command | child path |
| Child local root is unavailable while the parent still owns the alias lifecycle | parent root `local_root_unavailable` | no child work command until parent retry or user recovery | child path |
| Previously materialized child local root is renamed to one same-parent identity match | parent root remains protected while parent alias rename is applied | same child state DB restarts at new path | current path plus same-parent identity protected-root |
| Previously materialized child local root is missing with no same-parent identity match | parent deletes only the shortcut alias, then emits a final-drain child work command | child final-drains if runnable, then releases | child path until finalized |
| Previously materialized child local root has multiple same-parent identity matches | parent root `rename_ambiguous` | no child work command until user resolves ambiguity | current and candidate paths |
| Local alias rename/delete cannot mutate the placeholder | parent root `alias_mutation_blocked` | no child work command while parent retries | current and candidate paths |

Shortcut child work publication is split by authority. The parent engine is the
only runtime that calls Graph delta/list/get for the parent drive. It
classifies shortcut placeholder observations, persists parent-owned
`shortcut_roots`, suppresses those aliases from normal content planning,
mutates shortcut placeholders by `binding_item_id`, and publishes child work
commands to `internal/multisync`:

- normal child work commands start or continue child runners
- final-drain child work commands tell multisync to run the child once in
  bidirectional full-reconcile mode
- cleanup requests tell multisync to purge child-owned artifacts after child
  sync is clean and the parent has released its protected alias path; each
  request carries an opaque acknowledgement reference, child mount ID, local
  root, and cleanup reason, and multisync rejects incomplete requests instead
  of deriving scope from parent namespace data
- work snapshots contain only execution data and cleanup commands, so
  cleanup is an explicit parent-published executor request rather than inferred
  absence from a later snapshot
- complete batches mark previously known but absent parent roots
  `removed_final_drain`
- empty complete batches are applied through the same engine handler path
  because they mean the parent engine completed a parent-drive enumeration and
  saw no current shortcut aliases; empty incremental batches are skipped
- same-path replacement stays in parent `shortcut_roots` as a waiting
  replacement until the old child final drain is acknowledged
- all active, conflicted, unavailable, waiting, release-pending, and
  removed-final-drain paths
  remain parent protected paths
- runnable child work snapshots carry the parent-observed local root identity when
  available; child engines verify that identity before startup and full scans
  so a deleted/recreated or moved projection cannot be mistaken for an empty
  tree to sync
- in one-shot mode, each configured parent runs as an ordinary engine and
  children are admitted from that parent's latest exact child-work snapshot only
  after that parent reaches its safe point. No parent waits for unrelated
  parents before its children can run. The one-shot child-run coordinator only
  owns report aggregation, artifact cleanup, and live-parent acknowledgement
  ordering; children still execute ordinary engine `RunOnce` passes
- successful child work mutation in watch mode publishes a new
  parent-owned child snapshot through the live parent runner; multisync
  reconciles only that parent's child runners without stopping the parent

If the parent sync store cannot durably accept a topology batch, the parent
engine does not commit its remote observation cursor. The same Graph facts
replay later from the parent engine, preserving the one-observer ownership
boundary without letting the control plane rediscover remote state.

Parent shortcut lifecycle transition decisions are centralized in the sync
engine planner. The parent shell consumes remote shortcut observations,
protected-root local observations, stored `shortcut_roots`, and child-drain
acknowledgements, executes Graph/filesystem/store effects at its own I/O
boundary, and feeds those value outcomes through planner helpers. Multisync
consumes only the resulting child work snapshot: start normal child
commands, run final-drain commands to final drain, purge explicit cleanup
commands, acknowledge clean drain through the live parent
`shortcutChildAckHandle`, then stop and forget child runtime state after the
parent release succeeds.

Runtime mount-set construction does not inspect or mutate parent shortcut alias
roots. Parent engines create, reserve, move, block, or release alias
projections inside their sync root. Child engine construction may still fail
with `ErrMountRootUnavailable` when the parent has not made a runnable child
root available; that is reported as mount startup state rather than converted
into parent alias policy by multisync.

Authoritative removal is a two-owner lifecycle transition. The parent marks the
shortcut root `removed_final_drain` and keeps its protected path active. The
control plane runs the child as an ordinary bidirectional full reconcile so
local content changes in the projection can reach the shared-folder target. If
the child reports retry/block/content failures, root unavailability, target
access loss, or no mount report, the typed final-drain result is not
acknowledged and the parent protection remains. The child state DB remains the
owner of dirty content retry/block state. Only a `clean` final-drain result
produces an opaque acknowledgement to the live parent engine. The parent
first persists `removed_release_pending`, then idempotently removes/releases
the alias projection or promotes a waiting replacement. If release cleanup is
blocked, the parent persists `removed_cleanup_blocked` and retries from that
durable truth on startup or later parent refresh. When release cleanup
succeeds, the old binding stays in the parent store as
`removed_child_cleanup_pending`: the alias path is no longer protected and no
child runner is published, but the parent explicitly publishes a child artifact
cleanup request. Multisync purges the child-owned DB, SQLite sidecars and
upload sessions, then acknowledges artifact cleanup to the same
live parent engine. The parent deletes the cleanup-pending row only after that
acknowledgement, so cleanup retry is durable and parent-owned. Cleanup
diagnostics on the control socket are transient executor status: a failure
records the source and domain class, and a later successful retry clears that
source's old diagnostic. An empty cleanup work set only clears old published
cleanup diagnostics when it covers every configured parent; parent-scoped watch
reconciliation must not clear another parent's active cleanup failure.

Local alias deletion is explicit and local-only. If the user deletes the local
shortcut alias while the parent root is active, the parent deletes only the
shortcut placeholder and emits a final-drain child work command. This does not delete
the shared-folder target and does not mutate configured standalone shared-drive
catalog records.

Local shortcut alias deletion and same-parent local shortcut alias rename are
not target-content delete or target-content rename operations. Parent engines
own the local observation and shortcut placeholder mutation by
`binding_item_id`. If the alias directory later appears at exactly one sibling
path with the same stored identity, the parent alias rename is applied and the
same child mount ID restarts at the new path. If the alias disappears with no
same-parent identity candidate, the parent deletes only the shortcut alias and
emits a final-drain child work command. During all blocked or retiring states, parent
observation suppresses protected roots so they are not uploaded into the parent
drive. Cross-parent behavior is unsupported by design: parent engines are
isolated by account/namespace authority and never compare shortcut roots across
parents.

Status is the guided recovery surface for these states. Non-active shortcut
children expose the protected current path, reserved previous/candidate paths,
state/reason/detail, concise `recovery_action`, `auto_retry`, and waiting
replacement state when present from the parent shortcut-root status snapshot,
plus sync-owned typed issue/recovery classes for JSON consumers. None of this
status-only metadata comes from multisync child work snapshots.

## Boundary To The Engine

The control plane does not observe, plan, execute, or persist sync state
itself. Those responsibilities remain in the single-mount engine.

- `internal/multisync` owns runtime mount selection, session resolution,
  derivation of sync-owned engine mount config, per-mount goroutines, reload,
  and shutdown.
- `internal/sync` owns one-shot execution, watch-mode runtime state, conflict
  execution, retry/trial logic, scope lifecycle, and reconciliation.

This split keeps the engine package focused on one mounted content root at a
time while allowing the CLI to run any number of mounts through one consistent control
surface.

Parent engines rebuild protected child-root paths from parent-owned
`shortcut_roots`: reserved relative paths, binding IDs, target identity, and
optional filesystem identity. Multisync receives only the parent-declared child
child work snapshots derived from those rows. Parent namespace decisions
and retry/block state remain in the parent engine.

## `Orchestrator`

`Orchestrator` is the multi-mount coordinator used by both one-shot `sync` and
watch-mode `sync --watch`.

It is always used, even for a single configured drive. There is no separate
single-mount CLI path, because special-casing `n=1` would create a second
lifecycle model for startup, shutdown, and reload.

### RunOnce

`RunOnce` compiles runtime mount specs, resolves sessions, builds one engine per
mount, and runs all mounts concurrently. Startup eligibility is classified per
mount first, including CLI conversion failures, paused standalone parents, and
managed child mounts skipped because their parent is missing or their content
root conflicts. Paused standalone parents are preserved as paused mount inputs
without sync-dir structural validation, so stale dormant config cannot turn a
paused drive into a fatal startup failure for unrelated runnable mounts. When a
drive is unpaused, normal conversion and engine startup validation apply again.
Runnable mounts then produce one completed `MountReport` each,
while startup-ineligible mounts remain startup outcomes instead of synthetic
completed reports. The control plane never aborts the whole pass because one
mount failed; partial failure is isolated per mount. Both startup results and
completed reports carry a stable `SelectionIndex` matching the compiled runtime
order plus a `MountIdentity` matching the current boundary, so standalone
parents and their attached child mounts remain deterministic through
orchestration, rendering, and bookkeeping.

### RunWatch

`RunWatch` starts one watch-mode engine per runnable non-paused mount and then
owns the long-running control loop. A selection in which every mount is
paused starts the loop with no engine (`validateInitialWatchStart`): the
watch is ready, `WatchLifecycleEvent.Paused` carries the paused drives for
the service status, and the ordinary reload and reconcile passes start a
drive once it is resumed or its timed pause expires (R-3.4.2). Exiting
instead made a `Restart=on-failure` unit restart the service every interval
until the resume. It listens for:

- `ctx.Done()` for shutdown
- JSON-over-HTTP requests on the Unix control socket
- its `fatal` channel (buffered 1), on which a helper the watch cannot run
  without reports its end: the control server when `Serve` returns on its own
  or panics (see Control Socket), and the config file watcher when it panics,
  since a watch that no longer notices a config edit or a SIGHUP must not
  carry on as if it did. `RunWatch` returns the error, so the watch exits 1
  through the same drain as any other return

The loop also ends when a runner it had to stop did not stop within its
bound (see Stopping a runner): the reload, reconcile or event handler that
met it returns `ErrSyncStuck`, and so does `RunWatch`.

`RunWatch` is the panic boundary of the loop itself (R-6.3.10). Its first
deferred call recovers, so it runs after the drain and the socket close: a
panic on the loop stops the drives, closes the socket, is logged once with its
stack through `panicguard.Recovered`, and is returned (wrapping
`panicguard.ErrPanic`, joined with any drive that did not stop). The watch is
not kept alive: the loop's maps and report memory are of unknown consistency,
and exit 1 lets the service manager start a fresh process. The drain and the
socket close are registered before anything starts (`startWatchRuntime` sets
the control server once bound and adds each runner to the loop's map as it
starts it), so a panic at any point, during startup or in the
`WatchLifecycle` hook that runs on the loop, stops the drives that started
and then closes the socket
(`TestRunWatch_LoopPanicIsLoggedWithStackAndReturnsAnError`,
`TestRunWatch_APanicDuringStartupStopsTheDrivesThatStarted`). Without the
boundary a loop panic would end the process with Go's exit status 2 (the
usage-error status), nothing in the log file and no failure status for
systemd.

Pause semantics come from `config.Drive.IsPaused()` and
`config.ClearExpiredPauses()` for configured parents. Managed child mounts
inherit the parent drive state and have no independent pause state. The control
plane consumes those rules; it does not redefine them inside the engine.

Existing state DBs that fail store compatibility checks are reported as
per-mount startup outcomes. CLI conversion failures use the same startup-result
path, including on reload, so one bad selected mount does not prevent healthy
mounts from running. Watch startup warns about those mounts immediately, keeps
healthy mounts running, and exits non-zero only when no runnable mount starts.
An all-failed selection is a structured startup refusal
(`WatchStartupError`), not a special string-only path; a paused-only
selection is not a refusal at all.

Every reconcile tick, reload, and runner event re-runs the start logic, and
the carried-over CLI conversion failures ride along on each pass. The watch
loop therefore remembers what it has announced (`watchReportState`, owned by
the RunWatch goroutine alone): a start failure reaches `StartWarning` the
first time and again only when its status or error text changes, or after the
drive started or was paused in between. A full pass forgets announced
failures that no longer appear, except for drives waiting out a restart
backoff, which make no attempt and so produce no result. The same memory keeps
a persistently failing shortcut refresh or removed-shortcut cleanup at one
WARN per distinct error; the cleanup failure also stays visible in `status`
through the shortcut cleanup diagnostics.

A runner that exits with an error is not restarted in the same pass. Its
restart waits 5 s, doubling per consecutive failure up to 5 min; a runner that
stayed up for 5 min before failing starts the sequence over. The RunWatch loop
arms one timer for the earliest pending restart and reconciles when it fires,
so a backed-off drive does not wait for the next reconcile tick. The exit is
logged once per distinct error as "Stopped syncing <drive>; retrying in <delay>"
(a lost sync folder is INFO, because the restart's start failure is what gets
announced). A final drain the orchestrator canceled sends no event, so
reloads and parent changes never surface "context canceled".

Every runner gets a generation from a counter the RunWatch goroutine owns
(`startNextWatchRunner`), and every event it sends carries it, including the
parent child-work snapshots its parent sink publishes (the sink is attached
to the mount spec with the generation before the runner starts). An event
whose generation is not that of the runner now under its mount ID came from
a runner the orchestrator already stopped or replaced, and is ignored: before
the generation, a reload that replaced a failed runner under the same ID
applied the old runner's queued failure to the new one and waited for the new
one to exit, and a normal shortcut child's failure was taken for the result
of the final drain that replaced it. A parent snapshot event must also still
match the cached snapshot. A runner's sends give up once the runner is
canceled, so a canceled runner never holds its `done` open on a full event
channel while the loop waits for it.

#### Stopping a runner

`retireWatchRunner` is the only code that stops a watch runner, whatever
asked for it: a reload, a removed or paused drive, a shortcut retired or
replaced by its parent's snapshot, a parent's exit (its children), a runner
that exited on its own, and the end of the watch (`retireAllWatchRunners`).
It cancels the runner, waits for its goroutine, closes its engine on a
context detached from the watch's cancellation, and removes it, and both
waits are bounded:

| Bound | Value | Why |
| --- | --- | --- |
| Runner returns after its cancel | 45 s (`watchRunnerStopTimeout` = `syncengine.OutcomeCommitTimeout` + 15 s) | what a canceled engine still waits for is its detached store writes, each bounded by `OutcomeCommitTimeout` (30 s) |
| Engine closes | 15 s (`engineCloseTimeout` = `syncengine.StoreCloseCheckpointTimeout` + 5 s) | the store close's checkpoint (bounded at 10 s inside `SyncStore.Close`), the stale upload-session sweep and the database close; `Engine.Close` does not take its context into the sweep, so the bound is on the wait |

The arithmetic. A canceled engine's remaining waits are the detached store
writes of its drain: the commit of work that finished as the cancel landed,
the record of a created item, clearing a succeeded action's retry work, the
findings observed before the shutdown (sync-engine.md, Detached store
writes). Each runs on `context.WithTimeout(context.WithoutCancel(ctx),
OutcomeCommitTimeout)`; `TestDetachedContexts_AlwaysCarryADeadline` keeps
any detached context in `internal/sync` and `internal/multisync` from going
without a deadline. A healthy write takes milliseconds and waits at most
SQLite's 5 s busy timeout for a lock, so a healthy stop takes well under a
second. The stop bound covers one write running to its 30 s limit plus 15 s
for everything else; a store slow enough to need two is wedged, and ending
the watch is then the intended outcome, not a false alarm. The close bound
holds the checkpoint's own 10 s bound plus 5 s for the sweep and the close.

A one-shot run closes its engines with the same bounded close
(`closeEngineWithin`, shared with the watch): a close that runs over is
logged once with every goroutine's stack, the run's `RunOnceResult.Stuck`
wraps `ErrSyncStuck`, the pass's reports stand, and the command exits 1. A
shortcut whose engine did not close gets neither its final-drain
acknowledgement nor its artifact cleanup in that run, since both act on a
store its engine may still be writing; the next run retries them.

The stop bound runs from the runner's first cancel, so the drain at the end
of the watch, which cancels every runner before it waits for any, waits for
all of them at most once. A runner that misses a bound fails closed: it is
marked stuck and stays in `runners`, so nothing is started under its mount
ID and its engine, which may still be writing its store under the sync-root
lock, is never closed under it; every goroutine's stack is logged once at
Error; and the error, wrapping `ErrSyncStuck`, ends the watch with
exit status 1, also on a user's stop (R-6.3.9, R-6.6.27). Exiting is the
only way to end a goroutine that ignores its cancellation, and it releases
every lock the engine holds; where the filesystem cannot lock the sync root
(R-2.9.5), the orchestrator's refusal to start a replacement is the only
thing keeping the store single-owner. The stuck goroutine, and a close that
ran over, are left to that exit.

The runner goroutine is a panic boundary. `runWatchRunnerEngine` recovers a
panic from the engine's `RunWatch` (or a final-drain child's `RunOnce`) and
converts it into that runner's error through `recoveredSyncPanic`, so it
travels the same runner-event path as any other exit: the mount is reported
stopped, backed off and restarted, and its siblings never notice. Without the
recover a bug reachable from one account's data unwound the whole process
past every other mount and the drain this design promises. The goroutine's
own hand-off after the engine returns has a boundary of its own, which sends
the same runner event, so nothing on the runner goroutine can take the
process down (R-6.3.8).

An engine close that panics fails closed like a close past its bound
(`closeEngineWithin` returns the panic, logged once with its stack): the
store and its locks are in an unknown state, so the runner stays stuck, its
store is never reopened, acknowledged or cleaned up, and the watch ends with
the panic; a one-shot pass reports it in `RunOnceResult.Stuck` and the
command exits 1 (R-6.3.10,
`TestRetireWatchRunner_APanickingEngineCloseFailsClosed`,
`TestRunOnce_APanickingEngineCloseFailsTheRun`).

### Control Socket

`RunOnce` and `RunWatch` both bind the configured Unix control socket before
starting engine work. Ownership is an exclusive, non-blocking `flock` on
`<socket>.lock` (`owner_lock.go`), taken before anything touches the socket
file and held for the owner's lifetime. It replaced "a live socket means an
owner": two syncs starting together both failed to dial a stale socket, both
removed it, and one deleted the other's live socket, so two owners synced the
same drives (D-083). With the lock held, a socket file already at the path is
a crash leftover and is replaced; the old dial check stays only as a guard.
On close the socket is removed first, then the lock file (while still
locked), then the lock. A contender that opened the lock file just before
that unlink locks an orphan inode, so after locking the descriptor's inode is
compared with the path's and a mismatch retries
(`TestStartControlSocketServer_RacingOwnersYieldExactlyOne`,
`TestStartControlSocketServer_LockReleasedOnCloseAndOnlyThen`,
`TestLockFileStillAtPath_DetectsAnUnlinkedOrReplacedLock`).

Only the owner closes the server, after its drives have stopped: `RunOnce`
and `RunWatch` defer the close so it runs after their runners are done, and
nothing closes it when the context is canceled. A goroutine that did, at the
first signal, released the lock while the drives were still draining into
their stores, so `drive reset-sync-state` or a second sync could start
against a store still being written (`TestRunWatch_OwnerLockHeldUntilRunnersDrain`).
During the drain the socket stays up; a command the loop no longer takes is
answered `busy`.

That lock covers one control socket. Each engine also holds two exclusive
`flock`s on data-dir lock files (`internal/sync/engine_locks.go`), one named
after its sync root's device and inode and one after its state DB's path, so
a second configuration sharing the data dir is refused with
`ErrSyncRootInUse` when it reaches the same folder by any path, and with
`ErrSyncStoreInUse` when it names the same store from another folder
(R-2.9.5, `TestEngine_SecondEngineOnSameRootRefused`,
`TestEngine_SecondEngineOnSameStoreRefused`). A shortcut child's cleanup
deletes the child's state DB family only under that store lock: while another
engine has the store open, the cleanup fails as `state_artifact`, touches
nothing else of the child, and is retried from the parent's next cleanup
command (`TestShortcutChildArtifactCleanupExecutor_KeepsTheStateWhileAnEngineHoldsIt`).

The configured socket path normally lives under the app data directory. If that
absolute path would exceed the platform-safe Unix socket length, config derives
a stable hash-named runtime directory under the OS temp directory and stores
only the socket there; durable sync state remains in the drive state DB. If the
normal path and the hashed runtime fallback both exceed the Unix socket budget,
path derivation fails explicitly. `RunOnce` and `RunWatch` treat that as fatal
startup because the control socket is the single-owner lock.
If watch startup fails after the socket is bound, the control plane closes and
unlinks that socket before returning the startup error.

Wire facts live in `internal/synccontrol`: endpoint constants, owner modes,
request/response structs, response statuses, and stable error codes. Server
lifecycle stays in `internal/multisync`; CLI transport stays in `internal/cli`.

The socket speaks JSON over HTTP:

- `GET /v1/status` returns the owner mode (`oneshot` or `watch`) and managed mounts. CLI `status` treats those mount IDs as a transient runtime overlay: matching rows are active, displayed configured rows that are absent from the owner response are inactive, and the durable config/store read model remains unchanged.
- `GET /v1/perf` returns the owner mode plus the live aggregate and per-mount perf snapshots currently owned by the active sync runtime. This includes path-free stale-work, local-observation, and replan-idle aggregates. The surface is live-only and returns whatever the owner has collected so far; it does not materialize historical perf state from SQLite.
- `POST /v1/perf/capture` triggers an explicit local capture bundle from the active owner. The request carries a duration of at most 5 minutes (`maxPerfCaptureDuration`; longer is `invalid_request`, compared in milliseconds so a huge value cannot overflow into a short one) plus optional output-dir, trace, and full-detail toggles; the response returns the local artifact paths for the completed bundle.
- `POST /v1/reload` reloads config in the watch owner.
- `POST /v1/stop` asks the watch owner to stop cleanly.

One-shot sync exposes status plus the direct perf endpoints above. Durable
control requests still return a busy response with
`code="foreground_sync_running"` because a foreground one-shot sync is already
the active owner. The CLI probes the owner boundary to decide whether live
control requests can be sent at all, but there is no parallel direct-DB
mutation path for sync decisions anymore.

Error responses have the shape `{status, code, message}`. Stable codes are
`invalid_request`, `foreground_sync_running`, `capture_unavailable`,
`capture_in_progress`, `reload_failed`, `busy`, and `internal_error`. One
answer is not an error: `{"status": "reload_in_progress"}` with HTTP 202.

#### Deadlines

Every wait on the socket is bounded (R-2.9.7). A stuck client could hold a
handler and a connection for as long as it liked, and a handler waited for
the watch loop with no deadline of its own.

| Bound | Value | Where |
| --- | --- | --- |
| Request headers | 5 s | `http.Server.ReadHeaderTimeout` (`controlHeaderTimeout`) |
| Whole request, body included | 10 s | `ReadTimeout` (`controlReadTimeout`) |
| Idle keep-alive connection | 30 s | `IdleTimeout` (`controlIdleTimeout`) |
| Header block | 16 KiB | `MaxHeaderBytes` (`maxControlHeaderBytes`) |
| Watch loop taking a command | 5 s | `synccontrol.LoopAcceptTimeout`; past it HTTP 503 `busy`, nothing changed |
| Answer to a reload the loop took | 60 s | `synccontrol.ReloadAnswerTimeout`; past it HTTP 202 `reload_in_progress` |
| Answer to status or stop the loop took | 5 s | the loop answers these the moment it takes them; past it `busy` |
| Writing an answer | 5 s | `WriteTimeout` = `synccontrol.ReplyWriteTimeout` |
| Perf capture | 5 min | `maxPerfCaptureDuration` |

The two waits for the loop have different answers on purpose. A command the
loop never took changed nothing, so `busy` is exact. A reload the loop took
runs to the end whether or not anyone is still waiting (a failure is logged
by the reload itself), so the answer past its deadline is "in progress",
never a silence the client would read as a failure. The handler's response
channel is buffered, so a loop that answers after the handler left never
blocks. `ReloadAnswerTimeout` only shapes the answer; the reload itself is
not bounded here.

The server-wide `WriteTimeout` is the budget of an answer that is ready at
once (perf status, one-shot status, a refusal). A handler that waits moves
its own write deadline through `http.ResponseController`
(`extendControlWriteDeadline`): accept + answer wait + write for loop
commands, the capture's duration + write for a capture, set only once the
capture request is decoded. A server-wide budget long enough for a
five-minute capture would bound nothing else. The read deadline needs no
move: `ReadTimeout` bounds reading the request, and net/http clears the read
deadline when the handler starts; a handler that set one shorter than its
wait would have its request context canceled under it.

The wire deadlines live in `internal/synccontrol` because the client derives
its own timeouts from them: a client that gave up first would never hear
`busy` or `reload_in_progress`. Tests shorten the server's through the
orchestrator's `controlTimeouts`.

#### A control server that stops ends the watch

`Serve` returning without `Close` (the listener failed under it) would leave
a watch running with no way to pause, reload, query or stop it over the
socket. So `reportControlServeEnd` sends the error on the watch's `fatal`
channel and `RunWatch` returns it: the process exits 1 and a service manager
restarts a watch it can reach again. The send is
non-blocking; if another fatal error is already pending, this one is logged
at Error. A one-shot owner has no fatal channel: it keeps its owner lock (the
`flock`, not the socket), finishes its pass, and only logs the warning. A
panic on the serving goroutine takes the same path, logged once with its
stack first (R-6.3.10, `TestRunWatch_ControlServerPanicEndsTheWatch`,
`TestRunOnce_ControlServerPanicIsLoggedAndThePassFinishes`).

A handler's panic is not the serving goroutine's: `net/http` recovers it per
connection and reports it, with its stack, through `http.Server.ErrorLog`.
`newControlHTTPServer` points that at the owner's logger
(`slog.NewLogLogger`, at Error), so the record reaches the log file instead
of the standard `log` package's stderr
(`TestControlServer_HandlerPanicIsLoggedThroughTheOwnersLogger`).

A capture request's `output_dir` must be absolute: the owner runs in its own
working directory, so it refuses a relative path instead of resolving it
there. `synccontrol.PerfCaptureRequest` resolves a relative path against the
sending process's working directory when it is encoded, which is where the
user typed it.

When the socket is already owned, `ControlSocketInUseError` keeps the path on
the struct for diagnostics but its message only says another sync is running
and how to proceed. One-shot reports it as a single process-level startup
result rather than one copy per selected drive.

### Reload

A reload runs on three triggers: a control-socket request (sent by `pause`,
`resume`, `drive add` and `login` after they write the config), a change to
the config file on disk (`watch_config_file.go` compares size, mtime and
inode every 5 s, and reloads a new version once two consecutive checks agree,
so a poll between an in-place save's truncate and write is not reloaded), and
SIGHUP (the systemd unit's `ExecReload`). File and
signal triggers coalesce into one pending reload; a failed one is only
logged, since no client is waiting for an answer. Before the file trigger,
hand edits to `config.toml` were silently ignored until the watch restarted
(R-4.4.3).

Every reload does four things in order:

1. load config from disk
2. clear expired timed pauses
3. compile the new active runtime mount set
4. diff that set against running mounts

If the config cannot be loaded or compiled, nothing is applied: the running
drives keep their previous settings and the reload request is answered with
HTTP 422 and `code="reload_failed"`, so `pause`, `resume`, `drive add` and
`login` do not report that the running sync picked up their change. Drives
that fail to start under a config that did load are startup results, not a
reload failure. A successful reload releases pending restart backoff waits,
because the user asked for the current config to be applied now. A reload
that has to stop a drive that does not stop within its bound ends there,
before it starts anything: the client is answered HTTP 500 `internal_error`
("the running sync is stopping"), and the watch ends with
`ErrSyncStuck`.

Removed or newly paused mounts are stopped and closed. Newly added or newly
resumed mounts are started when they are runnable; incompatible-store mounts
are warned and skipped without bouncing healthy runners. Already-running mounts
remain running only when their runtime mount specs are equivalent. Content
filter fields (`included_dirs`, `ignored_dirs`, `ignored_paths`,
`ignore_dotfiles`, `ignore_junk_files`, `follow_symlinks`) are part of that
equivalence check, so a filter config change restarts the affected runner and
forces fresh startup observation under the new visibility policy. When a timed
pause has already expired by reload time, the config keys are cleaned up but the
running mount is not bounced.

Reload compiles active non-paused mounts without creating sync roots. A newly
added or resumed drive whose resolved `sync_dir` is missing is still included in
the new runtime set; its engine startup records `ErrMountRootUnavailable` as a
per-mount startup failure while the reload continues applying unrelated
add/remove/pause changes.

## Runtime Ownership

The control plane has one mutable runtime structure in watch mode: the active
runner set.

- The `RunWatch` select loop is the single writer for the `runners` map.
- `startWatchRunner` creates one goroutine per running mount. That goroutine owns closing the runner's `done` channel exactly once on exit. `closeEngineWithin` starts the goroutine that closes an engine (a stopped watch runner's, or a one-shot pass's) and its caller owns it through the wait of up to `engineCloseTimeout`; a runner goroutine or close goroutine that outlives its bound is left to the process exit that the stuck error causes.
- The control command channel is internal to `RunWatch`; socket handlers send commands into that channel and wait for one response.
- The control plane owns four timers, all read only by the RunWatch goroutine: the reconcile ticker (default every 5 minutes), a one-shot wake-up for the earliest backed-off runner restart, a one-shot wake-up for the earliest timed-pause expiry, and the bound `retireWatchRunner` arms on one stop or close. All are injectable for tests (`reconcileTicks`, `restartTimer`, `retireTimer`). Reload, stop, and perf capture are event-driven through control-socket requests and context cancellation.
- `watchReportState` (announced start failures, per-key last warnings, restart backoff) is owned by the RunWatch goroutine alone and needs no lock; it is reset at each RunWatch start and is disposable.
- The control plane owns live perf registration for active mounts, but not the
  counters themselves. `internal/perf` owns the aggregate/live snapshot state;
  the control plane only exposes that state through the local socket and
  forwards explicit capture requests into the owning runtime.
- The control plane puts each mount's collector in that mount's run context:
  `startWatchRunner` wraps the runner's context and the one-shot work function
  wraps the context its `MountRunner` hands it. The engine receives the same
  collector directly for its own phases and counters, but HTTP requests and
  retries, store transactions and transfers are recorded on whatever collector
  the context carries, so without the wrap a mount's snapshot shows none of
  them. The mount collector's parent is the owner's aggregate collector, whose
  parent is the command collector, so each record is counted once at every
  level.
- Work the control plane does for a mount outside the mount's run stays on the
  command collector only: connecting the session, constructing and closing the
  engine, and the parent-store steps the orchestrator drives itself (final-drain
  acknowledgement and child artifact cleanup).

## `MountRunner`

`MountRunner` wraps a single mount's sync function with panic recovery and
error isolation. One mount panicking must become one isolated mount outcome,
not a process-wide crash or a cross-mount failure cascade.

Both recoveries -- `MountRunner.run` for one-shot and the watch runner
goroutine's `runWatchRunnerEngine` -- go through `recoveredSyncPanic`, so a
panic reads the same in either mode. It calls `panicguard.Recovered`, which
every recover in the product calls (system.md, R-6.3.10): one Error record
with the `drive`, `mount_id`, `panic` and `stack` attributes, and the report
error, wrapping `panicguard.ErrPanic` ("internal error while syncing <drive>:
<panic>. This is a bug; please report it, with the log file if you have one"),
that is the user-facing account:
the CLI prints it for a one-shot sync, and the watch loop reports it as the
reason the drive stopped before restarting it. The stack is captured on the
panicking goroutine because the recovered value names what failed but not
where. It reaches the log file and the `--verbose` console only: the CLI's
console handler drops `stack` at default verbosity (`isInternalLogField`), so
a panic does not put kilobytes in front of a person or a `--quiet` console.
Evidence: `TestMountRunner_Run_PanicIsLoggedAtErrorWithStack`,
`TestRunWatch_RunnerPanicFailsOnlyThatMountAndRestartsIt`,
`TestCommandLogger_PanicStackOnlyInLogFileAndVerboseConsole`.

User-facing labels use the drive or shortcut display name (`mountSpec.label`);
the mount ID (`<parent canonical ID>|binding:<item ID>` for shortcuts) appears
only as a `mount_id` log attribute.

## CLI Contract

The `sync` Cobra command resolves drives, validates sync eligibility,
compiles selected drives into `multisync.StandaloneMountSelection`, constructs
an `Orchestrator`, and chooses between `RunOnce` and `RunWatch`.

- `--watch` selects daemon mode
- `--download-only` and `--upload-only` select sync mode
- `--dry-run` and `--full` apply only to one-shot mode
- `--confirm-deletes` applies only to one-shot mode: it releases the
  delete-safety hold for that pass (`RunOptions.ConfirmDeletes`), so the user
  confirms the held set they were shown rather than whatever a watch session
  deletes later (`TestNewSyncCmd_ConfirmDeletesIsOneShotOnly`)
- `--dry-run` binds, probes, and unlinks the control socket like any one-shot
  sync owner; it only changes plan execution and commit semantics
- first SIGINT/SIGTERM cancels the shared watch contexts and lets each mount's
  engine seal new admission and follow its normal shutdown path
- second signal forces exit

The first signal is bounded by the stop and close bounds of Stopping a
runner: a drive that has not stopped 45 s after it ends the watch with
`ErrSyncStuck` and exit status 1 instead of 130, inside systemd's
default 90 s stop timeout, and a `Restart=on-failure` unit starts a fresh
process. The second signal still forces exit at once.

`OrchestratorConfig.WatchLifecycle` reports `RunWatch`'s transitions to the
CLI (`watch_lifecycle.go`, R-6.6.32): `WatchReady` once startup finished with
at least one runner, `WatchReloading` and `WatchReloaded` around every
control-socket reload, and `WatchStopping` before the runners drain. A
`WatchReloaded` states its outcome in `Reload`: `ReloadApplied` (the drive
set is filled in) or `ReloadKeptPreviousSettings` (with the reason in
`Err`). The CLI switches on the outcome and never infers it from `Err`, so a
new kind of reload error cannot be reported as one that kept the previous
settings. A reload that ends the watch because a drive did not stop reports
no `WatchReloaded`; `WatchStopping` follows it. The hook runs on the watch-loop
goroutine that owns the runner set, so its `SyncRoots` and `Skipped` are a
consistent snapshot; it must not block. A watch that fails to start reports
nothing. The CLI turns these into systemd notifications; multisync knows
nothing about systemd.

The CLI command does not reach into per-mount engine internals. It only speaks
to the control-plane boundary.

## Design Constraints

- No mount-to-mount coordination state exists in memory or in SQLite.
- Each mount gets its own engine instance and state DB.
- Session creation goes through `driveops.SessionRuntime` with
  `driveops.MountSessionConfig`, keeping token-source caching owned in one
  place and keeping `ResolvedDrive` out of runtime construction. The same
  holds for account-level clients: the CLI's identity probes and discovery
  calls take theirs from `SessionRuntime.BootstrapClient`, so within one
  runtime one sign-in file has one token source (R-6.8.27). The `sync`
  command still has two runtimes: its sync-bootstrap identity probe runs on
  the command's runtime before `runSyncOnce`/`runSyncDaemon` build the
  engine's, which loads the sign-in file after the probe has saved any
  refresh. Giving the command one runtime for both is [planned].
- Reload updates config through one shared `config.Holder` and uses the
  CLI-supplied standalone-mount selection compiler, so both the control plane
  and session runtime see the same config snapshot without giving multisync
  authority over resolved-drive construction.

## Rationale

- **Separate control plane from runtime**: multi-mount lifecycle code changes
  for different reasons than single-mount execution logic. Keeping them in one
  package made both harder to reason about.
- **Always use the same top-level path**: one configured drive and many mounts share the
  same shutdown, reload, and panic-isolation semantics.
- **Per-mount isolation is explicit**: the control plane collects one report
  per mount and one panic cannot poison unrelated mounts.
