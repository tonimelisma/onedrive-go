# A17 report: a failed verify step prints one marker line, and the nightly keys the step

Worktree `~/src/onedrive-go-wt/step-failure-marker`, branch `ci/step-failure-marker`, commit 6d9e9e1a on af2a6cf5 (not pushed).

## What changed
- verify_summary.go: `verifyStepFailedPrefix`, `failedStepMarker`, `readFailedStepMarker`; `verifySummaryCollector.run`, the one runner (times the step, prints the marker on failure); `runStepWithOutcome` and `runBucket` record what it returns; the collector takes stderr.
- verify.go: `verifyStepName`, `verifyStepNames()` (closed set of 32), `verifyStep.name` typed, named struct fields; step constants for format, build, unit-tests, lint, workflow-lint, the 8 rules, integration.
- verify_quick.go, verify_coverage.go, verify_e2e.go, verify_stress.go, verify_race.go, verify_seeds.go, verify_failed.go: each step's name a constant beside it; buckets named by constants; stress names hold no count.
- nightly_verdict.go: `nightlyStepKey`, `nightlyFailedStepKeys`, `nightlyJobReport.steps`, `failsWithoutTest`; a failure no test names is keyed by its steps, else by the job.
- nightly_report.go: the issue line names the job's step keys (`; failed steps: ...`).
- verify_governs.go: `routingTableRows`; the routing check reads only the table rows and names the line.
- spec/requirements/non-functional.md (R-6.10.23, R-6.10.24), spec/design/system.md (lines 388, 392, 396, 653), CLAUDE.md:248: one sentence each, no reflow.
- Tests touched for the renamed summary names: verify_runner_test.go, verify_every_step_test.go, verify_e2e_full_test.go, verify_seeds_test.go, verify_test_match_test.go, verify_test_helpers_test.go (helpers take a `verifyStepName`; the fake runner can print on stderr).

## Tests added (all seen failing before the code, except the two marked guard)
- TestRunVerifyPrintsOneMarkerLineForAFailedStepAndNoneForAPassingOne: stderr is exactly the step's output then `verify step failed: quick build`.
- TestRunVerifyPublicPrintsAMarkerForEveryFailedStepInOrder: lint and unit-tests markers, in order; none for the not-run coverage gate.
- TestVerifyProfilesNameEveryStepOnceFromTheClosedSet: the set equals the declared constants (read from the source), every profile run against an all-pass fake runner names each step once from the set.
- TestNightlyFailureKeysKeyAFailedStepByItsMarker: markers without a failed test give `step-<job>-<step>`, once each, no job key.
- TestNightlyFailureKeysPreferATestKeyToAStepMarker (guard: holds today's behaviour): a failed test and a marker give the test key only.
- TestNightlyFailureKeysKeyAPackageFailureWithoutATestByItsStep: a package failure without a test is keyed by the steps beside the test.
- TestNightlyFailureKeysReadOnlyMarkersOfTheClosedSet (guard): unknown step or profile, extra field, indented line or -json event is no marker; the job key stays.
- TestNightlyCheckClaimsAFailedStepUnderItsOwnKey: every job x step key is its own valid ref name; a claim is taken under it and read back unchanged.
- TestNightlyReportIssueNamesTheFailedStepsOfAJobWithoutAFailedTest: the issue lists the step keys and no log text.
- TestSpecGoverns_ChecksOnlyTheRoutingTableRows, TestSpecGoverns_ReadsTheRealGuidesRoutingTable; TestSpecGoverns_RejectsDeadRoutingTableReference now wants the line.

## Checks
- go build ./..., go vet ./..., go vet -tags=e2e,e2e_full,integration,stress ./...: clean.
- gofumpt -l and goimports -l on internal/devtool and cmd/devtool: no output.
- go test ./internal/devtool/... ./cmd/devtool/...: ok; with -race behind the lock: ok.
- RealRepoIsClean tests: 4 PASS. Cited tests in the two spec files: all exist (0 missing).
- golangci-lint on ./internal/devtool/... ./cmd/devtool/...: 0 issues (A17-lint.log). The first run found gocritic hugeParam/rangeValCopy: the new field made nightlyJobReport 128 bytes; the two bools now sit together (120 bytes). The nightly tests ran again after that reorder; the race run came before it.
- git diff --stat: CLAUDE.md 1 line, system.md 4 lines, non-functional.md 2 lines changed.

## Deviations
- The brief leaves a log with failed tests and a package that failed without one open: today it gets the job key beside the tests. Here the step keys take the job key's place, so that case names the step too (tests plus steps).
- The platform profile's build step is `host-build` (quick's `build` is another step, the cross-platform build).
- The stress tree step is `sync-tree-race-repeated`: `sync-race-repeated` is a suffix of `multisync-race-repeated`, which made a NotContains assertion ambiguous.
- The interrupted work's tests were kept; one fixture word was renamed and a history phrase dropped from a comment; one test (the package-failure case) and the claim's key read-back were added. Its stubs were replaced by the code.

