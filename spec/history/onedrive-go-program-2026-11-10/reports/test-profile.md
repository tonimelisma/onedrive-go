# Why the sync unit tests are slow (measured 2026-11-20)

Tree: `origin/main` `f0ac23e5`, in a detached scratch worktree (now removed). Host: the dev host, 2 cores, shared. `go test -json -count=1 -p 1 -timeout 120m ./internal/sync/...`, `TMPDIR=~/.cache/onedrive-go-tmp` (ext4 on the SSD), no `-race`. Raw JSON: scratchpad `test-profile.json` (non-short) and `test-profile-short.json`.

"test-s" is a test's own `Elapsed`. Parallel parents report 0 s, so their cost is the sum of their subtests. On 2 cores the parallel tests overlap and wait for CPU, so test-s adds up to more than the package wall time (about 1.8x in `internal/sync`). Use test-s to rank tests, and the package wall time for totals.

## Headline

1. **Most of the CPU goes to compiling SQL.** In a CPU profile of the whole short `internal/sync` run, **77.5% of all CPU is in `sqlite3Prepare`**. 68% sits under `(*engineFlow).queryPlannerRowsTx`: 58% preparing the reconciliation query (`syncstore.QueryReconciliationRows`, `syncstore/sqlite_compare.go:860`, called from `engine_current_plan.go:586-594`), and 10% preparing the comparison query. Every plan prepares this roughly 430-line CTE query again, because `queryStateRows` calls `runner.QueryContext(ctx, query)` with no statement cache (`syncstore/sqlite_compare.go:766`). Only 8% of CPU is `sqlite3VdbeExec`, the work of actually running queries.
2. **fsync stalls take most of the rest.** Test stores open with `journal_mode(WAL)` and `synchronous(FULL)` (`syncstore/store.go:150-151`) on a real disk. Moving only `TMPDIR` to tmpfs, with no code change:
   - non-short tree: **1760 s → 720 s (−59%)**
   - short tree: **307 s → 190 s (−38%)**
   - `syncplan` non-short: **952 s → 225 s**
3. **The non-short tier is dominated by a few tests. The short tier is not.** In the non-short tier, the top 10 tests take 76% of the time and one test (`TestPlannerExhaustive_…`) takes 951 s. In the short tier, the top 10 take only 23% and the top 100 take 60%. There, 248 `TestRunOnce_` tests make up 342 of the 493 test-s in `internal/sync`. Each builds an engine on a real temp dir and SQLite file and runs a few passes, at about 0.1 s of CPU per plan.

## What per-PR time is made of

- **(a) pre-pr** (`verify --dod --stage pre-pr`, the `pr` profile, `internal/devtool/verify_quick.go:84-137`). It runs lint, format, repo rules, `go build`, tagged vet, then `go test -short -timeout=… ./...` over the whole repo (`verify_quick.go:172-176`; with `--shard` it adds a `-run` name-length pattern). R7 and R9 step timings (`reports/R7-prepr.log`, `R9-prepr.log`):

  | Step | Time |
  |---|---|
  | lint | 2m04 to 2m33 |
  | build | 0m26 to 2m00 |
  | format | 0m11 to 0m27 |
  | short-unit-tests | 4m19 to 6m02 |
  | **total** | **7m06 to 11m17** |

  My whole-repo `go test -json -short -count=1 ./...` at the default `-p`: 450 s wall. The packages on that run's critical path: `internal/sync` 260 s, `internal/devtool` 80 s, `internal/cli` 24 s, `internal/graph` 20 s, `syncstore` 16 s. The sync packages are 63% of all package time. In R7 and R9, `internal/sync` alone took 220 to 223 s.
