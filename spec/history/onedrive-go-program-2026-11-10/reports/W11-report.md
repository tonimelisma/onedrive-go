# W11 report: stored ordered and remote times saturate; the SQLite URI escapes its path

Commit `300bb872` on `fix/store-time-saturation` (on 27e76cc3), amended, not pushed. 60 files. Second pass: items 1-8 of the review.

## What changed (second pass; first pass kept as accepted)
- `store_sql_encoding.go`: `storedTimeNano` also encodes remote modification times; the epoch stores `storedEpochNano` (1 ns). `storedTimeIsSet` (only 0 is "no time"); `remoteModTime` (the one decoder of a stored remote mtime).
- Remote mtime writers through the encoder: `item_converter.go` (`classifyAndConvert`; `toUnixNano` deleted from `observer_remote.go`), `executor_transfer.go` (`uploadOutcome`), `executor_preconditions.go` (`remoteStateRowFromGraphItem`). Decoder users: `executor_transfer.go` (`ExecuteDownload` opts), `executor.go` (`createLocalFolder`). `driveops.DownloadOpts.RemoteMtime` is a `time.Time`; `publishPartial` sets it unless zero.
- Predicate users: `engine_runtime_lifecycle.go`, `engine_runtime_invariants.go` (2), `engine_runtime_owners.go`, `engine_watch_maintenance.go` (2), `store_observation_state.go` (2), `block_scope_rows.go`, `store_pending_creates.go` (`intentIsFresh`, SQL `<> 0`), `store_write_baseline.go`, `executor.go`.
- `store_write_baseline.go` `fillMutationFromBaselineDefaults`: a mtime is filled from the baseline only for a side the publication's view did not observe.
- `local_hash_reuse.go`: `modifiedInRacilyCleanWindow` (time-based mtime, integer ctime with the reason in a comment), used by `canReuseBaselineHash` and `scanner_hash_cache.go`; `nanosPerSecond` removed from `scanner.go` (tests use `racilyCleanWindow`).
- `store_observation_state.go`: `readObservationStateTx` removed; its 10 call sites (9 product in 7 files, 1 test) call `readObservationStateFromTx`; `loadCurrentInputsTx` lost the `store` parameter that then went unused (3 callers).
- `engine_runtime_lifecycle.go` `scopeHasBlockedRetryWork` asks the store's count; `store_scope_admin.go` returns the count error unwrapped and the prune names the scope.
- `store_retry_work.go`: `PruneRetryWorkToCurrentActions` in one transaction (`listRetryWorkWithRunner`).
- DSNs: `store.go` and `store_inspect.go` pass the builder call to `sql.Open`; `internal/synctest/sqlite.go` `SQLiteFileURI`; internal/sync tests (baseline, crash points, run once, schema, reset, disk full) use `sqliteFileURI`; cli status, e2e and multisync tests use `synctest.SQLiteFileURI`.
- Docs: `sync-store.md` "Stored times" (remote rows, decoder, accepted loss, local times, the epoch), Schema And Open Semantics (test builder, source test), Verified By row; `sync-observation.md` (window helper); `drive-transfers.md` (RemoteMtime). Requirement that governs modification times: R-6.7.34 (unchanged; R-2.2.3 for the epoch loop).

