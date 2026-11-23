# A2 report: devtool gates that work on a 2-core host; CI wait that survives a cancelled shard

Commit `2c5809c3` on `fix/devtool-host-gates` (base `70301c0e`), 22 files. The
first agent's uncommitted work was reviewed and kept, apart from the core reference.
I wrote the CI wait, cleanup wiring, pre-pr default, flags and all docs.

## What changed
- `internal/devtool/toolchain.go` (new): reads the `go.mod` toolchain pin; `GOTOOLCHAIN` added unless the caller set it.
- `verify.go`: `pr` profile; empty profile means `default`; pinned env for every command; deadline `20m x ceil(3/GOMAXPROCS)`, `--unit-timeout`.
- `verify_quick.go`: one plan runs lint then quick; usage and `--shard` text name `pr`.
- `verify_lint_version.go`: refuses a linter built with another Go minor; install command carries `GOTOOLCHAIN` and `GOBIN`.
- `worktree.go`, `fsutil.go`: `worktree add` fetches `origin main` first; `removeBootstrapEntries` is the exact inverse of bootstrap.
- `verify_dod.go`: pre-pr defaults to `pr`; cleanup uses `removeBootstrapEntries`; stages build a `ciWaitConfig`; wait code moved out.
- `verify_dod_ci_wait.go` (new): stall window from `ci.yml`, hard cap, newest-run reduction, rerun of cancelled jobs (head commit only, at most `--ci-max-reruns`).
- `cmd/devtool/main.go`: no default profile; `--unit-timeout`, `--ci-max-reruns`; `--ci-timeout` default 0 (derived).
- Docs: `spec/design/system.md`, R-6.10.2/16/21/22, `CLAUDE.md` (tiers, step 3, DoD 1-6 and 9-11), `CONTRIBUTING.md`, `ci.yml` header comment.

## Tests (first agent's unless marked; all seen failing for the right reason before the change where marked red)
- `TestPinnedGoToolchain_*` (5), `TestWithPinnedToolchain_*` (2): parse and fallbacks; an explicit `GOTOOLCHAIN` wins.
- `TestRunVerifyPassesThePinnedToolchainToCommands`, `TestRunVerifyKeepsAnExplicitGOTOOLCHAIN`: env of every started command.
- `TestAssertLintVersionMatchesCI_{VersionMismatchNamesThePinnedToolchain,BuiltWithAnotherGoPrintsTheRebuildCommand,BuiltWithThePinnedMinorPasses,OutputWithoutABuildToolchainIsNotChecked}`, `TestLintInstallDir`.
- `TestRunVerifyPRRunsLintThenQuick`; `TestDODPrePRRunsThePRProfileByDefault` (red), `TestDODPrePRKeepsTheFullTierSelectable`.
- `TestUnitTestTimeoutForCoresScalesBelowTheReference` (red after my reference change), `TestResolveUnitTestTimeoutPrefersTheExplicitValue`.
- `TestAddWorktreeFetchesOriginMainBeforeCreatingTheWorktree`, `TestAddWorktreeStopsWhenTheFetchFails`.
- `TestRemoveBootstrapEntries*` (5); `TestDODCleanupRemovesBootstrapEntriesBeforeTheWorktree`, `...LeavesTheWorktreeWhen...` (both red).
- `TestWaitForPRCI_*` (10), `TestWaitForPostMergePushCI_*` (4), `TestClassifyPRChecks_*` (4), `TestClassifyPostMergeRuns_*`, `TestCIRunIDFromDetailsURL`, `TestCIStallWindow_*` (2), `TestNewCIWaitConfig`, `TestSleepContext`: all red against stubs. These cover progress, stall, cap, context end, rerun once/twice/exhausted, older commit, real failure, run not finished, newest run, URL parse.
- Mine, written after the implementation to cover branches the inherited tests missed: `TestWaitForPRCI_CancelledRequiredCheckIsRerun` (concurrency-group cancel), `TestWaitForPRCI_FailureWithoutACancelledJobIsNotRerun`, `TestClassifyPRChecks_OnlyFailedOrCancelledChecksNameARunToExamine` (timed_out and skipped are not rerun).
- `cmd/devtool`: `TestNewVerifyCmdLeavesTheProfileToTheVerifierByDefault`, `TestNewVerifyCmdPassesFlagsThrough` (both red).

## Checks run
- `go test -race -count=1 ./internal/devtool/...` ok (93 s); `./cmd/devtool/...` ok (31 s).
- `go build ./...` ok; `go vet ./...` ok; gofumpt and goimports (`-local github.com/tonimelisma/onedrive-go`) applied.
- Lint and any `devtool verify`: not run (rules). Written to the `.golangci.yml` rules; I fixed one `govet shadow` case by hand.

## Deviations
- Core reference is 3, not the brief's 4. The code says the 20m deadline was set for the 3-core macOS runner. With 4, that runner's nightly deadline would become 40m inside `verify-full`'s 45m job timeout. 2/4/8 cores still give 40/20/20m; 1 core gives 60m, not 80m.
- `--ci-timeout` stays as a hard cap (default (reruns+1) x stall window, 5 h today). The analysis proposed no cap. This was a DoR assumption; now implemented.
- Reruns are counted per wait, not per run id. The stall window restarts when a rerun is requested.
- `CONTRIBUTING.md` and the `ci.yml` header comment were updated beyond the brief's doc list, because they would have drifted.

## Recommendations
- `bench.go` builds its subject with the host Go (`os.Environ()`): pin it the same way for comparable numbers.
- `nightly.go` and the CI wait each decode job JSON; one shared `githubJob` type would serve both.
- Log `runtime.NumCPU()` once in CI to confirm the 3- and 4-core runner assumptions.
- pre-pr and pre-merge could renew nightly claims (analysis section 5).
- `AddWorktree` still spells `"origin/main"` beside the `originMainRef` constant.

## Unfinished
- Nothing in scope. The reviewer runs `verify lint` (pinned toolchain) and `verify quick`; no package outside `internal/devtool` and `cmd/devtool` is affected.
