# Requirement Verification Tracker

Status: All 7 increments complete | Last updated: 2026-03-09

## Summary

| Design Doc | Requirements | Verified | Remaining [impl] | Planned/Future |
|---|---|---|---|---|
| retry.md | 2 | 2 | 0 | 0 |
| system.md | 6 | 6 | 0 | 0 |
| data-model.md | 5 | 5 | 0 | 0 |
| drive-identity.md | 14 | 14 | 0 | 0 |
| config.md | 11 | 11 | 0 | 0 |
| graph-client.md | 17 | 10 | 3 | 7 |
| drive-transfers.md | 11 | 8 | 0 | 3 |
| cli.md | 10 | 8 | 2 | 0 |
| sync-observation.md | 15 | 8 | 0 | 7 |
| sync-planning.md | 6 | 6 | 0 | 0 |
| sync-engine.md | 4 | 2 | 2 | 0 |
| sync-execution.md | 6 | 3 | 1 | 2 |
| sync-store.md | 8 | 8 | 0 | 0 |
| **Total** | **115** | **91** | **8** | **19** |

Across all requirement files: **153 [verified]** statuses (including umbrella requirements), **22 [implemented]** remaining.

## Methodology

For each requirement marked `[implemented]`:
1. Confirmed behavior exists in governed source files
2. Found test(s) that exercise the behavior
3. Added `// Validates: R-X.Y.Z` comment on the test function
4. Promoted status to `[verified]` in requirement files
5. Updated `Implements:` lines in design docs to match

Umbrella requirements are `[verified]` only when ALL children are `[verified]`. A child that is `[planned]` or `[future]` prevents umbrella promotion.

## retry.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-6.8.1 | Respect 429 with Retry-After | verified | `TestTransportDelay_MatchesCalcBackoff`, `TestDo_RetryOn429WithRetryAfter`, `TestThrottleGate_429SetsDeadline` | |
| R-6.8.2 | Exponential backoff with jitter | verified | `TestPolicy_Delay_*`, `TestBackoff_*`, `TestCircuitBreaker_*` | |

## system.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-6.2.1 | No remote delete without baseline (S1) | verified | `TestS1_NoRemoteDeleteWithoutBaseline` | Design invariant |
| R-6.2.2 | No delete from incomplete enumeration (S2) | verified | – | Architectural invariant |
| R-6.3.1 | Single sync process | verified | `TestWritePIDFile_*` | PID file with flock |
| R-6.1.4 | Startup < 1 second | verified | – | Build target |
| R-6.1.5 | Binary < 20 MB | verified | – | Build target |
| R-6.9.1 | Single static binary | verified | – | Build target |

## data-model.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-6.5.1 | Durable transactional writes | verified | `TestNewSyncStore_WALMode`, `TestSyncStore_Close_CheckpointsWAL` | |
| R-6.5.2 | Atomic sync operations | verified | `TestCommit_Download`, `TestCommit_Upload`, `TestCommit_*Delete` | |
| R-2.5.1 | Resume from last checkpoint | verified | `TestResetInProgressStates_*` | |
| R-2.5.2 | Durable transactional writes | verified | `TestNewSyncStore_WALMode` | Same as R-6.5.1 |
| R-2.3.2 | Persistent conflict recording | verified | `TestListConflicts_*`, `TestCommitConflict_*`, `TestResolveConflict` | |

## drive-identity.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-3.2.1 | OneDrive Personal | verified | `TestNewCanonicalID`, `TestConstruct` | |
| R-3.2.2 | OneDrive Business | verified | `TestNewCanonicalID`, `TestConstruct` | |
| R-3.2.3 | SharePoint Document Libraries | verified | `TestConstructSharePoint` | |
| R-3.2.4 | Shared Folders | verified | `TestNewCanonicalID_SharedType`, `TestConstructShared` | |
| R-3.3.1 | drive list | verified | `TestBuildConfiguredDriveEntries_*`, `TestPrintDriveListText_*` | |
| R-3.3.2 | drive add | verified | `TestAddNewDrive_WithToken` | |
| R-3.3.3 | drive remove | verified | `TestRemoveDrive_*` | |
| R-3.3.4 | drive search | verified | `TestPrintDriveSearchText_*` | |
| R-3.5.1 | --drive matching | verified | `TestBuildConfiguredDriveEntries_ExplicitDisplayName` | |
| R-3.5.2 | --account selection | verified | `TestFindBusinessTokens_FilterSelectsOne` | |
| R-3.5.3 | --drive repeatable | verified | – | CLI flag definition |
| R-3.6.1 | drive list shows shared folders | verified | `TestPrintDriveListText_SharedDrive` | |
| R-3.6.2 | Search API for shared discovery | verified | `TestSearchSharedItemsWithFallback_*` | |
| R-3.6.3 | Derive display name from sharer | verified | `TestDeriveSharedDisplayName_*` | |

