# A10 Definition of Ready: nightly sound verdicts, visible result

Worktree `~/src/onedrive-go-wt/nightly-report`, branch `ci/nightly-report`, base `bd2711b0`.

## Change points and the test that proves each

1. `nightly check` reads the newest *completed* scheduled run of any conclusion; a newer run still within its bound is passed over. `TestNightlyCheckReadsTheNewestCompletedRun`.
2. A run without a verdict is a finding: the newest scheduled run is still running past the longest a scheduled run can take (derived from `ci.yml`: the longest `needs:` chain of schedule jobs, each waiting one run of its concurrency group, plus the margin). One plain sentence, check fails. `TestNightlyCheckFindsARunStillRunningPastItsBound`, `TestNightlyRunBoundIsTheLongestScheduledChain`.
3. A stale run is a finding: no scheduled run started in the last 36 hours. `TestNightlyCheckFindsNoScheduledRunInTheLast36Hours`.
4. A red run from which no failure key derives is a finding. `TestNightlyCheckFailsOnRedRunWithoutKeys`.
5. One verdict rule for check and report: success passes; failure fails; timed out and cancelled fail named as such; a required job skipped or absent "did not run"; a job without a conclusion "did not finish". `TestNightlyJobOutcomeNamesEveryResult`, `TestNightlyCheckCountsTimedOutAndCanceledJobsAsFailures`.
6. A `fixed` claim is deleted only when every job that produced it passed in a run containing the fix (claims record their jobs). `TestNightlyCheckPrunesConfirmedFixesAndExpiredStaleClaims` (extended), `TestNightlyCheckKeepsAFixedClaimWhoseJobDidNotPass`.
7. Keys are package-qualified (`<package>.<Test>`; `job-<name>` when no test is named). `TestNightlyFailureKeysNameTestsOrJobs` (updated), `TestNightlyFailureKeysKeepSameNamedTestsOfTwoPackagesApart`.
8. A claim of the old format (record without the current format) is reported as stale, taken over when its key fails, deleted by `nightly release`; never ignored, never read as another test's claim. `TestNightlyCheckReportsAnOldFormatClaimAsStale`, `TestNightlyReleaseDeletesAStaleOldFormatClaim`.
9. Leases renew in the pre-pr and pre-merge stages. `TestNightlyRenewClaimsRestampsOnlyTheOwnersClaims`, `TestDODNightlyLaterStagesRenewAndWarn`.
10. The PR-body gate matches a key on a word boundary. `TestNightlyPreMergeGateMatchesWholeKeys`.
11. `nightly-report` job: schedule and dispatch only, `needs:` every required job with `if: always()`, `permissions:` issues and statuses only, no secret or variable; every required job is a schedule job of `ci.yml` without a `name:` override, and every schedule job is required. `TestNightlyRequiredJobsExistInWorkflow`.
12. The report posts commit status `nightly` on the run's head commit. `TestNightlyReportPostsCommitStatus` (table).
13. A red night with no open issue creates the `nightly-failure` label when missing and opens one issue. `TestNightlyReportOpensIssueOnFirstRedRun`.
14. A red night with an open issue adds one comment, opens no second issue. `TestNightlyReportCommentsOnTheOpenIssueWithoutDuplicating`.
15. A green night closes the open issue with one comment. `TestNightlyReportClosesIssueOnGreenRun`.
16. A skipped required job makes the night red. `TestNightlyReportTreatsSkippedRequiredJobAsFailure`.
17. The report ignores its own job. `TestNightlyReportIgnoresItsOwnJob`.
18. Dry run prints the status and the issue text and writes nothing; the CLI has `report --run-id --dry-run`; `workflow_dispatch` gets boolean `dry_run`, default true. `TestNightlyReportDryRunWritesNothing`, `TestNewNightlyCmdPassesFlagsThrough` (extended).
19. Issue text is built only from job names, test names and the run URL; log text never reaches it. `TestNightlyReportIssueTextHoldsOnlyJobsTestsAndTheRunURL`.
20. Post-merge reads push runs only. `TestDODPostMergeCIClassificationIgnoresRunsOfOtherEvents`, `TestLoadMainRunsForCommitAsksForPushRunsOnly`.
21. CI concurrency group per event; the live jobs keep one fixed group each. `TestCIWorkflowGroupsRunsPerEventAndLiveJobsPerAccount`.
22. README badge shows the scheduled run of `main`. `TestREADMEBadgeShowsTheScheduledRunOfMain`.
23. Rider: GitHub failing twice then answering does not stop the check. `TestNightlyCheck_GitHubFailingTwiceThenAnsweringPasses`.
24. Rider: GitHub failing past the retries: the start stage fails with one plain sentence naming GitHub and the endpoint; pre-pr, pre-merge and post-merge print it as a warning and pass; no usage text. `TestDODNightlyStartFailsWithOneSentenceWhenGitHubKeepsFailing`, `TestDODNightlyLaterStagesRenewAndWarn`, `TestRootCmdPrintsARunErrorOnceWithoutUsage`.
25. Rider: post-merge does not exit 1 for a failed nightly the PR did not touch (push runs only, nightly verdict only warns). Points 20 and 24.

