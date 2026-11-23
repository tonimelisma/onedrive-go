# A9 report: every e2e bucket runs; gate riders

Branch `ci/e2e-every-bucket`, one commit on base `3618a8aa` (hash in the final message).

## What changed
- `internal/devtool/verify_e2e.go`: `runE2EFull` runs every bucket, joins failures (`bucket <name>: ...`), records `not-run` after a full-preflight failure or cancellation; buckets and classified paths stream through a tee (no `CombinedOutput`); failed top-level tests returned per bucket; preflights wrap sentinel `errE2EPreflight` and carry explicit `-timeout`; one `rerunClassifiedQuirk` for all three quirks (rerun's own error, wrapping the first); auth rerun only when `TestE2E_AuthPreflight_Fast` itself failed; skipped auth preflight recorded `skipped` plus one greppable line naming no drive.
- `internal/devtool/verify.go`: per-bucket/preflight/rerun deadline constants (15m/40m/40m, 10m, 10m); `runLiveVerification` (fast-suite test failure continues to the buckets, preflight failure stops and marks buckets `not-run`); `public` runs every step (`runEveryVerifyStep`), coverage `not-run` after a unit-test failure; `--check-format` (`checkFormat`, `formatterCommands`), refused for profiles without a format step; `FailedTests` on step and bucket summaries; statuses `not-run`, `skipped`.
- `internal/devtool/verify_summary.go`: `stepOutcome`, `runStepWithOutcome` (replaces `runStepWithSlowTests`), `recordStepNotRun`, `recordBucketsNotRun`, `runBucket` takes failed tests; text summary prints `failed test:` lines.
- `internal/devtool/verify_test_match.go`: `runGoTestMatching` returns output too; `runGoTestTeed`; mutex-guarded `outputCapture`.
- `internal/devtool/verify_stress.go`: every stress step runs, errors joined.
- `internal/devtool/verify_failed.go`: rerun always has `-race`, plus `-shuffle=<seed>` from the failing package's `-test.shuffle` line (raw and `-json`).
- `internal/devtool/verify_quick.go`: format step honors `--check-format`.
- `cmd/devtool/main.go`: `--check-format` flag.
- `.github/workflows/ci.yml`: `--check-format` on `verify quick` and `verify public`; verify-full writes and uploads `--summary-json` (per OS); `stress` 90 to 120 min, `e2e` 90 to 180 min; credential steps print variable names and file counts instead of secret names, token file names and the drive; guidance pointer now "Test credential pipeline".
- `spec/requirements/non-functional.md`: new R-6.10.26 [verified]. `spec/design/system.md`: Implements line, two Verified-By rows, verifier bullets (failed-test, summary, public/stress, check-format, buckets, deadlines, classify, read-only skip, CI artifacts). `CLAUDE.md`: the "60-min bucket timeout" sentence.

## Tests added (all seen failing first)
- `TestRunVerifyE2EFullRunsEveryBucketAfterOneFails`: bucket 1 fails, all three run, summary fail/pass/pass with its failed test.
- `TestRunVerifyE2EFullReportsEveryFailingBucket`: buckets 1 and 3 fail, both in the error, `failed_tests` per bucket, text lines.
- `TestRunVerifyE2EFullRecordsBucketsNotRunAfterPreflightFailure`: auth, fast fixture, full fixture preflight failures each list three `not-run` buckets.
- `TestRunVerifyE2EFullDoesNotRerunAuthPreflightWithoutItsTestFailing`; `TestRunVerifyE2EFullReportsTheAuthPreflightRerunFailure`: cause check; failed rerun returns its own error.
- `TestRunVerifyRecordsASkippedAuthPreflightAsSkipped`: `skipped` status and notice.
- `TestCIJobTimeoutsCoverEveryGoTestDeadline`: reads `ci.yml`; worst-case deadline sums (e2e-full with all three reruns 165m, stress 100m, verify-full 20m, verify-unit 10m) plus 10m setup fit each job.
- `TestRunVerifyCheckFormatFailsOnUnformattedFiles`, `TestCheckFormatPassesOnAFormattedTree`, `TestRunVerifyRejectsCheckFormatWithoutAFormatStep`.
- `TestRunVerifyPublicRunsUnitTestsAfterLintFails`, `TestRunVerifyPublicSkipsOnlyTheCoverageGateAfterUnitTestsFail`, `TestRunVerifyStressRunsEveryStepAfterOneFails`.
- `TestRunVerifyFailedTestKeepsRaceAndTheShuffleSeed` (raw and json).
- Changed: `...DoesNotMaskUnknownFastSuiteFailure` (buckets now run), `TestRunVerifyWritesSummaryJSONOnFailure` (later bucket `pass`), `...DoesNotMaskUnknownWatchSharedFailure` (7 Run, 0 combined), the three `Classifies...` tests, preflight-arg tests, `TestRunVerifyStressFailsWhenConcurrencyPatternMatchesNothing`, the raw failed-test test, cmd `TestNewVerifyCmdPassesFlagsThrough`.

## Checks
- `go test -count=1 ./internal/devtool/... ./cmd/devtool/...` pass; same with `-race` pass; `go build ./...`, `go vet ./...` clean; gofumpt/goimports clean; lint log in `A9-lint.log`.

## Deviations
- Riders already done before A9: none of a, b, d, e, f, h, l, m, t. #872's stress tree and tree-aware guard were built on, not redone.
- The seed kept by `failed-test` is go test's shuffle seed; the model-seed replay line does not exist yet (A11).
- Deadlines are counted in full: preflights get an explicit 10m (go test's old default), one-test reruns 10m (the websocket rerun drops from 60m). So e2e needs 180 min, not the analysis's 120. Side effect: the DoD CI stall window (longest `timeout-minutes` + 10) grows from 100 to 190 min.
- Rerun error messages now carry the rerun's error (first attempt in parentheses); preflight errors start `e2e preflight failed:` and lost a doubled prefix.
- Classified reruns now go through the no-tests-matched guard. Fixed a latent race: the teed stdout and stderr shared one `bytes.Buffer` from two exec goroutines.

## Recommendations
1. Job-level `env:` still shows `ONEDRIVE_TEST_DRIVE` values in every step header of the live jobs; mask them (`::add-mask::`) or keep them out of job env.
2. Stream the stress x3 step too (rider s); it still uses `CombinedOutput`.
3. Name the hung test on a timeout panic (`running tests:` block) so `failed_tests` is not empty for deadline failures.
4. Raw parsing in `failed-test` needs lines without CI timestamp prefixes; use `strings.Index` as `nightly.go` does.
5. Consider deriving the DoD stall window from the jobs a PR runs, not the schedule-only maximum.

## Unfinished / for the reviewer
- Nothing unfinished. Only `internal/devtool` and `cmd/devtool` changed in Go; the `ci.yml` changes (quick `--check-format`, verify-full upload) are proven only by CI.
