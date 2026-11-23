# Race on every push; nightly seed rotation (branch ci/race-and-seeds)

**Finding first.** The first measured `verify seeds` night (start 318001) found two failing crash seeds, both replaying exactly: `ONEDRIVE_GO_MODEL_SEED=318012 go test -run '^TestModel_CrashAtSeededPointsRecovers$' ./internal/sync` (batch 2: an empty conflict-copy reservation is left, `sync_invariants_test.go:181`) and `ONEDRIVE_GO_MODEL_SEED=318011 ...` (batch 4 after the crash: no convergence in 8 passes). Product defects, out of scope; the first rotated nightly after merge is likely red and will open seed issues. The race run found no race and no failing test.

## Measurements
- CI, last green scheduled run 43615207680: stress step `sync race x3` (`go test -json -race -count=3 ./internal/sync/...`) 31.3 min, about 10.4 min an iteration (stress summary artifact; job and step times only).
- This host, `verify race` behind the lock (load average 6-7): pass, step 87.1 min (go test 5106.8 s). Deadline `raceSyncTimeout = 120m` (38% over 87 min), `verify-race` timeout 130 (120 + the 10-minute setup allowance).
- This host, `verify seeds` behind the lock: 11.5 min (go test 590 s) at engine and crash 12, planner 8, chaos 2: counts kept, deadline 30m, job 40.

## What changed
- `internal/devtool/verify_race.go` (new): `race` profile, one go test of `./internal/sync/...` under `-race`.
- `internal/devtool/verify_seeds.go` (new): `seeds` profile, `nightlySeedStart` (UTC date), stride 1000, `--seed-start`, prints failing seeds' replays.
- `internal/devtool/seed_replay.go` (new): `seedReplaysFromLog`, the one reader; `seedReplay.command/line/key/env/replays`.
- `internal/devtool/nightly_seed_issues.go` (new): find, plan, describe and write the `seed-failure` issues; check's replay lines.
- `internal/devtool/nightly_report.go`: plans and writes seed issues after the nightly issue; label and issue-view reads shared.
- `internal/devtool/nightly.go`: check adds open seed issues' keys, prints replays; a renewed claim keeps the jobs it failed in.
- `internal/devtool/nightly_verdict.go`: `model-seeds` required, `verify-race` in the aggregate's needs, failing seeds keyed by replay.
- `internal/devtool/verify_failed.go`: `failed-test` reruns a seed per its replay (env, `-race` as the line says).
- `internal/devtool/verify.go`, `verify_quick.go`, `verify_stress.go`, `cmd/devtool/main.go`: profiles, `SeedStart`/`Now`, usage, `syncTestTree` rename.
- `.github/workflows/ci.yml`: `verify-race` (push only) in `verify`'s needs and check loop; `model-seeds` (schedule, dispatch) in `nightly-report`'s needs; comments.
- `internal/sync/test_helpers_test.go`: `seedVars`, `seedsFromEnv`, `seedRun`, `logSeedReplayOnFailure`, `seedReplayLine`.
- `internal/sync/{engine_model_based,planner_model,chaos_remote}_test.go`: families call the helper and log the line; new `..._SEED_START` for planner and chaos.
- Docs: R-6.10.22 amended, R-6.10.25 new, R-6.10.26 (failed-test race); `system.md`; `CLAUDE.md`; `CONTRIBUTING.md`.

