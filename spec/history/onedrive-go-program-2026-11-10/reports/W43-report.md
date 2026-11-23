# W43 report: e2e content reads and poll predicates (base 51e554fa, one commit)

## What changed, by file
- e2e/daemon_wait_rule_test.go: the scan also records, per function, its first call that can fail the test, whether it returns a value, and the functions it passes to a poll primitive or suite function (`bodyScan`, `failingCall`, `passedFunc`); the planted source gains predicates (one reporting, three failing, two that are no predicate of a wait); the three read-after-exit exceptions and their reason are gone, only the four shortcut entries remain.
- e2e/poll_predicate_rule_test.go (new, untagged): the predicate rule over the same parsed sources and its two tests.
- e2e/watch_debounce_test.go (new, untagged): reads `defaultDebounce` and `watchLocalDebounce` from their files and holds them equal; a reader for `N * time.Unit` that fails with what to do for any other form.
- e2e/remote_read_rules_test.go: `cliReportsRemoteItemMissing` (was `isRemoteNotFoundCleanup` in e2e_test.go) matches only the CLI's `Error: <path> was not found on OneDrive.` line with its optional list-parent hint; the pure predicates and their tests moved here from the tagged helpers file; new `remoteStatCreatedAt`, `remoteListingEmptyOrGone`.
- e2e/remote_read_helpers_test.go: predicates and pure tests moved out; `waitForRemoteStatSize(ForDrive)` returns the `stat --json` answer that showed the size.
- e2e/sync_watch_e2e_test.go: the three reads after the daemon exited are gone (each repeated a content wait for the same path, bytes and config).
- e2e/sync_watch_full_test.go: TimedPauseExpiry has no reload after the pause ends, checks `created_at` against the pause's end with `remoteClockAllowance` (2 s), and validates R-2.8.12; the debounce comment names the test that holds it.
- e2e/sync_demoted_full_e2e_test.go, e2e/sync_full_test.go: the put-overwritten conflict.txt and the recreated fragile.txt are read through the content wait; `120*time.Second` is `syncConvergeTimeout` (declared in e2e_test.go).
- e2e/sync_edge_cases_full_test.go: the nested-deletion setup predicate reports and the test asserts after the wait; the transfer-workers predicate waits through a failed listing; named timeouts and file count.
- e2e/sync_safety_full_e2e_test.go: the confirmed-deletes wait is `pollRemoteEventually` with a pure predicate (empty listing or folder gone); `remoteFileNames` fails on a failed listing instead of returning none.
- e2e/shared_items_helpers_test.go, shared_items_e2e_test.go, shared_items_fast_e2e_test.go: `discoverSharedFile` holds the discovery the two shared-file tests repeated; their own steps and assertions are unchanged.
- e2e/e2e_test.go, e2e/sync_e2e_test.go: callers of the new match; `syncConvergeTimeout`; `cleanupRemoteFolderAttempts`.
- spec/requirements/non-functional.md (R-6.10.17), spec/design/system.md (Verification Policy, line 524), spec/reference/live-recurring-families.md (the exception rule): sentences changed, nothing reflowed.

## Tests added (each seen failing first)
- TestPollPredicateScanReportsFailingPredicates: the planted direct, helper-borne and by-name failing predicates are reported, the others not. Failed against a stub returning nil.
- TestE2EPollPredicatesAssertNothing (R-6.10.17): no predicate in the suite can fail the test. Failed on the tree before the predicate fixes with exactly the three DoR findings.
- TestCLIReportsRemoteItemMissing: the sentence for rm/ls/stat, the root and "the item"; a debug line holding "not found" and one holding the sentence itself do not match. Three cases failed with the old match.
- TestDeleteDisappearanceReady (moved) gained a 503 whose debug log holds a 404: failed with the old match.
- TestRemoteStatCreatedAt, TestRemoteListingEmptyOrGone: failed with the code deliberately broken, then restored.
- TestWatchLocalDebounceMatchesEngineDefault (R-2.8.12), TestDurationConstValueReadsUnitProducts (R-2.8.12): failed against a stub reader; the first failed again with the copy set to 6 s, then restored.

