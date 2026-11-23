# B8 report: watch runners carry an identity, every stop is bounded, a stuck drive fails closed

Branch `fix/watch-runner-stop`, one commit (amended in the second pass; hash in the final message) on `34fb1a66`. The first pass finished the draft: generations, one bounded `retireWatchRunner`, fail closed, the owner lock held through the drain, the reset under the folder lock. The second pass fixed the seven entries the first one left under "Not fixed".

First pass (read and accepted as `4bf0237b`; its tests, each seen red, are unchanged): `TestHandleWatchRunnerEvent_EventFromReplacedRunnerIsIgnored`, `_EventFromReplacedFinalDrainChildIsIgnored`, the stale-parent-snapshot case, `TestApplyWatchMountSetForParent_NormalToFinalDrainRestartsChild`, `TestRetireWatchRunner_TimeoutFailsClosed`, `_EngineCloseTimeoutFailsClosed`, `TestWatchRunnerStopPaths_ARunnerThatDoesNotStopFailsClosed` (six paths), `TestRetireAllWatchRunners_StuckDrivesShareOneStopBound`, `TestRunWatch_StuckRunnerOnReloadEndsTheWatchWithAnError`, `_OnShutdown...`, `TestStartWatchRunner_CancelledRunnerDoesNotBlockOnAFullEventChannel`, `TestRunWatch_OwnerLockHeldUntilRunnersDrain`, `TestCommandExitCode` cases.

## What changed (second pass)
- `internal/multisync/engine_close.go` (new): `ErrSyncStuck` (renamed from `ErrWatchRunnerStuck`), `engineCloseTimeout` = `syncengine.StoreCloseCheckpointTimeout` + 5 s, `closeEngine` (moved), `closeEngineWithin`, `waitWithin`, `reportStuckSync`, `allGoroutineStacks`, shared by watch and one-shot.
- `watch_runner_stop.go`: uses the shared close; the stop bound's comment now spells out the arithmetic.
- `orchestrator_run_once.go`, `orchestrator_run_once_children.go`, `report.go`: one `closeRunOnceEngines` with the bounded detached close; `RunOnceResult.Stuck`; a shortcut whose engine did not close is neither acknowledged nor cleaned up.
- `watch_lifecycle.go`, `watch_reload.go`, `orchestrator_watch.go`: `ReloadOutcome` (`ReloadApplied`, `ReloadKeptPreviousSettings`) on `WatchReloaded`; the emit takes the event.
- `internal/cli/sync_notify.go`: status follows `event.Reload`, never `Err`. `sync_render.go`: `runOnceResultError` joins `Stuck`.
- `internal/cli/drive_reset_sync_state.go`, `drive_rebind_root.go`: one `changeDriveStateUnderFolderLock` (and `driveSyncDir`) for both commands.
- `internal/sync/engine_root_lock.go`: `LockDriveForOfflineChange` replaces `LockSyncRoot`; it locks the folder at the path and the folder the store records (by recorded device and inode). `store_inspect.go`: read-only `readRecordedSyncRoot`.
- `internal/sync/store.go`: `Close` bounds its own detached checkpoint (`StoreCloseCheckpointTimeout`, 10 s); `engine.go`, `store_reset.go`, `store_scratch.go`, `engine_current_plan.go` pass their context instead of detaching it. `engine_retry_trial.go`, `engine_watch_loop.go`, `engine_runtime_invariants.go`: detached contexts bounded by `OutcomeCommitTimeout`; `worker.go`: its comment names every user.
- `internal/config/drive.go`: `ResolveAccountNames` gets its doc comment back; `EffectiveSyncDir`'s says a configured value comes back unexpanded.
- Specs: R-6.3.9 text; `sync-control-plane.md` (arithmetic, one-shot close, reload outcome, three Verified By rows, ownership line); `sync-engine.md` (offline-change lock, new "Detached store writes" table); `sync-store.md` (close bound, recorded-root read); `cli.md` (both offline commands, one-shot stuck exit, notifier); `degraded-mode.md` DM-12; `docs/running-as-a-service.md`.

## Offline commands that write a drive's state store
Behind `resolveDriveForStateChange` / `ensureNoLiveSyncOwner` are exactly two: `drive reset-sync-state` and `drive rebind-root`. Both now hold the folder locks. `drive remove --purge` and `logout --purge` also delete state stores, without the probe or the lock (see Not fixed).

