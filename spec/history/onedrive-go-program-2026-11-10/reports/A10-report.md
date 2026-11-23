# A10 report: nightly sound verdicts, visible result

Base `bd2711b0` (not rebased), branch `ci/nightly-report`, one commit, not pushed.

## What changed
- `.github/workflows/ci.yml`: `workflow_dispatch` input `dry_run` (boolean, default true); workflow concurrency group per event; new `nightly-report` job; two comments corrected.
- `README.md`: CI badge shows the scheduled run of `main` (`?branch=main&event=schedule`).
- `cmd/devtool/main.go`: `nightly report --run-id --dry-run`; the root prints a run error once and no usage text (`silenceRunErrorUsage`).
- `internal/devtool/nightly_verdict.go` (new): the one verdict rule, required jobs, newest completed run, findings, the run bound from `needs:`, package-qualified keys.
- `internal/devtool/nightly_report.go` (new): commit status, one-issue plan, dry run, writes.
- `internal/devtool/nightly.go`: claim format and jobs, stale claims, sound pruning, renewal, whole-word PR gate, plain GitHub sentence.
- `internal/devtool/gh_retry.go`: typed `githubUnavailableError` and its one sentence (error text unchanged).
- `internal/devtool/ci_workflow.go`: schedule/dispatch events, `needs()` reader.
- `internal/devtool/go_test_log.go`: `failedTopLevelTestsByPackage`, shared `topLevelTestName`.
- `internal/devtool/verify_dod.go`: `runDODNightly` in all four stages (start fails; later stages renew/warn).
- `internal/devtool/verify_dod_ci_wait.go`: post-merge lists and reads push runs only.
- `spec/requirements/non-functional.md`: R-6.10.23 amended, R-6.10.24 added (24 and 25 were unused; 24 taken).
- `spec/design/system.md`: Implements (R-6.10.23, R-6.10.24), Verified-By row, file ownership, post-merge, CI tiers, nightly paragraph.
- `CLAUDE.md`: Step 1.5, verification tiers, DoD item 10.

