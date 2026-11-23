# W58 report: every response body's silence is bounded, a lost connection is typed

Worktree `~/src/onedrive-go-wt/metadata-body-reads-bounded`, branch
`fix/metadata-body-reads-bounded`, one commit `77f97043` on `origin/main`
`0e31e63c` (rebased cleanly from `e248aae5`), not pushed. The first builder
left the whole change uncommitted; it was reviewed against the brief and
committed as it stood. DoR: `W58-dor.md`.

## What changed (one line per file)

- `internal/graph/response_body.go`: `responseBody` replaces `markedResponseBody`. Each Read arms a timer of the stall window, its return stops it. A timer that fires closes the body, and the read fails with `ErrResponseStalled`. Also holds `ErrConnectionLost`, `http2ConnectionLostText` and `markLostConnection`, the one place that reads the text.
- `internal/graph/client_preauth.go`: `dispatchRequestWith`, the one place every response comes back, wraps each body and passes round-trip failures through `markLostConnection`.
- `internal/graph/transfer_watchdog.go`: `defaultStallTimeout` is the one window. `stallWindow()`. `explainStall` is shared by both stalls. `reader` hands a `responseBody` to the watchdog alone (`leaveBoundToTransfer`).
- `internal/graph/client.go`: the field `transferStallTimeout` is now `stallTimeout` (the seam the tests use).
- `internal/sync/domain_graph_error.go`: `isRequestTimeout` reads `graph.ErrResponseStalled` and `graph.ErrConnectionLost`. The text constant and `isLostHTTP2Connection` are gone from sync.
- `internal/sync/request_timeout_outage_test.go`: the table of timed-out requests gains a stalled body, a lost connection on the round trip and one on a body read, all built from the types. The fake transport that faked the text is gone.
- `internal/graph/quirks_test.go`: `TestHandleChunkResponse_Unexpected2xx` now goes through `UploadChunk` against a fake server answering 204. A stale comment about deleted tests is removed.
- `internal/graph/service_read_only_test.go`, `client_test.go`, `error_evidence_test.go`: `Validates:` lines for R-6.7.35 and R-6.7.30.
- `spec/requirements/non-functional.md`: new R-6.7.35 (an inner `serviceReadOnly` is not token-propagation lag and reads as the provider read-only window). R-6.7.30 now says what an error-body read does. R-6.8.20 and R-6.8.29 name the stalled response and the lost connection.
- `spec/design/graph-client.md`: the Transfers paragraph (a download body is left to the watchdog) and the Error Handling paragraph (what bounds a body, the lost connection). `spec/design/sync-engine.md`: the request-never-answered row names both types. `spec/design/system.md`: the read-only row cites R-6.7.35.

## Tests added, and what each was seen failing on

- `TestGetItem_AStalledBodyFailsWithinTheStallWindow`: headers, then nothing, fails within the window with `ErrResponseStalled` and `*ResponseReadError`, not a context error. Seen failing before the change: it ran to the 10 s safety context and got `context deadline exceeded`.
- `TestGetItem_ABodyThatKeepsMovingIsNotFailed`: a body that sends one byte at a time, with gaps of window/20, completes though it takes longer than the window. Seen failing on a mutation that bounded total time instead of silence ("response stalled: no data moved for 400ms").
- `TestGetItem_AStalledErrorBodyReturnsItsAnswer`: a stalled 503 body returns its `*GraphError` (status, placeholder message, `ErrServerError`) within the window. Seen failing before the change: 10.0005 s, the safety context ended it.
- `TestDeleteItem_AStalledDrainFailsWithinTheStallWindow`: a stalled drain fails with `ErrResponseStalled`. Seen failing before the change: `context deadline exceeded` after 10 s.
- `TestGetItem_AConnectionLostBeforeTheAnswerIsTyped` (new `connection_lost_test.go`): the real HTTP/2 client against a TLS server that answers no ping, with 5 ms ping bounds and no sleep, about 40 ms. The round trip carries `ErrConnectionLost` inside `*url.Error`. Seen failing before the change: the chain held only the bare library text. This test also pins the standard library's text.
- `TestGetItem_AConnectionLostDuringTheBodyIsTyped`: the same server answers `:status 200` and no body. The body read carries `ErrConnectionLost` inside `*ResponseReadError`, outside any `*url.Error`. Seen failing before the change: the bare text. It pins the text for the body-read path.
- `TestStallWatchdog_ABodyItReadsStartsNoTimerOfItsOwn`: a `responseBody` read through `stallWatchdog.reader` starts no timer of its own. Seen failing before the hand-over: 2 reads had started timers.
- `TestClassifyResult_EveryTimedOutRequestIsOutageEvidenceLikeA504` (three new rows): seen failing with graph's types in place and sync not reading them. The scope was `0` instead of the service scope, and the window 0 instead of 5.
- Renamed: `TestMarkedResponseBody_PassesEOFThrough` is now `TestResponseBody_PassesEOFThrough`.