- **(b) ship-time `-race`**. The `race` profile is `go test -race -count=1 -timeout=… ./internal/sync/...`, not short (`verify_race.go:58-72`, `verify_stress.go:21`). Under `-race` the model and exhaustive tests already scale themselves down:
  - exhaustive: every 40th pair (`planner_exhaustive_test.go:107,1562`)
  - matrix: a subset (`reconcile_matrix_test.go:145`)
  - chaos: 1 seed × 2 batches (`chaos_remote_test.go:848-853`, `engine_model_based_test.go:66`)
  - model: 2 seeds; crash: 1; replay: 1 seed × 2 batches; orphan: 6 passes
  - planner model: 1 seed (`syncfixture/model.go:18`)

  **`TestReverseDispatch_RunOnceScenariosPass` and every ordinary `TestRunOnce_` and crash test run at full size under `-race`.** I did not run `-race`. All (b) figures below are estimates from the non-race numbers.

## Per package

| Package | non-short wall s | tmpfs non-short s | short wall s | tmpfs short s | top-level tests | subtests (non-short) | skipped in short |
|---|---|---|---|---|---|---|---|
| internal/sync/internal/syncplan | 951.8 | 225.1 | 4.5 | 3.2 | 107 | 137 | 3 |
| internal/sync | 657.6 | 468.6 | 271.6 | 163.0 | 844 | 782 | 8 |
| internal/sync/syncstore | 12.4 | 10.1 | 9.4 | 5.2 | 252 | 69 | 2 |
| internal/sync/internal/syncobserve | 5.1 | 5.1 | 6.0 | 5.0 | 355 | 186 | 1 (skips in both) |
| internal/sync/internal/syncexec | 2.0 | 0.8 | 1.8 | 0.9 | 277 | 99 | 0 |
| internal/sync/syncshortcut | 0.5 | 0.5 | 0.3 | 0.3 | 25 | 25 | 0 |
| internal/sync/syncdomain | 0.0 | 0.0 | 0.0 | 0.0 | 56 | 19 | 0 |
| syncfixture, syncstoretest | no tests | | | | 0 | 0 | |
| **sum of package wall** | **1629** | **710** | **294** | **178** | **1916** | | **14** |
| run wall incl. build | 1760 | 720 | 307 | 190 | | | |

There are 3233 tests including subtests in the non-short run and 2839 in the short run. 13 `SkipInShort` calls (`syncfixture/test_helpers.go:983`) gate the exhaustive tests. The peak tmpfs use of the whole non-short run was about 240 MB above the host baseline.

## Share of time in the slowest tests

| | non-short | short |
|---|---|---|
| sum of test-s (top-level families) | 2294 | 530 |
| top 10 | 1736 (75.7%) | 120 (22.6%) |
| top 40 | 1914 (83.4%) | 222 (41.9%) |
| top 100 | 2045 (89.2%) | 316 (59.6%) |

## 40 slowest, non-short, subtests included (test-s)

