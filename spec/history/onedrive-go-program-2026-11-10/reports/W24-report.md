# W24 report: a request's deadline reaches the token refresh and the retry wait
Commit `653e1aa1` (second pass, amended from `a076d13b`) on `fix/deadline-reaches-refresh-and-backoff`, one commit on `86674d33`, 54 files, not pushed.
## What changed
- `graph/auth_token.go`: `tokenBridge` caches the token itself (no `ReuseTokenSource`); one `tokenRefresh` at a time on a bridge-owned goroutine under `WithoutCancel(starter ctx)`; waiters bounded by their own ctx (cause kept); save via `OnTokenChange` before publish; panic recovered; `TokenSourceFromPath(path, authHTTP, logger)`.
- `graph/auth.go`, `auth_device.go`, `auth_browser.go`: `checkAuthClient` (nil or no `Timeout` refused) + `signInContext`; login flows take `authHTTP`, returned source keeps no ctx.
- `graph/client.go`, `client_auth.go`: `Token(ctx)`, `ForceRefresh(ctx)`; not-sent doc. `client_preauth.go`: `*retry.ThrottledError` -> `errRequestNotSent`+`ErrThrottled`. `upload_transfer.go`: `doRawUpload` marks unsent failures.
- `retry/transport.go`: one rule `waitFits` at the three waits; `ThrottledError`; the throttle deadline is read only by unexported `throttleDeadline`. `retry/throttle_gate.go`: dead `Wait` removed.
- `driveops/session.go`: `TokenSourceFn`, `BootstrapClient`, `InteractiveSession`, `SyncSession`, `SharedTargetClients` take no ctx; one `tokenSourceLoad` per sign-in file read outside `r.mu`; `BootstrapMeta(tokenPath)` per sign-in file. One-token call-site edits in 8 `internal/cli` files and 2 `internal/multisync` files.
- `graph/auth.go`: `SignInRequestError` + `signInFailure` (refresh, device code, poll, exchange). `cli/error_presentation.go`: `failedService`; `cli/sync_console.go`: `innermostErrorText` follows a multi-`%w` through its last error. `cli/auth_login.go`: discovery keyed by the pending sign-in file. `cli/root.go`: CLIContext comments.
- `cli/status_live.go`: answer before budget in the reason; a throttled/5xx storage request is reported.
- Docs: graph-client.md (Authentication, 401, Runtime Ownership, Verified By), retry.md, cli.md (60 s sentence gone), drive-transfers.md, R-6.6.8/R-6.8.8/R-6.8.27/R-6.8.28.
## Token(ctx): implementers and callers (12 implementers, 6 call sites)
| who | file | context passed | why |
| --- | --- | --- | --- |
| `tokenBridge.Token/ForceRefresh` | graph/auth_token.go | waits on caller ctx; refresh on `WithoutCancel(starter ctx)` + sign-in `Timeout` | a shared refresh must not die with one caller |
| `doOnce` | graph/client_auth.go | request ctx | the token is for that request |
| `retryUnauthorized` (Token, ForceRefresh) | graph/client_auth.go | the 401'd request's ctx | same request's retry |
| `doRawUpload` | graph/upload_transfer.go | upload request ctx | same |
| `ForceRefresh` -> `Token` | graph/auth_token.go | its own ctx | inside the interval |
| `runAuthPreflightAttempt` | e2e/auth_preflight_e2e_test.go | attempt ctx (`authPreflightTimeout`) | same request |
| 11 fakes | graph client_test (3), client_auth_refresh_test (2), item_create_test; cli drive_test, mv_test, status_live_test; driveops session_test; multisync orchestrator_test | take ctx, no refresh | — |
No `context.Background()` and no stored context remain on the refresh path (the bridge has no ctx field).
## Callers that special-case a deadline error from a retried request
- cli/status_live.go `statusLiveUnansweredReason`: budget cause -> "did not answer"; now OneDrive's answer first (throttle, "temporary server problem", refusal), then budget. `TestStatus_SaysWhyOneDriveGaveNoDetails`.
- cli/status_live.go `statusConfiguredDriveOverlay` storage loop: ctx ended -> reason, else silent; a 5xx/429 now arrives early, so it is reported too (later drives still asked). `TestStatus_AStorageServerErrorIsNoted`.
- cli/error_presentation.go `commandErrorText` (DeadlineExceeded -> "timed out"), cli/failure_class.go `classifyCommandError` (-> ClassShutdown), cli/put.go `isFatalUploadWalkError`: file commands carry no deadline, so unchanged; a refresh cut by Ctrl-C now reads Canceled (shutdown/stop walk: right); `ThrottledError` reads as `ErrThrottled` ("limiting requests").
- graph/item_create.go `createOutcomeUnknown`, upload_transfer.go final fragment: key on `errRequestNotSent`; a refresh cut by ctx and a throttle refusal are not sent -> no outcome-unknown (`TestDo_AThrottleWaitPastTheDeadlineIsNotSent`).
- sync/engine_result_classify.go `classifyResult` (and observer_remote.go:665/1010, engine_watch.go:141, engine_mount_root.go:437): sync clients are raw (`SyncClientSet`, no RetryTransport), so no 429/503 replaces a context error there; it is classified as before (throttle -> block scope, 503 -> requeue). The refresh now ends with the request ctx and keeps its cause: stall watchdog -> `ErrTransferStalled` -> requeue (checked first), shutdown -> Canceled -> shutdown.
- Detached-deadline cleanups (cli/replace_destination.go restore, driveops transfer_session.go placeholder/cancel, graph item_create.go/upload_transfer.go cleanups) treat every error alike; they now refresh under their own detached ctx instead of a possibly canceled command ctx.
## Tests added (each `// Validates:`)
- `TestTokenSource_ARefreshEndsWithTheCallersContext`: waiter returns at its deadline, refresh still open.
- `TestTokenSource_ARefreshOneCallerAbandonedStillServesTheNext`: cause kept, one request, rotated token saved.
- `TestTokenSource_AnAbandonedRefreshWritesNoTokenFile`: refresh nobody waits for ends at sign-in timeout, file unchanged.
- `TestTokenSource_ConcurrentCallersShareOneRefresh`; `TestTokenSource_ARefreshThatPanicsFailsItsCallers`.
- `TestDo_ATokenRefreshCutByTheContextIsNotSent`; `TestDo_AThrottleWaitPastTheDeadlineIsNotSent`; `TestSignInFlows_AuthClientWithoutATimeoutIsAnError`.
- `TestRetryTransport_AWaitPastTheDeadlineReturnsTheAnswerAtOnce` (429, 503, network, gate), `..._IsNotAnnounced`, `TestRetryTransport_NoDeadlineWaitsAsBefore`.
- `TestStatus_ASlowTokenRefreshIsCutByTheBudget`, `TestStatus_AStorageServerErrorIsNoted`; 503 row of `TestStatus_SaysWhyOneDriveGaveNoDetails` now "temporary server problem"; `TestSimpleUploadToItem_TokenError` asserts not-sent.
- Second pass: `TestSessionRuntime_OneSignInsThrottleDoesNotReachAnother`, `..._BootstrapClientsOfOneSignInShareItsThrottle` (pin), `..._ATokenLoadDoesNotHoldTheRuntime`, `..._ConcurrentCallersOfOneSignInLoadItOnce`, `TestStatus_OneAccountsThrottleLeavesTheOtherAlone` (two runs), `TestSignInRequestError_MarksEveryRequestToTheSignInService` (refresh via `do`, device code, poll, exchange), `..._NotOnAGraphRequest`, `TestCommandErrorText_ASignInFailureNamesMicrosoftSignIn` (timeout, TLS, dropped, lookup; refresh in `ls`), `..._AnExpiredSignInStillSaysSignInAgain` (text + exit 77), `..._AGraphTimeoutStillNamesOneDrive`, `TestStatus_ASignInConnectionFailureIsNamedInTheNote`; `TestRetryTransport_ThrottleCoordination`/`_429SetsThrottleDeadline` and `TestDo_AThrottleWaitPastTheDeadlineIsNotSent` now reach the throttle through a real 429.
- Red first: retry tests (waits/notices), token tests (2 min on the old bridge), status 503 rows and slow-refresh (30 s guard); second pass: throttle and locked-load tests, status two-account test (shared client), sign-in mark and text tests, all seen failing.
## Checks
Second pass: build ok; `go vet ./...` ok; vet `e2e e2e_full` on e2e, graph, devtool ok; `go test` retry, graph, graphtransport, driveops, cli, multisync ok; `-race -run` added tests ok; `ListsEvery|Table|Governs|Exhaustive|Registered` ok; lint over cli, driveops, retry, graph 0 issues (`W24-lint.log`; a first run found 4 in the new tests, fixed, rerun once). First pass also: vet `integration`, tokenfile tests.
## Deviations
- Interface: session constructors, `TokenSourceFn`, `TokenSourceFromPath` lose their ctx (a kept-nothing ctx would be a lie; unparam flags the unexported ones).
- A sign-in client without `Timeout` is now refused (it is what ends an unwaited refresh).
- A refresh is not canceled when its last waiter leaves (runs to the sign-in timeout, so a rotated refresh token is still saved), per the brief over decision 1's literal wording.
- New exported `retry.ThrottledError` for the throttle gate case.
- status storage loop reports throttled/5xx storage (was silent).
- Second pass: `BootstrapMeta` stays, keyed by token path: login's account discovery (`discoverAccount`, now given the pending sign-in file) is a product caller the second-pass message did not name; the no-mount branch of `interactiveClientsForMount` is gone (`InteractiveSession` refuses a nil mount first). `UpdateConfig` never cleared the token cache, so the per-sign-in clients are cleared with `FlushTokenCache` only. `innermostErrorText` changed (sync_console.go) so the TLS text names the real cause past the not-sent mark.
## Also fixed
`retryBackoff` unused `_ SleepFunc`; ghost "architecture.md §7.4" and "typically http.DefaultTransport" comments; dead `ThrottleGate.Wait`; stale ReuseTokenSource comment in `doOnce`; `tokenError` logs once per refresh, not per waiter; `writeStatusTestUser` always-same param; R-6.8.8 "MaxAttempts: 5" -> 6.
- driveops/session.go: one bootstrap metadata client per sign-in file, so one account's 429 no longer throttles another's status or discovery.
- driveops/session.go: the sign-in file is read once, outside `r.mu`; concurrent callers wait for that load; a failed load is not kept.
- graph `SignInRequestError` + cli `failedService`: sign-in timeouts, TLS failures, dropped connections and lookups say "Microsoft sign-in", in commands and status notes.
- cli/root.go: CLIContext doc moved from `cliSeams` to `CLIContext`; Env, GraphBaseURL, Cfg, Runtime, CfgPath comments say what they hold and when they are nil/zero.
- retry: `SetThrottleDeadline`/`ThrottleDeadline` removed; tests reach the throttle through a real 429 or an injected `ThrottleGate`.
## Seen and not fixed (1)
- internal/sync/engine_result_classify.go:60: every transport timeout answers `errors.Is(err, context.DeadlineExceeded)` and is read as shutdown; its own change in the sync lane, left here.
## Unfinished / for the reviewer
Nothing unfinished. Please run the whole `internal/cli` and `internal/sync` packages. Declarations my tests lean on from other files: `writeExpiredSignIn`, `newSignInStandIn`, `writeSignInJSON`, `signInFormLimit`, `stalledSignInTimeout` (auth_http_client_test.go); `newStatusGraphServer`, `blockUntilRequestEnds`, `newStatusLiveRun` (status_live_test.go); `makeResponse`, `roundTripFunc`, `testPolicy`, `warnMessages`, `closeTestResponse` (transport_test.go); second pass: `newFileCommandTestContext` (file_command_independence_test.go), `offlineHost`, `testDeviceAuthResponse`, `noopDisplay` (auth_test.go), `writeTestResponsef`, `discardLogger` (driveops tests). Live proof log lines are unchanged.
## Live proof steps (test account only, isolated tree)
1. `S=$(mktemp -d)`; `go -C ~/src/onedrive-go-wt/deadline-reaches-refresh build -o $S/onedrive-go .`
2. `export XDG_DATA_HOME=$S/data XDG_CONFIG_HOME=$S/config XDG_CACHE_HOME=$S/cache`; `mkdir -p $XDG_DATA_HOME/onedrive-go $XDG_CONFIG_HOME/onedrive-go $XDG_CACHE_HOME`
3. Copy from `~/src/onedrive-go/.testdata/`: `config.toml` -> `$XDG_CONFIG_HOME/onedrive-go/`, `catalog.json` and `token_personal_<test account>.json` -> `$XDG_DATA_HOME/onedrive-go/`; `chmod 600` the token.
4. Expire the access token: `jq '.token.expiry = "2000-01-01T00:00:00Z"' T > T.new`, `mv T.new T`, `chmod 600 T` (T = the copied token file).
5. `$S/onedrive-go --debug status`, expire again, then `$S/onedrive-go --debug ls /`.
6. Look for, in order: `loaded saved token ... expired=true`; one `refreshing access token`; `refreshed sign-in path=... expiry=<about 1 h ahead>` (the save); `token acquired ... valid=true`; then `request succeeded`. After it, `jq .token.expiry T` is about an hour ahead and `.token.access_token` changed. Must not appear: `stopped waiting for a token refresh`, `token refresh panicked`, any status Note. A second run shows `expired=false` and no `refreshing access token`.

