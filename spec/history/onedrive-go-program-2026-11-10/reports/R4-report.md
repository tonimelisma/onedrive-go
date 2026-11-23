# R4 report: token saves are errors (B6); refresh across processes (B7, stopped)

Branch `refactor/token-saves-and-refresh`, commit `49642621` on base `a8ad2f27`, pushed; no PR, no install.

## B6: done
- `go.mod`: the `replace` of `golang.org/x/oauth2` and its comment were removed. The build uses upstream v0.30.0 (the version `go.mod` already required) with no code change: the repo uses only upstream names (Config, Token, Endpoint, RetrieveError, DeviceAuthResponse, HTTPClient, GenerateVerifier, VerifierOption, S256ChallengeOption, AccessTypeOffline). `go.sum` swaps the fork's two lines for upstream's two. Nothing else in `go.mod` changed.
- `internal/graph/auth_persist.go` (new): `tokenSaver`, the one writer of a sign-in file per source. It writes only a token whose expiry is later than what the file holds, never an older one. A failed write stays pending and is retried at the next `Token` call. A successful write clears the failure and logs `refreshed sign-in` (Info, once per saved token). Also new: the exported `ErrTokenNotSaved`, `TokenSaveError(ts)`, and the optional `SaveError()` interface, so fakes need no change.
- `auth_token.go`: `OnTokenChange` removed. `oauthConfig()` takes no arguments. `newTokenBridge` takes the token path. `redeem` saves before `finish`. On this tree every path that yields a new token goes through `redeem`: `Token(ctx)` (the caller's deadline still reaches the refresh) and `ForceRefresh`. The analysis' separate deadline path and restart function no longer exist. `auth.go`, `auth_device.go` and `auth_browser.go` were adapted.
- `driveops/session.go`: added `TokenSaveFailure{TokenPath, Err}` and `SessionRuntime.TokenSaveFailures()`, sorted by file and covering only sources loaded since the last flush. I added the path struct beyond the brief so the status line can name files without parsing error text.
- CLI, the single place for P4: `mainWithWriters` calls `withTokenSaveFailure(cc.tokenSaveFailure(), err)`. That covers every runtime built through `CLIContext.newSessionRuntime`: the root's two, one-shot `sync` (`sync_flow.go`) and the watch (`sync_runtime.go`). A save failure replaces a nil or canceled result (exit 1) or joins an existing failure, which keeps its own status. A failure that a later save cleared changes nothing.
- systemd: the unit has `Restart=on-failure` and `RestartPreventExitStatus=77 78`. On exit 1 after a stop, the unit shows failed and is not restarted, and a start still works. A restart request is a stop then a start. A reload is a SIGHUP, so the daemon does not exit.
- Daemon: the reconcile tick (the tree has no heartbeat tick) calls `reportUnsavedSignIns`. It logs one ERROR per distinct failure set through `logOnChange`, which now generalises `warnOnChange`. When the set of files changes it emits the new `WatchSignInSaveChanged` lifecycle event. `syncNotifier` appends the files to every ready status while the set is non-empty. `WatchLifecyclePhase` and `ReloadOutcome` are now `uint8` so the event stays under the 128-byte `hugeParam` limit.
- Docs: `graph-client.md` has a new paragraph. I removed its fork bullet and the accepted-risk paragraphs, changed the `OnTokenChange` sentences, added `auth_persist.go` to GOVERNS and updated runtime ownership. I also updated `cli.md` (exit status rows and paragraph, the status event), `sync-control-plane.md` (the tick), `system.md` (two fork mentions), `developer-onboarding.md` (script row), `docs/scripting.md` (exit 1 row) and `docs/troubleshooting.md` (new section). `scripts/check-oauth2-fork.sh` is deleted. New requirement R-6.8.26 (the row's reserved number, free between .25 and .27). R-6.8.27, the deadline requirement, is unchanged.

Log texts for the live check:
- Info: `refreshed sign-in` with `path` and `expiry`.
- Error (watch): `Couldn't save a refreshed sign-in; still syncing, and trying again at the next request` with `error="couldn't save the refreshed sign-in to <path>; the next run may ask you to sign in again: tokenfile: writing token file: ..."`.
- Command (stderr, exit 1): the same error text.
- Status: `Watching <roots>; couldn't save the refreshed sign-in to <path>`.

## Lock extraction (commit 2 `2d100c12`, after a coordinator decision)
- New leaf package `internal/filelock` (`TryLock`, `Unlock`, `Wait(ctx)`, `IsUnsupported`; depguard leaf rule; governed by `system.md`). The config/catalog lock, the control-socket owner lock and the engine's folder/store locks now take their flock through it.
- Semantics unchanged: all three are still non-blocking try-locks, with the same lock-file paths and the same release points. The config lock keeps its own poll loop and sleep/flock seams. The owner lock keeps its inode recheck and its remove-on-release. The engine keeps warn-and-continue on unsupported filesystems (`isUnsupportedLockErrno` moved to `filelock.IsUnsupported`).
- About 30 lines changed in the three users, plus about 180 new. Their lock tests pass.

## B7 (commit 3 `e779803a`): done
- `tokenfile/lock.go`: `AcquireLock(ctx, tokenPath)` takes `<token>.json.lock` beside the file through `filelock.Wait`. A wait that ends returns `ErrLockWaitEnded` ("another onedrive-go process is refreshing the sign-in"). A filesystem that cannot lock returns `ErrLockUnsupported` with a no-op release. Also added: `WithLock`, `Install` (atomic write with directory sync, then the pending file removed) and `RemoveLock`. The depguard rule was renamed to `tokenfile-only-fsroot-and-filelock`.
- `graph/auth_refresh.go`: every refresh runs `refreshShared` under the lock:
  - The wait is the smaller of `signInLockWait` (90 s, cross-referenced with graphtransport's 60 s auth timeout) and what is left of the starting caller's deadline.
  - The file is read under the lock. A missing file means "sign-in required": no request, nothing written, and any pending save is dropped.
  - A differing valid sign-in in the file is adopted, logging Info "found a newer sign-in in the file and used it". After a 401, the rule is "access token differs from the rejected one".
  - Otherwise the file's refresh token is redeemed and the result saved under the lock.
  - On `invalid_grant` the file is read again and a different refresh token in it is tried once.
- `tokenSaver.retry` takes the lock and writes only while the file exists.
- `graph.Logout(ctx, …)` deletes under the lock. Logout tells a running watch also when only the token was removed. A purge removes the lock file.
- Login uses `tokenfile.Install`, and its undo runs under `WithLock`. `moveToken` and its two tests are gone.
- Requirements: new R-6.8.30, and R-3.1.3 now includes the reload. Docs: a new section "Refresh Across Processes" in `graph-client.md`, updates to `cli.md` and `system.md`.
- Deviations from the brief:
  - The unsupported-filesystem Warn is logged once per token source, not once per process. A per-process once would need a package global, which the linter forbids.
  - The refresh-level two-process test uses two sources with separate lock descriptors in one process. True cross-process exclusion is proven separately by `TestAcquireLock_ExcludesAnotherProcess`, which re-execs the test binary.
  - `TestRefresh_SaveFailureKeepsTokenAndRetriesSave` was not written. `TestPersistingTokenSource_SaveFailureIsReportedAndRetried` already covers it through the new locked path.
  - No separate cli "refresh racing logout" test was written. `TestRefresh_MissingFileIsSignInRequiredAndFileStaysAbsent` covers it: logout, then a refresh and a pending retried save, and the file stays absent.
  - A test finding: oauth2's auth-style probe sends a refused refresh twice. The test counts refusals, not requests.
- B7 tests (all `R-6.8.30`):
  - In graph: `TestRefresh_TwoSourcesOneFile_SecondAdoptsFirstsToken` (the probe; it failed on base), `TestRefresh_RotatedTokenOnDiskIsUsedInsteadOfStaleMemory`, `TestRefresh_InvalidGrantRereadsAndRetriesOnce`, `TestRefresh_MissingFileIsSignInRequiredAndFileStaysAbsent`, `TestForceRefresh_AdoptsNewerTokenFromDisk`, `TestRefresh_LockWaitIsBounded`, `TestRefresh_LockWaitEndsWithTheCallersDeadline`.
  - In tokenfile: `TestAcquireLock_ExcludesAnotherHandleUntilReleased`, `TestAcquireLock_ExcludesAnotherProcess`, `TestRemoveLock_RemovesTheLockFile`.
  - Elsewhere: `TestDiscoverCIDFiles_SkipsTheSignInLockFile`, `TestLogin_InstallsTokenUnderLock`, `TestLogout_NotifiesDaemon`, and three filelock tests.
- e2e: `go vet -tags 'e2e e2e_full'` passes. No e2e test calls the changed functions directly, so no live test to name.

## Tests (each `// Validates: R-6.8.26`)
- `TestPersistingTokenSource_SavesARefreshedToken`: a refresh writes the file before the caller gets the token, with one Info line.
- `TestPersistingTokenSource_SaveFailureIsReportedAndRetried`: the request still succeeds. The error names the file and the cause and holds no token material. The write is retried at the next use, and a successful write clears the error.
- `TestPersistingTokenSource_NeverWritesAnOlderToken`: newer then older is handed over, and the older is never written, including while the newer one's write is failing.
- `TestTokenBridge_ForceRefreshIsSaved` and `TestTokenBridge_TokenUnderACallersDeadlineIsSaved`: both refresh paths save.
- `TestTokenSaveError_SourceWithoutAFileReportsNone`.
- `TestSessionRuntime_TokenSaveErrors`: sorted per file, ignores sources that save fine, cleared by a flush.
- Four new `TestCommandExitCode` cases, including a watch stopped with the error set (exit 1) and a sign-in failure beside it (77).
- `TestCLIContext_TokenSaveFailureIsTheCommandsError`: two runtimes, one report, a cleared failure means exit 0.
- `TestRunWatch_UnsavedSignInIsLoggedOncePerDistinctFailure`.
- `TestSyncNotifier_StatusNamesAnUnsavedSignInWhileItIsSet`.
- The three callback tests (`TestOAuthConfig_OnTokenChange`, `TestTokenPath_UsesDriveName`, `TestOAuthConfig_OnTokenChangeError`) were replaced and are cited nowhere.
- The new tests failed before the change in two ways: on the base they do not compile, and with the `save` call removed the four bridge-level tests fail. The saver-only test was not checked that way.
- Test names deviate from the brief: the `TestPersistingTokenSource_*` names were kept, but the type is `tokenSaver`, because the tree has no upstream reuse source to wrap. The deadline test is named after `Token`.

## Checks
- Pass: `go build ./...`, `go vet` on the touched trees, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/...`, `go mod verify`.
- Pass: `go test -p 1 -count=1` over `graph`, `tokenfile`, `driveops`, `multisync` and `cli`, run again after the lint fixes.
- Pass: a GOVERNS, cited-test and routing check, run as a throwaway devtool test (deleted).
- Not run, per memory rules: `-race`, the 20x stress runs, `devtool verify`. I did not vet `./internal/devtool/...` with the tags.
- Lint: B6 (graph, driveops, multisync, cli, tokenfile) found 17 issues, all fixed, then 0. After the extraction and B7, a run over graph, tokenfile, cli, config, graphtransport, filelock, multisync, driveops and sync found 5 issues, all fixed. The re-lint of the packages with fixes reports 0 issues (`R4-lint.log`).
- After the extraction and B7, tests pass over cli, graph, tokenfile, driveops, config, multisync, filelock and graphtransport. The sync lock tests also pass (focused run). The GOVERNS and cited-test probe passes. The cross-process lock test passes 10 runs in a row.
- For the reviewer: run `-race` over the same trees.

## Also fixed
- `logOnChange` now logs at the right level without a fresh context; this removed 6 `contextcheck` hits.
- The unused `tokenPath` parameter of `testOAuthConfig` and `testAuthCodeConfig` is gone.
- The obsolete fork check script is deleted.

## Not fixed (in PARKED.md)
- R4-p2: `internal/cli/root.go`, PersistentPreRunE and `initializeResolvedCLIContext`. A data command builds two runtimes, so it can hold two sources for one sign-in file. The fix is to build one runtime and give it the resolved holder.
- (R4-p1 is resolved by commit 3.) R4-p3: the login source's saver points at the pending path, which `installLoginToken` (`internal/cli/auth_flow.go:124`) then moves. This belongs to B7's P5.

## Review fixes (R4a, commit 4 `ce694ac4`)
- One ordering rule, refresh-token lineage, replaces the expiry comparison. The saver records two refresh tokens: `fileRefresh`, which the file holds, and `pendingBase`, which the file held when an unsaved chain began.
  - It writes only while the file still holds the start of its chain.
  - A file holding anything else was written by another process, so it is newer, and the unsaved chain is dropped.
  - Save, retry and refresh all apply this rule.
- Each finding:
  - F1: a successor that expires before the file's token is written.
  - F2: a refresh redeems the in-memory refresh token whenever the file is this source's or the start of its chain.
  - F3: `retry` reads the file under the lock and never writes over a newer one.
  - F4: `tokenfile.Install` reads the previous contents under its own lock and returns the undo. The cli helpers `readManagedFileIfExists` and `writeManagedFile` became dead code and were removed.
- Tests:
  - New: `TestForceRefresh_ShorterLifetimeIsStillSaved` (probe A), `TestRefresh_PersistentSaveFailureStaysSignedIn` (probe B), `TestPersistingTokenSource_RetryDoesNotOverwriteANewerFile` (probe C), `TestLogin_UndoRestoresTheSignInTheInstallReplaced` (F4).
  - `TestPersistingTokenSource_NeverWritesAnOlderToken` was rewritten in lineage terms. Its successor token now expires first.
  - The F4 test passes reliably on the fix, but on the old code it fails only sometimes, because the old early read races the test's write.
- Checks: tests pass over graph, tokenfile and cli (`-p 1`). The focused `-race -count=3` auth run passes. Lint of graph, tokenfile and cli: 0 issues.


## Merged as #957

- Squashed to one commit on main `97f45626`, merged as `ed2f2441` (2026-11-19 19:48 PST), installed the same evening.
- Added at ship time: the login undo waits for the sign-in lock even after the login's context ended (the reviewer's "not counted" note; `TestInstall_UndoAfterTheLoginEndedWaitsForTheLock`, seen failing first), and `internal/localpath/folder_modes_test.go` no longer classifies the removed `internal/cli.tokenDirPerms` (main's #956 test met this branch's removal).
- Checks: build, vet plain and with all tags, `go mod verify`, `-race -p 1` over the nine touched packages (`internal/sync` 5793 s), pre-pr gate, PR CI, pre-merge, post-merge.
- Live: `TestE2E_AuthPreflight_Fast` on the test account passed after the merge.
- Nightly: four `internal/sync` claims released (parked as R2-n1, R2-n2, W101-n2). Nothing new parked.