## Tests added
- `TestRunVerifyRaceRunsExpectedSteps`, `TestRunVerifyRaceFailsWhenNoPackageRanATest`: the race step's command, summary, empty-run guard.
- `TestCIRaceJobRunsOnEveryPushInsideTheAggregate`: push only; the aggregate reads every need's result.
- `TestNewVerifyCmdAcceptsTheRaceAndSeedsProfiles`: both profiles and `--seed-start` pass through.
- `TestSeedRun_AppliesOneRuleToEveryFamily`, `TestSeededFamilies_DefaultRunsAreUnchanged`: the rule; defaults pinned, race and not.
- `TestLogSeedReplayOnFailure_LogsTheCommandThatRerunsTheSeed`: one line, a complete command, test exists, assignments right.
- `TestSeedReplaysFromLog`, `TestSeedReplayCommandRoundTrips`: raw and -json, dedupe, look-alikes refused; command, key, env.
- `TestRunVerifySeedsSetsRotatedStart`, `TestNightlySeedStrideCoversEveryCount`, `TestSeedsRunPatternNamesExistingTests`, `TestRunVerifySeedsPrintsTheReplayOfEveryFailingSeed`, `TestRunVerifyRefusesASeedStartOutsideTheSeedsProfile`.
- `TestRunVerifyFailedTestRerunsAFailingSeedAsItsReplayLineSays`.
- `TestNightlyReportOpensOneIssuePerFailingSeed`, `...CommentsOnceOnASeedThatFailsAgain`, `...NeverClosesSeedIssues`, `...SeedIssueTextHoldsOnlyTheSeedAndTheRunURL`, `...DryRunPrintsTheSeedIssuesItWouldWrite`.
- `TestNightlyFailureKeysKeyAFailingSeedByItsReplay`, `TestNightlyCheckClaimsOpenSeedIssues`, `TestNightlyCheckRenewsASeedClaimWithTheJobsItFailedIn`.

## Checks (commit a33ea1e6 on 0ff7c754)
- `go build ./...`, `go vet ./...`, tagged vet (e2e, graph, devtool): pass. `go test ./internal/devtool/... ./cmd/devtool/...`: pass; with `-race` behind the lock: pass.
- Four model tests, defaults (`-json`): engine 1-12, crash 1-4, planner 1-8, chaos 1-2, the same lists as a run of the base tree: unchanged. With all three starts 40 and counts 3/3/2: engine 40-42, crash 40-42, planner 40-42, chaos 40-41. Race defaults pinned by `TestSeededFamilies_DefaultRunsAreUnchanged` under `-race`: pass.
- Forced failure (seed 3, edit made in a scratch copy, removed): one line, `seed-replay: ONEDRIVE_GO_MODEL_SEED=3 go test -run '^TestRunOnce_RandomizedBidirectionalEditsConverge$' ./internal/sync`; that command ran seed 3 alone and failed the same way.
- `go test ./internal/sync/...` whole, behind the lock: pass (408 s). `verify race`: pass, 87.1 min. `verify seeds`: 2 failing seeds, 11.5 min.
- Spec by hand: every `TestXxx` cited in system.md, non-functional.md, CLAUDE.md exists; every R-6.10 id cited is declared. Convention test: pass. Lint: `0 issues.` (second run, after fixing two gocyclo, one lll, one gosec, one testifylint).