## Review (first pass, 2026-11-13 01:14, on `a076d13b`)

Checked: the whole product diff and the spec diff read; no plan ids or process words in the diff; the 15 added Validates citations; one timing wait in the new tests (a guard, not a deadline); `go build ./...`; the whole non-race suite on this commit (every package ok, `internal/sync` 522 s, no failure). R-6.8.8's corrected attempt count checked against `internal/retry/named.go` (`transportAttempts = 6`). `AuthClient()` has a 60 s `Timeout` and no retrying transport, so the refusal of a sign-in client without a timeout holds in production.

Deviations, all five accepted:

1. Session constructors, `TokenSourceFn` and `TokenSourceFromPath` without a context. Accepted.
2. A sign-in client without a `Timeout` is refused. Accepted: it is what bounds a refresh nobody waits for.
3. Decision (mine): a refresh is the bridge's, not a caller's. A caller's context bounds only its own wait and its cause is kept; the refresh runs detached to the sign-in client's timeout, so a rotated refresh token is still saved and serves the next caller. This replaces the literal "one request under that context" of the plan entry's first decision.
4. The exported `retry.ThrottledError`. Accepted.
5. `status` reports the first throttled or failed storage request. Accepted.

"Seen and not fixed", each decided:

- The sync classifier that reads a deadline error as shutdown: real, and wider than the writer said. A probe with a real `http.Client` against a stalled test server shows every transport timeout (response header, dial, client `Timeout`) answers true to `errors.Is(err, context.DeadlineExceeded)`. Nine sites read it as "the run is ending". Planned as its own change in the sync lane (W31 in PLAN.md).
- The bootstrap client's one throttle deadline for every account: second pass (item 1).
- The token file read under the runtime lock: second pass (item 2).
- The sign-in endpoint's failures named as OneDrive: second pass (item 3), through an error type from `internal/graph`, not a host-name comparison in the presentation layer.
- The two `CLIContext` comments that describe phases: second pass (item 4).
- `SetThrottleDeadline` and `ThrottleDeadline` exported for tests only: second pass (item 5). This change's own tests are their users, so it is fixed here and not left to the test-only-code sweep.