| s | pkg | test |
|---|---|---|
| 951.21 | syncplan | TestPlannerExhaustive_ShortHistoriesOnBothSidesKeepContentAndConverge |
| 469.06 | syncplan | …/pairs/worker=3 |
| 459.04 | syncplan | …/pairs/worker=0 |
| 450.97 | syncplan | …/pairs/worker=1 |
| 432.91 | syncplan | …/pairs/worker=2 |
| 127.04 | sync | TestModel_ChaosGraphConverges/seed=2 |
| 77.03 | sync | TestModel_ChaosGraphConverges/seed=1 |
| 32.44 | sync | TestFullRefresh_PermanentlyUndecidedOrphanBacksOff |
| 31.19 | syncplan | TestReconcileMatrix_Golden |
| 17.48 | sync | TestRunOnce_ThrottledBacklogRampsInsteadOfFlooding |
| 14.47 | sync | TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes |
| 13.92 | sync | TestReverseDispatch_RunOnceScenariosPass/TestRunOnce_ThrottledBacklogRampsInsteadOfFlooding |
| 12.40 | sync | TestModel_SameSeedReplaysIdentically/seed=28 |
| 10.79 | sync | TestModel_CrashAtSeededPointsRecovers/seed=2 |
| 10.44 | sync | TestModel_SameSeedReplaysIdentically/seed=3 |
| 9.81 | sync | TestReleaseLocalTree_AReadableFolderStopsAtTheFirstRefusal |
| 9.63 | sync | TestReverseDispatch_…/TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes |
| 9.13 | sync | TestModel_CrashAtSeededPointsRecovers/seed=3 |
| 9.05 | sync | TestModel_CrashAtSeededPointsRecovers/seed=4 |
| 7.89 | sync | TestRunOnce_SlowDeletionAcrossPassesIsHeldWithinTheWindow |
| 7.30 | sync | TestRunOnce_DeletesOlderThanTheWindowStopCounting |
| 7.00 | sync | TestModel_SameSeedReplaysIdentically/seed=28/run=1 |
| 6.74 | sync | TestRunOnce_ReportCountsTheWorkAThrottleLeftForTheNextSync |
| 6.66 | sync | TestReverseDispatch_…/TestRunOnce_SlowDeletionAcrossPassesIsHeldWithinTheWindow |
| 6.60 | sync | TestRunOnce_RandomizedBidirectionalEditsConverge/seed=5 |
| 6.58 | sync | TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked |
| 6.56 | sync | TestCaseInsensitiveShim_RemoteCaseVariantOfALocalFileKeepsBoth |
| 6.52 | sync | TestRunOnce_RandomizedBidirectionalEditsConverge/seed=11 |
| 6.31 | sync | TestReverseDispatch_…/TestRunOnce_DeletesOlderThanTheWindowStopCounting |
| 5.77 | sync | TestReverseDispatch_…/TestRunOnce_APauseThatRefusesItsTrialAgainLeavesEachFileOnce |
| 5.64 | sync | TestRunOnce_RandomizedBidirectionalEditsConverge/seed=12 |
| 5.62 | sync | TestReverseDispatch_…/TestRunOnce_APauseWaitedOutLeavesNothing |
| 5.57 | sync | TestReverseDispatch_…/TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked |
| 5.52 | sync | TestRunOnce_RandomizedBidirectionalEditsConverge/seed=8 |
| 5.39 | sync | TestModel_SameSeedReplaysIdentically/seed=28/run=2 |
| 5.31 | sync | TestRunOnce_RandomizedBidirectionalEditsConverge/seed=7 |
| 5.31 | sync | TestRunOnce_RandomizedBidirectionalEditsConverge/seed=4 |
| 5.27 | sync | TestRunOnce_RandomizedBidirectionalEditsConverge/seed=9 |
| 5.22 | sync | TestModel_SameSeedReplaysIdentically/seed=3/run=2 |
| 5.22 | sync | TestModel_SameSeedReplaysIdentically/seed=3/run=1 |

## Slowest families: disk vs tmpfs (test-s)

| # | test | non-short disk | non-short tmpfs | short disk | short tmpfs | in short? |
|---|---|---|---|---|---|---|
| 1 | syncplan TestPlannerExhaustive_ShortHistoriesOnBothSidesKeepContentAndConverge | 951.2 | 217.0 | – | – | no |
| 2 | TestReverseDispatch_RunOnceScenariosPass (357 subtests) | 350.8 | 237.1 | – | – | no |
| 3 | TestModel_ChaosGraphConverges (2 seeds) | 204.1 | 204.9 | – | – | no |
| 4 | TestRunOnce_RandomizedBidirectionalEditsConverge (12 seeds) | 65.3 | 54.9 | – | – | no |
| 5 | TestModel_CrashAtSeededPointsRecovers (4 seeds) | 33.5 | 25.2 | – | – | no |
| 6 | TestFullRefresh_PermanentlyUndecidedOrphanBacksOff | 32.4 | 19.1 | – | – | no |
| 7 | syncplan TestReconcileMatrix_Golden | 31.2 | 26.5 | – | – | no |
| 8 | TestCrashRecovery_EveryCrashPointConverges (20) | 24.2 | 16.8 | 26.3 | 15.8 | yes |
| 9 | TestModel_SameSeedReplaysIdentically (2×2 runs) | 22.8 | 17.1 | – | – | no |
| 10 | TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges (14) | 20.2 | 16.7 | 17.1 | 12.2 | yes |
| 11 | TestRunOnce_ThrottledBacklogRampsInsteadOfFlooding | 17.5 | 6.7 | 18.9 | 6.3 | yes |
| 12 | TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes | 14.5 | 8.4 | – | – | no |
| 13 | TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath (8) | 11.5 | 8.7 | 11.2 | 12.6 | yes |
| 14 | TestReleaseLocalTree_AReadableFolderStopsAtTheFirstRefusal | 9.8 | – | 11.0 | – | yes |
| 15 | TestRunOnce_SlowDeletionAcrossPassesIsHeldWithinTheWindow | 7.9 | – | 6.4 | – | yes |

