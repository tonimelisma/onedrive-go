# W31 report: a request the transport timed out is retried, not read as shutdown

Helper: `func runEnded(ctx context.Context, err error) bool` (`internal/sync/errors.go`): `err != nil && ctx.Err() != nil`; no engine state. Beside it `isCancellationEcho(err)`, the one reader of the error's shape for callers that already know the run ended.

## What changed
- `errors.go`: `runEnded`, `isCancellationEcho`.
- `worker_result.go`: `actionCompletion.RunEnded`; stale `Err` comment fixed.
- `worker.go`: `sendResult` sets `RunEnded` from the worker's start context (one place for workers); per-action and pool cancels get causes (`errActionReturned`, `errWorkerPoolStopped`).
- `engine_runtime_completion.go`: the publication drain sets `RunEnded` for the completions it builds; Warn (path, kind, error) for a cancel with a live run.
- `engine_result_classify.go`: shutdown only for `RunEnded && isCancellationEcho`; `isRequestTimeout` (deadline or `graph.ErrTransferStalled`) is retry work with `request_timeout`; `isLiveRunCancel` is retry work.
- `engine_run_once.go`: pass cancel causes `errOneShotPassFailed` / `errOneShotPassEnded` (no signature change).
- `observer_remote.go`, `engine_mount_root.go`, `socketio.go`, `socketio_conn.go`: `watchShouldStop` and `mountRootWatchStopped` removed, callers ask the run's end; `deltaPageRetryDelay` no longer refuses a deadline or cancel.
- `engine_watch_loop.go`, `engine_watch_drain.go`, `observer_local.go`: `logObserverError(ctx, err)`, `isFatalWatchSetupError(ctx, err)`.
- `engine_watch.go`, `engine_runtime_shutdown.go`, `engine_timer_panics.go`: use `isCancellationEcho` (behavior unchanged).
- `tracked_action.go`, `internal/multisync/engine_close.go`: dead code removed (see Also fixed).
- Spec: R-6.8.29 added; `sync-engine.md` new classifier row, precondition row and delta-page row; `sync-observation.md` new section.

## Site table (decision 4)
| Site | Context in play | Transport timeout today | After |
|---|---|---|---|
| `classifyResult` | worker's start context, via `RunEnded` | shutdown: dropped with its dependents, not recorded | retry work `request_timeout`, dependents wait; shutdown only once the run ended |
| remote `Watch` loop (`watchShouldStop`, 6 calls) | observer's run | the observer returned nil: OneDrive no longer polled, silently | poll backs off and retries; handler/emit failures are returned as observer errors |
| `deltaPageRetryDelay` / `fetchDeltaPageWithRetry` | run (one-shot) or observer run | page not retried | retried up to `DeltaPagePolicy` (4 attempts) |
| mount-root watch (`mountRootWatchStopped`, 3 calls) | watch run | mount-root watch ended silently | backs off and retries |
| socket.io wake source (3 `watchShouldStop` calls) | wake source's run | a timed-out connection ended the wake source (polling only) | reconnects after backoff; close noted at Debug as before |
| `logObserverError` | watch run | timed-out observer exit neither recorded nor warned | recorded and warned; Debug once the run ended |
| `isFatalWatchSetupError` | local observer's run | rollback on any context error | unchanged in practice: local setup makes no request, so its context errors come only from its run ending |
| `isWatchShutdownError` | watch run | already required the context to have ended | unchanged: `runEnded && isCancellationEcho` |
| `logSuppressedShutdownCompletionError` | stopping run | Debug for the echo | unchanged: echo at Debug, other failures warned |
| `withTimerPanic` | run ended by the panic | echo replaced by the panic | unchanged: echo replaced, other errors joined |
| `closeEngine` (multisync) | detached close context | Debug branch (unreachable: `Engine.Close` returns no context error) | branch deleted, every close failure warns |

## Tests added (errors from a real `graph.Client` against a stalled `httptest` server)
- `TestClassifyResult_TransportTimeoutIsARequestTimeoutUntilTheRunEnds`: header, dial and client timeouts are retry work `request_timeout` with a live run, shutdown once ended.
- `TestRunEnded_AsksTheRunNotTheError`: the helper's answers.
- `TestFullDelta_RetriesATimedOutPageUpToItsPolicy`: 4 requests, 3 backoffs.
- `TestRemoteObserverWatch_BacksOffAfterATimedOutPollInsteadOfStopping`: the watch backs off instead of returning.
- `TestRunOnce_TimedOutRequestIsKeptForRetryAndLeftWork`: failed 1, kept for retry 1, retry row, error shown, `LeftWork{WillRetry: 1}` (registered in `runOnceScenarios`).
- `TestRunWatch_TimedOutRequestIsRetriedAndItsDependentRuns`: folder create retried, upload below it runs.
- `TestContextErrorChecks_OnlyNamedReadersReadTheShape` (failed on 20 checks in 11 functions before), `TestContextErrorChecks_FindsEachFormInItsFunction`.
- Changed: `TestClassifyResult_SuccessAndShutdown` (+2 cases), `TestWatchRuntime_ObserverExit_ReturnedOnceWithCause` (context argument).