For stacking above the panic-boundary change (recorded in the stack notes): `redeem`'s own recover hands its value to `panicguard.Recovered` and the live-proof "must not appear" string changes with it; `status_live.go` keeps the boundary and takes this change's shapes; that branch's `TokenSource` fake and `TokenSourceFn` override take this change's signatures.

Second pass sent 2026-11-13 01:15 (five items, each test first). Live proof is the reviewer's, after it, when the service answers.

## Review (second pass, 2026-11-13 03:30, on `2442a960`)

The writer's second pass was `653e1aa1`; `2442a960` is that commit with one review fix of mine amended in.

Checked on `653e1aa1`: `go build ./...`; the whole non-race suites of retry, graph, graphtransport, driveops, cli and multisync; `go vet ./...` and the tagged vet; the plan-id check against `86674d33`; every product and test diff read; the spec word diff read; the Validates comments.

**Accepted as delivered**: a bootstrap metadata client per sign-in file (`BootstrapMeta(tokenPath)`), so one account's 429 no longer delays every account; the sign-in file read outside the runtime's lock, with callers for the same file waiting on the first load and no goroutine started; `graph.SignInRequestError` and the CLI's "Microsoft sign-in" wording for a timeout, TLS failure, dropped connection, lookup and proxy; `innermostErrorText` following an error that wraps several; the `CLIContext` doc comment back above its struct; the throttle accessors removed from the retry transport and their tests moved to real 429s.