Short-tier top 20 (disk, test-s): CrashRecovery_EveryCrashPoint 26.3, ThrottledBacklog 18.9, MoveIntoAFolderRecreated 17.1, NothingIsMade 11.2, ReleaseLocalTree_AReadable 11.0, FileMovedBackOutOfAFolderMovedIntoAnotherFoldersPath 8.4, AFolderDeletedHereStaysOnOneDrive…Waits 8.0, ConfirmDeletesKeepsDeletes… 6.8, SlowDeletionAcrossPasses 6.4, ReportCountsTheWorkAThrottleLeft 5.8, ErrnoShim_DownloadFailures 5.7, DeletesOlderThanTheWindow 5.5, AFileRenamedHereOntoAPath…UntilItsUploadRuns 5.2, syncobserve SocketIOWakeSource_RefreshesEndpointBeforeExpiry 5.0, CrashRecovery_DeleteBoundaries 4.7, SyncedItemsMadeUnrepresentable 4.1, Planner_DeleteGuardIgnoresMoves 4.1, AFileRenamedHere…WhileACreateBeforeItsUploadFails 4.0, AppliedMoveLeavesTheBaselineCurrent 4.0, ARebindKeepsEveryDisk 3.9.

## What costs the time, for the 15 slowest

None of the 15 sleeps on a real clock: the engine tests use `modelClock`, `ManualClock` or synctest virtual time. There are only 2 `time.Sleep` references in the sync tests and fixtures. Three costs recur:
- **P**: the planner's SQL is prepared again on every plan (measured, see headline).
- **F**: SQLite with `synchronous=FULL` on disk (measured by the tmpfs comparison).
- **E**: a fresh engine harness for each case (`t.TempDir` sync root plus a SQLite file from a template, `engine_test_helpers_test.go:240`).

1. **TestPlannerExhaustive_…**, `internal/syncplan/planner_exhaustive_test.go:1556-1566`, `exRunPairs` at `:1572-1597`.
   - It enumerates every pair of short local and remote histories (`exPairs`, `:572`), runs each pair, then runs it again once for each refusal it can make (`:1586-1591`). 4 workers share 2 cores (`exWorkers = 4`, `:104`). Each worker uses one SQLite **file** store (`exQuietStore`, `:1342-1347`).
   - Statements are already cached by `exCachingDriver` (`:1366-1420`), so P does not apply here. The cost is **F**.
   - Measured over 90 s of the run: 38 s user plus 11 s sys out of a possible 180 s of CPU, and about 780 MB written (1.59 M output blocks). In 60 s under strace: 16,159 `fsync` and 119,123 `pwrite64` calls.
   - On tmpfs it takes 217 s instead of 951 s.
   - The number of pairs is unknown: it is not logged.
