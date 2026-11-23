# A2 Definition of Ready: devtool gates that work on a 2-core host; CI wait that survives a cancelled shard

Worktree `~/src/onedrive-go-wt/devtool-host-gates`, branch
`fix/devtool-host-gates`, base `origin/main` at `70301c0e`. Written 2026-11-11,
before any code edit.

1. **PASS — Task intent.** A repo-maintenance outcome: the repository's own
   verification tool (`cmd/devtool`) works unaided on a host with 2 cores and a
   system Go newer than the pinned toolchain, and its two CI waits survive a
   job the runner cancelled. Six changes (decision D7, `PLAN.md` line 149 and
   280): pin `GOTOOLCHAIN` from `go.mod` and check the linter's build
   toolchain; a `pr` profile (lint, then quick) that `--dod --stage pre-pr`
   runs by default; a unit-test timeout that scales with cores; `worktree add`
   fetches `origin main` first; post-merge cleanup removes exactly the
   `.worktreeinclude` entries; the CI waits use a stall window and rerun a
   cancelled job of the head commit at most twice.

2. **PASS — Non-goals.** No e2e bucket derivation (analysis section 1); no move
   of repo-consistency checks to linters (section 2); no removal of devtool's
   import of the sync store (section 4); no change to `nightly check` or
   `nightly release` (section 5). No change to product code outside
   `internal/devtool` and `cmd/devtool`. No change to which CI jobs exist or to
   their `timeout-minutes`.

3. **PASS — Governing docs.** Read: `spec/design/system.md` (line 3 `GOVERNS:`
   covers `internal/devtool/*.go` by glob; verification section lines 236-273;
   the lint-version paragraph lines 374-382; the CI-tier paragraph line 423);
   `spec/requirements/non-functional.md` R-6.10.1, R-6.10.2, R-6.10.16,
   R-6.10.21, R-6.10.22 (lines 180-201); repository `CLAUDE.md` Dev Process
   (lines 215-354); `.github/workflows/ci.yml` lines 1-215; `.golangci.yml`;
   the analysis `09-devtool.md` lines 297-423 and 551-648 (no authority; each
   claim relied on was checked against the code).

4. **PASS — Authority owner.** `internal/devtool` owns all of it: the verify
   profiles and their command environment (`verify.go`, `verify_quick.go`),
   the lint-version gate (`verify_lint_version.go`), worktree bootstrap and its
   inverse (`worktree.go`), and the DoD stages with their CI waits and cleanup
   (`verify_dod.go`). `cmd/devtool/main.go` only maps flags to
   `devtool.VerifyOptions`. The source of truth for the toolchain is `go.mod`,
   for the lint pin and the job timeouts `.github/workflows/ci.yml`, for CI
   state GitHub as read through `gh`. Side effects stay where they are today
   (local commands, `gh` reads) plus one new write: `gh run rerun <id>
   --failed`, issued only by the two CI waits. User-facing output is the
   tool's status lines on stdout and its error text.

5. **PASS — Code scope.** `internal/devtool/verify.go`, `verify_quick.go`,
   `verify_lint_version.go`, `worktree.go`, `fsutil.go` (an `lstat` helper),
   `verify_dod.go`, two new files
   `toolchain.go` and `verify_dod_ci_wait.go` (both covered by the existing
   `GOVERNS:` glob), their `_test.go` files, `cmd/devtool/main.go` and
   `main_test.go`; docs `CLAUDE.md` (which `AGENTS.md` and `GEMINI.md` link
   to), `spec/design/system.md`, `spec/requirements/non-functional.md`,
   `CONTRIBUTING.md`, and the `ci.yml` header comment that called
   `verify default` the local gate. (Corrected after the commit: the two
   comments about devtool's 20-minute deadline stay true and are unchanged;
   `CONTRIBUTING.md` and the header comment would otherwise have drifted.)
   Must not be touched: `internal/devtool/nightly.go`
   behaviour, `verify_e2e.go` bucket logic, any package outside devtool, the
   main checkout, `~/tommi`, `/srv`.

