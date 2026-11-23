# A13 report: e2e buckets derived from test names

Commit `9730a092` on base `9e5498a5` (origin/main, #940), branch `test/e2e-family-buckets`,
worktree `~/src/onedrive-go-wt/e2e-family-buckets`. Not pushed. 26 files.

## What changed
- `internal/devtool/verify_e2e_tiers.go` (new): tiers by `go/build` `MatchFile` over every package `./e2e/...` names; `e2eTestFamily`; the one table `fullE2EFamilyBuckets`; `fullE2EBucketShapes`; `placeFullE2ETests`; `discoverFullE2EBuckets`.
- `internal/devtool/verify_e2e.go`: three hand lists and bucket constructors gone; `e2e-full-discovery` step (`runFullE2EDiscovery`); `runE2EFull` takes the discovered buckets.
- `internal/devtool/verify.go`: discovery before the fast lane under e2e-full; both not-run paths use its buckets; the step is in the closed set and runs no go test.
- devtool tests: `verify_e2e_tiers_test.go` (new tests, fake e2e tree, fast allow-list); `verify_runner_test.go` (two tests and `discoverTaggedE2ETests`/`hasBuildExpression` deleted, fake tree), `verify_e2e_full_test.go`, `verify_ci_deadline_test.go`, `verify_step_marker_test.go`, `nightly_test.go` (fake tree, new names).
- e2e, nine files: the 20 function names and their doc-comment mentions only (33 lines).
- `internal/config/drive.go`, `internal/cli/status_test.go`: the one comment each, new name, present tense.
- `spec/requirements/non-functional.md`: R-6.10.28 `[verified]`. `spec/design/system.md`: Implements line, evidence row, bucket bullet states the rule, devtool split names the new file, fast-lane bullet. `CLAUDE.md`: two sentences in the e2e paragraph.
- `spec/reference/live-incidents.md`: 20 "Historical test names" rows; marker on the five Suite / test lines that lacked it (207, 673, 951, 1315, 1816 on the base); one Current coverage cell. `spec/design/cli.md` (2), `spec/reference/graph-api-quirks.md` (4): new names.

## Tests added (Validates R-6.10.28; two also R-6.10.26)
- `TestE2ETestFamilyIsTheNameUpToTheNextUnderscore`: `Sync`, `Status`; `TestE2E_RoundTrip`, `TestE2E__Spins` and a non-`TestE2E_` name refused.
- `TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce` (body replaced): every real full-tier test in exactly one bucket or the preflight.
- `TestFullE2EBucketsRefuseATestNoFamilyPlaces`: unknown family, no family, not live: each named with its file, no buckets.
- `TestFullE2EFamilyTableNamesOnlyFamiliesTheSuiteUses`: every table family has a real test; every bucket non-empty.
- `TestFullE2EFamilyTableNamesEveryBucketAndNoOther`: table buckets equal the shapes; FixturePreflight in none.
- `TestDiscoverE2ETestsAsksTheBuildForEachTagSet`: `e2e_full && e2e`, `(e2e && e2e_full)`, host GOOS found; fast and untagged apart.
- `TestDiscoverE2ETestsReadsEveryPackageTheBucketsRun`: a nested package found; testdata, `.`, `_` skipped.
- `TestDiscoverE2ETestsFailsOnATestFileNoTagSetBuildsHere`: other-GOOS build line and file name fail by name, every tier.
- `TestFastE2ELiveTestsAreTheAllowList`, `TestFastE2ELiveTestsOutsideTheAllowListAreNamed`: real fast set equals the list; an extra and a missing name are named, helpers and TestMain are not.
- `TestRunVerifyE2EFullStopsBeforeItsFirstLiveCallOnATestNoBucketTakes`: no go test starts; step failed, marker printed, three buckets not-run.
- `TestRunVerifyE2EFullRunsTheTestsDiscoveryPlaced`: bucket `-run` patterns are the sorted discovered names; a fast test is not in them.
Seen failing: all new tests first failed to compile (no `e2eTest`); the two real-repo tests then failed on 11 unplaced tests until the renames; the stop test failed on a mutation that discovered after the fast lane; the package-walk test failed on a mutation that read only `e2e/`.

## Checks
- `go build ./...`, `go vet ./...`, `go vet -tags=e2e,e2e_full,integration,stress ./...`, `go vet -tags=e2e ./e2e/`: pass.
- `go test -count=1 -timeout 30m ./internal/devtool/` (lock) pass; same with `-race` (lock) pass, both on the final tree.
- `go test -count=1 -timeout 30m ./e2e/ ./testutil/...` untagged (lock): pass.
- `go test -v -run RealRepoIsClean ./internal/devtool/`: 4 PASS. Every `repoRuleSteps()` step (spec governs, layering, traceability included) passed on the worktree through a scratch test, deleted before the commit.
- gofumpt: nothing. Lint (lock), both runs: `0 issues.` (`A13-lint.log`).
- Cited tests: every `Test...` on an added spec line exists, apart from the old names in the ledger.
- Not run: any e2e/e2e_full/integration test (needs an account; not mine), `devtool verify`, `devtool nightly`.

## Deviations
- Discovery reads every package `./e2e/...` names, not only `e2e/` itself: the buckets run `./e2e/...`, so a test in a sub-package would otherwise run in no bucket.
- Discovery is a step of its own, `e2e-full-discovery` (closed step set, runs no go test): its failure prints a marker, so the nightly keys it `step-<job>-e2e-full-discovery`; the not-run bucket records then carry the pattern `^()$`.
- The fast allow-list `fastE2ELiveTestNames` lives in the test file, since only the test reads it. TestMain is not a discovered test.
- The brief's helper-test counts (21 fast, 63 untagged) count `func Test` lines inside raw-string fixtures; the AST counts 20 and 54. The full (136) and fast `TestE2E_` (8) sets match.

## Nightly claim
A failure of a renamed test is keyed by its new name, so a claim on an old key (such as `e2e.TestE2E_Sync_IncrementalDeltaToken`) never matches a failure again: it goes when its holder releases it, when its 24-hour lease runs out unrenewed, or, once marked fixed, when a nightly containing the fix passes its jobs.

## Also fixed
- `spec/design/system.md`: the fast-lane bullet lacked `TestE2E_TrackedFilesNameNoTestAccount`; "no longer runs" and "hid" (history) in the two bucket bullets now present tense. `CLAUDE.md`: "now lives there" now "lives there". The two Go comments now say what holds the rule, not when it was caught.

## Seen and not fixed (6)
- `e2e/fileops_full_e2e_test.go:17`: the doc comment of `TestE2E_FileOps_RoundTrip` tells history ("moved to the full battery") and does not name the test. Fix: say what it covers. Left: renames change names only.
- `e2e/fileops_full_e2e_test.go:148,181,217`: `TestE2E_FileOps_ErrorCases`, `_JSONOutput`, `_QuietFlag` have no doc comment or `Validates:` line. Fix: add both. Left for the same reason.
- `internal/devtool/verify_e2e.go` `runE2EFull`: ten parameters. Fix: one struct for the live lane's inputs, shared with `runE2E`.
- `internal/devtool/verify.go:214-228`: `fullE2EBucketSpec.Kind` repeats `Name` (one kind per bucket). Fix: drop `Kind`, key the classified rerun on the name. Out of scope (classified rerun).
- `spec/reference/live-incidents.md:20-22`: the table's intro says it was checked 2026-10-25; it now holds rows of 2026-11. Fix: drop the date or put it per row. Left: nothing else in that file changes.
- `CLAUDE.md:226`, `spec/design/system.md:430`: the fast lane's tests are spelled out in prose beside `fastE2ELiveTestNames`. Fix: name the list and drop the copies.

## For the reviewer
- Packages to test beyond devtool: none changed behaviour; `internal/config` and `internal/cli` changed a comment each.
- Live: the renamed tests that run on this host, one at a time (`-run '^TestE2E_<new name>$'` with `-tags 'e2e e2e_full'`).


## Shipped

Merged as #943 (`238bea94`, squash, 2026-11-17 08:05 PST), from `60c874d2` rebased onto `17564167` (#942). The two merges since the base added no e2e test; discovery and the real-repository tests passed unchanged after the rebase.

Changed after the review, test first:
- Should-fix 1: the full tier's `MatchFile` context carries `race` in `ToolTags` (`goRaceToolTag`, `e2eTier.toolTags`), as the buckets build under `-race`. New `TestDiscoverE2ETestsSelectsFullTierFilesAsTheRaceBuildDoes` (an `&& race` file found; an `&& !race` file fails discovery by name); seen failing before the fix. `system.md` sentence says "build under `-race`".
- Should-fix 2: `TestRunVerifyE2EFullRecordsBucketsNotRunAfterPreflightFailure` asserts each not-run record's `RunPattern` is discovery's; seen failing on the empty-shapes mutation.
- Should-fix 3: `TestE2E_FileOps_RoundTrip` has a present-tense doc comment naming it and what it covers.
- Note (Fuzz/Example): discovery collects fuzz targets and examples too (`isGoTestRunName`), so placement refuses one in a full-tier file by name. New `TestDiscoverFullE2EBucketsRefusesAFuzzTargetOrAnExample`; seen failing first. Spec sentence and both citation lists updated.

Left, and why: comparing the manifest test's file set with `go list` (optional; spawns go in a unit test); `runE2EFull` with no buckets (its one caller always passes three); fast-lane steps absent from the summary on a discovery failure (same as a preflight failure; a design choice); historical `-run` lines in the incidents ledger (the file is frozen apart from the name rows); the `.testdata` symlink (outside the commit). The author's seen-and-not-fixed items 2 to 6 stay as the review advised.

Gates: start stage twice (after the rebase and after the last amend): no claim, last nightly green. Pre-pr passed (lint 0 issues). Stress declarations: the change touches no `internal/sync` or `internal/multisync` test; `TestStressRepeatedStepsDeclareEveryTestTheirRuleNames` passed in the whole devtool package run. Full tier dispatched on the branch: every job green; e2e ran `e2e-full-discovery` (pass, 7 ms) and the same per-bucket counts as the last green nightly on main: misc 57 (55 pass, 2 skip), serial-sync 33, watch-shared 45 (37 pass, 8 skip); fast 79 pass, 3 skip. Pre-merge passed (no threads). Post-merge passed (it reran one macOS unit shard the runner canceled); the worktree and branch are removed.

No install: the only non-test, non-tooling, non-doc file is a two-line comment in `internal/config/drive.go`.
