# W48 report: a request OneDrive never answers is outage evidence

Commit: `1d13023a` "Count a request OneDrive never answers as outage evidence" on `3a80f900`, branch `fix/timeouts-are-outage-evidence`, 13 files, not pushed.

## What changed
- `domain_graph_error.go`: `classifyServerAnswer` reads a statusless timed-out request as `serverAnswerServiceOutage`; `isRequestTimeout` moved here from the classifier, refuses `errStoreAccessTimedOut`, and also takes network errors that say they timed out (`isNetworkTimeout`: `*url.Error`, `*net.OpError`), so a TLS handshake timeout and a keepalive-ended read count.
- `engine_result_classify.go`: the request-timeout branch returns `retryWithScopeEvidence(deriveScopeKey(r), issueRequestTimeout)`; the 5xx/408/... case of `classifyHTTPResult` uses the same builder. Evidence, window, trial hint are a 504's; the issue stays `request_timeout`.
- `engine_runtime_lifecycle.go`: `detectScope` no longer skips a completion with no status.
- `scope.go`: comments name the new evidence.
- `errors.go`: `errStoreAccessTimedOut`.
- `worker.go`: `runDetachedStoreAccess` (bound made with `context.WithTimeoutCause`, cause joined to the store's error); `commitOutcome` uses it; `workerPool.outcomeCommitTimeout` seam (default `OutcomeCommitTimeout`).
- `executor_transfer.go`: `recordPendingCreate` uses `runDetachedStoreAccess` (deviation 1).
- Tests: `request_timeout_outage_test.go` (new), `detached_context_test.go` (accepts the `Cause` forms), `run_ended_sites_test.go` (reader moved file), `dispatch_order_test.go` (three scenarios at the end).
- Spec: `sync-engine.md` (Verified By row, timeout clause of the shutdown row, `service` paragraph under Scope taxonomy, Detached store writes), `sync.md` R-2.10.3.

## Tests (red on the unchanged tree unless marked)
- `TestClassifyResult_EveryTimedOutRequestIsOutageEvidenceLikeA504`: seven rows (response header, dial, client timeout, TLS handshake, stalled transfer, keepalive read; real stalled/silent servers for the first four) each classify as a 504 does (evidence, detection, trial hint, window of 5) with issue `request_timeout`. Red: every row.
- `TestRunOnce_TimeoutsAgainstOneSilentServiceOpenOneOutageScope`: 5 creates against one stalled server open one `service` scope; the crossing one blocked, the others timed rows with the scope as evidence. Red.
- `TestRunOnce_OneTimedOutRequestOpensNoOutageScope`: one timeout opens none. Green before and after (pin).
- `TestEngineFlow_ProcessTrialDecision_ATimedOutTrialKeepsTheOutageScope`: a timed-out trial keeps the scope and its blocked row. Red (scope discarded).
- `TestEngineFlow_OnlyTheServicesSilenceIsOutageEvidence`: four non-evidence completions (timeout at run end, run's own deadline, run cancel, narrower cancel) plus one live timeout open no scope. Green before and after.
- `TestRunOnce_AnOutageOfTimeoutsIsOneConditionNotOnePerFile`: one activation line, one `service_outage` status group, one watch-log line. Red.
- `TestWorkerPool_ACommitThatOutlastsItsBoundIsAStoreFailure`: a wedged store's commit (its one connection held) is the class of any failed commit and opens no scope. Red with the seam only (requeue, `request_timeout`).

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. gofumpt and goimports on every changed Go file: clean.
- `go test -count=1 ./internal/sync/...` behind the lock: ok (761 s). It may have been built just before the two sentinel texts were reworded; the tests near them (`TestPassWithoutActions_*`, `TestHashAndEmit_*`, `TestHandleWrite_*`, `TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges`) were rerun after: ok.
- `-race` behind the lock: the 168 tests of the touched test files (worker, classifier, scope, lifecycle, pending-create, request-timeout files, the new file) plus `TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_<each of the three new scenarios>`: ok (457 s). A first attempt matched almost the whole package (a top-level `|` in `-run` alternates whole paths) and hit its 30 min deadline with no failure or race reported; rerun with the right pattern. The two tests edited after it, under `-race` again: ok.
- Chaos (`-run Chaos`, plain, once, behind the lock): ok (125 s).
- `ListsEvery|Table|Governs|Exhaustive|Registered` tests and the source checks of `internal/sync`: ok. `RealRepoIsClean` (devtool, four checks): pass, before and after the commit's last edits.
- golangci-lint `./internal/sync/...`: `0 issues.` (`W48-lint.log`; the first run found contextcheck and errorlint in the new test file and gocyclo 16 in `classifyServerAnswer`, fixed by `unansweredServerAnswer`).
- Spec by hand: the seven tests the new Verified By row cites and `TestWorkerPool_ACommitThatOutlastsItsBoundIsAStoreFailure` in Detached store writes exist; R-2.10.3 exists. No new production file, so no `GOVERNS:` change.
- Packages the reviewer may want: `internal/cli` (a statusless timeout now shows as the service scope's condition in status; no CLI code changed).

## Deviations
1. `executor_transfer.go` `recordPendingCreate` is the same detached bound and its failure fails the upload, so after Change 1 its expiry would have been outage evidence; it uses `runDetachedStoreAccess` too (no test of its own: the helper is exercised through the commit test).
2. No separate store branch in `classifyResult`: the service's silence lives in the server-answer table, which also reads `isRequestTimeout`, so the store bound must be refused in that one predicate; the expired bound then reaches `classifyLocalResult`, the class of every failed commit. A second check in `classifyResult` would repeat it.
3. TLS handshake timeouts and keepalive-ended reads, actionable `unexpected_condition` before, are now request timeouts (and evidence): the rule names every way a service does not answer.

## Also fixed
- `errors.go`: two sentinels carried a `sync:` prefix against the file's own rule; texts are now user words.
- `domain_graph_error.go`, `engine_result_classify.go`: four comments narrated history ("used to", "before,", "retried forever"); reworded as the code's reason.

## Seen and not fixed
- `internal/graph` (metadata requests; `transfer_watchdog.go:16-22` says the transports bound only dial, TLS and headers): a metadata response body that stalls after its headers has no bound, so a live but silent server holds the request until the run ends. Fix: arm the stall watchdog, or a body-read deadline, around metadata body reads.
- `internal/sync/domain_graph_error.go` `isRequestTimeout`: an HTTP/2 connection closed by an unanswered ping fails its streams with an error of no timeout type ("http2: client connection lost"), still actionable `unexpected_condition` per file. Fix: needs a typed signal from net/http or graph; matching the text alone is fragile.
- `internal/sync/engine_result_classify.go:36-44`: during a stop, an expired detached store bound (now marked `errStoreAccessTimedOut`) still reads as the cancellation's echo, a shutdown, so a record of done work lost to a wedged store is dropped silently. Fix: rule the store bound out of `isCancellationEcho`'s reading in `classifyResult` and decide what a shutdown does with a failed commit.
- `internal/sync/watch_summary_test.go:10,21,37,81`: the watch summary tests cite R-2.10.47 (status never mutates auth state), not a requirement they prove. Fix: cite R-6.6.30.

## Second pass

Rebased onto `04983d12` with no conflict (`1d13023a` became `bc432892`); build, both vets and the tests of `request_timeout_outage_test.go` and `detached_context_test.go` passed on the rebased tree before any edit. One amended commit, not pushed. Where the first report is wrong: the comments that narrated history were five, not four (three in `domain_graph_error.go`, two in `engine_result_classify.go`); "Seen and not fixed" 2 and 3 were in files this commit touches and are fixed here; entry 4 is fixed here too.

### Items
1. HTTP/2 lost ping. The text is produced at `h2_bundle.go:8674` of go1.26.11 (go.mod's toolchain), in `(*http2ClientConn).closeForLostPing`, called by `healthCheck` (`:8277`) when the ping fails; `go env GOROOT` in the worktree gives the host's go1.27.1, which moved the transport to `net/http/internal/http2/transport.go:1052`, same text. `closeForError` aborts every stream with that one value under `abortOnce` (`:7859-7863`), so no other text reaches a caller from that close: the read loop's later cleanup (`:9618-9626`) cannot replace it, and a body write that sees the closed connection (`:9414`) only ends the write while `RoundTrip` returns `cs.abortErr` (`:8820`), which `net/http` returns unchanged (`transport.go:702-739`) and the client wraps in `*url.Error`. The same text also reaches a response body being read when the connection closes (`:9138`), outside `*url.Error`; that shape is not matched (one text in one place, decided). Red: the new row failed every assertion. Fix: `http2ConnectionLostText` and `isLostHTTP2Connection` in `isNetworkTimeout`.
2. Store bound during a stop. Red: the wedged completion with `RunEnded` classified as a shutdown, and the new stop test got no error from the stop at all. Fix: `classifyResult` does not take an error carrying `errStoreAccessTimedOut` for the echo. What a stopping run then does: classifies it actionable (`unexpected_condition`), writes its retry row on the ended run's context, and `database/sql` refuses at once with `context.Canceled`; `logSuppressedShutdownCompletionError` dropped that to Debug as the echo (shown by taking the second exclusion out: the log held only "suppressed action completion error during shutdown"). The same exclusion went there, keyed on the completion's own error, since the error that site sees is the write's fresh cancel, which does not carry the sentinel: the stop now warns "Could not record the result for <path> while stopping; the next sync will check it again". No row is written and the failed count does not move (recordError runs after the persist).
3. Not done (stop condition). The seam needs a field on `executorConfig` or `executor`, both declared in `executor.go`, a file the managed-folders branch changes. The commit's own files offer none without a context value or a new parameter through `ExecuteUpload` and its test callers. No seam, no test; the wrapper at `executor_transfer.go:636` stays.
4. `plainErrorReason` (`sync_console.go:605-619`) tries `networkErrorReason` and `authErrorMessage`, neither of which matches a store error, then `innermostErrorText` (`:626-642`), which at an error with `Unwrap() []error` follows the last one joined: of `fmt.Errorf("%w: %w", errStoreAccessTimedOut, err)` it printed the innermost of `err`, "context deadline exceeded" (the red test showed exactly that). Fix: the sentinel is joined last. The property held in `internal/sync`: following the completion's error as the console does ends at `errStoreAccessTimedOut`; both `errors.Is` answers asserted.
5. Red: the row counted in the window and was `request_timeout`. Fix: `isNetworkTimeout` refuses a chain holding `*fs.PathError`. The completion is now actionable, no issue type, condition key `unexpected_condition`, retried on the durable recheck curve and warned once per file, the class of any local I/O error `driveops` does not name. Right for one unreadable file; for a whole network mount answering ETIMEDOUT every file warns once and nothing is disk-wide (Seen and not fixed 1).
6. Red: the keepalive-shaped timeout with `RunEnded` counted. Fix: with `RunEnded` the timeout branch returns retry work with `request_timeout`, no evidence, no detection, `trialHintReclassify`. Against item 2: the store row with `RunEnded` passes the echo test, `isRequestTimeout` refuses it, and it ends as a failed commit; every row of both tables ends as its own item says.
7. Three comments reworded as reason (`:290-294`, `:614-621`, `:703`).
8. Row built through `graph.Client.doOnce` from a real sign-in client timeout, wrapped as `tokenBridge.tokenError` and `signInFailure` wrap it. Pin: green before; red with the live timeout's evidence taken out; line put back. R-2.10.3 says "OneDrive or Microsoft sign-in".
9. Commit message rewritten: the trial sentence says when the scope was discarded and when rearmed; five plus three comments make eight; the user-visible changes of this pass are named.
10. to 12. Done as written (R-6.8.29 in its list sentence only; R-2.10.3 says "a trial of the service scope", and its not-evidence list also names a timeout once the sync has ended and a timed-out local read).
13. Pin: red with `extendScopeTrial`'s interval taken out of `NextTrialAt`; line put back.
14. Positive control: zero paths in the window after the non-evidence rows, one after the live timeout; red with the live evidence taken out. The worker test cites R-2.10.3.
15. All four cite R-6.6.30: they hold the one-line-per-condition summary of the watch condition log; none touches account-auth state.

### Tests (new or changed)
- `TestClassifyResult_EveryTimedOutRequestIsOutageEvidenceLikeA504`: rows for a lost HTTP/2 connection (red) and a silent sign-in (pin).
- `TestEngineFlow_OnlyTheServicesSilenceIsOutageEvidence`: rows for a network timeout after the run ended and a timed-out local read (red), positive control.
- `TestEngineFlow_ProcessTrialDecision_ATimedOutTrialKeepsTheOutageScope`: next trial a minute away (pin).
- `TestWorkerPool_ACommitThatOutlastsItsBoundIsAStoreFailure`: console reason and `RunEnded` row (red).
- `TestEngineFlow_ACommitThatOutlastsItsBoundWhileStoppingIsSaid` (new, red at both sites).

### Checks
Commit `a8fffa58` on `04983d12`, 16 files; `git status` clean after it.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l` and `goimports -l` on the eight changed Go files: no output.
- Red first: the five new or changed tests on the unchanged code failed where each item says (lost-connection row; keepalive and local-read rows counted, local read was `request_timeout`; console reason "context deadline exceeded"; `RunEnded` store row a shutdown; stop gave no error). Pins (sign-in row, positive control, next trial) shown red with their lines taken out, lines put back, and a grep confirmed no change was left.
- `go test -count=1 -timeout 60m ./internal/sync/...` behind the lock: ok (595 s).
- `-race`, behind the lock, `-timeout 30m`: the 134 tests of `request_timeout_outage_test.go`, `request_timeout_test.go`, `worker_test.go`, `engine_result_classify_test.go`, `output_noise_test.go`, `watch_summary_test.go`, `detached_context_test.go`, `run_ended_sites_test.go`, `pending_creates_test.go`, `pending_create_lifetime_test.go`, `executor_create_test.go`, `create_answer_lost_test.go`: ok (183 s); `TestReverseDispatch_RunOnceScenariosPass/` with the three scenarios of this commit: ok.
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: four checks pass.
- `go run ./cmd/devtool verify lint` behind the lock: `0 issues.`, actionlint pass (`W48-lint2.log`).
- Spec by hand: `TestEngineFlow_ACommitThatOutlastsItsBoundWhileStoppingIsSaid` and `TestWorkerPool_ACommitThatOutlastsItsBoundIsAStoreFailure` (the names added to `sync-engine.md`) exist; every requirement cited (R-2.10.3, R-2.10.5, R-2.5.3, R-6.3.9, R-6.6.21, R-6.6.30, R-6.8.29) exists.

### Deviations
- Item 3 not done (stop condition: needs `executor.go`).
- Item 2's exclusion at the stop reads the completion's error, not the error the stop's write returned (that one never carries the sentinel).
- R-2.10.3's not-evidence list also names a timeout once the sync has ended and a timed-out local read (items 5 and 6), in the same sentence items 8 and 12 change.

### Seen and not fixed
1. `internal/driveops/local_errors.go:58`: ETIMEDOUT and EAGAIN of a local read are `LocalErrorOther`, so a network filesystem that times out gives one actionable failure per file. Fix: map them as ESTALE is (`LocalErrorBusy`, retried). Another branch's package.
2. `internal/sync/engine_runtime_shutdown.go:17` (and its callers `engine_run_once_results.go:208`, `engine_watch_drain.go:92`): a stopping run applies every completion on the ended run's context, so the retry row of any failure that lands during the stop fails to write at once and is dropped at Debug as the echo; the failure keeps its class but is never recorded. Fix: run a stopping run's completion bookkeeping on a detached, bounded context, as the outcome commit is. Not here: it changes what every stop persists (failure rows, trial outcomes, scope changes) and needs its own tests and design text.



## Review fixes (2026-11-15, after the second pass)

- The second pass's commit was rebased from `04983d12` onto `341d9820` (no conflict) and amended; the commit is now `0889b66c` on `341d9820`, 17 files, not pushed.
- Item 3 of the second pass, which the writer left because it needs `internal/sync/executor.go`, was done in review:
  - `executorConfig` has a field `storeAccessTimeout`; `detachedStoreBound()` returns it, or `OutcomeCommitTimeout` when it is zero. The record of a created item (`recordPendingCreate`), `clearPendingCreateIntent` and `forgetRemovedPlaceholder` take their bound from it.
  - New test `TestUploadNewFile_ARecordThatOutlastsItsBoundIsAStoreFailure` (`request_timeout_outage_test.go`): a store whose `RecordPendingCreate` waits until its context ends, a bound of a few milliseconds through the field, and the assertions that the upload's error holds `context.DeadlineExceeded` and `errStoreAccessTimedOut`, that its decision is not `request_timeout`, and that it adds nothing to the service scope window.
  - Shown red first: with `runDetachedStoreAccess` taken out of `recordPendingCreate` (a plain bounded context in its place) the test failed three assertions (the store-failure sentinel missing; the condition was `request_timeout`; the service window held 1 path). The wrapper was put back and the test passes.
- Checks on the amended tree: `gofumpt -l` clean; `go build ./...`, `go vet ./...` and the tagged vet pass; the new test and the commit-bound tests beside it pass under `-race`; the four `RealRepoIsClean` checks pass; `golangci-lint run --allow-parallel-runners ./internal/sync/...` behind the lock ends `0 issues.`.
- Not run after the amend: the whole `./internal/sync/...` tree (the gate runs it).
- The two "Seen and not fixed" entries of the second pass each became a plan row (a local read that times out is retried as busy; a stopping run records its completions on a detached, bounded context). The lost-connection text reaching a body read outside `*url.Error` is a rider of the row that bounds every metadata body read.


## Review fixes (second review)

- Commit `8050c4a3` on `341d9820` (amended in place from `0889b66c`), 17 files, not pushed; `git status` shows only the untracked `.testdata` link.
- 1 done: R-2.10.3 (`spec/requirements/sync.md:130`) and R-6.8.29 (`spec/requirements/non-functional.md:175`) now name "a request whose HTTP/2 connection was closed for an unanswered ping before its answer began".
  - R-6.8.29's retry of a change-list page checked against `deltaPageRetryDelay` (`internal/sync/observer_remote.go:1037-1040`): every shape the row now lists is retried there. The request-shaped HTTP/2 loss and the dial, TLS and header timeouts reach it inside `*url.Error`, which implements `net.Error`; an expired request deadline is `context.DeadlineExceeded`, whose type answers `Timeout()` and `Temporary()`; a keepalive read keeps its `*net.OpError` through `*graph.ResponseReadError` (`internal/graph/response_body.go:23`); a stalled transfer cannot be a change-list page (the stall watchdog is built only in `download.go` and `upload_transfer.go`). So the row promises no retry the code does not make.
- 2 done: `TestUploadNewFile_ARecordThatOutlastsItsBoundIsAStoreFailure` is cited in the Verified By cell of `spec/design/sync-engine.md:113` and beside the worker test in "Detached store writes" (:1494-1495).
- 3 claim checked and holds: `validatingReaderAt.ReadAt` (`internal/driveops/transfer_upload.go:70-73`) wraps every read failure with `fmt.Errorf`; graph's `progressReader` and `io.SectionReader` pass it on unchanged; net/http returns it as the request's error (HTTP/1 `transport.go:2257-2263`, HTTP/2 `h2_bundle.go:9320-9331` of go1.26.11); `url.Error.Timeout()` type-asserts only the error it holds (`net/url/url.go:42-47`), and `*fmt.wrapError` has no `Timeout`. Reworded the comment of `localReadTimedOutError` (the shape of a reader that returns the file's error unwrapped, which a sync upload does not build), the matching sentence of `isNetworkTimeout`'s comment (`internal/sync/domain_graph_error.go:197-199`), and the commit-message clause. No code change.
- 4: nothing to do here (plan row).
- 5: nothing to do here (cost recorded, no change asked).
- 6 done: both comments of `internal/sync/executor.go` say the reason (:359-360, :760-762), and the sentence after the first one ("was recorded", :363-364) too, which narrated history the same way. Comment text only.
- 7 done: `internal/sync/engine_runtime_shutdown.go:57-60` reads as the finding proposes.
- 8 partly done: R-6.3.9 added to `TestUploadNewFile_ARecordThatOutlastsItsBoundIsAStoreFailure`, which now proves the record carries the executor's deadline. R-6.6.21 kept on `TestEngineFlow_ACommitThatOutlastsItsBoundWhileStoppingIsSaid`: R-2.5.3 promises only that the next startup rediscovers interrupted work, not that the stop says anything; the warning is the other side of R-6.6.21's "A clean shutdown shall produce no warnings", and the sibling test of the same Warn (`TestEngineFlow_SuppressedShutdownCompletionError_QuietForCancellationAndRepeatFatal`, `output_noise_test.go:93`) cites R-6.6.21 alone. Both rows stay.
- 9: nothing to do here (no change asked).
- 10: nothing to do here (plan row).
- 11 done: `unansweringRecordStore` waits only on a context whose deadline is no further away than the bound it is given, and fails at once on any other (no deadline, or one past the bound); the test gives it `workerTestCommitBound`. No sleep and no window: the check is the context's own deadline. Shown red: with `detachedStoreBound` made to ignore `storeAccessTimeout` the test failed in 0.14 s ("the record's context ends in 29.99999342s, past the executor's store bound of 20ms"); the change was put back and `git diff` shows `executor_transfer.go` unchanged.
- 12: nothing to do here (textual merge risk only).

### Checks
- `gofumpt -l` on the four changed Go files: no output.
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- The nine tests of `request_timeout_outage_test.go` by name in one `go test -count=1 -timeout 30m -run` on `./internal/sync/` (`go test -list` with the same pattern names all nine): ok. The same with `-race` behind the lock: ok (340 s with the wait for the lock).
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: the four checks pass.
- `golangci-lint run --allow-parallel-runners ./internal/sync/...` behind the lock: `0 issues.` (`W48-lint3.log`).
- Spec: the one test name added to `sync-engine.md` exists; no requirement number was added or changed in spec text.

### Seen and not fixed
none


## Merged as #920

- Squash-merged 2026-11-15 about 11:35 PST as `29e28200` on main.
- Gates: local pre-PR gate passed; PR CI and review-thread gate passed;
  post-merge stage passed ("merge, cleanup, and post-merge CI
  interpretation passed").
- Installed on this host at 11:55 PST (`onedrive-go --version` names
  `29e28200`).
- Not proven live: OneDrive going silent cannot be produced on demand
  and network fault injection is not allowed on this host. The proof is
  the test suite's fake transport. Owed only if a real outage occurs:
  read the watch log for one activation line and one `service_outage`
  condition.
