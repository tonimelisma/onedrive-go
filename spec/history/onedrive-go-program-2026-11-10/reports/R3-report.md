# R3 report

Branch `refactor/observation-types`, one commit `da043274`, pushed, no PR, nothing installed.
Base: R2 `refactor/names-one-rule` at `995c1e19`, not origin/main. R3 needs R2's code because the twin fix edits the same scanner name lookup (`storedPathOf`, `namekey.Stored`).
Diff: 43 files, +1119 / -419. No store generation change and no schema change.

## Rows done: G4 and G6. Rows not done (overrun): G5 and G7

### G4: a Unicode-form twin of a synced file is a collision; a watch delete removes the subtree
- `scanner.go`: `admitSyncedFileEntry` runs before classification. When a second regular file has the stored path of a synced file, the scan records `issueCaseCollision` once for that path, with the existing same-form detail, now the constant `sameUnicodeFormCollisionDetail`. The path's row keeps describing the synced inode, including that inode's own change. When neither file is the synced inode, the row keeps the baseline's facts. The twin's queued hash job is dropped (`withoutDroppedHashJobs`).
- `scanner_unavailable.go`: the per-scan state gains `syncedFileEntries` and `droppedHashJobs`.
- `local_observation_batch.go`: every delete becomes a subtree delete, whatever the observer guessed the path was. The guess stays in `HandleDelete` and still drives the watch cleanup.
- `store_local_state.go`: the subtree delete is now a primary-key range, `path = ? OR (path >= p/ AND path < p0)`.
- Measured on 100,100 rows: a folder takes 6.0 ms (substr form: 44.2 ms). 100 single-file deletes take 11.6 ms (substr form: 4.43 s). The query plan is `MULTI-INDEX OR` with two `COVERING INDEX` searches. The three existing semantics tests (SQL wildcards, Unicode lengths, siblings) pass on the range form.
- Seen failing on the R2 base, for the reasons described:
  - The twin scan tests: the row became the twin's and no finding was recorded.
  - `TestRunOnce_UnicodeFormTwinOfSyncedFileUploadsNothing`: over 10 runs, the twin passes planned uploads of the twin, and no issue was recorded.
  - `TestHandleDelete_UnwatchedNewDirectoryRemovesSubtreeRows`: the child rows stayed.
- Tests added:
  - `TestFullScan_UnicodeFormTwinOfSyncedFileIsACollision`, a table over other content, equal content and both files changed
  - `TestFullScan_RemovedUnicodeFormTwinClearsTheCollision`
  - `TestRunOnce_UnicodeFormTwinOfSyncedFileUploadsNothing`, added to `runOnceScenarios`
  - `TestHandleDelete_UnwatchedNewDirectoryRemovesSubtreeRows`
  - `TestHandleDelete_FileDeleteKeepsSiblingsThatShareItsNamePrefix`
  - `TestLocalObservationBatchForEvent_FileDeleteRemovesSubtree`
  - `TestDeleteLocalStatePrefix_SeeksThePrimaryKey`
  - `BenchmarkDeleteLocalStateSubtree`
- Test helper changed: `queryPlanText` now takes bind arguments.
- Spec changed: R-2.8.8 is reworded. `sync-observation.md` covers the batch row, the handler paragraph, the counters and the twin paragraph. `sync-store.md` describes the subtree delete with its numbers.

### G6: one local observation type; a scan commits in one transaction
- The type `localObservationBatch` is renamed `localObservation`. A `kind` field replaces the old flags, with three values: `localPatch`, `localSnapshot` and `localTruthSuspect`.
- The engine's apply switches over every kind, so the exhaustive linter catches a new kind the apply forgets.
- A snapshot carries its rows, its findings (the empty set included) and its crossed mounts.
- Removed: the findings channel, `SetSkippedChannel`, `TrySendScanFindings`, the loop arm, `handleWatchSkippedSignal` and `resources.skippedItems`.
- A snapshot dropped on a full channel asks for a recovery scan, as every dropped observation already did.
- Store: `CommitLocalSnapshot` commits findings, then rows, the root binding and mounts in one transaction. It reports rows changed, findings changed, the bound identity and the mount set after the commit.
- `ReplaceLocalStateReportingChange` and `ReplaceLocalStateBindingRoot` are deleted.
- `reconcileObservationFindingsTx` is extracted, and the existing reconcile function uses it.
- Engine: `commitLocalSnapshot` takes a `localSnapshotCommit`, binds the root while a bind is pending, and marks nothing itself.
- `engineFlow.commitLocalScan` is the one path, used by the watch snapshot, the replan and the protected-root event.
- `refreshLocalCurrentState` is removed, and so is the findings-reconcile failure step.
- The dry run's scratch commit uses the store's `CommitLocalSnapshot`. That needed no flag and no second code path; it does not bind.
- Stale mount flags: the observer's flags are now an `atomic.Pointer` map. Each scan copies the set at its start, and the runtime sets it from the store after every snapshot commit.
- A snapshot taken before a shutdown is still committed, on a detached context bounded by `OutcomeCommitTimeout`. This is what the removed findings handler did.
- Dirty marking is unchanged: the runtime replans when rows or truth-complete changed, or when the finding set changed.
- Tests added:
  - `TestLocalObserver_AFullScanSendsItsFindingsWithItsRows` (it also covers the full-channel recovery request)
  - `TestWatchRuntime_ASnapshotCommitsRowsFindingsAndMountsTogether`. Its failure case is a finding the batch does not manage, which stores none of the three.
  - `TestWatchRuntime_ASnapshotWhoseLastFindingClearedMarksDirty`
  - `TestWatchRuntime_ASnapshotRefreshesTheObserversMountFlags`
  - `TestWatchRuntime_ASnapshotTakenBeforeShutdownIsStillCommitted`, which replaces `TestWatchRuntime_HandleWatchSkippedSignal_ShutdownCancellationIsNonFatal`
