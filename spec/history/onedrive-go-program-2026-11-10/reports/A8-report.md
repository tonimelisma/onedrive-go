# A8 report: one coverage number; stress lane covers the sync tree

Worktree `~/src/onedrive-go-wt/one-coverage-number`, branch `ci/one-coverage-number`, one commit `d2ce268c` on `90c60a8f`, 18 files.

## What changed
- `internal/devtool/verify_coverage.go` (new): `requiredCoverage(repoRoot)` reads the one percentage on the R-6.10.4 line of `spec/requirements/non-functional.md` (error on missing file or line, zero or several percentages, out of range); `resolveCoverageThreshold` (0 = requirement, read only by profiles that gate, before any test; >0 overrides; <0 report-only); `runCoverageGate` and `parseCoverageTotal` moved here unchanged.
- `internal/devtool/verify.go`: `defaultCoverageThreshold` gone; threshold resolved through the requirement; unit tier runs with `-coverpkg=./...` (status line says so); `StressSlowTestSummary` gains `Package` (`json:"package,omitempty"`).
- `internal/devtool/verify_stress.go`: `stressSyncTree = "./internal/sync/..."` for both sync steps; a step with `-run` goes through `runGoTestMatchingTree`; `goTestJSONEvent` gains `Package`; slow tests keyed by package and test, tie-broken by package, printed as `<package> <test>`.
- `internal/devtool/verify_test_match.go`: `runGoTestMatchingTree`, `assertTestsMatchedInTree` and `lineShowsSelectedTest`; both guards share `runGoTestCheckingMatch`. The strict single-package guard is unchanged for the e2e callers.
- `internal/devtool/verify_summary.go`: the end summary's slow-test lines print the package too.
- `cmd/devtool/main.go`: second constant gone; `--coverage-threshold` defaults to 0, help text says 0 means R-6.10.4.
- `.github/workflows/ci.yml`: `COVERAGE_ARGS` is `--coverage-threshold -1` on macOS and empty on Linux (written with `!=`, as the analysis gives); header and step comments state where the number lives and that stress runs the sync tree.
- `spec/requirements/non-functional.md`: R-6.10.4 states `-coverpkg=./...`, 83.5%, and that this line is the only place the number is written.
- `spec/design/system.md`: verify profiles, the gate and its number, the stress tree and its guard (with the looseness risk), file and test ownership, CI tiers. `spec/reference/developer-onboarding.md`: file table. `CLAUDE.md` (verification tiers), `CONTRIBUTING.md`.
- Tests: new `verify_coverage_test.go` (with the three coverage tests moved from `verify_runner_test.go`); `verify_test_match_test.go`, `verify_runner_test.go`, `verify_test_helpers_test.go` (fake `Run` can write stdout), `verify_dod_test.go`, `cmd/devtool/main_test.go`.

## Tests added, and how each failed first (stubs let them compile: `requiredCoverage` returning 0, the tree guard delegating to the strict one, the `Package` field present)
- `TestRequiredCoverageReadsRequirement`: the real R-6.10.4 line parses to the number it states. Red: "0 is not positive".
- `TestRequiredCoverageRejectsUnusableRequirement` (no line, no %, two %, >100, missing file). Red: no error in each case.
- `TestRunVerifyZeroCoverageThresholdUsesRequirement`: 79.9% fails against a requirement of 80.0%. Red: passed against the 75.5 default.
- `TestAssertTestsMatchedInTree_AcceptsTreeWithSomeEmptyPackages`, `_AcceptsVerboseAndFailedPackages`, `_HandlesJSONOutput` (also a skipped test counts as matched). Red: the strict guard rejected partial trees (F6.2).
- `TestAssertTestsMatchedInTree_RejectsTreeWhereNothingRan`. Red: wrong message (strict guard), so the tree-specific text pins the new path.
- `TestRunVerifyStressFailsWhenConcurrencyPatternMatchesNothing`: marker-only output from the x50 step fails the profile before the x3 step. Red: no error (F6.1).
- `TestSummarizeStressSlowTestsKeysByPackage`: two packages' `TestShutdown` stay apart. Red: one merged entry (F6.3).
- Updated pins, red first: `-coverpkg=./...` in the default profile's arguments, `./internal/sync/...` in both stress steps, package in the slow-test output and JSON, `assert.Zero` for the flag default (red: 75.5). `TestRunVerifyCoverageThresholdFailure` now proves a positive value overrides without reading the requirement; the full-tier profile tests read a fixture requirement instead of passing a threshold.

## Checks
- `go build ./...`, `go vet ./...`: clean. gofumpt and goimports (`-local`) on all touched Go files: clean.
- `go test -race -count=1 -timeout=20m ./internal/devtool/... ./cmd/devtool/...`: ok (107 s, 33 s).
- `verify lint`: first run, one gocritic `stringConcatSimplify` (a two-element `strings.Join` in a test), fixed; second run: 0 issues. Race tests rerun on the final tree: ok.
- Old numbers: `grep -rn -E "75\.5|76\.0|76%|defaultCoverageThreshold"` over the tree finds nothing.

## Deviations
- The coverage number 83.5 is **provisional** (D2's estimate: 84.2 measured without `-race` at `70301c0e`, minus 0.7). It must be set from the reviewer's `workflow_dispatch` full run of this branch.
- A positive `--coverage-threshold` still overrides the requirement (the analysis keeps it for tests); only 0 means "the requirement".
- The tree guard counts a `-json` `skip` event as a selected test (the analysis lists pass and fail only), matching plain output, where a skip-only package prints `ok` without the marker.
- `./internal/multisync` stays a single-package target (the analysis offers `/...` as optional; out of the brief).

## Recommendations (not done)
1. `verify failed-test` reruns a `-run` pattern through plain `runner.Run`, so a rerun whose pattern matches nothing is green; route it through `runGoTestMatching`, which would make "every targeted step routes through here" true.
2. If the dispatch run shows `-coverpkg=./...` pushing the macOS full leg near its 20-minute deadline, make a negative threshold skip `-coverprofile` entirely (analysis 5.5).
3. A tree in which every package has no test files prints no marker and passes either guard; a "no package ran a test" rule would need fake runners to print realistic output.
4. Watch the 2-core local `verify default` time after `-coverpkg`; it instruments every package in every test binary.

## Unfinished
- The real coverage number (dispatch run). Nothing else.

## Packages the reviewer should test
- Only `internal/devtool` and `cmd/devtool` changed. The full `verify public` on CI exercises `-coverpkg`, and `verify stress` the tree target.