## Live proof

Not needed: neither silence can be provoked on OneDrive (decision (g)). No e2e test was added and no live check was run.

## Deviations

- A stalled error body returns its answer's `*GraphError` (the 64 KiB read fails, and `buildError` keeps the placeholder message), not `ErrResponseStalled`. The read is bounded the same way. The status already makes it retryable, and sync reads a 5xx as outage evidence already.

## Also fixed

- `transfer_watchdog.go`: the `ErrTransferStalled` comment described history ("held a worker slot forever"). It now says what the bound prevents.
- `quirks_test.go`: the false comment of `TestHandleChunkResponse_Unexpected2xx` ("we can't make doPreAuthRetry return 204") is gone, because the test now does it. A dead comment about deleted tests is removed too.

## Not fixed

- `internal/cli/error_presentation.go:125`: a body abandoned for silence (`graph.ErrResponseStalled`, which has no `Timeout()`) is not said as "did not answer in time". Over HTTP/1 its closed-body text matches no case, so it falls through to the generic text. Fix: add `errors.Is(err, graph.ErrResponseStalled)` to the timeout case, with a test.
- `internal/cli/error_presentation.go:172`: the lost connection is still known there by the broad `"http2: "` text. Fix: read `graph.ErrConnectionLost` first, and keep the text only for stream errors, which net/http does not export.

## Unfinished

None. Packages the reviewer should test beyond `internal/graph` and `internal/sync`: `internal/driveops` (a stalled download must stay `ErrTransferStalled` and keep its partial, `transfer_download.go:677`) and `internal/cli` (it presents the errors of every response body).

## Checks (on the rebased tree)

- After the rebase, `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...` both passed (0 lines).
- `go test -count=1 -timeout 30m ./internal/graph/... ./internal/graphtransport/... ./internal/sync/...` behind the lock: ok (graph 19.2 s, graphtransport 0.0 s, sync 369.9 s; log `S/W58-tests.log`).
- The same with `-race -timeout 90m` (sync as `./internal/sync/`) behind the lock: ok (graph 26.0 s, graphtransport 1.0 s, sync 3743.3 s; `S/W58-race.log`).
- First `go run ./cmd/devtool verify lint` behind the lock: 1 issue. testifylint require-error flagged `connection_lost_test.go:88`: an `assert.NoError` in a helper that the linter cannot see runs on the server goroutine. Fix: the read deadline is now set inside the server's goroutine, and the helper takes no `t`. After the fix the lost-connection tests passed 20 times under `-race`, and `./internal/graph/...` passed with and without `-race`. The commit was amended, and the second `devtool verify lint` returned `0 issues.`, actionlint pass (`S/W58-lint2.log`).
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: 4 PASS.
- I checked by hand that every test name the spec diff cites exists in the tree.
- Install: `onedrive-go-install` on the worktree reports `onedrive-go version 77f97043`. `CLAUDE.md` and `CHANGELOG.md` in the agent repo record it.

## Review fixes

On `77f97043` against `S/R-W58-findings.md`; amended as `29edcac1`, still one commit on `0e31e63c`. No finding was wrong; every one is fixed.