2. **TestReverseDispatch_RunOnceScenariosPass**, `dispatch_order_test.go:399-410`. It runs the whole `runOnceScenarios()` list (`:146`) again, 357 scenarios, under reverse dispatch order. The cost is a second full run of the engine scenario suite (E+P+F). On tmpfs: 237 test-s.
3. **TestModel_ChaosGraphConverges**, `chaos_remote_test.go:799-821`.
   - 2 seeds (`chaosDefaultSeeds = 2`, `:104`) × `modelBatches = 6` (`engine_model_based_test.go:63`). Each batch has 3 storm passes (`modelChaosPasses`) and then `syncUntilQuiet` (`engine_model_based_test.go:290-311`), with convergence and invariant checks after every batch.
   - The time is the same on disk and tmpfs (204 s both ways), so it is CPU-bound. It is most likely P, but this test was not profiled, so its exact split is unknown.
4. **TestRunOnce_RandomizedBidirectionalEditsConverge**, `engine_model_based_test.go:90-102`. 12 seeds (`modelDefaultSeeds`, `:69`) × 6 batches of `runModelScenarioWith` (`:254-314`), each on a real sync root plus a SQLite file (E+P, some F: 65 s on disk, 55 s on tmpfs).
5. **TestModel_CrashAtSeededPointsRecovers**, `engine_model_based_test.go:140-152`. 4 seeds (`modelCrashSeedCount`, `:70`) × 6 batches. Each batch arms a crash and recovers, which reopens the store (`:296-303`). E+P+F.
6. **TestFullRefresh_PermanentlyUndecidedOrphanBacksOff**, `engine_absence_confirmed_test.go:309-374`.
   - **About 98 `RunOnce` passes over a 2-file tree**: 24 + 48 + 24 passes plus 2 setup passes. The clock is virtual (`clock.advance`), so it never waits.
   - Profiled alone on tmpfs: 9.7 s, of which **84% of CPU is in SQLite's parser (`yy_reduce`)** and 72% is `QueryReconciliationRows` → prepare. That is about 0.1 s of CPU per pass, spent recompiling the same query.
   - Disk 14.5 s, tmpfs 9.7 s (alone). The rest of its 32 s in the suite run is CPU contention.
7. **TestReconcileMatrix_Golden**, `internal/syncplan/reconcile_matrix_test.go:134-175`.
   - 75 single-path cells (3 baselines × 5 local × 5 remote) plus the move matrix. Each cell is one plan on a fresh store.
   - Its own comment says "each plan compiles the comparison SQL (about 0.1 s here)" (`:139-140`). Profiled alone: 17 s, of which 77% is `QueryReconciliationRows` prepare. **P**.
8. **TestCrashRecovery_EveryCrashPointConverges**, `engine_crash_points_test.go:386-410`. Every crash point × lost WAL or not, 20 subtests. Each one is `crashAndRecover`: two engine harnesses, crash, reopen, converge (E+P+F). **Not gated by `-short`; it is the slowest short-tier test.**
9. **TestModel_SameSeedReplaysIdentically**, `engine_model_based_test.go:107-133`. 2 seeds × 2 complete model runs each, to compare transcripts (E+P).
10. **TestRunOnce_MoveIntoAFolderRecreatedAtAMovedFoldersPathConverges**, `engine_move_recreate_test.go:59-75`. A table of 14 cases. Each case is a fresh engine plus seed sync plus 1 or 2 runs (E+P). In short.
11. **TestRunOnce_ThrottledBacklogRampsInsteadOfFlooding**, `engine_throttle_ramp_test.go:285-330`.
    - **A large generated fixture**: 300 real local files (`files = 300`, `:289`) uploaded through a throttle window over up to 40 passes, on a manual clock.
    - Mostly F: 17.5 s on disk, 6.7 s on tmpfs. In short.
