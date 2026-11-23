# A21 report: the stress lane's fifty-times steps get declared members and a budget

Commit `7eb65097` on `ci/stress-x50-members` (one commit on `c0792e98`, not pushed). 12 files.

## What changed

- `internal/devtool/verify_stress.go`: `stressRepeatRuns` (50), `stressRepeatMargin` (1.5), per-step pass budgets and the per-member run budget (each with its measured figure and date), `stressRepeatedStep` / `stressRepeatedPackage` / `stressNonMember` / `stressPlacementRule`, the two steps built from them (`-json`, anchored `-run`, derived `-timeout`), the per-member check after go test, slow-test heading now "slowest completed race tests". Pattern constant gone.
- `internal/devtool/verify_stress_members.go` (new): the declarations: sync 217 members in five family lists, 28 non-members with reasons, the sync placement rule; multisync 177 members in three lists, rule "every test".
- `internal/devtool/verify.go`: the 20m and 80m constants removed; the deadline comment names the derived ones.
- `.github/workflows/ci.yml`: the `stress` job's timeout comment (25 + 75 + 90); the timeout stays 200.
- Docs: `spec/design/system.md` (stress paragraph, ownership lists), `CLAUDE.md` (stress lane line), R-6.10.8 (one sentence), LI-20261115-02 fix paragraph.
- Tests updated: `TestRunVerifyStressRunsExpectedSteps`, `TestRunVerifyStressSyncRaceStepTakesTheRaceDeadlineOncePerRun`, `TestRunVerifyStressRunsEveryStepAfterOneFails`, the slow-summary heading checks; `TestRunVerifyStressFailsWhenConcurrencyPatternMatchesNothing` renamed `TestRunVerifyStressFailsWhenNoMemberOfTheSyncStepRan`.

## Tests added (each seen failing first, `a21/red2.log`, `red4.log`)

- `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames`: the real tree: every rule-named test placed, every declared name exists, none twice (quick lane).
- `TestStressDeclarationCheckNamesAnUndeclaredTestAndBothLists`: an undeclared "Drain" or prefix test fails, naming the test and both lists.
- `TestStressDeclarationCheckFailsWhatTheSourcesDoNotHold`: a vanished member or non-member, a duplicate, a reasonless non-member, an undeclared test under the every-test rule.
- `TestStressRepeatedStepsRunExactlyTheirMembers`: `-run` is the anchored members; no extension, non-member or new "Drain" test matches.
- `TestStressRepeatedStepsDeadlinesFollowFromTheirPassBudget`: deadlines = budget x 50 x 1.5, literals on 4 and 2 cores; member budget per cores.
- `TestRunVerifyStressFailsAStepWhoseMemberRunsOverItsBudget`: one line naming test, time and budget; a slow subtest adds none; the go test failure stays in the error.

## Numbers

- Counts: sync step 245 before, 217 members after (28 non-members); multisync 177 before and after.
- Deadlines on a runner: multisync 20m -> 25m (20 s pass budget), sync 80m -> 75m (60 s pass budget); on 2 cores 56m15s and 7h30m (small-host budgets 90 and 720 core-seconds). Job: 25 + 75 + 90 + 10 = 200 = timeout-minutes.
- Room over the last three scheduled nights: multisync 4m43 / 11m33 / 11m57 is at most 48% of 25m; sync 11m32 / 23m58 / 39m23 at most 53% of 75m (dispatch runs 20m05 and 29m32 too).
- Per-member budget: 30 s on a runner, 90 s on 2 cores; slowest member measured 21.0, 8.4 and 11.5 s on 2 cores.
- By-hand run of the exact sync step command with `-count=1` (`a21/hand-sync.log`): 217 top-level tests ran, 0 failed, wall 84.5 s.
- Multisync growth explained: the eight child-process panic-boundary tests of #897 run serially at about 1 s each, 8.6 s a pass; CI grew 8.2 s a pass.

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `gofumpt -l internal/devtool cmd/devtool`, goimports: nothing.
- `go test -count=1 ./internal/devtool/ ./cmd/devtool/`: ok; with `-race` behind the lock: ok.
- `RealRepoIsClean`: 4 PASS.
- Lint (devtool, cmd/devtool, behind the lock): 0 issues.

## Deviations

