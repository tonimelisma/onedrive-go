# W1 report: DoD and nightly survive GitHub errors; CI logs name no drive

Commit `8e18f8f9` on `ci/dod-nightly-hardening` (base `34fb1a66`), amended for the second and third passes, not pushed. 52 files, +3715/-932.

## What changed
- `internal/devtool/gh_retry.go` (new): `ghClient`. `output` repeats a 5xx, a dropped connection or a request that never reached GitHub (dial, TLS, DNS, unreachable network) with backoff (5/10/20 s, 4 attempts). Rate limits are final. `outputUnlessDone`: a refusal of a repeat after a 5xx means the first attempt took effect. `outputOnce` covers requests that must not repeat.
- `internal/devtool/go_test_log.go` (new): the one go test log parser. Result lines are read only in go test's exact `ok  \t` / `FAIL\t` forms.
- `internal/devtool/ci_workflow.go` (new): the one `ci.yml` reader: jobs, `if:` per event, per-event or longest `timeout-minutes`, and the concurrency group (fails loudly on other forms).
- `verify_dod_ci_wait.go`: per-event stall window. A not-started check whose job is in a concurrency group gets that group's longest timeout on any event, plus the margin; the hard cap adds that window. A rerun refused as already running after a 5xx is confirmed by reading the run again.
- `verify_dod.go`, `nightly.go`, `verify_failed.go`, `verify_e2e.go`, `verify_stress.go`, `verify_test_match.go`, `worktree.go`, `cleanup_audit.go`: as in the first pass (one `githubRun`/`githubJob`, every gh call through `ghClient`, every go test reader through the parser, shared main-branch constants). `verify_test_match.go` also uses the exact result-line forms.
- `bench_live.go`, `live_env.go`: the benchmark's allowlist check is `testutil.CheckDriveAllowed`.
- `testutil/testenv.go`: `AllowlistIncludes` and `CheckDriveAllowed` are the one allowlist reader; errors name the variables, never values; `ValidateAllowlist` uses them.
- `testutil/driveroles.go` (new): `DriveRoles` maps configured drives to `drive-1`/`drive-2` (`-account` for the address) in every spelling: as-is, colon-as-underscore, lower-cased, percent-encoded.
- `e2e/e2e_test.go`, `timing_helpers_test.go`, `quirk_helpers_test.go`: TestMain names `ONEDRIVE_TEST_DRIVE_2`, not the drive, and the scrub prints roles. The timing and quirk recorders and every debug-log chunk go through `suiteDriveRoles`.
- `scripts/ci-mask-test-drives.sh` (new, bash 3.2): masks every spelling, lists them for redaction, and exports the values plus `<NAME>_VAULT_SECRET` and `<NAME>_TOKEN_FILE` through `$GITHUB_ENV`.
- `.github/workflows/ci.yml`:
  - Both live jobs run the script right after checkout; the credential steps read the exported names (no `sed` left).
  - `integration` timeout is 15: its 5-minute deadline plus the 10-minute setup allowance. Recent runs took 1.6 to 2.4 minutes.
  - e2e timeout expression is 40 on a pull request and 180 otherwise.
  - The redaction step stays as a second layer.
  - Core count: one `getconf _NPROCESSORS_ONLN` line, in a block scalar because a plain scalar containing `: ` is invalid YAML.
- Docs: `spec/design/system.md`, `spec/requirements/non-functional.md` (R-6.10.16, R-6.10.23, R-6.10.26, new R-6.10.27), `CLAUDE.md`, `CONTRIBUTING.md`.