- Seen failing: the commit test and the mount-flag test failed under a temporary revert (rows committed without their findings, flags not refreshed). The revert was then undone.
- Name adjustment: `TestWatchRuntime_FallbackScanRecordsTheMountsItCrossed` is not a separate test. A unit test cannot cross a real mount. The fallback and safety scans send through the same `TrySendFullLocalSnapshot`, and the observer test covers its mounts.
- Renamed: `TestReconcileSkippedObservationFindings_ReturnsErrorOnFailure` is now `TestCommitLocalScan_ReturnsErrorOnFailure`. No spec cited the old name.
- Migrated to the snapshot path: the safety-scan, fallback and replan-only-when-changed tests, the store diff tests, and the phase0, output-noise and root-identity tests.
- Spec changed: `sync-observation.md` (the runtime streams, the observation paragraph, the finding-set paragraph, two table rows), `sync-engine.md` (the observer lifecycle, the detached-commit table row, the watcher-events paragraph) and `sync-store.md` (a table row). The requirements keep their meaning.

### G5 (overrun, not started)
- The plan entry says about 85 test sites. Measured on the base:
  - about 72 event-channel lines in 15 test files
  - about 87 `scanResult.Events` reads, 79 of them in `observer_local_test.go`
  - 84 create/modify type sites
- That is about 240 sites in total, roughly three times the entry.
- G6 already fixed one of its five stale comments: the `runSafetyScan` doc.

### G7 (overrun, not started)
- The rider names 12 product files and 57 lines. That part holds.
- The tests do not fit it:
  - `observer_remote_test.go` (3,041 lines) has 148 call sites of `ConvertItems`, `ClassifyItem` and `FullDelta`, with about 64 field reads and 32 type reads.
  - About 60 more call sites are spread over 8 other test files.
- The converter's helpers (`normalizeRemoteFacts`, `applyBaselineKind`) mutate a `changeEvent` in place, so they need rewriting too.

## Checks
- `go build ./...`: pass.
- `go vet ./...`: pass.
- `go vet -tags=e2e,e2e_full,integration,stress ./...`: pass.
- Focused `internal/sync` runs, all pass on the final tree:
  - the G4 set: `TestHandleDelete|TestLocalObservationBatch|TestWatchRuntime_LocalObservation|TestWatch_Delete|TestWatch_Detects|TestHandleFsEvent|TestFullScan|TestRunOnce_UnicodeForm|TestDeleteLocalState|ListsEvery|Table|Governs|Exhaustive|Registered`, 156 s
  - the G6 set: `TestWatchRuntime_|TestLocalObserver_|TestLocalObservationBatch|TestReplaceLocalState|TestCommitLocalScan|TestPhase0_|TestOutput|TestRootIdentity|TestScanner|TestLocalHashCache|TestWatch_|TestReverseDispatchTable|ListsEvery|Governs|Exhaustive|Registered`, 373 s
  - the twin tests, 5 times over
- `go test -run RealRepoIsClean ./internal/devtool/`: 3 PASS lines (not the 4 expected), exit 0. The cause of the difference was not checked.
- Benchmark: numbers above (log in the scratchpad, `R3-g4-bench.log`).
- **Not completed.** `go run ./cmd/devtool verify lint` and `go test -count=1 -timeout 90m ./internal/sync/...` were both queued on the heavy lock behind R1's ship race run. The session then stopped both while they waited, because the host was critically low on memory. Neither produced a result, and neither was restarted. Both still need running on `da043274`.
- No `-race` run: the ship step runs it.

## PARKED lines added
- R3-p1: the exact-delete counter (`LocalObservationExactDeleteCount`) can only be nonzero for a local move batch, which no producer builds. Its readers are in `internal/perf` and `internal/cli`.


## Merged as #958

- Merge commit `f2250310` (2026-11-19 22:28 PST), installed the same evening; rows G4 and G6 set merged.
- Rebase onto `ed2f2441`: conflicts in `engine_watch.go` (main split observer start into a builder; the findings channel stays removed), `engine_current_plan.go` (main's planning-state error constant kept) and one spec table row (main's renamed mount test kept). No behaviour conflict.
- Fixed before the PR: lint on `./internal/sync/...` (12 issues, all from this change: gocyclo, shadow and an unused nolint in `CommitLocalSnapshot`, now split into two helpers; a duplicated path-list query, now one function; an unused bool result; goconst; two by-value large parameters; eight boolean comparisons in asserts). `TestIsForeignMountPoint_CrossingRecordsTheBoundary` passed on main and failed on the branch, because a scan now copies the mount flags at its start; the test copies them as a scan does. The four new watch-runtime tests are declared stress members and the replaced one is removed.
- Checks: both vets, lint 0 issues, `-race -p 1 ./internal/sync/` passed (4775 s), pre-pr, pre-merge and post-merge gates.
- Live (test account, one at a time): `TestE2E_Sync_NamingCorpusRoundtrip`, `TestE2E_Status_IssueLifecycle`, `TestE2E_Edge_UnicodeFilenameRoundtrip`: all PASS. No e2e test covers a Unicode-form twin of a synced file, nor an upload refusing a case-variant overwrite as such.
- Nightly claims released (not fixed here): the three `internal/sync` package timeouts and the stress failure of `TestRunWatch_ShutdownStopsRetryAndTrialTimers` (parked as R2-n1, R2-n2, W101-n2).
