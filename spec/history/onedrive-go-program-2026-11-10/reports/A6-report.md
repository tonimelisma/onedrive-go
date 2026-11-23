# A6 report: one-shot syncs in the e2e suite converge or say why not

Commit `833a9bb6` on `test/e2e-converge` (base `main` `31fd2a59`), 23 files.

## What changed
- `e2e/e2e_test.go`: `syncRunner` (default drive, one drive, all drives); both helpers take it, with `ForDrive`/`AllDrives` variants; `runSyncThroughTransientFailures*` now returns (last stdout, all stderr); new `syncPassFinishedWork` predicate; `syncConvergePollInterval` constant.
- `e2e/shared_items_helpers_test.go`: the shortcut projection loop moved here (fast tier) and rebuilt on `requireSyncEventuallyConvergesForDrive`; returns record and converged pass; dead `requireActiveShortcutChildAtPath` removed.
- `sync_full_test.go` 19 moved (3 on a predicate: 294, 906, 975), 8 commented.
- `sync_demoted_full_e2e_test.go` 4 moved, 3 commented. `sync_edge_cases_full_test.go` 5 moved, 2 commented.
- `sync_recovery_e2e_test.go` 6 moved (138 on a predicate for the two remote deletes), 4 commented.
- `sync_kill_mid_transfer_full_e2e_test.go` 3 commented. `sync_safety_full_e2e_test.go` 2 moved, 1 commented, `require.Eventually` loop converted.
- `sync_hardening_full_e2e_test.go` 6 moved (328 on a predicate), 8 commented.
- `sync_shortcut_full_e2e_test.go` 9 moved (8 via the projection loop, 187 via RSTF ForDrive), 2 commented; old loop removed.
- `sync_shortcut_e2e_test.go` 1 moved (fast-tier smoke uses the projection loop). `sync_shared_e2e_test.go` both `require.Eventually` loops converted, 1 commented.
- `cli_commands_e2e_test.go` 8 moved. `drive_list_full_test.go` 1 moved. `output_validation_e2e_test.go` 1 moved, 2 commented.
- `orchestrator_e2e_test.go` 3 moved (66 RSTF AllDrives, 120 RSEC AllDrives, 230 RSTF ForDrive); the drive-2 `stat` after 66 is now polled.
- `shared_items_e2e_test.go` 1 moved (195 RSTF ForDrive), 202 and the unclear 212 commented. `sync_watch_full_test.go`, `sync_e2e_test.go` 1 commented each. `edge_cases_e2e_test.go` takes the new return.
- Docs: `spec/design/system.md` (Verification Policy bullet on the helpers and the single-pass rule), `spec/reference/live-incidents.md` (LI-20260422-05 follow-up), `spec/reference/live-recurring-families.md` (policy line).

Totals: 66 sites moved, 37 single-pass sites commented (`Single pass by design:`), plus the unclear site, 4 hand-written loops converted.

## Unclear site
`shared_items_e2e_test.go` closing `--download-only` pass: SINGLE-PASS. The test cites R-2.14.2 ("... while still allowing downloads"); the pass runs after status shows the write block and proves one download-only pass still succeeds under it. A loop would retry a failing pass and hide exactly that.

## Tests added
None: a live convergence has no failing unit test to write first. The proof is the live runs below.

## Checks
- `go vet -tags 'e2e e2e_full' ./e2e` and `go vet -tags e2e ./e2e`: pass.
- `go test -short ./e2e/...`: pass.
- golangci-lint `./e2e/...` (one run at a time): `0 issues.` (`A6-lint.log`); the first run found one govet shadow, fixed.
- Live, one at a time, each passed: `TestE2E_Sync_BidirectionalMerge`, `TestE2E_Sync_LargeFileBetweenClients`, `TestE2E_Status_PerDrive_NoIssuesOrRetries`, `TestE2E_SyncFull_RemoteDeleteGoesToLocalTrashAndRestores`, `TestE2E_Sync_NestedFolderHierarchy`, `TestE2E_Sync_CrashRecoveryIdempotent`. Variants covered: RSTF default, RSEC default (with and without `syncPassFinishedWork`). Not runnable here (one drive, no shared or shortcut fixture): every `ForDrive` and `AllDrives` site; CI runs them.

## Deviations
- `runSyncThroughTransientFailures` returns `(stdout, stderr)` instead of keeping its single `string`, so banner sites (sync_full 591, demoted 121/135, shortcut 187, shared_items 195) use it rather than RSEC with an empty predicate.
- `syncRunner` is a small struct (config, env, resolved drive) with a `run` method, not the analysis's func type, so the timing record keeps the drive without a second parameter.
- New predicate sites require `syncPassFinishedWork` (exit 0 and no left work), because each reads, unpolled, what the same pass uploaded.
- Converted `require.Eventually` loops and the projection loop now tolerate a non-zero pass and retry it (the brief's intent); their timeouts grew from 30 s to 2 min (shared, trash) to fit 5 s pass spacing.
- Orchestrator drive-2 read polled (not a sync site, the analysis's named flake).

## Recommendations
1. `sync_hardening_full_e2e_test.go` NamingCorpus: the best-effort `--download-only` pass is followed by unpolled content checks (analysis 01c (a)); give it a predicate converge.
2. `findActiveShortcutChild` asserts the state inside a poll predicate, so a transient non-active state fails the test; return it and let the caller assert after convergence.
3. `TestE2E_Sync_DryRunNonDestructive` and the create-create conflict tests run their single-pass dry run or conflict pass straight after a remote put without waiting for delta; add a delta wait before the single pass.
4. Once B2 changes the exit code of a pass that left work, `runSyncThroughTransientFailuresWith` must accept that exit code instead of failing on it.

## Unfinished
Nothing in scope. Reviewer: no other package affected (test-only, `e2e` package).