## Tests added (each seen failing first)
- First pass: `TestNightlyCheck_RetriesAJobLog5xxThenSucceeds`, `TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound`, `TestGHClient_RetriesOnlyTransientFailures`, `TestGHClient_ContextEndingDuringTheBackoffEndsTheCall`, `TestWaitForPRCI_RepeatsAPollGitHubFailed`, `TestRunFailedTestVerification_PatternThatSelectsNothingFails`, `TestParseGoTestLog_{TimestampedRawLines,BraceInARawLineIsNotJSON,TimeoutPanicNamesTheRunningTest,PackageFailuresAndPassingPackages}`, `TestCIStallWindow_{PostMergeUsesOnlyPushJobs,PullRequestUsesOnlyPullRequestJobs}`, `TestCIWorkflow_RejectsAnExpressionItDoesNotKnow`, `TestCIWorkflowSetsNoTestDriveInJobEnv`.
- `TestParseGoTestLog_StrayOkLineDoesNotCloseARawPackage`: a stray "ok then" line keeps the pending failure; `[setup failed]` is read.
- `TestGHClient_RetriesARequestThatNeverReachedGitHub` (six real Go/gh error texts), `TestGHClient_RateLimitsAreFinal`, `TestGHClient_RefusalAfterA5xxMeansTheRequestTookEffect`.
- `TestWaitForPRCI_RerunGitHubStartedDespiteA5xxIsWaitedFor` (502, then the refusal, then the run is read again), `TestWaitForPRCI_RefusedRerunOfAFinishedRunFails`.
- `TestWaitForPRCI_CheckQueuedInItsGroupWaitsOneRunOfTheGroup` (not stalled at 56 min, stalls at 190), `TestWaitForPRCI_RunningCheckStallsAtTheEventsWindow` (50).
- `TestCIStallWindow_PullRequestUsesOnlyPullRequestJobs` also checks the group window (a nightly job sharing the group) and the hard cap; `TestCIWorkflow_RejectsAnExpressionItDoesNotKnow` gains three concurrency forms.
- `TestCIWorkflowSetsNoTestDriveInJobEnv`: script after checkout only, no `sed` after it, derived names used, redaction before uploads. Mutations: run step first, allowlist in job env, a `sed` step.
- `TestCIMaskScript_MasksEverySpellingAndExportsTheNames`: runs the script with synthetic values (one and two drives).
- `TestCILiveJobsPullRequestTimeoutsAreTheirNeed` (replaces the e2e-only test; adds integration).
- `TestAllowlistIncludes`, `TestCheckDriveAllowed_NamesTheVariablesAndNoValue`, `TestDriveRoles_RecordedLinesHoldNoConfiguredDrive` (testutil); `TestE2ERecordersRecordRolesNotDrives` (e2e, live suite only).

## Checks
- `go build ./...`, `go vet ./...`, and `go vet -tags 'e2e e2e_full' ./e2e/...`: clean.
- `go test -count=1 ./internal/devtool/... ./cmd/devtool/... ./testutil/...`: ok. With `-race` for devtool and testutil: ok.
- `golangci-lint` over `./internal/devtool/... ./cmd/devtool/... ./testutil/... ./e2e/...`: `0 issues.` (W1-lint.log).
- Script and redaction dry-run with synthetic values: only `::add-mask::` lines are printed, the exports are correct, and the lower-cased percent-encoded address is redacted. `ci.yml` parses as YAML.

## Deviations (behavior, scope, interface)
- Every DoD gh read retries, not only nightly and waits. Review reply and merge are single attempts.
- `nightlyRun`, `ciRun` and `githubRun` are folded into one type.
- New requirement R-6.10.27 (R-6.10.24/25 are reserved); check for an ID collision on rebase.
- Not-started statuses: no recent PR rollup showed a queued live check (all `COMPLETED`), so every status but `IN_PROGRESS`/`COMPLETED`, and an unreported check, counts as not started.
- Stall windows now: push 30 min; PR 50 (e2e 40). A PR check queued in its group gets 190. Default PR hard cap: 3×50 + 190 minutes.
- The live benchmark's allowlist error no longer prints the drive or the allowlist.

## Also fixed
- `system.md` said the stall window was 100 minutes; it was 190.
- The old timeout pattern counted step-level `timeout-minutes` as job timeouts.
- Go test actions were compared against summary-status constants.
- `cleanup_audit.go` duplicated `mainBranchName`; the thread-arg builders returned a constant command name.
- `system.md`'s fixture paragraph named test accounts; it now uses role names.
- `lineShowsSelectedTest` took any `ok ` line as a package result.