- 1 (blocking), both drains. `internal/retry/transport.go`: an answer the transport retries is closed unread (`drainAndCloseBody` is gone). Over HTTP/2 the close resets only that stream; over HTTP/1 it costs the connection. `internal/graph/client_preauth.go`: `dispatchRequestWith` sends through `closingRedirects(client)`. That copy's `CheckRedirect` runs the client's own policy, or net/http's limit of 10 when the client has none, then closes the redirect's body unread before net/http's 2 KB drain. A redirect the policy keeps (`http.ErrUseLastResponse`, the `/content` first leg) comes back open and goes through the stall window. Text made true: the doc comment of `dispatchRequestWith`, the Error Handling paragraph of `graph-client.md`, a new bullet in `retry.md`, and R-6.8.20 ("an answer the system does not read, one it retries or a redirect it follows, shall be closed unread").
- 2: R-6.8.20 now says that an error answer whose body stalls is reported as that answer.
- 3: `TestGetItem_AStalledBodyFailsWithinTheStallWindowOverHTTP2` (httptest with `EnableHTTP2` and `StartTLS`; the handler fails a request that is not HTTP/2). The stalled server helpers share `startStalledBodyServer` and `stallAfterHeaders`.
- 4: the unanswering server answers each PING (ACK with the same payload) until it has written the response HEADERS, then answers nothing. The two ping bounds are split: 5 ms of quiet before a ping, and 100 ms for its answer, which is the margin an answered ping has. Each test still takes about 0.13 s.
- 5: `TestGetItem_AStalledErrorBodyReturnsItsAnswer` asserts `elapsed < stalledBodySafetyNet/2`, and that the one `failed to read response body` record names `ErrResponseStalled`.
- 6, log only: the timer's recover now only logs through `panicguard.Recovered`, and `abandon` takes no cause. The comment says that a read the close ended reports the stall, and that a close that panicked counts as done. `explainStall`'s comment limits the panic case to the transfer watchdog. Pin: `TestResponseBody_AStallWhoseClosePanicsReportsTheStall`.
- 7: a new bullet, "after the headers, `graph` bounds every response body's silence (Error Handling)", and "Transport stall detection up to the response headers belongs to `graphtransport`".
- 8: the `Implements:` line of `graph-client.md` lists R-6.7.30, R-6.7.35 and R-6.8.20 as `[verified]`.
- Answer 5 (the two `internal/cli` items): not fixed, as decided. They are recorded as the new PLAN.md row W98. W60 is the sync classifier's family of unreachable failures and does not cover CLI wording.
- Also: `TestGetItem_RedirectsStopAtTheLimitOfNetHTTP` pins the redirect limit `closingRedirects` keeps (it passes on `77f97043` too).

Red on `77f97043` (new tests, old product code; `S/w58fix/red-retry.log`, `S/w58fix/red-graph.log`):
- `--- FAIL: TestRetryTransport_ARetriedAnswerIsClosedUnread (2.00s)`: "context deadline exceeded", "the request waited on the retried answer's body".
- `--- FAIL: TestGetItem_ARetriedAnswerWhoseBodyStallsIsNotWaitedOn (10.00s)`: "10.000343539s" is not less than "5s".
- `--- FAIL: TestGetItem_ARedirectWhoseBodyStallsIsFollowed (10.01s)`: `dispatching request: Get ".../moved": context deadline exceeded`.
- Pins that pass on `77f97043` (no behaviour change): the HTTP/2 stall test (0.17 s), the panicking close (0.00 s), the stalled error body (0.20 s), and both lost-connection tests (0.12 s, 0.13 s).

Green after (`S/w58fix/green-retry.log`, `S/w58fix/green-graph.log`):
- `--- PASS: TestRetryTransport_ARetriedAnswerIsClosedUnread (0.00s)`
- `--- PASS: TestGetItem_ARetriedAnswerWhoseBodyStallsIsNotWaitedOn (0.22s)`
- `--- PASS: TestGetItem_ARedirectWhoseBodyStallsIsFollowed (0.01s)`
- `--- PASS: TestGetItem_RedirectsStopAtTheLimitOfNetHTTP (0.01s)`, plus the pins above, all PASS.