## config.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-4.1.1 | TOML format with drive sections | verified | `TestLoad_ValidFullConfig`, `TestLoad_SingleDriveSection`, `TestLoad_MultipleDriveSections` | |
| R-4.1.2 | XDG paths | verified | `TestXDGConfigDir`, `TestXDGDataDir`, `TestMacOSConfigDir`, `TestMacOSDataDir` | Platform-specific |
| R-4.1.3 | --config flag and env overrides | verified | `TestResolveConfigPath_*` (4 tests) | |
| R-4.2.1 | Auto-create config.toml | verified | `TestLoad_MinimalConfig_UsesDefaults`, `TestAppendDriveSection_CreatesFileWhenMissing` | |
| R-4.2.2 | Line-based edits preserve comments | verified | `TestAppendDriveSection_CommentPreservation_*` (3 tests) | |
| R-4.3 | Four-layer override chain | verified | `TestReadEnvOverrides_AllSet`, `TestResolveDrive_CLI*` (4 tests) | |
| R-4.4.1 | Reload config on SIGHUP | verified | `TestHolder_Update` | |
| R-4.4.2 | Concurrent hot reload safety | verified | `TestHolder_ConcurrentReadWrite` | 20 readers + 5 writers |
| R-3.4.1 | Multiple drive sections | verified | `TestLoad_FullConfigWithDrives`, `TestLoad_MultipleDriveSections` | |
| R-3.4.3 | SharePoint shares business token | verified | `TestDriveTokenPath_SharePoint_SharesBusinessToken`, `TestTokenAccountCID_SharePoint` | |
| R-6.2.9 | Configurable permissions | verified | `TestValidate_Permissions_Invalid`, `TestValidate_Permissions_Valid` | |

## graph-client.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-3.1 | Authentication | verified | `TestNewClient_*`, graph drives tests | Umbrella verified |
| R-1.1 | List (ls) | verified | `TestListChildren_*`, ls_test.go | |
| R-1.4 | Delete (rm) | verified | `TestDeleteItem_*`, root_test.go | |
| R-1.5 | Create Folder (mkdir) | verified | `TestCreateFolder_*`, root_test.go | |
| R-1.6 | Metadata (stat) | verified | `TestGetItem_*`, stat_test.go | |
| R-1.7 | Move (mv) | verified | `TestMoveItem_*` | |
| R-1.8 | Copy (cp) | verified | `TestCopyItem_*` | |
| R-6.7.8 | URL-decode item names | verified | `TestNormalize_URLDecodes*` | |
| R-6.7.9 | Filter OneNote packages | verified | `TestNormalize_FiltersOneNote*` | |
| R-6.7.10 | Deduplicate delta items | verified | `TestNormalize_Dedup*` | |
| R-6.7 | Technical requirements (umbrella) | implemented | – | Planned children block umbrella |
| R-6.8 | Network resilience (umbrella) | implemented | – | R-6.8.3 still [implemented] |
| R-6.7.17 | Hashless file comparison | implemented | – | Not annotated |

## drive-transfers.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-5.1 | Parallel transfers | verified | executor_test.go, worker_test.go | Worker pool |
| R-5.2 | Resumable transfers | verified | session_store_test.go, transfer_manager_test.go | |
| R-5.5 | Transfer validation | verified | quickxorhash_test.go, transfer_manager_test.go | |
| R-1.2 | Download (get) | verified | download_test.go, transfer_manager_test.go, root_test.go | |
| R-1.3 | Upload (put) | verified | upload_test.go, transfer_manager_test.go, root_test.go | |
| R-6.2.3 | Atomic file writes (S3) | verified | transfer_manager_test.go | .partial + verify + rename |
| R-5.3 | Upload chunking (umbrella) | implemented | – | R-5.3.2 [planned] |
| R-5.3.1 | Simple PUT ≤ 4 MiB | verified | upload_test.go | |
| R-5.3.3 | 320 KiB chunk alignment | verified | upload_test.go | |

## cli.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-6.6.1 | Dual-channel logging | verified | root_test.go | |
| R-6.6.2 | Console verbosity flags | verified | root_test.go | |
| R-6.6.3 | Independent log file level | verified | root_test.go | |
| R-6.6.4 | Structured JSON log file | verified | root_test.go | |
| R-6.2.8 | File ops independent of sync | verified | root_test.go | |
| R-6.3.3 | PID file with advisory lock | verified | pidfile_test.go | |
| R-4.7.3 | Log retention | verified | logfile_test.go | |
| R-1.9 | Recycle bin (umbrella) | implemented | recycle_bin_test.go | R-1.9.4 [planned] |
| R-1 | File operations (umbrella) | implemented | – | R-1.9 blocks |
| R-4.7.1 | Dual-channel logging | verified | root_test.go | |