## Tests added (all `Validates:` R-6.10.16/23/24)
- `TestNightlyJobOutcomeNamesEveryResult`: every conclusion's result; missing required jobs did not run.
- `TestNightlyCheckReadsTheNewestCompletedRun`, `...FindsARunStillRunningPastItsBound`, `...FindsNoScheduledRunInTheLast36Hours`, `...FailsOnRedRunWithoutKeys`: run selection and the three findings, one sentence.
- `TestNightlyRunBoundIsTheLongestScheduledChain`: bound from `needs:` chain plus group queue (380 min fixture).
- `TestNightlyCheckCountsTimedOutAndCanceledJobsAsFailures`; `TestNightlyFailureKeysKeepSameNamedTestsOfTwoPackagesApart`.
- `TestNightlyCheckKeepsAFixedClaimWhoseJobDidNotPass`; `TestNightlyCheckReportsAnOldFormatClaimAsStale`; `TestNightlyReleaseDeletesAStaleOldFormatClaim`.
- `TestNightlyRenewClaimsRestampsOnlyTheOwnersClaims`; `TestNightlyPreMergeGateMatchesWholeKeys`.
- `TestNightlyCheck_GitHubFailingTwiceThenAnsweringPasses`; `TestDODNightlyStartFailsWithOneSentenceWhenGitHubKeepsFailing`; `TestDODNightlyLaterStagesRenewAndWarn`; `TestDODNightlyLaterStagesReportARedNightlyWithoutFailing`.
- `TestNightlyReport{OpensIssueOnFirstRedRun, CommentsOnTheOpenIssueWithoutDuplicating, ClosesIssueOnGreenRun, PostsCommitStatus, TreatsSkippedRequiredJobAsFailure, IgnoresItsOwnJob, DryRunWritesNothing, IssueTextHoldsOnlyJobsTestsAndTheRunURL}`.
- `TestNightlyRequiredJobsExistInWorkflow`: required = scheduled jobs, no `name:`, all in the report's `needs`, condition, permissions, no secret/var.
- `TestCIWorkflowGroupsRunsPerEventAndLiveJobsPerAccount`; `TestREADMEBadgeShowsTheScheduledRunOfMain`; `TestCIWorkflowJobNeedsReadsEveryForm`.
- `TestDODPostMergeCIClassificationIgnoresRunsOfOtherEvents`; `TestLoadMainRunsForCommitAsksForPushRunsOnly`; `TestRootCmdPrintsARunErrorOnceWithoutUsage`.
- Extended: `TestNightlyFailureKeysNameTestsOrJobs`, `TestNightlyCheckPrunesConfirmedFixesAndExpiredStaleClaims`, `TestNightlyCheckClaimsUnownedFailure`, `TestNightlyPreMergeGateRequiresHeldClaimsInPRBody`, `TestNewNightlyCmdPassesFlagsThrough`, `TestDODPrePRRunsThePRProfileByDefault`.
- Red seen: the word-boundary and push-only tests fail against the old logic (checked by reverting each line); the rest were red as compile failures before the code existed.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/... ./internal/graph/... ./internal/devtool/...`: pass.
- `go test -count=1 ./internal/devtool/... ./cmd/devtool/...`: pass; `./internal/devtool/` rerun after the lint fixes: pass.
- Same with `-race` behind the lock: pass (run before the lint fixes, which only renamed variables, fixed two comment spellings and passed a claim by pointer).
- Lint behind the lock: first run 9 issues (hugeParam, builtinShadow, govet shadow, misspell), all fixed; second run `0 issues.` (`A10-lint.log`).
- Workflow parses through the repository's own reader: `TestNightlyRequiredJobsExistInWorkflow`, `TestCIStallWindow_*`, `TestCIJobTimeoutsCoverEveryGoTestDeadline`, `TestCIWorkflowSetsNoTestDriveInJobEnv` pass on it. actionlint not run (not installed, by the rule).
- Spec by hand: every test name cited in `system.md` exists (the six unresolved by my grep live in `testutil/`, pre-existing); R-6.10.16, R-6.10.23, R-6.10.24 exist.
- Read-only proof against GitHub: `devtool nightly report --run-id 43274485305 --dry-run` printed the status and issue text (2 failed jobs, test keys only); it found the `gh label list --search` empty-output case, now handled and tested.

## Deviations (behaviour, scope, interface)
- A canceled job fails the night, named "canceled" (brief f), not the analysis's "no verdict"; no automatic `gh run rerun` (analysis 09) since the plan row has none.
- Stale claims are recognized by a record field `format` ("package-qualified"), because old `job-` keys look like new ones; the check reports them and never deletes them; `release` deletes them for any branch.
- pre-pr, pre-merge and post-merge now read the nightly (run list, run view) to print the verdict; every failure there is a warning. pre-pr/pre-merge renew claims (git push).
- Report job `permissions:` are `issues: write` and `statuses: write` only, no `contents: read`/`actions: read` (the analysis had them). Checkout and the Actions reads rely on the repository being public; the dispatch dry run proves it. If it fails, that is the brief's stop condition and the owner decides.
- Commit status is `success` or `failure` only (no `error`); the open issue gets one comment per red night and its body is not edited.
- The report exits 0 on a red night; it fails only when GitHub reads or writes fail, which the next check claims as `job-nightly-report`.
- devtool's root no longer prints cobra's `Error:` echo or usage for errors returned while a command runs (all subcommands); flag errors still show usage.
- (h): before, push, schedule and dispatch runs of one commit shared group `ci-<sha>` and queued behind (a third cancelled the pending) each other; now `ci-<event>-…`. The live jobs keep fixed job-level groups `live-account-integration` and `live-account-e2e`, so per-event grouping cannot run two live suites on one account.

## Also fixed
- `ci.yml` `verify` comment said it waits on every tier; it waits on the verify tiers only.
- `hostName` dropped its error without saying why; documented as a deliberate decision.
- `go_test_log.go`: the top-level-test rule was written once per caller; now `topLevelTestName`.
- `classifyPostMergeRuns`: empty-list special case folded into the one pending path.
- `blockEntries` comment said "map" only; it reads lists too.

## Not fixed
1. `.github/workflows/ci.yml:17`: cron `'0 10 * * *'` sits on the hour, GitHub's busiest slot; November runs started 6-10 h late. Fix: an off-the-hour minute; timing is the owner's call.
2. `internal/devtool/nightly_verdict.go` `nightlyFailureKeys`: a job whose log names a failed test and that also failed for another reason (build, coverage gate) yields only the test key. Fix: a step-failure marker line from devtool and a `step-<job>-<step>` key.
3. `internal/devtool/nightly_verdict.go` `attributeNightlyFailures`: a test that failed then passed its classified live-quirk rerun in the same e2e job is still keyed (unverified). Fix: read the classified reruns from the e2e summary.
4. `internal/devtool/verify_dod.go` `runDODPostMerge`: `--branch` (the claims' owner) is not checked against the PR's `headRefName`. Fix: read `headRefName` with the merge state and refuse a mismatch.
5. `internal/devtool/verify_dod.go`: the four stage functions have no stage-level test; only their parts are tested. Fix: one fake-runner test per stage.
6. `internal/devtool/nightly.go` `swapNightlyClaim`: every renew or prune leaves an unreachable local commit. Fix: none needed beyond `git gc`; cosmetic.
7. Run 43274485305: its failed `e2e` job's log names no test the parser reads, so the check would key it `job-e2e`; cause unknown. Fix: inspect that log with `devtool verify failed-test`.

## Unfinished
Nothing in code. GitHub-side proof is the owner's (steps in the final message).

## For the reviewer
Packages tested: `./internal/devtool/...`, `./cmd/devtool/...`. Declarations tests lean on: `fakeRunner`, `writeWorkflowFixture`, `repoRootFromDevtoolPackage`, `repoRootForTest`, `githubBadGateway`, `ghFailure`, `recordedSleeps`, `githubBackoffs`.

## Second pass (review fixes)

Base bd2711b0; the one commit amended, subject kept, body corrected.

### Items
1. Done. `startedAt` in the run list; the bound counts from the latest attempt (`parseAttemptStart`, falls back to `createdAt`); the 36-hour check keeps `createdAt`. Test `TestNightlyCheckPassesOverARerunStartedWithinItsBound`.
2. Done. `TestDODPrePRRunsThePRProfileByDefault` runs the whole stage through `nightlyStageRunner` (claim and gh commands to `claimRemote`, the rest to `fakeRunner`) with a branch and one held claim: asserts the renewal push and the printed verdict. Pre-merge: `runDODPreMergeNightly(…, prNumber, …)` holds gate, renewal and verdict; `TestDODPreMergeNightlyGatesAndRenewsThePRHeadsClaims` (refused gate fails, renews the PR head's claim only).
3. Done, narrower than the brief's literal rule (see "Where the review was wrong"). Parser records each failure's and pass's attempt (count of its package's earlier result lines); `failedTopLevelTestsByPackage` drops a pair only when a pass of a later attempt follows. Test `TestNightlyFailureKeysDropATestItsRerunPassed` (rerun dropped; `-count` fail-then-pass in one go test kept).
4. Done. The report reads the open issue's body and comments (`gh issue view --json body,comments`) and posts no comment whose verdict line (URL plus verdict, matched as a whole line) is already there; a close whose comment is there still closes. Test `TestNightlyReportRunTwiceWritesTheIssueOnce`.
5. Done. `concurrency: {group: nightly-report, cancel-in-progress: false}`; held in `TestNightlyRequiredJobsExistInWorkflow`.
6. Done. `event,headBranch` in the run view; `isNightlyRun`; a non-scheduled or non-main run posts its status only and says so; issue text and R-6.10.24 say so. Test `TestNightlyReportWritesTheIssueForAScheduledRunOfMainOnly` (five cases).
7. Done. `attributeNightlyFailures` always records an unreadable log (only a canceled context ends it); the check keys `job-<name>` and prints "…; its log could not be read". Test `TestNightlyCheckKeysAJobWhoseLogCannotBeRead`; `TestNightlyCheck_SaysGitHubFailedAfterTheRetryBound` now fails the run list.
8. Done. `TestNightlyCheckListsScheduledRunsOfMainOnly` (fake records run-list args).
9. Done. `failedPackageWithoutTest`; job keyed beside its tests. Test `TestNightlyFailureKeysKeyTheJobOfAPackageFailureWithoutATest`. Sentence on failures outside go test added to R-6.10.23 and system.md.
10. Done. `requirePRHeadBranch` before the merge (`headRefName` added to `pullRequestStateFields`). Test `TestDODPostMergeRefusesABranchThatIsNotThePRsHead`.
11. Done. `17 10 * * *`; ci.yml comment and system.md. No test or other repo document named the time. The host's own `~/agent/CLAUDE.md` still says "nightly at 03:00 Pacific" (not in the repo; not touched).
12. Done. Root hook is `PersistentPreRunE` that runs `ValidateRequiredFlags` and `ValidateFlagGroups` before silencing usage; `main` delegates to `execute`. Tests `TestRootCmdPrintsARunErrorOnceWithoutUsage`, `TestRootCmdShowsUsageForAMissingRequiredFlag` (the latter failed on the old hook: no usage).
13. Done. `nightlyAggregateNeeds` (held to verify's `needs:` in the workflow test). Test `TestNightlyFailureKeysNameTheAggregateThatFailedOnItsOwn`.
14. Done. Key keeps the package path (`internal/sync.TestPlanner`); sentinels `(unknown-package)`, `(module-root)`. `nightlyClaimRefName`: `[a-z0-9-]` literal (no leading `-`), uppercase `_`+lowercase, every other byte `_`+3 decimal digits; empty or over-200-byte ref → head + `_-` + SHA-256. Reader keys claims by ref name; a record whose format is old, unreadable, or whose ref is not its key's name is stale, printed by ref name, never matched. Tests `TestNightlyClaimRefNameIsInjectiveAndAValidRefComponent`, `TestNightlyFailedTestKeyKeepsPackagesApart`, stale case added to `TestNightlyCheckReportsAnOldFormatClaimAsStale`; whole-key gate takes `/ ~ +`.
15. Done. Canceled with no steps (or no start time) → "did not run"; check prints "…; nothing to claim", claims nothing, no finding; status description and issue name it. Tests `TestNightlyCheckClaimsNothingForAJobThatDidNotRun`, `TestNightlyReportNamesAJobThatDidNotRun`; cases added to `TestNightlyJobOutcomeNamesEveryResult`.
17. Done. Four permissions; the test holds exactly those four.
18. Done: R-6.10.24 issue text; R-6.10.23 and CLAUDE.md step 5 (gate still fails, its PR read included, now in the plain GitHub sentence); renew sentence; DoD item 10; job results listed as the code names them.
19. Done: root test goes through `newRootCmd`/`execute`; concurrency test holds the whole workflow `concurrency:` block and the live jobs' `cancel-in-progress: false`; pre-pr test as item 2.

### Where the review was wrong
- Must-fix 3: "the classified rerun is the only path that prints a second verdict for one test" is false. `stress` runs `-json -race -count=3` over `./internal/sync/...` (and `-count=50`), so a flaky test's fail then pass appears inside one go test. The literal rule would have hidden real stress flakes; the rule implemented drops only a pass from a later go test of the package.
- Finding 7 / note 15: a job canceled while queued has a start time (GitHub sets it at queueing). Read-only `gh run view 41672250722 --json jobs`: canceled macOS legs had `startedAt` set, `steps` empty, conclusion `cancelled`. Steps are the signal; empty/zero `startedAt` kept as a second one.

### Deviations
- Item 4 matches the verdict line, not the bare URL, so a rerun that turns a red night green still posts its closing comment.
- Item 15 applies "claims nothing, no finding" to every "did not run" result (skipped or missing required jobs too): they share one result. `TestNightlyCheckKeepsAFixedClaimWhoseJobDidNotPass` no longer expects `job-integration`.
- An unreadable claim record is now stale (it was a synthetic expired claim).
- Ref names are lowercase-only so claims stay apart on a case-insensitive file system.

### Checks
- `go build ./...`, `go vet ./...`, tagged vet: pass. gofumpt -l on the 11 Go files changed: empty.
- `go test -count=1 ./internal/devtool/... ./cmd/devtool/...`: pass; `-race` behind the lock: pass (devtool 168 s, cmd 35 s).
- Tests reading the real ci.yml: `TestNightlyRequiredJobsExistInWorkflow`, `TestCIWorkflowGroupsRunsPerEventAndLiveJobsPerAccount`, `TestNightlyRunBoundIsTheLongestScheduledChain`, `TestREADMEBadgeShowsTheScheduledRunOfMain`, plus the existing workflow tests: pass.
- Lint: 0 issues (log in A10-lint.log; two earlier runs found 5 issues, fixed: an unchecked Fprintln, a duplicated test string, an err shadow, a test helper without t.Helper).
- By hand: every `TestXxx` cited in system.md, non-functional.md and CLAUDE.md exists; every R-id cited exists.

### Seen and not fixed
1. `internal/devtool/go_test_log.go` `packageFailed`: a second go test of a package that already failed, failing without naming a test, settles no package record, so its job key is missed. Fix: key package failures by attempt.
2. `internal/devtool/nightly_report.go` `applyNightlyReport`: a rerun posts the same commit status again (idempotent, but one more status entry). Fix: read the commit's statuses first.

## Third pass (second review)

Commit dfac20b2, amended onto bd2711b0, not pushed.

### What changed
- `internal/devtool/go_test_log.go`: failures are kept per go test of a package (`goTestRun`, `goTestSettled`); a pass is recorded only inside a classified rerun window, from the `==> rerun known live quirk` marker to devtool's next `==> ` line, and only such a pass at a later attempt drops a failure; a package failure without a test settles per go test; a `-json` package result is closed by its action alone.
- `internal/devtool/verify_e2e.go`: the marker is printed from the parser's constant `classifiedRerunMarkerPrefix`.
- `internal/devtool/nightly_verdict.go`: `listNightlyRuns`, the one run-list call of the check and the report.
- `internal/devtool/nightly_report.go`: `newestNightlyRun` (`--limit 1`), action `nightlyIssueNotTheNewest`, its describe line; plan passed by pointer.
- `.github/workflows/ci.yml` comment, R-6.10.23, R-6.10.24, `system.md:327`, `CLAUDE.md:224`: rerun rule, per-go-test package failures, the report job judged like any job, the concurrency wording, newest-run rule.

### Tests (state on HEAD before the fix)
- `TestNightlyFailureKeysOnlyAClassifiedRerunAnswersAFailure`, six cases. Failed on HEAD: raw failure then `-json` passes beside another failure (only the other was keyed); the same alone (only `job-stress`); failure after a passing classified rerun (only `job-e2e`); package failure without a test in a later go test (no `job-stress`). Passed on HEAD, kept as guards: a rerun that fails again (keyed once); a `-json` failure whose result is printed as output too (no job key).
- `TestNightlyReportWritesTheIssueForTheNewestScheduledRunOnly` (older red/open, older green/open, older red/none, newest red/open; run-list flags; dry run): failed on HEAD.
- `TestNightlyReportWritesNothingWhenTheNewestRunIsUnknown` (5xx past the retries; an empty list): failed on HEAD.
- `TestNightlyReportClosesAnIssueThatHoldsItsCommentWithoutCommenting`, `TestNightlyReportMatchesTheHeldVerdictLineWhole` (both URL directions, red and green): pass on HEAD, no fault; the second fails when the match is made a substring match (mutation tried, reverted).
- `TestNightlyCheckKeysAFailedReportJob`: holds item 5.
- `TestNightlyRequiredJobsExistInWorkflow`: now holds the `DRY_RUN` expression, the `--dry-run` argument and the `dry_run` input's type and default; fails with the default flipped (tried, reverted).

### Where the review was wrong
- Must-fix 1's "keep failure records per attempt" alone would have keyed the job on every `-json` failure: go test -json prints a package's result line as output and then sends the package's action, and the parser counted both, so the action opened an empty attempt. The action alone closes a `-json` package now; the sixth case of the new test holds it.
- Should-fix 4 states the prefix case one way (this run 123, issue 1234), the brief the other; both directions are tested.

### Deviations
- An unreadable or empty newest-run list writes nothing at all, the commit status included: every read already comes before the first write, as for the issue reads. The brief asked only for no issue write.

### Checks
- `go build ./...`, `go vet ./...`: pass. `go test -count=1 ./internal/devtool/... ./cmd/devtool/...`: pass. Added and changed tests under `-race`, targeted: pass. gofumpt and goimports on the six Go files: clean.
- Lint behind the lock: 0 issues (first run found 3, fixed: gocritic hugeParam twice after the plan grew, gosec G304 in the new test helper).
- Every test and function the changed spec lines cite exists; no test renamed or removed.

### Seen and not fixed
- `internal/devtool/go_test_log.go` (`parseGoTestLog` status-line check): a test that prints a raw line starting `==> ` inside a rerun ends the window early, so its rerun pass is ignored and the failure stays keyed. It errs red, never green. Fix if it ever matters: devtool would need a status prefix no test prints.

## Fourth pass

Commit `0c7b02f2` (amended onto `bd2711b0`; was `dfac20b2`). 22 files.

### Items
1. Done. Case "a raw pass in a later go test that is no rerun" in `TestNightlyFailureKeysOnlyAClassifiedRerunAnswersAFailure`. Passes on `dfac20b2`; with `inRerun: true` at `go_test_log.go:382` it failed: expected `{"internal/widget.TestWidgetSpins": ["stress"]}`, actual `{"job-stress": ["stress"]}`. Restored; `git diff --stat` showed the file unchanged.
2. Done. Case "a passing classified rerun of a later go test than the failure". Failed on `dfac20b2`: expected `e2e.TestE2E_WidgetRoundTrip` and `e2e.TestE2E_WidgetShares`, actual only `e2e.TestE2E_WidgetShares`. Model difference: the code kept only the latest attempt a rerun passed (`rerunPassAttempt map[string]int`), so `== attempt+1` alone would re-key an answered failure once the same test is rerun and passes again later. It now keeps every rerun pass (`rerunPasses map[goTestInRun]bool`, `goTestSettled` renamed `goTestInRun`), and the check in `failedTopLevelTestsByPackage` is one line. R-6.10.23 and system.md say "the next go test of its package".
3. Done. The clause now says what is lost: the replaced report's commit status, the report job read as did not run (checked: canceled before start is `nightlyJobNotRun`), and, for the newest run, the issue keeps the earlier verdict until the next night's report.
4. Done. `postNightlyStatus` posts the status once the run, jobs and logs are read; the newest-run and issue reads follow; `applyNightlyReport` writes only the issue and label (one status write on every path). Renamed `TestNightlyReportPostsOnlyItsStatusWhenTheNewestRunIsUnknown`; twin `TestNightlyReportPostsOnlyItsStatusWhenTheIssueCannotBeRead` (new fake field `labelListErrs`). Failed on `dfac20b2`: "the commit status is the one write": `"[]" should have 1 item(s), but has 0`. R-6.10.24 and system.md changed. The dry run still writes nothing.
5. Done. `TestRerunClassifiedQuirkPrintsItsMarkerBeforeItsGoTest` (existing `fakeRunner`): the marker comes first and the rerun's pass answers the failure in `parseGoTestLog`. Passes on `dfac20b2`; with the marker written after the go test it failed: "Should be true" and `Should be empty, but was [{./e2e TestE2E_WidgetRoundTrip false 0}]`. Restored.
6. Done. Both statements checked against the code (`verify_e2e.go:549` prints the warning without the prefix; the window flag changes only on a `==> ` line).
7. Done. "the client's plain sentence" now applies to the unreadable list only; the empty list fails "in the report's own".
8. (a) Done: `nightlyRunReport.runID` is the given id; case "the newest red run, its view of no id" failed on `dfac20b2` ("the newest run writes the issue": 0 comments). (b) Done: `listNightlyRuns` sorts by creation for both readers (the check's own sort moved there); the report lists the check's 10 runs instead of 1, since sorting a one-run list decides nothing. `TestNightlyReportFindsTheNewestRunByItsCreation` failed on `dfac20b2`: 0 comments.
9. Done. CLAUDE.md: "does not require"; system.md: no duration.
10. Yes, on `dfac20b2` a classified rerun that only skipped its test passed: `runGoTestMatching` takes the `ok` line (or the -json skip event) as a selected test, so the first failure became a warning and the job could go green. Fixed in `rerunClassifiedQuirk` (`verify_e2e.go:540`, `skippedTopLevelTests`): a top-level skip fails the rerun, recorded as failed. `TestRerunClassifiedQuirkFailsARerunThatSkippedItsTest` failed on `dfac20b2` with "An error is expected but got nil." (raw and -json). R-6.10.26 and system.md line 457 changed.

### Where the review was wrong
- Should-fix 2's smallest fix assumes one pass attempt per test; the map held only the latest (item 2 above).
- Note 10(b): sorting alone cannot help while the list is `--limit 1`; GitHub's order picks the one run returned.

### Seen and not fixed
- `internal/devtool/verify_e2e.go:540`: a rerun whose top-level test passes while every subtest skipped still counts as a pass for the fast and watch reruns (for the auth preflight it is recorded as `skipped`, per R-6.10.26). Fix if wanted: fail a rerun whose top-level test ran no subtest that passed.

### Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test -count=1 ./internal/devtool/... ./cmd/devtool/...`: pass. Added and changed tests under `-race`, targeted: pass. `TestFindTestConventionViolations_RealRepoIsClean` and the list tests: pass.
- gofumpt and goimports on the seven Go files: clean. Lint behind the lock: 0 issues.
- Every test the changed spec lines cite exists (64 names); the renamed test and removed helpers are cited nowhere.