## Detached contexts in internal/sync and internal/multisync (all now bounded)
`worker.go` commitOutcome: OutcomeCommitTimeout (30 s). `executor_transfer.go` recordPendingCreate: 30 s. `engine_retry_trial.go` clearRetryWorkOnActionSuccess: 30 s (was none). `engine_watch_loop.go` shutdown findings: 30 s (was none). `engine_runtime_invariants.go` three checks: 30 s (was none). `store.go` Close checkpoint: StoreCloseCheckpointTimeout 10 s (was none). `engine_close.go` engine close: engineCloseTimeout 15 s. `orchestrator_watch.go` control socket close: controlCloseTimeout 1 s. Removed (Close now detaches itself): `engine.go` startup-failure close, `store_reset.go` x2, `store_scratch.go`, `engine_current_plan.go`. Arithmetic: a healthy detached write takes milliseconds (at most SQLite's 5 s busy wait); the 45 s stop bound is one write at its 30 s limit plus 15 s; the 15 s close bound is the 10 s checkpoint plus 5 s. A store that needs two full waits is wedged, and ending is the intended outcome; written into "Stopping a runner".

## Tests added in the second pass (red seen by reverting that part; logs `b8n-p2-mut*.log`)
- `TestLockDriveForOfflineChange_ContendsWithAnEngineOnTheRecordedFolder` (red: refusal missing with the recorded-folder lock skipped), `_ContendsWithAnEngineOnTheFolderAtItsPath`, `_NoFolderAndNoStoreTakeNoLock` (replace the `LockSyncRoot` test).
- `TestOfflineStateChanges_RefuseWhileAnEngineHoldsTheFolder` (reset and rebind; red for rebind without the lock: it opened the store).
- `TestDetachedContexts_AlwaysCarryADeadline` (red on an unbounded `engine_retry_trial.go:82`), `TestUnboundedDetachedContexts_FlagsOnlyTheUnboundedForm`.
- `TestRunOnce_AnEngineThatDoesNotCloseFailsTheRun`, `TestRunOnce_AShortcutEngineThatDoesNotCloseIsNotAcknowledged` (red: both hung with the unbounded close; the second also red with the early return removed: it acknowledged).
- `TestRunOnceResultError_AnEngineThatDidNotCloseFailsAPassThatSynced` (red: nil error).
- `TestSyncNotifier_ReloadStatusFollowsTheStatedOutcome` (red with the Err-based inference); `TestRunWatch_ReportsLifecycleTransitions` asserts the outcome (red with it unset).

## Checks
Commit `be824a15` (52 files). `go build ./...`, `go vet ./...`, tagged e2e vet: pass. `go test -count=1 -timeout 60m` over `internal/multisync`, `internal/cli`, `internal/config`, `internal/sync` (whole suites, sync 396 s): pass. `go test ./internal/devtool/...`: pass; the GOVERNS, cited-test, routing-table and layering checks also pass against this tree (run through a throwaway test, since removed). `-race -run` for every added or changed test in multisync, cli and sync: pass. golangci-lint over the four packages: 0 issues (`B8-lint.log`).

## Deviations
- `ErrWatchRunnerStuck` renamed `ErrSyncStuck` and used by one-shot too: one condition (an engine that may still write its store), one answer (exit 1, touch nothing), and "WatchRunner" was wrong for a one-shot.
- `LockSyncRoot` replaced by `LockDriveForOfflineChange`, which also locks the recorded folder; it needs a read-only read of the store.
- `SyncStore.Close` now detaches and bounds its checkpoint itself, so five callers stopped detaching.
- A stuck one-shot shortcut skips its final-drain acknowledgement and artifact cleanup for that run.

## Also fixed
- The sync-folder path rule ("effective, then expand ~") was written out in rebind and reset; now one `driveSyncDir`.
- `closeRunOnceChildEngines` and `closeRunOnceParentEngines` differed only in a projection filter; one function.
- `RunOnce`'s comment said errors live in `syncengine.Report`; they live in `MountReport` (and now `Stuck`).
- First-pass list unchanged: log after the stop, merged parent ackers, unused contexts, `writeJSON` doc, nil-context guard, timer comments, dead nil checks, service doc wording.

## Control server closing goroutine
Removed in the first pass, not bounded: the only close is the owner's deferred `closeControlSocket` after the drain (and `startWatchRuntime` on failure), bounded by `controlCloseTimeout` (1 s).

## Not fixed
- `internal/cli/drive_remove.go:48` and `drive_cleanup.go:75` (`drive remove --purge`), `auth_logout.go:172-175` (`logout --purge`): delete a drive's state DB family with neither the owner probe nor the folder lock, before the watch is told to reload. Fix: remove the config section, notify the watch (its reload retires the runner within the bound), then delete under `LockDriveForOfflineChange`, keeping the DB and saying so when it is refused. Marked [planned] in `cli.md`.
- `internal/sync/engine_root_lock.go:46`: the folder lock keys on the sync folder, but the state DB is keyed on the drive; two configurations sharing a data dir with different `sync_dir`s for one drive share a store under different locks. Fix: also lock the state DB path itself for engines and offline changes.
- `internal/sync/engine_root_lock.go` recorded-folder lock: keyed by the recorded device number, which can change across reboots (the identity check uses the fsid for that reason); then the key names another inode or none. Fix: key root locks by fsid when the filesystem reports one.

## Unfinished
None. Live validation (reload with a drive paused and resumed, SIGTERM during a transfer, exit inside the bound, a one-shot `sync` to completion) is for the reviewer.

## Packages the reviewer should test
`internal/sync` and `internal/devtool` were run here; e2e (watch shutdown, `TestE2E_SyncWatch_PauseResume`, kill-mid tests, `drive rebind-root` and reset flows) was not.
