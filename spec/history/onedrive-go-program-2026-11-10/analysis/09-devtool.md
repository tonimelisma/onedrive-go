# 09 — devtool: buckets, repo checks, DoD stages, store import, nightly claims, worktrees

Repository `~/src/onedrive-go`, branch `main`, commit `70301c0e`. Read-only analysis; nothing was built, tested or run.

Conventions used below:

- Paths are relative to `~/src/onedrive-go` unless they start with `/`.
- **VERIFIED** = I read the code or file that proves the statement. **SUSPECTED** = not proven; the sentence says what would settle it.
- "Owner-reported" = stated in the host notes `~/agent/CLAUDE.md` (section "OneDrive"), not reproduced here because the task forbids running the tools.
- `system.md` = `spec/design/system.md`; `non-functional.md` = `spec/requirements/non-functional.md`.

Governing text read first: `CLAUDE.md` (370 lines) and `spec/design/system.md` (475 lines; line 3 GOVERNS `internal/devtool/*.go`, line 5 `Implements:` R-6.10.1, .5, .7, .8, .9, .11, .12, .13, .16, .17, .21, .22).

---

## 1. E2E bucket membership

### 1.1 Current behaviour (VERIFIED)

`verify e2e-full` runs three buckets sequentially, each as one `go test` over `./e2e/...` with an explicit name pattern:

- Bucket specs: `internal/devtool/verify_e2e.go:20-56` (`fullE2EBuckets()` at 50); struct `fullE2EBucketSpec{Name, Kind, TestNames, Parallel, Timeout}` at `internal/devtool/verify.go:150-156`.
- Membership is three hand-maintained string slices:
  - `fullE2EParallelMiscTestNames()` `verify_e2e.go:62-123` — 58 names, `-parallel 5` (`verify.go:45`).
  - `fullE2ESerialSyncTestNames()` `verify_e2e.go:125-161` — 33 names, `-parallel 1` (`verify.go:46`).
  - `fullE2ESerialWatchSharedTestNames()` `verify_e2e.go:163-210` — 44 names, `-parallel 1`.
  - Standalone: `fullE2EStandaloneTests()` = `{"TestE2E_FixturePreflight_Full"}` (`verify_e2e.go:58-60`, constant `verify.go:25`).
- Command (`verify_e2e.go:212-224`): `go test -tags=e2e e2e_full -race -v -run=<pattern> -parallel <n> -timeout=60m ./e2e/...`, with

  ```go
  return "^(" + strings.Join(testNames, "|") + ")$"      // verify_e2e.go:231-233
  ```

- `runE2EFull` (`verify_e2e.go:676-724`) runs the preflights and then each bucket with `ONEDRIVE_E2E_SKIP_SUITE_SCRUB=1` (697-698). Only the watch-shared bucket has a classified rerun (`bucketSupportsClassifiedRerun`, 743-745), and only when exactly one test failed and it is `TestE2E_SyncWatch_WebsocketRemoteWakeAndRestart` (`classifyFullE2EQuirk`, 766-789; constant `verify.go:36`).
- The per-PR fast suite has **no `-run`**: `fastE2EArgs()` = `test -tags=e2e -v -parallel 5 -timeout=10m ./e2e/...` (`verify_e2e.go:726-736`). Every `Test*` in a `//go:build e2e` file therefore joins the per-PR live suite automatically.
- Test discovery exists but is test-only: `discoverTaggedE2ETests(e2eDir, buildExpression)` (`verify_e2e.go:280-319`) parses `e2e/*_test.go` with `go/parser` and keeps files whose first `//go:build` line **equals the expression string**:

  ```go
  return strings.TrimSpace(strings.TrimPrefix(line, "//go:build ")) == buildExpression   // verify_e2e.go:334
  ```

  Its only callers are `internal/devtool/verify_runner_test.go:616` (`"e2e && e2e_full"`) and `:644` (`"e2e"`). Production bucket runs use only the hand lists.

### 1.2 What happens to a new e2e test nobody adds to a list (VERIFIED)

- New test in an `e2e && e2e_full` file, not listed: `TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce` (`verify_runner_test.go:610-636`) fails. It discovers every full-tagged test, adds the standalone test (620) and asserts each discovered name is assigned exactly once and each assigned name exists. The test has no short-mode skip, so it runs in the PR `verify quick` shards as well as in `verify default/public`. The test is therefore not silently skipped; a renamed or deleted test left in a list also fails.
- New test in an `e2e`-only file: runs on every PR with `-parallel 5` (no list, no review of whether it is cheap enough). `TestFastE2EExecutionManifestKeepsOnlySmokeLiveTests` (`verify_runner_test.go:638-669`) only has `assert.Contains`/`assert.NotContains` on fixed names, so it does not reject a new fast-tagged test.
- **Blind spot:** a file whose build line is spelled differently (`//go:build e2e_full && e2e`, or `e2e && e2e_full && linux`) is invisible to the string-equality discovery (334) and matches no bucket pattern. Its tests compile into the full binary and are never run by `e2e-full`; no check fails.
- Bucket runs do not pass through the "no tests matched" guard (`internal/devtool/verify_test_match.go:1-66`), so a pattern that matches nothing passes.

### 1.3 Cost

- 135 names maintained twice (the `func` and the list); the second edit is discovered only when the unit test fails.
- Placement decides `-parallel 5` versus `-parallel 1`, and nothing ties that decision to the name. The prefix does not predict the bucket today: `Sync_` appears in all three buckets (5 / 29 / 1), `Shared_` splits 3 misc / 5 watch-shared, unprefixed names split 10 misc / 1 serial-sync (table below).
- Two of the three manifest tests assert on names that no longer exist and so prove nothing: `TestFastE2EExecutionManifestKeepsOnlySmokeLiveTests` (e.g. `TestE2E_FileOps_Whoami`, `TestE2E_FileOps_LsRoot`) and `TestFullE2EBucketsOwnDemotedFastTests` (`verify_runner_test.go:671-712`; e.g. `TestE2E_Whoami_ConfigTolerance`, `TestE2E_Sync_ResolveAll`, `TestE2E_Sync_DeleteSafetyThreshold`, `TestE2E_Sync_ResolveDryRun`, `TestE2E_Resolve_WithWatchDaemonExecutesQueuedIntent`, `TestE2E_Resolve_DeletesWithWatchDaemon`). VERIFIED: none of these names is a `func` in `e2e/`.
- None of the three manifest tests carries a `// Validates:` line (VERIFIED: `verify_runner_test.go` has no such line between 145 and 741).

### 1.4 All `TestE2E_` names by prefix and current bucket (VERIFIED)

Bucket membership is from the three lists in `verify_e2e.go` (line ranges above). Definition files are given where read.

**Not in a full bucket**

| Name | Tag | Defined |
|---|---|---|
| `TestE2E_AuthPreflight_Fast` | `e2e` | `e2e/auth_preflight_e2e_test.go:29` |
| `TestE2E_FixturePreflight_Fast` | `e2e` | `e2e/fixture_preflight_e2e_test.go:11` |
| `TestE2E_FileOpsSmokeCRUD` | `e2e` | `e2e/fileops_smoke_e2e_test.go:17` |
| `TestE2E_Sync_UploadOnly` | `e2e` | `e2e/sync_e2e_test.go:626` |
| `TestE2E_Sync_DownloadOnly` | `e2e` | `e2e/sync_e2e_test.go:652` |
| `TestE2E_ShortcutSmoke_DownloadOnlyProjectsChildMount` | `e2e` | `e2e/sync_shortcut_e2e_test.go:16` |
| `TestE2E_SyncWatch_WebsocketDisabledLongPollRegression` | `e2e` | `e2e/socketio_e2e_test.go:62` |
| `TestE2E_FixturePreflight_Full` | `e2e && e2e_full`, standalone preflight | `e2e/fixture_preflight_full_e2e_test.go:11` |

**`full-parallel-misc` (58, `-parallel 5`)**

