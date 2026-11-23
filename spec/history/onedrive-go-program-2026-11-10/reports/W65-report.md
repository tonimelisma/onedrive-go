# W65 report: a stopping run records its completions on a detached, bounded context

Commit `b9b12ee3` on `fix/stop-records-completions`, one commit on `ca8ecac6`, not pushed. 20 files.

## What changed
- `worker.go`: `runDetachedStoreAccess` = `detachedStoreContext(ctx, deadline)` (a nested one keeps the outer deadline) + `storeAccessError` (joins `errStoreAccessTimedOut` once); `sendResult` delivers whenever the channel has room.
- `engine_runtime_shutdown.go`: `stopDrain` (one deadline per stop, a count, the first error); `beginStopDrain`/`endStopDrain` (one Warn with the count, returns the store failure); `processShutdownCompletion` derives each completion's context from the run's, marked `isStoppingRun`; `lostToTheStore` case dropped; receivers `f` -> `flow`.
- `engine_runtime_types.go`: `engineFlow.stop`, `engineFlow.stopBound` (= `OutcomeCommitTimeout`, tests shorten).
- `engine_run_once_results.go`: stop begins the drain, seals the outbox, no re-select of an ended context, releases no held work; returns the drain's store failure when nothing failed first.
- `engine_watch_drain.go`: `beginWatchDrain` begins the drain; `runDrainStep` ends it when the drain is over.
- `engine_watch.go`: `isWatchShutdownError` never reads `errStoreAccessTimedOut` as a clean stop.
- `engine_runtime_admission.go`, `engine_runtime_completion.go`, `engine_runtime_permissions.go`, `engine_runtime_invariants.go`: a stopping run admits, publishes, probes and checks nothing.
- `engine_results.go`: a draining watch arms no retry or trial timer.
- `engine_retry_trial.go`: the success's retry clear via the helper; `action_freshness.go`: dead shutdown bypass removed.
- Spec: `sync-engine.md` (5 places), `sync-execution.md` (1 row), `requirements/sync.md` R-2.8.3.

## Tests (internal/sync only; none in internal/multisync; none renamed or removed)
Added, each seen failing before the product change (`S/w65/tests-first.log`):
- `TestOneShotEngineLoop_ACanceledPassRecordsTheCompletionsItDrains`: was retry rows `[]`, gone item kept (`context canceled`).
- `TestWatchRuntime_ADrainRecordsTheCompletionsItReceives`: same, through the watch drain.
- `TestWatchRuntime_AStoreThatDoesNotAnswerEndsTheDrainAtItsBound`: was no store-failure error, no count Warn.
- `TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound` (stop limit): was 30.1 s (own bound), second clear ran.
- `TestRunDetachedStoreAccess_ANestedAccessKeepsTheOuterBound`: was a new deadline.
- `TestOneShotEngineLoop_ACanceledPassSealsItsOutbox`: was a spin until the 1-minute wait.
- `TestEngineFlow_AStoppingRunAsksOneDriveNothing`: was the 403 not recorded.
- `TestWorkerPool_AStoppedPoolDeliversEveryCompletionItHasRoomFor`: was 15 of 32 delivered.
Changed: `TestEngineFlow_ACommitThatOutlastsItsBoundWhileStoppingIsSaid` (rider: recorded, no Warn; drain write cut -> one Warn, store failure; was `context canceled`), `TestIsWatchShutdownError_OnlyTheEndedRunsOwnCancellation` (+ store timeout; seen failing on the old line, `S/w65/watch-shutdown-old.log`).

