# H8 report: outer silent failures (`fix/outer-silent-failures`)

Commit `a665e13c` on parent `6421bf98` (origin/main at worktree creation and again before install; #942/#944 included). Not pushed. Installed as the watch client (`onedrive-go-install`, version `a665e13c`).

## Definition of Ready (summary; no separate DoR file, by the caller's instruction)
- A: the requirement is `AuthRequirementReason` on the account record in `catalog.json`. `AddDrive` already writes that file in the login's transaction (config lock, then catalog lock); the old clear was a second `UpdateCatalogForDataDir` (catalog lock only) that looked the account up by email and only Warn-logged a failure. Folding the clear into `recordLogin` needs no format or lock-order change. Other clears: CLI proof (`authProofRecorder`), sync startup proof (`engine.go`), logout. A failed enrollment already restores the token and the catalog, so the requirement stays with the old token.
- B: `perf.RoundTripper` is built 4 times in `graphtransport/profiles.go` and once in `multisync/perf_control_test.go`, all with `Inner` set. Nothing relied on the nil fallback.
- C: one token per signed-in account, no cap. Per token: token load (maybe a refresh), `/me/drives`, `/me/drive` on failure, and for business a site search plus one `SiteDrives` per site (10, or 999 with `--all`), in sequence. Shared discovery: accounts in sequence, item lookups bounded at 5. `drive list` returns on `ctx.Err()` before printing, so stopping launches on cancel changes no output.
- D: one product caller of `NewSession` (`root.go`). `Complete` runs on every path except `reportMainPanic` (a panic on the command goroutine), where the periodic goroutine ran until the process exited (in tests it leaked).
- E: fault shown. A request through `InteractiveMetadataClient` (real `http.Transport`, local server) that the signal handler's panic interrupts returns `Get ...: request canceled: internal error in the signal handler: ...`, and `errors.Is(err, context.Canceled)` is false. A plain `http.Transport` returns the cause bare. In `internal/sync` (a scratch test, deleted afterwards) `classifyResult` with `RunEnded` gives class `actionable` with persistence (a durable record), where a plain stop gives `shutdown`. After the fix it gives `shutdown`. `WithCancelCause` sites handed a recovered panic: `cli/signal.go`, `cli/sync_notify.go` (`failWatch`), `perf/logging.go`, and `graph/transfer_watchdog.go`. The last fails its one transfer by design (R-6.3.10), so it is left as it is.
- Re-check against main: all five items were still open at `6421bf98`. #942/#944 touched none of these sites.

## What changed, per item
- A, done: `config/catalog_lifecycle.go` `recordLogin` writes the account without a requirement. `config/drive_enroll.go` plans it (`endsSignInRequirement`), reports `AddDriveResult.EndedSignInRequirement` and logs `cleared catalog auth requirement` with source `login`. `cli/auth_flow.go` drops the separate clear. Beyond: the clear now targets the signed-in account's record, not the first record with its email.
- B, done: `perf/http.go` returns `the performance transport has no inner transport to send the request through`, closes the body, sends and records nothing. It stays a plain struct: the zero value is always constructible in Go, so a constructor could not remove the nil state.
- C, done: `shared_discovery.go` has `discoveryConcurrency = 5` and one helper, `runBounded`, which owns the semaphore, the ctx stop, the wait and the panic boundary (`where` and `failed` callbacks). Both drive discovery and the shared-item lookups use it. The panic boundary, result order and recorded failures are unchanged.
- D, done: `runPeriodic` ends on the session context. `Complete` cancels, waits, then writes the summary once. `stopCh` is gone.
- E, done: `panicguard.AsCancelCause` (type `stopCauseError`) answers to `context.Canceled` and, through `Unwrap`, to `ErrPanic` and `Stack`. It is used at the three CLI and perf sites. `perf.resultForError` puts the panic first, so the summary reads `error`. `commandPanicCause` and the exit status 1 are unchanged. `internal/sync`'s `cancelCauseError` is untouched.
- Docs: `spec/design/config.md` (login transition and `recordLogin`), `spec/design/cli.md` (helper panics and the evidence table), R-6.3.10 (the interrupted work ends as at a stop).

## Tests (each seen failing first, except the last)
- `TestRoundTripper_WithoutAnInnerTransportRefusesTheRequest` (perf): no error, request sent.
- `TestSession_PeriodicUpdatesEndWithTheCommandContext` (perf): goroutine alive 5 s after cancel.
- `TestDiscoverAvailableDrives_ReadsAtMostTheDiscoveryBoundOfAccountsAtOnce` (cli, testing/synctest bubble): 8 at once, want 5.
- `TestAsCancelCause_AnswersToThePanicAndToTheStop` (panicguard): compile failure (new API).
- `TestShutdownContext_HandlerPanicStopsTheRequestInFlight`, `TestRunSyncDaemon_WatchdogPanicStopsTheWorkInFlight` (cli), `TestSession_UpdatePanicStopsTheRequestInFlight` (perf): not `context.Canceled`. The perf summary assertion also failed (`canceled`) until `resultForError` was fixed.
- `TestAddDrive_LoginEndsTheSignInRequirementInItsOneCatalogWrite` (config): the requirement was kept.
- `TestLogin_EndsTheSignInRequirementOfTheAccountItSignedIn` (cli): with a same-email twin, the old clear cleared the twin and kept the signed-in account's requirement.
- `TestDiscoverAvailableDrives_APanicIsThatAccountsDegradedNotice` (cli): guards the boundary that already worked. It passed before; it had no test until now.
- Tier: the stress-member rule names `internal/sync` tests only, and none of the new tests are there. `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` passes. None of them is exhaustive.

## Checks
- `go build ./...` ok. `go vet ./...` ok. `go vet -tags=e2e,e2e_full,integration,stress ./...` ok.
- `go test` of cli, perf, panicguard, graphtransport and config, plain and with `-race` behind the lock: all ok (run twice, the second time after the lint fixes).
- `-run RealRepoIsClean ./internal/devtool/`: 4 PASS.
- `PATH=~/go/bin:$PATH go run ./cmd/devtool verify lint` behind the lock: status pass, 0 issues. A first run found 3 issues (errname, goconst, gocritic hugeParam), now fixed. Two `// Validates:` lines in tests gained `R-2.10.47` while the second lint ran; `RealRepoIsClean` was run again afterwards and passed.
- Every test name added to a spec file was checked by hand: all 7 exist.

## Live proof
Only `login` makes the change visible to a user, and it needs an interactive sign-in. Not run.

One read-only `drive list --json` against the test account with the installed binary: exit 0, the configured drive and one available shared folder listed, no account needing sign-in or degraded. This covers the bounded discovery and the perf transport live.

## Deviations
- C: once the context ends, discovery starts no more accounts. Before, every account was started and each recorded a failure. `drive list` returns the cancellation before printing, so the output is the same.
- The DoR is in this report, not in a separate `H8-dor.md`, and there is no lint log file in the notes repo. Both follow the caller's instructions.

## Also fixed
- `drive_catalog.go` `appendAvailableCatalogDrive`: a drive with no account ID (a document library) was dropped silently. It is now logged at Debug.
- `shared_discovery.go`: the dead `degraded` flag is gone. Both branches returned the same notice.
- `perf/logging.go`: doc comments for `Session`, `Collector`, `SetLogger` and `SnapshotAttrs`. The stale `mu` comment ("Start and Stop") is fixed.
- `config/validated_state.go`: the comment saying `ClearAccountAuthRequirement` was the single writer is fixed. `config/drive_enroll.go`: the `AddDrive` doc's "writes nothing" now notes a login's catalog refresh.
- R-6.8.8 and R-6.8.14 no longer say sync uses "raw `http.DefaultTransport`". Sync uses the tuned transport without the retry layer.

## Seen and not fixed (4)
1. `internal/cli/auth_health.go:182-208` (CLI proof and logout clears), `internal/config/validated_state.go:137-171` (`MarkAccountAuthRequired` and `ClearAccountAuthRequirement`), and the sync engine's mark: these address the account by email (`AccountByEmail`, first sorted record). With two accounts sharing an email, a 401 or proof for one marks or clears the other. Fix: key on the account's canonical ID. The callers include `internal/sync`.
2. `internal/cli/root.go:885-898` `reportMainPanic` never calls `PerfSession.Complete`, so a command ended by a panic on its own goroutine logs no performance summary. Its updates now stop with the context. Fix: complete the session with the panic's error there.
3. `internal/cli/auth_flow.go:208-212` `cleanupPendingToken` only Warn-logs a pending token it could not remove after a failed login. This is minor: the login already fails with its own error, and the next login overwrites the fixed path. Fix: join it into the login's returned error.
4. `internal/graph/transfer_watchdog.go:89`: the stall watchdog's panic cause fails that one transfer, as R-6.3.10 says. This is not a fault, but it is the only `WithCancelCause` panic site left without `AsCancelCause`. Recorded so it is not mistaken for a missed site.

## For the reviewer
`internal/multisync` builds `perf.RoundTripper` in a test (`Inner` set) and was not run here. `internal/sync` and `internal/driveops` were not run either. Declarations the tests rely on: `driveops.SessionRuntime.TokenSourceFn`, `synctest.InChild`/`RunInChild`, `seedCatalogAccount`/`loadCatalogAccount`, `runWithin`, and `writeNotifyTestConfig`/`shortSocketPath`.
