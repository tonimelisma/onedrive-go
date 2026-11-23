# C4 report: an interrupted removal's condition clears when the leftover is gone

Commit `42700289` on `fix/interrupted-removal-clears` (one commit on `05336a1a`, amended, not pushed), 34 files, +699/-198.

## What changed
- `observation_findings.go`: manages `issueLocalInterruptedRemoval` (the only reported type left out); doc states the rule.
- `observation_reconcile_policy.go`: `buildObservationReconcilePlan` refuses a batch issue of an unmanaged type (error names type and path, nothing written); exact-path filtering (`managedObservationPathSet`) removed; dead empty-type checks removed.
- `store_types.go`: `ObservationFindingsBatch.ManagedPaths` removed (no production user since single-path observation went); doc rewritten.
- `store_observation_issues.go`: unused `now` removed from both reconcile functions and every caller (sync, cli tests); returns the plan error.
- `observer_local.go`: `TrySendScanFindings` (forwards the set, empty included); `skippedCh` comments. `observer_local_handlers.go` (`runSafetyScan`) and `engine_watch.go` (`runFallbackScan`, was log-only) use it.
- `scanner.go`: only a regular file under a set-aside name is reported; a folder/FIFO stays excluded, unreported; debug line names the path.
- Test seeding: `seedObservationIssueRowForTest` reports the stored issues of the seeded type again next to the new one; the cli seeder seeds one type per fresh store. The ManagedPaths test became `TestSyncStore_ReconcileObservationFindings_ClearsEveryUnreportedIssueOfAManagedType`; plan tests rewritten without paths.
- Requirements: the watch-fallback requirement, which shared R-2.8.12 with the timed pause, is now R-2.8.22 (with the fallback-findings sentence); R-2.8.18's reference and every sync-side `Validates:`/doc reference (inotify tests, watch-health tests, `engine_watch_rescan_replan_test.go`, sync-observation.md Implements line and two rows, sync-engine.md row) follow. R-2.8.12 stays on config/multisync/sync-control-plane (pause), untouched. A scan of all `spec/requirements/*.md` bullets and headings (575 IDs) found no other duplicate. R-6.4.9 wording.
- Docs: sync-observation.md, sync-store.md, sync-engine.md, sync-execution.md, data-model.md updated for all of the above; `dispatch_order_test.go` lists the new `TestRunOnce_`.

## Tests added
- `TestSyncStore_ReconcileObservationFindings_InterruptedRemovalLastsAsLongAsTheLeftover`, `TestRunOnce_InterruptedRemovalReportClearsOnceTheLeftoverIsGone`: kept while found, cleared (status too) once gone.
- `TestLocalObservationManagedIssueTypes_ManageEveryTypeALocalScanReports`: source guard both ways.
- `TestLocalObserver_RunSafetyScanForwardsAnEmptyFindingSet`, `TestWatchRuntime_FallbackScanRecordsAndClearsFindings`: safety and fallback scans forward the set, empty included.
- `TestBuildObservationReconcilePlan_RefusesAnIssueOfATypeTheBatchDoesNotManage`, `TestSyncStore_ReconcileObservationFindings_RefusesAnIssueOfATypeTheBatchDoesNotManage`: refusal names the type, table unchanged.
- `TestFullScan_OnlyARegularFileIsReportedAsAnInterruptedRemoval`; `TestSyncStore_ReconcileObservationFindings_ClearsEveryUnreportedIssueOfAManagedType`.
Behavioural tests failed first (`c4-red.log`, `c4-red2.log`).

## Checks
- `internal/sync`, `-race`, `-run` over every touched test, every seed-helper user, the watch-health/inotify tests, `ListsEvery|Table|Governs|Exhaustive|Registered`: ok. Reverse-dispatch subtest of the new `TestRunOnce_`: pass.
- `go test -race ./internal/cli/...`: ok.
- Requirement traceability: no standalone devtool command (it runs only inside `verify` profiles), so I ran its real-repo test `TestFindRequirementViolations_RealRepoIsClean` (and the backlog-ID one): pass. Spec governs has no standalone entry or real-repo test; a script over `spec/design/*.md` found every cited `TestXxx` declared (0 missing), and no file was added or removed.
- `go build ./...`, `go vet ./...`, `go vet -tags 'e2e e2e_full' ./e2e/...`: clean.
- `golangci-lint run ./internal/sync/... ./internal/cli/...`: `0 issues.` (`C4-lint.log`).

## Deviations
- `TestSyncStore_ReconcileObservationFindings_RefusesToWriteAnUnknownIssueType` manages the unknown type it reports, so it still tests the write's refusal.

## Also fixed
- `SetLocalObservationBatchChannel` doc; plan tests' missing `Validates:`; unused `now` values and duplicate reads in store tests; a hanging receive in `TestLocalObserver_RunSafetyScanEmitsFullLocalSnapshotBatch`; the hand-kept managed list in the batch test.

## Unfinished
- None. Reviewer: run the full `internal/sync` tier (every watch full scan now reconciles once).

## Not fixed
- `internal/devtool/verify_requirements.go:167-216`: `readDeclaredRequirements` accepts an ID declared twice, which is how R-2.8.12 stayed doubled. Fix: report a second declaration of an ID as a traceability violation, with a test.