- `TestRunOnce_ACancelWhileTheRunGoesOnIsRetriedAndSaid`: a cancel with a live run is kept for retry and said once at Warn with path and kind (hand-built cancel: no stalled server produces one; registered in `runOnceScenarios`).

## Checks (commit `cd91efe1`)
- Red first: every new test failed on the base for the stated reason (watch test: the retry never came).
- `go build ./...`, `go vet ./...`, tagged vet of `./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -count=1 ./internal/sync/...` behind the lock: ok (1008 s).
- `-race` on the added/changed tests and the other tests of the touched test files, plus `TestWorkerPool_`: ok; after the lint fixes, `TestSocketIOWakeSource_`, the classifier and source tests again under `-race`: ok.
- `go test -race ./internal/multisync/...`: ok.
- Each new test `-count=20`: ok (the classifier test again after its helper changed).
- `TestFindTestConventionViolations_RealRepoIsClean`: ok. `ListsEvery|Table|Governs|Exhaustive|Registered`: ok.
- golangci-lint (sync, multisync): `0 issues.` (`W31-lint.log`; a first run found goconst, nilerr and wrapcheck, fixed).
- Spec by hand: every test cited in the three spec files exists; R-6.8.29, R-6.8.10, R-6.6.28, R-2.15.5 exist.