6. **PASS — Regression plan.** Tests first, all with the package's fakes
   (`fakeRunner`, an injected `now`/`sleep` pair), none calling `gh`, `git
   push` or the network: toolchain parsed from `go.mod` and passed to the
   commands; an explicit `GOTOOLCHAIN` wins; the lint check fails on a
   built-with mismatch and prints the rebuild command; the `pr` profile runs
   lint then quick, and pre-pr selects it by default; timeout arithmetic for
   2, 4 and 8 cores; `git fetch origin main` runs before `git worktree add`;
   cleanup removes the bootstrap entries only; with the injected clock:
   progress extends the wait, a stall ends it naming the pending checks, a
   cancelled shard is rerun once and the wait then succeeds, the third
   cancellation fails, a cancelled run of an older commit is not rerun.
   Existing tests that pin the old behaviour (`TestRunVerifyDefaultRunsExpectedSteps`
   timeout argument, `TestAddWorktreeCleansUpWhenBootstrapFails` command
   count, `TestDODPRCIClassification`, `TestNewVerifyCmdDefaultsToDefaultProfile`,
   renamed `TestNewVerifyCmdLeavesTheProfileToTheVerifierByDefault`)
   are updated in the same commit.

7. **PASS — Verification plan.** By this agent: `go -C <worktree> test -race
   -count=1 ./internal/devtool/... ./cmd/devtool/...`, `go -C <worktree> build
   ./...`, `go -C <worktree> vet ./...`, `gofumpt` and `goimports -local
   github.com/tonimelisma/onedrive-go`. Not by this agent (rules): any `go run
   ./cmd/devtool verify ...`, any e2e or live test, lint through devtool. The
   reviewer runs `verify lint` and `verify quick`, then the PR's CI is the
   gate. No stress or E2E need: the change is in the tool, not the product.

8. **PASS — Data safety.** No sync, store or transfer code changes. The one
   deleting change narrows what devtool deletes: post-merge cleanup removes a
   `.worktreeinclude` `@` target only when it is a symlink and a copied entry
   only when it is a regular file, refuses a real directory (never
   `RemoveAll`), and refuses to run against the root checkout, so the shared
   `.testdata` bundle and the root `.env` cannot be removed through it.

9. **PASS — Secret and credential safety.** `.env` and `.testdata` are
   involved only as paths: the cleanup removes the worktree's copy and
   symlink and never reads either. Tests build their own temp trees with
   placeholder content. No account name, address, drive ID or token goes into
   code, tests, docs, the commit message or the reports. `.testdata` (a
   symlink, shown as untracked in the worktree) and `.env` are not committed.

10. **PASS — Live provider and destructive operations.** Nothing touches
    OneDrive, the test account, sync state or release artifacts; no git
    history is rewritten and no dependency changes. The new operation against
    GitHub is `gh run rerun <run-id> --failed` inside the DoD CI waits: at most
    `--ci-max-reruns` (default 2) per wait, only for a completed run of the
    head commit whose only non-success jobs are cancelled ones. It is not
    executed in this increment's tests (fakes only). Recovery: a rerun is
    harmless to cancel in the GitHub UI, and `--ci-max-reruns 0` turns the
    behaviour off.

11. **PASS — `go run ./cmd/devtool verify --dod --stage start` (2026-11-11): manifest ready, no actionable stale review threads; nightly run 42865435904 concluded failure; `TestE2E_SyncWatch_PauseResume` is held by branch `fix/upload-placeholder-retry` and skipped here.**

