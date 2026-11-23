## Part 1: status rows

| W1 | DoD and nightly survive GitHub errors; CI logs name no drive | `ci/dod-nightly-hardening` | x | planned | | |
| W2 | Verify profiles stream every step and refuse an empty test tree | `ci/verify-profile-leftovers` | x | planned | | |
| W3 | e2e tests wait for what they read; no name-only waits | `test/e2e-wait-leftovers` | x | planned | | |
| W4 | Graph client: retries rebuild bodies, create marks are honest, quirks recorded | `fix/graph-client-leftovers` | o | planned | | |
| W5 | Pending-create records end once their file is settled | `fix/pending-create-lifetime` | x | planned | | |
| W6 | Control client: one probe, honest errors, one runtime per sync | `fix/control-client-leftovers` | o | planned | | |
| W7 | The control server's closing goroutine gets an owner | `fix/control-server-closer` | o | planned | | |
| W8 | Engine entry points carry their collector and wrap load errors once | `fix/engine-entry-leftovers` | s | planned | | |
| W9 | Planner: unreachable branches refuse, equal-content edit decided, dead code removed | `fix/planner-leftovers` | s | planned | | |
| W10 | Issue types: exhaustive verdicts and keys, names that say what they hold | `fix/issue-type-leftovers` | s | planned | | |
| W11 | Stored retry and trial times saturate instead of wrapping past 2262 | `fix/store-time-saturation` | s | planned | | |
| W12 | Sync test harness: load-proof waits, chaos summaries, session route covered | `test/sync-harness-leftovers` | s | planned | | |
| W13 | Config writers: one write per expired pause, a context, portable locks | `fix/config-writer-leftovers` | o | planned | | |
| W14 | fsroot: precise absence errors and directory sync on FUSE and SMB | `fix/fsroot-leftovers` | o | planned | | |
| W15 | A live test loses a create's answer on the wire | `test/e2e-lost-create-answer` | x | planned | | |

## Part 2: entries

All paths are relative to the repository root and were checked at main `05336a1a` on 2026-11-11. No two sweep PRs change the same file. Every line of `SWEEP.md` marked TODO is in exactly one entry below.

#### W1. DoD and nightly survive GitHub errors; CI logs name no drive

- **Source**:
  - Reviewer: one transient HTTP 5xx from GitHub (502 four times in a row on 2026-11-11, a 503 earlier) ends the DoD start stage. The job-log fetch in `internal/devtool/nightly.go:644-650` calls `gh` once and returns its error; so do the run list (`:590-601`) and the run view (`:620-624`).
  - A2 report, recommendation 2: `nightlyJob` (`internal/devtool/nightly.go:85-89`) and `ciRunJob` (`internal/devtool/verify_dod_ci_wait.go:163`) are two private types for the same GitHub job JSON.
  - A2 report, recommendation 5: `internal/devtool/worktree.go:90` spells "origin/main" as a literal, and `internal/devtool/cleanup_audit.go:33` declares a second constant beside `originMainRef` (`internal/devtool/verify_dod.go:24`).
  - A8 report, recommendation 1: `verify failed-test` reruns with plain `runner.Run` (`internal/devtool/verify_failed.go:56`), so a rerun whose `-run` pattern matches nothing passes.
  - A9 report, recommendation 4, and the reviewer: two hand-written go test log parsers exist. `lastFailedGoTestTarget` (`internal/devtool/verify_failed.go:63-149`) needs raw lines without a CI timestamp prefix (`:75-81`) and guesses JSON from the first `{` anywhere in a line (`:106-108`); `failedTopLevelTests` (`internal/devtool/nightly.go:668-690`) searches with `strings.Index` instead.
  - A9 report, recommendation 3: neither parser reads the "running tests:" block of a test-timeout panic, so a hung test is reported under its job's name, with no test named.
  - A9 report, recommendation 5 and its deviation, and the reviewer: `ciStallWindow` (`internal/devtool/verify_dod_ci_wait.go:99-121`) takes the longest `timeout-minutes` anywhere in `ci.yml`. With #878 that is the 180-minute e2e job, so every wait, post-merge included, waits 190 minutes for a stall, although a push to main runs only the 15- and 20-minute jobs.
  - A9 report, recommendation 1, and the reviewer: `.github/workflows/ci.yml:262-263` and `:369-371` set `ONEDRIVE_TEST_DRIVE` and `ONEDRIVE_TEST_DRIVE_2` in job-level `env`, so GitHub prints their values in every step's environment header in public logs.
  - A2 report, recommendation 3: nothing in `ci.yml` (`:88`, `:131`) logs the runner's core count, which the unit-test deadline formula (20 minutes times ceil(3 / GOMAXPROCS)) assumes.
