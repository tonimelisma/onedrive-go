# A20 Definition of Ready

1. PASS **Task intent**: repo maintenance. The stress lane's race runs of the
   sync tree take their go test deadline from the race profile (runs times
   `raceSyncTimeoutForCores`), from one run-count constant.
2. PASS **Non-goals**: what the steps test; the nightly reader; new profiles,
   jobs or flags; the product; the race profile's 30-minute base.
3. PASS **Governing docs**: `spec/design/system.md` (governs
   `internal/devtool`), R-6.10.8 and R-6.10.26 in
   `spec/requirements/non-functional.md`, `spec/reference/live-incidents.md`.
4. PASS **Authority owner**: `internal/devtool/verify_stress.go` owns the
   stress steps; `verify_race.go` owns the per-run race deadline;
   `verify.go` resolves the cores (`verifyCores`); `.github/workflows/ci.yml`
   owns the job timeout, held by `TestCIJobTimeoutsCoverEveryGoTestDeadline`.
5. PASS **Code scope**: `internal/devtool` (three source files, three test
   files), `ci.yml` (stress job's timeout and its comment only), two spec
   files. No product package touched.
6. PASS **Regression plan**:
   `TestRunVerifyStressSyncRaceStepTakesTheRaceDeadlineOncePerRun` (seen
   failing at `-timeout=40m`), `TestCIJobTimeoutsCoverEveryGoTestDeadline`
   (seen failing on the old 120 minutes).
7. PASS **Verification plan**: build, vet, tagged vet, gofumpt, devtool tests
   with and without `-race`, the test-convention check, targeted lint. No
   verify profile, stress, e2e or integration run.
8. PASS **Data safety**: no sync state, local or remote file is touched;
   devtool tests run a fake runner.
9. PASS **Secret and credential safety**: no token, `.env`, `.testdata` or
   account name read or written.
10. PASS **Live provider and destructive operations**: none; no push, no
    history rewrite, no dependency change.
11. ASSUMPTION **Review carryover and nightly**: the brief states the DoD start
    stage has already been run in this worktree; no carryover threads or
    nightly claims were handed to this increment.
12. ASSUMPTION **Unknowns and assumptions**:
    Question: do the CI runners have at least 3 cores?
    Current assumption: yes, as the repository already states
    (`verify_ci_deadline_test.go`, `verify-race` job comment).
    Validation step: the next nightly's stress step runs with
    `-timeout=1h30m0s` in its status line.
    Owner document/code path affected if assumption is wrong:
    `.github/workflows/ci.yml` stress job, `internal/devtool/verify_race.go`.