Mutation run (`b.window` replaced by `time.Hour` in `startRead`, then restored; `S/w58fix/mutation-window.log`):
- `--- FAIL: TestGetItem_AStalledBodyFailsWithinTheStallWindowOverHTTP2 (10.01s)`: the chain ends in "context deadline exceeded", not `ErrResponseStalled`.
- `--- FAIL: TestGetItem_AStalledErrorBodyReturnsItsAnswer (10.01s)`: "10.010032539s" is not less than "5s"; the record reads "graph: reading the response: context deadline exceeded".

Checks on the final tree:
- `go build ./...`: ok. `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...`: 0 lines.
- `go test -count=1 -timeout 30m ./internal/graph/... ./internal/retry/... ./internal/graphtransport/... ./internal/driveops/...`: ok (graph 18.0 s, retry 0.0 s, graphtransport 0.0 s, driveops 5.6 s).
- `go test -race -count=1 -timeout 30m ./internal/graph/... ./internal/retry/...` behind the lock: ok (graph 25.6 s, retry 1.1 s).
- `go test -race -count=500 -cpu 1 -run TestGetItem_AConnectionLost ./internal/graph/`: ok, 107.4 s. It ran beside a parallel load, `-race -count=500 -cpu 2` of the stall, retry, redirect and lost-connection tests: ok, 436.3 s. An earlier run beside the package race run: ok, 88.1 s.
- `go test -count=1 -v -run RealRepoIsClean ./internal/devtool/`: 4 PASS. `./internal/panicguard/...`: ok.
- `PATH=~/go/bin:$PATH go run ./cmd/devtool verify lint` behind the lock: `0 issues.`, actionlint pass (`S/w58fix/lint.log`).
- I checked by hand that every test the spec diff cites exists.
- Plan-id and process-word sweep of the diff: empty.
- Not run: a full `devtool verify`, live e2e (no live check is possible), and the `internal/sync` and `internal/cli` tests (neither package changed; the cli run was skipped because another pre-PR gate held the host).
- Install: `onedrive-go-install` reports `onedrive-go version 29edcac1`. It replaces `cdb02def`, the move-into-new-folder branch, in the watch client.


## Merge

Merged as #940, squash commit `9e5498a5` on main, 2026-11-16 21:03 PST; installed from the main checkout by 21:07 (`onedrive-go version 9e5498a5`).

- Rebased onto main `52053271` (the stress-lane declarations): clean, still one commit. The branch adds no test in `internal/sync`, so no stress declaration was needed; `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` passed.
- The second review's one should-fix: `closingRedirects` sets `next.Response.Body = http.NoBody` after its close, so a redirect's body is closed once and net/http's own read and close reach only the empty body. New test `TestGetItem_TheBodyOfARedirectIsClosedOnceAndNeverRead` (a fake transport whose redirect body counts its calls): red before the line (2 closes, 1 read), green after. The comment of `closingRedirects` and the graph-client spec paragraph say it, and cite the test.
- Note N4 fixed (one line): R-6.8.20's clause is "an answer the Graph client does not read", since sign-in requests do not pass through the client. Notes N2 (identity compare of `http.ErrUseLastResponse`) and N3 (HTTP/2 variants of the retry and redirect tests) were left: neither is one line; the PR says the closes are tested over HTTP/1.1 only.
- Gates: `go vet` plain and with all tags 0 lines; lint of `./internal/graph/...` 0 issues; `./internal/graph/` and `./internal/retry/` ok; RealRepoIsClean 4 PASS; start stage ok (the nightly passed, nothing claimed); pre-pr passed; pre-merge passed (no review threads); post-merge passed (merge, cleanup and main's CI), and it removed the worktree.
- No live run: neither silence can be provoked on OneDrive (decision (g)).