- The ~300-name stop read per declared list: sync 245 names, multisync 177; together 422. If it meant both, this is the stop condition.
- Multisync declares every test of the package as a member (the step repeated the whole package), so a new multisync test fails the check until declared.
- Both repeated steps now run through `go test -json` (the per-member check needs the times) and record their slowest tests in the summary.
- The status lines print the full command, the sync `-run` being 11,825 characters.

## Seen and not fixed

- `internal/multisync/panic_boundary_test.go:185` and seven siblings: each reruns the test binary in a child process, serially, about 1 s; about 7 minutes of every multisync night. Fix: run them in parallel where their hooks allow.
- `internal/sync/engine_pending_create_intent_test.go:126` `TestWatchRuntime_ReplanKeepsAFreshCreateIntentForTheBatchThatShowsItsFile`: the slowest member, 8.4 to 21.0 s a run on 2 cores. Fix: profile what it waits on.
- `internal/sync/engine_shortcut_release_test.go` `TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain`: 10.6 to 25.3 s, the slowest test of the old selection; no longer repeated, still slow in the unit tiers. Fix: profile it.
- Nightly reader (`CLAUDE.md` nightly rule): a go test step is keyed by its marker only when the job's log names no failed test, so a member over budget on a night with a test failure gets no key of its own. Fix: key a budget line by its member.
- `.github/workflows/ci.yml:52`: the header still says `stress` is "race repetition of ./internal/multisync and the ./internal/sync/... tree"; outside the stress job's timeout, so not edited. Fix: name the declared members.
- `spec/design/system.md:768`: the [planned] watch-ordering note calls the step `sync race x50` with its old pattern. Fix: name the step `sync-concurrency-race-repeated`.
- Unknown, not measured: why the same 244 sync tests took 29.5 and 39.4 minutes on the two runs of 2026-11-15.

## For the reviewer

- Packages to test beyond devtool: none (no product code changed). Declared names from `internal/multisync`: all 177 of `stressMultisyncMembers`; the branch that renames multisync tests must place them on rebase.

## Review fixes

Amended into the one commit on `ca287a32`: HEAD `12576ffd`, 16 files (new: `internal/devtool/go_test_text.go` and its test; now also touched: `go_test_log.go`, `verify_test_helpers_test.go`). Not pushed. The multisync names above are gone (item 2), so the renaming branch has nothing to place.