| Prefix | n | Names (suffix after the prefix) |
|---|---|---|
| `DriveList_` | 9 | HappyPath_Text, JSON, NoAccounts, AccountsNoDrives, PersonalAccountDoesNotDuplicateCanonicalDrive, ConfiguredNoSyncDir, ConfigTolerance, AllFlag, StaleStateDB |
| `Status_` | 13 | ConfigTolerance, LiveOverlay_ConfigTolerance, PersonalAccountShowsSinglePersonalLiveDrive, AfterSync, JSON, PausedDrive, PerDrive_NoIssuesOrRetries, NoLegacyHistorySurface, JSON_IssueDetails, IssueLifecycle, JSONShape, FilteredDriveIsSubsetOfAllDrives, NoDrives |
| `Shared_` | 3 | FileDiscoveryAndSelectorReadCommands (`e2e/shared_items_fast_e2e_test.go:13`), FileDiscoveryRejectsDriveAdd (`:35`), JSON_RecipientListingUsesLiveAccountCatalog (`e2e/w7_live_caller_proof_e2e_test.go:176`) |
| `Logout_` | 1 | PreservesOfflineAccountCatalog |
| `Pause_` | 2 | WithDuration, IndefiniteAndResume |
| `Resume_` | 2 | NotPaused, AllDrives |
| `CLI_` | 1 | NoResolveCommand |
| `InternalBaselineVerification_` | 1 | AfterSync |
| `Mv_` | 6 | Rename, MoveToFolder, JSON, NotFound, ForceOverwrite, Folder |
| `Cp_` | 5 | File, IntoFolder, JSON, NotFound, ForceOverwrite |
| (no prefix) | 10 | `TestE2E_RoundTrip`, `_ErrorCases`, `_JSONOutput`, `_QuietFlag` (`e2e/fileops_full_e2e_test.go:19, 148, 181, 213`); `_ZeroByteFileSync`, `_UnicodeFilenameRoundtrip`, `_InvalidFilenameRejection`, `_RapidFileChurn` (`e2e/edge_cases_e2e_test.go:25, 57, 90, 125`); `_RecycleBinRoundtrip`, `_RecycleBinEmpty` (`e2e/cli_commands_e2e_test.go:310, 374`) |
| `Sync_` | 5 | QuietMode (`e2e/output_validation_e2e_test.go:93`), MtimeOnlyChange, TransferWorkersConfig (`e2e/sync_edge_cases_full_test.go:156, 227`), IncrementalDeltaToken, DriveRemovePurgeResetsState (`e2e/sync_recovery_e2e_test.go:27, 249`) |

**`full-serial-sync` (33, `-parallel 1`)**

| Prefix | n | Names |
|---|---|---|
| `Sync_` | 29 | DryRun, InternalBaselineVerification, Conflicts, DriveRemoveAndReAdd, BidirectionalMerge, EditDeleteConflict, DirectionalModes_PreserveEditEditConflict, DirectionalModes_PreserveEditDeleteConflict, DirectionalModes_PreserveCreateCreateConflict, DeletePropagation, DownloadOnlyDefersLocalOnlyChanges, UploadOnlyDefersRemoteOnlyChanges, NestedFolderHierarchy, DryRunNonDestructive, ConvergentEdit, InternalBaselineVerificationDetectsTampering, EmptyDirectory, NestedDeletion, IdempotentReSync, CrashRecoveryIdempotent, FolderMovesBothWays, CrossMoves, NamingCorpusRoundtrip, TwoWriters, LargeFileBetweenClients, TypeChangesBothWays, ReconcilesDurableRemoteMirrorTruthWithoutFreshDelta, KillMidUploadResumesSession, KillMidDownloadResumesPartial |
| `SyncFull_` | 3 | DeleteHoldKeepsOneDriveUntilConfirmed, RemoteDeleteGoesToLocalTrashAndRestores, ReplacedSyncFolderIsRefusedUntilRebound (`e2e/sync_safety_full_e2e_test.go`) |
| (no prefix) | 1 | `TestE2E_EdgeCases` (`e2e/sync_full_test.go:27`) |

**`full-serial-watch-shared` (44, `-parallel 1`)**

| Prefix | n | Names |
|---|---|---|
| `SyncWatch_` | 17 | WebsocketStartupSmoke, RemoteToLocal, Bidirectional, ConflictDuringWatch, FileModification, FileDeletion, FolderCreation, MultipleFiles, LargeFile, RapidChurn, GracefulShutdown, TimedPauseExpiry, BasicRoundTrip, OwnerSocketBlocksCompetingOwners, PauseResume, ControlSocketReload, WebsocketRemoteWakeAndRestart |
| `Sync_` | 1 | MultiDriveReport (`e2e/output_validation_e2e_test.go:126`) |
| `Shared_` | 5 | FileDiscoveryAndSelectorRoundTrip, FolderDiscoveryContinuesToDriveAdd, RawFolderLinkDriveAdd_NormalizesToCanonicalSharedDrive, FolderNameDriveAdd_HonorsAccountFilter, ReadOnlyFolder_DiscoveryDriveAddAndBlockedWriteUX (`e2e/shared_items_e2e_test.go`) |
| `SharedFolder_` | 3 | DriveList_ShowsExplicitSharedFixtures, RemoteMutationSyncsToRecipient, RecipientSyncTwice_Idempotent (`e2e/sync_shared_e2e_test.go`) |
| `Shortcut_` | 10 | ReadOnlyDownloadOnlyProjectsChildMount, ExplicitStandaloneSharedFolderRemainsConfiguredDrive, RestartIdempotentKeepsChildMountVisible, RenameReusesChildMountState, MoveReusesChildMountState, WritableUploadSyncsToSharedTarget, ReadOnlyBlockedUploadStatus, LocalRootCollisionSkipsChildButParentCompletes, WatchLocalUploadSyncsToSharedTarget, WatchRemoteWakeUpdatesChildRoot (`e2e/sync_shortcut_full_e2e_test.go`) |
| `Orchestrator_` | 8 | SimultaneousSync, Status, DriveIsolation, OneDriveFails, SelectiveDrive, WatchSimultaneous, WatchDriveIsolation, WatchPausedDrive (`e2e/orchestrator_e2e_test.go`) |

Total 58 + 33 + 44 = 135, plus the standalone preflight = 136 full-tagged tests.

### 1.5 Proposed end state

**Convention.** A full-tier test is named `TestE2E_<Family>_<Rest>`. `<Family>` is the text between `TestE2E_` and the next `_`. The family alone decides the bucket through one exact-match table in devtool. A name with no second `_`, or a family not in the table, is an error. No longest-prefix logic.

| Bucket | Families |
|---|---|
| `full-serial-sync` | `Sync`, `SyncFull` |
| `full-serial-watch-shared` | `SyncWatch`, `Shared`, `SharedFolder`, `Shortcut`, `Orchestrator` |
| `full-parallel-misc` | `DriveList`, `Status`, `Logout`, `Pause`, `Resume`, `CLI`, `Mv`, `Cp`, `InternalBaselineVerification`, and new: `SyncIsolated`, `SharedRead`, `FileOps`, `Edge`, `RecycleBin` |
| standalone preflight | `FixturePreflight` |

**Renames (20), each keeping its current bucket and `-parallel` value:**

| Today | New name |
|---|---|
| `TestE2E_Sync_QuietMode`, `_MtimeOnlyChange`, `_TransferWorkersConfig`, `_IncrementalDeltaToken`, `_DriveRemovePurgeResetsState` | `TestE2E_SyncIsolated_<same suffix>` |
| `TestE2E_Sync_MultiDriveReport` | `TestE2E_Orchestrator_MultiDriveReport` |
| `TestE2E_Shared_FileDiscoveryAndSelectorReadCommands`, `_FileDiscoveryRejectsDriveAdd`, `_JSON_RecipientListingUsesLiveAccountCatalog` | `TestE2E_SharedRead_<same suffix>` |
| `TestE2E_EdgeCases` | `TestE2E_Sync_EdgeCases` |
| `TestE2E_RoundTrip`, `_ErrorCases`, `_JSONOutput`, `_QuietFlag` | `TestE2E_FileOps_<same>` |
| `TestE2E_ZeroByteFileSync`, `_UnicodeFilenameRoundtrip`, `_InvalidFilenameRejection`, `_RapidFileChurn` | `TestE2E_Edge_<same>` |
| `TestE2E_RecycleBinRoundtrip`, `TestE2E_RecycleBinEmpty` | `TestE2E_RecycleBin_Roundtrip`, `TestE2E_RecycleBin_Empty` |

**Mechanism.**

1. `fullE2EBucketFamilies` — one `map[string]string` (family -> bucket name) replaces the three name slices.
2. `discoverTaggedE2ETests` becomes production code for `e2e-full`: discover full-tagged tests, map each through the table, build the per-bucket name list, then generate the same explicit `^(a|b|...)$` pattern as today. The pattern must stay an explicit name list, not a prefix regexp: the fast-tagged `TestE2E_Sync_UploadOnly`/`_DownloadOnly` compile into the full binary (the full tag set includes `e2e`) and a `^TestE2E_Sync_` pattern would re-run them in the serial bucket.
3. `e2e-full` fails before its first live call when a discovered test has an unknown family or no family, naming the test and file.
4. Build-tag recognition evaluates the constraint with `go/build/constraint` (satisfied by `{e2e, e2e_full}` plus any GOOS/GOARCH tag) instead of string equality. Any `e2e/*_test.go` whose constraint mentions `e2e_full` and is not selected is a failure. This closes the blind spot in 1.2.
5. Bucket runs go through the no-tests-matched guard.
6. Fast suite: an exact allow-list of the seven fast names; the unit test asserts the discovered `e2e`-only set equals it. Adding a per-PR live test then requires an explicit edit, which is the cost decision that deserves one.

**Fallback if the 20 renames are judged too much churn:** keep names, add a frozen 20-entry exceptions map (name -> bucket) and a unit test that the map's size cannot grow. This is a transitional layer; `CLAUDE.md` asks for end states, so the renames in the same PR are the recommendation.

### 1.6 Existing tests that pin this