## Tests added (second pass; each seen failing first, or not compiling where it names a new function)
- `TestRunOnce_ARemoteFileDated2300DownloadsWithTheLastStorableTime` (was 1715), `...1601DownloadsWithTheFirstStorableTime` (was 2185; tmpfs here holds exactly 1677-09-21T00:12:43.145224192Z, the first storable time; ext4 would clamp to 1901-12-13; the outcome records the stat), `TestRunOnce_ALocalFolderForARemoteFolderDated2300GetsTheLastStorableTime`, `TestRunOnce_ALocalFileDated2300UploadsAndConverges` (the outcome stores MaxInt64 and the next observation of the item plans nothing; the file is dated with `syscall.UtimesNano`, because Go's `os.Chtimes` converts through `UnixNano` and wraps such a time itself).
- `TestRunOnce_AFileRedatedToTheEpochConverges` (local and remote subtests; was a baseline update every pass), `TestRunOnce_ARemoteFileDatedTheEpochKeepsItsTimeLocally` (was the download time). All listed in `runOnceScenarios()`; they pass under reverse dispatch.
- `TestStoredTimeNano_RemoteModificationTimeWritersAgree` (converter, upload outcome, live row at 2300, 1601, the epoch, in range), `TestRemoteModTime_DecodesAStoredRemoteModificationTime`, the epoch row in `TestStoredTimeNano_SaturatesAtBothEndsOfTheRange`.
- `TestStoredTimes_EveryDecodeIsDeclared`, `TestStoredTimes_EveryPlainUnixNanoIsDeclared` (source-reading).
- `TestModifiedInRacilyCleanWindow_IsDecidedOnTimes` (last second racy, pinned), `TestCanReuseBaselineHash_AFileDated1601IsNotRacyAndItsHashIsReused`, `TestCanReuseBaselineHash_AFileDated2300IsRehashedLikeAnyFutureDatedFile` (passed before too).
- `TestPendingCreate_AnIntentRecordedBefore1970KeepsItsTimeAndStaysFresh`, `TestRetryLedger_ADelayedRowTimedBefore1970IsHeldUntilItsTime`.
- `TestSQLiteFileURI_TheTestBuilderMatchesTheStoreBuilder` (passed once the builder existed), `TestSQLiteDSNs_AreBuiltOnlyByTheTwoBuilders` (repository walk; it failed first on every concatenation and bare-path open of the first pass's list and on the two product opens that passed a variable).
- `TestEngineFlow_ScopeHasBlockedRetryWork_AnswersForOneScope` (pins the answer; passed before), `TestFault_RetryWorkPruneThatFailsMidwayDeletesNothing` (the fault seam gained `armAfter`), `TestDownloadToFile_SetsTheGivenModificationTimeAndLeavesAZeroOneAlone`.

## Conversions
- `storedTimeNano` (16 sites): the 13 ordered sites of the first pass, plus remote mtimes: `remote_state.mtime` via `classifyAndConvert` (the item's `lastModifiedDateTime`, unbounded), `baseline.remote_mtime` via `uploadOutcome` (same, unbounded), the live precondition row via `remoteStateRowFromGraphItem`. Other remote-mtime fields carry an already encoded value (move outcome, publication, placeholder adopt/follow, download outcome).
- Plain `UnixNano()` in internal/sync product (27, declared): the encoder itself 1; a local time compared for equality 19 (scanner 7, observer_local_handlers 4, executor_preconditions 3, executor_transfer 2, executor_delete 1, local_hash_reuse 1, scanner_hash_cache 1); in memory 7 (scan start ×2, activity stamps ×2, trash purge stamp, executor_delete's two error texts).
- `time.Unix(0, …)` in internal/sync and internal/driveops product (14, declared): ordered stored time 7 (block_scope_rows 1, engine_runtime_lifecycle 1, engine_runtime_owners 1, engine_watch_maintenance 3, store_pending_creates 1); remote mtime 1 (`remoteModTime`); range bound 2 (`storedTimeNano`); in memory 4 (scan start in the window helper, two activity stamps, trash purge stamp). driveops: none.
- The sixteen mtime comparisons are all same-side (checked again: Go and SQL). One cross-side assignment: `downloadOutcome` records the remote stored value as the intended local mtime when the stat fails; it agrees with the scanner, since `time.Unix(0, s).UnixNano()` is `s`.

## Item 4: the epoch
- Local file dated exactly the epoch: stores 0, read as unknown. Skipped: the mtime test of `validateUploadSourcePrecondition`, `validateDownloadTargetMetadata` and `validateLocalSourceAgainstBaseline`; each still has size, ctime (the kernel moves it on any write), identity and a hash check (the upload's expected hash, the download target's hash, `verifyLocalDeleteContent`'s hash or its size-and-mtime equality, which an edit breaks), so no path acts on a changed file. A synced file re-dated to the epoch looped: `local_state` 0 against the baseline's time is metadata drift, and `fillMutationFromBaselineDefaults` put the baseline's time back, so the same `baseline_update` (and a re-hash) came every pass. Fixed: an observed side's time is published as observed. The time is never lost: it is the file's own.
- Remote item dated exactly the epoch: stored 0, so the download and the folder create set nothing (the local copy got the download time: the time was lost), the remote precondition and freshness tests skipped the mtime (eTag, hash and size still decide), and a remote re-date to the epoch looped as above. Fixed: the epoch stores 1 ns after it, a known time; a filesystem coarser than a nanosecond rounds it back to the epoch and the outcome records what stat returns. A remote item with no time at all (0) is now published as 0 too; a move outcome still fills an unknown remote time from the baseline, and the next pass's publication corrects it once. No schema change needed.

## Checks
build ./... PASS; vet ./... PASS; vet -tags 'e2e e2e_full' ./e2e/... PASS; the added tests red then green (except those marked above as passing before); a broad internal/sync `-run` set (store, run-once, fault, engine flow, scanner, observer, executor, list tests) PASS (111 s); reverse dispatch of the new scenarios PASS; `-race` on every added sync test and the driveops one PASS; `go test -count=1 ./internal/driveops/... ./internal/cli/... ./internal/multisync/... ./internal/synctest/... ./internal/syncverify/...` PASS; lint of `./internal/sync/... ./internal/driveops/... ./internal/synctest/...` 0 issues (W11-lint.log; the first run found two dupl and one unparam, fixed: a shared schema-refusal helper, the unused `store` parameter). Live proof: none.

## Deviations
- The epoch stores 1 ns after it (item 4's fix), which changes the encoder for ordered times too (an ordered time exactly at the epoch, reachable only with a clock at the epoch).
- The local-2300 upload test sets its time with `syscall.UtimesNano`: `os.Chtimes` cannot set a time outside 1678-2262.
- The two epoch-convergence scenarios are one test with two subtests (dupl).
- `readObservationStateTx` had 10 call sites, not 11.

## Not fixed
None.

## Review (first pass, 2026-11-12 21:55, on `af77ed72`)

Read: the report, the definition of done, and the whole diff against main (21 files).

Accepted, all of it:

- `storedTimeNano` and its use at every write of an ordered time; the two readers that took a negative time for "no time".
- `sqliteFileURI` in the store and in the read-only inspector.
- The scope prune that counts by scope key, the single block-scope delete, the `ReadObservationState` change, the spec text.

Checked myself:

- `readObservationStateFromTx` ensures the row first and `rememberContentDriveID` returns at once on a zero ID, so `ReadObservationState` keeps its behaviour.
- `ListBlockScopes` returns the query's rows with no nil elements.
- No plan ids, process words, retired backlog ids or principle labels in the diff.
- The conversion count: 150 `UnixNano()` in 47 files with tests, in line with the report's 42 product sites.

Found in review:

- The report names one place where a stored remote modification time becomes a file time (the download). There are two: the local folder create in `executor.go` does the same with `Chtimes`.
- Every comparison of a modification time in sync product code is same-side (local with local, remote with remote; sixteen sites). So a saturating encoder for remote times cannot make a local value disagree with a remote one.
- A modification time of exactly the epoch stores 0, which every reader takes for "unknown". Not in the report.

The ten "Not fixed" entries (decisions are in the plan entry):

- 1: remote modification times saturate through one encoder; local ones keep the plain conversion and are never decoded; one decoder in sync, none in driveops; two source-reading tests. Sent back.
- 2: the racily-clean window is computed on times. Sent back.
- 3, 4: one predicate, only zero is "no time". Sent back.
- 5: the pass-through is removed. Sent back.
- 6, 7: test DSNs go through the builder, held by a source-reading test. Sent back.
- 8: the scope question asks the store's count. Sent back.
- 9: no action. Another branch deletes the query.
- 10: the prune runs in one transaction. Sent back.
- New: the zero sentinel is traced and fixed where it is wrong. Sent back.

Suites: not run on this hash. The second pass changes the same files; I run them on its hash.

## Review (second pass, 2026-11-12 23:48, on `300bb872`, amended by me to `a0c8efa3`)

Read: the product diff against main (806 lines), the test diff (974 lines), the spec word diff (122 lines) and this report.

Accepted, all eight items of the second-pass message:

- 1. Remote modification times saturate at the ends of what Unix nanoseconds hold and are decoded in one function (`remoteModTime`); the three writers (`item_converter.go`, `uploadOutcome`, `remoteStateRowFromGraphItem`) agree, held by `TestStoredTimeNano_RemoteModificationTimeWritersAgree`.
- 2. The racily-clean window compares times (`modifiedInRacilyCleanWindow`), so a file dated outside 1678 to 2262 is not re-hashed on every scan.
- 3. One rule for "no time": only zero (`storedTimeIsSet`).
- 4. The epoch is stored as 1 ns after it (`storedEpochNano`), so a file dated exactly at the epoch is not "no time"; the publication records the observed time, not the baseline's.
- 5. `readObservationStateTx` is gone (10 call sites, not the 11 the message named).
- 6. Test DSNs go through `synctest.SQLiteFileURI`; `TestSQLiteDSNs_AreBuiltOnlyByTheTwoBuilders` reads the source.
- 7. `scopeHasBlockedRetryWork` asks the store's count and no longer fails on an unrelated unreadable row.
- 8. `PruneRetryWorkToCurrentActions` runs in one transaction; `TestFault_RetryWorkPruneThatFailsMidwayDeletesNothing` holds it.

Deviations accepted: the epoch's stored value; `syscall.UtimesNano` in the local-2300 upload test, because `os.Chtimes` itself wraps times outside 1678 to 2262; the two epoch scenarios as one test with two subtests.

Checked myself: `publicationMutationFromAction` sets `LocalIdentityObserved` exactly when the view observed the local side, so item 4's publication fix reads the right side. `downloadOutcome` records what stat returns after the file's time is set, so a filesystem that clamps (ext4's floor is 1901-12-13; the test tree is tmpfs, which holds 1677) converges on what it holds. The local zero-checks (`store_write_baseline.go:317`, `executor.go:1022`, `executor_preconditions.go:396`, `575`, `901`) rightly stay raw: local times keep the plain conversion and 0 is "unknown" there. The six `TestRunOnce_` scenarios are listed in `runOnceScenarios()`. `R-6.7.34` exists.

Corrected by me, in the same commit:

- Three sites still compared a remote stored time with a raw zero beside the new predicate: `action_freshness.go:553`, `executor_preconditions.go:228` and `fillRemoteOutcomeFromBaseline`. All three use `storedTimeIsSet` now.
- The report's residual, "a move outcome still fills an unknown remote time from the baseline, and the next pass's publication corrects it once", is removed. `fillOutcomeFromBaseline` and `fillRemoteOutcomeFromBaseline` take `remoteObserved` and fill the remote time only when the view did not observe the remote side, as a publication does. New test, seen failing first ("Should be zero, but was 9876543210"): `TestExecutor_MoveOutcome_TakesTheBaselineRemoteTimeOnlyForAnUnobservedRemoteSide`. The case where the remote content differs from the baseline already set the time explicitly and is unchanged.
- Two comments that told the code's history (`fillMutationFromBaselineDefaults`, `TestRunOnce_AFileRedatedToTheEpochConverges`) say what the code does.
- `spec/design/sync-store.md` names the move outcome's rule and its test.

A report that says "Not fixed: None" beside a documented residual is not accepted as written; I fixed it instead of a third pass.

Review decisions (mine):

- (i) Every remote stored time compared with "no time" goes through `storedTimeIsSet`.
- (ii) A move outcome, like a publication, takes a remote modification time from the baseline only when the remote side was not observed.
- (iii) History phrasing stays in spec design-doc rationale, which is the repository's convention there; in code and test comments it is reworded.

The source-reading tests' declared counts (`TestStoredTimes_EveryDecodeIsDeclared`, `TestStoredTimes_EveryPlainUnixNanoIsDeclared`) are unchanged: my edits add and remove no such call.

Suites on the amended tree, non-race: `internal/sync` 535 s, `internal/cli` 7.8 s, `internal/multisync` 3.9 s, `internal/driveops` 6.3 s, `internal/synctest` 0.05 s, all ok. `go vet` on sync, driveops and synctest: clean. Targeted lint `./internal/sync/...`: 0 issues. No plan ids or process words in the diff.

Next: place in the stack after the shared-folder release work, pre-pr gate there, PR. No live proof is needed.


## Restack above the shared-folder release (lead, 2026-11-13 19:45 Pacific)

The commit moved from `27e76cc3` onto the accepted shared-folder release commit (then `e6d9d0e8`, now `02fb7271` on main `bb0d76fd`); it is `071b19de`.

Textual conflicts, three:

- `internal/sync/store_reset_test.go` `sqliteHasTable`: the base's body stays (it takes an open `*sql.DB`); this change's stray `sql.Open` lines are dropped.
- `internal/sync/observer_remote_test.go`: `TestToUnixNano` stays deleted (this change's side); the base's `// Validates: R-6.7.28` line above `TestFullDelta_OrphanedItem` stays.
- `internal/sync/store_retry_work.go`: `CountRetryingWork` stays deleted (the base deleted it); `PruneRetryWorkToCurrentActions` is this change's one-transaction version.

Code that arrived below it and broke this change's source-reading tests, two:

- `TestStoredTimes_EveryPlainUnixNanoIsDeclared`: `engine_shortcut_release.go` `releaseFileLooksUnchanged` compares `info.ModTime().UnixNano()` with `row.LocalMtime`. Declared as one local time compared for equality. It needs no `storedTimeIsSet`: a local time is compared with itself and never decoded, as in `canReuseBaselineHash`. `ForgetBaselineLocalCtime` and the release's `restored.LocalCtime = 0` write the zero that `fileChangeTime` already uses for "unknown".
- `TestSQLiteDSNs_AreBuiltOnlyByTheTwoBuilders`: `openPlainSQLite` in `store_reset_test.go` built `"file:"+dbPath`. It calls `sqliteFileURI(dbPath, nil)` now.

Checks on the restacked tree: `go vet` of `internal/sync`, `internal/driveops`, `internal/synctest` clean; the source-reading tests and the SQLite pairing tests pass; the whole-package run is recorded below when it finishes.


## Gate on main `bd2711b0` (lead, 2026-11-14 01:35 Pacific)

- The first pre-PR gate on this base failed in lint: goconst, the string `vendor` twice in the package. The tree-removal check that arrived on main (`walkModuleProductFiles`) and this branch's `TestSQLiteDSNs_AreBuiltOnlyByTheTwoBuilders` each walked the module with their own copy of the walk.
- Fixed as one walker: the new `internal/sync/module_source_test.go` holds `walkModuleGoFiles` (every Go file; hidden directories, vendor and testdata skipped) and `walkModuleProductFiles` on top of it. The DSN test uses the first and now requires that the walk read more than 100 files, so an empty walk cannot pass it.
- Added `TestSQLiteDSNViolations_ReportsAJoinedSchemeAndAnOpenWithoutABuilder`: the DSN check had no test showing it can fail. It gives the check a sample file with the two refused shapes and the two allowed ones.
- `go vet ./internal/sync/` clean; the DSN, URI and tree-removal tests pass; the commit is amended and the gate runs again.


## The pre-PR gate that failed on the short tier's deadline (2026-11-14, orchestrator)

- The gate at about 02:00 ended in `go test -short ./internal/sync/` reaching go test's fixed 10-minute package deadline. The goroutine dump named no hung test.
- Measured afterwards, not guessed. The same package alone behind the heavy-run lock: 269.2 s on main (1631 tests) and 475.6 s on this branch (1657 tests), at different host loads. The four slowest tests back to back: 47.7 s on main, 38.2 s on this branch. So this branch is not slower than main; the same tests vary about 2.5 times with what else runs on the 2-core host.
- The same gate, unchanged, passed at 03:14 with the short tier at 9 min 39 s for the whole tier. PR 901 was opened from that pass.
- The fixed limit is the defect. It is fixed separately: the short tier's go test deadline now scales with the cores like the race tier's (plan row A18, branch `ci/short-tier-deadline-by-cores`).


## Merged as #901 (2026-11-14 04:00 Pacific, orchestrator)

- Main is `a6f6fbf3`; the client is installed from it.
- The first CI run of the pull request failed on one macOS shard only: `TestRunOnce_ALocalFileDated2300UploadsAndConverges` asserted the time ext4 holds for a file dated 2300. APFS holds the asked time itself. The test's helper now accepts any held time from the last storable one up to the asked one, and the test is a two-case table. Only the macOS shard can show this, so a test that asserts what a filesystem holds past 2262 is checked against both platforms' CI before it is trusted.
- The pre-PR gate passed on its second run with nothing changed (short tier 9m39s under two other builders' load); the first ran into the short tier's fixed 10-minute go test deadline with no test hung. That is its own plan row (the short tier's deadline scales with the host's cores).