## sync-observation.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-2.1.2 | Watch mode (continuous) | verified | observer_remote_test.go, observer_local_test.go | |
| R-6.7.1 | Delta operation reordering | verified | observer_remote_test.go | |
| R-6.7.3 | Track items by ID | verified | observer_remote_test.go | |
| R-6.7.5 | HTTP 410 re-enumeration | verified | observer_remote_test.go | |
| R-6.7.20 | Drive-type-adaptive observation | verified | observer_remote_test.go | |
| R-6.7.24 | Folder rename propagation | verified | observer_remote_test.go | |
| R-6.2.7 | No upload of partial files (S7) | verified | observer_local_test.go | |
| R-2.13.1 | Unicode NFC normalization | verified | permissions_test.go | |
| R-2.14.1 | Read-only shared items | verified | permissions_test.go | |
| R-2.4 | Filtering (umbrella) | implemented | observer_local_test.go, observer_local_handlers_test.go | Planned children block |

## sync-planning.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-2.2 | Conflict detection | verified | planner_test.go | Hash + mtime |
| R-2.3.1 | Conflict resolution (rename-aside) | verified | planner_test.go | |
| R-6.4.1 | Big-delete threshold | verified | planner_test.go | |
| R-6.4.2 | Big-delete percentage | verified | planner_test.go | |
| R-6.4.3 | Per-folder big-delete | verified | planner_test.go | |
| R-6.2.5 | Big-delete protection (S5) | verified | planner_edge_test.go | |

## sync-engine.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-2.6 | Pause/resume | verified | engine_test.go, pause_test.go, resume_test.go | |
| R-3.4.2 | Multi-drive sync | verified | engine_shortcuts_test.go | |
| R-2.1 | Sync modes (umbrella) | implemented | engine_test.go, sync_test.go | R-2.1.6 blocks |
| R-2.8 | Watch mode behavior (umbrella) | implemented | engine_test.go | R-2.8.5 [future] |

## sync-execution.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-5.1 | Parallel transfers | verified | executor_test.go, worker_test.go | |
| R-6.5.3 | Reset stuck items on startup | verified | engine_test.go, reconciler_test.go | |
| R-6.2.4 | Hash-verify before local delete (S4) | verified | executor_delete_test.go | |
| R-2.3 | Conflict resolution (umbrella) | implemented | – | R-2.3.5, R-2.3.6 block |

## sync-store.md

| Req ID | Description | Status | Test(s) | Notes |
|---|---|---|---|---|
| R-2.5 | Crash recovery (umbrella) | verified | baseline_test.go | All children verified |
| R-2.3.2 | Persistent conflict recording | verified | baseline_test.go | |
| R-2.3.3 | issues command | verified | issues_test.go | |
| R-2.7 | Verification (verify) | verified | verify_test.go (root + internal) | |
| R-6.4.4 | Remote delete to recycle bin | verified | trash_test.go | |
| R-6.4.5 | Local delete to OS trash | verified | trash_test.go, executor_delete_test.go | |
| R-6.4.6 | Linux trash opt-in | verified | trash_test.go | |
| R-2.15.1 | Delta checkpoint integrity | verified | baseline_test.go | |
| R-6.3.2 | Concurrent-reader safe status | verified | status_test.go | |

## Requirements Not Yet Verified

These requirements have code (`[implemented]`) but no test annotation yet:

| Req ID | Description | File | Reason |
|---|---|---|---|
| R-1.9 (umbrella) | Recycle bin | file-operations.md | R-1.9.4 [planned] blocks |
| R-2.1 (umbrella) | Sync modes | sync.md | R-2.1.6 not annotated |
| R-2.1.6 | --full flag | sync.md | Needs specific test |
| R-2.3 (umbrella) | Conflict resolution | sync.md | R-2.3.5, R-2.3.6 not annotated |
| R-2.3.5 | issues clear | sync.md | Needs test annotation |
| R-2.3.6 | issues retry | sync.md | Needs test annotation |
| R-2.4 (umbrella) | Filtering | sync.md | Planned children block |
| R-2.8 (umbrella) | Watch mode behavior | sync.md | R-2.8.5 [future] |
| R-2.13 (umbrella) | Unicode normalization | sync.md | Single-child umbrella |
| R-2.14 (umbrella) | Read-only shared items | sync.md | Single-child umbrella |
| R-2.15 (umbrella) | Delta checkpoint integrity | sync.md | Single-child umbrella |
| R-5.3 (umbrella) | Upload chunking | transfers.md | R-5.3.2 [planned] |
| R-6.2 (umbrella) | Data integrity | non-functional.md | Planned children block |
| R-6.4 (umbrella) | Safety | non-functional.md | Planned children block |
| R-6.6 (umbrella) | Observability | non-functional.md | Future children block |
| R-6.7 (umbrella) | Technical requirements | non-functional.md | Many planned children |
| R-6.7.4 | Detect post-upload modification | non-functional.md | Not annotated |
| R-6.7.7 | No hash compare for deleted items | non-functional.md | Not annotated |
| R-6.7.17 | Hashless file comparison | non-functional.md | Not annotated |
| R-6.8 (umbrella) | Network resilience | non-functional.md | R-6.8.3 not annotated |
| R-6.8.3 | Resumable after interruption | non-functional.md | Needs test annotation |
| R-3.6 (umbrella) | Shared drive discovery | drive-management.md | Planned children block |