- `TestRunVerifyE2EFullRunsPreflightsBeforeSuites` (`verify_runner_test.go:204-245`) pins the command sequence of `e2e-full`.
- `TestFullE2EExecutionManifestCoversTaggedTestsExactlyOnce` (`:610-636`) — the exactly-once check; keep its intent, rewrite its body.
- `TestFastE2EExecutionManifestKeepsOnlySmokeLiveTests` (`:638-669`), `TestFullE2EBucketsOwnDemotedFastTests` (`:671-712`) — delete (1.3).
- Quirk-rerun tests at `:326, 355, 391, 421, 468` and `TestRunVerifyE2EFullStopsAfterFullPreflightFailure` (`:511`) — unaffected, but their fake-runner patterns contain bucket name lists and must be regenerated.

### 1.7 Tests to write first

1. Family extraction table test: `TestE2E_Sync_X` -> `Sync`; `TestE2E_Status_LiveOverlay_ConfigTolerance` -> `Status`; `TestE2E_RoundTrip` -> error "no family"; a non-`TestE2E_` top-level test in a full-tagged file -> error.
2. Real-repo: every full-tagged test maps to exactly one bucket (rewritten exactly-once test).
3. Unknown family fails and the message names test and file.
4. Every family in the table matches at least one discovered test; every bucket is non-empty.
5. Constraint evaluation on temp files: `e2e_full && e2e`, `(e2e && e2e_full)`, `e2e && e2e_full && linux` are discovered; a file mentioning `e2e_full` that is not selected fails.
6. Fast-tag set equals the allow-list exactly; an extra fast-tagged test fails.
7. `runE2EFull` with a fake runner over a temp `e2e/` dir: an unknown family aborts before any `go test`; patterns are generated from discovery.
8. A bucket run printing "no tests to run" fails.

### 1.8 Docs to update

- `spec/design/system.md:333` (bucket paragraph): replace "explicit verifier-owned buckets" with the family table and the rule; `:336` unchanged; `:252-253` test ownership.
- `CLAUDE.md:226` (and therefore `AGENTS.md`/`GEMINI.md`, symlinks): state the naming convention. The four names it cites are fast-tier names and are not renamed (VERIFIED by grep).
- Citations of renamed tests (VERIFIED by grep; `findMissingCitedTests`, `internal/devtool/verify_governs.go:308-347`, enforces only `spec/design/*.md`):
  - `spec/design/cli.md:40, 599` (`TestE2E_RoundTrip`).
  - `spec/reference/graph-api-quirks.md:332, 471` (`TestE2E_RoundTrip`), `:498, 500` (`TestE2E_EdgeCases`).
  - `spec/reference/live-incidents.md`: `TestE2E_RoundTrip` at 28, 737, 744, 757, 1432, 1444, 1636, 1648, 1652, 1776, 1800, 1847, 1858, 1862, 1902, 1915, 1934, 1956, 2008, 2017, 2020; `TestE2E_Sync_IncrementalDeltaToken` 305, 320; `TestE2E_Sync_QuietMode` 449; `TestE2E_Sync_DriveRemovePurgeResetsState` 583, 598, 616; `TestE2E_Sync_TransferWorkersConfig` 947, 952, 961; `TestE2E_RapidFileChurn` 984; `TestE2E_InvalidFilenameRejection` 985; `TestE2E_ErrorCases`, `TestE2E_JSONOutput` 1316; `TestE2E_QuietFlag` 1317; `TestE2E_EdgeCases` 1754, 1764, 2145.
  - `internal/cli/status_test.go:2144` (comment naming `TestE2E_RoundTrip`); doc comments above the renamed functions in `e2e/`.
- Requirement: no requirement names the buckets and the manifest tests cite none. Add one line under R-6.10 ("every full-tier e2e test shall belong to exactly one execution bucket, derived from its name; an unassigned test shall fail verification") and cite it from the new tests and `system.md:5`.

### 1.9 Risks

- `live-incidents.md` is a historical log; rewriting names in it changes history text. Alternative: leave the log and add one mapping note at its top. Owner's call.
- Nightly claim keys are test names (`internal/devtool/nightly.go:657`); a rename orphans a claim keyed by the old name until the 24 h lease expires and it is pruned (`nightly.go:354-394`). Land when no claim is open, or release first.
- The quirk constants `websocketRestartTestName` and `fastDownloadTestName` (`verify.go:34, 36`) are not renamed. `verify_failed.go:156` only tests the `TestE2E_` prefix and is unaffected.
- Other planned work that edits `e2e/*_test.go` will conflict with the renames; sequence it.
- `-parallel` safety is now vetted per family, not per test: adding a test to a `parallel-misc` family runs it with `-parallel 5` with no further gate. State this in the convention text.

---

## 2. Repo-consistency checks written as Go in devtool

### 2.1 Inventory (VERIFIED)

Eight checks are registered in `repoRuleSteps()` (`internal/devtool/verify.go:582-593`); they run in `verify quick` (`verify_quick.go:105-107`) and in `public`/`default` (`verify.go:349-351`).

| # | Step name | File | Enforces | How | Tests |
|---|---|---|---|---|---|
| 1 | output boundaries | `verify_output_boundaries.go` (251 lines) | No `slog.Debug/Info/Warn/Error` (217), `os.Stdout`/`os.Stderr` (221), `log.Print*` (225) in production code outside allowed files (123-136); no `slog.String("error", x.Error())` (192-212) | `go/ast`, no types, name matching | `verify_output_boundaries_test.go:12, 40, 87` (no `Validates:` line) |
| 2 | test conventions | `verify_test_conventions.go` (156) | No `t.Fatal/Fatalf/Error/Errorf` (126); no `time.Sleep` in `_test.go` except `e2e/sleep_test_helpers_test.go` (16, 130) | `go/parser`, no types; receiver must be the identifier `t` | `verify_test_conventions_test.go:14, 41, 66, 91` (R-6.10.17) |
| 3 | README status | `verify_readme.go` (94) | `## Status` section exists (67, 80) and lacks four literal phrases (86-93) | string search | `verify_readme_test.go:14, 35, 52, 61, 76, 90` (R-6.10.1) |
| 4 | spec governs | `verify_governs.go` (463) | (a) every production `.go` under `internal`, `pkg`, `cmd` owned by exactly one design-doc GOVERNS pattern (153-216); (b) every backticked `Test/Benchmark/Fuzz` name in design docs exists (308-347); (c) every backticked path in `CLAUDE.md` exists (357-395) | `filepath.Glob`, regex, `go/parser` for test names | `verify_governs_test.go:49, 60, 76, 88, 104, 117, 134, 148, 162, 178, 200, 216, 230` (R-6.10.5, R-6.10.13) |
| 5 | layering | `verify_layering.go` (301) | A decl owned by a lower `LAYER:` doc must not reference a decl owned by a higher one (186-229) | `go/parser`, identifier-name matching, no types | `verify_layering_test.go:36, 54, 72, 90, 105` (R-6.10.20) |
| 6 | retired backlog ids | `verify_backlog_ids.go` (209) | Every `B-NNN` in `.go` is registered in `spec/reference/retired-backlog-ids.md`; unreferenced rows fail (64-79); `_BNNN_` in identifiers fails (193-200) | regex line scan | `verify_backlog_ids_test.go:36, 47, 60, 72, 84` (R-6.10.18) |
| 7 | mutex ownership docs | `verify_mutex_docs.go` (155) | Each mutex field/var has a trailing or preceding `//` comment (148-154) | regex line scan | `verify_mutex_docs_test.go:24, 40, 69, 87` (R-6.3.5) |
| 8 | requirement traceability | `verify_requirements.go` (340) | Every `Validates:`/`Implements:` id is declared (98-111); every `[verified]` requirement is referenced (113-165) | regex | `verify_requirements_test.go:31, 47, 61, 77, 94, 108, 120` (R-6.10.12) |

Other hand-written guards outside `repoRuleSteps`: lint version pin (`verify_lint_version.go`, tests `verify_lint_version_test.go:63, 77, 91, 107, 116`, R-6.10.2); no-tests-matched (`verify_test_match.go`, tests `:16, 28, 39`); tagged vet (`verify_e2e.go:413-432`); e2e-full compile check (`verify_e2e.go:343-395`); cleanup audit (`cleanup_audit_test.go:18, 90`); lint-config contract (`lint_config_contract_test.go:24`).

**Checks the brief expected that do not exist (VERIFIED by grep of `internal/devtool` and `cmd/devtool` for `ForTest` and `recover(`):** devtool has no check on `...ForTest` usage and none on panic-recovery sites. The only hit is the test helper `repoRootForTest` (`verify_backlog_ids_test.go:12`).

### 2.2 Could an enabled linter enforce it?

`.golangci.yml` enables depguard, forbidigo, gocritic, sloglint (line 62), testifylint, thelper, nolintlint (27-72); revive is not enabled. The pinned golangci-lint 2.11.3 embeds forbidigo v2.3.0 (`~/go/pkg/mod/github.com/golangci/golangci-lint/v2@v2.11.3/go.mod:35`).