## Checks
- go build ./..., go vet ./..., go vet -tags=e2e,e2e_full,integration,stress ./..., go vet -tags=e2e ./e2e/...: pass.
- gofumpt -l and goimports -l on every changed file: no output.
- go test -count=1 ./e2e/ (untagged): pass. devtool RealRepo|Governs|ListsEvery|Registered|Bucket tests, TestFindTestConventionViolations_RealRepoIsClean among them: pass.
- Lint behind the host lock (W43-lint.log): first run one goconst finding (the testify package names twice), fixed with `isTestifyPackage`; second run `0 issues.`; the convention test and the untagged e2e tests run again after: pass.
- Commit 8165483d on 51e554fa; `git status --short --untracked-files=no` is empty.
- 317 test names cited by the three spec files all exist; the spec diffs change one line each in system.md and non-functional.md and three lines (to two) in live-recurring-families.md.

## Deviations (scope)
- The predicate rule is wider than "literal passed to a poll primitive, direct calls": it also counts a literal (or a suite function by name) given to a suite function that reaches a primitive, and calls through suite functions. The narrow form finds nothing in the tree; the three faults found are one of each widening kind. Testify calls count only when given `t` (assert.ObjectsAreEqual compares). No declared-safe entry.
- Reads kept: copy.txt (exited cp), churn.txt (exited one-shot sync), local-deferred.txt (made by an exited sync, never rewritten: the read checks it was not), collision.txt and converge-create.txt (exited put). system.md names cp and a one-shot sync beside put.
- The live list leaves out the tests that only call `waitForRemoteStatSize`, which gained a return value and nothing else, and the per-test teardown: `cleanupRemoteFolderForDrive` and the suite scrub use the new missing-item match in every live run.
- Fixed in files touched: the confirmed-deletes wait read a failed listing as "no files"; magic numbers 4, 5 and 180 s; a stale `clearExpiredPauses` comment.

## P9 check
`pollRemoteEventually`'s timeout message goes through `remotePollTimeoutMessage(suiteDriveRoles, ...)`, held by TestRemotePollTimeoutMessageNamesDrivesByRole: holds, nothing to change.

## Seen and not fixed
1. e2e/sync_watch_e2e_test.go:244-254: PauseResume's negative check only logs and breaks when the file uploads while paused, so it cannot fail. Fix: fail on an answer other than the CLI's missing sentence through a fixed window, as requireNotUploadedDuringPause does (it needs a form without a pause end).
2. internal/devtool/bench_live.go:548: its own isRemoteNotFoundCleanup matches "not found" anywhere in stderr. Fix: match the CLI's `was not found on OneDrive.` error line.
3. e2e/poll_predicate_rule_test.go header: a predicate a suite function builds and returns (dryRunPlanHasLineStarting, the content-tree closures) or passes through a variable is outside the scan. Fix: follow a returned FuncLit of a called suite function.

## Live names
/tmp/scratch/scratchpad/W43-live-names.txt (23 names). No live test was run here.



## Review fixes (2026-11-14 about 21:30 PST; commit 9b1ada52 on 51e554fa, amended into the one commit)

Read-only review (findings in the scratchpad file R-W43-findings.md): "accept after your small fixes". Fixed by the reviewer of record, not the writer:

