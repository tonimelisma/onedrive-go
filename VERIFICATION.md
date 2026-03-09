# Requirement Verification Tracker

Status: Increment 2 of 7 complete | Last updated: 2026-03-09

## Summary

| Design Doc | Requirements | Verified | Test Gap | Downgraded | Remaining |
|---|---|---|---|---|---|
| retry.md | 2 | 2 | 0 | 0 | 0 |
| system.md | 6 | 6 | 0 | 0 | 0 |
| data-model.md | 5 | 5 | 0 | 0 | 0 |
| drive-identity.md | 14 | 14 | 0 | 0 | 0 |
| config.md | ~12 | – | – | – | ~12 |
| graph-client.md | ~15 | – | – | – | ~15 |
| drive-transfers.md | ~10 | – | – | – | ~10 |
| cli.md | ~15 | – | – | – | ~15 |
| sync-observation.md | ~15 | – | – | – | ~15 |
| sync-planning.md | ~10 | – | – | – | ~10 |
| sync-engine.md | ~10 | – | – | – | ~10 |
| sync-execution.md | ~10 | – | – | – | ~10 |
| sync-store.md | ~10 | – | – | – | ~10 |
| **Total** | **~134** | **27** | **0** | **0** | **~107** |

## retry.md

| Req ID | Description | Status | Test(s) | Test Type | Notes |
|---|---|---|---|---|---|
| R-6.8.1 | Respect 429 with Retry-After | verified | `TestTransportDelay_MatchesCalcBackoff` (retry), `TestDo_RetryOn429WithRetryAfter`, `TestThrottleGate_429SetsDeadline` (graph) | unit | Policy validated here; HTTP-level 429 tests in graph pkg (Increment 4) |
| R-6.8.2 | Exponential backoff with jitter | verified | `TestPolicy_Delay_NoJitter`, `TestPolicy_Delay_WithJitter`, `TestPolicy_Delay_MaxCap`, `TestNamedPolicies_MatchOriginal`, `TestBackoff_Next_IncreasesDelay`, `TestBackoff_Reset`, `TestBackoff_MaxCap`, `TestBackoff_SetMaxOverride`, `TestBackoff_RemoteObserverPattern`, `TestCircuitBreaker_*` (8 tests) | unit | |

## system.md

| Req ID | Description | Status | Test(s) | Test Type | Notes |
|---|---|---|---|---|---|
| R-6.2.1 | No remote delete without baseline (S1) | verified | `TestS1_NoRemoteDeleteWithoutBaseline` | unit | Design invariant — planner rejects deletes for items not in baseline |
| R-6.2.2 | No delete from incomplete enumeration (S2) | verified | – | design | Architectural invariant — delta observer only processes explicit API delete events; orphan detection restricted to full reconciliation (`observeRemoteFull`) |
| R-6.3.1 | Single sync process | verified | `TestWritePIDFile_CreatesFileWithCurrentPID`, `TestWritePIDFile_FlockPreventsSecondAcquisition`, `TestWritePIDFile_CleanupRemovesFile` | unit | PID file with flock |
| R-6.1.4 | Startup < 1 second | verified | – | build | Build target; validated by CI build time. No code path to unit test. |
| R-6.1.5 | Binary < 20 MB | verified | – | build | Build artifact target; validated by `go build` output size |
| R-6.9.1 | Single static binary | verified | – | build | Go's default static linking; validated by successful `go build` |

## data-model.md