- **Change**:
  - Every `gh` call of the nightly check and the CI waits goes through one helper that retries an HTTP 5xx or a dropped connection with backoff, at most four attempts, and then fails with a message that says GitHub answered with that status and names the call.
  - One `githubJob` type replaces `nightlyJob` and `ciRunJob`.
  - `AddWorktree` and the cleanup audit use `originMainRef`; the literal and the second constant go.
  - `verify failed-test` reruns through `runGoTestMatching`, so a rerun that selects no test fails.
  - One go test log parser replaces both: it strips a CI timestamp prefix, decodes only lines that are go test JSON events, reads raw `--- FAIL` and package `FAIL` lines, and names the test listed in a timeout panic's "running tests:" block. The nightly check, `verify failed-test` and the per-bucket failed-test lists #878 adds all use it.
  - The stall window is the longest timeout among the jobs that run for the event being waited on (pull request, or push for post-merge), read from each job's `if:` condition; the PR's e2e job gets a timeout that fits the quick tier it runs on a PR (see Order).
  - The drive identifiers leave job-level `env`: a first step reads the repository variables, masks each with `::add-mask::`, and exports it through `$GITHUB_ENV`.
  - Each verify job prints the runner's core count once (`nproc`, or `sysctl -n hw.ncpu` on macOS).