| # | Check | Linter equivalent |
|---|---|---|
| 1 | output boundaries | **forbidigo** for the three ambient-output rules (2.3). The flattened-error-attr shape is an AST pattern no enabled linter expresses: keep that part. Alternative for the slog rule: `sloglint` option `no-global: all` (`~/go/pkg/mod/go-simpler.org/sloglint@v0.11.1/sloglint.go:30, 58-61`), currently enabled with no settings. |
| 2 | test conventions | **forbidigo**, both halves. The `time.Sleep` half is already enforced (2.4). |
| 3 | README status | None. Delete (2.4). |
| 4 | spec governs | None. Keep. |
| 5 | layering | depguard cannot see inside one package. After `internal/sync` is split, depguard rules (precedent `.golangci.yml:107-136`) replace it. Keep until then. |
| 6 | retired backlog ids | None (forbidigo matches identifiers, not comments). Delete (2.4). |
| 7 | mutex docs | None enabled. Keep (runner-up for low value: any comment satisfies it, `verify_mutex_docs.go:148-154`). |
| 8 | requirement traceability | None. Keep. |

### 2.3 The two best moves to forbidigo, with exact rule text

How forbidigo v2.3.0 matches with `analyze-types: true` (VERIFIED, `~/go/pkg/mod/github.com/ashanbrown/forbidigo/v2@v2.3.0/forbidigo/forbidigo.go:312-361`): for a selector `x.f`, if `x` has a recorded type the match text is `<pkgname>.<TypeName>.<f>` and the package text is the type's package path (319-323, 339-343); if `x` is a package name the match text is `<pkgname>.<f>` (335-337). So `t.Fatal` with `t *testing.T` matches as `testing.T.Fatal`, and `f.t.Errorf` (a field) matches as `testing.T.Errorf` through line 319. Config keys `pattern`, `pkg`, `msg` are confirmed at `~/go/pkg/mod/github.com/golangci/golangci-lint/v2@v2.11.3/pkg/config/linters_settings.go:474-478`.

Current forbidigo block (`.golangci.yml:138-144`) has two rules: raw `os.*` filesystem calls, and `^time\.Sleep$` with message "use explicit test synchronization instead of wall-clock sleep". Exclusions at 280-294 scope the `time.Sleep` rule to test files (`path-except: _test\.go`, 288-291).

**Move A — direct testing failures (replaces the `t.Fatal*` half of check 2):**

```yaml
        - pattern: ^testing\.(T|B|F|TB)\.(Fatal|Fatalf|Error|Errorf)$
          pkg: ^testing$
          msg: use testify assert or require instead of direct testing failures
```

This is stricter than the current check, which only sees a receiver literally named `t` (`verify_test_conventions.go:126`). VERIFIED by grep of every `.Fatal(`/`.Fatalf(`/`.Error(`/`.Errorf(` receiver in `*_test.go` and in non-test `.go`: the only calls the new rule adds are four `f.t.Errorf` calls in `internal/cli/json_contract_fake_graph_test.go:414, 446, 458, 463` (`t *testing.T` field at `:22`). They run inside an HTTP handler; replace with `assert.Failf(f.t, ...)`/`assert.NoError(f.t, err)` in the same PR. The fixture text at `internal/devtool/verify_test_conventions_test.go:25-26` is inside a string and disappears with the check.

**Move B — ambient output (replaces three of the four rules of check 1):**

```yaml
        - pattern: ^slog\.(Debug|Info|Warn|Error)$
          pkg: ^log/slog$
          msg: pass an injected *slog.Logger instead of the package-level slog logger
        - pattern: ^os\.(Stdout|Stderr)$
          pkg: ^os$
          msg: write through the CLI-owned output and status writers
        - pattern: ^log\.Print(f|ln)?$
          pkg: ^log$
          msg: use the CLI-owned logger
```

with exclusion rules, in the style of `.golangci.yml:280-294`, that reproduce `outputBoundaryFileAllowed` (`verify_output_boundaries.go:123-136`):

```yaml
      - path: (_test\.go|^cmd/devtool/|^internal/devtool/|^e2e/|^testutil/|^internal/cli/(root|format)\.go)
        linters: [forbidigo]
        text: (package-level slog logger|CLI-owned output and status writers|CLI-owned logger)
```

Two details the PR must handle:

- `.golangci.yml:292-294` excludes **all** forbidigo findings under `internal/(fsroot|synctree|localpath)/`. The devtool check does not exempt those directories, so add a `text:` filter for the raw-`os.*` message to that exclusion, otherwise Move B is weaker than today there.
- A method call on an injected logger matches as `slog.Logger.Error`, not `slog.Error` (forbidigo.go:339-343), so the slog rule does not hit `logger.Error(...)`.

After Move B, `verify_output_boundaries.go` keeps only `isFlattenedErrorAttr` (192-212); rename the step "flattened error attrs" or leave the step name.

Pin the new rules with a config-contract test next to `TestGolangciConfig_DeferErrcheckExclusionStaysScopedToTests` (`internal/devtool/lint_config_contract_test.go:24`, R-6.10.2): parse `.golangci.yml`, assert the four patterns and their exclusions exist.

SUSPECTED: that golangci-lint's `text:` exclusion matches forbidigo's `msg` verbatim for the new messages as it does for the existing one at 288-291. Settled by the PR's own `verify-lint` CI job.

### 2.4 The three lowest-value checks: delete