1. Per-member bound (must fix 1). `verify_stress.go` `stressMembersOverRunBudget` takes the step's timings and folds every subtest's runs into its member (name before the first `/`, slowest kept, a tie goes to the member itself); one line per member, sorted by package and name, naming the subtest when that run was one ("..., of subtest X/y, over the 30s budget of one run"). The rule in words (a member fails its step when the slowest run of the member itself or of one of its subtests is over the budget) is in the constants comment, `stressCommandSpec.memberRunBudget`, `stressMemberRunBudget`, R-6.10.8, system.md and the commit message. Fast-parent case (parent 10 ms, subtest 31.5 s) added to `TestRunVerifyStressFailsAStepWhoseMemberRunsOverItsBudget`: red on the tip (`a21fix/red-fast-parent.log`: only the Spins line came), green after.
2. Smaller form (finding 4). `stressRepeatedStep.selection *stressSelection` (packages and placement) is nil for multisync: no `-run`, nothing declared. The 177 names, `stressMultisyncPackage`, `stressMultisyncPlacement` and `stressPlacementRule.everyTest` are gone. `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` asserts multisync has no selection and checks the sync step only. Pass budget, derived deadline and the per-member check stay for both. "A new multisync test needs no declaration" is said in the members file header, CLAUDE.md, system.md, LI-20261115-02 and R-6.10.8.
3. a. `runStressCommand` returns a `stepOutcome` with `failedTests: parseGoTestLog(output).failedTopLevelTests()` for all three steps. b. `go_test_text.go` `goTestTextWriter` wraps stdout for all three steps, the tree step included: nothing reads raw events from the job log (the nightly reads it through `parseGoTestLog`, which the new tests show reads text and events alike; the marker reader skips events). Package-level output as it comes; a test's output buffered from its `run` event, written on `fail`, dropped on `pass`/`skip`; before a package's result line (also at its result action and at `Flush`) the output of its tests that never ended; `build-output` written; non-event lines as they are; output of a test that already ended at once. The tee still captures the raw events for the empty-run guard, the per-member check, the slow-test summary and the failed tests. `goTestActionRun` and `goTestActionBuildOutput` added to `go_test_log.go`. The fake runner's `-json` output now carries the package result line go test prints, and `TestRunVerifyStressStreamsTheRepeatStep` checks that this line reaches stdout while the step runs (a passing test's line no longer does).
4. `TestStressDeclarationsAreInNameOrder` (finding 3): every member list and the non-members; a failure names the first name out of order.
5. ci.yml header (fifty race runs of every multisync test and of the sync tree's declared members, then three of the whole tree) and system.md's watch-ordering note (`sync-concurrency-race-repeated`, the quoted `-run` kept).
6. Note 7: all four exist. `TestPhase0_RunWatch_BootstrapQuiescesWithFutureHeldRetryWork` to `stressSyncRunWatchMembers`; `TestRemoteObserverWatch_BacksOffAfterATimedOutPollInsteadOfStopping` and `TestHandleWrite_DeleteClearsTimer` to `stressSyncWatchMembers`; `TestRealClock_ATimerPanicWithNoRunBoundIsOnlyLogged` to `stressSyncShutdownAndCancellationMembers`. The check already allowed a member the rule does not name; comments and system.md now say so.
7. Note 8: "tests of the watch, one-shot, worker-pool, cancellation and shutdown code" in the members file, `runStress`, system.md, CLAUDE.md and the commit message.
8. Note 9: one plain run of the ten `TestRunOnce_` non-members here (`-count=1 -parallel=1`, no race, subtests folded, `a21fix/runonce-timing.log`): the slowest took 0.37 s, the package 4.5 s. None is over 5 s, so all ten are members again, in `stressSyncOneShotAndWorkerPoolMembers` ("the one-shot result loop, whole one-shot passes and the worker pool"). `stressNonMemberRemovedFolderPass` removed; non-member comment now: "a state of a shortcut root, a release rule, a log summary or a lookup structure, not concurrency. When in doubt a test stays a member: a one-shot pass that runs the worker pool is one, whatever its subject."
9. Note 10: the status line reads `-run <the 231 declared members of sync-concurrency-race-repeated>`; multisync's is its whole command. Note 11: the lists are functions in `stressRepeatedPackage.memberLists` / `nonMemberList`, and the failure names them ("add it to a member list, stressSyncRunWatchMembers, ..., or with its reason to the non-members, stressSyncConcurrencyNonMembers, in verify_stress_members.go"; for a package the step does not declare, "a declaration of <dir>"). Note 13: `summarizeStressSlowTests(output, 0)` once; the slow list is its head and the budget check reads the same timings. Note 14: system.md gives the 20 s from the multisync fifty (11m57s) and the 60 s from the same run's fifty passes of the 244 tests the step ran before its members were declared (39m23s).

Counts now: sync 231 members (lists of 25, 46, 70, 35, 55) and 18 non-members, 249 declared names; multisync none (422 declared names before). Recounted in `verify_stress.go` from the writer's two 2-core race passes of 2026-11-15 (`a21/sync-race-pass{1,2}.log`): the 227 members among those tests took 96.2% and 95.8% of their summed time (stated as 96%; the 217 took 71 and 75%), the other 4 were not in those passes; the slowest member took 25.3 and 10.6 s (`TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain`; was 21.0 and 8.4). The 244 in two comments is CI's measured old selection and stays. The spec states no member count.

By-hand pass of the sync step's exact command for this host (`go test -json -count=1 -shuffle=on -timeout=7h30m0s -run <the 231> ./internal/sync/...`, without `-race`, behind the lock, `a21fix/hand-sync.log`): 231 top-level tests ran, equal to the declared members, 0 failed; wall 13.8 s (package 10.1 s; slowest 0.65 s).

Tests added or rewritten, each seen failing first:
- `TestRunVerifyStressFailsAStepWhoseMemberRunsOverItsBudget`, fast-parent case: red on the tip (above).
- `TestRunVerifyStressLogsTextAndNamesTheFailedTests`: red on the tip on all five assertions (raw events and a passing test's chatter on stdout, `FailedTests` nil; `a21fix/red-text-summary.log`).
- `TestGoTestTextWriterKeepsWhatTheLogReaderFinds` (a failing test, a data race, a panic, a deadline, a build failure, no test ran, a run cut off), `TestGoTestTextWriterLeavesOutWhatPassed`, `TestGoTestTextWriterReturnsTheLogsFailure`: red against a stub that wrote nothing (`a21fix/red-writer.log`: 6 of 7 cases failed the parse equality, every case the content check). Streams built by hand from invented names (the event shapes checked first against real go test output of a throwaway module), fed in 7-byte pieces.
- `TestStressDeclarationsAreInNameOrder`: red with the 14 added names at the ends of four lists (`a21fix/red-sorted.log`), green once sorted.
- `TestStressDeclarationCheckNamesAnUndeclaredTestAndItsLists` (renamed from `...AndBothLists`) and the new undeclared-package case of `TestStressDeclarationCheckFailsWhatTheSourcesDoNotHold`: red with the old message put back (`a21fix/red-lists-message.log`), green with the new.

Checks: `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass. `gofumpt -l` and `goimports -l` on devtool and cmd/devtool: nothing. `go test -count=1 ./internal/devtool/ ./cmd/devtool/`: ok on the final tree. `RealRepoIsClean`: 4 PASS. Lint once, last, behind the lock (`--timeout=45m0s`, `GOTOOLCHAIN=go1.26.11`, `a21fix/lint.log`): 0 issues.

Decided differently, and why:
- The list functions are held as function values (`memberLists`, `nonMemberList`) so the check names them by reflection in the test; strings naming them would drift on a rename.
- The multisync step has no selection at all rather than an every-test placement rule, so "declares nothing" is a fact of its type.
- A failed test's output keeps go test's `-v` framing lines (`=== RUN`) as the events carry them, not reordered into the non-`-v` layout; `parseGoTestLog` reads both alike.
- Also fixed: `stressCommandSpec.collectSlowTests` was true for every step, so it is gone; the fake's `-json` output lacked the package result line go test prints.

Seen and not fixed:
- `internal/sync/engine_shortcut_release_test.go` `TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain`: 0.37 to 0.63 s in plain runs, 25.3 s in a 2-core race pass; now a member and the slowest. Fix: profile it under `-race` beside its parallel siblings.

## Second review fixes

Rebased onto `origin/main` `0e31e63c` before the edits (clean), amended into the one commit: HEAD `38a7be02`. Logs are in the scratch directory, prefixed `A21-`; the real event files and their texts are in `a21fix2/`.

Real runs ("To run" 1 and 2, throwaway module `a21fix2/t2j`, go1.26.11): in the hang run the timeout panic, its running-test list and the whole goroutine dump are output events of the test that passed, after its own `--- PASS:` line, and that test's pass event comes just before go test's `FAIL` line; in the race run the race report is credited the same way to the quick test that passed, and the racing test fails with only "race detected during execution of test". Through the old writer the hang's text lacked the panic and its parse had no deadline and no timed-out test; through the new one both texts hold the blocks and both parses equal the events' (`a21fix2/scratch-summary.txt` before, `scratch-summary-after.txt` after). The paused shape did not come from a real run: a parallel subtest's pause is followed by `=== NAME <parent>` when `t.Run` returns in -json mode (testing.go:2115-2117), so the race in `a21fix2/pause.json` went to the parent, and a top-level test's window between its PAUSE line and the next framing line is too short to hit on purpose. That case is built from lines of the real runs.

Healthy output: five real passing runs went through the new writer (a multisync subset run here, one multisync race pass whose tests print 212 raw `slog.Default` lines, two sync race passes, the earlier by-hand sync pass): each text is only the seed, `PASS` and `ok` lines. In those runs no output event at all came while a test was paused or after its own result line (`a21fix2/rawlines.py`). The writer is 326 lines.

1. Must fix 1. `go_test_text.go`: output credited to a test while it is paused (pause to cont event) or after its own `--- PASS:`/`--- SKIP:` line is written at once (`writeOutput`); a passing or skipping test's held output gives only its race reports (`goTestRaceReports`, rule line to closing rule line or the end). The reports are written at the test's own result line, so they stay before what follows it, and anything left at its pass or skip event. New cases of `TestGoTestTextWriterKeepsWhatTheLogReaderFinds`: "a deadline credited to a test that passed" and "a race credited to a test that passed" (the two real shapes, invented names), "a race credited to a paused test", "a race held by a test that passes" (its other line asserted absent). All four red on the old writer (`A21-red-writer.log`; the deadline case's parse had no deadline), green after (`A21-green-writer.log`). The type comment, the system.md sentence and the commit message say what holds now.
2. Must fix 2. `TestRunVerifyStressFailsAStepWhoseMemberRunsOverItsBudget` gives the multisync command one run of 31.5 s and asserts its budget line and the multisync step's fail status; `stressRunEvents` takes the package. Red with `memberRunBudget` set only for a step with a selection (`A21-red-multisync-budget.log`: line missing, status pass), green restored. system.md now says the multisync step, which declares no members, bounds each of its tests the same way, citing that test.
3. Should fix 3. Both `TestReleaseLocalTree_` tests moved to `stressSyncOneShotAndWorkerPoolMembers` (name order). Main's new `internal/sync` copy of `TestWatchHelpers_FailNamingWhatTheyWaitedFor` is a non-member with the reason "the failure messages of the tests' wait helpers, in a child test binary"; the non-member comment names that subject. Counts: 233 members, 17 non-members, 250 names (commit message; no spec or CLAUDE.md sentence names a count, `git grep -l`).
4. Should fix 4. The writer keeps each package's text together: the first package with an event streams, every other one holds its text (package lines, failed and unfinished tests' blocks, result line) until the packages before it ended; build output and non-event lines at once; `Flush` ends the packages in order. Cases "two packages whose events interleave" (red: text interleaved, parse lost the failed test and the first package's seed) and "two packages, one cut off" (red: the second package's block came before the first's unfinished test), green after.
5. Note 5. `verify_stress.go` pass-budget comment: no member count, no "not timed"; "Two race passes of that selection ..., 245 tests there, ... the members among them took 99% of their summed time in each", recomputed with `a21fix/share.py` over `a21/sync-race-pass{1,2}.log` and the 233 members (229 of them in those passes): 99.1% and 99.0%.
6. Note 6. `verify_test_match.go` `runGoTeed` comment: a streaming step keeps every printed line; a step whose text comes from events holds a running test's output until it ends, so a job killed from outside loses it, and go test's own deadline comes first.
7. Note 7. The capture comes first in both `io.MultiWriter`s. `TestRunGoTeedKeepsTheCaptureWhenTheLogRefusesAWrite` (fits the existing fake runner, no new seam): red with the old order (`A21-red-capture.log`: empty capture, the guard said "matched no tests"), green after.
8. Note 8. The cases assert whole blocks: the race report with its stack lines (`gizmoRaceReport`), the timeout panic with its running-test list and goroutine lines (`gizmoTimeoutPanic`), the build failure with its `# pkg` header; parse equality for every case.
9. Note 9. `ci.yml` header rewrapped; the block's longest line is 77.
10. Note 10. Repository CLAUDE.md: "each fifty-times step's deadline".
11. Note 11. `stressFuncName` takes the test and requires a non-empty name; the check helpers carry the test through.

Checks: on the rebased tree: `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass; `gofumpt -l` and `goimports -l` on devtool: nothing; "To run" 3 once: ok; behind the lock `go test -count=1 -timeout 60m ./internal/devtool/`: ok (`A21-devtool-test.log`), with `-race`: ok (`A21-devtool-race.log`), and once more on the committed tree after the lint fix: ok (`A21-devtool-final.log`); `RealRepoIsClean`: 4 PASS; lint of `./internal/devtool/...` (`reports/A21-lint.log`): first one unparam issue (the timeout-panic helper always got the same test; the parameter went), then 0 issues; `go run ./cmd/devtool verify lint`: pass (lint 4m49s, workflow lint 2 s, `A21-verify-lint.log`). Not run: any x50 stress tier, `devtool verify stress`, any test with a build tag, anything against OneDrive or GitHub. One fetch of `origin/main` after the amend: still `0e31e63c`. No push, no PR.

Install: not installed. The commit changes no code of the onedrive-go binary (only `cmd/devtool` imports `internal/devtool`), so a build of it is `main` at `0e31e63c`; the installed client is the bounded-response-body branch, `main` at the same commit plus a client fix, and reinstalling would take that fix out of the running watch client. The agent CLAUDE.md line stays as it is; the CHANGELOG says so.

Seen and not fixed:
- The multisync copy of `TestWatchHelpers_FailNamingWhatTheyWaitedFor` joins the multisync step, which runs every test, and starts five child test binaries a run: one race run took 0.22 s here (`A21-msync-helpers.log`), so nothing to fix.
- `internal/multisync/orchestrator_test.go:96,104`: the test orchestrators log through `slog.Default()`, so their lines go raw to stderr and test2json credits them to whichever test the last framing line named; in a paused or after-result window the writer writes them. None did in the real passes. Fix: give the test orchestrators a test logger.
- testing.go:2019-2023: a goroutine that logs between its test's result line and `t.done` is credited after that line and written; that is a test logging after it returned, itself a race.