- **Tests first**: `TestNightlyCheck_RetriesAJobLog5xxThenSucceeds`, `TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound`, `TestRunFailedTestVerification_PatternThatSelectsNothingFails`, `TestParseGoTestLog_TimestampedRawLines`, `TestParseGoTestLog_BraceInARawLineIsNotJSON`, `TestParseGoTestLog_TimeoutPanicNamesTheRunningTest`, `TestCIStallWindow_PostMergeUsesOnlyPushJobs`, `TestCIStallWindow_PullRequestUsesOnlyPullRequestJobs`, and `TestCIWorkflowSetsNoTestDriveInJobEnv`, which reads `ci.yml` as `TestCIJobTimeoutsCoverEveryGoTestDeadline` does. The shared type and the constant are refactors with no failing test.
- **Docs**: `spec/design/system.md` (nightly check, failed-test, the CI wait and its stall window, the credential steps); the requirement lines in `spec/requirements/non-functional.md` that state the stall window and the nightly check; the repository's `CLAUDE.md` where it states the stall window.
- **Order**: after A9 (#878 changes `verify_failed.go`, adds the failed-test lists in `verify_e2e.go`, and edits the credential steps and job timeouts of `ci.yml`). Before A10, which rewrites `nightly.go` and the post-merge run selection in `verify_dod_ci_wait.go`, so that A10 builds on the retrying call and the one parser; before A11, which adds jobs to `ci.yml`. Size M. Design decisions: how the e2e job's timeout fits the PR tier (an expression on `timeout-minutes`, or a separate quick-tier job), without which a PR's stall window stays 190 minutes; whether the drive identifiers stay masked repository variables or become secrets.

#### W2. Verify profiles stream every step and refuse an empty test tree

- **Source**:
  - A9 report, recommendation 2: the stress profile's x3 step still runs with `CombinedOutput` (`internal/devtool/verify_stress.go:195`), so a hang shows no output until the step ends.
  - A8 report, recommendation 3: `assertTestsMatchedInTree` (`internal/devtool/verify_test_match.go:102-105`) passes any output that lacks the "no tests to run" marker, including a tree whose packages all print "[no test files]".
  - A2 report, recommendation 1: the benchmark builds its subject with plain `os.Environ()` (`internal/devtool/bench.go:621-624`), so with the host's Go 1.27 instead of the toolchain pinned in `go.mod`.
  - A8 report, recommendation 2: the unit tier always runs with `-coverpkg=./...` and `-coverprofile` (`internal/devtool/verify.go:577-599`), also on the macOS leg, where the threshold is -1 and coverage is only reported.
  - A8 report, recommendation 4: nobody measured how long a local `verify default` takes on this two-core host since `-coverpkg=./...` instruments every package in every test binary.
- **Change**:
  - The x3 step streams through the same tee as the other stress steps and keeps its slow-test summary.
  - The tree guard fails when no package ran any test, including a tree with no test files; the fake runners print realistic go test output.
  - The benchmark builds its subject with the pinned toolchain (`withPinnedToolchain`).
  - Read the macOS full leg's time from the next full-tier run: if it is within five minutes of its deadline, a negative threshold skips `-coverprofile` and `-coverpkg`; otherwise record the time in `spec/design/system.md` and change nothing.
  - Measure one local `verify default` and record it in the repository's `CLAUDE.md` verification tiers.
- **Tests first**: `TestRunVerifyStressStreamsTheRepeatStep` (output reaches stdout before the step returns), `TestAssertTestsMatchedInTree_RejectsATreeWithNoTestFiles`, `TestPrepareBenchSubject_BuildsWithThePinnedToolchain`; only if the measurement calls for it, `TestRunVerifyReportOnlyCoverageWritesNoProfile`.
- **Docs**: `spec/design/system.md` (stress lane, the tree guard, the benchmark, the macOS measurement); the repository's `CLAUDE.md` (the measured local time).
- **Order**: after A9 (#878 changes `verify.go`, `verify_stress.go` and `verify_test_match.go`). A11 (new `race` and `seeds` profiles) and A12 (deleted checks) also edit `verify.go`; land W2 before whichever of them starts later, or rebase it. Size S. No design decision; the coverage-profile change depends on a measurement.

#### W3. e2e tests wait for what they read; no name-only waits

- **Source**:
  - A5 report, recommendation 1: `TestE2E_SyncWatch_RapidChurn` (`e2e/sync_watch_full_test.go:465`) and `TestE2E_SyncWatch_GracefulShutdown` (`:528`) keep name-only waits that the content and size waits beside them now cover.
  - A5 report, recommendation 2: `e2e/sync_full_test.go:593` and `:647` wait on a name for files that were modified, not created; `e2e/cli_commands_e2e_test.go:654` runs a `--force` overwrite, and its callers read the destination without waiting for the new content.
  - A5 report, recommendation 3: on timeout, `pollRemoteEventually` (`e2e/remote_read_helpers_test.go:176-178`) prints `get`'s stdout, which is not the downloaded file, so the last content seen is unknown.
  - A5 report, recommendation 4: no check stops a new name-only wait in a test that starts a daemon; the rule is prose in `spec/design/system.md`.
  - A5 report, recommendation 5: sixteen of the seventeen live tests #867 changed run only in the full tier, and nobody has read their full-tier result.
  - A6 report, recommendation 1: `TestE2E_Sync_NamingCorpusRoundtrip` (`e2e/sync_hardening_full_e2e_test.go:235`) checks content unpolled after a best-effort `--download-only` pass.
  - A6 report, recommendation 2: `findActiveShortcutChild` (`e2e/shared_items_helpers_test.go:541-542`) asserts the record's state inside a poll predicate, so one transient non-active state fails the test.
  - A6 report, recommendation 3: `TestE2E_Sync_DirectionalModes_PreserveCreateCreateConflict` (`e2e/sync_full_test.go:819`) and `TestE2E_Sync_DryRunNonDestructive` (`:937`) run their single pass straight after a remote put, before delta shows it.
  - A4 report, recommendation 3: no end-to-end test asserts that `status --perf` shows requests for a syncing mount; `e2e/status_helpers_test.go:74` parses the field and nothing reads it.
- **Change**:
  - Delete the two redundant name waits.
  - Modified files wait for their new content with `waitForRemoteFileContent`; the `--force` callers wait for the overwritten content before reading it.
  - On timeout, the content wait prints the bytes it last downloaded.
  - A source-scan test in `e2e` fails when a test that starts a daemon (`cmd.Start`, `startDaemon`, `startFastDaemon`, `withShortcutWatchDaemon`) calls a name-only wait helper.
  - NamingCorpus's download pass becomes a converge whose predicate is the content it then checks.
  - `findActiveShortcutChild` returns the record, and its callers assert the state after convergence.
  - The two single-pass tests wait until delta shows the remote put before their one pass.
  - A watch test runs `status --perf --json` while the daemon syncs one file and asserts a non-zero request count for that mount.
  - Before starting, read the full-tier e2e results for the sixteen tests in the A6 dispatch run 43092588842 (it contains #867) or the next nightly; fix here any that failed.
- **Tests first**: `TestE2EDaemonTestsUseNoNameOnlyWait` (fails today on the two waits above); `TestE2E_SyncWatch_StatusPerfCountsRequests` (live; it would have failed before #862). The other changes are proven by one live run of each touched test, one at a time.
- **Docs**: `spec/design/system.md` Verification Policy (the rule is now enforced, with the test's name); `spec/reference/live-recurring-families.md:137` (the family's policy lines).
- **Order**: after A6 (#877 rewrites the converge helpers and touches `sync_full_test.go`, `shared_items_helpers_test.go` and `sync_hardening_full_e2e_test.go`) and after B2 (it changes the helpers' exit-code handling and audits every direct `sync` call file by file); before A13, which renames tests across `e2e`. Size M. No design decision.

#### W4. Graph client: retries rebuild bodies, create marks are honest, quirks recorded

- **Source**:
  - H3 report, recommendation 1: `do` (`internal/graph/client_auth.go:18`) takes an `io.Reader`, and the 401 retry rewinds that reader (`rewindBody`, `:313-323`). Since #868 a body that cannot be rewound fails loudly, but the signature still accepts one.
  - H3 report, recommendation 2: `Seek(0, io.SeekStart)` (`internal/graph/client_auth.go:323`) rewinds to the start of the slice, not to where the reader stood when the call began.
  - A14 report, recommendation 3: `createOutcomeUnknown` (`internal/graph/item_create.go:118-122`) marks every error that is not a `*GraphError` as "create outcome unknown", including token-source and request-building failures where nothing was sent; the intent is then kept until it expires.
  - A1 report, recommendation 4: `internal/graph/upload_transfer.go:330` bounds a session cancel with `shutdownTimeout`, the sign-in callback server's constant (`internal/graph/auth.go:87-88`), and `internal/graph/auth_browser.go:105` uses that constant as a header read timeout.
  - A1 report, recommendation 2 and deviation 10: `RemoveEmptyItem` (`internal/graph/item_create.go:246-248`, `:294-297`) makes three immediate attempts. Whether an orphaned upload session keeps moving the empty item's eTag, and whether the canceled request reached the server, was inferred and never probed.
  - A3 report, recommendation 4: whether opening and canceling a session moves an empty item's mtime and hash on OneDrive for Business was never probed; `spec/reference/graph-api-quirks.md:1146` describes Personal only.
  - A7 report, recommendation 2, and the reviewer: no quirk or incident entry records the HTTP 400 that Graph gave a folder create in the 2026-11-11 nightly (`TestE2E_Sync_IdempotentReSync`).
  - B5 report, recommendation 3: `spec/design/graph-client.md:22-24` lists the profile builders without the `notices` parameter each one takes (`internal/graphtransport/profiles.go:47-59`).
- **Change**:
  - The 401 retry builds its second request from the first request's `GetBody`, so it carries exactly the bytes the first request sent; `rewindBody` and its Seek go.
  - A failure before the request leaves (token source, request building) carries a not-sent mark and is never "create outcome unknown".
  - Named constants for the session-cancel bound and for the callback server's header timeout.
  - A live probe on the test account: create an empty item, open and cancel an upload session client side, read the eTag at once and over one minute, try the conditional delete. Record the result in the quirk entry. If the eTag keeps moving, `RemoveEmptyItem` waits briefly between attempts.
  - The same probe on Business only if a Business test drive exists; otherwise the quirk entry says it was not probed and why.
  - A quirk entry and a ledger entry for the folder-create 400: the request, the answer, and why it is classed as transient.
  - The profile list in `graph-client.md` gets the `notices` parameter.
- **Tests first**: `TestDo_401RetrySendsTheBytesOfTheFirstRequest` (a reader advanced before the call), `TestCreateFile_TokenFailureIsNotOutcomeUnknown`, `TestCreateFile_RequestBuildFailureIsNotOutcomeUnknown`; only if the probe shows a moving eTag, `TestRemoveEmptyItem_WaitsBetweenRefusedDeletes`.
- **Docs**: `spec/design/graph-client.md` (profiles, 401 Token Refresh, Transfers); R-6.8.14 in `spec/requirements/non-functional.md`; `spec/reference/graph-api-quirks.md`; `spec/reference/live-incidents.md`.
- **Order**: before B6 (it changes the token source and `graph-client.md`) and before G2 (it changes `readCreatedFileByName` in `item_create.go`). Size M. Design decisions: a `GetBody`-based retry (smaller) or `do` taking `[]byte` (stronger, touches every caller); the backoff only if the probe shows the need; the Business probe needs a Business test drive.

#### W5. Pending-create records end once their file is settled

- **Source**:
  - A3 report, recommendation 1: `SyncStore.ClearPendingCreate` (`internal/sync/store_pending_creates.go:138-141`) has no caller.
  - A3 report, recommendation 2: when Graph's cleanup reports the empty item removed, `removePlaceholder` (`internal/driveops/transfer_session.go:471-483`) only logs it, and the pending record of that item stays until observation sees the deletion.
  - A3 report, recommendation 3, and A14 report, recommendation 2: a row naming an item is dropped only when observation sees the item gain content or disappear (`internal/sync/store_pending_creates.go:26-36`). A zero-byte file never gains content, so its row stays for good, and a later mtime-only remote change is absorbed as the placeholder following its eTag.
  - A14 report, recommendation 1: in `settleUploadedItem` (`internal/driveops/transfer_verify.go:186-198`), a failed lookup after `ErrUploadCompletedWithoutItem` wraps only the lookup error and drops the upload's own. Checked today: no create mark is lost, because a create never returns that error (`internal/graph/item_create.go:127-146`); the drop would hide any future mark.
- **Change**:
  - Once a create's outcome is committed to the baseline, the row naming that item is dropped; `ClearPendingCreate` becomes that call, or is deleted if the drop lives inside the commit transaction.
  - A removal the cleanup reports done drops the record of that item.
  - A zero-byte file's row goes once its committed outcome is in the baseline, so a later remote change reads as a change.
  - The lookup failure joins the upload error with the lookup error.
- **Tests first**: `TestUploadNewFile_ZeroByteCreateLeavesNoPendingRow`, `TestRunOnce_ClaimedZeroByteFileThenRemoteMtimeChangeIsARemoteChange`, `TestSessionUpload_RemovedPlaceholderClearsItsRecord`, `TestSettleUploadedItem_LookupFailureKeepsTheUploadError`.
- **Docs**: the comment block at the top of `internal/sync/store_pending_creates.go`; `spec/design/sync-store.md` (Pending creates); `spec/design/drive-transfers.md` (New files and their placeholder); R-5.6.15 in `spec/requirements/transfers.md`.
- **Order**: after A15 and A16 (A16 rewrites this store file's comment block and adds the expiry prune, and both use `pending_creates_test.go`) and after B8 (it changes `executor_transfer.go`). H5 edits another section of `drive-transfers.md` (its lines 313-323) and does not conflict. If W11 has landed, `intended_at` uses its time helper. Size M. Design decisions: how a done removal reaches sync (a driveops callback beside the create hook, or a typed result sync reads); that dropping the row at outcome commit keeps the kill-mid-create recovery A16 checks.

#### W6. Control client: one probe, honest errors, one runtime per sync

- **Source**:
  - B3 report, recommendation 1, and the reviewer: the one-shot `sync` refuses early only for a watch or one-shot owner (`internal/cli/sync_flow.go:96-99`); an unresponsive owner gets the "Checking ..." banner and then fails on the lock.
  - B3 report, recommendation 2, and the reviewer: `status --perf` probes the control socket twice (`internal/cli/status_runtime.go:27` and `internal/cli/status_perf.go:34`), so a busy watch costs two 2-second probe timeouts.
  - B3 report, recommendation 3, and the reviewer: `decodeControlJSONResponse` (`internal/cli/control_client.go:316-326`) turns every decode failure into `internal_error` and flattens its cause to text, so a body-read timeout on a 2xx answer never reads as "did not answer".
  - B5 report, recommendation 1 and its deviation: `sync` builds a second `SessionRuntime` for its engine (`internal/cli/sync_flow.go:186`, `internal/cli/sync_runtime.go:187`), so one process keeps two token-source caches; `spec/design/sync-control-plane.md` records the unification as planned.
  - B5 report, recommendation 2: `sharedBootstrapMetaClient` and `sharedTargetClients` (`internal/cli/shared_target.go:146`, `:175`) do not flush the token cache after a probe renames the account, unlike `cc.Session` (`internal/cli/root.go:366-371`).
  - A4 report, recommendation 4: `status --perf` reads only the per-mount snapshots (`internal/cli/status_perf.go:136-160`); the owner aggregate that `GET /v1/perf` returns is never shown.
- **Change**:
  - The early refusal also refuses for an unresponsive owner, with the existing "already running" error.
  - `status` probes once and gives the result to both overlays.
  - A decode failure caused by a timeout or deadline is classified as "did not answer"; every decode error keeps its cause with `%w`.
  - `sync` uses one session runtime for its probe and its engine; the daemon receives the command's runtime with the timestamped logger.
  - The shared-target clients flush the token cache when the probe renames the account (or `probeAccountIdentity` flushes on any rename).
  - Decide whether `status --perf` shows the owner aggregate; implement it, or write the decision into `cli.md`.
- **Tests first**: `TestRunSyncCommand_UnresponsiveOwnerRefusesBeforeTheBanner`, `TestRunStatusCommand_PerfProbesTheSocketOnce`, `TestDecodeControlJSONResponse_BodyTimeoutReadsAsDidNotAnswer`, `TestRunSync_ProbeAndEngineShareOneSessionRuntime`, `TestSharedTargetClients_FlushTheTokenCacheAfterARename`; a status test for the aggregate if it is shown.
- **Docs**: `spec/design/cli.md` (Control Socket client, status); `spec/design/sync-control-plane.md` (the planned one-runtime row); R-6.8.27 in `spec/requirements/non-functional.md` can then say "within a process".
- **Order**: after B2 (it changes the summary and exit status in `sync_flow.go`); before B6, which carries token-save errors out of `SessionRuntime` and should find one runtime per process. Size M. Design decisions: whether `status --perf` shows the aggregate (a product decision); merging the runtimes changes the daemon's runtime logger and eight `runSyncDaemonWithFactory` test calls.

#### W7. The control server's closing goroutine gets an owner

- **Source**: B3 report, recommendation 4: `startControlSocketServer` (`internal/multisync/control_socket.go:178-190`) starts a goroutine that waits for the context to end and then closes the server; nothing waits for that goroutine.
- **Change**: remove the goroutine if every owner already closes the server after the drain (B8's readiness notes assume they do), or make `Close` wait for it.
- **Tests first**: `TestStartControlSocketServer_CancelLeavesNoRunningGoroutine`.
- **Docs**: `spec/design/sync-control-plane.md` (Control Socket).
- **Order**: after B8 and B9, which both change `control_socket.go`. If B8 removes the goroutine, record this item as done by B8 and drop W7. Size S. No design decision.

#### W8. Engine entry points carry their collector and wrap load errors once

- **Source**:
  - A4 report, recommendation 1: `RunOnce` (`internal/sync/engine_run_once.go:31`) and `RunWatch` (`internal/sync/engine_watch.go:58`) do not put the engine's own perf collector on their context; only the multisync owners do (`internal/multisync/watch_runner.go:88`, `internal/multisync/orchestrator_run_once.go:249`), so an owner that forgets loses every count.
  - A4 report, recommendation 2: `Engine.Close` (`internal/sync/engine.go:429`) runs under the orchestrator's context, so anything it records counts on the command collector only; it records nothing today.
  - B4 report, recommendation 2: `normalizePersistedAccountAuthRequirement` (`internal/sync/engine.go:530-532`) fails the mount's start-up when another process holds the catalog lock (`config.ErrConfigLocked`), where the fatal-auth path only warns.
  - C1 report, recommendation 3: planning-state load errors are wrapped three ways by entry point (bare in one-shot, "reading sync state:" at `internal/sync/engine_current_plan.go:76`, "sync: reading sync state:" at `:95`), and a comparison query error is wrapped twice (`internal/sync/engine_current_plan.go:589` around `internal/sync/sqlite_compare.go:798`).
  - A3 report, recommendation 5: `internal/sync/doc.go:24` says the watch loop owns "graceful drain", the wording #864 retired elsewhere.
- **Change**:
  - `RunOnce`, `RunWatch` and `Close` wrap their context with the engine's collector when one is set; a repeated wrap with the same collector changes nothing.
  - A locked catalog at start-up logs one Warn, keeps the requirement and lets the engine start; the next start clears it.
  - One wrap for planning-state load errors in every entry point; the outer comparison wrap goes (the change stays in `engine_current_plan.go`).
  - `doc.go` says the loop owns shutdown: cancel in-flight actions, then drain their completions.
- **Tests first**: `TestRunOnce_CountsOnTheEngineCollectorWithoutAnOwnerWrap`, `TestRunWatch_CountsOnTheEngineCollectorWithoutAnOwnerWrap`, `TestEngineStartup_LockedCatalogWarnsAndStarts`, `TestPlanningLoadError_SameTextInOneShotAndWatch`.
- **Docs**: `spec/design/sync-engine.md` (start-up, the perf context); `spec/design/sync-control-plane.md` (the list #862 wrote of work outside the mount collector).
- **Order**: before E1 (it edits `engine.go` and `engine_watch.go`), E2 (`RunWatch`) and E7 (`engine_current_plan.go`); before W13, which changes the signature of `config.ClearAccountAuthRequirement` that this start-up path calls. B8 does not touch these files. Size S. No design decision.

#### W9. Planner: unreachable branches refuse, equal-content edit decided, dead code removed

- **Source**:
  - C1 report, recommendation 4: `writesLocally` (`internal/sync/planner_actions.go:215`), two switches in `internal/sync/planner_dependencies.go` (`:287`, `:519`) and `createSide` (`internal/sync/planner_move_composition.go:1492`) return a quiet value for an action type no case lists.
  - C1 report, recommendation 1: the reconciliation cell for a diverged row whose local side changed, remote side did not, and whose content is equal is `upload` (the CASE at `internal/sync/sqlite_compare.go:606`, pinned by `TestQueryReconciliationState_LocalEditEqualToUnchangedRemoteIsAnUpload`, `internal/sync/sqlite_compare_test.go:692`), so sync uploads bytes OneDrive already holds.
  - C1 report, recommendation 2: `actionType.Direction()` and the `direction` type (`internal/sync/enums.go:37`, `:221`) have no production caller; only `internal/sync/core_types_test.go:121` uses them.
- **Change**:
  - An unlisted action type is refused: the plan's validation rejects any action type outside the enum before these functions run, and each default branch says it is unreachable instead of returning a value that looks like a decision.
  - Decide the equal-content cell. If it becomes `baseline_update`, change the reconciliation CASE and flip the pinning test; if `upload` stays, write the reason beside the test.
  - Delete `Direction()`, the `direction` type and their test row.
- **Tests first**: `TestPlanCurrentState_UnlistedActionTypeFailsThePlan`; the flipped pinning test and an updated `TestReconcileMatrix_Golden` if the cell changes.
- **Docs**: `spec/design/sync-planning.md` (Decision Table Kinds); `spec/design/sync-domain.md` (the Direction row).
- **Order**: before G1 (it retypes planner helpers), before the S wave (it moves these files) and before F3 (its Go oracle must mirror the decided cell). Size S. Design decision: the equal-content cell, a product decision.

#### W10. Issue types: exhaustive verdicts and keys, names that say what they hold

- **Source**:
  - C2 report, recommendation 1, and the reviewer: `observationIssueBlocksRemoteTruth` (`internal/sync/truth_status.go:172-174`) is two equality tests, so an unknown issue type does not block remote truth; the local verdict has failed closed since #875.
  - C2 report, recommendation 3, and the reviewer: `conditionKeyForIssueType` (`internal/sync/condition_keys.go:153-178`) is a lookup table, so a new issue type silently maps to "unexpected condition".
  - C2 report, recommendation 4, and the reviewer: `hash_panic` (`internal/sync/issue_types.go:30`) is recorded for ordinary read errors too (`internal/sync/scanner.go:289`, `:324`), and its condition key is `ConditionHashError` (`internal/sync/condition_keys.go:175`). The value is stored in `observation_issues`, and since #875 an unknown stored value fails the read, so a rename needs a new store generation.
  - C2 report, recommendation 5, and the reviewer: `fallbackFailureSummaryIssueType` (`internal/sync/engine_results.go:15`, used at `:101` and `:135`) is a summary and log key, not an issue type.
- **Change**:
  - Both functions become exhaustive switches over `IssueType`; a non-member blocks remote truth and maps to a condition chosen on purpose.
  - Rename `hash_panic` to a name for "the file could not be read or hashed" (for example `hash_error`, matching its condition key) in code, store and docs.
  - Rename the summary key so its name says what it is.
- **Tests first**: `TestObservationIssueBlocksRemoteTruth_ValueOutsideTheEnumIsUnavailable`; `TestConditionKeyForIssueType_EveryMemberIsDecided`, which reads the members from source as #875's verdict test does; a store test that a store of the previous generation is refused after the rename.
- **Docs**: `spec/design/sync-domain.md` (issue types); `spec/design/sync-observation.md` (it already calls it a hash error); `spec/design/sync-store.md` (the generation).
- **Order**: after C4, which edits the managed issue set in `internal/sync/observation_findings.go`, a file the rename also touches. B2 adds a table that classifies every issue type for the one-shot exit status; whichever lands second carries the renamed constant. Size M. Design decision: the rename's store generation rides C3's generation 29 (the rename inside C3, or W10 first and C3 becomes 30), or takes its own bump with the reset procedure.

#### W11. Stored retry and trial times saturate instead of wrapping past 2262

- **Source**:
  - H2 report, recommendation 1: `internal/sync/store_write_block_scopes.go:56` stores `NextTrialAt.UnixNano()`. For a time past 2262, which a saturated `Retry-After` produces since #866, that value is undefined, so after a restart the trial is due at once.
  - H2 report, recommendation 2: the same conversion writes other stored times (`internal/sync/store_retry_work.go:202`, `internal/sync/store_scope_admin.go:24`). Checked today: only the block-scope trial takes a server-given wait (`internal/sync/engine_runtime_lifecycle.go:279` through `computeTrialInterval`, `internal/sync/scope_lifecycle_policy.go:88-93`).
- **Change**: one helper converts a time to stored nanoseconds and saturates at `math.MaxInt64`; the block-scope, retry-work and scope-admin writes use it, and the read side maps the saturated value back to "not before the far future".
- **Tests first**: `TestUpsertBlockScope_SaturatedTrialSurvivesAReload` (write, reopen the store, the trial is not due).
- **Docs**: `spec/design/sync-store.md` (time columns); `spec/design/retry.md`, section "Reading `Retry-After`" (saturation survives a restart).
- **Order**: before E4 (one write path for retry work in `store_retry_work.go`) and before C3 (it rewrites the block-scope write). W5 can adopt the helper for `pending_creates.intended_at`. Size S. Design decision: saturate, which keeps decision D3's "no cap", or refuse such a time at the store boundary.

#### W12. Sync test harness: load-proof waits, chaos summaries, session route covered

- **Source**:
  - Reviewer: watch tests in `internal/sync` wait ten seconds for an event and fail under host load alone: `debugEventTimeout` (`internal/sync/engine_test_helpers_test.go:586`) and literal `time.After(10 * time.Second)` waits in `internal/sync/engine_watch_test.go:83`, `:92`, `:303`, `:355`, `:530`, `:583`, `:593`, `internal/sync/engine_watch_panic_recovery_test.go:145` and `internal/sync/observer_local_watch_health_test.go:139`, `:572`.
  - A7-seeds and the reviewer: `TestModel_ChaosGraphConverges` logs the fault-kind summary only after the run returns (`internal/sync/chaos_remote_test.go:535-537`), so a failing seed never prints it.
  - A7 report, recommendation 3, and the reviewer: A7's fake upload-session client `scenarioSessionClient` (`internal/sync/scenario_remote_session_test.go`, on A7's branch, not yet on main) does not go through the chaos layer, so the storm never covers the large-file route.
  - A7 report, recommendation 4, and the reviewer: under `-race` the chaos model runs seed 1 only (`internal/sync/chaos_remote_test.go:551-553`), and that seed draws no create or session fault.
- **Change**:
  - One named wait for event-driven watch tests, long enough for a loaded two-core host (for example 60 seconds; a wait returns as soon as its event arrives), replaces the ten-second literals.
  - The fault summary is logged through `t.Cleanup`, so it prints for a failing seed too.
  - The session client's requests pass through the chaos layer.
  - The race run uses a seed that draws a create fault and a session fault, and asserts that it drew them.
- **Tests first**: the race run's new assertion (it fails today with seed 1); `TestChaosGraph_SessionRouteDrawsFaults`. The wait change has no failing test; it is proven by the watch tests passing under `-race -cpu 1` beside a parallel load.
- **Docs**: `spec/design/system.md` Verification Policy (the wait rule for event-driven tests).
- **Order**: after A7 (it adds the session client and edits `chaos_remote_test.go`); before A11, whose seed helper replaces `chaosSeeds` and should keep the summary and the race-seed rule. Size M. No design decision.

#### W13. Config writers: one write per expired pause, a context, portable locks

- **Source**:
  - B4 report, recommendation 1: `ClearExpiredPauses` (`internal/config/resolver.go:178-179`) deletes `paused` and `paused_until` in two separately locked writes, so a stuck lock holder stalls a reload up to 20 seconds per expired drive.
  - B4 report, recommendation 3: config and catalog writers take no context (`internal/config/mutation_lock.go:61-71`), so Ctrl-C cannot end the ten-second lock wait.
  - B4 report, recommendation 4: the lock is a flock on the directory's descriptor (`internal/config/mutation_lock.go:82-100`); NFS (EBADF) and FUSE (ENOSYS) may refuse it, and then every config change fails. Unconfirmed.
- **Change**:
  - Both pause keys go in one locked write.
  - Every config and catalog writer takes a context, and the lock wait ends on `ctx.Done()`.
  - Probe a directory flock on a FUSE mount on this host (for example sshfs). If it is refused, fall back to a lock file beside the config, with an unsupported-errno rule like the sync root lock's (`isUnsupportedLockErrno`, `internal/sync/engine_root_lock.go:77`, which `config` cannot import); if it is not, record the result in `spec/design/config.md` and change nothing.
- **Tests first**: `TestClearExpiredPauses_OneLockedWritePerDrive`, `TestWithConfigMutationLock_CanceledContextEndsTheWait`; only if the probe shows the failure, `TestWithConfigMutationLock_UnsupportedDirectoryLockUsesALockFile`.
- **Docs**: `spec/design/config.md` (the lock); R-4.4.4 in `spec/requirements/configuration.md`; `docs/troubleshooting.md` (the lock section).
- **Order**: after H4 (`config.AddDrive` takes both locks through `mutation_lock.go`) and after W8 (it changes the start-up path that calls `config.ClearAccountAuthRequirement`, whose signature W13 changes). The context reaches callers in `internal/cli`, `internal/multisync` and `internal/sync`; if one of them is in W6's files, land W6 first. Size M. Design decision: how far the context reaches in one PR.

#### W14. fsroot: precise absence errors and directory sync on FUSE and SMB

- **Source**:
  - H1 report, recommendation 1: `normalizeNotExist` (`internal/fsroot/fsroot.go:598-604`) answers `os.ErrNotExist` for any failure on an absent name, so a permission or disk-full error on a create reads as "missing".
  - H1 report, recommendation 2 and its deviation: `syncDirectory` (`internal/fsroot/fsroot.go:544-549`) reports every directory-sync error, so a filesystem that refuses it (EINVAL or ENOTSUP on FUSE or SMB home folders) fails every config and token write.
  - H1 report, recommendation 3: `Root.CreateTemp` (`internal/fsroot/fsroot.go:391`) is exported, but nothing outside the package calls it.
  - H1 report, recommendation 4: the rooted-boundary evidence row in `spec/design/threat-model.md:81` cites none of the containment tests #874 added.
  - H1 report, recommendation 5: R-6.4.10 (`spec/requirements/non-functional.md:58`) leaves the state database out, because SQLite opens it by path, without saying so.
- **Change**:
  - `normalizeNotExist` maps only the errors that mean absence (not-exist and not-a-directory, as `synctree` does) and returns every other error as it is.
  - The directory sync tolerates EINVAL and ENOTSUP, using the same test as `localpath.IsDirSyncUnsupported` (`internal/localpath/localpath.go:377`).
  - `CreateTemp` becomes unexported.
  - The threat-model row cites the `TestRoot_*_RefusesSymlinkedParent` tests.
  - R-6.4.10 and the managed-state bullet in `spec/design/system.md` say that the state database is opened by path and why.
- **Tests first**: `TestRoot_AtomicWrite_PermissionDeniedIsNotNotExist`, `TestRoot_AtomicWrite_ToleratesUnsupportedDirectorySync` (a fake handle whose directory `Sync` returns EINVAL).
- **Docs**: `spec/design/threat-model.md:81`; `spec/design/system.md` (the managed-state bullet at `:450` and after); R-6.4.10.
- **Order**: any time; no planned PR changes `internal/fsroot` (H9 changes `synctree`). Size S. Design decision: `fsroot` imports no internal package today, so either it may import `localpath` for the errno test or that test moves to a leaf package.

#### W15. A live test loses a create's answer on the wire

- **Source**: A14 report, recommendation 4: no live test loses a create's answer on the wire. The rule that a retry finishes into the file its lost create made (R-5.6.15) is proven only by fakes: the chaos fault `create:response-lost` in `internal/sync/chaos_remote_test.go` and the tests in `internal/sync/create_answer_lost_test.go`.
- **Change**: a test-only way to drop the answer to the first file create for one name in a live run (a fault-injecting HTTPS forward proxy started by the e2e harness, or a transport hook compiled only under the e2e build tag); then a one-shot sync and a second pass, asserting one remote file with the user's content and no conflict copy.
- **Tests first**: `TestE2E_Sync_CreateWhoseAnswerWasLostFinishesIntoItsOwnFile` (live; fails if the claim rule regresses).
- **Docs**: `spec/design/system.md` Verification Policy (the fault injection); the e2e family table if A13 has landed.
- **Order**: after W3 and A13 (the new test needs a family name). Size L. Design decision: the injection point, and whether a test-only hook in product code is acceptable. Doubt: the harness may cost more than the fakes already prove; the owner's rule keeps it planned.
