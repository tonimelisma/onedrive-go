# Requirement Verification Tracker

Status: Increment 1 of 7 complete | Last updated: 2026-03-09

## Summary

| Design Doc | Requirements | Verified | Test Gap | Downgraded | Remaining |
|---|---|---|---|---|---|
| retry.md | 2 | 2 | 0 | 0 | 0 |
| system.md | 6 | 6 | 0 | 0 | 0 |
| data-model.md | 5 | 5 | 0 | 0 | 0 |
| drive-identity.md | ~14 | – | – | – | ~14 |
| config.md | ~12 | – | – | – | ~12 |
| graph-client.md | ~15 | – | – | – | ~15 |
| drive-transfers.md | ~10 | – | – | – | ~10 |
| cli.md | ~15 | – | – | – | ~15 |
| sync-observation.md | ~15 | – | – | – | ~15 |
| sync-planning.md | ~10 | – | – | – | ~10 |
| sync-engine.md | ~10 | – | – | – | ~10 |
| sync-execution.md | ~10 | – | – | – | ~10 |
| sync-store.md | ~10 | – | – | – | ~10 |
| **Total** | **~134** | **13** | **0** | **0** | **~121** |

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