## Third pass (same commit)
- One `.env` reader: `testutil.LoadDotEnvFile`/`LoadTestEnvFiles` return errors (missing file is fine); only `LoadDotEnv`/`LoadTestEnv` exit; `LoadLiveTestConfig` and devtool's `loadBenchLiveConfig` use them; `loadBenchDotEnv` deleted. Test: `TestLoadDotEnvFile_ReturnsErrors`.
- No drive in test names, file names or messages: auth preflight subtests are named by role; `debugLogFileName` redacts before sanitizing (debug logs and the timing recorder); about twenty e2e messages (auth preflight, catalog bootstrap, CLI runner, drive list, shared items, status, sync, shortcut, remote read, quirk) write drives through `suiteDriveRoles` (the untagged `catalog_drive_bootstrap.go` wraps by role through `testutil.NewDriveRoles`); command construction unchanged. `TestE2ERecordersRecordRolesNotDrives` now covers a test named after the drive and asserts file names, printing no content.
- No test account in the tree: the two default recipient addresses and the guessing are gone; the parent drives come only from `ONEDRIVE_TEST_SHORTCUT_{WRITABLE,READONLY}_PARENT_DRIVE` (the e2e message names both). The e2e masking step maps them by variable name (`..._FROM: ONEDRIVE_TEST_DRIVE` / `ONEDRIVE_TEST_DRIVE_2`) and the script exports them, refusing any other source. Swept `.env.example`, `testutil/testenv_test.go`, `spec/reference/graph-api-quirks.md`, `spec/reference/live-incidents.md`, `spec/design/system.md`, `e2e/sync_e2e_test.go`, `internal/sync/executor_test.go` (role names in prose, example.com placeholders in unit tests; the folder-name defaults stay). New check `TestE2E_TrackedFilesNameNoTestAccount` (quick live tier, in the fast manifest test) over `testutil.FindTestAccounts`; unit test `TestFindTestAccounts_NamesFileLineAndRoleNeverTheAddress`. A masked local scan of the tracked files finds no CI or local test account.
- Tests added: `TestLoadDotEnvFile_ReturnsErrors`, `TestFindTestAccounts_NamesFileLineAndRoleNeverTheAddress`, `TestCIMaskScript_RefusesAShortcutMappingToAnotherVariable`, `TestE2E_TrackedFilesNameNoTestAccount`; rewritten `TestLoadLiveTestConfig_DefaultShortcutFixtures`; extended `TestE2ERecordersRecordRolesNotDrives`, `TestCIMaskScript_MasksEverySpellingAndExportsTheNames`, `TestCIWorkflowSetsNoTestDriveInJobEnv` (fixture mapping), `TestFastE2EExecutionManifestKeepsOnlySmokeLiveTests`.
- Checks: `go build ./...`, `go vet ./...`, tagged e2e vet clean; `go test -count=1 ./internal/devtool/... ./cmd/devtool/... ./testutil/...` ok; `TestExecutor_Download_MountRootAllowsGraphDriveRootPath` ok; lint over devtool, cmd/devtool, testutil, e2e `0 issues.`; script dry run (fixture variables exported from the mapping); `ci.yml` parses as YAML. `internal/sync` was not linted (one string literal changed).
- Deviation: three testifylint `nolint`s in `TestE2ERecordersRecordRolesNotDrives`, because `NotContains`/`Contains` would print the account into the public log on failure.

## Review fixes (same commit)
- The e2e job wrote the verifier's `--summary-json` to the runner's temp directory and uploaded it unredacted. It now goes into `/tmp/e2e-debug-logs`, so every artifact the job uploads comes from the one redacted directory; `ciLaterStepExposures` checks each upload path against the directory the redaction step walks (two mutation cases in `TestCIWorkflowSetsNoTestDriveInJobEnv`); `system.md` says so.
- `FindTestAccounts` skipped binary files for no stated reason; it reads them like any other file (`TestFindTestAccounts_NamesFileLineAndRoleNeverTheAddress` expects the finding).

## Not fixed
- Closed by decision: e2e messages that quote the CLI's own stdout or stderr are not rewritten. They reach only the job log, which the masks cover, and no artifact is uploaded from outside the redacted directory (checked by the workflow test).
- Closed: the two `@outlook.com` local parts in `internal/config` unit tests are not configured accounts (an independent scan of the tree with the configured values finds none).
- Planned elsewhere: a workflow lint in `verify lint` (rides on the verify-profile change). The rollup status of a queued check is read in this change's own PR gate.

## Unfinished
- None. `TestE2ERecordersRecordRolesNotDrives` runs only in the live suite, so the caller's live run exercises it.