## DoR checklist

1. PASS **Task intent**: repo-maintenance outcome: the nightly run has a verdict someone sees (commit status, one issue, filtered badge), the verdict is right (timeouts, cancellations, skipped required jobs, stale and unfinished runs, package-qualified keys, sound pruning), and no increment is stopped by GitHub being slow.
2. PASS **Non-goals**: product packages (`internal/sync`, `cli`, `graph`, `config`, `driveops`, `multisync`), e2e tests, bucket lists, moving checks to the linter, `-race` on push, seed rotation, automatic reruns of the nightly.
3. PASS **Governing docs**: `spec/design/system.md` (governs `internal/devtool/*.go`), `spec/requirements/non-functional.md` R-6.10.16, R-6.10.22, R-6.10.23, R-6.10.26, R-6.10.27; `CLAUDE.md` Step 1.5 and verification tiers; `.github/workflows/ci.yml`.
4. PASS **Authority owner**: verdict rule and report text: `internal/devtool` (new `nightly_verdict.go`, `nightly_report.go`; `nightly.go` claims); workflow job only calls `devtool nightly report`; claim refs on the remote stay the durable state; GitHub issue/status are side effects owned by the report command.
5. PASS **Code scope**: `internal/devtool/{nightly,nightly_verdict,nightly_report,ci_workflow,gh_retry,verify_dod,verify_dod_ci_wait}.go` and tests, `cmd/devtool/main.go` and test, `.github/workflows/ci.yml`, `README.md`, `CLAUDE.md`, `spec/design/system.md`, `spec/requirements/non-functional.md`.
6. PASS **Regression plan**: the tests listed above, written first and seen failing.
7. PASS **Verification plan**: `go build ./...`, `go vet ./...`, tagged vet; `go test -count=1 ./internal/devtool/... ./cmd/devtool/...`, then `-race` behind the host lock; targeted golangci-lint behind the lock; workflow read by the repository's own reader tests. GitHub-side proof (dispatch dry run, first scheduled write) is prepared for the owner.
8. PASS **Data safety**: no sync code touched; no local or remote OneDrive data involved.
9. PASS **Secret and credential safety**: the report job uses only `github.token`, reads no secret or variable (asserted by test); issue text carries no log text (asserted by test).
10. PASS **Live provider and destructive operations**: none run here. The claim-key format changes; precondition checked: `git ls-remote origin 'refs/nightly-claims/*'` printed nothing.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start stage has already been run in this worktree; no carryover threads or nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    - Question: does checkout and the Actions read API work on this public repository with a job token of only `issues: write` and `statuses: write`? Current assumption: yes (public resources). Validation step: the `workflow_dispatch` dry run. Owner path if wrong: `ci.yml` `nightly-report.permissions` (the owner decides on `contents: read`/`actions: read`).
    - Question: which conclusion does GitHub give a job that hits `timeout-minutes`? Current assumption: `timed_out` or `cancelled`; both fail the night, each named. Validation step: none needed. Owner path: `nightly_verdict.go`.
    - Question: are a completed job's logs readable while its run is still in progress? Current assumption: yes; an unreadable log is named in the issue and never changes a job's verdict. Validation step: dispatch dry run. Owner path: `nightly_report.go`.
