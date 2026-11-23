# e2e tests wait for what they read — commit f940aa19 on bd2711b0 (not pushed)

## What changed
- `e2e/daemon_wait_rule_test.go` (new, untagged): the source scan and its self-test.
- `e2e/remote_read_helpers_test.go`: `pollRemoteEventually` split over a non-failing `pollRemoteUntil`; `waitForRemoteFileContent` prints its own last download on timeout (`summarizeContent`: length + first 200 bytes, `%q`); `readRemoteFileOnce` for predicates.
- `e2e/sync_watch_full_test.go`: RapidChurn and GracefulShutdown name waits removed (GracefulShutdown's one-shot file now a size wait); ConflictDuringWatch predicate reads only locally, content wait after; new `TestE2E_SyncWatch_StatusPerfCountsRequests`.
- `e2e/sync_full_test.go`: content waits for modified files (DownloadOnly/UploadOnly defers, NestedFolderHierarchy); dry-run delta wait (`dryRunPlans`) before the single pass of CreateCreate, EditEdit and DryRun; predicates in EditDelete tests no longer call `getRemoteFile`; `globConflictCopies`.
- `e2e/cli_commands_e2e_test.go`: mv/cp `--force` read dst through the content wait; mv waits for src to leave the listing.
- `e2e/sync_hardening_full_e2e_test.go`: NamingCorpus download converges on the content it checks, upload via `runSyncUntilOnlyConditionsRemain`; `readContentTree`/`contentTreeHolds` for six predicates.
- `e2e/shared_items_helpers_test.go`: `findActiveShortcutChild` returns (record, isActive, err) and asserts nothing; callers assert after convergence; `readShortcutChildRecords`; `eventuallySharedContentEquals` makes its temp dir outside the testify goroutine.
- `e2e/status_helpers_test.go`: typed `Perf` (`http_request_count`), `waitForStatusPerfRequests`.
- `internal/devtool/verify_e2e.go`: one line, the new test in `full-serial-watch-shared`.
- `spec/design/system.md` Verification Policy (names the test); `spec/reference/live-recurring-families.md` (both families' policy lines).

## Tests added
- `TestE2EDaemonTestsUseNoNameOnlyWait`: no test that starts a process reaches a name-only wait helper; every wait helper classified; no stale entry or exception.
- `TestDaemonWaitScanFindsDirectAndHelperCalls`: planted source; a direct call and one through a helper (inside a subtest) are both found.
- `TestSummarizeContentBoundsAndQuotes`: bound, length, binary bytes escaped.
- `TestE2E_SyncWatch_StatusPerfCountsRequests` (live): `status --perf --json` counts the watched mount's requests.

## Scan
Depth: unbounded through the suite's own functions (calls, method selectors, function values passed as arguments, closures), cycle-safe, stopping at a wait helper. Wait helper = a non-test suite function that calls a poll primitive (`sleepForLiveTestPropagation`, `retry.TimeSleep`, `pollRemoteEventually`, `pollRemoteUntil`, testify `Eventually*`/`Never*`). Classes: 7 name-only, 25 size/content/state; 26 daemon tests; 7 exceptions (3 reads after daemon exit, 4 shortcut-fixture waits). Failure lines:
- red, before deletion: `TestE2E_SyncWatch_RapidChurn starts a process that may still be writing and waits for a name alone with waitForRemoteReadContains`; same line for `TestE2E_SyncWatch_GracefulShutdown`; also `TestE2E_SyncWatch_ConflictDuringWatch ... with getRemoteFileForDrive through getRemoteFile`
- planted direct: `TestE2E_SyncWatch_FolderCreation starts a process that may still be writing and waits for a name alone with waitForRemoteExactStatVisible`
- planted via helper: `TestE2E_SyncWatch_MultipleFiles ... with waitForRemoteReadContains through plantedListingCheck > waitForRemoteParentListingContains`

## Predicates fixed under (d)
findActiveShortcutChild (state asserts) and shortcutChildRecords (require) in the projection converge; ConflictDuringWatch (getRemoteFile, conflictCopiesForPath require, in testify's goroutine); EditDeleteConflict and PreserveEditDeleteConflict (getRemoteFile); DownloadOnlyDefers (runCLIWithConfig ls); CrossMoves, TwoWriters ×4, TypeChangesBothWays (contentTree require); eventuallySharedContentEquals (t.TempDir in testify's goroutine).

## Checks
`go build ./...` ok; `go vet ./...` ok; `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...` ok; quick lane `go test -short ./e2e/...` ok (scan + self-test pass); `go test -count=1 ./internal/devtool/... ./cmd/devtool/...` ok (bucket manifest test included); lint `./e2e/...` behind the lock: 0 issues (W3-lint.log). Spec names checked by hand: all four cited tests exist.

## Deviations
- No source-reading test holding the converge rule exists in `e2e` at bd2711b0 (only the live `TestE2E_TrackedFilesNameNoTestAccount`; the bucket-manifest scan is in `internal/devtool`). The scan is new, in an untagged file like `catalog_drive_bootstrap_test.go`, so the quick lane's untagged `go test -short ./...` runs it; depth, classes and exception form are designed here.
- `pollRemoteEventually`/`pollRemoteUntil` are poll primitives, not wait helpers: their callers' predicates are what is classified. A test's own inline predicate is outside the scan.
- Exceptions rather than fixes for BasicRoundTrip, PauseResume, ControlSocketReload (tests the source does not name).
- Dry-run delta wait also added to PreserveEditEditConflict (same defect as CreateCreate, a few lines in a touched file).

## Also fixed
- GracefulShutdown's one-shot file: name wait → size wait (no exception needed).
- NamingCorpus upload: best-effort single pass → `runSyncUntilOnlyConditionsRemain`.
- NestedFolderHierarchy: modified `another.txt` read through the content wait.
- `requireActiveShortcutChild` failure now prints the record's state and reason.

## Not fixed (seen)
- e2e/sync_watch_full_test.go:194: ConflictDuringWatch sleeps 5 s "for delta"; fix: wait on an observable (a daemon debug event for the remote change).
- e2e/sync_watch_full_test.go:571,576: TimedPauseExpiry's fixed sleeps around the pause; fix: wait on the control socket's pause state.
- e2e/sync_watch_e2e_test.go:121,272,384: `getRemoteFile` after daemon exit (declared exceptions); fix: `waitForRemoteFileContent`, drop the 3 exceptions.
- e2e/sync_full_test.go:1121: ConvergentEdit's single pass runs straight after two remote puts; fix: dry-run wait until the plan no longer uploads the edit (negative predicate).
- e2e/sync_full_test.go:1134-1135: ConvergentEdit reads put-overwritten content via `getRemoteFile`; fix: content waits.
- e2e/remote_read_helpers_test.go:424: `waitForRemoteFixtureSeedVisible`'s predicate can start a nested `pollRemoteEventually` that fails the test (at the outer deadline only); fix: a non-failing recovery.
- e2e/remote_read_helpers_test.go:588: `getRemoteFileForDrive` waits for a name and returns whatever one get holds; fix: replace content-comparing callers suite-wide with `waitForRemoteFileContent`.
- spec/requirements/non-functional.md:202: R-6.10.17 does not name the wait rule its tests now enforce; fix: add a clause.
- File-name-only grep (a poller and an asserting helper in one file, not chased): e2e/sync_e2e_test.go, e2e/sync_shortcut_full_e2e_test.go, e2e/orchestrator_e2e_test.go, e2e/sync_safety_full_e2e_test.go, e2e/sync_recovery_e2e_test.go, e2e/sync_edge_cases_full_test.go, e2e/sync_shared_e2e_test.go.

Packages for the reviewer: none beyond `./e2e` (live) and `./internal/devtool` (run here).

## Live run list (one at a time; before the change, in a few words)
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_StatusPerfCountsRequests$' ./e2e  # did not exist; no test read the count
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_RapidChurn$' ./e2e  # extra name poll, same result
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_GracefulShutdown$' ./e2e  # passed on names alone
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_ConflictDuringWatch$' ./e2e  # remote get failing inside testify's goroutine
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges$' ./e2e  # old content read; ls failure in predicate
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges$' ./e2e  # old content read after a name wait
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_EditDeleteConflict$' ./e2e  # asserting get inside the predicate
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict$' ./e2e  # asserting get inside the predicate
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DirectionalModes_PreserveEditEditConflict$' ./e2e  # pass before delta: upload, no conflict
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict$' ./e2e  # pass before delta: upload, no conflict
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DryRunNonDestructive$' ./e2e  # dry run could miss the download
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_NestedFolderHierarchy$' ./e2e  # old content of an edited file
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Mv_ForceOverwrite$' ./e2e  # old dst content; lagging listing
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Cp_ForceOverwrite$' ./e2e  # old dst content
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_NamingCorpusRoundtrip$' ./e2e  # one best-effort pass, content unpolled
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_CrossMoves$' ./e2e  # tree read asserted in predicate
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_TwoWriters$' ./e2e  # tree read asserted in predicate
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_TypeChangesBothWays$' ./e2e  # tree read asserted in predicate
go test -count=1 -tags 'e2e' -run '^TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount$' ./e2e  # transient child state failed the test
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_RenameReusesChildMountState$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_MoveReusesChildMountState$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_WritableUploadSyncsToSharedTarget$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_ReadOnlyBlockedUploadStatus$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot$' ./e2e  # same
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shared_FileDiscoveryAndSelectorRoundTrip$' ./e2e  # t.TempDir in testify's goroutine


## Second pass (review)

Changed, by file:
- e2e/sync_full_test.go: `dryRunPlans` is now two predicates over one line scanner, `dryRunPlanHasLineStarting(prefix)` and `dryRunPlanHasNoTransferUnder(folder)`; ConvergentEdit converges dry runs until no trimmed `Would ` line holds `<folder>/`, then runs its one pass, and reads converge-edit.txt through `waitForRemoteFileContent`; EditEdit's upload-only branch reads shared.txt through the content wait. converge-create.txt (new file, exited put) keeps `getRemoteFile`.
- e2e/remote_read_helpers_test.go: the recovery in `runRemoteReadAttemptWithRecovery` is `sharedRootListingAnswersWithin` (on `pollRemoteUntil`, fails nothing); an unrecovered attempt carries `errSharedRootListingLagging` and `pollRemoteUntil` never offers it to ready; `waitForSharedRootListingVisible` keeps the failing form (the `Within` variant is folded in); the content-wait comment says what is true; `summarizeContent` moved out.
- e2e/content_summary_test.go (new, no build tags): `summarizeContent`, its bound and `TestSummarizeContentBoundsAndQuotes`, which now runs in the quick lane (ran here: PASS).
- e2e/daemon_wait_rule_test.go: `checkDaemonWaitRule` returns the findings (name-only waits without exception, unclassified, doubly classified, stale entries, stale exceptions) and the main test asserts on them; `pollCLIWithConfigRetryingTransientGraphFailures` is name-only (no daemon test reaches it: the scan passes); entries for the two new wait helpers; the header names the two blind spots (package-level initialisers, exceptions keyed by test and helper).
- e2e/shared_items_helpers_test.go, e2e/sync_shortcut_full_e2e_test.go: the two shortcut-child failure messages print `remote=fixture.SharedItem` instead of the drive and item IDs; new `assertSameIdentity`, `assertSharedFileFixtureConsistent`, `assertStatNamesSharedItem`.
- e2e/shared_items_e2e_test.go, e2e/shared_items_fast_e2e_test.go, e2e/fixture_preflight_full_e2e_test.go: identity comparisons go through those helpers.
- e2e/status_helpers_test.go: `readStatusDriveState`, `waitForStatusDriveUnpaused`; the sync_state message names the role, not the canonical ID.
- e2e/sync_watch_full_test.go: ConflictDuringWatch's 5 s sleep is gone and its comments say what the daemon does; TimedPauseExpiry pauses 20 s, fails on an upload inside the window, asserts status still reports `paused` at the window's end, and waits for expiry through status instead of sleeping.
- spec/design/system.md:455, spec/reference/live-recurring-families.md:131-136, spec/requirements/non-functional.md:202 (one sentence, not reflowed).

Test added: `TestDaemonWaitRuleReportsUnclassifiedAndStaleEntries` (planted sources): an unclassified helper, a doubly classified one, a stale entry that polls nothing and one the sources lack, an unexcused name-only wait, and a stale exception are each reported. Each of its five assertions failed once with the condition it guards inverted (scripted, file restored after; five times exit 1 with that assertion's message).

Item 3: the non-failing recovery cannot be tested without an account, and runs only when a shared-root read hits the listing-lag 404, so no run is sure to take it. The shared-root tests on the run list reach it (the directional, dry-run, convergent-edit and watch tests on isolated shared roots, through the content wait and the fixture-seed predicate). A recovery that only stopped failing would have handed the unrecovered 404 to the ready function, and `deleteDisappearanceReady` accepts a "not found" stderr, so a delete wait could pass where it failed before; with the sentinel every wait ends as before, with its own message.

Item 9:
- ConflictDuringWatch: the steady-state replan refreshes local truth and plans from committed remote truth (internal/sync/engine_watch_replan.go:12-15, 39-52); remote truth changes only when the observer's poll batch commits (engine_observation_postprocess.go:59-113, poll every 30 s); the upload of a known item checks its live state first (executor_transfer.go:173-194, 217-224). The sleep ordered nothing the pass reads, and a poll landing inside it would have downloaded the remote edit before the local one, the opposite of the setup. Deleted; the polled waits after it are the waits.
- The review's suggested observable does not exist for this: `remote_refresh_*` events are emitted only for the full remote refresh (engine_watch_maintenance.go:130, engine_observation_postprocess.go:163-181), not for the 30 s poll.
- TimedPauseExpiry: `status --json` gives the drive state `paused` from the configured pause and the clock (internal/cli/status_snapshot.go:498-506), not from the daemon; the reload still makes the daemon clear it. The pause went from 5 s to 20 s because pause, reload, write and the stats take about as long as the old pause, so "still paused at the window's end" would otherwise fail; the test takes about 15 s longer. The old warning let an upload during the pause pass; it now fails.

Where the review was wrong or short: the `remote_refresh_applied` observable (full refresh only); item 4 understated the leak, since `assert.Equal` on the two ID fields printed them in 4 more places (fixed, with 10 email, selector and canonical-ID prints of the same kind in the touched files); its item 3 fix alone would have changed what delete waits accept.

Checks: `go build ./...` ok; `go vet ./...` ok; `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...` ok; `go vet -tags e2e ./e2e/...` ok; `go test -count=1 -short ./e2e/` ok, and the four untagged scan and summary tests by name ok; devtool's repository-rule tests (`RealRepo|Governs|ListsEvery|Registered|Bucket`) ok; gofumpt and goimports clean; every test the changed spec lines cite exists; lint: 0 issues over `./e2e/...` (W3-lint.log); the first run found one gocritic typeDefFirst on the new check method, now a plain function, and the five inversions were rerun on it.

Seen and not fixed:
- e2e/fixture_preflight_e2e_test.go, e2e/w7_live_caller_proof_e2e_test.go: `assert.Equal` on emails, selectors or canonical IDs prints both values on failure (found by file-name grep, not read line by line); fix: `assertSameIdentity`.
- e2e/remote_read_helpers_test.go `pollRemoteEventually`: its timeout message prints the last stdout and stderr, which for `stat --json`, `shared` and `drive list --json` hold remote IDs, and under `--debug` Graph URLs; fix: a bounded summary through `suiteDriveRoles.Redact`.
- e2e/socketio_helpers_test.go:84,132-146,176,202-226: failure messages print whole debug events with `%+v`; whether an event can carry a drive or item ID was not checked; fix: print the type and observer only.
- e2e/shared_items_e2e_test.go:18-34 and e2e/shared_items_fast_e2e_test.go:13-31: the two file-discovery tests repeat the same discovery and stat block; fix: one helper, or keep one copy.

Live run list (one at a time; corrected, 34 tests):
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_StatusPerfCountsRequests$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_RapidChurn$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_GracefulShutdown$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_ConflictDuringWatch$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_SyncWatch_TimedPauseExpiry$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_EditDeleteConflict$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DirectionalModes_PreserveEditEditConflict$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_DryRunNonDestructive$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_ConvergentEdit$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_NestedFolderHierarchy$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Mv_ForceOverwrite$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Cp_ForceOverwrite$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_NamingCorpusRoundtrip$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_CrossMoves$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_TwoWriters$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Sync_TypeChangesBothWays$' ./e2e
go test -count=1 -tags 'e2e' -run '^TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_RenameReusesChildMountState$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_MoveReusesChildMountState$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_WritableUploadSyncsToSharedTarget$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_ReadOnlyBlockedUploadStatus$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shared_FileDiscoveryAndSelectorRoundTrip$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shared_FileDiscoveryAndSelectorReadCommands$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_Shared_RawFolderLinkDriveAdd_NormalizesToCanonicalSharedDrive$' ./e2e
go test -count=1 -tags 'e2e e2e_full' -run '^TestE2E_FixturePreflight_Full$' ./e2e

## Third pass

Commit `b145f2c7` (amended, one commit on `2b1c5afa`, subject kept). Files this pass changed: `e2e/sync_watch_full_test.go`, `e2e/remote_read_helpers_test.go`, `e2e/status_helpers_test.go`, `e2e/shared_items_helpers_test.go`, `e2e/sync_full_test.go`, `e2e/sync_shortcut_full_e2e_test.go`, `e2e/content_summary_test.go`, `e2e/e2e_test.go` (item 7 only), new untagged `e2e/remote_read_rules_test.go` and `e2e/dry_run_plan_test.go`, `spec/design/system.md`, `spec/reference/live-recurring-families.md`.

Per item:
1. TimedPauseExpiry: `pausedUploadWindow = watchLocalDebounce (5 s, read from `defaultDebounce` in `internal/sync/engine_watch.go:23`; no e2e config sets `Debounce`, only unit tests do) + pausedFileUploadAllowance (10 s)` = 15 s; `timedPauseDuration = window + pauseOutlastsWindowMargin (15 s)` = 30 s; each constant's comment says what it covers. The second half is unchanged: wait until status no longer reports paused, reload, wait for the file's size. Designed waiting about 45 to 55 s, worst case bounded by the existing timeouts.
2. `pollRemoteEventually` builds its timeout message through `remotePollTimeoutMessage(suiteDriveRoles, ...)` (redacts description, args, stdout, stderr); `readStatus`/`readStatusAllDrives` redact stdout and stderr; `waitForRemoteFileContent` redacts `get`'s stderr. The two status waits go through `pollRemoteEventually`, so they are covered.
3. `decideRemoteReadNextStep`: a listing-lag failure with no time left is marked `errSharedRootListingLagging` (was returned plain).
4. Shared helpers: `get <selector>` output, `ls --json` stdout and error in the placeholder fail/skip texts, `shared --json` stderr, the candidate drive now by `Role`, the recipient email by `Redact`, all stdout/stderr in the drive-add wait; the shortcut-record read errors (they name the state DB, whose file name spells the drive) through `requireNoErrorByRole` and a redacted log; `sync_full_test.go` stat logs and the concurrent-put error; `sync_shortcut_full_e2e_test.go` cleanup log. `shared --json` and `drive list --json` stdout is no longer printed: messages give counts (`N shared items listed`, `N configured and M available drives`, or the byte count of undecodable output).
5. The window loop requires `isRemoteNotFoundCleanup(stderr)` on every failed stat; any other failure fails with the redacted stderr (brief's rule, not the review's "keep watching").
6. `dryRunStdoutPlansNoTransferUnder` also requires a summary line (`Dry run: nothing to do.` or `Dry run, nothing was changed.`, `internal/cli/sync_console.go:330-335`).
7. Converge helper timeout: description, stdout, stderr redacted.
8. Content wait description uses `summarizeContent([]byte(want))`.
9. `TestSummarizeContentBoundsAndQuotes`: `// Validates: R-6.10.17` (the content wait's failure output; system.md cites the test in that rule's paragraph).
10. Reworded.
11. Done at low cost: classification, decision, mark and gate moved to `remote_read_rules_test.go`; `TestClassifyRemoteReadRecoveryAction` moved there (now runs in the quick lane).

Tests (untagged, quick lane), each shown failing with its fix removed: `TestDecideRemoteReadNextStep` (deadline cases fail when the deadline returns the attempt as is), `TestRemotePollAcceptsNoAttemptMarkedLagging` (fails without the `errors.Is` gate), `TestRemotePollTimeoutMessageNamesDrivesByRole` (Validates R-6.10.27; fails without `Redact`), `TestDryRunStdoutPlansNoTransferUnder` ("no output" and "cut before the summary" fail without the summary check). `TestDryRunStdoutHasLineStarting` covers the moved predicate. No requirement fits the sentinel or dry-run tests (R-6.10.17 speaks of daemon tests only), so they carry no `Validates:` line.

Checks: gofumpt clean; `go build ./...`, `go vet ./...`, tagged vet clean; `go test -count=1 ./e2e/` untagged ok (no live test started); `TestFindTestConventionViolations_RealRepoIsClean` and the other devtool real-repo checks ok; lint `0 issues` (W3-lint.log); all 38 cited test names exist.

Where the review was wrong or short: its facts held where checked (the lag 404 text does contain "could not be found", so the deadline path was a real hole). Its "one-call `Redact`" fix is not enough for `shared --json` and `drive list --json` stdout: `suiteDriveRoles` knows only the two configured drives, so it cannot see the address of a sharer or owner outside them, nor any display name or Graph drive/item id; those outputs are now summarized by count. Status output lists only configured accounts, so `Redact` covers its addresses; its `display_name`/`org_name` stay (not in the rule's list), as they do in the debug log. Should-fix 3's "keep watching" was replaced by the brief's "fail".

Live e2e tests this pass changed (own body, or a helper they call):
TestE2E_SyncWatch_TimedPauseExpiry
TestE2E_Sync_ConvergentEdit
TestE2E_Sync_DirectionalModes_PreserveEditEditConflict
TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict
TestE2E_Sync_DryRunNonDestructive
TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict
TestE2E_EdgeCases
TestE2E_SyncWatch_ConflictDuringWatch
TestE2E_Shortcut_ExplicitStandaloneSharedFolderRemainsConfiguredDrive
TestE2E_SharedFolder_DriveList_ShowsExplicitSharedFixtures
TestE2E_Shared_FolderNameDriveAdd_HonorsAccountFilter
TestE2E_Shared_FileDiscoveryAndSelectorRoundTrip
TestE2E_Shared_FileDiscoveryAndSelectorReadCommands
TestE2E_FixturePreflight_Fast
TestE2E_FixturePreflight_Full
TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount
TestE2E_Shortcut_ReadOnlyDownloadOnlyProjectsChildMount
TestE2E_Shortcut_RestartIdempotentKeepsChildMountVisible
TestE2E_Shortcut_RenameReusesChildMountState
TestE2E_Shortcut_MoveReusesChildMountState
TestE2E_Shortcut_WritableUploadSyncsToSharedTarget
TestE2E_Shortcut_ReadOnlyBlockedUploadStatus
TestE2E_Shortcut_LocalRootCollisionSkipsChildButParentCompletes
TestE2E_Shortcut_WatchLocalUploadSyncsToSharedTarget
TestE2E_Shortcut_WatchRemoteWakeUpdatesChildRoot

Outcome changes only in the first two (and, for every remote poll on a shared drive, a lag 404 at the deadline is no longer accepted); the rest change messages or a refactor with the same decision; ConflictDuringWatch is a comment. `readStatus`, `pollRemoteEventually` and the converge helper reach most tests with message-only changes. First live run must show: TimedPauseExpiry passes, every window stat fails with a not-found stderr, status still `paused` after the window, the test's waiting near 45 to 55 s; ConvergentEdit's dry-run convergence ends on output holding a summary line.

Seen and not fixed:
1. `e2e/cli_commands_e2e_test.go:46-48,89`, `e2e/shared_items_e2e_test.go:206-208`, `e2e/sync_shortcut_full_e2e_test.go:210-212` and the suite's `assert.Contains` on sync output: testify prints the whole output, and status and sync text name the account email (`sync_console.go:108`). Fix: an output assertion helper that prints redacted output.
2. `e2e/e2e_test.go:510,516,685,720,745,798,883`: raw stdout and `--debug` stderr in failure messages. Fix: `suiteDriveRoles.Redact`, as at 645.
3. `e2e/e2e_test.go:282-299,347`: the debug log, dumped on failure, records `shared --json` and `drive list --json` output through `Redact`, which cannot see an unconfigured sharer. Fix: log those by summary.
4. `e2e/remote_read_helpers_test.go:604,635,647`: pure predicate tests in an `e2e`-tagged file, so only live runs execute them. Fix: move them with their predicates to an untagged file.
5. `e2e/sync_full_test.go:371,466,611,679,707,822`: `120*time.Second` repeated inline. Fix: a named timeout constant.


## Third step review, fixed in review (2026-11-14, about 12:20 to 12:45 PST)

Commit `eafcaa38`, one commit on main `2e465121` (amended from `b145f2c7`, then rebased; one conflict in `spec/design/system.md`, resolved word by word, both sides kept). The review (`R-W3c-findings.md` in the scratch folder) had no must-fix, four should-fix items and ten notes. I made the fixes myself; no writer pass.

Should-fix, all four done:
1. The timed-pause window read a lagging listing of the mount root as "missing". The window's stat now goes through the suite's shared-root recovery (`runRemoteReadAttemptWithRecovery`) with the pause's end as the recovery deadline; a lag that is not recovered fails the test with its own message. New helper `requireNotUploadedDuringPause` in `e2e/sync_watch_full_test.go`, classified in `readyWaitHelpers()`.
2. A stat that answers after the pause could have ended proved nothing. The test computes `pauseEnds` before the pause command, as `now + duration` cut to whole seconds (the CLI stores the end in whole seconds, `internal/cli/sync_pause_resume.go:48-49`, so the review's uncut value was not a safe lower bound), and fails a later answer as a timing fault that names `pauseOutlastsWindowMargin`.
3. A read that was run again after the listing answered and met the lag a second time was offered to the predicate. It is now marked `errSharedRootListingLagging` (one recovery per attempt); two cases added to `TestDecideRemoteReadNextStep`; `spec/design/system.md` says so.
4. The skip of a drive list that omits the shared fixture printed the fixture's selector, which holds the sharer's drive and item IDs. It names the fixture instead.

Notes fixed here: 4 (margin comment says what the margin covers), 5 (the content-summary test carried `Validates: R-6.10.17`; no requirement fits a failure-message helper, so the line is gone), 6 (`sharedListingNotDecoded` comment), 7 (dry-run comment), 9 (the status assertion after the window names the margin).

Decisions (mine): in the pause window a stat is read through the recovery with the pause's end as its deadline; an unrecovered lag and a late answer each fail with their own message; one recovery per read attempt; the content-summary test validates no requirement; the drive-list skip names the fixture, not its selector.

Handed to the follow-up row for e2e content reads and predicates (plan riders): note 1 (the e2e copy of the debounce is held by a comment only), 2 (an upload after the window and before expiry is unseen), 3 (the reload hides the daemon's own expiry wake), 8 (what `Redact` leaves: Graph drive and item IDs in several outputs), 10 (`isRemoteNotFoundCleanup` scans the whole debug stderr). Seen in this pass's live run and added to the same rider: the debug log's command line holds the `--drive` selector as given, not through `Redact`, for commands run with a shared-root selector.

Checks after the fixes and again after the rebase: `go build ./...`, `go vet ./...`, the tagged vet, gofumpt on the edited files, `go test -count=1 ./e2e/` (untagged), the test-convention test: all pass.

Live, test account, one test: `TestE2E_SyncWatch_TimedPauseExpiry` passes (55 s). From the debug log's command times: pause at +0 s, nine window stats from +0 s to +15 s, each a not-found answer (the helper requires it), the status read after the window at +17 s still paused, 13 s before the 30 s pause ends; status stops reporting paused at +34 s; the file is on OneDrive at +43 s. So the 15 s margin had 13 s of slack on this host. The rest of the run list (the 25 tests above and the first list) is still owed.



## Live run of the changed tests (mine, 2026-11-14 12:40 to 13:50 PST, commit `eafcaa38`)

Every test this change edits or whose helper it edits, run alone, one at a time, on this host against the test account. 42 tests: 24 passed, 16 failed, 2 skipped.

- Passed (24), seconds in brackets: `TestE2E_SyncWatch_StatusPerfCountsRequests` (54), `TestE2E_SyncWatch_RapidChurn` (71), `TestE2E_SyncWatch_GracefulShutdown` (72), `TestE2E_SyncWatch_ConflictDuringWatch` (141), `TestE2E_SyncWatch_FolderCreation` (89), `TestE2E_SyncWatch_MultipleFiles` (530), `TestE2E_SyncWatch_TimedPauseExpiry` (189), `TestE2E_TrackedFilesNameNoTestAccount` (32), `TestE2E_Sync_DownloadOnlyDefersLocalOnlyChanges` (79), `TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges` (103), `TestE2E_Sync_EditDeleteConflict` (69), `TestE2E_Sync_DirectionalModes_PreserveEditDeleteConflict` (110), `TestE2E_Sync_DirectionalModes_PreserveEditEditConflict` (83), `TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict` (93), `TestE2E_Sync_DryRunNonDestructive` (215), `TestE2E_Sync_NestedFolderHierarchy` (112), `TestE2E_Mv_ForceOverwrite` (52), `TestE2E_Cp_ForceOverwrite` (40), `TestE2E_Sync_NamingCorpusRoundtrip` (80), `TestE2E_Sync_CrossMoves` (500), `TestE2E_Sync_TwoWriters` (89), `TestE2E_Sync_TypeChangesBothWays` (68), `TestE2E_Sync_ConvergentEdit` (74), `TestE2E_EdgeCases` (116).
- Failed (16), every one at its first line, on a fixture this host does not have, before any request the change touches: the ten `TestE2E_Shortcut_…` tests and `TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount` (ten of the eleven say the two shortcut parent-drive variables are not set, one says the shared-folder fixture is missing), `TestE2E_Shared_FileDiscoveryAndSelectorRoundTrip` and `TestE2E_Shared_FileDiscoveryAndSelectorReadCommands` (the shared-link variable is not set), `TestE2E_FixturePreflight_Full`, `TestE2E_SharedFolder_DriveList_ShowsExplicitSharedFixtures` and `TestE2E_Shared_FolderNameDriveAdd_HonorsAccountFilter` (the shared-folder fixture is missing). I read the first failure lines of each of the 16 logs; none failed anywhere else.
- Skipped (2): `TestE2E_Shared_RawFolderLinkDriveAdd_NormalizesToCanonicalSharedDrive` (its raw-link variable is not set) and `TestE2E_FixturePreflight_Fast` (the verifier turns it on, a plain `go test` does not).
- So the shortcut, shared and fixture tests were NOT seen live on this host. Their only live proof is the pull request's e2e job and the nightly, on the CI account, which holds those fixtures.
- The slowest two (`TestE2E_SyncWatch_MultipleFiles`, 530 s; `TestE2E_Sync_CrossMoves`, 500 s) passed; why they take that long on this host was not looked into here.



## Merged as #911 (`0ff7c754`, 2026-11-14 about 15:33 PST)

- One commit `6474e50f` on main at `36f2e2b6`. The pre-pr gate passed on its first full run (45 minutes with other work on the host; lint 9 of them); pre-merge passed with no review threads; post-merge passed and removed the `e2e-wait-leftovers` worktree and its branch.
- Installed `0ff7c754`; the watch client reports "Up to date, watching for changes", 19 files.
- Live proof: 24 changed tests were run one at a time on the test account before the gate. The shortcut, shared-folder and fixture tests ran only in the PR's CI e2e job, which holds the account they need.
- Still to read from the nightlies after this merge: whether the three watch tests the source check caught stay green over several nights, and how often a shared-folder listing lags twice in a row (unknown today).
- Free to start after this: the row that takes the content reads and poll predicates of the same suite.
