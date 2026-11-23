# Track 7 — Test suite quality, onedrive-go @ `6ca9474f` (= `26b3bc8b` + docs)

Read-only review. No `go test` was run. Function-level coverage comes from the
caller's `coverage.out` (every package except `internal/sync`) via
`go tool cover -func`; `internal/sync` was judged by reading its tests. The
invariant mutation checks (16/17 red, download fsync gap already in a PR) are
not repeated here.

Caveat that colours every coverage number below: both CI's per-package
percentages and the local profile are produced by `go test -coverprofile ./...`
without `-coverpkg`, so a package's number counts only its *own* tests.
`synctree` 74%, `localtrash` 69%, `localpath` 66% and `driveops` 78% exclude the
heavy exercise they get from `internal/sync`'s 1,406 tests. Real coverage of
those packages is higher than reported but unmeasured; the 76% gate partly
measures where tests live rather than what runs.

## Answers

### 1. Coverage holes that matter — verdict: ADEQUATE

Support packages, functions on a safety/integrity path at <50% (from the
profile):

| Package | Function (file:line) | Cov | Why it matters |
| --- | --- | --- | --- |
| localtrash | `Empty` (localtrash.go:534) | 0% | purges the trash; only exercised by cli `TestTrashCommands_ListRestoreAndEmpty` through the CLI |
| localtrash | `Abandon` (:236) 40%, `moveNoReplace` (:397) 38%, `IsRestoreArtifactName` (:127) 0% | | `Abandon` is the executor's "could not remove unused trash entry" path (executor_delete.go:305, executor_preconditions.go:631); `moveNoReplace` is the never-overwrite guarantee of restore |
| synctree | `linkBackNoReplace` (detach_unix.go:346) 0%, `moveDetachedOut` (:361) 33%, `copyThroughDescriptor` (:401) 52% | | the set-aside/detach path used by verify-before-destroy; `SyncDir` (synctree.go:1073) 0%, `SameFile` (:1005) 0%, `birthtime.go` `Open/Stat/ReadDir` (:36/:73/:84) 0%, `ChangeTimeFromFileInfo` (ctime_linux.go:15) 0% — birth-time/ctime feed local identity (R-2.2.4, R-6.2.12) |
| localpath | `SyncDir` (localpath.go:272) 0%, `MkdirTemp` (:312) 0%, `Chmod` (:185) 0%, `closeWithContext` (:424) 0%; `RemoveAll` 66%, `AtomicWrite` 69%, `copyExclusive` (rename_noreplace.go:90) 65% | | the directory-fsync of uploads/moves is in `SyncDir` |
| driveops | `removePlaceholder` (transfer_session.go:362) 0%, `uploadHashMismatchError` (transfer_upload.go:116) 0%, `DownloadOptsForItem` (transfer_download.go:104) 0%, `removePartial` (:472) 50%, `finalizePartial` (:347) 61%, `partialCleanupPathInIncludedScope` (stale_partials.go:169) 15% | | upload/download finalize and partial cleanup |
| driveops | `MoveItem/CopyItem/ListRecycleBinItems/RestoreItem` (session.go:683-722) | 0% | thin wrappers; live-covered by `TestE2E_Mv_*`, `TestE2E_Cp_*`, `TestE2E_RecycleBinRoundtrip` (e2e_full only) |
| graph | `PermanentDeleteItem` (items_mutation.go:375), `deleteAndDrain` (:404) | 0% | the recycle-bin bypass. Only caller `cli/recycle_bin_flow.go:156`; only test `TestRecycleBinEmpty_FallsBackToDelete` (cli_workflows_test.go:476) via a mock session — no test pins the HTTP verb/path |
| graph | `Login`/`LoginWithBrowser` (auth.go:41/:82), `CheckSavedLogin` (auth_token.go:48), `readCreatedFileByName` (item_create.go:97) | 0% | token acquisition; save path itself is `fsroot.AtomicWrite` (tokenfile.go:76-96), which has failure-injection tests |
| config | `RemapCanonicalID` (email_reconcile.go:43), `rewriteManagedEmailPath` (:456), `sharedStateOwnedByRenamedAccount` (:479) 0%, `collectStateRenames` (:316) 44%; `IsChildMountID` (mount_state_path.go:58) 0% | | renames state directories when an account email changes — a path-rewrite over sync state with no unit test |
| cli | `confirmRebindRootIntent` (drive_rebind_root.go:100) 47%, `confirmDriveStateResetIntent` (drive_reset_sync_state.go:159) 23% | | the typed `REBIND`/`RESET` token branch and its mismatch-cancels branch are untested: `isWriterTTY(stdin)` has no seam, so tests reach only `--yes` and the non-TTY refusal |
| cli | `runSyncWatch` (sync_flow.go:120) 13%, `runSyncDaemon` (sync_runtime.go:132) 0%, `downloadFolder` (get.go:187), `uploadFolder/uploadWalkEntry/uploadFileEntry` (put.go:200-367), all of `get_shared.go`, `cleanupPendingToken` (auth_flow.go:309) | 0% | daemon entry and recursive put/get are live-only |
| multisync | `NewOrchestrator` (orchestrator.go:137) 25% — the real `syncengine.NewMountEngine` factory closure and hook wiring; `newRestartTimer` (:180) 0%; `closeEngine` (orchestrator_watch.go:275) 44%; `removeCleanupWorkForParents` (:937) 25% | | unit tests inject fake engines; real wiring runs only in E2E |
| retry | `throttle_gate.Wait` (:19) 0%, `TimeSleep` (policy.go:55) 0% | | |
| fsroot | nothing <50%; `CreateTemp` 50%, `ReadFile` 58%, `openWithRoot` 65% | | |

