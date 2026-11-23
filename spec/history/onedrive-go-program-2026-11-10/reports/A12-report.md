# A12 report: lint states what lint can; devtool imports no product package

Commit `a6c9e591` on `chore/lint-moves`, one commit on `origin/main` `6421bf98`, not pushed. Worktree `~/src/onedrive-go-wt/lint-moves`, made with `devtool worktree add`; the start stage was not run.

## What changed

- `.golangci.yml`: forbidigo rules for the `testing` failure methods (any receiver), package-level `slog.Debug/Info/Warn/Error`, `os.Stdout`/`os.Stderr`, `log.Print/Printf/Println`, with the brief's patterns, `pkg` and messages; one exclusion for the output rules (tests, `cmd/devtool/`, `internal/devtool/`, `e2e/`, `testutil/`, `internal/cli/root.go`, `format.go`, anchored); the filesystem packages' exclusion now names the raw `os.*` message only; depguard rule `devtool-no-product` (lax, `**/internal/devtool/**`, `**/cmd/devtool/**`, deny `internal/`, allow `internal/devtool$`).
- `internal/devtool/lint_config_contract_test.go`: a small reader for the YAML subset the config uses, a model of how golangci-lint applies an exclusion to a forbidigo finding, five pins (below); the folder-existence test also checks `**/<folder>/**` patterns.
- `verify_test_conventions.go` / `_test.go`: the selector half (Fatal/Error, Sleep, the allowed-file constant) and its four tests gone; the integration-name rule and its three tests stay.
- `verify_output_boundaries.go` / `_test.go`: the selector half gone; the flattened-error-attribute rule and the file allowance stay; the allowance test now writes a sample of every allowed kind and ten bound files and expects exactly the bound ones.
- `verify_readme.go` / `_test.go` deleted; `readme-status` out of the constants, the closed set (33 to 32 names), the switch, `repoRuleSteps()` and its comment; `writeRequirementsBackedReadme` (22 calls in 9 test files) existed only for that step and is gone.
- `bench_live_bidirectional.go` / `_test.go`: the cursor read whole (interface, store reader, test double, both fields, baseline read, the wait's two parameters, the three product imports, the drive-ID parse that fed only the state path); `TestWaitForBenchRemoteDeltaReadyFailsIfDryRunAdvancesCursor` deleted; the other wait test renamed `...ReturnsOnceThePlanHoldsTheRemoteChanges`.
- `bench_live.go` / `_test.go`: `isRemoteNotFoundCleanup` replaced by `cliReportsRemoteItemMissing` (the e2e rule: the CLI's `Error: <path> was not found on OneDrive.` line, optional list-the-parent hint); the visibility classifier no longer lowercases; fixtures use the CLI's real lines.
- Docs: `spec/design/system.md` (README row gone; evidence rows for the lint rules; Verification Policy bullets; which files lint reads; the `public` list), `spec/design/performance-benchmarking.md` (governs `bench*.go`, cited the deleted test; the cursor guard text).

## Tests (each seen failing first on the unchanged tree, `a12/A12-red.log`, `A12-red-sleep.log`)

- `TestGolangciConfig_ForbidigoRefusesDirectTestingFailures`: rule, `pkg`, `analyze-types`; matches `testing.TB.Errorf` etc.; no file exempt. Failed: no such rule.
- `TestGolangciConfig_ForbidigoRefusesWallClockSleepInTests`: Sleep rule binds test files, the filesystem packages' included. Failed on the old config for `internal/synctree/root_test.go`.
- `TestGolangciConfig_OutputRulesExemptOnlyTheCLIWritersAndNonProductCode`: three rules; on 17 sample paths lint exempts exactly what `outputBoundaryFileAllowed` allows; fs packages keep the raw `os.*` exemption. Failed: no such rules.
- `TestGolangciConfig_DevtoolImportsNoProductPackage`: the depguard rule verbatim. Failed: rule missing.
- `TestVerifyStepNamesSayWhetherTheyRunGoTest` (list without `readme-status`): failed, elements differ.
- `TestCLIReportsRemoteItemMissing_ReadsOnlyTheCLIErrorLine`: run on `isRemoteNotFoundCleanup`, four cases misread (a retried not-found debug line before another failure, another failure saying "not found", the sentence on a log line, an error going on after it).
- `TestRunBidirectionalSampleIsInvalidWhenTheMeasuredSyncMissesTheRemoteChanges`: passed on the unchanged tree; it pins the existing guard the cursor read's removal leans on (fixture check marks the sample invalid).

## Seeded lint (`--enable-only forbidigo,depguard`, `a12/A12-seeded-lint.log`, seeds removed)

Seeded `tb.Errorf` and `holder.t.Fatal` in a loglevel test file: both reported; the debug log shows forbidigo compared `testing.TB.Errorf` and `testing.T.Fatal`, package `testing`. `slog.Info`, `log.Printf`, `os.Stderr` in a loglevel product file: reported; `slog.Info` in the test file: silent. In localpath, `os.Stdout`: reported, `os.ReadFile`: silent. An `internal/driveid` import in devtool: reported with the rule's description; `cmd/devtool`'s import of `internal/devtool`: silent.

## Checks

- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- `go test ./internal/devtool/... ./cmd/devtool/...`: ok; with `-race` behind the lock: ok.
- Stress-declaration, discovery and step-set tests, `-v`: pass. `TestCLIStrings_EveryCountIsWordedByCountOf`: pass.
- `RealRepoIsClean`: 4 PASS lines, as before; none went (test-conventions keeps its own by decision (b)).
- `verify lint` (golangci-lint whole repo, then actionlint), behind the lock: `0 issues.`, status pass (6m24s); run again on the committed tree after a comment edit in `.golangci.yml` and a test-helper rename: `0 issues.`, pass.
- Whole-repo `--enable-only forbidigo` with the new rules before any edit (scratch config): `0 issues.`
- Every test name put into a spec file checked by hand (14).
- Not installed: no product code or main command changed.

## What no longer held in the brief, checked against `6421bf98`

- "BASE-LINE-TO-FILL" and "start stage has passed": false; worktree made here, start stage not run.
- The closed set had 33 names, not 32 (main added `e2e-full-discovery`); 32 afterwards, not 31.
- Decision (b)'s test name: it is `TestRunTestConventionsRequiresTheIntegrationPrefixInIntegrationFiles`.
- "Which RealRepoIsClean test went": none; 4 before and after.
- The analysis's four `f.t.Errorf` sites (`internal/cli/json_contract_fake_graph_test.go`) are gone from main: the whole-repo forbidigo run with the new rules reported 0 issues, so Part A converted nothing.
- Decision (e) first choice held: with the raw-`os.*`-only filter, the Sleep, JSON and `synctree.Root` rules report 0 sites in the three packages.
- The brief's doc list missed `performance-benchmarking.md`, which cited the deleted test.
- `TestCreateBenchBidirectionalRuntimeWritesScopedIncludedDirs` lost its state-path assertion with the field; nothing else a benchmark test proves changed.
- Line numbers have drifted (verify.go step names now near 1039-1065); no effect.
- Main's stress declarations, e2e-full discovery and count-wording check do not touch this change; their tests pass.

## DoR facts

- Part 1: forbidigo, analyze-types, matches a selector by `<package name>.<receiver type>.<method>`, pointer stripped (forbidigo v2.3.0 `forbidigo.go:312-390`), so struct fields and `testing.TB` are covered. Not covered: a method promoted through an embedding struct reads as the embedding type (0 such calls today).
- Part 2: files the Linux lint does not build: darwin-only `internal/graph/http_request_darwin_test.go`, `internal/synctree/{ctime,rename_noreplace,rootmark}_darwin.go`; darwin/BSD `internal/synctree/birthtime_bsd.go`, `internal/sync/{permissions_rdonly_bsd,inotify_other}.go`; neither Linux nor macOS `internal/localpath/opendir_other.go`, `internal/synctree/{rootmark,rename_noreplace,ctime,birthtime}_other.go`; race `internal/sync/race_enabled_test.go`. The old checks read all of them; read by hand, none holds a site a moved rule reports. `verify public` runs lint (`verify.go:498`) and `verify-full` runs it on `macos-latest` (`ci.yml:187-244`), so the darwin and BSD-or-darwin files are linted nightly; the five `_other` files and the race file nowhere.
- Part 4: no other non-test (or test) file under `internal/devtool` or `cmd/devtool` imports an `internal/` package directly.
- Part 5 (by reading): the benchmark passes only `--config`/`--drive`; `log_level` binds the log file alone and the console stays at NOTICE (`internal/cli/root.go:972-1008`), so a debug line such as the retry's (`internal/retry/transport.go:341`) never reaches stderr there; warnings can. An unrecognized failure prints `Error: ` plus its chain (`internal/cli/error_presentation.go:299`), which can say "not found" of something else (e.g. `internal/config/decoder.go:55`). Whether a warning's text can hold "not found": unknown. The CLI's own line: `error_presentation.go:185, 211, 302`.

## Seen and not fixed (5)

1. `internal/devtool/bench_live.go:18`, `live_env.go:7` import `testutil`, which imports `internal/localpath` and `internal/sharedref` (`testutil/testenv.go:14-15`, `testutil/driveroles.go:15-16`): devtool still links two product packages through it, and depguard reads direct imports only. Fix: give devtool what it needs from testutil without those imports.
2. `internal/devtool/verify_output_boundaries.go:116` `shouldSkipOutputBoundaryDir` does not skip `.worktrees`, unlike `shouldSkipTestConventionDir` (`verify_test_conventions.go:183`); the rule walks each keep their own skip list (`verify_requirements.go:316`, `verify_backlog_ids.go:156`, `verify_mutex_docs.go:83`). Fix: one skip helper for every repo-rule walk.
3. `internal/devtool/verify_governs.go:279, 450` use `shouldSkipTestConventionDir` under a test-convention name. Fix: the neutral helper of item 2.
4. `internal/devtool/bench_live.go:448, 500` and `bench_live_bidirectional.go` (the delta wait) sleep on the wall clock between attempts, so `TestResetBenchRemoteScopeRetriesUntilSuccessOrNotFound` waits 500 ms of real time. Fix: inject the backoff wait.
5. forbidigo's testing rule misses a failure method promoted through an embedding struct (pattern seen at `internal/synctest/helpers_test.go:11-14`, no call today). Fix: none in forbidigo's matching; a review rule or a devtool check if it ever matters.


## Merged as #946

- Review fix, its own commit before the merge: the lint-config pins also fail when forbidigo or depguard is dropped from `linters.enable`, when `run.tests` is false, when `linters.exclusions.paths` is set, when an exclusion that names no linter (it applies to every linter) admits a rule, when an exclusion of forbidigo or depguard filters by `source`, and when an exclusion for one package outside the sample paths admits the testing, Sleep, output or devtool-import rule. Seen failing on 13 temporary edits of `.golangci.yml` (the review's 7, flow-syntax forms of three, and depguard, Sleep and output exclusions for one package), each restored.
- Rebased onto main `89c388f6` (#945) with no conflict; `verify lint` over the rebased tree: 0 issues, so #945's code needed nothing.
- Start stage: the nightly on `6421bf98` passed, nothing claimed. Pre-pr, pre-merge and post-merge passed. Squash `4886b38c`, installed from main.
- Seen and not fixed, now planned: item 1 is W102; items 2 and 3 are W103; item 4 and the review's note 7 are W104; the review's note 2(b) is a rider on W94. Item 5 (a failure method promoted through an embedding struct) is not planned: no such call exists and no forbidigo pattern closes it without false positives.