## Deviations
- Stop item (a site needs the error's shape): once the run has ended, `isWatchShutdownError`, `logSuppressedShutdownCompletionError`, `withTimerPanic` and the classifier still tell the cancellation's echo from a separate failure (`isCancellationEcho`). Without it a loop that ended without draining would read as a clean stop (`TestRunWatchLoop_SteadyStateCompletionWithoutDrainingIsAnError`), a disk error during a stop would be hidden (output noise test), and a 403 or stalled transfer finishing during a stop would no longer be recorded. Behavior there is unchanged; the run's end is still asked first.
- Decision 3: no cancel in the engine reaches an action with a live run, so the live-cancel branch has no cause case; causes were added only to the cancels on the action path. The socket.io wake source, its read loop and the multisync config watcher keep plain cancels: no action runs under them.
- The publication drain sets `RunEnded` too: the engine, not a worker, builds those completions.
- Watch test (b) times out a folder create whose dependent is the upload below it: no planner rule makes anything wait for an upload in a plain tree.
- Transient retry rows carry no `condition_type`, so the one-shot test checks the row and the shown error.
- Exit code: unchanged. The timed-out one-shot action was already left work (as unplanned `LeftForNextSync`); now it is failed and kept for retry.
- Requirement number R-6.8.29 (the next after the highest), not the gap at R-6.8.26.

## Also fixed
- `tracked_action.go`: `trackedAction.Cancel` was set and never called; removed.
- `engine_watch_loop.go:504`: the `isDraining` branch was dead (a drain begins only once the run ended).
- `multisync/engine_close.go:45`: an unreachable branch and its comment.
- `worker_result.go:29`: comment named `context.Canceled` as the classification input.

## Not fixed
- `internal/sync/store.go:199`: `Close` logs a failed checkpoint at Debug only, though `Checkpoint`'s doc says cross-process readers depend on it. Fix: Warn, or return it as a close failure.
- `internal/sync/domain_clock.go:66`: "the actions in flight then fail as canceled, which reads as a shutdown" now holds because the run ended (`RunEnded`), not because of the error. Fix: reword.
- `internal/sync/observer_remote.go:425`/`:434`: a handler or emit failure with a live run now ends the observer with that error, which is right, but it has no test. Fix: add one.

## Packages for the reviewer
`internal/cli` (its error presentation may meet `request_timeout` failures more often).

## Second pass

Commit `d3947377` (the one commit, amended, on `3e5c1bd4`; not pushed).

### Items
- A (M1): the claim holds. graph wraps `context.Cause(ctx)` (`client_preauth.go:62`, `:95`, `client_auth.go:215`, `upload_transfer.go:173`; `retry/transport.go:223`) and Go 1.26.11 `net/http/transport.go:2467`, `:2963` cancel with `context.Cause`.
  - A1 `TestRunOnce_AFatalFailureEndsTheTransferItInterruptsAsAShutdown`. On cd91efe1 the three asserts the brief named held: the download, misclassified as actionable `unexpected_condition`, had its retry-work write run on the canceled pass context ("begin transaction: context canceled"), swallowed at Debug by `logSuppressedShutdownCompletionError`; `Failed` was 1; no WARN. What failed is the file event, so the test also asserts it: "only the upload is shown as failed: [local.txt 401, remote.txt request canceled: sync: one-shot pass failed]".
  - A2: `cancelCauseError` (`errors.go`), a string type whose `Is` answers `context.Canceled` only; the four causes are its constants, where they were.
  - A3 `TestCancelCauses_AnswerToCanceledAndNameTheirCancel`: on cd91efe1 "Target error should be in err chain: expected context canceled, in chain: sync: action returned".
  - A4: no fake this commit added or changed returns `ctx.Err()` for an interrupted request; nothing to change.
- B1 `TestWatch_MountRootPollThatTimedOutBacksOffAndPollsAgain`. Needed a seam: the mount-root watch slept on wall time (`timeSleep`, a 5 s first backoff), so its sleeps now use the run's clock (one line); `timeSleep` then had only its own two tests and is removed with them (`observer_remote.go`, `executor_test.go`). Broken (shape check back in `handleMountRootPollError`): "Condition never satisfied: the watch waits out one backoff after the timed-out poll".
- B2 `TestSocketIOWakeSource_ReconnectsAfterAConnectionThatTimedOut` (DialFunc, Clock, LifecycleHook; a connection whose read is expired with `context.DeadlineExceeded`). Broken: "Should not be: stopped: the wake source stopped before connection_dropped".
- B3 `TestWatchRuntime_AnObserverWhoseRequestTimedOutIsWarnedAndReturned`. Broken: WARN count "expected 1, actual 0", and "does not contain Stopped checking OneDrive for changes".
- B4 `TestRemoteObserverWatch_AFailedBatchEndsTheWatchUnlessItsRunEnded` (handler and apply failures, live and ended). Broken: "Expected error with commit observations: context deadline exceeded in chain but got nil" (both live cases). This closes the first report's third "Not fixed" entry.
- C: `stalledGraphClient` returns a client-side dispatch count (a counting `http.RoundTripper`); every request count asserts on it. The server's count stays only where it must be zero (the dial timeout), which no race can break.
- D1 `TestIsWatchShutdownError_OnlyTheEndedRunsOwnCancellation`. Broken (`runEnded` alone): "Should be false: a separate failure during the stop".
- D2: the level assert; broken (Warn to Info): "expected 1, actual 0".
- D3: both lists keyed by file and function (methods as `Recv.Name`); `TestContextErrorChecks_RefusesAnUnnamedSite` plants both cases. On cd91efe1 the real-repo test failed: "errors.go cancelCauseError.Is is named but reads no error's shape".
- D4, D5, D6, D7: done; no "used to", "previously", "no longer" or "now" in new text.
- E: done as a report, no code. (1) The expired bound makes database/sql and the sqlite driver return `ctx.Err()`, so `CommitMutation` fails wrapping `context.DeadlineExceeded`. (2) It reaches the classifier as the completion's error through `wp.sendResult(ctx, ta, nil, commitErr)` (`worker.go:240`); with the run live `isRequestTimeout` takes it: `resultRequeue`, retry work, `request_timeout`. (3) Any other failed store commit has no server answer and no errno, so `classifyLocalResult` gives `resultSkip` (actionable) with condition key `unexpected_condition`: store failures have no class of their own, so the branch was not written.
- F: done. `TestSyncStore_CloseSaysAFailedCheckpointAtWarn`, seam: the store tests' closed database (`rawDB().Close()` first). On cd91efe1: "expected 1, actual 0" (the line was at DEBUG). Also fixed: a second `SyncStore.Close` did a checkpoint of the closed database, which would now warn although `Engine.Close` is "safe to call more than once"; it is a no-op now (`TestSyncStore_SecondCloseDoesNothing`, failed first with the new WARN). Production closes each engine once (`retireWatchRunner`, `closeRunOnceEngines`), so no clean stop gains a line.
- G: `TestSyncConsole_LeftWorkComesFromTheReportAlone` (`internal/cli/sync_left_work_test.go:32`): a report whose `LeftWork` has `WillRetry` gives `errSyncLeftWork`, and `TestCommandExitCode` (`failure_class_test.go:39`, row "sync left work") maps that to exit 1. Its report sets `LeftForNextSync`; `LeftWork.WillRetry` sums that with `LastPassKeptForRetry` (`report_left_work.go:47`).

### Where the review was wrong
- Nowhere in substance. M1's effect on the path it names is a download shown as failed during the stop plus a retry write that fails on the canceled context, not a retry row or a WARN.

### Checks
- `go build ./...`, `go vet ./...`, tagged vet: pass. gofumpt: clean.
- New and changed tests alone and `-count=20`: ok.
- `./internal/sync/...` and `./internal/multisync/...` without `-race` behind the lock: ok (twice; the second after the store close fix).
- `-race` on the touched tests and their files' other tests, and the reverse-dispatch runs of the three one-shot tests: ok.
- `TestFindTestConventionViolations_RealRepoIsClean`: ok. golangci-lint: `0 issues.` (a first run found bodyclose, gosec G101 on the `Pass` names, nolintlint and QF1008; fixed).
- Every test the touched specs cite exists; no spec line was reflowed.


## Review of the second pass (2026-11-14, about 19:45 to 20:30 PST)

Read-only reviewer on Opus; findings in the session scratch file `R-W31b-findings.md`. Verdict: accept. 0 must fix, 3 should fix, 7 notes. The orchestrator applied the should-fix items and two notes in the same commit:

- Should fix 1, and the second pass's own "seen and not fixed" entry: `sleepWatch` (`internal/sync/observer_remote.go`) armed its backoff with `time.NewTimer` when the watch has a wake channel. It now waits on the run's clock (`o.clock.AfterFunc`), as the wait without a wake channel does. New test `TestWatch_BackoffWithAWakeChannelWaitsOnTheRunClock` (helper `fetcherFailingTwiceThenRecovering`, shared with `TestWatch_BackoffOnError`). Against the wall-clock code the test fails with `"0" is not greater than or equal to "2"` ("each backoff is armed on the run's clock").
- Should fix 2: R-6.8.29's first sentence said every failure is a shutdown only when it is the ended run's own cancellation. That is the rule for an action's failure; an observer loop stops on any failure once its run has ended. The sentence now says both (`spec/requirements/non-functional.md`). The wording was the orchestrator's brief, not the owner's.
- Should fix 3: `TestWatchRuntime_AnObserverWhoseRequestTimedOutIsWarnedAndReturned` called `logObserverError` and `handleObserverExit` by hand. It now goes through `handleWatchObserverError` twice, the second time with no error for the observer that exits clean, so the context the loop passes on is part of what is checked.
- Note 5: the comment on `cancelCauseError` (`errors.go`) and the doc of `TestCancelCauses_AnswerToCanceledAndNameTheirCancel` said "a cancel on an action's path"; both now say "each cancel the worker pool and the one-shot pass make".
- The other "seen and not fixed" entry, `internal/sync/domain_clock.go` (`timerPanics`): the comment said the canceled actions "read as a shutdown". Reworded: the run has ended, so they end as a shutdown.
- Note 6 (the CLI's two panic cancel causes do not answer to `context.Canceled`): not in this commit; a rider on the plan row H8.
- Note 7 (the detached commit bound's expiry is classified `request_timeout`, so a wedged store is shown as a timed-out request): not in this commit; a rider on the plan row W48.
- Notes 4, 8, 9, 10: accepted as they are.

Checks after these edits: the changed tests pass (`TestWatch_Backoff…`, `TestWatchRuntime_AnObserverWhoseRequestTimedOut…`, `TestCancelCauses_…`, `TestWatch_WakeSignal…`); gofumpt clean; the lint over `./internal/sync/...` and the full pre-pr gate are recorded in the merge section.



## Merged as #915 (`3a80f900`, 2026-11-14 about 21:45 PST)

- The first full pre-pr gate failed on the retired-backlog-ids check: this change removed the two tests that were the last Go citations of `B-106`, so its row in `spec/reference/retired-backlog-ids.md` had to go too. The row was removed in the same commit (`2b63751b`) and the four `RealRepoIsClean` source checks of devtool passed before the gate was queued again.
- The second gate passed (lint from cache, short unit tests 4m10s). The pre-merge stage took 264 s and the post-merge stage 140 s; the worktree and branch are removed.
- Installed at 21:48 PST; the watch client restarted on `3a80f900` and reports "Up to date, watching for changes".
- No live proof against a stalled OneDrive is possible on this host (no network fault injection); the in-process stalled server is the proof. The decision is in the final-report list. The live list run for the e2e change that follows runs on a tree holding this change and serves as its regression run.
- Lesson kept in the pipeline rules: run `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/` before queuing any gate.