12. **TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes**, `engine_virtual_clock_test.go:25-62`. 40 `RunOnce` passes inside a synctest bubble. `VirtualWait(time.Minute)` is virtual time and does not wait for real. P+F: 14.5 s on disk, 8.4 s on tmpfs.
13. **TestRunOnce_NothingIsMadeInAFolderMovingAsideForTheNewFolderAtItsPath**, `engine_move_recreate_test.go:1314-1327`. `ownLandingCases()` × refused (2), 8 subtests, each a fresh engine run (E+P). In short.
14. **TestReleaseLocalTree_AReadableFolderStopsAtTheFirstRefusal**, `engine_shortcut_release_test.go:504-535`. **A large fixture**: `releaseBatchSize + 44` = 300 remote files (`engine_shortcut_release.go:40`). They are downloaded to a real sync root by a first full sync, then released. F + E. In short.
15. **TestRunOnce_SlowDeletionAcrossPassesIsHeldWithinTheWindow**, `engine_delete_safety_test.go:284-321`. A 200-file seeded remote (`seedDeleteSafetyRemote(t, 200)`), synced and then 5 passes. The 200-file seed is shared by `DeletesOlderThanTheWindow`, `ConfirmDeletesKeeps…` and the three `TestPlanner_DeleteGuard…` tests. F+E. In short.

## Size

| Package | test files | test LOC | product files | product LOC | test/product | top-level tests |
|---|---|---|---|---|---|---|
| internal/sync | 137 | 45,031 | 69 | 17,059 | 2.6 | 844 |
| internal/syncobserve | 41 | 13,390 | 23 | 9,463 | 1.4 | 355 |
| syncstore | 49 | 10,272 | 35 | 10,297 | 1.0 | 252 |
| internal/syncexec | 27 | 10,256 | 13 | 5,926 | 1.7 | 277 |
| internal/syncplan | 12 | 8,463 | 9 | 5,211 | 1.6 | 107 |
| syncdomain | 16 | 1,712 | 43 | 5,391 | 0.3 | 56 |
| syncshortcut | 3 | 908 | 16 | 3,034 | 0.3 | 25 |
| internal/syncfixture (test support, non-`_test` files) | 0 | 0 | 43 | 3,172 | – | 0 |
| internal/syncstoretest (test support) | 0 | 0 | 10 | 397 | – | 0 |
| **total** | **285** | **90,032** | **261** | **59,950** (56,381 without the two test-support packages) | **1.5** (1.6) | **1916** |

## Ranked cuts

Baselines on this host:
- **(a)**: short tier of the sync tree 307 s wall; in pre-pr, `internal/sync` takes about 220 to 260 s and is the critical path of a 4.3 to 6.0 minute test step.
- **(b)**: no `-race` measurement. The race profile's own comment records 87 minutes on this loaded 2-core host (`verify_race.go:20-23`).

"Measured" means I ran it. "Estimate" means I did not.