## Checks (final tree)
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/sync/...` (lock): ok (377 s).
- `go test -race ./internal/sync/` (lock, 90m): ok (4177 s).
- New tests 30x `-race`, one `-run` (lock): ok.
- Lint (lock): `0 issues.` (`W65-lint.log`; the first lint found containedctx/contextcheck: fixed by keeping a deadline, not a context; all test runs above are after that fix).
- `RealRepoIsClean`: 4 PASS. Spec test names: all 12 exist.

## Deviations
- Beyond the drain (same rule, found on the way): `sendResult` dropped about half the completions that finished after a stop; the one-shot loop busy-spun and offered its outbox to workers after a cancel; a draining watch armed timers once records succeed; the permission probe would have made a Graph request during a stop on a detached context. All fixed, with tests.
- The bound starts when the run first sees its stop, not at the cancel itself (see Not fixed 1).

## Also fixed
- `worker.go` stale comments ("drain goroutine", "silently dropped"); hand-rolled detaches in `engine_retry_trial.go:82`, `engine_runtime_invariants.go:115` now use the helper.

## Not fixed (Seen and not fixed: 8)
1. `multisync/watch_runner_stop.go:28`: if the cancel lands while the loop is inside a detached wait (live success clear, `engine_shortcut_release.go:668`, findings `engine_watch_loop.go:389`) and the store does not answer, drain starts up to 30 s late: 60 s > 45 s, runner stuck. Fix: anchor the drain deadline at the cancel.
2. `engine_runtime_completion.go:75`: `if handled, err := maybeHandlePermissionFailure(...); handled` drops `err` when not handled (handle403's boundary read failure). Fix: return it; test with a store whose read fails.
3. `engine_watch_loop.go:389`: findings after a cancel on a hand-rolled bound outside the stop's one bound, no `errStoreAccessTimedOut`. Fix: use the drain's deadline.
4. `engine_shortcut_release.go:668`: hand-rolled detach. Fix: `runDetachedStoreAccess`.
5. `executor_transfer.go:681,700`: hand-rolled detaches (another branch's file). Fix: the helper, after it merges.
6. `engine_run_once_results.go:148`: a reduction failure sets `fatalErr` without canceling the pass, so in-flight actions run on. Fix: cancel there as the live branch does.
7. `worker.go` `sendResult`: with the channel full after the cancel (watch, 4096) a completion can still be dropped. Fix: block when the engine is known to drain to close.
8. `multisync/watch_runner_stop.go:19-27`, `spec/design/sync-control-plane.md:504`: wording says each wait has its own bound; the drain now shares one. Fix: name it.

## For the reviewer
- Packages to test besides `internal/sync`: `internal/multisync`, `internal/cli` (watch exit on a drain store failure). Live check (kill-mid-transfer e2e) is the owner's.

## Review fixes

Commit `0d22935f` on `fix/stop-records-completions`, one commit on `main` `0e31e63c` (rebased from `ca287a32` without conflicts: main's two new commits touch none of this commit's files). Not pushed. 23 files (16 touched by this pass).

What changed, per item:
- Finding 1 (`engine_runtime_completion.go:75-81`): the error of `maybeHandlePermissionFailure` goes through `failAfterControlStateError` before `handled` is read; a handled failure returns nil as before. Test `TestEngineFlow_ProcessActionCompletion_BoundaryReadFailureFailsTheCompletion` (`engine_runtime_completion_test.go`): red before the fix (the error named the later retry write, "record retry_work ... database is closed", not the boundary read), green after.
- Finding 2: `engineFlow.canceledAt` (`engine_runtime_types.go:59-62`, an `atomic.Pointer[time.Time]`) is set once by `noteRunCancel` (`engine_runtime_shutdown.go:45-56`), a `context.AfterFunc` on the run's context whose callback first defers `wallClock.recoverTimerPanic`, the engine's timer-callback boundary (the panicguard boundary scan accepts it; `internal/panicguard` tests pass). The one-shot results loop (`engine_run_once_results.go:24-25`) and `runWatch` (`engine_watch.go:77-78`) register it and stop it as they return. `beginStopDrain` (`engine_runtime_shutdown.go:58-72`) takes the earlier of now and `canceledAt`, plus `stopBound`; a stop no cancel caused keeps now. About 20 product lines; no multisync code. The `stopDrain` comment (`:11-24`), the `OutcomeCommitTimeout` comment, the drain and loop comments and the commit message say the bound is measured from the cancel. Test `TestWatchRuntime_AStopsBoundStartsAtItsCancel`: red with `noteRunCancel` in place but `beginStopDrain` unchanged (the deadline was first sight plus the bound; both assertions failed), green after.
- Finding 3: R-2.8.3 (`spec/requirements/sync.md:92`), the drain paragraph of `sync-engine.md` (about :940-971) and the commit message: a one-shot run then fails as for a store failure; a watch says the loss in the one warning and stops as asked.
- Finding 4: `sync-control-plane.md:504` (the row) and :507-523 (the arithmetic) name the drain's one bound and its start, the cancel; nothing says "each bounded by" of the drain's records.
- Note 5 (`engine_runtime_shutdown.go:74-108`): the warning is "The stop's time ran out before N results were recorded; the next sync will check them again", the error "sync: the stop's time ran out before recording N of its results: ..." with the store's error and `errStoreAccessTimedOut` kept as the cause. The three tests that read the text (two in `engine_runtime_shutdown_test.go`, one in `request_timeout_outage_test.go`; one gained an assertion on the error text) were red against the old text and green against the new.
- Note 7: the guard went in. `reduceReadyFrontier` returns `ready` unchanged for a stopping run (`engine_runtime_start.go:62-73`); the same check in `runPublicationDrainStage` became unreachable and is removed, its comment saying a stopping run never gets there. No existing test needed the release (the whole `./internal/sync/...` run passed), so `sync-engine.md:470` and `engine_run_once_results.go:99-101` are now true of the stage. Test `TestEngineFlow_AStoppingRunReleasesNoHeldWork`: red before (the due held action was released and completed as shutdown), green after.
- Note 8 (`worker.go:284-291`): the comment says the rule: a ctx that runs under a detached context, itself or one derived from it, is returned as it is, any cancel of its own included.
- Note 9: the pool holds a logger (`workerPool.logger`). `sendResult` logs one Debug line with the action id and path when it drops a completion after the run ended (`worker.go:397-421`). Test `TestWorkerPool_ACompletionDroppedAfterTheStopIsLogged`: red before (no line), green after. The `sync-execution.md` row says it and cites the test.
- Note 10: the clause is gone; the comment names `TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound`.
- Note 11: the `OutcomeCommitTimeout` paragraph (`worker.go:43-54`) is reflowed.
- `internal/multisync/watch_runner_stop.go:19-27`: unchanged. After finding 2's fix no sentence of it is untrue: each detached write is bounded by `OutcomeCommitTimeout`, the drain's records end within one bound of the cancel, and a store that needs two waits is wedged.
- Spec tables: `sync-engine.md:102` and `:104` name the new behaviour and tests; every test the four spec files cite exists.

Tests in `internal/sync`: added `TestEngineFlow_ProcessActionCompletion_BoundaryReadFailureFailsTheCompletion`, `TestWatchRuntime_AStopsBoundStartsAtItsCancel`, `TestEngineFlow_AStoppingRunReleasesNoHeldWork`, `TestWorkerPool_ACompletionDroppedAfterTheStopIsLogged`. None renamed or removed. Changed: `TestWatchRuntime_AStoreThatDoesNotAnswerEndsTheDrainAtItsBound` (comment, warning text, error-text assertion), `TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound` and `TestEngineFlow_ACommitThatOutlastsItsBoundWhileStoppingIsSaid` (warning text).

Stress declarations, for the rebase onto the stress-members change (its file is not on this base). Every new test of the branch:
- `TestOneShotEngineLoop_ACanceledPassRecordsTheCompletionsItDrains`: member, `stressSyncOneShotAndWorkerPoolMembers`: the loop drains a canceled pass's completions.
- `TestOneShotEngineLoop_ACanceledPassSealsItsOutbox`: member, same list: a canceled loop that once spun.
- `TestWorkerPool_AStoppedPoolDeliversEveryCompletionItHasRoomFor`: member, same list: it guards a send that once chose at random.
- `TestWorkerPool_ACompletionDroppedAfterTheStopIsLogged`: rule-named; non-member, `stressSyncConcurrencyNonMembers`: one goroutine, and a full channel with an ended context leaves the send one choice.
- `TestWatchRuntime_ADrainRecordsTheCompletionsItReceives`: member, `stressSyncWatchRuntimeMembers`: the watch drain.
- `TestWatchRuntime_AStoreThatDoesNotAnswerEndsTheDrainAtItsBound`: member, same list: a held store and a 20 ms bound.
- `TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound`: member, same list: a held store and a shared deadline.
- `TestWatchRuntime_AStopsBoundStartsAtItsCancel`: member, same list: a context callback on its own goroutine and a detached deadline.
- `TestEngineFlow_ProcessActionCompletion_BoundaryReadFailureFailsTheCompletion`, `TestEngineFlow_AStoppingRunReleasesNoHeldWork`, `TestEngineFlow_AStoppingRunAsksOneDriveNothing`, `TestRunDetachedStoreAccess_ANestedAccessKeepsTheOuterBound`: not named by the rule, so no declaration.

Checks (final tree): `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. gofumpt and goimports: clean. `go test ./internal/sync/...` (lock): ok (398 s). `go test ./internal/multisync/ ./internal/cli/` (lock): ok. `-race` (lock): the 13 new and changed stop tests 30 times, ok; the 70 tests of the four changed test files once, ok. `internal/panicguard` tests: ok. `RealRepoIsClean`: 4 PASS. `devtool verify lint` (lock): pass (`W65-lint.log`). Commit message: lines at most 72, trailer kept, no plan id or process word.