**Deviation accepted**: `BootstrapMeta` stays exported and keyed by token path, because login's account discovery is a product caller for the pending sign-in file; `FlushTokenCache` clears the per-sign-in clients too.

**Finding withdrawn**: I first noted that no test proves a failed load is retried by the next caller. It exists already: `TestSessionRuntime_BootstrapClientKeepsNotLoggedIn` in `session_test.go`.

**Review fix (mine, failing first)**: a load whose loader panicked never closed its `done` channel and left its entry in the map, so every caller already waiting, and every later caller for that sign-in file, blocked for good. `getOrCreateTokenSource` now settles the load in a deferred step: it counts as failed until the loader answers, so a panic releases the waiters with `errTokenSourceLoadAbandoned`, leaves no entry, and goes on to the caller that ran the load (whose own boundary handles it). Tests: `TestSessionRuntime_ATokenLoadThatPanicsHoldsNoLaterCaller` and `TestSessionRuntime_ATokenLoadThatPanicsReleasesItsWaiters` (the second in a `testing/synctest` bubble). Both failed on `653e1aa1` (the first after its 30 s guard, the second with the waiter still blocked). After the fix: driveops and cli suites pass, the session tests pass three times under `-race`, gofumpt is clean, the driveops lint reports 0 issues. `spec/design/drive-transfers.md` gained one sentence citing the two tests.

