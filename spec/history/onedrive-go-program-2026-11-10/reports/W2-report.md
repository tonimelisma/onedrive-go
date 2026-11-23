# W2 report: commit 1e568496 on ci/verify-profile-leftovers (rebased onto origin/main 27e76cc3; the writer's ebc24072 plus the review section's changes)

## Changed
- `internal/devtool/verify_test_match.go`: one empty-run guard (`assertTestsMatched` through `runGoTestMatching`) for a package or a tree. It passes a step only when a line shows a test that ran; a `FAIL ... [build failed]`/`[setup failed]` line ran nothing; when the run failed too, both errors are joined. `runGoTestMatchingTree` is gone; `runGoTestTeed` is now `runGoTeed`.
- Every go test step of every profile goes through the guard. `verify_stress.go`: all three stress steps (x3 off `CombinedOutput`; slow tests come from the captured output). `verify_e2e.go`: the fast suite and the full buckets; dead `writeCommandOutput` removed. `verify_failed.go`: one guard call. `verify.go`: `runUnitTests` and the integration tier. `verify_quick.go`: `runShortUnitTests`. The only exception is the compile-only `go test -c`, which runs no test.
- `verify.go`: the integration tier runs `-run=^TestIntegration_`. `integrationTimeout`, `integrationBuildTag` and `integrationTestPrefix` are constants, and the tagged vet uses the tag constant.
- `verify_test_conventions.go`: parses comments now, and fails a `Test...` func other than `TestMain` in a file whose `//go:build` names `integration` unless it is named `TestIntegration_...`.
- `verify_workflow_lint.go` (new): the `workflow lint` step. A finding must start with one of the given workflow files followed by ":". `verify.go`/`verify_quick.go`: `lintSteps()` (lint, then workflow lint) in default/public/pr/lint.
- `verify_lint_version.go`: `ciWorkflowPath = workflowDir + "/ci.yml"`.
- `bench.go`: the subject is built with `withPinnedToolchain(os.Environ(), pinnedGoToolchain(repoRoot))`.
- `verify_requirements.go`: a `requirementDeclarations` struct with `definedAt`; a duplicate ID is a violation naming every line that declares it.
- `go_test_log.go`: `rawGoTestPassPrefix`. Tests: `verify_test_helpers_test.go` (`passingGoTestOutput` is the fake's default for go commands) plus fixture updates in the runner, e2e-full, every-step, ci-deadline and dod-ci-wait tests.
- Docs: `spec/design/system.md` (Verified-by rows, the lint and integration profile entries, the shard sentence, the stress lane, ownership, the rollup sentence, the benchmark, the guard paragraph, the workflow-lint bullet, the test-convention rule, traceability, the macOS numbers, R-6.10.2 in Implements); `spec/requirements/non-functional.md` R-6.10.2, .12, .17, .26; the lint-profile line in `CLAUDE.md` and `CONTRIBUTING.md`. CLAUDE.md never described what the integration tier runs, so it needed no change.

## Tests added (each seen red)
- `TestRunVerifyStressStreamsTheRepeatStep`: red on old code, "the x3 step did not stream".
- `TestAssertTestsMatched_RejectsATreeWithNoTestFiles`: red, nil for plain, json and empty output.
- `TestPrepareBenchSubject_BuildsWithThePinnedToolchain`: red, GOTOOLCHAIN "" instead of go1.26.7.
- `TestRequirementTraceability_RefusesAnIDDefinedTwice`: red, nil error.
- `TestVerifyLint_RefusesAnInvalidWorkflowFile`: red, nil error. It runs the real pinned actionlint, which finds `broken.yml:8:`.
- `TestRunVerifyLintRunsThePinnedActionlintOverEveryWorkflow`: red, golangci-lint only.
- `TestWaitForPRCI_CheckQueuedInItsGroupWaitsOneRunOfTheGroup/PENDING` (and `/QUEUED`): red by mutating the rule (pending counted as started), reverted.
- `TestRunVerifyE2EFullFailsASuiteThatRanNoTests`: red with the two calls put back unguarded for one run.
- `TestRunVerifyStressFailsAStepWhosePackagesHaveNoTests`, `TestAssertTestsMatched_ABinaryThatNeverRanRanNoTest`, `TestRunWorkflowLint_NamesEveryFinding`: red by mutation, each reverted.
- Second pass, each red on the first-pass code:
  - `TestRunTestConventionsRequiresTheIntegrationPrefixInIntegrationFiles`: nil error.
  - `TestRunVerifyIntegrationRunsOnlyTheIntegrationTests`: no `-run`, and nil on an empty run.
  - `TestRunVerifyPublicFailsAUnitTierThatRanNoTest` and `TestRunVerifyQuickFailsAShardThatRanNoTest`: nil errors.
  - `TestRunWorkflowLint_ACompileErrorIsNotAFinding`: the compile line was reported as a finding.
- `TestFindTestConventionViolations_RealRepoIsClean` (the real tree passes the new rule) and `TestRunWorkflowLint_TreeWithoutWorkflowsRunsNothing`: not seen red.

## Checks
- `go test -count=1 ./internal/devtool/... ./cmd/devtool/...`: pass. With `-race`: the whole package after the first pass, then `-race -run` over the second-pass and nearby tests. `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...` and `go vet -tags integration ./internal/graph/...`: pass.
- golangci-lint over `./internal/devtool/... ./cmd/devtool/...`: 0 issues (`W2-lint.log`). Each lint started only after `pgrep` showed nothing running.
- Every backticked `TestXxx` in system.md, CLAUDE.md, CONTRIBUTING.md and non-functional.md exists, CLAUDE.md's `TestXxx` placeholder apart. The requirements declare 579 IDs with 0 duplicates. actionlint finds nothing in the real workflows.
- The unit tiers' output is unchanged for its readers: the tee writes the same stdout and stderr, the coverage gate reads the profile file, and the summary records only the step's error. Plain output (`ok  \t...`, with or without `coverage:`) and `[no tests to run]` lines are read as before.

## Workflow lint: actionlint, as preferred
- `go.mod`: go 1.26.0, toolchain go1.26.11. `go.yaml.in/yaml/v3` is available (indirect) but was not needed. A `go get -tool` pin was tried and reverted: it adds ten modules to the product's go.mod.
- Chosen: `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -shellcheck= -pyflakes= -oneline <workflow files>`, under a 5-minute deadline. `go run` checks the module against the checksum database and needs no install in CI or on this host. The pin is `actionlintModule`.

## Measurement (run 43130170152, dispatch, head 3003e3ee)
- macOS job 22 min 25 s of 45; unit step 16 min 46 s on 3 cores; `internal/sync` 852.5 s of the 20-minute per-binary deadline. Ubuntu: 11 min 17 s, 9 min 55 s, 506.4 s on 4 cores.
- No package is within five minutes of its deadline, so nothing changed. The numbers are in system.md.

## Deviations
- The tree guard and the single-package guard are one. The entry's `TestAssertTestsMatchedInTree_RejectsATreeWithNoTestFiles` is named `TestAssertTestsMatched_...`, and the four `...InTree_...` tests are renamed the same way.
- A build or setup failure counts as "ran no test". The guard's error is joined with the run error, not substituted for it.
- The workflow lint also runs in default and public, which share the lint step list.
- `TestVerifyLint_RefusesAnInvalidWorkflowFile` needs the Go module proxy on its first run (about 25 s cold).
- The integration tier no longer runs graph's untagged unit tests; the unit tiers already run them.

## Also fixed
- `verify_stress.go`: a failed summary write no longer drops the step's error.
- `verify_e2e.go`: dead `writeCommandOutput` removed.
- Literals named as constants: `integrationTimeout`, `integrationBuildTag`, `rawGoTestPassPrefix`; `ciWorkflowPath` built from `workflowDir`.
- `TestAssertTestsMatched_RejectsEmptyRun`'s fixture printed an `ok` line without `[no tests to run]`, which go test never prints.
- `system.md` described R-6.10.2 without listing it in Implements.

## Not fixed
- None. Every go test that devtool starts and that runs tests is guarded. The one that is not, the compile-only `go test -c -race -tags='e2e e2e_full' ./e2e`, runs no test by design.

## Unfinished
- The local `verify default` timing on this host was not measured (left to the coordinator), so CLAUDE.md's verification tiers still carry no number.

## For the reviewer
- Only devtool changed. Your verify run exercises the workflow lint (it needs the module proxy), the guarded unit, integration, live and stress steps, and the test-convention rule on the real tree.

## Review (coordinator, 2026-11-11 and 2026-11-12)
- Second pass ordered and accepted (commit fee9bed5): the actionlint finding pattern was loose (a failed build of actionlint read as a finding), and a negated build tag counted as naming the tag.
- Local measurement, 2026-11-11, host shared with two writers, an install and a lint. `verify default`: 52m48s, failed at the 40-minute deadline in `internal/sync`'s race tier. The race tier alone with a 150-minute deadline: 69m37s wall, 103% CPU, 4333 CPU-seconds, max RSS 785 MB; `internal/sync` 3790.9 s (63 min); every other package ok; one test failed, `TestRunWatch_FinishesWorkTheBootstrapMovesLeaveForTheNextPass` (a fixed 10 s wait, `engine_one_shot_quiescence_test.go:112`), handed to W12.
- Decision (mine): below the 3-core reference the deadline is a core budget, 180 core-minutes divided by the cores (90 min on 2, 180 on 1); 3 cores and more keep 20 minutes. Implemented in review (`unitTestSmallHostCoreBudget`, `TestUnitTestTimeoutForCoresGivesSmallHostsACoreBudget`), with `system.md`, the repository `CLAUDE.md`, `CONTRIBUTING.md` and the two `ci.yml` comments.
- Also fixed in review: three `os.IsNotExist` checks in devtool product code (`verify.go`, `worktree.go`, `fsutil.go`) and six in `verify_runner_test.go` now use `errors.Is` / `assert.ErrorIs`.
- Open: one local `verify default` end to end after W12 merges and the gate is idle, as the proof that the full tier passes on this host.