## Step review of the fourth pass, answered in review (2026-11-14)

A read-only reviewer read the step from the third to the fourth pass (findings in `R-A10d-findings.md` in the scratch area): verdict "accept after the must-fix item", 1 must, 1 should, 8 notes. The reviewer of record made the changes, because each was a few lines. The commit is `8b9ee044` on `2b1c5afa`.

- Must 1, done: the table of `TestNightlyFailureKeysOnlyAClassifiedRerunAnswersAFailure` has the case "two failures of one test, each answered by its own passing classified rerun". It holds that every rerun pass is kept, not only the latest. Shown: with `recordPass` changed to keep only the latest pass of a test, that case alone fails (the test is keyed where only the job should be); restored afterwards.
- Should 2, done: `TestRerunClassifiedQuirkKeepsTheErrorOfARerunThatFailedAndSkipped`. A rerun go test failed keeps that error when its output also shows a top-level skip. Shown: with `rerunErr == nil &&` removed from `rerunClassifiedQuirk`, the test fails; restored afterwards.
- Note 3, done: the comment on `devtoolStatusPrefix` in `go_test_log.go` no longer reads as if one line were the only one without the prefix, and says what the rerun window relies on (a go test started after a rerun prints a status line first).
- Note 4, done: the `nightlyRunList` comment says "in the order given".
- Note 5, done: an auth preflight rerun that fails on a skip left the step's failed tests empty. `runE2EPreflightAuth` now keeps the first attempt's failed tests when the rerun's output names none. Test: `TestRunVerifyE2EFullNamesTheFailedTestWhenTheAuthPreflightRerunSkipped` (fails with the old behavior, shown by a temporary change, restored).
- Notes 6, 7, 9, 10: no change, as the review says.
- Note 8, recorded, not reachable today: a go test devtool started after a classified rerun without a status line would inherit the rerun's window. The comment of note 3 now states the rule.

