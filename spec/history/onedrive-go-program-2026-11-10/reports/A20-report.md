# A20 report

Commit `da36df37` on `ci/stress-race-deadline`, one commit on `341d9820`,
not pushed. 9 files, +201 / -75.

## What changed

- `internal/devtool/verify_stress.go`: `stressSyncRaceRuns` (3) is the one
  constant. `stressSyncRaceTimeout(cores)` is runs times
  `raceSyncTimeoutForCores`. `stressSyncRaceStepName()` and
  `stressSyncRaceCommand(cores)` build the name, `-count`, status line and
  args from it. `runStress` takes `cores`. Comment gives the measurements.
- `internal/devtool/verify.go`: `stressSyncTimeout` ("40m") removed. New
  `VerifyOptions.Cores` (no flag; zero is GOMAXPROCS) and `verifyCores`,
  used by the unit, short, race and stress deadlines. Const-block and
  option comments updated.
- `internal/devtool/verify_race.go`: `raceSyncBaseTimeout` comment gives the
  measurements with dates; says `--race-timeout` is the race profile's alone.
- `internal/devtool/verify_ci_deadline_test.go`: the helper sets
  `Cores: unitTestReferenceCores` instead of three explicit timeouts, so it
  sees every derived deadline as a runner gets it, on any host.
- `internal/devtool/verify_runner_test.go`, `verify_test_match_test.go`:
  stress tests take name, count and args from the constant; stale
  "about 10.4 minutes" and "forty minutes" comments corrected.
- `.github/workflows/ci.yml`: `stress` job `timeout-minutes` 120 -> 160
  (20 + 40 + 90 + 10 of setup) and its comment. Nothing else.
- `spec/design/system.md`: lines 388 and 520 only (no reflow).
- `spec/reference/live-incidents.md`: index row and entry LI-20261115-02 (written as -01; renumbered at the rebase, since main took -01 meanwhile).

## Tests

- New `TestRunVerifyStressSyncRaceStepTakesTheRaceDeadlineOncePerRun`
  (2 and 4 cores: deadline, `-count`, status line, step name from the
  constant; the other two steps keep 20m/40m). Seen failing: `-timeout=40m`
  against `6h0m0s` and `1h30m0s`.
- `TestCIJobTimeoutsCoverEveryGoTestDeadline` (unchanged): seen failing on
  the old 120 ("2h30m0s plus 10m0s of setup exceed ... 2h0m0s"), passes at 160.
- `TestRunVerifyStressRunsExpectedSteps`: now expects the derived deadline.

## Checks

1. build, vet, tagged vet: pass.
2. gofumpt -l: no output.
3. devtool tests: pass; with `-race`: pass.
4. `TestFindTestConventionViolations_RealRepoIsClean`: pass.
5. Lint: `0 issues.` (A20-lint.log).
6. Cited tests exist; spec diff 2 lines in system.md.
7. `git status --short --untracked-files=no`: empty.

## Deviations

- Interface: new `VerifyOptions.Cores` field (no CLI flag). Without it the
  job-timeout test reads this host's cores, 360m on 2 cores.
- The workflow's stress-job comment was rewritten with the timeout.

## P4 and P5

- P4: done. live-incidents.md records CI incidents of this kind
  (LI-20260412-04), so the entry was added. No cause written.
- P5: base kept at 30 minutes. No test or comment states a headroom rule
  for the base; the only headroom rules are the small-host budgets.

## Seen and not fixed

- `.github/workflows/ci.yml:168`: verify-race comment says "A healthy run
  here takes about ten minutes"; measured 12.2 and 15.8 with setup. Fix:
  give the measurements (left: brief said to change nothing else there).
- `internal/devtool/verify.go:699`: `runLint` takes its cores from
  GOMAXPROCS, so `VerifyOptions.Cores` does not reach the linter's
  deadline. Fix: `lintSteps(cores)` with a closure, as `formatStep` does.

Packages to test beyond mine: none.


## Review fixes (done by the orchestrator, 2026-11-15)

Commit `524bec02` on main `12a48bb2`, one commit, 10 files.

All 13 review items applied:

- `verify.go`: `stressSyncConcurrencyTimeout` is `80m` with a comment that
  gives the three measured nights (11.5, 24.0, 39.4 minutes); the `Cores`
  comments say "zero or less" and "go test deadlines".
- `verify_stress.go`: the `runStress` comment gives one pass (about 47 s)
  and the fifty (39.4 minutes) instead of "well under a minute".
- `verify_runner_test.go`: the stress tests pin `-count=3`, `-timeout=20m`,
  `-timeout=80m`, `-timeout=1h30m0s` (runner) and `-timeout=6h0m0s`
  (2 cores) and the step name `sync race x3` as literals; no want is
  computed from the constants under test. Comments say "whole-tree step".
- `ci.yml`: the stress job's `timeout-minutes` is 200 (20 + 80 + 90 plus
  10 of setup; everything outside the verify step took 9 seconds on
  2026-11-15); the verify-race comment gives 12.2 and 15.8 minutes.
- `system.md` and `live-incidents.md` (`LI-20261115-02`, `Recurring: yes`,
  linked to `LI-20260412-04`): the run ids, the x50 step's three nights,
  the selection growth 214 to 244, the 9 seconds of setup, the fix text.

Found while fixing, from the CI step times (the reviewer had no `gh`):
the step before the failed one, `sync concurrency race x50`, passed the
same night 37 seconds short of its own fixed 40 minutes. Its deadline is
raised here as a measured number; its selection by name substring and its
missing budget are plan row A21, which holds the table of five runs.

Mutation checks (each reverted):

- `-count` 3 to 4: `TestRunVerifyStressRunsExpectedSteps`,
  `TestRunVerifyStressSyncRaceStepTakesTheRaceDeadlineOncePerRun` and
  `TestCIJobTimeoutsCoverEveryGoTestDeadline` fail.
- concurrency deadline 80m to 85m: the same three fail.
- job timeout 200 to 190: `TestCIJobTimeoutsCoverEveryGoTestDeadline` fails.

Checks: `gofumpt -l` clean; the devtool tests named above and the
`TestSummarizeStress` family pass; `RealRepoIsClean` four PASS; the pre-pr
gate on the amended commit (result in the "Merged" section).

Not known: why one race run of the tree grew; why the x50 step took 3.4
times as long in two days; why `multisync x50` more than doubled between
11-13 and 11-14.


## Merged as #922

- Squash-merged on 2026-11-15 at about 14:40 PST; main is `b8b72df8`
  ("Give the stress lane's sync steps deadlines their runs fit").
- Gate: the first local pre-PR run failed on the host ("disk quota
  exceeded" under `/tmp` in a bench test), not on the change; after
  freeing space the rerun passed. PR CI and the review-thread gate
  passed.
- Installed at 14:43 PST (`onedrive-go --version` names `b8b72df8`).
- The nightly claim on the stress lane was this branch's; the next
  scheduled night shows whether the lane fits its new deadlines.
- Left for other rows: the fifty-times step selects its tests by a
  substring of their names (plan row A21).