**Left as it is**: `innermostErrorText` reaches the multi-wrap through `errors.As` rather than a direct assertion; the effect is the same for every error the product builds.

**Seen and not fixed (1)**: the sync result classifier reading a transport timeout as shutdown. It has its own planned change (the transport-timeout entry in the plan), written after this one is stacked.

**For stacking above the panic-boundary change**: `redeem`'s own recover hands its value to `panicguard.Recovered`; the status test's `panickingTokenSource` and its `TokenSourceFn` override take this change's shapes; `status_live.go` keeps the boundary and this change's storage loop; the new driveops test imports the standard `testing/synctest` as `gosynctest`.

**Still mine**: the live proof (steps above), once the service answers.

## Stack pass

Second writer, 2026-11-13. `3a29c75a` (on `8d6f7d46`) against the reviewed `2442a960` (on `86674d33`), then moved onto `origin/main`.

### Step 1: where `3a29c75a` differs from `2442a960` beyond context (13 rows, 0 defects)
Every difference comes from #897 (panic boundary), which also replaced the status command's duration budget with the constant `statusLiveBudget` behind the `statusLiveBudgetContext` seam. #898 and the squash-merged forms of #891 and #893 show only as context (R-6.6.19, `sync_console.go`, `auth_test.go`, the browser callback row and comment).