Judged, no change: the writer's "seen and not fixed" (a rerun whose top-level test passes while every subtest skipped counts as a pass). A first attempt with the same output passes too, so the rerun is no weaker than the first attempt.

Checks on `8b9ee044`: gofumpt clean on the four files; `go test -count=1 ./internal/devtool/ ./cmd/devtool/` passes; `go build ./...`, `go vet ./...`, the tagged vet and the convention test are clean; the lint of the two devtool packages reports 0 issues (`A10-lint.log`, 2026-11-14 about 09:10).



## Merged as #907 (`2e465121`, 2026-11-14 about 11:57 PST)

- One commit `7d757347` on main at `3e5c1bd4`. The first pre-pr gate failed in the "spec governs" step: the routing table of the repository's `CLAUDE.md` named a file that does not exist. The reference was corrected and the commit amended; the second gate passed. The check is now replayed before any gate is spent on a branch that edits `CLAUDE.md` (`routing_check.py` in the session's scratch tools).
- Pre-merge passed with no review threads; post-merge passed (165 s) and removed the `nightly-report` worktree and its branch. The cleanup audit kept one remote branch that is not this program's: the dependency update for the SQLite driver, whose merge is the owner's.
- Installed `2e465121`; the watch client reports "Up to date, watching for changes", 19 files.
- No live test applies: the change is devtool and workflow code. Its proof on GitHub is still owed: one manual dispatch of the workflow with the dry run on, and the first scheduled run after the merge (10:17 UTC), which must end with the commit status `nightly`.
- Claim refs have new names. Every worktree in flight is rebased onto this main before its next devtool stage, or its nightly check reads the old claims as stale.
- No schema change and no store-generation bump.