## Also fixed
- Positional `verifyStep{...}` literals now use named fields.
- The stress comment that said the step's name follows from the run count.

## Not fixed
- internal/devtool/nightly_verdict.go:305: with "test keys preferred", a job whose log names a failed test hides its other failed steps (a lint failure beside a unit-test failure in `verify public`). Fix: attribute failed tests to the step whose marker follows them and key the other steps too.
- internal/devtool/nightly_verdict.go:386: `step-<job>-<step>` is not injective in theory (job `a-b` step `c` vs job `a` step `b-c`); no current pair collides. Fix: a separator a job or step name cannot hold.
- spec/design/system.md:768 and spec/reference/live-incidents.md:150-190 name the stress steps by their old names in historical prose.

## Unfinished
- Nothing. Packages to test beyond devtool: none (no product code changed).

## Review fixes

Amended commit `37aed9ac` on `2afdb376`.

- Item 1: spec/design/system.md:590 says `workflow-lint` (one word, no reflow).
- Item 2: internal/devtool/verify_step_marker_test.go:142, TestVerifyProfilesNameEveryStepOnceFromTheClosedSet compares the union of the profiles' recorded steps to `verifyStepNames()`. Seen failing with a planted declared and listed `planted` name (only the new assertion, "extra elements in list A: planted"); plant removed.
- Item 3: internal/devtool/verify.go:991 `verifyStepName.runsGoTest`, an exhaustive switch over all 32 names (16 go test steps, 16 others). internal/devtool/nightly_verdict.go:256 and :399 read markers on every log: a non-test step's marker is keyed always, a go test step's only when `failsWithoutTest`; the comments of `steps`, `attributeNightlyFailures`, `nightlyFailureKeys` and `nightlyFailedStepKeys` say so. New tests: TestNightlyFailureKeysKeyAStepOutsideGoTestBesideAFailedTest (nightly_step_key_test.go:62) and TestNightlyReportIssueNamesAStepOutsideGoTestBesideTheFailedTests (nightly_report_test.go:307), both seen failing before the change (the lint step key missing; the issue line without "failed steps"); TestVerifyStepNamesSayWhetherTheyRunGoTest (verify_step_marker_test.go:153), seen failing with the planted name. TestNightlyFailureKeysPreferATestKeyToAStepMarker and TestNightlyFailureKeysKeyAFailedStepByItsMarker unchanged and passing. The shared failed-test fixture is the constant `nightlyTestPlannerFailure`.
- Item 4: nightly_step_key_test.go:136, TestNightlyCheckClaimsAFailedStepUnderItsOwnKey records each key's job and step and asserts it new. Seen failing with a planted job `verify-full--ubuntu-latest` (all 32 keys collide with the ubuntu leg); plant removed.
- Note 8: verify_e2e.go `classifiedQuirkRerun.phase` is now a `verifyStepName`, set from `fastE2EStepName` (:522), `authPreflightStepName` (:613) and `bucket.Name` (:910), so the compiler holds the spelling. Assertions added at verify_runner_test.go:425 and verify_e2e_full_test.go:266, seen failing first ("fast e2e", "auth preflight").
- Note 11: verify_step_marker_test.go:73 spells out `verify step failed: quick build`. Seen failing with the constant planted as `verify step failed:: `; plant removed.
- Note 12: verify_governs.go:385 reads a row after any leading spaces. TestSpecGoverns_ReadsARowIndentedBySpaces (verify_governs_test.go:235) seen failing first ("An error is expected but got nil"); TestSpecGoverns_ReadsTheRealGuidesRoutingTable now also asserts the row count equals `countRoutingTableRows` (:292), seen failing with a planted 5-row cap in `routingTableRows`; plant removed.
- Docs: R-6.10.23 (spec/requirements/non-functional.md:209), spec/design/system.md:396 (keying rule, `runsGoTest`, closed set both ways, key uniqueness, issue test) and :653 (indented rows, second count), CLAUDE.md:248, and the `verifyStepNames` comment.
- Checks: `go vet ./...` and `go vet -tags=e2e,e2e_full,integration,stress ./...` clean; `go test -count=1 -timeout 30m ./internal/devtool/ ./cmd/devtool/` ok (behind the heavy lock); RealRepoIsClean 4 PASS; no u0026/u003c/u003e in changed files; no added line over 140; no function over 100 lines. Lint not run, as instructed.
- Not done: nothing.


## Merged as #930

- Merge commit `c0792e984e082b8d7f2fcc6c27978acf2c5fa5b0`, 22:27 PST on 2026-11-15. Gates: pre-pr passed; the first pre-merge wait failed on macOS shard 2/3 with the known move-order flake of #926 (case `same name on OneDrive`), a rerun of the failed job passed; post-merge passed; installed at 22:48.
- Nightly claims: none held by this branch. The red nightly of 2026-11-15 was all "fix merged, awaiting the next nightly" or held by another branch.