| # | Cut | Saves | (a) | (b) | Coverage | How |
|---|---|---|---|---|---|---|
| 1 | **Test stores stop paying for fsync**: run the PR tiers with `TMPDIR` on tmpfs (zero code: devtool sets it for `pr`/`quick`/`platform`/`race`; CI uses `/dev/shm`), or a test-only store option with `synchronous=OFF` | **Measured**: short tree 307 → 190 s (−117 s, −38%); `internal/sync` short 272 → 163 s; non-short tree 1760 → 720 s (−1040 s, −59%). (b) estimate: 30 to 50% of the race run, because the race-scaled tests are the CPU-bound ones | yes | yes | Unchanged for test logic. Real-disk fsync is no longer exercised per PR; keep the nightly full tier on disk | env (`TMPDIR`) in the pr/race profiles; nightly unchanged |
| 2 | **Prepare the planner's two queries once**, not on every plan: a statement cache in `syncstore.queryStateRows`, or the `exCachingDriver` pattern installed for all test stores | **Estimate**: 77.5% of the short suite's CPU is `sqlite3Prepare`, so after cut 1 (now CPU-bound) `internal/sync` short might go 163 → about 50 to 70 s (−90 to −110 s). Not measured. Caveat: the exhaustive test had to keep the planner-visible tables alive on its connection for cached statements to stay valid (`planner_exhaustive_test.go:1352-1355`); the engine path creates and rolls back those tables per plan, so a cache needs the same change to hit. (b): the SQLite here is pure Go (modernc), so `-race` instruments the parser too; the saving is likely as large or larger (estimate) | yes | yes | None. Product code would benefit too | product change, or a test-harness driver like `exCachingDriver`; no tier move |
| 3 | **Reverse dispatch out of the ship-time race run**: `TestReverseDispatch_RunOnceScenariosPass` runs all 357 scenarios again and is not scaled down under `-race` | Non-race 351 test-s (disk), 237 (tmpfs). About 39% of the unscaled engine-scenario work in `internal/sync` (351 of about 904 test-s), so an estimated 25 to 35% of the race run's `internal/sync` time | no (already `SkipInShort`) | yes | Reverse-order coverage moves to nightly. **Removes per-PR coverage** | env gate, e.g. run only when the nightly or full profile sets a variable, or a stride under `RaceDetectorEnabled` like the exhaustive test's `exRaceStride` |
| 4 | **Ship-time race run at `-short` over the touched packages; full race tree stays nightly** (CI already runs `verify race` on every push to `main`) | Estimate: drops the race runs of the reverse dispatch, chaos, model, orphan, virtual-time, matrix and exhaustive tests. Non-race, these are 1629 − 294 = about 1335 of the tree's 1629 package-s (82%). Race scaling already shrinks some of them, so (b) likely falls by more than 50% | no | yes | **Removes per-PR race coverage of the model and chaos tests**; they still race on every push to `main` and nightly | flag in the ship-time command; nightly and push-to-main unchanged |
| 5 | **Keep one representative case per table family in `-short`**: CrashRecovery_EveryCrashPoint (26.3 test-s short), MoveIntoAFolderRecreated (17.1), NothingIsMade (11.2), FileMovedBackOutOfAFolderMoved… (8.4), AFolderDeletedHere…Waits (8.0), CrashRecovery_DeleteBoundaries (4.7) | About 76 test-s, or about 40 s wall at the measured 1.8 test-s per wall-s (estimate) | yes | no (race runs non-short) | **Removes per-PR coverage** of the other cases; full in nightly | `SkipInShort` on the non-representative subtests |
| 6 | **Shrink the big generated fixtures in `-short`**: 300-file throttle (18.9), 300-file release (11.0), the 200-file delete-safety tests (SlowDeletion 6.4, DeletesOlder 5.5, ConfirmDeletes 6.8, DeleteGuard×3 about 11.6, MassRemoteDelete 3.6) | About 64 test-s, or about 35 s wall (estimate). Most of it is F, so it overlaps with cut 1: after cut 1 the throttle test is already 6.3 s | yes | yes if shrunk | Kept if the thresholds scale with N (needs `releaseBatchSize` and the delete-safety threshold injectable); otherwise **removes coverage** | parametrise N; or `SkipInShort` plus a small variant |
| 7 | Exhaustive planner: only the local `verify default` and nightly pay for it (951 s, or 217 s on tmpfs). Under race it runs every 40th pair | about 0 for (a) and (b) | – | – | – | leave as is; cut 1 already makes it 4x cheaper |

**Can (a) be halved?** At 13 min, halving means about 6.5 min. Lint (2 to 2.5 min), build (0.5 to 2 min) and format stay as they are, so the test step has to fall from 4.3 to 6.0 min to about 1.5 min. Cuts 1 and 2 together would bring `internal/sync` short to an estimated 50 to 70 s. `internal/devtool` (80 s in my run) would then be the next critical path. Cuts 1 + 2 look sufficient for (a) without removing coverage; that rests on the cut-2 estimate. Cuts 5 and 6 are the coverage-reducing reserve.

**Can (b) be halved?** Cuts 1 + 2 cost no coverage and likely halve it (estimate). Cut 3 or 4 adds a large step at a per-PR coverage cost.

Not measured: lint's own breakdown, any `-race` timing, the pair count of the exhaustive test, and a CPU profile of the chaos test.