## GitHub-side proof, part 1: the dry-run dispatch (2026-11-14)

Run 43644281830 (`workflow_dispatch` on main at `dd0932f4`, input `dry_run` left at its default, started 13:43 PST) ended with every job green: stress, verify-full on both systems, e2e, integration, verify, nightly-report. The report job ran `devtool nightly report --run-id … --dry-run=true` and printed:

- `nightly report: dry run, nothing is written`
- `would post commit status nightly=success on dd0932f4…: every nightly job passed (<run URL>)`
- `a workflow_dispatch run of main does not write the nightly-failure issue: only a scheduled run of main does`

So on GitHub the job reads the run it belongs to, names the commit status it would write and keeps the issue for scheduled runs, as the row asked. Nothing was written (checked only through the job's own first line; the commit's statuses were not listed).

Still owed, part 2: the first scheduled run after #907 must end with the commit status `nightly` on its commit (and, if a job fails, the failure issue). Next schedule: 02:17 PST on 2026-11-15; GitHub has started it hours late before.


## The night of 2026-11-15 (first scheduled run with both changes on main)

- The scheduled run on `341d9820` was red, and it said so on GitHub: the
  `nightly-report` job posted the commit status and opened the
  `nightly-failure` issue #919. The issue closes on the next green
  scheduled run, not by hand.
- Red jobs: the full e2e tier (one test, claimed and fixed on the e2e
  output branch); `model-seeds` (two seeds of the crash model, 318011
  and 318012, each a replay command; plan rows W50 and W51); the stress
  lane (the sync tree under the race detector passed its go test
  deadline; plan row A20).
- Two faults of the report itself, both a rider on plan row A17: the
  issue said of each job "failed; its log could not be read" (cause not
  checked), and two tests that were only running when the stress alarm
  fired were keyed as failing.
- What worked as designed: the verdict rule, the status, the issue, the
  date-derived start seed, one claim per failing key through
  `devtool nightly check`.