| # | file | what differs | cause | both kept |
| --- | --- | --- | --- | --- |
| 1 | cli/status_live.go | `statusLiveUnansweredReason` loses its `budget` parameter and names `statusLiveBudget`; the change's order (OneDrive's answer before the budget) stays | #897 budget constant | yes |
| 2 | cli/status_live_test.go consts | `statusLiveTestRetryBudget` (500 ms) gone; `statusLiveTestRetryDeadline` (10 min) and `statusLiveTestRetryAfter` ("3600") added; `statusLiveTestUnanswered` is main's "...within 10 s" | #897 seam replaced per-test durations | yes |
| 3 | status_live_test.go helper | `statusLiveTestBudget` gains an optional `deadline` (`newStatusLiveTestDeadlineBudget`) so the retry rule has a deadline to compare with; spend unchanged | #897 | yes |
| 4 | `TestStatus_AStorageServerErrorIsNoted`, `TestStatus_OneAccountsThrottleLeavesTheOtherAlone` | deadline budget plus Retry-After 3600 in place of the 500 ms budget; same assertions | #897 | yes |
| 5 | `TestStatus_ASlowTokenRefreshIsCutByTheBudget` | the sign-in handler spends the budget in place of a 1 s budget; note "...within 10 s." | #897 | yes |
| 6 | `TestStatus_ASignInConnectionFailureIsNamedInTheNote` | `newStatusLiveTestBudget()` in place of the 1 min budget | #897 | yes |
| 7 | `TestStatus_SaysWhyOneDriveGaveNoDetails` | main's row "a server error still failing when the budget ends" (spent, no deadline: "did not answer") kept; the change's row "a server error whose retry would outlast the budget" added beside it (`retryAfter`, `deadline` fields); doc comment joins both | #897 | yes |
| 8 | `TestStatus_APanicAskingOneAccount...` (#897's test) | `writeStatusTestUser(t, w)`, `panickingTokenSource.Token(context.Context)`, `TokenSourceFn(tokenPath, logger)`: this change's shapes | #897 | yes |
| 9 | graph/auth_token.go `redeem` | recover hands the value to `panicguard.Recovered(..., "while refreshing the sign-in", ...)`, error wraps `panicguard.ErrPanic`; the request moved to `requestToken` so `finish` runs once on each path; `runtime/debug` gone | #897 | yes (waiters released, process goes on) |
| 10 | graph/auth_token_test.go `TestTokenSource_ARefreshThatPanicsFailsItsCallers` | `ErrorIs(panicguard.ErrPanic)` in place of the "panicked" text; asserts panicguard's log line and that the next caller is not held; Validates adds R-6.3.10 | #897 (repointed) | yes |
| 11 | spec/design/cli.md (status budget) | #897's seam sentences follow the change's "budget covers the whole request" sentence; one clause names row 3's deadline budget | #897 | yes |
| 12 | spec/design/graph-client.md (refresh paragraph) | panic recovered through `panicguard.Recovered`, error wraps `ErrPanic`, R-6.3.10 | #897 | yes |
| 13 | graph-client.md Runtime Ownership | "recovers its own panics" becomes "hands a panic to `panicguard.Recovered` and gives the error to its waiters" | #897 | yes |

Line 111's four items in the tree after the rebase:
- `redeem` -> `panicguard.Recovered`: internal/graph/auth_token.go:301-307.
- `panickingTokenSource` and its `TokenSourceFn` override in this change's shapes: internal/cli/status_live_test.go:647 and :676.
- status keeps the boundary and the storage loop: internal/cli/status_live.go:117 (`panicguard.Recovered` per account) and `statusConfiguredDriveOverlay`'s `incomplete` (throttle/5xx reported, later drives asked).
- `gosynctest "testing/synctest"`: internal/driveops/session_signin_test.go:14.

### Step 2: onto `origin/main` (`bd2711b0`: #899, #900)
`git rebase --onto origin/main 8d6f7d46` applied with no conflict (0 rows). #899's deletions touch nothing this commit's code or tests call (build and every tagged vet pass), so no test was repointed.

### The commit was amended by another writer during the checks
The rebase gave `952d9390`. At 2026-11-14 01:38 someone else in this worktree amended it to `e2b4913f`; I did not make that amend. It edits `internal/cli/error_presentation.go`: the new `urlErrorTimedOut` reads a timeout from the whole chain of a `*url.Error`, not only from the error it holds. It adds one row, "timeout a round tripper wrapped", to `TestCommandErrorText_EveryTransportFailureIsPlain`, and one sentence to the commit message. This goes beyond what the rebase needed (a product change), so whether it stays is the owner's call. It answers the race failure below.

### Checks
- On `952d9390`: `go build ./...` ok; `go vet ./...` ok; `go vet -tags 'e2e e2e_full'` e2e, graph, devtool ok; `go vet -tags integration ./...` ok; `go test -count=1` retry, graph, driveops, multisync, cli ok (each whole, one run each).
- `go test -race -count=1` cli, multisync, graph/..., retry/..., driveops/... behind the lock: first run FAIL in one subtest, `TestCommandErrorText_ASignInFailureNamesMicrosoftSignIn/a_refresh_that_timed_out` ("The operation timed out" in place of "Microsoft sign-in did not answer in time"). Cause: the test's `stalledSignIn` transport returns when its request's context deadline fires. For a transport net/http does not know, `http.Client` sets that deadline and a separate timer flag (`setRequestCancel`), and only the flag marks the `*url.Error` as a timeout, so under load the error can come back unmarked. This is timing in the change's own test, not the rebase. The rerun under the lock: ok (its cli build came after the edit at 01:16, so it most likely ran the amended tree).
- The change's 30 tests, `-race -count=10 -run`, behind the lock, on `e2b4913f`: ok. (A first attempt was killed by SIGTERM from outside while it waited for the lock; nothing had run.)
- Lint (`golangci-lint run` over e2e, cli, driveops, graph, multisync, retry) behind the lock, on `e2b4913f`: 0 issues (`W24-lint.log`).
- On `e2b4913f` again: `go build ./...`, `go vet ./...` and both tagged vets ok; `go test -count=1 ./internal/cli/...` ok.
- Spec citations: all 55 test names the commit's spec lines cite exist on `e2b4913f`.
- No plan ids or process words in the commit (diff and message).

### Result
`e2b4913f` on `bd2711b0` (origin/main), one commit, not pushed. Rows: 13 from step 1, 0 defects; 0 conflicts in step 2; no test repointed. For the live proof: the panic line is now panicguard's "Internal error while refreshing the sign-in", no longer "token refresh panicked".


## Review of the stack pass (lead, 2026-11-14)

- Accepted: 13 rows, 0 defects, one commit on `bd2711b0`.
- **Live proof passed** at about 01:10 Pacific on `952d9390` (`w24live.py`: isolated tree, test account, the private token copy's expiry rewritten, nothing of the token printed). With the access token expired, `--debug status` and `--debug ls /` each loaded it as expired, refreshed once, saved the sign-in, and then succeeded; the five expected lines were in order, no "stopped waiting for a token refresh", no panic, no warning or error line, no status Note. A third run with the token fresh did not refresh and left the token file unchanged.
- **Review fix (mine, failing test first).** The writer's race run over `internal/cli` failed one subtest of `TestCommandErrorText_ASignInFailureNamesMicrosoftSignIn` once: the timeout text came out as the generic one, without the service. Behind it is a product defect: `transportErrorReason` asked only the `url.Error`'s own `Timeout()`, so a timeout that reaches it wrapped by a round tripper (the `url.Error` holds an error that wraps `context.DeadlineExceeded` instead of being a timeout itself) lost the service name. Added the row "timeout a round tripper wrapped" to `TestCommandErrorText_EveryTransportFailureIsPlain`, saw it fail, then fixed it with `urlErrorTimedOut` (the `url.Error`'s `Timeout()` or a timeout anywhere in its chain). Both tests then ran 200 times under `-race`: ok (236 s).
- **Not verified**: that the one failed run hit exactly this shape (it is inferred from its output, the generic timeout text), and why a timeout would arrive wrapped only on some runs. The defect itself is proven by the failing row, and the fix reads both shapes.
- Commit amended (`e2b4913f`); its message says the fourth bullet's last sentence.


On the amended commit `e2b4913f` (2026-11-14, about 02:20): the writer's race rerun over cli, multisync, graph, retry and driveops is green (`W24s-race2.log`: five `ok` lines), and the live proof ran again and passed with the same shape as on `952d9390` (`W24-live2.log`: runs 1 and 2 load the sign-in as expired, refresh once, save and succeed with no warning, no note and no abandoned wait; run 3 with a fresh token does not refresh and leaves the file unchanged; the private tree is removed). The live proof is owed once more on the commit that is merged, after the rebase onto the main that holds the two branches ahead of it.



## Merged as #905 (`2b1c5afa`, 2026-11-14 07:33 PST)

- Rebased onto main at `7d7bc06b` as one commit `3bee6427`. The rebase left one test token source with the old signature (the create-mark chaos test in `internal/sync`); the first pre-pr gate failed on it in the lint typecheck. The second gate failed in the test convention check on two direct `t.Errorf` calls in `internal/driveops/session_signin_test.go`, which became `assert.Failf`. The third gate passed (6m28s, classified reruns: none).
- Two rules came out of this and are in every dispatch now: after every rebase build and vet the whole tree, with and without the e2e tags; and run `TestFindTestConventionViolations_RealRepoIsClean` before a gate.
- Live proof on `3bee6427` (the merged content), test account, private tree: with the access token of a private copy of the sign-in expired, `--debug status` and `--debug ls /` each loaded it as expired, refreshed once, saved and succeeded with no warning, no note and no abandoned wait; a third run with a fresh token did not refresh and left the file unchanged; the tree was removed.
- Pre-merge passed in 260 s; post-merge passed; installed `2b1c5afa`; the watch client reports "Up to date, watching for changes", 19 files.
- Left for another change, named in the PR under "Not in this change": the sync engine reads every transport timeout as shutdown (`engine_result_classify.go`).