12. **ASSUMPTION — Unknowns and assumptions.**

    - Question: does this host's `gh` (2.46.0) expose a run's attempt number?
      Current assumption: no. `gh run view --json` lists no `attempt` field
      (checked locally through its field-list error, no network). The guard
      against issuing one rerun twice therefore remembers the run's
      `updatedAt` at rerun time and treats an unchanged value as "rerun not
      started yet".
      Validation step: a fake-runner test where the second poll still shows
      the cancelled run with the same `updatedAt` and exactly one rerun is
      issued; in live use, the first cancelled shard on a PR.
      Owner document/code path affected if assumption is wrong:
      `internal/devtool/verify_dod_ci_wait.go`.

    - Question: after `gh run rerun --failed`, does `statusCheckRollup` show
      only the new attempt of a check, or the old failed one beside it?
      Current assumption: it can show both, so checks are reduced to the
      latest `startedAt` per name and workflow before they are classified
      (the same reduction `gh pr checks` does internally).
      Validation step: unit test with a repeated check; in live use, the first
      rerun on a PR.
      Owner document/code path affected if assumption is wrong:
      `classifyPRChecks` in `internal/devtool/verify_dod_ci_wait.go`.

    - Question: is a check's `detailsUrl` always of the form
      `.../actions/runs/<run-id>/job/<job-id>`?
      Current assumption: yes for GitHub Actions checks. A URL that does not
      match is treated as a plain failure and nothing is rerun.
      Validation step: unit test for both forms.
      Owner document/code path affected if assumption is wrong: the run-id
      parser in `internal/devtool/verify_dod_ci_wait.go`.

    - Question: does a job killed by its `timeout-minutes` report `cancelled`
      or `timed_out`?
      Current assumption: either. `timed_out` is treated as a real failure. If
      GitHub reports `cancelled`, a deterministic hang is rerun twice and then
      fails with "cancelled 3 times": slower, but bounded and still a failure.
      Validation step: read the conclusion the next time a job times out.
      Owner document/code path affected if assumption is wrong:
      `internal/devtool/verify_dod_ci_wait.go`, `spec/design/system.md`.

    - Question: does `gh run rerun <id> --failed` rerun cancelled jobs, and
      only once the whole run has completed?
      Current assumption: yes to both (its help says "failed jobs, including
      dependencies"; the host's notes record `gh run rerun <id> --failed`
      recovering a cancelled macOS shard). The wait therefore holds the rerun
      until the run's status is `completed`.
      Validation step: the first cancelled shard on a PR after this merges.
      Owner document/code path affected if assumption is wrong:
      `internal/devtool/verify_dod_ci_wait.go`, repository `CLAUDE.md` step 10.

    - Question: what core count is the reference for the 20-minute deadline?
      Current assumption (corrected after the commit, which uses 3, not the
      brief's 4): 3. The code comment on the deadline says it was set because
      `internal/sync` under `-race` outgrew go test's default on the macOS
      CI runner, which has 3 cores; with a reference of 4 that runner's
      nightly deadline would become 40 minutes inside `verify-full`'s
      45-minute job timeout, so the job, not go test, would end a hang. With
      3 the results for 2, 4 and 8 cores are the brief's (40, 20, 20
      minutes) and both CI runners keep 20.
      Validation step: the reviewer's `verify default` timing on this host, or
      `--unit-timeout` if 40 minutes is still short; `runtime.NumCPU()` in
      one CI run would confirm the runner core counts.
      Owner document/code path affected if assumption is wrong:
      `unitTestReferenceCores` in `internal/devtool/verify.go`.

    - Question: the brief says "`--profile default` remains selectable", but
      the profile is a positional argument and there is no `--profile` flag.
      Current assumption: keep the positional form. `verify --dod --stage
      pre-pr` runs `pr`; `verify default --dod --stage pre-pr` runs the full
      tier.
      Validation step: command test for both invocations.
      Owner document/code path affected if assumption is wrong:
      `cmd/devtool/main.go`, repository `CLAUDE.md` step 9.

    - Question: what is the overall cap once the 30-minute limit goes?
      Current assumption: `--ci-timeout` stays as the hard cap; its default
      becomes one stall window per attempt, `(max reruns + 1) x (longest job
      timeout + 10 minutes)`, which is 5 hours with today's workflow.
      Validation step: unit test of the derivation and of the cap ending a
      wait that keeps changing.
      Owner document/code path affected if assumption is wrong:
      `internal/devtool/verify_dod_ci_wait.go`, `cmd/devtool/main.go`.