Live check (installed `0d22935f`; test account; a new folder of synthetic files in the test sync folder, 12 of 512 KiB and 4 of 64 MiB; the watch client stopped with SIGTERM in mid-transfer, its wrapper restarted it): at the stop 8 small and 2 large files were on OneDrive and 6 actions in flight; the client exited 10.5 s after the signal, with no "time ran out" warning and no other warning or error. After the exit 12 of 16 files were on OneDrive and in the baseline (717 entries before, 730 at the restart: the folder and 12 files). The restart planned 4 uploads and 2 baseline updates, no upload of a recorded file; two large uploads resumed their sessions from 60 of 64 MiB; all 16 files ended on OneDrive, and the 12 recorded before the stop kept their eTags (not uploaded again). The folder was then deleted; its 17 remote deletes ran and it is gone from OneDrive.

Seen and not fixed (causes unknown, not checked):
- The live stop took 10.5 s from the signal to the exit with 6 actions in flight; the log at Info does not say what it waited for.
- The restart planned 2 baseline updates for files whose uploads were recorded before the stop; the log at Info does not say which or why.

## Second review fixes

Commit `e71646e6` on `fix/stop-records-completions`, one commit on `main` `9e5498a5` (rebased from `0d22935f` over #939 and #940 without conflicts). Not pushed. 25 files.

Per finding:
- 1 (blocking), settled. `applyTrialPermissionReclassification` (`engine_runtime_permissions.go`) returns `false, nil` for a stopping run (`isStoppingRun`), as `maybeHandlePermissionFailure` does; the drained trial falls back to `applyTrialRetryFallback`. Test `TestEngineFlow_AStoppingRunAsksOneDriveNothingForATrial`: red before (one Graph probe, and the row recorded blocked under a new `perm:remote:write:` scope at the drive root, with a "Paused syncing /" warning, during the stop), green after (no probe, the row recorded as unblocked retry work). Every other request a stop can start on the detached context: a VTA call graph of `internal/sync` from `processShutdownCompletion` (x/tools 0.49.0; plain CHA gave false paths through the drain's `cancel` func value) reaches one network call, `permissionChecker.ListItemPermissions` in `handle403` (`permission_probe_remote.go:99`, `:224`), and one disk probe, `handleLocalPermission` (`synctree.Root.Open` and `os.File.Close` in `isDirAccessible`, `permissions.go:38-46`). Both are reached only through `maybeHandlePermissionFailure` and `permissionEvidenceForCompletion`, now guarded on both branches. The other `internal/graph` and `internal/driveops` calls it reaches are pure classification (`classifyServerAnswer`, `ClassifyLocalError`, `EvaluateWriteAccess`). Not followed: the activity and debug hooks the caller installs.
- 2 (should), settled. `applyTrialReclassification` (`engine_runtime_completion_trial.go`) reads the error of `applyTrialPermissionReclassification` before `handled`, so its three error paths (boundary read, blocked-row clear, evidence write) fail the completion through `failAfterControlStateError`. Test `TestEngineFlow_ProcessTrialDecision_BoundaryReadFailureFailsTheCompletion`: red before (the error was the later "record retry_work ... database is closed"), green after ("list remote block scopes"). `sync-engine.md:104` and the commit message say "a scope trial's included" and cite the test.
- 3 (should), settled. One-shot: `TestOneShotEngineLoop_ACanceledPassRecordsTheCompletionsItDrains` now asserts `canceledAt` is set after the loop returns. Watch: `runWatch` takes its runtime as a parameter (`RunWatch` passes `newWatchRuntime(e)`; a refactor), and the new `TestRunWatch_RecordsTheCancelOfItsRun` runs it on a stopped context and asserts `canceledAt` is set. With either production registration replaced by a no-op its test failed ("Condition never satisfied"); restored, both pass. The debug-event deadline was not used: whether the loop sees the cancel before the `AfterFunc` callback records it is a race, so such a test could not be deterministic.
- 4 (should), settled (declarations below).

Notes:
- 5: fixed. The stopping-run paragraph of `sync-engine.md`, the `isStoppingRun` and `finishResultsLoopIfSettled` comments, the test comment and the commit message say "no due held work", and that a scope trial that succeeded still releases its scope's next batch, which completes as shutdown with its rows unblocked.
- 6: fixed. `sync-control-plane.md` (the arithmetic), the `stopDrain` comment, the drain paragraph of `sync-engine.md`, the test comment and the commit message say the drain's records end within one bound of the cancel; the control-plane paragraph adds that the drain also waits for every worker, whose own writes are bounded from their own start.
- 7: nothing to change; each state is one the next plan or start repairs.

Stress declarations (`internal/devtool/verify_stress_members.go`), every rule-named test of the branch, in name order:
- `stressSyncOneShotAndWorkerPoolMembers`: `TestOneShotEngineLoop_ACanceledPassRecordsTheCompletionsItDrains` (a canceled pass's drain and its cancel callback), `TestOneShotEngineLoop_ACanceledPassSealsItsOutbox` (a canceled loop that once spun), `TestWorkerPool_ACompletionDroppedAfterTheStopIsLogged` (a pool send against an ended context, the shape of `TestWorkerPool_SendResultDropsWhenContextCancellationWins`), `TestWorkerPool_AStoppedPoolDeliversEveryCompletionItHasRoomFor` (a send that once chose at random).
- `stressSyncWatchRuntimeMembers`: `TestWatchRuntime_ADrainRecordsTheCompletionsItReceives` (the watch drain), `TestWatchRuntime_AStopsBoundStartsAtItsCancel` (a context callback on its own goroutine), `TestWatchRuntime_AStoreThatDoesNotAnswerEndsTheDrainAtItsBound` (a held store and a 20 ms bound), `TestWatchRuntime_EveryRecordOfAStopSharesItsOneBound` (a held store and a shared deadline).
- `stressSyncRunWatchMembers`: `TestRunWatch_RecordsTheCancelOfItsRun` (the watch's entry and its cancel callback).
- Not named by the rule: the four `TestEngineFlow_` stop and boundary-read tests, `TestEngineFlow_AStoppingRunAsksOneDriveNothingForATrial`, `TestRunDetachedStoreAccess_ANestedAccessKeepsTheOuterBound`.

Behaviour changed beyond the findings: none.

Checks (final tree, heavy ones behind the lock): `go build ./...`, `go vet ./...`, tagged vet: pass. gofumpt, goimports: clean. `go test ./internal/sync/...`: ok (596 s). `go test ./internal/multisync/ ./internal/cli/`: ok. `go test -race` on the stop, pool, completion, permission, classification and watch-runtime tests (`-run` pattern): ok; the 10 new and changed stop tests 20 times with `-race`: ok. `devtool verify lint`: pass (`W65-lint.log`). Stress declaration test: ok.

Live check (installed `e71646e6`; test account; new folders of synthetic files, 12 of 512 KiB and 4 of 192 MiB, moved into the test sync folder; SIGTERM to the watch client once the baseline held 3 of the files; the wrapper restarted it 60 s later). Two earlier attempts (64 MiB files; then a trigger at 6 recorded files) sent no stop, because the large uploads finished first; both folders were deleted.
- Run A: SIGTERM with 1 small and 2 large recorded; exit 1.0 s later; no WARN or ERROR; after the exit OneDrive had 3 files, all 3 in the baseline, no retry rows. The restart planned 14 uploads and 1 baseline update for 13 unrecorded files, one upload resumed its session, all 16 ended on OneDrive and in the baseline; 2 of the 3 recorded files had a different OneDrive eTag at the end than their baseline eTag at the exit.
- Run B: SIGTERM with 3 large recorded; exit 0.7 s later; no WARN or ERROR; after the exit OneDrive had 5 items, 3 in the baseline, each with its OneDrive eTag, hashes, size, mtime, ctime and inode matching. A dry run in the restart gap planned 11 uploads and, for the 2 unrecorded items, 2 downloads with conflict copies; the restarted watch planned 13 uploads, no conflict copy, and resumed one 192 MiB upload from 190 MiB; the 3 recorded files kept their eTags.
- Both folders were then deleted locally and from OneDrive.

Seen and not fixed (causes unknown, not checked):
- Run A: which recorded file the extra upload and the baseline update were for, and why.
- Run B: whether the unrecorded 512 KiB item OneDrive had at the exit held its whole content; it was uploaded again after the restart.
- Run B: the dry run in the restart gap planned conflict copies and downloads for the 2 unrecorded items where the watch run planned uploads.


## Shipped

Merged as #941, `d339dd23` on `main` (squash of `90e7a737`), 2026-11-17 04:06 PST; installed from `main` the same morning (`onedrive-go --version` prints `d339dd23`).

After the last review (no blocking, no should-fix, seven notes):
- Note 2, done (test only, no product change): `TestEngineFlow_AStoppingRunProbesNoLocalFolder` and `TestEngineFlow_AStoppingRunProbesNoLocalFolderForATrial` in `engine_runtime_shutdown_test.go`: a download refused with a local permission error in a listable, unwritable folder during a stop is recorded as unblocked retry work with no folder scope. With either stop guard in `engine_runtime_permissions.go` removed, both failed (the row recorded blocked under a local-write scope, the scope active); green with the guards. 10 times with `-race`: ok. Not named by the stress placement rule, so no declaration; the three stress-declaration tests and the four RealRepoIsClean tests pass.
- Notes 1, 3, 4, 5: confirmations, nothing to change.
- Note 6 (a dry run in a restart gap is seeded without `pending_creates`): left, it predates this change and is outside it; a plan-row candidate. The PR names the preview difference under "Not known".
- Note 7 (the live check's reading): taken into the PR body and the CHANGELOG. The two files run A seemed to touch were this client's empty placeholders for uploads still in flight, adopted as baseline rows by the poll 0.8 s before the stop: one resumed its session, one was recorded by a baseline update without a transfer. No completed transfer was repeated or lost in the four live runs. The live runs did not exercise the changed paths (no failure was drained, no retry row at any exit); the unit tests cover them. The earlier "Seen and not fixed" items of the second review fixes are answered by this, except the dry run's preview difference (note 6), whose cause is unknown.

Gates on `90e7a737`: start stage passed (the nightly of 2026-11-16 passed; nothing claimed); pre-pr passed (lint `0 issues.`, then the quick tier). PR CI: all jobs passed, the live e2e and integration jobs included. Pre-merge passed; post-merge passed and removed the worktree.

Still open, planned elsewhere: the three hand-made detached store waits after a cancel (watch findings write, shortcut release, transfer executor) and a completion cut while being applied at the cancel. Not yet planned: a full result channel after a stop can still drop a completion (said at Debug); a one-shot reduction failure does not cancel the pass.