`internal/sync`, judged by reading (1,406 tests; 332 names match safety
keywords; 170 contain "Delete", 23 "Precondition", 23 "Symlink", 14 "Refus"):

- `executor_delete.go` — strong on local delete: hash/size/mtime verify
  (`TestExecutor_LocalDelete_HashMismatch_ReturnsStalePrecondition` asserts
  stale error, file content intact, no conflict copy, no invented upload;
  `executor_delete_unhashed_test.go`), disposable-children family (5 tests),
  symlink families, trash (`TestDeleteLocalFile_MovesTheFileToTheTrash`,
  `TestRunOnce_RemoteDeleteMovesLocalFileToTrash` assert path/reason/size/root
  of the entry). Branches with **no** test evidence: `errRemoteFolderTooLargeToVerify`
  and the `remoteFolderDeleteListLimit` cut-off (:578-:585, :636 — 0 tests
  mention either); `validateLiveFolderDeleteChild` "changed on OneDrive since
  it was synced" (:613; "has not seen" at :605 has 1); `validateRemoteDeleteLocalAbsence`
  "exists locally again" (:474-:509, 0) with `hasRegularFileAncestor`/`exactLocalNameExists`
  (:511-:558, 0 direct refs); `commitTrashPlacement` failure (:319-:329, 0 in
  sync; the trash layer's own `TestTrash_FailedCommitLeavesARecoverablePendingEntry`
  covers the entry, not the executor's handling); `DeleteLocalSymlink` error (:110).
- `executor_preconditions.go` (1,000 lines, no test file of its own) —
  exercised through 22 `*Precondition*` tests and 14 files asserting
  `errActionPreconditionChanged`; the WAL/journal guard is tested by sentinel
  (`executor_database_guard_test.go:55`, `errDatabaseInUse`, data unchanged, no
  download). No evidence for: `remoteMoveAlreadyApplied` (:117; "remote move
  already applied" 0), `validateLocalMoveParentExists` (:670-:684, 0),
  `validateLocalSourceAgainstBaseline` type/mtime branches (:869-:879; only
  "changed size" has a test), `preserveReplacedDownloadTarget` failure branch
  (:624-:637, 0), `validateRemoteParentPrecondition` "is not a folder" (1).
- `planner_delete_safety.go` — strong: 5 `TestDeleteSafetyHold_*` + 3
  `TestPlanner_*` + `TestRunOnce_ConfirmDeletesKeepsDeletesThatAppearedAfterTheUserLooked`
  (59 run / 61 held; "a confirmation is bound to the item it held";
  `remote.deletes` empty while held; emptied-side rule with 6 items). Only
  the threshold formula (`deleteSafetyThreshold`, :190) is tested indirectly.
- `observation_root_identity.go` — `TestNewEngine_RootRecreatedAtSamePathIsRefusedEvenIfInodeIsReused`,
  `TestRunOnce_SyncRootReplacedWhileEngineRunsIsRefused` (both assert
  `ErrMountRootUnavailable` **and** `remote.deletes` empty), token/birth-time
  tests, `TestValidateExpectedSyncRootIdentity_NamesTheMarkerAndTheWayOut`.
  Gaps: `resolveExpectedSyncRoot` error branches (:154, :167, :175 "no record
  of its sync folder" — 0), `SyncedFileCount` (0 refs), symlinked-root
  resolution error (:212).
- `scanner_unavailable.go` — `TestRootDeviceForScan_UnreadableRootAbortsScan`,
  `TestRunOnce_RenumberedDeviceTransfersNothing`,
  `TestRunOnce_RemoteFolderDeleteAfterDeviceRenumberingIsApplied`,
  `TestPlannerPlanCurrentState_UnavailableTruthNeverDeletesData` (6 cases,
  each `assert.Empty(plan.Actions)`). `recordSyncedItemMovedOutOfReach`,
  `hasOtherHardLinks`, `recordUnreadableEntry` have no direct reference.
- `store_write_baseline.go` — `CommitMutation` referenced in 12 files,
  `CheckCacheConsistency` 2, `RefreshLocalBaseline` 1; folder rebind
  cache tests exist. **Every SQL error branch is unreachable by any test**
  (:407-:439, :492-:514, :545-:565, :595-:655, :743-:764, :862, :893-:931)
  because there is no SQLite fault seam (see Q3).
- `engine_run_once.go` — "sync pass limit reached" (:123) 0 tests,
  `oneShotPasses` 0 refs, plan-invariant violations (:234, :241) 0, and the
  cursor/refresh commit errors (:346-:394) are store faults (0).
- `engine_watch_drain.go` — `engine_watch_drain_error_test.go` plus the
  LI-20260830-01 fix test; `handleDrainingObserverError` /
  `handleDrainingRefreshResult` have no direct reference.

### 2. Model-based and scenario tests — verdict: STRONG (design), with a CI-breadth caveat

- **Engine model** `internal/sync/engine_model_based_test.go` (CanopyCheck
  style, header :23-:55). Per batch: 0-3 random ops per side from
  `{create, edit, delete, move, mkdir, replace, hard-link, move-then-recreate,
  recreate-folder, case-rename (case-sensitive only)}` locally and
  `{…, swap, delete-and-recreate, case-rename}` remotely
  (`engine_model_ops_test.go:65-105`), plus mirrored moves (1 in 3),
  cross-side ops (divergent renames, move-vs-edit, case-variant creates) and
  a whole-side wipe that must be *held* (D-002, :480-:503). Six batches, up
  to 8 sync passes each. Invariants (`sync_invariants_test.go:9-28`,
  `assertSyncInvariants`): convergence, no content loss (either side,
  recycle bin, version history or local trash), idempotence, DB integrity
  (`PRAGMA integrity_check`, no `:park:` rows, baseline agrees with
  remote_state and local_state), nothing waiting (`retry_work`, `held_deletes`,
  `pending_creates` empty), no stray artifacts. Named user intervention:
  `resolveNameCollisions` (:430-:449) renames a colliding local entry "as the
  user would", so durable collisions are deliberately not a convergence target.
- **Determinism**: seeds are `1..N` (`modelSeeds`, :170-:198), PCG seeded,
  dispatch order shuffled by seed on one worker and the fake's delta pages
  shuffled by seed (D-090); `TestModel_SameSeedReplaysIdentically` compares two
  transcripts (:108-:132). Contents are fixed strings (`nextContent`, :430), so
  conflict-copy timestamps are masked in the transcript (:255-:258).
  Failures print `ONEDRIVE_GO_MODEL_SEED=<n>` (:426).
- **Companions**: `TestModel_CrashAtSeededPointsRecovers` (seeded crash +
  power loss); `TestCrashRecovery_EveryCrashPointConverges`
  (`crash_points_test.go:316-353`: 6 named commit boundaries × {WAL kept, WAL
  lost}, snapshot re-opened, re-keyed inodes, must converge; `reopen` asserts
  the point was actually reached, :245); `TestModel_ChaosGraphConverges`
  (`chaos_remote_test.go`: 429/503/500/502/504/408/reset, lost replies after
  effect, upload-session timeout, partial download, delta page faults/410/
  splits with repeated items, a concurrent writer — "the storm injected no
  fault" is itself asserted, :487); planner-level model
  `TestPlannerModel_RandomTriplesConverge` (`planner_model_test.go:8-18`,
  thousands of seeds/minute against the real comparison SQL);
  `TestReconcileMatrix_Golden` (99 baseline×local×remote cells planned by the
  real SQL, checked into `testdata/reconcile/matrix.golden`, plus a reference
  planner safety check; `ONEDRIVE_GO_RECONCILE_CONVERGE=1` runs each cell to
  convergence); 33 named scenario tests in `engine_move_scenario_test.go`
  (each a seed that found a bug); `TestReverseDispatch_RunOnceScenariosPass`
  re-runs **123** RunOnce scenarios under reversed dispatch order
  (`dispatch_order_test.go:148-286`).
- **Known non-converging classes**: none open. `rearchitecture-candidates.md:52-62`
  says the pre-#810 non-converging seeds were cross-move cycles, divergent
  renames and move-of-an-edited-file, and that after #810/#812 the engine model
  converges 80/80 and the planner model 500/500. 10 commits cite a model seed
  (#798, #799, #801, #810, #811, #812, #813, #825 …) — the harness is finding
  real bugs, most recently #825.
- **Golden review**: `matrix.golden` was created in #812 and has never been
  regenerated (`git log` shows one commit); the assertion message tells the
  developer to "review the diff, then rerun with -update"
  (`reconcile_matrix_test.go:249`). Nothing mechanical prevents a blind
  `-update`; the PR review is the control. One other golden-ish asset,
  `internal/sync/testdata/watch_capture/{linux,darwin}/dir_move_*/default.json`,
  is referenced by **no Go file** (dead since #470/#667).
- **The CI caveat**: CI's only unit lane is `go test -race` (`verify.go:552-558`,
  both OS legs; stress is manual and also `-race`). Under `-race` the model
  drops to **2 seeds** (`modelRaceSeeds`, :65), replay to 1 seed × 2 batches
  (:113-:117), crash to 1 seed (:161-:166), chaos to 1 seed × 2 batches
  (`chaos_remote_test.go:483-:503`), planner model to 1 seed
  (`planner_model_test.go:650-:653`), the matrix to every 5th cell
  (`reconcile_matrix_test.go:141`), and the absence-confirmed backoff test
  from 24 to 6 passes (`engine_absence_confirmed_test.go:339`). The documented
  defaults (12 / 4 / 2 / 8 / 99) run only on a developer's machine without
  `-race`, or when someone sets `ONEDRIVE_GO_*_SEEDS`. There is no nightly
  non-race job in `ci.yml`.

### 3. Failure injection — verdict: ADEQUATE (SQLite is the hole)

| Boundary | Seam | Example tests | Gap |
| --- | --- | --- | --- |
| Graph transport | `chaosGraph` wrapper over the scenario fake (`chaos_remote_test.go`); `executorMock{Item,Download,Upload}er`; `httptest` in graph/graphtransport/retry (86 test names match failure keywords) | `TestModel_ChaosGraphConverges`, `TestChaosGraph_SplitKeepsLastOccurrence`, `TestDownload_StalledBodyIsAbandoned`, `TestUploadChunk_StalledBodyIsAbandoned`, `TestDownloadRange_DroppedConnectionIsAResponseReadError`, `TestResumeUpload_SessionExpired`, `TestSessionUpload_FinalFragmentFailureAdoptsCommittedItem`, `TestTransferManager_HashMismatch_Retry`, `TestDoOnce_401_*` | `graphtransport.InteractiveTransferClient` 0%; `retry.throttle_gate.Wait` 0% |
| Filesystem (synctree/localpath/fsroot) | `synctest.ErrnoShim` (ops: open, write, stat, mkdir, remove, rename, chtimes — `fsshim.go:32-46`), `CaseInsensitive`, `IdentityShim`; fsroot's own injection hooks | `TestErrnoShim_DownloadFailuresAreClassifiedThenHeal`, `TestCaseInsensitiveShim_*`, `TestRoot_{Open,ReadDir,Remove,Rename,Stat,WalkDir,AtomicWrite,OpenRoot}_FailureInjection`, `TestAtomicWrite_CleansTempOnRenameFailure`, `TestAtomicWriteReportsDirectorySyncFailure`, `TestDownloadToFile_WriteOutOfSpaceReturnsErrDiskFull`, `TestDetachRegularFile_*`, `TestRoot_RemoveEmptyDirNoFollow_ConcurrentChildCreationFailsClosed` | uploads, hashing and followed-symlink reads bypass the seam via `localpath` (`rearchitecture-candidates.md:194-196`): no engine-level injection of EIO/truncation during hash or upload; no `fsync` seam (mutation check MC-1) |
| SQLite | **none** — no fake `sqlTxRunner`, no failing driver, no readonly/ENOSPC DB dir | `TestFault_BaselineCommitError` (closes the DB, `assert.Error` only, `fault_injection_test.go:49-85`), `TestIntegrityCheckFailure_OnlyDamageIsCorrupt` (classifies an error *string* "disk I/O error"), `TestSyncStore_CheckpointReportsFailure`, `TestNewEngine_RequiresResetForCorruptStateDB`; crash harness covers kill + WAL loss | a write failing mid-transaction (`SQLITE_FULL`/`IOERR`/`BUSY`) never happens in any test; cache-vs-DB divergence after a failed commit is untested |
| Clock | `syncClock` interface (`domain_clock.go:20-47`); `inVirtualTime` testing/synctest bubble (`virtual_clock_test.go:9-20`) used in 38 files; `clock.advance` | `TestRunWatch_WatchFallbackScansAtOnceThenAtSafetyInterval`, absence-confirmed backoff, debounce/settle tests | 6 raw `time.Now()` sites and 4 files with raw timers remain in production `internal/sync` |
| inotify / watcher | `fsWatcher` interface (`observer_local.go:425`), `mockFsWatcher` (5 files) | `TestAddWatchesRecursive_{EMFILE,ENOSPC*}`, `TestWatch_EventOverflowRunsRecoveryScan`, `TestRunWatch_WatcherCreateEMFILEFallsBackToPeriodicScan`, `TestWatch_NewDirectoryEMFILEStopsWatchingForFallback` | real inotify only via `TestReadInotifyLimit_ReadsRealProc`; no kqueue/darwin failure test; `watch_capture` replay fixtures unused |
| Token persistence | `fsroot.AtomicWrite` (temp + rename) | `TestRoot_AtomicWrite_FailureInjection`, `TestSaveToken_AtomicWrite` (overwrite + no temp leftovers, `auth_test.go:360-385`), `TestTokenBridge_ForceRefreshRedeemsRefreshTokenOncePerBurst` | — |
| Process signals / shutdown | context cancellation in-process; SIGTERM only in E2E (`sync_watch_e2e_test.go:106`, `socketio_full_e2e_test.go:59`) | `TestRunWatch_ShutdownStopsRetryAndTrialTimers`, worker-pool cancel tests | no SIGKILL anywhere (unit crash harness is in-process) |

### 4. Flakiness — verdict: ADEQUATE

`live-incidents.md` holds 46 entries; the index classifies them: product bug 9,
test bug / test harness 18, Graph quirk 14, CI configuration 1, provider
outage 1 (and 3 "mitigated" quirks with "Recurring: yes" still live). The
live suite has caught real bugs: LI-20261029-01 (NFD-named file never
uploaded while `sync` reported success — a silent-loss class the emulated
case-insensitive volume could not reproduce), LI-20260405-05 (one-shot crash
recovery left durable work unreplayed), LI-20260430-02 (conflict-copy
removal rejected), LI-20260413-01, LI-20260412-02, LI-20260422-02/03.

Recurring families and how each was closed:

| Family | Fix type | State |
| --- | --- | --- |
| LI-20260830-01 watch shutdown stopped before draining (1 in 25 under `-race -count=50`) | product fix: a step failing *because* of cancellation now begins the drain (#753); test now races the run result against the debug event; replay nondeterminism fixed in #813 | fixed |
| LI-20260405-09 / -07 / -08 mount-root children-route lag, destination-path visibility, delete-by-ID 404 | quirk classification + bounded waits in helpers (`live-recurring-families.md`) | mitigated, recurring |
| LI-20260405-06 strict auth preflight `/me` glitches | narrow retry + classified rerun | mitigated, recurring |
| LI-20260408-01 post-upload mtime PATCH 404/5xx | narrow product retry on the finalization path only | mitigated, recurring |
| LI-20260405-04 fast DownloadOnly delta visibility | classified rerun | "closed as test", recurring |
| LI-20260405-03 websocket restart timing | classified rerun | mitigated, recurring |
| LI-20260422-04 / -05, LI-20260424-02, LI-20260430-03 | test-harness fixes (poll, accept retryable first pass, narrow 504 handling) | fixed |
| LI-20260412-04 stress outgrew the 20-min race budget | harness: stress repetition moved to the concurrency pattern (`verify_stress.go:50-58`) | fixed |

`git log --grep=flak -i` has 13 commits; the last four (#722, #753, #759,
#813) are product or harness fixes, not retries. Nothing is "under
investigation" today.

`--classify-live-quirks` (`verify_e2e.go:742-783`, `:527-:560`,
`:596-:625`): it is on only for `workflow_dispatch` (`ci.yml` e2e step), so
PR runs never classify. It reruns **once** and only when exactly one test
failed (`len(failedTests) != 1` refuses) and that test is one of
`TestE2E_Sync_DownloadOnly` (LI-20260405-04), the auth preflight
(LI-20260405-06) or, in the `full-serial-watch-shared` bucket,
`TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart` (LI-20260405-03). A pass on
rerun is recorded (`recordClassifiedRerun`) and printed as a warning. It does
**not** mask broad failures. It can mask a genuine intermittent regression in
those two product tests, because it reruns the test rather than checking that
the failed run showed the documented quirk signature, and `quirk-summary.json`
is uploaded but nothing gates on it.

### 5. Hygiene against the rules — verdict: STRONG (mechanics), ADEQUATE (traceability)

- **`time.Sleep`**: zero in non-E2E tests. Enforced by an AST verifier
  (`verify_test_conventions.go:125-134`, also bans `t.Fatal/Error*`); the only
  hits are fixtures inside `verify_test_conventions_test.go`. E2E has one
  helper, `sleepForLiveTestPropagation` (`sleep_test_helpers_test.go:9-14`,
  `nolint:forbidigo`), with 29 call sites. `TestTimeSleep_Completes`
  (`executor_test.go:3082`) sleeps 1 ms through the production `timeSleep` —
  harmless.
- **"No error only" tests**: a heuristic over 11 packages flagged 143
  functions, but most use helper assertions (`waitForEvent`, `requireConverged`,
  `assertSyncInvariants`, `assert.NotNil`) and are false positives. Genuine
  thin ones: `fault_injection_test.go` (B-318 era, last touched 2026-08-30):
  `TestFault_ContextCancel_WorkerPool` asserts **nothing** although its comment
  promises "no orphaned goroutines"; `TestFault_BaselineCommitError` asserts only
  `assert.Error`; `TestFault_PartialFileCleanup` tests `CleanTransferArtifacts`,
  not an interrupted download. Also `TestReadInotifyLimit_NoPanic` /
  `TestCheckInotifyCapacity_NoPanic` (log only), `TestNewSessionRuntime`,
  `TestCleanTransferArtifacts_NilSessionStore`, `TestExecutor_LocalDelete_FolderNotEmpty`
  (`executor_test.go:2095-2112`: asserts the outcome failed, not that
  `child.txt` survived — the property the test exists for),
  `TestClearSyncRootIdentity_RebindAcceptsReplacedFolder`
  (`engine_root_identity_test.go:202-227`: asserts the engine opens, not that
  the rebind produced no deletes or re-downloads).
- **Name vs assertion, 30 sampled across delete/hold/trash/root/precondition/
  trash/transfer**: 26 assert exactly the named property with specific values
  (e.g. `assert.Empty(remote.deletes)` + `HeldRemoteDeletes == 60` + store rows
  + condition group; trash entry path/reason/size/root; `ErrorIs
  errActionPreconditionChanged` + file content intact + no conflict copy + no
  upload; database guard keeps old bytes and classifies the issue). The four
  weak ones are the ones named in the previous bullet.
- **`Validates:` tags**: the verifier (`verify_requirements.go:41-64`) checks
  that every cited id exists and that every `[verified]` criterion has a citing
  test or an implementing design doc; it accepts a *group* id as evidence for
  its criteria (`requirementKnown`, :96-:98; `requirementCovered`, :113-:115).
  2,370 tags cite 411 of 549 declared ids. Tag density: sync 917/1406 tests,
  driveops 68/157, synctree 60/72, localtrash 10/10, fsroot 1/16, localpath
  3/15. Spot-check of 40 tags (two seeded draws over the safety files): 34
  accurate, 6 loose or wrong —
  `TestPlannerPlanCurrentState_ExpandsEditEditConflictIntoConcreteActions` →
  R-2.1.3 (download-only) and R-2.10.4 (status display) instead of R-2.3.x;
  `TestExecutor_CreateRemoteFolder_RecordsObservedLocalIdentity` → R-2.17.1
  (local move propagation) instead of R-2.17.3;
  `TestExecutor_Upload_LargeFileSuccess` → group R-5.1 "Parallel Transfers";
  `TestNewSyncStore_WALMode` → R-6.5.1/R-2.5.2 "durable, transactional writes
  that survive process kill" (a PRAGMA check; R-6.5.1 has exactly **one**
  citing test, this one, and is `[verified]`, while the crash harness cites
  R-6.8.9);
  `TestTransferManager_EmptyItemWithoutDownloadURLCreatesEmptyFile` → R-5.5.1
  (hash verification); `TestWatch_OrphanUnderFetchedVaultStaysExcluded` →
  R-2.1.2 (generic watch mode). Group-only citations (R-2.10, R-6.2, R-1.3,
  R-2.4) on synctree/executor tests are common.

### 6. E2E tiers — verdict: WEAK for the safety behaviours

- **Fast lane** (every PR, tag `e2e`, 10 min, `-parallel 5`, no race): 7
  tests — auth preflight, fixture preflight, `TestE2E_FileOpsSmokeCRUD`,
  `TestE2E_Sync_UploadOnly`, `TestE2E_Sync_DownloadOnly`,
  `TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount`,
  `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression`. No bidirectional
  sync, no delete of a synced file through the engine, no conflict, no watch
  round trip runs on a PR.
- **`e2e-full`** (manual `workflow_dispatch` only; three buckets, 60 min,
  `-race`): about 115 tests — bidirectional merge, edit/delete conflict,
  delete propagation, nested deletion, directional modes, mtime-only,
  idempotent re-sync, naming corpus, folder moves both ways, cross moves, two
  writers, type changes, 25 MiB file between clients (`hardeningLargeFileSize`),
  5 MiB watch large file, crash-recovery-idempotent (re-runs the CLI after
  `rm`s — no process is killed), orchestrator, shortcut, shared folders, watch
  (SIGTERM graceful shutdown, pause/resume, control socket), recycle-bin
  round trip and empty, mv/cp, status/JSON shape.
- **Integration lane** (every PR): 12 Graph read tests plus one
  create-and-delete-folder.
- **No live coverage at all** (grep over `e2e/`): delete safety hold /
  `--confirm-deletes` (0 hits; the one "held" is a case-collision comment at
  `sync_hardening_full_e2e_test.go:200`); local trash (the word never appears);
  root identity refusal and `drive rebind-root` (0); `reset-sync-state` (0);
  symlink policy (0); conflict copies at scale (only single-file conflicts;
  `RapidFileChurn` is churn, not conflict); large-file resume (both large-file
  tests complete uninterrupted; the only signal ever sent is SIGTERM to watch,
  never a kill mid-transfer); `PermanentDeleteItem` success path (recycle-bin
  empty is live but asserts the bin is empty, not which verb ran).

### 7. Local reproducibility — verdict: WEAK

- `devtool verify` offers `public`, `e2e`, `e2e-full`, `integration`,
  `stress` (`verify.go:56-60`). `public` always runs `go test -race
  -timeout=20m -coverprofile ./...` (`:552-:566`) — all packages, always the
  race detector, no `-run`, no `-short`, no quick profile. CI's Linux leg
  needed 555 s for `internal/sync` alone; on this 2-core host it exceeded the
  budget, which is why the caller's profile lacks that package. No document in
  the repo (CLAUDE.md, developer-onboarding.md, README) explains how to run the
  suite in parts; the only such guidance is in the owner's agent notes outside
  the repo. The de-facto knobs are the env vars in the model/chaos/planner
  headers (`ONEDRIVE_GO_MODEL_SEED{,S,_START}`, `ONEDRIVE_GO_CHAOS_SEEDS`,
  `ONEDRIVE_GO_PLANNER_SEEDS`, `ONEDRIVE_GO_RECONCILE_CONVERGE`,
  `ONEDRIVE_GO_DISPATCH_ORDER`), plus the stress pattern
  `TestRunWatch|TestWatch|TestOneShot|TestWorkerPool|Shutdown|Drain|Concurren|Cancel`
  (`verify_stress.go:13`).
- Load-sensitive by design: wall-clock *upper bounds* — `debugEventTimeout =
  10 s` (8 uses), `fakeGraphTimeout = 30 s`, `testWaitTimeout = 30 s`,
  `controlSocketTestReadHeaderTimeout = 5 s`, `browserLaunchTimeout = 15 s`, and
  10/20/150 s budgets in `internal/multisync` watch tests. These fail, not
  pass, on a slow host, so they produce false reds rather than hide bugs.
  Timer *logic* runs under `inVirtualTime` (38 files) and is immune to load.
  The real masking effect on a slow host is the opposite one documented in
  LI-20260830-01: a one-step-wide race reproduced 2/48 under `-race -count=50`
  and vanished when the loop had more work — load hides races; and the
  `-race` lane's reduced seeds (Q2) explore less exactly where the host is slow.
  No test asserts "nothing happened within N ms" with a real clock (the
  pattern that would pass falsely under load).

## Findings, ranked

**7-1 (high) — Crash points skip the delete boundaries.** The harness's six
points (`crash_points.go:13-28`; reach sites `executor.go:517,696`,
`executor_transfer.go:104,270`, `store_write_baseline.go:433`,
`store_write_observation.go:65`) cover move, upload, download and the two
commits. There is no point between the trash move and `commitTrashPlacement`
(`executor_delete.go:291-329`), nor after the remote `DELETE` succeeds and
before `CommitMutation` (`:331-:387`). The trash layer's own
`TestTrash_CrashBetweenMoveAndCommitIsRecoverable` proves the pending entry
survives, but no test shows the *engine* converging from that state (the next
pass sees the file gone locally and a baseline row that still claims it) nor
with the WAL lost. A delete is the one action that destroys data, and it is the
one action outside the crash harness. Proposed: add
`crashPointLocalDeleteTrashed` and `crashPointRemoteDeleteDone`, extend
`TestCrashRecovery_EveryCrashPointConverges`'s list (8 × 2 cases) and the
model's seeded crash picker, and assert through `assertSyncInvariants` that the
trashed content counts as "still somewhere" and the remote item is not
re-uploaded from the trash.

**7-2 (high) — The remote folder-delete gate has untested branches.**
`validateRemoteFolderDeleteChildren` (`executor_delete.go:560-636`) is the
last check before a recursive delete on OneDrive. No test mentions
`errRemoteFolderTooLargeToVerify` or `remoteFolderDeleteListLimit` (:578-:585,
:636), none the "changed on OneDrive since it was synced" child branch (:613),
none `validateRemoteDeleteLocalAbsence` ("exists locally again", :474-:509).
Proposed (scenario fake): (a) folder with `limit+1` live children → outcome
error `ErrorIs errRemoteFolderTooLargeToVerify`, `DeleteItem` never called,
durable issue recorded, next pass does not relist; (b) a child whose eTag
moved → `errActionPreconditionChanged`, no delete; (c) file recreated locally at
the folder's path (and the regular-file-ancestor case) → stale, no delete.

**7-3 (medium) — CI never runs the non-race breadth of the model family.**
`verify.go:552-558` always passes `-race`; under it the engine model runs 2
seeds, replay 1 × 2 batches, crash 1 seed, chaos 1 × 2 batches, planner model 1
seed, matrix 20/99 cells (evidence in Q2). The 12/4/2/8/99 defaults and the
80/500-seed sweeps cited as convergence evidence exist only on a developer's
machine. Proposed: a scheduled (nightly) and dispatchable `verify model` step,
no `-race`, running `-run 'RandomizedBidirectionalEditsConverge|TestModel_|TestChaos|TestPlannerModel|TestReconcileMatrix'`
with `ONEDRIVE_GO_MODEL_SEEDS=80 ONEDRIVE_GO_PLANNER_SEEDS=500
ONEDRIVE_GO_RECONCILE_CONVERGE=1`, failing the workflow and uploading the
failing seed.

**7-4 (medium) — No SQLite fault seam.** Every `fmt.Errorf("sync: …: %w")`
in `store_write_baseline.go` (:407-:439, :492-:514, :743-:764, :862,
:893-:931), the cache-reload branches (:595-:655) and the cursor commits in
`engine_run_once.go:346-394` are unreachable; the only fault is a closed DB
asserted with `assert.Error`. The cache-vs-DB invariant after a failed commit
is therefore unproven. Proposed: a failing `sqlTxRunner` (fail the N-th
`Exec`/`Commit` with `SQLITE_FULL`, then heal) injected into `SyncStore`;
tests: `CommitMutation` fails → `CheckCacheConsistency` reports 0 mismatches and
the next `RunOnce` re-plans the same action without loss; `RefreshLocalBaseline`
mid-transaction failure leaves no half-refreshed rows.

**7-5 (medium) — The live suite exercises none of the user-facing safety
mechanisms.** Delete hold / `--confirm-deletes`, local trash, root-identity
refusal and `rebind-root`, `reset-sync-state`, symlink policy and
kill-mid-transfer resume have zero E2E coverage (Q6), and the PR fast lane runs
no delete or conflict at all. Proposed `e2e-full` tests: (a) remove >50 synced
files locally → `sync` reports held, OneDrive unchanged, `sync
--confirm-deletes` applies exactly that set; (b) remote delete → `trash list`
shows it, `trash restore` round-trips bytes and mtime; (c) rename the sync dir
away and recreate it → `sync` refuses with the rebind hint and makes no remote
delete, `drive rebind-root --yes` recovers with zero deletes; (d) SIGKILL
`sync` mid 25 MiB upload and mid download → next run adopts the session /
reuses the partial (assert via the debug-event log) and the hash verifies.

**7-6 (medium) — The classified rerun reruns the test, not the quirk.**
`classifyFastE2EQuirk` / `classifyFullE2EQuirk` (`verify_e2e.go:742-783`)
permit a single rerun of `TestE2E_Sync_DownloadOnly` and
`TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart` on any failure of exactly
that test; a genuine intermittent regression in either becomes a warning in
`quirk-summary.json`, which nothing gates. Proposed: require the quirk
recorder's event for the named incident in the failed run's `-json` output
before allowing the rerun, and fail the step when the rerun was taken without
it; publish the count of classified reruns per month.

**7-7 (medium) — Confirmation-token branches of destructive commands are
untested.** `confirmRebindRootIntent` 47% and `confirmDriveStateResetIntent`
23% (`drive_rebind_root.go:120-133`, `drive_reset_sync_state.go:168-191`): the
"type REBIND/RESET", mismatch-cancels and EOF paths never run because
`isWriterTTY(stdinAsWriter(stdin))` has no seam. Proposed: inject `isTTY
func(io.Reader) bool` through `CLIContext`; table test: exact token proceeds,
lowercase or empty cancels with the exact error, EOF cancels, `--yes`
bypasses, non-TTY refuses.

**7-8 (medium) — Upload-side filesystem faults bypass the ErrnoShim.**
Uploads, hashing and followed-symlink reads go through `localpath`
(`rearchitecture-candidates.md:194-196`), so `TestErrnoShim_*` can only fault
downloads; an EIO mid-hash or a file truncated between hash and upload has no
engine-level test (`TestExecutor_Upload_SourceHashChangedBeforeTransferReturnsStalePrecondition`
covers change, not failure; `TestUploadFile_StatFailure_WrapsError` is
driveops-local). Proposed: `TestErrnoShim_UploadReadFailuresAreClassifiedThenHeal`
once reads are routed through synctree; until then, a driveops test that fails
the `io.ReaderAt` after N bytes and asserts no item is created or the created
item is adopted only on hash match.

**7-9 (low) — `PermanentDeleteItem` has no HTTP-level test.**
`items_mutation.go:375-420` is 0%; the cli test asserts fallback IDs on a mock.
Proposed: `httptest` asserting `POST …/items/{id}/permanentDelete`, 405 →
`ErrMethodNotAllowed`, 204 with body drained, and the cli success path
recording `Deleted` without calling `DeleteItem`.

**7-10 (low) — Thin tests whose names promise more.** `fault_injection_test.go`
(three tests, Q5), `TestExecutor_LocalDelete_FolderNotEmpty` (add
`assert.FileExists(child.txt)`), `TestClearSyncRootIdentity_RebindAcceptsReplacedFolder`
(add `assert.Empty(remote.deletes)` and no downloads after rebind), the
`*_NoPanic` pair, `TestNewSessionRuntime`. Proposed: add the missing property
assertions; retire `fault_injection_test.go` in favour of the chaos/crash
harnesses or make the worker-pool test use `goleak`.

**7-11 (low) — Traceability drift.** 6 of 40 sampled `Validates:` tags are
loose or wrong (list in Q5); R-6.5.1 "survive process kill" is `[verified]` on
one PRAGMA test; the verifier accepts group ids as evidence
(`verify_requirements.go:96-115`); fsroot 1/16 and localpath 3/15 tests are
tagged. Proposed: a verifier rule that a `[verified]` *criterion* needs at
least one test citing the criterion itself; retag the six; tag fsroot/localpath.

**7-12 (low) — Dead fixtures.** `internal/sync/testdata/watch_capture/` (4
JSON files) is referenced by no Go file since #470/#667. Proposed: delete, or
wire into a watcher-replay test for the dir-move-into/out-of-scope cases it
describes.

**7-13 (low) — No documented partial run or quick profile.** Nothing in the
repo says how to run the suite on a small host; `verify public` is all-or-
nothing with `-race`. Proposed: `devtool verify quick` (no race, `-short`,
model 2 seeds, matrix subset) and an onboarding section listing the
`ONEDRIVE_GO_*` knobs, the stress pattern, and expected minutes per package on
2 cores.

**7-14 (low) — Coverage is measured per package only.** `-coverprofile`
without `-coverpkg` makes synctree/localtrash/localpath look 66-74% covered
when `internal/sync` exercises most of them. Proposed: an informational
`-coverpkg=./internal/...` profile for the `internal/sync` run beside the gate.

## Overall

The suite is unusually strong where it is strong: a deterministic,
replayable model test with a shared invariant library, a planner-level model,
a crash harness with WAL loss, a chaos Graph layer, filesystem shims, an AST
verifier banning sleeps and bare `t.Fatal`, a requirement-traceability check,
and an incident ledger that turned live flakes into product fixes rather than
retries. Its trust boundaries are: (1) the delete path is the best unit-tested
area but is outside the crash harness and has untested last-gate branches
(7-1, 7-2); (2) the breadth that justifies "converges 80/80" never runs in CI
(7-3); (3) SQLite failures are assumed, not tested (7-4); (4) none of the
safety mechanisms a user would actually hit — hold, trash, rebind, resume — is
proven against live OneDrive, and the PR lane proves almost nothing about sync
(7-5). For a go/no-go: the unit and model evidence supports shipping the
engine; the live evidence for the safety features rests on unit tests alone.