- PauseResume's negative check (seen-and-not-fixed 1) fails the test now: the log-and-break loop is `requireNotUploadedDuringPause`, the check TimedPauseExpiry uses.
- The commit subject was 81 characters; it is 76.
- The predicate rule counts the methods of `t` that fail or skip the test (`testEndingMethods`: Fatal, Fatalf, Error, Errorf, Fail, FailNow, Skip, Skipf, SkipNow), not four of them. The planted source gained a `t.Fatalf` predicate, a skipping one, one that asserts two calls down inside a function that waits only through another, and a by-name action without a result; `TestPollPredicateScanReportsFailingPredicates` expects six findings. The real-tree rule still passes: no predicate of the suite skips.
- TimedPauseExpiry reads the pause's end from `pause --json` (`pauseCommandEnd`, pure, with `TestPauseCommandEnd`) instead of estimating it from the test's clock; the comment and the commit message say which sends the test leaves out (no reload, resume or config change after the reload that applies the pause).
- The download-only wait of the directional test took any failed `stat` as the remote delete holding; it requires `cliReportsRemoteItemMissing(stderr)`.
- One convergence bound: `syncConvergeTimeout` is 3 minutes with the reason in its comment, and `emptyFolderDeleteConvergeTimeout` (3 minutes, one site) is gone. Eight other waits gain a minute that only a lagging or failing pass uses. Decision of the program lead; in the final report.
- `watch_debounce_test.go` reads the one form both declarations use (`N * time.Unit`); the two unused forms and `scaledUnit` are gone, and the planted reversed and bare declarations moved to the cannot-read cases.
- `// Validates:` lines on `TestCLIReportsRemoteItemMissing`, `TestRemoteListingEmptyOrGone` (R-6.10.17) and `TestRemoteStatCreatedAt` (R-2.8.12).
- spec/design/system.md line 524: the name-wait scan sentence no longer reads as covering the predicate rule; the predicate rule names the methods, says the scan does not check that the test asserts after the wait, and names the two shapes outside it; the "missing only from the CLI's own error line" sentence is scoped to a live test's wait or cleanup. R-6.10.17 says a predicate neither fails nor skips the test.
- internal/graph/items.go: a comment said an invalid timestamp falls back to now; it stays the zero time (comment only).

Checks after the fixes: gofumpt clean; `go test -count=1 ./e2e/` (untagged) pass; `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...` clean; the test-convention check passes; targeted lint over `./e2e/...` and `./internal/graph/...` reports 0 issues; the four source checks of devtool (RealRepoIsClean) pass.

Riders written for what stays open: the predicate scan does not follow a predicate a suite function builds and returns, nor one forwarded from a variable (rider on W47, in its plan section and its brief); `internal/devtool/bench_live.go` matches "not found" anywhere in stderr (seen-and-not-fixed 2; rider on A12, in its plan row and its brief).

For the final report: the three watch tests no longer read remote state after the daemon exits (the content waits check it before; nothing checks that the shutdown itself leaves the file alone); an unproven possible gap in TimedPauseExpiry (a create that failed inside the pause and was made again after it would date `created_at` late and pass; product path not checked).



## Merged as #916 (`cf345986`, 2026-11-15 00:18 PST)

- One commit, `c867b645` on `3a80f900`; the pre-pr gate passed at 22:58 on 11-14 (1545 s), the pre-merge stage in 42 s, the post-merge stage after the push CI of main.
- Live proof: 20 of the 23 live tests the change touches passed alone on the development host. The other three cannot run there (no second test drive, no shared-file fixture). A full-tier run of the branch, started by hand (run `43687851252`), passed all three: `TestE2E_SyncWatch_ControlSocketReload`, `TestE2E_Shared_FileDiscoveryAndSelectorRoundTrip`, `TestE2E_Shared_FileDiscoveryAndSelectorReadCommands`.
- That run's e2e job was red because of one other full-tier test, `TestE2E_Mv_Folder`: OneDrive answered its one folder move with HTTP 400 `Invalid request`. The cause is unknown. The change touches neither that test nor the move code; the test passed alone on the development host afterwards (42 s); no other test of the job failed, and the run's other jobs passed. I merged on that evidence and said so in the PR body.
- The 400 became plan row W56 the same day: neither this log nor the one of 2026-11-11 (LI-20261111-01) shows whether the answer carried an error code, because the client prints the code chain only in quirk-retry records. W56 makes the code visible; no retry rule is added until an occurrence with a recorded code exists.
- A correction to my own notes: the test did not "pass in the pull request's own run", as one of my summaries said. `e2e/cli_commands_e2e_test.go` is tagged `e2e && e2e_full`, so a pull request's e2e job never runs it. The claim was checked before use and written nowhere.
- Installed `cf345986` at 00:27 PST; the watch client reports the test drive up to date.
- Still owed from this row: item 5 and rider 8 are read from the nightlies after the merge; the two shapes the predicate scan does not follow are W47's.