**(1) README status (`verify_readme.go`, six tests).**
- It rejects four literal phrases of an old roadmap wording: `"Phase "`, `"Phases "`, `"complete. Working CLI"`, `"rewrite in progress"` (`verify_readme.go:86-93`).
- History (VERIFIED, `git log`): check and test have one commit, `aa545b11` 2026-05-03 (#698). `README.md` has been edited since by #699, `2601169b`, #704, #779, #786, a full rewrite in `f9f0bd18` 2026-10-25 (#800), and #826, with no change to the check or its phrase list.
- No exemption list exists, so there is no exemption history.
- SUSPECTED that it never fired: local failures leave no trace in git. What history proves is that seven later README edits, including a rewrite, needed no interaction with it.
- Side effect to accept: it also pins the existence of a `## Status` heading (`README.md:9`).

**(2) Retired backlog ids (`verify_backlog_ids.go`, five tests, registry `spec/reference/retired-backlog-ids.md`).**
- The backlog it refers to is dissolved; the registry header forbids new ids (`spec/reference/retired-backlog-ids.md:14-18`), so the registry can only shrink.
- History (VERIFIED): created `40a9c963` 2026-08-30 (#724). The registry's only later change is `426edeea` 2026-10-25 (#788): one row edited and one removed, because an unrelated fix deleted test code citing those ids. The check's only recorded effect since creation is that maintenance edit on an unrelated PR.
- Deleting it removes R-6.10.18 (`non-functional.md:197`), the sentence in `system.md:421`, the registry file, the check and its tests.

**(3) The `time.Sleep` half of test conventions (`verify_test_conventions.go:16, 130`).**
- Exact duplicate: forbidigo already bans `time.Sleep` in test files (`.golangci.yml:143-144`, scoped by 288-291), and the single allowed call already carries `//nolint:forbidigo` with a reason (`e2e/sleep_test_helpers_test.go:11-14`; one commit, `a6f85836` #491).
- With Move A taking the other half, the whole of `verify_test_conventions.go` and `verify_test_conventions_test.go` is deleted. R-6.10.17 (`non-functional.md:196`) stays true, enforced by lint.

### 2.5 Tests to write first, docs, risks

- Tests first: the lint-config contract test for the new forbidigo rules and exclusions; a real-repo assertion that `repoRuleSteps()` no longer lists the removed steps (update the step-order assertions in `verify_runner_test.go` and `verify_quick_test.go`).
- Docs: `system.md:238` (output boundary), `:239-245` (test conventions), `:248` (the `public` profile lists "output-boundary check, test-convention check, README status check"), `:421` (retired ids); `non-functional.md:197` (delete R-6.10.18), `:196` (R-6.10.17 wording unchanged, evidence moves to lint), `:184` (R-6.10.5 already says "code-shape rules enforced by linting"); `CLAUDE.md:208, 217` unchanged in meaning.
- Risks: a lint rule is only as strong as its exclusions; a broad `//nolint:forbidigo` now silences what a devtool check could not be silenced on. `nolintlint` requires a specific linter and an explanation (`.golangci.yml:220-223`), which bounds that. Requirement traceability (check 8) fails if a `Validates: R-6.10.18` line survives the deletion; remove them with the tests.

---

## 3. DoD stages

### 3.1 What each stage does (VERIFIED, `internal/devtool/verify_dod.go`)

Constants (17-31): 20 recent merged PRs, `DefaultDODCITimeout = 30 * time.Minute`, manifest `.dod-pr-comments.json`, `prCIPollInterval = 10 * time.Second`. `--ci-timeout` help: "maximum time to wait for DoD CI checks" (`cmd/devtool/main.go:193`); `dodCITimeout` (292-297).

**`start`** (`runDODStart`, 157-184): `gh repo view`; fetch review threads of the 20 most recently updated merged PRs (440-481, paginated 560-589); merge into the manifest keeping manual classification (339-376); validate (378-410); then `runDODStartNightly` (189-199), which calls `checkNightly` with `--branch` or the current branch as owner. No waits, no timeouts.

**`pre-pr`** (`runDODPrePR`, 201-218):

```go
profileOpts := *opts
profileOpts.DOD = false
if err := runVerifyProfile(ctx, runner, &profileOpts); err != nil {
```

then `verifyBranchRebased` (720-739: `git fetch origin`, `git merge-base --is-ancestor origin/main HEAD`), then manifest load and validation. The profile is the positional argument, default `default` (`cmd/devtool/main.go:133-140`). `default` = public checks plus fast live E2E (`verify.go:249-253`). Public checks (`runPublicVerification`, `verify.go:334-373`): format, lint, eight repo rules, build with cross-builds (501-531), then

```
go test -race -timeout=20m -coverprofile=<file> ./...        (verify.go:533-565; unitTestTimeout = "20m", 569)
```

then coverage gate and e2e-full compile. No nightly-claim renewal happens here.

**`pre-merge`** (`runDODPreMerge`, 220-239): requires `--pr`; `requireNightlyClaimsInPR`; `waitForPRCI(..., dodCITimeout(opts))`; `applyManifestReviewThreads` (831-878); `requireNoUnresolvedPRThreads` (1090-1126).

`waitForPRCI` (741-767) uses one wall-clock deadline and fails on the first non-success:

```go
deadline := time.Now().Add(timeout)
...
case prCheckStateFail:
    return fmt.Errorf("dod pre-merge PR CI failed: %s", reason)
case prCheckStatePending:
    if time.Now().After(deadline) {
        return fmt.Errorf("dod pre-merge PR CI timed out after %s: %s", timeout, reason)
    }
```

`classifyPRChecks` (769-807): required check names `verify`, `integration`, `e2e` (770-774); not completed -> pending (781-782); `conclusion != "success"` -> fail with "<name> concluded <conclusion>" (784-786); missing -> pending (803-804). State comes from `gh pr view <n> --json state,mergedAt,mergeCommit,statusCheckRollup,url` (809-829); `prCheckRun.DetailsURL` is decoded (95-100) and never used.

**`post-merge`** (`runDODPostMerge`, 241-283): requires `--pr`, `--worktree`, `--branch`; `mergePR` (1139-1168, `gh pr merge --squash --delete-branch`, tolerating the multi-worktree and already-merged cases); `markNightlyClaimsFixed` (260); `cleanupMergedIncrement` (1209-1243); `waitForPostMergePushCI(..., dodCITimeout(opts))` (266-274; 1321-1356, same fixed deadline at 1331); `RunCleanupAudit` (275-281). `classifyPostMergeRuns` (1390-1414): no run -> pending; not completed -> pending; `success` -> pass; `skipped` -> fail; anything else, including `cancelled`, -> fail. The worktree and branch are already removed when the CI wait fails; a rerun of the stage is idempotent.

### 3.2 The three pains, proven

**(a) `pre-pr` and the 20-minute timeout.** VERIFIED: the timeout is the constant `"20m"` (`verify.go:569`); nothing in devtool or `ci.yml` refers to `GOMAXPROCS`, `NumCPU`, `GOFLAGS` or `-p` (grep empty). The comment explains the intent (`verify.go:553-556`): "A deadline with headroom fails a real hang, not a slow runner." Owner-reported: on the 2-core host the stage exceeded the timeout on 2026-11-04 with two exhaustive tests still running. So the constant fails a slow machine, against its own stated intent.

What the docs promise for `pre-pr` (VERIFIED): `CLAUDE.md:346` says to run `verify --dod --stage pre-pr` with no profile, i.e. `default`; DoD items 1-6 (`CLAUDE.md:338-343`) are each "reported by `go run ./cmd/devtool verify default`", item 4 "(the full tier, race detector on; PR CI runs only the `-short` tier ...)"; R-6.10.22 (`non-functional.md:200`): "`verify default` shall remain the local gate". `CLAUDE.md:224` accepts that "a data race, a coverage drop or an exhaustive-tier failure can reach `main` and is caught by the nightly run" for PR CI only.

**(b) A cancelled shard fails the stage.** VERIFIED: the aggregate job treats every result other than `success`/`skipped` as failure (`.github/workflows/ci.yml:199-204`):

```bash
*) echo "::error::a verify job did not succeed (${result})"; exit 1 ;;
```

so a `cancelled` `verify-unit` leg makes `verify` conclude `failure`, and `classifyPRChecks` (784-786) fails with "verify concluded failure". It never inspects the legs, so it cannot tell cancelled from failed. Likewise a queue-cancelled `integration` or `e2e` (concurrency groups `live-account-integration`/`live-account-e2e`, `ci.yml:250-251, 357-358`; comment 241-245: "a third arrival cancels the older pending one, which is rerun rather than raced") fails with "<name> concluded cancelled". Owner-reported remedy: `gh run rerun <id> --failed` by hand, then the stage again.

**(c) Fixed deadline shorter than a queued job.** VERIFIED: default wait 30 min (`verify_dod.go:20`), measured from the stage start (744), while `e2e` has `timeout-minutes: 90` (`ci.yml:355`), `integration` 45 (248), and both can sit queued behind another PR's run of up to the same length.

No test drives either wait loop, the deadline, or a `cancelled` conclusion: `TestDODPRCIClassification` (`verify_dod_test.go:328-347`) has two cases; `TestDODPostMergeCIClassificationRejectsSkippedPushRun` (350-373) one.

### 3.3 Proposed end state

**(b) Re-run cancelled work, bounded.**

1. `classifyPRChecks` returns a fourth state, `cancelled-candidate`, for a required check that concluded `cancelled`, and for `verify` concluded `failure`. It carries the check's `DetailsURL`.
2. For a candidate, parse the run id from `DetailsURL` (`.../actions/runs/<run_id>/...`) and load `gh run view <run_id> --json status,conclusion,headSha,attempt,jobs` (the job-list shape is already decoded in `internal/devtool/nightly.go:619-666`; share the type).
3. Decide from job conclusions, not check names: if any job other than the aggregate `verify` concluded `failure` or `timed_out` -> real failure, fail fast as today but name the job. Otherwise, if at least one job concluded `cancelled` -> rerunnable.
4. Guard: only when the run's `headSha` equals the PR head (`gh pr view --json headRefOid`); a superseded run is ignored.
5. Wait until the run's `status` is `completed` (a run cannot be re-run earlier), then `gh run rerun <run_id> --failed`; print one line saying which jobs were cancelled and that attempt N was requested.
6. At most 2 reruns per run id (constant, with a flag `--ci-max-reruns`). On exhaustion fail with "cancelled 3 times" and the run URL.
7. Same for `waitForPostMergePushCI`: add `DatabaseID` to `githubRun` (114-121; the JSON already requests `databaseId`, 1358-1383) and rerun a `cancelled` push run under the same bound.

**(c) Deadline on no progress.**

1. Each poll computes a fingerprint: sorted `name|status|conclusion` of every check in the rollup, plus the run attempt.
2. The timer restarts on every fingerprint change. The stage fails only when the fingerprint has not changed for the stall window.
3. Stall window default = longest `timeout-minutes` in `.github/workflows/ci.yml` plus 10 minutes (90 + 10 today), read from the workflow the same way the lint pin is (`verify_lint_version.go:13-17`), so there is no second copy of the number. Reason: one state cannot legitimately persist longer than the longest job ahead of it; the concurrency groups keep only one pending run (`ci.yml:241-245`), and a displaced pending run turns `cancelled`, which (b) handles.
4. `--ci-timeout` stays and now means the stall window; help text "maximum time without any change in CI check state".
5. There is no wall-clock cap. Total wait is still bounded: each check has at most three transitions per attempt and attempts are bounded by (b).
6. Clock and sleeper are injected into both wait functions (today they call `time.Now` and `time.NewTimer` directly, 744, 758, 1331). Precedent for clock injection: `checkNightly(..., time.Now, stdout)` (`verify_dod.go:198`).

**(a) `pre-pr`.** Two parts, separable:

1. *Make the promise achievable (no doc change).* `unitTestTimeout` becomes a function: `20m x max(1, ceil(4 / runtime.GOMAXPROCS(0)))` (40 m on 2 cores, unchanged on 4 or more), plus an explicit `--unit-timeout <duration>` flag that wins. This matches the code comment's intent and keeps `CLAUDE.md:338-346` and R-6.10.22 true. SUSPECTED: the reference of 4 cores and the factor. Settle with one timed `go test -race -timeout=0 ./internal/sync` on the idle host (the owner's call; not run here) and by logging `runtime.NumCPU()` in one CI run.
2. *Change the promise (owner decision).* Add a `pr` profile that runs exactly what PR CI requires locally (lint, then the quick lane) and make it the `pre-pr` default; `verify default` stays available. This matches the owner-reported practice ("use lint plus `verify quick` locally and let the PR's `pre-merge` stage ... be the gate") and the trade-off already accepted at `CLAUDE.md:224`, with nightly failures owned through R-6.10.23. It requires rewording `CLAUDE.md:338-346`, `system.md:254`, R-6.10.16 and R-6.10.22 in the same PR, and restructuring `runPRLaneVerification` (`verify_quick.go:92-125`), whose `switch` plus `if plan.runLint { return nil }` cannot run lint and quick in one plan today (VERIFIED).

Recommendation: do part 1 regardless. Part 2 is the one that removes the 2-core bottleneck for parallel increments, but it moves race, coverage and exhaustive failures from "before PR" to "next nightly"; that is a policy change only the owner can make.

Also: `pre-pr` and `pre-merge` should renew the increment's nightly claims (section 5, gap 7).

### 3.4 Existing tests

All `// Validates: R-6.10.16` in `internal/devtool/verify_dod_test.go`: manifest tests (18, 77, 136, 167), reply templates (189, 206), thread mutations (219, 254, 303), `TestDODPRCIClassification` (328), `TestDODPostMergeCIClassificationRejectsSkippedPushRun` (350), merge-wrapper tests (376, 383, 390, 418), branch cleanup (439, 459). None covers `runDODPrePR`, the wait loops or reruns.

### 3.5 Tests to write first

With a fake runner returning scripted `gh pr view` / `gh run view` JSON and a fake clock:

1. Required check queued longer than the old 30 min, then success -> stage passes.
2. Fingerprint unchanged for the stall window -> fails, message names the unchanged checks.
3. `verify` failure with one `cancelled` leg and no failed leg -> exactly one `gh run rerun <id> --failed`, then pass.
4. `verify` failure with one failed leg and one cancelled leg -> fails at once, no rerun, message names the failed job.
5. `e2e` concluded `cancelled` -> rerun; third cancellation -> fails with the run URL.
6. Run `headSha` differs from PR head -> no rerun.
7. Rerun is not issued while the run is still `in_progress`.
8. Post-merge: cancelled push run on `main` is rerun; `skipped` still fails.
9. `TestDODPRCIClassification` extended with `cancelled`, `timed_out`, `skipped` cases.
10. Stall window derived from a temp `ci.yml` (max `timeout-minutes` + 10 m); explicit `--ci-timeout` wins.
11. Unit timeout: `GOMAXPROCS` 1, 2, 4, 8 -> 80 m, 40 m, 20 m, 20 m; `--unit-timeout` wins; the `go test` argv contains the value.
12. If part 2 is taken: `pre-pr` with no profile runs lint then quick, in that order, and still enforces the rebase and manifest gates.

### 3.6 Docs

`system.md:254` (stage paragraph: rerun rule, stall window, where the number comes from), `:248` (profiles, if `pr` is added); R-6.10.16 (`non-functional.md:195`: add "re-runs cancelled CI jobs a bounded number of times and waits while CI makes progress"); R-6.10.22 (`:200`) only under part 2; `CLAUDE.md:346-348` (items 9-11); flag help in `cmd/devtool/main.go:193`.

### 3.7 Risks and open points

- SUSPECTED: `DetailsURL` of an Actions check has the form `.../actions/runs/<run_id>/job/<job_id>`. Settle by printing one `statusCheckRollup`.
- SUSPECTED: after a rerun the rollup shows only the latest attempt per check name. Settle by inspecting a PR that has had a rerun. If older attempts remain listed, classify by the newest entry per name.
- SUSPECTED: a job killed by `timeout-minutes` reports `cancelled`. If so, a genuinely hanging test is re-run twice before the stage fails (bounded cost, at most two extra job budgets). Settle with `gh run view <id> --json jobs,conclusion` on a timed-out run; if confirmed, treat a cancelled job whose duration reached its timeout as a failure.
- Owner-reported, not re-verified: `gh run rerun <id> --failed` re-runs cancelled jobs (it is the manual remedy in use).
- The host's `gh` lacks `pr checks --json` (owner-reported); the design uses only `gh pr view --json`, `gh run view --json` and `gh run rerun`, which devtool already uses or the owner already uses.
- A rerun consumes the shared live-account concurrency slot; with two PRs open, reruns can displace each other. The bound of 2 keeps this finite.

---

## 4. devtool imports the sync store

### 4.1 Current behaviour (VERIFIED)

The only product import in `internal/devtool` is `internal/devtool/bench_live_bidirectional.go:18`:

```go
syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
```

used only in `readObservationCursor` (753-775):

```go
store, err := syncengine.NewSyncStore(ctx, runtime.stateDBPath, logger)
...
state, err := store.ReadObservationState(ctx)
...
return state.Cursor, nil
```

Purpose: the `sync-bidirectional-catchup-100m` benchmark reads the delta cursor after the baseline sync (504-507) and after each successful `sync --dry-run` readiness probe (`waitForBenchRemoteDeltaReady`, 871-923), and aborts if a probe moved it (893-895: "dry-run probe advanced observation cursor"). Seam: interface `benchObservationCursorReader` (95-97), field `cursorReader` (79), production value at 152, test double `benchStaticCursorReader` (`bench_live_bidirectional_test.go:14-22`), test `TestWaitForBenchRemoteDeltaReadyFailsIfDryRunAdvancesCursor` (`:220-242`, R-6.10.14). DB path: `<dataDir>/state_<driveID>.db` (727-729).

### 4.2 Cost

- devtool links the whole sync package and the SQLite driver (`go.mod:23`). When `internal/sync` is split, the verification tool must be edited with it.
- `NewSyncStore` opens with schema ensure (`internal/sync/store.go:84-144`) and `validateStoreGeneration` rejects any generation other than the linked one (`internal/sync/schema.go:433-448`; `currentSyncStoreGeneration = 28`, `:72`). The benchmark claims older builds are "still measured when comparing against earlier revisions" (`bench_live_bidirectional.go:936-939`), but a subject binary with a different store generation makes the cursor read fail.
- `ReadObservationState` first executes an INSERT (`internal/sync/store_observation_state.go:134-137`): the measuring harness writes to the measured subject's database.

### 4.3 Proposed end state: delete the cursor read

The invariant "a dry run never commits the observation cursor" is already pinned in the product's own tests, for both engine kinds (VERIFIED):

- `TestRunOnce_DryRun_NoExecution` (`internal/sync/engine_run_once_test.go:433-471`, R-2.1.5), ending

  ```go
  savedToken := readObservationCursorForTest(t, eng.baseline, ctx, eng.driveID.String())
  assert.Empty(t, savedToken, "dry-run should not save delta token")      // 468-470
  ```

- `TestRunOnce_DryRun_MountRootDoesNotSaveScopedDeltaToken` (`:705-769`, assertion 767-768).

The benchmark keeps its black-box guards, which do not need the store: readiness is judged from the dry-run plan output (`parseBenchSyncPlanOutput` 940 ff., `benchRemoteDeltaPlanReady` 925-934); the baseline slot is validated by a zero-action dry-run plan (601-635); the measured `sync` is followed by `verifyBenchLiveFixture` (553-559), which invalidates the sample when the expected end state is not on disk.

Change list:

1. Delete `benchObservationCursorReader`, `benchSyncStoreObservationCursorReader`, the `cursorReader` field, the `baselineCursor` plumbing, the import at line 18, `benchStaticCursorReader` and `TestWaitForBenchRemoteDeltaReadyFailsIfDryRunAdvancesCursor`.
2. Add a depguard rule so the dependency cannot return:

   ```yaml
       devtool-no-product:
         files: ["**/internal/devtool/**", "**/cmd/devtool/**"]
         deny:
           - pkg: github.com/tonimelisma/onedrive-go/internal/
             desc: the verification tool must not link product packages
   ```

   with an allow for `github.com/tonimelisma/onedrive-go/internal/devtool` itself (depguard `allow` list or a narrower set of deny entries; follow the form of the existing rules at `.golangci.yml:80-136`).
3. Pin the rule with a config-contract test (`lint_config_contract_test.go` style).

Rejected alternatives: (A) a machine-readable CLI surface for the cursor — a new product surface, absent from older subject binaries, for one benchmark guard; (C) `database/sql` with `SELECT cursor FROM observation_state` — keeps the SQLite link and a string-coupled schema dependency.

### 4.4 Tests to write first

- Unit test with a fake subject: a measured sync that leaves expected files missing marks the sample invalid (pins the guard that replaces the cursor check).
- The depguard contract test.

SUSPECTED: `verifyBenchLiveFixture` catches every case the cursor comparison caught (a probe that consumed the delta would make the measured sync a no-op, leaving files missing). Settle with the first test above using a fake subject whose dry run "consumes" the delta.

### 4.5 Docs and risks

- `system.md:258-264` (benchmark entrypoint) and the R-6.10.14 evidence (`non-functional.md:193`). SUSPECTED that another design doc mentions the cursor guard; settle with a grep of `spec/design` for "observation cursor".
- Risk: the benchmark loses a direct diagnostic ("probe advanced cursor") and reports the same condition as an invalid sample. Acceptable for a harness; the product invariant stays tested where it belongs.
- Ordering: land before the `internal/sync` split so the split does not have to touch devtool.

---

## 5. `devtool nightly check` / `nightly release`

### 5.1 How it works (VERIFIED, `internal/devtool/nightly.go`, 726 lines; CLI at `cmd/devtool/main.go:55-64`)

- **Last nightly run** (`loadLatestNightlyRun`, 589-614): `gh run list --workflow ci.yml --event schedule --branch main --status completed --limit 10 --json databaseId,conclusion,headSha,createdAt,url`, newest first, first whose conclusion is exactly `success` or `failure`. Comment 587-588: "Canceled and in-flight runs say nothing about the tree". None found -> prints "nightly: no finished scheduled run found; nothing to claim" and succeeds (151-153).
- **Failing tests** (`loadNightlyFailureKeys`, 619-666): `gh run view <id> --json jobs`; only jobs with `Conclusion == "failure"` (633-638); the aggregate `verify` job is skipped when more than one job failed (641-643); per job, the log from `gh api repos/{owner}/{repo}/actions/jobs/<id>/logs`; `failedTopLevelTests` (668-690) takes names from go-test JSON `fail` events and `--- FAIL:` lines, cut at `/`, starting with `Test`. A job whose log names no test yields `job-<sanitized job name>` (652-655; `sanitizeNightlyKey` 693-704).
- **Claim** (`resolveNightlyKey` 228-273, `decideNightlyKey` 275-322): ref `refs/nightly-claims/<key>` (23), value an empty-tree commit whose message is a JSON record (59-68: `state, key, owner, host, run_id, run_head, at, fix_commit`). Compare-and-swap by `git push --porcelain --force-with-lease=<ref>:<expectSHA>` (`swapNightlyClaim`, 552-585), three attempts (`nightlyCASAttempts`). Outcomes: unowned -> take; own -> renew; another owner's unexpired -> `held`; expired -> take over; `fixed` and the fix commit is not in the failing run -> `awaiting`; `fixed` and the fix is in the failing run -> reopen and take.
- **Lease**: `NightlyClaimLease = 24 * time.Hour` (39); `nightlyClaimExpired` (324-330), an unparsable time counts as expired.
- **Removal of a claim when the test passes again** (`pruneNightlyClaims`, 354-394), at the end of every `checkNightly`: for a key not failing in the latest verdict run, a `fixed` claim is deleted when its fix commit is an ancestor of that run's head; a `claimed` claim only once expired. So the answer to "who removes it" is: the next increment that runs `nightly check` (normally through `--stage start`) after the passing nightly. Nothing in CI prunes.
- **Release** (`releaseNightlyClaim`, 405-428): needs `--branch` and `--key`; the claim must be `claimed` and owned by that branch; deletes the ref.
- **Pre-merge gate** (`requireNightlyClaimsInPR`, 448-483): every `claimed` key held by the PR's `headRefName` must appear in the PR body by `strings.Contains` (468).
- **Post-merge** (`markNightlyClaimsFixed`, 488-511): swaps each held claim to `fixed` with the merge commit.
- **No-test-name cases**: lint, build, coverage gate — the job concluded `failure`, its log has no `--- FAIL:`, so the key is `job-<name>`. A cancelled job produces no key at all (gap 2).

Tests (all R-6.10.23, `internal/devtool/nightly_test.go`): 203, 222, 238, 258, 280, 299, 317, 332, 349, 362, 397, 416, 433.

### 5.2 Provable gaps (VERIFIED unless marked)

1. **A failed run with no `failure` job passes the check.** Keys come only from jobs with conclusion `failure` (633-638). A run that concluded `failure` because of `cancelled`/`timed_out` legs yields zero keys; `checkNightly` (156-161) has no empty-keys guard and returns success. The failed nightly has no owner.
2. **Cancelled legs surface as an unfixable key.** When a cancelled `verify-full` leg fails the aggregate (`ci.yml:199-204`), the only `failure` job is `verify`; its log names no test; the key is `job-verify`. No code change fixes it; released, the next increment claims it again (427), until a nightly passes.
3. **A run of non-verdict nightlies is invisible.** Runs concluding anything but `success`/`failure` are skipped (609); ten in a row give "no finished scheduled run" and success (151-153). SUSPECTED whether a `timeout-minutes` kill produces run conclusion `cancelled`; settle with `gh run view` on such a run.
4. **A job key is emitted only when the log names no test** (652-658). A build failure, test-binary timeout or coverage-gate failure in a job that also has one `--- FAIL:` line gets no key until that test is fixed.
5. **Keys are bare top-level test names** (657). Same-named tests in two packages, or the same test failing on two platforms, share one claim.
6. **Any `--- FAIL:` line is claimed**, including a test that failed and then passed on a classified live-quirk rerun in the same job. SUSPECTED; settle by reading one nightly `e2e` log where a rerun passed and another test failed, against `verify_e2e.go:743-789`.
7. **The lease is renewed only by `checkNightly`** (`--stage start` or a manual `nightly check`). `pre-pr` and `pre-merge` do not renew (`verify_dod.go:201-239`). An increment older than 24 h can lose its claim to another increment's check (313-321); its own `pre-merge` gate then sees no held claim and `post-merge` records no `fixed` state.
8. **Substring match in the PR-body gate** (468): key `TestFoo` is satisfied by a body that names `TestFooBar`.
9. **Three sources for the owner**: `--branch` or current branch at `start` (`verify_dod.go:190-197`), the PR's `headRefName` at `pre-merge` (462), `--branch` at `post-merge` (`verify_dod.go:260`). Nothing checks they agree.
10. **Renaming a test orphans its claim** until expiry and prune (657, 354-394). Relevant to section 1.
11. Cosmetic: each renewal or prune creates one unreachable commit locally (567).

### 5.3 Proposed end state

- Gaps 1-3: treat every non-success job as a finding. `failure` -> test keys or job key as today. `cancelled`/`timed_out` -> `nightly check` re-runs the run once (`gh run rerun <id> --failed`, only when `attempt == 1`, sharing the helper from section 3) and reports "no verdict yet, rerun requested"; if the rerun is cancelled again, key `job-<name>` for the cancelled job (not for the aggregate). Skip the aggregate whenever any other job is non-success, not only when another job is `failure`. A `failure` run that still yields zero keys gets the key `run-<id>-unparsed` and must be owned. Two consecutive non-verdict runs yield the key `nightly-no-verdict`.
- Gap 4: emit the job key in addition to test keys when the log shows a non-test failure. This needs a stable marker; none was found (`internal/devtool/verify_summary.go` contains no "failed"/"FAIL" text, VERIFIED by grep). Add one line printed by `runVerifyProfile` on step failure, e.g. `devtool: step "<name>" failed`, and key `step-<job>-<step>` for failed steps other than the test steps.
- Gap 5: add the package to the key when the log provides it (`FAIL\t<pkg>` line following the `--- FAIL:` lines, or the JSON event's package): `<pkg-last-elements>.<TestName>`. Changes the key format once; open claims lapse within 24 h.
- Gap 7: `pre-pr` and `pre-merge` call a `renewNightlyClaims(owner)` that re-swaps the owner's `claimed` records with a new `at`.
- Gap 8: match the key with non-identifier boundaries: `(^|[^A-Za-z0-9_])<key>($|[^A-Za-z0-9_])`.
- Gap 9: `post-merge` takes the owner from the PR's `headRefName` and fails if `--branch` differs; `pre-merge` does the same when `--branch` is given.

### 5.4 Tests to write first

One per gap, in `nightly_test.go` with the existing fake runner: failed run with only cancelled jobs (rerun requested once, then job key); aggregate skipped when a leg is cancelled; ten cancelled runs -> `nightly-no-verdict`; job with a failed test and a failed step yields both keys; same test name in two packages yields two keys; `pre-pr` renews a 23-hour-old claim; `TestFoo` not satisfied by `TestFooBar`; `post-merge` with a `--branch` that differs from the PR head fails.

### 5.5 Docs and risks

- `system.md:255` (key derivation, renewal points, rerun), R-6.10.23 (`non-functional.md:202`), `CLAUDE.md:248` (Step 1.5) and `:347`.
- Risk: auto-rerun of the nightly from a developer machine spends CI minutes and the shared live-account slots; bound it to one rerun per run id and print it.
- Risk: changing the key format while claims are open; do it when `git ls-remote origin 'refs/nightly-claims/*'` is empty.

---

## 6. `devtool worktree add`, `.worktreeinclude`, and the Go toolchain

### 6.1 Worktrees (VERIFIED, `internal/devtool/worktree.go`, 221 lines; CLI `cmd/devtool/main.go:252-353`)

- `AddWorktree` (41-76): `git worktree add -b <branch> <path> origin/main` (49-62), then `BootstrapWorktree`; on bootstrap failure `cleanupFailedWorktree` (78-109: `git worktree remove --force`, `git branch -D`).
- It does not fetch. `CLAUDE.md:282` makes `git fetch origin` a manual step before it.
- `.worktreeinclude`: line 1 `@.testdata` (symlink), line 2 `.env` (copy). `readIncludeEntries` (146-175): blank and `#` lines skipped, `@` = symlink. `applyIncludeEntry` (179-221): missing source skipped with a note (116), a directory without `@` is an error (208).
- Tests (R-6.10.21, `worktree_test.go`): 14, 38, 57, 74.

Gaps:

1. **Cleanup does not derive from `.worktreeinclude`.** `removeDODWorktree` (`verify_dod.go:1245-1264`) hard-codes `.testdata`, uses `os.Remove` (works for a symlink or an empty directory) and then `git worktree remove` without `--force`:

   ```go
   if err := os.Remove(filepath.Join(worktreePath, ".testdata")); err != nil && !errors.Is(err, os.ErrNotExist) {
       return fmt.Errorf("dod cleanup remove worktree .testdata: %w", err)
   ```

   A copied (non-symlink, non-empty) `.testdata` in a worktree makes `post-merge` fail here. This matches the owner-reported note "the cleanup cannot remove a worktree holding it". End state: read the root's `.worktreeinclude`, remove each `@` target only if it is a symlink and each copied file, refuse with a clear message if an `@` target is a real directory (never `RemoveAll` a credentials bundle), then `git worktree remove`.
   SUSPECTED: an ignored copied `.env` does not block `git worktree remove` (ignored files are not "untracked" for that check). Settle with one trial in a scratch repository.
2. **`worktree add` should fetch.** Run `git fetch origin main` inside `AddWorktree` before `git worktree add`, so R-6.10.21 ("from `origin/main`") holds without a manual step. Test: fake runner asserts the fetch precedes the add.

### 6.2 Toolchain (VERIFIED)

- Nothing in the repository mentions `GOTOOLCHAIN` (grep over `.go`, `.yml`, `.yaml`, `.md`, `.mod`, `.sh`, `Makefile` is empty). So: **no, devtool does not pin it** for anything it execs.
- `go.mod`: `go 1.26.0` (3), `toolchain go1.26.10` (10). CI installs Go from `go.mod` (`.github/workflows/ci.yml:57-59` and the other `setup-go` steps).
- devtool passes its environment through unchanged: `env := os.Environ()` once at `internal/devtool/verify.go:223`; `ExecRunner` sets `cmd.Env = env` (`internal/devtool/runner.go:34, 55, 83`). `runLint` (`verify.go:475-487`) execs `golangci-lint run --allow-parallel-runners` with that env.
- The lint pin check (`internal/devtool/verify_lint_version.go`, 86 lines) compares only the linter's `x.y.z` against the workflow pin (17, 20, 31-65). It ignores the Go version the linter was built with, although `--version` reports it (fixtures: `"golangci-lint has version v2.11.3 built with go1.26.6\n"`, `verify_lint_version_test.go:81`). On mismatch it advises (57-61):

  ```
  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v%s
  ```

  with no toolchain pin. `go install pkg@version` ignores the current module's `go.mod`, so on a host whose default Go is newer this advice builds the linter with the newer Go.

What breaks when the default `go` is newer than the `go.mod` toolchain:

- Owner-reported (not reproduced): golangci-lint 2.11.3 built with Go 1.27 fails every typecheck ("export data version 4 is greater than maximum supported version 2"); built with 1.26.10 but run without `GOTOOLCHAIN=go1.26.10` it loads the 1.27 standard library and panics ("file requires newer Go version go1.27").
- By the Go toolchain rules (not reproduced): with `GOTOOLCHAIN` unset, a newer local `go` builds and tests the module itself, so local `verify` runs on 1.27 while CI runs on 1.26.10. A 1.27-only standard-library call compiles locally and fails in CI.

### 6.3 Proposed end state: devtool sets the toolchain from `go.mod`

1. `goModToolchain(repoRoot)`: returns the `toolchain` directive, or `go<go directive>` when absent.
2. At `verify.go:223`, when `GOTOOLCHAIN` is not already in the environment, append `GOTOOLCHAIN=<value>`. Every child (`go build`, `go test`, `go vet`, `golangci-lint`, which shells out to `go list`) then uses the module's toolchain, identical to CI. A value already set by the caller (for example `local`) is left alone.
3. `assertLintVersionMatchesCI`: parse `built with go(\d+\.\d+)` and fail when its major.minor differs from the module toolchain's. Change the advice to

   ```
   GOTOOLCHAIN=<toolchain> go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v<pin>
   ```

   When the `built with` part is absent (fixtures at `verify_lint_version_test.go:95, 110`), keep today's behaviour.

This makes the owner's workaround (`GOTOOLCHAIN=go1.26.10 go run ./cmd/devtool verify lint`) the tool's own behaviour, and removes the local/CI toolchain split.

Tests first (fake runner, temp `go.mod`):

- The env of the `golangci-lint run` call and of the `go test`/`go build` calls contains `GOTOOLCHAIN=go1.26.10`.
- A pre-set `GOTOOLCHAIN` is untouched.
- Linter built with go1.27 against toolchain go1.26.10 fails, and the message contains the pinned install command.
- Real-repo: `goModToolchain` parses the checked-in `go.mod`.

Docs: `system.md:374-382` ("The lint version is checked, not just pinned"); R-6.10.2 (`non-functional.md:181`: add the toolchain); `CLAUDE.md` tooling text if it names the install command.

Risks: the first run on a machine without the toolchain cached downloads it (network); `go run ./cmd/devtool` itself is still built by the host default Go, which is harmless; a bug in the `go.mod` parse must fail closed (no silent fallback to the host toolchain).

---

## 7. Dependency-ordered PR breakdown

| PR | Content | Depends on | Why this position |
|---|---|---|---|
| 1 | Toolchain from `go.mod`: `GOTOOLCHAIN` in the verify env, built-with check, pinned install advice (6.3) | — | Independent; makes local lint work unaided for every later PR |
| 2 | Lint moves and deletions: forbidigo Moves A and B with exclusions and contract test; fix the four `f.t.Errorf` calls; delete `verify_test_conventions.go`, README status check, retired-backlog-ids check, registry and R-6.10.18 (2.3, 2.4) | 1 | Edits `.golangci.yml`; needs local lint |
| 3 | Remove devtool's `internal/sync` import; depguard `devtool-no-product` (4.3) | 2 (same file `.golangci.yml`) | Must land before the `internal/sync` split |
| 4 | E2E: 20 renames, family table, run-time discovery, constraint evaluation, fast allow-list, delete the two vacuous tests, new requirement line, citation updates (1.5-1.8) | — (coordinate with other `e2e/` work; no open nightly claims) | Changes nightly claim keys, so before PR 7 |
| 5 | DoD CI wait: injected clock, stall window from `ci.yml`, bounded rerun of cancelled jobs, for pre-merge and post-merge (3.3 b, c) | — | Provides the run/job classification helper PR 7 reuses |
| 6 | `pre-pr`: scaled unit timeout and `--unit-timeout`; optionally the `pr` profile with the doc and requirement rewording (owner decision); `.worktreeinclude`-derived cleanup; `worktree add` fetches (3.3 a, 6.1) | 5 | Stage semantics and `CLAUDE.md` DoD text change once |
| 7 | Nightly gaps: non-success job handling and rerun, step keys, package-qualified keys, renewal in `pre-pr`/`pre-merge`, boundary match, single owner source (5.3) | 4, 5, 6 | Keys depend on final test names; reuses the rerun helper; renewal hooks live in the stages PR 6 edits |
| 8 | After the `internal/sync` split: replace `verify_layering.go` with depguard rules; drop `LAYER:` handling and R-6.10.20's devtool evidence | the split | Only possible once layers are packages |

PRs 1, 4 and 5 have no dependency on each other and can run in parallel on separate worktrees; 2 -> 3 is a chain on `.golangci.yml`.

---

## 8. Open SUSPECTED items and what settles each

| Item | Settled by |
|---|---|
| `DetailsURL` form of an Actions check | Print one `gh pr view <n> --json statusCheckRollup` |
| Rollup shows only the latest attempt after a rerun | Inspect a PR that had a rerun |
| Job killed by `timeout-minutes` reports `cancelled`; run conclusion in that case | `gh run view <id> --json jobs,conclusion` on a timed-out run |
| CI runner core counts; right scale factor for `unitTestTimeout` | Log `runtime.NumCPU()` in one CI run; one timed full race run on the idle 2-core host |
| `text:` exclusions match the new forbidigo messages | The PR's `verify-lint` job |
| README status check never fired | Not provable from git; history shows no interaction |
| Live-quirk rerun leaves a `--- FAIL:` line that gets claimed | Read one nightly `e2e` log with a passed rerun |
| Ignored copied `.env` does not block `git worktree remove` | One trial in a scratch repository |
| `verifyBenchLiveFixture` covers every cursor-advance case | Unit test with a fake subject (4.4) |
| Another design doc mentions the benchmark cursor guard | `grep -rn "observation cursor" spec/design` |