## Deviations
- One deadline for CI and this host, as decided: the push job's timeout is 130 min, so the post-merge stall window for a push grows from 30 to 140 min; `TestCIStallWindow_PostMergeUsesOnlyPushJobs` now holds it to the race job's timeout plus the margin (push < pull request no longer holds).
- The aggregate `verify` job's `needs` and check loop change (required by the race job joining it); no PR job's name or trigger changes.
- Key form `internal/sync.<Test>/<VAR>=<seed>` (not the analysis' `seed-<env>-<seed>`); a seed key replaces its test's key wherever a log carries the replay.
- Issue code lives in `nightly_seed_issues.go`; the body quotes the `seed-replay:` line so the check reads the command back through the one reader. Analysis' `seedEnv` is `seedVars`; `nightlySeedEpoch` is a function (gochecknoglobals).
- The chaos family's race default (seed 2 alone) stays a two-line special case in its caller: the helper takes a default count only. The analysis said engine and crash change batches under -race; only chaos does, and `-race` in the line covers it. No family has a second input today; the builder and the parser take any number.
- `failed-test` follows the replay's `-race` (R-6.10.26 amended); the seeds profile empties the single-seed variables; a renewed seed claim keeps its earlier jobs.

## Also fixed
- `verify_stress.go` and `system.md`: "six minutes" a race iteration of the sync tree is about ten (31 for three).
- `verify.go` deadline comment named only the live and stress profiles.

## Not fixed
- `internal/sync` crash recovery, seed 318012 (`sync_invariants_test.go:181` via `engine_model_based_test.go:331`): an empty conflict-copy reservation is left after a crash in batch 2. Fix: root-cause in the conflict-copy reservation path after a crash; keep the seed as a named scenario.
- `internal/sync` crash recovery, seed 318011 (`engine_model_based_test.go:502`): batch 4 after the crash still changing after 8 passes. Fix: as above; replay with its seed-replay command.
- `spec/reference/developer-onboarding.md:556`: the profile list names five profiles of thirteen. Fix: list them all, or point at `verify --help`.
- `spec/reference/developer-onboarding.md:587`: the verifier file table lacks `verify_quick.go`, `verify_race.go`, `verify_seeds.go`, `seed_replay.go` and the nightly files. Fix: add them, mirroring system.md's ownership line.
- `spec/design/system.md:358` (the stress sentence): "since the WS9 harness" names a work label a reader cannot resolve. Fix: name the change by what it added.
- `internal/sync/engine_model_based_test.go:418` and `planner_model_test.go:688,704`: the "(rerun with VAR=N)" hints in the failure text lack `-race` and the test name; the replay line is now the rerun command. Fix: drop the hints or point at the replay line.
- Outside the repository: `~/agent/CLAUDE.md` says the post-merge stall window is 30 minutes after a push; with `verify-race` it is 140.

## Unfinished
None. Packages to test that I did not touch: none affected beyond devtool, cmd/devtool and internal/sync.

## Second pass

Commit 4b05eeb8 on 9d08ad70 (main's head), one commit, not pushed.

**Rebase.** Only `internal/sync/chaos_remote_test.go` stopped on a conflict (the test's comment, the subtest body, the helper block); `engine_model_based_test.go`, `system.md`, `non-functional.md` and `test_helpers_test.go` merged by themselves. As decided: main's `chaosRerunPrefix/Package/Timeout`, `chaosRerunLine`, `parseChaosRerunLine`, `parseChaosRerunFlags`, `declaredTestFuncs` and `TestChaosRerunLine_NamesTheTestTheSeedAndTheBatchCount` are gone and the chaos cleanup logs the fault summary only; main's `chaosRunOptions`/`chaosBatches`, fault layer, draws, log and `failureContext` stay; the chaos family calls `logSeedReplayOnFailure`; the two chaos header comments and the system.md sentence name the `seed-replay:` line, and `TestModelRun_ChaosFailureContextCarriesNoSecondRerunHint` carries `Validates: R-6.10.25`. After it: build, vet, tagged vet pass; `chaosRaceSeed` is 2; `TestModel_ChaosGraphConverges` plain passes (369 s).

**Failing first, on the unchanged code.** `TestRunVerifyFailedTestRerunsATimedOutSeedFromItsSubtestName` (the rerun was `-race -run=^T$/^seed=318011$` with no seed variable; with 318012's line in the log the env named 318012), `TestRunVerifyFailedTestFailsWhenTheSubtestItAskedForDidNotRun` (both cases returned nil), `TestRunVerifyFailedTestRefusesASeededFamilySubtestThatNamesNoSeed`, `TestNightlyFailureKeysKeyATimedOutSeedByItsSubtestName` (the test key; then only 318012's key), `TestSeedReplaysFromLogTakesOnlyDeclaredVariables` (GOTOOLCHAIN and GOFLAGS lines were read), `TestNightlyReportAppliesTheVerdictWhenTheSeedIssuesCannotBeRead` (status posted, nightly issue never written), `TestNightlyPreMergeGateRequiresAClaimedSeedsIssueToClose`, `TestNightlyCheckClaimsOpenSeedIssues` (no close line), `TestRunVerifySeedsSetsRotatedStart` (no dispatch-order entry), `TestLogSeedReplayOnFailure_CarriesTheDispatchOrder`, `TestModelRun_FailureContextCarriesNoSecondRerunHint` (plain and planner contexts held "rerun with"), `TestDeveloperOnboardingListsEveryVerifyProfile`. `TestSeededFamiliesLogTheirReplayLineWithTheirVariable` failed with the planner family's call taken out and passed once it was back.

### Items
1. Done. `seededFamilies()` beside the seed constants is the one table and `seedsRunPattern()` is built from it. `seedReplayFromSubtest` builds a family subtest's replay from `<Test>/seed=<n>`; `replayOfFailure` never takes another seed's line and refuses a family subtest that names no seed; `failingSeedsOf` serves `nightlyFailureNames` and the seeds profile's closing list; `verify failed-test` fails a rerun whose output reports no result for the test it asked for. A seed issue says when its line was built from the name.
2. Done: the clause is dropped ("three took 31 minutes in November 2026").
3. Done: `modelRun.failureContext` and a new `pmRun.failureContext` (the planner's two sites share it) name the seed only; the test is renamed `TestModelRun_FailureContextCarriesNoSecondRerunHint` and covers plain, chaos and planner.
4. Done: `seedAssignmentsDeclared` takes the line's family variable first, then at most `ONEDRIVE_GO_DISPATCH_ORDER` with `forward|reverse|shuffle:<n>`; the package must be `./internal/sync`.
5. Done: verify_seeds.go, ci.yml, R-6.10.25, CONTRIBUTING.md and the same sentence in CLAUDE.md say "a rerun on the same UTC day" and that a later rerun passes `--seed-start` with the printed start. The start line existed; its test now holds the counts and that it precedes go test's output. system.md's seeds paragraph says the seed issue keeps a failing seed from being forgotten.
6. Done: `seedsEnv` empties it; `seedReplayInputs` adds it when set; tests on both sides; the reader takes it.
7. Done as decided: the seed read's error is joined after the nightly plan is applied, no seed issue written; `plainGitHubFailure` turns it into GitHub's sentence, which names "seed label list" or "seed issue list".
8. Done: `TestSeededFamiliesLogTheirReplayLineWithTheirVariable` (go/ast over internal/sync's test files) holds the prefix, the package and the dispatch variable, and each family's `logSeedReplayOnFailure` call and `seed=` subtests. The chaos call now passes `modelSeedEnv` (it passed `chaosSeedVars().single`, the same value) so the check reads a constant.
9. Both halves: the check prints "the pull request that fixes the seed closes seed issue #N: put `Closes #N` in its body" under a claimed seed key with an open issue; the gate, when it holds a seed key, reads the open seed issues through the check's own `findSeedIssues` and requires `Closes #N` (keyword in any case, the whole number).
10. Done: 30m base, 240 core-minutes via `deadlineForCores`, `--race-timeout`/`VerifyOptions.RaceTimeout`; verify-race `timeout-minutes: 40`, push stall window 50; the deadline test pins the reference cores; race and seeds steps run `-count=1`; R-6.10.22 and both system.md places follow.
11. Done: the verify_stress.go comment is rewrapped with "31 minutes"; the seed issue names the CI job or jobs the seed failed in.
12. Done: all 13 profiles and five file-table rows in developer-onboarding.md. `VerifyProfileUsage` (now exported) is the one list, cobra's usage line is built from it, and `TestDeveloperOnboardingListsEveryVerifyProfile` holds the guide's verifier section to it.

### Deviations
- From the plan row: one commit holds the seed issue code and the rotation.
- Item 1, "a subtest name outside the table": read as a family subtest not named `seed=<n>`, which is refused. A `seed=` subtest of a test outside the table (`TestModel_SameSeedReplaysIdentically` runs fixed seeds 3 and 28) is still rerun by name, now under the guard that its result must be reported; refusing it would lose a working rerun.
- Item 1: a replay built from a subtest name carries no `-race`, whichever run failed; in the nightly it is a seed key with a seed issue, so a deadline in a race run of a default seed now opens a seed issue too.
- `VerifyProfileUsage` is exported so cmd/devtool's usage line reads the one list.

### Also fixed
- `findSeedIssues`: a list as long as its 500 limit fails the read instead of silently missing issues (`TestFindSeedIssuesRefusesAListThatMayMissSome`).
- The seeds profile's empty-run guard has its own test (`TestRunVerifySeedsFailsWhenNoPackageRanATest`).
- `TestLogSeedReplayOnFailure_LogsTheCommandThatRerunsTheSeed` no longer fails when the caller's environment pins a dispatch order.
- `TestCIStallWindow_PostMergeUsesOnlyPushJobs` asserts push <= pull request again.
- verify.go's deadline comments name the race profile among the core-scaled deadlines.

### Seed replays on the final tree (plain, `-timeout 30m`)
- 318012: fails. Batch 2, `sync_invariants_test.go:181`: "invariant: no empty conflict-copy reservation left at <a conflict copy> (scenario content is never empty)". The run printed its `seed-replay:` line.
- 318011: fails. `engine_model_based_test.go:511`: "sync did not converge", "batch 4 after the crash: still changing after 8 passes".

### Checks (final tree)
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l` on every changed Go file: no output.
- `go test -count=1 ./internal/devtool/... ./cmd/devtool/...`: pass; with `-race` behind the lock: pass (297 s).
- The four model tests, defaults, plain, one `-run`: pass (648 s).
- `verify seeds --seed-start 1`: pass (start=1 printed first, go test 175.8 s, total 3m20s).
- `go test -count=1 -timeout 60m ./internal/sync/...`: pass (650 s).
- `TestFindTestConventionViolations_RealRepoIsClean`: pass. No function over 100 lines.
- `verify lint` (golangci-lint and actionlint): `0 issues.`, log A11-lint2.log; a targeted lint before it found one gocritic issue, fixed.
- By hand: every test name the changed spec files, CLAUDE.md and CONTRIBUTING.md cite exists, and every requirement id cited is declared.

### Seen and not fixed
- `internal/devtool/verify_test_helpers_test.go:280-288`: `assertCommandHasEnvVar`/`assertCommandLacksEnvVar` print the whole process environment on failure (`assert.Contains` on the env slice), in CI every variable of the job. Fix: assert on the entries with the wanted name only.
- `internal/devtool/go_test_log.go:18-19`: the raw result prefixes lack `--- SKIP:`, which `verify_failed.go` now declares beside its one use. Fix: move it next to the others when that file is next changed.
- `~/agent/CLAUDE.md` (outside the repository): says the post-merge stall window after a push is 30 minutes; with this commit it is 50.



## Third pass (mine, 2026-11-14 23:00 to 2026-11-15 00:55 PST)

The second version (`4b05eeb8`) was reviewed read-only (findings: 0 must fix, 3 should fix, 12 notes; verdict accept). I did the should-fix items and most notes myself and amended the one commit.

- **Should fix 1, a replay built from a subtest's name lost the race detector.** A seed that go test's deadline ended prints no line, so its replay is built from `<Test>/seed=<n>`, and a log does not say whether that run had `-race`. The line still carries no `-race` (it would be a claim the log does not support), the seed's issue says so beside the command, and `verify failed-test` now reruns such a seed under the race detector (`failedGoTestRerunArgs`: `-race` when the line has it or the line was built from a name). Tests: `TestRunVerifyFailedTestRerunsATimedOutSeedFromItsSubtestName`, the caveat assertions in `TestNightlyReportOpensAnIssueForASeedThatPrintedNoReplayLine`.
- **Should fix 2, "deadline or a panic" was untrue.** A panic on the test's own goroutine still runs the cleanup that prints the line, and a panic on another goroutine ends the binary with no list of running tests, so nothing is built from a name there (such a failure is left with its job's key alone): the subtest-name path covers go test's deadline and nothing else. Code comments, `system.md`, R-6.10.25 and the commit message now say the deadline alone.
- **Should fix 3, one seed failing in two jobs of one run.** `TestNightlyReportOpensOneIssueForASeedThatFailsInTwoJobs` holds that it gets one issue naming both jobs.
- **Note 4** done. **Note 5**: one function, `seedReplay.rerunEnv()`, gives the rerun's environment, and it sets `ONEDRIVE_GO_DISPATCH_ORDER` empty when the line names none, so a value in the caller's environment cannot change what the seed does. **Note 6**: a seed's rerun has a 30-minute go test deadline (whether any seed needs more than go test's default ten minutes under `-race` on this host was not measured; the deadline makes the question moot). **Note 7**: a rerun in which the asked-for test skipped now fails and says so (`TestRunVerifyFailedTestFailsWhenTheTestItAskedForSkipped`); go test exits 0 on a skip. This holds for every `failed-test` rerun, not only for seeds.
- **Note 8**: `TestSeededFamiliesLogTheirReplayLineWithTheirVariable` now also requires the writer call inside the `seed=` subtest and that no test outside the table hands the writer its own `t`. Left as it is: `internal/sync/seed_replay_test.go` keeps its own small map of the families next to the tests that use it (lines 130 to 135); the devtool source check is what ties the two together.
- **Note 9**: `TestVerifyProfileUsageNamesExactlyTheProfiles` holds the usage line to the `VerifyProfile` constants and to the plans they resolve to. **Note 10**: "about ten" minutes in `ci.yml` and `system.md`. **Note 11**: five comments rewrapped. **Note 13**: `TestCISeedsJobRunsOnTheScheduleAndOnDispatchOnly`.
- **Left and named (notes 12 and 14)**: a seed whose issue read or write fails survives only in that run's log (the report fails, so the night is red, but no issue holds the seed); `findSeedIssues` refuses a list of exactly 500 open seed issues; `bodyClosesIssue` matches `Closes #N` inside a longer word. None is reachable in normal use; the first is the price of never writing a half-read issue list.
- **Note 15, a correction to "Second pass" above**: its "Rebase" paragraph cites `TestModel_ChaosFailureContextCarriesNoSecondRerunHint`; the test is named `TestModel_FailureContextCarriesNoSecondRerunHint`.
- **Checked and left unchanged**: `ci.yml`'s two "without the race detector" phrases describe the seeds job itself and are true; `non-functional.md`'s short-tier sentence is true.

Checks on the amended tree: `gofumpt -l` clean, `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`, `go test -count=1 ./internal/devtool/ ./cmd/devtool/` (both ok; the four `RealRepoIsClean` source checks are in the first), and the routing check of the repository guide (179 references, 0 dead). A first targeted lint over both devtool packages found one issue, a testifylint `require-error` in the new `TestVerifyProfileUsageNamesExactlyTheProfiles` (`assert.NoError` on an error); it was changed to `require.NoError` before the amend, and a second run over both packages reports 0 issues.

Rebase: the amended commit was moved onto main after #916 (`cf345986`) without a conflict and is `a516a226`. On that tree the build, both vets, the two devtool package test runs and the routing check passed again. Still to come when this was written: the whole pre-pr gate, which waits behind the heavy-run lock.



## Merged as #917 (`04983d12`, 2026-11-15 01:15 PST)

- One commit, `a516a226` on `cf345986`. The pre-pr gate passed at about 01:08 PST (1411 s: lint 5m36s, build 1m54s, short unit tests 4m03s), the pre-merge stage in 261 s. The post-merge stage merged it at 01:15 PST and then waited for the push CI of main, which for the first time holds the race job.
- Installed `04983d12` at 01:18 PST; the watch client reports the test drive up to date. The change holds no product code (devtool, the CI workflow, test files and docs).
- What only GitHub can show, and where it is recorded once seen (STATE.md, "For the final report"): how long `verify-race` takes on GitHub's runner on the push of the merge; the first scheduled run with the `model-seeds` job; and the two seed issues that run is expected to open, because seeds 318011 and 318012 of `TestModel_CrashAtSeededPointsRecovers` belong to the UTC day 2026-11-15. A run started on a later UTC day runs other seeds and says nothing about these two.
- Follow-up rows: W50 (seed 318012) and W51 (seed 318011), briefs written; each fix closes its seed issue with `Closes #N`. A17 and W45 were waiting for this merge.
- After this merge every push to main runs the whole sync tree under the race detector, so the whole race tier on the development host is no longer a duty of mine between sync-lane merges; a sync-lane branch still runs `-race` over its own tests before it is handed back.

- Seen on GitHub (2026-11-15): on the push of the merge (run 43698353290) the job `verify-race` passed in 12 min 14 s (09:15:23Z to 09:27:37Z) and the aggregate `verify` check passed at 09:27:43Z. The post-merge stage, which now waits for that job, passed after 761 s and removed the worktree. Still to be seen: the first scheduled run with the `model-seeds` job and its two seed issues.


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