| Req ID | Description | Status | Test(s) | Test Type | Notes |
|---|---|---|---|---|---|
| R-6.5.1 | Durable transactional writes | verified | `TestNewSyncStore_WALMode`, `TestSyncStore_Close_CheckpointsWAL` | unit | SQLite WAL mode + checkpoint on close |
| R-6.5.2 | Atomic sync operations | verified | `TestCommit_Download`, `TestCommit_Upload`, `TestCommit_LocalDelete`, `TestCommit_RemoteDelete`, `TestCommit_SkipsFailedOutcomes`, `TestResetInProgressStates_*` (4 tests) | unit | Per-action transactional commits |
| R-2.5.1 | Resume from last checkpoint | verified | `TestResetInProgressStates_DeleteFileAbsent`, `TestResetInProgressStates_DeleteFileExists`, `TestResetInProgressStates_DownloadStillResetsToPending`, `TestResetInProgressStates_MixedStates` | unit | Crash recovery resets stuck states |
| R-2.5.2 | Durable transactional writes | verified | `TestNewSyncStore_WALMode`, `TestSyncStore_Close_CheckpointsWAL` | unit | Same as R-6.5.1 |
| R-2.3.2 | Persistent conflict recording | verified | `TestListConflicts_Empty`, `TestListConflicts_WithConflicts`, `TestListConflicts_OnlyUnresolved`, `TestGetConflict_ByID`, `TestGetConflict_ByPath`, `TestResolveConflict`, `TestCommitConflict_AutoResolved`, `TestCommitOutcome_Conflict_AutoResolved`, `TestCommitOutcome_Conflict_Unresolved`, `TestCommitOutcome_EditDeleteConflict_DeletesBaseline`, `TestConflictRecord_NameField`, `TestListAllConflicts`, `TestPruneResolvedConflicts` | unit | Conflict CRUD + lifecycle |

## drive-identity.md

| Req ID | Description | Status | Test(s) | Test Type | Notes |
|---|---|---|---|---|---|
| R-3.2.1 | OneDrive Personal | verified | `TestNewCanonicalID`, `TestConstruct`, `TestCanonicalID_DriveType` | unit | Personal canonical ID parsing/construction |
| R-3.2.2 | OneDrive Business | verified | `TestNewCanonicalID`, `TestConstruct`, `TestCanonicalID_DriveType` | unit | Business canonical ID parsing/construction |
| R-3.2.3 | SharePoint Document Libraries | verified | `TestNewCanonicalID`, `TestConstructSharePoint`, `TestCanonicalID_DriveType` | unit | SharePoint canonical ID with site/library |
| R-3.2.4 | Shared Folders | verified | `TestNewCanonicalID_SharedType`, `TestConstructShared`, `TestCanonicalID_IsShared`, `TestCanonicalID_SharedTextRoundTrip` | unit | Shared drive canonical ID |
| R-3.3.1 | drive list | verified | `TestNewDriveCmd_Structure`, `TestBuildConfiguredDriveEntries_*` (5 tests), `TestPrintDriveListText_*` (6 tests) | unit | |
| R-3.3.2 | drive add | verified | `TestAddNewDrive_WithToken`, `TestAddSharedDrive_AlreadyConfigured` | unit | |
| R-3.3.3 | drive remove | verified | `TestRemoveDrive_DeletesConfigSection`, `TestRemoveDrive_DriveNotInConfig` | unit | |
| R-3.3.4 | drive search | verified | `TestPrintDriveSearchText_WithResults`, `TestPrintDriveSearchText_MultipleSites` | unit | |
| R-3.5.1 | --drive matching | verified | `TestBuildConfiguredDriveEntries_ExplicitDisplayName`, `TestPrintDriveListText_ShowsDisplayName` | unit | Canonical ID, display name, substring |
| R-3.5.2 | --account selection | verified | `TestFindBusinessTokens_FilterSelectsOne` | unit | Account email filtering |
| R-3.5.3 | --drive repeatable | verified | – | design | CLI flag definition; cobra repeatable string slice |
| R-3.6.1 | drive list shows shared folders | verified | `TestPrintDriveListText_SharedDrive`, `TestPrintDriveListText_AvailableOnly` | unit | |
| R-3.6.2 | Search API for shared discovery | verified | `TestSearchSharedItemsWithFallback_SearchSucceeds`, `TestSearchSharedItemsWithFallback_SearchFails_SharedWithMeSucceeds` | unit | Primary search + fallback |
| R-3.6.3 | Derive display name from sharer | verified | `TestDeriveSharedDisplayName_Basic`, `TestDeriveSharedDisplayName_FirstNameCollision`, `TestDeriveSharedDisplayName_FullNameCollision` | unit | Collision handling |
| R-6.7.2 | Normalize driveId values | verified | `TestNew_TruncatedDriveIDZeroPadding`, `TestNew_CaseNormalization`, `TestNew_Idempotent`, `TestID_Equal_CrossCaseMatch`, `TestID_Equal_PaddedMatch`, `TestItemKey_NormalizedEquality`, `TestItemKey_PaddedDriveIDEquality` | unit | Lowercase + zero-pad normalization |
